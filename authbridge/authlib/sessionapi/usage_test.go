package sessionapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	// EMBEDDED TZDATA, so mustZone can treat a load failure as a test failure rather than a
	// skip. A scratch CI container may carry no zone database, and a skip there would report
	// success for the one assertion in this file that needs a zone whose offset CHANGES.
	_ "time/tzdata"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/costledger"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// fetchUsage GETs the endpoint and returns status plus raw body.
func fetchUsage(t *testing.T, base, query string) (int, string) {
	t.Helper()
	resp, err := http.Get(base + "/v1/usage" + query)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(body)
}

// Without WithUsage the endpoint must 404, not serve an empty snapshot: zeroed
// buckets would render as a flat chart implying idle traffic, hiding the fact
// that aggregation is not running at all.
func TestHandleUsage_NoAggregator404s(t *testing.T) {
	ts, _ := newTestServer(t)
	status, body := fetchUsage(t, ts.URL, "")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
	if !strings.Contains(body, "not enabled") {
		t.Errorf("body should say why: %q", body)
	}
}

func TestHandleUsage_Defaults(t *testing.T) {
	agg := usage.New()
	ts, _ := newTestServer(t, WithUsage(agg))

	status, body := fetchUsage(t, ts.URL, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var snap usage.Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(snap.Buckets) != 10 {
		t.Errorf("default window should be 10 buckets, got %d", len(snap.Buckets))
	}
	if snap.BucketSeconds != 60 {
		t.Errorf("bucketSeconds = %d, want 60", snap.BucketSeconds)
	}
	if snap.Priced {
		t.Error("priced = true when no request carried a cost event")
	}
}

// Query parameters must actually reach Snapshot — a handler that parsed them and
// then ignored them would pass every validation test while serving the default
// view.
func TestHandleUsage_ParamsReachSnapshot(t *testing.T) {
	agg := usage.New()
	ts, store := newTestServer(t, WithUsage(agg))
	store.AddRecorder(agg)

	store.Append("alice", pipeline.SessionEvent{
		At:         time.Now(),
		Direction:  pipeline.Outbound,
		Phase:      pipeline.SessionResponse,
		StatusCode: 200,
		Duration:   time.Second,
		Inference:  &pipeline.InferenceExtension{Model: "claude-sonnet-5", TotalTokens: 300},
	})

	// resolution: 1h at 5m must fold to 12 buckets, not 60.
	status, body := fetchUsage(t, ts.URL, "?window=1h&resolution=5m")
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var snap usage.Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(snap.Buckets) != 12 || snap.BucketSeconds != 300 {
		t.Errorf("1h@5m gave %d buckets at %ds, want 12 at 300s",
			len(snap.Buckets), snap.BucketSeconds)
	}

	// group: the series must be keyed by the requested dimension.
	_, body = fetchUsage(t, ts.URL, "?group=method")
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Group != usage.GroupMethod {
		t.Errorf("group = %q, want method", snap.Group)
	}
	found := false
	for _, b := range snap.Buckets {
		if _, ok := b.Series["claude-sonnet-5"]; ok {
			found = true
		}
	}
	if !found {
		t.Error("group=method did not key the series by model")
	}

	// session: scoping must filter, and echo back which session it covers.
	_, body = fetchUsage(t, ts.URL, "?session=alice")
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Session != "alice" {
		t.Errorf("session = %q, want alice", snap.Session)
	}
	if snap.Totals.Tokens != 300 {
		t.Errorf("alice tokens = %d, want 300", snap.Totals.Tokens)
	}

	// Fresh value: Counts fields are omitempty, so an all-zero Totals is absent
	// from the JSON entirely. Decoding into the struct reused above would leave
	// alice's 300 in place and the assertion would pass for the wrong reason.
	var bobSnap usage.Snapshot
	_, body = fetchUsage(t, ts.URL, "?session=bob")
	if err := json.Unmarshal([]byte(body), &bobSnap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if bobSnap.Totals.Tokens != 0 {
		t.Errorf("bob should have no traffic, got %d tokens", bobSnap.Totals.Tokens)
	}
}

func TestHandleUsage_BadParams400(t *testing.T) {
	ts, _ := newTestServer(t, WithUsage(usage.New()))

	for _, q := range []string{
		"?window=30s",               // finer than a bucket
		"?window=7h",                // beyond retention
		"?window=90s",               // not a multiple
		"?window=nonsense",          // unparseable
		"?group=bogus",              // unknown grouping
		"?window=1h&resolution=90s", // resolution not a multiple
		"?window=1h&resolution=2h",  // resolution wider than the window
		"?resolution=30s",           // finer than storage
	} {
		status, body := fetchUsage(t, ts.URL, q)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body %q)", q, status, body)
		}
		if !strings.Contains(body, `"error"`) {
			t.Errorf("%s: body is not a JSON error: %q", q, body)
		}
	}
}

// The endpoint is unauthenticated, so an error body must never reflect
// caller-supplied bytes back — that is a reflection primitive.
func TestHandleUsage_ErrorsDoNotEchoInput(t *testing.T) {
	ts, _ := newTestServer(t, WithUsage(usage.New()))
	const probe = "<script>alert(1)</script>"

	for _, q := range []string{"?group=" + probe, "?window=" + probe, "?resolution=" + probe} {
		status, body := fetchUsage(t, ts.URL, q)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, status)
		}
		if strings.Contains(body, "script") {
			t.Errorf("error body echoes caller input: %q", body)
		}
	}
}

// An over-long session id is rejected rather than truncated or echoed. Bounded
// at the store's own cap so the two cannot drift.
func TestHandleUsage_SessionIDLengthCap(t *testing.T) {
	ts, _ := newTestServer(t, WithUsage(usage.New()))

	ok := strings.Repeat("a", session.MaxSessionIDLen)
	if status, body := fetchUsage(t, ts.URL, "?session="+ok); status != http.StatusOK {
		t.Errorf("id at exactly the cap should be accepted, got %d: %s", status, body)
	}

	tooLong := strings.Repeat("a", session.MaxSessionIDLen+1)
	status, body := fetchUsage(t, ts.URL, "?session="+tooLong)
	if status != http.StatusBadRequest {
		t.Errorf("over-long id: status = %d, want 400", status)
	}
	if strings.Contains(body, tooLong) {
		t.Error("error body echoes the over-long id back")
	}
}

// inferenceEventForAPI builds a response event carrying an explicit token split.
//
// A local copy of authlib/usage's own inferenceEvent: that one is unexported and
// this is a different package, so there is nothing to import.
func inferenceEventForAPI(model string, in, cacheRead, cacheWrite, out int) pipeline.SessionEvent {
	return pipeline.SessionEvent{
		At:         time.Now(),
		Direction:  pipeline.Outbound,
		Phase:      pipeline.SessionResponse,
		StatusCode: 200,
		Host:       "gw.example.com",
		Inference: &pipeline.InferenceExtension{
			Model:            model,
			InputTokens:      in,
			CacheReadTokens:  cacheRead,
			CacheWriteTokens: cacheWrite,
			OutputTokens:     out,
			TotalTokens:      in + cacheRead + cacheWrite + out,
		},
	}
}

// The handler delegates group parsing to usage.ParseGroup, so the axes a cost
// table needs are served with no code change here. This pins that: a future
// refactor that reintroduced a local allow-list would silently drop the new
// groupings while every other test in this file kept passing.
func TestHandleUsage_AcceptsModelAndEndpointGroups(t *testing.T) {
	for _, group := range []string{"model", "endpoint", "method", "status", "plugin", "none", ""} {
		t.Run("group="+group, func(t *testing.T) {
			ts, _ := newTestServer(t, WithUsage(usage.New()))

			status, body := fetchUsage(t, ts.URL, "?group="+group)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", status, body)
			}
			var snap usage.Snapshot
			if err := json.Unmarshal([]byte(body), &snap); err != nil {
				t.Fatalf("decode: %v", err)
			}
		})
	}
}

// A deliberate DUPLICATE of TestHandleUsage_ErrorsDoNotEchoInput's group case, kept
// only so the group parameter's no-reflection guarantee is asserted under a name
// that says so — ParseGroup's error string is the one most likely to be reworded as
// groupings are added, and this is what a reworder will grep for.
//
// It adds no coverage: percent-encoding the probe changes nothing, because both
// tests read through r.URL.Query().Get, which decodes. Delete this rather than the
// broader test if one has to go.
func TestHandleUsage_RejectsUnknownGroupWithoutReflectingIt(t *testing.T) {
	ts, _ := newTestServer(t, WithUsage(usage.New()))

	status, body := fetchUsage(t, ts.URL, "?group=%3Cscript%3E")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if strings.Contains(body, "script") {
		t.Errorf("response reflects caller input: %s", body)
	}
}

// The whole point of the schema rule is that a consumer reads the same field
// names the parser published. Assert on the JSON, not the struct.
//
// Presence is a real assertion rather than a trivial one because every split
// field is omitempty: a name only reaches the wire if the aggregate actually
// carried a non-zero value for it. So this fails both if a field is renamed and
// if foldInto stops populating it from the event.
func TestHandleUsage_SplitFieldsAppearOnTheWire(t *testing.T) {
	agg := usage.New()
	ts, store := newTestServer(t, WithUsage(agg))
	store.AddRecorder(agg)

	store.Append("s1", inferenceEventForAPI("claude-opus-5", 10, 2000, 50, 30))

	status, body := fetchUsage(t, ts.URL, "?session=s1")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", status, body)
	}
	for _, field := range []string{"inputTokens", "cacheReadTokens", "cacheWriteTokens", "outputTokens"} {
		if !strings.Contains(body, field) {
			t.Errorf("response body has no %q field: %s", field, body)
		}
	}
}

// ledgerWithOneCostedMinute builds a cost ledger holding exactly one CLOSED minute
// carrying a settled cost, at the instant `at`.
//
// Closed, not open: the writer keeps the open minute in memory on purpose, so a row
// only reaches disk once the minute rolls or Flush is called. A test that skipped
// the flush would assert against an empty ledger and read as a routing bug.
func ledgerWithOneCostedMinute(t *testing.T, at time.Time, host, model string, costUSD float64) *costledger.Writer {
	t.Helper()
	led := newTestLedger(t, at)
	recordCostedMinute(t, led, at, host, model, costUSD)
	if err := led.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	return led
}

// startOfToday is the LOCAL start of today, which is the boundary window=today uses.
//
// Fixtures for a symbolic window have to be anchored here rather than offset from
// time.Now(): backing off the current clock crosses the day boundary during the first
// minutes of each day, filing the row under yesterday while the request asks about today.
// Local, not UTC, because that is what usage.ParseWindowSpec means by "today" — see its doc
// for why a laptop crossing a timezone must not have its day reset mid-afternoon.
//
// TAKEN FROM THE WINDOW ITSELF, not computed a second way, and it used to be computed. This
// was time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, n.Location()) — a copy of the
// expression ParseWindowSpec carried, with the identical flaw: in a zone whose DST
// transition falls at 00:00 that instant does not exist on the spring-forward day, and
// time.Date resolves it onto the neighbouring one. A fixture whose whole job is to sit
// INSIDE the window under test then sat outside it, and the assertion it fed measured an
// empty ledger while reading as a routing failure.
//
// Asking ParseWindowSpec is stronger than sharing usage.StartOfLocalDay with it would be:
// there is no second derivation left to drift, in any zone, on any date — the anchor IS the
// bound the request will be served with. Taking t is the price, and it buys a Fatalf on the
// parse rather than a zero time silently becoming the anchor. The dependency runs sessionapi
// to usage, which is the direction this package's imports already go; usage must never
// import back.
func startOfToday(t *testing.T) time.Time {
	t.Helper()
	spec, err := usage.ParseWindowSpec(usage.WindowToday, time.Now())
	if err != nil {
		t.Fatalf("ParseWindowSpec(%q): %v", usage.WindowToday, err)
	}
	return spec.From
}

// insideToday is an instant `into` after today began, CLAMPED TO NOW so it is always inside
// the window a window=today request will actually serve.
//
// The clamp is the whole helper. "today" is a boundary, not a length, so during the first
// `into` of any local day the offset lands in the FUTURE: the row is outside [From, now], the
// total comes back zero, and the test fails as though routing or grouping were broken. Two
// tests each carried their own version of this guard and a third carried none. MEASURED with
// the third: this suite under TZ=Asia/Beirut at 00:20 local, where a three-hour offset put
// the fixture at 03:00 while window=today ended at 00:20, and
// TestHandleUsage_ALedgerWindowSaysWhichGroupingItCouldApply reported
// "totals.costMicros = 0, want 1000000" — an assertion about grouping, failing for a reason
// that has nothing to do with grouping, on two mornings' worth of clock a day.
//
// Clamping to now rather than skipping: the row still lands in today's day file at an instant
// the request covers, so the test measures what it is for. Only the OFFSET is approximate,
// and no assertion depends on it.
func insideToday(t *testing.T, into time.Duration) time.Time {
	t.Helper()
	at := startOfToday(t).Add(into)
	if now := time.Now(); at.After(now) {
		return now
	}
	return at
}

// mustZone loads a real zone, FAILING rather than skipping when it cannot.
//
// t.Fatalf, deliberately: a skip would report success for a test that never ran the code it
// exists to cover, which is how the local-midnight bound survived a suite whose only
// non-UTC zone was a transition-free time.FixedZone.
func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v — this file embeds time/tzdata precisely so this "+
			"cannot happen; a failure here means the import was dropped, not that the host "+
			"lacks a zone database", name, err)
	}
	return loc
}

// TestStartOfToday_TheFixtureAnchorLandsInsideTheWindowItIsQueriedWith is the assertion
// every window=today test in this file silently depends on.
//
// Each of them writes a ledger row a couple of minutes after the anchor and then asks the
// endpoint for window=today. If the anchor is not inside [From, now] the row is in a
// different day file from the one the request reads, the response is empty, and the test
// fails as though routing or grouping were broken. So the anchor is checked against the
// window directly, once, here.
//
// AND AGAINST THE EXPRESSION IT REPLACED, in a real zone, because that is where the
// property has teeth. time.Local cannot be changed inside a running process, so the live
// clock only exercises this on a host whose zone shifts at 00:00, on one of the two dates a
// year that it does. Pinning America/Havana on 2026-03-08 makes the damage deterministic:
// the old anchor lands at 23:02 on 2026-03-07 while the window it is meant to sit inside
// begins at 01:00 on 2026-03-08 — an hour and fifty-eight minutes outside it, on the wrong
// date, in the wrong day file.
func TestStartOfToday_TheFixtureAnchorLandsInsideTheWindowItIsQueriedWith(t *testing.T) {
	// The live clock, which is what the fixtures actually use, and BOTH ends of the window.
	// The lower bound is the boundary defect; the upper bound is the clamp, which matters for
	// the first hours of every local day and is what TZ=Asia/Beirut at 00:20 found.
	spec, err := usage.ParseWindowSpec(usage.WindowToday, time.Now())
	if err != nil {
		t.Fatalf("ParseWindowSpec: %v", err)
	}
	for _, into := range []time.Duration{2 * time.Minute, 3 * time.Hour} {
		at := insideToday(t, into)
		if at.Before(spec.From) {
			t.Errorf("the anchor %v after the day began (%v) is before window=today begins (%v), "+
				"so a row written there is filed under a day the request never reads",
				into, at, spec.From)
		}
		// Re-read rather than reusing spec.To: it can only have moved later, so this cannot
		// fail for having taken time.
		if now := time.Now(); at.After(now) {
			t.Errorf("the anchor %v after the day began (%v) is in the future (now %v), so a row "+
				"written there falls outside the window and the total comes back zero",
				into, at, now)
		}
	}

	// America/Havana shifts AT 00:00, so 2026-03-08 has no midnight at all.
	loc := mustZone(t, "America/Havana")
	const date = "2026-03-08"
	hav, err := usage.ParseWindowSpec(usage.WindowToday, time.Date(2026, 3, 8, 15, 0, 0, 0, loc))
	if err != nil {
		t.Fatalf("ParseWindowSpec: %v", err)
	}
	naive := time.Date(2026, 3, 8, 0, 0, 0, 0, loc)
	if naive.Format("2006-01-02") == date {
		t.Fatalf("local midnight of %s in America/Havana resolved onto its own date (%v), so "+
			"this test no longer exercises the defect and is now vacuous", date, naive)
	}
	if !naive.Add(2 * time.Minute).Before(hav.From) {
		t.Errorf("the old anchor plus two minutes (%v) is not outside window=today (from %v); "+
			"the whole reason the anchor is taken from the window is that computing it a "+
			"second way put the fixture on another date", naive.Add(2*time.Minute), hav.From)
	}
}

// newTestLedger opens an empty ledger with its clock pinned to at.
func newTestLedger(t *testing.T, at time.Time) *costledger.Writer {
	t.Helper()
	led, err := costledger.New(t.TempDir(), costledger.WithClock(func() time.Time { return at }))
	if err != nil {
		t.Fatalf("costledger.New: %v", err)
	}
	t.Cleanup(func() { _ = led.Close() })
	return led
}

// recordCostedMinute feeds one settled-cost inference response to a ledger, without
// flushing — so the caller chooses whether the minute is open or closed.
func recordCostedMinute(t *testing.T, led *costledger.Writer, at time.Time, host, model string, costUSD float64) {
	t.Helper()
	rec, err := json.Marshal(costevent.Event{
		CostUSD: costUSD, Settled: true,
		Source: costevent.SourceUsageFallback, Provenance: "bundled",
	})
	if err != nil {
		t.Fatalf("marshal cost record: %v", err)
	}
	led.Record("s1", &pipeline.SessionEvent{
		At: at, Phase: pipeline.SessionResponse, StatusCode: 200, Host: host,
		Inference: &pipeline.InferenceExtension{
			Model: model, InputTokens: 100, OutputTokens: 50, TotalTokens: 150, PresentKinds: 0b1001,
		},
		Plugins: map[string]json.RawMessage{costevent.Key: rec},
	})
}

func TestHandleUsage_TodayIsServedFromTheLedger(t *testing.T) {
	// A closed minute on disk, plus an empty ring, so a non-zero total can only
	// have come from the ledger.
	led := ledgerWithOneCostedMinute(t, insideToday(t, 2*time.Minute), "gw", "opus", 0.25)
	ts, _ := newTestServer(t, WithUsage(usage.New()), WithCostLedger(led))

	status, body := fetchUsage(t, ts.URL, "?window=today")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var snap usage.Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if snap.Window != "today" {
		t.Errorf("window = %q, want \"today\" echoed back", snap.Window)
	}
	if snap.Totals.CostMicros == 0 {
		t.Error("CostMicros = 0; the ledger row did not reach the response")
	}
	if !snap.Priced {
		t.Error("priced = false for a ledger-sourced total")
	}
	// One bucket spanning the window, and BucketSeconds says so, so a client cannot
	// mistake a whole-window total for a fine series.
	if len(snap.Buckets) != 1 {
		t.Errorf("got %d buckets, want 1 spanning the window", len(snap.Buckets))
	}
	if snap.BucketSeconds <= int(usage.BucketWidth.Seconds()) {
		t.Errorf("bucketSeconds = %d, want the whole window's span", snap.BucketSeconds)
	}
}

// The open minute must reach the response. Nothing else supplies it: the day files
// hold closed minutes only, so a session whose whole conversation fit inside one
// minute has zero rows on disk — and "today" would have reported priced:false over
// real spend, which the CLI renders as "cost unavailable".
func TestHandleUsage_TodayIncludesTheStillOpenMinute(t *testing.T) {
	now := time.Now()
	led := newTestLedger(t, now)
	recordCostedMinute(t, led, now, "gw", "opus", 0.25) // no Flush: the minute is open
	// An empty ring, so a non-zero total can only have come from the ledger's own
	// in-memory half rather than from the aggregator.
	ts, _ := newTestServer(t, WithUsage(usage.New()), WithCostLedger(led))

	status, body := fetchUsage(t, ts.URL, "?window=today")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var snap usage.Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if snap.Window != "today" {
		t.Errorf("window = %q, want \"today\"", snap.Window)
	}
	if snap.Totals.CostMicros != 250_000 {
		t.Errorf("CostMicros = %d, want 250000 from the open minute", snap.Totals.CostMicros)
	}
	if !snap.Priced {
		t.Error("priced = false while the ledger holds a priced minute in memory")
	}
}

// And it must be counted ONCE. Same spend, asked for either side of the flush that
// moves it from memory to disk: a total that doubled would mean both halves claimed
// the minute.
func TestHandleUsage_TodayCountsTheOpenMinuteOnceAcrossAFlush(t *testing.T) {
	now := time.Now()
	led := newTestLedger(t, now)
	recordCostedMinute(t, led, now, "gw", "opus", 0.25)
	ts, _ := newTestServer(t, WithUsage(usage.New()), WithCostLedger(led))

	readTotal := func(when string) int64 {
		t.Helper()
		status, body := fetchUsage(t, ts.URL, "?window=today")
		if status != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", when, status, body)
		}
		var snap usage.Snapshot
		if err := json.Unmarshal([]byte(body), &snap); err != nil {
			t.Fatalf("%s: decode: %v", when, err)
		}
		return snap.Totals.CostMicros
	}

	open := readTotal("while open")
	if err := led.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	closed := readTotal("after the flush")

	if open != 250_000 || closed != 250_000 {
		t.Errorf("total was %d while open and %d once written; want 250000 both times "+
			"(500000 after the flush would mean the minute was counted twice)", open, closed)
	}
}

func TestHandleUsage_TodayWithoutALedgerDegradesAndSaysSo(t *testing.T) {
	// Kubernetes has no ledger by design. A 400 would make the abctl cost view
	// fail there rather than showing what IS available, so the handler serves the
	// ring's maximum window and reports the window it actually served.
	ts, _ := newTestServer(t, WithUsage(usage.New())) // no ledger

	status, body := fetchUsage(t, ts.URL, "?window=today")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var snap usage.Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if snap.Window == "today" {
		t.Error("window = \"today\" but no ledger exists; the response must name the window actually served")
	}
	if snap.Window != usage.MaxWindow.String() {
		t.Errorf("window = %q, want the ring maximum %q", snap.Window, usage.MaxWindow)
	}
}

func TestHandleUsage_SevenDaysIsServedFromTheLedger(t *testing.T) {
	// Same shape as today, over a span the ring cannot cover at all: six hours of
	// buckets can never answer for a row written two days ago.
	led := ledgerWithOneCostedMinute(t, time.Now().Add(-48*time.Hour), "gw", "opus", 1.50)
	ts, _ := newTestServer(t, WithUsage(usage.New()), WithCostLedger(led))

	status, body := fetchUsage(t, ts.URL, "?window=7d")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var snap usage.Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if snap.Window != "7d" {
		t.Errorf("window = %q, want \"7d\"", snap.Window)
	}
	if snap.Totals.CostMicros != 1_500_000 {
		t.Errorf("CostMicros = %d, want 1500000 from a two-day-old row", snap.Totals.CostMicros)
	}
}

func TestHandleUsage_UnknownWindowStillDoesNotEchoInput(t *testing.T) {
	ts, _ := newTestServer(t, WithUsage(usage.New()))
	status, body := fetchUsage(t, ts.URL, "?window=%3Cscript%3E")

	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if strings.Contains(body, "script") {
		t.Errorf("response reflects caller input: %s", body)
	}
}

func TestHandleUsage_LedgerBackedWindowGroupsByModel(t *testing.T) {
	// The Cost pane's by-model table must work over "today", not only over the
	// ring's windows — otherwise the breakdown silently covers a different span
	// from the total above it.
	led := ledgerWithOneCostedMinute(t, insideToday(t, 2*time.Minute), "gw", "opus", 0.25)
	ts, _ := newTestServer(t, WithUsage(usage.New()), WithCostLedger(led))

	status, body := fetchUsage(t, ts.URL, "?window=today&group=model")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var snap usage.Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(snap.Buckets) != 1 {
		t.Fatalf("got %d buckets, want 1", len(snap.Buckets))
	}
	got := snap.Buckets[0].Series["opus"]
	if got.CostMicros != 250_000 {
		t.Errorf("series[opus].CostMicros = %d, want 250000", got.CostMicros)
	}
	// The breakdown accounts for the total it sits under.
	if got.CostMicros != snap.Totals.CostMicros {
		t.Errorf("series sums to %d but totals is %d", got.CostMicros, snap.Totals.CostMicros)
	}
}

// THE SHORTFALL A group=model BREAKDOWN LEAVES HAS TO REACH THE WIRE, or a client
// summing it cannot tell an incomplete series from a complete one.
//
// The ledger keeps a gateway-priced response the inference parser could not read
// (/v1/embeddings, /v1/rerank) as a row with no model, so it counts toward Totals and
// cannot be a group=model key. costledger.Fold computes that residual; this pins that
// ledgerSnapshot carries it out to the JSON.
func TestHandleUsage_LedgerBackedModelSeriesDisclosesWhatItLeavesOut(t *testing.T) {
	at := insideToday(t, 2*time.Minute)
	led := ledgerWithOneCostedMinute(t, at, "gw", "opus", 0.10)
	// A second row in the same minute: priced by the gateway, with no Inference extension
	// at all. A different composite key, so it is its own row.
	rec, err := json.Marshal(costevent.Event{
		CostUSD: 0.25, Settled: true,
		Source: costevent.SourceGatewayHeader, Provenance: "authoritative",
	})
	if err != nil {
		t.Fatalf("marshal cost record: %v", err)
	}
	led.Record("s1", &pipeline.SessionEvent{
		At: at, Phase: pipeline.SessionResponse, StatusCode: 200, Host: "gw",
		Plugins: map[string]json.RawMessage{costevent.Key: rec},
	})
	if ferr := led.Flush(); ferr != nil {
		t.Fatalf("Flush: %v", ferr)
	}
	ts, _ := newTestServer(t, WithUsage(usage.New()), WithCostLedger(led))

	status, body := fetchUsage(t, ts.URL, "?window=today&group=model")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var snap usage.Snapshot
	if uerr := json.Unmarshal([]byte(body), &snap); uerr != nil {
		t.Fatalf("decode: %v (%s)", uerr, body)
	}
	if snap.Totals.CostMicros != 350_000 {
		t.Fatalf("Totals.CostMicros = %d, want 350000 — both rows are real spend: %s",
			snap.Totals.CostMicros, body)
	}
	var sum int64
	for _, b := range snap.Buckets {
		for _, c := range b.Series {
			sum += c.CostMicros
		}
	}
	if snap.UngroupedCostMicros == nil {
		t.Fatalf("no ungroupedCostMicros while the series accounts for %d of %d micros: a client "+
			"summing the breakdown is short and nothing in the response explains it: %s",
			sum, snap.Totals.CostMicros, body)
	}
	if sum+*snap.UngroupedCostMicros != snap.Totals.CostMicros {
		t.Errorf("series (%d) + ungrouped (%d) != totals (%d): %s",
			sum, *snap.UngroupedCostMicros, snap.Totals.CostMicros, body)
	}
}

// The other half of the convention: absent, not zero, when the breakdown accounts for
// every dollar. Same reasoning as TestHandleUsage_ACleanLedgerReadCarriesNoDegradedBlock.
func TestHandleUsage_ALedgerWindowWithNothingUngroupedOmitsTheField(t *testing.T) {
	led := ledgerWithOneCostedMinute(t, insideToday(t, 2*time.Minute), "gw", "opus", 0.25)
	ts, _ := newTestServer(t, WithUsage(usage.New()), WithCostLedger(led))

	status, body := fetchUsage(t, ts.URL, "?window=today&group=model")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if strings.Contains(body, "ungroupedCostMicros") {
		t.Errorf("a window whose series carries every dollar serialised the residual: %s", body)
	}
}

// A symbolic window with session= is refused, not quietly answered with
// all-sessions data under a session label. See the guard in handleUsage for why the
// refusal is unconditional.
func TestHandleUsage_SessionWithSymbolicWindowIsRejected(t *testing.T) {
	led := ledgerWithOneCostedMinute(t, insideToday(t, 2*time.Minute), "gw", "opus", 0.25)
	for _, srvName := range []string{"with ledger", "without ledger"} {
		opts := []Option{WithUsage(usage.New())}
		if srvName == "with ledger" {
			opts = append(opts, WithCostLedger(led))
		}
		ts, _ := newTestServer(t, opts...)
		for _, window := range []string{"today", "7d"} {
			status, body := fetchUsage(t, ts.URL, "?window="+window+"&session=s1")
			if status != http.StatusBadRequest {
				t.Errorf("%s, window=%s: status = %d, want 400: %s", srvName, window, status, body)
			}
			if !strings.Contains(body, "session") {
				t.Errorf("%s, window=%s: error does not name the problem: %s", srvName, window, body)
			}
		}
	}
}

// A duration window with session= keeps working. The rejection above must not have
// widened into "session is unsupported".
func TestHandleUsage_SessionWithDurationWindowStillWorks(t *testing.T) {
	ts, _ := newTestServer(t, WithUsage(usage.New()))
	status, body := fetchUsage(t, ts.URL, "?window=10m&session=s1")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
}

// TestHandleUsage_ACorruptLedgerLineIsDisclosedInTheResponse closes the last step of the
// skip-don't-discard change.
//
// The ledger skipping a corrupt line rather than throwing away the rest of the day is
// strictly better than what it replaced — but until the count reached the wire, the
// response was byte-identical to a clean one: a short dollar total under priced:true,
// with nothing anywhere in it saying rows were missing. A client cannot caveat what it
// cannot see, so the improvement was invisible to every consumer.
func TestHandleUsage_ACorruptLedgerLineIsDisclosedInTheResponse(t *testing.T) {
	// Two minutes into TODAY, not two minutes before NOW. Backing off the current clock
	// crosses the day boundary for the first two minutes of every day: the row lands in
	// yesterday's day file while window=today asks about this one, and the test fails for
	// a reason that has nothing to do with what it is checking. `today` is a boundary, not a
	// length, so a fixture near it has to be anchored to the boundary rather than to now, and
	// to the SAME boundary — see insideToday, which also handles the other end.
	when := insideToday(t, 2*time.Minute)
	// An explicit dir rather than newTestLedger's hidden t.TempDir(), because this test
	// has to reach the day file the writer produced.
	dir := t.TempDir()
	led, lerr := costledger.New(dir, costledger.WithClock(func() time.Time { return when }))
	if lerr != nil {
		t.Fatalf("costledger.New: %v", lerr)
	}
	t.Cleanup(func() { _ = led.Close() })

	recordCostedMinute(t, led, when, "gw", "opus", 0.25)
	if err := led.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	// A line the decoder cannot read, appended to the day file the ledger just wrote.
	// Deliberately a MID-file corruption with a good row BEFORE it, so the response has a
	// real total to be short of rather than nothing at all.
	entries, derr := os.ReadDir(dir)
	if derr != nil || len(entries) == 0 {
		t.Fatalf("no day file to corrupt (err=%v, entries=%d)", derr, len(entries))
	}
	f, oerr := os.OpenFile(filepath.Join(dir, entries[0].Name()), os.O_APPEND|os.O_WRONLY, 0o600)
	if oerr != nil {
		t.Fatalf("open day file: %v", oerr)
	}
	if _, werr := f.WriteString("{this is not a row\n"); werr != nil {
		t.Fatalf("append garbage: %v", werr)
	}
	if cerr := f.Close(); cerr != nil {
		t.Fatalf("close: %v", cerr)
	}

	ts, _ := newTestServer(t, WithUsage(usage.New()), WithCostLedger(led))
	status, body := fetchUsage(t, ts.URL, "?window=today")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var snap usage.Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Degraded == nil {
		t.Fatalf("a day file with an undecodable line answered with no degraded block; "+
			"the total is short and the response does not say so: %s", body)
	}
	if snap.Degraded.SkippedLines < 1 {
		t.Errorf("degraded.skippedLines = %d, want at least 1", snap.Degraded.SkippedLines)
	}
}

// The request's context reaches the ledger, so a client that hangs up stops the read.
//
// The ledger walk is the only unbounded IO this endpoint does: one day file per day in
// the window, up to eight for window=7d, against an operator-configured path. Handed
// context.Background() instead, every abandoned request read every one of them to the end
// for a client that had gone — and a chart that re-requests on each keystroke cancels its
// own reads constantly.
//
// The handler is called directly rather than through httptest, because the assertion is
// about which context reaches costledger.Window and a real client disconnect cannot be
// timed against the read.
func TestHandleUsage_TheRequestContextReachesTheLedgerRead(t *testing.T) {
	led := ledgerWithOneCostedMinute(t, insideToday(t, 2*time.Minute), "gw", "opus", 0.25)
	srv := New(":0", session.New(5*time.Minute, 100, 0), WithUsage(usage.New()), WithCostLedger(led))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/usage?window=today", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.handleUsage(rec, req)

	// 503, not 200: the read was abandoned, so there is no total to serve. A 200 here
	// means the handler passed a context of its own and read the day files anyway.
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 for a cancelled request; 200 means the request's "+
			"context never reached the ledger: %s", rec.Code, rec.Body.String())
	}
	// Still a JSON body rather than an empty one — net/http would otherwise send an empty
	// 200 for the rare cancellation whose client is still listening.
	if !strings.Contains(rec.Body.String(), "cost history unavailable") {
		t.Errorf("body = %q, want the fixed unavailable message", rec.Body.String())
	}
}

// TestHandleUsage_ACleanLedgerReadCarriesNoDegradedBlock is the other half, and it is
// the half that makes the field worth having: zeros on every clean read would train a
// reader to ignore it.
func TestHandleUsage_ACleanLedgerReadCarriesNoDegradedBlock(t *testing.T) {
	led := ledgerWithOneCostedMinute(t, insideToday(t, 2*time.Minute), "gw", "opus", 0.25)
	ts, _ := newTestServer(t, WithUsage(usage.New()), WithCostLedger(led))

	status, body := fetchUsage(t, ts.URL, "?window=today")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if strings.Contains(body, "degraded") {
		t.Errorf("a clean read serialised a degraded block; absence is how a client tells "+
			"clean from damaged: %s", body)
	}
}

// TestInsideToday_NeverLeavesTheWindowItIsAnchoredTo pins the contract that made this helper
// necessary, and it holds at every time of day rather than only at the convenient ones.
//
// The fixtures here used `time.Now().Add(-2 * time.Minute)`, which lands YESTERDAY during the
// first two minutes after local midnight — so the row went into yesterday's day file while
// `window=today` asked about this one, and seven tests reported `CostMicros = 0` for a reason
// unrelated to what any of them was checking. CI caught it at 00:00:30 UTC, roughly 30 seconds
// into a day, where the window is 30 seconds wide.
//
// Worth recording how it survived: the same defect was found and fixed in ONE test in this file
// earlier, and the six siblings using the identical expression were left. Fixing the instance
// rather than the pattern is what put it in CI.
//
// The assertion is deliberately a property, not a value: an anchor must be at or after the
// start of today and at or before now, whatever the clock says when the suite runs. At 00:00:30
// that forces the clamp; at 15:00 it does not, and a test that only ever runs at 15:00 proves
// nothing about the case that broke.
func TestInsideToday_NeverLeavesTheWindowItIsAnchoredTo(t *testing.T) {
	start := startOfToday(t)
	for _, into := range []time.Duration{
		0, time.Second, 2 * time.Minute, time.Hour, 12 * time.Hour, 23 * time.Hour, 48 * time.Hour,
	} {
		at := insideToday(t, into)
		now := time.Now()
		if at.Before(start) {
			t.Errorf("insideToday(%s) = %s, before the start of today (%s): a row anchored there "+
				"lands in yesterday's day file and window=today cannot see it", into, at, start)
		}
		if at.After(now) {
			t.Errorf("insideToday(%s) = %s, after now (%s): a row anchored in the future is "+
				"outside [From, now] and window=today cannot see it either", into, at, now)
		}
	}
}
