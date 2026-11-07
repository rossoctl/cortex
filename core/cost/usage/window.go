package usage

import (
	"errors"
	"fmt"
	"time"
)

// ParseWindow validates a window parameter against the storage resolution.
//
// Durations ONLY. It rejects the symbolic windows "today", "7d" and "month", because a
// time.Duration genuinely cannot express a boundary and silently substituting a
// length would report a number for a span nobody asked for. Callers that accept
// those use ParseWindowSpec, which delegates here for every fixed length so the
// duration rules are defined once.
func ParseWindow(s string) (time.Duration, error) {
	if s == "" {
		return 10 * BucketWidth, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		// Not wrapped with the caller's string, for the reason in ParseGroup.
		// time.ParseDuration's own error quotes the input, so it is not
		// forwarded either.
		// NAMES THE SYMBOLIC WINDOWS TOO. This is the only guidance a mistyped window gets, and
		// ?window=mtd was told to pick a duration — advice that cannot lead to "month". The
		// duration examples stay first because a duration is the default shape.
		return 0, errors.New("bad window (want a duration such as 10m, 1h or 6h, " +
			"or one of today, 7d, month)")
	}
	if d < BucketWidth {
		return 0, fmt.Errorf("window %s is shorter than the %s bucket width", d, BucketWidth)
	}
	if d > MaxWindow {
		return 0, fmt.Errorf("window %s exceeds the %s retained", d, MaxWindow)
	}
	if d%BucketWidth != 0 {
		return 0, fmt.Errorf("window %s is not a multiple of %s", d, BucketWidth)
	}
	return d, nil
}

// dayAnchorHour is the hour used to establish WHICH local date is meant, before any
// boundary arithmetic is done on it.
//
// NOON, and deliberately the same convention and the same value as cost/ledger's dayHour:
// these two layers have to agree about where a day starts, because this package computes
// the window bounds that select the day files that package names. A DST transition moves
// the clock by an hour (Australia/Lord_Howe by thirty minutes), so no transition can move
// noon onto a different DATE. Midnight is the opposite, and that is this constant's whole
// reason to exist.
//
// AN ANCHOR, NEVER A BOUND, which is the one place the two layers legitimately differ.
// ledger.dayOf may stop at noon because it only has to IDENTIFY a day — it names a
// file. A window's From has to be the day's FIRST INSTANT, so noon is not reusable
// directly and StartOfLocalDay sweeps back from it.
const dayAnchorHour = 12

// dayStartSweepSteps refine StartOfLocalDay's forward sweep, coarsest first.
//
// Hours then minutes then seconds is at most about 150 zone lookups, against roughly
// 90,000 for a second-by-second sweep of a whole day. A second is the finest step worth
// taking: every UTC offset and every transition instant in the IANA database is a whole
// number of seconds, so the boundary this lands on is exact rather than rounded.
var dayStartSweepSteps = []time.Duration{time.Hour, time.Minute, time.Second}

// StartOfLocalDay is the earliest instant that EXISTS on t's local calendar date, in t's
// own zone. It is the lower bound of "today".
//
// IT IS NOT time.Date(y, m, d, 0, 0, 0, 0, loc), and the difference is money. In a zone
// whose DST transition falls AT 00:00 the spring-forward day has no midnight at all, and
// time.Date resolves a wall time inside the gap onto the far side of it. MEASURED, with
// that expression as the bound:
//
//	now  = 2026-03-08 15:00 -0400 America/Havana
//	From = 2026-03-07 23:00 -0500   ← an hour before the previous day even ended
//	now  = 2026-09-06 15:00 -0300 America/Santiago
//	From = 2026-09-05 23:00 -0400   ← same shape
//
// So window=today reached an hour and fifty-nine minutes back into YESTERDAY and folded
// its last hour of spend into today's total. America/New_York, whose transition is at
// 02:00, was unaffected — which is why a suite whose only non-UTC zone was a
// time.FixedZone passed. A fixed offset has no transitions and structurally cannot express
// this. See TestStartOfLocalDay_IsTheFirstInstantOnTheDateInEveryZone.
//
// IT HAS TO AGREE WITH THE LAYER UNDERNEATH. The cost ledger names its day files from a date
// carried at noon (ledger.dayOf, dayFromName and dayHour) and this function is what
// SELECTS those files, so a midnight bound and that naming would disagree about where a
// Havana day starts — silently: no error, no caveat, just a total including an hour that
// belongs to another date.
//
// THE RULE, stated plainly: sweep the instant axis forward from a point certainly before
// the date began and take the FIRST instant whose local date is the one wanted. That is
// the definition of "earliest existing instant" evaluated directly, rather than a wall
// time handed to time.Date and hoped to exist. It needs no assumption about which
// direction time.Date normalises, or that local time is monotone across a transition.
//
// AUTUMN-BACK IS THE OTHER HALF, and it is a real choice rather than a corollary. Where
// the clock goes back THROUGH midnight the wall time 00:00 occurs TWICE — in
// America/Havana on 2026-11-01 at 00:00 -0400 and again at 00:00 -0500, an hour apart.
// Both are on the date, so "midnight" alone does not name a bound. THE EARLIER ONE IS
// CHOSEN: a lower bound of the later instant would exclude the first hour of the day, and
// any spend in it would be missing from today's total with nothing saying so — the same
// silent shortfall in the other direction. The sweep picks it for free, because the
// earlier instant is the first one it reaches. Go's own time.Date happens to resolve an
// ambiguous wall time to the earlier occurrence here, but its documentation explicitly
// declines to guarantee that, so the choice is made here instead of inherited.
//
// EXPORTED because sessionapi's tests have to anchor a "today" fixture to the SAME
// boundary this serves. That helper was a second copy of the midnight expression and
// carried the identical flaw; a boundary derived twice is a boundary that drifts. The
// dependency direction is sessionapi to usage, never the reverse.
func StartOfLocalDay(t time.Time) time.Time {
	loc := t.Location()
	y, m, d := t.Date()
	onDate := func(x time.Time) bool {
		xy, xm, xd := x.Date()
		return xy == y && xm == m && xd == d
	}

	// before must be an instant OUTSIDE this date and earlier than it, so the sweep below
	// always starts before the boundary it is looking for. Noon anchors the date; whole days
	// back from that noon leave it.
	//
	// SUBTRACTED FROM THE ANCHOR AS A DURATION, not stepped with AddDate, and the difference
	// is a hang. A zone can skip a whole calendar date — Pacific/Apia dropped 2011-12-30
	// entirely when it crossed the date line — and then that date has no noon either, so
	// time.Date resolves it onto the NEXT one. AddDate on an already-normalised anchor
	// re-normalises to the same instant and the loop spins forever; MEASURED as a hang on
	// Pacific/Apia 2011-12-31 while writing this. A duration subtraction cannot normalise.
	//
	// It terminates because each iteration moves a fixed 24h further back on the instant
	// axis while a local date covers a bounded interval of it — under 50h even for
	// 1892-07-04 in Pacific/Apia, the longest in the database, where crossing the line the
	// other way made the date happen twice. So no iteration cap is needed, and one that
	// gave up while still inside the date would only hide the failure.
	noon := time.Date(y, m, d, dayAnchorHour, 0, 0, 0, loc)
	before := noon
	for back := 1; onDate(before); back++ {
		before = noon.Add(-time.Duration(back) * 24 * time.Hour)
	}

	first := before
	for _, step := range dayStartSweepSteps {
		first = before
		for !onDate(first) {
			first = first.Add(step)
		}
		// first is on the date and first-step is not — the sweep visited it and moved on —
		// so the boundary lies in (first-step, first]. Restart the next, finer pass there.
		before = first.Add(-step)
	}
	return first
}

// StartOfLocalMonth is the earliest instant that EXISTS in t's local calendar month, in t's
// own zone. It is the lower bound of "month to date".
//
// IT IS StartOfLocalDay ASKED ABOUT THE FIRST, not a second sweep, and that is deliberate:
// the earliest instant in a month is the earliest instant on its first date, so there is one
// definition of "where a local period begins" in this package rather than two that agree
// until a zone makes them disagree. StartOfLocalDay's own doc records what a duplicated
// boundary cost the layer above it.
//
// THE FIRST IS ANCHORED AT NOON before being handed over, for the reason dayAnchorHour
// exists: no DST transition can move midday onto a different date, so the anchor identifies
// the month's first date without depending on midnight existing on it.
//
// AND IT HAS TO, because a month bound can be wrong in a way a day bound cannot. Paraguay
// moved its clocks forward AT 00:00 ON 1 OCTOBER in 2017 and 2023, so midnight on that date
// does not exist and time.Date(y, 10, 1, 0, 0, 0, 0, loc) resolves BACKWARDS to 23:00 on 30
// September — an hour before the previous month ended. A month-to-date total on that bound
// folds September's last hour into October and reports a budget closer to its limit than it
// is. See TestStartOfLocalMonth_TheNaiveFirstOfMonthExpressionIsStillWrong, which is the
// negative control, and note that the day-level cases StartOfLocalDay documents are all
// mid-month: this shape needed its own fixture to be found at all.
//
// A ZONE THAT SKIPS THE FIRST resolves correctly by construction. Pacific/Apia dropped
// 2011-12-30 entirely when it crossed the date line; were a zone ever to drop a first of the
// month, the noon anchor normalises onto the next date and the sweep returns the start of
// THAT date — which is then genuinely the earliest instant in the month.
func StartOfLocalMonth(t time.Time) time.Time {
	y, m, _ := t.Date()
	return StartOfLocalDay(time.Date(y, m, 1, dayAnchorHour, 0, 0, 0, t.Location()))
}

// WindowToday, Window7d and WindowMonth are the symbolic windows the API accepts.
//
// Symbolic because none is a LENGTH: "today" and "month" are boundaries, and while "7d" has
// a fixed span it is longer than the ring retains, so all three can only be answered from
// the durable cost ledger. time.ParseDuration reads none of these strings, which is why
// ParseWindow already rejects them and why they need their own parse.
//
// "month" RATHER THAN "mtd", because "today" is already a boundary word rather than a length
// and this joins that family. "mtd" is less ambiguous read cold, at the cost of a second
// naming convention on the same small enum; the label is echoed back on the wire either way,
// so a client always learns which window it got. It means month-TO-DATE — since the first of
// the local month, not a rolling thirty days — which is what a budget that resets on the
// first is measured against.
const (
	WindowToday = "today"
	Window7d    = "7d"
	WindowMonth = "month"
)

// Window7dSpan is what "7d" MEANS: a rolling seven times twenty-four hours back from
// now. Stated once, and read by ParseWindowSpec, so nothing that has to reason about
// the span can spell it differently.
const Window7dSpan = 7 * 24 * time.Hour

// WindowMonthLocalDays is the MOST distinct LOCAL DATES a month-to-date window can touch:
// THIRTY-ONE, the length of the longest calendar month.
//
// NO +1, unlike Window7dLocalDays, and the asymmetry is the whole difference between a
// boundary window and a rolling one. 7d needs two increments because it starts part-way
// through a date and because a spring-forward week is 167 hours, so it reaches an hour
// further back than a calendar week does. This window's From IS a date boundary —
// StartOfLocalMonth returns the first instant of the first — so it begins exactly where a day
// file begins and cannot spill onto an earlier date. A DST transition inside the month moves
// which instants the window covers but not the set of DATES, and a date is what a day file is
// named by.
//
// It is therefore the number of day files a durable ledger must hold to answer this window on
// the 31st of a 31-day month, which is why cost/ledger's default retention is derived from this
// rather than written as its own number — the lesson Window7dLocalDays records about two
// constants that had to agree and did not.
const WindowMonthLocalDays = 31

// Window7dLocalDays is the MOST distinct LOCAL DATES a 7d window can touch: NINE.
//
// A CEILING, NOT A COUNT. Eight is what the window touches in an ordinary week, and it is
// wrong once a year in every zone that observes DST.
//
// Two increments, for two different reasons:
//
//   - +1 because the span is ROLLING rather than calendar-aligned. Unless it begins exactly
//     at midnight it starts part-way through one date and ends part-way through another —
//     seven days' worth of hours spread across eight dates. Asked at 15:30 it runs from
//     15:30 seven days ago to 15:30 today and needs a piece of every date between, both ends
//     included. Asked exactly at midnight it needs the eighth date for one instant, which is
//     still a file to open.
//   - +1 again because A SPRING-FORWARD WEEK IS 167 HOURS. Eight assumed every day in the
//     week is 24 hours long, and one week a year is not: a 168-hour span reaches an hour
//     further back than a calendar week does. MEASURED at 00:00 local on 2026-03-15, From
//     lands at 23:00 on 2026-03-07 and the window touches NINE dates — in America/Havana, in
//     America/Santiago and in America/New_York alike, because this has nothing to do with
//     WHERE in the day the transition falls. Autumn is the harmless direction: a 169-hour
//     week means the span reaches less far and the ceiling is slack by a day.
//
// It is therefore the number of DAY FILES a durable ledger must still hold to answer this
// window, which is why config's retention floor is derived from this rather than written as
// its own independent number. As two constants that had to agree, they did not: a floor of 7
// let retention_days: 7 pass validation and then answer window:"7d" over a partial week — the
// exact case the floor exists to refuse.
//
// COSTS ONE DAY FILE OF RETENTION ALL YEAR to cover two mornings of it, and that is the
// trade taken deliberately. The alternative considered was making 7d CALENDAR-ALIGNED, which
// is the more correct fix — it would make the count exactly 8 and true every week — but it
// changes what the window MEANS on the wire: "the last 7 days" and "since midnight 7 days
// ago" differ by up to a day of spend, every client comparing figures across the change
// would see a step, and ParseWindowSpec's own doc promises the rolling reading. That is a
// product decision, so it is disclosed rather than made here. A day file is roughly 300 KB
// at the volumes this ledger is sized for; the partial week it prevents is a wrong dollar
// total that nothing downstream can detect.
//
// Distinct from the local-midnight defect StartOfLocalDay fixes: 7d's From is a plain
// duration subtraction from now and contains no calendar arithmetic at all.
// TestParseWindowSpec_SevenDaysTouchesAtMostWindow7dLocalDays pins the ordinary week and
// TestParseWindowSpec_ASpringForwardWeekReachesTheNinthLocalDate pins the week that forced the
// second +1. The agreement with config's retention floor is pinned on the CONFIG side, by
// TestCostLedgerConfig_TheFloorCoversEveryDayTheWindowTouches, which this change adds along with the
// cost_ledger settings it guards.
const Window7dLocalDays = int(Window7dSpan/(24*time.Hour)) + 2

// Spec is a parsed window request. Either Dur is set (a fixed length the ring can
// serve) or From/To are (a boundary only the ledger can serve).
//
// Two representations in one type rather than two functions, because the handler's
// job is exactly to choose between them and a caller that forgets to ask which
// kind it has would silently serve the wrong span.
type Spec struct {
	// Label is what the response echoes back, so a client always learns which
	// window it actually got — which matters most when it is not the one asked for.
	Label string
	// Dur is non-zero for a fixed-length window.
	Dur time.Duration
	// From and To bound a symbolic window. Zero when Dur is set.
	From, To time.Time
}

// Symbolic reports whether this window needs the ledger.
func (s Spec) Symbolic() bool { return s.Dur == 0 }

// ParseWindowSpec parses any window the API accepts, including the symbolic ones.
//
// now is passed rather than read from the clock so a test can pin a day boundary,
// and so "today" is computed once per request instead of drifting between the
// bound calculation and the response label.
//
// "today" is the START OF THE LOCAL DAY to now. Local, not UTC: a laptop that
// crosses a timezone must not have its day reset mid-afternoon, which a UTC day would
// do. Just after the day starts it is a five-minute window, not a 24-hour one — that
// is the point of a boundary rather than a length.
//
// The start of the day, NOT "local midnight", and the distinction is not pedantry:
// midnight does not exist on the spring-forward day of any zone whose transition is at
// 00:00, and it occurs twice on the autumn one. StartOfLocalDay states which instant is
// meant and why, and it is the same day boundary the cost ledger names its files by.
//
// "7d" is exactly 7x24h back from now, ROLLING rather than seven calendar days.
// The label is echoed as "7d" so a client can read it that way; a "last 7 days"
// that silently meant "since last Monday" would be a different number, and the two
// differ by up to a day of spend.
//
// Delegates every fixed-length case to ParseWindow, so the duration set is defined
// in one place and the two cannot drift.
func ParseWindowSpec(s string, now time.Time) (Spec, error) {
	switch s {
	case WindowToday:
		return Spec{
			Label: WindowToday,
			From:  StartOfLocalDay(now),
			To:    now,
		}, nil
	case Window7d:
		// Window7dSpan rather than a second spelling of 7x24h: the ledger's retention
		// floor is derived from the same constant, and a span defined twice is how the two
		// came to disagree. See Window7dLocalDays.
		return Spec{Label: Window7d, From: now.Add(-Window7dSpan), To: now}, nil
	case WindowMonth:
		// THE START OF THE LOCAL MONTH to now, so this is month-TO-DATE and grows through the
		// month rather than being a fixed span. On the first it is minutes wide, which is the
		// point of a boundary: a budget that resets on the first has spent nothing yet.
		//
		// StartOfLocalMonth for the same reason "today" uses StartOfLocalDay rather than
		// midnight — see there, and note that this window reaches a shape the day window
		// cannot, a zone shifting at 00:00 on a first of the month.
		return Spec{
			Label: WindowMonth,
			From:  StartOfLocalMonth(now),
			To:    now,
		}, nil
	}
	d, err := ParseWindow(s)
	if err != nil {
		// Not wrapped with the caller's string, for the reason in ParseGroup: this
		// travels back over an unauthenticated endpoint. ParseWindow's own messages are
		// fixed strings for the same reason, so forwarding one is safe.
		return Spec{}, err
	}
	// Label is the caller's own spelling, not d.String(). It is what a response
	// SHOULD echo — "1h", not "1h0m0s" — though on the fixed-length path the label
	// actually served comes from Aggregator.Snapshot, which stringifies the duration;
	// only the ledger path reads this field. Carried anyway so the type answers "which
	// window is this" uniformly, and so a future caller cannot get a symbolic label
	// right and a duration label wrong.
	//
	// An empty window means the ten-bucket default, which has no caller spelling to
	// echo, so the duration's own form is the only label available.
	label := s
	if label == "" {
		label = d.String()
	}
	return Spec{Label: label, Dur: d}, nil
}

// resolutionError is a resolution rejection that knows whether its reason came from the WINDOW.
//
// A TYPE RATHER THAN A SENTINEL PER CASE, because the caller's question is "did this bound come from
// a window the requester actually named?" and there are two rejections for which the answer is no —
// the span being too short, and the span not dividing evenly. The first version of this exported one
// sentinel for the first of them, so the second still reached a caller as an unclassifiable
// errors.New: window=7d&resolution=7m was refused because 6h does not divide by 7m, even though 7m
// divides seven days exactly and the ledger path never reads the resolution at all.
//
// Keeping the flag on the error means a FIFTH check has to choose a side here, in the constructor,
// instead of being classified by a switch somewhere else that nobody updates.
type resolutionError struct {
	msg string
	// cause is which window-dependent rejection fired, or ResolutionWindowNone. Not a boolean:
	// a caller restating these has to say WHICH bound the served span broke, and one flag let a
	// message written for "too coarse" be reused for "does not divide" — where the requested
	// resolution was 51x FINER than the ceiling that message quotes, so the restatement named a
	// bound the caller had not hit. That is the defect the restatement exists to prevent, arriving
	// through the mechanism built to prevent it.
	cause ResolutionWindowReason
}

func (e *resolutionError) Error() string { return e.msg }

// ResolutionWindowReason names WHY a resolution was rejected against a window, for a caller that has
// to explain it in terms of a span the requester never named.
type ResolutionWindowReason int

const (
	// ResolutionWindowNone: the rejection was about the resolution alone — unparseable, finer than
	// the storage bucket, or not a multiple of it — and its own wording is already correct.
	ResolutionWindowNone ResolutionWindowReason = iota
	// ResolutionTooCoarseForWindow: the resolution is wider than the span being served.
	ResolutionTooCoarseForWindow
	// ResolutionIndivisibleByWindow: the resolution fits, but does not divide the span evenly, so the
	// newest bucket would be narrower than the width reported for it.
	ResolutionIndivisibleByWindow
)

// ResolutionWindowCause reports which window-dependent rejection produced err, or
// ResolutionWindowNone.
//
// ONE FUNCTION RATHER THAN A PREDICATE PLUS A LOOKUP, so a caller cannot ask "was it the window?"
// without being handed the answer to "which one?". The version this replaces answered only the first
// question, and the caller then wrote one message for both.
func ResolutionWindowCause(err error) ResolutionWindowReason {
	var re *resolutionError
	if errors.As(err, &re) {
		return re.cause
	}
	return ResolutionWindowNone
}

// parseResolutionAgainstStorage runs the checks that hold for ANY window: the value parses, and it is
// a whole number of storage buckets. Both callers start here, so the split is where the two kinds of
// rejection are separated rather than a fact restated in two places.
func parseResolutionAgainstStorage(s string) (time.Duration, error) {
	if s == "" {
		return BucketWidth, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		// Does not echo the caller's value — see ParseGroup.
		return 0, &resolutionError{msg: "bad resolution (want a duration such as 1m, 5m or 30m)"}
	}
	if d < BucketWidth {
		return 0, &resolutionError{msg: fmt.Sprintf("resolution %s is finer than the %s storage bucket", d, BucketWidth)}
	}
	if d%BucketWidth != 0 {
		return 0, &resolutionError{msg: fmt.Sprintf("resolution %s is not a multiple of %s", d, BucketWidth)}
	}
	return d, nil
}

// ParseResolutionUnbounded validates a resolution that will not be used to slice anything, so only
// the checks that do not depend on a window apply.
//
// FOR THE WINDOW THAT IS ANSWERED AS ONE BUCKET. A symbolic window served from the cost ledger
// returns a single bucket spanning the whole window and never reads the resolution — so validating it
// against the RING's maximum refused requests that were fine: window=7d&resolution=7m came back 400
// because 6h does not divide by 7m, while 7m divides seven days exactly (1440 buckets). The bound was
// a property of a window the caller had not named and of a code path that was not going to run.
//
// The two remaining checks are still meaningful, because they are about the STORAGE bucket rather
// than the window: a resolution finer than BucketWidth or not a multiple of it cannot be honoured by
// any window. And a malformed value is still refused rather than ignored, so a typo in a parameter
// this path does not read is not silently accepted.
//
// NOT ParseResolution(s, MaxWindow), which is the shape I first wrote and which reproduces the very
// defect: MaxWindow is 360 minutes, so a 7m resolution fails the divisibility check against it just
// as it did against the 6h window. "Unbounded" has to mean the checks are skipped, not that a wide
// window is passed.
func ParseResolutionUnbounded(s string) (time.Duration, error) {
	return parseResolutionAgainstStorage(s)
}

// ParseResolution validates a resolution parameter — the width of the buckets
// the caller wants back, as opposed to the window's total span.
//
// Folding happens server-side so every client gets the same arithmetic. A 1h
// window at 1m resolution is 60 bars, which no terminal renders legibly; at 5m
// it is 12. Doing that here rather than in each client means the mean-of-means
// problem below is solved once, correctly, instead of per consumer.
//
// Zero or empty means "storage resolution" (BucketWidth). A resolution finer
// than BucketWidth is an error rather than a silent upgrade: returning coarser
// data than asked for would make a client's axis labels wrong.
func ParseResolution(s string, window time.Duration) (time.Duration, error) {
	d, err := parseResolutionAgainstStorage(s)
	if err != nil {
		return 0, err
	}
	if d > window {
		// CLASSIFIED, because a caller has to tell the two WINDOW-dependent rejections from the three
		// that are about the resolution alone — and from each other. sessionapi restates these for a
		// symbolic window, where the span validated against is the ring's maximum rather than the
		// seven days the caller named; the others must keep their own wording, or the restatement
		// points at a bound the caller never hit.
		//
		// A SIXTH CHECK BELONGS HERE, with a cause chosen deliberately: ResolutionWindowNone if it is
		// about the resolution alone, a new reason if it is about the span. Leaving it unclassified is
		// how the divisibility case below stayed invisible for a round.
		return 0, &resolutionError{
			msg:   fmt.Sprintf("resolution %s exceeds the %s window", d, window),
			cause: ResolutionTooCoarseForWindow,
		}
	}
	// The window must divide by the resolution, or the NEWEST bucket is a lie.
	//
	// fold() emits a partial trailing group rather than truncating it — deliberately,
	// because the still-filling block is the one an operator is watching — but every
	// bucket is labelled with the same BucketSeconds. So window=10m at resolution=3m
	// returned four buckets of which the last covered ONE minute while claiming 180
	// seconds, and a client deriving a burn rate from the newest bar tripled it.
	//
	// Rejected here rather than papered over downstream: the caller asked for a slicing
	// this window cannot express, and serving a differently-sized bucket under the
	// requested label would be a wrong number wearing a right label. A client that
	// wants the fine bar picks a window the resolution divides.
	//
	// FIXED LITERAL, no interpolation. This endpoint is unauthenticated, so an error
	// body must never reflect caller-supplied bytes back — see ParseGroup. The messages
	// above interpolate only a re-stringified time.Duration, which cannot carry
	// arbitrary bytes; this one needs neither operand to be actionable.
	if window%d != 0 {
		return 0, &resolutionError{
			msg: "resolution does not divide the window evenly (the newest bucket would be " +
				"shorter than the width reported for it)",
			cause: ResolutionIndivisibleByWindow,
		}
	}
	return d, nil
}
