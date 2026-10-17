// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build cpex

package cpex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// configuredEngine builds a cpex plugin through Configure, so plugin config
// keys reach the real manager, firing hooks on requests.
func configuredEngine(t *testing.T, cfg map[string]any, hooks ...string) *CPEX {
	t.Helper()
	cfg["hooks"] = map[string]any{"on_request": hooks}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := NewCPEX()
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := p.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p
}

// A CEL step on tool http_egress reads the synthetic call's arguments as
// args.<name>. Opaque CONNECT tunnels pass on their host alone; inner
// requests must also stay inside project A and carry no AWS key.
const egressPolicy = `
global:
  pdp:
    - kind: cel
routes:
  - tool: http_egress
    authorization:
      pre_invocation:
        - cel:
            expr: "args.host in allowed"
            allowed: ["support.local", "github.com"]
            on_deny: ["deny('host not allowed', 'egress_host')"]
        - cel:
            expr: "args.method == 'CONNECT' || args.host != 'support.local' || args.path.startsWith('/projects/A/')"
            on_deny: ["deny('wrong project', 'egress_project')"]
        - cel:
            expr: "!args.body.matches('AKIA[0-9A-Z]{16}')"
            on_deny: ["deny('AWS key in body', 'secret_in_body')"]
`

func egressEngine(t *testing.T) *CPEX {
	return configuredEngine(t, map[string]any{"config": egressPolicy, "synthetic_egress": true}, HookToolPreInvoke)
}

func TestEngineEgressHostAllowlist(t *testing.T) {
	p := egressEngine(t)
	ctx := context.Background()
	wantContinue(t, "support.local", p.OnRequest(ctx,
		egressRequest("POST", "support.local:8080", "/projects/A/reports", `{"r":"ok"}`, nil)))
	wantReject(t, "evil.example", p.OnRequest(ctx,
		egressRequest("POST", "evil.example", "/projects/A/reports", `{"r":"ok"}`, nil)), "cpex.egress_host")
}

func TestEngineEgressConnectIsHostOnly(t *testing.T) {
	p := egressEngine(t)
	ctx := context.Background()
	wantContinue(t, "CONNECT github.com", p.OnRequest(ctx, egressRequest("CONNECT", "github.com:443", "", "", nil)))
	wantContinue(t, "CONNECT support.local", p.OnRequest(ctx, egressRequest("CONNECT", "support.local:443", "", "", nil)))
	wantReject(t, "CONNECT evil.example", p.OnRequest(ctx,
		egressRequest("CONNECT", "evil.example:443", "", "", nil)), "cpex.egress_host")
	wantReject(t, "inner request to project B", p.OnRequest(ctx,
		egressRequest("POST", "support.local", "/projects/B/reports", `{}`, nil)), "cpex.egress_project")
}

// A denied body never reaches the client or the session record: only the
// violation code, a static message and the excerpt's size do.
func TestEngineEgressDenyDoesNotRecordBody(t *testing.T) {
	p := egressEngine(t)
	secret := "AKIAABCDEFGHIJKLMNOP"
	pctx := egressRequest("POST", "support.local", "/projects/A/reports", `{"note":"`+secret+`"}`, nil)
	a := p.OnRequest(context.Background(), pctx)
	wantReject(t, "secret body", a, "cpex.secret_in_body")

	recorded, err := json.Marshal(map[string]any{"invocations": pctx.Extensions.Invocations, "violation": a.Violation})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(recorded), secret) || strings.Contains(string(recorded), "AWS key") {
		t.Fatalf("recorded output carries policy text or the body: %s", recorded)
	}
	inv := lastInvocation(t, pctx)
	if inv.Details["egress_excerpt_bytes"] != "31" || inv.Details["egress_truncated"] != "false" {
		t.Fatalf("details = %v", inv.Details)
	}
}

func principalPolicy() string {
	jwk, _ := json.Marshal(map[string]string{"kty": "oct", "alg": "HS256", "k": base64.RawURLEncoding.EncodeToString(spikeSecret)})
	return `
plugins:
  - name: idp
    kind: identity/jwt
    hooks: [identity.resolve]
    config:
      claim_mapper: standard
      header: X-User-Token
      trusted_issuers:
        - issuer: ` + spikeIssuer + `
          audiences: [` + spikeAudience + `]
          algorithms: [HS256]
          decoding_key: {kind: jwk, jwk: ` + string(jwk) + `}
routes:
  - tool: http_egress
    authentication: [idp]
    authorization:
      pre_invocation: ["require(subject.id == 'alice')"]
  - llm: claude-test
    authentication: [idp]
    authorization:
      pre_invocation: ["require(subject.id == 'alice')"]
`
}

// The policy sees the Cortex-held principal on a tool call and on an
// inference request; the forwarded request never carries it.
func TestEnginePrincipalInjection(t *testing.T) {
	p := configuredEngine(t, map[string]any{
		"config":           principalPolicy(),
		"synthetic_egress": true,
		"principal":        map[string]any{"token_file": writeToken(t, spikeJWT("alice", nil))},
	}, HookToolPreInvoke, HookLLMInput)
	ctx := context.Background()

	pctx := egressRequest("POST", "support.local", "/projects/A/reports", `{}`, http.Header{"X-User-Token": {"spoofed"}})
	wantContinue(t, "http_egress", p.OnRequest(ctx, pctx))
	if got := pctx.Headers.Values("X-User-Token"); len(got) != 1 || got[0] != "spoofed" {
		t.Fatalf("forwarded X-User-Token = %v, want the caller's own value only", got)
	}
	if strings.Contains(strings.Join(pctx.Headers.Values("Authorization"), ""), ".") {
		t.Fatal("principal written to Authorization")
	}

	// Inference gets the principal too, in place of the caller's own token,
	// so the llm route sees the subject; the forwarded request does not.
	llm := anthropicToolResultRequest("s1")
	llm.Identity = nil
	llm.Headers.Set("X-User-Token", spikeJWT("mallory", nil))
	wantContinue(t, "inference", p.OnRequest(ctx, llm))
	if got := llm.Headers.Values("X-User-Token"); len(got) != 1 || got[0] != spikeJWT("mallory", nil) {
		t.Fatalf("forwarded inference X-User-Token = %v, want the caller's own value only", got)
	}
	bare := anthropicToolResultRequest("s1")
	bare.Identity = nil
	wantContinue(t, "inference without caller headers", p.OnRequest(ctx, bare))
	if _, ok := bare.Headers["X-User-Token"]; ok {
		t.Fatal("principal written to the forwarded inference request")
	}
}

// Without principal injection the same requests resolve the caller's own
// header, which is what makes the test above meaningful.
func TestEnginePrincipalInjectionControl(t *testing.T) {
	p := configuredEngine(t, map[string]any{"config": principalPolicy(), "synthetic_egress": true},
		HookToolPreInvoke, HookLLMInput)
	ctx := context.Background()
	if a := p.OnRequest(ctx, egressRequest("POST", "support.local", "/projects/A/reports", `{}`,
		http.Header{"X-User-Token": {"spoofed"}})); a.Type != pipeline.Reject {
		t.Fatalf("http_egress with a spoofed token: action = %v, want reject", a.Type)
	}
	llm := anthropicToolResultRequest("s1")
	llm.Identity = nil
	llm.Headers.Set("X-User-Token", spikeJWT("alice", nil))
	wantContinue(t, "inference with the caller's token", p.OnRequest(ctx, llm))
}

func TestEnginePrincipalMissingDenies(t *testing.T) {
	p := configuredEngine(t, map[string]any{
		"config":           principalPolicy(),
		"synthetic_egress": true,
		"principal":        map[string]any{"token_file": t.TempDir() + "/absent"},
	}, HookToolPreInvoke)
	wantReject(t, "http_egress without principal", p.OnRequest(context.Background(),
		egressRequest("POST", "support.local", "/projects/A/reports", `{}`, nil)), "cpex.principal_unavailable")
}

// An inbound caller is never asserted as the Cortex-held principal, even
// on a tool call.
func TestEnginePrincipalNotInjectedInbound(t *testing.T) {
	policy := strings.Replace(principalPolicy(), "tool: http_egress", "tool: get_secret", 1)
	p := configuredEngine(t, map[string]any{
		"config":    policy,
		"principal": map[string]any{"token_file": writeToken(t, spikeJWT("alice", nil))},
	}, HookToolPreInvoke)
	ctx := context.Background()

	wantContinue(t, "outbound tool call", p.OnRequest(ctx, toolCall("get_secret", nil)))
	inbound := toolCall("get_secret", nil)
	inbound.Direction = pipeline.Inbound
	if a := p.OnRequest(ctx, inbound); a.Type != pipeline.Reject {
		t.Fatalf("inbound tool call: action = %v, want reject (no principal)", a.Type)
	}
}

// A body shaped as an MCP protocol method or an A2A message is still
// egress: it is held to the host allowlist like any other.
func TestEngineEgressCoversParsedProtocolMechanics(t *testing.T) {
	p := egressEngine(t)
	ctx := context.Background()
	mcp := egressRequest("POST", "evil.example", "/projects/A/x", `{"jsonrpc":"2.0","method":"tools/list","params":{"cursor":"s3cr3t"}}`, nil)
	mcp.Extensions.MCP = &pipeline.MCPExtension{Method: "tools/list", Params: map[string]any{"cursor": "s3cr3t"}}
	wantReject(t, "mcp tools/list to evil.example", p.OnRequest(ctx, mcp), "cpex.egress_host")

	a2a := egressRequest("POST", "evil.example", "/projects/A/x", `{"jsonrpc":"2.0","method":"message/send"}`, nil)
	a2a.Extensions.A2A = &pipeline.A2AExtension{Method: "message/send", Parts: []pipeline.A2APart{{Kind: "text", Content: "s3cr3t"}}}
	wantReject(t, "a2a to evil.example", p.OnRequest(ctx, a2a), "cpex.egress_host")

	ok := egressRequest("POST", "support.local", "/projects/A/x", `{"jsonrpc":"2.0","method":"tools/list"}`, nil)
	ok.Extensions.MCP = &pipeline.MCPExtension{Method: "tools/list"}
	wantContinue(t, "mcp tools/list to support.local", p.OnRequest(ctx, ok))
}
