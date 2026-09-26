package opa

import (
	"net/http"
	"testing"

	"github.com/open-policy-agent/opa/v1/ast"

	"github.com/rossoctl/cortex/core/pipeline"
)

// A tool's JSON schema must reach rego as an OBJECT, not as a string.
//
// WHY THIS TEST EXISTS: the neighbouring TestBuildInput_Inference_WithToolsDetail only
// asserts that the "parameters" KEY is present, which stays true no matter what type the
// value has. That is the whole failure mode worth guarding. Every policy that reads a
// schema does so by indexing into it — input.inference.tools[_].parameters.properties —
// and against a string that expression is simply undefined: no error, no log line, the
// rule just never fires and the request is allowed or denied for the wrong reason.
//
// The conversion asserted here is the real one. sdk.DecisionOptions is a type alias over
// v1/sdk (opa/sdk/opa.go), whose Decision calls ast.InterfaceToValue on the input
// (v1/sdk/opa.go:636) — so InterfaceToValue is the exact boundary a mis-typed field would
// cross, and it is what this test drives rather than a stand-in for it.
//
// InterfaceToValue's type switch (v1/ast/term.go) is what makes the field's Go type load
// bearing: it has a `case string:` arm ahead of its `default:`, so anything Go considers
// a plain string is converted as a JSON string. Only the default arm round-trips a value
// through encoding/json and re-expands it into an object. A named string type with a
// MarshalJSON method lands in default and survives; a plain string, or a type ALIASED to
// string, does not.
func TestBuildInput_ToolParametersReachRegoAsAnObject(t *testing.T) {
	inc := newIncludeSet([]string{"inference.tools.detail"})
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    "POST",
		Path:      "/",
		Host:      "svc",
		Headers:   http.Header{},
	}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{
		Model: "gpt-4",
		Tools: []pipeline.InferenceTool{{
			Name:        "create_issue",
			Description: "Creates issues",
			Parameters:  `{"type": "object", "properties": {"title": {"type": "string"}}}`,
		}},
	}

	value, err := ast.InterfaceToValue(buildInput(pctx, inc, ""))
	if err != nil {
		t.Fatalf("ast.InterfaceToValue: %v", err)
	}
	// Back to plain Go so the assertions read as the shape a policy author sees. A
	// mis-typed field arrives here as a string and every lookup below fails loudly.
	decoded, err := ast.JSON(value)
	if err != nil {
		t.Fatalf("ast.JSON: %v", err)
	}

	root, ok := decoded.(map[string]any)
	if !ok {
		t.Fatalf("input is %T, want map[string]any", decoded)
	}
	inference, ok := root["inference"].(map[string]any)
	if !ok {
		t.Fatalf("input.inference is %T, want map[string]any", root["inference"])
	}
	tools, ok := inference["tools"].([]any)
	if !ok {
		t.Fatalf("input.inference.tools is %T, want []any", inference["tools"])
	}
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools))
	}
	tool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("tools[0] is %T, want map[string]any", tools[0])
	}

	params, ok := tool["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("rego sees parameters as %T (%v); a policy indexing "+
			"input.inference.tools[_].parameters.properties would silently never match",
			tool["parameters"], tool["parameters"])
	}
	if params["type"] != "object" {
		t.Errorf(`parameters.type = %v, want "object"`, params["type"])
	}
	// One level deeper, because a schema is only useful to a policy if it can walk into
	// it — the shallow check above would pass for a flat object that lost its nesting.
	properties, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("parameters.properties is %T, want map[string]any", params["properties"])
	}
	title, ok := properties["title"].(map[string]any)
	if !ok {
		t.Fatalf("parameters.properties.title is %T, want map[string]any", properties["title"])
	}
	if title["type"] != "string" {
		t.Errorf(`parameters.properties.title.type = %v, want "string"`, title["type"])
	}
}
