package costledger

import (
	"testing"
	"time"
)

// RetentionCutoff IS A STATEMENT ABOUT CONFIGURATION, and this test exists to hold it to that
// and no more.
//
// An earlier version of this feature counted absent pre-cutoff day files and called the result
// "their spend was pruned, so the totals are SHORT". That was a false positive by construction:
// nothing here records the ledger's inception or what prune removed, so an absent old day is
// indistinguishable from a day that was never written. A three-day-old install with
// retention_days=10 reported twenty-two days of loss and had lost nothing — and the test that
// shipped with it asserted exactly that scenario, writing no day files at all and then asserting
// a shortfall of twenty-one.
//
// So the ledger now answers only "how far back does my configuration reach", and the coverage
// question is asked one layer up, where the requested window is known. See
// sessionapi.daysOutsideRetention.
func TestRetentionCutoff_IsTheConfiguredHorizonFromTheClock(t *testing.T) {
	now := time.Date(2026, time.March, 31, 15, 0, 0, 0, time.Local)
	for _, tc := range []struct {
		retain int
		want   time.Time
	}{
		// prune keeps [ref-(retainDays-1), ref], so N days is exactly N distinct dates and the
		// cutoff is the oldest of them.
		{retain: 31, want: time.Date(2026, time.March, 1, 0, 0, 0, 0, time.Local)},
		{retain: 10, want: time.Date(2026, time.March, 22, 0, 0, 0, 0, time.Local)},
		{retain: 1, want: time.Date(2026, time.March, 31, 0, 0, 0, 0, time.Local)},
	} {
		w := newRetentionWriter(t, t.TempDir(), tc.retain, now)
		got := w.RetentionCutoff()
		if gy, gm, gd := got.Date(); gy != tc.want.Year() || gm != tc.want.Month() || gd != tc.want.Day() {
			t.Errorf("retain %d: cutoff = %s, want the date %s",
				tc.retain, got.Format(dayLayout), tc.want.Format(dayLayout))
		}
	}
}

// AND IT IS A DAY IDENTIFIER, NOT THE DAY'S FIRST INSTANT — which its one consumer depends on.
//
// dayOf carries every ledger day at dayHour, so this value sits twelve hours LATER than the
// midnight that opens the same date. sessionapi.daysOutsideRetention differences it against
// usage.StartOfLocalDay and StartOfLocalMonth, which ARE midnights, and reading this as a bound
// made a month-to-date request report one day short of itself: the spend band stamped its partial
// marker on a complete and correct total every 31-day month.
//
// The test above compares dates and deliberately ignores the clock, which is exactly what let
// that through — so the shape is pinned here rather than left implicit in dayOf's doc.
func TestRetentionCutoff_IsADayIdentifierNotABound(t *testing.T) {
	now := time.Date(2026, time.March, 31, 15, 0, 0, 0, time.Local)
	got := newRetentionWriter(t, t.TempDir(), 10, now).RetentionCutoff()
	if h, m, s := got.Clock(); h != dayHour || m != 0 || s != 0 {
		t.Errorf("cutoff = %s, want the %02d:00:00 anchor every ledger day is carried at: a "+
			"consumer differencing this against a local midnight has to know which of the two "+
			"shapes it is holding", got.Format(time.RFC3339), dayHour)
	}
}

// RetentionCutoffAt USES THE CALLER'S INSTANT, which is the whole reason it exists.
//
// Its caller derives a window from one reading of the clock; asking RetentionCutoff() would read
// the clock a second time, and the comparison between the two is only sound while no day boundary
// falls in between. The margin is exactly zero in the default configuration — retention_days 31
// against month-to-date on the 31st — so a request straddling local midnight measured its window
// against one month and its horizon against the next day, and published a complete total as short.
//
// THE FIXTURE IS THAT STRADDLE: a writer whose own clock has already ticked past midnight into the
// 1st, asked for the cutoff as of the last instant of the 31st. Taking the argument, the horizon is
// the 1st of the previous month and the month is covered; taking w.now(), it is a day later and the
// month is reported one day short of itself.
func TestRetentionCutoffAt_UsesTheGivenInstantNotTheWritersClock(t *testing.T) {
	justAfterMidnight := time.Date(2026, time.February, 1, 0, 0, 0, int(time.Millisecond), time.Local)
	lastInstantOfJanuary := time.Date(2026, time.January, 31, 23, 59, 59, int(999*time.Millisecond), time.Local)
	w := newRetentionWriter(t, t.TempDir(), 31, justAfterMidnight)

	got := w.RetentionCutoffAt(lastInstantOfJanuary)
	if gy, gm, gd := got.Date(); gy != 2026 || gm != time.January || gd != 1 {
		t.Errorf("RetentionCutoffAt(%s) = %s, want the 1 January identifier: the argument names "+
			"the day the horizon is measured from, and a writer clock that has already crossed "+
			"midnight must not move it",
			lastInstantOfJanuary.Format(dayLayout), got.Format(dayLayout))
	}
	// And the two really do differ here, or the assertion above proves nothing about which clock
	// was read.
	if own := w.RetentionCutoff(); own.Equal(got) {
		t.Fatalf("RetentionCutoff() and RetentionCutoffAt(%s) both return %s, so this fixture no "+
			"longer straddles a day boundary and cannot tell the two clocks apart",
			lastInstantOfJanuary.Format(dayLayout), own.Format(dayLayout))
	}
}

// AND IT REPORTS NO LOSS, because it cannot know of one.
//
// A ledger with no files at all has the same cutoff as a full one: the horizon is a function of
// the clock and the retention setting, not of what is on disk. This is the assertion that stops
// the false positive coming back — a fresh install must be indistinguishable here from a
// long-running one, because on the evidence available it is.
func TestRetentionCutoff_DoesNotDependOnWhatIsOnDisk(t *testing.T) {
	now := time.Date(2026, time.March, 31, 15, 0, 0, 0, time.Local)

	empty := newRetentionWriter(t, t.TempDir(), 10, now)

	dir := t.TempDir()
	// A day file well inside retention, and one well outside it that prune has not reached.
	writeDay(t, dir, now, line(now, "gw", "m", 1, 10, 5, 100))
	old := time.Date(2026, time.March, 5, 12, 0, 0, 0, time.Local)
	writeDay(t, dir, old, line(old, "gw", "m", 1, 10, 5, 100))
	full := newRetentionWriter(t, dir, 10, now)

	if !empty.RetentionCutoff().Equal(full.RetentionCutoff()) {
		t.Errorf("an empty ledger reports cutoff %s and a populated one %s — the horizon is a "+
			"fact about configuration, and making it depend on the files present is how "+
			"\"absent and old\" got mistaken for \"pruned\"",
			empty.RetentionCutoff().Format(dayLayout), full.RetentionCutoff().Format(dayLayout))
	}
}

// A window reaching past the cutoff still reads every row it CAN, and reports the read clean.
//
// The shortfall is coverage, stated by the layer that knows the request; it is not damage, so
// Caveats must stay empty. A non-clean Caveats here would put the answer under Degraded, whose
// meaning is "rows are missing from the sum".
func TestQuery_AWindowPastTheCutoffStillReportsACleanRead(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, time.March, 31, 15, 0, 0, 0, time.Local)
	writeDay(t, dir, now, line(now, "gw", "m", 1, 10, 5, 100))
	w := newRetentionWriter(t, dir, 10, now)

	from := time.Date(2026, time.March, 1, 0, 0, 0, 0, time.Local)
	rows, caveats, err := w.Query(t.Context(), from, now)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("got %d rows, want the 1 inside retention", len(rows))
	}
	if !caveats.Clean() {
		t.Errorf("Caveats = %+v for a window that merely reaches past retention — nothing was "+
			"lost, and a non-clean read here would be published as damage", caveats)
	}
}

// newRetentionWriter is a Writer with a pinned clock and an explicit retention, which is the pair
// the cutoff is derived from.
func newRetentionWriter(t *testing.T, dir string, retainDays int, now time.Time) *Writer {
	t.Helper()
	w, err := New(dir, WithClock(func() time.Time { return now }), WithRetentionDays(retainDays))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}
