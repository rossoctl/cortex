package contextguru

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/rossoctl/context-guru/lifecycle"
	"github.com/rossoctl/context-guru/models"
	"github.com/rossoctl/context-guru/models/litellm"
	"github.com/rossoctl/cortex/core/pipeline"
)

// windowsConfig says where the plugin learns a model's context window. The engine needs it for
// every fraction-of-window trigger — cache_aware_summarizer's shipped 0.9 among them — and
// declines any such trigger unless the window is EXACT (Trigger.FracResolvable). Before this
// existed the plugin passed no window at all, so those triggers could never fire in Cortex.
type windowsConfig struct {
	// ModelInfo is "litellm" (the LiteLLM public price/window map, fetched in the background
	// and cached) or "off" (no network; only Windows and the engine's built-in table, which is
	// never exact). Unset, it is "litellm" on a local install and "off" anywhere else, so a pod
	// does not fetch from the public internet unless its config asks: see buildWindows.
	ModelInfo string `json:"model_info"`
	// ModelInfoURL overrides the map's URL. Empty means the public map. Setting it with
	// ModelInfo unset turns the map on everywhere: the operator pointed the plugin at one.
	ModelInfoURL string `json:"model_info_url"`
	// Windows maps a model id, as the request names it, to its window in tokens. Checked first,
	// and treated as exact: the operator wrote it for this model.
	Windows map[string]int `json:"windows"`
}

// fixedWindows is the operator's own table. Exact by construction.
type fixedWindows map[string]int

func (f fixedWindows) Window(_ context.Context, model string) (int, bool) {
	w, ok := f[strings.ToLower(strings.TrimSpace(model))]
	return w, ok && w > 0
}

func (f fixedWindows) WindowExact(ctx context.Context, model string) (int, bool, bool) {
	w, ok := f.Window(ctx, model)
	return w, ok, ok
}

// buildWindows returns the resolver chain: the operator's table, then the LiteLLM map when it is
// on (modelInfoOn), then the engine's built-in table as an inexact last resort.
func buildWindows(c windowsConfig, local bool) models.Resolver {
	chain := models.Chain{}
	if len(c.Windows) > 0 {
		f := make(fixedWindows, len(c.Windows))
		for k, v := range c.Windows {
			f[strings.ToLower(strings.TrimSpace(k))] = v
		}
		chain = append(chain, f)
	}
	if modelInfoOn(c, local) {
		// A standalone client on the plugin's model transport, so on a local install the fetch
		// ignores HTTPS_PROXY (which points at Cortex itself there) and is not routed back through
		// this proxy (modelTransport). The map refreshes in the background, so a lookup never waits
		// on the network; until the first fetch lands the chain falls through and the fraction
		// triggers decline, which is the safe direction.
		chain = append(chain, litellm.New(c.ModelInfoURL,
			&http.Client{Timeout: 30 * time.Second, Transport: modelTransport(local)}, 0))
	}
	return append(chain, models.DefaultStatic())
}

// modelInfoOn says whether the LiteLLM map is fetched. An explicit model_info wins: "off" turns it
// off, anything else on. Unset, a model_info_url turns it on, and otherwise it is on only on a
// local install: every pod fetching the public map on its first gated request (and again after
// every hot reload) is network egress a cluster operator did not ask for.
func modelInfoOn(c windowsConfig, local bool) bool {
	switch v := strings.TrimSpace(c.ModelInfo); {
	case v != "":
		return !strings.EqualFold(v, "off")
	case strings.TrimSpace(c.ModelInfoURL) != "":
		return true
	default:
		return local
	}
}

// windowFor resolves the request's model window. exact is false whenever the answer came from a
// guess, which keeps every fraction trigger declining rather than firing on it.
func (p *ContextGuru) windowFor(ctx context.Context, pctx *pipeline.Context) (window int, exact bool) {
	inf := pctx.Extensions.Inference
	if p.windows == nil || inf == nil || inf.Model == "" {
		return 0, false
	}
	w, ex, ok := models.ExactWindow(p.windows, ctx, inf.Model)
	if !ok {
		return 0, false
	}
	return w, ex
}

// finishEngine runs context-guru's steps after a response (lifecycle.Finish), with the session and
// thread the engine used on this request's OnRequest (cgThread). The engine reads every one of
// them back under that pair (apply.Opts.ThreadOf's host contract), so OnFinish must write them
// under it too:
//
//   - It releases a cache-aware summary that the turn deferred
//     (offload.ResolveDeferredCacheAwareSummary). This plugin sets Ctx.PrefixAsk on Anthropic, and
//     without this call such a summary waits for the thread's next request or a fallback timer.
//     The caller runs stashSent first, because the summary's side call reads this turn's stashed
//     body.
//   - It runs the warm sweep's question, if the trigger registered one (offload.ResolveWarmQuestion).
//   - When the response has usage, it keeps the billed input for the next turn, under the session
//     and under the thread. The size trigger reads only the thread's figure
//     (Ctx.PrevBilledInputThread, Trigger.FracResolvable). Without it the trigger declines on
//     every turn, and cache_aware_summarizer never fires (#2130). Billed input is fresh + cache
//     read + cache write: the ruler a context window is stated in.
//
// A request the engine did not run on (no cgThread) has nothing to finish.
func (p *ContextGuru) finishEngine(pctx *pipeline.Context) {
	th := pipeline.GetState[cgThread](pctx, cgThreadStateKey)
	if th == nil {
		return
	}
	var billed int64
	cacheRead := 0
	inf := pctx.Extensions.Inference
	if inf != nil {
		billed = int64(inf.InputTokens) + int64(inf.CacheReadTokens) + int64(inf.CacheWriteTokens)
		cacheRead = inf.CacheReadTokens
	}
	lifecycle.Finish(lifecycle.Fin{
		Store:     p.store,
		Session:   th.session,
		ThreadID:  th.id,
		UsageOK:   inf != nil && pctx.StatusCode < 400 && billed > 0,
		Billed:    billed,
		Forwarded: pctx.StatusCode >= 200 && pctx.StatusCode < 300,
		CacheRead: cacheRead,
	})
}
