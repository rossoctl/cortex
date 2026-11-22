package sessionexport

// Per-turn facts a toolCall record needs that a toolpair.Call does not carry: the client and
// agent role of the asking turn (T5), and the gap before it with that gap's kind (T24, T25).
//
// WHY NOT ON Call. toolpair returns Turns beside Calls precisely so a consumer does not walk
// again — its own doc says doing that twice measured ~30% of a timeline build. These derivations
// are the export's policy, not the pairing's, so they live here and read Turns.

import (
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session/toolpair"
)

// maxCallsPerSession caps how many toolCall records one session contributes (spec §6.4, settled
// normative in §10.1). A fixed cap with a per-session marker record when it is hit, matching the
// export's degrade-loudly posture.
//
// 4,096 IS 4x THE OBSERVED CEILING. Measured 2026-11-22 on a reference archive of 75 sessions and
// 44,325 events: 12,458 paired calls, and the busiest single session held 1,030. The cap exists
// to bound memory on a session far outside that distribution, not to trim normal traffic, so the
// headroom is deliberate and the observed figure is recorded here so the next reader knows what
// the margin is over.
//
// ON THAT ARCHIVE THIS CAP WOULD NEVER HAVE FIRED, and the marker record ships exercised by unit
// tests only. That is a property of the traffic rather than a reason to drop the bound: the
// projection accumulates a whole session to pair it, so an unbounded session is unbounded memory.
const maxCallsPerSession = 4096

// maxPairedEvents bounds how many of one session's events the projection HOLDS in order to pair
// them, which is the memory bound maxCallsPerSession cannot be: that one caps the records written,
// and it can only do so once the accumulation is already complete.
//
// 50,000 EVENTS, matching the cap sessionapi already puts on a timeline request
// (timelineMaxEvents) — the same pairing over the same events, so a second, lower number here
// would mean the export refused to pair a session the timeline pane draws happily.
//
// At ~1 KiB per decoded event without its inference payload, this is tens of MiB for one session:
// above every session observed (the deepest held 2,911 events) and still a bound. Earlier walks
// newest first, so what a bound drops is the session's OLDEST events; see Walk for why that is
// sound rather than merely cheap.
const maxPairedEvents = 50_000

// turnTables derives the per-asking-response facts the records need, from the turns the pairing
// already resolved. Keyed by the asking RESPONSE's seq, which is what Call.CallSeq holds.
//
// THE GAP IS DEFINED ONCE, AND NOT THE WAY A FIRST READING SUGGESTS (spec §4.3a). It is the idle
// interval BEFORE this call's asking response: from the END of the previous response of the same
// agent, to the START of the request that produced this one. So it contains no model time and no
// tool execution of this call. The obvious alternative — previous tool call's end to this call's
// start — cannot be checked, because a call starts when its asking response ends, so that
// interval always contains the asking response's own inference.
func turnTables(res toolpair.Result) (map[uint64]turnFacts, map[uint64]gapFacts) {
	turns := make(map[uint64]turnFacts, len(res.Turns))
	gaps := make(map[uint64]gapFacts, len(res.Turns))
	// prevEnd is each agent's previous response end, and prevHadTools whether that response asked
	// for any tool — which is what classifies the gap (T25).
	prevEnd := map[string]time.Time{}
	prevHadTools := map[string]bool{}

	for _, t := range res.Turns {
		if t.Req == nil || t.Resp == nil {
			continue
		}
		seq := t.Resp.Seq
		turns[seq] = turnFacts{
			client:    clientLabel(t.Req.Client),
			agentRole: agentRoleOf(t.Req),
		}
		// The gap runs to the START of this turn's request.
		if end, ok := prevEnd[t.Agent]; ok {
			ms := t.Req.At.Sub(end).Milliseconds()
			if ms < 0 {
				// Events are appended under a lock after At is stamped, so two concurrent
				// requests in one session can land with At inverted by that build→append gap
				// (Walk's since note documents the same fuzziness). A negative idle interval is
				// not a fact; zero is.
				ms = 0
			}
			kind := GapUser
			if prevHadTools[t.Agent] {
				kind = GapTool
			}
			gaps[seq] = gapFacts{ms: &ms, kind: kind}
		} else {
			// T24: absent on an agent's first turn, where there is no previous response —
			// ABSENT, NEVER ZERO. gapKind says which case it was.
			gaps[seq] = gapFacts{kind: GapFirst}
		}
		prevEnd[t.Agent] = toolpair.ResponseEnd(t.Req, t.Resp)
		prevHadTools[t.Agent] = respAskedForTools(t.Resp)
	}
	return turns, gaps
}

// agentRoleOf is the asking request's agent role, as a CLOSED VOCABULARY — the same rule and the
// same reason as Record.AgentRole's switch: this package's input is a decoded archive event,
// where the field is whatever the JSON said, and a 144-byte agentRole carrying an escape sequence
// round-tripped verbatim before that switch existed.
func agentRoleOf(req *model.SessionEvent) string {
	if req.Inference == nil {
		return ""
	}
	switch req.Inference.AgentRole {
	case model.AgentRoleMain, model.AgentRoleSubagent:
		return string(req.Inference.AgentRole)
	}
	return ""
}

// respAskedForTools reports whether a response asked for any tool call, which is T25's test for
// whether the interval after it was a `tool` gap or a `user` one.
//
// READS THE SLICE AND THE COUNT, like FromEvent's three counts do: a projected copy (sessionapi's
// view=summary) carries ToolCallCount with the slice already dropped, so reading only len() would
// classify every such turn as a `user` gap — turning tool-execution time into apparent idle time,
// which is exactly the misreading T37 warns about.
func respAskedForTools(resp *model.SessionEvent) bool {
	if resp.Inference == nil {
		return false
	}
	return len(resp.Inference.ToolCalls) > 0 || resp.Inference.ToolCallCount > 0
}

// MarkerRecord is the per-session record written when a session's calls exceed
// maxCallsPerSession (spec §6.4). It says the projection was incomplete FOR THAT SESSION, in
// band, rather than letting a consumer read a truncated set as a complete one.
//
// A RECORD RATHER THAN A LOG LINE, because the consumer of an export is a pipeline that never
// sees the server's logs, and a silently short answer is the failure this endpoint has twice been
// built to avoid (#1543, and handleExport's ErrAbortHandler note). A consumer totalling tool time
// per session can detect the shortfall and exclude that session rather than under-reporting it.
type MarkerRecord struct {
	Schema int    `json:"schema"`
	Kind   string `json:"kind"`
	// SessionID is the session whose projection was capped.
	SessionID string `json:"sessionId"`
	// Dropped is how many calls were not exported; Cap is the bound that was hit, so a consumer
	// reading an old file knows what limit produced it.
	Dropped int `json:"dropped"`
	Cap     int `json:"cap"`
	// DroppedEvents is how many of the session's OLDEST events were not held for pairing, and
	// EventCap the bound that dropped them. Reported separately from Dropped because the two
	// are different losses: Dropped is calls the projection paired and then would not write,
	// where these are events it never saw, so the calls whose asking response was among them
	// produce no record at all and are not counted in Dropped.
	//
	// Either field being non-zero makes the session's tool time a LOWER BOUND rather than a
	// total. Both are omitted when zero, so an uncapped session's marker never appears at all.
	DroppedEvents int `json:"droppedEvents,omitempty"`
	EventCap      int `json:"eventCap,omitempty"`
}

// MarkerKind is the `kind` of a MarkerRecord. A third kind on the stream, named for the same
// reason the other two are: a consumer filters on it.
const MarkerKind = "toolCallsTruncated"

func markerRecord(sessionID string, dropped, droppedEvents int) MarkerRecord {
	m := MarkerRecord{
		Schema:    SchemaVersion,
		Kind:      MarkerKind,
		SessionID: sessionID,
		Dropped:   dropped,
		Cap:       maxCallsPerSession,
	}
	if droppedEvents > 0 {
		m.DroppedEvents, m.EventCap = droppedEvents, maxPairedEvents
	}
	return m
}
