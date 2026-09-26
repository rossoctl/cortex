package usage

import (
	"math"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// TestEvictColdest_AnEmptySessionIDIsAnOrdinaryKey covers a sentinel that collides with real data.
//
// evictColdestLocked used "" as its "nothing chosen yet" marker while Record accepts "" as a
// session id and creates a.sessions[""] for it — snapshot_test.go records exactly that, because an
// unattributed event is a case this package handles rather than rejects. Two failures follow: the ""
// ring can never be chosen (its own id reads as "unset", so the next key seen overwrites it
// UNCONDITIONALLY, whatever its lastSeen), and if "" is visited last the function deletes nothing at
// all and the map stays over its cap.
//
// Not reachable from the four listeners, which all fall back to session.DefaultSessionID, but the
// aggregator is an exported API and this is its documented cap behaviour.
func TestEvictColdest_AnEmptySessionIDIsAnOrdinaryKey(t *testing.T) {
	base := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	// A fixed clock: eviction is decided by lastSeen, so the three writes must be ordered.
	a := New(WithMaxSessions(2), WithClock(func() time.Time { return base }))

	rec := func(id string, at time.Time) {
		a.Record(id, &pipeline.SessionEvent{
			At:         at,
			Phase:      pipeline.SessionResponse,
			StatusCode: 200,
			Host:       "gw.example.com",
		})
	}
	rec("", base)                       // coldest
	rec("hot", base.Add(time.Minute))   // warmer
	rec("new", base.Add(2*time.Minute)) // at the cap: something must go, and it is the coldest

	a.mu.RLock()
	ids := make(map[string]bool, len(a.sessions))
	for id := range a.sessions {
		ids[id] = true
	}
	a.mu.RUnlock()

	if len(ids) > 2 {
		t.Errorf("%d session rings with a cap of 2: nothing was evicted, which happens when the coldest key is the one the sentinel cannot name", len(ids))
	}
	if ids[""] {
		t.Errorf(`the "" ring survived (rings: %v): it was the coldest and must be evictable like any other key`, keysOf(ids))
	}
	if !ids["hot"] {
		t.Errorf(`the "hot" ring was evicted (rings: %v): it is warmer than "", so it was discarded only because "" read as "no candidate yet"`, keysOf(ids))
	}
}

// TestSnapshot_WindowReportsWhatItCovers is the field's own godoc, asserted.
//
// Snapshot.Window promises "the span this snapshot ACTUALLY covers... Not necessarily the span
// requested", and names the consequence: a client that echoes its own request "will label six hours
// of spend as a day's". The value was the requested span, so a window past the ring's capacity
// reported itself at full length over a clamped number of buckets — and "6h0m0s", the godoc's own
// example, is exactly what the clamp produces from a 12h request.
func TestSnapshot_WindowReportsWhatItCovers(t *testing.T) {
	a := New()
	ringSpan := time.Duration(NumBuckets) * BucketWidth

	for _, tc := range []struct {
		name    string
		request time.Duration
		want    string
		buckets int
	}{
		{"within the ring", 10 * time.Minute, (10 * time.Minute).String(), 10},
		{"exactly the ring", ringSpan, ringSpan.String(), NumBuckets},
		{"past the ring, clamped", 2 * ringSpan, ringSpan.String(), NumBuckets},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := a.Snapshot(tc.request, BucketWidth, "", GroupModel)

			if len(got.Buckets) != tc.buckets {
				t.Fatalf("buckets = %d, want %d: the fixture is not exercising the clamp it claims to", len(got.Buckets), tc.buckets)
			}
			if got.Window != tc.want {
				t.Errorf("Window = %q over %d buckets (%v of data), want %q: a client reading this labels the chart with it",
					got.Window, len(got.Buckets), time.Duration(len(got.Buckets))*BucketWidth, tc.want)
			}
		})
	}
}

// TestSnapshot_CoverageBreakdownsSaturate applies Counts.Add's own argument to the three maps that
// were summed outside it.
//
// Add checks Requests, Errors and the coverage counters even though traffic cannot reach 2^63 of
// them, on the stated grounds that "a uniform call site is the only kind that cannot be forgotten
// when a field is added". These three breakdowns were three bare += on int64, so two saturated
// buckets summed to -2 — a negative request count, which reads downstream as a series overshooting
// its own total, the same failure CostSum exists to prevent.
func TestSnapshot_CoverageBreakdownsSaturate(t *testing.T) {
	base := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC).Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return base }))

	// Written straight into two raw buckets: Record cannot produce these counts, and the point is
	// what the SUM does when a bucket already holds one.
	for i := 0; i < 2; i++ {
		t0 := base.Add(-time.Duration(i) * BucketWidth)
		b := &a.all[slot(t0)]
		*b = bucket{start: t0}
		b.byUnpriced = map[string]Counts{"gw|m": {Requests: math.MaxInt64}}
		b.byProvenance = map[string]Counts{"configured": {Requests: math.MaxInt64}}
		b.byIncomplete = map[string]Counts{"output-uncounted": {Requests: math.MaxInt64}}
	}

	got := a.Snapshot(10*time.Minute, BucketWidth, "", GroupModel)

	for name, m := range map[string]map[string]int64{
		"UnpricedBy":   got.UnpricedBy,
		"PricedBy":     got.PricedBy,
		"IncompleteBy": got.IncompleteBy,
	} {
		for k, v := range m {
			if v < 0 {
				t.Errorf("%s[%q] = %d: a wrapped request count is worse than a clamped one — it reads as a negative population", name, k, v)
			}
			if v != math.MaxInt64 {
				t.Errorf("%s[%q] = %d, want %d (clamped)", name, k, v, int64(math.MaxInt64))
			}
		}
	}
	if !got.Totals.Saturated {
		t.Error("Totals.Saturated = false after a clamp: the flag is the only thing telling a reader these figures are floors")
	}
}
