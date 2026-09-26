package sessionapi

import (
	"context"
	"testing"
	"time"

	// EMBEDDED TZDATA. mustZone treats a load failure as a test failure, so the database has
	// to be in the binary; see the same import in usage/dst_test.go and costledger/dst_test.go
	// for why a skip here would be worse than no test.
	_ "time/tzdata"

	"github.com/rossoctl/cortex/core/cost/ledger"
	"github.com/rossoctl/cortex/core/cost/usage"
)

// dayBoundaryCase is one zone and date, and where its local day actually begins.
type dayBoundaryCase struct {
	zone string
	// date is the LOCAL calendar date, which is also what the ledger names its day file.
	date string
	// start is the earliest instant that EXISTS on that date, MEASURED against tzdata and
	// written out in RFC3339 so the offset is part of the fixture. Deliberately NOT taken
	// from usage.StartOfLocalDay: an expectation computed by the code under test moves with
	// the defect and cannot detect it. That is not hypothetical — the first draft of this
	// test derived its samples from spec.From and passed with the old bound in place.
	start string
}

// dayBoundaryCases are the transition shapes, with controls. The same set usage/dst_test.go
// pins the bound against, so a failure here and a pass there localises the break to the
// join rather than to either side.
var dayBoundaryCases = []dayBoundaryCase{
	{"America/Havana", "2026-03-08", "2026-03-08T01:00:00-04:00"},   // spring gap at 00:00
	{"America/Havana", "2026-11-01", "2026-11-01T00:00:00-04:00"},   // 00:00 occurs twice
	{"America/Santiago", "2026-09-06", "2026-09-06T01:00:00-03:00"}, // spring gap at 00:00
	{"America/Santiago", "2026-04-05", "2026-04-05T00:00:00-04:00"}, // autumn back to 23:00
	{"Asia/Beirut", "2026-03-29", "2026-03-29T01:00:00+03:00"},      // spring gap at 00:00
	{"Asia/Beirut", "2026-10-25", "2026-10-25T00:00:00+02:00"},      // autumn back to 23:00
	{"America/New_York", "2026-03-08", "2026-03-08T00:00:00-05:00"}, // control: 02:00 shift
	{"America/New_York", "2026-11-01", "2026-11-01T00:00:00-04:00"}, // control: 02:00 shift
	{"UTC", "2026-03-08", "2026-03-08T00:00:00Z"},                   // control: no shifts
}

// TestTodayWindowAndLedgerDayAgreeInEveryZone is the JOIN, and it is the reason the bound
// was corrected when it was.
//
// Two layers decide where a day starts and they have to give the same answer.
// costledger names a day file from a date carried at noon, so the file is named for the date
// a row is genuinely on in every zone. usage.ParseWindowSpec computes the bounds that SELECT
// those files. Each layer has its own tests; neither could catch a disagreement BETWEEN them,
// and a disagreement is silent — no error, no caveat, just a dollar total that includes spend
// from a date nobody asked about.
//
// This package is where the two meet, so this is where the join is asserted: build a ledger
// whose clock is in the zone, write costed minutes on both sides of the boundary, ask
// window=today for its bounds and hand exactly those to ledger.Window. What comes back
// must be precisely the rows whose local date is the date in question.
//
// MEASURED WITH THE OLD BOUND, to say what it cost rather than that it was wrong: in
// America/Havana on 2026-03-08 and in America/Santiago on 2026-09-06 this returned FIVE rows
// where three belong — two extra minutes of the previous date's spend, $0.50 of a $1.25
// total, reported as today's. The controls returned three either way, which is why nothing
// caught it.
func TestTodayWindowAndLedgerDayAgreeInEveryZone(t *testing.T) {
	for _, c := range dayBoundaryCases {
		t.Run(c.zone+"/"+c.date, func(t *testing.T) {
			loc := mustZone(t, c.zone)
			day, err := time.Parse("2006-01-02", c.date)
			if err != nil {
				t.Fatalf("Parse(%q): %v", c.date, err)
			}
			// 15:00, an hour every one of these zones has on both sides of its transition, so
			// `now` itself is never the thing under test.
			now := time.Date(day.Year(), day.Month(), day.Day(), 15, 0, 0, 0, loc)
			start, err := time.Parse(time.RFC3339, c.start)
			if err != nil {
				t.Fatalf("Parse(%q): %v", c.start, err)
			}
			start = start.In(loc)

			spec, err := usage.ParseWindowSpec(usage.WindowToday, now)
			if err != nil {
				t.Fatalf("ParseWindowSpec: %v", err)
			}
			// The ledger's clock zone IS its day boundary (see costledger's store.loc), and in
			// production both it and ParseWindowSpec read time.Now(), so pinning them to the
			// same zone here is the production arrangement rather than a convenience.
			led, err := ledger.New(t.TempDir(), ledger.WithClock(func() time.Time { return now }))
			if err != nil {
				t.Fatalf("ledger.New: %v", err)
			}
			t.Cleanup(func() { _ = led.Close() })

			// Straddle the boundary. The two before it are the spend that must NOT be counted;
			// the two after it, and one near now, are the spend that must be.
			samples := []time.Time{
				start.Add(-time.Hour),
				start.Add(-time.Minute),
				start,
				start.Add(time.Minute),
				now.Add(-time.Minute),
			}
			for _, at := range samples {
				recordCostedMinute(t, led, at, "gw", "opus", 0.25)
			}
			if ferr := led.Flush(); ferr != nil {
				t.Fatalf("Flush: %v", ferr)
			}

			// EXACTLY the bounds the handler would use, so the assertion is about the join and
			// not about a range this test invented.
			rows, caveats, err := led.Window(context.Background(), spec.From, spec.To)
			if err != nil {
				t.Fatalf("Window: %v", err)
			}
			if !caveats.Clean() {
				t.Errorf("caveats = %+v, want clean; nothing here damages a day file", caveats)
			}

			present := map[string]bool{}
			for _, r := range rows {
				present[r.At.In(loc).Format("2006-01-02 15:04 -0700")] = true
			}
			for _, at := range samples {
				local := at.In(loc)
				key := local.Format("2006-01-02 15:04 -0700")
				// Decided by the LOCAL DATE alone. Never by spec.From, which is the thing being
				// checked: an expectation derived from it would move with a wrong bound.
				want := local.Format("2006-01-02") == c.date
				if got := present[key]; got != want {
					verb := "is missing from"
					if got {
						verb = "was counted in"
					}
					t.Errorf("spend at %s %s window=today (From = %s). Every row on %s belongs in "+
						"the total and no row on another date does; the two layers disagree about "+
						"where this day starts.",
						key, verb, spec.From.Format("2006-01-02 15:04 -0700"), c.date)
				}
			}
			if len(rows) != 3 {
				t.Errorf("window=today returned %d rows, want 3 — the three samples on %s",
					len(rows), c.date)
			}
		})
	}
}
