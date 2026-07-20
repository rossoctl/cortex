package forwardproxy

import (
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
)

// TestRecordOutboundResponse_PinsRequestSession is the regression guard for
// the streaming-response session-correlation bug: an outbound response must
// record into the SAME session as its request, even when interleaving traffic
// (e.g. a health probe bucketed under "default") flips the global
// ActiveSession() between the request and the stream-end response.
func TestRecordOutboundResponse_PinsRequestSession(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	// The inbound A2A turn's request event landed under "conv-A".
	store.Append("conv-A", pipeline.SessionEvent{
		At:        time.Now().Add(-3 * time.Second),
		Direction: pipeline.Inbound,
		Phase:     pipeline.SessionRequest,
		A2A:       &pipeline.A2AExtension{Method: "message/stream", SessionID: "conv-A"},
	})
	s := &Server{Sessions: store}

	// The outbound inference REQUEST pinned conv-A on the context.
	pctx := &pipeline.Context{
		Direction:         pipeline.Outbound,
		Host:              "ete-litellm.example",
		StartedAt:         time.Now().Add(-3 * time.Second),
		OutboundSessionID: "conv-A",
	}

	// A health probe (no A2A contextId) lands under "default" mid-stream,
	// flipping the global most-recently-updated session.
	store.Append(session.DefaultSessionID, pipeline.SessionEvent{
		At:        time.Now(),
		Direction: pipeline.Inbound,
		Phase:     pipeline.SessionRequest,
		Host:      "claude-agent.team1.svc",
	})
	if got := store.ActiveSession(); got != session.DefaultSessionID {
		t.Fatalf("precondition: ActiveSession() = %q, want %q", got, session.DefaultSessionID)
	}

	// Record the streaming response — must land in the pinned conv-A.
	s.recordOutboundResponseEvent(pctx, 200)

	countResp := func(id string) int {
		v := store.View(id)
		if v == nil {
			return 0
		}
		n := 0
		for _, e := range v.Events {
			if e.Direction == pipeline.Outbound && e.Phase == pipeline.SessionResponse {
				n++
			}
		}
		return n
	}
	if got := countResp("conv-A"); got != 1 {
		t.Fatalf("response not recorded in pinned session conv-A: got %d, want 1", got)
	}
	if got := countResp(session.DefaultSessionID); got != 0 {
		t.Fatalf("response leaked into default (the bug): got %d, want 0", got)
	}
}

// TestRecordOutboundResponse_FallsBackToActiveSession confirms the defensive
// fallback: with no pinned id, recording uses ActiveSession() as before.
func TestRecordOutboundResponse_FallsBackToActiveSession(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	store.Append("conv-B", pipeline.SessionEvent{
		At:        time.Now(),
		Direction: pipeline.Inbound,
		Phase:     pipeline.SessionRequest,
		A2A:       &pipeline.A2AExtension{Method: "message/stream", SessionID: "conv-B"},
	})
	s := &Server{Sessions: store}
	// No OutboundSessionID set.
	pctx := &pipeline.Context{Direction: pipeline.Outbound, Host: "x.example", StartedAt: time.Now()}

	s.recordOutboundResponseEvent(pctx, 200)

	v := store.View("conv-B")
	if v == nil || len(v.Events) != 2 {
		t.Fatalf("expected response recorded under ActiveSession conv-B (2 events), got %+v", v)
	}
}
