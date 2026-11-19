package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// TestCapabilities_Normalize: WritesRequestBody auto-promotes to ReadsBody so a
// mutator always satisfies the "must have read" invariant.
func TestCapabilities_Normalize(t *testing.T) {
	tests := []struct {
		name       string
		in         PluginCapabilities
		wantReads  bool
		wantWrites bool
	}{
		{
			name:       "WritesRequestBody implies ReadsBody",
			in:         PluginCapabilities{WritesRequestBody: true},
			wantReads:  true,
			wantWrites: true,
		},
		{
			name:       "explicit ReadsBody passes through",
			in:         PluginCapabilities{ReadsBody: true},
			wantReads:  true,
			wantWrites: false,
		},
		{
			name:      "empty is empty",
			in:        PluginCapabilities{},
			wantReads: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.Normalize()
			if got.ReadsBody != tc.wantReads {
				t.Errorf("ReadsBody = %v, want %v", got.ReadsBody, tc.wantReads)
			}
			if got.WritesRequestBody != tc.wantWrites {
				t.Errorf("WritesRequestBody = %v, want %v", got.WritesRequestBody, tc.wantWrites)
			}
		})
	}
}

// TestContext_SetBody_FlipsFlagAndEmitsInvocation: SetBody is the only
// sanctioned way to mutate pctx.Body; it must (a) actually replace the
// bytes, (b) flip BodyMutated so the listener knows to propagate, and
// (c) record a modify-action Invocation with Reason "body_rewritten".
func TestContext_SetBody_FlipsFlagAndEmitsInvocation(t *testing.T) {
	c := &Context{
		Direction: Inbound,
		Body:      []byte("original"),
	}
	c.SetCurrentPlugin("redactor", InvocationPhaseRequest)

	c.SetBody([]byte("redacted"))

	if string(c.Body) != "redacted" {
		t.Errorf("Body = %q, want redacted", c.Body)
	}
	if !c.BodyMutated() {
		t.Error("BodyMutated should be true after SetBody")
	}
	if c.ResponseBodyMutated() {
		t.Error("ResponseBodyMutated should remain false")
	}

	inv := c.Extensions.Invocations
	if inv == nil || len(inv.Inbound) != 1 {
		t.Fatalf("expected 1 inbound invocation, got %+v", inv)
	}
	got := inv.Inbound[0]
	if got.Action != ActionModify {
		t.Errorf("Action = %v, want modify", got.Action)
	}
	if got.Reason != "body_rewritten" {
		t.Errorf("Reason = %q, want body_rewritten", got.Reason)
	}
	if got.Plugin != "redactor" {
		t.Errorf("Plugin = %q, want redactor (framework attribution)", got.Plugin)
	}
}

// TestContext_SetBody_EmitsCustomEvent: the body-mutation/event custom
// entry must (a) land under the framework-owned key, (b) carry length
// delta + sha256 before/after, (c) never carry the raw body bytes.
func TestContext_SetBody_EmitsCustomEvent(t *testing.T) {
	c := &Context{Direction: Inbound, Body: []byte("original-payload")}
	c.SetCurrentPlugin("redactor", InvocationPhaseRequest)

	c.SetBody([]byte("redacted"))

	raw, ok := c.Extensions.Custom["body-mutation"+PluginEventSuffix]
	if !ok {
		t.Fatalf("expected body-mutation event in Custom map; got keys: %v", keys(c.Extensions.Custom))
	}
	ev, ok := raw.(bodyMutationEvent)
	if !ok {
		t.Fatalf("event type = %T, want bodyMutationEvent", raw)
	}
	if ev.Phase != "request" {
		t.Errorf("Phase = %q, want request", ev.Phase)
	}
	if ev.Plugin != "redactor" {
		t.Errorf("Plugin = %q, want redactor", ev.Plugin)
	}
	if ev.LengthBefore != len("original-payload") {
		t.Errorf("LengthBefore = %d, want %d", ev.LengthBefore, len("original-payload"))
	}
	if ev.LengthAfter != len("redacted") {
		t.Errorf("LengthAfter = %d, want %d", ev.LengthAfter, len("redacted"))
	}
	// Verify sha256 is a real hash of the raw bytes, not garbage.
	wantBefore := sha256.Sum256([]byte("original-payload"))
	if ev.SHA256Before != hex.EncodeToString(wantBefore[:]) {
		t.Errorf("SHA256Before hash mismatch")
	}
	wantAfter := sha256.Sum256([]byte("redacted"))
	if ev.SHA256After != hex.EncodeToString(wantAfter[:]) {
		t.Errorf("SHA256After hash mismatch")
	}
}

// TestContext_SetResponseBody_PhaseLabel: response-side mutation
// reports Phase "response" in the custom event so operators can tell
// request-side redactions (prompt sanitization) from response-side
// redactions (LLM output filtering) in agentop.
func TestContext_SetResponseBody_PhaseLabel(t *testing.T) {
	c := &Context{
		Direction:    Outbound,
		ResponseBody: []byte("llm completion"),
	}
	c.SetCurrentPlugin("llm-guardrail", InvocationPhaseResponse)

	c.SetResponseBody([]byte("[redacted]"))

	if !c.ResponseBodyMutated() {
		t.Error("ResponseBodyMutated should be true")
	}
	if c.BodyMutated() {
		t.Error("BodyMutated (request side) should remain false")
	}
	ev := c.Extensions.Custom["body-mutation"+PluginEventSuffix].(bodyMutationEvent)
	if ev.Phase != "response" {
		t.Errorf("Phase = %q, want response", ev.Phase)
	}
}

// --- helpers --------------------------------------------------------

func mustBuild(t *testing.T, ps ...Plugin) *Pipeline {
	t.Helper()
	p, err := New(ps)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	return p
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
