package session

import (
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

func trailingEvent() pipeline.SessionEvent {
	return pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionResponse, Tunnel: true}
}

// An expired session is gone to every reader before cleanup gets to it, so a trailing
// event must not land in it either.
func TestAppendTrailing_RefusesAnExpiredSession(t *testing.T) {
	clk := &procTestClock{t: time.Unix(1_700_000_000, 0)}
	s := New(time.Minute, 0, 0, WithClock(clk.now))
	defer s.Close()
	b := s.AppendBucket("sess-1", pipeline.SessionEvent{At: clk.now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest})
	clk.advance(2 * time.Minute)

	if s.AppendTrailing(b, trailingEvent()) {
		t.Error("AppendTrailing appended to a session past its ttl")
	}
}

func TestAppendTrailing_NilBucketRecordsNothing(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	if s.AppendTrailing(nil, trailingEvent()) {
		t.Error("AppendTrailing reported success for a nil bucket")
	}
	if n := len(s.ListSessions()); n != 0 {
		t.Errorf("a nil bucket created %d session(s)", n)
	}
}
