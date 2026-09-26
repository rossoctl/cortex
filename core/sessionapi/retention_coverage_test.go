package sessionapi

import (
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/ledger"
	"github.com/rossoctl/cortex/core/cost/usage"
)

// daysOutsideRetention is the coverage statement that replaced a false disclosure.
//
// The first version of this counted absent pre-cutoff day FILES and reported them as pruned
// spend under usage.Degraded, whose meaning is "rows are missing from the sum". That cannot be
// known: nothing in cost/ledger records its inception or what prune removed, so an absent old day
// is indistinguishable from a day nobody wrote. A three-day-old install with retention_days=10
// reported twenty-two days of loss and had lost nothing.
//
// So the claim is narrowed to what the server can prove: this window asked for N days the
// configuration does not reach. Whether spend happened on them is unknowable, and so is
// whether the total includes it — prune floors its own window at the newest day file, so an
// idle ledger keeps days this figure calls outside and a query sums them. The figure is a
// CEILING on what is absent; TestLedgerSnapshot_ADayReportedOutsideRetentionCanStillBeInTheTotal
// reproduces the state, and every consumer's wording is hedged accordingly.
//
// THE CUTOFF IS BUILT AT THE PRODUCER'S ANCHOR HOUR, and that is the whole reason this file was
// rewritten. Every fixture here used to hand-build it at MIDNIGHT while the only caller passes
// cost/ledger's day identifier, which is carried at NOON — so the suite was green over a
// twelve-hour error that reported a complete month one day short of itself. A test fixture that
// is shaped differently from the production value proves nothing about production.
func TestDaysOutsideRetention(t *testing.T) {
	anchor := producerAnchorHour(t)
	day := func(d int) time.Time {
		return time.Date(2026, time.March, d, anchor, 0, 0, 0, time.Local)
	}
	midnight := func(d int) time.Time {
		return time.Date(2026, time.March, d, 0, 0, 0, 0, time.Local)
	}
	cutoff := day(22) // retention_days=10 against a clock on the 31st

	for _, tc := range []struct {
		name string
		from time.Time
		want int64
	}{
		// A window that starts inside retention asks for nothing it cannot have. The first case
		// is the regression: midnight on the cutoff's own date is twelve hours EARLIER than the
		// identifier, and an instant comparison called that a day of missing coverage.
		{"starts at midnight on the cutoff date", midnight(22), 0},
		{"starts inside retention", midnight(25), 0},
		{"starts today", midnight(31), 0},
		// Month-to-date on the 31st against ten days: the 1st through the 21st are unreachable.
		{"month to date", midnight(1), 21},
		{"one day past", midnight(21), 1},
		{"a week past", midnight(15), 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := daysOutsideRetention(tc.from, cutoff); got != tc.want {
				t.Errorf("daysOutsideRetention(%s, %s) = %d, want %d",
					tc.from.Format("Jan 2 15:04"), cutoff.Format("Jan 2 15:04"), got, tc.want)
			}
		})
	}

	// PART OF A DAY IS A WHOLE DAY, because the ledger stores and prunes whole day files: a
	// window opening in the last hour of the date before the cutoff has one date it cannot
	// cover, not a twenty-fourth of one.
	//
	// Stated as an earlier DATE rather than as "six hours before the cutoff", which is what it
	// used to say and which is not past the cutoff at all — six hours before a noon identifier
	// is six in the morning of the retained date itself.
	if got := daysOutsideRetention(midnight(21).Add(23*time.Hour), cutoff); got != 1 {
		t.Errorf("23:00 on the date before the cutoff = %d days, want 1", got)
	}
}

// AND IT SAYS NOTHING ON A COMPLETE MONTH, which is the false disclosure that reached the screen.
//
// retention_days defaults to 31 BECAUSE that is the longest month — so on the 31st the horizon
// reaches exactly the 1st and month-to-date is fully covered. Reporting one day short there is
// not a rounding quibble: the band stamps the partial marker on the figure, so a correct total is
// published as a floor, every 31-day month, on the one day of the month an operator is most
// likely to be reconciling a budget.
//
// DRIVEN THROUGH THE REAL PRODUCER — cost/ledger's own RetentionCutoff against usage's own
// StartOfLocalMonth — because the defect was entirely in the disagreement between the two shapes.
// Nothing built by hand in this file can pin that.
func TestDaysOutsideRetention_ACompleteMonthIsNotShort(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ref    time.Time
		retain int
	}{
		// The exact case: 31 days of retention on the 31st reaches the 1st and no further.
		{"31-day month at the default retention", time.Date(2026, time.January, 31, 15, 4, 0, 0, time.Local), 31},
		// A shorter month has a day of slack, and must be just as silent.
		{"30-day month at the default retention", time.Date(2026, time.April, 30, 9, 30, 0, 0, time.Local), 31},
		// Nine is config's minimum retention_days (see minCostLedgerRetentionDays), so the 9th is
		// where the same arithmetic runs exactly at the other end of the allowed range.
		{"the 9th at the minimum retention", time.Date(2026, time.June, 9, 23, 59, 0, 0, time.Local), 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cutoff := ledgerCutoff(t, tc.ref, tc.retain)
			from := usage.StartOfLocalMonth(tc.ref)
			// The premise: this horizon really does cover the month, or a non-zero answer would
			// be correct and the case would be asserting the opposite of what it claims.
			if dateOf(cutoff).After(dateOf(from)) {
				t.Fatalf("retention %d on %s reaches back only to %s, which is after the month's "+
					"first day — this case is about a horizon that covers the whole month",
					tc.retain, tc.ref.Format("Jan 2"), cutoff.Format("Jan 2"))
			}
			if got := daysOutsideRetention(from, cutoff); got != 0 {
				t.Errorf("month-to-date on %s with retention %d reports %d days outside a horizon "+
					"that reaches its first day: the band marks the total partial when it is whole",
					tc.ref.Format("Jan 2"), tc.retain, got)
			}
		})
	}
}

// AND THE COUNT IS THE SAME NUMBER IN EVERY ZONE, which the arithmetic it replaced was not.
//
// Truncate(24*time.Hour) truncates on the absolute UTC-epoch axis, not to a local date. East of
// Greenwich a local midnight and a local noon fall on DIFFERENT UTC dates, so the difference
// gained a day: month-to-date on the 20th at retention 10 answered 11 in Berlin and Tokyo and 10
// in UTC, New_York and Auckland. A suite running in one zone — or in a fixed-offset test zone —
// cannot see that at all, which is why the count is now asserted as a zone-invariant.
func TestDaysOutsideRetention_IsTheSameCountInEveryZone(t *testing.T) {
	anchor := producerAnchorHour(t)
	for _, name := range []string{
		"UTC",
		"America/New_York", // behind Greenwich, and the answer the old code got right
		"Europe/Berlin",    // +1/+2: the zone where it gained a day
		"Asia/Tokyo",       // +9, no transitions at all
		"Pacific/Auckland", // +12/+13, the far side of the date line
	} {
		loc := mustZone(t, name)
		t.Run(name, func(t *testing.T) {
			// Month-to-date on the 20th at retention 10: the horizon reaches the 11th, so the 1st
			// through the 10th are outside it.
			ref := time.Date(2026, time.January, 20, 15, 4, 0, 0, loc)
			cutoff := time.Date(2026, time.January, 20, anchor, 0, 0, 0, loc).AddDate(0, 0, -9)
			from := time.Date(2026, time.January, 1, 0, 0, 0, 0, loc)
			if got := daysOutsideRetention(from, cutoff); got != 10 {
				t.Errorf("month-to-date on %s = %d days outside retention, want 10 in every zone",
					ref.Format("Jan 2"), got)
			}
			// And the complete-month case, which must be silent everywhere.
			ref = time.Date(2026, time.January, 31, 15, 4, 0, 0, loc)
			cutoff = time.Date(2026, time.January, 31, anchor, 0, 0, 0, loc).AddDate(0, 0, -30)
			if got := daysOutsideRetention(from, cutoff); got != 0 {
				t.Errorf("a complete 31-day month = %d days outside retention, want 0", got)
			}
		})
	}
}

// AND A DST TRANSITION BETWEEN THE TWO DATES DOES NOT MOVE THE COUNT, which is why the difference
// is taken on noon-UTC anchors rather than on the local dates themselves.
//
// Europe/Berlin springs forward on 29 March 2026, so the span from the 25th to the 31st is 143
// hours, not 144. Subtracting local midnights and dividing by 24h yields 5 for six dates — one
// date of coverage silently forgiven. usage and cost/ledger both carry this hazard in their own
// docs; this is the third place it has to be handled and the only one that was counting hours.
func TestDaysOutsideRetention_SurvivesASpringForwardBetweenTheDates(t *testing.T) {
	loc := mustZone(t, "Europe/Berlin")
	anchor := producerAnchorHour(t)
	from := time.Date(2026, time.March, 25, 0, 0, 0, 0, loc)
	cutoff := time.Date(2026, time.March, 31, anchor, 0, 0, 0, loc)

	// The premise: the offset really does change in between, or this test asserts nothing.
	_, offFrom := from.Zone()
	_, offCutoff := cutoff.Zone()
	if offFrom == offCutoff {
		t.Fatalf("Europe/Berlin has the same offset (%ds) on 25 and 31 March 2026, so this "+
			"fixture no longer straddles a transition and cannot catch an hour-counting "+
			"implementation", offFrom)
	}
	if got := daysOutsideRetention(from, cutoff); got != 6 {
		t.Errorf("25 to 31 March across Berlin's spring forward = %d days, want 6: the span is "+
			"143 hours and six dates, and dates are what the ledger stores", got)
	}
}

// AND IT SAYS NOTHING ON A FRESH INSTALL, which is the false positive the old design encoded.
//
// A three-day-old ledger and a year-old one have the same horizon, so a request inside that
// horizon reports zero regardless of what is on disk. This is the assertion that would have
// failed the previous implementation.
func TestDaysOutsideRetention_IsSilentForAWindowInsideTheHorizon(t *testing.T) {
	now := time.Date(2026, time.March, 31, 15, 0, 0, 0, time.Local)
	cutoff := ledgerCutoff(t, now, 10)

	// "today" on a ledger installed three days ago: nothing outside retention, whatever the
	// directory holds. StartOfLocalDay rather than a 24h truncation, because that is what the
	// caller passes and the two are different instants in every zone but UTC.
	if got := daysOutsideRetention(usage.StartOfLocalDay(now), cutoff); got != 0 {
		t.Errorf("window=today reports %d days outside retention on a ten-day ledger; a fresh "+
			"install must be indistinguishable from a long-running one here", got)
	}
	// And 7d, which fits a ten-day retention by three days.
	if got := daysOutsideRetention(now.AddDate(0, 0, -7), cutoff); got != 0 {
		t.Errorf("window=7d reports %d days outside a ten-day retention, which covers it", got)
	}
}

// ledgerCutoff is the horizon the REAL producer reports for a given clock and retention.
//
// A live ledger.Writer rather than a hand-built time, so the shape of the value under test is
// the shape production passes. The ledger needs no rows for this: RetentionCutoff is a statement
// about configuration, which is the property its own test pins.
func ledgerCutoff(t *testing.T, now time.Time, retain int) time.Time {
	t.Helper()
	w, err := ledger.New(t.TempDir(),
		ledger.WithRetentionDays(retain),
		ledger.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("ledger.New: %v", err)
	}
	return w.RetentionCutoff()
}

// dateOf drops a timestamp's clock so two values can be compared as calendar dates.
//
// The test's own version of what the code under test does, written independently rather than by
// calling utcNoonOfDate: a premise that shares its arithmetic with the subject cannot catch the
// subject being wrong.
func dateOf(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// producerAnchorHour is the hour of day cost/ledger carries its day identifiers at.
//
// READ FROM THE PRODUCER rather than written as 12 here, so the fixtures in this file follow
// ledger.dayHour if it ever moves instead of quietly testing a shape nothing produces —
// which is the exact failure this file is a rewrite of.
func producerAnchorHour(t *testing.T) int {
	t.Helper()
	return ledgerCutoff(t, time.Date(2026, time.March, 31, 15, 0, 0, 0, time.Local), 10).Hour()
}
