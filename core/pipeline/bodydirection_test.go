package pipeline

import (
	"strings"
	"testing"
)

// TestCapabilities_Normalize_EitherWriteImpliesReadsBody: both write flags
// promote ReadsBody, so a mutator of either direction always satisfies the
// "must have read the body" invariant.
func TestCapabilities_Normalize_EitherWriteImpliesReadsBody(t *testing.T) {
	tests := []struct {
		name string
		in   PluginCapabilities
	}{
		{"request writer", PluginCapabilities{WritesRequestBody: true}},
		{"response writer", PluginCapabilities{WritesResponseBody: true}},
		{"both", PluginCapabilities{WritesRequestBody: true, WritesResponseBody: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.in.Normalize().ReadsBody {
				t.Error("Normalize() should promote ReadsBody")
			}
		})
	}
	if (PluginCapabilities{}).Normalize().ReadsBody {
		t.Error("empty capabilities must not gain ReadsBody")
	}
}

// TestPipeline_BodyWritePredicates_TruthTable pins the two predicates across
// all four plugin shapes. WritesResponseBody is the SSE streaming predicate,
// so a request-only writer reporting true here would silently cost every
// caller incremental relay — the exact defect this split fixes.
func TestPipeline_BodyWritePredicates_TruthTable(t *testing.T) {
	tests := []struct {
		name              string
		caps              PluginCapabilities
		wantReq, wantResp bool
		wantNeedsBody     bool
	}{
		{
			name:          "request-only writer (tool-prune, context-guru)",
			caps:          PluginCapabilities{WritesRequestBody: true},
			wantReq:       true,
			wantResp:      false,
			wantNeedsBody: true,
		},
		{
			name:          "response-only writer",
			caps:          PluginCapabilities{WritesResponseBody: true},
			wantReq:       false,
			wantResp:      true,
			wantNeedsBody: true,
		},
		{
			name:          "both directions (sparc, cpex)",
			caps:          PluginCapabilities{WritesRequestBody: true, WritesResponseBody: true},
			wantReq:       true,
			wantResp:      true,
			wantNeedsBody: true,
		},
		{
			name:          "neither (pure reader)",
			caps:          PluginCapabilities{ReadsBody: true},
			wantReq:       false,
			wantResp:      false,
			wantNeedsBody: true,
		},
		{
			name:          "neither, no body at all",
			caps:          PluginCapabilities{},
			wantReq:       false,
			wantResp:      false,
			wantNeedsBody: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := mustBuild(t, &stubPlugin{name: "p", caps: tc.caps})
			if got := p.WritesRequestBody(); got != tc.wantReq {
				t.Errorf("WritesRequestBody() = %v, want %v", got, tc.wantReq)
			}
			if got := p.WritesResponseBody(); got != tc.wantResp {
				t.Errorf("WritesResponseBody() = %v, want %v", got, tc.wantResp)
			}
			if got := p.NeedsBody(); got != tc.wantNeedsBody {
				t.Errorf("NeedsBody() = %v, want %v", got, tc.wantNeedsBody)
			}
		})
	}
}

// TestValidateCapabilities_Directional: the mutator-exclusivity rule is
// per-direction, and reader-ordering is triggered by either write flag.
// Crucially, every combination that exists in-tree today validates exactly
// as it did before the split.
func TestValidateCapabilities_Directional(t *testing.T) {
	req := PluginCapabilities{WritesRequestBody: true}
	resp := PluginCapabilities{WritesResponseBody: true}
	both := PluginCapabilities{WritesRequestBody: true, WritesResponseBody: true}
	reader := PluginCapabilities{ReadsBody: true}

	tests := []struct {
		name    string
		plugins []Plugin
		wantErr string
	}{
		{
			name:    "two request writers rejected",
			plugins: []Plugin{&stubPlugin{name: "a", caps: req}, &stubPlugin{name: "b", caps: req}},
			wantErr: "WritesRequestBody",
		},
		{
			name:    "two response writers rejected",
			plugins: []Plugin{&stubPlugin{name: "a", caps: resp}, &stubPlugin{name: "b", caps: resp}},
			wantErr: "WritesResponseBody",
		},
		{
			name:    "one of each direction is fine — they never collide",
			plugins: []Plugin{&stubPlugin{name: "a", caps: req}, &stubPlugin{name: "b", caps: resp}},
		},
		{
			name:    "two both-direction writers rejected on the request rule first",
			plugins: []Plugin{&stubPlugin{name: "a", caps: both}, &stubPlugin{name: "b", caps: both}},
			wantErr: "WritesRequestBody",
		},
		{
			name:    "reader before mutator is fine",
			plugins: []Plugin{&stubPlugin{name: "r", caps: reader}, &stubPlugin{name: "m", caps: req}},
		},
		{
			name:    "reader after request mutator rejected",
			plugins: []Plugin{&stubPlugin{name: "m", caps: req}, &stubPlugin{name: "r", caps: reader}},
			wantErr: "reads body after mutator",
		},
		{
			name:    "reader after response mutator rejected too",
			plugins: []Plugin{&stubPlugin{name: "m", caps: resp}, &stubPlugin{name: "r", caps: reader}},
			wantErr: "reads body after mutator",
		},
		{
			name:    "today's shape: parser then single both-direction mutator",
			plugins: []Plugin{&stubPlugin{name: "inference-parser", caps: reader}, &stubPlugin{name: "sparc", caps: both}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCapabilities(tc.plugins)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateCapabilities() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateCapabilities() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestValidateCapabilities_ResponseAndRequestMutatorsCoexist is the payoff the
// directional split was arguing for. Before it, SPARC's undirected flag occupied
// the only mutator slot, so a request-only mutator could not share a chain with
// it even though the two write different bodies. Now the real in-tree shape —
// parser, response mutator, request mutator — builds.
func TestValidateCapabilities_ResponseAndRequestMutatorsCoexist(t *testing.T) {
	err := validateCapabilities([]Plugin{
		&stubPlugin{name: "inference-parser", caps: PluginCapabilities{ReadsBody: true}},
		&stubPlugin{name: "sparc", caps: PluginCapabilities{WritesResponseBody: true}},
		&stubPlugin{name: "tool-prune", caps: PluginCapabilities{WritesRequestBody: true}},
	})
	if err != nil {
		t.Errorf("[parser, sparc, tool-prune] should build: %v", err)
	}
	// Two mutators on the SAME side are still rejected.
	if err := validateCapabilities([]Plugin{
		&stubPlugin{name: "sparc", caps: PluginCapabilities{WritesResponseBody: true}},
		&stubPlugin{name: "cpex", caps: PluginCapabilities{WritesResponseBody: true}},
	}); err == nil {
		t.Error("two response mutators must still be rejected")
	}
}

// TestNeedsBody_DirectionalDoesNotCrossContaminate: the undirected NeedsBody made
// each write flag force the other direction's buffering — a response-only mutator
// had the request body buffered for nothing, and a request-only mutator had
// non-SSE responses buffered for nothing. That is the mirror image of the waste
// the directional capabilities exist to remove.
func TestNeedsBody_DirectionalDoesNotCrossContaminate(t *testing.T) {
	tests := []struct {
		name             string
		caps             PluginCapabilities
		wantReq, wantRsp bool
	}{
		{"request-only mutator", PluginCapabilities{WritesRequestBody: true}, true, false},
		{"response-only mutator", PluginCapabilities{WritesResponseBody: true}, false, true},
		{"both", PluginCapabilities{WritesRequestBody: true, WritesResponseBody: true}, true, true},
		// ReadsBody is itself undirected, so it must still count for both.
		{"pure reader", PluginCapabilities{ReadsBody: true}, true, true},
		{"neither", PluginCapabilities{}, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := mustBuild(t, &stubPlugin{name: "p", caps: tc.caps})
			if got := p.NeedsRequestBody(); got != tc.wantReq {
				t.Errorf("NeedsRequestBody() = %v, want %v", got, tc.wantReq)
			}
			if got := p.NeedsResponseBody(); got != tc.wantRsp {
				t.Errorf("NeedsResponseBody() = %v, want %v", got, tc.wantRsp)
			}
			// The aggregate stays the OR, for callers that need either.
			if got := p.NeedsBody(); got != (tc.wantReq || tc.wantRsp) {
				t.Errorf("NeedsBody() = %v, want %v", got, tc.wantReq || tc.wantRsp)
			}
		})
	}
}
