package usage

import (
	"testing"
	"time"
)

// monthZones are the zones the month boundary is swept in.
//
// America/Asuncion leads because it is the only one in the set that springs forward AT 00:00
// ON THE FIRST OF A MONTH, which is the shape a month bound can get wrong that a day bound
// cannot — see TestStartOfLocalMonth_TheNaiveFirstOfMonthExpressionIsStillWrong. The rest are
// dayStartCases' zones, carried over so the month bound is exercised against every transition
// shape that table was built to cover, plus Pacific/Apia for the skipped-calendar-date case
// and UTC as the control that never shifts.
var monthZones = []string{
	"America/Asuncion",
	"America/Havana",
	"America/Santiago",
	"Asia/Beirut",
	"America/New_York",
	"Pacific/Apia",
	"Australia/Lord_Howe",
	"UTC",
}

// TestStartOfLocalMonth_IsTheFirstInstantInTheMonthInEveryZone is the contract: the lower
// bound of "this month" is the earliest instant that EXISTS in that local month.
//
// A SWEEP rather than hand-picked fixtures, because the failure this guards is a
// zone-and-month coincidence and nobody can enumerate those by hand. Every month of a
// twenty-one year span in every zone above is about two thousand cases, and the two that
// actually matter (Asuncion 2017-10 and 2023-10) were found by scanning rather than by
// knowing. A future tzdata release adding another is covered the day it ships.
//
// Two assertions per case, and they are the two halves of "earliest instant in the month":
// the bound is IN the month, and the instant before it is NOT. Either one alone passes on a
// bound that is off by a month in the harmless-looking direction.
func TestStartOfLocalMonth_IsTheFirstInstantInTheMonthInEveryZone(t *testing.T) {
	for _, zone := range monthZones {
		loc := mustZone(t, zone)
		if got := loc.String(); got != zone {
			t.Fatalf("LoadLocation(%q) returned zone %q — every case in this zone would be vacuous",
				zone, got)
		}
		for year := 2015; year <= 2035; year++ {
			for month := 1; month <= 12; month++ {
				// Mid-month at midday: an instant every zone has, so `now` is never the thing
				// under test. The bound must be derived from the month, not from the hour asked.
				now := time.Date(year, time.Month(month), 15, 12, 0, 0, 0, loc)
				got := StartOfLocalMonth(now)

				gotYear, gotMonth, _ := got.Date()
				if gotYear != year || int(gotMonth) != month {
					t.Errorf("%s %04d-%02d: StartOfLocalMonth = %v, which is in %04d-%02d — a bound "+
						"in another month pulls that month's spend into this one",
						zone, year, month, got, gotYear, int(gotMonth))
					continue
				}
				// One second earlier must be in the PREVIOUS month, or this is not the earliest
				// instant and spend before it would be missing from the month with nothing saying so.
				beforeYear, beforeMonth, _ := got.Add(-time.Second).Date()
				if beforeYear == year && int(beforeMonth) == month {
					t.Errorf("%s %04d-%02d: the second before %v is still in the same month, so it "+
						"is not the EARLIEST instant in it", zone, year, month, got)
				}
			}
		}
	}
}

// TestStartOfLocalMonth_TheNaiveFirstOfMonthExpressionIsStillWrong is the NEGATIVE CONTROL.
//
// Without it every assertion above would keep passing if the zone table were replaced with
// UTC, or if tzdata stopped shipping these transitions — the correct implementation and the
// naive one agree in any zone that does not shift at 00:00 on a first of the month.
//
// THE DEFECT IS THE SAME SHAPE AS StartOfLocalDay's, one unit up. Paraguay moved its clocks
// forward at 00:00 on 1 October in 2017 and again in 2023, so local midnight on those dates
// does not exist and time.Date resolves it BACKWARDS — to 23:00 on 30 September, an hour
// before the previous month had even ended. A month-to-date total built on that bound folds
// the last hour of September into October, silently: no error, no caveat, just a figure that
// is too large and a budget that looks closer to its limit than it is.
//
// If a future tzdata drops these transitions this fails loudly and says the fixtures no
// longer exercise the defect, rather than passing for the wrong reason.
func TestStartOfLocalMonth_TheNaiveFirstOfMonthExpressionIsStillWrong(t *testing.T) {
	loc := mustZone(t, "America/Asuncion")
	for _, year := range []int{2017, 2023} {
		naive := time.Date(year, time.October, 1, 0, 0, 0, 0, loc)
		if naive.Month() == time.October {
			t.Fatalf("%d: time.Date(1 Oct 00:00, Asuncion) = %v, which IS in October — this "+
				"fixture no longer exercises the defect and the test above is now vacuous "+
				"for want of a zone that shifts at 00:00 on a first of the month", year, naive)
		}
		// And the real bound is not the naive one: it is the first instant that exists.
		got := StartOfLocalMonth(time.Date(year, time.October, 15, 12, 0, 0, 0, loc))
		if got.Equal(naive) {
			t.Errorf("%d: StartOfLocalMonth returned the naive bound %v", year, got)
		}
		if h := got.Hour(); h != 1 {
			t.Errorf("%d: StartOfLocalMonth = %v, want 01:00 — midnight does not exist on this "+
				"date, so the first instant is an hour later", year, got)
		}
	}
}

// TestStartOfLocalMonth_IsStartOfLocalDayOnTheFirst pins the two boundaries to ONE definition.
//
// The month bound is not a second sweep; it is the day bound asked about the first of the
// month. Stated as a test because it is the reason StartOfLocalMonth is four lines rather than
// forty: a boundary derived twice is a boundary that drifts, which is the lesson
// StartOfLocalDay's own doc records about the copy sessionapi used to keep.
//
// AND IT IS AN IDENTITY CHECK, NOT ZONE COVERAGE — worth saying, because the loop below looks
// like eight zones' worth of evidence and is not. `want` is StartOfLocalMonth's own body written
// out, so every one of these iterations compares the implementation with itself and they cannot
// disagree while the delegation stands. What the sweep is FOR is the case where it stops standing:
// a reimplementation that is only wrong in some zones or some months fails here rather than
// passing a single hand-picked date.
//
// The zone-specific power in this file is elsewhere and should be counted there:
// TestStartOfLocalMonth_FindsTheFirstInstantThatExists derives its expectation from the returned
// value rather than from the expression, and the Asunción case is the negative control that fails
// the naive first-of-month expression.
func TestStartOfLocalMonth_IsStartOfLocalDayOnTheFirst(t *testing.T) {
	for _, zone := range monthZones {
		loc := mustZone(t, zone)
		for year := 2015; year <= 2035; year++ {
			for month := 1; month <= 12; month++ {
				now := time.Date(year, time.Month(month), 15, 12, 0, 0, 0, loc)
				got := StartOfLocalMonth(now)
				// The first of the month, anchored at noon for the reason dayAnchorHour exists:
				// no transition can move midday onto another date.
				want := StartOfLocalDay(time.Date(year, time.Month(month), 1, dayAnchorHour, 0, 0, 0, loc))
				if !got.Equal(want) {
					t.Errorf("%s %04d-%02d: StartOfLocalMonth = %v, StartOfLocalDay of the 1st = %v",
						zone, year, month, got, want)
				}
			}
		}
	}
}

// TestParseWindowSpec_MonthIsACalendarBoundary: "month" is month-to-date, not a rolling
// thirty days.
//
// A BOUNDARY, like "today" and unlike "7d", and that is the whole reason it needs a symbolic
// spelling rather than a duration. A budget resets on the first of the month, so "how much of
// this month's budget is gone" cannot be answered by any fixed length — and ParseWindow would
// refuse a thirty-day duration anyway, since it exceeds the ring's MaxWindow.
func TestParseWindowSpec_MonthIsACalendarBoundary(t *testing.T) {
	loc := mustZone(t, "America/New_York")
	now := time.Date(2026, time.September, 20, 15, 30, 0, 0, loc)

	spec, err := ParseWindowSpec(WindowMonth, now)
	if err != nil {
		t.Fatalf("ParseWindowSpec(%q): %v", WindowMonth, err)
	}
	if !spec.Symbolic() {
		t.Errorf("spec.Symbolic() = false — a calendar boundary cannot be served from the ring, "+
			"so it must carry From/To rather than a duration: %+v", spec)
	}
	if spec.Label != WindowMonth {
		t.Errorf("spec.Label = %q, want %q — the response echoes this so a client learns which "+
			"window it actually got", spec.Label, WindowMonth)
	}
	if want := StartOfLocalMonth(now); !spec.From.Equal(want) {
		t.Errorf("spec.From = %v, want %v", spec.From, want)
	}
	if !spec.To.Equal(now) {
		t.Errorf("spec.To = %v, want %v", spec.To, now)
	}
	// Month-to-date, NOT a whole month: twenty days in, the span is twenty days and not thirty-one.
	if span := spec.To.Sub(spec.From); span < 19*24*time.Hour || span > 20*24*time.Hour {
		t.Errorf("span = %v on the 20th of the month, want about 19 days — a bound that produced a "+
			"whole month would report spend that has not happened yet", span)
	}
}

// TestParseWindowSpec_MonthIsNotADuration guards the spelling itself: "month" must reach the
// symbolic branch, never ParseWindow. A duration parse would either refuse it or, worse,
// accept some future spelling as a length and serve a rolling window under a calendar label.
func TestParseWindowSpec_MonthIsNotADuration(t *testing.T) {
	if _, err := ParseWindow(WindowMonth); err == nil {
		t.Errorf("ParseWindow(%q) succeeded — %q must be handled as a boundary before any "+
			"duration parse sees it", WindowMonth, WindowMonth)
	}
	// And a caller spelling a month as a length is refused rather than silently served short:
	// the ring keeps six hours, so there is no honest duration answer here.
	if _, err := ParseWindow("720h"); err == nil {
		t.Error("ParseWindow(720h) succeeded — a month as a duration exceeds the ring's MaxWindow " +
			"and must be refused, not served from a shorter window")
	}
}

// WindowMonthLocalDays IS COUNTED, not asserted against another constant.
//
// Its only pin was cost/ledger's TestDefaultRetention_CoversEveryDayTheMonthWindowTouches, which
// compares it with defaultRetentionDays — so setting BOTH to 30 left all three modules green, and
// the 30-against-31 defect this window exists to fix would have come back unnoticed. Two constants
// that must agree cannot check each other; that is the lesson Window7dLocalDays already records,
// and it is derived from its own span for exactly that reason.
//
// A month's length is not expressible as a compile-time expression the way 7x24h is, so the
// constant stays a literal and the CALENDAR checks it: walk every month of a twenty-one year span
// and count the distinct local dates a month-to-date window covers at the last instant of each.
// The maximum over all of them is what the constant has to be — no more, because the ledger's
// default retention is derived from it and history nobody needs is an operator's choice.
func TestWindowMonthLocalDays_IsTheLongestMonthsDateCount(t *testing.T) {
	for _, zone := range monthZones {
		loc := mustZone(t, zone)
		worst, when := 0, ""
		for year := 2015; year <= 2035; year++ {
			for month := 1; month <= 12; month++ {
				// The last instant of the month, which is when month-to-date is widest.
				end := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, loc).
					AddDate(0, 1, 0).Add(-time.Second)
				from := StartOfLocalMonth(end)

				dates := map[string]bool{}
				for d := from; !d.After(end); d = d.Add(time.Hour) {
					dates[d.Format("2006-01-02")] = true
				}
				if len(dates) > worst {
					worst, when = len(dates), end.Format("2006-01")
				}
			}
		}
		if worst != WindowMonthLocalDays {
			t.Errorf("%s: the widest month-to-date window covers %d local dates (%s), but "+
				"WindowMonthLocalDays is %d — cost/ledger derives its default retention from this "+
				"constant, so a value below the count answers window=month from too few day files "+
				"and a value above it keeps history nothing asked for",
				zone, worst, when, WindowMonthLocalDays)
		}
	}
}
