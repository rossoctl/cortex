package sessionapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/rossoctl/cortex/core/cost/ledger"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/session"
)

// handleUsage serves GET /v1/usage — time-bucketed volume, error, latency and cost
// aggregates for charting.
//
// Query parameters:
//
//	window      10m (default), or any multiple of the bucket width up to the ring's
//	            maximum, or one of the three SYMBOLIC windows: "today" (LOCAL midnight to
//	            now, so a laptop crossing a timezone does not reset its day
//	            mid-afternoon), "7d" (a rolling 7x24h) and "month" (the start of the
//	            LOCAL month to now, which is month-to-date and grows through the month
//	            rather than being a fixed span). Symbolic windows are served from the
//	            durable cost ledger; where it is off, the ring's maximum is served
//	            instead and the response's own "window" field names what was served.
//	resolution  bucket width to return; defaults to the 1m storage resolution. Folded
//	            here rather than in the client so every consumer gets the same
//	            arithmetic — see usage.fold for why latency cannot be folded naively.
//	session     session ID; omit for all sessions. REFUSED alongside a symbolic window:
//	            the ledger holds no session ids, and serving all-sessions data under a
//	            session label would be worse than refusing.
//	group       none (default), model, endpoint, session, agent, status, plugin, host.
//	            "method" is an alias for "model".
//
// THREE THINGS A CLIENT MUST NOT GET WRONG:
//
//  1. The two window kinds disagree about the same traffic, by design. A duration
//     window comes from the ring, which prices an unpriced request from the process
//     rate table and counts non-inference traffic; the ledger does neither. Never
//     subtract a ledger figure from a ring figure and present the result as spend.
//  2. priceableRequests is the coverage denominator, never requests — which counts
//     tool calls, health checks and tunnels that can never carry a price, so a client
//     using it would mark every total "partial" forever and train readers to ignore
//     the one caveat that matters.
//  3. priced:false means nothing was priced. Render "cost unavailable", never $0.00,
//     which reads as "this traffic was free".
//
// UNAUTHENTICATED, like every endpoint on this listener; bind it in-cluster only,
// never behind ingress. It carries no message content, but it is not free of
// information: group=model (and its "method" alias) exposes the model names in use,
// including any internal or preview one an operator is testing against; group=plugin
// exposes the active pipeline composition, which /v1/pipeline already publishes in
// full; group=endpoint and group=host both disclose which upstreams this workload
// calls — the LLM endpoint, each MCP tool, any internal service — which sketches the
// deployment's dependency graph; group=agent discloses which coding agents at which
// versions run on a workstation; group=session pairs client-chosen ids with spend; and
// the cost figures disclose spend.
//
// THE SYMBOLIC WINDOWS RAISE THAT MATERIALLY and are the first thing here that would
// need a credential if this port were ever exposed. Everything else is bounded by the
// six-hour ring — the worst an unauthenticated reader takes is an afternoon from a
// process that happened to be up. window=7d answers "what has this developer's agent
// cost over a week", which is a fact about a person. Two things bound it and neither
// is authentication: the ledger needs a durable location (so in Kubernetes it is
// usually off, and the --local config pins every listener to loopback), and session=
// is refused for a symbolic window, so a week of spend cannot be pinned to one named
// session through this path.
//
// COST IS NOT SINGLE-SOURCED, and this comment deliberately does not claim it is.
// core/cost/settle settles the figure most requests arrive with, but the aggregator
// still prices independently when none is present, so the two can answer differently
// about the same request. Do not write here that cost is computed in one place.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if s.usage == nil {
		// Aggregation not wired up (session store disabled, or an older binary
		// composition). 404 rather than an empty snapshot: "no such channel" and
		// "channel with nothing in it" are different answers, and a client that
		// gets zeros would render a flat chart implying idle traffic.
		http.Error(w, `{"error":"usage aggregation not enabled"}`, http.StatusNotFound)
		return
	}

	spec, err := usage.ParseWindowSpec(r.URL.Query().Get("window"), time.Now())
	if err != nil {
		writeUsageError(w, err)
		return
	}
	// The span resolution is validated against is the one that can actually be
	// SERVED, not the one requested. A symbolic window is served either as one
	// ledger-backed bucket, where the requested resolution is not read at all, or as
	// the ring's maximum; validating "7d" against seven days would accept a 24-hour
	// bucket and then return a response whose own BucketSeconds contradicted it.
	// WHICH SPAN A RESOLUTION IS JUDGED AGAINST DEPENDS ON WHO WILL SERVE IT.
	//
	//   - a duration window: the ring slices it, so the caller's own span is the bound.
	//   - a symbolic window WITH a ledger: one bucket spanning the whole window, and the resolution
	//     is never read — so only the storage-bucket checks can mean anything. Judging it against the
	//     ring's maximum refused requests that were fine: window=7d&resolution=7m came back 400
	//     because 6h does not divide by 7m, while 7m divides seven days exactly and the code path
	//     that cared was not going to run.
	//   - a symbolic window with NO ledger: the ring slices what it HAS, which before 06:00 local is
	//     shorter than its maximum — so the bound is that span, not the maximum, and the rejection is
	//     restated below because the caller never named it.
	resSpan, oneBucket := resolutionSpan(spec, s.ledger != nil)
	var resolution time.Duration
	if oneBucket {
		resolution, err = usage.ParseResolutionUnbounded(r.URL.Query().Get("resolution"))
	} else {
		resolution, err = usage.ParseResolution(r.URL.Query().Get("resolution"), resSpan)
	}
	if err != nil {
		// RESTATED FOR A SYMBOLIC WINDOW, because ParseResolution can only name the span it was
		// GIVEN: asking for window=7d&resolution=24h came back "resolution 24h exceeds the 6h0m0s
		// window", which names a window the caller never asked for, for a parameter the ledger path
		// does not read. The bound is real — see resSpan above — but the reason has to travel with it.
		//
		// KEYED ON THE ERROR, NOT ON THE WINDOW KIND. Conditioning on Symbolic() alone overwrote the
		// reason for every OTHER rejection — 30s is finer than the bucket, 90s is not a multiple, abc
		// does not parse — and told all three of them about a 6h ceiling they never reached. That is
		// the same defect this restatement exists to fix.
		//
		// AND THERE ARE TWO WINDOW-DEPENDENT REJECTIONS, NOT ONE, which the first version of this
		// missed: the span being too short, and the span not dividing evenly. Asking
		// usage.ResolutionWindowCause rather than naming a sentinel means a third one is classified
		// where it is constructed instead of here.
		//
		// AND A MESSAGE PER REASON, which the previous version did not have: one text written for "too
		// coarse" was used for both, so ?window=7d&resolution=7m — 51x FINER than the ceiling that text
		// quotes — was told that 6h is the coarsest resolution available. Advice the request already
		// satisfied, about a bound it never hit, from the very restatement that exists to stop exactly
		// that. Switching on the cause is what makes reusing the wrong text impossible rather than
		// merely discouraged.
		//
		// AND IT INTERPOLATES NOTHING FROM THE QUERY. The first version of this message put the raw
		// resolution parameter in the body, which is a reflection primitive on an unauthenticated
		// endpoint — see writeUsageError, whose whole doc is that requirement, and note that the
		// branch runs precisely BECAUSE those bytes failed validation. spec.Label is safe here and
		// only here: it is one of two constants on the symbolic path, while for a duration window
		// ParseWindowSpec echoes the caller's own spelling into it.
		if spec.Symbolic() {
			switch usage.ResolutionWindowCause(err) {
			case usage.ResolutionTooCoarseForWindow:
				err = fmt.Errorf("resolution too coarse for window=%s: with no cost ledger this window "+
					"is served from the ring, which has %s of it, so %s is the coarsest resolution "+
					"available",
					spec.Label, resSpan, resSpan)
			case usage.ResolutionIndivisibleByWindow:
				// THE SERVED SPAN, NOT MaxWindow. Before 06:00 local the ring has less of today than
				// its maximum, and quoting the maximum would send the caller to pick a resolution that
				// divides six hours when the span they will get is ninety minutes.
				err = fmt.Errorf("resolution does not divide evenly for window=%s: with no cost ledger "+
					"this window is served from the ring, which has %s of it, and a resolution that "+
					"does not divide that span leaves the newest bucket narrower than the width "+
					"reported for it — pick one that divides %s (%s always does)",
					spec.Label, resSpan, resSpan, usage.BucketWidth)
			case usage.ResolutionWindowNone:
				// About the resolution alone. Its own wording is already right.
			}
		}
		writeUsageError(w, err)
		return
	}
	group, err := usage.ParseGroup(r.URL.Query().Get("group"))
	if err != nil {
		writeUsageError(w, err)
		return
	}
	sessionID := r.URL.Query().Get("session")
	if len(sessionID) > session.MaxSessionIDLen {
		writeUsageError(w, errSessionIDTooLong)
		return
	}
	// REJECTED rather than ignored, and rejected whether or not a ledger exists.
	//
	// A ledger row carries no session id: a session is a laptop-lifetime concept
	// while the ledger is a day-lifetime one, and persisting a client-supplied id per
	// minute would both grow the row key without bound and put an identifier of the
	// client's choosing on disk. So the filter cannot be applied — and serving
	// all-sessions data under a session label would be a wrong number wearing a right
	// label, which is the single failure this whole branch keeps refusing.
	//
	// Unconditional, even where the ring COULD answer for one session over six hours,
	// because the alternative is an API whose behaviour depends on the deployment: the
	// same request would 400 on a laptop and degrade in Kubernetes, and a client
	// cannot code against that. Asking for a duration window instead is one edit and
	// the error says so.
	if sessionID != "" && spec.Symbolic() {
		writeUsageError(w, errSessionWithSymbolicWindow)
		return
	}

	var snap usage.Snapshot
	switch {
	case !spec.Symbolic():
		snap = s.usage.Snapshot(spec.Dur, resolution, sessionID, group)
	case s.ledger == nil:
		// No ledger: Kubernetes by design, where files in a pod are the wrong sink
		// and the central collector is the right one. Serve what the ring HAS rather
		// than 400 — an abctl cost view must degrade to a shorter window, not fail —
		// and Snapshot reports the window actually served, so the client never
		// mislabels a 6-hour figure as a day's.
		//
		// CLAMPED TO THE WINDOW ASKED FOR, not just to the ring's maximum. Serving MaxWindow
		// unconditionally reached BACKWARDS past the start of the window: at 00:30 local,
		// window=today returned five and a half hours of YESTERDAY plus thirty minutes of today.
		// The label said 6h0m0s and was literally true, so nothing was mislabelled — it
		// over-reported today instead, which is the opposite of the degrade this comment claims.
		snap = s.usage.Snapshot(servedSpan(spec), resolution, sessionID, group)
	default:
		// r.Context(), so a client that hangs up stops the read. The ledger walks one day
		// file per day in the window — up to eight for window=7d, against a path an
		// operator configured and possibly a slow mount — and without this every abandoned
		// request kept reading to the end for nobody. See ledger.Query.
		snap, err = s.ledgerSnapshot(r.Context(), spec, group)
		if err != nil {
			// A read failure is not a client error and must not look like one.
			//
			// CANCELLATION IS NOT A FAULT, and it is separated out because it would otherwise
			// be the loudest line in the log on the surface most likely to produce it: a chart
			// that re-requests on every keystroke cancels its own in-flight reads, and a Warn
			// per cancelled request would teach an operator to filter out the message that also
			// reports a genuinely unreadable ledger. The 503 is still written — net/http would
			// otherwise send an empty 200 body, which is not decodable JSON for the rare
			// cancellation that is not a vanished client.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				slog.Debug("sessionapi: cost ledger read abandoned; the request went away",
					"window", spec.Label, "error", err)
			} else {
				slog.Warn("sessionapi: cost ledger read failed", "window", spec.Label, "error", err)
			}
			http.Error(w, `{"error":"cost history unavailable"}`, http.StatusServiceUnavailable)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(snap); err != nil {
		slog.Debug("sessionapi: usage encode failed", "error", err)
	}
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

var errSessionIDTooLong = usageError{"session id too long"}

// errSessionWithSymbolicWindow refuses session= alongside window=today|7d|month. A fixed
// string, like every other message this endpoint returns — see writeUsageError.
//
// EVERY SYMBOLIC WINDOW IS NAMED, because a caller who asked for the one the message omits
// reads it as being about a different request than the one they made. "month" joined the
// symbolic set without joining this list, so window=month&session= was refused for
// "(today, 7d)". TestUsageErrorNamesEverySymbolicWindow keeps the two in step.
var errSessionWithSymbolicWindow = usageError{
	"session= cannot be combined with a symbolic window (today, 7d, month); " +
		"the durable cost ledger holds no session ids — ask for a duration window such as 1h or 6h"}

// ledgerSnapshot builds a Snapshot from persisted rows.
//
// ONE bucket spanning the whole window, not a series at the requested resolution.
// The ledger exists to answer "what did today cost", and a client wanting a shaped
// chart asks for a ring window instead — synthesising per-minute buckets from disk
// for a 7-day span would read millions of rows to draw a chart nothing requests.
// BucketSeconds reports the real span so a client cannot mistake it for a fine
// series.
//
// Reads through ledger.Window, never Query: the ledger's day files hold only
// CLOSED minutes, so the minute currently accumulating is in the writer's memory
// and Query alone would omit it. That omission is not small in the case this
// endpoint exists for — a session whose whole conversation fit inside one minute has
// nothing on disk at all, and the response would have said priced:false over real
// spend. Window composes the two halves and guarantees no minute is in both; see its
// doc.
//
// Takes no session id: handleUsage rejects that combination before reaching here,
// for the reason recorded at the guard.
//
// Takes the REQUEST's context, so the day-file walk stops when the caller does. Threaded
// rather than context.Background() because this is the only work this endpoint does that
// is neither bounded nor in memory.
//
// UnpricedBy and PricedBy are deliberately absent. Provenance IS in the row key, so
// PricedBy is reconstructible and a later change can add it; UnpricedBy needs the
// endpoint-and-model pair of the requests that could NOT be priced, which a row
// carrying only its own labels cannot distinguish from a priced one of the same
// pair. Emitting one map and not the other would read as "no pricing gaps here",
// which is a claim the rows do not support — the gap is still visible, in
// Totals.PricedRequests against Totals.PriceableRequests.
func (s *Server) ledgerSnapshot(ctx context.Context, spec usage.Spec, group usage.Group) (usage.Snapshot, error) {
	rows, caveats, err := s.ledger.Window(ctx, spec.From, spec.To)
	if err != nil {
		return usage.Snapshot{}, err
	}
	// HOW MUCH OF THE WINDOW THE CONFIGURATION CANNOT REACH, computed here rather than in the
	// ledger because it is a fact about the REQUEST measured against the ledger's horizon, and
	// the ledger does not know what was asked for. See usage.Snapshot.DaysOutsideRetention for
	// why this is coverage rather than damage.
	//
	// MEASURED AGAINST THE WINDOW'S OWN INSTANT, not a second reading of the clock.
	// RetentionCutoff() would call time.Now() again, and the two sides of this comparison are only
	// sound if no day boundary fell between the two reads — a margin that is exactly zero in the
	// default configuration, so a month-to-date request crossing local midnight reported a
	// complete total as one day short. spec.To is the instant ParseWindowSpec built this window
	// from, and this path is symbolic-only, so it is always set: the Window call above already
	// depends on it.
	outside := daysOutsideRetention(spec.From, s.ledger.RetentionCutoffAt(spec.To))
	// THE GROUPING THIS SOURCE CAN APPLY, which is not always the one that was asked for,
	// and the response says which it was.
	//
	// A ledger row is (endpoint, model, agent, provenance) per minute, so group=session,
	// group=status and group=plugin have no column to key on here — while the ring, which
	// serves the same axes over a duration window, answers all three. Echoing the
	// requested group over an empty series made those two states indistinguishable from
	// the response: a client asking for status got group:"status" with series:null and no
	// way to tell "this source cannot break down by status" from "there was no traffic".
	// Worse, Fold used to publish a residual for them, so the response also claimed 100%
	// of its own total was unaccounted for. See ledger.Groupable.
	//
	// Reported as the grouping IN EFFECT rather than refused with a 400, because a
	// rejection would have to be conditional on a ledger being wired up at all — the same
	// request is served from the ring in Kubernetes, where it is answerable — and an API
	// whose validity depends on the deployment is one a client cannot code against. That
	// is the argument the session= guard above makes in the other direction, and it is
	// load-bearing in both.
	applied := group
	if !ledger.Groupable(group) {
		applied = usage.GroupNone
	}
	totals, series, ungroupedCost, ungroupedAvoided := ledger.Fold(rows, applied)
	// Carried onto the totals BEFORE the snapshot is built, not left to
	// SetUngroupedCost's own assignment below. This response's single bucket is a copy of
	// totals, so setting the flag afterwards would mark Totals as a bound while the bucket
	// holding the same figure claimed to be exact — and a client charting the bucket would
	// never see it. The ring's Snapshot has the opposite shape (many buckets, one
	// cross-bucket residual), which is why the setter flags Totals there and this flags
	// both here.
	// Either residual having clamped makes every money figure in this response a bound, so
	// both are consulted: the flag means "read them all as bounds", and checking only one
	// would let a clamped avoided residual arrive beside a total claiming exactness.
	if ungroupedCost.Saturated || ungroupedAvoided.Saturated {
		totals.Saturated = true
	}
	// FROM THE READ THAT PRODUCED THEM, which is why they come back from Window rather
	// than off the ledger. They used to be two atomics on the Writer, sampled here right
	// after Window returned — so any other /v1/usage request landing between those two
	// calls handed this response its caveats. Measured: a reader of a day file holding one
	// undecodable line reported SkippedLines 0, while a reader of a CLEAN day reported 1.
	// A chart polling this endpoint is exactly the traffic that produces it.
	//
	// Surfaced here because the ledger having the numbers is not the same as a client
	// being able to see them. Until this, a day that lost lines produced a response
	// byte-identical to a clean one — a short total under priced:true — so the skip that
	// saved the rest of the day was invisible to everyone downstream of it.
	// THE WRITER'S OWN LOSSES COUNT TOO, and they are not in Caveats: Caveats describes what this
	// READ could not decode, while a dropped row never reached the file for any read to find. So a day
	// that lost a minute to ENOSPC or to a full queue under a stalled filesystem served a short total
	// under priced:true with nothing set — the silence this object exists to end, on the half that was
	// never wired up. Writer.Dropped's own doc named this as the change that belonged next.
	dropped := s.ledger.Dropped()

	var degraded *usage.Degraded
	if !caveats.Clean() || dropped > 0 {
		// EVERY FIELD Clean() TESTS, or the disclosure is emptier than the fault. Clean() is
		// `c == Caveats{}` over all three counters while this copied only two, so a read whose
		// only fault was an unopenable day file produced `"degraded":{}` — the caveat raised
		// and nothing in it. TestLedgerSnapshot_EveryCaveatFieldReachesTheWire keeps the two
		// structs in step by counting fields, because adding a fourth counter to Caveats would
		// otherwise reintroduce exactly this.
		degraded = degradedFrom(caveats)
		degraded.DroppedRowsTotal = dropped
	}
	// THE READ-SIDE WARNING IS PER READ; THE WRITE-SIDE ONE IS PER CHANGE. Logging both together made
	// one dropped row at 03:00 print "cost ledger read was incomplete" on every request for the life of
	// the process — around 86,000 lines a day at a chart's poll rate, on an endpoint anyone who can
	// reach the port can drive, and saying the READ was incomplete when the read was clean. The total
	// being short is true; the claim about this read is not.
	if !caveats.Clean() {
		// At Warn, and on every such read: a client may not render the field, and an operator with a
		// corrupt day file wants to hear about it once per read rather than never.
		slog.Warn("sessionapi: cost ledger read was incomplete — the total is short",
			"window", spec.Label, "skippedLines", caveats.SkippedLines,
			"truncatedDays", caveats.TruncatedDays,
			"unreadableDays", caveats.UnreadableDays)
	}
	// Monotonic, so Swap gives the previous high-water mark and only an INCREASE logs. A race between
	// two reads lets exactly one of them log, which is the property that matters.
	if prev := s.loggedDropped.Swap(dropped); dropped > prev {
		slog.Warn("sessionapi: the cost ledger writer has lost rows; every total from it is short",
			"droppedRowsTotal", dropped, "newlyDropped", dropped-prev,
			"cause", "an append failed, or the queue filled while the filesystem stalled",
			"effect", "reported cost is a floor; the figure is disclosed on every response as degraded.droppedRowsTotal")
	}
	snap := usage.Snapshot{
		Window:        spec.Label,
		BucketSeconds: bucketSecondsFor(spec.From, spec.To),
		// The grouping SERVED, on the same rule as Window above: a response says what it
		// actually did, and a client that asked for something else learns so by comparing.
		Group:    applied,
		Buckets:  []usage.Bucket{{At: spec.From, Counts: totals, Series: series}},
		Totals:   totals,
		Degraded: degraded,
		// Same rule as Aggregator.Snapshot: an inexact figure is still a figure, so a
		// window whose every request was a truncated stream reports priced:true over a
		// real total and discloses the caveat in Totals.IncompleteRequests. Withholding
		// the figure would render "cost unavailable" over dollars that are known.
		Priced:               totals.PricedRequests > 0,
		DaysOutsideRetention: outside,
	}
	// The dollars the breakdown above does not account for — a gateway-priced response the
	// inference parser could not read is stored with no model, so it counts toward Totals
	// and cannot be a group=model key. Set through the setter so a window with nothing to
	// disclose serialises no field at all, exactly like Degraded; see
	// usage.Snapshot.UngroupedCostMicros for what a client does with it.
	snap.SetUngroupedCost(ungroupedCost)
	// And the same for the saving, which is MORE likely to be unattributable than the cost:
	// Writer.Record admits a row whose only figure is an applied saving even when the
	// response carried no inference extension, so under group=model that row has no label at
	// all. See usage.Snapshot.UngroupedAvoidedMicros.
	snap.SetUngroupedAvoided(ungroupedAvoided)
	return snap, nil
}

// writeUsageError returns 400 with the validation message. Every message it can
// carry is a fixed string authored in this package or in core/cost/usage: none
// interpolates query input. That is a requirement, not an accident — this
// endpoint is unauthenticated, so reflecting caller-supplied bytes into a
// response body would hand an attacker a reflection primitive. Keep it that way
// when adding validation.
func writeUsageError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	if encErr := json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: err.Error()}); encErr != nil {
		slog.Debug("sessionapi: usage error encode failed", "error", encErr)
	}
}

// daysOutsideRetention is how many whole days of a window start before a retention cutoff.
//
// Zero when the window fits, which is the normal case and says nothing. Counted in DAYS because
// that is the unit the ledger stores and prunes in: a window reaching back part of a day past the
// cutoff has one date it cannot cover, not a fraction of one.
//
// BOTH ARGUMENTS IDENTIFY A DATE AND NEITHER IS A BOUND, which is where two defects lived.
// cutoff comes from costledger's retentionCutoff, and a ledger day is carried at NOON —
// ledger.dayOf's doc forbids reading it as the day's first instant — while from is a local
// MIDNIGHT, from usage.StartOfLocalDay or StartOfLocalMonth. Comparing them as instants made a
// month-to-date request report one day short of ITSELF: from sits twelve hours before the cutoff
// of the very date it starts on, so "from is earlier" was true and a floor turned it into 1. With
// retention_days defaulting to 31, deliberately equal to the longest month, that stamped the
// partial marker on a complete and correct total every 31-day month, and on the 9th of any month
// at the 9-day minimum.
//
// So both sides are reduced to DATES and differenced as dates. NEVER Truncate(24*time.Hour),
// which truncates on the absolute UTC-epoch axis rather than to a local date: east of Greenwich a
// local midnight and a local noon belong to different UTC dates, so the difference gained a day
// in Berlin and Tokyo while UTC, New_York and Auckland answered correctly. Neither a
// fixed-offset test zone nor a single-zone one can see that.
func daysOutsideRetention(from, cutoff time.Time) int64 {
	// The LEDGER's zone decides the grid: retention is counted in the ledger's own day files, so
	// the question is which of its dates the window reaches past, not which of the caller's.
	d := utcNoonOfDate(cutoff).Sub(utcNoonOfDate(from.In(cutoff.Location())))
	if n := int64(d / (24 * time.Hour)); n > 0 {
		return n
	}
	return 0
}

// utcNoonOfDate re-anchors a timestamp's calendar date at noon UTC, so two dates can be
// differenced as dates.
//
// NOON, and in UTC, for the same reason costledger carries its days at noon: UTC has no
// transitions, so the gap between two of these is always an exact multiple of 24 hours and the
// division below cannot be off by one — where a local date's length is 22, 23, 24 or 25 hours and
// subtracting local midnights drifts by the offset change.
func utcNoonOfDate(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 12, 0, 0, 0, time.UTC)
}

// degradedFrom is the whole of the Caveats-to-wire conversion, in one place so a field cannot be
// added on one side and forgotten on the other.
//
// Extracted rather than left inline because that is what makes it testable: the defect it replaces
// was a literal that copied two of three counters, which no test of the handler could see — a
// response saying "degraded" with an empty object looks like a client-side rendering problem, not a
// producer dropping a number. TestDegradedFrom_CarriesEveryCaveatField compares the two structs
// field by field through reflection, so a fourth counter fails here rather than shipping silently.
func degradedFrom(c ledger.Caveats) *usage.Degraded {
	return &usage.Degraded{
		SkippedLines:   c.SkippedLines,
		TruncatedDays:  c.TruncatedDays,
		UnreadableDays: c.UnreadableDays,
	}
}

// bucketSecondsFor is the length of a ledger window's single bucket, floored at one second.
//
// A ledger window returns ONE bucket spanning the whole window, so this is the window's own length —
// and truncating that to an int makes it ZERO in the first second of the local day, once a day, for
// every polling client. And `bucketSeconds` has NO omitempty — I claimed it did, twice, in the commit
// that added this floor — so a zero is serialised as `"bucketSeconds":0` for every client to divide
// by. That makes this floor the only defence rather than the second one, which is a reason to keep it
// rather than a reason to relax it.
//
// A separate function so the boundary is testable: the handler reads time.Now() directly, so there is
// no seam through which a test could stand at midnight.
func bucketSecondsFor(from, to time.Time) int {
	if s := int(to.Sub(from).Seconds()); s > 1 {
		return s
	}
	return 1
}

// servedSpan is how much of a symbolic window the RING can answer: the shorter of the window itself
// and the ring's maximum.
//
// A function so the boundary is testable — the handler reads time.Now() directly, so a test cannot
// stand at 00:30 through it, and 00:30 is the only interesting hour. Whole buckets are left to
// Snapshot, which truncates the span to a bucket count and reports what it covered, so a sub-minute
// window comes back as one bucket labelled a minute rather than as zero.
func servedSpan(spec usage.Spec) time.Duration {
	span := spec.To.Sub(spec.From)
	if span > usage.MaxWindow {
		return usage.MaxWindow
	}
	// TRUNCATED TO WHOLE BUCKETS, and that is not cosmetic: this span is now what a resolution is
	// validated against, and the raw one carries sub-second precision — 1h42m59.241861s at 01:42. A
	// divisibility check against that rejects EVERY resolution including the 1m default, so before
	// 06:00 local nothing was answerable at all. Measured, by shipping it: every request 400.
	//
	// Truncating also makes the bound exactly what gets served, because Snapshot counts whole buckets
	// the same way. Two roundings that have to agree, and this is the one that makes them.
	//
	// FLOORED AT ONE BUCKET, because truncation reaches zero in the first 60 seconds of the local day
	// and a zero bound refuses EVERY resolution — including the 1m default, so ?window=today returned
	// 400 for a full minute each day with no resolution parameter at all. That is a 400 on the one path
	// whose entire purpose is to degrade instead of failing. Snapshot clamps its bucket count to at
	// least one regardless, so a minute is what gets served either way; saying so here is what keeps
	// the bound and the service agreeing at the boundary rather than only away from it.
	if span < usage.BucketWidth {
		return usage.BucketWidth
	}
	return span.Truncate(usage.BucketWidth)
}

// resolutionSpan is the span a resolution must fit, and whether the window is answered as one bucket.
//
// THE SPAN ACTUALLY SLICED, WHICH IS NOT ALWAYS THE RING'S MAXIMUM. Validating against MaxWindow let
// through a resolution that the SERVED span does not divide: at 01:30 local, window=today with no
// ledger serves 90 minutes, and resolution=1h passed (1h divides 6h) and then produced two buckets —
// one full hour and a 30-minute remainder — both labelled 3600 seconds. fold puts the remainder LAST,
// so the newest bar reads double its real rate, which is verbatim the failure ParseResolution's
// divisibility guard exists to prevent: "the NEWEST bucket is a lie".
//
// REJECTED RATHER THAN ROUNDED, which is a real trade and worth recording. Rounding the span down to a
// whole multiple of the resolution keeps the accepted resolution set stable, but it drops up to one
// resolution period of the NEWEST data — and the still-filling block is the one an operator is
// watching (see fold). So it would fix a label by discarding the thing the label is for. The cost of
// rejecting is that a fixed request is invalid for part of the day, which the message says how to fix.
//
// A function so the choice is testable: the handler reads time.Now() directly, and the only
// interesting spans are before 06:00 local.
func resolutionSpan(spec usage.Spec, hasLedger bool) (span time.Duration, oneBucket bool) {
	switch {
	case !spec.Symbolic():
		return spec.Dur, false
	case hasLedger:
		// One bucket over the whole window; the resolution is never read.
		return 0, true
	default:
		return servedSpan(spec), false
	}
}
