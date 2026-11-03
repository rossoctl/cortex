package sessionapi

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/ledger"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
)

func agentResponse(t *testing.T, at time.Time, c *pipeline.EventClient, costUSD float64) *pipeline.SessionEvent {
	t.Helper()
	rec, err := json.Marshal(event.Event{CostUSD: costUSD, Settled: true, Source: event.SourceUsageFallback, Provenance: "bundled"})
	if err != nil {
		t.Fatal(err)
	}
	return &pipeline.SessionEvent{At: at, Phase: pipeline.SessionResponse, StatusCode: 200, Host: "gw", Client: c,
		Inference: &pipeline.InferenceExtension{Model: "opus", InputTokens: 100, OutputTokens: 50, TotalTokens: 150, PresentKinds: 0b1001},
		Plugins:   map[string]json.RawMessage{event.Key: rec}}
}

// agent= narrows both producers to one agent and echoes it; without it the response is unchanged
// and carries no agent key, which is how a client tells an old server that ignored the parameter.
func TestHandleUsage_AgentNarrowsLedgerAndRingWindows(t *testing.T) {
	now := time.Now()
	bob := &pipeline.EventClient{Name: "bob-shell", Version: "2.0.5"}
	claude := &pipeline.EventClient{Name: "claude-code", Version: "2.1.284"}
	led := newTestLedger(t, now)
	agg := usage.New()
	for _, e := range []*pipeline.SessionEvent{agentResponse(t, now, bob, 0.01), agentResponse(t, now, claude, 0.25)} {
		led.Record("s1", e)
		agg.Record("s1", e)
	}
	ts, _ := newTestServer(t, WithUsage(agg), WithCostLedger(led))

	for _, window := range []string{"today", "1h"} {
		_, body := fetchUsage(t, ts.URL, "?window="+window+"&agent="+url.QueryEscape(bob.Label()))
		var snap usage.Snapshot
		if err := json.Unmarshal([]byte(body), &snap); err != nil {
			t.Fatalf("%s: decode: %v (%s)", window, err, body)
		}
		// The query is the pre-convergence label "bob-shell/2.0.5"; the handler folds
		// it through AgentName, so it narrows to today's "bob" and echoes that.
		if snap.Agent != "bob" || snap.Totals.CostMicros != 10_000 {
			t.Errorf("window=%s agent=bob: Agent=%q CostMicros=%d, want the echo and Bob's 10000 alone",
				window, snap.Agent, snap.Totals.CostMicros)
		}
		if _, plain := fetchUsage(t, ts.URL, "?window="+window); strings.Contains(plain, `"agent"`) {
			t.Errorf("window=%s without agent= carries an agent key: %s", window, plain)
		}
	}
}

// agent= names an agent, not a release: it finds every version's traffic, and a versioned label
// — what group=agent reported before versions were folded — is read as its agent. The echo is
// the agent the figures describe.
func TestHandleUsage_AgentCoversEveryVersion(t *testing.T) {
	now := time.Now()
	led := newTestLedger(t, now)
	agg := usage.New()
	for _, e := range []*pipeline.SessionEvent{
		agentResponse(t, now, &pipeline.EventClient{Name: "claude-code", Version: "2.1.284"}, 0.25),
		agentResponse(t, now, &pipeline.EventClient{Name: "claude-code", Version: "2.1.285"}, 0.50),
		agentResponse(t, now, &pipeline.EventClient{Name: "bob-shell", Version: "2.0.5"}, 0.01),
	} {
		led.Record("s1", e)
		agg.Record("s1", e)
	}
	ts, _ := newTestServer(t, WithUsage(agg), WithCostLedger(led))

	for _, window := range []string{"today", "1h"} {
		for _, agent := range []string{"claude-code", "claude-code/2.1.284"} {
			_, body := fetchUsage(t, ts.URL, "?window="+window+"&agent="+url.QueryEscape(agent))
			var snap usage.Snapshot
			if err := json.Unmarshal([]byte(body), &snap); err != nil {
				t.Fatalf("%s: decode: %v (%s)", window, err, body)
			}
			if snap.Agent != "claude-code" || snap.Totals.CostMicros != 750_000 {
				t.Errorf("window=%s agent=%s: Agent=%q CostMicros=%d, want claude-code's 750000 across both releases",
					window, agent, snap.Agent, snap.Totals.CostMicros)
			}
		}
	}
}

// agent= matches the label group=agent reports, so traffic that carried no User-Agent — stored
// by the ledger as no agent at all — is found under "unknown", as the AGENTS pane lists it.
func TestHandleUsage_AgentMatchesTheUnknownBucket(t *testing.T) {
	now := time.Now()
	led := newTestLedger(t, now)
	agg := usage.New()
	for _, e := range []*pipeline.SessionEvent{agentResponse(t, now, nil, 0.02), agentResponse(t, now, &pipeline.EventClient{Name: "bob-shell"}, 0.01)} {
		led.Record("s1", e)
		agg.Record("s1", e)
	}
	ts, _ := newTestServer(t, WithUsage(agg), WithCostLedger(led))
	for _, window := range []string{"today", "1h"} {
		_, body := fetchUsage(t, ts.URL, "?window="+window+"&agent="+pipeline.UnknownClientLabel)
		var snap usage.Snapshot
		if err := json.Unmarshal([]byte(body), &snap); err != nil {
			t.Fatalf("%s: decode: %v (%s)", window, err, body)
		}
		if snap.Totals.CostMicros != 20_000 {
			t.Errorf("window=%s agent=unknown: CostMicros=%d, want the untagged 20000", window, snap.Totals.CostMicros)
		}
	}
}

// agent= is bounded by the longest label either producer stores; one byte past it can match
// nothing and is refused.
func TestHandleUsage_AgentLabelBound(t *testing.T) {
	ts, _ := newTestServer(t, WithUsage(usage.New()))
	if code, body := fetchUsage(t, ts.URL, "?window=1h&agent="+strings.Repeat("a", event.MaxLabelLen)); code != 200 {
		t.Errorf("a %d-byte agent: %d %s, want 200", event.MaxLabelLen, code, body)
	}
	if code, _ := fetchUsage(t, ts.URL, "?window=1h&agent="+strings.Repeat("a", event.MaxLabelLen+1)); code != 400 {
		t.Errorf("a %d-byte agent: %d, want 400", event.MaxLabelLen+1, code)
	}
}

// Both producers split one agent by unit: the ring from that agent's own ring, the ledger from the
// unit on every row. For an agent in one unit the ring still files its plain traffic under USD.
func TestHandleUsage_AgentAndGroupCurrency(t *testing.T) {
	tbl, err := pricing.Build(&pricing.Config{Endpoints: []pricing.EndpointConfig{{
		Hosts: []string{"gw.bob"}, Unit: "Bobcoins",
		Models: map[string]pricing.ModelConfig{"opus": {TierRates: pricing.TierRates{InputCostPerMillion: 2}}},
	}}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	bob := &pipeline.EventClient{Name: "bob-shell", Version: "2.0.5"}
	events := func(at time.Time, twoUnits bool) []*pipeline.SessionEvent {
		inBobcoins := agentResponse(t, at, bob, 0.01)
		inBobcoins.Host = "gw.bob"
		second := &pipeline.SessionEvent{At: at, Phase: pipeline.SessionResponse, StatusCode: 200, Host: "github.com", Client: bob}
		if twoUnits {
			second = agentResponse(t, at, bob, 0.02)
		}
		return []*pipeline.SessionEvent{inBobcoins, second}
	}
	query := "&agent=" + url.QueryEscape(bob.Label()) + "&group=currency"
	decode := func(body string) usage.Snapshot {
		var snap usage.Snapshot
		if err := json.Unmarshal([]byte(body), &snap); err != nil {
			t.Fatalf("decode: %v (%s)", err, body)
		}
		return snap
	}

	for _, twoUnits := range []bool{false, true} {
		agg := usage.New(usage.WithPricing(tbl))
		for _, e := range events(time.Now(), twoUnits) {
			agg.Record("s1", e)
		}
		ts, _ := newTestServer(t, WithUsage(agg))
		_, body := fetchUsage(t, ts.URL, "?window=1h"+query)
		snap := decode(body)
		series := usage.FoldSeriesAcrossWindow(snap.Buckets)
		wantUSD := int64(0)
		if twoUnits {
			wantUSD = 20_000
		}
		if snap.Group != usage.GroupCurrency || series["Bobcoins"].CostMicros != 10_000 ||
			series[pricing.CurrencyUSD].Requests != 1 || series[pricing.CurrencyUSD].CostMicros != wantUSD {
			t.Errorf("ring, two units %v: group=%q series %v, want Bobcoins 10000 and one USD request of %d",
				twoUnits, snap.Group, series, wantUSD)
		}
	}

	at := insideToday(t, 2*time.Minute)
	led, err := ledger.New(t.TempDir(), ledger.WithClock(func() time.Time { return at }), ledger.WithPricing(tbl))
	if err != nil {
		t.Fatalf("ledger.New: %v", err)
	}
	t.Cleanup(func() { _ = led.Close() })
	for _, e := range events(at, true) {
		led.Record("s1", e)
	}
	ts, _ := newTestServer(t, WithUsage(usage.New()), WithCostLedger(led))
	_, body := fetchUsage(t, ts.URL, "?window=today"+query)
	snap := decode(body)
	series := usage.FoldSeriesAcrossWindow(snap.Buckets)
	if snap.Group != usage.GroupCurrency || series["Bobcoins"].CostMicros != 10_000 || series[pricing.CurrencyUSD].CostMicros != 20_000 {
		t.Errorf("ledger: group=%q series %v, want Bobcoins 10000 and USD 20000", snap.Group, series)
	}
}
