package usage

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// BucketScope says whether ScopeToAgent narrows the per-bucket series as well as the
// window-level figures.
//
// AN EXPLICIT PARAMETER RATHER THAN A DEFAULT, because the two callers want different things
// and the cheaper one is not the safe one. agentop's usage pane renders a CHART from Buckets, so
// leaving them whole-window would draw every agent's traffic under a title naming one — silent,
// and wrong in the direction a reader cannot detect. `agentop cost` reads only window totals on
// its scoped path, so it asks for no narrowing and pays for none.
//
// NEITHER VALUE IS A SAFE DEFAULT, which is why there is no zero-argument form. Defaulting to
// KeepBuckets hands a chart the whole window; defaulting to NarrowBuckets silently drops the
// latency a caller may be about to render, and drops Series from a response a caller may be
// about to break down. The caller knows which it reads; this package does not.
type BucketScope int

const (
	// KeepBuckets narrows only the window-level figures. Buckets and their Series come
	// through untouched.
	KeepBuckets BucketScope = iota
	// NarrowBuckets additionally rewrites every bucket's counts to the scoped agent's share
	// of it and drops Series. See ScopeToAgent for what this cannot carry across.
	NarrowBuckets
)

// ScopeToAgent narrows a group=agent snapshot to one agent's figures.
//
// IT REWRITES Totals AND HANDS BACK A SNAPSHOT, rather than rendering the agent itself, so
// callers apply their existing writers to it with no second implementation and no chance of the
// two drifting: `agentop cost`'s negative-total refusal, coverage-gap disclosure and
// incomplete-read admission each read one agent's numbers, and the TUI's chart reads the same
// narrowing. Which fields do NOT survive, and why, is stated at each narrowing below. A COPY,
// never the caller's snapshot mutated in place.
//
// The fold is FoldSeriesAcrossWindow, the same one agentop's AGENTS pane uses, so the figure
// printed by the CLI and the row shown in the pane cannot disagree.
//
// AN UNKNOWN AGENT IS AN ERROR THAT NAMES THE KNOWN ONES. The labels are User-Agents, so they
// are neither short nor guessable — "bob" is the obvious thing to try and is not what Bob
// sends. The set is already in hand, so withholding it would be a choice.
func ScopeToAgent(snap *Snapshot, agent string, buckets BucketScope) (*Snapshot, error) {
	series := FoldSeriesAcrossWindow(snap.Buckets)
	counts, ok := series[agent]
	if !ok {
		known := make([]string, 0, len(series))
		for label := range series {
			known = append(known, label)
		}
		// Sorted so the same window reports the same order every run; a set printed in map
		// order is a set a reader cannot diff against yesterday's.
		sort.Strings(known)
		if len(known) == 0 {
			return nil, fmt.Errorf("no agent traffic in the %s window, so %q matches nothing",
				snap.Window, agent)
		}
		return nil, fmt.Errorf("no agent %q in the %s window; seen: %s",
			agent, snap.Window, strings.Join(known, ", "))
	}
	scoped := *snap
	scoped.Totals = counts
	// EVERY WHOLE-WINDOW STATEMENT ABOUT WHERE THE TOTALS CAME FROM GOES WITH Totals, or it is
	// printed beside one agent's figure while describing all of them. Replacing only Totals left
	// `--agent <an agent nothing priced>` printing $0.00 — the window was priced, just not this
	// agent's traffic — for exactly the agent the AGENTS pane prints "—" for, which breaks both
	// writeCostSummary's "cost unavailable rather than $0.00" rule and this function's own claim
	// that the figure there and the row in the pane cannot disagree.
	//
	// Priced is RE-DERIVED with the producers' own rule rather than one invented here: both
	// snapshot.go and sessionapi set it to Totals.PricedRequests > 0, so the narrowed snapshot is
	// the one they would have emitted had this agent's traffic been the whole window.
	scoped.Priced = counts.PricedRequests > 0
	// The three by-model maps are DROPPED, not narrowed, because nothing here can narrow them: a
	// bucket's series is keyed by agent and carries no per-model breakdown, so the only available
	// readings are the window's maps — which describe other agents' traffic — or none. They are
	// omitempty on the wire, and `agentop cost`'s costIncompleteReasonLines already treats an
	// absent map as nothing to say, which is its common case for a ledger-backed window anyway.
	scoped.PricedBy = nil
	scoped.UnpricedBy = nil
	scoped.IncompleteBy = nil
	// Degraded and DaysOutsideRetention STAY, and the asymmetry is the point: they describe the
	// READ and the retention configuration, which are the same facts whichever agent is scoped
	// to. Dropping them would hide a short sum behind a narrower question.
	//
	// SeriesOvershootMicros and SeriesAvoidedOvershootMicros stay too, and they are among the
	// fields the "every" above has to account for rather than pass over. Both are defect reports about a
	// breakdown — the series summed to MORE than the total — so they belong with Degraded rather
	// than with the provenance maps. A correct producer never sends either on this path:
	// residualOf leaves them nil unless the series overshoots, which cannot happen where the
	// figures reconcile. Where one does arrive it is upstream's bug, and forwarding it says so;
	// narrowing it to an agent would be inventing a per-agent overshoot nothing computed.
	//
	// UngroupedCostMicros and UngroupedAvoidedMicros STAY, and they are the two fields this
	// narrowing hands on with a DUTY ATTACHED rather than settles. Both are whole-window
	// residuals — the part of a total that NO series entry carries — so under a scope they
	// describe traffic belonging to no agent while the Totals beside them describe one agent.
	//
	// KEPT rather than dropped, because unlike the by-model maps above there IS a correct
	// reading available: a residual is a fact about the WINDOW, true whichever agent is scoped
	// to, which is the same argument that keeps DaysOutsideRetention. Dropping them would also
	// retract a disclosure already being made — `agentop cost` reads UngroupedCostMicros off the
	// snapshot this function returns, at writeCostSummary's --agent note.
	//
	// THE DISCLOSURE ITSELF IS THE CALLER'S DUTY, and it is the one thing this function cannot
	// discharge for it: rewriting Totals is what creates the obligation, and only the caller
	// knows whether it renders a money figure at all. A surface that renders one under a scope
	// has to say what it leaves out, or a reader who scopes to each agent in turn and sums the
	// figures finds a shortfall with nothing to explain it. `agentop cost` does this in
	// writeCostSummary; agentop's usage pane does it in tui.costUngroupedRow.
	//
	// THE SURVIVAL IS PINNED IN THIS PACKAGE because every guard it had was a module away, in
	// the consumer: dropping UngroupedCostMicros here fails two cmd/agentop tests, and dropping
	// UngroupedAvoidedMicros failed nothing at all — measured, both ways. See
	// TestScopeToAgent_KeepsTheWindowResidualsForTheCallerToDisclose.

	// Currencies IS NARROWED TO THIS AGENT'S UNITS when the producer sent SeriesCurrencies, the
	// agent-by-unit cross-tabulation a folded Counts cannot carry. Without it the window's list is
	// carried over, as it always was: it then no longer describes Totals, but narrowing needs the
	// cross-tabulation and dropping the list is worse — an absent list reads as "single unit", and
	// this agent's own traffic may be the mixed part. THAT FALLBACK OVER-REFUSES, deliberately: a
	// per-agent figure is withheld in a mixed window even when the agent billed in one unit.
	// `agentop cost`'s writeCostSummary says which of the two a refusal is, because "no figure for
	// this agent" and "no figure for this window" have different fixes. SeriesCurrencies itself is
	// dropped with the series it described.
	if snap.SeriesCurrencies != nil {
		scoped.Currencies = append([]string(nil), snap.SeriesCurrencies[agent]...)
	}
	scoped.SeriesCurrencies = nil

	if buckets == NarrowBuckets {
		scoped.Buckets = narrowBucketsToAgent(snap.Buckets, agent)
	}
	return &scoped, nil
}

// narrowBucketsToAgent rewrites each bucket to one agent's share of it.
//
// A NEW SLICE, because Bucket holds a map and the caller's snapshot must survive intact —
// see ScopeToAgent's copy rule. Assigning through scoped.Buckets[i] would write into the
// array the caller still owns.
//
// EVERY BUCKET SURVIVES, including the ones this agent sent nothing in, which become zero
// buckets at their original timestamps. The chart reads Buckets positionally against a time
// axis — Snapshot.Buckets' own doc says the zeroed entries are what let a client tell an idle
// minute from one that fell off the ring — so dropping an agent's idle buckets would compress
// its history and move every bar left of where it happened.
//
// LATENCY IS ZEROED RATHER THAN CARRIED, and it is the one reading this narrowing cannot
// produce: Series is map[string]Counts and Counts holds no latency, so LatMeanMs, LatStdDevMs
// and LatSamples describe every agent that shared the bucket. Keeping them would draw one
// agent's chart out of another's response times. A caller that offers a latency view under a scope
// has to read it from somewhere else or say it is unavailable. The somewhere else is AgentSnapshot,
// which answers a recognised agent with no session from that agent's own ring, latency included —
// agentop's usage pane takes it from there (tui.graftAgentLatency) and says unavailable otherwise.
func narrowBucketsToAgent(buckets []Bucket, agent string) []Bucket {
	out := make([]Bucket, 0, len(buckets))
	for _, b := range buckets {
		// The zero Counts when absent, which is the idle-bucket case above.
		out = append(out, Bucket{At: b.At, Counts: b.Series[agent]})
	}
	return out
}

// AgentSnapshot is Snapshot narrowed to one agent's traffic, for /v1/usage?agent=.
//
// A RECOGNISED AGENT IS READ FROM ITS OWN RING, so group is honoured — see Aggregator.agents. Any
// other label, and any agent within a session, is narrowed from the agent axis instead, read
// uncapped so an agent past MaxSeriesInResponse is still found, and served as group none.
func (a *Aggregator) AgentSnapshot(window, resolution time.Duration, sessionID, agent string, group Group) Snapshot {
	if sessionID == "" && pipeline.IsKnownAgent(agent) {
		snap := a.snapshot(window, resolution, "", agent, group, MaxSeriesInResponse)
		snap.Agent = agent
		return snap
	}
	return NarrowToAgent(a.snapshot(window, resolution, sessionID, "", GroupAgent, math.MaxInt), agent)
}

// NarrowToAgent is ScopeToAgent for a producer answering agent=: a group=agent snapshot becomes
// one agent's, with the echo set. An agent with no traffic in the window gets zeroed buckets
// rather than an error. The narrowed buckets carry no series, so any grouping is served as none,
// and Group says so.
func NarrowToAgent(snap Snapshot, agent string) Snapshot {
	scoped, err := ScopeToAgent(&snap, agent, NarrowBuckets)
	if err != nil {
		idle := snap
		idle.Totals, idle.Priced = Counts{}, false
		idle.PricedBy, idle.UnpricedBy, idle.IncompleteBy = nil, nil, nil
		idle.Currencies, idle.SeriesCurrencies = nil, nil
		idle.Buckets = narrowBucketsToAgent(snap.Buckets, agent)
		scoped = &idle
	}
	scoped.UngroupedCostMicros, scoped.UngroupedAvoidedMicros = nil, nil
	scoped.Group = GroupNone
	scoped.Agent = agent
	return *scoped
}
