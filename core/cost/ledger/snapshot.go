package ledger

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/model"
)

var errDurationWindow = errors.New("costledger: a snapshot needs a symbolic window (today, 7d, month)")

// Snapshot is the ledger's answer to a /v1/usage window: every row in spec's window,
// narrowed to agent's rows when agent is set, folded into ONE bucket spanning the window.
// The ledger answers "what did today cost"; a shaped chart comes from the ring.
//
// It reads through Window, so the minute still accumulating in memory is included.
// ctx stops the day-file walk when the caller goes away.
//
// The grouping in effect is reported in Snapshot.Group: an axis a row has no column for
// (session, status, plugin) is served as GroupNone, so a client can tell "this source
// cannot break down by that" from "there was no traffic".
//
// UnpricedBy and PricedBy are left unset. A row carries only its own labels, so it cannot
// say which requests of its endpoint-and-model pair went unpriced; the gap stays visible
// as Totals.PricedRequests against Totals.PriceableRequests.
//
// spec must be symbolic: a duration window is the ring's, and carries no From or To.
func (w *Writer) Snapshot(ctx context.Context, spec usage.Spec, group usage.Group, agent string) (usage.Snapshot, error) {
	if !spec.Symbolic() {
		return usage.Snapshot{}, errDurationWindow
	}
	rows, caveats, err := w.Window(ctx, spec.From, spec.To)
	if err != nil {
		return usage.Snapshot{}, err
	}
	if agent != "" {
		rows = FilterAgent(rows, agent)
	}
	// Measured against spec.To rather than a second clock read, so a request crossing
	// midnight compares both sides against the same day.
	outside := daysOutsideRetention(spec.From, w.RetentionCutoffAt(spec.To))
	applied := group
	if !Groupable(group) {
		applied = usage.GroupNone
	}
	totals, series, ungroupedCost, ungroupedAvoided := Fold(rows, applied)
	// Flagged on totals before the bucket copies it, so the bucket and Totals agree. Either
	// residual clamping makes every money figure here a bound.
	if ungroupedCost.Saturated || ungroupedAvoided.Saturated {
		totals.Saturated = true
	}
	// Rows the writer dropped never reached a file, so they are disclosed beside the
	// caveats of this read.
	dropped := w.Dropped()
	var degraded *usage.Degraded
	if !caveats.Clean() || dropped > 0 {
		degraded = degradedFrom(caveats)
		degraded.DroppedRowsTotal = dropped
	}
	if !caveats.Clean() {
		slog.Warn("costledger: cost ledger read was incomplete — the total is short",
			"window", model.SanitizeLogValue(spec.Label), "skippedLines", caveats.SkippedLines,
			"truncatedDays", caveats.TruncatedDays,
			"unreadableDays", caveats.UnreadableDays)
	}
	// Once per increase: the count is monotonic, so Swap returns the last value warned about.
	if prev := w.loggedReadDrops.Swap(dropped); dropped > prev {
		slog.Warn("costledger: the writer has lost rows; a total covering them is short",
			"droppedRowsTotal", dropped, "newlyDropped", dropped-prev,
			"cause", "an append failed, or the queue filled while the filesystem stalled",
			"effect", "a total covering them is a floor; the count is disclosed on every response as degraded.droppedRowsTotal")
	}
	snap := usage.Snapshot{
		Window:        spec.Label,
		BucketSeconds: bucketSecondsFor(spec.From, spec.To),
		Group:         applied,
		Buckets:       []usage.Bucket{{At: spec.From, Counts: totals, Series: series}},
		Totals:        totals,
		Degraded:      degraded,
		// An inexact figure is still a figure; its caveat is Totals.IncompleteRequests.
		Priced:               totals.PricedRequests > 0,
		DaysOutsideRetention: outside,
	}
	snap.SetUngroupedCost(ungroupedCost)
	snap.Currencies = CurrenciesIn(rows)
	snap.SeriesCurrencies = SeriesCurrenciesIn(rows, applied, series)
	snap.Agent = agent
	snap.SetUngroupedAvoided(ungroupedAvoided)
	return snap, nil
}

// daysOutsideRetention is how many whole ledger days of a window start before cutoff.
//
// Both arguments identify a date: cutoff is a ledger day carried at dayHour, from a local
// midnight. So both are reduced to dates in the ledger's zone and differenced as dates.
func daysOutsideRetention(from, cutoff time.Time) int64 {
	d := utcDayOf(cutoff).Sub(utcDayOf(from.In(cutoff.Location())))
	if n := int64(d / (24 * time.Hour)); n > 0 {
		return n
	}
	return 0
}

// utcDayOf is t's calendar date at dayHour UTC. UTC has no transitions, so two of these
// differ by an exact multiple of 24 hours.
func utcDayOf(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, dayHour, 0, 0, 0, time.UTC)
}

// degradedFrom is the whole Caveats-to-wire conversion, so a counter added to Caveats has
// one place to land.
func degradedFrom(c Caveats) *usage.Degraded {
	return &usage.Degraded{
		SkippedLines:   c.SkippedLines,
		TruncatedDays:  c.TruncatedDays,
		UnreadableDays: c.UnreadableDays,
	}
}

// bucketSecondsFor is the length of a window's single bucket, at least one second: in the
// first second of a local day a window is shorter than that, and clients divide by it.
func bucketSecondsFor(from, to time.Time) int {
	if s := int(to.Sub(from).Seconds()); s > 1 {
		return s
	}
	return 1
}
