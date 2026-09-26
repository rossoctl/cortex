package pipeline

// Fixtures for the prompt-context rule. DELIBERATELY A SECOND COPY of the set in
// cmd/abctl/tui/sessions_context_test.go, not a shared helper package.
//
// authbridge has no exported test-helper package anywhere, and introducing the first one to
// avoid copying ~94 lines would cost ~450 lines of churn across 80 call sites. Drift in what the
// fixtures SAY is loud or harmless, never silently wrong: a manifest that goes missing makes the
// fold return 0 and fails every test that reads it, while 27-tools-versus-1 changes nothing
// because the rule only asks whether a manifest is non-empty.
//
// ONE DIVERGENCE IS SILENT, AND IT IS IN WHAT THEY DO RATHER THAN WHAT THEY SAY, so the claim above
// is scoped rather than left to be contradicted nine lines down: roled below COPIES its argument
// where the tui original MUTATES it and hands the same slice back. A test moved between the two
// packages therefore changes meaning without failing — `base := conversation(...)` followed by
// `roled(base, AgentRoleSubagent)` restamps base over there and leaves it alone here. Nothing catches
// that but reading this paragraph, which is why it is here.
//
// The tui copy also has a job this one does not: it states NO role, so the tests over there
// keep exercising the unstated fallback rule on purpose.

import (
	"fmt"
	"time"
)

// toolsOf builds a manifest of n tools. Only its LENGTH matters to PromptContextOf: a request that
// carries any tools is an agentic conversation, one that carries none is a one-shot completion.
func toolsOf(n int) []InferenceTool {
	out := make([]InferenceTool, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, InferenceTool{Name: fmt.Sprintf("Tool%02d", i)})
	}
	return out
}

// exchange is a request/response pair as the store records one: the manifest and the message count
// on both sides, the token counts on the response, since the provider is the only party that
// tokenizes.
func exchange(id string, at time.Time, msgs, ntools, input, cacheRead int) []SessionEvent {
	inf := func() *InferenceExtension {
		return &InferenceExtension{
			Model:    "claude-opus-5",
			Messages: make([]InferenceMessage, msgs),
			Tools:    toolsOf(ntools),
		}
	}
	respInf := inf()
	respInf.InputTokens, respInf.CacheReadTokens = input, cacheRead
	return []SessionEvent{
		{At: at, RequestID: id, Phase: SessionRequest,
			Direction: Outbound, Inference: inf()},
		{At: at.Add(time.Second), RequestID: id, Phase: SessionResponse,
			Direction: Outbound, Inference: respInf},
	}
}

// conversation is an agentic turn: 27 tools, as every main-thread request measured carried.
func conversation(id string, at time.Time, msgs, context int) []SessionEvent {
	return exchange(id, at, msgs, 27, 300, context-300)
}

// oneShot is a title / quota / summary call: no tools, three messages, and — the part that
// matters — a context that can be large.
func oneShot(id string, at time.Time, context int) []SessionEvent {
	return exchange(id, at, 3, 0, 300, context-300)
}

// roled states the caller's role on every event of a turn, which is what a proxy that reads
// Claude Code's billing-header line publishes.
//
// COPIES FIRST, unlike the tui original, which mutated its argument and returned it. That reads
// as pure and is not: `base := conversation(...)` followed by `roled(base, AgentRoleSubagent)` would
// restamp base too. Safe there only because every caller happens to pass a fresh turn — mainAgent and
// subagent build the turn themselves, so no caller in tree hands either copy a slice it still holds.
func roled(evs []SessionEvent, role AgentRole) []SessionEvent {
	out := make([]SessionEvent, len(evs))
	for i := range evs {
		out[i] = evs[i]
		if evs[i].Inference != nil {
			inf := *evs[i].Inference
			inf.AgentRole = role
			out[i].Inference = &inf
		}
	}
	return out
}

// mainAgent is a conversation turn whose request declared itself the interactive thread.
func mainAgent(id string, at time.Time, msgs, context int) []SessionEvent {
	return roled(conversation(id, at, msgs, context), AgentRoleMain)
}

// subagent is a Task-spawned agent's turn. It carries its OWN tool manifest, which is why the
// manifest alone cannot filter it out — measured at 11 tools against the main thread's 27.
func subagent(id string, at time.Time, msgs, context int) []SessionEvent {
	return roled(conversation(id, at, msgs, context), AgentRoleSubagent)
}

// atUnset strips the arrival time from every event of a turn, modelling a producer that never set
// SessionEvent.At. NO PRODUCER IN TREE DOES — all four listeners stamp it at every construction — so
// this exists for one test, which pins what better()'s stated arm degenerates to without it.
//
// COPIES, like roled: a caller that stamped a turn and then wanted the same turn timeless would
// otherwise silently lose the original.
func atUnset(evs []SessionEvent) []SessionEvent {
	out := make([]SessionEvent, len(evs))
	for i := range evs {
		out[i] = evs[i]
		out[i].At = time.Time{}
	}
	return out
}

// projected is the shape the TIMELINE delivers, which no other fixture in this file produces: the
// two SLICES nilled and their LENGTHS recorded in ToolCount and MessageCount first.
//
// MIRRORED RATHER THAN CALLED, exactly as the tui copy of it is — sessionapi.summarizeEvent is
// unexported and in a package that imports this one, so calling it here would invert the dependency.
// core/sessionapi pins its half (TestSummarizeEvent_CountsTheConversationItDrops).
//
// IT MATTERS THAT EVERY OTHER FIXTURE HERE BUILDS THE SLICES, because the tool-manifest filter and
// the projection strip the same two fields: a hand-built fixture models the SSE stream and nothing
// else. toolCount/messageCount exist for this shape, so their count branch needs it to be reachable
// from this package's own suite rather than only across the module boundary.
func projected(events []SessionEvent) []SessionEvent {
	out := make([]SessionEvent, 0, len(events))
	for _, e := range events {
		c := e
		if e.Inference != nil {
			inf := *e.Inference
			inf.MessageCount, inf.ToolCount = len(inf.Messages), len(inf.Tools)
			inf.Messages = nil
			inf.Tools = nil
			inf.ToolCalls = nil
			c.Inference = &inf
		}
		out = append(out, c)
	}
	return out
}
