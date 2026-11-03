package sessionapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
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
//	agent       an agent, as group=agent reports it (versionless: a versioned label is
//	            read as its agent); narrows the window to that agent's traffic, and the
//	            response echoes it in "agent". A window served from the ring keeps its
//	            group for a recognised agent (pipeline.IsKnownAgent) with no session;
//	            any other is served with group none, and "group" says which. Refused
//	            past event.MaxLabelLen.
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

	spec, err := usage.ParseWindowSpec(r.URL.Query().Get("window"), s.clock())
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
	// agent= narrows to one agent's traffic, by the label group=agent reports. A versioned label
	// is read as its agent, since group=agent reported those until versions were folded.
	agent := r.URL.Query().Get("agent")
	if len(agent) > maxAgentLabelLen {
		writeUsageError(w, errAgentLabelTooLong)
		return
	}
	agent = pipeline.AgentName(agent)

	var snap usage.Snapshot
	switch {
	case !spec.Symbolic():
		snap = s.ringSnapshot(spec.Dur, resolution, sessionID, agent, group)
	case s.ledger == nil:
		// No ledger: Kubernetes by design, where files in a pod are the wrong sink
		// and the central collector is the right one. Serve what the ring HAS rather
		// than 400 — an agentop cost view must degrade to a shorter window, not fail —
		// and Snapshot reports the window actually served, so the client never
		// mislabels a 6-hour figure as a day's.
		//
		// CLAMPED TO THE WINDOW ASKED FOR, not just to the ring's maximum. Serving MaxWindow
		// unconditionally reached BACKWARDS past the start of the window: at 00:30 local,
		// window=today returned five and a half hours of YESTERDAY plus thirty minutes of today.
		// The label said 6h0m0s and was literally true, so nothing was mislabelled — it
		// over-reported today instead, which is the opposite of the degrade this comment claims.
		snap = s.ringSnapshot(servedSpan(spec), resolution, sessionID, agent, group)
	default:
		// r.Context(), so a client that hangs up stops the read. The ledger walks one day
		// file per day in the window — up to eight for window=7d, against a path an
		// operator configured and possibly a slow mount — and without this every abandoned
		// request kept reading to the end for nobody. See ledger.Query.
		snap, err = s.ledger.Snapshot(r.Context(), spec, group, agent)
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

// maxAgentLabelLen bounds agent=, which is caller-supplied; an agent label is at most a truncated
// User-Agent.
const maxAgentLabelLen = event.MaxLabelLen

var errAgentLabelTooLong = usageError{"agent label too long"}

// ringSnapshot is the ring's answer, narrowed to one agent when agent is set.
func (s *Server) ringSnapshot(window, resolution time.Duration, sessionID, agent string, group usage.Group) usage.Snapshot {
	if agent == "" {
		return s.usage.Snapshot(window, resolution, sessionID, group)
	}
	return s.usage.AgentSnapshot(window, resolution, sessionID, agent, group)
}

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
