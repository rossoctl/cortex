package reverseproxy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// Issue #1045, inbound half. When the backend call fails at the transport level
// ReverseProxy hands the error to errorHandler, which answered 502 and returned
// without recording anything — so the failure existed only on the wire, and the
// request event left behind read as a request still in flight.

// TestReverseProxy_UnreachableBackend_RecordsResponseEvent points a server at an
// address nothing is listening on and asserts the paired 502 response event.
func TestReverseProxy_UnreachableBackend_RecordsResponseEvent(t *testing.T) {
	// allowOnlyPlugin records an invocation on both phases, which is what makes
	// the inbound REQUEST event eligible for recording in the first place (the
	// request-phase gate needs A2A, Invocations, or Custom). The response event
	// under test is recorded unconditionally, but without a request event there
	// is no pair to assert on.
	p, err := pipeline.New([]pipeline.Plugin{allowOnlyPlugin{}})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	store := session.New(5*time.Minute, 100, 100)
	defer store.Close()

	srv, err := NewServer(pipeline.NewHolder(p), store, "http://"+closedAddr(t), nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/work")
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("client status = %d, want 502", resp.StatusCode)
	}

	v := store.View(session.DefaultSessionID)
	if v == nil {
		t.Fatal("no session recorded")
	}
	var respEvent *pipeline.SessionEvent
	for i := range v.Events {
		if v.Events[i].Phase == pipeline.SessionResponse {
			respEvent = &v.Events[i]
		}
	}
	if respEvent == nil {
		t.Fatalf("no response event recorded for a failed backend call; events = %+v", v.Events)
	}
	if respEvent.Direction != pipeline.Inbound {
		t.Errorf("Direction = %q, want inbound", respEvent.Direction)
	}
	if respEvent.StatusCode != http.StatusBadGateway {
		t.Errorf("StatusCode = %d, want 502", respEvent.StatusCode)
	}
	if respEvent.Error == nil {
		t.Fatal("Error = nil, want the classified transport failure")
	}
	if respEvent.Error.Kind != "upstream_refused" {
		t.Errorf("Error.Kind = %q, want upstream_refused", respEvent.Error.Kind)
	}
	if respEvent.Error.Message == "" {
		t.Error("Error.Message is empty; it carries the address and the cause")
	}
	if respEvent.Duration <= 0 {
		t.Errorf("Duration = %v, want > 0 (a zero reads as 'not measured')", respEvent.Duration)
	}
}

// TestReverseProxy_ResponseRejection_RecordsNoResponseEvent pins the branch the
// fix deliberately left alone. A plugin that rejects on the response phase also
// reaches errorHandler, but as a *responseRejectedError — a policy verdict, not a
// transport failure. It must not pick up a synthetic 502 "upstream failed" row,
// which would misattribute the proxy's own decision to the backend.
//
// That this records no response event at all is a real sibling gap (modifyResponse
// returns before its append), but it has a different right answer — a denial row
// carrying the Action — so it is out of scope here. This test documents the
// current behavior so the distinction is deliberate rather than incidental.
func TestReverseProxy_ResponseRejection_RecordsNoResponseEvent(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	p, err := pipeline.New([]pipeline.Plugin{responseRejecter{}})
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

	resp, err := http.Get(proxy.URL + "/work")
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusBadGateway {
		t.Errorf("status = 502; a plugin rejection must not be rendered as a bad gateway")
	}

	v := store.View(session.DefaultSessionID)
	if v == nil {
		return // nothing recorded at all is also "no response event"
	}
	for _, ev := range v.Events {
		if ev.Phase == pipeline.SessionResponse {
			t.Errorf("recorded a response event for a plugin rejection: %+v", ev)
		}
	}
}

// responseRejecter allows the request and rejects on the response phase, which
// is what makes modifyResponse return a *responseRejectedError.
type responseRejecter struct{}

func (responseRejecter) Name() string { return "response-rejecter" }
func (responseRejecter) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}

func (responseRejecter) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	pctx.Allow("ok")
	return pipeline.Action{Type: pipeline.Continue}
}

func (responseRejecter) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	return pipeline.Action{
		Type: pipeline.Reject,
		Violation: &pipeline.Violation{
			Code:   "policy_denied",
			Reason: "test rejects every response",
			Status: http.StatusForbidden,
		},
	}
}

// closedAddr returns an address nothing is listening on: bind an ephemeral port,
// note it, release it. A hardcoded port could collide with a real local service
// and turn the expected refusal into a confusing success.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}
