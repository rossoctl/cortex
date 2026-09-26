package pipeline

import (
	"testing"
)

// Locking in the SetState / GetState contract so a future refactor of
// Extensions.Custom doesn't break plugins that have been ported to the
// generic helpers. These helpers are the canonical way to attach plugin-
// private per-request state without extending the core Extensions struct.
func TestSetGetState_RoundTrip(t *testing.T) {
	type myState struct {
		TokensAtStart int
		Decision      string
	}

	pctx := &Context{}

	// Get on a fresh pctx returns nil, no panic on nil map.
	if got := GetState[myState](pctx, "rate-limiter"); got != nil {
		t.Errorf("GetState on empty pctx = %+v, want nil", got)
	}

	// Set then get returns the same pointer.
	orig := &myState{TokensAtStart: 100, Decision: "allow"}
	SetState(pctx, "rate-limiter", orig)
	got := GetState[myState](pctx, "rate-limiter")
	if got == nil {
		t.Fatal("GetState returned nil after SetState")
	}
	if got != orig {
		t.Error("GetState returned a different pointer than stored")
	}
	if got.TokensAtStart != 100 || got.Decision != "allow" {
		t.Errorf("GetState value = %+v, want {100 allow}", got)
	}
}

// Mutations through the retrieved pointer must be visible on subsequent
// GetState calls — plugins rely on this to update state between
// OnRequest and OnResponse.
func TestSetGetState_MutationVisible(t *testing.T) {
	type cnt struct{ N int }
	pctx := &Context{}
	SetState(pctx, "k", &cnt{N: 1})
	got := GetState[cnt](pctx, "k")
	got.N = 42
	again := GetState[cnt](pctx, "k")
	if again.N != 42 {
		t.Errorf("after mutation, got.N = %d, want 42", again.N)
	}
}

// A GetState with the wrong type parameter must not panic; it should
// return nil so a buggy consumer degrades rather than crashing the pipeline.
func TestGetState_WrongType(t *testing.T) {
	type a struct{ X int }
	type b struct{ Y string }
	pctx := &Context{}
	SetState(pctx, "k", &a{X: 1})
	if got := GetState[b](pctx, "k"); got != nil {
		t.Errorf("wrong-type GetState = %+v, want nil", got)
	}
}

// Absent keys return nil even when the map has other data.
func TestGetState_MissingKey(t *testing.T) {
	type a struct{ X int }
	pctx := &Context{}
	SetState(pctx, "k1", &a{X: 1})
	if got := GetState[a](pctx, "k2"); got != nil {
		t.Errorf("missing key returned %+v, want nil", got)
	}
}

// Two plugins using distinct keys must see only their own state. This is
// the key property that lets arbitrary plugins land without touching the
// Extensions struct — the Custom map is partitioned by plugin name.
func TestSetGetState_MultiplePlugins(t *testing.T) {
	type rlState struct{ Tokens int }
	type auditState struct{ Hits int }
	pctx := &Context{}
	SetState(pctx, "rate-limiter", &rlState{Tokens: 50})
	SetState(pctx, "audit", &auditState{Hits: 3})

	if rl := GetState[rlState](pctx, "rate-limiter"); rl == nil || rl.Tokens != 50 {
		t.Errorf("rate-limiter state = %+v", rl)
	}
	if au := GetState[auditState](pctx, "audit"); au == nil || au.Hits != 3 {
		t.Errorf("audit state = %+v", au)
	}
}

// --- Classification ---

// No populated extensions → both booleans false. Defense-in-depth
// guardrails (IBAC) treat this as "not our traffic, pass through."
func TestClassification_NoExtensions(t *testing.T) {
	pctx := &Context{}
	anyAction, anyBypass := pctx.Classification()
	if anyAction || anyBypass {
		t.Errorf("Classification() = (%v, %v), want (false, false) on empty pctx",
			anyAction, anyBypass)
	}
}

// MCP populated with IsAction=true → action; default false → bypass.
func TestClassification_MCPAction(t *testing.T) {
	pctx := &Context{}
	pctx.Extensions.MCP = &MCPExtension{Method: "tools/call", IsAction: true}
	anyAction, anyBypass := pctx.Classification()
	if !anyAction || anyBypass {
		t.Errorf("Classification() = (%v, %v), want (true, false)", anyAction, anyBypass)
	}
}

func TestClassification_MCPBypass(t *testing.T) {
	pctx := &Context{}
	pctx.Extensions.MCP = &MCPExtension{Method: "tools/list"} // IsAction=false default
	anyAction, anyBypass := pctx.Classification()
	if anyAction || !anyBypass {
		t.Errorf("Classification() = (%v, %v), want (false, true)", anyAction, anyBypass)
	}
}

// A2A and Inference behave the same way as MCP.
func TestClassification_A2AAndInference(t *testing.T) {
	pctx := &Context{}
	pctx.Extensions.A2A = &A2AExtension{Method: "message/send", IsAction: true}
	pctx.Extensions.Inference = &InferenceExtension{Model: "gpt-4o", IsAction: true}
	anyAction, anyBypass := pctx.Classification()
	if !anyAction || anyBypass {
		t.Errorf("Classification() = (%v, %v), want (true, false) for two action extensions",
			anyAction, anyBypass)
	}
}

// Mixed: one extension says action, another says bypass — both flags
// true. Callers decide their precedence (IBAC chooses bypass-wins).
func TestClassification_Mixed(t *testing.T) {
	pctx := &Context{}
	pctx.Extensions.MCP = &MCPExtension{Method: "tools/call", IsAction: true}
	pctx.Extensions.Inference = &InferenceExtension{Model: "gpt-4o"} // IsAction=false
	anyAction, anyBypass := pctx.Classification()
	if !anyAction || !anyBypass {
		t.Errorf("Classification() = (%v, %v), want (true, true) on mixed classification",
			anyAction, anyBypass)
	}
}
