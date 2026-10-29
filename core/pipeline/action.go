package pipeline

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"time"
)

// ActionType represents the result of a plugin's processing.
type ActionType int

const (
	// Continue the pipeline to the next plugin.
	Continue ActionType = iota
	// Reject the request; the listener synthesizes an HTTP response from
	// the accompanying Violation.
	Reject
)

// Action is returned by a plugin to indicate how the pipeline should proceed.
// For Continue, Violation is nil. For Reject, Violation is populated (either
// by the plugin itself or by a helper constructor such as Deny / Challenge).
type Action struct {
	Type      ActionType
	Violation *Violation
}

// Violation is the structured denial a plugin returns when it rejects a
// request or response. The shape is intentionally close to CPEX's
// PluginViolation so bridges and cross-framework tooling can reason about
// authbridge and CPEX denials uniformly: Code is machine-readable, Reason
// is a short human message, Description is a longer explanation, Details
// is plugin-arbitrary structured context.
//
// HTTP-rendering hints (Status, Body, BodyType, Headers) are optional
// overrides — when empty, Render() synthesizes sensible defaults from Code,
// Reason, Description, Details, and PluginName.
type Violation struct {
	// Structured fields — the authoritative description of the denial.
	// Plugins that only care about semantics (not wire shape) set just
	// Code and Reason; listeners and clients can inspect the rest.
	Code        string         // machine-readable error code, e.g. "auth.missing-token"
	Reason      string         // short human-readable message
	Description string         // longer explanation; optional
	Details     map[string]any // structured context; optional

	// HTTP rendering hints — all optional. When empty, Render() picks
	// defaults. Plugins set these only when they need to override.
	Status   int         // HTTP status; if 0, StatusFromCode(Code) is used
	Body     []byte      // response body; if nil, synthesized JSON
	BodyType string      // Content-Type for Body; defaults to application/json
	Headers  http.Header // merged into response headers

	// RPCCode is the JSON-RPC error code when the rejection is rendered
	// as an MCP error frame; 0 means the generic -32000. Set it only for
	// a code a client acts on, such as a retry signal.
	RPCCode int

	// PluginName is populated by the pipeline framework from Plugin.Name()
	// when the Reject is returned. Plugins should leave this empty.
	PluginName string
}

// StatusClientClosedRequest is nginx's 499, for a request the client abandoned
// before it was answered. No client ever receives it — it has gone — which is
// what makes it useful in a record: it keeps a hangup out of the 5xx series that
// an upstream outage shows up in.
const StatusClientClosedRequest = 499

// codeToStatus maps well-known violation codes to HTTP status. Plugins
// introducing new codes should either populate Violation.Status explicitly
// or the host can extend this table (future: make this configurable).
var codeToStatus = map[string]int{
	"auth.missing-token":             http.StatusUnauthorized,
	"auth.invalid-token":             http.StatusUnauthorized,
	"auth.audience-mismatch":         http.StatusUnauthorized,
	"auth.unauthorized":              http.StatusUnauthorized,
	"policy.forbidden":               http.StatusForbidden,
	"policy.rate-limited":            http.StatusTooManyRequests,
	"policy.content-blocked":         http.StatusForbidden,
	"upstream.unreachable":           http.StatusServiceUnavailable,
	"upstream.token-exchange-failed": http.StatusServiceUnavailable,
	"upstream.timeout":               http.StatusGatewayTimeout,
	"pipeline.cancelled":             StatusClientClosedRequest,
	"budget.exceeded":                http.StatusForbidden,
}

// StatusFromCode returns the HTTP status a Violation maps to when its
// Status field is unset. Falls back to 500 for unknown codes.
func StatusFromCode(code string) int {
	if s, ok := codeToStatus[code]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// Render produces the HTTP response triple (status, headers, body) for a
// Violation. Called by each listener's reject path so the translation
// from structured Violation to wire bytes lives in one place.
//
// Defaults when fields are unset:
//   - Status: StatusFromCode(Code), or 500 for unknown codes
//   - Body: JSON {"error":Code,"message":Reason, plus description/plugin/details when non-empty}
//   - BodyType: application/json
//   - Content-Type header: set from BodyType if not already present in Headers
//
// Render does not mutate the receiver — it's safe to call multiple times.
// When Headers is nil, a fresh map is allocated.
func (v *Violation) Render() (int, http.Header, []byte) {
	if v == nil {
		return http.StatusInternalServerError, http.Header{}, []byte(`{"error":"internal","message":"reject without violation"}`)
	}
	status := v.Status
	if status == 0 {
		status = StatusFromCode(v.Code)
	}
	body := v.Body
	bodyType := v.BodyType
	if body == nil {
		payload := map[string]any{
			"error":   v.Code,
			"message": v.Reason,
		}
		if v.Description != "" {
			payload["description"] = v.Description
		}
		if v.PluginName != "" {
			payload["plugin"] = v.PluginName
		}
		if len(v.Details) > 0 {
			payload["details"] = v.Details
		}
		body, _ = json.Marshal(payload)
		bodyType = "application/json"
	}
	headers := v.Headers
	if headers == nil {
		headers = http.Header{}
	} else {
		// Clone so the caller's headers are not mutated.
		cloned := make(http.Header, len(headers))
		for k, vs := range headers {
			cloned[k] = append([]string(nil), vs...)
		}
		headers = cloned
	}
	if bodyType != "" && headers.Get("Content-Type") == "" {
		headers.Set("Content-Type", bodyType)
	}
	return status, headers, body
}

// ----------------------------------------------------------------------------
// Helper constructors — the 95% cases. Use these over manually constructing
// Action{Type: Reject, Violation: &Violation{...}} for readability.
// ----------------------------------------------------------------------------

// Deny returns a Reject action with a code and short reason. HTTP status
// is derived from the code via StatusFromCode.
func Deny(code, reason string) Action {
	return Action{Type: Reject, Violation: &Violation{Code: code, Reason: reason}}
}

// DenyStatus overrides the HTTP status inferred from Code. Use when the
// code-to-status default doesn't match the caller's intent (e.g. a policy
// plugin that wants a 451 Unavailable For Legal Reasons).
func DenyStatus(status int, code, reason string) Action {
	return Action{Type: Reject, Violation: &Violation{
		Code: code, Reason: reason, Status: status,
	}}
}

// MocaRefusal builds the MOCA egress contract's refusal shape: a Reject
// whose rendered response is the status, the header
// "X-Moca-Egress-Error: <code>", Content-Type text/plain, and the body
// "<code>: <name>\n" — exactly what the moca egress conformance suite's
// wantRefusal asserts for every refusal family (destination_denied,
// destination_private, plaintext_credential, conn_cap,
// upstream_unreachable). name is family-specific: destination_denied names
// the host, conn_cap the VM, upstream_unreachable the host.
//
// "X-Moca-Egress-Error" is that contract's wire identifier, used verbatim
// for the same reason x-authbridge-* is: another system's harness matches
// it byte for byte, and a "nicer" spelling here breaks the conformance run.
//
// Because the Violation carries a pre-rendered body, Violation.Render emits
// it unchanged and every listener's WriteRejection delivers the contract
// shape as-is. Do NOT route one of these through WriteRejectionForRequest
// on a path where the named refusal IS the answer: the MCP JSON-RPC 200
// rewrite would hide it behind a protocol frame.
//
// name is HTML-escaped into the body. Every name the contract defines — a
// host, a VM id — is drawn from a character set EscapeString never touches,
// so the conformance bytes are identical for every legitimate input; the
// escape only bites on a hostile "host" carrying HTML metacharacters, which
// no valid destination can. The body is client-visible reflection, so it
// gets the same neutralisation any reflected value owes (CodeQL's reflected-
// XSS query, the high-severity alert this keeps closed), and the refusal's
// header stays the raw code, which is a constant of the contract.
func MocaRefusal(status int, code, name string) Action {
	h := http.Header{}
	h.Set("X-Moca-Egress-Error", code)
	return Action{Type: Reject, Violation: &Violation{
		Status:   status,
		Code:     code,
		Reason:   name,
		Headers:  h,
		Body:     []byte(code + ": " + html.EscapeString(name) + "\n"),
		BodyType: "text/plain",
	}}
}

// DenyWithDetails attaches plugin-arbitrary structured context. The
// details map surfaces as a "details" object in the default JSON body
// and is available to clients parsing the error programmatically.
func DenyWithDetails(code, reason string, details map[string]any) Action {
	return Action{Type: Reject, Violation: &Violation{
		Code: code, Reason: reason, Details: details,
	}}
}

// Challenge returns a 401 with a Bearer challenge so clients know to
// present credentials. The realm appears in the WWW-Authenticate header
// per RFC 6750.
func Challenge(realm, reason string) Action {
	h := http.Header{}
	h.Set("WWW-Authenticate", fmt.Sprintf("Bearer realm=%q", realm))
	return Action{Type: Reject, Violation: &Violation{
		Code:    "auth.missing-token",
		Reason:  reason,
		Headers: h,
	}}
}

// RateLimited returns a 429 with a Retry-After header expressed in
// seconds. Code defaults to "policy.rate-limited" when empty so a plugin
// can call RateLimited(30*time.Second, "", "slow down") without ceremony.
func RateLimited(retryAfter time.Duration, code, reason string) Action {
	if code == "" {
		code = "policy.rate-limited"
	}
	h := http.Header{}
	h.Set("Retry-After", fmt.Sprintf("%d", int(retryAfter.Seconds())))
	return Action{Type: Reject, Violation: &Violation{
		Code: code, Reason: reason, Headers: h,
	}}
}
