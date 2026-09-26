package sessionapi

import (
	"context"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/ledger"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/session"
)

// ledgerSnapshot's ASSEMBLY of the coverage figure, not the arithmetic under it.
//
// retention_coverage_test.go pins daysOutsideRetention, the pure helper, and cmd_cost_test.go
// pins the CLI against a fake server that hands it the field already filled in. Between the two
// sat the line that actually produces it — and both of its mistakes survived the whole suite:
// deleting the assignment reported every window as fully covered, and asking RetentionCutoff()
// instead of RetentionCutoffAt(spec.To) read the clock a second time, which is the defect
// cost/ledger's retention_cutoff_test.go exists for one layer down.
//
// A HAND-BUILT SPEC rather than an HTTP request, because the mutation that matters needs the
// ledger's clock and the window's instant to fall on DIFFERENT DAYS — the straddle that made a
// complete month report one day short. Through ?window=month both come from time.Now() and the
// two are indistinguishable, so the request path cannot see the difference it is about.
func TestLedgerSnapshot_CarriesTheCoverageShortfallMeasuredAtTheWindowsOwnInstant(t *testing.T) {
	loc := time.FixedZone("TST", 3*3600)

	for _, tc := range []struct {
		name      string
		retain    int
		want      int64
		clockSkew time.Duration
	}{
		// A MONTH ASKED OF A TEN-DAY LEDGER. March has 31 days and the window opens on the 1st;
		// ten days of retention counted back from the 31st reaches the 22nd, so the 1st through
		// the 21st are outside it.
		{name: "a month against ten days of retention", retain: 10, want: 21},
		// AND A WINDOW THE LEDGER FULLY COVERS REPORTS NOTHING, which is the other half: 31 days
		// counted back from the 31st reaches exactly the 1st. This is the case the noon-versus-
		// midnight defect got wrong — a complete month marked partial every 31-day month.
		{name: "a month against a month of retention", retain: 31, want: 0},
		// THE STRADDLE. The ledger's clock has rolled past local midnight into April while the
		// window still ends in March. Measured at spec.To the answer is unchanged; measured at
		// the ledger's own clock every figure moves a day.
		{
			name:   "the ledger's clock has crossed midnight since the window was built",
			retain: 10, want: 21, clockSkew: 13 * time.Hour,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 11:30 on the last day of March, so the 13h skew below lands at 00:30 on April 1st —
			// PAST local midnight, which is the whole point. An earlier version of this used
			// 12h30m and landed at 23:30:30, still inside March, and the RetentionCutoff()
			// mutation passed: a straddle fixture that does not straddle proves nothing.
			windowEnd := time.Date(2026, 3, 31, 11, 30, 0, 0, loc)
			from := time.Date(2026, 3, 1, 0, 0, 0, 0, loc)

			clock := windowEnd.Add(tc.clockSkew)
			led, err := ledger.New(t.TempDir(),
				ledger.WithClock(func() time.Time { return clock }),
				ledger.WithRetentionDays(tc.retain))
			if err != nil {
				t.Fatalf("ledger.New: %v", err)
			}
			t.Cleanup(func() { _ = led.Close() })
			// One costed minute inside the window, so the snapshot is a real answer rather than
			// an empty one — a coverage figure attached to nothing proves less.
			recordCostedMinute(t, led, windowEnd.Add(-time.Minute), "gw", "opus", 0.25)
			if ferr := led.Flush(); ferr != nil {
				t.Fatalf("Flush: %v", ferr)
			}

			store := session.New(5*time.Minute, 100, 0)
			t.Cleanup(store.Close)
			srv := New(":0", store, WithUsage(usage.New()), WithCostLedger(led))

			snap, err := srv.ledgerSnapshot(context.Background(),
				usage.Spec{Label: usage.WindowMonth, From: from, To: windowEnd}, usage.GroupModel)
			if err != nil {
				t.Fatalf("ledgerSnapshot: %v", err)
			}
			if snap.DaysOutsideRetention != tc.want {
				t.Errorf("DaysOutsideRetention = %d, want %d (retention %d days, window %s to %s, "+
					"ledger clock %s)", snap.DaysOutsideRetention, tc.want, tc.retain,
					from.Format(time.RFC3339), windowEnd.Format(time.RFC3339), clock.Format(time.RFC3339))
			}
			// The figure it qualifies has to be there, or the assertion above is about an empty
			// snapshot: a shortfall reported over no total says nothing to a reader.
			if snap.Totals.CostMicros == 0 {
				t.Errorf("the window carries no cost, so the coverage figure qualifies nothing: %+v",
					snap.Totals)
			}
		})
	}
}

// THE SHORTFALL IS A CEILING, NOT A DEDUCTION, and this is the state that proves it.
//
// prune floors its own reference day at the NEWEST day file (see cost/ledger's prune), so a ledger
// that has been idle keeps its last retainDays files however long ago they were written — while
// the coverage figure is measured from the CLOCK. In that state a day can be reported outside
// retention and still be summed into the total.
//
// Written because two comments claimed the opposite: `abctl cost` printed "any spend on them is
// outside the total" and usage.Snapshot's own doc said "that the total cannot include it is
// certain". Both are false here, and in the direction that matters — the disclosure overstates
// what is missing rather than hiding it. The wording on both now says "may".
func TestLedgerSnapshot_ADayReportedOutsideRetentionCanStillBeInTheTotal(t *testing.T) {
	loc := time.FixedZone("TST", 3*3600)
	dir := t.TempDir()

	// Twenty-one consecutive days of spend, one costed minute each, on a ten-day ledger. The
	// clock is advanced between writes rather than the rows being back-dated, because the day
	// file a row lands in comes from the WRITER's clock.
	clock := time.Date(2026, 3, 1, 12, 0, 0, 0, loc)
	led, err := ledger.New(dir,
		ledger.WithClock(func() time.Time { return clock }),
		ledger.WithRetentionDays(10))
	if err != nil {
		t.Fatalf("ledger.New: %v", err)
	}
	for day := 1; day <= 21; day++ {
		clock = time.Date(2026, 3, day, 12, 0, 0, 0, loc)
		recordCostedMinute(t, led, clock, "gw", "opus", 0.25)
		if ferr := led.Flush(); ferr != nil {
			t.Fatalf("Flush on day %d: %v", day, ferr)
		}
	}
	if cerr := led.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}

	// TEN DAYS LATER, NOTHING WRITTEN SINCE. Reopening prunes, and prune's reference day is the
	// newest file (the 21st) rather than the clock's day (the 31st) — so it keeps the 12th
	// through the 21st, all ten of which are before the horizon the clock implies.
	reopened := time.Date(2026, 3, 31, 11, 30, 0, 0, loc)
	led2, err := ledger.New(dir,
		ledger.WithClock(func() time.Time { return reopened }),
		ledger.WithRetentionDays(10))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = led2.Close() })

	store := session.New(5*time.Minute, 100, 0)
	t.Cleanup(store.Close)
	srv := New(":0", store, WithUsage(usage.New()), WithCostLedger(led2))

	from := time.Date(2026, 3, 1, 0, 0, 0, 0, loc)
	snap, err := srv.ledgerSnapshot(context.Background(),
		usage.Spec{Label: usage.WindowMonth, From: from, To: reopened}, usage.GroupModel)
	if err != nil {
		t.Fatalf("ledgerSnapshot: %v", err)
	}

	// The coverage figure counts every day from the 1st to the 21st as outside.
	if snap.DaysOutsideRetention == 0 {
		t.Fatalf("no shortfall reported, so this test's subject is absent: %+v", snap)
	}
	// AND THE TOTAL HOLDS MORE THAN ONE DAY OF SPEND ANYWAY. Ten surviving files at $0.25 each,
	// so anything above a single day's quarter proves days the figure called outside were summed.
	const oneDayMicros = 250_000
	if snap.Totals.CostMicros <= oneDayMicros {
		t.Errorf("total is %d micros, want more than one day's %d — the surviving files inside the "+
			"reported shortfall are supposed to be in this sum", snap.Totals.CostMicros, oneDayMicros)
	}
	t.Logf("reported %d days outside retention, and the total is %d micros — the shortfall is a "+
		"ceiling on what may be missing, not a statement that it is",
		snap.DaysOutsideRetention, snap.Totals.CostMicros)
}
