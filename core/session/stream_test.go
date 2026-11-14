package session

import (
	"sync/atomic"
	"testing"
	"time"
)

func newStreamStore(t *testing.T) *Store {
	t.Helper()
	s := New(5*time.Minute, 100, 0)
	t.Cleanup(s.Close)
	return s
}

func TestTrackStream_ReadsTheLiveCount(t *testing.T) {
	s := newStreamStore(t)
	var n int64 = 10
	untrack := s.TrackStream("r1", &n)
	defer untrack()
	if got, ok := s.StreamBytes("r1"); got != 10 || !ok {
		t.Fatalf("StreamBytes = (%d, %v), want (10, true)", got, ok)
	}
	atomic.StoreInt64(&n, 25)
	if got, ok := s.StreamBytes("r1"); got != 25 || !ok {
		t.Fatalf("StreamBytes = (%d, %v), want (25, true)", got, ok)
	}
}

func TestTrackStream_UntrackForgets(t *testing.T) {
	s := newStreamStore(t)
	var n int64 = 10
	untrack := s.TrackStream("r1", &n)
	untrack()
	if got, ok := s.StreamBytes("r1"); got != 0 || ok {
		t.Fatalf("StreamBytes = (%d, %v), want (0, false)", got, ok)
	}
	untrack() // a second call must not panic
}

func TestStreamBytes_UnknownID(t *testing.T) {
	s := newStreamStore(t)
	if got, ok := s.StreamBytes("r1"); got != 0 || ok {
		t.Fatalf("StreamBytes = (%d, %v), want (0, false)", got, ok)
	}
}

func TestTrackStream_UntrackKeepsANewerEntry(t *testing.T) {
	s := newStreamStore(t)
	var a, b int64 = 1, 2
	untrackA := s.TrackStream("r1", &a)
	untrackB := s.TrackStream("r1", &b)
	defer untrackB()
	untrackA()
	if got, ok := s.StreamBytes("r1"); got != 2 || !ok {
		t.Fatalf("StreamBytes = (%d, %v), want (2, true)", got, ok)
	}
}
