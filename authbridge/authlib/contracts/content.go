// Package contracts defines capability interfaces that protocol
// extensions implement to participate in framework services like content
// inspection, session bucketing, and summarization. The package is
// deliberately dependency-free — parser plugins and consumer plugins
// (guardrails) both import it without importing each other, so no
// implicit coupling forms between producers and consumers of a
// capability.
//
// Capability interfaces are checked by consumers via type assertion. A
// protocol extension that doesn't implement one simply isn't offered
// that capability; guardrails skip it.
package contracts

// ContentSource is implemented by protocol extensions whose payload
// contains user-visible text that guardrails might inspect — PII
// scrubbing, jailbreak detection, content classification, credential
// leakage scanning, and similar content-oriented checks.
//
// Usage on the consumer side:
//
//	for _, src := range pctx.ContentSources() {
//	    for _, f := range src.Fragments() {
//	        if f.Role == contracts.RoleUser {
//	            scan(f.Text)
//	        }
//	    }
//	}
//
// The guardrail does not import any parser package — it knows only
// this contract. When a new protocol ships with a Fragments
// implementation, existing guardrails pick it up automatically.
//
// Protocols that carry no inspectable text (control-plane RPCs like
// MCP initialize, binary protocols, identity-only auth messages) simply
// do not implement this interface.
type ContentSource interface {
	// Fragments returns every inspectable text fragment on the current
	// request or response phase, in document order. Returns nil or an
	// empty slice when there's nothing to inspect at this phase.
	// Fragments with empty Text are filtered by the producer.
	Fragments() []Fragment
}

// Fragment is one inspectable piece of text, tagged with the role that
// produced it.
type Fragment struct {
	// Role identifies who produced the text. Use the standard values
	// below when the semantic fit is clear; protocols that don't fit
	// any standard value may emit their own role strings.
	Role string

	// Text is the raw text content. Never empty — producers filter
	// empty fragments before returning.
	Text string
}

// Standard Role values. Parsers reference these instead of string
// literals so spelling is compiler-checked. Guardrails may compare
// against either the constant or a string literal — the over-the-wire
// representation is the same.
//
//   - RoleUser: end-user input, human-authored.
//   - RoleAssistant: model or agent output.
//   - RoleSystem: system prompt or instructions to the model.
//   - RoleTool: the name of a tool being invoked.
//   - RoleToolArgs: an argument value passed to a tool invocation.
//   - RoleToolResult: content returned from a tool invocation.
//
// The vocabulary is open. A protocol that invents a role outside this
// list is valid; guardrails that don't recognize a role treat it per
// their own policy (typically: scan anyway, or skip).
const (
	RoleUser       = "user"
	RoleAssistant  = "assistant"
	RoleSystem     = "system"
	RoleTool       = "tool"
	RoleToolArgs   = "tool_args"
	RoleToolResult = "tool_result"
)
