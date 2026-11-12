package usage

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
)

// projectEvent is one priced response from agent in project.
func projectEvent(t *testing.T, agent, project string) *pipeline.SessionEvent {
	t.Helper()
	e := withCostRecord(t, pricedRespEvent("gw.internal", "claude-opus-5", 100, 50),
		event.Event{CostUSD: 0.01, Settled: true, Provenance: "configured"})
	e.Client = &pipeline.EventClient{Name: agent, Version: "1.0"}
	e.Project = project
	return e
}

// threeProjects are the events of the project tests: two projects and one event with no project.
func threeProjects(t *testing.T) []*pipeline.SessionEvent {
	return []*pipeline.SessionEvent{
		projectEvent(t, "claude-code", "/repo/a"),
		projectEvent(t, "claude-code", "/repo/b"),
		projectEvent(t, "claude-code", ""),
	}
}

func assertProjectSeries(t *testing.T, snap Snapshot) {
	t.Helper()
	series := FoldSeriesAcrossWindow(snap.Buckets)
	keys := make([]string, 0, len(series))
	var sum Counts
	for k, v := range series {
		keys = append(keys, k)
		sum.Add(v)
		if v.Requests != 1 {
			t.Errorf("series %q requests = %d, want 1", k, v.Requests)
		}
	}
	slices.Sort(keys)
	if want := []string{"/repo/a", "/repo/b", UnknownProjectLabel}; !slices.Equal(keys, want) {
		t.Errorf("series keys = %v, want %v", keys, want)
	}
	if sum.Requests != snap.Totals.Requests || sum.CostMicros != snap.Totals.CostMicros {
		t.Errorf("series sum %+v does not equal totals %+v", sum, snap.Totals)
	}
}

func TestParseGroup_AcceptsProject(t *testing.T) {
	if g, err := ParseGroup("project"); err != nil || g != GroupProject {
		t.Errorf("ParseGroup(project) = %q, %v; want %q, nil", g, err, GroupProject)
	}
	if _, err := ParseGroup("bogus"); err == nil || !strings.Contains(err.Error(), "project") {
		t.Errorf("ParseGroup(bogus) error = %v, want one that names project", err)
	}
}

func TestSnapshot_GroupProjectBreaksDownByProject(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }))
	for _, e := range threeProjects(t) {
		a.Record("s1", e)
	}
	snap := a.Snapshot(10*BucketWidth, BucketWidth, "", GroupProject)
	if snap.Group != GroupProject {
		t.Fatalf("Group = %q, want %q", snap.Group, GroupProject)
	}
	assertProjectSeries(t, snap)
}

func TestAgentSnapshot_GroupProjectOnTheAgentRing(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }))
	for _, e := range threeProjects(t) {
		a.Record("s1", e)
	}
	a.Record("s2", projectEvent(t, "bob", "/repo/c"))
	assertProjectSeries(t, a.AgentSnapshot(10*BucketWidth, BucketWidth, "", "claude-code", GroupProject))
}

func TestSnapshot_GroupProjectReportsSeriesCurrencies(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }))
	a.Record("s1", projectEvent(t, "claude-code", "/repo/a"))
	bob := withCostRecord(t, pricedRespEvent("api.us-east.bob.ibm.com", "premium-ide", 3852, 47),
		event.Event{CostUSD: 0.0078, Settled: true, Provenance: "configured", Currency: "Bobcoins"})
	bob.Client = &pipeline.EventClient{Name: "bob", Version: "1.0"}
	bob.Project = "/repo/a"
	a.Record("s2", bob)

	got := a.Snapshot(10*BucketWidth, BucketWidth, "", GroupProject).SeriesCurrencies["/repo/a"]
	if want := []string{"Bobcoins", pricing.CurrencyUSD}; !slices.Equal(got, want) {
		t.Errorf("/repo/a units = %v, want %v", got, want)
	}
}

// FinishReplay merges buckets through bucket.add, which must carry the project series.
func TestReplay_RebuildsTheProjectSeries(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 30, 0, time.UTC)
	live := New(WithClock(func() time.Time { return now }))
	priv := live.BeginReplay()
	for i, e := range threeProjects(t) {
		e.At = now.Add(-time.Duration(i+1) * time.Hour)
		priv.Replay("s1", e)
	}
	// Live traffic in the same session, so FinishReplay merges into an existing ring.
	live.Record("s1", projectEvent(t, "claude-code", "/repo/a"))
	live.FinishReplay(priv)

	series := FoldSeriesAcrossWindow(live.Snapshot(MaxWindow, MaxWindow, "", GroupProject).Buckets)
	for k, want := range map[string]int64{"/repo/a": 2, "/repo/b": 1, UnknownProjectLabel: 1} {
		if got := series[k].Requests; got != want {
			t.Errorf("series %q requests = %d, want %d (series %v)", k, got, want, series)
		}
	}
}
