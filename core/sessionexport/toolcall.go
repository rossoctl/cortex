package sessionexport

// The toolCall record: a tool call paired with its result, projected content-free onto the
// export stream as a sibling of the event record (spec #2009, requirements T1–T37).
//
// WHY A SIBLING RECORD RATHER THAN A WIDER EVENT RECORD (T19). A call's duration spans two
// events — the response that asked and the later request that carried the result — so it belongs
// to neither. Hanging it off either one would make a consumer join `seq` by hand, which is the
// work this record exists to remove. It is discriminated by `kind`, so an existing consumer
// either filters `select(.kind != "toolCall")` or ignores the key.
//
// THE PAIRING IS NOT DONE HERE. core/session/toolpair does it, shared with core/session/timeline,
// because closing a call correctly has five cases beyond "its result arrived" and each was a bug
// before it was a case. This file is the PROJECTION of that answer: it decides what leaves the
// machine, never how a call is paired.
//
// WHAT A RECORD DELIBERATELY DOES NOT CARRY: the tool-use id (T8 — it joins a record back to a
// conversation, the one thing a content-free export must not offer), the call's arguments (T6),
// its result content (T7), and the timeline's `preview`, which is built from argument keys. The
// join a consumer gets instead is `sessionId` + `callSeq` + `callIndex` (T34).

import (
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session/toolpair"
)

// ToolCallKind is the `kind` of a toolCall record, and EventKind of an event record on the same
// stream. Named constants rather than literals, because a consumer's filter is written against
// these strings and T19 makes them API.
const (
	ToolCallKind = "toolCall"
	EventKind    = "event"
)

// GapKind is the closed vocabulary for a toolCall record's `gapKind` (T25). It classifies the
// gapMs interval by THE PREVIOUS RESPONSE OF THE SAME AGENT — the one whose end begins it.
type GapKind string

const (
	// GapUser is a previous response that asked for no tool: the model stopped and waited for a
	// human. The one gap a duty cycle starts from (T37) — with that requirement's scope
	// qualification, since it is AGENT-LOCAL and a background job or a sibling agent can run
	// inside it.
	GapUser GapKind = "user"
	// GapTool is a previous response that asked for tools: they were running. NOT IDLE TIME, and
	// it must not be added to tool time — it overlaps those calls' own durationMs, so adding the
	// two counts the same wall clock twice (T37).
	GapTool GapKind = "tool"
	// GapFirst is an agent's first turn: there is no previous response, and gapMs is ABSENT
	// rather than zero (T24).
	GapFirst GapKind = "first"
)

// ToolCallRecord is one paired tool call, reduced to what a consumer outside the proxy can act on.
//
// ITS ZERO RULE IS THE OPPOSITE OF Record'S, FOR NINE FIELDS (T35). Record treats every zero as
// "not recorded" and marks them all omitempty. Here, for the fields named below, ZERO IS A REAL
// VALUE and the key is always written: the first call of every batch has callIndex 0, a result
// clamped to its call's start is a real 0 ms call, an empty result is a real 0 bytes, and a gap
// under half a millisecond rounds to a real 0. T14 and T24 each reserve ABSENCE for "not
// measured", so a measured zero has to be distinguishable from an unmeasured field. An
// implementation that reaches for omitempty on them silently turns an empty result into "not
// measured" and a sub-millisecond gap into "first turn".
//
// Only isError and backgrounded use omitempty, where absent reads as false — which is what they
// mean.
type ToolCallRecord struct {
	Schema int `json:"schema"`

	// Kind is the discriminator (T19), always ToolCallKind on this type. No omitempty: it is the
	// field a consumer's filter reads, so it cannot be conditional.
	Kind string `json:"kind"`

	// --- identity and placement (T5, T29, T30, T34) ---

	// SessionID is the session, as the client asserted it. NOT A UNIT OF WORK until
	// sessionIncarnation lands (#1997): per-session aggregates average across unrelated sittings
	// exactly as event records do.
	SessionID string `json:"sessionId"`

	// StartedAt is when the call began — the asking response's end. ALWAYS PRESENT, including for
	// a call that never got a result, so every record can be placed on a timeline (T29). The end,
	// where there is one, is StartedAt + DurationMs. UTC, like Record.At, so every timestamp on
	// the stream parses and sorts as one.
	StartedAt time.Time `json:"startedAt"`

	// CallSeq is the seq of the response that asked for this call; ResultSeq the seq of the
	// request that carried its result, ABSENT when nothing did. Together they rejoin a call to
	// the events either side of it WITHOUT the tool-use id (T8).
	CallSeq   uint64 `json:"callSeq"`
	ResultSeq uint64 `json:"resultSeq,omitempty"`

	// CallIndex is the call's position among the calls its response asked for, from 0, in the
	// order the model emitted them (T30). A batch is otherwise unorderable. ALWAYS WRITTEN: the
	// first call of every batch is 0 and T34 makes this a join key.
	CallIndex int `json:"callIndex"`

	// --- the tool (T4, T10, T10a, T11, T12) ---

	// Tool is a built-in name verbatim, or an `hmac-sha256:`-prefixed keyed hash for an MCP or
	// unknown name. See toolname.go: the branch is on "is this provably a built-in".
	Tool string `json:"tool,omitempty"`
	// ToolSource says what KIND of tool it is, NEVER WHERE IT RAN (spec §7.1b). A built-in may
	// run on the developer's machine or in a remote sandbox; an MCP tool may be served locally
	// over stdio or remotely over HTTP. The proxy sees neither, and must not infer a site.
	ToolSource ToolSource `json:"toolSource,omitempty"`

	// --- timing (T16, T18, T23, T24, T25, T31) ---

	// DurationMs is the paired duration, present exactly when EndReason.HasEnd() (T16, T31). A
	// call with no end carries NO duration rather than the zero-length span the timeline draws
	// it with: a zero would pass a consumer's `durationMs > 0` filter as a 0 ms call and drag a
	// p50 toward zero. Milliseconds, truncated, matching Record.
	//
	// A POINTER so "ended at 0 ms" and "never ended" are different wire shapes. T35 requires the
	// key when the reason has an end — including at a real 0, for a result clamped to its call's
	// start — and omitempty on an int64 cannot express that.
	DurationMs *int64 `json:"durationMs,omitempty"`

	// EndReason names how the call ended, from toolpair's closed vocabulary. Always written.
	//
	// IT DOES NOT SAY WHETHER A RESULT ARRIVED — ResultSeq does — nor whether the call was
	// backgrounded, which Backgrounded says. Both qualifications exist because `stranded` (T31,
	// deferred to #2046) wins over both `result` and `background` when they coincide.
	EndReason toolpair.EndReason `json:"endReason"`

	// ModelMs is the asking response's own measured duration: model time, per call, with no join
	// back to the event record. GapMs is the idle interval BEFORE that response — from the end of
	// the previous response of the same agent to the start of the request that produced this
	// one — so it contains no ModelMs and no tool execution of this call.
	//
	// BOTH BELONG TO THE ASKING RESPONSE, NOT TO THE CALL (T36), so every record of a batch
	// repeats the same two values. TAKE THEM ONCE PER CallSeq: summing ModelMs over records
	// counts a batch's inference ParallelN times, and DurationShared does not mark that.
	ModelMs int64 `json:"modelMs,omitempty"`
	// GapMs is always written when measured, including a real 0 — T24 reserves ABSENCE for an
	// agent's first turn, so a sub-millisecond gap must not read as "no previous response".
	GapMs   *int64  `json:"gapMs,omitempty"`
	GapKind GapKind `json:"gapKind,omitempty"`

	// --- effort proxies, never content (T14) ---

	// InputBytes is the size of the call's arguments and ResultBytes the size of its result.
	// BYTE COUNTS, AS THE NAMES SAY — a size is not a character count and the two differ for any
	// non-ASCII payload. Always written when measured, because an empty result and an
	// argument-free call are real zeros and T14 reserves absence for "not measured".
	//
	// A size is not content: the same concession the trajectory schema already makes with
	// command_chars and result_preview_chars.
	InputBytes  *int `json:"inputBytes,omitempty"`
	ResultBytes *int `json:"resultBytes,omitempty"`

	// IsError is whether the result said it was an error — the tool failure rate, per tool. False
	// when there was no result. omitempty: absent reads as false, which is what it means.
	IsError bool `json:"isError,omitempty"`

	// --- batching (T15) ---

	// ParallelN is how many calls the asking response made and DurationShared whether this
	// record's duration comes from a window shared with them. The batch's identity is CallSeq, so
	// no separate group key exists.
	//
	// THE MARKER SAYS THE WINDOW IS SHARED, NEVER THAT THE CALLS RAN CONCURRENTLY. A consumer can
	// then divide, take the window once per batch, or exclude it — explicitly, by its own choice.
	ParallelN      int  `json:"parallelN,omitempty"`
	DurationShared bool `json:"durationShared,omitempty"`

	// --- context (T5, T32a) ---

	// AgentRole is `main` or `subagent`, so subagent tool traffic reads apart. A closed
	// vocabulary, exported by naming its values, as Record.AgentRole is.
	AgentRole string `json:"agentRole,omitempty"`
	// Client is the cost-attribution and display key. NEVER AN AUTHORIZATION SUBJECT.
	Client string `json:"client,omitempty"`

	// Backgrounded says the call started a background job, WHATEVER ITS EndReason (T32a). Count
	// missing job time on this field, never on `endReason: "background"`: when `stranded` lands
	// it wins over `background`, and then the reason no longer carries the fact. omitempty, like
	// IsError.
	Backgrounded bool `json:"backgrounded,omitempty"`
}

// toolCallOpts is what a projection needs beyond the call itself: the key that labels a hashed
// tool name, and the per-turn facts the pairing does not carry on a Call.
type toolCallOpts struct {
	key ToolNameKey
	// turnOf maps an asking response's seq to the client label and agent role of its turn. Built
	// once per session by projectToolCalls, because a Call carries neither.
	turnOf map[uint64]turnFacts
	// gapOf maps an asking response's seq to the gap before it and that gap's kind.
	gapOf map[uint64]gapFacts
}

type turnFacts struct {
	client    string
	agentRole string
}

type gapFacts struct {
	ms   *int64
	kind GapKind
}

// projectToolCalls projects one session's paired calls to records, newest call first so the
// stream's within-session order matches the event records' (Walk documents newest-first).
//
// events MUST BE IN ASCENDING SEQ ORDER — toolpair.Calls's own precondition. The caller reverses
// what Walk's archive read hands it; see Walk. Pairing a descending slice closes nothing, because
// every result precedes its call, and T17 then drops every record silently.
//
// THE CAP IS APPLIED HERE (spec §6.4). When a session holds more calls than maxCallsPerSession,
// the projection keeps the newest and reports the overflow, so the caller can emit the marker
// record. Degrading loudly is this endpoint's posture: a truncated export resets the connection
// rather than handing back a clean 200 (handleExport's ErrAbortHandler), and silently pairing
// fewer calls than a session holds is the same class of lie.
func projectToolCalls(events []model.SessionEvent, sessionID string, key ToolNameKey) ([]ToolCallRecord, int) {
	// ONE PAIRING PASS, whose Turns feed turnTables. toolpair returns Turns beside Calls so a
	// consumer does not walk again — pairing twice over one session measured ~30% of a whole
	// timeline build, per that package's own doc.
	res := toolpair.Calls(events, toolpair.Options{})
	if len(res.Calls) == 0 {
		return nil, 0
	}
	turns, gaps := turnTables(res)
	opts := toolCallOpts{key: key, turnOf: turns, gapOf: gaps}
	out := make([]ToolCallRecord, 0, min(len(res.Calls), maxCallsPerSession))
	dropped := 0
	// Newest first, matching the event records' within-session order.
	for i := len(res.Calls) - 1; i >= 0; i-- {
		c := res.Calls[i]
		// T17: a span still open when the walk reads it is NOT exported. An unfinished call has
		// no duration to report, and a record that appeared with one in a later export would make
		// the export non-reproducible.
		if c.EndReason == "" {
			continue
		}
		if len(out) >= maxCallsPerSession {
			dropped++
			continue
		}
		out = append(out, toolCallRecord(c, sessionID, opts))
	}
	return out, dropped
}

// toolCallRecord projects one paired call.
func toolCallRecord(c toolpair.Call, sessionID string, opts toolCallOpts) ToolCallRecord {
	source := classifyTool(c.Name, clientName(opts.turnOf[c.CallSeq].client))
	r := ToolCallRecord{
		Schema:     SchemaVersion,
		Kind:       ToolCallKind,
		SessionID:  sessionID,
		StartedAt:  c.Start.UTC(),
		CallSeq:    c.CallSeq,
		ResultSeq:  c.ResultSeq,
		CallIndex:  c.CallIndex,
		Tool:       opts.key.label(c.Name, source),
		ToolSource: source,
		EndReason:  c.EndReason,
		IsError:    c.IsError,
		ModelMs:    c.ModelDur.Milliseconds(),
	}
	// T16/T31: a duration exactly when the reason has an end. Asked via HasEnd rather than by
	// listing the reasons, so a reason added later (stranded, #2046) is handled by default —
	// toolpair's own doc makes that the contract.
	if c.EndReason.HasEnd() && c.End != nil {
		ms := c.End.Sub(c.Start).Milliseconds()
		if ms < 0 {
			// toolpair clamps End up to Start, so this should not occur; if it ever does, a
			// zero-length call is the honest answer rather than a negative one.
			ms = 0
		}
		r.DurationMs = &ms
	}
	// T14: sizes, always written when measured. ResultBytes is 0 for a call with no result, which
	// is "not measured" rather than an empty result — so it is written only when a result came.
	if c.ResultSeq != 0 || c.EndReason.HasEnd() {
		rb := c.ResultBytes
		r.ResultBytes = &rb
	}
	// InputBytes is the argument size. Options.Arguments is OFF for the export, so Call.Arguments
	// is empty by construction and no argument bytes exist to measure — the field stays absent
	// rather than reporting a 0 that would read as an argument-free call. Keeping the export
	// argument-free is worth more than this one effort proxy; see spec T6 and T14.
	if n := len(c.Arguments); n > 0 {
		r.InputBytes = &n
	}
	// T15: the batch marker. SiblingN is 1 for a lone call, so > 1 is the batch predicate, and
	// the shared window is only a fact when the call actually has a duration to share.
	if c.SiblingN > 1 {
		r.ParallelN = c.SiblingN
		r.DurationShared = r.DurationMs != nil
	}
	// T32a: the background fact rides on its own field whatever the reason.
	if c.EndReason == toolpair.ReasonBackground {
		r.Backgrounded = true
	}
	// T36/T24/T25: model time and the gap belong to the asking response.
	if g, ok := opts.gapOf[c.CallSeq]; ok {
		r.GapMs, r.GapKind = g.ms, g.kind
	}
	t := opts.turnOf[c.CallSeq]
	r.Client, r.AgentRole = t.client, t.agentRole
	return r
}

// clientName is the name half of a client label ("claude-code/2.1.270" → "claude-code"), which is
// the key builtinTools is keyed by. clientLabel joins the two with a slash and the name itself
// never contains one, so cutting at the first is exact.
func clientName(label string) string {
	for i := range len(label) {
		if label[i] == '/' {
			return label[:i]
		}
	}
	return label
}
