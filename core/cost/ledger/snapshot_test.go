package ledger

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
)

// recordCostedMinute records one settled $costUSD response at when.
func recordCostedMinute(t *testing.T, w *Writer, when time.Time, model string, costUSD float64) {
	t.Helper()
	w.Record("s1", costedEventAt(t, when, model, costUSD))
}

// Snapshot serves one bucket equal to its totals, narrows to an agent's rows, and reports
// GroupNone for an axis a row has no column for.
func TestSnapshot_ShapeAgentAndGroupInEffect(t *testing.T) {
	now := at
	w := newTestWriter(t, t.TempDir(), func() time.Time { return now })
	for _, c := range []struct {
		name string
		usd  float64
	}{{"claude-code", 0.25}, {"opencode", 0.10}} {
		e := costedEvent(t, "gw", "opus", c.usd, 100, 50)
		e.Client = &pipeline.EventClient{Name: c.name, Version: "1.0"}
		w.Record("s1", e)
	}
	spec := usage.Spec{Label: usage.WindowToday, From: usage.StartOfLocalDay(at), To: at.Add(time.Minute)}

	snap, err := w.Snapshot(context.Background(), spec, usage.GroupStatus, "")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Group != usage.GroupNone {
		t.Errorf("Group = %q for group=status, want %q", snap.Group, usage.GroupNone)
	}
	if len(snap.Buckets) != 1 || snap.Buckets[0].Counts != snap.Totals || !snap.Buckets[0].At.Equal(spec.From) {
		t.Errorf("Buckets = %+v, want one bucket at %s equal to Totals %+v", snap.Buckets, spec.From, snap.Totals)
	}
	if snap.Totals.CostMicros != 350_000 || !snap.Priced || snap.Window != spec.Label {
		t.Errorf("snapshot = %+v, want $0.35 priced under %q", snap, spec.Label)
	}

	one, err := w.Snapshot(context.Background(), spec, usage.GroupAgent, "opencode")
	if err != nil {
		t.Fatalf("Snapshot(agent): %v", err)
	}
	if one.Agent != "opencode" || one.Totals.CostMicros != 100_000 || one.Group != usage.GroupAgent {
		t.Errorf("agent snapshot = agent %q, %d micros, group %q; want opencode, 100000, agent",
			one.Agent, one.Totals.CostMicros, one.Group)
	}
	if _, ok := one.Buckets[0].Series["opencode"]; !ok || len(one.Buckets[0].Series) != 1 {
		t.Errorf("agent series = %v, want only opencode", one.Buckets[0].Series)
	}
}

// A duration window carries no From or To, so it is refused rather than served as an empty total.
func TestSnapshot_RefusesADurationWindow(t *testing.T) {
	w := newTestWriter(t, t.TempDir(), func() time.Time { return at })
	recordCostedMinute(t, w, at, "opus", 0.25)
	spec, err := usage.ParseWindowSpec("1h", at)
	if err != nil {
		t.Fatalf("ParseWindowSpec: %v", err)
	}
	if _, err := w.Snapshot(context.Background(), spec, usage.GroupNone, ""); err == nil {
		t.Error("Snapshot served a duration window")
	}
}

// The coverage figure is measured at spec.To, the window's own instant. A hand-built spec puts the
// ledger's clock and the window's end on different days, which a request through time.Now() cannot.
func TestSnapshot_CarriesTheCoverageShortfallMeasuredAtTheWindowsOwnInstant(t *testing.T) {
	loc := time.FixedZone("TST", 3*3600)

	for _, tc := range []struct {
		name      string
		retain    int
		want      int64
		clockSkew time.Duration
	}{
		// Ten days back from March 31st reaches the 22nd, so the 1st through the 21st are outside.
		{name: "a month against ten days of retention", retain: 10, want: 21},
		// 31 days back from the 31st reaches exactly the 1st.
		{name: "a month against a month of retention", retain: 31, want: 0},
		// The 13h skew moves the ledger's clock to 00:30 on April 1st while the window ends in March.
		{
			name:   "the ledger's clock has crossed midnight since the window was built",
			retain: 10, want: 21, clockSkew: 13 * time.Hour,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			windowEnd := time.Date(2026, 3, 31, 11, 30, 0, 0, loc)
			from := time.Date(2026, 3, 1, 0, 0, 0, 0, loc)

			clock := windowEnd.Add(tc.clockSkew)
			w, err := New(t.TempDir(), WithClock(func() time.Time { return clock }), WithRetentionDays(tc.retain))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = w.Close() })
			recordCostedMinute(t, w, windowEnd.Add(-time.Minute), "opus", 0.25)
			if ferr := w.Flush(); ferr != nil {
				t.Fatalf("Flush: %v", ferr)
			}

			snap, err := w.Snapshot(context.Background(),
				usage.Spec{Label: usage.WindowMonth, From: from, To: windowEnd}, usage.GroupModel, "")
			if err != nil {
				t.Fatalf("Snapshot: %v", err)
			}
			if snap.DaysOutsideRetention != tc.want {
				t.Errorf("DaysOutsideRetention = %d, want %d (retention %d days, ledger clock %s)",
					snap.DaysOutsideRetention, tc.want, tc.retain, clock.Format(time.RFC3339))
			}
			if snap.Totals.CostMicros == 0 {
				t.Errorf("the window carries no cost, so the coverage figure qualifies nothing: %+v", snap.Totals)
			}
		})
	}
}

// The shortfall is a ceiling on what may be missing. prune keeps the newest retainDays files of an
// idle ledger, while the figure is measured from the clock, so days it reports outside retention can
// still be summed.
func TestSnapshot_ADayReportedOutsideRetentionCanStillBeInTheTotal(t *testing.T) {
	loc := time.FixedZone("TST", 3*3600)
	dir := t.TempDir()

	// One costed minute on each of 21 days, on a ten-day ledger.
	clock := time.Date(2026, 3, 1, 12, 0, 0, 0, loc)
	w, err := New(dir, WithClock(func() time.Time { return clock }), WithRetentionDays(10))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for day := 1; day <= 21; day++ {
		clock = time.Date(2026, 3, day, 12, 0, 0, 0, loc)
		recordCostedMinute(t, w, clock, "opus", 0.25)
		if ferr := w.Flush(); ferr != nil {
			t.Fatalf("Flush on day %d: %v", day, ferr)
		}
	}
	if cerr := w.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}

	// Reopened ten days later: prune keeps the 12th through the 21st, all before the clock's horizon.
	reopened := time.Date(2026, 3, 31, 11, 30, 0, 0, loc)
	w2, err := New(dir, WithClock(func() time.Time { return reopened }), WithRetentionDays(10))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = w2.Close() })

	from := time.Date(2026, 3, 1, 0, 0, 0, 0, loc)
	snap, err := w2.Snapshot(context.Background(),
		usage.Spec{Label: usage.WindowMonth, From: from, To: reopened}, usage.GroupModel, "")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.DaysOutsideRetention == 0 {
		t.Fatalf("no shortfall reported, so this test's subject is absent: %+v", snap)
	}
	const oneDayMicros = 250_000
	if snap.Totals.CostMicros <= oneDayMicros {
		t.Errorf("total is %d micros, want more than one day's %d from the files inside the reported shortfall",
			snap.Totals.CostMicros, oneDayMicros)
	}
}

// A dropped row is warned about once per increase in the count, and a clean read logs nothing
// about the read.
func TestSnapshot_TheDropWarningFiresOnChangeNotPerRead(t *testing.T) {
	now := at
	w := newTestWriter(t, t.TempDir(), func() time.Time { return now })
	// The drop lands on the day after the window, so the window's own read stays clean.
	nextDay := at.AddDate(0, 0, 1)
	blockDayFile(t, w, nextDay)
	recordCostedMinute(t, w, nextDay, "m", 1.0)
	now = nextDay.Add(time.Minute)
	if err := w.Flush(); err == nil {
		t.Fatal("Flush succeeded into a directory, so no row was dropped")
	}
	if w.Dropped() == 0 {
		t.Fatal("the fixture induced no drop")
	}

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	spec := usage.Spec{Label: usage.WindowToday, From: usage.StartOfLocalDay(at), To: at}
	for i := 0; i < 3; i++ {
		snap, err := w.Snapshot(context.Background(), spec, usage.GroupNone, "")
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if snap.Degraded == nil || snap.Degraded.DroppedRowsTotal != w.Dropped() {
			t.Fatalf("read %d: Degraded = %+v, want DroppedRowsTotal %d", i, snap.Degraded, w.Dropped())
		}
	}
	if n := strings.Count(logs.String(), "has lost rows"); n != 1 {
		t.Errorf("the drop warning fired %d times across three reads, want 1:\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), "read was incomplete") {
		t.Errorf("a clean read logged \"read was incomplete\":\n%s", logs.String())
	}
}

// degradedFrom carries every Caveats counter to the field of the same name.
func TestDegradedFrom_CarriesEveryCaveatField(t *testing.T) {
	var c Caveats
	cv := reflect.ValueOf(&c).Elem()
	ct := cv.Type()
	// A distinct value per field, so a copy from the wrong source field is caught too.
	for i := 0; i < ct.NumField(); i++ {
		if cv.Field(i).Kind() != reflect.Int64 {
			t.Fatalf("Caveats.%s is %s, not int64: update this test with the type", ct.Field(i).Name, cv.Field(i).Kind())
		}
		cv.Field(i).SetInt(int64(i + 1))
	}
	if ct.NumField() < 3 {
		t.Fatalf("Caveats has %d fields; the reflection is not seeing the type", ct.NumField())
	}

	got := reflect.ValueOf(degradedFrom(c)).Elem()
	for i := 0; i < ct.NumField(); i++ {
		name := ct.Field(i).Name
		f := got.FieldByName(name)
		if !f.IsValid() {
			t.Errorf("usage.Degraded has no %s field, so that caveat cannot reach a client", name)
			continue
		}
		if f.Int() != cv.Field(i).Int() {
			t.Errorf("Degraded.%s = %d, want %d", name, f.Int(), cv.Field(i).Int())
		}
	}
}

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

// The cutoff in these cases is built at dayHour, the shape RetentionCutoff returns, while from is
// a local midnight, the shape usage's window starts take.
func TestDaysOutsideRetention(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, time.March, d, dayHour, 0, 0, 0, time.Local) }
	midnight := func(d int) time.Time { return time.Date(2026, time.March, d, 0, 0, 0, 0, time.Local) }
	cutoff := day(22) // retention_days=10 against a clock on the 31st

	for _, tc := range []struct {
		name string
		from time.Time
		want int64
	}{
		// Midnight on the cutoff's own date is twelve hours before it and still the same date.
		{"starts at midnight on the cutoff date", midnight(22), 0},
		{"starts inside retention", midnight(25), 0},
		{"starts today", midnight(31), 0},
		{"month to date", midnight(1), 21},
		{"one day past", midnight(21), 1},
		{"a week past", midnight(15), 7},
		// Part of a day is a whole day: the ledger stores and prunes whole day files.
		{"23:00 on the date before the cutoff", midnight(21).Add(23 * time.Hour), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := daysOutsideRetention(tc.from, cutoff); got != tc.want {
				t.Errorf("daysOutsideRetention(%s, %s) = %d, want %d",
					tc.from.Format("Jan 2 15:04"), cutoff.Format("Jan 2 15:04"), got, tc.want)
			}
		})
	}
}

// A month the horizon reaches the first day of reports zero. Driven through RetentionCutoff and
// usage.StartOfLocalMonth, the two real producers of the arguments.
func TestDaysOutsideRetention_ACompleteMonthIsNotShort(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ref    time.Time
		retain int
	}{
		{"31-day month at the default retention", time.Date(2026, time.January, 31, 15, 4, 0, 0, time.Local), 31},
		{"30-day month at the default retention", time.Date(2026, time.April, 30, 9, 30, 0, 0, time.Local), 31},
		// Nine is config's minimum retention_days.
		{"the 9th at the minimum retention", time.Date(2026, time.June, 9, 23, 59, 0, 0, time.Local), 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cutoff := ledgerCutoff(t, tc.ref, tc.retain)
			from := usage.StartOfLocalMonth(tc.ref)
			if dateOf(cutoff).After(dateOf(from)) {
				t.Fatalf("retention %d on %s reaches back only to %s, after the month's first day",
					tc.retain, tc.ref.Format("Jan 2"), cutoff.Format("Jan 2"))
			}
			if got := daysOutsideRetention(from, cutoff); got != 0 {
				t.Errorf("month-to-date on %s with retention %d reports %d days outside a horizon that reaches its first day",
					tc.ref.Format("Jan 2"), tc.retain, got)
			}
		})
	}
}

// East of Greenwich a local midnight and a local noon fall on different UTC dates, so the count is
// asserted in zones on both sides of it.
func TestDaysOutsideRetention_IsTheSameCountInEveryZone(t *testing.T) {
	for _, name := range []string{"UTC", "America/New_York", "Europe/Berlin", "Asia/Tokyo", "Pacific/Auckland"} {
		loc := mustZone(t, name)
		t.Run(name, func(t *testing.T) {
			from := time.Date(2026, time.January, 1, 0, 0, 0, 0, loc)
			// Month-to-date on the 20th at retention 10: the horizon reaches the 11th.
			cutoff := time.Date(2026, time.January, 20, dayHour, 0, 0, 0, loc).AddDate(0, 0, -9)
			if got := daysOutsideRetention(from, cutoff); got != 10 {
				t.Errorf("month-to-date on Jan 20 = %d days outside retention, want 10", got)
			}
			cutoff = time.Date(2026, time.January, 31, dayHour, 0, 0, 0, loc).AddDate(0, 0, -30)
			if got := daysOutsideRetention(from, cutoff); got != 0 {
				t.Errorf("a complete 31-day month = %d days outside retention, want 0", got)
			}
		})
	}
}

// Berlin springs forward on 29 March 2026, so the 25th to the 31st is 143 hours and six dates.
func TestDaysOutsideRetention_SurvivesASpringForwardBetweenTheDates(t *testing.T) {
	loc := mustZone(t, "Europe/Berlin")
	from := time.Date(2026, time.March, 25, 0, 0, 0, 0, loc)
	cutoff := time.Date(2026, time.March, 31, dayHour, 0, 0, 0, loc)

	_, offFrom := from.Zone()
	_, offCutoff := cutoff.Zone()
	if offFrom == offCutoff {
		t.Fatalf("Europe/Berlin has the same offset (%ds) on 25 and 31 March 2026, so the fixture straddles no transition", offFrom)
	}
	if got := daysOutsideRetention(from, cutoff); got != 6 {
		t.Errorf("25 to 31 March across Berlin's spring forward = %d days, want 6", got)
	}
}

// A window inside the horizon reports zero, however young the ledger is.
func TestDaysOutsideRetention_IsSilentForAWindowInsideTheHorizon(t *testing.T) {
	now := time.Date(2026, time.March, 31, 15, 0, 0, 0, time.Local)
	cutoff := ledgerCutoff(t, now, 10)

	if got := daysOutsideRetention(usage.StartOfLocalDay(now), cutoff); got != 0 {
		t.Errorf("window=today reports %d days outside a ten-day retention", got)
	}
	if got := daysOutsideRetention(now.AddDate(0, 0, -7), cutoff); got != 0 {
		t.Errorf("window=7d reports %d days outside a ten-day retention", got)
	}
}

// ledgerCutoff is RetentionCutoff for a given clock and retention.
func ledgerCutoff(t *testing.T, now time.Time, retain int) time.Time {
	t.Helper()
	w, err := New(t.TempDir(), WithRetentionDays(retain), WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w.RetentionCutoff()
}

// dateOf is t's calendar date at midnight UTC, written independently of utcDayOf so a premise
// built with it cannot share the subject's arithmetic.
func dateOf(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
