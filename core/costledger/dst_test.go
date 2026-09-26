package costledger

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	// EMBEDDED TZDATA, and it is the point of this file rather than a convenience.
	//
	// Every test here needs a zone whose UTC offset CHANGES, because the defect they
	// pin is a local midnight that does not exist — and a time.FixedZone has no
	// transitions, so it cannot express one at all. Real zones come from tzdata, which
	// a scratch container may not carry: time.LoadLocation would then fail, and the
	// obvious response — t.Skip — would turn the whole file into a green pass that
	// asserted nothing, on exactly the platform (CI) where nobody looks. Linking the
	// database into the test binary removes that failure mode: LoadLocation cannot
	// fail for want of files, so mustZone can treat an error as a test failure.
	_ "time/tzdata"

	"github.com/rossoctl/cortex/core/costevent"
	"github.com/rossoctl/cortex/core/pipeline"
)

// mustZone loads a real zone, FAILING rather than skipping when it cannot.
//
// t.Fatalf, deliberately. A skip here would report success for a test that never ran
// the code it exists to cover, which is the failure mode that let the local-midnight
// bug ship: the only zone in this package's tests was a fixed offset, so a green suite
// meant nothing about any zone that shifts.
func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v — this package embeds time/tzdata precisely so this "+
			"cannot happen; a failure here means the import was dropped, not that the host "+
			"lacks a zone database", name, err)
	}
	return loc
}

// dstDay is one zone's DST transition, and what the ledger must do with it.
//
// EVERY FIELD BELOW date IS A MEASURED TZDATA FACT, and
// TestMidnightTransitions_EveryFixtureIsStillTheDayItClaimsToBe asserts each one against
// the zone database linked into this binary. That is not belt-and-braces. THREE of the
// rows here named a date with no transition on it at all — Asia/Beirut 2027-03-29
// (Lebanon shifts on 2026-03-29 and 2027-03-28), Asia/Beirut 2026-10-25 and
// America/Santiago 2026-04-05 (both shift at 23:00 on the PREVIOUS date) — so those
// three subtests ran the ledger over an ordinary 24-hour day while their names, this
// table's `what` column and the commit that added them all claimed a DST transition. A
// green suite reported the transition cases as covered; a third of them were not.
//
// A fixture table that cannot detect its own irrelevance is how that survives review, so
// these fields exist to make the table check itself: the properties a row must have to be
// the day it says it is are stated as data and asserted, rather than described in prose
// that nothing reads.
type dstDay struct {
	zone string
	// date is the LOCAL calendar date of the transition, as a day file is named.
	date string
	// dayLen is how long that local date is on the wall clock: 23h for a spring-forward
	// date, 25h for an autumn-back one. NEVER 24h — a 24-hour date has no transition on
	// it, which is exactly how three of these rows stopped testing anything.
	dayLen time.Duration
	// midnights is how many distinct instants carry the wall clock 00:00 on that date:
	// 0 where the clocks jump forward AT midnight (the gap this whole file exists for),
	// 2 where an autumn transition puts the repeated hour ON midnight, 1 where midnight
	// is ordinary.
	//
	// A 25-HOUR DATE CAN STILL HAVE EXACTLY ONE MIDNIGHT, which is the fact the deleted
	// prose got wrong about two zones. Where the fall-back lands at 00:00 — Asia/Beirut,
	// America/Santiago — the repeated hour is 23:00-00:00 of the same date, so midnight
	// happens once and the ambiguity sits at the other end of the day. Only a fall-back
	// at 01:00 (America/Havana) makes midnight itself ambiguous.
	midnights int
	// naiveDate is the calendar date the PRE-FIX boundary resolved to: what
	// time.Date(y, m, d, 0, 0, 0, 0, loc) formats as, which is what dayOf used to return.
	// Different from date exactly where the old code filed a row under the wrong day,
	// equal where it happened to survive. It is what makes "regression case" versus
	// "control" a measured property of the row rather than a claim in a comment.
	naiveDate string
	// what says how this zone's transition is shaped, so a failure message names the
	// property that broke rather than only the date.
	what string
}

// midnightTransitions are the zones this package got wrong, and the shapes it has to keep
// getting right. Every date and every measured field was checked against tzdata.
//
// FOUR REAL SHAPES plus a control, chosen because they fail differently:
//
//   - America/Havana and America/Santiago SPRING FORWARD AT 00:00, so local midnight does
//     not exist on that date and time.Date normalises it BACKWARDS into the previous day —
//     which named the day file after the wrong date (naiveDate says so) and made a two-day
//     walk open the same file twice.
//   - Asia/Beirut springs forward at 00:00 too, but time.Date normalises the gap FORWARDS
//     onto 01:00 of the same date. The old boundary therefore kept the right DATE at the
//     wrong instant, an hour late — enough for a walk anchored on it to overshoot the end
//     of the window and drop the day AFTER the transition (measured: a 2026-03-29 to
//     2026-03-30 window opened 2026-03-29 only). It is also the row least likely to be
//     noticed as pointing at the wrong day, because the file name looked right.
//   - America/Havana FALLS BACK AT 01:00, which puts the repeated hour on midnight: 00:00
//     exists twice. Asia/Beirut and America/Santiago fall back at 00:00 instead, so their
//     repeated hour is 23:00-00:00 of the same date and their midnight is ordinary — the
//     25-hour day is what makes them a transition case, not an ambiguous midnight.
//   - America/New_York shifts at 02:00, where midnight exists. It is the CONTROL: it
//     passed before this fix and must keep passing, or the fix broke the ordinary case
//     to rescue the unusual one.
//
// Autumn dates are included as well as spring ones, because a 25-hour day is the case that
// must NOT regress while the 23-hour one is fixed. Asia/Beirut appears in two years for one
// reason: the original row was a year-specific off-by-one (2027-03-29 for a 2027-03-28
// transition), so both years are pinned and a tzdata change to either fails by name.
var midnightTransitions = []dstDay{
	{zone: "America/Havana", date: "2026-03-08", dayLen: 23 * time.Hour, midnights: 0, naiveDate: "2026-03-07",
		what: "spring forward at local midnight, normalising backwards"},
	{zone: "America/Havana", date: "2026-11-01", dayLen: 25 * time.Hour, midnights: 2, naiveDate: "2026-11-01",
		what: "autumn back at 01:00, so midnight occurs twice"},
	{zone: "America/Santiago", date: "2026-09-06", dayLen: 23 * time.Hour, midnights: 0, naiveDate: "2026-09-05",
		what: "spring forward at local midnight, normalising backwards"},
	{zone: "America/Santiago", date: "2026-04-04", dayLen: 25 * time.Hour, midnights: 1, naiveDate: "2026-04-04",
		what: "autumn back at local midnight, so 23:00 occurs twice and the day is 25h"},
	{zone: "Asia/Beirut", date: "2026-03-29", dayLen: 23 * time.Hour, midnights: 0, naiveDate: "2026-03-29",
		what: "spring forward at local midnight, normalising forwards onto 01:00"},
	{zone: "Asia/Beirut", date: "2027-03-28", dayLen: 23 * time.Hour, midnights: 0, naiveDate: "2027-03-28",
		what: "spring forward at local midnight a year later, on a different date"},
	{zone: "Asia/Beirut", date: "2026-10-24", dayLen: 25 * time.Hour, midnights: 1, naiveDate: "2026-10-24",
		what: "autumn back at local midnight, so 23:00 occurs twice and the day is 25h"},
	{zone: "America/New_York", date: "2026-03-08", dayLen: 23 * time.Hour, midnights: 1, naiveDate: "2026-03-08",
		what: "control: spring forward at 02:00"},
	{zone: "America/New_York", date: "2026-11-01", dayLen: 25 * time.Hour, midnights: 1, naiveDate: "2026-11-01",
		what: "control: autumn back at 02:00"},
}

// TestMidnightTransitions_EveryFixtureIsStillTheDayItClaimsToBe checks the TABLE, not the
// ledger, and it is the only test here that would have failed on the day this file was
// written.
//
// Three rows named a date with no DST transition on it. Every other test in this file ran
// them, passed, and reported coverage of a transition it never touched: the ledger handles
// an ordinary Tuesday correctly, so a fixture that has quietly become an ordinary Tuesday
// is indistinguishable from a fixture that works. Nothing in a green run says which.
//
// So the properties that make a row a DST case are asserted against tzdata here, ahead of
// the ledger assertions that depend on them:
//
//   - the local date is 23 or 25 hours long, never 24 — a 24-hour date has no transition,
//     which is the precise shape of the three dead rows;
//   - its midnight is a gap, ambiguous, or ordinary, as declared;
//   - the pre-fix boundary lands on the date the row says it does, which is what
//     distinguishes a regression case from a control.
//
// The measurements are made independently of the code under test — no dayOf, no dayNoon,
// no StartOfLocalDay — because a fixture verified with the implementation it is verifying
// would agree with any consistent mistake.
func TestMidnightTransitions_EveryFixtureIsStillTheDayItClaimsToBe(t *testing.T) {
	shapes := map[string]int{}
	for _, c := range midnightTransitions {
		t.Run(c.zone+"/"+c.date, func(t *testing.T) {
			loc := mustZone(t, c.zone)
			y, m, d := mustDate(t, c.date)

			if got := localDayLength(t, loc, y, m, d); got != c.dayLen {
				hint := ""
				if got == 24*time.Hour {
					hint = " — a 24-hour date has NO transition on it, so this row exercises an " +
						"ordinary day under a name that promises a DST one, and every other test " +
						"in this file passes it for the wrong reason"
				}
				t.Errorf("local date %s in %s is %v long, want %v (%s)%s",
					c.date, c.zone, got, c.dayLen, c.what, hint)
			}
			if got, at := midnightOccurrences(loc, y, m, d); got != c.midnights {
				t.Errorf("the wall clock %s 00:00 in %s exists at %d instants %v, want %d (%s); "+
					"0 is the DST gap this file pins, 2 is an ambiguous midnight, 1 is ordinary",
					c.date, c.zone, got, at, c.midnights, c.what)
			}
			naive := time.Date(y, m, d, 0, 0, 0, 0, loc)
			if got := naive.Format(dayLayout); got != c.naiveDate {
				t.Errorf("the pre-fix boundary time.Date(%s 00:00, %s) resolves to %s, on date %s, "+
					"want date %s (%s); this field is what says whether the row is a regression "+
					"case or a control", c.date, c.zone, naive.Format(time.RFC3339), got, c.naiveDate, c.what)
			}
		})
		if c.midnights == 0 {
			shapes["gap"]++
		}
		if c.midnights == 2 {
			shapes["fold"]++
		}
		if c.dayLen == 25*time.Hour {
			shapes["autumn"]++
		}
		if c.naiveDate != c.date {
			shapes["misdated"]++
		}
	}
	// The table AS A WHOLE has to keep covering each shape. Per-row checks cannot catch a
	// row being deleted, and the gap rows are the ones a future edit is most likely to drop
	// for being awkward — they are the only rows where the pre-fix code got the DATE wrong.
	for _, want := range []string{"gap", "fold", "autumn", "misdated"} {
		if shapes[want] == 0 {
			t.Errorf("no fixture left with shape %q: the table no longer covers it, and the "+
				"tests below will pass without exercising it", want)
		}
	}
}

// mustDate splits a fixture date into calendar fields. Parsed in UTC and used only for
// those fields, so no zone arithmetic happens here and the parse cannot itself normalise.
func mustDate(t *testing.T, date string) (int, time.Month, int) {
	t.Helper()
	d, err := time.Parse(dayLayout, date)
	if err != nil {
		t.Fatalf("Parse(%q): %v", date, err)
	}
	y, m, dd := d.Date()
	return y, m, dd
}

// wallAsUTC reads a wall clock AS IF it were UTC, which is how the two measurements below
// step along the instant axis without any zone deciding what the date means.
func wallAsUTC(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// scanWindow is the instant range every measurement below sweeps: a day either side of the
// date, which contains any transition that can affect it even in a zone that skipped a
// whole calendar date.
const scanWindow = 30 * time.Hour

// offsetsAround collects the distinct UTC offsets loc uses near a date.
//
// A MINUTE at a time. Zone offsets and transition instants in the database are
// minute-aligned for every zone in living memory, so a minute step cannot step over one,
// and 3,600 Zone() lookups per fixture is nothing.
func offsetsAround(loc *time.Location, y int, m time.Month, d int) []int {
	base := wallAsUTC(y, m, d)
	seen := map[int]bool{}
	var out []int
	for t := base.Add(-scanWindow); t.Before(base.Add(scanWindow)); t = t.Add(time.Minute) {
		if _, off := t.In(loc).Zone(); !seen[off] {
			seen[off] = true
			out = append(out, off)
		}
	}
	return out
}

// midnightOccurrences counts the distinct instants whose wall clock in loc is that date at
// 00:00:00, and names them: 0 means the midnight does not exist (a DST gap), 1 ordinary, 2
// ambiguous.
//
// Candidate-and-verify rather than a scan, because it has to be exact: for each offset the
// zone uses nearby, the instant that WOULD show that wall clock at that offset is computed
// and then asked what offset it actually has. Only an instant that agrees with itself
// counts, which is precisely the definition of "this wall clock happened".
func midnightOccurrences(loc *time.Location, y int, m time.Month, d int) (int, []string) {
	wall := wallAsUTC(y, m, d)
	seen := map[int64]bool{}
	var at []string
	for _, off := range offsetsAround(loc, y, m, d) {
		cand := wall.Add(-time.Duration(off) * time.Second)
		if _, got := cand.In(loc).Zone(); got != off || seen[cand.Unix()] {
			continue
		}
		seen[cand.Unix()] = true
		at = append(at, cand.In(loc).Format(time.RFC3339))
	}
	return len(at), at
}

// localDayLength measures how long a local date lasts on the wall clock — 23h on a
// spring-forward date, 25h on an autumn-back one, 24h on a date with no transition.
//
// Found by sweeping the instant axis and asking each minute which local date it is on,
// which needs no notion of when a day "starts" and so cannot inherit the local-midnight
// mistake it is here to detect. Returns 0 for a date the zone skipped entirely
// (Pacific/Apia 2011-12-30), which is a shape no fixture here declares.
func localDayLength(t *testing.T, loc *time.Location, y int, m time.Month, d int) time.Duration {
	t.Helper()
	base := wallAsUTC(y, m, d)
	want := time.Date(y, m, d, 12, 0, 0, 0, time.UTC).Format(dayLayout)
	var first, last time.Time
	for i := base.Add(-scanWindow); i.Before(base.Add(scanWindow)); i = i.Add(time.Minute) {
		if i.In(loc).Format(dayLayout) != want {
			continue
		}
		if first.IsZero() {
			first = i
		}
		last = i
	}
	if first.IsZero() {
		return 0
	}
	return last.Sub(first) + time.Minute
}

// localNoonish is an instant on the given local date, at an hour every zone has.
//
// 09:30 rather than 00:30: the point of these tests is the DAY a row belongs to, and an
// instant inside the transition hour would be testing time.Date's normalisation of the
// input as well as the ledger's handling of it. A workload spending money mid-morning
// is also the ordinary case, which is what makes the resulting loss ordinary too.
func localNoonish(t *testing.T, loc *time.Location, date string) time.Time {
	t.Helper()
	d, err := time.Parse(dayLayout, date)
	if err != nil {
		t.Fatalf("Parse(%q): %v", date, err)
	}
	return time.Date(d.Year(), d.Month(), d.Day(), 9, 30, 0, 0, loc)
}

// costedEventAt is costedEvent with the instant, host and cost chosen by the caller,
// so a test can put spend on a specific local day in a specific zone.
func costedEventAt(t *testing.T, when time.Time, model string, costUSD float64) *pipeline.SessionEvent {
	t.Helper()
	rec, err := json.Marshal(costevent.Event{
		CostUSD: costUSD, Settled: true,
		Source: costevent.SourceUsageFallback, Provenance: "bundled",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return &pipeline.SessionEvent{
		At: when, Phase: pipeline.SessionResponse, StatusCode: 200, Host: "gw",
		Inference: &pipeline.InferenceExtension{
			Model: model, InputTokens: 100, OutputTokens: 50,
			TotalTokens: 150, PresentKinds: 0b1001,
		},
		Plugins: map[string]json.RawMessage{costevent.Key: rec},
	}
}

// TestDayOf_NamesTheCalendarDayEvenWhereLocalMidnightDoesNotExist is the unit-level
// statement of the defect: the ledger day of an instant is the date that instant is
// ON, in every zone.
//
// dayOf used to be local midnight, and time.Date normalises a midnight inside a DST
// gap into the neighbouring day — so on a Havana laptop the ledger day of 2026-03-08
// 09:30 was 2026-03-07. Everything else in this file is a consequence of that one
// mapping: the file a row is written to, the files a walk opens, and the date prune
// reads back off a name.
func TestDayOf_NamesTheCalendarDayEvenWhereLocalMidnightDoesNotExist(t *testing.T) {
	for _, c := range midnightTransitions {
		t.Run(c.zone+"/"+c.date, func(t *testing.T) {
			loc := mustZone(t, c.zone)
			when := localNoonish(t, loc, c.date)
			if got := dayOf(when).Format(dayLayout); got != c.date {
				t.Errorf("dayOf(%s).Format = %s, want %s (%s); the ledger day of an instant "+
					"must be the date that instant is on, or its row is filed under a day no "+
					"query for that day ever visits", when.Format(time.RFC3339), got, c.date, c.what)
			}
			// The zone must actually be the one asked for. A zone that silently resolved to
			// UTC would make every assertion here vacuous.
			if when.Location() != loc {
				t.Fatalf("instant is in %s, want %s", when.Location(), loc)
			}
		})
	}
}

// TestStore_ARowIsWrittenToAndReadFromTheSameDayFileInEveryZone pins the WRITE and
// READ halves against each other, which is the property that decides whether money
// recorded on a day can be reported for that day.
//
// store.path names the file from dayOf and readDay resolves its argument through the
// same function, so the two agreed even while both were wrong — the row was written to
// the previous day's file and read back from it. What did NOT agree is the DATE a
// caller asks about: a query for 2026-03-08 walks to the day whose name is 2026-03-08,
// and on a Havana laptop that file never existed.
func TestStore_ARowIsWrittenToAndReadFromTheSameDayFileInEveryZone(t *testing.T) {
	for _, c := range midnightTransitions {
		t.Run(c.zone+"/"+c.date, func(t *testing.T) {
			loc := mustZone(t, c.zone)
			when := localNoonish(t, loc, c.date)
			dir := t.TempDir()
			s, err := newStore(dir, 30, loc)
			if err != nil {
				t.Fatalf("newStore: %v", err)
			}
			want := filepath.Join(dir, c.date+".jsonl")
			if got := s.path(when); got != want {
				t.Errorf("path(%s) = %s, want %s (%s)", when.Format(time.RFC3339), got, want, c.what)
			}
			if _, aerr := s.append([]Row{{At: when.Truncate(time.Minute)}}); aerr != nil {
				t.Fatalf("append: %v", aerr)
			}
			if _, serr := os.Stat(want); serr != nil {
				t.Errorf("no day file named for the row's own date: %v; a reader asking for %s "+
					"opens that name and finds nothing", serr, c.date)
			}
			rows, issues, rerr := s.readDay(dayOf(when))
			if rerr != nil {
				t.Fatalf("readDay: %v", rerr)
			}
			if issues != (dayIssues{}) {
				t.Errorf("readDay reported %+v, want a clean read", issues)
			}
			if len(rows) != 1 {
				t.Errorf("readDay returned %d rows, want 1", len(rows))
			}
		})
	}
}

// TestQuery_TheDayWalkOpensEachDayExactlyOnceAcrossADSTMidnight is the money
// assertion: two days of spend must read back as two days of spend.
//
// The walk was `for d := dayOf(from); !d.After(dayOf(to)); d = d.AddDate(0,0,1)` over
// local midnights, and a midnight inside a DST gap breaks it at BOTH ENDS of the window.
// Measured by reimplementing that expression against tzdata, over a two-day window on
// each side of the transition:
//
//	Havana, 2026-03-07 → 2026-03-08:  opens 2026-03-07 TWICE and 2026-03-08 never — the
//	                                  first day counted double, the transition day absent
//	Havana, 2026-03-08 → 2026-03-09:  opens 2026-03-07 (a date OUTSIDE the window) then
//	                                  2026-03-08, and steps past 2026-03-09 entirely
//	Beirut, 2026-03-29 → 2026-03-30:  opens 2026-03-29 ONLY. Beirut's gap normalises
//	                                  forwards, so the anchor is an hour late rather than
//	                                  a day early, and the walk overshoots the end
//
// BOTH WINDOW SHAPES ARE EXERCISED because they fail differently and only one of them was
// covered. Every case here used to put the transition on the window's LAST day, which is
// the double-count; the "steps past the end" half — the one this doc has always claimed,
// and the one that loses money silently rather than inventing it — went untested, and in
// Asia/Beirut it is the ONLY shape that fails at all. A fixture that cannot fail in the
// direction its own comment describes is the defect this file keeps finding.
//
// Every one of these is a wrong dollar figure with nothing in the response saying so,
// which is the one outcome this package treats as worse than an error.
func TestQuery_TheDayWalkOpensEachDayExactlyOnceAcrossADSTMidnight(t *testing.T) {
	for _, shape := range []struct {
		name string
		// step is where the window's OTHER day sits relative to the transition day.
		step int
	}{
		{name: "transition day last", step: -1},
		{name: "transition day first", step: +1},
	} {
		for _, c := range midnightTransitions {
			t.Run(shape.name+"/"+c.zone+"/"+c.date, func(t *testing.T) {
				loc := mustZone(t, c.zone)
				transition := localNoonish(t, loc, c.date)
				// AddDate on a mid-morning instant, not on a day boundary: 09:30 exists on every
				// date in this table, so this step cannot itself normalise and the fixture is not
				// quietly testing time.Date instead of the ledger.
				first, last := transition, transition.AddDate(0, 0, shape.step)
				if shape.step < 0 {
					first, last = last, first
				}
				dir := t.TempDir()
				// The clock's zone is the ledger's day boundary; see New. It is the LATER day so
				// neither day file is outside the writer's own retention.
				w := newTestWriter(t, dir, func() time.Time { return last })
				w.Record("s", costedEventAt(t, first, "m", 0.25))
				w.Record("s", costedEventAt(t, last, "m", 0.25))
				if ferr := w.Flush(); ferr != nil {
					t.Fatalf("Flush: %v", ferr)
				}

				rows, _, err := w.Window(context.Background(), first.Add(-time.Hour), last.Add(time.Hour))
				if err != nil {
					t.Fatalf("Window: %v", err)
				}
				var micros int64
				seen := map[string]int{}
				for _, r := range rows {
					micros += r.CostMicros
					seen[r.At.In(loc).Format(dayLayout)]++
				}
				if micros != 500_000 {
					t.Errorf("two days of $0.25 read back as %d micros, want 500000 (%s); rows=%d by day=%v",
						micros, c.what, len(rows), seen)
				}
				for _, d := range []string{first.Format(dayLayout), last.Format(dayLayout)} {
					if seen[d] != 1 {
						t.Errorf("day %s appears %d times in the answer, want exactly 1 (%s); by day=%v",
							d, seen[d], c.what, seen)
					}
				}
			})
		}
	}
}

// TestStore_DayFromNameReadsTheDateTheNameSpells is prune's half of the same mapping.
//
// prune parsed a file name with time.ParseInLocation, which resolves "2026-03-08" to
// that day's local midnight — the instant that does not exist in Havana, normalised
// back into 2026-03-07. Every comparison prune makes is against a day derived from
// dayOf, so one skewed date on one side of `Before` decides whether a file lives.
func TestStore_DayFromNameReadsTheDateTheNameSpells(t *testing.T) {
	for _, c := range midnightTransitions {
		t.Run(c.zone+"/"+c.date, func(t *testing.T) {
			loc := mustZone(t, c.zone)
			s, err := newStore(t.TempDir(), 30, loc)
			if err != nil {
				t.Fatalf("newStore: %v", err)
			}
			day, ok := s.dayFromName(c.date + ".jsonl")
			if !ok {
				t.Fatalf("dayFromName(%q) refused a name this package writes", c.date+".jsonl")
			}
			if got := day.Format(dayLayout); got != c.date {
				t.Errorf("dayFromName(%q) = %s, want %s (%s)", c.date+".jsonl", got, c.date, c.what)
			}
			// And it must be the SAME representation dayOf produces, since prune compares the
			// two directly. Equal instants, not merely equal dates.
			if want := dayOf(localNoonish(t, loc, c.date)); !day.Equal(want) {
				t.Errorf("dayFromName(%q) = %s, want %s — prune compares this against a "+
					"dayOf-derived cutoff, so a different instant for the same date decides "+
					"a deletion by hours", c.date+".jsonl", day.Format(time.RFC3339), want.Format(time.RFC3339))
			}
		})
	}
}

// TestPrune_ADayFileNamedOnADSTMidnightIsKeptWhenRetentionCoversIt is the deletion the
// name-parsing skew causes, priced in the only currency that matters here: a day file
// inside the retention window, unlinked.
//
// retainDays 2 keeps today and yesterday. Yesterday is the transition day, whose name
// parsed thirteen hours early — putting it before a cutoff derived from today's ledger
// day, so retention deleted a day of spend it was configured to keep.
func TestPrune_ADayFileNamedOnADSTMidnightIsKeptWhenRetentionCoversIt(t *testing.T) {
	for _, c := range midnightTransitions {
		t.Run(c.zone+"/"+c.date, func(t *testing.T) {
			loc := mustZone(t, c.zone)
			yesterday := localNoonish(t, loc, c.date)
			today := yesterday.AddDate(0, 0, 1)
			// Three days: today, the transition day, and one that retention must still take.
			// The third is what proves the test would notice a prune that deleted nothing.
			old := yesterday.AddDate(0, 0, -2)
			dir := t.TempDir()
			s, err := newStore(dir, 2, loc)
			if err != nil {
				t.Fatalf("newStore: %v", err)
			}
			for _, d := range []time.Time{old, yesterday, today} {
				if _, aerr := s.append([]Row{{At: d.Truncate(time.Minute)}}); aerr != nil {
					t.Fatalf("append: %v", aerr)
				}
			}
			if perr := s.prune(today); perr != nil {
				t.Fatalf("prune: %v", perr)
			}
			exists := func(when time.Time) bool {
				_, serr := os.Stat(s.path(when))
				return serr == nil
			}
			if !exists(today) {
				t.Errorf("today's day file was deleted (%s)", c.what)
			}
			if !exists(yesterday) {
				t.Errorf("the day file for %s was deleted though retainDays=2 covers it (%s); "+
					"its rows are a day of spend and the file is the only copy", c.date, c.what)
			}
			if exists(old) {
				t.Errorf("the day file for %s survived retainDays=2 (%s); retention did not run",
					old.Format(dayLayout), c.what)
			}
		})
	}
}
