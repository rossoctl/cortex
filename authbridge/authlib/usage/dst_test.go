package usage

import (
	"testing"
	"time"

	// EMBEDDED TZDATA, and it is the point of this file rather than a convenience.
	//
	// Every test here needs a zone whose UTC offset CHANGES, because the defect they pin is
	// a local midnight that does not exist — and a time.FixedZone has no transitions, so it
	// cannot express one at all. Real zones come from tzdata, which a scratch container may
	// not carry: time.LoadLocation would then fail, and the obvious response — t.Skip —
	// would turn the whole file into a green pass that asserted nothing, on exactly the
	// platform (CI) where nobody looks. Linking the database into the test binary removes
	// that failure mode, so mustZone can treat an error as a test failure.
	//
	// The same import for the same reason is in costledger's dst_test.go. That package
	// shipped this bug under a commit titled "Pin a non-UTC zone, so the local-midnight
	// guard actually guards" — the zone it pinned was a FixedZone.
	_ "time/tzdata"
)

// mustZone loads a real zone, FAILING rather than skipping when it cannot.
//
// t.Fatalf, deliberately. A skip here would report success for a test that never ran the
// code it exists to cover, which is the failure mode that let the local-midnight bug ship
// one layer down.
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

// dayStartCase is one zone-and-date and the instant its local day actually begins at.
type dayStartCase struct {
	zone string
	// date is the LOCAL calendar date, as a ledger day file is named.
	date string
	// wantStart is the EARLIEST INSTANT THAT EXISTS on that local date, in RFC3339 so the
	// offset is part of the assertion and a right-wall-clock-wrong-instant answer cannot
	// pass. Every value here was MEASURED against tzdata, not derived from the code under
	// test.
	wantStart string
	// wantSpan is what ParseWindowSpec("today") must report at 15:00 local on that date.
	wantSpan time.Duration
	// badSpan is what the OLD local-midnight bound reported for the same instant. Equal to
	// wantSpan where the old bound happened to be right; naming it either way lets a
	// failure message say whether the defect returned or something new broke.
	badSpan time.Duration
	// what says how the day is shaped, so a failure names the property rather than a date.
	what string
}

// dayStartCases are the shapes a day boundary can take, with a control for each.
//
// FOUR SHAPES, chosen because they fail differently:
//
//   - America/Havana and America/Santiago shift AT 00:00 in spring, so local midnight does
//     not exist and time.Date resolves it BACKWARDS onto the previous date. This is the
//     defect: From landed an hour before the previous day had even ended, so window=today
//     folded yesterday evening's spend into today's total.
//   - Asia/Beirut shifts at 00:00 too but normalises the other way, onto 01:00 on the
//     RIGHT date. Its old bound was accidentally correct, which is the case that must not
//     regress while the other two are fixed.
//   - America/Havana in AUTUMN is the ambiguous one: the clock goes back THROUGH midnight,
//     so 00:00 occurs twice, an hour apart, and both instants are on the date. The earlier
//     is required — see StartOfLocalDay.
//   - America/Santiago and Asia/Beirut in autumn go back to 23:00 on the PREVIOUS date, so
//     their midnight occurs exactly once. Included to show the autumn cases are not all
//     one shape.
//   - America/New_York shifts at 02:00 and UTC never shifts. They are the CONTROLS: both
//     passed before this fix and must keep passing, or the fix broke the ordinary case to
//     rescue the unusual one.
var dayStartCases = []dayStartCase{
	{
		zone: "America/Havana", date: "2026-03-08",
		wantStart: "2026-03-08T01:00:00-04:00", wantSpan: 14 * time.Hour, badSpan: 15 * time.Hour,
		what: "spring forward AT 00:00; midnight does not exist, time.Date normalises backwards",
	},
	{
		zone: "America/Santiago", date: "2026-09-06",
		wantStart: "2026-09-06T01:00:00-03:00", wantSpan: 14 * time.Hour, badSpan: 15 * time.Hour,
		what: "spring forward AT 00:00; midnight does not exist, time.Date normalises backwards",
	},
	{
		zone: "Asia/Beirut", date: "2026-03-29",
		wantStart: "2026-03-29T01:00:00+03:00", wantSpan: 14 * time.Hour, badSpan: 14 * time.Hour,
		what: "spring forward AT 00:00; midnight does not exist but normalises onto the right date",
	},
	{
		zone: "America/Havana", date: "2026-11-01",
		wantStart: "2026-11-01T00:00:00-04:00", wantSpan: 16 * time.Hour, badSpan: 16 * time.Hour,
		what: "autumn back THROUGH midnight; 00:00 occurs twice and the earlier one is the bound",
	},
	{
		zone: "America/Santiago", date: "2026-04-05",
		wantStart: "2026-04-05T00:00:00-04:00", wantSpan: 15 * time.Hour, badSpan: 15 * time.Hour,
		what: "autumn back to 23:00 on the previous date; midnight occurs once",
	},
	{
		zone: "Asia/Beirut", date: "2026-10-25",
		wantStart: "2026-10-25T00:00:00+02:00", wantSpan: 15 * time.Hour, badSpan: 15 * time.Hour,
		what: "autumn back to 23:00 on the previous date; midnight occurs once",
	},
	{
		zone: "America/New_York", date: "2026-03-08",
		wantStart: "2026-03-08T00:00:00-05:00", wantSpan: 14 * time.Hour, badSpan: 14 * time.Hour,
		what: "control: spring forward at 02:00, so midnight exists",
	},
	{
		zone: "America/New_York", date: "2026-11-01",
		wantStart: "2026-11-01T00:00:00-04:00", wantSpan: 16 * time.Hour, badSpan: 16 * time.Hour,
		what: "control: autumn back at 02:00, so midnight occurs once",
	},
	{
		zone: "UTC", date: "2026-03-08",
		wantStart: "2026-03-08T00:00:00Z", wantSpan: 15 * time.Hour, badSpan: 15 * time.Hour,
		what: "control: no transitions at all",
	},
}

// mustInstant parses an RFC3339 instant, resolved into loc so a comparison against a
// zone-carrying result is an instant comparison and not a formatting one.
func mustInstant(t *testing.T, s string, loc *time.Location) time.Time {
	t.Helper()
	x, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return x.In(loc)
}

// afternoonOn is 15:00 on a local date — an hour every one of these zones has, on both
// sides of every transition in the table, so `now` itself is never the thing under test.
func afternoonOn(t *testing.T, loc *time.Location, date string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", date)
	if err != nil {
		t.Fatalf("Parse(%q): %v", date, err)
	}
	return time.Date(d.Year(), d.Month(), d.Day(), 15, 0, 0, 0, loc)
}

// TestStartOfLocalDay_IsTheFirstInstantOnTheDateInEveryZone is the unit-level statement of
// the contract: the lower bound of a local day is the earliest instant that EXISTS on that
// date.
//
// Three assertions, and all three are needed. The instant, because a bound on the wrong
// date is the defect. That it is ON the date, because that is the property a client reads.
// That the second BEFORE it is not, because that is what makes it the EARLIEST rather than
// merely a plausible instant somewhere inside the day — an implementation that returned
// noon would satisfy the first two readings of "on the date" and lose half a day of spend.
func TestStartOfLocalDay_IsTheFirstInstantOnTheDateInEveryZone(t *testing.T) {
	for _, c := range dayStartCases {
		t.Run(c.zone+"/"+c.date, func(t *testing.T) {
			loc := mustZone(t, c.zone)
			// The zone must be the one asked for. A name that silently resolved to UTC would
			// make every case below vacuous, which is how this bug shipped one layer down.
			if got := loc.String(); got != c.zone {
				t.Fatalf("LoadLocation(%q) returned zone %q", c.zone, got)
			}
			want := mustInstant(t, c.wantStart, loc)
			got := StartOfLocalDay(afternoonOn(t, loc, c.date))

			if !got.Equal(want) {
				t.Errorf("StartOfLocalDay = %v, want %v\n  zone %s: %s",
					got, want, c.zone, c.what)
			}
			if d := got.Format("2006-01-02"); d != c.date {
				t.Errorf("StartOfLocalDay landed on date %s, want %s — a bound on another "+
					"date pulls that date's spend into this one (%s)", d, c.date, c.what)
			}
			if d := got.Add(-time.Second).Format("2006-01-02"); d == c.date {
				t.Errorf("the second before StartOfLocalDay is still on %s, so %v is not the "+
					"EARLIEST instant on it — spend before it would be missing from the total",
					c.date, got)
			}
		})
	}
}

// TestStartOfLocalDay_TheNaiveMidnightExpressionIsStillWrong is the NEGATIVE CONTROL, and
// it is what stops this file decaying into a green pass that asserts nothing.
//
// Every other test here would keep passing if someone replaced the zone table with UTC, or
// if tzdata quietly stopped shipping these transitions: the fix and the defect agree in a
// zone that never shifts. This test asserts the fixtures ACTUALLY EXERCISE the defect — that
// time.Date(y, m, d, 0, 0, 0, 0, loc), the expression ParseWindowSpec used to use, still
// produces the wrong date on the two spring cases. If tzdata ever drops those transitions
// this fails loudly and says so, rather than passing for the wrong reason.
func TestStartOfLocalDay_TheNaiveMidnightExpressionIsStillWrong(t *testing.T) {
	// The zones and dates where local midnight resolves onto the PREVIOUS date. MEASURED:
	// Havana gives 2026-03-07 23:00 -0500, Santiago gives 2026-09-05 23:00 -0400.
	for _, c := range []struct{ zone, date string }{
		{"America/Havana", "2026-03-08"},
		{"America/Santiago", "2026-09-06"},
	} {
		t.Run(c.zone+"/"+c.date, func(t *testing.T) {
			loc := mustZone(t, c.zone)
			now := afternoonOn(t, loc, c.date)
			y, m, d := now.Date()
			naive := time.Date(y, m, d, 0, 0, 0, 0, loc)
			if naive.Format("2006-01-02") == c.date {
				t.Fatalf("local midnight of %s in %s resolved onto its own date (%v), so this "+
					"fixture no longer exercises the defect and every DST assertion in this "+
					"file is now vacuous for it", c.date, c.zone, naive)
			}
			if !naive.Before(StartOfLocalDay(now)) {
				t.Errorf("naive midnight %v is not earlier than the real day start %v; the "+
					"defect was that the bound reached BACKWARDS out of the day",
					naive, StartOfLocalDay(now))
			}
		})
	}
}

// TestStartOfLocalDay_PicksTheEarlierOfTwoMidnights pins the AUTUMN-BACK decision.
//
// In America/Havana on 2026-11-01 the clock goes back THROUGH midnight — 00:59:59 -0400 is
// followed by 00:00:00 -0500 — so the wall time 00:00 occurs at TWO instants an hour apart
// and both are on 2026-11-01. "Local midnight" does not name a bound there, so a choice has
// to be made and stated.
//
// THE EARLIER INSTANT IS CHOSEN. Taking the later one would put the first hour of the day
// outside window=today: any spend in it would be absent from the total, with no error and
// no caveat — the same silent shortfall as the spring case, in the other direction.
//
// The test proves the ambiguity is real before asserting which side was taken, so it cannot
// pass by accident on a day whose midnight is unique.
func TestStartOfLocalDay_PicksTheEarlierOfTwoMidnights(t *testing.T) {
	loc := mustZone(t, "America/Havana")
	const date = "2026-11-01"
	earlier := mustInstant(t, "2026-11-01T00:00:00-04:00", loc)
	later := mustInstant(t, "2026-11-01T00:00:00-05:00", loc)

	// The premise: two distinct instants, both reading 00:00 on the same local date.
	if !later.After(earlier) {
		t.Fatalf("fixture is not two instants: %v, %v", earlier, later)
	}
	for _, x := range []time.Time{earlier, later} {
		if got := x.Format("2006-01-02 15:04:05"); got != date+" 00:00:00" {
			t.Fatalf("%v reads %q, want midnight on %s — this zone no longer goes back "+
				"through midnight and the test's premise is gone", x, got, date)
		}
	}

	got := StartOfLocalDay(afternoonOn(t, loc, date))
	if !got.Equal(earlier) {
		t.Errorf("StartOfLocalDay = %v, want the EARLIER midnight %v; the later one (%v) "+
			"excludes the first hour of the day from window=today", got, earlier, later)
	}
	if d := later.Sub(earlier); d != time.Hour {
		t.Errorf("the two midnights are %v apart, want 1h — that is the spend at stake", d)
	}
}

// TestParseWindowSpec_TodayStartsWhereTheLocalDayStartsInEveryZone is the same contract at
// the layer that serves it: window=today must be bounded by the day's first instant.
//
// The SPAN is the assertion that names the damage. At 15:00 on 2026-03-08 in America/Havana
// the day is 14 hours old, because it began at 01:00. The old bound reported 15 hours — an
// hour of the PREVIOUS date's spend, added to today's total with nothing in the response
// saying where it came from.
func TestParseWindowSpec_TodayStartsWhereTheLocalDayStartsInEveryZone(t *testing.T) {
	for _, c := range dayStartCases {
		t.Run(c.zone+"/"+c.date, func(t *testing.T) {
			loc := mustZone(t, c.zone)
			now := afternoonOn(t, loc, c.date)
			spec, err := ParseWindowSpec(WindowToday, now)
			if err != nil {
				t.Fatalf("ParseWindowSpec(%q): %v", WindowToday, err)
			}
			if !spec.Symbolic() {
				t.Fatal("today reported a fixed length; want symbolic")
			}
			want := mustInstant(t, c.wantStart, loc)
			if !spec.From.Equal(want) {
				t.Errorf("From = %v, want %v (%s)", spec.From, want, c.what)
			}
			if d := spec.From.Format("2006-01-02"); d != c.date {
				t.Errorf("From is on %s but now is on %s — today's window reaches into "+
					"another date, and that date's spend lands in this total", d, c.date)
			}
			if !spec.To.Equal(now) {
				t.Errorf("To = %v, want now %v", spec.To, now)
			}
			if got := spec.To.Sub(spec.From); got != c.wantSpan {
				extra := ""
				if got == c.badSpan && c.badSpan != c.wantSpan {
					extra = " — that is exactly the old local-midnight bound, so the defect is back"
				}
				t.Errorf("span = %v, want %v%s", got, c.wantSpan, extra)
			}
		})
	}
}

// sweptZones are swept date by date rather than tabulated, so the guarantee is not limited
// to the transitions someone thought to write down.
//
// Beyond the four zones the table names: Australia/Lord_Howe shifts by THIRTY MINUTES
// rather than an hour, America/Asuncion is another 00:00 shifter in the southern
// hemisphere, Europe/Lisbon shifts at 01:00 which is neither of the two shapes above, and
// UTC never shifts at all.
var sweptZones = []string{
	"America/Havana",
	"America/Santiago",
	"Asia/Beirut",
	"America/New_York",
	"America/Asuncion",
	"Australia/Lord_Howe",
	"Europe/Lisbon",
	"UTC",
}

// TestParseWindowSpec_TodayNeverReachesIntoAnotherDate is the PROPERTY, swept over every
// date in two years in eight zones — about six thousand day boundaries.
//
// A table only ever covers the transitions its author knew about, and the two that mattered
// here were not obvious: they are the ones where the shift happens to fall at 00:00. The
// sweep needs no such knowledge. It asserts the two halves of "earliest instant on the
// date" directly — From is on now's date, and the second before From is not — which is the
// whole contract, so any zone or year that breaks it fails here whatever its shape.
func TestParseWindowSpec_TodayNeverReachesIntoAnotherDate(t *testing.T) {
	for _, name := range sweptZones {
		loc := mustZone(t, name)
		t.Run(name, func(t *testing.T) {
			for i := 0; i < 730; i++ {
				// Built from a UTC walk and moved into the zone, so a date that does not exist
				// locally is skipped by construction instead of being silently normalised.
				now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC).AddDate(0, 0, i).In(loc)
				spec, err := ParseWindowSpec(WindowToday, now)
				if err != nil {
					t.Fatalf("ParseWindowSpec at %v: %v", now, err)
				}
				date := now.Format("2006-01-02")
				if d := spec.From.Format("2006-01-02"); d != date {
					t.Fatalf("now = %v: From = %v is on %s, not on %s — the window reaches "+
						"into another date", now, spec.From, d, date)
				}
				if d := spec.From.Add(-time.Second).Format("2006-01-02"); d == date {
					t.Fatalf("now = %v: the second before From = %v is still on %s, so From "+
						"is not the earliest instant on the date and spend before it is lost",
						now, spec.From, date)
				}
			}
		})
	}
}

// TestStartOfLocalDay_TerminatesWhereAZoneSkippedAWholeCalendarDate covers the one hazard
// the noon anchor introduces rather than removes.
//
// Pacific/Apia crossed the date line on 2011-12-29 and 2011-12-30 never happened locally.
// Noon on the previous date is therefore itself missing, and time.Date resolves it onto
// 2011-12-31 — the very date being bounded. An anchor stepped back with AddDate from that
// normalised value re-normalises to the same instant and never escapes: this HUNG the test
// binary while the fix was being written, which is why the backoff is a duration
// subtraction. The assertion is both that it returns and that it returns the right instant.
func TestStartOfLocalDay_TerminatesWhereAZoneSkippedAWholeCalendarDate(t *testing.T) {
	loc := mustZone(t, "Pacific/Apia")
	// The premise: noon on 2011-12-30 is not on 2011-12-30.
	if got := time.Date(2011, 12, 30, 12, 0, 0, 0, loc).Format("2006-01-02"); got == "2011-12-30" {
		t.Fatalf("2011-12-30 exists in Pacific/Apia (noon = %s); the premise of this test is "+
			"gone and it no longer covers the anchor backoff", got)
	}
	want := mustInstant(t, "2011-12-31T00:00:00+14:00", loc)
	got := StartOfLocalDay(afternoonOn(t, loc, "2011-12-31"))
	if !got.Equal(want) {
		t.Errorf("StartOfLocalDay = %v, want %v", got, want)
	}
}

// TestParseWindowSpec_SevenDaysIsUnaffectedByTheDayBoundaryInEveryZone answers the
// question this fix has to answer about the OTHER symbolic window: does 7d share the
// defect?
//
// It does not, and the reason is structural rather than lucky: 7d's From is
// now.Add(-Window7dSpan), a subtraction on the instant axis with no calendar arithmetic in
// it. There is no wall time for a DST gap to swallow. The test states that as an
// assertion — the span is exactly 7x24h on every transition day in the table, spring and
// autumn — so that if anyone ever rewrites 7d in terms of dates, this fails.
//
// HOW MANY LOCAL DATES the span touches is a separate question and is now asserted in
// TestParseWindowSpec_ASpringForwardWeekReachesTheNinthLocalDate. It used to be recorded here
// as a known-wrong note — nine in the week after a spring-forward, against a
// Window7dLocalDays of eight — which is what a fixture says instead of failing.
func TestParseWindowSpec_SevenDaysIsUnaffectedByTheDayBoundaryInEveryZone(t *testing.T) {
	for _, c := range dayStartCases {
		t.Run(c.zone+"/"+c.date, func(t *testing.T) {
			loc := mustZone(t, c.zone)
			// Both 15:00 and the first minute of the day: the second is where a calendar
			// reading of "seven days ago" would diverge from an instant one, because the day
			// it is counting back from has just started.
			for _, now := range []time.Time{
				afternoonOn(t, loc, c.date),
				StartOfLocalDay(afternoonOn(t, loc, c.date)),
			} {
				spec, err := ParseWindowSpec(Window7d, now)
				if err != nil {
					t.Fatalf("ParseWindowSpec(%q) at %v: %v", Window7d, now, err)
				}
				if d := spec.To.Sub(spec.From); d != Window7dSpan {
					t.Errorf("at %v: span = %v, want exactly %v — 7d is rolling on the instant "+
						"axis and no zone transition may change its length", now, d, Window7dSpan)
				}
				if !spec.From.Equal(now.Add(-Window7dSpan)) {
					t.Errorf("at %v: From = %v, want a plain subtraction %v — 7d must not "+
						"acquire calendar arithmetic, which is what a DST gap can swallow",
						now, spec.From, now.Add(-Window7dSpan))
				}
			}
		})
	}
}

// TestParseWindowSpec_ASpringForwardWeekReachesTheNinthLocalDate is why
// Window7dLocalDays is nine.
//
// A spring-forward week is 167 HOURS LONG, so a rolling 168-hour span reaches an hour
// further back than a calendar week does — into a NINTH local date. Eight was derived from
// the rolling span alone and silently assumed every day in the week is 24 hours; it is one
// day short for one week a year, in every zone that observes DST.
//
// It is the CONSTANT'S PREMISE and not the local-midnight defect, which is why
// America/New_York is in the table beside the 00:00-transition zones rather than serving as
// a control that escapes it: 7d's From is a plain duration subtraction from now, with no
// calendar arithmetic for a gap to swallow, so where in the day the transition falls does not
// matter at all.
//
// It matters because config.minCostLedgerRetentionDays agrees with this constant. At eight,
// retention_days: 8 passed validation and then answered window:"7d" over a partial week on
// the two mornings a year the span reaches nine dates — the exact case that floor exists to
// refuse, arrived at a second time by a second route.
//
// ASSERTED AT 00:00 LOCAL, which is where the span reaches furthest back. Any later hour on
// the same date pulls From forward into the eighth date and the ninth disappears, so a test
// at 15:30 would report eight and prove nothing about the ceiling.
func TestParseWindowSpec_ASpringForwardWeekReachesTheNinthLocalDate(t *testing.T) {
	// One date per zone: the day AFTER that zone's spring-forward, so the rolling week
	// behind it is the short one. Every value measured against tzdata.
	for _, c := range []struct{ zone, date string }{
		{"America/Havana", "2026-03-15"},
		{"America/Santiago", "2026-09-13"},
		{"America/New_York", "2026-03-15"},
		{"Asia/Beirut", "2026-04-05"},
	} {
		t.Run(c.zone+"/"+c.date, func(t *testing.T) {
			loc := mustZone(t, c.zone)
			now := StartOfLocalDay(afternoonOn(t, loc, c.date))
			// The premise: the week behind this instant really is 167 hours of wall clock, i.e.
			// the span crosses a transition. Without this the test would pass in a zone with no
			// DST at all while asserting nothing about it.
			_, nowOff := now.Zone()
			_, thenOff := now.Add(-Window7dSpan).Zone()
			if nowOff-thenOff != 3600 {
				t.Fatalf("the week before %v in %s does not spring forward by an hour "+
					"(offsets %d then %d); this fixture no longer covers the short week",
					now, c.zone, thenOff, nowOff)
			}
			if days := localDatesInWindow(t, now); days != Window7dLocalDays {
				t.Errorf("at %v, window=%q spans %d local days, want Window7dLocalDays = %d — "+
					"the ceiling must be REACHED by the worst week, or a floor derived from it "+
					"keeps a day file nobody needs; and if it is exceeded, the floor is short and "+
					"7d answers over a partial week", now, Window7d, days, Window7dLocalDays)
			}
		})
	}
}
