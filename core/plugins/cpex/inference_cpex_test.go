// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build cpex

package cpex

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// enginePluginHooks is enginePlugin firing every hook in hooks on requests.
func enginePluginHooks(t *testing.T, mgr Manager, policy string, hooks ...string) *CPEX {
	t.Helper()
	p := NewCPEX()
	p.newManager = func(ManagerOptions) (Manager, error) { return mgr, nil }
	raw, err := json.Marshal(map[string]any{
		"hooks":  map[string]any{"on_request": hooks},
		"config": policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := p.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p
}

func anthropicToolResultRequest(sid string) *pipeline.Context {
	return &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "api.anthropic.com",
		Path:      "/v1/messages",
		Headers:   http.Header{"X-Session-Id": {sid}},
		Body:      []byte(`{"model":"claude-test","messages":[]}`),
		Identity:  claimsIdentity{sub: "alice"},
		Extensions: pipeline.Extensions{Inference: &pipeline.InferenceExtension{
			Model:    "claude-test",
			Messages: []pipeline.InferenceMessage{{Role: "user", Content: "read .env"}},
			ToolResults: []pipeline.InferenceToolResult{
				{ToolUseID: "toolu_1", Content: "AWS_SECRET_ACCESS_KEY=abc123"},
			},
		}},
	}
}

func lastInvocation(t *testing.T, pctx *pipeline.Context) pipeline.Invocation {
	t.Helper()
	if pctx.Extensions.Invocations == nil || len(pctx.Extensions.Invocations.Outbound) == 0 {
		t.Fatal("no invocation recorded")
	}
	inv := pctx.Extensions.Invocations.Outbound
	return inv[len(inv)-1]
}

// A taint on an Anthropic request is a label, not a payload change: the
// request is allowed unmodified and the session stays tainted for the
// tool call that follows.
func TestEngineTaintOnAnthropicLLMInput(t *testing.T) {
	p := enginePluginHooks(t, newEngineManager(t), `
routes:
  - llm: claude-test
    authorization:
      pre_invocation: ["taint(secret, session)"]
  - tool: send_report
    authorization:
      pre_invocation:
        - "security.labels contains \"secret\": deny('session touched secret data', 'session_tainted')"
`, HookLLMInput, HookToolPreInvoke)
	ctx := context.Background()

	req := anthropicToolResultRequest("s1")
	body := string(req.Body)
	wantContinue(t, "llm_input", p.OnRequest(ctx, req))
	if inv := lastInvocation(t, req); inv.Action != pipeline.ActionAllow {
		t.Fatalf("llm_input invocation = %+v, want allow", inv)
	}
	if req.BodyMutated() || string(req.Body) != body {
		t.Fatalf("body rewritten: %s", req.Body)
	}

	report := toolCall("send_report", http.Header{"X-Session-Id": {"s1"}})
	report.Identity = claimsIdentity{sub: "alice"}
	wantReject(t, "send_report after taint", p.OnRequest(ctx, report), "cpex.session_tainted")
}
