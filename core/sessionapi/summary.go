package sessionapi

import "github.com/rossoctl/cortex/core/pipeline"

// summarizeEvent returns a copy of e with the content-bearing payloads removed,
// keeping every field the events timeline renders.
//
// WHY THIS EXISTS: an inference request carries the whole conversation, so a
// single event measures ~209KB and 99.5% of that is the `inference` field alone.
// The timeline shows time, direction, host, method, status, duration, tokens and
// cost — none of which needs a message body. Measured on real traffic, dropping
// the payloads is a 203x reduction: a 1000-event session goes from 199 MiB to
// 0.98 MiB, which is the difference between waiting seconds to open a session
// and fetching the whole thing.
//
// THE RULE: a field may be dropped only if NEITHER the events table nor its
// filter reads it. The filter is the half that is easy to forget, and forgetting
// it is silent — a `/completion text` search simply stops matching. abctl's
// eventHaystack and matchEventRow (cmd/abctl/tui/events_pane.go) are the source of
// truth for that half; TestSummarizeEvent_KeepsEverythingTheFilterSearches names
// each field they read so this cannot drift again.
//
// WHAT STAYS, and why each one is not obvious:
//   - Invocations. Timeline data, not detail data — abctl renders one row PER
//     INVOCATION, the ACTION column comes from it, and the filter searches each
//     invocation's plugin, action, reason, path and Details.
//   - Token counts, all of them separately. Cost is derived from the
//     cache-read/write split rather than the total, so a partial set silently
//     changes the COST column.
//   - Inference.Completion and A2A.Parts. Both are searched by the free-text
//     filter, so dropping them made `/` stop matching completion and A2A message
//     text. Measured cost of keeping them: 299x → 289x. Not a trade.
//   - Plugins. The `plugin:<name>` filter is a lookup into this map, so dropping
//     it made that filter match nothing at all. It is the expensive one to keep —
//     299x → 163x — and still worth it, because 163x is 1.12 MiB for a
//     1000-event session against 0.61, and both open instantly.
//   - Error fields and IsAction. Small; a failure is what an operator scans a
//     timeline for, and IsAction says whether a row is an action or mechanics.
//
// WHAT GOES: Inference.Messages / Tools / ToolCalls, A2A.Artifact, and
// MCP.Params / Result. Those are read by the detail pane alone, which fetches the
// full event for the row under the cursor. Messages is the one that matters —
// every inference request carries the whole conversation, and it is essentially
// all of the 99.5%.
//
// One consistency note that motivated keeping the filter's fields rather than
// narrowing the filter: SSE-streamed events are NOT projected, so a filter that
// worked on the stream but not on the snapshot would match newly arrived rows and
// miss everything the snapshot replaced — the same query giving different answers
// depending on when a row happened to arrive.
//
// COPIES, NEVER CLEARS. The store hands out pointers to the events it keeps, so
// clearing fields in place would delete the operator's conversation from the
// store — permanently, and only for the sessions somebody happened to open. One
// shallow copy of the event plus one per present extension; the payload slices
// and maps are dropped by reference, not walked.
func summarizeEvent(e *pipeline.SessionEvent) *pipeline.SessionEvent {
	s := *e

	// Plugins is NOT dropped: `plugin:<name>` is a lookup into it.

	if e.Inference != nil {
		inf := *e.Inference
		// COUNTED BEFORE BEING DROPPED. Two facts about a conversation survive with none of its
		// content — whether the request carried a tool manifest, and how long the conversation
		// is — and abctl's CONTEXT gauge needs both: the manifest separates an agentic turn from
		// a one-shot completion, the message count tells its thread from a subagent's where the
		// proxy states no agentRole. It read them off the slices below, so this projection blanked
		// that column for every row the timeline delivered while the unprojected SSE stream kept
		// working. Two ints against a payload that is 99.5% of the event; see
		// pipeline.InferenceExtension.MessageCount for the "zero means not stated" rule.
		//
		// AgentRole is not in this list and needs nothing: the struct copy above carries it, which
		// is the whole difference between a scalar and the slices below.
		inf.MessageCount, inf.ToolCount = len(inf.Messages), len(inf.Tools)
		inf.Messages = nil
		inf.Tools = nil
		inf.ToolCalls = nil
		// Completion is NOT dropped: the free-text filter searches it.
		s.Inference = &inf
	}
	if e.A2A != nil {
		a := *e.A2A
		// Parts is NOT dropped: the free-text filter searches Parts[].Content.
		a.Artifact = ""
		s.A2A = &a
	}
	if e.MCP != nil {
		m := *e.MCP
		m.Params = nil
		m.Result = nil
		s.MCP = &m
	}
	return &s
}
