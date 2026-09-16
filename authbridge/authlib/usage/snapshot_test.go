package usage

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// mergeSeries sums every bucket's Series into one map, so a test can assert on a
// window total without caring which bucket a record landed in.
//
// Delegates to Counts.Add rather than hand-summing the fields it happens to care
// about: a helper that lists fields is a helper that goes stale the next time one
// is added, which is the bug Add is exported to prevent.
func mergeSeries(buckets []Bucket) map[string]Counts {
	out := map[string]Counts{}
	for _, b := range buckets {
		for k, v := range b.Series {
			cur := out[k]
			cur.Add(v)
			out[k] = cur
		}
	}
	return out
}

func TestSnapshot_GroupModelReturnsTheModelSeries(t *testing.T) {
	a := New()
	a.Record("s1", inferenceEvent("claude-opus-5", 10, 0, 0, 5, 0, 0b1001))
	a.Record("s1", inferenceEvent("claude-haiku-4-5", 20, 0, 0, 7, 0, 0b1001))

	snap := a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupModel)

	series := mergeSeries(snap.Buckets)
	if len(series) != 2 {
		t.Fatalf("series has %d keys (%v), want 2", len(series), series)
	}
	if got := series["claude-opus-5"].InputTokens; got != 10 {
		t.Errorf("claude-opus-5 InputTokens = %d, want 10", got)
	}
	if got := series["claude-haiku-4-5"].InputTokens; got != 20 {
		t.Errorf("claude-haiku-4-5 InputTokens = %d, want 20", got)
	}
}

// A GATEWAY-PRICED RESPONSE WITH NO MODEL COUNTS TOWARD THE TOTAL AND CANNOT BE A
// group=model KEY, so a client summing the breakdown got a smaller number than the
// total beside it with nothing in the response to explain the difference.
//
// inference-parser reads six chat/completion paths plus Anthropic Messages, so
// /v1/embeddings and /v1/rerank arrive with no Inference extension — and costOf still
// settles them from the gateway's own cost header. foldInto guards byMethod on a
// non-empty model, so that spend lands in the bucket total and in no series entry. This
// is the RING half of the claim Snapshot.UngroupedCostMicros makes about both window
// kinds; TestFold_GatewayPricedRowWithNoModelIsDisclosedAsUngrouped is the ledger half.
func TestSnapshot_GatewayPricedTrafficWithNoModelIsDisclosedAsUngrouped(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }))

	// A model the parser read, priced.
	a.Record("s1", withCost(t, respEvent(now, 200, time.Second, "claude-opus-5", 1000), 0.10))
	// And a response it could not: no model, no tokens, a real settled cost.
	a.Record("s1", withCost(t, respEvent(now, 200, time.Second, "", 0), 0.25))

	snap := a.Snapshot(10*BucketWidth, BucketWidth, "s1", GroupModel)

	if snap.Totals.CostMicros != 350_000 {
		t.Fatalf("Totals.CostMicros = %d, want 350000 — both responses are real spend", snap.Totals.CostMicros)
	}
	series := mergeSeries(snap.Buckets)
	if got := series["claude-opus-5"].CostMicros; got != 100_000 {
		t.Errorf("series[claude-opus-5].CostMicros = %d, want 100000; series = %v", got, series)
	}
	if snap.UngroupedCostMicros == nil {
		t.Fatalf("UngroupedCostMicros is absent while the group=model series accounts for only "+
			"%d of %d micros: a client summing the breakdown is short by 250000 dollars-worth "+
			"and nothing in the response says so", seriesCost(series).Micros, snap.Totals.CostMicros)
	}
	if *snap.UngroupedCostMicros != 250_000 {
		t.Errorf("UngroupedCostMicros = %d, want 250000", *snap.UngroupedCostMicros)
	}
	// The arithmetic the field exists to restore, asserted rather than assumed.
	if sum := seriesCost(series).Micros + *snap.UngroupedCostMicros; sum != snap.Totals.CostMicros {
		t.Errorf("series (%d) + ungrouped (%d) = %d, want Totals.CostMicros = %d",
			seriesCost(series).Micros, *snap.UngroupedCostMicros, sum, snap.Totals.CostMicros)
	}
}

// ABSENT, NOT ZERO, when the breakdown accounts for everything — the Degraded
// convention. A `"ungroupedCostMicros":0` on every clean response would read as
// "checked, complete" from paths that check nothing (group=none, group=plugin), which is
// the same false reassurance as $0.00 over unpriced traffic.
func TestSnapshot_AWindowWhoseSeriesAccountsForEverythingCarriesNoUngroupedField(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }))
	a.Record("s1", withCost(t, respEvent(now, 200, time.Second, "claude-opus-5", 1000), 0.10))

	snap := a.Snapshot(10*BucketWidth, BucketWidth, "s1", GroupModel)

	if snap.UngroupedCostMicros != nil {
		t.Errorf("UngroupedCostMicros = %d on a window whose series carries every dollar; "+
			"absence is how a client tells a complete breakdown from a short one", *snap.UngroupedCostMicros)
	}
	body, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "ungroupedCostMicros") {
		t.Errorf("a clean window serialised the field: %s", body)
	}
}

// The two groups that offer no reconciliation must not report one.
//
// group=none asks for no breakdown, so a residual equal to the whole total would appear
// on the DEFAULT request and read as a fault. group=plugin's series counts one request
// once per plugin, so totals-minus-series is not a residual there at all — and with one
// request touching no plugin and another touching two, the shortfall and the duplication
// cancel to a plausible zero, which is why Group.Reconcilable refuses it rather than
// clamping it. See that method.
func TestSnapshot_GroupsThatCannotReconcileReportNoUngroupedCost(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }))
	a.Record("s1", withCost(t, respEvent(now, 200, time.Second, "", 0), 0.25))

	for _, g := range []Group{GroupNone, GroupPlugin} {
		snap := a.Snapshot(10*BucketWidth, BucketWidth, "s1", g)
		if snap.UngroupedCostMicros != nil {
			t.Errorf("group=%s reported ungrouped cost %d; that group has no series to be short of",
				g, *snap.UngroupedCostMicros)
		}
		if g.Reconcilable() {
			t.Errorf("Group(%s).Reconcilable() = true; it has no reconcilable breakdown", g)
		}
	}
}

// TestSetUngroupedCost_ANegativeResidualIsDisclosedNotDropped covers the case the setter
// used to discard.
//
// `micros <= 0` collapsed two answers: "the breakdown accounts for every dollar", which is
// the ordinary clean result, and "the breakdown accounts for MORE dollars than the total
// beside it", which correct code cannot produce. A reconcilable group's series sums to the
// total or to less than it — every event lands in at most one entry — so a negative residual
// says this process is wrong about its own arithmetic, either because a Group is marked
// reconcilable while its series double-counts or because an accumulator counted an event
// twice. Discarding it meant the one place that could see the fault was the place that
// deleted the evidence.
//
// Each direction asserts what the OTHER field does too. A negative residual that set
// UngroupedCostMicros would put a bug report in the field clients render as a spend band.
func TestSetUngroupedCost_ANegativeResidualIsDisclosedNotDropped(t *testing.T) {
	for _, tc := range []struct {
		name          string
		micros        int64
		wantUngrouped *int64
		wantOvershoot *int64
	}{
		{name: "short breakdown is a residual", micros: 250_000, wantUngrouped: ptr(int64(250_000))},
		{name: "exact breakdown discloses nothing", micros: 0},
		{name: "overshooting breakdown is a fault", micros: -250_000, wantOvershoot: ptr(int64(250_000))},
		{
			// The magnitude of math.MinInt64 is not representable, so a blind negation returns
			// the same negative number and publishes the sign confusion the field exists to
			// avoid. Reachable only from a saturated total.
			name:          "the residual is the int64 floor",
			micros:        math.MinInt64,
			wantOvershoot: ptr(int64(math.MaxInt64)),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var snap Snapshot
			snap.SetUngroupedCost(CostSum{Micros: tc.micros})
			for _, f := range []struct {
				name      string
				got, want *int64
			}{
				{"UngroupedCostMicros", snap.UngroupedCostMicros, tc.wantUngrouped},
				{"SeriesOvershootMicros", snap.SeriesOvershootMicros, tc.wantOvershoot},
			} {
				switch {
				case f.want == nil && f.got != nil:
					t.Errorf("SetUngroupedCost(%d) set %s = %d, want absent", tc.micros, f.name, *f.got)
				case f.want != nil && f.got == nil:
					t.Errorf("SetUngroupedCost(%d) left %s absent, want %d — a residual with this "+
						"sign is a signal, and dropping it is how the fault stays invisible",
						tc.micros, f.name, *f.want)
				case f.want != nil && *f.got != *f.want:
					t.Errorf("SetUngroupedCost(%d) set %s = %d, want %d", tc.micros, f.name, *f.got, *f.want)
				}
			}
		})
	}
}

// seriesCost IS THE STEP THAT TURNS A CLAMP INTO A FABRICATED DEFECT REPORT, so it is
// pinned separately from the residual that consumes it.
//
// Snapshot subtracts this sum from the bucket total to get the residual. When the sum
// wrapped negative, that subtraction produced a residual LARGER than the total it was
// computed from — and past the ceiling, so it wrapped in turn, to -1. SetUngroupedCost then
// published SeriesOvershootMicros: 1, telling an operator the series double-counted, from a
// bucket where nothing had. One bucket was enough; it did not need two.
//
// WHY THIS AND NOT THE RING PATH END TO END. The ring cannot be driven here through Record:
// every event's cost is capped at pricing.MaxPlausibleRequestCostMicros ($10,000, 1e10
// micros), so saturating an int64 through the front door takes ~9.2e8 requests inside one
// six-hour ring. The accumulate in Snapshot is therefore DEFENSIVE, and this is the honest
// place to prove the arithmetic: costledger.Fold is the reachable half, because its rows
// come off disk with no bound at all (TestFold_ASaturatedResidualIsABoundNotAFabricatedOvershoot).
//
// Reverting either accumulate to `+=` fails the first assertion, which is the sign.
func TestSeriesCost_ClampsInsteadOfWrappingAndSaysSo(t *testing.T) {
	// Two entries that sum to math.MaxInt64+1 — the first value past the range.
	half := int64(math.MaxInt64/2) + 1
	series := map[string]Counts{
		"claude-opus-5":    {Requests: 1, CostMicros: half},
		"claude-haiku-4-5": {Requests: 1, CostMicros: half},
	}

	got := seriesCost(series)

	if got.Micros < 0 {
		t.Fatalf("seriesCost = %d — negative, so it wrapped. Subtracted from a bucket total that "+
			"is a positive int64, a negative series sum yields a residual bigger than the total "+
			"it came from, and SetUngroupedCost publishes that as the series overshooting",
			got.Micros)
	}
	if got.Micros != math.MaxInt64 {
		t.Errorf("seriesCost = %d, want math.MaxInt64", got.Micros)
	}
	if !got.Saturated {
		t.Error("seriesCost clamped silently: the residual that consumes this sum has no other " +
			"way to learn the breakdown it is being compared against is itself a bound")
	}

	// The consequence, stated as the thing a client sees. Old arithmetic on one bucket
	// holding a saturated total and this series: MaxInt64 - MinInt64 wraps to -1, and -1
	// becomes SeriesOvershootMicros: 1.
	var ungrouped CostSum
	ungrouped.Add(math.MaxInt64)
	ungrouped.Sub(got.Micros)
	if got.Saturated {
		ungrouped.Saturated = true
	}
	var snap Snapshot
	snap.SetUngroupedCost(ungrouped)
	if snap.SeriesOvershootMicros != nil {
		t.Errorf("SeriesOvershootMicros = %d from a bucket whose series sums to its own total: "+
			"that field means this process counted an event twice, and it has not",
			*snap.SeriesOvershootMicros)
	}
	if !snap.Totals.Saturated {
		t.Error("the clamp reached no field a client reads — Totals.Saturated is the one that " +
			"means every money figure here is a bound")
	}
}

// Sub's one special input, which is the input SetUngroupedCost already needed a guard for:
// math.MinInt64 has no positive counterpart, so a naive Add(-micros) would ADD it and the
// residual would come out with the wrong sign — the exact confusion the overshoot field
// exists to make impossible.
func TestCostSum_SubtractingTheInt64FloorClampsRatherThanChangingSign(t *testing.T) {
	var s CostSum
	s.Add(1_000)
	s.Sub(math.MinInt64)

	if s.Micros != math.MaxInt64 {
		t.Errorf("Micros = %d, want math.MaxInt64: subtracting the floor adds 2^63, which is out "+
			"of range whatever the accumulator held", s.Micros)
	}
	if !s.Saturated {
		t.Error("clamped without disclosing it")
	}
}

// The overshoot is absent from a clean response and present when it is not, on the same wire
// rule as every other disclosure here: a client must be able to tell "checked and fine" from
// "not checked", and a zero cannot say both.
func TestSnapshot_SeriesOvershootIsOmittedUnlessItHappened(t *testing.T) {
	var clean Snapshot
	clean.SetUngroupedCost(CostSum{})
	body, err := json.Marshal(clean)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "seriesOvershootMicros") {
		t.Errorf("a clean window serialised the overshoot: %s", body)
	}

	var broken Snapshot
	broken.SetUngroupedCost(CostSum{Micros: -1})
	body, err = json.Marshal(broken)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"seriesOvershootMicros":1`) {
		t.Errorf("an overshooting window did not serialise the fault: %s — the disclosure only "+
			"works if it reaches the client", body)
	}
}

// ptr is a pointer to a value, for the absent-versus-present tables above.
func ptr[T any](v T) *T { return &v }

// Summed from the RAW buckets, like Totals: a client that asked for coarser bars must
// get the same residual as one that asked for fine ones, or the reconciliation would
// hold at one resolution and fail at another.
func TestSnapshot_UngroupedCostIsUnaffectedByTheRequestedResolution(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }))
	a.Record("s1", withCost(t, respEvent(now, 200, time.Second, "", 0), 0.25))

	fine := a.Snapshot(10*BucketWidth, BucketWidth, "s1", GroupModel)
	coarse := a.Snapshot(10*BucketWidth, 5*BucketWidth, "s1", GroupModel)
	if fine.UngroupedCostMicros == nil || coarse.UngroupedCostMicros == nil {
		t.Fatalf("one of the two windows disclosed nothing: fine = %v, coarse = %v",
			fine.UngroupedCostMicros, coarse.UngroupedCostMicros)
	}
	if *fine.UngroupedCostMicros != *coarse.UngroupedCostMicros {
		t.Errorf("ungrouped cost = %d at %v resolution and %d at %v: it must be summed from the "+
			"raw buckets, exactly like Totals", *fine.UngroupedCostMicros, BucketWidth,
			*coarse.UngroupedCostMicros, 5*BucketWidth)
	}
}

// PresentKinds has to be right per SERIES ENTRY, not merely in the window total.
// The blank-column case the field exists for is a by-model read: one model that
// reports cache counters and one that does not must carry DIFFERENT flags under the
// same grouping, or a reader cannot tell an empty CACHE-WRITE cell meaning "this
// model wrote no cache" from one meaning "this model never reports it".
//
// It is carried today because foldInto passes `one` whole to addLabel, but nothing
// asserted it at this level: Totals would stay green if a series entry lost the
// field or inherited another entry's bits.
func TestSnapshot_PresentKindsIsPerSeriesEntry(t *testing.T) {
	a := New()
	a.Record("s1", inferenceEvent("reports-cache", 10, 200, 5, 5, 0, 0b1111))
	a.Record("s1", inferenceEvent("no-cache-fields", 10, 0, 0, 5, 0, 0b1001))

	series := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupModel).Buckets)

	if got := series["reports-cache"].PresentKinds; got != 0b1111 {
		t.Errorf("reports-cache PresentKinds = %#b, want %#b", got, 0b1111)
	}
	if got := series["no-cache-fields"].PresentKinds; got != 0b1001 {
		t.Errorf("no-cache-fields PresentKinds = %#b, want %#b — one model must not inherit another's reported kinds", got, 0b1001)
	}
}

func TestSnapshot_GroupMethodIsAnAliasForModel(t *testing.T) {
	// group=method is on the wire today and tui/usage_pane.go's cycleGroup passes
	// it, so it must keep working -- and it must return the SAME series as
	// group=model, because it was already the model series under a wrong name.
	a := New()
	a.Record("s1", inferenceEvent("claude-opus-5", 10, 0, 0, 5, 0, 0b1001))

	byModel := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupModel).Buckets)
	byMethod := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupMethod).Buckets)

	if len(byModel) != len(byMethod) {
		t.Fatalf("model series has %d keys, method series has %d; want identical", len(byModel), len(byMethod))
	}
	for k, v := range byModel {
		if byMethod[k] != v {
			t.Errorf("key %q: model = %+v, method = %+v; want identical", k, v, byMethod[k])
		}
	}
}

func TestParseGroup_AcceptsModelAndEndpointAndStillAcceptsMethod(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Group
	}{
		{"", GroupNone},
		{"none", GroupNone},
		{"model", GroupModel},
		{"method", GroupMethod},
		{"endpoint", GroupEndpoint},
		{"status", GroupStatus},
		{"plugin", GroupPlugin},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseGroup(tc.in)
			if err != nil {
				t.Fatalf("ParseGroup(%q) error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseGroup(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseGroup_RejectsUnknownWithoutEchoingInput(t *testing.T) {
	// The message crosses an UNAUTHENTICATED endpoint. Reflecting caller bytes
	// into a response body hands out a reflection primitive, so the error names
	// the valid set instead of quoting what it got.
	const attack = "<script>alert(1)</script>"
	_, err := ParseGroup(attack)
	if err == nil {
		t.Fatal("ParseGroup accepted an unknown group")
	}
	if strings.Contains(err.Error(), attack) || strings.Contains(err.Error(), "script") {
		t.Errorf("error echoes caller input: %q", err.Error())
	}
	for _, want := range []string{"model", "endpoint", "status", "plugin"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name the valid value %q", err.Error(), want)
		}
	}
}

func TestSnapshot_GroupEndpointBreaksDownByHost(t *testing.T) {
	a := New()
	e1 := inferenceEvent("m", 10, 0, 0, 5, 0, 0b1001)
	e1.Host = "gw-a.example.com"
	e2 := inferenceEvent("m", 20, 0, 0, 7, 0, 0b1001)
	e2.Host = "gw-b.example.com"
	a.Record("s1", e1)
	a.Record("s1", e2)

	series := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupEndpoint).Buckets)

	if got := series["gw-a.example.com"].InputTokens; got != 10 {
		t.Errorf("gw-a InputTokens = %d, want 10", got)
	}
	if got := series["gw-b.example.com"].InputTokens; got != 20 {
		t.Errorf("gw-b InputTokens = %d, want 20", got)
	}
}

func TestSnapshot_GroupEndpointOmitsEventsWithNoHost(t *testing.T) {
	// Host is empty when the listener did not populate it. An empty-string key in
	// a breakdown table renders as a blank row that looks like a bug.
	a := New()
	e := inferenceEvent("m", 10, 0, 0, 5, 0, 0b1001)
	e.Host = ""
	a.Record("s1", e)

	series := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupEndpoint).Buckets)

	if _, ok := series[""]; ok {
		t.Error(`series has an "" key; an unknown host must be omitted, not shown as a blank row`)
	}
}

func TestSnapshot_GroupSessionBreaksDownBySession(t *testing.T) {
	a := New()
	a.Record("sess-a", inferenceEvent("m", 10, 0, 0, 5, 0, 0b1001))
	a.Record("sess-b", inferenceEvent("m", 20, 0, 0, 7, 0, 0b1001))

	// The ALL-sessions ring must carry the breakdown: one request has to answer
	// for every session, or a sessions list costs one request per row.
	series := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "", GroupSession).Buckets)

	if got := series["sess-a"].InputTokens; got != 10 {
		t.Errorf("sess-a InputTokens = %d, want 10", got)
	}
	if got := series["sess-b"].InputTokens; got != 20 {
		t.Errorf("sess-b InputTokens = %d, want 20", got)
	}
}

func TestSnapshot_GroupSessionOmitsAnEmptyID(t *testing.T) {
	a := New()
	a.Record("", inferenceEvent("m", 10, 0, 0, 5, 0, 0b1001))
	series := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "", GroupSession).Buckets)
	if _, ok := series[""]; ok {
		t.Error(`series has an "" key; an unattributed event must not render as a blank row`)
	}
}

// The per-session ring carries the label too, redundant though it is there. Not
// for its own sake: it pins that foldInto records the id on WHICHEVER ring it is
// folding, so the uniform call site cannot be "optimised" into an all-ring-only
// conditional that a later reader would have to re-derive.
func TestSnapshot_GroupSessionOnAScopedSnapshotNamesOnlyThatSession(t *testing.T) {
	a := New()
	a.Record("sess-a", inferenceEvent("m", 10, 0, 0, 5, 0, 0b1001))
	a.Record("sess-b", inferenceEvent("m", 20, 0, 0, 7, 0, 0b1001))

	series := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "sess-a", GroupSession).Buckets)

	if len(series) != 1 {
		t.Fatalf("scoped series has %d keys (%v), want just sess-a", len(series), series)
	}
	if got := series["sess-a"].InputTokens; got != 10 {
		t.Errorf("sess-a InputTokens = %d, want 10", got)
	}
}

// Session ids are request-derived (the A2A contextId, via
// reverseproxy.inboundSessionID), so the bound that protects every other label map
// has to protect this one. Without it a client varying the contextId every turn
// grows a retained map entry and a retained string per request, in a ring that
// frees a slot only a full lap later.
func TestSnapshot_GroupSessionIsBoundedLikeEveryOtherLabel(t *testing.T) {
	a := New()
	for i := 0; i < maxLabelsPerBucket+50; i++ {
		a.Record(fmt.Sprintf("sess-%03d", i), inferenceEvent("m", 1, 0, 0, 1, 0, 0b1001))
	}

	snap := a.Snapshot(10*time.Minute, BucketWidth, "", GroupSession)
	for _, b := range snap.Buckets {
		if len(b.Series) > maxLabelsPerBucket {
			t.Fatalf("bucket carries %d session labels, want at most %d", len(b.Series), maxLabelsPerBucket)
		}
	}
	series := mergeSeries(snap.Buckets)
	if _, ok := series[overflowLabel]; !ok {
		t.Errorf("no %q key past the cap; the excess was dropped silently instead of being named", overflowLabel)
	}
	// The totals must still reconcile with the sum of the series, overflow
	// included: that is the whole reason overflow is named rather than dropped.
	var sum int64
	for _, c := range series {
		sum += c.Requests
	}
	if sum != snap.Totals.Requests {
		t.Errorf("series requests sum to %d, totals say %d; the overflow key is not absorbing the excess", sum, snap.Totals.Requests)
	}
}

func TestParseGroup_AcceptsSession(t *testing.T) {
	g, err := ParseGroup("session")
	if err != nil {
		t.Fatalf("ParseGroup(\"session\") errored: %v", err)
	}
	if g != GroupSession {
		t.Errorf("ParseGroup(\"session\") = %q, want %q", g, GroupSession)
	}
}

// The error names the valid set instead of echoing the caller's input (see
// ParseGroup). A new grouping the message does not list is a grouping an operator
// cannot discover from the only place the endpoint tells them.
func TestParseGroup_ErrorNamesTheSessionGroup(t *testing.T) {
	_, err := ParseGroup("nonsense")
	if err == nil {
		t.Fatal("ParseGroup accepted a bogus group")
	}
	if !strings.Contains(err.Error(), "session") {
		t.Errorf("error %q does not mention the session group", err)
	}
}

func TestParseWindowSpec_FixedLengths(t *testing.T) {
	now := time.Date(2026, 9, 14, 15, 30, 0, 0, time.Local)
	for _, in := range []string{"10m", "1h", "6h"} {
		t.Run(in, func(t *testing.T) {
			got, err := ParseWindowSpec(in, now)
			if err != nil {
				t.Fatalf("ParseWindowSpec(%q): %v", in, err)
			}
			if got.Symbolic() {
				t.Errorf("%q reported Symbolic; want a fixed length", in)
			}
			if got.Label != in {
				t.Errorf("Label = %q, want %q", got.Label, in)
			}
		})
	}
}

// testZone is a REAL zone, seven hours off UTC on every date the tests below use.
//
// Not time.Local, which is what the first version of these tests used on both sides
// of the assertion. On a host where time.Local IS UTC — the default in most CI
// containers — that made the guard vacuous: an implementation reading
// time.Date(..., time.UTC) passed, because the expectation was computed in the same
// zone as the input. A zone seven hours off UTC means midnight here is 07:00 UTC, so a
// UTC reading lands on the wrong instant on every machine.
//
// AND NOT A time.FixedZone, which is what it was, added under a commit titled "Pin a
// non-UTC zone, so the local-midnight guard actually guards". A fixed offset does stop a
// UTC reading passing — but it has NO TRANSITIONS, so it cannot express a local midnight
// that does not exist, and the guard did not guard: the local-midnight bound shipped in
// this file's subject, and the same expression shipped in the cost ledger, under that very
// commit. America/Los_Angeles is -0700 on 2026-09-14 with a real transition table behind
// it, so it is a drop-in that can fail. The zones whose transitions fall where this one's
// do not are in dst_test.go, which is where the boundary itself is pinned.
func testZone(t *testing.T) *time.Location {
	t.Helper()
	return mustZone(t, "America/Los_Angeles")
}

func TestParseWindowSpec_TodayIsTheLocalDayStartToNow(t *testing.T) {
	// The caller's zone, not UTC. A laptop crossing a timezone must not have its day
	// reset mid-afternoon, and a UTC day would do exactly that.
	zone := testZone(t)
	now := time.Date(2026, 9, 14, 15, 30, 0, 0, zone)
	got, err := ParseWindowSpec("today", now)
	if err != nil {
		t.Fatalf("ParseWindowSpec: %v", err)
	}
	if !got.Symbolic() {
		t.Fatal("today reported a fixed length; want symbolic")
	}
	// Midnight spelled literally, and correct HERE because 2026-09-14 in this zone is an
	// ordinary date whose midnight exists and occurs once. It is written out rather than
	// taken from StartOfLocalDay so the expectation is not the implementation restated. The
	// dates where midnight is the WRONG answer are in dst_test.go.
	wantFrom := time.Date(2026, 9, 14, 0, 0, 0, 0, zone)
	if !got.From.Equal(wantFrom) {
		t.Errorf("From = %v, want the start of the day in the caller's zone %v", got.From, wantFrom)
	}
	// The span is the load-bearing assertion, because it is the one a UTC reading gets
	// wrong: 15:30 minus midnight is 15h30m in the caller's zone and 22h30m if the
	// boundary is taken in UTC.
	if d := got.To.Sub(got.From); d != 15*time.Hour+30*time.Minute {
		t.Errorf("span = %v, want 15h30m; a UTC day boundary would give 22h30m", d)
	}
	if !got.To.Equal(now) {
		t.Errorf("To = %v, want now %v", got.To, now)
	}
	if got.Label != "today" {
		t.Errorf("Label = %q, want \"today\"", got.Label)
	}
}

// Just after midnight in a non-UTC zone is where a UTC boundary is not merely a
// different length but a different DAY: 00:30 at UTC-7 is 07:30 UTC on the same date,
// so a UTC reading reports seven and a half hours of "today" — most of it yesterday
// evening's spend.
func TestParseWindowSpec_TodayJustAfterMidnightInANonUTCZone(t *testing.T) {
	now := time.Date(2026, 9, 14, 0, 30, 0, 0, testZone(t))
	got, err := ParseWindowSpec("today", now)
	if err != nil {
		t.Fatalf("ParseWindowSpec: %v", err)
	}
	if d := got.To.Sub(got.From); d != 30*time.Minute {
		t.Errorf("span = %v, want 30m; a UTC boundary would give 7h30m of someone else's day", d)
	}
}

func TestParseWindowSpec_TodayJustAfterMidnightIsAShortWindow(t *testing.T) {
	// The boundary case: at 00:05, "today" is five minutes, not 24 hours. A
	// fixed-length reading would report yesterday evening's spend as today's.
	//
	// A pinned zone, not time.Local: with time.Local the result depends on the TZ the suite
	// happens to run under, and 00:05 is exactly the wall time that does not exist on some
	// zones' spring-forward day. Pinning makes the assertion mean the same thing on every
	// host. dst_test.go is where the zone is the variable under test.
	now := time.Date(2026, 9, 14, 0, 5, 0, 0, testZone(t))
	got, err := ParseWindowSpec("today", now)
	if err != nil {
		t.Fatalf("ParseWindowSpec: %v", err)
	}
	if d := got.To.Sub(got.From); d != 5*time.Minute {
		t.Errorf("span = %v, want 5m", d)
	}
}

func TestParseWindowSpec_SevenDaysIsRollingNotCalendar(t *testing.T) {
	now := time.Date(2026, 9, 14, 15, 30, 0, 0, testZone(t))
	got, err := ParseWindowSpec("7d", now)
	if err != nil {
		t.Fatalf("ParseWindowSpec: %v", err)
	}
	if d := got.To.Sub(got.From); d != 7*24*time.Hour {
		t.Errorf("span = %v, want exactly 7x24h (rolling, not calendar)", d)
	}
	if !got.Symbolic() {
		t.Error("7d reported a fixed length; the ring cannot serve it, only the ledger can")
	}
	if got.Label != "7d" {
		t.Errorf("Label = %q, want \"7d\"", got.Label)
	}
}

// A ROLLING WEEK TOUCHES EIGHT DATES IN AN ORDINARY WEEK, NOT SEVEN, and the difference is a
// day file the durable ledger has to still hold. The two were confused: the cost ledger's
// retention floor was the literal 7, so retention_days: 7 loaded cleanly and then answered
// window:"7d" over a partial week — a figure nothing downstream could tell from a quiet one.
//
// AT MOST, NOT EXACTLY, which is the assertion this test used to get wrong. Window7dLocalDays
// is a CEILING of nine: eight is what an ordinary week reaches, nine is what a
// spring-forward week reaches, and a retention floor has to cover the worst case rather than
// the common one. Asserting equality here made the constant look like a count and is why it
// sat at 8 with the nine-date week recorded beside it as a known-wrong note.
// TestParseWindowSpec_ASpringForwardWeekReachesTheNinthLocalDate is the other half: it pins
// that the ceiling is REACHED, so this test cannot be satisfied by a bound that is merely
// large.
//
// Every hour of the day is checked, midnight included: the count has to hold at 00:00 —
// where the eighth date contributes a single instant, and is still a file to open — as at
// 15:30, because a floor that holds only for part of the day is not a floor.
//
// The zone is PINNED to a transition-free week on purpose. Under TZ=America/Santiago an
// unpinned "now" could land in the 167-hour week and count nine, which is legal against the
// ceiling but would stop this test measuring the ordinary case it exists for.
func TestParseWindowSpec_SevenDaysTouchesAtMostWindow7dLocalDays(t *testing.T) {
	zone := testZone(t)
	for hour := 0; hour < 24; hour++ {
		now := time.Date(2026, 9, 14, hour, 30, 0, 0, zone)
		if hour == 0 {
			now = time.Date(2026, 9, 14, 0, 0, 0, 0, zone)
		}
		days := localDatesInWindow(t, now)
		if days > Window7dLocalDays {
			t.Errorf("at %v, window=%q spans %d local days, more than Window7dLocalDays = %d — "+
				"the retention floor that agrees with that constant keeps too few day files and "+
				"the window answers over a partial week",
				now.Format("15:04"), Window7d, days, Window7dLocalDays)
		}
		// And an ordinary week must still reach eight, or the rolling-versus-calendar point
		// this test was written for has quietly stopped being true.
		if days != 8 {
			t.Errorf("at %v, a transition-free week spans %d local days, want 8: seven days of "+
				"hours across eight dates is what makes the floor bigger than 7",
				now.Format("15:04"), days)
		}
	}
}

// localDatesInWindow counts the whole local dates a 7d window from now covers, which is the
// walk costledger.Writer.Query makes over day files.
//
// Walked at dayAnchorHour, which is what costledger.dayOf does. It is NOT a walk over local
// midnights: that expression counts a date twice or skips one in a zone whose transition is
// at 00:00, and a midnight walk in the helper that measures how many day files a window needs
// was a third copy of that defect, wrong about the very thing being counted.
func localDatesInWindow(t *testing.T, now time.Time) int {
	t.Helper()
	spec, err := ParseWindowSpec(Window7d, now)
	if err != nil {
		t.Fatalf("ParseWindowSpec(%q) at %v: %v", Window7d, now, err)
	}
	days := 0
	day := time.Date(spec.From.Year(), spec.From.Month(), spec.From.Day(), dayAnchorHour, 0, 0, 0, spec.From.Location())
	last := time.Date(spec.To.Year(), spec.To.Month(), spec.To.Day(), dayAnchorHour, 0, 0, 0, spec.To.Location())
	for ; !day.After(last); day = day.AddDate(0, 0, 1) {
		days++
	}
	return days
}

func TestParseWindowSpec_RejectsUnknownWithoutEchoingInput(t *testing.T) {
	const attack = "<script>alert(1)</script>"
	_, err := ParseWindowSpec(attack, time.Now())
	if err == nil {
		t.Fatal("accepted an unknown window")
	}
	if strings.Contains(err.Error(), "script") {
		t.Errorf("error echoes caller input: %q", err.Error())
	}
}

func TestParseWindow_StillRejectsSymbolicWindows(t *testing.T) {
	// A duration caller cannot express "today". Refusing beats silently
	// substituting a length, which would report a number for a span nobody asked
	// for.
	for _, in := range []string{"today", "7d"} {
		if _, err := ParseWindow(in); err == nil {
			t.Errorf("ParseWindow(%q) succeeded; want an error", in)
		}
	}
}
