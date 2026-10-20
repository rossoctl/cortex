package contextguru

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/proxyapp"
	"github.com/rossoctl/cortex/core/pipeline"
)

// windowsConfig says where the plugin learns a model's context window. The engine needs it for
// every fraction-of-window trigger — cache_aware_summarizer's shipped 0.9 among them — and
// declines any such trigger unless the window is EXACT (Trigger.FracResolvable). Before this
// existed the plugin passed no window at all, so those triggers could never fire in Cortex.
type windowsConfig struct {
	// ModelInfo is "litellm" (default: the LiteLLM public price/window map, fetched in the
	// background and cached) or "off" (no network; only Windows and the engine's built-in
	// table, which is never exact).
	ModelInfo string `json:"model_info"`
	// ModelInfoURL overrides the map's URL. Empty means the public map.
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

// buildWindows returns the resolver chain: the operator's table, then the LiteLLM map unless
// turned off, then the engine's built-in table as an inexact last resort.
func buildWindows(c windowsConfig) proxyapp.Resolver {
	chain := proxyapp.Chain{}
	if len(c.Windows) > 0 {
		f := make(fixedWindows, len(c.Windows))
		for k, v := range c.Windows {
			f[strings.ToLower(strings.TrimSpace(k))] = v
		}
		chain = append(chain, f)
	}
	if !strings.EqualFold(strings.TrimSpace(c.ModelInfo), "off") {
		// A standalone client, not one routed back through this proxy. The map refreshes in the
		// background, so a lookup never waits on the network; until the first fetch lands the
		// chain falls through and the fraction triggers decline, which is the safe direction.
		chain = append(chain, proxyapp.NewLiteLLM(c.ModelInfoURL, &http.Client{Timeout: 30 * time.Second}, 0))
	}
	return append(chain, proxyapp.DefaultStatic())
}

// windowFor resolves the request's model window. exact is false whenever the answer came from a
// guess, which keeps every fraction trigger declining rather than firing on it.
func (p *ContextGuru) windowFor(ctx context.Context, pctx *pipeline.Context) (window int, exact bool) {
	inf := pctx.Extensions.Inference
	if p.windows == nil || inf == nil || inf.Model == "" {
		return 0, false
	}
	w, ex, ok := proxyapp.ExactWindow(p.windows, ctx, inf.Model)
	if !ok {
		return 0, false
	}
	return w, ex
}

// recordBilled stores the provider-billed input of a finished request, which the next request
// reads as Ctx.PrevBilledInput. Without it a fraction trigger has nothing to compare against
// and declines on every turn. Billed input is fresh + cache read + cache write: the ruler a
// context window is stated in.
//
// Only a successful main-agent request is recorded. A subagent shares the session id but carries
// its own, much smaller transcript, and recording it would understate the main thread's fill.
func (p *ContextGuru) recordBilled(pctx *pipeline.Context, sid string) {
	inf := pctx.Extensions.Inference
	if inf == nil || isSubagent(pctx) || pctx.StatusCode >= 400 {
		return
	}
	billed := int64(inf.InputTokens) + int64(inf.CacheReadTokens) + int64(inf.CacheWriteTokens)
	if billed <= 0 {
		return
	}
	// The key the pipeline derived on the request (Trace.Session), not one re-derived here: the
	// engine scopes and validates the id, and the two must match for the next request to read it.
	key, ok := p.cgSessions.Load(sid)
	if !ok {
		return
	}
	apply.RecordBilledInput(p.store, key.(string), billed)
}
