package session

import (
	"maps"
	"sort"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
)

// SessionSummary is a per-session view for list endpoints: counters, money figures and
// timestamps rather than the event backlog.
//
// "METADATA-ONLY" WOULD NOW BE WRONG, and the phrase used to be here: Title carries up to
// maxTitleLen runes of VERBATIM USER PROMPT. This is not a new exposure — /v1/sessions/{id}
// returns whole request and response bodies on the same unauthenticated listener, which the
// session API documents as its trust model — but a reader deciding how this endpoint may be
// exposed should not be told by this comment that no content reaches it. Note in particular
// core/config/config.go's tls_bridge guard, which forces the session API to loopback on the
// rationale that it "may carry decrypted request/response bodies": that guard keys off the
// bridge, not off this field, and nothing here should be read as having considered whether
// it wants widening.
type SessionSummary struct {
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	EventCount int       `json:"eventCount"`
	// Title names this session from its own events, absent when nothing in them named it.
	// Folded by Append; see entry.Title for the rule and titleFrom for the ranks.
	//
	// A SUGGESTION, NOT AN IDENTIFIER: derived from the events and nothing addresses a
	// session by it.
	//
	// FIRST-WINS, EXCEPT THAT A /rename ALWAYS OVERRIDES. Two consequences a client may rely
	// on, both the opposite of what this field promised while it was recomputed per call:
	// ordinary conversation does NOT re-title the session on every turn, and a /rename is not
	// lost when the event carrying it is evicted.
	//
	// omitempty on the standing rule CostMicros states below: an unknown value must not
	// render as a real one, so an absent key is what a client should expect for a session the
	// events never named.
	//
	// ABSENT IS A DISPLAYED STATE NOW, not just an unread one. agentop renders this in its TITLE
	// column as a FALLBACK: it prefers a title harvested from Claude Code's transcripts and
	// reaches for this one only when that harvest named nothing. So the sessions where this
	// field decides what an operator sees are exactly those with no transcript on the machine
	// running agentop — an agent that routes through the proxy without writing Claude Code
	// transcripts is the case that motivated it. Do not assume a change here is invisible.
	Title string `json:"title,omitempty"`
	// Agent is the client this session belongs to, as event.AgentLabel names it, which is the
	// label agent= matches: the affinity owner's when one claimed the session, else the first
	// recognised agent's, else — #1214 — the first UNRECOGNISED client's, which is its raw
	// User-Agent folded by pipeline.AgentName. Absent for the default and pending buckets, which
	// are no one agent's, for a claimed session its owner has sent nothing into, and for a session
	// whose every event carried no User-Agent at all, since "unknown" is a reserved bucket rather
	// than a name (see pipeline.EventClient.Label).
	//
	// NOT A STATEMENT THAT THE AGENT IS RECOGNISED, and a consumer that reads it as one is the
	// one thing this field's widening can surprise. pipeline.IsKnownAgent(agent) still answers
	// correctly — that is what it exists for, and agentop's AGENTS pane uses it to pool every
	// label it rejects under one row — but presence alone no longer implies it. The alternative
	// was a session that nothing in the list could attribute to any program, which is what #1214
	// was filed about; a spoofable label is the same axis the usage ring and the ledger already
	// key on, never an identity.
	Agent       string `json:"agent,omitempty"`
	TotalTokens int    `json:"totalTokens,omitempty"` // sum of Inference.TotalTokens across response events
	// CostMicros is what this session's events cost, in millionths of a dollar, summed from
	// the records the session itself holds — where a session archive runs, including those from
	// before this entry.
	//
	// FROM THE EVENTS, not from the ledger, and that is the only honest source available: the
	// ledger's row key is (endpoint, model, agent, provenance) with no session dimension by
	// design, so it cannot answer "what did this session cost" at all. The live ring can, but
	// only over its rolling window, which would put a partial figure beside the LIFETIME
	// TotalTokens above and understate the row by whatever fell off the ring.
	//
	// SCOPED TO WHAT THE STORE STILL HOLDS, PLUS — WHERE A SESSION ARCHIVE RUNS — THE SESSION'S
	// HISTORY FROM BEFORE THIS ENTRY (see PriorKeeper). So with an archive it continues across a
	// restart, and without one it resets; a ledger window always continues. Both are correct:
	// this is "the cost of the events in this session", the ledger is "the cost of the day". A
	// client showing them together must not present one as a check on the other.
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
	// Currencies names the units CostMicros and AvoidedMicros are in, sorted, dollars spelled
	// "USD" — and is ABSENT when every priced event was in dollars, which is every deployment
	// with no pricing unit configured, so those serve the bytes they always have.
	//
	// MORE THAN ONE ENTRY MEANS THE FIGURES ARE A CROSS-UNIT SUM and must not be rendered as a
	// single amount. They are still summed here rather than split, because the running totals
	// predate units and a client that ignores this field must keep reading what it always read;
	// splitting is the job of a surface that can show two figures. Follows the events through a
	// trim, like the totals themselves; see entry.units.
	Currencies []string `json:"currencies,omitempty"`
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
	// store still holds (plus, where a session archive runs, the session's history from before
	// this entry), so a trim sheds exactly what left the slice, which is why CostMicros can
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
	// CONTINUES ACROSS A RESTART WHERE A SESSION ARCHIVE RUNS, like CostMicros, and resets where
	// none does, because the store is in-memory per-pod. A client that has been watching longer
	// than this proxy has been up may hold a larger figure legitimately — see
	// pipeline.MergePromptContext, which is how the two combine.
	PromptContext *pipeline.PromptContext `json:"promptContext,omitempty"`

	// InferenceHost is where this session's inference traffic goes: the host of its most recent
	// request that carried an inference parse, as the event recorded it. After a redirect that is
	// the server it was sent to, whether or not that server answered, and not the host the client
	// named (see SessionEvent.RequestedHost).
	// Absent for a session with no inference traffic — and for one resumed from an archive written
	// before this field existed, until its next inference request, where absent means unknown:
	// that history was folded without it (see foldJSON.InferenceHost).
	//
	// LATEST WINS, AND ONLY AN INFERENCE REQUEST MOVES IT. A CONNECT tunnel row, an MCP call, any
	// response or a denial leaves it where it was: live traffic interleaves those between turns,
	// and a field they cleared would read "no inference traffic" on a session that has plenty. A
	// denial carries its request's parse but went nowhere, so its host is not where anything goes.
	//
	// GENERIC, NOT THE ROUTER'S: it says where a session's inference goes with or without the
	// inference-router. agentop names a session's server from it, which is why the router refuses
	// two servers on one host. A host[:port] as the request carried it.
	//
	// CONTINUES ACROSS A RESTART WHERE A SESSION ARCHIVE RUNS, like Title: the archive's fold
	// persists it, and a resumed session reports it until its own next inference request.
	InferenceHost string `json:"inferenceHost,omitempty"`

	// InferenceHostFromHistory is true when InferenceHost came from the session's history before
	// this store entry — the archive's fold — because no request the entry holds has set it: a
	// session resumed after a restart, or re-created after an eviction, that has sent no inference
	// request since. Absent otherwise, and on a row only the archive holds (Resident false), all of
	// whose figures are history.
	//
	// WHAT IT TELLS A CLIENT: the host is the archive's word, from before this process, and not a
	// request this process saw. Whether something still holds the session there is the router's
	// business: on a local install it keeps its pins in the plugin store, which a restart keeps, and
	// agentop counts such a session as staying; a process with no plugin store keeps them in memory
	// and forgets them on restart. InferenceHost itself is unchanged by this: it still says where
	// the session's inference last went.
	InferenceHostFromHistory bool `json:"inferenceHostFromHistory,omitempty"`

	// Resident is false on a row the session archive served because the store no longer holds
	// the session (GET /v1/sessions?archived=true), and absent otherwise. A POINTER because
	// omitempty on a bool drops exactly the false the wire needs; absent then means "resident",
	// so a client reading a proxy that predates the archive sees every row as it always did.
	Resident *bool `json:"resident,omitempty"`

	// Procs are the processes that named this session through their own session header,
	// "<pid>.<start>", sorted. Absent when none did, and on a summary from before the field.
	Procs []string `json:"procs,omitempty"`
	// LastRequestAt is the time of the newest request this session holds, zero when it holds
	// none. On a pending bucket it is what Link compares with a process's claims.
	LastRequestAt time.Time `json:"lastRequestAt,omitzero"`
	// LinkedTo, on a pending bucket, is the sessions its process named that OwnerAt gives a
	// call of the bucket, each with the span of its naming requests (Link says how close that
	// is); LinkedAll says every request in the bucket falls before one of them. Set by Link when
	// the list is read, never stored. agentop folds a fully linked bucket that links to one
	// session into it and lists any other as its own row.
	LinkedTo  []PendingLink `json:"linkedTo,omitempty"`
	LinkedAll bool          `json:"linkedAll,omitempty"`

	// procSpans is Procs with each process's span, for Link; not on the wire.
	procSpans map[string]ProcSpan
}

// sessionAgent is one client's first label in a session.
//
// name is how the agents slice is KEYED, so each client contributes one entry: a recognised
// agent's canonical AffinityName, and — since #1214 — an unrecognised client's own label, which
// is the only name it has. known is what tells the two apart, and it has to be a field rather
// than a pipeline.IsKnownAgent(name) call at the point of use: name is an AffinityName, which
// reads an agent's canonical name out of a COMMENT (Claude Code's WebFetch), so the recognised
// entry's name and its label can disagree, and only the producer knows which rule made it.
type sessionAgent struct {
	name, label string
	known       bool
}

// agentLabel is the first label the agent named name sent into this session, or "".
func (e *entry) agentLabel(name string) string {
	return agentLabelIn(e.agents, name)
}

// ListSessions returns summaries for every non-expired session. Order is
// UpdatedAt descending (most recent first). Safe for concurrent use.
func (s *Store) ListSessions() []SessionSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := s.clock()
	out := make([]SessionSummary, 0, len(s.sessions))
	for id, sess := range s.sessions {
		if s.isExpired(sess, now) {
			continue
		}
		sum := SessionSummary{
			ID:         id,
			CreatedAt:  sess.CreatedAt,
			UpdatedAt:  sess.UpdatedAt,
			EventCount: len(sess.Events),
			// Read, not computed — like the money figures below and for the same reason,
			// squared. Append folds this; see entry.Title for the rule (first-wins, except
			// that a /rename always overrides), for why the name outlives the event that
			// carried it, and for the measurements that put the fold there rather than here.
			//
			// A FIELD READ, so no content shape can reach it: the 190KB nested-reminder fixture
			// that costs 38.1s to walk reads in 1.6µs. BenchmarkListSessions_Title measures five
			// shapes that agree at ~165ns, which is the standing evidence against a walk
			// reappearing here — they would diverge immediately if one did. This obeys the rule
			// PromptContext states below, which rejects an O(events) walk under the read lock.
			Title: sess.Title,
			Agent: pickAgent(id, sess.owner, nil, sess.agents),
			// Still a walk, and deliberately left as one: it is a pointer deref per event
			// with no allocation, where the money figures below needed a JSON unmarshal.
			TotalTokens: sumTokens(sess.Events),
			// Read, not computed. Append maintains these; see entry.cost.
			CostMicros:    sess.cost.Micros,
			AvoidedMicros: sess.avoided.Micros,
			Currencies:    currenciesOf(sess.units),
			// Either total having clamped makes BOTH figures on this row bounds rather than
			// sums, which is why one flag covers them — the same reasoning
			// usage.Counts.Saturated gives for covering every money field in a Counts.
			Saturated: sess.cost.Saturated || sess.avoided.Saturated,
			Active:    id == s.activeID,
			// Read, not computed — and unlike TotalTokens above this is NOT a walk. Folding the rule
			// here instead would repeat the mistake entry.cost documents: O(events) per session
			// under the read lock, on agentop's two-second poll, in front of a lock whose writer side
			// is Append on the proxy's request path, with maxEvents unset by default.
			PromptContext: sess.context.Publish(),
			// Read, not computed, like Title: Append folds it.
			InferenceHost: sess.inferenceHost,
			Procs:         procsList(sess.procs),
			procSpans:     maps.Clone(sess.procs),
			LastRequestAt: sess.lastRequestAt,
		}
		s.withPriorLocked(&sum, id, sess)
		out = append(out, sum)
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
	// priced says cost is a figure, including a settled zero, and unit is what it is in ("" for
	// USD). Carried so a trim can shed the unit with the figure; see entry.units.
	priced bool
	unit   string
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
		m.priced = true
	}
	m.unit = ev.Currency
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
func sumCost(events []pipeline.SessionEvent) (cost, avoided event.CostSum) {
	for i := range events {
		m := moneyOf(&events[i])
		cost.Add(m.cost)
		avoided.Add(m.avoided)
	}
	return cost, avoided
}

// currenciesOf is SessionSummary.Currencies for one session's unit counts: nil when every priced
// event was in dollars, else every unit sorted, with dollars spelled out.
//
// NIL FOR DOLLARS-ONLY so a deployment with no unit configured serves the bytes it always has; a
// client finding the field absent may print "$" exactly as before. Once a second unit is present
// dollars must be NAMED, or a reader would take the one unit it did see for the whole figure.
func currenciesOf(units map[string]int) []string {
	if len(units) == 0 {
		return nil
	}
	if _, onlyUSD := units[""]; onlyUSD && len(units) == 1 {
		return nil
	}
	out := make([]string, 0, len(units))
	for u := range units {
		if u == "" {
			u = pricing.CurrencyUSD
		}
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}
