// Package sessionexport projects a session event to a flat, content-free record for
// post-processing outside the proxy.
//
// WHY A PROJECTION RATHER THAN THE EVENT. An inference request carries the whole
// conversation, and 99.5% of such an event is its inference payload — the same measurement
// sessionapi.summarizeEvent is built on. Exporting events whole is unaffordable (a 30-day
// local archive whose events total ~6GB of request bodies holds 29,606 events) and it would
// carry every prompt off the machine. A record is ~30 scalars, so a month of traffic is a
// file a laptop can process.
//
// AN ALLOWLIST, DELIBERATELY, AND THE OPPOSITE CHOICE FROM usage.ReplayCopy. That function
// copies the event and clears the heavy fields, so a field added to the event later is kept
// without anyone remembering to add it — the right default for an in-process consumer. An
// export inverts the risk: a content field added to the event later would start leaving the
// machine, silently, in whatever the operator already configured to ship. So Record names
// every field it carries, and a new event field reaches an export only when someone adds it
// here on purpose. TestRecord_CarriesNoContent pins that by rejecting any record whose
// encoding contains payload text.
//
// NOT IN A RECORD, AND NOT BY OVERSIGHT: message content, the completion, tool-call
// arguments, tool-result bodies, MCP params and results, A2A parts, artifacts and error
// messages, plugin event payloads, thinking blocks, authenticated identity, TLS peer
// identity, and request headers — the User-Agent included, which is why Client carries a
// parsed name or nothing. Tool-call NAMES are also absent here (see rossoctl/cortex#1810's
// Tier 2) — the names alone are low risk, but arguments are not, and nothing in this
// package's first form needs either.
//
// THE REQUEST PATH IS ABSENT TOO, and it is the subtlest of these: a path SEGMENT can hold a
// bot token or a webhook secret, which nothing can tell from a resource id — SessionEvent.
// HTTPPath says so itself, naming an off-box export as the case to know it before. So
// neither HTTPPath nor Invocation.Path, which is a back-filled copy of the same string,
// reaches a record. HTTPMethod alone carries the shape of the request.
package sessionexport

import (
	"time"

	"github.com/rossoctl/cortex/core/model"
)

// SchemaVersion is the record format's version, carried on every record so a consumer
// reading a mixed file can tell which shape it has. Bump it when a field is removed or its
// meaning changes; adding a field does not need a bump, since an absent key already reads
// as "not present" (see the zero-means-absent note below).
const SchemaVersion = 1

// Record is one session event, reduced to what a consumer outside the proxy can act on.
//
// ZERO MEANS "NOT RECORDED", NEVER A REAL ZERO, for every numeric field here, and every one
// is `omitempty` to say so on the wire. That is the event's own convention — BytesUp and
// BytesDown are absent rather than 0 when no plugin wanted the body, and Duration is zero on
// a request-phase row — and a consumer that renders an absent figure as 0 will report a
// request that was never measured as one that took no time and sent no bytes.
type Record struct {
	Schema int `json:"schema"`

	// --- identity ---

	// SessionID is the session the event belongs to, as the client asserted it.
	//
	// NOT A UNIT OF WORK. A client may reuse one id indefinitely: on the archive this
	// package was designed against, the id "default" spans four weeks of unrelated work in
	// one directory. Aggregating per SessionID therefore averages across tasks that have
	// nothing to do with each other. A derived incarnation key would fix that and does not
	// exist yet (rossoctl/cortex#1810, rossoctl/cortex#1739); until it does, a consumer
	// wanting "one sitting" must infer it from gaps in At.
	SessionID string `json:"sessionId"`

	// Seq is the event's position in its session, counting from 1.
	//
	// ORDER BY At, THEN Seq — and never by Seq alone. The numbering is unique within one
	// incarnation of a session, not forever: the counter lives on the store's entry, so a
	// session re-created under the same id restarts at 1, and a consumer cannot tell that
	// from a trimmed prefix. But At cannot break its own ties either (see DurationMs below),
	// and ties are common on a fast local proxy, so At alone leaves tied rows in arbitrary
	// order. Seq is the tiebreak within a session, which is the rule SessionEvent.Seq states
	// for paging the session API; it is also here to spot gaps.
	Seq uint64 `json:"seq,omitempty"`

	// RequestID pairs a request record with its response record. Positional pairing
	// misattributes whenever a client has concurrent requests in flight, which a coding
	// agent routinely does.
	RequestID string `json:"requestId,omitempty"`

	At time.Time `json:"at"`

	// --- shape of the exchange ---

	Direction string `json:"direction,omitempty"`
	Phase     string `json:"phase,omitempty"`

	// Host is where the bytes went; RequestedHost is what the client asked for, and is set
	// only when a plugin redirected the request. The two differing is the only way an export
	// reader sees a redirect happened.
	Host          string `json:"host,omitempty"`
	RequestedHost string `json:"requestedHost,omitempty"`

	HTTPMethod string `json:"httpMethod,omitempty"`

	// --- outcome ---

	StatusCode int    `json:"statusCode,omitempty"`
	ErrorKind  string `json:"errorKind,omitempty"`
	ErrorCode  string `json:"errorCode,omitempty"`

	// --- timing ---

	// DurationMs is the event's own measured duration; TtfbMs is time to response headers.
	//
	// SUBTRACT NEITHER TIMESTAMPS NOR RECORDS TO GET A LATENCY. Two events recorded in the
	// same instant are common on a fast local proxy, and At cannot break its own ties, so
	// pairing by RequestID and subtracting At yields noise at the scale fast exchanges live
	// at. DurationMs is the measurement; DurationMs − TtfbMs is how long the body took.
	DurationMs int64 `json:"durationMs,omitempty"`
	TtfbMs     int64 `json:"ttfbMs,omitempty"`

	// --- size ---

	BytesUp   int64 `json:"bytesUp,omitempty"`
	BytesDown int64 `json:"bytesDown,omitempty"`

	// --- attribution ---

	// Client is the coding agent, as "name/version", parsed from the User-Agent.
	//
	// CLIENT-ASSERTED AND TRIVIALLY SPOOFABLE: a cost-attribution and display key, never an
	// authorization subject. Worth restating here rather than only on the event, because an
	// external collector is exactly where this is likely to be mistaken for identity.
	//
	// A RECOGNISED AGENT OR NOTHING. Empty both when the request carried no User-Agent and
	// when it carried one this proxy does not parse — absence, not an agent named "unknown",
	// and never the raw header. So a consumer counting agents undercounts rather than
	// inventing a name, and the record cannot carry whatever a caller put in that header.
	Client string `json:"client,omitempty"`

	// AgentRole says which caller under one session id made the request: "main" or
	// "subagent". EMPTY MEANS NOT STATED, NOT MAIN — only Claude Code declares it today.
	AgentRole string `json:"agentRole,omitempty"`

	// --- inference ---

	// Model is the model the request was sent for. RequestedModel is set only when a plugin
	// changed it, ServedModel only when the response named a different one than was asked
	// for.
	Model          string `json:"model,omitempty"`
	RequestedModel string `json:"requestedModel,omitempty"`
	ServedModel    string `json:"servedModel,omitempty"`

	// Stream is what the request asked for; StreamedResponse is what arrived. The two
	// disagreeing is a real condition worth counting and is invisible from either alone.
	Stream           bool `json:"stream,omitempty"`
	StreamedResponse bool `json:"streamedResponse,omitempty"`

	FinishReason string `json:"finishReason,omitempty"`

	// Token counts, THE SPLIT ONLY AND NEVER AN AGGREGATE. These five are exactly what
	// cost/ledger.Row prices from, and InputTokens is the UNCACHED prompt — the three
	// prompt-side kinds are disjoint, so a consumer adds them itself if it wants a prompt
	// total.
	//
	// NO promptTokens / completionTokens HERE, DELIBERATELY. The event carries a legacy pair
	// of those names, but InferenceExtension.PromptTokens is already
	// Input+CacheRead+CacheWrite (parsercommon.TokenUsage.Fill writes it; model.PromptTokensOf
	// documents it). Exporting it beside CacheReadTokens would ship a total next to its own
	// components, and a consumer pricing the input rate on one and the cache rate on the other
	// counts every cached token twice — on a real archive, 196k of them on a single response.
	// An aggregate a consumer can compute is not worth a figure it can misread.
	InputTokens      int `json:"inputTokens,omitempty"`
	CacheReadTokens  int `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int `json:"cacheWriteTokens,omitempty"`
	OutputTokens     int `json:"outputTokens,omitempty"`
	ReasoningTokens  int `json:"reasoningTokens,omitempty"`

	// PresentKinds names which of the five counts above the provider actually reported, and is
	// the one field here that escapes this type's zero-means-absent rule: a set bit with a
	// zero count is "the provider reported zero", an unset bit is "the provider does not
	// expose this kind". Without it those two are the same absent key, and a consumer cannot
	// tell a model that wrote no cache from a provider that never says.
	//
	// Bit layout is parsercommon.Kind (Input=1, CacheRead=2, CacheWrite=4, Output=8,
	// Reasoning=16); uint8 rather than the typed Kind for the same reason
	// InferenceExtension.PresentKinds is — neither type imports parsercommon.
	PresentKinds uint8 `json:"presentKinds,omitempty"`

	// MessageCount and ToolCount are how long the conversation was and whether the request
	// carried a tool manifest. The manifest separates an agentic turn from a one-shot
	// utility completion, which matters because a coding agent interleaves both under one
	// session id.
	//
	// Zero here is "not stated" rather than "none" for the reason the rest of this type's
	// numbers are: a view=summary source states them while a full event states the slices,
	// and FromEvent reads whichever is there.
	MessageCount int `json:"messageCount,omitempty"`
	ToolCount    int `json:"toolCount,omitempty"`

	// ToolCallCount is how many tool calls the model emitted on this response. Distinct from
	// ToolCount above, which is the manifest the REQUEST offered.
	//
	// DELIBERATELY NOT A COUNT OF ToolResults. A request re-sends the whole conversation, so
	// every earlier tool result is present again on every later turn: summing len(ToolResults)
	// over a session overcounts by the conversation's length — measured at 272x on one real
	// archive, which turns a 3.6% tool failure rate into an apparent 8 failures per call.
	// The per-event counts that make that a sum rather than a whole-session dedup are
	// rossoctl/cortex#1740; until they exist this record carries no tool-result figure at
	// all, rather than one a consumer would reasonably misread.
	ToolCallCount int `json:"toolCallCount,omitempty"`

	// --- tunnels ---

	// Tunnel marks an opaque CONNECT or transparent-redirect row, whose bytes were never
	// parsed. TunnelReason says why the bytes were left opaque — "passthrough-host" is a
	// configured passthrough working as intended, while "dial-failed", "program-refused" and
	// "os-trust-only" each mean every plugin was blind to that traffic.
	//
	// Tunnel rows carry latency and bytes and nothing else, and they are a third of the
	// events on a real archive. An export that dropped them would lose every non-LLM timing
	// the proxy holds.
	Tunnel       bool   `json:"tunnel,omitempty"`
	TunnelReason string `json:"tunnelReason,omitempty"`
}

// FromEvent projects e to a Record. sessionID is the id the event belongs to, for a caller
// reading an archive, which stamps the id on events whose own field may be unset; e's own
// SessionID wins when it has one.
//
// Every field a Record carries is named here. Adding one to Record without adding it here
// leaves it silently absent from every export; TestRecord_EveryFieldIsPopulated holds that
// by projecting a fully-populated event and failing on any zero field.
func FromEvent(e *model.SessionEvent, sessionID string) Record {
	if e == nil {
		return Record{Schema: SchemaVersion}
	}
	r := Record{
		Schema:        SchemaVersion,
		SessionID:     e.SessionID,
		Seq:           e.Seq,
		RequestID:     e.RequestID,
		At:            e.At,
		Direction:     e.Direction.String(),
		Phase:         e.Phase.String(),
		Host:          e.Host,
		RequestedHost: e.RequestedHost,
		HTTPMethod:    e.HTTPMethod,
		StatusCode:    e.StatusCode,
		BytesUp:       e.BytesUp,
		BytesDown:     e.BytesDown,
		Tunnel:        e.Tunnel,
		TunnelReason:  string(e.TunnelReason),
	}
	if r.SessionID == "" {
		r.SessionID = sessionID
	}
	// Durations are stored as ns and exported as ms: a consumer plotting latency wants ms,
	// and ns on a JSON number is needless precision on a wall-clock measurement. Truncated
	// rather than rounded, so a sub-millisecond exchange reads 0 — which, by this type's
	// zero-means-absent rule, is indistinguishable from unmeasured. Accepted deliberately:
	// the alternative is a float, and a consumer summing floats over a month of events
	// accumulates error in a figure that is a wall-clock estimate to begin with.
	r.DurationMs = e.Duration.Milliseconds()
	r.TtfbMs = e.Ttfb.Milliseconds()

	// ONLY A PARSED CLIENT REACHES THE EXPORT, which is why this reads Name rather than
	// filtering Label()'s output. Label() falls back to the RAW User-Agent when Name is empty
	// — so a header of "claude-cli/private-data" would arrive here verbatim, putting
	// attacker-controlled free text in a record this package promises carries none. Guarding
	// on Name covers the nil client too: Label() answers UnknownClientLabel in exactly the
	// cases Name is empty, and that reserved word must not become a real agent in an
	// aggregate either. Absence stays absence.
	if e.Client != nil && e.Client.Name != "" {
		r.Client = e.Client.Label()
	}
	if e.Error != nil {
		r.ErrorKind, r.ErrorCode = e.Error.Kind, e.Error.Code
	}
	if inf := e.Inference; inf != nil {
		r.Model = inf.Model
		r.RequestedModel = inf.RequestedModel
		r.ServedModel = inf.ServedModel
		r.Stream = inf.Stream
		r.StreamedResponse = inf.StreamedResponse
		r.FinishReason = inf.FinishReason
		r.AgentRole = string(inf.AgentRole)
		r.InputTokens = inf.InputTokens
		r.CacheReadTokens = inf.CacheReadTokens
		r.CacheWriteTokens = inf.CacheWriteTokens
		r.OutputTokens = inf.OutputTokens
		r.ReasoningTokens = inf.ReasoningTokens
		r.PresentKinds = inf.PresentKinds

		// All three counts come from the slice when present and from the event's own count
		// field otherwise: a projected copy (sessionapi's view=summary) carries the counts
		// with the slices already dropped, so reading only len() would report zero for every
		// row such a reader delivers.
		if n := len(inf.Messages); n > 0 {
			r.MessageCount = n
		} else {
			r.MessageCount = inf.MessageCount
		}
		if n := len(inf.Tools); n > 0 {
			r.ToolCount = n
		} else {
			r.ToolCount = inf.ToolCount
		}
		if n := len(inf.ToolCalls); n > 0 {
			r.ToolCallCount = n
		} else {
			r.ToolCallCount = inf.ToolCallCount
		}
	}
	return r
}
