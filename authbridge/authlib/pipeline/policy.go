package pipeline

// ErrorPolicy controls how the pipeline reacts when a plugin returns a
// Reject action (i.e., decides to deny a request or response). The
// policy wraps the plugin — plugin authors do not consume it.
//
// Default is ErrorPolicyEnforce: Reject becomes an HTTP error response
// and pipeline execution stops. Operators roll out a new guardrail in
// ErrorPolicyObserve first: the plugin still evaluates and may still
// return Reject, but the framework converts the Reject into a
// pass-through and records the would-have-denied Invocation as
// Shadow=true. ErrorPolicyOff is a kill-switch — the plugin is
// not dispatched at all.
//
// Reserved built-in gates (jwt-validation, token-exchange) are locked
// to ErrorPolicyEnforce: shadowing auth is an authentication bypass
// dressed as a feature. plugins.BuildChain validates this at startup.
type ErrorPolicy string

const (
	// ErrorPolicyEnforce is the default. A Reject action becomes the
	// HTTP error the Violation describes, and the pipeline stops.
	ErrorPolicyEnforce ErrorPolicy = "enforce"

	// ErrorPolicyObserve (shadow mode) evaluates the plugin normally
	// but turns a Reject into a pass-through. Used to canary a new
	// guardrail: operators watch the shadow-deny counter before
	// flipping to enforce. Body mutations (SetBody / SetResponseBody)
	// also do not propagate to the wire under observe — the plugin's
	// decision logic runs identically, but its effect is muted.
	ErrorPolicyObserve ErrorPolicy = "observe"

	// ErrorPolicyOff disables the plugin. The plugin is not dispatched.
	// Off is an operator kill-switch without a redeploy.
	ErrorPolicyOff ErrorPolicy = "off"
)

// Valid reports whether p is a recognized ErrorPolicy. The empty
// string is valid and means "use the default" (enforce); callers that
// need the defaulted value use the Resolved method instead.
func (p ErrorPolicy) Valid() bool {
	switch p {
	case "", ErrorPolicyEnforce, ErrorPolicyObserve, ErrorPolicyOff:
		return true
	default:
		return false
	}
}

// Resolved returns the policy with "" normalized to ErrorPolicyEnforce.
// Callers that dispatch on policy value should use Resolved so they
// don't have to treat "" as a synonym for "enforce" themselves.
func (p ErrorPolicy) Resolved() ErrorPolicy {
	if p == "" {
		return ErrorPolicyEnforce
	}
	return p
}
