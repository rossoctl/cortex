package reverseproxy

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// agentIDMutatingPlugin rewrites X-Claude-Code-Agent-Id on pctx.Headers, as
// uaMutatingPlugin does the User-Agent. It records an invocation so the inbound request
// gate records the request row at all.
type agentIDMutatingPlugin struct{}

func (agentIDMutatingPlugin) Name() string { return "agent-id-mutate" }
func (agentIDMutatingPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (agentIDMutatingPlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	pctx.Headers.Set(pipeline.ClaudeCodeAgentIDHeader, "a-plugin-wrote-this")
	pctx.Headers.Set(pipeline.ClaudeCodeParentAgentIDHeader, "a-plugin-wrote-this")
	pctx.Allow("ok")
	return pipeline.Action{Type: pipeline.Continue}
}
func (agentIDMutatingPlugin) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// TestReverseProxy_SubagentIsResolvedBeforeThePipelineRuns drives a real request through
// the proxy, so it covers the request row and modifyResponse's response row together, and
// it pins the ordering the way TestReverseProxy_ClientIsResolvedBeforeThePipelineRuns does
// for the User-Agent: a plugin rewrote the header, and both rows must still name the
// subagent the caller sent. Fails if either site does not copy pctx.Subagent(), and if
// ResolveClient stops pinning it.
func TestReverseProxy_SubagentIsResolvedBeforeThePipelineRuns(t *testing.T) {
	proxyURL, store := serveWithUAPlugin(t, agentIDMutatingPlugin{})
	sub, unsubscribe := store.Subscribe()
	defer unsubscribe()

	req, err := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("x-claude-code-agent-id", "a1a2e36cb5946f31c")
	req.Header.Set("x-claude-code-parent-agent-id", "a0974b0b8591dc76b")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request through the proxy: %v", err)
	}
	_ = resp.Body.Close()

	for _, ev := range inboundEvents(t, sub, 2) {
		if ev.Subagent != "a1a2e36cb5946f31c" {
			t.Errorf("phase %s: Subagent = %q, want the a1a2e36cb5946f31c the CALLER sent", ev.Phase, ev.Subagent)
		}
		if ev.ParentSubagent != "a0974b0b8591dc76b" {
			t.Errorf("phase %s: ParentSubagent = %q, want the a0974b0b8591dc76b the CALLER sent", ev.Phase, ev.ParentSubagent)
		}
	}
}

// TestRecordInboundResponseEvent_CarriesTheSubagent covers the third site, the streaming
// and error-path recorder, called directly because reaching it through the proxy needs
// a streamed or failed exchange.
func TestRecordInboundResponseEvent_CarriesTheSubagent(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	h := http.Header{}
	h.Set(pipeline.ClaudeCodeAgentIDHeader, "a1a2e36cb5946f31c")
	h.Set(pipeline.ClaudeCodeParentAgentIDHeader, "a0974b0b8591dc76b")
	s.recordInboundResponseEvent(&pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    "POST",
		Host:      "weather-agent",
		Path:      "/",
		Headers:   h,
	}, http.StatusOK, nil)

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 1 {
		t.Fatalf("expected 1 response event, got %+v", v)
	}
	if ev := v.Events[0]; ev.Subagent != "a1a2e36cb5946f31c" || ev.ParentSubagent != "a0974b0b8591dc76b" {
		t.Errorf("Subagent/ParentSubagent = %q/%q, want a1a2e36cb5946f31c/a0974b0b8591dc76b", ev.Subagent, ev.ParentSubagent)
	}
}
