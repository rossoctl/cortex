// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build cpex

package cpex

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// CEL on an llm route reads request tool results from custom.tool_results
// (keyed by position) and taints the session when one carries a secret;
// a later http_egress call in that session is denied on the label.
//
// A CEL step cannot taint and allow: true runs on_allow, false always
// denies after on_deny. So the step that taints also denies the inference
// request, and the secret never reaches the model.
const toolResultTaintPolicy = `
global:
  pdp:
    - kind: cel
routes:
  - llm: claude-test
    authorization:
      pre_invocation:
        - cel:
            expr: "custom.tool_result_count == 0 || !custom.tool_results.exists(k, custom.tool_results[k].content.matches('AKIA[0-9A-Z]{16}'))"
            on_deny:
              - "taint(secret, session)"
              - "deny('secret in a tool result', 'secret_in_tool_result')"
  - tool: http_egress
    authorization:
      pre_invocation:
        - "security.labels contains \"secret\": deny('session touched secret data', 'session_tainted')"
`

func TestEngineToolResultSecretTaintsSession(t *testing.T) {
	p := configuredEngine(t, map[string]any{"config": toolResultTaintPolicy, "synthetic_egress": true},
		HookLLMInput, HookToolPreInvoke)
	ctx := context.Background()
	egress := func(sid string) *pipeline.Context {
		e := egressRequest("POST", "support.local", "/projects/A/reports", `{"r":"ok"}`, http.Header{"X-Session-Id": {sid}})
		e.Identity = claimsIdentity{sub: "alice"}
		return e
	}

	clean := anthropicToolResultRequest("s1")
	clean.Extensions.Inference.ToolResults[0].Content = "PATH=/usr/bin"
	wantContinue(t, "clean tool result", p.OnRequest(ctx, clean))
	wantContinue(t, "egress before taint", p.OnRequest(ctx, egress("s1")))

	secret := "AKIAABCDEFGHIJKLMNOP"
	leaky := anthropicToolResultRequest("s1")
	leaky.Extensions.Inference.ToolResults[0].Content = "AWS_ACCESS_KEY_ID=" + secret
	wantReject(t, "secret tool result", p.OnRequest(ctx, leaky), "cpex.secret_in_tool_result")
	recorded, err := json.Marshal(leaky.Extensions.Invocations)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(recorded), secret) {
		t.Fatalf("invocation carries the tool result: %s", recorded)
	}

	wantReject(t, "egress after taint", p.OnRequest(ctx, egress("s1")), "cpex.session_tainted")
	wantContinue(t, "egress in another session", p.OnRequest(ctx, egress("s2")))
}

// Without tool results the count is still present, so a policy can guard
// on it.
func TestEngineToolResultCountWithoutResults(t *testing.T) {
	p := configuredEngine(t, map[string]any{"config": toolResultTaintPolicy}, HookLLMInput)
	req := anthropicToolResultRequest("s1")
	req.Extensions.Inference.ToolResults = nil
	wantContinue(t, "no tool results", p.OnRequest(context.Background(), req))
}

// With the step's code in observe_codes, the inference request carrying
// the secret is forwarded and the session is still tainted.
func TestEngineToolResultTaintWithoutDeny(t *testing.T) {
	p := configuredEngine(t, map[string]any{
		"config":           toolResultTaintPolicy,
		"synthetic_egress": true,
		"observe_codes":    []string{"secret_in_tool_result"},
	}, HookLLMInput, HookToolPreInvoke)
	ctx := context.Background()
	leaky := anthropicToolResultRequest("s1")
	leaky.Extensions.Inference.ToolResults[0].Content = "AWS_ACCESS_KEY_ID=AKIAABCDEFGHIJKLMNOP"
	wantContinue(t, "secret tool result", p.OnRequest(ctx, leaky))

	e := egressRequest("POST", "support.local", "/projects/A/reports", `{"r":"ok"}`, http.Header{"X-Session-Id": {"s1"}})
	e.Identity = claimsIdentity{sub: "alice"}
	wantReject(t, "egress after taint", p.OnRequest(ctx, e), "cpex.session_tainted")
}
