package tui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/usage"
	"github.com/rossoctl/cortex/authbridge/cmd/abctl/apiclient"
)

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
	if got := m.spendSummary().Spans[spanHour]; got.Priced || got.USD != 0 {
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

func TestSpendSummary_TidiesTheAggregatorsWindowString(t *testing.T) {
	// The aggregator sets Window from time.Duration.String(), so a one-hour request
	// comes back "1h0m0s" -- six columns where two would do, on a line whose whole
	// design problem is width. No test saw this because the brief's fixtures used
	// the tidy form.
	for _, tc := range []struct{ wire, want string }{
		// THE HOUR IS A SPAN THE BAND NAMES, and the wire form does not change that: "1h0m0s"
		// and "1h" are the same span, so both come back LAST 1H. This case wanted "1h" until
		// spanLabelFor stopped matching spans with == — the hour is the only one written as a
		// duration, so it was the only one that missed its own label and flipped the caption
		// from LAST 1H to 1h the moment a poll landed.
		{"1h0m0s", spendSpanDefs[spanHour].label},
		// A duration the band does NOT name still gets the compaction, which is what this test
		// was written for.
		{"6h0m0s", "6h"},
		{"10m0s", "10m"},
		{"30m", "30m"},
		// The tidy form of the same span, which has always come back as the label.
		{"1h", "LAST 1H"},
		{"today", "TODAY"},
		{"month", "MONTH"},
		{"90s", "1m30s"}, // not a whole minute or hour: falls back to String()
	} {
		// THROUGH drawerLabels, which is where this tidy lives now. It used to be asserted on a
		// summary field no renderer read; the drawer's caption is the
		// surface that actually prints a served duration window, and spanLabelFor is what
		// compacts it.
		m := &model{}
		m.spend.drawer.snap = &usage.Snapshot{Window: tc.wire, Totals: usage.Counts{Requests: 1}}
		if _, got := m.drawerLabels(); got != tc.want {
			t.Errorf("wire %q -> label %q, want %q", tc.wire, got, tc.want)
		}
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

// Cadence rises with the cost of the answer, which is what keeps four chains affordable.
//
// The hour is a ring read and polls fastest; each longer span walks more day files off an
// operator-configured path and polls more slowly. Asserted as an ORDERING over the table
// rather than as four literals, so it stays true when a cadence is retuned and fails when
// one is retuned the wrong way — a month polled at the hour's rate would walk up to
// thirty-one files every twenty seconds to move a figure by under a tenth of a percent.
func TestSpendSpanDefs_CadenceRisesWithTheCostOfTheAnswer(t *testing.T) {
	if spendSpanDefs[spanHour].interval != spendPollInterval {
		t.Errorf("the hour's interval is %v, want spendPollInterval (%v) — that constant IS the "+
			"hour's cadence, and pointing the table at it is what keeps it from being a "+
			"write-only symbol beside a duplicate literal",
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
