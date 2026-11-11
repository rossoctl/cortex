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
// arguments, tool-result bodies, MCP params and results, A2A parts, plugin event payloads,
// TLS peer identity and request headers. Tool-call NAMES are also absent here (see
// rossoctl/cortex#1810's Tier 2) — the names alone are low risk, but arguments are not, and
// nothing in this package's first form needs either.
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
	// ORDER BY At, NOT BY Seq. The numbering is unique within one incarnation of a session,
	// not forever: the counter lives on the store's entry, so a session re-created under the
	// same id restarts at 1, and a consumer cannot tell that from a trimmed prefix. Seq is
	// here to pair with the session API and to spot gaps, not to sort a file.
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
	// external collector is exactly where this is likely to be mistaken for identity. Empty
	// when the request carried no User-Agent — absence, not an agent named "unknown".
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

	// Token counts, ALL FOUR SEPARATELY AND NEVER AS A TOTAL. Cost is derived from the
	// cache-read/write split rather than the sum, so a consumer handed a partial set
	// computes a different figure than the proxy does — the same argument cost/ledger.Row
	// makes for keeping them apart on disk.
	PromptTokens     int `json:"promptTokens,omitempty"`
	CompletionTokens int `json:"completionTokens,omitempty"`
	CacheReadTokens  int `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int `json:"cacheWriteTokens,omitempty"`

	// MessageCount and ToolCount are how long the conversation was and whether the request
	// carried a tool manifest. The manifest separates an agentic turn from a one-shot
	// utility completion, which matters because a coding agent interleaves both under one
	// session id.
	MessageCount int `json:"messageCount,omitempty"`
	ToolCount    int `json:"toolCount,omitempty"`

	// ToolCallCount is how many tool calls the model emitted on this response.
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

	// Client.Label() answers "unknown" for a nil client, which must not become a real agent
	// name in an aggregate. Absence stays absence.
	if e.Client != nil {
		if l := e.Client.Label(); l != model.UnknownClientLabel {
			r.Client = l
		}
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
		r.PromptTokens = inf.PromptTokens
		r.CompletionTokens = inf.CompletionTokens
		r.CacheReadTokens = inf.CacheReadTokens
		r.CacheWriteTokens = inf.CacheWriteTokens
		r.ToolCallCount = len(inf.ToolCalls)

		// The counts come from the slices when present and from the event's own count fields
		// otherwise: a projected copy (sessionapi's view=summary) carries the counts with
		// the slices already dropped, so reading only len() would report zero for every row
		// such a reader delivers.
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
	}
	return r
}
