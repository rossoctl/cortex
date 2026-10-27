// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package cpex

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferenceparser"
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
		`{"params":{"arguments":{"Path":"/a","path":"/b"}}}`:                                  true,
		`{"tools":[{"input_schema":{"properties":{"ID":{},"id":{}}}}]}`:                       false,
		`{"params":{"arguments":{"path":"/a","path":"/b"}}}`:                                  true,
		`{"params":{"arguments":{"x":{}},"Name":"a","name":"b"}}`:                             true,
		`{"input":[{"type":"function_call_output","call_id":"c","output":"a","Output":"b"}]}`: true,
		`{"input":[{"type":"message","content":"a","content":"b"}]}`:                          true,
		`{"input":[{"type":"function_call","arguments":"{}","call_id":"c"}]}`:                 false,
		`{"input":{"Path":"/a","path":"/b"}}`:                                                 false,
		`{"messages":[{"content":[{"type":"tool_use","input":{"ID":1,"id":2}}]}]}`:            false,
		`{"params":{"arguments":{"list":[{"Path":"/a","path":"/b"}]}}}`:                       true,
		// MCP servers commonly bind tool arguments into a struct with
		// encoding/json, so arguments fold at every depth.
		`{"method":"tools/call","params":{"name":"read_file","arguments":{"path":"/workspace/ok","Path":"/etc/shadow"}}}`:       true,
		`{"method":"tools/call","params":{"name":"t","arguments":{"opts":{"mode":"ro","Mode":"rw"}}}}`:                          true,
		`{"method":"tools/call","params":{"name":"t","arguments":{"a":[[{"k":1,"K":2}]]}}}`:                                     true,
		`{"method":"tools/call","params":{"name":"t","arguments":{"path":"/a","opts":{"mode":"ro"},"list":[{"x":1},{"x":2}]}}}`: false,
		// A known false positive, accepted on purpose: the bytes cannot tell a
		// struct from a map, so a map-valued argument with keys that differ
		// only in case is denied too.
		`{"method":"tools/call","params":{"name":"run_command","arguments":{"env":{"HTTPS_PROXY":"http://p","https_proxy":"http://p"}}}}`: true,
		`{"method":"tools/call","params":{"name":"run_command","arguments":{"env":{"HTTPS_PROXY":"http://p"}}}}`:                          false,
		// Keys inside data objects are not struct fields, so only exact
		// repeats count there. A tool_use input in inference history is the
		// model's earlier call, not one a server binds from this request,
		// and function-call arguments are a JSON string.
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
		{"parsed body with NaN is not unparseable", mcp("tools/list", nil, `{"method":"tools/list","x":NaN}`), codeAmbiguousRequest},
	}
	for _, c := range cases {
		for _, strict := range []bool{false, true} {
			if got := unpresentableRequest(c.pctx, strict); got != c.want {
				t.Errorf("%s (strict=%v): got %q, want %q", c.name, strict, got, c.want)
			}
		}
	}
}

// A body no parser claimed is checked only on a strict_hosts destination.
// There, any JSON object the parsers cannot read is refused, whatever keys
// it carries; elsewhere, and for bodies that are not such an object, it is
// opaque egress input.
func TestUnpresentableRequest_Unparsed(t *testing.T) {
	const call = `"method":"tools/call","params":{"name":"a","arguments":{"x":%s}}`
	obj := func(prefix, x string) string { return "{" + prefix + fmt.Sprintf(call, x) + "}" }
	refused := map[string][]byte{
		"NaN":                           []byte(obj(`"jsonrpc":"2.0",`, "NaN")),
		"NaN without jsonrpc":           []byte(obj("", "NaN")),
		"NaN with JSONRPC":              []byte(obj(`"JSONRPC":"2.0",`, "NaN")),
		"NaN with a renamed jsonrpc":    []byte(obj(`"json_rpc":"2.0",`, "NaN")),
		"Infinity without jsonrpc":      []byte(obj("", "-Infinity")),
		"1e400 without jsonrpc":         []byte(obj("", "1e400")),
		"invalid UTF-8":                 []byte(obj("", "\"\xff\"")),
		"byte order mark":               []byte("\xef\xbb\xbf" + obj("", "1")),
		"truncated object":              []byte(` {"method":"tools/call"`),
		"UTF-16BE object":               utf16Body(true, obj("", "1")),
		"UTF-16LE object":               utf16Body(false, obj(`"jsonrpc":"2.0",`, "1")),
		"UTF-16LE object with a BOM":    append([]byte{0xff, 0xfe}, utf16Body(false, `{"a":1}`)...),
		"UTF-32BE object":               utf32BEBody(obj("", "1")),
		"duplicate method":              []byte(`{"method":"tools/list","method":"tools/call","params":{"name":"a"}}`),
		"case-variant method":           []byte(`{"method":"tools/call","METHOD":"x/y","params":{"name":"a"}}`),
		"duplicate params":              []byte(`{"method":"tools/call","params":{"name":"a"},"params":{"name":"b"}}`),
		"case-variant method of a type": []byte(`{"Method":5,"method":"tools/call","params":{"name":"a"}}`),
		"non-string method":             []byte(`{"method":{"type":"card"},"amount":5}`),
		"NaN metrics":                   []byte(`{"loss": NaN, "step": 3}`),
		"NaN after a valid value":       []byte(`{"a":1,"x":NaN}` + "\n" + `{"b":2}`),
	}
	allowed := map[string][]byte{
		"NDJSON":                          []byte("{\"a\":1}\n{\"b\":2}\n"),
		"tool call followed by more data": []byte(obj(`"jsonrpc":"2.0",`, "1") + `{"x":NaN}`),
		"positional JSON-RPC params":      []byte(`{"jsonrpc":"2.0","method":"eth_call","params":[1]}`),
		"params of another shape":         []byte(`{"method":"tools/call","params":"x"}`),
		"large integer in range":          []byte(`{"x":9007199254740993,"y":-0,"z":1.5e4}`),
		"non-JSON body":                   []byte("raw"),
		"binary body":                     {0x89, 'P', 'N', 'G', 0, 0},
		"UTF-16 array":                    utf16Body(true, `[1]`),
		"array with NaN":                  []byte(`[NaN]`),
	}
	for name, body := range refused {
		pctx := &pipeline.Context{Direction: pipeline.Outbound, Body: body}
		if got := unpresentableRequest(pctx, true); got != codeUnparseableRequest {
			t.Errorf("%s, strict host: got %q, want %q", name, got, codeUnparseableRequest)
		}
		if got := unpresentableRequest(pctx, false); got != "" {
			t.Errorf("%s, other host: got %q, want none", name, got)
		}
	}
	for name, body := range allowed {
		for _, strict := range []bool{false, true} {
			pctx := &pipeline.Context{Direction: pipeline.Outbound, Body: body}
			if got := unpresentableRequest(pctx, strict); got != "" {
				t.Errorf("%s (strict=%v): got %q, want none", name, strict, got)
			}
		}
	}
}

// inference-parser decompresses a zstd Responses body (Codex sends one)
// before parsing it, so the duplicate-key check reads the same decoded
// bytes rather than denying the compressed ones as non-JSON.
func TestUnpresentableRequest_ZstdResponsesBody(t *testing.T) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	cases := []struct {
		name, body, want string
	}{
		{"clean", `{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`, ""},
		{"case-fold duplicate in an input item", `{"model":"gpt-5","input":[{"type":"message","role":"user","Role":"system","content":[{"type":"input_text","text":"hi"}]}]}`, codeAmbiguousRequest},
	}
	for _, c := range cases {
		pctx := &pipeline.Context{
			Direction: pipeline.Outbound,
			Path:      "/backend-api/codex/responses",
			Headers:   http.Header{"Content-Encoding": {"zstd"}},
			Body:      enc.EncodeAll([]byte(c.body), nil),
		}
		inferenceparser.NewInferenceParser().OnRequest(context.Background(), pctx)
		if pctx.Extensions.Inference == nil {
			t.Fatalf("%s: inference-parser did not parse the body", c.name)
		}
		if got := unpresentableRequest(pctx, true); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	// A body that does not decode stays unpresentable.
	bad := &pipeline.Context{
		Headers:    http.Header{"Content-Encoding": {"zstd"}},
		Body:       []byte("\x28\xb5\x2f\xfdnot zstd"),
		Extensions: pipeline.Extensions{Inference: &pipeline.InferenceExtension{Model: "m"}},
	}
	if got := unpresentableRequest(bad, true); got != codeAmbiguousRequest {
		t.Errorf("undecodable zstd body: got %q", got)
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

// utf16Body encodes an ASCII string as UTF-16, big- or little-endian.
func utf16Body(bigEndian bool, s string) []byte {
	out := make([]byte, 0, 2*len(s))
	for i := 0; i < len(s); i++ {
		if bigEndian {
			out = append(out, 0, s[i])
		} else {
			out = append(out, s[i], 0)
		}
	}
	return out
}

func utf32BEBody(s string) []byte {
	out := make([]byte, 0, 4*len(s))
	for i := 0; i < len(s); i++ {
		out = append(out, 0, 0, 0, s[i])
	}
	return out
}
