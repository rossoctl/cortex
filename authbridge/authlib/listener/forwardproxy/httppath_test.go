package forwardproxy

import (
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
)

// TestRecordOutboundResponse_CarriesMethodAndPath is the producer half of
// issue #906: traffic that parses as neither A2A, MCP nor inference used to
// reach the timeline carrying only a host, so an operator could not tell a
// token refresh from an object download. The verb and path were already on
// the pipeline context; these assertions pin that they now reach the event.
func TestRecordOutboundResponse_CarriesMethodAndPath(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Method:    "POST",
		Host:      "api.us-east.bob.ibm.com",
		Path:      "/v2/tokens",
		StartedAt: time.Now(),
	}
	s.recordOutboundResponseEvent(pctx, 200)

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 1 {
		t.Fatalf("expected 1 event, got %+v", v)
	}
	if got := v.Events[0].HTTPMethod; got != "POST" {
		t.Errorf("HTTPMethod = %q, want %q", got, "POST")
	}
	if got := v.Events[0].HTTPPath; got != "/v2/tokens" {
		t.Errorf("HTTPPath = %q, want %q", got, "/v2/tokens")
	}
}

// TestRecordTunnelOpened_EmptyPathIsAccurate pins the deliberate asymmetry the
// SessionEvent godoc claims: an opaque tunnel carries a CONNECT verb and NO
// path, because the bytes are never parsed as HTTP and there is no request
// line to read one from. Without this test, someone "fixing" the blank path
// would be fixing an accurate answer.
func TestRecordTunnelOpened_EmptyPathIsAccurate(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	// Mirrors HandleTransparentConn's context: synthetic CONNECT, no Path set.
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Method:    "CONNECT",
		Host:      "api.us-east.bob.ibm.com:443",
	}
	s.recordTunnelOpened(pctx, pipeline.TunnelPassthroughHost)

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 1 {
		t.Fatalf("expected 1 tunnel-open event, got %+v", v)
	}
	ev := v.Events[0]
	if ev.HTTPMethod != "CONNECT" {
		t.Errorf("HTTPMethod = %q, want CONNECT", ev.HTTPMethod)
	}
	if ev.HTTPPath != "" {
		t.Errorf("HTTPPath = %q, want empty: opaque bytes have no request line", ev.HTTPPath)
	}
}
