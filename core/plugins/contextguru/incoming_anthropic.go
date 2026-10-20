package contextguru

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"sync"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/cheapmodel"
	cgcomponents "github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/cost/spend"
	"github.com/rossoctl/cortex/core/pipeline"
)

// The incoming model for an Anthropic Messages request, and the prefix asker the summarizer
// needs on that dialect. llmclient speaks only OpenAI Chat Completions, so before this existed
// every model.source: incoming component — cache_aware_summarizer and extract_llm among them —
// had no model on Claude Code's traffic and did nothing. This is the engine proxy's arrangement
// (proxy/proxy.go incomingModel, proxy/prefixask.go), rebuilt on Cortex's dispatcher:
//
//   - cheapmodel.Anthropic is the client: the request's own endpoint, credential and model.
//   - The credential is HELD, not borrowed for one request. cache_aware_summarizer's call is
//     detached: the turn that crosses the threshold commissions it in a goroutine with a 300 s
//     budget and forwards unchanged, so a hold released when the request returns would be gone
//     before the call is sent. One hold per credential, reused, released incomingHoldTTL after
//     its last use (the engine proxy's IncomingRefTTL), and all of them at Shutdown.
//   - On Anthropic the summarizer asks through a components.PrefixAsker: one question appended
//     to the previous turn's FORWARDED body, so the provider's prompt cache serves the whole
//     transcript. The plugin keeps that body in a sentStash.

// incomingHoldTTL is how long a caller's credential stays held after the last request that used
// it. Matches the engine proxy's IncomingRefTTL.
const incomingHoldTTL = 2 * time.Hour

// credHolds dedups holds on one dispatcher by credential, so a session holds one copy of its key
// however many requests it makes.
type credHolds struct {
	d  *cortexDispatcher
	mu sync.Mutex
	m  map[[32]byte]*credHold
}

type credHold struct {
	ref      dispatch.Ref
	lastUsed time.Time
}

func newCredHolds(d *cortexDispatcher) *credHolds {
	return &credHolds{d: d, m: map[[32]byte]*credHold{}}
}

// hold returns the ref for this request's endpoint and credential, holding it if it is new, and
// releases every hold idle past incomingHoldTTL.
func (h *credHolds) hold(pctx *pipeline.Context, now time.Time) dispatch.Ref {
	key := credKey(pctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	for k, e := range h.m {
		if now.Sub(e.lastUsed) > incomingHoldTTL {
			h.d.Release(e.ref)
			delete(h.m, k)
		}
	}
	if e, ok := h.m[key]; ok {
		e.lastUsed = now
		return e.ref
	}
	ref := h.d.hold(pctx.Scheme, pctx.Host, pctx.Headers)
	h.m[key] = &credHold{ref: ref, lastUsed: now}
	return ref
}

func (h *credHolds) releaseAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for k, e := range h.m {
		h.d.Release(e.ref)
		delete(h.m, k)
	}
}

// credKey identifies an endpoint and credential without keeping the credential: a digest.
func credKey(pctx *pipeline.Context) [32]byte {
	var b strings.Builder
	b.WriteString(pctx.Scheme + "\x00" + pctx.Host)
	for _, name := range authHeaderNames {
		for _, v := range pctx.Headers.Values(name) {
			b.WriteString("\x00" + name + "\x00" + v)
		}
	}
	return sha256.Sum256([]byte(b.String()))
}

// anthropicIncoming returns the agent's own model for an Anthropic request. nil when the request
// carries no credential or names no model.
func (p *ContextGuru) anthropicIncoming(pctx *pipeline.Context, to *chargeTo) (cheapmodel.Anthropic, bool) {
	if p.holds == nil || pctx.Host == "" || !hasCredential(pctx) {
		return cheapmodel.Anthropic{}, false
	}
	model := modelFromBody(pctx.Body)
	if model == "" {
		return cheapmodel.Anthropic{}, false
	}
	ref := p.holds.hold(pctx, time.Now())
	d := meteredDispatcher{cortexDispatcher: p.holds.d, to: to, host: pctx.Host}
	return cheapmodel.Anthropic{D: d, Ref: ref, Model: model}, true
}

func hasCredential(pctx *pipeline.Context) bool {
	if pctx.Headers == nil {
		return false
	}
	for _, name := range authHeaderNames {
		if pctx.Headers.Get(name) != "" {
			return true
		}
	}
	return false
}

// meteredDispatcher charges each call it sends to the session, from the usage block of the
// Anthropic response, as the OpenAI path's cgModel does from llmclient's.
type meteredDispatcher struct {
	*cortexDispatcher
	to   *chargeTo
	host string
}

func (m meteredDispatcher) Do(ctx context.Context, req dispatch.Request) (dispatch.Response, error) {
	start := time.Now()
	resp, err := m.cortexDispatcher.Do(ctx, req)
	if err != nil || m.to == nil {
		return resp, err
	}
	var body struct {
		Model string `json:"model"`
		Usage struct {
			Input      int `json:"input_tokens"`
			CacheRead  int `json:"cache_read_input_tokens"`
			CacheWrite int `json:"cache_creation_input_tokens"`
			Output     int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(resp.Body, &body) == nil {
		u := body.Usage
		m.to.record(spend.PurposeCompaction, m.host, body.Model,
			pricing.Usage{Input: u.Input, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, Output: u.Output},
			resp.Status, start, time.Since(start))
	}
	return resp, err
}

// Stash bounds. The engine proxy stashes no body over 1.5 MB, which is about 375k tokens of
// Claude Code traffic: below the 900k at which the summarizer fires on a 1M window, so there the
// ask would never find a prefix. These are sized for that window. Only main-agent bodies are
// stashed, one entry per session.
const (
	maxStashSessions = 16
	maxStashBody     = 8 << 20
	maxStashBytes    = 96 << 20
)

// sentStash holds, per engine session key, the last body forwarded upstream and accepted
// (forwarded, which Ask appends to) and the body the engine received for it (preBody, which
// CoversSpan normalizes: the summarizer hashes its span from the messages as the pipeline
// received them, before any later component rewrote them).
type sentStash struct {
	mu    sync.Mutex
	m     map[string]stashEntry
	bytes int
	seq   uint64
}

type stashEntry struct {
	forwarded, preBody []byte
	provider           bschemas.ModelProvider
	seq                uint64
}

func newSentStash() *sentStash { return &sentStash{m: map[string]stashEntry{}} }

// put replaces the session's entry. A body over maxStashBody deletes the old entry rather than
// leaving it: an older body would make Ask answer about a transcript missing everything since.
func (s *sentStash) put(session string, provider bschemas.ModelProvider, preBody, forwarded []byte) {
	if session == "" || len(forwarded) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.m[session]; ok {
		s.bytes -= len(old.forwarded) + len(old.preBody)
		delete(s.m, session)
	}
	if len(forwarded) > maxStashBody || len(preBody) > maxStashBody {
		return
	}
	size := len(forwarded) + len(preBody)
	for len(s.m) > 0 && (len(s.m) >= maxStashSessions || s.bytes+size > maxStashBytes) {
		oldestKey, oldest := "", uint64(0)
		for k, e := range s.m {
			if oldestKey == "" || e.seq < oldest {
				oldestKey, oldest = k, e.seq
			}
		}
		s.bytes -= len(s.m[oldestKey].forwarded) + len(s.m[oldestKey].preBody)
		delete(s.m, oldestKey)
	}
	s.seq++
	s.m[session] = stashEntry{forwarded: append([]byte(nil), forwarded...),
		preBody: append([]byte(nil), preBody...), provider: provider, seq: s.seq}
	s.bytes += size
}

func (s *sentStash) get(session string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[session].forwarded
}

// CoversSpan reports whether the stashed body holds the first count messages of the span the
// caller hashed. With nothing stashed it answers true, and Ask then reports ErrNoPrefix.
func (s *sentStash) CoversSpan(session string, count int, hash string) bool {
	s.mu.Lock()
	e, ok := s.m[session]
	s.mu.Unlock()
	if !ok || count < 0 {
		return true
	}
	norm := apply.NormalizeMessages(e.provider, e.preBody)
	if count > len(norm) {
		return false
	}
	return cgcomponents.SpanHash(norm[:count]) == hash
}

// stashSent stores a finished main-agent request's two bodies for the next turn's prefix ask.
// Only a 200 is a valid prefix: a rejected send populated no prompt cache. pctx.Body in OnFinish
// is the body that went upstream.
func (p *ContextGuru) stashSent(pctx *pipeline.Context, sid string) {
	pre, ok := p.pending.LoadAndDelete(sid)
	if !ok || p.stash == nil || isSubagent(pctx) || pctx.StatusCode != 200 {
		return
	}
	key, ok := p.cgSessions.Load(sid)
	if !ok {
		return
	}
	provider, _ := dialectFor(pctx.Path)
	p.stash.put(key.(string), provider, pre.([]byte), pctx.Body)
}

// prefixAsker is the components.PrefixAsker the engine receives on an Anthropic request.
type prefixAsker struct {
	stash *sentStash
	cli   cheapmodel.Anthropic
}

func (p prefixAsker) Ask(ctx context.Context, session, ask string) (string, cgcomponents.PrefixUsage, error) {
	body := p.stash.get(session)
	if len(body) == 0 {
		return "", cgcomponents.PrefixUsage{}, cgcomponents.ErrNoPrefix
	}
	reply, u, err := p.cli.CompletePrefixed(ctx, body, ask)
	return reply, cgcomponents.PrefixUsage(u), err
}

func (p prefixAsker) CoversSpan(session string, count int, hash string) bool {
	return p.stash.CoversSpan(session, count, hash)
}
