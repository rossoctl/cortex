package usage

import (
	"math"
	"strings"
	"testing"
	"time"
)

// Folding must sum counts and keep the bucket count right: a 1h window at 5m
// resolution is 12 bars, which is what a terminal can actually render.
func TestFold_CountsAndBucketCount(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 0, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	// One request per minute for the last 60 minutes, 100 tokens each.
	for i := 0; i < 60; i++ {
		at := now.Add(-time.Duration(i) * time.Minute)
		a.Record("s1", respEvent(at, 200, time.Second, "m", 100))
	}

	snap := a.Snapshot(time.Hour, 5*time.Minute, "", GroupNone)
	if len(snap.Buckets) != 12 {
		t.Fatalf("got %d buckets, want 12 (1h at 5m)", len(snap.Buckets))
	}
	if snap.BucketSeconds != 300 {
		t.Errorf("bucketSeconds = %d, want 300 — a client reads this to label its axis", snap.BucketSeconds)
	}
	for i, b := range snap.Buckets {
		if b.Requests != 5 {
			t.Errorf("bucket %d requests = %d, want 5", i, b.Requests)
		}
		if b.Tokens != 500 {
			t.Errorf("bucket %d tokens = %d, want 500", i, b.Tokens)
		}
	}
	// Totals are summed pre-fold, so they must agree with the folded chart.
	if snap.Totals.Requests != 60 {
		t.Errorf("totals requests = %d, want 60", snap.Totals.Requests)
	}
}

// The mean-of-means trap: a minute with 1 slow request must not weigh as much as
// a minute with 99 fast ones. A naive average of the two per-minute means gives
// 3000ms; the correct request-weighted mean is much lower.
func TestFold_LatencyIsRequestWeightedNotMeanOfMeans(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 5, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	// Minute A: 99 requests at 1000ms.
	minA := now.Add(-1 * time.Minute)
	for i := 0; i < 99; i++ {
		a.Record("s1", respEvent(minA, 200, 1000*time.Millisecond, "m", 0))
	}
	// Minute B: 1 request at 5000ms.
	a.Record("s1", respEvent(now, 200, 5000*time.Millisecond, "m", 0))

	folded := a.Snapshot(2*time.Minute, 2*time.Minute, "", GroupNone)
	if len(folded.Buckets) != 1 {
		t.Fatalf("got %d buckets, want 1", len(folded.Buckets))
	}

	// Correct: (99*1000 + 1*5000) / 100 = 1040ms.
	const want = 1040.0
	if got := folded.Buckets[0].LatMeanMs; math.Abs(got-want) > 1e-6 {
		t.Errorf("folded mean = %v, want %v", got, want)
	}
	// The naive answer would be (1000+5000)/2 = 3000. Guard against regressing
	// to it explicitly, since it is the plausible-looking wrong implementation.
	if got := folded.Buckets[0].LatMeanMs; math.Abs(got-3000) < 1 {
		t.Error("folded mean is the mean-of-means (3000ms), not request-weighted")
	}
}

// Folding must reproduce exactly what one wider bucket would have recorded.
// Compared against the aggregator's own single-bucket arithmetic rather than a
// hand-computed constant, so the two paths are pinned to each other.
func TestFold_MatchesNativeWideBucket(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 2, 30, 0, time.UTC)

	// Folded: three separate minutes, folded to 3m.
	multi := New(WithClock(fixedClock(now)))
	latencies := [][]int{{100, 200}, {300}, {400, 500, 600}}
	for i, group := range latencies {
		at := now.Add(-time.Duration(2-i) * time.Minute)
		for _, ms := range group {
			multi.Record("s1", respEvent(at, 200, time.Duration(ms)*time.Millisecond, "m", 0))
		}
	}
	folded := multi.Snapshot(3*time.Minute, 3*time.Minute, "", GroupNone).Buckets[0]

	// Native: the same six samples all inside one minute.
	single := New(WithClock(fixedClock(now)))
	for _, group := range latencies {
		for _, ms := range group {
			single.Record("s1", respEvent(now, 200, time.Duration(ms)*time.Millisecond, "m", 0))
		}
	}
	native := single.Snapshot(time.Minute, BucketWidth, "", GroupNone).Buckets[0]

	if math.Abs(folded.LatMeanMs-native.LatMeanMs) > 1e-6 {
		t.Errorf("folded mean %v != native %v", folded.LatMeanMs, native.LatMeanMs)
	}
	if math.Abs(folded.LatStdDevMs-native.LatStdDevMs) > 1e-6 {
		t.Errorf("folded stddev %v != native %v", folded.LatStdDevMs, native.LatStdDevMs)
	}
	if folded.Requests != native.Requests {
		t.Errorf("folded requests %d != native %d", folded.Requests, native.Requests)
	}
}

// Grouped series must merge across folded buckets, not be dropped or overwritten.
func TestFold_MergesSeries(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 3, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	a.Record("s1", respEvent(now.Add(-2*time.Minute), 200, time.Second, "sonnet", 10))
	a.Record("s1", respEvent(now.Add(-time.Minute), 429, time.Second, "sonnet", 20))
	a.Record("s1", respEvent(now, 200, time.Second, "opus", 30))

	b := a.Snapshot(3*time.Minute, 3*time.Minute, "", GroupStatus).Buckets[0]
	if got := b.Series["200"].Requests; got != 2 {
		t.Errorf("status 200 requests = %d, want 2", got)
	}
	if got := b.Series["429"].Requests; got != 1 {
		t.Errorf("status 429 requests = %d, want 1", got)
	}
	if got := b.Series["200"].Tokens; got != 40 {
		t.Errorf("status 200 tokens = %d, want 40", got)
	}
}

// An idle stretch must survive folding as a zeroed bucket, not vanish — the
// whole point of emitting idle buckets in the first place.
func TestFold_PreservesIdleBuckets(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 10, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))
	a.Record("s1", respEvent(now, 200, time.Second, "m", 100))

	snap := a.Snapshot(10*time.Minute, 5*time.Minute, "", GroupNone)
	if len(snap.Buckets) != 2 {
		t.Fatalf("got %d buckets, want 2", len(snap.Buckets))
	}
	if snap.Buckets[0].Requests != 0 {
		t.Errorf("first 5m block should be idle, got %d requests", snap.Buckets[0].Requests)
	}
	if snap.Buckets[1].Requests != 1 {
		t.Errorf("second 5m block = %d requests, want 1", snap.Buckets[1].Requests)
	}
}

// A partial trailing group must still be emitted rather than truncated: the
// current, still-filling block is the one an operator is watching.
func TestFold_PartialGroupIsKept(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 7, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))
	a.Record("s1", respEvent(now, 200, time.Second, "m", 100))

	// 7 minutes at 3m resolution: 3 + 3 + 1.
	snap := a.Snapshot(7*time.Minute, 3*time.Minute, "", GroupNone)
	if len(snap.Buckets) != 3 {
		t.Fatalf("got %d buckets, want 3 (3+3+1)", len(snap.Buckets))
	}
	if snap.Buckets[2].Requests != 1 {
		t.Errorf("trailing partial block lost its traffic: %+v", snap.Buckets[2].Counts)
	}
}

func TestParseResolution(t *testing.T) {
	for _, tc := range []struct {
		res    string
		window time.Duration
		want   time.Duration
		ok     bool
	}{
		{"", time.Hour, BucketWidth, true},
		{"1m", time.Hour, time.Minute, true},
		{"5m", time.Hour, 5 * time.Minute, true},
		// Every pair abctl actually sends, so the divisibility rule below cannot
		// break a live client: usageWindows is {10m,1m}, {1h,5m}, {6h,30m}, and a
		// symbolic window omits the parameter entirely and validates 1m against
		// MaxWindow.
		{"1m", 10 * time.Minute, time.Minute, true},
		{"30m", 6 * time.Hour, 30 * time.Minute, true},
		{"", MaxWindow, BucketWidth, true},
		{"30s", time.Hour, 0, false}, // finer than storage
		{"90s", time.Hour, 0, false}, // not a multiple
		{"2h", time.Hour, 0, false},  // coarser than the window
		{"garbage", time.Hour, 0, false},
		// The window must divide by the resolution, or the newest bucket is shorter
		// than the width every bucket is labelled with. 10m at 3m returned four
		// buckets of which the last covered ONE minute under bucketSeconds 180, so a
		// client deriving a burn rate from the newest bar tripled it.
		{"3m", 10 * time.Minute, 0, false},
		{"4m", 10 * time.Minute, 0, false},
		// And a resolution that DOES divide an unusual window is still accepted:
		// the rule is divisibility, not an allowlist of round numbers.
		{"7m", 21 * time.Minute, 7 * time.Minute, true},
	} {
		got, err := ParseResolution(tc.res, tc.window)
		if tc.ok && err != nil {
			t.Errorf("ParseResolution(%q, %v) = %v, want nil", tc.res, tc.window, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("ParseResolution(%q, %v) = nil, want an error", tc.res, tc.window)
		}
		if tc.ok && got != tc.want {
			t.Errorf("ParseResolution(%q, %v) = %v, want %v", tc.res, tc.window, got, tc.want)
		}
	}
}

// The error body must not reflect caller bytes: /v1/usage is unauthenticated, and
// sessionapi forwards this package's message verbatim. The divisibility message is a
// fixed literal for that reason, and this pins it — the two operands are not needed
// to act on it, so there is no reason to interpolate anything.
func TestParseResolution_DivisibilityErrorIsAFixedLiteral(t *testing.T) {
	_, err := ParseResolution("3m", 10*time.Minute)
	if err == nil {
		t.Fatal("ParseResolution(3m, 10m) = nil, want an error")
	}
	for _, leak := range []string{"3m", "10m", "3", "10"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error %q carries %q; this message travels back over an unauthenticated endpoint and must be a fixed literal", err, leak)
		}
	}
}
