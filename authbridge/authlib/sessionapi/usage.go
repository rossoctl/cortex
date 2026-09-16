package sessionapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/costledger"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// handleUsage serves GET /v1/usage — time-bucketed volume, error, latency and
// cost aggregates for charting.
//
// Query parameters:
//
//	window      10m (default), 1h, 6h — any multiple of the bucket width up to
//	            the ring's retained maximum — or the SYMBOLIC windows "today"
//	            (local midnight to now) and "7d" (a rolling 7x24h). A symbolic
//	            window is answered from the durable cost ledger, which is on for a
//	            local install and off in Kubernetes; where it is off, the ring's
//	            maximum window is served instead and the response's own "window"
//	            field names what was actually served, never what was asked for.
//	            "today" is LOCAL midnight because a laptop crossing a timezone
//	            must not have its day reset mid-afternoon.
//
//	            THE TWO KINDS OF WINDOW CAN DISAGREE ABOUT THE SAME TRAFFIC, and a
//	            client showing both — "$X today" beside "$Y /1h" is exactly that —
//	            has to expect it. A duration window is answered from the ring,
//	            which prices a request the parser left unpriced from the process
//	            rate table; the ledger does not, and records such a request as
//	            priceable-but-unpriced instead. The ring also counts non-inference
//	            traffic in requests where the ledger counts inference only. On the
//	            live pipeline inference-parser settles every inference response, so
//	            the dollar figures agree; a composition without it shows the gap
//	            (pricedRequests below priceableRequests) rather than a wrong number.
//	            Do not compute a difference between a ledger figure and a ring
//	            figure and present it as spend.
//	resolution  bucket width to return, e.g. 5m for a 1h window rendered as 12
//	            bars. Defaults to the 1m storage resolution. Folding is done
//	            here, not in the client, so every consumer gets the same
//	            arithmetic — see usage.fold for why latency in particular cannot
//	            be folded naively.
//	session     session ID; omit for all sessions combined. REJECTED alongside a
//	            symbolic window — the ledger holds no session ids, and serving
//	            all-sessions data under a session label would be worse than
//	            refusing. See the guard in handleUsage.
//	group       none (default), model, endpoint, session, agent, status, plugin.
//	            "method" is accepted as an alias for "model" — the series shipped
//	            under that name before it was clear the aggregator only ever
//	            populated it from the inference model. "session" is meant for
//	            session="" (all sessions), where one response answers for every
//	            session a client is listing.
//
// UNAUTHENTICATED, like every endpoint on this listener. Bind it on in-cluster
// addresses only, never behind ingress — the trust model is documented in
// authbridge/CLAUDE.md and applies here unchanged.
//
// This response is less sensitive than /v1/sessions, which serves raw prompts,
// completions and tool results. It carries no message content at all: only
// counts, timings and cost. But it is not free of information either, and five
// groupings leak deployment shape to anyone who can reach the port:
//
//   - group=model exposes the model names in use (claude-sonnet-5, and any
//     internal or preview model an operator is testing against). group=method is
//     an alias for it and exposes exactly the same thing.
//
//   - group=endpoint exposes every upstream host the proxy talked to, not only the
//     inference ones: the accumulator is populated for any response or denial
//     carrying a Host, with no inference guard, so MCP servers, A2A peers and tool
//     backends appear alongside model gateways — internal hostnames included. That
//     is deployment topology rather than model choice, which makes it the most
//     sensitive of the groupings.
//
//     It also means the two axes of one cost table have different denominators:
//     group=endpoint rows can carry requests with no tokens and no cost, while
//     group=model is inference-only because that accumulator requires a model name.
//     Their request totals will not reconcile, and that is correct rather than a
//     bug — but a client putting the two side by side has to say so.
//
//   - group=session exposes session identifiers, and attaches spend to each one.
//     /v1/sessions already lists the ids (along with the message content), so the
//     ids themselves are no new exposure on this listener; pairing them with cost
//     is.
//
//     Unlike every other grouping, its keys are not drawn from a vocabulary this
//     process controls: the id arrives from the client. How much it discloses is
//     therefore set off-host — an opaque uuid discloses nothing, an id derived
//     from a user, agent or ticket name discloses a great deal. That is why it is
//     not ranked against group=endpoint above rather than placed below it.
//
//   - group=agent exposes which coding agents, AT WHICH VERSIONS, run on the
//     operator's workstation, and attaches spend to each one. That is
//     fingerprinting-adjacent and the most PERSONAL of the groupings: the others
//     describe a deployment, this one describes a person's tooling. A reader learns
//     that this machine runs claude-code 2.1.14, and — combined with a symbolic
//     window — what that person's use of it has cost over a week. An outdated version
//     in the answer is also a hint about unpatched local software.
//
//     Unlike group=session, its keys ARE drawn from a vocabulary this process
//     controls: they are parsed from the User-Agent, so the values are predictable
//     rather than set off-host. That cuts both ways. It bounds what an unrecognised
//     agent can put in the response, but it does not make the axis less sensitive —
//     a predictable key that names software on someone's laptop discloses more than
//     an opaque id does, which is why this bullet sits below group=session rather
//     than above it.
//
//     The key is also CLIENT-ASSERTED and trivially spoofable, so nothing here may be
//     read as an authenticated statement about what called the proxy. Its accumulator
//     has no inference guard, so — like group=endpoint — its request denominator
//     differs from group=model's.
//
//   - group=plugin exposes the active pipeline composition — though /v1/pipeline
//     already publishes that in full, so this adds no new exposure.
//
// Cost figures also disclose spend, which is business-sensitive in a way raw
// request counts are not.
//
// And the symbolic windows raise that last exposure materially — the largest single
// increase in it on this endpoint. Until they existed, everything served here was
// bounded by the in-memory ring: six hours, gone on restart, so the worst an
// unauthenticated reader could take was an afternoon's traffic from a process that
// happened to be up. window=today and window=7d serve a DURABLE spend history from
// disk, so the same port now answers "what has this developer's agent cost over the
// last week", which is a business fact about a person and their project rather than
// a snapshot of current load. Combined with group=model and group=endpoint it also
// says which models and which gateways that money went to, over a week rather than
// over an afternoon.
//
// Two things bound it rather than remove it: the ledger is off in Kubernetes, so
// this reach exists only where the listener is already pinned to loopback (the
// --local config pins every listener to 127.0.0.1 for exactly this class of
// reason), and session= is refused for a symbolic window, so a week of spend cannot
// be attributed to one named session through this path. Neither is authentication.
// If this endpoint is ever exposed beyond loopback or beyond a cluster-internal
// address, the ledger-backed windows are the first thing that needs a credential.
//
// None of this changes the listener's existing posture; it is written down so the
// decision to expose it is a decision rather than an oversight.
//
// costMicros is populated from the figure authlib/costing settles for one
// response. It prefers the gateway's own post-discount cost header — the
// authoritative figure — and falls back to pricing the parsed token counters when
// the header is absent or reports 0, which every streamed response does.
//
// costing is its own package rather than logic inside the parser or inside a
// plugin, and deliberately so: a gateway's cost header is vendor-specific knowledge
// with no place in a provider-shaped body parser, and a ledger has no business
// deciding what a request cost. inference-parser CALLS it at the point the token
// counters are final, because that is the only place that knows when they are.
//
// Cost used to be decided in two places with two shapes — inside
// litellm-budget-track and again inside the usage aggregator — and the two could
// disagree about the same request: one could carry a token count with no money in a
// live abctl view while showing dollars here. litellm-budget-track now amends the
// settled record to enforce a budget rather than computing a figure of its own.
//
// Requests that arrive with no settled figure contribute no cost and appear as the gap
// between totals.pricedRequests and totals.priceableRequests. Where those differ the
// dollar total covers only the priced subset, so a client rendering it must
// present it as partial rather than complete. priced:false means nothing at all
// was priced — render "cost unavailable", never $0.00, which would read as "this
// traffic was free".
//
// PRICEABLE is the denominator, never requests. requests counts every proxied
// response, including MCP tool calls, health checks and tunnels, none of which can
// ever carry a price — so priced-over-requests never reaches parity and a client
// obeying it would mark every total "partial" forever, which trains a reader to
// ignore the one caveat that matters. This paragraph named the wrong pair until it
// was corrected; the agreeing statement is on totals.priceableRequests, and both this
// endpoint's own clients (abctl's spend strip and `abctl cost`) use the priceable
// pair.
//
// Traffic that carries no settled figure is still priced here, from the process
// rate table: the pricing resolver has landed, so modelled rates are no longer
// something that arrives later — usage.Aggregator.costOf resolves a rate for any
// request carrying a model and tokens but no cost record. See
// docs/superpowers/specs/2026-09-09-pricing-consolidation-design.md.
//
// Which means cost is NOT single-sourced, and this comment deliberately stops
// short of claiming that it is. authlib/costing settles the figure most requests
// arrive with, but the aggregator still prices independently when none is present,
// so the two can answer differently about the same request — and the aggregator's
// path does not go through costing's precedence rule at all. Collapsing them onto
// the settled figure alone is a later change; until it lands, do not write here
// that cost is computed in exactly one place, because it is not.
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
	resSpan := spec.Dur
	if spec.Symbolic() {
		resSpan = usage.MaxWindow
	}
	resolution, err := usage.ParseResolution(r.URL.Query().Get("resolution"), resSpan)
	if err != nil {
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
		snap = s.usage.Snapshot(usage.MaxWindow, resolution, sessionID, group)
	default:
		// r.Context(), so a client that hangs up stops the read. The ledger walks one day
		// file per day in the window — up to eight for window=7d, against a path an
		// operator configured and possibly a slow mount — and without this every abandoned
		// request kept reading to the end for nobody. See costledger.Query.
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

// errSessionWithSymbolicWindow refuses session= alongside window=today|7d. A fixed
// string, like every other message this endpoint returns — see writeUsageError.
var errSessionWithSymbolicWindow = usageError{
	"session= cannot be combined with a symbolic window (today, 7d); " +
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
// Reads through costledger.Window, never Query: the ledger's day files hold only
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
	// of its own total was unaccounted for. See costledger.Groupable.
	//
	// Reported as the grouping IN EFFECT rather than refused with a 400, because a
	// rejection would have to be conditional on a ledger being wired up at all — the same
	// request is served from the ring in Kubernetes, where it is answerable — and an API
	// whose validity depends on the deployment is one a client cannot code against. That
	// is the argument the session= guard above makes in the other direction, and it is
	// load-bearing in both.
	applied := group
	if !costledger.Groupable(group) {
		applied = usage.GroupNone
	}
	totals, series, ungrouped := costledger.Fold(rows, applied)
	// Carried onto the totals BEFORE the snapshot is built, not left to
	// SetUngroupedCost's own assignment below. This response's single bucket is a copy of
	// totals, so setting the flag afterwards would mark Totals as a bound while the bucket
	// holding the same figure claimed to be exact — and a client charting the bucket would
	// never see it. The ring's Snapshot has the opposite shape (many buckets, one
	// cross-bucket residual), which is why the setter flags Totals there and this flags
	// both here.
	if ungrouped.Saturated {
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
	var degraded *usage.Degraded
	if !caveats.Clean() {
		degraded = &usage.Degraded{SkippedLines: caveats.SkippedLines, TruncatedDays: caveats.TruncatedDays}
		// At Warn, and unconditionally: a client may not render the field, and an operator
		// with a corrupt day file wants to hear about it once per read rather than never.
		slog.Warn("sessionapi: cost ledger read was incomplete — the total is short",
			"window", spec.Label, "skippedLines", caveats.SkippedLines,
			"truncatedDays", caveats.TruncatedDays)
	}
	snap := usage.Snapshot{
		Window:        spec.Label,
		BucketSeconds: int(spec.To.Sub(spec.From).Seconds()),
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
		Priced: totals.PricedRequests > 0,
	}
	// The dollars the breakdown above does not account for — a gateway-priced response the
	// inference parser could not read is stored with no model, so it counts toward Totals
	// and cannot be a group=model key. Set through the setter so a window with nothing to
	// disclose serialises no field at all, exactly like Degraded; see
	// usage.Snapshot.UngroupedCostMicros for what a client does with it.
	snap.SetUngroupedCost(ungrouped)
	return snap, nil
}

// writeUsageError returns 400 with the validation message. Every message it can
// carry is a fixed string authored in this package or in authlib/usage: none
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
