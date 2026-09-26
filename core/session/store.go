// Package session provides an in-memory session store for correlating
// inbound user intents with outbound tool calls across request boundaries.
// The store is per-pod (AuthBridge sidecar) and does not persist across restarts.
package session

import (
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
)

// DefaultSessionID is used when no explicit A2A SessionID is present and no
// active session exists. This collapses all such requests into one shared session,
// which is correct for single-agent pods but may cause cross-user correlation in
// multi-tenant deployments. Future work: derive session ID from JWT claims.
const DefaultSessionID = "default"

// entry holds the events for one conversation.
type entry struct {
	ID        string
	Events    []pipeline.SessionEvent
	CreatedAt time.Time
	UpdatedAt time.Time

	// intern collapses the message content this session repeats on every turn. Per
	// session, so it is freed with the session and never shares content between two
	// conversations. See intern.go for why the table only holds one event's strings.
	intern Interner

	// nextSeq is the Seq the next appended event gets, counting from 1.
	//
	// Never reset FOR THE LIFETIME OF THIS ENTRY: trimming events must not reuse their
	// numbers, or a client holding a cursor from before the trim would page into the wrong
	// place. The scope matters and is easy to overstate — this counter lives on the entry,
	// and cleanupLocked and evictOldestLocked delete the entry outright, so a session
	// re-created under the same id afterwards starts a NEW counter at 1. Numbers are
	// therefore unique within one incarnation of a session, not across the id forever.
	//
	// Nothing here can detect that, and nothing here needs to: the store cannot tell a
	// re-created session from a trimmed one. A paging client compares wall-clock time
	// rather than Seq for exactly this reason — see abctl's applyOlderPage. See also
	// pipeline.SessionEvent.Seq.
	nextSeq uint64

	// cost and avoided are RUNNING TOTALS over Events, maintained by Append and read by
	// ListSessions. Micros, in the units usage.Counts uses.
	//
	// INCREMENTAL BECAUSE THE ALTERNATIVE WAS A REGRESSION. These were first summed on
	// demand, by decoding every event of every session inside ListSessions. That runs under
	// the read lock, abctl re-fetches /v1/sessions every two seconds, and the event list is
	// UNCAPPED by default (see New) — so a long session put an unbounded number of
	// json.Unmarshal calls on a timer, in front of a lock whose writer side is
	// Store.Append on the proxy's request path. sumTokens beside it is a pointer-deref
	// loop; this was parsing. ledger.Writer.Record hoists its own phase guard above
	// event.Record for exactly this reason.
	//
	// usage.CostSum, not two int64s: it saturates rather than wrapping AND records that it
	// did, so a clamped lifetime total arrives labelled instead of as a plausible
	// ~$9.2 trillion. It is the one saturating money accumulator in this codebase; a local
	// copy here would be a second implementation of that rule with the label dropped.
	//
	// MAINTAINED IN LOCKSTEP WITH Events, including on trim: whatever leaves the slice is
	// subtracted, so these stay equal to sumCost(Events) — the invariant
	// TestAppend_RunningTotalsMatchAFullRecomputation exists to hold. That is what keeps
	// the COST column scoped exactly like the TOKENS column beside it, which is the claim
	// SessionSummary.CostMicros makes.
	cost    usage.CostSum
	avoided usage.CostSum

	// money is each event's decoded contribution, PARALLEL TO Events: same length, same
	// order, same trim.
	//
	// It exists so a TRIM needs no decode. The running totals above have to shed whatever
	// leaves Events, and the first version did that by re-decoding each evicted event —
	// inside the write lock, one json.Unmarshal per append in the capped configuration, on
	// the proxy's request path. That is the same critical-section parsing the hoist in Append
	// exists to avoid, reintroduced through the back door; the defence written for it argued
	// only against a full O(maxEvents) re-sum, not against being in the lock at all.
	//
	// Keeping the figure costs 16 bytes per event against an event struct orders of magnitude
	// larger, and it is the only way to subtract without either parsing again or re-deriving
	// the pin rule. applyTrim reshapes this and Events together for that second reason.
	money []eventMoney

	// context is the CONTEXT gauge's answer for this session: the main-agent turn that RANKS
	// HIGHEST under the rule's total order, in prompt tokens — the LATEST such turn where the
	// client states its role, the one with the MOST MESSAGES where it does not. Maintained by
	// Append and read by ListSessions; see pipeline.PromptContextFold for the order itself.
	//
	// NOT A MAXIMUM OVER TOKENS, so this can DECREASE: the stated arm leads with the timestamp,
	// which is the point of the column. A compaction is the ordinary case — 830,000 before it and
	// 12,000 on the turn after — and nothing here or on the wire may be read as a high-water mark.
	//
	// AN EXTREMUM, NOT A SUM, and that is the whole difference from cost above. It is deliberately
	// NOT maintained in lockstep with Events: nothing about it is accumulated, so a trim has
	// nothing to subtract from it, where a partial sum over a trimmed slice sheds exactly what left
	// the slice. Recomputing it over the survivors would not understate by a known amount the way
	// that sum does — it would report a small conversation for a session whose conversation is
	// large and merely aged out. See TestAppend_PromptContextSurvivesATrim.
	//
	// So this needs none of the machinery cost needs: no subtraction on trim, and no parallel
	// per-event slice to make that subtraction decode-free. Fixed size per SESSION.
	context pipeline.PromptContextFold
}

// MaxSessionIDLen is the longest session ID the store keeps intact; longer ids
// are truncated on Append. Exported so callers that validate an id before
// reaching the store — the session API's query parameters, for one — bound it at
// the same length instead of duplicating the number.
const MaxSessionIDLen = 256

// Recorder receives every event the store appends, for side-channel
// aggregation. Declared here as a narrow interface rather than importing the
// usage package so the dependency points one way: usage knows about events,
// the store knows nothing about usage.
//
// Implementations are called while the store holds its write lock, so they must
// not block or call back into the store.
type Recorder interface {
	Record(sessionID string, e *pipeline.SessionEvent)
}

// Store is an in-memory, per-pod session store. It is safe for concurrent use.
type Store struct {
	mu          sync.RWMutex
	sessions    map[string]*entry
	ttl         time.Duration
	maxEvents   int
	maxSessions int
	activeID    string
	stop        chan struct{}
	closeOnce   sync.Once

	// subscribers is the fan-out list for Subscribe(). Protected by mu.
	subscribers []*subscriber

	// recorders are notified of every appended event. Registered at setup via
	// AddRecorder, before the store serves traffic.
	recorders []Recorder
}

// subscriberChanBuf caps each subscriber's channel depth. 64 absorbs short
// bursts without unbounded memory; slow consumers drop rather than blocking
// Append.
const subscriberChanBuf = 64

// subscriber wires one live consumer of session events. Writes on ch are
// non-blocking — a full channel increments drops and discards the event.
type subscriber struct {
	ch        chan pipeline.SessionEvent
	drops     uint64 // atomic; reads via LoadDrops
	closeOnce sync.Once
}

// closeCh closes ch at most once. Called by the cancel func returned from
// Subscribe and by Store.Close during shutdown; whichever runs first wins.
func (s *subscriber) closeCh() {
	s.closeOnce.Do(func() { close(s.ch) })
}

// LoadDrops returns the total events dropped for this subscriber since Subscribe.
func (s *subscriber) LoadDrops() uint64 {
	return atomic.LoadUint64(&s.drops)
}

// Subscribe registers a listener for session events written after the call.
// The returned channel is buffered (64). Events are sent non-blockingly; if
// the consumer falls behind, events are dropped — the caller can observe the
// drop count via the returned Subscription's Drops method. Always call the
// cancel func to remove the subscriber and close the channel.
func (s *Store) Subscribe() (*Subscription, func()) {
	sub := &subscriber{ch: make(chan pipeline.SessionEvent, subscriberChanBuf)}

	s.mu.Lock()
	s.subscribers = append(s.subscribers, sub)
	s.mu.Unlock()

	cancel := func() {
		s.mu.Lock()
		for i, existing := range s.subscribers {
			if existing == sub {
				s.subscribers = append(s.subscribers[:i], s.subscribers[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
		sub.closeCh()
	}
	return &Subscription{sub: sub}, cancel
}

// Subscription is the consumer-facing handle returned by Store.Subscribe.
type Subscription struct {
	sub *subscriber
}

// Events returns the receive-only channel of session events.
func (s *Subscription) Events() <-chan pipeline.SessionEvent {
	return s.sub.ch
}

// Drops returns the cumulative event drops due to a full channel.
func (s *Subscription) Drops() uint64 {
	return s.sub.LoadDrops()
}

// New creates a Store with the given TTL, per-session event limit, and max
// concurrent sessions. maxEvents <= 0 keeps every event a session produces, which
// is what the binaries pass unless session.max_events is configured; maxSessions
// <= 0 likewise keeps every session. A background goroutine runs cleanup every
// TTL/2.
// Call Close() during graceful shutdown to stop the background goroutine.
func New(ttl time.Duration, maxEvents int, maxSessions int) *Store {
	s := &Store{
		sessions:    make(map[string]*entry),
		ttl:         ttl,
		maxEvents:   maxEvents,
		maxSessions: maxSessions,
		stop:        make(chan struct{}),
	}
	// No reaper when nothing can expire: with ttl <= 0 the interval below clamps to one
	// second and Cleanup would wake every second forever to find nothing.
	if ttl > 0 {
		go s.backgroundCleanup()
	}
	return s
}

// Close stops the background cleanup goroutine and closes every subscriber's
// channel so consumers unblock cleanly on shutdown. Safe to call multiple times.
func (s *Store) Close() {
	s.closeOnce.Do(func() {
		close(s.stop)

		s.mu.Lock()
		subs := s.subscribers
		s.subscribers = nil
		s.mu.Unlock()

		for _, sub := range subs {
			sub.closeCh()
		}
	})
}

func (s *Store) backgroundCleanup() {
	interval := s.ttl / 2
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.Cleanup()
		}
	}
}

// Append adds an event to the named session. Creates the session if it
// doesn't exist. Updates activeID to this session. Evicts the oldest event if the
// session exceeds maxEvents — which by default it cannot, maxEvents being unset.
func (s *Store) Append(sessionID string, event pipeline.SessionEvent) {
	if len(sessionID) > MaxSessionIDLen {
		sessionID = sessionID[:MaxSessionIDLen]
	}

	// BEFORE THE LOCK. This is a json.Unmarshal of the event's plugin map, the most
	// expensive thing on this path, and it touches no store state — so it has no business
	// inside a critical section that blocks every reader and every other appender. Doing it
	// here is also what lets ListSessions read two integers instead of decoding every event
	// of every session on abctl's two-second poll; see entry.cost.
	money := moneyOf(&event)

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()

	sess, ok := s.sessions[sessionID]
	if !ok {
		sess = &entry{
			ID:        sessionID,
			CreatedAt: now,
		}
		s.sessions[sessionID] = sess
	}

	// Stamp the bucket ID so downstream consumers can attribute the event
	// without needing to know which session it was appended to — critical for
	// outbound events that have no protocol-native session field.
	event.SessionID = sessionID

	// Stamp the cursor before publishLocked below, so a subscriber streaming events
	// live sees the same Seq a paging client would get for that event. If this were
	// assigned after publishing, the two views of one event would disagree and a
	// client could not stitch a page onto its stream.
	sess.nextSeq++
	event.Seq = sess.nextSeq

	// Before the copy is taken: an LLM request re-sends the whole conversation, so most
	// of this event's message text is already in the session. Point at what is there
	// rather than keeping a second copy of it.
	sess.intern.InternEvent(&event)

	sess.Events = append(sess.Events, event)
	// Both in lockstep with the append above: the running totals, and the per-event figure
	// the trim below subtracts from them without decoding anything.
	sess.money = append(sess.money, money)
	sess.cost.Add(money.cost)
	sess.avoided.Add(money.avoided)
	// AND THE PROMPT-CONTEXT FIGURE, which unlike the two above sheds nothing on trim.
	//
	// THE PROPERTY IS THAT AN EVENT APPENDED AND IMMEDIATELY EVICTED STILL CONTRIBUTES, because
	// the figure outlives the events it was read from — TestAppend_PromptContextSurvivesATrim pins
	// it. WHAT SECURES IT IS THAT Add READS &event, THE LOCAL PARAMETER, rather than the tail of
	// sess.Events. An earlier version of this comment called the placement "load-bearing rather than
	// incidental" instead, which is false as stated and would have sent a reader guarding the wrong
	// thing: what breaks the property is sourcing the candidate from the stored slice.
	//
	// FREE OF THE TRIM, NOT FREE OF EVERYTHING BELOW IT, and the distinction is worth stating
	// because the first correction of that claim overshot in the other direction. The trim reshapes
	// sess.Events and sess.money and subtracts from sess.cost and sess.avoided, none of which this
	// reads, and there is no early return between here and it — so moving this call past the TRIM
	// would change nothing. THE RECORDERS ARE NOT IN THAT CLASS: they run between here and the trim
	// and are handed &event, and Recorder's contract forbids blocking and re-entering the store
	// while saying nothing about MUTATION. Every implementation in tree reads only
	// (usage.Aggregator, ledger.Writer, and logAppended just below), so nothing is wrong today —
	// but that is a property of those implementations rather than a guarantee from the interface, so
	// moving this call below them is not provably free and must not be done casually.
	//
	// NOT HOISTED ABOVE THE LOCK like moneyOf, and that is not an oversight. moneyOf is a
	// json.Unmarshal and was hoisted because of a measured regression; this is a phase check, a
	// nil check, len(Tools), two AgentRole comparisons, a messageCount read, TWO int adds and
	// one comparison (pipeline.PromptTokensOf sums three fields), a Round(0) that clears a
	// monotonic reading, and — once the fold already holds a figure — better()'s time.Time
	// Equal/After chain: tens of nanoseconds, no allocation, NO DECODE. Splitting it to hoist the
	// extraction would add an exported type for plumbing alone and save nothing measurable.
	sess.context.Add(&event)
	sess.UpdatedAt = now
	s.activeID = sessionID

	logAppended(sessionID, &event)
	s.publishLocked(event)

	// Side-channel aggregation (e.g. /v1/usage). Runs under the write lock, so
	// a Recorder must be cheap and must not re-enter the store. Unlike
	// subscribers this cannot drop: a dropped event silently skews a cumulative
	// counter, where a dropped stream frame only costs one client one row.
	for _, r := range s.recorders {
		r.Record(sessionID, &event)
	}

	if p, ok := planTrim(sess.Events, s.maxEvents); ok {
		// NO DECODE HERE. The figures come off the parallel money slice, so the only work
		// this adds to the critical section is integer subtraction — see entry.money for
		// what the first version did instead.
		//
		// Sub, not Add(-x): see usage.CostSum.Sub for the math.MinInt64 case it exists for.
		for i, m := range sess.money {
			if p.keeps(i) {
				continue
			}
			sess.cost.Sub(m.cost)
			sess.avoided.Sub(m.avoided)
		}
		// One plan, both slices. They must come out the same length in the same order, and
		// applyTrim is what makes that structural rather than a convention.
		sess.Events = applyTrim(sess.Events, p)
		sess.money = applyTrim(sess.money, p)
	}

	// Evict oldest session if cap is exceeded.
	if s.maxSessions > 0 && len(s.sessions) > s.maxSessions {
		s.evictOldestLocked()
	}
}

// isIntentEvent matches the SessionView.LastIntent predicate: an
// inbound A2A request event. IBAC and any future intent-aware
// guardrail call LastIntent and need the most recent such event to
// survive FIFO eviction. Defined here so the eviction policy and
// the consumer view can never drift apart.
func isIntentEvent(e pipeline.SessionEvent) bool {
	return e.Direction == pipeline.Inbound &&
		e.Phase == pipeline.SessionRequest &&
		e.A2A != nil
}

// trimEventsPinIntent reduces events to len <= maxEvents while
// preserving the most-recent intent event, even when that intent
// sits in the prefix that FIFO would normally evict. All other
// events evict in chronological order; the protected intent stays
// at its original index, leaving a temporal gap between it and the
// first non-evicted event after it. That gap is visible in
// /v1/sessions and abctl as a discontinuity in the timeline, which
// is the right shape: the intent's append time is preserved
// (consumers can correlate against the inbound request's wall-clock
// timestamp) and chronological order across surviving events stays
// monotonic.
//
// Why pin only the MOST-RECENT intent: a session can carry several
// user turns ("solve this", then "now do that") and only the latest
// is what IBAC aligns subsequent tool calls against. Pinning all
// intents would let stale ones pile up under pathological loads
// (slow turns + huge fan-out) and starve the buffer.
//
// Pathological case — the buffer is mostly intents, exceeds
// maxEvents, and only the latest is pinned: older intents evict via
// normal FIFO. There is no scenario where this returns more than
// maxEvents events.
//
// Caller guarantees len(events) > maxEvents and maxEvents > 0;
// otherwise this is a no-op shape (returns events unchanged).
//
// RETURNS A PLAN RATHER THAN A SLICE, expressed as POSITIONS. The entry carries a parallel
// money slice, and both have to survive the trim identically — so the decision is made once
// here and applied by applyTrim to each. A caller re-deriving "which events went" would be a
// second implementation of the pin rule below, and the two would drift the day either changed.
//
// ok is false when nothing needs trimming, so a caller can skip the work entirely.
func planTrim(events []pipeline.SessionEvent, maxEvents int) (trimPlan, bool) {
	if maxEvents <= 0 || len(events) <= maxEvents {
		return trimPlan{}, false
	}
	excess := len(events) - maxEvents

	// Locate the most-recent intent. If it sits in the keep window
	// (index >= excess) it survives a plain FIFO trim — fast path.
	intentIdx := -1
	for i := len(events) - 1; i >= 0; i-- {
		if isIntentEvent(events[i]) {
			intentIdx = i
			break
		}
	}
	if intentIdx == -1 || intentIdx >= excess {
		// No protected event in the eviction prefix; FIFO trim.
		return trimPlan{pinned: -1, tailStart: excess}, true
	}

	// Protected intent is in the eviction prefix. Build the result
	// as: [pinned intent] ++ [last maxEvents-1 non-intent-prefix
	// events]. We keep the intent at the front so its original
	// timestamp is preserved AND the resulting slice stays
	// chronologically ordered (intent.At < kept-tail.At by construction —
	// it's the oldest surviving event).
	return trimPlan{pinned: intentIdx, tailStart: len(events) - (maxEvents - 1)}, true
}

// trimPlan is which POSITIONS survive a trim. See planTrim for the rule and why it is
// positions rather than values.
type trimPlan struct {
	// pinned is the index moved to the front of the result, or -1 for a plain FIFO drop.
	pinned int
	// tailStart is the first index of the surviving suffix.
	tailStart int
}

// keeps reports whether the element at i survives.
func (p trimPlan) keeps(i int) bool { return i == p.pinned || i >= p.tailStart }

// applyTrim rebuilds one slice under a plan.
//
// GENERIC so the events and their parallel money slice cannot be reshaped by two different
// pieces of code. They are different element types and must end up the same length in the same
// order, and the only way to guarantee that is for one function to do both.
func applyTrim[T any](in []T, p trimPlan) []T {
	if p.pinned < 0 {
		out := make([]T, len(in)-p.tailStart)
		copy(out, in[p.tailStart:])
		return out
	}
	out := make([]T, 0, len(in)-p.tailStart+1)
	out = append(out, in[p.pinned])
	return append(out, in[p.tailStart:]...)
}

// AddRecorder registers a Recorder. Call before the store serves traffic; it is
// not safe to call concurrently with Append.
func (s *Store) AddRecorder(r Recorder) {
	if r != nil {
		s.recorders = append(s.recorders, r)
	}
}

// View returns a read-only snapshot of the session's events.
// Returns nil if the session doesn't exist or is expired.
func (s *Store) View(sessionID string) *pipeline.SessionView {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sess, ok := s.sessions[sessionID]
	if !ok {
		return nil
	}
	if s.isExpired(sess, time.Now()) {
		return nil
	}

	events := make([]pipeline.SessionEvent, len(sess.Events))
	copy(events, sess.Events)
	return &pipeline.SessionView{ID: sessionID, Events: events}
}

// ViewTail returns the most recent limit events of a session, and reports how many
// the session holds in total.
//
// It exists because View copies every event a session has, and with max_events unset
// that is unbounded: one session reached 5078 events, whose JSON encoding is 1.1GB and
// takes 17s to write on loopback. The only consumer of that response is a TUI with a
// 10s client timeout, so the whole of it was spent producing bytes nobody received.
//
// limit <= 0 is treated as 1, not as "unlimited": an accidental zero from a caller
// that forgot to set it should return a small answer, not the largest one the store
// can produce. Callers that genuinely want everything call View.
//
// TotalEvents and OldestSeq on the returned view are set only when events were left out,
// so a tail that happens to cover the whole session is byte-identical to what View
// produces. Use ViewPage to reach the events a tail leaves behind.
// The tail IS the newest page, so this delegates rather than repeating the slicing and
// the two-field bookkeeping — which were identical in both, and are the part a later
// change would update in one place and not the other.
func (s *Store) ViewTail(sessionID string, limit int) *pipeline.SessionView {
	return s.ViewPage(sessionID, 0, limit)
}

// ViewPage returns up to limit events ending just BEFORE the given Seq — the page older
// than a cursor the caller already holds.
//
// It is what makes the whole store readable. ViewTail can only ever return the most
// recent events, capped by what one response is willing to carry, so on a long session
// every event before that window was unreachable through any API: a laptop session held
// 2119 events where nothing could read past the most recent 2000, while the oldest events
// still cost memory. Paging backward from the tail reaches all of them without any single
// response growing.
//
// The cursor is a Seq rather than an offset because offsets do not survive eviction: a
// FIFO trim between two requests shifts every index, so a client would silently skip or
// repeat a page. Seq survives that trim, and the events a session holds are ascending in
// Seq (see trimEventsPinIntent), so the boundary is a binary search.
//
// What Seq does NOT survive is the session itself being evicted and re-created under the
// same id, which restarts the numbering — see entry.nextSeq. A cursor from the previous
// incarnation is then above everything held, and this function answers with the tail,
// because from here every event held does precede that cursor. That is deliberate rather
// than defensive: the store cannot distinguish the two cases, so the client compares
// timestamps instead of trusting the ordering it gets back.
//
// before == 0 means "from the newest", making ViewPage(id, 0, n) equivalent to
// ViewTail(id, n) — a client can then page with one code path rather than special-casing
// its first request. limit <= 0 is treated as 1, as in ViewTail.
//
// TotalEvents and OldestSeq are set on the same terms as ViewTail: only when this page is
// not the whole session, so a caller can tell "this is the beginning" from "there is more
// behind me" without a second request.
func (s *Store) ViewPage(sessionID string, before uint64, limit int) *pipeline.SessionView {
	if limit <= 0 {
		limit = 1
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	sess, ok := s.sessions[sessionID]
	if !ok {
		return nil
	}
	if s.isExpired(sess, time.Now()) {
		return nil
	}

	// The first event whose Seq is >= before is where this page has to stop; everything
	// at or after it either is the cursor event or is newer than it, and the caller
	// already has those. sort.Search finds that index whether or not an event with
	// exactly that Seq is still held, which is what makes a cursor pointing at an
	// already-evicted event behave sensibly instead of being an error.
	end := len(sess.Events)
	if before > 0 {
		end = sort.Search(len(sess.Events), func(i int) bool {
			return sess.Events[i].Seq >= before
		})
	}
	start := end - limit
	if start < 0 {
		start = 0
	}

	page := sess.Events[start:end]
	events := make([]pipeline.SessionEvent, len(page))
	copy(events, page)

	view := &pipeline.SessionView{ID: sessionID, Events: events}
	if start > 0 || end < len(sess.Events) {
		view.TotalEvents = len(sess.Events)
		view.OldestSeq = sess.Events[0].Seq
	}
	return view
}

// SessionSummary is a metadata-only view of a session, suitable for list
// endpoints that shouldn't copy the full event backlog.
type SessionSummary struct {
	ID          string    `json:"id"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	EventCount  int       `json:"eventCount"`
	TotalTokens int       `json:"totalTokens,omitempty"` // sum of Inference.TotalTokens across response events
	// CostMicros is what this session's events cost, in millionths of a dollar, summed from
	// the records the session itself holds.
	//
	// FROM THE EVENTS, not from the ledger, and that is the only honest source available: the
	// ledger's row key is (endpoint, model, agent, provenance) with no session dimension by
	// design, so it cannot answer "what did this session cost" at all. The live ring can, but
	// only over its rolling window, which would put a partial figure beside the LIFETIME
	// TotalTokens above and understate the row by whatever fell off the ring.
	//
	// SCOPED TO WHAT THE STORE STILL HOLDS, and it therefore RESETS ON RESTART while a ledger
	// window does not. Both are correct: this is "the cost of the events in this session", the
	// ledger is "the cost of the day". A client showing them together must not present one as
	// a check on the other.
	//
	// Omitted when zero rather than sent as 0, on this codebase's standing rule that an
	// unknown cost must never render as $0.00: a session whose traffic nothing could price is
	// indistinguishable here from one that was free, so the field's absence lets a client show
	// "—" instead of asserting either.
	CostMicros int64 `json:"costMicros,omitempty"`
	// AvoidedMicros is cost this session did NOT incur, same unit, APPLIED savings only.
	//
	// NOT SPEND, and never to be added to CostMicros — see usage.Counts.AvoidedMicros and the
	// invariant on event.Event.Avoided. Reported beside it because the two answer
	// different questions about the same session.
	AvoidedMicros int64 `json:"avoidedMicros,omitempty"`
	// Saturated says the two figures above are FLOORS: an addition into one of them reached
	// the int64 ceiling and was clamped rather than allowed to wrap.
	//
	// It exists because the clamp on its own is not honest. MaxInt64 micros is about
	// $9.2 trillion, which renders as a perfectly well-formed dollar amount and is
	// indistinguishable from a measured one — "a clamped figure is a wrong number wearing a
	// right label", which is why pricing.MicrosFromUSD refuses an out-of-range figure
	// instead of clamping it. Here the sum cannot be refused (the money was really spent),
	// so the figure is clamped and this field is what makes that admissible.
	//
	// ONE FLAG FOR BOTH, on usage.Counts.Saturated's reasoning: it means "read every money
	// figure here as a bound", and a second flag for the same fact would let a client
	// believe one while the other contradicted it.
	//
	// Practically unreachable — it takes over a thousand maximal per-request figures in one
	// session — and carried anyway, because the alternative is a number no consumer can
	// tell apart from a real one.
	Saturated bool `json:"saturated,omitempty"`
	Active    bool `json:"active"` // true if this is the most recently updated session
	// PromptContext is how full this session's conversation got, by the rule in
	// pipeline.PromptContextFold: the main-agent turn that RANKS HIGHEST under that rule's total
	// order, in prompt tokens. Where the client states its role that is the LATEST such turn;
	// where it does not it is the one with the MOST MESSAGES. See pipeline.PromptContext, which is
	// this field's type and carries the order.
	//
	// NO MONOTONICITY IS PROMISED, and a client must not build on one: this figure can DECREASE
	// between two polls. A compaction is the ordinary case, not a corner — the stated arm leads
	// with arrival time, so a session reporting 830,000 before one reports 12,000 on the turn
	// after it. WHERE THE ROLE IS STATED that is what the column is for: an operator needs how full
	// the conversation is NOW rather than how full it has ever been. The unstated arm claims no such
	// thing — it ranks by message count, so it can legitimately hold a pre-compaction turn for the
	// rest of a session, which is the documented cost of having no role to read rather than a defect
	// (see pipeline.PromptContextOf). So the figure decreases on one arm and goes stale on the
	// other, and neither is a guarantee to build on.
	//
	// RETENTION-INDEPENDENT ALL THE SAME — unlike TotalTokens and CostMicros above, and the
	// asymmetry is deliberate rather than an inconsistency. Those are sums over the events the
	// store still holds, so a trim sheds exactly what left the slice, which is why CostMicros can
	// honestly call itself the cost of the events in this session. This is an extremum under a
	// total order: nothing about it is accumulated, so a trim has nothing to subtract from it and
	// it outlives the events it was read from. Recomputing it over the survivors would not
	// understate by a known amount the way that sum does — it would report a small conversation
	// for a session whose conversation is large and merely aged out of the store. A client showing
	// the three on one row must not present any of them as a check on another.
	//
	// A POINTER SO ABSENT AND ZERO STAY APART, which is the same standing rule CostMicros states
	// for its omitempty: an unknown figure must never render as a real one. A session with only
	// one-shot completions has no conversation to measure and is indistinguishable here from one
	// nobody observed; both must reach a client as an absent field so it can draw an em dash.
	//
	// RESETS ON PROXY RESTART, like CostMicros and unlike a cost-ledger window, because the store
	// is in-memory per-pod. A client that has been watching longer than this proxy has been up may
	// hold a larger figure legitimately — see pipeline.MergePromptContext, which is how the two combine.
	PromptContext *pipeline.PromptContext `json:"promptContext,omitempty"`
}

// ListSessions returns summaries for every non-expired session. Order is
// UpdatedAt descending (most recent first). Safe for concurrent use.
func (s *Store) ListSessions() []SessionSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now()
	out := make([]SessionSummary, 0, len(s.sessions))
	for id, sess := range s.sessions {
		if s.isExpired(sess, now) {
			continue
		}
		out = append(out, SessionSummary{
			ID:         id,
			CreatedAt:  sess.CreatedAt,
			UpdatedAt:  sess.UpdatedAt,
			EventCount: len(sess.Events),
			// Still a walk, and deliberately left as one: it is a pointer deref per event
			// with no allocation, where the money figures below needed a JSON unmarshal.
			TotalTokens: sumTokens(sess.Events),
			// Read, not computed. Append maintains these; see entry.cost.
			CostMicros:    sess.cost.Micros,
			AvoidedMicros: sess.avoided.Micros,
			// Either total having clamped makes BOTH figures on this row bounds rather than
			// sums, which is why one flag covers them — the same reasoning
			// usage.Counts.Saturated gives for covering every money field in a Counts.
			Saturated: sess.cost.Saturated || sess.avoided.Saturated,
			Active:    id == s.activeID,
			// Read, not computed — and unlike TotalTokens above this is NOT a walk. Folding the rule
			// here instead would repeat the mistake entry.cost documents: O(events) per session
			// under the read lock, on abctl's two-second poll, in front of a lock whose writer side
			// is Append on the proxy's request path, with maxEvents unset by default.
			PromptContext: sess.context.Publish(),
		})
	}
	// Most recently updated first.
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

// sumTokens aggregates Inference.TotalTokens across all response events
// in a session. Used by ListSessions so the summary endpoint can report a
// per-session cost without the caller loading every event first.
func sumTokens(events []pipeline.SessionEvent) int {
	var total int
	for i := range events {
		if events[i].Phase != pipeline.SessionResponse {
			continue
		}
		if events[i].Inference == nil {
			continue
		}
		total += events[i].Inference.TotalTokens
	}
	return total
}

// eventMoney is one event's contribution to a session's running totals.
//
// Decoded ONCE, by Append, and never again. See Store.Append for why that matters and
// entry.cost for what holds the result.
type eventMoney struct {
	cost    int64
	avoided int64
}

// moneyOf reads one event's settled cost and its avoided cost, in micros.
//
// ONE DECODE FOR BOTH, because both come off the same record and event.Record is a JSON
// unmarshal. Asking for them separately would parse every plugin map twice.
//
// Priced dollars only in cost — event.Event.Priced is the one predicate for that, so a
// refused or unsettled figure contributes nothing here exactly as it contributes nothing to
// /v1/usage. avoided ignores pricedness on purpose: a request that could not be priced still
// had prompt tokens removed. See usage.Aggregator.costOf, which this deliberately mirrors,
// and note the two must never be added together.
//
// CALLED OUTSIDE THE STORE'S LOCK. It touches no store state and takes the event by pointer
// only to avoid copying it, so Append resolves it before acquiring mu — the unmarshal is the
// most expensive thing on that path and it does not belong inside the critical section.
func moneyOf(e *pipeline.SessionEvent) eventMoney {
	// The two phases that carry a settled figure, matching what the aggregator and the
	// durable ledger both fold. SessionResponse is the ordinary path; a DENIAL can still
	// carry one, because the proxy charges for a response whose body never arrived — so
	// restricting this to sumTokens' response-only rule would drop real dollars. It adds no
	// token skew against TotalTokens: a denial carries no Inference extension, so those
	// events contribute zero tokens either way.
	//
	// CHECKED BEFORE THE DECODE, matching ledger.Writer.Record's own hoisted guard and
	// for the same reason it gives: a request event's plugin map must not be parsed to
	// answer a question the phase already settles.
	if e.Phase != pipeline.SessionResponse && e.Phase != pipeline.SessionDenied {
		return eventMoney{}
	}
	ev, ok := event.Record(e)
	if !ok {
		return eventMoney{}
	}
	var m eventMoney
	if ev.Priced() {
		m.cost = ev.Micros()
	}
	m.avoided = ev.TotalAvoidedMicros()
	return m
}

// sumCost aggregates one session's settled cost and its avoided cost by walking every event.
//
// NOT ON ANY SERVING PATH. ListSessions reads the running totals Append maintains; this is
// the REFERENCE DEFINITION those totals must agree with, kept because an incremental total is
// only as good as the full recomputation it claims to equal, and
// TestAppend_RunningTotalsMatchAFullRecomputation asserts exactly that after appends, trims
// and evictions. Deleting it would leave the increments with nothing to check them against.
func sumCost(events []pipeline.SessionEvent) (cost, avoided usage.CostSum) {
	for i := range events {
		m := moneyOf(&events[i])
		cost.Add(m.cost)
		avoided.Add(m.avoided)
	}
	return cost, avoided
}

// ActiveSession returns the most recently updated session ID.
// Used for outbound correlation when no explicit session ID is available.
func (s *Store) ActiveSession() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.activeID == "" {
		return ""
	}
	sess, ok := s.sessions[s.activeID]
	if !ok || s.isExpired(sess, time.Now()) {
		return ""
	}
	return s.activeID
}

// Rekey renames a session from oldID to newID, preserving all events.
// Used to merge the bootstrap "default" session into the server-assigned
// contextId after the backend response reveals it, so events recorded
// during the request phase (under "default") and subsequent turns (under
// the real contextId) land in the same bucket.
//
// Safe to call when oldID does not exist (no-op) or newID already exists
// (no-op — preserves the existing newID entry). If oldID was the active
// session, activeID is updated to newID.
//
// Assumes single-tenant, no concurrent conversations per pod. In a
// multi-tenant deployment two in-flight first-turn requests could both
// land under "default"; rekeying the first to arrive would strand the
// second's events. Call sites are expected to guard against that.
func (s *Store) Rekey(oldID, newID string) {
	if oldID == newID || oldID == "" || newID == "" {
		return
	}
	if len(newID) > MaxSessionIDLen {
		// Two long IDs sharing a prefix would collide on the same truncated key,
		// silently turning the second Rekey into a no-op. Log so that's diagnosable.
		slog.Warn("session: newID truncated for rekey", "origLen", len(newID), "maxLen", MaxSessionIDLen)
		newID = newID[:MaxSessionIDLen]
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[oldID]
	if !ok {
		return
	}
	if _, exists := s.sessions[newID]; exists {
		return
	}

	sess.ID = newID
	s.sessions[newID] = sess
	delete(s.sessions, oldID)
	if s.activeID == oldID {
		s.activeID = newID
	}
	// Retrofit already-recorded events with the new session ID so snapshot
	// reads stay consistent with live-streamed events after a rekey.
	for i := range sess.Events {
		sess.Events[i].SessionID = newID
	}
}

// Cleanup removes expired sessions. Safe for concurrent use.
func (s *Store) Cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(time.Now())
}

func (s *Store) cleanupLocked(now time.Time) {
	for id, sess := range s.sessions {
		if s.isExpired(sess, now) {
			delete(s.sessions, id)
			if s.activeID == id {
				s.activeID = ""
			}
		}
	}
}

func (s *Store) evictOldestLocked() {
	var oldestID string
	var oldestTime time.Time
	for id, sess := range s.sessions {
		if id == s.activeID {
			continue
		}
		if oldestID == "" || sess.UpdatedAt.Before(oldestTime) {
			oldestID = id
			oldestTime = sess.UpdatedAt
		}
	}
	if oldestID == "" {
		// All sessions are the active session — evict it as last resort.
		oldestID = s.activeID
		s.activeID = ""
	}
	if oldestID != "" {
		delete(s.sessions, oldestID)
	}
}

func (s *Store) isExpired(sess *entry, now time.Time) bool {
	// ttl <= 0 means sessions never expire on time, which is the default. Time-based
	// expiry read as data loss: traffic you were looking at vanished because you
	// stepped away, not because anything overflowed.
	//
	// This used to add "and nothing about it was load-bearing either — memory is
	// bounded by maxSessions x maxEvents". That is no longer true: maxEvents is unset
	// by default, so the product is unbounded and one session that never goes idle has
	// no ceiling at all. The default stands on the first reason alone — a clock is the
	// wrong tool for bounding memory — and the tools for bounding it are an explicit
	// maxEvents or an explicit ttl. See config.SessionConfig.Limits for the whole
	// picture; it is where the defaults are resolved and where they are argued.
	//
	// What a ttl buys is hygiene: raw prompts not lingering in memory. Still available
	// by setting session.ttl explicitly.
	if s.ttl <= 0 {
		return false
	}
	return now.Sub(sess.UpdatedAt) > s.ttl
}

// publishLocked fans out event to every current subscriber. Must be called
// with s.mu held. Sends are non-blocking — a full channel increments the
// subscriber's drop counter and discards the event.
//
// Performance note: fan-out runs under s.mu, which serializes Append on the
// subscriber list length. This is fine for the debug API's expected load
// (O(1) live consumers: one abctl instance + maybe a curl tail). If the
// store ever needs to support many concurrent subscribers, refactor this
// to an unlocked broadcast (snapshot the slice under the lock, send outside).
func (s *Store) publishLocked(event pipeline.SessionEvent) {
	for _, sub := range s.subscribers {
		select {
		case sub.ch <- event:
		default:
			atomic.AddUint64(&sub.drops, 1)
		}
	}
}

// logAppended emits a structured DEBUG line so operators can observe session
// state evolution. Fields are chosen to cover the data captured by all four
// record helpers — extension payloads themselves are intentionally omitted
// since the parsers already log them.
func logAppended(sessionID string, e *pipeline.SessionEvent) {
	attrs := []any{
		"sessionId", sessionID,
		"direction", e.Direction.String(),
		"phase", e.Phase.String(),
	}
	if e.Host != "" {
		attrs = append(attrs, "host", e.Host)
	}
	if e.StatusCode != 0 {
		attrs = append(attrs, "status", e.StatusCode)
	}
	if e.Duration != 0 {
		attrs = append(attrs, "durationMs", e.Duration.Milliseconds())
	}
	if e.Identity != nil {
		if e.Identity.Subject != "" {
			attrs = append(attrs, "subject", e.Identity.Subject)
		}
		if e.Identity.ClientID != "" {
			attrs = append(attrs, "clientID", e.Identity.ClientID)
		}
		if e.Identity.AgentID != "" {
			attrs = append(attrs, "agent", e.Identity.AgentID)
		}
		if n := len(e.Identity.Scopes); n > 0 {
			attrs = append(attrs, "scopes", n)
		}
	}
	switch {
	case e.A2A != nil:
		attrs = append(attrs, "proto", "a2a", "method", e.A2A.Method)
	case e.MCP != nil:
		attrs = append(attrs, "proto", "mcp", "method", e.MCP.Method)
	case e.Inference != nil:
		attrs = append(attrs, "proto", "inference", "model", e.Inference.Model)
	}
	if e.Error != nil {
		attrs = append(attrs, "errorKind", e.Error.Kind)
	}
	slog.Debug("session: event appended", attrs...)
}
