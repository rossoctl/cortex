package reverseproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/rossocortex/authbridge/authlib/session"
)

// allowOnlyPlugin records an ALLOW invocation but never sets
// pctx.Extensions.A2A — it simulates a jwt-validation gate on an
// auth-only pipeline that has no a2a-parser. The Holder's pipeline run
// stamps the plugin name onto the invocation (via setCurrent), so no
// manual SetCurrentPlugin is needed here.
type allowOnlyPlugin struct{}

func (allowOnlyPlugin) Name() string                              { return "test-allow" }
func (allowOnlyPlugin) Capabilities() pipeline.PluginCapabilities { return pipeline.PluginCapabilities{} }
func (allowOnlyPlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	pctx.Allow("ok") // records ActionAllow; holder stamps the plugin name
	return pipeline.Action{Type: pipeline.Continue}
}
func (allowOnlyPlugin) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	pctx.Observe("resp-ok") // response-phase invocation; exercises the modifyResponse gate
	return pipeline.Action{Type: pipeline.Continue}
}

// TestReverseProxy_RecordsInboundAllowWithoutA2A is the regression guard
// for the observability bug: reverseproxy only recorded inbound session
// events when pctx.Extensions.A2A was set, so a jwt-validation ALLOW on
// an auth-only pipeline (no a2a-parser) was never recorded — while
// denials always were (recordInboundReject gates on Invocations). The
// request-phase gate is now widened to record when A2A OR Invocations OR
// plugin-public Custom entries are present (mirroring extproc). Before the
// fix, the asserted inbound request event would be absent entirely.
func TestReverseProxy_RecordsInboundAllowWithoutA2A(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	p, err := pipeline.New([]pipeline.Plugin{allowOnlyPlugin{}})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	store := session.New(5*time.Minute, 100, 100)
	defer store.Close()

	srv, err := NewServer(pipeline.NewHolder(p), store, backend.URL, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	// Drive an A2A-less GET request through the proxy.
	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/work", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	resp.Body.Close()

	// No A2A means no contextId, so the event lands in the default bucket.
	v := store.View(session.DefaultSessionID)
	if v == nil {
		t.Fatalf("no session recorded in default bucket; inbound ALLOW was dropped (the bug)")
	}

	var reqEvent *pipeline.SessionEvent
	for i := range v.Events {
		ev := v.Events[i]
		if ev.Direction == pipeline.Inbound && ev.Phase == pipeline.SessionRequest {
			reqEvent = &v.Events[i]
			break
		}
	}
	if reqEvent == nil {
		t.Fatalf("no inbound request event recorded; events=%+v", v.Events)
	}

	// The recorded request event must carry the test-allow ALLOW invocation.
	if reqEvent.Invocations == nil || len(reqEvent.Invocations.Inbound) == 0 {
		t.Fatalf("request event has no inbound invocations: %+v", reqEvent)
	}
	var foundAllow bool
	for _, inv := range reqEvent.Invocations.Inbound {
		if inv.Plugin == "test-allow" && inv.Action == pipeline.ActionAllow {
			foundAllow = true
			break
		}
	}
	if !foundAllow {
		t.Fatalf("test-allow ALLOW invocation not found on request event: %+v", reqEvent.Invocations.Inbound)
	}

	// The response-phase gate in modifyResponse is widened the same way.
	// Assert the inbound response event was recorded with its response-phase
	// invocation, locking in the second gate against regression too.
	var respEvent *pipeline.SessionEvent
	for i := range v.Events {
		ev := v.Events[i]
		if ev.Direction == pipeline.Inbound && ev.Phase == pipeline.SessionResponse {
			respEvent = &v.Events[i]
			break
		}
	}
	if respEvent == nil {
		t.Fatalf("no inbound response event recorded; events=%+v", v.Events)
	}
	if respEvent.Invocations == nil || len(respEvent.Invocations.Inbound) == 0 {
		t.Fatalf("response event has no inbound invocations: %+v", respEvent)
	}
	var foundResp bool
	for _, inv := range respEvent.Invocations.Inbound {
		if inv.Plugin == "test-allow" && inv.Action == pipeline.ActionObserve {
			foundResp = true
			break
		}
	}
	if !foundResp {
		t.Fatalf("test-allow response invocation not found on response event: %+v", respEvent.Invocations.Inbound)
	}
}
