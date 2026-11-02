package pipeline

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestStatusFromCode(t *testing.T) {
	cases := map[string]int{
		"auth.missing-token":             401,
		"auth.invalid-token":             401,
		"policy.forbidden":               403,
		"policy.rate-limited":            429,
		"upstream.unreachable":           503,
		"upstream.token-exchange-failed": 503,
		"upstream.timeout":               504,
		"pipeline.cancelled":             499,
		"anything.unknown":               500,
		"":                               500,
	}
	for code, want := range cases {
		if got := StatusFromCode(code); got != want {
			t.Errorf("StatusFromCode(%q) = %d, want %d", code, got, want)
		}
	}
}

// Default body synthesis covers the standard fields. This is the contract
// external clients rely on when a plugin uses Deny() without overriding Body.
func TestViolation_Render_DefaultBody(t *testing.T) {
	v := &Violation{
		Code:        "auth.missing-token",
		Reason:      "Bearer required",
		Description: "No Authorization header present",
		Details:     map[string]any{"realm": "rossoctl"},
		PluginName:  "jwt-validation",
	}
	status, headers, body := v.Render()

	if status != 401 {
		t.Errorf("status = %d, want 401", status)
	}
	if ct := headers.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body not valid JSON: %v\n  body: %s", err, body)
	}
	if got["error"] != "auth.missing-token" {
		t.Errorf("error = %v, want auth.missing-token", got["error"])
	}
	if got["message"] != "Bearer required" {
		t.Errorf("message = %v, want Bearer required", got["message"])
	}
	if got["description"] != "No Authorization header present" {
		t.Errorf("description = %v", got["description"])
	}
	if got["plugin"] != "jwt-validation" {
		t.Errorf("plugin = %v", got["plugin"])
	}
	details, ok := got["details"].(map[string]any)
	if !ok {
		t.Fatalf("details not a map: %v", got["details"])
	}
	if details["realm"] != "rossoctl" {
		t.Errorf("details.realm = %v", details["realm"])
	}
}

// A plugin that supplies its own body bytes must have them written
// verbatim, with BodyType used for Content-Type.
func TestViolation_Render_CustomBody(t *testing.T) {
	v := &Violation{
		Code:     "policy.content-blocked",
		Reason:   "blocked",
		Body:     []byte("blocked by policy: rule-42"),
		BodyType: "text/plain",
	}
	status, headers, body := v.Render()
	if status != 403 {
		t.Errorf("status = %d, want 403", status)
	}
	if string(body) != "blocked by policy: rule-42" {
		t.Errorf("body = %q, want verbatim", body)
	}
	if ct := headers.Get("Content-Type"); ct != "text/plain" {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
}

// Optional fields (description, plugin, details) must be omitted from the
// default body when empty so the wire payload stays clean.
func TestViolation_Render_OmitsEmptyOptionals(t *testing.T) {
	v := &Violation{Code: "auth.unauthorized", Reason: "go away"}
	_, _, body := v.Render()
	s := string(body)
	for _, field := range []string{"description", "plugin", "details"} {
		if strings.Contains(s, `"`+field+`":`) {
			t.Errorf("expected %q omitted from %s", field, s)
		}
	}
}

// Headers passed in by the plugin (e.g. WWW-Authenticate) must be
// preserved. Render must clone so the caller's map isn't mutated with
// Content-Type additions.
func TestViolation_Render_PreservesAndClonesHeaders(t *testing.T) {
	// Use http.Header.Set so the key is canonicalized by the stdlib — this
	// mirrors what a real plugin would do via the Challenge() helper.
	origHeaders := http.Header{}
	origHeaders.Set("WWW-Authenticate", `Bearer realm="rossoctl"`)
	v := &Violation{
		Code:    "auth.missing-token",
		Reason:  "bearer required",
		Headers: origHeaders,
	}
	_, h, _ := v.Render()
	if got := h.Get("WWW-Authenticate"); got != `Bearer realm="rossoctl"` {
		t.Errorf("WWW-Authenticate = %q", got)
	}
	if h.Get("Content-Type") == "" {
		t.Error("Content-Type not auto-set from synthesized body")
	}
	// Original must not have Content-Type appended.
	if origHeaders.Get("Content-Type") != "" {
		t.Error("Render mutated caller's headers map")
	}
}

// Explicit Status overrides the code-to-status lookup.
func TestViolation_Render_ExplicitStatusWins(t *testing.T) {
	v := &Violation{Code: "auth.missing-token", Reason: "", Status: 418}
	status, _, _ := v.Render()
	if status != 418 {
		t.Errorf("status = %d, want explicit 418", status)
	}
}

// Render must be safe on a nil receiver — used in error paths where a
// plugin returned Reject with no violation populated.
func TestViolation_Render_NilReceiver(t *testing.T) {
	var v *Violation
	status, h, body := v.Render()
	if status != 500 {
		t.Errorf("status = %d, want 500", status)
	}
	if h == nil {
		t.Error("headers nil on nil receiver")
	}
	if len(body) == 0 {
		t.Error("body empty on nil receiver")
	}
}

// ----------------------------------------------------------------------------
// Helper constructors
// ----------------------------------------------------------------------------

func TestDeny(t *testing.T) {
	a := Deny("auth.invalid-token", "expired")
	if a.Type != Reject || a.Violation == nil {
		t.Fatalf("not a Reject: %+v", a)
	}
	if a.Violation.Code != "auth.invalid-token" || a.Violation.Reason != "expired" {
		t.Errorf("unexpected violation: %+v", a.Violation)
	}
	status, _, _ := a.Violation.Render()
	if status != 401 {
		t.Errorf("derived status = %d", status)
	}
}

func TestDenyStatus(t *testing.T) {
	a := DenyStatus(451, "policy.forbidden", "unavailable for legal reasons")
	status, _, _ := a.Violation.Render()
	if status != 451 {
		t.Errorf("status = %d, want 451", status)
	}
}

func TestDenyWithDetails(t *testing.T) {
	a := DenyWithDetails("policy.rate-limited", "quota hit", map[string]any{
		"quota": 100, "remaining": 0,
	})
	_, _, body := a.Violation.Render()
	if !strings.Contains(string(body), `"details"`) {
		t.Errorf("details missing from body: %s", body)
	}
}

func TestChallenge(t *testing.T) {
	a := Challenge("rossoctl", "Authorization required")
	status, h, _ := a.Violation.Render()
	if status != 401 {
		t.Errorf("status = %d", status)
	}
	if got := h.Get("WWW-Authenticate"); got != `Bearer realm="rossoctl"` {
		t.Errorf("WWW-Authenticate = %q", got)
	}
}

func TestRateLimited(t *testing.T) {
	a := RateLimited(30*time.Second, "", "slow down")
	status, h, _ := a.Violation.Render()
	if status != 429 {
		t.Errorf("status = %d", status)
	}
	if got := h.Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want 30", got)
	}
	if a.Violation.Code != "policy.rate-limited" {
		t.Errorf("default code = %q", a.Violation.Code)
	}
}

// TestMocaRefusal pins the MOCA egress contract's refusal shape byte for
// byte: status, X-Moca-Egress-Error, text/plain, and the body
// "<code>: <name>\n" — what the moca egress conformance suite's wantRefusal
// asserts for every refusal family. A change to any of the four breaks the
// conformance run, so this test is the in-tree stand-in for it.
func TestMocaRefusal(t *testing.T) {
	a := MocaRefusal(403, "destination_denied", "off.test")
	status, h, body := a.Violation.Render()
	if status != 403 {
		t.Errorf("status = %d, want 403", status)
	}
	if got := h.Get("X-Moca-Egress-Error"); got != "destination_denied" {
		t.Errorf("X-Moca-Egress-Error = %q, want destination_denied", got)
	}
	if got := h.Get("Content-Type"); got != "text/plain" {
		t.Errorf("Content-Type = %q, want text/plain", got)
	}
	if want := "destination_denied: off.test\n"; string(body) != want {
		t.Errorf("body = %q, want %q", body, want)
	}
	if a.Type != Reject {
		t.Errorf("Type = %v, want Reject", a.Type)
	}
	// Render must not mutate the receiver's headers — the same action is
	// rendered once for the client and possibly read again for a row.
	if a.Violation.Headers.Get("Content-Type") != "" {
		t.Error("Render leaked a Content-Type into the action's own headers")
	}

	// The body is client-visible reflection of a client-named destination, so
	// a hostile "host" carrying HTML metacharacters is escaped, not echoed: the
	// refusal names the destination without becoming a rendering surface. For
	// every legitimate host — the conformance suite's included — EscapeString
	// is the identity, which is why the case above pins exact bytes.
	hostile := MocaRefusal(403, "destination_denied", `<script>x</script>`)
	_, _, hbody := hostile.Violation.Render()
	if want := "destination_denied: &lt;script&gt;x&lt;/script&gt;\n"; string(hbody) != want {
		t.Errorf("body = %q, want %q — HTML metacharacters must not reach the body raw", hbody, want)
	}
	if hostile.Violation.Reason != `<script>x</script>` {
		t.Error("Reason must stay raw — it never reaches the wire body and is read as data")
	}
}
