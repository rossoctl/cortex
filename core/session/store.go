// Package session provides an in-memory session store for correlating
// inbound user intents with outbound tool calls across request boundaries.
// The store is per-pod (AuthBridge sidecar) and does not persist across restarts;
// where a session archive runs, ListSessions continues each session from it (see
// PriorKeeper).
package session

import (
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/model"
)

// DefaultSessionID is used when no explicit A2A SessionID is present and no
// active session exists. This collapses all such requests into one shared session,
// which is correct for single-agent pods but may cause cross-user correlation in
// multi-tenant deployments. Future work: derive session ID from JWT claims.
const DefaultSessionID = "default"

// entry holds the events for one conversation.
type entry struct {
	ID        string
	Events    []model.SessionEvent
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
	// re-created under the same id afterwards starts a NEW counter at 1 — unless a SeqSeeder
	// (the session archive) still holds the id's earlier events, in which case the new counter
	// starts after them. Without one, numbers are unique within one incarnation of a session,
	// not across the id forever.
	//
	// Nothing here can detect that, and nothing here needs to: the store cannot tell a
	// re-created session from a trimmed one. A paging client compares wall-clock time
	// rather than Seq for exactly this reason — see agentop's applyOlderPage. See also
	// model.SessionEvent.Seq.
	nextSeq uint64
	// after is the seq this entry numbers its events after: the highest a SeqSeeder held for the id
	// when the entry was created, 0 when none held any. Unlike nextSeq it never moves. Above 0 the
	// id had history before this entry, and ListSessions asks a PriorKeeper for it.
	after uint64

	// cost and avoided are RUNNING TOTALS over Events, maintained by Append and read by
	// ListSessions. Micros, in the units usage.Counts uses.
	//
	// INCREMENTAL BECAUSE THE ALTERNATIVE WAS A REGRESSION. These were first summed on
	// demand, by decoding every event of every session inside ListSessions. That runs under
	// the read lock, agentop re-fetches /v1/sessions every two seconds, and the event list is
	// UNCAPPED by default (see New) — so a long session put an unbounded number of
	// json.Unmarshal calls on a timer, in front of a lock whose writer side is
	// Store.Append on the proxy's request path. sumTokens beside it is a pointer-deref
	// loop; this was parsing. ledger.Writer.Record hoists its own phase guard above
	// event.Record for exactly this reason.
	//
	// event.CostSum, not two int64s: it saturates rather than wrapping AND records that it
	// did, so a clamped lifetime total arrives labelled instead of as a plausible
	// ~$9.2 trillion. It is the one saturating money accumulator in this codebase; a local
	// copy here would be a second implementation of that rule with the label dropped.
	//
	// MAINTAINED IN LOCKSTEP WITH Events, including on trim: whatever leaves the slice is
	// subtracted, so these stay equal to sumCost(Events) — the invariant
	// TestAppend_RunningTotalsMatchAFullRecomputation exists to hold. That is what keeps
	// the COST column scoped exactly like the TOKENS column beside it, which is the claim
	// SessionSummary.CostMicros makes. ListSessions adds the history before this entry on top
	// where a PriorKeeper has it (withPriorLocked); these running totals stay the entry's own.
	cost    event.CostSum
	avoided event.CostSum
	// units counts the priced events behind cost by the unit they were priced in, "" for USD.
	// Maintained with cost, on append and on trim, and read by SessionSummary.Currencies — a
	// count rather than a set because a trim has to know when the LAST event in a unit leaves.
	units map[string]int

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

	// Title is this session's name and titleRank the rank that named it, folded by Append and
	// read by ListSessions. See the rank constants in title.go for the ranks themselves.
	//
	// FIRST-WINS, EXCEPT THAT A /rename ALWAYS OVERRIDES: Append replaces these when a candidate
	// ranks strictly better, and additionally when an equally-ranked candidate is a rename. So
	// ordinary conversation names a session by its FIRST prompt and the name then stays put,
	// while an explicit /rename can rename it at any time, repeatedly.
	//
	// FIRST-WINS ACROSS EVENTS, LAST-WINS WITHIN ONE, and the two rules meeting here is deliberate
	// rather than an inconsistency to reconcile. titleCandidate scans one event's messages in
	// REVERSE, so within a single inference request — which carries the whole conversation — the
	// newest user message is the ask. The fold then keeps whichever event named the session first.
	// A session is therefore named by its first prompt and keeps that name, while the messages of
	// any one request are read newest-first. TestSessionTitle_LastUserMessage asserts both halves
	// side by side, because reading either one alone suggests the other.
	//
	// AN EXTREMUM, NOT A SUM, so this is in context's class below and not cost's: nothing is
	// accumulated, so a trim has nothing to subtract, and it is NOT maintained in lockstep with
	// Events. A /rename therefore OUTLIVES the event that carried it — evicting that event leaves
	// the name in place, where the previous recompute-per-call demoted it back to later prose.
	// That is the intended behavior (a user who names a session expects the name to stick) and it
	// is the same property TestAppend_PromptContextSurvivesATrim pins for context.
	//
	// FOLDED AT APPEND BECAUSE THE READ PATH CANNOT AFFORD IT. Picking the title per call means
	// walking the retained events and their message CONTENT — the largest payload here — under
	// s.mu.RLock(), whose writer side is Append on the proxy's request path with caller-supplied
	// content. Measured on a 190KB message of nested <system-reminder> opens: 593ms of lock hold,
	// and 38.1s with a ceiling on how many events are examined, since bounding the count does not
	// bound the cost of any one of them. Folding here is two integers and a string, and leaves the
	// read path a field read — see ListSessions, and stripReminders for the quadratic part.
	Title     string
	titleRank int

	// agents is the first label each CLIENT sent into this session, one entry per client —
	// recognised coding agents and, since #1214, the clients the parser could not name, which
	// carry known: false and are preferred only after every recognised one. See
	// SessionSummary.Agent and sessionAgent. First-wins and not shed on trim, like Title.
	agents []sessionAgent

	// context is the CONTEXT gauge's answer for this session: the main-agent turn that RANKS
	// HIGHEST under the rule's total order, in prompt tokens — the LATEST such turn where the
	// client states its role, the one with the MOST MESSAGES where it does not. Maintained by
	// Append and read by ListSessions; see model.PromptContextFold for the order itself.
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
	context model.PromptContextFold

	// inferenceHost is SessionSummary.InferenceHost, folded by Append and read by ListSessions.
	// A LATEST VALUE, NOT A SUM, so in context's class above: a trim has nothing to subtract, and
	// the request that set it is the newest of its kind, which is the last a trim would take.
	inferenceHost string

	// procs is each process that NAMED this session through its own header, with when it first
	// and last did (applyProcs); owner is the agent whose header named it first (applyOwner).
	// Neither is shed on trim. lastRequestAt is the newest request's time, for Link.
	procs         map[string]ProcSpan
	owner         string
	lastRequestAt time.Time

	// lastIntent and lastInference are the Seq of the newest event that model.IsIntent and
	// model.IsInferenceRequest accept, 0 for none. ViewFacts reads them. LATEST VALUES, in
	// inferenceHost's class: a trim does not change them, and ViewFacts gives FactNone when the
	// event left.
	lastIntent, lastInference uint64
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
	Record(sessionID string, e *model.SessionEvent)
}

// Store is an in-memory, per-pod session store. It is safe for concurrent use.
type Store struct {
	// now stamps CreatedAt and UpdatedAt and is what expiry is judged against. nil means
	// time.Now; see clock.
	now func() time.Time

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

// New creates a Store with the given TTL, per-session event limit, and max
// concurrent sessions. maxEvents <= 0 keeps every event a session produces, which
// is what the binaries pass unless session.max_events is configured; maxSessions
// <= 0 likewise keeps every session. A background goroutine runs cleanup every
// TTL/2.
// Call Close() during graceful shutdown to stop the background goroutine.
func New(ttl time.Duration, maxEvents int, maxSessions int, opts ...Option) *Store {
	s := &Store{
		sessions:    make(map[string]*entry),
		ttl:         ttl,
		maxEvents:   maxEvents,
		maxSessions: maxSessions,
		stop:        make(chan struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}
	// No reaper when nothing can expire: with ttl <= 0 the interval below clamps to one
	// second and Cleanup would wake every second forever to find nothing.
	if ttl > 0 {
		go s.backgroundCleanup()
	}
	return s
}

// Option configures a Store at construction time.
type Option func(*Store)

// WithClock replaces the clock sessions are stamped and expired by. No production
// caller: the README demo runs its whole stack on one shifted clock, and a store left on
// the real one stamps UpdatedAt weeks away from the clock the TUI measures it against.
func WithClock(fn func() time.Time) Option {
	return func(s *Store) { s.now = fn }
}

// clock is the instant sessions are stamped and expired by. See Store.now.
func (s *Store) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
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
func (s *Store) Append(sessionID string, event model.SessionEvent) {
	s.append(sessionID, nil, event, nil)
}

// CaptureRecorder is optionally implemented by a Recorder that takes the request captures
// AppendCapture hands over (#1494). RecordCapture is called in place of Record, under the
// store's write lock, with c nil for an event that has none. The store never keeps c: raw
// bodies are megabytes, and the store keeps every event in memory.
type CaptureRecorder interface {
	RecordCapture(sessionID string, e *model.SessionEvent, c *model.RequestCapture)
}

// TakesCaptures reports whether some recorder is a CaptureRecorder, so a listener can skip copying
// bodies nobody keeps. False on a nil store.
func (s *Store) TakesCaptures() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.recorders {
		if _, ok := r.(CaptureRecorder); ok {
			return true
		}
	}
	return false
}

// AppendCapture is Append for a request event with its capture c, which only a
// CaptureRecorder receives.
func (s *Store) AppendCapture(sessionID string, event model.SessionEvent, c *model.RequestCapture) {
	s.append(sessionID, nil, event, c)
}

// Bucket is the session an event was appended to. It follows that session through a
// Rekey or an Adopt, which rename a session rather than replace it, and names nothing once
// the store has evicted or expired it.
type Bucket struct{ e *entry }

// AppendBucket is Append, returning the bucket the event went into.
func (s *Store) AppendBucket(sessionID string, event model.SessionEvent) *Bucket {
	return &Bucket{e: s.append(sessionID, nil, event, nil)}
}

// AppendTrailing appends an event that ends something b's session started earlier — a
// tunnel's close, recorded when the tunnel ends. It is not that session speaking now, so
// it neither makes the session active nor refreshes its UpdatedAt. It reports false, and
// records nothing, when b's session has been evicted or has expired: the event would pair
// with nothing, and re-creating the session to hold it could evict another.
func (s *Store) AppendTrailing(b *Bucket, event model.SessionEvent) bool {
	if b == nil {
		return false
	}
	return s.append("", b, event, nil) != nil
}

// AppendPair appends first and then second to the named session under one lock
// acquisition, so no concurrent append can land between them, and returns the bucket
// both went into. agentop folds a bridged tunnel's open row into the event directly
// after it in the session, and two separate appends could be split by any traffic in
// between.
func (s *Store) AppendPair(sessionID string, first, second model.SessionEvent) *Bucket {
	return s.AppendPairCapture(sessionID, first, second, nil)
}

// AppendPairCapture is AppendPair with second's capture c; see AppendCapture.
func (s *Store) AppendPairCapture(sessionID string, first, second model.SessionEvent, c *model.RequestCapture) *Bucket {
	p1, p2 := prepareAppend(&first), prepareAppend(&second)
	p2.capture = c
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.appendLocked(sessionID, nil, first, p1)
	// e.ID, not sessionID: the first append has already capped the id and followed any
	// adoption, and the second must land in the same entry.
	s.appendLocked(e.ID, nil, second, p2)
	return &Bucket{e: e}
}

// appendPrep is what an append computes from the event alone, before taking the lock.
// See prepareAppend for why each part is hoisted out of the critical section.
type appendPrep struct {
	money     eventMoney
	titleRank int
	titleText string
	agentName string
	// capture is the event's request capture, for CaptureRecorders only; see AppendCapture.
	capture *model.RequestCapture
}

// append is Append when b is nil, and AppendTrailing when it is not. It returns the
// session the event went into, nil when AppendTrailing refused it.
func (s *Store) append(sessionID string, b *Bucket, event model.SessionEvent, c *model.RequestCapture) *entry {
	prep := prepareAppend(&event)
	prep.capture = c
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(sessionID, b, event, prep)
}

// prepareAppend computes everything an append needs from the event alone. It runs
// before the store's lock is taken.
func prepareAppend(event *model.SessionEvent) appendPrep {
	// BEFORE THE LOCK. This is a json.Unmarshal of the event's plugin map, the most
	// expensive thing on this path, and it touches no store state — so it has no business
	// inside a critical section that blocks every reader and every other appender. Doing it
	// here is also what lets ListSessions read two integers instead of decoding every event
	// of every session on agentop's two-second poll; see entry.cost.
	money := moneyOf(event)

	// BEFORE THE LOCK FOR THE SAME REASON, and unlike sess.context.Add below — see the comment
	// there, which correctly declines to hoist a phase check and a couple of int adds. This is not
	// that: titleCandidate walks every message of the event and, for each one whose rank is worth
	// settling, scans its CONTENT (stripReminders plus the tag extractions). That is the largest
	// payload on this path and it is attacker-supplied.
	//
	// MEASURED BOTH WAYS, because "it scans content" is not on its own an argument. One goroutine
	// appending the 190KB nested-open fixture in a loop, against a concurrent ListSessions:
	//
	//   candidate extracted inside the lock:   reader mean 259µs, worst 2.51ms
	//   candidate extracted here (this line):  reader mean 319ns, worst 28.1µs
	//
	// ~810x on the mean. Note what did NOT change: serial Append stayed at ~578µs, because the scan
	// is O(content) and cannot be made cheap — the work moved off the critical section rather than
	// going away, which is the whole claim. It belongs here because it is a pure function of the
	// local event and touches no store state.
	//
	// WHY THAT MATTERS HERE RATHER THAN BEING A MICRO-OPTIMISATION: Append is called synchronously
	// from the reverseproxy, forwardproxy and extproc request paths, so this lock sits in front of
	// live proxied traffic, and the writer side excludes every reader and every other appender.
	//
	// Only the candidate's EXTRACTION is hoisted, which is the expensive half; the rank comparison
	// and sanitizeTitle both stay under the lock, for the reason given at the fold site below.
	titleRank, titleText := titleCandidate(event)
	return appendPrep{money: money, titleRank: titleRank, titleText: titleText, agentName: event.Client.AffinityName()}
}

// appendLocked is append's critical section. s.mu must be held; prep is what
// prepareAppend computed from event before the lock was taken.
func (s *Store) appendLocked(sessionID string, b *Bucket, event model.SessionEvent, prep appendPrep) *entry {
	if len(sessionID) > MaxSessionIDLen {
		sessionID = sessionID[:MaxSessionIDLen]
	}
	money, titleRank, titleText, agentName := prep.money, prep.titleRank, prep.titleText, prep.agentName

	now := s.clock()
	var sess *entry
	if b != nil {
		if s.sessions[b.e.ID] != b.e || s.isExpired(b.e, now) {
			return nil
		}
		sess, sessionID = b.e, b.e.ID
	} else {
		var ok bool
		sess, ok = s.sessions[sessionID]
		if !ok {
			seed := s.archivedSeqLocked(sessionID)
			sess = &entry{
				ID:        sessionID,
				CreatedAt: now,
				// After whatever the archive already numbered under this id; see SeqSeeder.
				nextSeq: seed,
				after:   seed,
				// NOT THE ZERO VALUE: rankRename is 0, so a zero-valued titleRank would claim this
				// session had already been renamed and no candidate could ever beat it — the first
				// prose message would be unnameable. rankNone is the "nothing has named it" rank.
				titleRank: rankNone,
			}
			s.sessions[sessionID] = sess
			s.entryStartedLocked(sessionID, seed)
		}
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
	if money.priced || money.avoided > 0 {
		if sess.units == nil {
			sess.units = make(map[string]int, 1)
		}
		sess.units[money.unit]++
	}
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
	// one comparison (model.PromptTokensOf sums three fields), a Round(0) that clears a
	// monotonic reading, and — once the fold already holds a figure — better()'s time.Time
	// Equal/After chain: tens of nanoseconds, no allocation, NO DECODE. Splitting it to hoist the
	// extraction would add an exported type for plumbing alone and save nothing measurable.
	sess.context.Add(&event)
	// AND THE TITLE — but NOT on the same terms as context.Add: the candidate was extracted ABOVE
	// THE LOCK, because unlike context.Add it scans message content. Only the comparison is here.
	//
	// Read from &event, the local parameter, so the fold outlives the events it was read from — see
	// entry.Title, and entry.context just above for why sourcing the candidate from the stored slice
	// is what breaks that property.
	//
	// The rule itself, and why its sanitizeTitle is safe under this lock, are applyTitle's: it is
	// shared with SummaryFold so an archived session is titled exactly as a resident one.
	sess.titleRank, sess.Title = applyTitle(sess.titleRank, sess.Title, titleRank, titleText)
	sess.agents = applyAgent(sess.agents, sessionID, agentName, event.Client)
	// A field compare and a string header copy, so under the lock like context.Add. From &event,
	// the local parameter, for the reason given there.
	if h := inferenceHostOf(&event); h != "" {
		sess.inferenceHost = h
	}
	// An event of neither kind must not change the facts.
	if model.IsIntent(&event) {
		sess.lastIntent = event.Seq
	}
	if model.IsInferenceRequest(&event) {
		sess.lastInference = event.Seq
	}
	sess.procs = applyProcs(sess.procs, sessionID, &event)
	sess.owner = applyOwner(sess.owner, sessionID, agentName, &event)
	if event.Phase == model.SessionRequest && event.At.After(sess.lastRequestAt) {
		sess.lastRequestAt = event.At
	}
	if b == nil {
		sess.UpdatedAt = now
		s.activeID = sessionID
	}

	logAppended(sessionID, &event)
	s.publishLocked(event)

	// Side-channel aggregation (e.g. /v1/usage). Runs under the write lock, so
	// a Recorder must be cheap and must not re-enter the store. Unlike
	// subscribers this cannot drop: a dropped event silently skews a cumulative
	// counter, where a dropped stream frame only costs one client one row.
	for _, r := range s.recorders {
		if cr, ok := r.(CaptureRecorder); ok {
			cr.RecordCapture(sessionID, &event, prep.capture)
		} else {
			r.Record(sessionID, &event)
		}
	}

	if p, ok := planTrim(sess.Events, s.maxEvents); ok {
		// NO DECODE HERE. The figures come off the parallel money slice, so the only work
		// this adds to the critical section is integer subtraction — see entry.money for
		// what the first version did instead.
		//
		// Sub, not Add(-x): see event.CostSum.Sub for the math.MinInt64 case it exists for.
		for i, m := range sess.money {
			if p.keeps(i) {
				continue
			}
			sess.cost.Sub(m.cost)
			sess.avoided.Sub(m.avoided)
			if m.priced || m.avoided > 0 {
				if sess.units[m.unit]--; sess.units[m.unit] <= 0 {
					delete(sess.units, m.unit)
				}
			}
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
	return sess
}

// isIntentEvent matches the SessionView.LastIntent predicate: an
// inbound A2A request event. IBAC and any future intent-aware
// guardrail call LastIntent and need the most recent such event to
// survive FIFO eviction. It calls model.IsIntent, so the eviction
// pin and the LastIntent fact of ViewFacts use one rule.
func isIntentEvent(e model.SessionEvent) bool {
	return model.IsIntent(&e)
}

// trimEventsPinIntent reduces events to len <= maxEvents while
// preserving the most-recent intent event, even when that intent
// sits in the prefix that FIFO would normally evict. All other
// events evict in chronological order; the protected intent stays
// at its original index, leaving a temporal gap between it and the
// first non-evicted event after it. That gap is visible in
// /v1/sessions and agentop as a discontinuity in the timeline, which
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
func planTrim(events []model.SessionEvent, maxEvents int) (trimPlan, bool) {
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

// AddRecorder registers a Recorder. Safe at any time: every reader of the list holds the
// store's lock, and so does this. A recorder added late misses the events before it.
func (s *Store) AddRecorder(r Recorder) {
	if r != nil {
		s.mu.Lock()
		s.recorders = append(s.recorders, r)
		s.mu.Unlock()
	}
}

// View returns a read-only snapshot of the session's events.
// Returns nil if the session doesn't exist or is expired.
func (s *Store) View(sessionID string) *model.SessionView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, _ := s.viewLocked(sessionID)
	return v
}

// viewLocked is View's body, and the entry it copied. Both are nil for no entry or an expired
// one. The caller holds s.mu.
func (s *Store) viewLocked(sessionID string) (*model.SessionView, *entry) {
	sess, ok := s.sessions[sessionID]
	if !ok {
		return nil, nil
	}
	if s.isExpired(sess, s.clock()) {
		return nil, nil
	}

	events := make([]model.SessionEvent, len(sess.Events))
	copy(events, sess.Events)
	return &model.SessionView{ID: sessionID, Events: events}, sess
}

// ViewFacts is View and the session's facts, read under one lock, so the two agree. Each
// present fact's Event points into the returned view, as SessionView.LastIntent does. When View
// returns nil (no entry, or an expired one), the view is nil and both facts are FactNone.
func (s *Store) ViewFacts(sessionID string) (*model.SessionView, model.SessionFacts) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	v, sess := s.viewLocked(sessionID)
	if v == nil {
		return nil, model.SessionFacts{LastIntent: model.SessionFact{State: model.FactNone}, LastInference: model.SessionFact{State: model.FactNone}}
	}
	return v, model.SessionFacts{LastIntent: factAt(v.Events, sess.lastIntent), LastInference: factAt(v.Events, sess.lastInference)}
}

// factAt finds the event with this seq in events, which rise in seq. A seq of 0, or one that a
// session.max_events trim removed, gives FactNone.
func factAt(events []model.SessionEvent, seq uint64) model.SessionFact {
	if seq == 0 {
		return model.SessionFact{State: model.FactNone}
	}
	i := sort.Search(len(events), func(i int) bool { return events[i].Seq >= seq })
	if i < len(events) && events[i].Seq == seq {
		return model.SessionFact{Event: &events[i], State: model.FactPresent}
	}
	return model.SessionFact{State: model.FactNone}
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
func (s *Store) ViewTail(sessionID string, limit int) *model.SessionView {
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
func (s *Store) ViewPage(sessionID string, before uint64, limit int) *model.SessionView {
	if limit <= 0 {
		limit = 1
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	sess, ok := s.sessions[sessionID]
	if !ok {
		return nil
	}
	if s.isExpired(sess, s.clock()) {
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
	events := make([]model.SessionEvent, len(page))
	copy(events, page)

	view := &model.SessionView{ID: sessionID, Events: events}
	if start > 0 || end < len(sess.Events) {
		view.TotalEvents = len(sess.Events)
		view.OldestSeq = sess.Events[0].Seq
	}
	return view
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
	if !ok || s.isExpired(sess, s.clock()) {
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
	s.rekeyLocked(oldID, newID)
}

// rekeyLocked is Rekey's body, shared with Adopt. Reports whether it renamed anything.
func (s *Store) rekeyLocked(oldID, newID string) bool {
	sess, ok := s.sessions[oldID]
	if !ok {
		return false
	}
	if _, exists := s.sessions[newID]; exists {
		return false
	}
	// A target the archive holds history for counts as existing. Its events on disk are
	// numbered from 1 and so are the bucket's, so a merge would give one session two events
	// of each low seq — the same reason a resident target refuses. See SeqSeeder.
	if s.archivedSeqLocked(newID) > 0 {
		return false
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
	// Every rename tells the recorders that keep per-session state, so that state follows the
	// session: Adopt's pending bucket → session and Rekey's A2A default → contextId alike. Here
	// rather than in adoptLocked because both reach this, and only a rename that happened may
	// be announced.
	for _, r := range s.recorders {
		if rk, ok := r.(Rekeyer); ok {
			rk.Rekeyed(oldID, newID)
		}
	}
	return true
}

// removeLocked drops id's entry and every map that names it: its owner, its adoption records and
// the active-session mark. Eviction, ttl expiry and Delete all remove a session through it. s.mu
// held.
func (s *Store) removeLocked(id string) {
	delete(s.sessions, id)
	if s.activeID == id {
		s.activeID = ""
	}
}

// Cleanup removes expired sessions. Safe for concurrent use.
func (s *Store) Cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(s.clock())
}

func (s *Store) cleanupLocked(now time.Time) {
	for id, sess := range s.sessions {
		if s.isExpired(sess, now) {
			s.removeLocked(id)
			s.evictedLocked(id)
		}
	}
}

// evictedLocked tells every Evicter the store dropped id. s.mu held.
func (s *Store) evictedLocked(id string) {
	for _, r := range s.recorders {
		if e, ok := r.(Evicter); ok {
			e.Evicted(id)
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
	}
	if oldestID != "" {
		s.removeLocked(oldestID)
		s.evictedLocked(oldestID)
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

// logAppended emits a structured DEBUG line so operators can observe session
// state evolution. Fields are chosen to cover the data captured by all four
// record helpers — extension payloads themselves are intentionally omitted
// since the parsers already log them.
func logAppended(sessionID string, e *model.SessionEvent) {
	attrs := []any{
		// A session id is an attribution key the operator needs in the log to
		// answer "which session was this bucket?" — validated at the source
		// before IDHeaderFrom returns it, and the headers the query targets
		// (Authorization, Cookie) are read elsewhere.
		"sessionId", sessionID,
		"direction", e.Direction.String(),
		"phase", e.Phase.String(),
	}
	if e.Host != "" {
		attrs = append(attrs, "host", model.SanitizeLogValue(e.Host))
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
	// The session id in attrs is an attribution key — see the comment on it
	// where attrs is built.
	// codeql[go/clear-text-logging]
	slog.Debug("session: event appended", attrs...)
}
