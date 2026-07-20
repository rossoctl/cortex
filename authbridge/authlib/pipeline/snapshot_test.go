package pipeline

import (
	"encoding/json"
	"testing"
	"time"
)

// TestSnapshotA2A_PointerAliasing protects the no-aliasing contract:
// after Snapshot, mutating the source must not affect the snapshot. The
// proxy listeners depend on this so a request-phase event isn't
// retroactively rewritten when OnResponse mutates the same A2A struct
// (the symptom that triggered this work).
func TestSnapshotA2A_PointerAliasing(t *testing.T) {
	src := &A2AExtension{
		Method:      "message/stream",
		SessionID:   "ctx-1",
		FinalStatus: "",
		Artifact:    "",
	}
	snap := SnapshotA2A(src)

	// Mutate source after snapshot — common pattern: parser stamps
	// SessionID + final fields on the live ext during OnResponse.
	src.SessionID = "ctx-2"
	src.FinalStatus = "completed"
	src.Artifact = "the answer is 42"

	if snap.SessionID != "ctx-1" {
		t.Errorf("SessionID = %q, want %q (snapshot retroactively mutated)", snap.SessionID, "ctx-1")
	}
	if snap.FinalStatus != "" {
		t.Errorf("FinalStatus = %q, want empty (snapshot picked up response-phase mutation)", snap.FinalStatus)
	}
	if snap.Artifact != "" {
		t.Errorf("Artifact = %q, want empty (snapshot picked up response-phase mutation)", snap.Artifact)
	}
}

func TestSnapshotA2A_Nil(t *testing.T) {
	if got := SnapshotA2A(nil); got != nil {
		t.Errorf("SnapshotA2A(nil) = %v, want nil", got)
	}
}

func TestSnapshotMCP_PointerAliasing(t *testing.T) {
	src := &MCPExtension{Method: "tools/call"}
	snap := SnapshotMCP(src)

	// Result map is assigned on the live ext during OnResponse.
	src.Result = map[string]any{"content": "tool output"}
	src.Err = &MCPError{Code: -32000, Message: "boom"}

	if snap.Result != nil {
		t.Errorf("Result = %v, want nil (snapshot picked up response-phase Result)", snap.Result)
	}
	if snap.Err != nil {
		t.Errorf("Err = %v, want nil (snapshot picked up response-phase Err)", snap.Err)
	}
}

func TestSnapshotMCP_Nil(t *testing.T) {
	if got := SnapshotMCP(nil); got != nil {
		t.Errorf("SnapshotMCP(nil) = %v, want nil", got)
	}
}

func TestSnapshotInference_PointerAliasing(t *testing.T) {
	src := &InferenceExtension{Model: "llama3.2"}
	snap := SnapshotInference(src)

	// Completion + token counts are assigned on the live ext during OnResponse.
	src.Completion = "the answer is 42"
	src.PromptTokens = 100
	src.CompletionTokens = 25

	if snap.Completion != "" {
		t.Errorf("Completion = %q, want empty (snapshot picked up response-phase mutation)", snap.Completion)
	}
	if snap.PromptTokens != 0 {
		t.Errorf("PromptTokens = %d, want 0 (snapshot picked up response-phase mutation)", snap.PromptTokens)
	}
	if snap.CompletionTokens != 0 {
		t.Errorf("CompletionTokens = %d, want 0 (snapshot picked up response-phase mutation)", snap.CompletionTokens)
	}
}

func TestSnapshotInference_Nil(t *testing.T) {
	if got := SnapshotInference(nil); got != nil {
		t.Errorf("SnapshotInference(nil) = %v, want nil", got)
	}
}

// stubIdentity is a minimal pipeline.Identity implementation for tests.
type stubIdentity struct {
	subject  string
	clientID string
	scopes   []string
}

func (s stubIdentity) Subject() string  { return s.subject }
func (s stubIdentity) ClientID() string { return s.clientID }
func (s stubIdentity) Scopes() []string { return s.scopes }

func TestSnapshotIdentity_NoIdentity(t *testing.T) {
	pctx := &Context{}
	if got := SnapshotIdentity(pctx); got != nil {
		t.Errorf("SnapshotIdentity(empty pctx) = %v, want nil", got)
	}
}

func TestSnapshotIdentity_FromIdentity(t *testing.T) {
	pctx := &Context{
		Identity: stubIdentity{
			subject:  "alice",
			clientID: "rossoctl-ui",
			scopes:   []string{"openid", "agent-aud"},
		},
	}
	got := SnapshotIdentity(pctx)
	if got == nil {
		t.Fatal("SnapshotIdentity returned nil despite populated Identity")
	}
	if got.Subject != "alice" || got.ClientID != "rossoctl-ui" {
		t.Errorf("Subject/ClientID = %q/%q, want alice/rossoctl-ui", got.Subject, got.ClientID)
	}
	if len(got.Scopes) != 2 || got.Scopes[0] != "openid" {
		t.Errorf("Scopes = %v, want [openid agent-aud]", got.Scopes)
	}

	// Mutating the snapshot's Scopes slice must not bleed back to the
	// caller's identity (we copy the slice).
	got.Scopes[0] = "ZAP"
	if pctx.Identity.Scopes()[0] != "openid" {
		t.Error("snapshot mutation bled back to source Identity.Scopes")
	}
}

func TestSnapshotIdentity_FromAgent(t *testing.T) {
	pctx := &Context{Agent: &AgentIdentity{WorkloadID: "team1/weather-agent"}}
	got := SnapshotIdentity(pctx)
	if got == nil {
		t.Fatal("SnapshotIdentity returned nil despite populated Agent")
	}
	if got.AgentID != "team1/weather-agent" {
		t.Errorf("AgentID = %q, want team1/weather-agent", got.AgentID)
	}
}

func TestSnapshotPlugins_Empty(t *testing.T) {
	if got := SnapshotPlugins(nil); got != nil {
		t.Errorf("SnapshotPlugins(nil) = %v, want nil", got)
	}
	if got := SnapshotPlugins(map[string]any{}); got != nil {
		t.Errorf("SnapshotPlugins(empty) = %v, want nil", got)
	}
}

func TestSnapshotPlugins_FilterAndStripSuffix(t *testing.T) {
	custom := map[string]any{
		"rate-limiter" + PluginEventSuffix:  map[string]any{"remaining": 42},
		"audit" + PluginEventSuffix:         "logged",
		"some-internal-key":                 "should-not-appear", // no /event suffix
		"more.internal.state":               struct{ X int }{X: 1},
	}
	got := SnapshotPlugins(custom)

	if _, ok := got["some-internal-key"]; ok {
		t.Error("non-/event key leaked into Plugins map")
	}
	if _, ok := got["more.internal.state"]; ok {
		t.Error("non-/event key leaked into Plugins map")
	}
	if _, ok := got["rate-limiter"]; !ok {
		t.Error("rate-limiter key missing (suffix should be stripped)")
	}
	if _, ok := got["audit"]; !ok {
		t.Error("audit key missing (suffix should be stripped)")
	}

	// Round-trip the rate-limiter value and verify it preserved structure.
	var rl struct {
		Remaining int `json:"remaining"`
	}
	if err := json.Unmarshal(got["rate-limiter"], &rl); err != nil {
		t.Fatalf("unmarshal rate-limiter: %v", err)
	}
	if rl.Remaining != 42 {
		t.Errorf("Remaining = %d, want 42", rl.Remaining)
	}
}

func TestSnapshotPlugins_SkipsUnmarshalable(t *testing.T) {
	// channels can't json.Marshal — should be skipped, not abort recording.
	custom := map[string]any{
		"goodplugin" + PluginEventSuffix: map[string]string{"ok": "yes"},
		"badplugin" + PluginEventSuffix:  make(chan int),
	}
	got := SnapshotPlugins(custom)
	if _, ok := got["badplugin"]; ok {
		t.Error("non-marshalable value should have been skipped")
	}
	if _, ok := got["goodplugin"]; !ok {
		t.Error("good plugin event was dropped along with the bad one — should be independent")
	}
}

func TestDeriveError_Blocked(t *testing.T) {
	pctx := &Context{
		Extensions: Extensions{
			Security: &SecurityExtension{Blocked: true, BlockReason: "PII detected"},
		},
	}
	got := DeriveError(pctx)
	if got == nil {
		t.Fatal("DeriveError returned nil for Blocked=true")
	}
	if got.Kind != "blocked" || got.Message != "PII detected" {
		t.Errorf("Kind/Message = %q/%q, want blocked/PII detected", got.Kind, got.Message)
	}
}

func TestDeriveError_Non2xx(t *testing.T) {
	pctx := &Context{StatusCode: 502}
	got := DeriveError(pctx)
	if got == nil {
		t.Fatal("DeriveError returned nil for StatusCode=502")
	}
	if got.Kind != "backend_error" || got.Code != "502" {
		t.Errorf("Kind/Code = %q/%q, want backend_error/502", got.Kind, got.Code)
	}
}

func TestDeriveError_2xx(t *testing.T) {
	pctx := &Context{StatusCode: 200}
	if got := DeriveError(pctx); got != nil {
		t.Errorf("DeriveError(200) = %+v, want nil", got)
	}
}

func TestDeriveError_BlockedTakesPrecedenceOverStatusCode(t *testing.T) {
	// A guardrail block returning 200 still produces a "blocked" error —
	// the failure mode is policy, not the eventual HTTP status.
	pctx := &Context{
		StatusCode: 200,
		Extensions: Extensions{Security: &SecurityExtension{Blocked: true, BlockReason: "policy"}},
	}
	got := DeriveError(pctx)
	if got == nil || got.Kind != "blocked" {
		t.Errorf("expected blocked error to win over 200 StatusCode, got %+v", got)
	}
}

func TestDurationSince_ZeroStart(t *testing.T) {
	if got := DurationSince(time.Time{}); got != 0 {
		t.Errorf("DurationSince(zero) = %v, want 0", got)
	}
}

func TestDurationSince_ValidStart(t *testing.T) {
	start := time.Now().Add(-50 * time.Millisecond)
	got := DurationSince(start)
	if got < 40*time.Millisecond || got > 5*time.Second {
		t.Errorf("DurationSince(50ms ago) = %v, want roughly ≥40ms", got)
	}
}
