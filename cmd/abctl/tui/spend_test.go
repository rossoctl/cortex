package tui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/cmd/abctl/apiclient"
)

// Every figure the strip renders, derived from one snapshot. There is deliberately no
// per-minute rate among them: the window figure is already "$1.12 /1h", so a rate off the
// same quotient restates it in smaller units while inheriting every caveat on the line.
func TestSpendSummary_DerivesEveryWindowFigure(t *testing.T) {
	m := &model{}
	m.spend.chains[spanHour].snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{
			Requests:          10,
			Errors:            2,
			Tokens:            9_890_000,
			CostMicros:        1_120_000, // $1.12
			AvoidedMicros:     180_400,   // $0.18
			InputTokens:       1_000_000,
			CacheReadTokens:   8_100_000,
			CacheWriteTokens:  1_000_000,
			PricedRequests:    10,
			PriceableRequests: 10,
			PresentKinds:      usage.KindInput | usage.KindCacheRead | usage.KindCacheWrite,
		},
		Priced: true,
	}

	got := m.spendSummary()

	if got.WindowUSD != 1.12 {
		t.Errorf("WindowUSD = %v, want 1.12", got.WindowUSD)
	}
	if got.WindowLabel != "1h" {
		t.Errorf("WindowLabel = %q, want %q", got.WindowLabel, "1h")
	}
	if !got.Priced {
		t.Error("Priced = false, want true")
	}
	if got.Tokens != 9_890_000 {
		t.Errorf("Tokens = %d, want 9890000", got.Tokens)
	}
	if got.Errors != 2 {
		t.Errorf("Errors = %d, want 2", got.Errors)
	}
	// 8.1M cache reads over a 10.1M PROMPT — input + cache-read + cache-write, never the
	// 9.89M total, which includes output tokens that cannot be served from a cache.
	if !got.HasCacheHit {
		t.Fatal("HasCacheHit = false with all three prompt kinds reported")
	}
	if want := 8_100_000.0 / 10_100_000.0 * 100; got.CacheHitPct < want-1e-9 || got.CacheHitPct > want+1e-9 {
		t.Errorf("CacheHitPct = %v, want %v (cache reads over PROMPT tokens; 81.9%% of the "+
			"9.89M total would mean output tokens entered the denominator)", got.CacheHitPct, want)
	}
	if !got.HasSaved || got.SavedUSD != 0.1804 {
		t.Errorf("SavedUSD = %v (has = %v), want 0.1804 — the avoided aggregate is not reaching "+
			"the strip", got.SavedUSD, got.HasSaved)
	}
}

func TestSpendSummary_NothingPricedIsNotZero(t *testing.T) {
	// The distinction the whole strip rests on: an unknown cost is not a zero one.
	m := &model{}
	m.spend.chains[spanHour].snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{Requests: 10, PriceableRequests: 10},
		Priced: false,
	}

	got := m.spendSummary()

	if got.Priced {
		t.Error("Priced = true, want false")
	}
	if got.WindowUSD != 0 {
		t.Errorf("WindowUSD = %v; a caller must read Priced, not a sentinel", got.WindowUSD)
	}
	if got.Unpriced != 10 {
		t.Errorf("Unpriced = %d, want 10", got.Unpriced)
	}
}

func TestSpendSummary_CountsTheCoverageGap(t *testing.T) {
	m := &model{}
	m.spend.chains[spanHour].snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{
			Requests: 318, CostMicros: 4_170_000,
			PricedRequests: 306, PriceableRequests: 318,
		},
		Priced: true,
	}

	got := m.spendSummary()

	// Priceable minus priced, NOT requests minus priced: Requests counts every
	// proxied response including MCP and health checks, and using it as the
	// denominator made a correctly configured deployment read a permanent warning.
	if got.Unpriced != 12 {
		t.Errorf("Unpriced = %d, want 12", got.Unpriced)
	}
	if got.Priceable != 318 {
		t.Errorf("Priceable = %d, want 318", got.Priceable)
	}
}

func TestSpendSummary_NoSnapshotYet(t *testing.T) {
	m := &model{}
	got := m.spendSummary()
	if got.Priced {
		t.Error("Priced = true with no snapshot; want false so the strip says nothing yet")
	}
}

func TestSpendSummary_NoSavedFigureUntilItIsMeasured(t *testing.T) {
	// Rendering "saved $0.00" would assert that pruning saved nothing, when the
	// truth is that nothing measures it yet. Same rule as "cost unavailable".
	m := &model{}
	m.spend.chains[spanHour].snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{Requests: 1, CostMicros: 100, PricedRequests: 1, PriceableRequests: 1},
		Priced: true,
	}

	if got := m.spendSummary(); got.HasSaved {
		t.Error("HasSaved = true; no Avoided data exists until a later commit")
	}
}

// THE DAY'S SAVING COMES FROM THE DAY'S POLL. The strip renders the saved figure as the
// partner of the headline — "what it cost and what it would have cost" — and the headline is
// today, so a saving read from the 1h ring was a figure from one span standing in for another.
//
// The fixture is MEASURED, not invented: these are the two AvoidedMicros a local proxy served
// at the same instant, and the ratio is the size of the error. The hour had avoided $1.03
// while the day had avoided $2.19, so the line understated the day's saving by 2.1x — and
// nothing on it said which span the number was about.
//
// BOTH TWINS ARE POPULATED, which is the point of carrying two fields rather than one plus a
// discriminator: it is the same shape TodayUnpriced/Unpriced, TodayIncomplete/Incomplete and
// TodayClamped/Clamped already have, and the renderer decides which to show. A deployment with
// no cost ledger still has the window's figure to fall back to.
func TestSpendSummary_TodaysSavingComesFromTheDaysPoll(t *testing.T) {
	m := &model{}
	m.spend.chains[spanHour].snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{
			Requests: 299, Tokens: 84_576_928, CostMicros: 36_572_297,
			AvoidedMicros:  1_029_134, // $1.03 — THE HOUR's
			PricedRequests: 290, PriceableRequests: 290,
		},
		Priced: true,
	}
	m.spend.chains[spanToday].snap = &usage.Snapshot{
		Window: usage.WindowToday,
		Totals: usage.Counts{
			Requests: 537, Tokens: 122_486_000, CostMicros: 64_176_512,
			AvoidedMicros:  2_189_140, // $2.19 — THE DAY's
			PricedRequests: 537, PriceableRequests: 537,
		},
		Priced: true,
	}

	got := m.spendSummary()

	if !got.HasTodaySaved {
		t.Fatal("HasTodaySaved = false; the day's avoided aggregate is not reaching the strip, " +
			"so the figure beside the day's cost is still the hour's")
	}
	if want := 2.18914; got.TodaySavedUSD != want {
		t.Errorf("TodaySavedUSD = %v, want %v", got.TodaySavedUSD, want)
	}
	// The window twin keeps its own figure. It is the fallback, not dead weight.
	if !got.HasSaved || got.SavedUSD != 1.029134 {
		t.Errorf("SavedUSD = %v (has = %v), want 1.029134 — the window's own saving must survive "+
			"for the deployments that have no other", got.SavedUSD, got.HasSaved)
	}
}

// NO LEDGER, so no day figure and no day saving — and the window's saving is what the strip
// has. Kubernetes by design, per applyTodayFigure: window=today is answered from the ring's
// maximum span there, which leaves HasToday unset.
func TestSpendSummary_WindowSavingSurvivesWithNoDayFigure(t *testing.T) {
	m := &model{}
	m.spend.chains[spanHour].snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{
			Requests: 10, CostMicros: 1_120_000, AvoidedMicros: 180_400,
			PricedRequests: 10, PriceableRequests: 10,
		},
		Priced: true,
	}

	got := m.spendSummary()

	if got.HasTodaySaved {
		t.Error("HasTodaySaved = true with no today snapshot; a day's saving cannot be known " +
			"from an hour's ring")
	}
	if !got.HasSaved || got.SavedUSD != 0.1804 {
		t.Errorf("SavedUSD = %v (has = %v), want 0.1804", got.SavedUSD, got.HasSaved)
	}
}

// An UNPRICED day publishes no figure, so it must publish no saving either: the saved figure's
// whole justification is that it sits beside the spend it is measured against, and a saving with
// no spend next to it is a number with no scope on a line that mixes two spans.
//
// applyTodayFigure already returns early for this, so the assertion is that the saving is INSIDE
// that guard rather than beside it.
func TestSpendSummary_UnpricedDayPublishesNoSaving(t *testing.T) {
	m := &model{}
	m.spend.chains[spanToday].snap = &usage.Snapshot{
		Window: usage.WindowToday,
		Totals: usage.Counts{Requests: 400, AvoidedMicros: 2_189_140, PriceableRequests: 400},
		Priced: false,
	}

	got := m.spendSummary()

	if got.HasToday {
		t.Fatal("HasToday = true for an unpriced day")
	}
	if got.HasTodaySaved {
		t.Error("HasTodaySaved = true for a day that published no cost; the saving would sit " +
			"beside the hour's figure and read as the hour's")
	}
}

func TestSpendTick_StaleGenerationIsDropped(t *testing.T) {
	// The guard usage_pane.go:74-79 documents: two live chains each rescheduling
	// the other's successor doubles the request rate for the life of the session.
	m := &model{}
	m.spend.chains[spanHour].tickGen = 2

	if m.spendTickIsCurrent(spanHour, 1) {
		t.Error("a tick from generation 1 was accepted while generation 2 is current")
	}
	if !m.spendTickIsCurrent(spanHour, 2) {
		t.Error("the current generation's tick was dropped")
	}
}

func TestSpendLoaded_StaleReplyIsDropped(t *testing.T) {
	m := &model{}
	m.spend.chains[spanHour].reqSeq = 5
	fresh := &usage.Snapshot{Window: "1h"}

	m.applySpendLoaded(spendLoadedMsg{req: 4, snap: fresh})
	if m.spend.chains[spanHour].snap != nil {
		t.Error("a reply from an older request was applied")
	}
	if !m.spend.chains[spanHour].lastFetch.IsZero() {
		t.Error("a stale reply moved lastFetch; the strip would report data it discarded")
	}

	m.applySpendLoaded(spendLoadedMsg{req: 5, snap: fresh})
	if m.spend.chains[spanHour].snap != fresh {
		t.Error("the current request's reply was dropped")
	}
	if m.spend.chains[spanHour].lastFetch.IsZero() {
		t.Error("lastFetch was not recorded for the accepted reply")
	}
}

func TestStartSpendPolling_InvalidatesThePreviousChain(t *testing.T) {
	// Re-entering a session view must not leave the old chain alive: two chains
	// each rescheduling their own successor doubles the request rate permanently.
	m := &model{}
	m.startSpendPolling()
	first := m.spend.chains[spanHour].tickGen
	m.startSpendPolling()

	if m.spend.chains[spanHour].tickGen == first {
		t.Errorf("tickGen still %d after a restart; the previous chain's ticks stay current", first)
	}
	if !m.spendTickIsCurrent(spanHour, m.spend.chains[spanHour].tickGen) ||
		m.spendTickIsCurrent(spanHour, first) {
		t.Error("the restart did not make exactly the newest generation current")
	}
}

func TestSpendInvalidate_DropsTheSnapshotAndDisownsInFlight(t *testing.T) {
	// A different pod is a different aggregator. Finding 1: without this the old
	// pod's figure survives the switch and is drawn as the new pod's.
	m := &model{}
	m.spend.chains[spanHour].snap = &usage.Snapshot{Window: "1h"}
	m.spend.chains[spanHour].err = errUsageUnsupported
	m.spend.chains[spanHour].lastFetch = time.Now()
	beforeSeq, beforeGen := m.spend.chains[spanHour].reqSeq, m.spend.chains[spanHour].tickGen

	m.spend.invalidate()

	if m.spend.chains[spanHour].snap != nil {
		t.Error("the old pod's snapshot survived invalidate; the strip would draw it as the new pod's")
	}
	if m.spend.chains[spanHour].err != nil {
		t.Error("the old pod's error survived invalidate")
	}
	if !m.spend.chains[spanHour].lastFetch.IsZero() {
		t.Error("lastFetch survived invalidate; the strip would report the old pod's freshness")
	}
	if m.spend.chains[spanHour].reqSeq == beforeSeq {
		t.Error("reqSeq was not bumped, so an in-flight old-pod reply still passes the staleness guard")
	}
	if m.spend.chains[spanHour].tickGen == beforeGen {
		t.Error("tickGen was not bumped, so the old chain keeps scheduling")
	}
}

func TestSpendInvalidate_InFlightOldPodReplyIsDropped(t *testing.T) {
	// The half a bare tickGen++ cannot do. GetUsage has a 5s timeout, so a request
	// issued against the old pod can easily land after the switch; it must not be
	// stored, and above all must not be stored with a fresh lastFetch.
	m := &model{}
	m.spend.chains[spanHour].reqSeq = 7
	inFlight := m.spend.chains[spanHour].reqSeq // the id the old pod's request carries

	m.spend.invalidate()

	m.applySpendLoaded(spendLoadedMsg{req: inFlight, snap: &usage.Snapshot{Window: "1h"}})

	if m.spend.chains[spanHour].snap != nil {
		t.Error("an old-pod reply landed after the switch and was stored as the new pod's spend")
	}
	if !m.spend.chains[spanHour].lastFetch.IsZero() {
		t.Error("an old-pod reply moved lastFetch, presenting a stale number as a current one")
	}
}

func TestBackToPodsPane_InvalidatesTheSpendStrip(t *testing.T) {
	// Finding 1 at the real call site: backToPodsPane resets m.usage but left
	// m.spend untouched, so the figure survived a pod switch.
	m := &model{}
	m.events = map[string][]pipeline.SessionEvent{}
	m.pane = paneEvents
	// backToPodsPane re-derives m.ctx from parentCtx for the next session view; it
	// panics on a nil parent.
	m.parentCtx = context.Background()
	m.spend.chains[spanHour].snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{Requests: 1, CostMicros: 9_990_000, PricedRequests: 1, PriceableRequests: 1},
		Priced: true,
	}
	m.spend.chains[spanHour].lastFetch = time.Now()
	beforeSeq := m.spend.chains[spanHour].reqSeq

	m.backToPodsPane()

	if m.spend.chains[spanHour].snap != nil {
		t.Error("the previous pod's spend snapshot survived backToPodsPane")
	}
	if got := m.spendSummary(); got.Priced || got.WindowUSD != 0 {
		t.Errorf("spendSummary still reports the old pod: %+v", got)
	}
	if m.spend.chains[spanHour].reqSeq == beforeSeq {
		t.Error("backToPodsPane left reqSeq alone, so an in-flight old-pod reply would be accepted")
	}
}

func TestStartSpendPolling_StartsOnACleanSlate(t *testing.T) {
	// The way IN, complementing backToPodsPane's way OUT: entering a session view
	// must never inherit a figure from a previous one.
	m := &model{}
	m.spend.chains[spanHour].snap = &usage.Snapshot{Window: "1h"}
	m.spend.chains[spanHour].err = errUsageUnsupported
	m.spend.chains[spanHour].lastFetch = time.Now()

	m.startSpendPolling()

	if m.spend.chains[spanHour].snap != nil || m.spend.chains[spanHour].err != nil || !m.spend.chains[spanHour].lastFetch.IsZero() {
		t.Errorf("startSpendPolling inherited previous state: snap=%v err=%v lastFetch=%v",
			m.spend.chains[spanHour].snap, m.spend.chains[spanHour].err, m.spend.chains[spanHour].lastFetch)
	}
}

func TestSpendSummary_AFailedPollIsUnknownNotEmpty(t *testing.T) {
	// Finding 2: an errored poll reported as the zero value made the renderer fall
	// through to "nothing to say", so a broken /v1/usage drew an empty strip
	// forever with the row still reserved and no diagnostic anywhere.
	m := &model{}
	m.spend.chains[spanHour].err = errUsageUnsupported

	got := m.spendSummary()

	if !got.Failed {
		t.Error("Failed = false after an errored poll; the strip cannot tell silence from unavailability")
	}
	if got.Priced {
		t.Error("Priced = true after an errored poll")
	}
	if got.WindowUSD != 0 {
		t.Errorf("WindowUSD = %v after an errored poll, want 0", got.WindowUSD)
	}
}

func TestSpendSummary_NoErrorMeansNotFailed(t *testing.T) {
	// The other half, so Failed cannot be hardwired true: a healthy answer that
	// simply priced nothing is NOT a failure, and the two render differently
	// (the coverage counters are only meaningful for the non-failure case).
	m := &model{}
	m.spend.chains[spanHour].snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{Requests: 10, PriceableRequests: 10},
		Priced: false,
	}

	if got := m.spendSummary(); got.Failed {
		t.Error("Failed = true for a successful poll that happened to price nothing")
	}
}

// The label must come from what the server ANSWERED with, never from the window that was
// requested. The two are not the same promise: the hour chain asks for 1h, and a reply covering
// 30m labelled "/1h" is a wrong number wearing a right-looking label.
//
// This was the burn rate's test — the rate divided by the wrong span — and the concern
// outlived the rate, because the label is derived from exactly the same parse.
func TestSpendSummary_WindowLabelComesFromTheSnapshotNotTheRequest(t *testing.T) {
	m := &model{}
	m.spend.chains[spanHour].snap = &usage.Snapshot{
		Window: "30m",
		Totals: usage.Counts{
			Requests: 5, CostMicros: 1_200_000,
			PricedRequests: 5, PriceableRequests: 5,
		},
		Priced: true,
	}

	got := m.spendSummary()

	if got.WindowLabel != "30m" {
		t.Errorf("WindowLabel = %q, want %q — the reply covered 30 minutes and the hour chain "+
			"asks for an hour, so %q is the requested span echoed back", got.WindowLabel, "30m",
			got.WindowLabel)
	}
	if got.WindowUSD != 1.20 {
		t.Errorf("WindowUSD = %v, want 1.20", got.WindowUSD)
	}
}

func TestSpendSummary_UnparseableWindowStillReportsItsTotal(t *testing.T) {
	// A symbolic window the strip cannot parse into a duration leaves the LABEL
	// underived, and the total is still known and still shown. This tested a rate
	// suppression before the rate was removed; what survives is the rule that an
	// unparseable span costs the label and nothing else.
	m := &model{}
	m.spend.chains[spanHour].snap = &usage.Snapshot{
		Window: "today", // a future symbolic, ledger-backed span
		Totals: usage.Counts{
			Requests: 5, CostMicros: 1_200_000,
			PricedRequests: 5, PriceableRequests: 5,
		},
		Priced: true,
	}

	got := m.spendSummary()

	if got.WindowUSD != 1.20 {
		t.Errorf("WindowUSD = %v, want 1.20: the total is still known", got.WindowUSD)
	}
	if got.WindowLabel != "today" {
		t.Errorf("WindowLabel = %q, want the server's own %q preserved verbatim", got.WindowLabel, "today")
	}
}

func TestSpendSummary_TidiesTheAggregatorsWindowString(t *testing.T) {
	// The aggregator sets Window from time.Duration.String(), so a one-hour request
	// comes back "1h0m0s" -- six columns where two would do, on a line whose whole
	// design problem is width. No test saw this because the brief's fixtures used
	// the tidy form.
	for _, tc := range []struct{ wire, want string }{
		{"1h0m0s", "1h"},
		{"6h0m0s", "6h"},
		{"10m0s", "10m"},
		{"30m", "30m"},
		{"1h", "1h"},
		{"90s", "1m30s"}, // not a whole minute or hour: falls back to String()
	} {
		m := &model{}
		m.spend.chains[spanHour].snap = &usage.Snapshot{Window: tc.wire, Totals: usage.Counts{Requests: 1}}
		if got := m.spendSummary().WindowLabel; got != tc.want {
			t.Errorf("wire %q -> label %q, want %q", tc.wire, got, tc.want)
		}
	}
}

func TestSpendSummary_HasSnapshotSeparatesLookedFromNotLooked(t *testing.T) {
	// Identical in every counter, opposite in meaning.
	var notLooked model
	if got := notLooked.spendSummary(); got.HasSnapshot {
		t.Error("HasSnapshot = true with no snapshot")
	}

	looked := &model{}
	looked.spend.chains[spanHour].snap = &usage.Snapshot{Window: "1h", Totals: usage.Counts{}}
	if got := looked.spendSummary(); !got.HasSnapshot {
		t.Error("HasSnapshot = false after a poll answered with an empty window")
	}
}

// The one poll has to ask for the DRAWER's axis, or the breakdown is empty against a
// perfectly healthy proxy.
//
// Asserted on the WIRE rather than by reading the constant back, because the constant is not
// the contract: apiclient.GetUsage OMITS the group parameter entirely for GroupNone, so a
// revert to group=none is invisible in the request except by its absence. Nothing else in the
// package would notice — the strip reads Totals, which grouping does not affect, so every
// strip test stays green while the drawer silently loses every row.
//
// This asked for group=session while the sessions table summed its COST column out of this
// snapshot. That column is now summed server-side over each session's whole LIFE, which is
// both the honest scope beside a lifetime token count and what freed the axis for the drawer;
// see sessionsColumns.
func TestFetchSpendDrawer_AsksForTheDrawersAxis(t *testing.T) {
	var gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"window":"1h","group":"model","buckets":[],"totals":{}}`))
	}))
	defer ts.Close()

	m := &model{client: apiclient.New(ts.URL)}
	cmd := m.fetchSpendDrawer()
	if cmd == nil {
		t.Fatal("fetchSpendDrawer returned no command with a client set")
	}
	msg, ok := cmd().(spendDrawerLoadedMsg)
	if !ok {
		t.Fatalf("fetchSpendDrawer produced %T, want spendDrawerLoadedMsg", cmd())
	}
	if msg.err != nil {
		t.Fatalf("fetch errored: %v", msg.err)
	}
	// Parsed rather than substring-matched: "group=session" contains no "session="
	// and "session=x" contains no "group=", so a pair of Contains checks written to
	// cover both parameters would silently cover only one of them.
	q, err := url.ParseQuery(gotQuery)
	if err != nil {
		t.Fatalf("unparseable query %q: %v", gotQuery, err)
	}
	if got := q.Get("group"); got != string(usage.GroupModel) {
		t.Errorf("group = %q, want %q; the drawer has no rows without it", got, usage.GroupModel)
	}
	// The default span too, on the same wire: a zero windowStep means the live hour, which is
	// the first entry of the cycle now that the cycle is the band's four ascending spans.
	//
	// "1h" RATHER THAN "1h0m0s", and the change is an improvement rather than a break. The
	// drawer's poll goes through GetUsageWindow now — it has to, since three of the four spans
	// `w` reaches are symbolic boundaries a time.Duration cannot express — so the wire carries
	// the CALLER's spelling instead of Go's stringification of a duration. The server echoes
	// back what it served either way, and servedAsRequested treats the two as the same span.
	if got, want := q.Get("window"), spendSpanDefs[spanHour].window; got != want {
		t.Errorf("window = %q, want %q: a fresh model must request the live hour", got, want)
	}
	// The breakdown is only useful on the ALL-sessions ring, so a session parameter would
	// collapse it to the one row it scoped to — and the strip is global, so its figures must
	// never be scoped to whichever session a pane happens to have selected.
	if got := q.Get("session"); got != "" {
		t.Errorf("session = %q; the poll must cover every session", got)
	}
}

// A ledger-backed "today" answer becomes the headline. The strip's whole reason for
// a second poll is that the rolling window cannot answer "what did today cost".
func TestSpendSummary_LedgerBackedTodayBecomesTheHeadline(t *testing.T) {
	m := &model{}
	m.spend.chains[spanHour].snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{Requests: 10, CostMicros: 1_120_000, PricedRequests: 10, PriceableRequests: 10},
		Priced: true,
	}
	m.spend.chains[spanToday].snap = &usage.Snapshot{
		Window: "today",
		Totals: usage.Counts{Requests: 318, CostMicros: 4_170_000, PricedRequests: 318, PriceableRequests: 318},
		Priced: true,
	}

	got := m.spendSummary()

	if !got.HasToday {
		t.Fatal("HasToday = false for a ledger-backed today snapshot")
	}
	if got.TodayUSD != 4.17 {
		t.Errorf("TodayUSD = %v, want 4.17", got.TodayUSD)
	}
	// The window figure is unaffected: today is an additional reading, not a
	// replacement, and the burn rate is still derived from the rolling window.
	if got.WindowUSD != 1.12 {
		t.Errorf("WindowUSD = %v, want the window figure untouched", got.WindowUSD)
	}
}

// The today figure's OWN coverage, which applyTodayFigure used to throw away.
//
// The fixture is the shape every "today" fixture in this file lacked: PricedRequests
// != PriceableRequests. With the counters discarded, this day — one priced request out
// of four hundred, a total of unknown magnitude and certainly far larger — reached the
// strip as a bare "<$0.01 today" with no gap marker anywhere on the line, because the
// only coverage note the strip built came from the 1h ring snapshot.
func TestSpendSummary_TodayCarriesItsOwnCoverageGap(t *testing.T) {
	m := &model{}
	m.spend.chains[spanToday].snap = &usage.Snapshot{
		Window: usage.WindowToday,
		Totals: usage.Counts{
			Requests: 400, CostMicros: 3_100,
			PricedRequests: 1, PriceableRequests: 400,
		},
		Priced: true,
	}

	got := m.spendSummary()

	if !got.HasToday || got.TodayUSD != 0.0031 {
		t.Fatalf("today figure lost: HasToday=%v TodayUSD=%v", got.HasToday, got.TodayUSD)
	}
	if got.TodayPriceable != 400 {
		t.Errorf("TodayPriceable = %d, want 400 — without the denominator the gap is unreadable", got.TodayPriceable)
	}
	if got.TodayUnpriced != 399 {
		t.Errorf("TodayUnpriced = %d, want 399 (priceable minus priced)", got.TodayUnpriced)
	}
}

// A fully priced day must carry NO gap, for the reason a fully priced window carries
// none: a permanent warning with nothing to act on is what teaches an operator to
// ignore the one signal that matters.
func TestSpendSummary_FullyPricedTodayCarriesNoGap(t *testing.T) {
	m := &model{}
	m.spend.chains[spanToday].snap = &usage.Snapshot{
		Window: usage.WindowToday,
		Totals: usage.Counts{
			Requests: 318, CostMicros: 4_170_000,
			PricedRequests: 318, PriceableRequests: 318,
		},
		Priced: true,
	}

	if got := m.spendSummary(); got.TodayUnpriced != 0 {
		t.Errorf("TodayUnpriced = %d for a fully priced day, want 0", got.TodayUnpriced)
	}
}

// The two windows' coverage counters must not be copied into each other. This is the
// second verified misread at the data layer: the day was complete and the HOUR had the
// gap, and one shared set of counters is how the day ended up wearing the hour's
// warning.
func TestSpendSummary_TodayCoverageIsNotTheWindowsCoverage(t *testing.T) {
	m := &model{}
	m.spend.chains[spanHour].snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{Requests: 40, PriceableRequests: 40, PricedRequests: 0},
		Priced: false,
	}
	m.spend.chains[spanToday].snap = &usage.Snapshot{
		Window: usage.WindowToday,
		Totals: usage.Counts{
			Requests: 318, CostMicros: 4_170_000,
			PricedRequests: 318, PriceableRequests: 318,
		},
		Priced: true,
	}

	got := m.spendSummary()

	if got.Unpriced != 40 || got.Priceable != 40 {
		t.Fatalf("the window's own gap was lost: Unpriced=%d Priceable=%d", got.Unpriced, got.Priceable)
	}
	if got.TodayUnpriced != 0 {
		t.Errorf("TodayUnpriced = %d — the hour's 40-request gap leaked onto the day, which is complete",
			got.TodayUnpriced)
	}
	if got.TodayPriceable != 318 {
		t.Errorf("TodayPriceable = %d, want the DAY's 318, not the hour's 40", got.TodayPriceable)
	}
}

// An inexact total must reach the renderer AS inexact, from either snapshot.
//
// The fixture is the other shape every "today" fixture on this branch lacked:
// IncompleteRequests > 0. This branch carries a commit titled "Stop publishing a
// truncated stream's floor as an exact total", and spendSummary had no field for the
// counter that says so — so the strip republished the floor as "$4.17 today" and
// "$1.12 /1h", exact to four decimal places.
func TestSpendSummary_CarriesTheInexactCountFromBothSnapshots(t *testing.T) {
	m := &model{}
	m.spend.chains[spanHour].snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{
			Requests: 10, CostMicros: 1_120_000,
			PricedRequests: 10, PriceableRequests: 10, IncompleteRequests: 3,
		},
		Priced: true,
	}
	m.spend.chains[spanToday].snap = &usage.Snapshot{
		Window: usage.WindowToday,
		Totals: usage.Counts{
			Requests: 318, CostMicros: 4_170_000,
			PricedRequests: 318, PriceableRequests: 318, IncompleteRequests: 4,
		},
		Priced: true,
	}

	got := m.spendSummary()

	if got.Incomplete != 3 {
		t.Errorf("Incomplete = %d, want the window's 3", got.Incomplete)
	}
	if got.TodayIncomplete != 4 {
		t.Errorf("TodayIncomplete = %d, want the day's 4", got.TodayIncomplete)
	}
	// Disclosed, not deducted: the dollars are real and belong in the total.
	if got.WindowUSD != 1.12 || got.TodayUSD != 4.17 {
		t.Errorf("an inexact figure was withheld instead of qualified: window=%v today=%v",
			got.WindowUSD, got.TodayUSD)
	}
}

// And an exact answer must carry no count, so the marker cannot become permanent
// furniture.
func TestSpendSummary_AnExactTotalReportsNothingInexact(t *testing.T) {
	m := &model{}
	m.spend.chains[spanHour].snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{Requests: 10, CostMicros: 1_120_000, PricedRequests: 10, PriceableRequests: 10},
		Priced: true,
	}
	m.spend.chains[spanToday].snap = &usage.Snapshot{
		Window: usage.WindowToday,
		Totals: usage.Counts{Requests: 318, CostMicros: 4_170_000, PricedRequests: 318, PriceableRequests: 318},
		Priced: true,
	}

	got := m.spendSummary()

	if got.Incomplete != 0 || got.TodayIncomplete != 0 {
		t.Errorf("an exact pair of snapshots reported inexact counts: window=%d today=%d",
			got.Incomplete, got.TodayIncomplete)
	}
}

// THE case where the honest answer is to show less. A proxy with no durable cost
// ledger — Kubernetes by design — answers window=today from the ring's maximum span
// and reports THAT span. Trusting the request rather than the answer would label a
// six-hour total as a day's.
func TestSpendSummary_DegradedTodayWindowLeavesHasTodayFalse(t *testing.T) {
	m := &model{}
	m.spend.chains[spanHour].snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{Requests: 10, CostMicros: 1_120_000, PricedRequests: 10, PriceableRequests: 10},
		Priced: true,
	}
	m.spend.chains[spanToday].snap = &usage.Snapshot{
		Window: usage.MaxWindow.String(), // "6h0m0s" — the ring, not the ledger
		Totals: usage.Counts{Requests: 50, CostMicros: 2_000_000, PricedRequests: 50, PriceableRequests: 50},
		Priced: true,
	}

	got := m.spendSummary()

	if got.HasToday {
		t.Errorf("HasToday = true for a %q window; a 6h total must not be labelled a day's", m.spend.chains[spanToday].snap.Window)
	}
	if got.TodayUSD != 0 {
		t.Errorf("TodayUSD = %v, want 0 when there is no today figure", got.TodayUSD)
	}
}

// An unpriced today must not become a headline. renderSpendBand renders the today
// figure whenever HasToday is set, with no Priced guard of its own, so admitting an
// unpriced day here would print "$0.00 today" — a settled zero for a cost nobody
// knows.
func TestSpendSummary_UnpricedTodayLeavesHasTodayFalse(t *testing.T) {
	m := &model{}
	m.spend.chains[spanToday].snap = &usage.Snapshot{
		Window: "today",
		Totals: usage.Counts{Requests: 10, PriceableRequests: 10},
		Priced: false,
	}

	got := m.spendSummary()

	if got.HasToday {
		t.Error("HasToday = true for an unpriced today; the strip would render $0.00")
	}
}

// A failed today poll must not become a zero headline either.
func TestSpendSummary_FailedTodayPollLeavesHasTodayFalse(t *testing.T) {
	m := &model{}
	m.spend.chains[spanToday].err = context.DeadlineExceeded
	m.spend.chains[spanToday].snap = &usage.Snapshot{Window: "today", Priced: true}

	if got := m.spendSummary(); got.HasToday {
		t.Error("HasToday = true after a failed today poll")
	}
}

// The today figure survives a window poll that has not answered yet: the two chains
// are independent, and the headline is the figure a user actually wants.
func TestSpendSummary_TodaySurvivesAWindowPollThatHasNotAnswered(t *testing.T) {
	m := &model{}
	m.spend.chains[spanToday].snap = &usage.Snapshot{
		Window: "today",
		Totals: usage.Counts{Requests: 318, CostMicros: 4_170_000, PricedRequests: 318, PriceableRequests: 318},
		Priced: true,
	}

	got := m.spendSummary()

	if !got.HasToday || got.TodayUSD != 4.17 {
		t.Errorf("today figure lost with no window snapshot: HasToday=%v TodayUSD=%v", got.HasToday, got.TodayUSD)
	}
	if got.HasSnapshot {
		t.Error("HasSnapshot = true with no window snapshot")
	}
}

// The today chain must ask for window=today, which no time.Duration can express —
// the reason GetUsageWindow exists at all.
func TestFetchSpendSpan_AsksForTheSymbolicWindow(t *testing.T) {
	var gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"window":"today","buckets":[],"totals":{}}`))
	}))
	defer ts.Close()

	m := &model{client: apiclient.New(ts.URL)}
	cmd := m.fetchSpendSpan(spanToday)
	if cmd == nil {
		t.Fatal("fetchSpendSpan returned no command with a client set")
	}
	msg, ok := cmd().(spendLoadedMsg)
	if !ok {
		t.Fatalf("fetchSpendSpan produced %T, want spendLoadedMsg", cmd())
	}
	if msg.span != spanToday {
		t.Errorf("reply tagged span %v, want %v — applySpendLoaded routes on this",
			msg.span, spanToday)
	}
	if msg.err != nil {
		t.Fatalf("fetch errored: %v", msg.err)
	}
	q, err := url.ParseQuery(gotQuery)
	if err != nil {
		t.Fatalf("parse %q: %v", gotQuery, err)
	}
	if q.Get("window") != "today" {
		t.Errorf("window = %q, want \"today\"", q.Get("window"))
	}
	// session= alongside a symbolic window is refused by the server, because the
	// ledger holds no session ids. Asking for one would 400 every poll.
	if q.Has("session") {
		t.Errorf("today poll sent session=%q; the server refuses that with a symbolic window", q.Get("session"))
	}
	// No breakdown: the strip needs one number from this poll, and the sessions table
	// reads the window snapshot's series instead.
	if q.Has("group") && q.Get("group") != string(usage.GroupNone) {
		t.Errorf("group = %q, want none", q.Get("group"))
	}
	// resolution is omitted rather than sent as "0s", which the server rejects as
	// finer than its storage bucket.
	if q.Has("resolution") {
		t.Errorf("resolution = %q; the ledger serves one bucket and does not read it", q.Get("resolution"))
	}
}

// A today reply must not be accepted under the window chain's sequence, and vice
// versa: they ask different questions on different cadences, and a shared counter
// would let the fast chain invalidate the slow one's request forever.
func TestApplySpendTodayLoaded_UsesItsOwnSequence(t *testing.T) {
	m := &model{}
	m.spend.chains[spanHour].reqSeq = 7
	m.spend.chains[spanToday].reqSeq = 2
	fresh := &usage.Snapshot{Window: "today", Priced: true, Totals: usage.Counts{CostMicros: 1, PricedRequests: 1}}

	// The window chain's current sequence is not the today chain's.
	m.applySpendLoaded(spendLoadedMsg{span: spanToday, req: 7, snap: fresh})
	if m.spend.chains[spanToday].snap != nil {
		t.Error("a reply carrying the window chain's sequence was accepted as today's")
	}
	m.applySpendLoaded(spendLoadedMsg{span: spanToday, req: 2, snap: fresh})
	if m.spend.chains[spanToday].snap != fresh {
		t.Error("the today chain's own sequence was rejected")
	}
	// lastFetch reports the age of the WINDOW figure and must not move for a today
	// reply, or the strip claims a freshness the rolling figure does not have.
	if !m.spend.chains[spanHour].lastFetch.IsZero() {
		t.Errorf("lastFetch moved on a today reply: %v", m.spend.chains[spanHour].lastFetch)
	}
}

// invalidate must disown EVERY chain, and the drawer's. A different pod is a different
// month total just as much as a different hour, and each reply outlives the switch by the
// fetch timeout.
//
// ASSERTED OVER THE WHOLE TABLE rather than on the two chains that used to exist. That is
// the point of the array: a fifth span added tomorrow is covered by this test the day it is
// added, where two hand-written blocks would have left it silently retained.
func TestSpendInvalidate_DisownsEveryChain(t *testing.T) {
	s := &spendState{}
	for i := range s.chains {
		s.chains[i] = spendChain{
			snap:    &usage.Snapshot{Window: "seeded"},
			err:     context.DeadlineExceeded,
			reqSeq:  3,
			tickGen: 4,
		}
	}
	s.drawer = spendChain{
		snap:    &usage.Snapshot{Window: "seeded"},
		err:     context.DeadlineExceeded,
		reqSeq:  3,
		tickGen: 4,
	}

	s.invalidate()

	check := func(name string, c spendChain) {
		t.Helper()
		if c.snap != nil || c.err != nil {
			t.Errorf("%s: state survived invalidate: snap=%v err=%v", name, c.snap, c.err)
		}
		if !c.lastFetch.IsZero() {
			t.Errorf("%s: lastFetch survived invalidate: %v — the age of discarded data", name, c.lastFetch)
		}
		if c.reqSeq != 4 {
			t.Errorf("%s: reqSeq = %d, want 4 — an in-flight reply must be disowned", name, c.reqSeq)
		}
		if c.tickGen != 5 {
			t.Errorf("%s: tickGen = %d, want 5 — the old chain must stop scheduling", name, c.tickGen)
		}
	}
	for span := spendSpan(0); span < numSpendSpans; span++ {
		check(spendSpanDefs[span].label, s.chains[span])
	}
	check("drawer", s.drawer)
}

// lastFetch was maintained on every accepted reply and asserted by six tests, and no
// renderer read it — so a poll chain that stopped answering was indistinguishable from a
// current reading: no error, no staleness, the last good figure sitting there.
func TestSpendSummary_AWedgedPollChainReportsItsAge(t *testing.T) {
	m := &model{}
	m.spend.chains[spanHour].snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{Requests: 10, CostMicros: 1_120_000, PricedRequests: 10, PriceableRequests: 10},
		Priced: true,
	}
	m.spend.chains[spanHour].lastFetch = time.Now().Add(-3 * time.Minute)

	got := m.spendSummary()

	if !got.Stale {
		t.Fatalf("Stale = false for a figure fetched 3m ago on a %v poll", spendPollInterval)
	}
	if got.Age < 2*time.Minute || got.Age > 4*time.Minute {
		t.Errorf("Age = %v, want about 3m", got.Age)
	}
	// The figure itself is unaffected: it is old, not wrong, and withholding it would be
	// the worse answer.
	if got.WindowUSD != 1.12 {
		t.Errorf("WindowUSD = %v; a stale figure was withheld instead of dated", got.WindowUSD)
	}
}

// And a healthy figure carries no timestamp. A permanent age on a line whose whole budget
// is width is noise, and noise on an always-on indicator is how a real signal is learnt to
// be ignored.
func TestSpendSummary_AFreshFigureCarriesNoAge(t *testing.T) {
	// Up to a second short of the threshold, not the threshold itself: time.Since is read
	// after lastFetch is set, so an age of exactly spendStaleAfter is already past it by
	// the time spendSummary looks. Pinning the exact boundary would need an injected
	// clock, and what matters is that a figure inside the window is silent.
	// The last entry keeps HALF A POLL INTERVAL of margin rather than one second: the age is
	// measured against wall time, so a second is a second of budget for -race or a loaded runner
	// before "inside the window" becomes "past it" for reasons unrelated to freshness.
	for _, age := range []time.Duration{0, time.Second, spendPollInterval, spendStaleAfter - spendPollInterval/2} {
		m := &model{}
		m.spend.chains[spanHour].snap = &usage.Snapshot{
			Window: "1h",
			Totals: usage.Counts{Requests: 1, CostMicros: 1_120_000, PricedRequests: 1, PriceableRequests: 1},
			Priced: true,
		}
		m.spend.chains[spanHour].lastFetch = time.Now().Add(-age)

		if got := m.spendSummary(); got.Stale {
			t.Errorf("age %v: Stale = true at or below the %v threshold", age, spendStaleAfter)
		}
	}
}

// Before the first reply there is no age to report, and "no poll has answered" must not
// render as "this figure is infinitely old".
func TestSpendSummary_NoFetchYetIsNotStale(t *testing.T) {
	m := &model{}
	m.spend.chains[spanHour].snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{Requests: 1, CostMicros: 1_120_000, PricedRequests: 1, PriceableRequests: 1},
		Priced: true,
	}
	// lastFetch left zero, as invalidate() leaves it.

	if got := m.spendSummary(); got.Stale {
		t.Error("Stale = true with a zero lastFetch; the age would be measured from the epoch")
	}
}

// The threshold is derived from the poll interval, not chosen: "twice the cadence" is what
// makes one dropped reply quiet and a wedged chain loud, and hardcoding a duration would
// let the two drift apart the next time the interval moves.
func TestSpendStaleAfter_IsTwiceThePollInterval(t *testing.T) {
	if spendStaleAfter != 2*spendPollInterval {
		t.Errorf("spendStaleAfter = %v, want 2 x the %v poll interval", spendStaleAfter, spendPollInterval)
	}
}

// Cadence rises with the cost of the answer, which is what keeps four chains affordable.
//
// The hour is a ring read and polls fastest; each longer span walks more day files off an
// operator-configured path and polls more slowly. Asserted as an ORDERING over the table
// rather than as four literals, so it stays true when a cadence is retuned and fails when
// one is retuned the wrong way — a month polled at the hour's rate would walk up to
// thirty-one files every twenty seconds to move a figure by under a tenth of a percent.
func TestSpendSpanDefs_CadenceRisesWithTheCostOfTheAnswer(t *testing.T) {
	if spendSpanDefs[spanHour].interval != spendPollInterval {
		t.Errorf("the hour's interval is %v, want spendPollInterval (%v) — spendStaleAfter is "+
			"derived from that constant, so the fastest chain has to be the one it describes",
			spendSpanDefs[spanHour].interval, spendPollInterval)
	}
	for span := spendSpan(1); span < numSpendSpans; span++ {
		prev, cur := spendSpanDefs[span-1], spendSpanDefs[span]
		if cur.interval < prev.interval {
			t.Errorf("%s polls every %v, faster than the shorter %s at %v — a longer span costs "+
				"the server more, not less", cur.label, cur.interval, prev.label, prev.interval)
		}
	}
	// And the ledger-backed spans are all meaningfully slower than the ring, not merely
	// not-faster: the whole reason the band can afford four chains.
	for _, span := range []spendSpan{spanToday, span7d, spanMonth} {
		if spendSpanDefs[span].interval <= spendPollInterval {
			t.Errorf("%s polls every %v, no slower than the ring-served hour at %v — this span "+
				"reads day files off disk", spendSpanDefs[span].label,
				spendSpanDefs[span].interval, spendPollInterval)
		}
	}
	if spendSpanDefs[spanMonth].interval < 5*time.Minute {
		t.Errorf("the month polls every %v, faster than the 5m its own comment calls ample for a "+
			"figure that moves under a tenth of a percent in twenty seconds",
			spendSpanDefs[spanMonth].interval)
	}
}

// Every span has to be fully described, or the band renders a cell it cannot label and a
// chain that polls a window the server will refuse.
// AND THE ZERO CASE IS ANSWERED THE SAME WAY BY BOTH READERS OF IT, which is the half the
// completeness test above cannot cover: it fails the build-time mistake, and this pins what
// happens if one ever ships anyway.
//
// The two disagreed. spendTick clamped a non-positive interval to spendPollInterval and polled at
// that cadence, while spanReadings compared the age against 2*interval — 2*0 — so every age
// exceeded it and the span reported itself stale on every frame. A span that polls correctly and
// wears a permanently dated label is worse than either half alone, because the age on the label is
// the signal an operator is meant to act on, and it also costs width: the age rides on the label
// and the band is one uniform cell width.
//
// Both now read pollInterval, so this test is about the two CALLERS agreeing rather than about the
// clamp's value.
func TestSpendSpanDef_AMissingIntervalIsClampedForBothItsReaders(t *testing.T) {
	var def spendSpanDef // no interval, the shape a fifth span added to the keyed literal would have

	if got := def.pollInterval(); got != spendPollInterval {
		t.Fatalf("pollInterval() = %v, want the default %v", got, spendPollInterval)
	}

	// THE STALENESS SIDE, with a span that actually HAS no interval — spendSpanDefs is a var, so
	// the zero can be staged here rather than argued about. Against the raw zero this reading
	// was stale at one nanosecond of age.
	restore := spendSpanDefs[spanHour]
	t.Cleanup(func() { spendSpanDefs[spanHour] = restore })
	spendSpanDefs[spanHour] = spendSpanDef{window: restore.window, resolution: restore.resolution,
		label: restore.label}

	m := &model{}
	m.spend.chains[spanHour].lastFetch = time.Now().Add(-spendPollInterval)
	m.spend.chains[spanHour].snap = &usage.Snapshot{
		Window: string(spendSpanDefs[spanHour].window),
		Totals: usage.Counts{Requests: 1, CostMicros: 1_000_000, PricedRequests: 1, PriceableRequests: 1},
		Priced: true,
	}
	if r := m.spanReadings()[spanHour]; r.Stale {
		t.Errorf("a span fetched one cadence ago is stale (age %v) — the threshold read a zero "+
			"interval where the scheduler read the clamp", r.Age)
	}
}

func TestSpendSpanDefs_EverySpanIsComplete(t *testing.T) {
	for span := spendSpan(0); span < numSpendSpans; span++ {
		def := spendSpanDefs[span]
		if def.window == "" {
			t.Errorf("span %d has no window parameter", span)
		}
		if def.label == "" {
			t.Errorf("span %d (%q) has no label — every band cell must name its span", span, def.window)
		}
		if def.interval <= 0 {
			t.Errorf("span %d (%q) has no poll interval, so its chain would never reschedule",
				span, def.window)
		}
		// NO SURROUNDING WHITESPACE ON A LABEL, which is a width rule and not tidiness.
		//
		// bandCell.width() is max(label, value), so a stray space costs a whole column in exactly
		// the cells whose LABEL is the wider half — and the band is one uniform width, so it costs
		// it in every cell at once and can drop a span at a width where all four had fitted.
		//
		// INHERITED FROM #1074, whose band this one replaced. That change built labels by
		// concatenating a span suffix and found the bug the hard way: unguarded concatenation gave
		// "LAST ", "SAVED ", "TOKENS ", "CACHE HIT " a trailing space each. Its guard went with its
		// band, and the rule outlived it — these labels are literals now, so the bug takes a typo
		// rather than a concatenation, which is exactly the kind nothing else would catch.
		// Verified by mutation: adding one space to "LAST 1H" failed nothing before this.
		if def.label != strings.TrimSpace(def.label) {
			t.Errorf("span %d label %q carries surrounding whitespace: bandCell.width() charges it "+
				"a column, and the band's uniform width charges every cell", span, def.label)
		}
	}
}

// TestSpendSummary_ANegativeWindowTotalIsUnpricedNotARefund.
//
// The fixture the reviewer used, on the surface that had no guard. cost_pane.go refused a
// negative CostMicros in two places and nothing else did, so the strip republished it as a
// dollar amount: "SPEND  $-5.0000 /1h", a credit nobody issued, on the always-on line.
//
// Refused in spendSummary rather than in the renderer so ONE guard covers every figure
// derived from the total, and so the summary never carries a number the strip is forbidden
// to draw.
func TestSpendSummary_ANegativeWindowTotalIsUnpricedNotARefund(t *testing.T) {
	m := &model{}
	m.spend.chains[spanHour].snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{
			Requests: 10, CostMicros: -5_000_000,
			PricedRequests: 10, PriceableRequests: 10,
		},
		Priced: true,
	}

	s := m.spendSummary()
	if s.Priced {
		t.Error("Priced is true for a total that cannot be spend")
	}
	if s.WindowUSD != 0 {
		t.Errorf("WindowUSD = %v, want 0 — an impossible figure is not a figure", s.WindowUSD)
	}
	// And the counters survive, because the traffic is real even where the price is not.
	if s.Priceable != 10 {
		t.Errorf("Priceable = %d, want 10; the refusal dropped the coverage denominator", s.Priceable)
	}
}

// TestApplyTodayFigure_ANegativeDayTotalLeavesHasTodayFalse.
//
// The today figure is the headline of this branch and reaches the strip by a different
// path from the window figure, so it needed its own refusal: without it the strip printed
// "SPEND  $-5.0000 today". HasToday false is this field's own spelling of "no figure",
// which is what an impossible number is.
func TestApplyTodayFigure_ANegativeDayTotalLeavesHasTodayFalse(t *testing.T) {
	m := &model{}
	m.spend.chains[spanToday].snap = &usage.Snapshot{
		Window: usage.WindowToday,
		Totals: usage.Counts{
			Requests: 400, CostMicros: -5_000_000,
			PricedRequests: 400, PriceableRequests: 400,
		},
		Priced: true,
	}

	var out spendSummary
	m.applyTodayFigure(&out)
	if out.HasToday {
		t.Error("HasToday is true for a day total that cannot be spend")
	}
	if out.TodayUSD != 0 {
		t.Errorf("TodayUSD = %v, want 0", out.TodayUSD)
	}
}

// "cache 0%" and "nobody reported caching" are different answers, and only one of them is
// a claim about the traffic. usage.Counts.PresentKinds is what tells them apart, and this
// is the case where testing the denominator alone is not enough.
//
// A provider that reports input tokens and no cache counters leaves a NON-ZERO prompt with
// zero cache reads, so the arithmetic succeeds and yields 0% — a statement that this
// traffic missed the cache every time, made from the fact that nothing measured it. For an
// agent behind a prompt cache that is the most misleading figure the strip could print,
// because the true value is usually above 80%.
func TestCacheHitPct_AnUnreportedBreakdownIsNotAZeroHitRate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kinds uint8
		ok    bool
	}{
		// The realistic gateway: input counted, caching not reported at all.
		{name: "input only", kinds: usage.KindInput},
		// The mirror: cache reads reported with no input, so the denominator is short by
		// an unreported term and the ratio would read HIGH rather than low.
		{name: "cache-read only", kinds: usage.KindCacheRead},
		// Nothing reported — a gateway that sends only total_tokens.
		{name: "no kinds", kinds: 0},
		// INPUT AND CACHE-READ IS ENOUGH, and this row is the whole OpenAI-compatible path:
		// that parser never sets KindCacheWrite because OpenAI bills cache writes as ordinary
		// input, and it reports Input = prompt_tokens − cached, so input + cacheRead is already
		// the exact prompt total. Demanding the third bit made HasCacheHit structurally false
		// for every response on that path.
		{name: "input and cache-read, no cache-write", ok: true,
			kinds: usage.KindInput | usage.KindCacheRead},
		// All three, the Anthropic shape once cache_creation is on the wire.
		{name: "every prompt tier", ok: true,
			kinds: usage.KindInput | usage.KindCacheRead | usage.KindCacheWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Counters deliberately populated in every case, so the ONLY thing that varies
			// is what the provider said it was reporting. A test whose fixtures zeroed the
			// tokens would pass on the `prompt <= 0` guard and prove nothing about kinds.
			got, ok := cacheHitPct(usage.Counts{
				Tokens:          9_890_000,
				InputTokens:     1_000_000,
				CacheReadTokens: 8_100_000,
				PresentKinds:    tc.kinds,
			})
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (kinds = %05b): the denominator is the SUM of all three "+
					"prompt tiers, so a ratio is only meaningful when the provider reported every "+
					"one of them", ok, tc.ok, tc.kinds)
			}
			if !ok && got != 0 {
				t.Errorf("pct = %v alongside ok=false; a suppressed figure must carry no value "+
					"for a caller to render by mistake", got)
			}
		})
	}
}

// Reported kinds with ZERO counters must not divide.
//
// The reporting guard above and this one are different checks: that one asks whether the
// provider said anything, this one asks whether what it said can be divided by. A response can
// carry the kind bits and then carry zeroes — a denied request after the parser ran, a
// body-less response — and 0/0 is NaN, which renders as "cache NaN%" one Sprintf later. The one
// output worse than no figure is a nonsensical one.
//
// Untested until now: removing the `prompt <= 0` guard left the whole tui suite green, because
// every other fixture on this path carries real counters.
func TestCacheHitPct_ReportedKindsWithZeroCountersIsNotNaN(t *testing.T) {
	got, ok := cacheHitPct(usage.Counts{
		PresentKinds: usage.KindInput | usage.KindCacheRead | usage.KindCacheWrite,
		// Every counter zero, every kind reported.
	})
	if ok {
		t.Errorf("ok = true over a zero prompt: the ratio is 0/0, which renders as \"cache NaN%%\"")
	}
	if got != 0 {
		t.Errorf("pct = %v alongside ok=false; a suppressed figure must carry no value for a "+
			"caller to render by mistake", got)
	}
	// NO RENDERER HALF, deliberately. There used to be one asserting that renderSpendStrip drew no
	// "cache NaN%", and re-pointing it at renderSpendBand made it non-falsifiable instead of dead:
	// the band reads only s.Spans, so no value of CacheHitPct or HasCacheHit can put "NaN" or
	// "CACHE" in its output, and the assertion held for a reason that had nothing to do with the
	// guard under test. A test that cannot fail is worse than a missing one, because the suite
	// reports it as coverage.
	//
	// There is nothing to re-point it to: no live renderer reads a cache figure since the band
	// replaced the strip, which is a real coverage loss and is recorded as one rather than papered
	// over. The arithmetic above is the falsifiable part — removing cacheHitPct's `prompt <= 0`
	// guard fails it — and it is what this test is for.
}

// The DATA half of "a failed window poll must not blank the day figure".
//
// spendSummary is where the today figure is carried through the failure, and a renderer test
// could not see it: the strip renderer took a hand-built spendSummary, so a summary that dropped
// HasToday on failure still renders correctly when a test hands it one that did not. Verified by
// mutation — restoring `return spendSummary{Failed: true}` leaves the renderer test green.
//
// Which is the same data-versus-renderer split that produced the original defect: the values were
// carried on one side and discarded on the other, and each side's tests passed. Both halves now
// have one.
func TestSpendSummary_AFailedWindowPollStillCarriesTheDayFigure(t *testing.T) {
	m := &model{}
	m.spend.chains[spanHour].err = errors.New("dial tcp: connection refused")
	// The today chain answered, on its own generation, moments ago.
	m.spend.chains[spanToday].snap = &usage.Snapshot{
		Window: usage.WindowToday, Priced: true,
		Totals: usage.Counts{
			Requests: 250, CostMicros: 30_935_000, PricedRequests: 250, PriceableRequests: 250,
		},
	}
	m.spend.chains[spanToday].lastFetch = time.Now()

	got := m.spendSummary()

	if !got.Failed {
		t.Fatal("Failed = false with a window poll error")
	}
	if !got.HasToday {
		t.Error("HasToday = false: the window poll failed and took the day figure with it, which " +
			"is the one thing splitting the two chains was meant to prevent")
	}
	if got.TodayUSD != 30.935 {
		t.Errorf("TodayUSD = %v, want 30.935", got.TodayUSD)
	}
	// And the LIVE renderer really shows it, so the two halves are joined rather than each
	// correct in isolation. Against renderSpendBand for the reason above: through
	// renderSpendStrip this half vouched for a renderer paneView no longer called.
	band := strings.Join(renderSpendBand(got, 200), "\n")
	// $30.94, not $30.93: 30_935_000 micros is exactly half a cent over $30.93, and money now
	// rounds HALF-UP FROM MICROS so that this cell and `abctl cost`'s headline answer identically.
	// %.2f gave $30.93 here, because 30.935 has no exact binary form and lands a shade below the
	// half — which is the disagreement that rounding from the integer removes.
	if !strings.Contains(band, "$30.94") {
		t.Errorf("band %q lost the day figure the summary carried", band)
	}
	if !strings.Contains(band, "TODAY") {
		t.Errorf("band %q carries the figure without labelling its span", band)
	}
}

// A NON-POSITIVE CADENCE MUST NOT BECOME AN UNBOUNDED POLL. spendSpanDefs is a keyed array
// literal, so a span added without an interval carries zero — and tea.Tick(0) fires at once and
// reschedules at zero, which is one /v1/usage request per event-loop iteration against the proxy.
//
// TestSpendSpanDefs_EverySpanIsComplete is the guard that catches the mistake; this asserts what it
// costs if that guard is ever bypassed, and that the answer is not "a request storm". Asserted by
// timing the command rather than by reading the constant: tea.Tick's duration is not observable, so
// the only honest evidence is that the message does not arrive immediately.
func TestSpendTick_AZeroCadenceDoesNotFireImmediately(t *testing.T) {
	saved := spendSpanDefs[spanMonth].interval
	spendSpanDefs[spanMonth].interval = 0
	defer func() { spendSpanDefs[spanMonth].interval = saved }()

	cmd := spendTick(spanMonth, 1)
	if cmd == nil {
		t.Fatal("spendTick returned nil for a zero cadence: the span is then never polled, which " +
			"is a permanent em dash with no explanation")
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		t.Errorf("a zero-cadence tick delivered %T immediately; rescheduling on that would poll "+
			"the proxy once per loop iteration", msg)
	case <-time.After(250 * time.Millisecond):
		// Still waiting, which is the whole assertion: the cadence was clamped to something real.
	}
}
