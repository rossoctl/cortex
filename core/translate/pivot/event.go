package pivot

// EventKind is the kind of a reply event. A reply is a list of events,
// for streamed and not-streamed replies alike (rule H3).
type EventKind int

// The reply event kinds of rule H3.
const (
	EventStart EventKind = iota
	EventBlockStart
	EventDelta
	EventBlockStop
	EventStop
	EventUsage
	EventError
)

// DeltaKind is the kind of payload in an EventDelta.
type DeltaKind int

// The delta kinds of rule H3.
const (
	DeltaText DeltaKind = iota
	DeltaThinking
	DeltaSignature
	DeltaInputJSON // a JSON piece of tool input
)

// Event is one reply event. Kind tells which fields apply.
type Event struct {
	Kind             EventKind
	ID, Model        string    // start
	Index            int       // block_start, delta, block_stop
	Block            BlockKind // block_start
	ToolID, ToolName string    // block_start of tool_use
	Delta            DeltaKind // delta
	Value            string    // delta payload
	StopReason       string    // stop: Anthropic vocabulary (end_turn, max_tokens, tool_use, refusal)
	Usage            Usage     // usage
	Status           int       // error
	ErrorType        string    // error: Anthropic error type vocabulary
	Message          string    // error
}
