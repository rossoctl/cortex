package sessionapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	// EMBEDDED TZDATA, so mustZone can treat a load failure as a test failure rather than a
	// skip. A scratch CI container may carry no zone database, and a skip there would report
	// success for the one assertion in this file that needs a zone whose offset CHANGES.
	_ "time/tzdata"

	"github.com/rossoctl/cortex/core/costevent"
	"github.com/rossoctl/cortex/core/costledger"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/usage"
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
		Host:       "api.anthropic.com",
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

	// group=host: the same request, keyed by the upstream it went to. Asserted
	// through the real handler because the bucket's series lookup returns nil for a
	// group it does not know, and abctl falls back to ungrouped bars on an empty
	// series set — so a missing arm looks like "this host had no traffic" rather
	// than like an error.
	_, body = fetchUsage(t, ts.URL, "?group=host")
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Group != usage.GroupHost {
		t.Errorf("group = %q, want host", snap.Group)
	}
	found = false
	for _, b := range snap.Buckets {
		if _, ok := b.Series["api.anthropic.com"]; ok {
			found = true
		}
	}
	if !found {
		t.Error("group=host did not key the series by host")
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

	// CROSSED WITH THE WINDOW KIND, because that is the axis this test was missing. Handlers take
	// different branches for a duration window and a symbolic one, and a message added on the
	// symbolic branch alone reflected the resolution parameter for a whole round while this test —
	// which probed the same parameter against the DEFAULT window — stayed green.
	queries := []string{"?window=" + probe}
	// group and resolution are validated on every path, so each is crossed with the window KINDS: a
	// duration window, both symbolic ones, and the default. `session` is deliberately absent — a
	// short session id is VALID, so it comes back in a 200 whose Session field is the id that was
	// asked about, which is the endpoint answering rather than an error reflecting. Its rejection
	// path is TestHandleUsage_SessionIDLengthCap.
	for _, window := range []string{"", "today", "7d", "10m"} {
		for _, param := range []string{"group", "resolution"} {
			q := "?" + param + "=" + probe
			if window != "" {
				q = "?window=" + window + "&" + param + "=" + probe
			}
			queries = append(queries, q)
		}
	}
	for _, q := range queries {
		status, body := fetchUsage(t, ts.URL, q)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, status)
		}
		if strings.Contains(body, "script") {
			t.Errorf("%s: error body echoes caller input: %q", q, body)
		}
	}

	// A PROBE THAT CAN ACTUALLY REACH THE SYMBOLIC RESTATEMENT, which the script probe cannot:
	// that branch is keyed on ErrResolutionExceedsWindow, and a value that fails to parse never
	// gets there. So the reachable worst case is a WELL-FORMED duration — a weaker primitive than
	// script bytes, and still caller-supplied bytes in an unauthenticated error body, which
	// writeUsageError's contract forbids outright.
	//
	// Only the symbolic windows: on a duration window this rejection is usage's own message, which
	// interpolates a RE-STRINGIFIED time.Duration rather than the caller's bytes, and is allowed to.
	for _, window := range []string{"today", "7d"} {
		const coarse = "72h"
		q := "?window=" + window + "&resolution=" + coarse
		status, body := fetchUsage(t, ts.URL, q)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, status)
		}
		if strings.Contains(body, coarse) {
			t.Errorf("%s: error body echoes the caller's resolution %q: %q", q, coarse, body)
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
// A local copy of core/usage's own inferenceEvent: that one is unexported and
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
	return insideTodayAt(startOfToday(t), time.Now(), into)
}

// insideTodayAt is insideToday with both ends of the window passed in, so the clamp can be
// exercised at a chosen instant instead of whenever the suite happens to run.
//
// SPLIT OUT BECAUSE THE COVERAGE WAS CLOCK-DEPENDENT. The wall-clock test below does kill a
// removal of the clamp — its 48h row anchors two days out, so it fires at any hour — but the
// case that actually broke CI was 00:00:30, where "today" is thirty seconds wide, and nothing
// reproduced THAT on purpose. A test whose strength varies with the time of day is a test
// that will be strong again only by luck.
func insideTodayAt(start, now time.Time, into time.Duration) time.Time {
	at := start.Add(into)
	if at.After(now) {
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
	// fail there rather than showing what IS available, so the handler serves what
	// the ring can cover and reports the window it actually served.
	//
	// WHAT IT SERVES IS CLOCK-DEPENDENT, and asserting the flat ring maximum here encoded a defect:
	// before 06:00 local, "the ring's maximum" reaches back into YESTERDAY, so window=today answered
	// with up to five and a half hours that are not today's. This test held that in place and passed
	// for eighteen hours a day — it failed in CI at 01:17 local, against a fix that was correct.
	//
	// So the assertion is the RULE — the shorter of the ring's maximum and however much of today has
	// happened — which discriminates at every hour instead of only after six.
	ts, _ := newTestServer(t, WithUsage(usage.New())) // no ledger

	before := time.Now()
	status, body := fetchUsage(t, ts.URL, "?window=today")
	after := time.Now()
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
	got, err := time.ParseDuration(snap.Window)
	if err != nil {
		t.Fatalf("window = %q, which is not a duration: %v", snap.Window, err)
	}
	// Bracketed by the clock either side of the request, so a day boundary crossed mid-test cannot
	// make this flap: the elapsed part of today is somewhere between the two readings.
	lower := elapsedToday(before)
	upper := elapsedToday(after)
	if upper < lower {
		t.Skip("the local day rolled over during the request; nothing to assert about which day it served")
	}
	want := func(d time.Duration) time.Duration {
		if d > usage.MaxWindow {
			return usage.MaxWindow
		}
		return d.Truncate(usage.BucketWidth)
	}
	if got < want(lower) || got > want(upper)+usage.BucketWidth {
		t.Errorf("window = %v, want between %v and %v: the shorter of the ring's %v maximum and the part of today that has happened",
			got, want(lower), want(upper)+usage.BucketWidth, usage.MaxWindow)
	}
}

// elapsedToday is how much of the local day has happened at t — the span window=today asks for.
func elapsedToday(t time.Time) time.Duration {
	midnight := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	return t.Sub(midnight)
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

// The same property at the instant that broke CI, with the clock passed in rather than read.
//
// TWO THINGS THE TEST ABOVE CANNOT DO, and this is why it exists rather than replacing it.
// First, it pins the 30-second window deterministically: at 00:00:30 every offset in the
// table except zero is in the future, so the clamp is the only thing keeping an anchor inside
// [From, now] — the wall-clock version only reaches that state if the suite runs in the first
// minutes of a local day. Second, the lower bound is REACHABLE here: `at.Before(start)` cannot
// fire above, because a non-negative offset added to start and a clamp to a now that is itself
// at or after start make it unreachable by construction, so that assertion documents the
// invariant without testing it. Driving `now` lets a negative offset be stated as the illegal
// input it is and checked.
func TestInsideTodayAt_ClampsAtThirtySecondsPastMidnight(t *testing.T) {
	start := time.Date(2026, 9, 16, 0, 0, 0, 0, time.Local)
	now := start.Add(30 * time.Second)

	for _, into := range []time.Duration{
		0, time.Second, 29 * time.Second, 30 * time.Second, 31 * time.Second,
		2 * time.Minute, time.Hour, 48 * time.Hour,
	} {
		at := insideTodayAt(start, now, into)
		if at.Before(start) {
			t.Errorf("insideTodayAt(%s) = %s, before the start of the window (%s): the row lands "+
				"in yesterday's day file and window=today cannot see it", into, at, start)
		}
		if at.After(now) {
			t.Errorf("insideTodayAt(%s) = %s, after now (%s) — this is the CI failure: at 00:00:30 "+
				"the window is 30 seconds wide, and every offset past it must clamp", into, at, now)
		}
	}

	// The clamp is not vacuous the other way either: an offset that FITS must be returned
	// unchanged, or "inside today" could be satisfied by always answering `now` and every
	// fixture would silently share one instant.
	if got := insideTodayAt(start, now, 10*time.Second); !got.Equal(start.Add(10 * time.Second)) {
		t.Errorf("insideTodayAt(10s) = %s, want %s: an offset inside the window is not clamped",
			got, start.Add(10*time.Second))
	}
}

// TestDegradedFrom_CarriesEveryCaveatField keeps the ledger's caveat counters and the wire's in step.
//
// The conversion was a struct literal copying two of the three counters, and Caveats.Clean() tests all
// three — so a read whose only fault was an unopenable day file set the caveat and serialised
// `"degraded":{}`: the disclosure raised with nothing in it, which is worse than silence because it
// looks like the client's fault. Reflection rather than three assertions, so the NEXT counter added to
// Caveats fails here instead of shipping.
func TestDegradedFrom_CarriesEveryCaveatField(t *testing.T) {
	var c costledger.Caveats
	cv := reflect.ValueOf(&c).Elem()
	ct := cv.Type()
	// A distinct non-zero value per field, so a copy that reads the wrong source field is caught too.
	for i := 0; i < ct.NumField(); i++ {
		if cv.Field(i).Kind() != reflect.Int64 {
			t.Fatalf("Caveats.%s is %s, not int64: this test assumes counters and must be updated with the type",
				ct.Field(i).Name, cv.Field(i).Kind())
		}
		cv.Field(i).SetInt(int64(i + 1))
	}
	if ct.NumField() < 3 {
		t.Fatalf("Caveats has %d fields; the reflection is not seeing the type, so this proves nothing", ct.NumField())
	}

	got := reflect.ValueOf(degradedFrom(c)).Elem()

	for i := 0; i < ct.NumField(); i++ {
		name := ct.Field(i).Name
		f := got.FieldByName(name)
		if !f.IsValid() {
			t.Errorf("usage.Degraded has no %s field: a caveat the ledger counts cannot reach a client, so the disclosure is emptier than the fault", name)
			continue
		}
		if f.Int() != cv.Field(i).Int() {
			t.Errorf("Degraded.%s = %d, want %d from Caveats.%s", name, f.Int(), cv.Field(i).Int(), name)
		}
	}
}

// TestHandleUsage_ASymbolicWindowExplainsAResolutionRefusal covers a message that named the wrong
// window.
//
// The bound is real: a symbolic window is one ledger-backed bucket, or the ring's maximum where there
// is no ledger, so a resolution coarser than that cannot be honoured. But ParseResolution can only
// name the span it was handed, so ?window=7d&resolution=24h answered "resolution 24h exceeds the
// 6h0m0s window" — a window the caller never asked for, for a parameter the ledger path does not read.
func TestHandleUsage_ASymbolicWindowExplainsAResolutionRefusal(t *testing.T) {
	ts, _ := newTestServer(t, WithUsage(usage.New()))

	status, body := fetchUsage(t, ts.URL, "?window=7d&resolution=24h")

	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: the resolution is genuinely unavailable", status)
	}
	// window=7d IS quotable: on the symbolic path spec.Label is one of two constants, never the
	// caller's spelling. The requested RESOLUTION is not, and asserting it was here is what pinned a
	// reflection primitive into place for a round — the body must not carry those bytes at all.
	if !strings.Contains(body, "window=7d") {
		t.Errorf("error body %q does not say which window it is talking about", body)
	}
	if strings.Contains(body, "24h") {
		t.Errorf("error body %q echoes the caller's resolution parameter: this endpoint is unauthenticated, so that is a reflection primitive — see writeUsageError", body)
	}
	// And it must not present 6h as the window that was asked for, which is what the old message did.
	if strings.Contains(body, "exceeds the 6h0m0s window") {
		t.Errorf("error body %q still names a 6h window the caller never requested", body)
	}
}

// TestHandleUsage_ALedgerWindowDoesNotJudgeResolutionAgainstTheRing is the fourth rejection, and the
// one that refused a legitimate request.
//
// There are FOUR resolution rejections and two of them depend on the window. The first version of this
// classified one — "exceeds the window" — so the other window-dependent one still fired against a span
// the caller never named: window=7d&resolution=7m was refused because 6h does not divide by 7m, while
// 7m divides seven days exactly (1440 buckets) AND the ledger answers a symbolic window as one bucket
// without reading the resolution at all. A bound from a window nobody asked for, enforced on a code
// path that was not going to run.
//
// Both halves are asserted, because the refusal is correct in the other deployment: with no ledger the
// ring really does slice its 6h maximum, and 7m really cannot label those buckets honestly.
func TestHandleUsage_ALedgerWindowDoesNotJudgeResolutionAgainstTheRing(t *testing.T) {
	at := insideToday(t, 3*time.Hour)

	t.Run("with a ledger, the resolution is not read and must not be judged", func(t *testing.T) {
		led := ledgerWithOneCostedMinute(t, at, "gw.example", "m", 1.0)
		ts, _ := newTestServer(t, WithUsage(usage.New()), WithCostLedger(led))

		status, body := fetchUsage(t, ts.URL, "?window=7d&resolution=7m")

		if status != http.StatusOK {
			t.Errorf("status = %d, want 200: %s\n7m divides 7d exactly, and this window is answered as one bucket that never reads it",
				status, body)
		}
	})

	// WITHOUT A LEDGER THE REFUSAL IS CORRECT, and then the message has to name the RIGHT bound. One
	// restatement text served both window rejections, so a resolution 51x finer than the ceiling was
	// told that 6h is the coarsest available — advice the request already satisfied. Asserting only
	// "names the window" and "does not echo the resolution" accepted that text, which is how this test
	// locked the defect in for a round: both rows below assert the CAUSE, and that the other cause's
	// wording is absent.
	t.Run("without a ledger, the ring's span is what gets sliced", func(t *testing.T) {
		for _, tc := range []struct {
			name       string
			resolution string
			wantSaid   string
			wantAbsent string
		}{
			{
				name: "too coarse for the span", resolution: "12h",
				wantSaid: "too coarse", wantAbsent: "divide",
			},
			{
				// 7m divides 7d exactly and is far finer than 6h; what it cannot do is divide 6h.
				name: "fits but does not divide the span", resolution: "7m",
				wantSaid: "does not divide", wantAbsent: "coarsest",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ts, _ := newTestServer(t, WithUsage(usage.New()))

				status, body := fetchUsage(t, ts.URL, "?window=7d&resolution="+tc.resolution)

				if status != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400: the ring serves its maximum here", status)
				}
				if !strings.Contains(body, tc.wantSaid) {
					t.Errorf("body %q does not state the actual cause (%q)", body, tc.wantSaid)
				}
				if strings.Contains(body, tc.wantAbsent) {
					t.Errorf("body %q gives the OTHER rejection's reason (%q), which names a bound this request did not hit",
						body, tc.wantAbsent)
				}
				// Restated in terms of the window the caller named, and never echoing their bytes.
				if !strings.Contains(body, "window=7d") {
					t.Errorf("body %q does not name the window the caller asked for", body)
				}
				if strings.Contains(body, tc.resolution) {
					t.Errorf("body %q echoes the caller's resolution parameter", body)
				}
			})
		}
	})
}

// TestHandleUsage_OnlyTheWindowBoundRejectionIsRestated is the other half of that message.
//
// Conditioning the restatement on Symbolic() ALONE overwrote the reason for every other resolution
// rejection: too fine for the storage bucket, not a multiple of it, and unparseable all came back
// "so 6h0m0s is the coarsest resolution available", which points the caller at a bound they never
// hit. That is the same defect the restatement exists to fix, one layer along — so it is keyed on
// ErrResolutionExceedsWindow rather than on the window kind.
func TestHandleUsage_OnlyTheWindowBoundRejectionIsRestated(t *testing.T) {
	ts, _ := newTestServer(t, WithUsage(usage.New()))

	for _, tc := range []struct {
		resolution string
		wantSaid   string
		why        string
	}{
		{resolution: "30s", wantSaid: "finer", why: "finer than the storage bucket"},
		{resolution: "90s", wantSaid: "multiple", why: "not a whole number of buckets"},
		{resolution: "abc", wantSaid: "want a duration", why: "not a duration at all"},
	} {
		t.Run(tc.resolution, func(t *testing.T) {
			status, body := fetchUsage(t, ts.URL, "?window=today&resolution="+tc.resolution)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", status, body)
			}
			if !strings.Contains(body, tc.wantSaid) {
				t.Errorf("body %q does not mention %q — the caller hit %s and needs to be told that",
					body, tc.wantSaid, tc.why)
			}
			if strings.Contains(body, "coarsest resolution available") {
				t.Errorf("body %q restates the window bound for a rejection that was not about it", body)
			}
		})
	}
}

// TestBucketSecondsFor_IsNeverZero is the first second of the local day, once a day, for every
// polling client.
//
// A ledger window returns one bucket whose length is the window's own, and int truncation makes that
// zero at the start of the day. The field carries no omitempty — the comment that said otherwise was
// wrong — so the zero is serialised as `"bucketSeconds":0` and a client deriving a burn rate divides by
// it. This floor is the only thing stopping that.
func TestBucketSecondsFor_IsNeverZero(t *testing.T) {
	start := time.Date(2026, 9, 17, 0, 0, 0, 0, time.Local)
	for _, tc := range []struct {
		name string
		to   time.Time
		want int
	}{
		{"the very first instant of the day", start, 1},
		{"half a second in", start.Add(500 * time.Millisecond), 1},
		{"three hours in", start.Add(3 * time.Hour), 10800},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := bucketSecondsFor(start, tc.to); got != tc.want {
				t.Errorf("bucketSecondsFor = %d, want %d", got, tc.want)
			}
		})
	}
}

// breakAndRestore makes the writer really lose a row, then leaves the ledger READABLE again.
//
// The drop is induced by replacing the ledger's directory with a regular FILE: every append then fails
// with ENOTDIR, which root cannot bypass where a chmod could, and no test-only setter has to exist on
// the Writer for it.
//
// THE RESTORE IS THE LOAD-BEARING HALF. A broken directory also fails the READ, so without putting the
// day file back, `degraded` appears because of UnreadableDays and a test about the write-side count
// passes whether or not that count is disclosed at all.
func breakAndRestore(t *testing.T, dir string, led *costledger.Writer, at time.Time) {
	t.Helper()

	saved := map[string][]byte{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			t.Fatalf("stash %s: %v", e.Name(), rerr)
		}
		saved[e.Name()] = b
	}
	if len(saved) == 0 {
		t.Fatal("no day file to stash, so the restore cannot make the read clean")
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("clear the ledger dir: %v", err)
	}
	if err := os.WriteFile(dir, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatalf("put a file where the ledger dir was: %v", err)
	}
	recordCostedMinute(t, led, at.Add(time.Minute), "gw.example", "m", 2.0)
	_ = led.Flush() // expected to fail; the point is the counter it moves
	if led.Dropped() == 0 {
		t.Fatal("the fixture induced no drop, so nothing it is meant to expose is observable")
	}

	if err := os.Remove(dir); err != nil {
		t.Fatalf("remove the blocking file: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("restore the ledger dir: %v", err)
	}
	for name, b := range saved {
		if werr := os.WriteFile(filepath.Join(dir, name), b, 0o600); werr != nil {
			t.Fatalf("restore %s: %v", name, werr)
		}
	}
}

// TestHandleUsage_TheWriterDropWarningFiresOnChangeNotPerRead is the log volume the disclosure bought.
//
// Dropped() is process-cumulative, so folding it into the per-read warning made one lost row at 03:00
// print "cost ledger read was incomplete" on EVERY request for the life of the process — roughly 86,000
// lines a day at a chart's poll rate, on an endpoint anyone who can reach the port can drive, and
// asserting something false about reads that were clean. The field stays on every response; the log
// fires when the number moves.
func TestHandleUsage_TheWriterDropWarningFiresOnChangeNotPerRead(t *testing.T) {
	at := insideToday(t, 3*time.Hour)
	dir := t.TempDir()
	led, err := costledger.New(dir, costledger.WithClock(func() time.Time { return at }))
	if err != nil {
		t.Fatalf("costledger.New: %v", err)
	}
	t.Cleanup(func() { _ = led.Close() })
	recordCostedMinute(t, led, at, "gw.example", "m", 1.0)
	if ferr := led.Flush(); ferr != nil {
		t.Fatalf("Flush: %v", ferr)
	}

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ts, _ := newTestServer(t, WithUsage(usage.New()), WithCostLedger(led))

	// A drop, then three clean reads.
	breakAndRestore(t, dir, led, at)
	for i := 0; i < 3; i++ {
		if status, body := fetchUsage(t, ts.URL, "?window=today"); status != http.StatusOK {
			t.Fatalf("read %d: status %d: %s", i, status, body)
		}
	}

	n := strings.Count(logs.String(), "the cost ledger writer has lost rows")
	if n != 1 {
		t.Errorf("the write-side warning fired %d times across three reads, want 1: it is a cumulative counter, so per-read logging never stops",
			n)
	}
	if strings.Contains(logs.String(), "cost ledger read was incomplete") {
		t.Errorf("a clean read logged \"read was incomplete\": %s", logs.String())
	}
}

// TestServedSpan_DoesNotReachBackPastTheWindow is 00:30 local, which is the only interesting hour.
//
// The no-ledger degrade served the ring's maximum unconditionally, so window=today just after midnight
// answered with five and a half hours of YESTERDAY plus thirty minutes of today. The label was
// literally true — 6h0m0s of data — so nothing was mislabelled; today was over-reported instead, which
// is not what "degrade to a shorter window" means.
func TestServedSpan_DoesNotReachBackPastTheWindow(t *testing.T) {
	midnight := time.Date(2026, 9, 17, 0, 0, 0, 0, time.Local)
	for _, tc := range []struct {
		name string
		from time.Time
		to   time.Time
		want time.Duration
	}{
		{"today, thirty minutes in", midnight, midnight.Add(30 * time.Minute), 30 * time.Minute},
		// THE SUB-SECOND REMAINDER, which is every real request: the span is now the bound a
		// resolution is validated against, and an untruncated 30m30.4s divides by nothing — not even
		// the 1m default — so before 06:00 every request was refused.
		{"a span with seconds and fractions", midnight, midnight.Add(30*time.Minute + 30*time.Second + 400*time.Millisecond), 30 * time.Minute},
		{"today, an hour in", midnight, midnight.Add(time.Hour), time.Hour},
		{"today, past the ring's reach", midnight, midnight.Add(9 * time.Hour), usage.MaxWindow},
		{"7d is always past it", midnight.AddDate(0, 0, -7), midnight, usage.MaxWindow},
		// FLOORED, NOT ZERO. The handler validates a resolution against this span, and a zero bound
		// refuses every resolution including the default — a 400 on the path whose whole job is to
		// degrade rather than fail. Snapshot serves at least one bucket anyway, and at 00:00:30 that
		// bucket is today's in-progress minute rather than any of yesterday.
		{"the first instant of the day", midnight, midnight, usage.BucketWidth},
		{"half a minute in", midnight, midnight.Add(30 * time.Second), usage.BucketWidth},
		{"fifty-nine seconds in", midnight, midnight.Add(59 * time.Second), usage.BucketWidth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := servedSpan(usage.Spec{From: tc.from, To: tc.to}); got != tc.want {
				t.Errorf("servedSpan = %v, want %v: serving more than the window asked for over-reports it",
					got, tc.want)
			}
		})
	}
}

// TestHandleUsage_AWriterDropReachesTheResponse is the half of Degraded that was never wired up.
//
// Caveats describes what a READ could not decode. A dropped row never reached the file, so no read can
// rediscover it — which is why Writer.Dropped is process-wide, and why a day that lost a minute to
// ENOSPC or to a full queue under a stalled filesystem served a short total under priced:true with
// `degraded` absent entirely. Writer.Dropped had no non-test caller at all; its own doc named this as
// the change that belonged next.
//
// Cumulative on the wire, deliberately: a per-read delta would attribute a drop to whichever request
// happened to notice it. The name says so.
func TestHandleUsage_AWriterDropReachesTheResponse(t *testing.T) {
	at := insideToday(t, 3*time.Hour)
	// Built inline rather than through newTestLedger, because this test needs the directory: it breaks
	// it below to make the writer really lose a row.
	dir := t.TempDir()
	led, err := costledger.New(dir, costledger.WithClock(func() time.Time { return at }))
	if err != nil {
		t.Fatalf("costledger.New: %v", err)
	}
	t.Cleanup(func() { _ = led.Close() })
	recordCostedMinute(t, led, at, "gw.example", "m", 1.0)
	if ferr := led.Flush(); ferr != nil {
		t.Fatalf("Flush: %v", ferr)
	}
	ts, _ := newTestServer(t, WithUsage(usage.New()), WithCostLedger(led))

	// Clean read first: no caveat object at all, which is the convention absence carries.
	_, body := fetchUsage(t, ts.URL, "?window=today")
	if strings.Contains(body, "degraded") {
		t.Fatalf("a clean read already reports degraded: %s", body)
	}

	// A ROW THE WRITER REALLY LOSES, induced by replacing its directory with a regular FILE so every
	// append fails with ENOTDIR. Not chmod: root ignores permission bits, and a fixture that silently
	// stops inducing the fault under CI is worse than no fixture. Not a test-only setter either — that
	// would be production API existing for this test.
	//
	// AND THE DAY FILE IS PUT BACK AFTERWARDS, so the read that follows is CLEAN. Without that, the
	// broken directory also fails the read, `degraded` appears because of UnreadableDays, and this test
	// passes whether or not the write-side count is disclosed at all — which is exactly what it is here
	// to check. Verified: with the restore, removing `|| dropped > 0` from the emission fails this test.
	breakAndRestore(t, dir, led, at)

	_, body = fetchUsage(t, ts.URL, "?window=today")
	var snap usage.Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Degraded == nil {
		t.Fatalf("no degraded object after the writer lost rows: the total below it is short and says nothing — %s", body)
	}
	if snap.Degraded.DroppedRowsTotal != led.Dropped() {
		t.Errorf("DroppedRowsTotal = %d, want %d — the writer's count is what the field reports",
			snap.Degraded.DroppedRowsTotal, led.Dropped())
	}
	if snap.Degraded.DroppedRowsTotal == 0 {
		t.Error("DroppedRowsTotal = 0 while the writer counted losses: the response would pass for the wrong reason on the read error alone")
	}
}

// TestResolutionSpan_IsTheSpanActuallySliced pins which span a resolution is judged against, which is
// the whole of the residual defect: validating against the ring's MAXIMUM let through a resolution the
// SERVED span does not divide.
//
// At 01:30 local, window=today with no ledger serves 90 minutes. resolution=1h passed, because 1h
// divides 6h — and then produced a full hour plus a 30-minute remainder, both labelled 3600 seconds,
// with fold putting the remainder last. The newest bar, which is the one being watched, read double its
// real rate. That is verbatim what ParseResolution's divisibility guard exists to prevent.
func TestResolutionSpan_IsTheSpanActuallySliced(t *testing.T) {
	midnight := time.Date(2026, 9, 17, 0, 0, 0, 0, time.Local)
	todayAt := func(d time.Duration) usage.Spec {
		return usage.Spec{Label: "today", From: midnight, To: midnight.Add(d)}
	}
	for _, tc := range []struct {
		name          string
		spec          usage.Spec
		hasLedger     bool
		wantSpan      time.Duration
		wantOneBucket bool
	}{
		{
			// The case that was wrong: 90 minutes served, 6h validated against.
			name: "today at 01:30, no ledger", spec: todayAt(90 * time.Minute),
			wantSpan: 90 * time.Minute,
		},
		{
			name: "today after 06:00, no ledger", spec: todayAt(9 * time.Hour),
			wantSpan: usage.MaxWindow,
		},
		{
			// With a ledger the resolution is not read at all, so there is no span to judge.
			name: "today with a ledger", spec: todayAt(90 * time.Minute), hasLedger: true,
			wantOneBucket: true,
		},
		{
			name: "a duration window is its own bound",
			spec: usage.Spec{Label: "10m", Dur: 10 * time.Minute}, wantSpan: 10 * time.Minute,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			span, oneBucket := resolutionSpan(tc.spec, tc.hasLedger)
			if oneBucket != tc.wantOneBucket {
				t.Errorf("oneBucket = %v, want %v", oneBucket, tc.wantOneBucket)
			}
			if !tc.wantOneBucket && span != tc.wantSpan {
				t.Errorf("span = %v, want %v: a resolution judged against a longer span than the one served can leave the newest bucket short",
					span, tc.wantSpan)
			}
		})
	}
}

// TestResolutionSpan_TheDefaultResolutionIsAlwaysAccepted is the composition nothing asserted.
//
// servedSpan's own test pinned that it returns zero at midnight, so the zero was deliberate at that
// layer — and the layer above it turns a zero span into "resolution 1m0s exceeds the 0s window" for
// every request in the first minute of the local day, including requests that name no resolution. Two
// tested-in-isolation halves composing into a 400 on the one path that exists to avoid one.
//
// Sweeping offsets across the boundary rather than asserting servedSpan alone: what has to hold is that
// the default resolution survives whatever span this produces, at any hour.
func TestResolutionSpan_TheDefaultResolutionIsAlwaysAccepted(t *testing.T) {
	midnight := time.Date(2026, 9, 17, 0, 0, 0, 0, time.Local)
	for _, offset := range []time.Duration{
		0, time.Second, 30 * time.Second, 59 * time.Second, time.Minute,
		90 * time.Second, 30 * time.Minute, 6 * time.Hour, 9 * time.Hour,
	} {
		t.Run(offset.String(), func(t *testing.T) {
			spec := usage.Spec{Label: "today", From: midnight, To: midnight.Add(offset)}
			span, oneBucket := resolutionSpan(spec, false)
			if oneBucket {
				t.Fatal("a window with no ledger is not answered as one bucket")
			}
			// "" is the default resolution — the parameter a client omits.
			if _, err := usage.ParseResolution("", span); err != nil {
				t.Errorf("the default resolution is refused %v into the day (span %v): %v — this path returns 400 where it is meant to degrade",
					offset, span, err)
			}
		})
	}
}
