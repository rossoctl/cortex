// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package cpex

import (
	"context"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

func TestAmbiguousJSON(t *testing.T) {
	cases := map[string]bool{
		`{"method":"tools/call","params":{"name":"a"}}`:                       false,
		`{"a":[{"x":1},{"x":2}],"b":{"x":{"x":1}}}`:                           false,
		`[1,"two",{"k":null}]`:                                                false,
		`{"method":"tools/call","METHOD":"tools/list"}`:                       true,
		`{"params":{"name":"a"},"Params":{"name":"b"}}`:                       true,
		`{"params":{"name":"a","name":"b"}}`:                                  true,
		`{"messages":[{"role":"user","content":"a","Content":"b"}]}`:          true,
		`{"messages":[],"meſſages":[]}`:                                       true,
		`{"model":"a","model":"b"}`:                                           true,
		`{"kind":"a","Kind":"b"}`:                                             true,
		`{"params":{"arguments":{"path":"/ok"},"arguments":{"path":"/etc"}}}`: true,
		`{"a":1`: true,
		`{"params":{"arguments":{"Path":"/a","path":"/b"}}}`:                                  false,
		`{"tools":[{"input_schema":{"properties":{"ID":{},"id":{}}}}]}`:                       false,
		`{"params":{"arguments":{"path":"/a","path":"/b"}}}`:                                  true,
		`{"params":{"arguments":{"x":{}},"Name":"a","name":"b"}}`:                             true,
		`{"input":[{"type":"function_call_output","call_id":"c","output":"a","Output":"b"}]}`: true,
		`{"input":[{"type":"message","content":"a","content":"b"}]}`:                          true,
		`{"input":[{"type":"function_call","arguments":"{}","call_id":"c"}]}`:                 false,
		`{"input":{"Path":"/a","path":"/b"}}`:                                                 false,
		`{"messages":[{"content":[{"type":"tool_use","input":{"ID":1,"id":2}}]}]}`:            false,
		`{"params":{"arguments":{"list":[{"Path":"/a","path":"/b"}]}}}`:                       false,
		// Keys inside data objects are not struct fields, so only exact
		// repeats count there.
		`{"model":"m","messages":[],"response_format":{"type":"json_schema","json_schema":{"schema":{"properties":{"ID":{},"id":{}}}}}}`: false,
		`{"model":"m","input":[],"text":{"format":{"type":"json_schema","schema":{"properties":{"ID":{},"id":{}}}}}}`:                    false,
		`{"method":"message/send","params":{"message":{"parts":[{"kind":"data","data":{"ID":1,"id":2}}]}}}`:                              false,
		`{"model":"m","metadata":{"User":"a","user":"b"}}`:                                                                               false,
		`{"tools":[{"type":"function","function":{"parameters":{"properties":{"ID":{},"id":{}}}}}]}`:                                     false,
		`{"model":"m","metadata":{"user":"a","user":"b"}}`:                                                                               true,
		// The envelopes the parsers decode into structs fold.
		`{"method":"message/send","params":{"message":{"parts":[{"kind":"text","Kind":"data"}]}}}`:   true,
		`{"method":"message/send","params":{"message":{"role":"user","Role":"agent"}}}`:              true,
		`{"messages":[{"content":[{"type":"text","text":"a","Text":"b"}]}]}`:                         true,
		`{"messages":[{"content":[{"type":"tool_result","content":[{"type":"text","Type":"x"}]}]}]}`: true,
		`{"input":[{"type":"message","content":[{"type":"input_text","text":"a","TEXT":"b"}]}]}`:     true,
		`{"input":[{"tools":[{"tools":[{"name":"a","Name":"b"}]}]}]}`:                                true,
		`{"tools":[{"type":"function","function":{"name":"a","Name":"b"}}]}`:                         true,
	}
	for body, want := range cases {
		if got := ambiguousJSON([]byte(body)); got != want {
			t.Errorf("ambiguousJSON(%s) = %v, want %v", body, got, want)
		}
	}
}

func TestUnpresentableRequest(t *testing.T) {
	mcp := func(method string, params map[string]any, body string) *pipeline.Context {
		return &pipeline.Context{
			Direction:  pipeline.Outbound,
			Body:       []byte(body),
			Extensions: pipeline.Extensions{MCP: &pipeline.MCPExtension{Method: method, Params: params}},
		}
	}
	cases := []struct {
		name string
		pctx *pipeline.Context
		want string
	}{
		{"named tool call", mcp("tools/call", map[string]any{"name": "a"}, `{"method":"tools/call","params":{"name":"a"}}`), ""},
		{"protocol method", mcp("tools/list", nil, `{"method":"tools/list"}`), ""},
		{"tool call without a name", mcp("tools/call", map[string]any{}, `{"method":"tools/call","params":{}}`), codeUnnamedEntity},
		{"tool call with a non-string name", mcp("tools/call", map[string]any{"name": 7.0}, `{}`), codeUnnamedEntity},
		{"prompt without a name", mcp("prompts/get", nil, `{}`), codeUnnamedEntity},
		{"resource without a uri", mcp("resources/read", map[string]any{"uri": ""}, `{}`), codeUnnamedEntity},
		{"case-variant method", mcp("tools/list", nil, `{"method":"tools/call","METHOD":"tools/list","params":{"name":"a"}}`), codeAmbiguousRequest},
		{"duplicate params", mcp("tools/call", map[string]any{"name": "safe"}, `{"method":"tools/call","params":{"name":"x"},"PARAMS":{"name":"safe"}}`), codeAmbiguousRequest},
		{"inference duplicate", &pipeline.Context{Body: []byte(`{"model":"a","Model":"b"}`),
			Extensions: pipeline.Extensions{Inference: &pipeline.InferenceExtension{Model: "b"}}}, codeAmbiguousRequest},
		{"a2a duplicate", &pipeline.Context{Body: []byte(`{"method":"message/send","Method":"tasks/get"}`),
			Extensions: pipeline.Extensions{A2A: &pipeline.A2AExtension{Method: "tasks/get"}}}, codeAmbiguousRequest},
		{"unparsed duplicate is egress input, not refused", &pipeline.Context{Body: []byte(`{"a":1,"a":2}`)}, ""},
	}
	for _, c := range cases {
		if got := unpresentableRequest(c.pctx); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPresentsAsEgress(t *testing.T) {
	pctx := egressRequest("POST", "evil.example", "/", "{}", nil)
	if !presentsAsEgress(pctx, false, true) {
		t.Fatal("unparsed outbound request not egress")
	}
	if presentsAsEgress(pctx, true, true) || presentsAsEgress(pctx, false, false) {
		t.Fatal("egress on a response or with egress off")
	}
	pctx.Direction = pipeline.Inbound
	if presentsAsEgress(pctx, false, true) {
		t.Fatal("inbound request presented as egress")
	}
}

// An ambiguous or unnamed request is denied before CPEX is invoked, and
// fail_open does not let it through: it is a refusal, not a CPEX error.
func TestDispatch_DeniesUnpresentableRequests(t *testing.T) {
	fake := &FakeManager{Hooks: map[string]Result{HookToolPreInvoke: {Decision: DecisionAllow}}}
	p := setupAndInit(t, fake, `{"hooks":{"on_request":["cmf.tool_pre_invoke"]},"fail_open":true}`)
	pctx := &pipeline.Context{
		Direction:  pipeline.Outbound,
		Body:       []byte(`{"method":"tools/call","params":{"name":"get_secret"},"METHOD":"tools/list"}`),
		Extensions: pipeline.Extensions{MCP: &pipeline.MCPExtension{Method: "tools/list"}},
	}
	a := p.OnRequest(context.Background(), pctx)
	if a.Type != pipeline.Reject || a.Violation == nil || a.Violation.Code != "cpex."+codeAmbiguousRequest {
		t.Fatalf("action = %+v, want reject %s", a, codeAmbiguousRequest)
	}
	if len(fake.Invokes) != 0 {
		t.Fatalf("CPEX invoked %d times", len(fake.Invokes))
	}
}

// With synthetic egress, the destination host and path are the caller's
// choice, so outbound requests are never bypassed by path and the default
// bypass hosts do not apply.
func TestDispatch_EgressIgnoresOutboundBypass(t *testing.T) {
	fake := &FakeManager{Hooks: map[string]Result{HookToolPreInvoke: {Decision: DecisionDeny, Code: "egress_host"}}}
	p := setupAndInit(t, fake, `{"hooks":{"on_request":["cmf.tool_pre_invoke"]},"synthetic_egress":true}`)
	for _, pctx := range []*pipeline.Context{
		egressRequest("GET", "evil.example", "/healthz", "", nil),
		egressRequest("POST", "keycloak.evil.example", "/x", "s3cr3t", nil),
	} {
		if a := p.OnRequest(context.Background(), pctx); a.Type != pipeline.Reject {
			t.Errorf("%s%s bypassed CPEX", pctx.Host, pctx.Path)
		}
	}
	// Inbound path bypass still applies.
	in := &pipeline.Context{Direction: pipeline.Inbound, Path: "/healthz"}
	if a := p.OnRequest(context.Background(), in); a.Type != pipeline.Continue {
		t.Fatalf("inbound /healthz: %v", a.Type)
	}
	// An explicit bypass_hosts list is the operator's choice and is kept.
	p = setupAndInit(t, fake, `{"hooks":{"on_request":["cmf.tool_pre_invoke"]},"synthetic_egress":true,"bypass_hosts":["otel.internal"]}`)
	if a := p.OnRequest(context.Background(), egressRequest("POST", "otel.internal", "/v1/traces", "", nil)); a.Type != pipeline.Continue {
		t.Fatalf("explicit bypass host: %v", a.Type)
	}
}
