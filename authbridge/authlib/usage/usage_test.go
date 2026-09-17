package usage

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// fixedClock returns a controllable now, so bucket boundaries are exact rather
// than dependent on when the test happens to run.
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func respEvent(at time.Time, status int, dur time.Duration, model string, tokens int) *pipeline.SessionEvent {
	e := &pipeline.SessionEvent{
		At:         at,
		Direction:  pipeline.Outbound,
		Phase:      pipeline.SessionResponse,
		StatusCode: status,
		Duration:   dur,
	}
	if model != "" || tokens > 0 {
		e.Inference = &pipeline.InferenceExtension{Model: model, TotalTokens: tokens}
	}
	return e
}

// withCost attaches the cost event litellm-budget-track publishes, exactly as a
// listener would after SnapshotPlugins. Returns e so it composes with respEvent.
func withCost(t *testing.T, e *pipeline.SessionEvent, costUSD float64) *pipeline.SessionEvent {
	t.Helper()
	raw, err := json.Marshal(costevent.Event{
		CostUSD: costUSD,
		Source:  costevent.SourceGatewayHeader,
	})
	if err != nil {
		t.Fatalf("marshal cost event: %v", err)
	}
	if e.Plugins == nil {
		e.Plugins = map[string]json.RawMessage{}
	}
	e.Plugins[costevent.PluginName] = raw
	return e
}

// TestCountsAddFoldsPricedRequests is why coverage is a counter and not a
// boolean: buckets are summed when a client asks for a coarser resolution, and
// a bool cannot express "12 of 40 requests in this window were priced".
func TestCountsAddFoldsPricedRequests(t *testing.T) {
	a := Counts{Requests: 10, CostMicros: 500, PricedRequests: 4}
	a.Add(Counts{Requests: 5, CostMicros: 250, PricedRequests: 3})

	if a.Requests != 15 {
		t.Errorf("Requests = %d, want 15", a.Requests)
	}
	if a.CostMicros != 750 {
		t.Errorf("CostMicros = %d, want 750", a.CostMicros)
	}
	if a.PricedRequests != 7 {
		t.Errorf("PricedRequests = %d, want 7", a.PricedRequests)
	}
	if unpriced := a.Requests - a.PricedRequests; unpriced != 8 {
		t.Errorf("unpriced = %d, want 8", unpriced)
	}
}

// TestCountsPricedRequestsOmittedWhenZero keeps the wire quiet for deployments
// that price nothing.
func TestCountsPricedRequestsOmittedWhenZero(t *testing.T) {
	b, err := json.Marshal(Counts{Requests: 3})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "pricedRequests") {
		t.Errorf("zero PricedRequests should be omitted, got %s", b)
	}
}

// The cost is taken from the figure litellm-budget-track already settled and
// published, not modelled here from a rate table.
func TestRecord_PricesFromCostEvent(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 30, 0, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	a.Record("s1", withCost(t, respEvent(now, 200, time.Second, "claude-opus-5", 1000), 0.0421))

	snap := a.Snapshot(time.Minute, BucketWidth, "", GroupNone)
	if snap.Totals.Requests != 1 {
		t.Fatalf("Requests = %d, want 1", snap.Totals.Requests)
	}
	if snap.Totals.CostMicros != 42_100 {
		t.Errorf("CostMicros = %d, want 42100", snap.Totals.CostMicros)
	}
	if snap.Totals.PricedRequests != 1 {
		t.Errorf("PricedRequests = %d, want 1", snap.Totals.PricedRequests)
	}
}

// Partial coverage: the dollar total covers only the priced subset, and the gap
// between PricedRequests and Requests is what tells a client to say so rather
// than present the figure as complete.
func TestRecord_MixedCoverage(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 30, 0, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	a.Record("s1", withCost(t, respEvent(now, 200, time.Second, "claude-opus-5", 1000), 0.02))
	a.Record("s1", respEvent(now, 200, time.Second, "unpriced-model", 500)) // no cost event

	snap := a.Snapshot(time.Minute, BucketWidth, "", GroupNone)
	if snap.Totals.Requests != 2 {
		t.Fatalf("Requests = %d, want 2", snap.Totals.Requests)
	}
	if snap.Totals.PricedRequests != 1 {
		t.Errorf("PricedRequests = %d, want 1", snap.Totals.PricedRequests)
	}
	if snap.Totals.CostMicros != 20_000 {
		t.Errorf("CostMicros = %d, want 20000 (only the priced request)", snap.Totals.CostMicros)
	}
	// Tokens are unaffected by pricing: a request nobody could price is still a
	// request that sent tokens.
	if snap.Totals.Tokens != 1500 {
		t.Errorf("Tokens = %d, want 1500 (both requests)", snap.Totals.Tokens)
	}
}

func TestRecord_UnpricedContributesNoCost(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 30, 0, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	a.Record("s1", respEvent(now, 200, time.Second, "claude-opus-5", 700))

	snap := a.Snapshot(time.Minute, BucketWidth, "", GroupNone)
	if snap.Totals.CostMicros != 0 {
		t.Errorf("CostMicros = %d, want 0", snap.Totals.CostMicros)
	}
	if snap.Totals.PricedRequests != 0 {
		t.Errorf("PricedRequests = %d, want 0", snap.Totals.PricedRequests)
	}
	if snap.Totals.Tokens != 700 {
		t.Errorf("Tokens = %d, want 700", snap.Totals.Tokens)
	}
}

// Coverage must survive folding to a coarser resolution, which is the whole
// reason it is a counter.
func TestRecord_CoverageFoldsAcrossBuckets(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 2, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	a.Record("s1", withCost(t, respEvent(now.Add(-time.Minute), 200, time.Second, "m", 100), 0.01))
	a.Record("s1", respEvent(now.Add(-time.Minute), 200, time.Second, "m", 100))
	a.Record("s1", withCost(t, respEvent(now, 200, time.Second, "m", 100), 0.02))

	folded := a.Snapshot(2*time.Minute, 2*time.Minute, "", GroupNone).Buckets[0]
	if folded.Requests != 3 {
		t.Fatalf("folded Requests = %d, want 3", folded.Requests)
	}
	if folded.PricedRequests != 2 {
		t.Errorf("folded PricedRequests = %d, want 2", folded.PricedRequests)
	}
	if folded.CostMicros != 30_000 {
		t.Errorf("folded CostMicros = %d, want 30000", folded.CostMicros)
	}
}

// An idle minute must come back as a present, zeroed bucket. A client cannot
// otherwise tell "no traffic" from "fell off the ring", and that distinction is
// what makes a gap visible in a bar chart.
func TestSnapshot_IdleBucketsArePresentAndZero(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 0, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	a.Record("s1", respEvent(now.Add(-9*time.Minute), 200, time.Second, "claude-sonnet-5", 100))

	snap := a.Snapshot(10*time.Minute, BucketWidth, "", GroupNone)
	if len(snap.Buckets) != 10 {
		t.Fatalf("got %d buckets, want 10", len(snap.Buckets))
	}
	if snap.Buckets[0].Requests != 1 {
		t.Errorf("oldest bucket requests = %d, want 1", snap.Buckets[0].Requests)
	}
	for i, b := range snap.Buckets[1:] {
		if b.Requests != 0 || b.Tokens != 0 {
			t.Errorf("bucket %d should be idle, got %+v", i+1, b.Counts)
		}
		if b.At.IsZero() {
			t.Errorf("idle bucket %d has no timestamp — a client cannot place it on an axis", i+1)
		}
	}
	// Buckets must be chronological and exactly one width apart.
	for i := 1; i < len(snap.Buckets); i++ {
		if d := snap.Buckets[i].At.Sub(snap.Buckets[i-1].At); d != BucketWidth {
			t.Errorf("gap between bucket %d and %d = %s, want %s", i-1, i, d, BucketWidth)
		}
	}
}

// Mean and stddev must come out of the running sums correctly. Values chosen so
// the answer is exact in binary floating point.
func TestSnapshot_LatencyMeanAndStdDev(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	// 1000ms, 2000ms, 3000ms -> mean 2000, population stddev sqrt(2/3)*1000.
	for _, ms := range []int{1000, 2000, 3000} {
		a.Record("s1", respEvent(now, 200, time.Duration(ms)*time.Millisecond, "m", 0))
	}

	b := a.Snapshot(time.Minute, BucketWidth, "", GroupNone).Buckets[0]
	if b.LatMeanMs != 2000 {
		t.Errorf("mean = %v, want 2000", b.LatMeanMs)
	}
	want := math.Sqrt(2.0/3.0) * 1000
	if math.Abs(b.LatStdDevMs-want) > 1e-6 {
		t.Errorf("stddev = %v, want %v", b.LatStdDevMs, want)
	}
}

// Identical samples have zero variance; float cancellation can make the
// intermediate negative, which would NaN the sqrt.
func TestSnapshot_IdenticalLatenciesGiveZeroStdDev(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))
	for i := 0; i < 5; i++ {
		a.Record("s1", respEvent(now, 200, 1234*time.Millisecond, "m", 0))
	}
	b := a.Snapshot(time.Minute, BucketWidth, "", GroupNone).Buckets[0]
	if b.LatStdDevMs != 0 {
		t.Errorf("stddev = %v, want exactly 0", b.LatStdDevMs)
	}
	if math.IsNaN(b.LatStdDevMs) {
		t.Error("stddev is NaN — negative variance was not guarded")
	}
}

// A zero duration means "not measured", not "instant": folding it in would drag
// the mean toward zero and misreport latency.
func TestSnapshot_ZeroDurationExcludedFromMean(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))
	a.Record("s1", respEvent(now, 200, 2*time.Second, "m", 0))
	a.Record("s1", respEvent(now, 200, 0, "m", 0)) // unmeasured

	b := a.Snapshot(time.Minute, BucketWidth, "", GroupNone).Buckets[0]
	if b.Requests != 2 {
		t.Errorf("requests = %d, want 2 (both count as traffic)", b.Requests)
	}
	// The mean is over MEASURED requests only. Dividing by Requests would report
	// 1000ms for a single 2000ms response, understating latency in proportion to
	// how many responses arrived unmeasured.
	if b.LatMeanMs != 2000 {
		t.Errorf("mean = %v, want 2000 (one measured 2000ms response)", b.LatMeanMs)
	}
	if b.LatSamples != 1 {
		t.Errorf("latSamples = %d, want 1 — only one response carried a duration", b.LatSamples)
	}
}

// The same dilution must not reappear when buckets are folded: a wider bucket's
// mean has to weight each source bucket by its measured-sample count, not by its
// request count.
func TestFold_LatencyExcludesUnmeasuredRequests(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 2, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	// Minute A: one measured 2000ms response plus 9 unmeasured ones.
	minA := now.Add(-time.Minute)
	a.Record("s1", respEvent(minA, 200, 2000*time.Millisecond, "m", 0))
	for i := 0; i < 9; i++ {
		a.Record("s1", respEvent(minA, 200, 0, "m", 0))
	}
	// Minute B: one measured 4000ms response.
	a.Record("s1", respEvent(now, 200, 4000*time.Millisecond, "m", 0))

	folded := a.Snapshot(2*time.Minute, 2*time.Minute, "", GroupNone).Buckets[0]
	if folded.Requests != 11 {
		t.Fatalf("requests = %d, want 11", folded.Requests)
	}
	// Two measured samples: (2000 + 4000) / 2 = 3000.
	if folded.LatMeanMs != 3000 {
		t.Errorf("folded mean = %v, want 3000 (two measured samples)", folded.LatMeanMs)
	}
	if folded.LatSamples != 2 {
		t.Errorf("folded latSamples = %d, want 2", folded.LatSamples)
	}
}

// >=400 counts as an error, and a denial counts too: an auth outage must not
// look like a traffic drop.
func TestRecord_ErrorsAndDenials(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	a.Record("s1", respEvent(now, 200, time.Second, "m", 10))
	a.Record("s1", respEvent(now, 429, time.Second, "m", 0))
	a.Record("s1", respEvent(now, 500, time.Second, "m", 0))
	denied := respEvent(now, 0, time.Second, "", 0)
	denied.Phase = pipeline.SessionDenied
	a.Record("s1", denied)

	b := a.Snapshot(time.Minute, BucketWidth, "", GroupStatus).Buckets[0]
	if b.Requests != 4 {
		t.Errorf("requests = %d, want 4", b.Requests)
	}
	if b.Errors != 3 {
		t.Errorf("errors = %d, want 3 (429, 500, denied)", b.Errors)
	}
	if _, ok := b.Series["denied"]; !ok {
		t.Errorf("denied events need their own status key; got keys %v", keys(b.Series))
	}
}

// Request events carry no status, duration or usage. Counting them would double
// every request.
func TestRecord_IgnoresRequestPhase(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))
	req := respEvent(now, 0, 0, "m", 0)
	req.Phase = pipeline.SessionRequest
	a.Record("s1", req)

	if got := a.Snapshot(time.Minute, BucketWidth, "", GroupNone).Totals.Requests; got != 0 {
		t.Errorf("requests = %d, want 0 — request-phase events must not count", got)
	}
}

// All four groupings accumulate simultaneously, so an operator cycling the
// group parameter sees the same history from each angle rather than each
// grouping starting empty when first selected.
func TestSnapshot_AllGroupingsPopulatedFromOnePass(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	e := respEvent(now, 200, time.Second, "claude-sonnet-5", 500)
	e.Host = "api.anthropic.com"
	e.Invocations = &pipeline.Invocations{Outbound: []pipeline.Invocation{{Plugin: "inference-parser"}}}
	a.Record("s1", e)

	for _, tc := range []struct {
		group Group
		key   string
	}{
		{GroupMethod, "claude-sonnet-5"},
		{GroupStatus, "200"},
		{GroupPlugin, "inference-parser"},
		{GroupHost, "api.anthropic.com"},
	} {
		b := a.Snapshot(time.Minute, BucketWidth, "", tc.group).Buckets[0]
		if _, ok := b.Series[tc.key]; !ok {
			t.Errorf("group=%s missing key %q; got %v", tc.group, tc.key, keys(b.Series))
		}
	}
	if b := a.Snapshot(time.Minute, BucketWidth, "", GroupNone).Buckets[0]; b.Series != nil {
		t.Error("group=none must omit series entirely")
	}
}

// Cost needs a source. Without a cost event on the request, CostMicros stays
// zero AND Priced is false, so a client can say "unavailable" instead of
// rendering $0.00 — which would read as "this traffic was free".
//
// Formerly TestSnapshot_CostRequiresPricer: the assertions are unchanged, but the
// source is now the figure litellm-budget-track publishes rather than an injected
// Pricer that no production caller ever supplied.
func TestSnapshot_CostRequiresACostSource(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 30, 0, time.UTC)

	unpriced := New(WithClock(fixedClock(now)))
	unpriced.Record("s1", respEvent(now, 200, time.Second, "claude-sonnet-5", 1000))
	snap := unpriced.Snapshot(time.Minute, BucketWidth, "", GroupNone)
	if snap.Priced {
		t.Error("Priced = true with no cost event on any request")
	}
	if snap.Totals.CostMicros != 0 {
		t.Errorf("costMicros = %d, want 0", snap.Totals.CostMicros)
	}

	// $0.00152 — roughly 1000 sonnet input tokens at $1.52/Mtok.
	priced := New(WithClock(fixedClock(now)))
	priced.Record("s1", withCost(t, respEvent(now, 200, time.Second, "claude-sonnet-5", 1000), 0.00152))
	snap = priced.Snapshot(time.Minute, BucketWidth, "", GroupNone)
	if !snap.Priced {
		t.Error("Priced = false after a request carried a cost event")
	}
	if snap.Totals.CostMicros != 1520 {
		t.Errorf("costMicros = %d, want 1520", snap.Totals.CostMicros)
	}
}

// Priced is derived from what actually happened, not from whether a hook was
// installed. Partial coverage still reports Priced — the caller distinguishes
// complete from partial by comparing PricedRequests against Requests.
func TestSnapshot_PricedDerivedFromCoverage(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 30, 0, 0, time.UTC)

	t.Run("nothing priced", func(t *testing.T) {
		a := New(WithClock(fixedClock(now)))
		a.Record("s", respEvent(now, 200, time.Second, "m", 100))
		if a.Snapshot(time.Minute, BucketWidth, "", GroupNone).Priced {
			t.Error("Priced = true, want false when no request was priced")
		}
	})

	t.Run("partially priced still reports priced", func(t *testing.T) {
		a := New(WithClock(fixedClock(now)))
		a.Record("s", withCost(t, respEvent(now, 200, time.Second, "m", 100), 0.01))
		a.Record("s", respEvent(now, 200, time.Second, "n", 100))

		snap := a.Snapshot(time.Minute, BucketWidth, "", GroupNone)
		if !snap.Priced {
			t.Error("Priced = false, want true")
		}
		if snap.Totals.PricedRequests != 1 || snap.Totals.Requests != 2 {
			t.Errorf("coverage = %d/%d, want 1/2",
				snap.Totals.PricedRequests, snap.Totals.Requests)
		}
	})
}

// Per-session rings must isolate: one session's traffic cannot appear in
// another's chart, while both land in the all-sessions total.
func TestSnapshot_SessionIsolation(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	a.Record("alice", respEvent(now, 200, time.Second, "m", 100))
	a.Record("bob", respEvent(now, 200, time.Second, "m", 700))

	if got := a.Snapshot(time.Minute, BucketWidth, "alice", GroupNone).Totals.Tokens; got != 100 {
		t.Errorf("alice tokens = %d, want 100", got)
	}
	if got := a.Snapshot(time.Minute, BucketWidth, "bob", GroupNone).Totals.Tokens; got != 700 {
		t.Errorf("bob tokens = %d, want 700", got)
	}
	if got := a.Snapshot(time.Minute, BucketWidth, "", GroupNone).Totals.Tokens; got != 800 {
		t.Errorf("all-sessions tokens = %d, want 800", got)
	}
}

// An unknown session yields zeroed buckets, not an error: a live session that
// has produced no response events yet is a normal state.
func TestSnapshot_UnknownSessionIsZeroedNotError(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))
	snap := a.Snapshot(10*time.Minute, BucketWidth, "nope", GroupNone)
	if len(snap.Buckets) != 10 {
		t.Fatalf("got %d buckets, want 10", len(snap.Buckets))
	}
	if snap.Totals.Requests != 0 {
		t.Errorf("totals = %+v, want zero", snap.Totals)
	}
}

// At the cap, the NEWEST session must be answerable and the coldest ring
// reclaimed. Refusing new sessions instead would leave /v1/sessions listing a
// live session while /v1/usage?session=<id> returned zeros — indistinguishable
// from "that session did nothing" — and would stay that way for the life of the
// process, since the store expires sessions without telling the aggregator.
func TestRecord_SessionCapReclaimsColdestRing(t *testing.T) {
	base := time.Date(2026, 9, 4, 23, 0, 0, 0, time.UTC)
	now := base
	a := New(WithClock(func() time.Time { return now }), WithMaxSessions(2))

	// s1 at 23:00, s2 at 23:01 — both tracked, s1 is now the coldest.
	a.Record("s1", respEvent(base, 200, time.Second, "m", 100))
	a.Record("s2", respEvent(base.Add(time.Minute), 200, time.Second, "m", 100))

	// s3 arrives: the cap is reached, so s1's ring is reclaimed.
	now = base.Add(2 * time.Minute)
	a.Record("s3", respEvent(now, 200, time.Second, "m", 100))

	if got := a.Snapshot(10*time.Minute, BucketWidth, "s3", GroupNone).Totals.Requests; got != 1 {
		t.Errorf("newest session s3 must be answerable, got %d requests", got)
	}
	if got := a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupNone).Totals.Requests; got != 0 {
		t.Errorf("coldest session s1 should have been reclaimed, got %d requests", got)
	}
	if got := a.Snapshot(10*time.Minute, BucketWidth, "s2", GroupNone).Totals.Requests; got != 1 {
		t.Errorf("s2 was warmer than s1 and should survive, got %d requests", got)
	}
	// Every event still counts in the all-sessions aggregate regardless.
	if got := a.Snapshot(10*time.Minute, BucketWidth, "", GroupNone).Totals.Requests; got != 3 {
		t.Errorf("all-sessions requests = %d, want 3", got)
	}
}

// Memory must stay bounded no matter how many distinct session ids arrive.
func TestRecord_SessionRingsStayBounded(t *testing.T) {
	base := time.Date(2026, 9, 4, 23, 0, 0, 0, time.UTC)
	now := base
	a := New(WithClock(func() time.Time { return now }), WithMaxSessions(4))

	for i := 0; i < 200; i++ {
		now = base.Add(time.Duration(i) * time.Second)
		a.Record(fmt.Sprintf("session-%d", i), respEvent(now, 200, time.Second, "m", 1))
	}
	a.mu.RLock()
	n := len(a.sessions)
	a.mu.RUnlock()
	if n > 4 {
		t.Errorf("retained %d rings, cap is 4", n)
	}
}

// A slot reused on a later lap must reset, not accumulate onto stale data —
// this is what makes the ring self-expiring with no sweeper.
func TestRecord_StaleSlotResetsOnReuse(t *testing.T) {
	base := time.Date(2026, 9, 4, 20, 0, 30, 0, time.UTC)
	now := base
	a := New(WithClock(func() time.Time { return now }))

	a.Record("s1", respEvent(base, 200, time.Second, "m", 999))

	// Exactly one full lap later: same slot, different minute.
	later := base.Add(NumBuckets * BucketWidth)
	now = later
	a.Record("s1", respEvent(later, 200, time.Second, "m", 5))

	b := a.Snapshot(time.Minute, BucketWidth, "", GroupNone).Buckets[0]
	if b.Tokens != 5 {
		t.Errorf("tokens = %d, want 5 — stale lap data was not reset", b.Tokens)
	}
}

// Invocations is nil on any event no plugin recorded against — the common case.
// This panicked inside Store.Append (the request hot path) before the nil guard.
func TestRecord_NilInvocationsDoesNotPanic(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	e := respEvent(now, 200, time.Second, "m", 10)
	if e.Invocations != nil {
		t.Fatal("precondition: helper should leave Invocations nil")
	}
	a.Record("s1", e) // must not panic

	if got := a.Snapshot(time.Minute, BucketWidth, "", GroupPlugin).Buckets[0]; got.Requests != 1 {
		t.Errorf("requests = %d, want 1", got.Requests)
	}
}

// WithMaxSessions(0) means track NO per-session rings — the reading the name
// implies. It previously fell through a `> 0` guard and disabled the cap
// entirely, so a 0 from config would have meant "unlimited".
func TestWithMaxSessions_ZeroTracksNoSessions(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)), WithMaxSessions(0))

	a.Record("s1", respEvent(now, 200, time.Second, "m", 100))

	// The all-sessions aggregate still counts everything.
	if got := a.Snapshot(time.Minute, BucketWidth, "", GroupNone).Totals.Tokens; got != 100 {
		t.Errorf("all-sessions tokens = %d, want 100", got)
	}
	// But no per-session breakdown exists.
	if got := a.Snapshot(time.Minute, BucketWidth, "s1", GroupNone).Totals.Tokens; got != 0 {
		t.Errorf("per-session tokens = %d, want 0 with maxSessions=0", got)
	}
}

func TestParseWindow(t *testing.T) {
	if d, err := ParseWindow(""); err != nil || d != 10*time.Minute {
		t.Errorf("default = %v, %v; want 10m, nil", d, err)
	}
	for _, s := range []string{"10m", "1h", "6h"} {
		if _, err := ParseWindow(s); err != nil {
			t.Errorf("ParseWindow(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range []string{"30s", "7h", "90s", "garbage"} {
		if _, err := ParseWindow(s); err == nil {
			t.Errorf("ParseWindow(%q) = nil, want an error", s)
		}
	}
}

// Validation errors must not echo caller input: this is served unauthenticated,
// so reflecting query bytes into a response body would be a reflection
// primitive.
func TestParseErrors_DoNotEchoInput(t *testing.T) {
	const probe = "<script>alert(1)</script>"
	if _, err := ParseGroup(probe); err == nil {
		t.Fatal("expected an error")
	} else if contains(err.Error(), probe) {
		t.Errorf("group error echoes caller input: %q", err)
	}
	if _, err := ParseWindow(probe); err == nil {
		t.Fatal("expected an error")
	} else if contains(err.Error(), probe) {
		t.Errorf("window error echoes caller input: %q", err)
	}
}

func contains(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func keys(m map[string]Counts) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// The model name is request-controlled, so label cardinality is set off-host. A
// caller varying it every request must not be able to grow a bucket's map
// without bound — that is memory growth driven from outside the process, on the
// synchronous session-append path.
func TestRecord_LabelCardinalityIsCapped(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	const attempts = maxLabelsPerBucket * 4
	for i := 0; i < attempts; i++ {
		a.Record("s1", respEvent(now, 200, time.Second, fmt.Sprintf("model-%d", i), 10))
	}

	b := a.Snapshot(time.Minute, BucketWidth, "", GroupMethod).Buckets[0]
	if len(b.Series) > maxLabelsPerBucket {
		t.Errorf("byMethod holds %d labels, want at most %d", len(b.Series), maxLabelsPerBucket)
	}
	if _, ok := b.Series[overflowLabel]; !ok {
		t.Errorf("excess labels should fold into %q; got keys %v", overflowLabel, keys(b.Series))
	}
	// Totals must survive the capping: the point is to bound keys, not lose data.
	if b.Requests != attempts {
		t.Errorf("requests = %d, want %d — capping must not drop traffic", b.Requests, attempts)
	}
	var seriesTotal int64
	for _, c := range b.Series {
		seriesTotal += c.Requests
	}
	if seriesTotal != attempts {
		t.Errorf("series sums to %d, want %d — segments must still sum to the bucket", seriesTotal, attempts)
	}
}

// A pathologically long model name must not be retained whole.
func TestRecord_LabelLengthIsCapped(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	huge := strings.Repeat("m", 100_000)
	a.Record("s1", respEvent(now, 200, time.Second, huge, 10))

	b := a.Snapshot(time.Minute, BucketWidth, "", GroupMethod).Buckets[0]
	for k := range b.Series {
		if len(k) > maxLabelLen {
			t.Errorf("retained a %d-byte label, cap is %d", len(k), maxLabelLen)
		}
	}
}

// One upstream must be one band. A CONNECT tunnel-open records the authority
// from the request line, port included, while the request parsed inside that
// tunnel records the bare host — so without stripping, "api.anthropic.com:443"
// and "api.anthropic.com" are two series for one host, and neither carries the
// real total.
func TestRecord_HostGroupingStripsThePort(t *testing.T) {
	now := time.Date(2026, 9, 17, 14, 0, 0, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	for _, h := range []string{"api.anthropic.com:443", "api.anthropic.com", "api.anthropic.com:443"} {
		e := respEvent(now, 200, time.Second, "claude-sonnet-5", 100)
		e.Host = h
		a.Record("s1", e)
	}

	b := a.Snapshot(time.Minute, BucketWidth, "", GroupHost).Buckets[0]
	if len(b.Series) != 1 {
		t.Fatalf("one host must be one series; got %v", keys(b.Series))
	}
	got, ok := b.Series["api.anthropic.com"]
	if !ok {
		t.Fatalf("series not keyed by the bare host; got %v", keys(b.Series))
	}
	if got.Requests != 3 {
		t.Errorf("Requests = %d, want 3 — the band must carry every request to that host", got.Requests)
	}
}

// An event the listener left host-less must not invent a band. It stays out of
// the series and abctl renders the difference as "(unlabelled)", which is what
// the other groupings already do for events they skip.
func TestRecord_HostGroupingSkipsEmptyHost(t *testing.T) {
	now := time.Date(2026, 9, 17, 14, 0, 0, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	withHost := respEvent(now, 200, time.Second, "claude-sonnet-5", 100)
	withHost.Host = "github-tool-mcp"
	a.Record("s1", withHost)
	a.Record("s1", respEvent(now, 200, time.Second, "claude-sonnet-5", 100)) // Host unset

	snap := a.Snapshot(time.Minute, BucketWidth, "", GroupHost)
	if snap.Totals.Requests != 2 {
		t.Fatalf("Totals.Requests = %d, want 2", snap.Totals.Requests)
	}
	b := snap.Buckets[0]
	if len(b.Series) != 1 {
		t.Fatalf("host-less event must not add a band; got %v", keys(b.Series))
	}
	if _, ok := b.Series[""]; ok {
		t.Error(`a "" key would render as a nameless band`)
	}
	// The banded total is deliberately less than the bucket total: that shortfall
	// is what the renderer names "(unlabelled)".
	if got := b.Series["github-tool-mcp"].Requests; got != 1 {
		t.Errorf("Requests = %d, want 1", got)
	}
}

// IPv6 authorities must survive: SplitHostPort understands the bracketed form,
// and a bare literal with no port is already the label.
func TestRecord_HostGroupingHandlesIPv6(t *testing.T) {
	now := time.Date(2026, 9, 17, 14, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ authority, want string }{
		{"[::1]:9094", "::1"},
		{"[::1]", "[::1]"},
		{"127.0.0.1:47600", "127.0.0.1"},
	} {
		t.Run(tc.authority, func(t *testing.T) {
			a := New(WithClock(fixedClock(now)))
			e := respEvent(now, 200, time.Second, "", 0)
			e.Host = tc.authority
			a.Record("s1", e)
			b := a.Snapshot(time.Minute, BucketWidth, "", GroupHost).Buckets[0]
			if _, ok := b.Series[tc.want]; !ok {
				t.Errorf("want key %q; got %v", tc.want, keys(b.Series))
			}
		})
	}
}
