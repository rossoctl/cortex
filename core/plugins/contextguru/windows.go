package contextguru

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rossoctl/context-guru/apply"
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

// recordBilled stores the provider-billed input of a finished request, which the next request
// reads as Ctx.PrevBilledInput. Without it a fraction trigger has nothing to compare against
// and declines on every turn. Billed input is fresh + cache read + cache write: the ruler a
// context window is stated in.
//
// The session figure records only a successful main-agent request. A subagent shares the session
// id but carries its own, much smaller transcript, and recording it would understate the main
// thread's fill.
//
// The per-thread figure (Ctx.PrevBilledInputThread) is recorded for every thread, subagents
// included, under the thread the engine resolved on this request's OnRequest (cgThread): the
// engine reads it under that thread, so the write must use the same one.
func (p *ContextGuru) recordBilled(pctx *pipeline.Context, sid string) {
	inf := pctx.Extensions.Inference
	if inf == nil || pctx.StatusCode >= 400 {
		return
	}
	billed := int64(inf.InputTokens) + int64(inf.CacheReadTokens) + int64(inf.CacheWriteTokens)
	if billed <= 0 {
		return
	}
	// The key the pipeline derived on the request (Trace.Session), not one re-derived here: the
	// engine scopes and validates the id, and the two must match for the next request to read it.
	key, ok := p.cgSessions.get(sid)
	if !ok {
		return
	}
	if th := pipeline.GetState[cgThread](pctx, cgThreadStateKey); th != nil {
		apply.RecordBilledInputThread(p.store, key, th.id, billed)
	}
	if isSubagent(pctx) {
		return
	}
	apply.RecordBilledInput(p.store, key, billed)
}

// sessionKeysCap and sessionKeysTrimTo bound sessionKeys the way core/accounting bounds its
// per-thread maps (and accountant.Trackers its threads): past the cap, the sessions seen
// longest ago are dropped down to the trim mark, so the sort runs once per 1,000 new sessions
// rather than on every request. A dropped session only loses its billed-input record until its
// next request sets the key again.
const (
	sessionKeysCap    = 10000
	sessionKeysTrimTo = sessionKeysCap - sessionKeysCap/10
)

// sessionKeys is cgSessions: Cortex session id to the engine's session key, bounded. Safe for
// concurrent use; the zero value is ready.
type sessionKeys struct {
	mu  sync.Mutex
	m   map[string]sessionKey
	now func() time.Time // nil: time.Now
}

type sessionKey struct {
	key      string
	lastSeen time.Time
}

func (s *sessionKeys) set(sid, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]sessionKey{}
	}
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	s.m[sid] = sessionKey{key: key, lastSeen: now()}
	if len(s.m) <= sessionKeysCap {
		return
	}
	type aged struct {
		sid string
		at  time.Time
	}
	all := make([]aged, 0, len(s.m))
	for k, e := range s.m {
		all = append(all, aged{k, e.lastSeen})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	for _, a := range all[:len(s.m)-sessionKeysTrimTo] {
		delete(s.m, a.sid)
	}
}

func (s *sessionKeys) get(sid string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[sid]
	return e.key, ok
}

func (s *sessionKeys) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}
