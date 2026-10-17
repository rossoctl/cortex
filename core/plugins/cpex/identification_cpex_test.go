// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build cpex

package cpex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

const toolOnlyPolicy = `
routes:
  - tool: get_compensation
    authorization:
      pre_invocation: ["require(role.hr)"]
`

func mcpRequest(method string) *pipeline.Context {
	return &pipeline.Context{
		Direction:  pipeline.Outbound,
		Host:       "tools.example",
		Headers:    http.Header{},
		Body:       []byte(`{"jsonrpc":"2.0","id":1,"method":"` + method + `"}`),
		Extensions: pipeline.Extensions{MCP: &pipeline.MCPExtension{Method: method}},
	}
}

func inferenceRequest(model string) *pipeline.Context {
	return &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "api.anthropic.com",
		Headers:   http.Header{},
		Extensions: pipeline.Extensions{Inference: &pipeline.InferenceExtension{
			Model:    model,
			Messages: []pipeline.InferenceMessage{{Role: "user", Content: "hello"}},
		}},
	}
}

// Traffic that names no entity is identified as CPEX's global entity, so
// a config that declares policy does not refuse it as unidentified.
func TestEngineIdentifiesEntitylessTraffic(t *testing.T) {
	p := enginePlugin(t, newEngineManager(t), toolOnlyPolicy)
	ctx := context.Background()

	wantContinue(t, "tools/list", p.OnRequest(ctx, mcpRequest("tools/list")))
	wantContinue(t, "initialize", p.OnRequest(ctx, mcpRequest("initialize")))
	wantContinue(t, "a2a", p.OnRequest(ctx, &pipeline.Context{
		Direction: pipeline.Outbound,
		Headers:   http.Header{},
		Extensions: pipeline.Extensions{A2A: &pipeline.A2AExtension{
			Method: "message/send",
			Parts:  []pipeline.A2APart{{Kind: "text", Content: "hi"}},
		}},
	}))
	wantContinue(t, "unparsed", p.OnRequest(ctx, &pipeline.Context{
		Direction: pipeline.Outbound, Headers: http.Header{}, Body: []byte("raw"),
	}))
	// The tool route itself still gates.
	if a := p.OnRequest(ctx, toolCall("get_compensation", nil)); a.Type != pipeline.Reject {
		t.Fatalf("get_compensation without role: action = %v, want reject", a.Type)
	}
}

func TestEngineFallbackNoneLeavesRequestUnidentified(t *testing.T) {
	mgr := newEngineManager(t)
	mgr.(*cpexManager).opts.FallbackIdentification = fallbackNone
	p := enginePlugin(t, mgr, toolOnlyPolicy)
	wantReject(t, "tools/list", p.OnRequest(context.Background(), mcpRequest("tools/list")),
		"cpex.unidentified_request")
}

func TestEngineRoutesInferenceByModel(t *testing.T) {
	p := enginePluginHook(t, newEngineManager(t), `
routes:
  - llm: claude-guarded
    authorization:
      pre_invocation: ["require(role.hr)"]
  - llm: claude-open
    authorization:
      pre_invocation: ["allow"]
`, HookLLMInput)
	ctx := context.Background()
	if a := p.OnRequest(ctx, inferenceRequest("claude-guarded")); a.Type != pipeline.Reject {
		t.Fatalf("claude-guarded: action = %v, want reject from its route", a.Type)
	}
	wantContinue(t, "claude-open", p.OnRequest(ctx, inferenceRequest("claude-open")))
}

// identity.resolve sees the same meta as the hook, so a route's
// authentication list runs for an llm route.
func TestEngineIdentityResolvesOnLLMRoute(t *testing.T) {
	jwk, _ := json.Marshal(map[string]string{"kty": "oct", "alg": "HS256", "k": base64.RawURLEncoding.EncodeToString(spikeSecret)})
	p := enginePluginHook(t, newEngineManager(t), `
plugins:
  - name: idp
    kind: identity/jwt
    hooks: [identity.resolve]
    config:
      claim_mapper: standard
      trusted_issuers:
        - issuer: `+spikeIssuer+`
          audiences: [`+spikeAudience+`]
          algorithms: [HS256]
          decoding_key: {kind: jwk, jwk: `+string(jwk)+`}
routes:
  - llm: claude-guarded
    authentication: [idp]
    authorization:
      pre_invocation: ["require(role.hr)"]
`, HookLLMInput)
	ctx := context.Background()

	hr := inferenceRequest("claude-guarded")
	hr.Headers.Set("Authorization", "Bearer "+spikeJWT("alice", []string{"hr"}))
	wantContinue(t, "hr bearer", p.OnRequest(ctx, hr))

	eng := inferenceRequest("claude-guarded")
	eng.Headers.Set("Authorization", "Bearer "+spikeJWT("evan", []string{"engineer"}))
	if a := p.OnRequest(ctx, eng); a.Type != pipeline.Reject {
		t.Fatalf("engineer bearer: action = %v, want reject", a.Type)
	}
}
