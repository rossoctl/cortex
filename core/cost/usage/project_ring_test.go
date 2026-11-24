package usage

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

// ringRequests is the Requests count of the bucket of ring that holds minute t.
func ringRequests(ring []bucket, t time.Time) int64 {
	t = t.Truncate(BucketWidth)
	if b := ring[slot(t)]; b.start.Equal(t) {
		return b.Requests
	}
	return 0
}

// ringTotal is the Requests count over every bucket of ring.
func ringTotal(ring []bucket) int64 {
	var n int64
	for i := range ring {
		n += ring[i].Requests
	}
	return n
}

func TestRecord_FillsBothProjectRings(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 30, 0, time.UTC)
	a := New(WithClock(func() time.Time { return now }))
	a.Record("s1", projectEvent(t, "claude-code", "/repo/a"))

	if len(a.projects) != 2 {
		t.Fatalf("len(projects) = %d, want 2: %v", len(a.projects), a.projects)
	}
	for _, k := range []projectKey{{"", "/repo/a"}, {"claude-code", "/repo/a"}} {
		r, ok := a.projects[k]
		if !ok {
			t.Fatalf("no ring for %+v", k)
		}
		if got := ringRequests(r.buckets, now); got != 1 {
			t.Errorf("%+v current bucket Requests = %d, want 1", k, got)
		}
	}
}

func TestRecord_UnknownAgentFillsOnlyTheAllAgentsRing(t *testing.T) {
	a := New()
	a.Record("s1", projectEvent(t, "not-an-agent", "/repo/a"))
	if len(a.projects) != 1 {
		t.Fatalf("len(projects) = %d, want 1: %v", len(a.projects), a.projects)
	}
	if _, ok := a.projects[projectKey{"", "/repo/a"}]; !ok {
		t.Errorf("no all-agents ring for /repo/a: %v", a.projects)
	}
}

func TestRecord_ProjectRingCapEvictsColdest(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 30, 0, time.UTC)
	a := New(WithClock(func() time.Time { return now }))
	for i := 0; i <= maxProjectRings; i++ {
		e := projectEvent(t, "not-an-agent", fmt.Sprintf("/repo/%d", i))
		e.At = now
		a.Record("s1", e)
		now = now.Add(time.Minute)
	}
	if len(a.projects) != maxProjectRings {
		t.Errorf("len(projects) = %d, want %d", len(a.projects), maxProjectRings)
	}
	if _, ok := a.projects[projectKey{"", "/repo/0"}]; ok {
		t.Error("the coldest ring /repo/0 was kept")
	}
	if _, ok := a.projects[projectKey{"", fmt.Sprintf("/repo/%d", maxProjectRings)}]; !ok {
		t.Error("the newest ring was evicted")
	}
}

func TestDeleted_DropsTheSessionFromProjectRings(t *testing.T) {
	a := New()
	a.Record("s1", projectEvent(t, "claude-code", "/repo/a"))
	a.Record("s2", projectEvent(t, "claude-code", "/repo/a"))
	a.Deleted("s1")
	for k, r := range a.projects {
		for i := range r.buckets {
			if _, ok := r.buckets[i].bySession["s1"]; ok {
				t.Errorf("%+v still names s1", k)
			}
		}
		if got := ringTotal(r.buckets); got != 2 {
			t.Errorf("%+v total = %d, want 2 kept", k, got)
		}
	}
}

func TestCleared_DropsSessionLabelsInProjectRings(t *testing.T) {
	a := New()
	a.Record("s1", projectEvent(t, "claude-code", "/repo/a"))
	a.Cleared()
	for k, r := range a.projects {
		for i := range r.buckets {
			if len(r.buckets[i].bySession) != 0 {
				t.Errorf("%+v still names sessions: %v", k, r.buckets[i].bySession)
			}
		}
		if got := ringTotal(r.buckets); got != 1 {
			t.Errorf("%+v total = %d, want 1 kept", k, got)
		}
	}
}

func TestRekeyed_RelabelsProjectRings(t *testing.T) {
	a := New()
	a.Record("pending:x", projectEvent(t, "claude-code", "/repo/a"))
	a.Rekeyed("pending:x", "task-1")
	for k, r := range a.projects {
		var got int64
		for i := range r.buckets {
			if _, ok := r.buckets[i].bySession["pending:x"]; ok {
				t.Errorf("%+v still names pending:x", k)
			}
			got += r.buckets[i].bySession["task-1"].Requests
		}
		if got != 1 {
			t.Errorf("%+v task-1 Requests = %d, want 1", k, got)
		}
	}
}

func TestFinishReplay_MergesProjectRings(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 30, 0, time.UTC)
	live := New(WithClock(func() time.Time { return now }))
	priv := live.BeginReplay()
	e := projectEvent(t, "claude-code", "/repo/a")
	e.At = now.Add(-time.Hour)
	priv.Replay("s1", e)
	live.Record("s1", projectEvent(t, "claude-code", "/repo/a"))
	live.FinishReplay(priv)

	for _, k := range []projectKey{{"", "/repo/a"}, {"claude-code", "/repo/a"}} {
		r, ok := live.projects[k]
		if !ok {
			t.Fatalf("no ring for %+v", k)
		}
		if got := ringTotal(r.buckets); got != 2 {
			t.Errorf("%+v total = %d, want 2", k, got)
		}
	}
}

func TestFinishReplay_KeepsTheProjectRingCap(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 30, 0, time.UTC)
	live := New(WithClock(func() time.Time { return now }))
	priv := live.BeginReplay()
	// Replayed rings are the older ones: an hour back, a minute apart.
	for i := 0; i < 20; i++ {
		e := projectEvent(t, "not-an-agent", fmt.Sprintf("/replay/%d", i))
		e.At = now.Add(-time.Hour + time.Duration(i)*time.Minute)
		priv.Replay("s1", e)
	}
	for i := 0; i < 20; i++ {
		e := projectEvent(t, "not-an-agent", fmt.Sprintf("/live/%d", i))
		e.At = now
		live.Record("s1", e)
	}
	live.FinishReplay(priv)

	if len(live.projects) != maxProjectRings {
		t.Fatalf("len(projects) = %d, want %d", len(live.projects), maxProjectRings)
	}
	// 40 rings, cap 32: the 8 oldest replayed ones go.
	for i := 0; i < 20; i++ {
		_, kept := live.projects[projectKey{"", fmt.Sprintf("/replay/%d", i)}]
		if want := i >= 8; kept != want {
			t.Errorf("/replay/%d kept = %v, want %v", i, kept, want)
		}
		if _, ok := live.projects[projectKey{"", fmt.Sprintf("/live/%d", i)}]; !ok {
			t.Errorf("/live/%d was evicted", i)
		}
	}
}

// BenchmarkProjectRingHeap reports the heap one full ("", p) ring holds: 360 minutes at 5
// events a minute.
func BenchmarkProjectRingHeap(b *testing.B) {
	t := &testing.T{}
	now := time.Date(2026, 11, 20, 12, 0, 30, 0, time.UTC)
	var perRing float64
	for i := 0; i < b.N; i++ {
		clock := now
		a := New(WithClock(func() time.Time { return clock }))
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		for m := 0; m < NumBuckets; m++ {
			clock = now.Add(time.Duration(m) * time.Minute)
			for j := 0; j < 5; j++ {
				e := projectEvent(t, "not-an-agent", "/repo/a")
				e.At = clock
				a.Record("s1", e)
			}
		}
		// The all-sessions and session rings fill alongside. Put both back as they were before
		// the fill, so only the project ring counts.
		a.all, a.sessions = make([]bucket, NumBuckets), map[string]*sessionRing{}
		runtime.GC()
		runtime.ReadMemStats(&after)
		perRing = float64(after.HeapAlloc) - float64(before.HeapAlloc)
		runtime.KeepAlive(a)
	}
	b.ReportMetric(perRing, "B/ring")
}
