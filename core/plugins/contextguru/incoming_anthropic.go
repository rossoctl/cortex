package contextguru

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/cheapmodel"
	cgcomponents "github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/thread"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/tidwall/gjson"
)

// The incoming model for an Anthropic Messages request, and the prefix asker the summarizer
// needs on that dialect. llmclient speaks only OpenAI Chat Completions, so without this every
// model.source: incoming component (cache_aware_summarizer and extract_llm among them) has no
// model on Claude Code's traffic and does nothing. This is the engine proxy's arrangement
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
//   - Each call is reported to the request it was made for as a KindSpend op, as cgModel does
//     for the OpenAI path (meteredDispatcher).

// incomingHoldTTL is how long a caller's credential stays held after the last request that used
// it. Matches the engine proxy's IncomingRefTTL.
const incomingHoldTTL = 2 * time.Hour

// holdSweepInterval is how often credHolds checks every held credential for incomingHoldTTL,
// independent of traffic. Without this, a hold already past its TTL was only ever noticed
// (and released) by `hold`, so it is in `hold`'s request path: on a quiet installation with no
// later Anthropic request, a masked credential stayed held — and the session's forwarded-body
// stash beside it — for as long as the process ran (review finding). The sweep bounds that to
// one interval past the TTL, whether or not any request arrives.
const holdSweepInterval = 10 * time.Minute

// credHolds dedups holds on one dispatcher by credential, so a session holds one copy of its key
// however many requests it makes.
type credHolds struct {
	d  *cortexDispatcher
	mu sync.Mutex
	m  map[[32]byte]*credHold

	sweepStop chan struct{}
	sweepDone chan struct{}
}

type credHold struct {
	ref      dispatch.Ref
	lastUsed time.Time
}

func newCredHolds(d *cortexDispatcher) *credHolds {
	return &credHolds{d: d, m: map[[32]byte]*credHold{}}
}

// startSweep releases every hold idle past incomingHoldTTL, once now and then every
// holdSweepInterval, until stopSweep runs. Starting it twice on one credHolds (without an
// intervening stopSweep) panics on the closed channel, the same guard releaseAll-after-Shutdown
// would need; callers hold one credHolds per plugin instance and call this at most once, from
// Init.
func (h *credHolds) startSweep() {
	h.sweepStop = make(chan struct{})
	h.sweepDone = make(chan struct{})
	h.releaseExpired(time.Now())
	go func() {
		defer close(h.sweepDone)
		t := time.NewTicker(holdSweepInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				h.releaseExpired(time.Now())
			case <-h.sweepStop:
				return
			}
		}
	}()
}

// stopSweep stops the sweep goroutine and waits for it to exit, so a Shutdown that calls
// releaseAll right after never races the sweep's own release of the same holds. A credHolds
// that never had startSweep called (no Init ran, as in a test built directly with
// newCredHolds) has a nil sweepStop, so this is a no-op rather than a nil-channel block.
func (h *credHolds) stopSweep() {
	if h.sweepStop == nil {
		return
	}
	close(h.sweepStop)
	<-h.sweepDone
}

// hold returns the ref for this request's endpoint and credential, holding it if it is new, and
// releases every hold idle past incomingHoldTTL. Also done on a timer (startSweep), so this is
// belt-and-suspenders for a key that holds right up to its own release.
func (h *credHolds) hold(pctx *pipeline.Context, now time.Time) dispatch.Ref {
	key := credKey(pctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.releaseExpiredLocked(now)
	if e, ok := h.m[key]; ok {
		e.lastUsed = now
		return e.ref
	}
	ref := h.d.hold(pctx.Scheme, pctx.Host, pctx.Headers)
	h.m[key] = &credHold{ref: ref, lastUsed: now}
	return ref
}

// releaseExpired is releaseExpiredLocked with its own lock, for the sweep goroutine, which does
// not otherwise touch h.mu.
func (h *credHolds) releaseExpired(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.releaseExpiredLocked(now)
}

func (h *credHolds) releaseExpiredLocked(now time.Time) {
	for k, e := range h.m {
		if now.Sub(e.lastUsed) > incomingHoldTTL {
			h.d.Release(e.ref)
			delete(h.m, k)
		}
	}
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

// anthropicIncoming returns the agent's own model for an Anthropic request. ok is false when the
// request carries no credential or names no model.
func (p *ContextGuru) anthropicIncoming(pctx *pipeline.Context) (cheapmodel.Anthropic, bool) {
	if p.holds == nil || pctx.Host == "" || !hasCredential(pctx) {
		return cheapmodel.Anthropic{}, false
	}
	model := modelFromBody(pctx.Body)
	if model == "" {
		return cheapmodel.Anthropic{}, false
	}
	ref := p.holds.hold(pctx, time.Now())
	d := meteredDispatcher{
		cortexDispatcher: p.holds.d,
		host:             pctx.Host,
		model:            model,
		report:           pctx.OpReporter(),
		lateDropped:      &p.lateModelCalls,
	}
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

// meteredDispatcher reports each call it sends as a KindSpend op on the request it was made for,
// from the usage block of the Anthropic response, as cgModel.Complete does from llmclient's. Like
// cgModel, it captures pctx, dryRun and the late-drop counter on the request goroutine. A call
// that returns after the request was accounted is booked by the accountant as a detached spend
// (pipeline.LateOpsAccountant); one that it cannot book is counted in lateDropped.
type meteredDispatcher struct {
	*cortexDispatcher
	host, model string
	report      pipeline.OpReporter
	lateDropped *atomic.Int64
}

func (m meteredDispatcher) Do(ctx context.Context, req dispatch.Request) (dispatch.Response, error) {
	resp, err := m.cortexDispatcher.Do(ctx, req)
	if err != nil || resp.Status < 200 || resp.Status > 299 {
		return resp, err
	}
	if call, ok := anthropicCall(resp.Body, m.host, m.model); ok {
		accepted := m.report.Report(accountant.Operation{
			Kind: accountant.KindSpend,
			Call: &call,
		})
		if !accepted && m.lateDropped != nil {
			m.lateDropped.Add(1)
		}
	}
	return resp, err
}

// anthropicCall reads the billed usage of one Anthropic Messages response. ok is false when the
// body has no usage, so nothing is reported for a call that billed nothing.
func anthropicCall(body []byte, host, model string) (accountant.ModelCall, bool) {
	var r struct {
		Model string `json:"model"`
		Usage struct {
			Input         int `json:"input_tokens"`
			CacheRead     int `json:"cache_read_input_tokens"`
			CacheWrite    int `json:"cache_creation_input_tokens"`
			Output        int `json:"output_tokens"`
			CacheCreation struct {
				Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &r) != nil {
		return accountant.ModelCall{}, false
	}
	u := r.Usage
	if u.Input == 0 && u.CacheRead == 0 && u.CacheWrite == 0 && u.Output == 0 {
		return accountant.ModelCall{}, false
	}
	if r.Model != "" {
		model = r.Model
	}
	return accountant.ModelCall{
		Host:  host,
		Model: model,
		Usage: accountant.CallUsage{
			Usage: accountant.Usage{
				Input:        u.Input,
				CacheRead:    u.CacheRead,
				CacheWrite:   u.CacheWrite,
				CacheWrite1h: min(u.CacheCreation.Ephemeral1h, u.CacheWrite),
			},
			Output: u.Output,
		},
	}, true
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

// sentStash holds, per engine thread key (thread.Key(session, thread)), the last body forwarded upstream and accepted
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

// getCovered checks coverage and reads the stashed body under ONE lock (components.
// PrefixCoveredAsker's own reason to exist: CoversSpan followed by get reads the stash twice,
// and a concurrent put -- the next request on this thread, since the ask that calls this runs
// detached -- can replace the entry between the two reads, so the ask goes out over a body the
// check never approved).
func (s *sentStash) getCovered(session string, count int, hash string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[session]
	if !ok {
		return nil, cgcomponents.ErrNoPrefix
	}
	if count >= 0 {
		norm := apply.NormalizeMessages(e.provider, e.preBody)
		if count > len(norm) || cgcomponents.SpanHash(norm[:count]) != hash {
			return nil, cgcomponents.ErrStalePrefix
		}
	}
	return e.forwarded, nil
}

// pendingBody is OnRequest's snapshot of the body this plugin received, kept on the REQUEST's
// own pctx (pipeline.SetState) rather than in a map keyed by session. A session-keyed map let
// two requests sharing one session id (Claude Code's one-shot calls, the main turn) overlap in
// flight and clobber each other's entry, pairing one request's received body with a different
// request's forwarded body (review finding). pctx is single-threaded per request, so keying on
// it makes the pairing exact by construction. See the comment where OnRequest sets this.
type pendingBody struct {
	body []byte
}

// pendingBodyStateKey is this plugin's pipeline.SetState key for pendingBody.
const pendingBodyStateKey = "context-guru-pending-body"

// stashSent stores a finished main-agent request's two bodies for the next turn's prefix ask.
// Only a 200 is a valid prefix: a rejected send populated no prompt cache. pctx.Body in OnFinish
// is the body that went upstream. A shadow run (on_error: observe) sent the client's body, which
// is still pctx.Body, so the stash matches what upstream cached in that case too.
//
// KEYED BY THREAD: thread.Key(session, thread), the engine session key and the thread this
// request's OnRequest used (cgThread). With Opts.ThreadOf set, the engine asks with that key
// (cache_aware_summarizer, extract_sweep), and context-guru-proxy stashes under it too. A
// session-keyed stash was right only on the primary thread: after a rewind the main conversation
// moves to a p: thread, and every prefix ask there found nothing, so its summary never came.
// thread.Key gives the session itself for the primary thread, so that key is unchanged.
//
// A tool-less request on a NON-PRIMARY thread is not stashed (review finding): a session's side
// calls (a title, a permission check) are always such threads, and the stash holds only
// maxStashSessions entries. The primary thread is stashed regardless of tools, as main did before
// this PR: an Anthropic client that declares no tools at all (a plain chat app through Cortex)
// still gets its prefix ask stashed, and so still gets a summary.
//
// Nothing is stashed for a redirected request (review finding): inference-router runs after
// context-guru (agentop server add appends it last), so the incoming-model hold this plugin took
// in OnRequest is always on the host and credential the CLIENT named — captured before any
// redirect — while pctx.Body by OnFinish can be inference-router's own rewrite of what actually
// went out (a different host, possibly a translated model name). A prefix ask on the next turn
// would then send that routed body, under the routed model's shape, to the client's ORIGINAL
// host, which may not recognise it. Whether the incoming model should instead follow the
// redirect is a product question (does context-guru's own side-channel traffic mean to honor a
// routing rule meant for the agent's main traffic, or always reach the agent's real endpoint?)
// this fix does not answer; it only removes the concrete risk of sending a mismatched body.
// pctx.Redirected stays false under on_error: observe and after a refused redirect, so neither
// case is affected.
func (p *ContextGuru) stashSent(pctx *pipeline.Context) {
	pre := pipeline.GetState[pendingBody](pctx, pendingBodyStateKey)
	if pre == nil || p.stash == nil || isSubagent(pctx) || pctx.StatusCode != 200 || pctx.Redirected() {
		return
	}
	th := pipeline.GetState[cgThread](pctx, cgThreadStateKey)
	if th == nil || (th.id != thread.Primary && !gjson.GetBytes(pre.body, "tools.0").Exists()) {
		return
	}
	provider, _ := dialectFor(pctx.Path)
	p.stash.put(thread.Key(th.session, th.id), provider, pre.body, pctx.Body)
}

// prefixAsker is the components.PrefixAsker the engine receives on an Anthropic request.
type prefixAsker struct {
	stash *sentStash
	cli   cheapmodel.Anthropic
}

// Also implements components.PrefixCoveredAsker (AskCovered, below): usePrefixAsk makes this
// the engine's only path on Anthropic, where the call runs detached, so the check-then-ask race
// PrefixCoveredAsker exists to close is live here, not theoretical.
var _ cgcomponents.PrefixCoveredAsker = prefixAsker{}

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

// AskCovered implements components.PrefixCoveredAsker: the engine's only path on Anthropic
// (usePrefixAsk), where the call runs detached, so a plain CoversSpan-then-Ask would read the
// stash twice and could ask over a body a concurrent put already replaced (the race
// PrefixCoveredAsker's doc describes). getCovered checks and reads under one lock instead.
func (p prefixAsker) AskCovered(ctx context.Context, session string, count int, hash, ask string) (string, cgcomponents.PrefixUsage, error) {
	body, err := p.stash.getCovered(session, count, hash)
	if err != nil {
		return "", cgcomponents.PrefixUsage{}, err
	}
	reply, u, err := p.cli.CompletePrefixed(ctx, body, ask)
	return reply, cgcomponents.PrefixUsage(u), err
}
