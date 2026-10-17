package cpex

import (
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rossoctl/cortex/core/pipeline"
)

func egressRequest(method, host, path, body string, h http.Header) *pipeline.Context {
	if h == nil {
		h = http.Header{}
	}
	return &pipeline.Context{
		Direction: pipeline.Outbound, Method: method, Host: host, Path: path,
		Headers: h, Body: []byte(body),
	}
}

func TestEgressPart(t *testing.T) {
	cases := []struct {
		name string
		pctx *pipeline.Context
		want map[string]any
	}{
		{
			name: "plaintext post",
			pctx: egressRequest("POST", "support.local:8080", "/projects/A/reports", `{"r":"ok"}`, nil),
			want: map[string]any{
				"method": "POST", "host": "support.local", "path": "/projects/A/reports",
				"body": `{"r":"ok"}`, "truncated": false, "content_encoding": "",
			},
		},
		{
			name: "bridged inner request",
			pctx: egressRequest("put", "API.GitHub.com:443", "/repos/x", "", http.Header{"Content-Encoding": {"GZIP"}}),
			want: map[string]any{
				"method": "PUT", "host": "api.github.com", "path": "/repos/x",
				"body": "", "truncated": false, "content_encoding": "gzip",
			},
		},
		{
			name: "connect is host only",
			pctx: egressRequest("CONNECT", "github.com:443", "/ignored", "bytes", http.Header{"Content-Encoding": {"gzip"}}),
			want: map[string]any{
				"method": "CONNECT", "host": "github.com", "path": "",
				"body": "", "truncated": false, "content_encoding": "",
			},
		},
		{
			name: "ipv6 literal",
			pctx: egressRequest("GET", "[::1]:9000", "/", "", nil),
			want: map[string]any{
				"method": "GET", "host": "::1", "path": "/",
				"body": "", "truncated": false, "content_encoding": "",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parts, et, en := pctxToCMFParts(tc.pctx, false, true)
			if et != "tool" || en != egressToolName || len(parts) != 1 {
				t.Fatalf("entity = %s/%s, parts = %+v", et, en, parts)
			}
			if parts[0].Kind != cmfPartToolCall || !reflect.DeepEqual(parts[0].Arguments, tc.want) {
				t.Fatalf("args = %#v, want %#v", parts[0].Arguments, tc.want)
			}
		})
	}
}

func TestEgressPart_TruncatesOnRuneBoundary(t *testing.T) {
	body := strings.Repeat("a", egressExcerptBytes-1) + "é tail"
	args := egressPart(egressRequest("POST", "h", "/", body, nil)).Arguments
	got := args["body"].(string)
	if args["truncated"] != true || !utf8.ValidString(got) || len(got) != egressExcerptBytes-1 {
		t.Fatalf("truncated = %v, len = %d, valid = %v", args["truncated"], len(got), utf8.ValidString(got))
	}
	if d := egressDetails(egressToolName, args); d["egress_excerpt_bytes"] != "8191" || d["egress_truncated"] != "true" {
		t.Fatalf("details = %v", d)
	}
}

func TestEgressPart_DisabledOrResponse(t *testing.T) {
	pctx := egressRequest("POST", "support.local", "/x", "body", nil)
	for _, tc := range []struct {
		response, egress bool
	}{{false, false}, {true, true}} {
		parts, et, en := pctxToCMFParts(pctx, tc.response, tc.egress)
		if et != "" || en != "" || len(parts) != 1 || parts[0].Kind != cmfPartText {
			t.Fatalf("response=%v egress=%v: entity = %s/%s parts = %+v", tc.response, tc.egress, et, en, parts)
		}
	}
}

// With synthetic egress on, any outbound request that names no entity is
// presented as http_egress, whatever parser claimed it: a body shaped as
// an MCP protocol method or an A2A message must not route around the
// egress policy. A named entity keeps its route, and inbound requests are
// never egress.
func TestEgressPart_CoversUnnamedParsedRequests(t *testing.T) {
	cases := map[string]pipeline.Extensions{
		"mcp tools/list": {MCP: &pipeline.MCPExtension{Method: "tools/list", Params: map[string]any{"cursor": "s3cr3t"}}},
		"mcp initialize": {MCP: &pipeline.MCPExtension{Method: "initialize"}},
		"a2a":            {A2A: &pipeline.A2AExtension{Method: "message/send", Parts: []pipeline.A2APart{{Kind: "text", Content: "s3cr3t"}}}},
		"unparsed":       {},
	}
	for name, ext := range cases {
		pctx := egressRequest("POST", "evil.example", "/x", `{"x":"s3cr3t"}`, nil)
		pctx.Extensions = ext
		parts, et, en := pctxToCMFParts(pctx, false, true)
		if et != "tool" || en != egressToolName || len(parts) != 1 || parts[0].Arguments["host"] != "evil.example" {
			t.Errorf("%s: entity = %s/%s parts = %+v, want http_egress", name, et, en, parts)
		}
		pctx.Direction = pipeline.Inbound
		if _, et, en := pctxToCMFParts(pctx, false, true); en == egressToolName {
			t.Errorf("%s: inbound request presented as %s/%s", name, et, en)
		}
	}

	named := egressRequest("POST", "tools.example", "/mcp", `{}`, nil)
	named.Extensions.MCP = &pipeline.MCPExtension{Method: "tools/call", Params: map[string]any{"name": "get_weather"}}
	if _, et, en := pctxToCMFParts(named, false, true); et != "tool" || en != "get_weather" {
		t.Fatalf("named tool call presented as %s/%s", et, en)
	}
	llm := egressRequest("POST", "api.example", "/v1/messages", `{}`, nil)
	llm.Extensions.Inference = &pipeline.InferenceExtension{Model: "m"}
	if _, et, en := pctxToCMFParts(llm, false, true); et != entityTypeLLM || en != "m" {
		t.Fatalf("inference presented as %s/%s", et, en)
	}
}

func TestEgressDetailsOtherTools(t *testing.T) {
	if d := egressDetails("get_weather", map[string]any{"body": "x"}); d != nil {
		t.Fatalf("details = %v", d)
	}
}

func writeToken(t *testing.T, tok string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestIdentityHeaders(t *testing.T) {
	caller := http.Header{"Authorization": {"Bearer c"}, "X-User-Token": {"spoofed"}}

	got, err := identityHeaders(caller, true, nil)
	if err != nil || got["x-user-token"] != "spoofed" || got["authorization"] != "Bearer c" {
		t.Fatalf("no principal: %v %v", got, err)
	}

	p, err := newPrincipalSource(principalConfig{TokenFile: writeToken(t, "jwt"), Header: "X-User-Token"})
	if err != nil {
		t.Fatal(err)
	}
	got, err = identityHeaders(caller, true, p)
	if err != nil || got["x-user-token"] != "jwt" || got["authorization"] != "Bearer c" {
		t.Fatalf("tool call: %v %v", got, err)
	}
	got, err = identityHeaders(caller, false, p)
	if _, has := got["x-user-token"]; err != nil || has {
		t.Fatalf("no injection: %v %v, want no principal header", got, err)
	}
	if caller.Get("X-User-Token") != "spoofed" {
		t.Fatal("caller headers mutated")
	}
	if got, err := identityHeaders(nil, true, p); err != nil || got["x-user-token"] != "jwt" {
		t.Fatalf("no caller headers: %v %v", got, err)
	}
}

func TestPrincipalSourceMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	p, err := newPrincipalSource(principalConfig{TokenFile: path, Header: "x-user-token"})
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v, want one naming %s", err, path)
	}
	if _, err := identityHeaders(nil, true, p); err != errPrincipalUnavailable {
		t.Fatalf("tool call err = %v, want errPrincipalUnavailable", err)
	}
	if _, err := identityHeaders(nil, false, p); err != nil {
		t.Fatalf("no injection err = %v, want nil", err)
	}
	if err := os.WriteFile(path, []byte("late"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := identityHeaders(nil, true, p); err != nil || got["x-user-token"] != "late" {
		t.Fatalf("after the file appears: %v %v", got, err)
	}
}

func TestPrincipalSourceRotation(t *testing.T) {
	path := writeToken(t, "first")
	p, err := newPrincipalSource(principalConfig{TokenFile: path, Header: "x-user-token"})
	if err != nil {
		t.Fatal(err)
	}
	rotate := func(content string, at time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().Add(-time.Hour)
	rotate("second", base)
	if tok, ok := p.Token(); !ok || tok != "second" {
		t.Fatalf("after rotation: %q %v, want second", tok, ok)
	}
	rotate("", base.Add(time.Minute))
	if tok, ok := p.Token(); !ok || tok != "second" {
		t.Fatalf("after an empty rewrite: %q %v, want the cached second", tok, ok)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if tok, ok := p.Token(); !ok || tok != "second" {
		t.Fatalf("after removal: %q %v, want the cached second", tok, ok)
	}
	rotate("third", base.Add(2*time.Minute))
	if tok, ok := p.Token(); !ok || tok != "third" {
		t.Fatalf("after the file returns: %q %v, want third", tok, ok)
	}
}

func TestConfigure_EgressAndPrincipal(t *testing.T) {
	var seen ManagerOptions
	p := NewCPEX()
	p.newManager = func(opts ManagerOptions) (Manager, error) {
		seen = opts
		return &FakeManager{}, nil
	}
	missing := filepath.Join(t.TempDir(), "absent")
	raw := `{"synthetic_egress":true,"principal":{"token_file":"` + missing + `"}}`
	if err := p.Configure([]byte(raw)); err != nil {
		t.Fatalf("Configure with a missing token file: %v", err)
	}
	if !seen.SyntheticEgress || seen.Principal == nil || seen.Principal.header != defaultPrincipalHeader {
		t.Fatalf("options = %+v", seen)
	}

	if err := p.Configure([]byte(`{}`)); err != nil || seen.SyntheticEgress || seen.Principal != nil {
		t.Fatalf("defaults: err = %v, options = %+v", err, seen)
	}
	for _, h := range []string{"Authorization", "host", "x-api-key", "bad header"} {
		raw := `{"principal":{"token_file":"/x","header":"` + h + `"}}`
		if err := setupFake(&FakeManager{}).Configure([]byte(raw)); err == nil {
			t.Errorf("principal.header %q accepted", h)
		}
	}
}

// The principal is asserted only on outbound requests Cortex presents as
// a tool call or an inference request; an inbound caller never gets it.
func TestInjectsPrincipal(t *testing.T) {
	cases := []struct {
		dir        pipeline.Direction
		entityType string
		want       bool
	}{
		{pipeline.Outbound, "tool", true},
		{pipeline.Outbound, entityTypeLLM, true},
		{pipeline.Outbound, globalEntityType, false},
		{pipeline.Outbound, "prompt", false},
		{pipeline.Outbound, "", false},
		{pipeline.Inbound, "tool", false},
		{pipeline.Inbound, entityTypeLLM, false},
	}
	for _, c := range cases {
		if got := injectsPrincipal(&pipeline.Context{Direction: c.dir}, c.entityType); got != c.want {
			t.Errorf("injectsPrincipal(%v, %q) = %v, want %v", c.dir, c.entityType, got, c.want)
		}
	}
}

func TestWithoutHeader(t *testing.T) {
	m := map[string]string{"X-User-Token": "spoofed", "Accept": "a"}
	if got := withoutHeader(m, "x-user-token"); len(got) != 1 || got["Accept"] != "a" {
		t.Fatalf("got %v", got)
	}
	if _, ok := m["X-User-Token"]; !ok {
		t.Fatal("input mutated")
	}
	if got := withoutHeader(m, ""); len(got) != 2 {
		t.Fatalf("empty name: %v", got)
	}
	var nilSource *principalSource
	if nilSource.headerName() != "" {
		t.Fatal("nil source has a header name")
	}
}
