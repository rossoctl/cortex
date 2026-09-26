package ledger

import (
	"context"
	"log/slog"
	"time"

	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
)

// Window returns everything the ledger knows about [from, to]: the closed minutes on
// disk plus the open minute still in memory.
//
// THIS is what a reader calls. Query alone answers only for what has been flushed,
// which omits the minute currently accumulating — and omits it indefinitely once
// traffic stops, since the flush is driven by the next event. A session whose whole
// conversation fit inside one minute produced no disk rows at all.
//
// NON-OVERLAP IS RECONCILED, NOT ASSUMED, because a minute counted twice is worse than
// one counted late. The accumulator is read first together with its GENERATION (see
// Writer.flushGen), then the day files, then the generation again. Unchanged means
// nothing left the accumulator during the read, so its rows are still memory-only and
// are added; changed means they may now be on disk too, so the MEMORY half is dropped
// and only disk is returned.
//
// NOTHING ON DISK IS EVER DROPPED. This function can only ADD to what the day files hold,
// and that rule is load-bearing rather than tidy: a disk row for the held minute is
// indistinguishable from this writer's own racing flush versus another process's committed
// spend — same key, same counters — so no matching on the row can decide it. What CAN be
// decided exactly is whether this writer flushed during this read, which is what the
// generation asks. Dropping the disk copy instead loses committed spend on an ordinary
// restart, with no drop count and no caveat anywhere in the response — measured at $1.00 of
// $1.25.
//
// The cost is that a flush racing a read can leave the just-flushed minute out of that
// one answer; it is bounded by one read and self-heals on the next.
//
// Seeding flushedThrough from disk in New was the other candidate and is deliberately
// not what this does: it would re-establish the ownership rule for a sequential restart
// only, since two live writers over one directory still put a disk row in the minute one
// of them holds — so a reader would still need this rule.
func (w *Writer) Window(ctx context.Context, from, to time.Time) ([]Row, Caveats, error) {
	fromMin, toMin := span(from, to)
	pending, _, gen := w.pending()
	if w.betweenWindowReads != nil {
		// Test seam. Nil in production; see the field.
		w.betweenWindowReads()
	}

	rows, caveats, err := w.Query(ctx, from, to)
	if err != nil {
		return nil, Caveats{}, err
	}
	if w.flushGeneration() != gen {
		// Step 3: the accumulator moved while this read ran, so the snapshot may be on
		// disk too. Return the disk half alone rather than risk counting a minute twice.
		return rows, caveats, nil
	}
	for _, r := range pending {
		if m := r.At.Truncate(time.Minute); m.Before(fromMin) || m.After(toMin) {
			// The held minute can fall outside the window asked for — an idle proxy still
			// holds yesterday's last minute at 00:05 today, and that spend belongs to
			// yesterday. Filtered with the same bounds Query applies so the two halves of
			// one answer cannot disagree about which minutes are in it.
			continue
		}
		rows = append(rows, r)
	}
	return rows, caveats, nil
}

// Caveats is what ONE READ could not deliver — and it belongs to that read.
//
// RETURNED, NOT STORED, and that is a correctness rule rather than a style choice. As
// atomics on the Writer — set by whichever Query ran last, sampled by the caller afterwards —
// two concurrent /v1/usage readers swap each other's answers: measured with one clean day
// file and one holding an undecodable line, the reader of the CORRUPT day reported
// SkippedLines 0 while the reader of the CLEAN day reported 1. Both directions of the same
// defect in one interleaving: a damaged day served as complete, and a clean day carrying a
// caveat that described someone else's file. No ordering is needed to produce it — the read
// and the sample are two separate calls, and anything between them is another reader's write.
// sessionapi does exactly that, once per request, on an endpoint a chart polls.
//
// A GAUGE RATHER THAN A COUNTER: it describes the read that returned it and never
// accumulates. A day file with one corrupt line is re-read on every /v1/usage request, and a
// cumulative count would climb forever over one piece of damage and read as an escalating
// fault.
type Caveats struct {
	// SkippedLines is how many lines this read could not decode and stepped over. Each is
	// spend that happened and is not in the rows returned beside it.
	//
	// A FLOOR ON ROWS LOST, not an exact count of them. One undecodable line is usually one
	// row, but a line that is a crash fragment with the next append concatenated onto it is
	// one line holding two lost rows — measured. So a non-zero value here means "at least
	// this many rows are missing", which is the reading a client has to present. See
	// store.appendBytes for why the bytes cannot support an exact figure.
	SkippedLines int64
	// TruncatedDays is how many day files this read ABANDONED part-way — an IO error, or a
	// line past maxLineBytes that a scanner cannot step over.
	//
	// Worse than a skipped line by an unknown amount: everything after that offset is
	// missing and the file gives no way to say how much. Tracked separately from a skip
	// rather than added to it for exactly that reason.
	TruncatedDays int64
	// UnreadableDays is how many day files this read could not open or scan AT ALL — a
	// permission change, a vanished mount, an IO error on the first read.
	//
	// A CAVEAT RATHER THAN AN ERROR, which is the whole reason it exists. Query used to return
	// the error and no rows, so one unreadable file inside a seven-day window discarded the six
	// readable ones and answered "no cost data" for a week that had plenty. A short answer that
	// says it is short is strictly better than no answer that says nothing. Counted separately
	// from TruncatedDays because nothing at all was read from these, not merely a prefix.
	UnreadableDays int64
}

// Clean reports that the read lost nothing, so a caller can disclose the caveats only
// when there are some. The absent-not-zero convention usage.Degraded documents: zeros in
// an always-present object read as "checked, fine" from a producer that never checked.
func (c Caveats) Clean() bool { return c == Caveats{} }

// RetentionDays is how many day files this ledger keeps, AFTER the zero-means-default and
// clamp-to-maximum resolution New performs.
//
// Exported so a caller can log what it got rather than what it asked for. The proxy logged
// cfg.CostLedger.RetentionDays, which is 0 on a default install — so the line that tells an
// operator how much history they have said "retentionDays=0" for a ledger keeping 31 days, on the
// setting that decides whether window=month can be answered in full.
func (w *Writer) RetentionDays() int { return w.store.retainDays }

// RetentionCutoff is the oldest day this ledger's configuration reaches back to, measured from
// this writer's own clock.
//
// EXPORTED FOR THE COVERAGE STATEMENT, and for nothing else. A caller comparing a requested
// window's start against this learns whether the answer CAN cover the whole window — which is a
// fact about configuration, not about data. It deliberately does not, and cannot, say whether
// anything was deleted: see store.retentionCutoff.
func (w *Writer) RetentionCutoff() time.Time { return w.RetentionCutoffAt(w.now()) }

// RetentionCutoffAt is RetentionCutoff measured from an instant the CALLER already holds.
//
// THE POINT IS ONE CLOCK READ, not two. A caller that derives a window from time.Now() and then
// asks RetentionCutoff() has read the clock twice, and the coverage comparison between them is
// only sound if no day boundary fell in between. In the default configuration the margin is
// exactly zero — retention_days is 31 and a month-to-date request on the 31st reaches precisely
// the 1st — so a request straddling local midnight computed its window for one month and its
// horizon for the next day, and reported a complete total as one day short. Sub-millisecond, once
// a month, and indistinguishable from the real disclosure when it happens.
//
// Passing the window's own instant makes the race structurally impossible rather than unlikely.
func (w *Writer) RetentionCutoffAt(now time.Time) time.Time {
	return w.store.retentionCutoff(now)
}

// Query returns every row whose minute falls in [from, to], inclusive at minute
// granularity.
//
// Reads only what the Writer has flushed — closed minutes. Prefer Window, which
// adds the open minute; this is the disk half on its own, kept separate so
// "only closed minutes reach disk" stays directly testable.
//
// RETURNS WHAT IT COULD NOT READ, as Caveats, beside the rows it did. Without them a day
// file that lost lines, or one whose read was abandoned part-way, produces exactly the same
// answer as a clean one — a short total labelled priced:true with no caveat anywhere in it.
// They travel with the rows rather than on the Writer because they describe THIS read; see
// Caveats for the two readers that swapped them.
//
// EXPORTED WITH NO NON-TEST CALLER, and it stays that way: it is the named disk half of this
// package's contract, cited by name from sessionapi's ledgerSnapshot, from config's
// cost-ledger tests and from usage's snapshot tests as the thing that walks day files.
//
// CONTEXT IS HONOURED BETWEEN DAY FILES, not inside one. A 7d window is up to eight
// os.Open-plus-scan calls against an operator-configured path — an NFS or FUSE mount in
// the worst case — and the client that asked may be gone before the second one. Checked
// per day rather than per line because a single day file is bounded (maxLabelsPerMinute
// x 1440 rows) while the number of them is the caller's to choose, so the per-day check
// is what bounds the work an abandoned read can still do. A cancelled read returns the
// context's error and NO rows: a partial day would be a short total with nothing saying
// it was short, which is the failure SkippedLines exists to stop being invisible.
func (w *Writer) Query(ctx context.Context, from, to time.Time) ([]Row, Caveats, error) {
	fromMin, toMin := span(from, to)

	var out []Row
	var caveats Caveats
	// Walk dates rather than globbing the directory: the read stays bounded by the
	// span the caller asked for instead of by how long the ledger has been running.
	//
	// store.dayOf, not the package dayOf: the day boundary is the LEDGER's, so a caller
	// handing this a UTC window gets the same day files as one handing it the equivalent
	// local window. The package dayOf preserves its argument's zone, so it would answer
	// whichever day the caller happened to spell — near midnight, a different file from the
	// one the row was written to.
	// AND BOUNDED BY WHAT COULD EXIST, not only by what was asked for. The walk is one os.Open per
	// day, so the caller's span alone set the amount of IO: Query(time.Time{}, now) is 106,751 opens
	// and 1.67s against a tmpdir — worse against the NFS or FUSE mount this doc names as the real
	// target. ctx is checked per day, so a cancelled request stops, but nothing made an absurd span
	// cheap for a caller who waits.
	//
	// LOSSLESS BY CONSTRUCTION, which is why it clamps rather than errors. No day file can exist
	// outside maxRetentionDays either side of the clock's day: newStore clamps retainDays to that
	// ceiling, and prune's own cutoff and horizon are both derived from retainDays. So a day outside
	// this band is a day whose file was never kept, and skipping it cannot shorten an answer.
	//
	// NOT CLAMPED TO retainDays, which would be tighter and would narrow real answers: prune runs at
	// startup and on day roll, so between rolls a file just outside the retention window is still
	// live and readable, and a caller asking a wider window should still see it.
	first, last := w.dayWalk(fromMin, toMin)
	for d := first; !d.After(last); d = d.AddDate(0, 0, 1) {
		if err := ctx.Err(); err != nil {
			// Before the first read too, so a request cancelled while it queued does no IO
			// at all. No caveats are returned with it: they describe an answer, and a
			// cancelled read is not one.
			return nil, Caveats{}, err
		}
		rows, issues, err := w.store.readDay(d)
		if err != nil {
			// Reported and skipped, not fatal: see Caveats.UnreadableDays. Warn per day, because
			// there can only be as many as the window has days and an operator whose ledger has
			// become unreadable needs the filename.
			slog.Warn("costledger: a day file could not be read; the total is short by whatever it held",
				"day", d.Format(dayLayout), "error", err,
				"effect", "the rest of the window is still answered, with Caveats.UnreadableDays set")
			caveats.UnreadableDays++
			continue
		}
		caveats.SkippedLines += int64(issues.skippedLines)
		if issues.truncated {
			caveats.TruncatedDays++
		}
		for _, r := range rows {
			if m := r.At.Truncate(time.Minute); m.Before(fromMin) || m.After(toMin) {
				continue
			}
			out = append(out, r)
		}
	}
	// THE ONE PLACE EVERY DISK ROW PASSES THROUGH, which is why the split is recovered here
	// and not at the four call sites above this in the stack. Window delegates to Query and
	// then appends only the unflushed minute — rows Record has already split — so enriching
	// there would leave Query's own callers blank, and enriching in sessionapi would leave
	// anything else that reads the ledger blank. A no-op with no rate table wired; see
	// repriceTiers for what it will and will not model.
	//
	// AFTER the window filter, so no work is done for rows nobody asked for.
	repriceTiers(out, w.rates)
	return out, caveats, nil
}

// dayWalk is the range of day files Query will open: the requested span, intersected with the days a
// file could exist for.
//
// Separate from Query so the bound is assertable without doing the IO — the thing worth pinning is
// how many days an absurd span walks, and counting os.Open calls from a test is not something this
// package exposes. Returns a first AFTER last when the span lies wholly outside the band, which the
// caller's `!d.After(last)` reads as an empty walk.
func (w *Writer) dayWalk(fromMin, toMin time.Time) (first, last time.Time) {
	today := w.store.dayOf(w.now())
	oldest := today.AddDate(0, 0, -maxRetentionDays)
	newest := today.AddDate(0, 0, maxRetentionDays)

	first, last = w.store.dayOf(fromMin), w.store.dayOf(toMin)
	if first.Before(oldest) {
		first = oldest
	}
	if last.After(newest) {
		last = newest
	}
	return first, last
}

// span normalises a caller's range to inclusive minute bounds.
//
// A reversed range is a caller mistake, not a reason to return nothing: swapping
// answers the question that was meant instead of an empty result a client would
// render as "no spend". Shared by Query and Window so one answer cannot be
// assembled from two different readings of the same range.
func span(from, to time.Time) (time.Time, time.Time) {
	if to.Before(from) {
		from, to = to, from
	}
	return from.Truncate(time.Minute), to.Truncate(time.Minute)
}

// dayOf is the LEDGER DAY of an instant: the local calendar date it falls on, carried
// as an instant at dayHour so the date can be formatted and walked. Local, not UTC:
// "today" means the operator's day, and a laptop that crosses a timezone must not have
// its day reset mid-afternoon.
//
// A DAY IDENTIFIER, NOT A DAY BOUNDARY. Nothing here may treat the result as the first
// instant of the day: row filtering is done on the caller's own minute bounds (see
// span), and this function decides only WHICH DAY FILE an instant belongs to.
//
// NOT LOCAL MIDNIGHT, which is a date that does not exist in every zone. Where a DST
// transition falls AT 00:00 — America/Havana, America/Santiago, Asia/Beirut and others — the
// spring-forward day has no midnight at all, and time.Date resolves a time inside the gap
// onto the far side of it. Measured, with midnight as the representation:
//
//	dayOf(2026-03-08 09:30 America/Havana)   = 2026-03-07 23:00  → the WRONG DATE
//	dayOf(2026-09-06 09:30 America/Santiago) = 2026-09-05 23:00  → the WRONG DATE
//
// Every consequence of that one mapping was money: store.path names the row's file after the
// previous day, so the file a reader for that date opens never existed; the day walk in Query
// steps from a normalised midnight and opens 2026-03-07 TWICE, counting a Havana laptop's
// spend for that day twice over; and in Asia/Beirut, where the normalisation goes the other
// way, AddDate steps PAST the transition day so its spend is absent from the answer entirely.
// A zone whose transition is at 02:00 — America/New_York — is unaffected, which is why a
// fixed-offset test zone cannot express any of this. See dst_test.go.
//
// AddDate on the result is how the day walk advances, and it is DST-correct where a 24h
// addition is not: a day is not always 24 hours long, so adding one would drift by the
// transition's offset and eventually skip or repeat a date.
func dayOf(t time.Time) time.Time {
	y, m, d := t.Date()
	return dayNoon(y, m, d, t.Location())
}

// dayHour is the hour of day every ledger day is represented at.
//
// NOON, because it is the hour furthest from both midnights. A zone's DST transition
// moves the clock by an hour (Australia/Lord_Howe by thirty minutes), so no transition
// can move noon into a different DATE — it would take a twelve-hour shift, which no zone
// has. Midnight is the opposite: it sits one hour from the previous day, which is exactly
// how a midnight inside a DST gap normalised backwards into it.
//
// Nothing depends on the value being 12 rather than any other mid-afternoon hour; it
// depends on it being an hour that EXISTS on every local date, in every zone, forever.
const dayHour = 12

// dayNoon builds the ledger day for a calendar date in loc.
//
// The single place the dayHour convention is applied, so dayOf and store.dayFromName —
// the two directions of the same mapping, which prune compares against each other —
// cannot drift.
func dayNoon(y int, m time.Month, d int, loc *time.Location) time.Time {
	return time.Date(y, m, d, dayHour, 0, 0, 0, loc)
}

// Fold sums rows into one total, an optional per-label series, and the cost that
// series does not account for, in the shape /v1/usage already serves.
//
// Returns usage.Counts and a Group-keyed map so an HTTP handler need not branch on
// whether the data came from the ring or from disk. Summation is Counts.Add, so a
// field added there is carried here with no edit — the same property that keeps
// fold() and abctl's (other)-band collapse correct.
//
// The THIRD return is the dollars of every row this grouping had to skip, for
// usage.Snapshot.UngroupedCostMicros. A row with no value for the requested axis counts
// toward the total and cannot be a series key — a gateway-priced /v1/embeddings
// response is stored with Model "" — so summing the series gives a smaller number than
// the total beside it, and this is the size of that difference. It is a usage.CostSum
// rather than a bare int64 so that a residual which reached the int64 ceiling arrives at
// SetUngroupedCost saying so, instead of arriving negative and being published as this
// process having lost track of its own arithmetic.
//
// ZERO FOR AN AXIS THIS SOURCE CANNOT GROUP BY AT ALL, which is a stronger condition
// than usage.Group.Reconcilable and the fix for a defect that reported real spend as
// entirely unaccounted for. Reconcilable answers for the ring, and is false only for
// GroupNone and GroupPlugin; a ledger row also carries no session id and no status, so
// group=session and group=status produced NO series and a residual equal to the whole
// total — "none of this money can be attributed", over a window where every dollar was
// attributable to an endpoint, a model and an agent. Both conditions are now required:
// Groupable, because a residual against an absent breakdown is meaningless, and
// Reconcilable, because GroupPlugin's series is not a partition. See Groupable, and see
// sessionapi's ledgerSnapshot for how a client tells "cannot group by that" from "the
// breakdown is complete".
//
// Counted HERE rather than left to the caller as totals-minus-series, because this is
// the loop that decides what to skip. A caller deriving it would be re-deriving a
// number this function already knows exactly, and would get it wrong for any axis whose
// series is not a partition.
func Fold(rows []Row, group usage.Group) (totals usage.Counts, series map[string]usage.Counts, ungroupedCost, ungroupedAvoided usage.CostSum) {
	// A usage.CostSum, not an int64, and returned as one: rows come off DISK, so nothing
	// between a hand-edited day file and this loop bounds r.CostMicros, and two rows near the
	// ceiling turn a bare int64 residual negative — which sessionapi hands to
	// Snapshot.SetUngroupedCost, which reads a negative residual as this process being wrong
	// about its own arithmetic. See usage.CostSum.
	// Named results, and two of them are CostSums of the same type — so the names are what
	// keep a caller from swapping them. ungroupedAvoided is the residual for the OTHER money
	// field, added when Counts.AvoidedMicros made a per-label "saved" breakdown possible:
	// without it that breakdown sums to less than the total with nothing to explain the gap.
	// Especially reachable here, because Writer.Record admits a saving-only row that has no
	// model to label. See usage.Snapshot.UngroupedAvoidedMicros.
	//
	// BOTH predicates, and the source's one first: a residual is only meaningful where
	// this source can produce a breakdown to be the residual OF.
	reconcilable := Groupable(group) && group.Reconcilable()
	for _, r := range rows {
		totals.Add(r.Counts)
		label, ok := labelFor(r, group)
		if !ok {
			if reconcilable {
				ungroupedCost.Add(r.CostMicros)
				ungroupedAvoided.Add(r.AvoidedMicros)
			}
			continue
		}
		if series == nil {
			series = map[string]usage.Counts{}
		}
		cur := series[label]
		cur.Add(r.Counts)
		series[label] = cur
	}
	// BOUNDED BEFORE IT IS SERVED. These rows come off disk, so the number of distinct
	// (endpoint, model, agent) keys in one response is however many a month of traffic
	// produced. Leaving the (other) band to the client is the wrong place for it: the bytes
	// are already on the wire by then, and every other consumer of /v1/usage would get an
	// unbounded response.
	//
	// AFTER the totals and the residual, so the cap changes what the breakdown NAMES and
	// nothing about what it sums to. usage.CapSeries folds the rest into the same (other) band
	// the ring produces, so a client renders one band and not two.
	series = usage.CapSeries(series, usage.MaxSeriesInResponse)
	return totals, series, ungroupedCost, ungroupedAvoided
}

// Groupable reports whether a LEDGER ROW can carry a value for this axis — that is,
// whether a breakdown by it is answerable from this source at all.
//
// RECONCILABILITY IS A PROPERTY OF THE SOURCE, NOT OF THE GROUP, which is why this predicate
// exists beside usage.Group.Reconcilable rather than reusing it. Reconcilable answers for the
// RING, whose buckets keep a status series and a plugin series and can be filtered by
// session; it is false only for GroupNone and GroupPlugin. The ledger's rows are (endpoint,
// model, agent, provenance) per minute and carry none of the other three: a session is a
// laptop-lifetime concept, and status and plugin composition are per-request facts a
// per-minute row cannot represent without one entry per combination. So a group the ring can
// reconcile may be unanswerable here, and deciding a residual with the ring's predicate turns
// GET /v1/usage?window=today&group=status into a response whose residual equals its entire
// total. See Fold.
//
// EXPORTED because sessionapi has to ask the same question one layer up, to report the
// grouping the ledger could actually APPLY rather than the one that was requested. A
// client comparing the two learns "this source cannot break down by that axis", which is
// a different fact from "the breakdown is complete" and from "the breakdown fell short by
// this much" — and all three have to be distinguishable from the response alone.
//
// KEPT HONEST BY A TEST rather than by matching comments: labelFor is the loop that
// actually produces the keys, and TestGroupable_MatchesWhatLabelForCanActuallyProduce
// asserts this switch and that one agree for every axis usage defines. A new axis fails
// that test until somebody decides which side it belongs on.
func Groupable(group usage.Group) bool {
	switch group {
	// Every axis a Row has a field for. GroupMethod is the model series under an older
	// name — see labelFor.
	case usage.GroupModel, usage.GroupMethod, usage.GroupEndpoint, usage.GroupAgent:
		return true
	}
	// GroupNone included: it asks for no breakdown, so there is nothing to answer and
	// nothing to reconcile, which is the same conclusion Reconcilable reaches for it.
	return false
}

// unknownAgentLabel is the reserved DISPLAY bucket for traffic that carried no
// User-Agent. The ring and the ledger both serve group=agent, and two spellings of
// "unattributed" would surface as two rows in any client that merged them, each
// holding half the unattributed spend.
//
// DEFINED BY pipeline rather than duplicated here, so that agreement is the
// compiler's to keep rather than a comment's to assert. It was a matching literal in
// both packages until the axis was wired into abctl, which would have made a third.
//
// NOT an agent name, and a consumer must not present it as one — the row is
// unattributed traffic, not a program that spent money.
const unknownAgentLabel = pipeline.UnknownClientLabel

// labelFor picks the grouping key, returning ok=false when the row carries no
// value for that axis — so an empty key never becomes a blank row, the same guard
// the live foldInto applies. The row still counts toward totals either way.
//
// usage.GroupSession, GroupStatus and GroupPlugin fall to the default and produce
// no series, because a ledger row carries none of the three: a session is a
// laptop-lifetime concept, and status and plugin composition are per-request facts
// a per-minute row cannot represent without one entry per combination. Returning
// no series is the honest answer; an empty map that a client renders as "no
// breakdown" would be the same answer said less clearly.
//
// usage.GroupAgent is the ONE axis that maps an absent value to a label rather than
// dropping the row from the series, so it returns early instead of reaching that
// guard. See its case.
func labelFor(r Row, group usage.Group) (string, bool) {
	var v string
	switch group {
	// Both spellings read Model: group=method shipped as the model series under a
	// wrong name, and the alias must behave identically here too.
	case usage.GroupModel, usage.GroupMethod:
		v = r.Model
	case usage.GroupEndpoint:
		v = r.Endpoint
	// Agent maps "" to a label instead of dropping the row.
	//
	// The non-empty guard is right for model and endpoint: a row with no model is not
	// an inference row, so it is not ABOUT that axis and excluding it is honest. An
	// absent agent is a different thing — that spend certainly happened and certainly
	// belongs somewhere in a per-agent breakdown, so dropping it would leave a client
	// unable to reconcile a per-agent table against the total printed beside it. The
	// live aggregator makes the same call: byAgent folds unconditionally where
	// byEndpoint and byMethod are guarded on non-empty.
	//
	// This is also the join point between the two representations: the ledger stores
	// absence losslessly as "", and here it becomes the same display string /v1/usage
	// returns from the ring, so group=agent answers identically from either source.
	// See Row.Agent.
	case usage.GroupAgent:
		if r.Agent == "" {
			return unknownAgentLabel, true
		}
		return r.Agent, true
	default:
		return "", false
	}
	return v, v != ""
}
