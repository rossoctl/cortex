package sessionapi

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/ledger"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
)

// projectFixture records claude-code in /repo/a ($0.25) and in no project ($0.50), and
// bob-shell in /repo/c ($0.01), into both producers.
func projectFixture(t *testing.T) (*ledger.Writer, *usage.Aggregator) {
	t.Helper()
	now := time.Now()
	claude := &pipeline.EventClient{Name: "claude-code", Version: "2.1.284"}
	bob := &pipeline.EventClient{Name: "bob-shell", Version: "2.0.5"}
	a := agentResponse(t, now, claude, 0.25)
	a.Project = "/repo/a"
	none := agentResponse(t, now, claude, 0.50)
	c := agentResponse(t, now, bob, 0.01)
	c.Project = "/repo/c"
	led := newTestLedger(t, now)
	agg := usage.New()
	for _, e := range []*pipeline.SessionEvent{a, none, c} {
		led.Record("s1", e)
		agg.Record("s1", e)
	}
	return led, agg
}

// fetchProjectSeries asks for query and returns the snapshot with its series folded across
// the window, as label -> cost in micros.
func fetchProjectSeries(t *testing.T, base, query string) (usage.Snapshot, map[string]int64) {
	t.Helper()
	code, body := fetchUsage(t, base, query)
	if code != 200 {
		t.Fatalf("%s: %d %s", query, code, body)
	}
	var snap usage.Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("%s: decode: %v (%s)", query, err, body)
	}
	costs := map[string]int64{}
	for k, v := range usage.FoldSeriesAcrossWindow(snap.Buckets) {
		costs[k] = v.CostMicros
	}
	return snap, costs
}

func checkProjectCosts(t *testing.T, query string, snap usage.Snapshot, got, want map[string]int64) {
	t.Helper()
	if snap.Group != usage.GroupProject {
		t.Errorf("%s: group=%q, want project", query, snap.Group)
	}
	if !maps.Equal(got, want) {
		t.Errorf("%s: series %v, want %v", query, got, want)
	}
}

func TestHandleUsage_GroupProjectOnTheRing(t *testing.T) {
	led, agg := projectFixture(t)
	ts, _ := newTestServer(t, WithUsage(agg), WithCostLedger(led))
	q := "?window=1h&group=project"
	snap, got := fetchProjectSeries(t, ts.URL, q)
	checkProjectCosts(t, q, snap, got, map[string]int64{"/repo/a": 250_000, usage.UnknownProjectLabel: 500_000, "/repo/c": 10_000})
}

func TestHandleUsage_GroupProjectOnTheLedger(t *testing.T) {
	led, agg := projectFixture(t)
	ts, _ := newTestServer(t, WithUsage(agg), WithCostLedger(led))
	q := "?window=today&group=project"
	snap, got := fetchProjectSeries(t, ts.URL, q)
	checkProjectCosts(t, q, snap, got, map[string]int64{"/repo/a": 250_000, usage.UnknownProjectLabel: 500_000, "/repo/c": 10_000})
}

// agent= with group=project splits that agent's traffic by project, from either producer;
// another agent's project does not appear.
func TestHandleUsage_AgentAndGroupProject(t *testing.T) {
	led, agg := projectFixture(t)
	ts, _ := newTestServer(t, WithUsage(agg), WithCostLedger(led))
	for _, window := range []string{"today", "1h"} {
		q := "?window=" + window + "&agent=claude-code&group=project"
		snap, got := fetchProjectSeries(t, ts.URL, q)
		if snap.Agent != "claude-code" {
			t.Errorf("%s: agent=%q, want claude-code", q, snap.Agent)
		}
		checkProjectCosts(t, q, snap, got, map[string]int64{"/repo/a": 250_000, usage.UnknownProjectLabel: 500_000})
	}
}

// project= on a ring window keeps the group when the project ring is complete, and echoes it.
func TestUsage_ProjectOnRingWindow(t *testing.T) {
	led, agg := projectFixture(t)
	ts, _ := newTestServer(t, WithUsage(agg), WithCostLedger(led))
	q := "?window=1h&group=model&agent=claude-code&project=/repo/a"
	snap, got := fetchProjectSeries(t, ts.URL, q)
	if snap.Project != "/repo/a" || snap.Agent != "claude-code" || snap.Group != usage.GroupModel {
		t.Errorf("%s: project=%q agent=%q group=%q, want /repo/a, claude-code, model", q, snap.Project, snap.Agent, snap.Group)
	}
	if want := map[string]int64{"opus": 250_000}; !maps.Equal(got, want) {
		t.Errorf("%s: series %v, want %v", q, got, want)
	}
}

func TestUsage_ProjectOnLedgerWindow(t *testing.T) {
	led, agg := projectFixture(t)
	ts, _ := newTestServer(t, WithUsage(agg), WithCostLedger(led))
	q := "?window=today&group=model&project=/repo/a"
	snap, got := fetchProjectSeries(t, ts.URL, q)
	if snap.Project != "/repo/a" || snap.Group != usage.GroupModel {
		t.Errorf("%s: project=%q group=%q, want /repo/a, model", q, snap.Project, snap.Group)
	}
	if want := map[string]int64{"opus": 250_000}; !maps.Equal(got, want) {
		t.Errorf("%s: series %v, want %v", q, got, want)
	}
}

// A project label longer than the ring stores cannot match anything, so it is refused with a fixed
// message that does not reflect the input.
func TestUsage_ProjectTooLong(t *testing.T) {
	ts, _ := newTestServer(t, WithUsage(usage.New()))
	if code, body := fetchUsage(t, ts.URL, "?window=1h&project="+strings.Repeat("a", event.MaxLabelLen)); code != 200 {
		t.Errorf("project at the limit: %d %s, want 200", code, body)
	}
	code, body := fetchUsage(t, ts.URL, "?window=1h&project="+strings.Repeat("a", event.MaxLabelLen+1))
	if code != 400 || !strings.Contains(body, errProjectLabelTooLong.msg) {
		t.Errorf("project one byte over the limit: %d %s, want 400 %q", code, body, errProjectLabelTooLong.msg)
	}
}

// Without project= the reply carries no project key: absent means "not applied".
func TestUsage_NoProjectIsUnchanged(t *testing.T) {
	led, agg := projectFixture(t)
	ts, _ := newTestServer(t, WithUsage(agg), WithCostLedger(led))
	for _, window := range []string{"1h", "today"} {
		if _, body := fetchUsage(t, ts.URL, "?window="+window+"&group=model&agent=claude-code"); strings.Contains(body, `"project"`) {
			t.Errorf("window=%s without project= carries a project key: %s", window, body)
		}
	}
}
