package usage

import (
	"fmt"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// modelProjectEvent is projectEvent with the inference model set to m.
func modelProjectEvent(t *testing.T, agent, project, m string) *pipeline.SessionEvent {
	t.Helper()
	e := projectEvent(t, agent, project)
	e.Inference.Model = m
	return e
}

func TestProjectSnapshot_GroupModelForOneAgent(t *testing.T) {
	a := New()
	a.Record("s1", modelProjectEvent(t, "claude-code", "/repo/a", "m1"))
	a.Record("s1", modelProjectEvent(t, "claude-code", "/repo/a", "m1"))
	a.Record("s1", modelProjectEvent(t, "claude-code", "/repo/a", "m2"))
	a.Record("s1", modelProjectEvent(t, "claude-code", "/repo/b", "m1"))

	snap := a.ProjectSnapshot(time.Hour, time.Hour, "", "claude-code", "/repo/a", GroupModel)
	if snap.Group != GroupModel {
		t.Fatalf("Group = %q, want %q", snap.Group, GroupModel)
	}
	series := FoldSeriesAcrossWindow(snap.Buckets)
	if len(series) != 2 || series["m1"].Requests != 2 || series["m2"].Requests != 1 {
		t.Errorf("series = %v, want m1: 2 and m2: 1 requests", series)
	}
	if snap.Totals.Requests != 3 {
		t.Errorf("Totals.Requests = %d, want 3", snap.Totals.Requests)
	}
	if snap.Project != "/repo/a" || snap.Agent != "claude-code" {
		t.Errorf("echo = (agent %q, project %q), want (claude-code, /repo/a)", snap.Agent, snap.Project)
	}
}

func TestProjectSnapshot_AllAgents(t *testing.T) {
	a := New()
	a.Record("s1", projectEvent(t, "claude-code", "/repo/a"))
	a.Record("s2", projectEvent(t, "bob", "/repo/a"))
	a.Record("s2", projectEvent(t, "bob", "/repo/b"))

	snap := a.ProjectSnapshot(time.Hour, time.Hour, "", "", "/repo/a", GroupModel)
	if snap.Group != GroupModel {
		t.Fatalf("Group = %q, want %q", snap.Group, GroupModel)
	}
	if snap.Totals.Requests != 2 {
		t.Errorf("Totals.Requests = %d, want 2", snap.Totals.Requests)
	}
	if snap.Project != "/repo/a" || snap.Agent != "" {
		t.Errorf("echo = (agent %q, project %q), want (\"\", /repo/a)", snap.Agent, snap.Project)
	}
}

// An evicted ring that comes back holds less than the window. The reply must still count every
// request, so it is narrowed from the all-sessions ring and served as group none.
func TestProjectSnapshot_EvictedRingIsServedNarrowed(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 30, 0, time.UTC)
	a := New(WithClock(func() time.Time { return now }))
	record := func(project string) {
		e := projectEvent(t, "not-an-agent", project)
		e.At = now
		a.Record("s1", e)
		now = now.Add(time.Minute)
	}
	record("/repo/a")
	for i := 0; i < maxProjectRings; i++ {
		record(fmt.Sprintf("/repo/%d", i))
	}
	if _, ok := a.projects[projectKey{"", "/repo/a"}]; ok {
		t.Fatal("setup: /repo/a was not evicted")
	}
	record("/repo/a")

	snap := a.ProjectSnapshot(time.Hour, time.Minute, "", "", "/repo/a", GroupModel)
	if snap.Group != GroupNone {
		t.Errorf("Group = %q, want %q", snap.Group, GroupNone)
	}
	if snap.Totals.Requests != 2 {
		t.Errorf("Totals.Requests = %d, want 2", snap.Totals.Requests)
	}
	if snap.Project != "/repo/a" {
		t.Errorf("Project = %q, want /repo/a", snap.Project)
	}
}

// An unrecognised agent has no ring per project, but the project's all-agents ring has an agent
// axis: the reply is narrowed from it and both filters are echoed.
func TestProjectSnapshot_UnknownAgentIsNarrowedFromTheProjectRing(t *testing.T) {
	a := New()
	a.Record("s1", projectEvent(t, "not-an-agent", "/repo/a"))
	a.Record("s1", projectEvent(t, "not-an-agent", "/repo/a"))
	a.Record("s1", projectEvent(t, "not-an-agent", "/repo/b"))
	a.Record("s2", projectEvent(t, "claude-code", "/repo/a"))
	snap := a.ProjectSnapshot(time.Hour, time.Hour, "", "not-an-agent", "/repo/a", GroupModel)
	if snap.Group != GroupNone {
		t.Errorf("Group = %q, want %q", snap.Group, GroupNone)
	}
	if snap.Totals.Requests != 2 {
		t.Errorf("Totals.Requests = %d, want 2 (not-an-agent in /repo/a only)", snap.Totals.Requests)
	}
	if snap.Project != "/repo/a" || snap.Agent != "not-an-agent" {
		t.Errorf("echo = (agent %q, project %q), want (not-an-agent, /repo/a)", snap.Agent, snap.Project)
	}
}

// With no complete project ring, an unrecognised agent has no agent-by-project data, so the
// reply is AgentSnapshot's and does not echo the project.
func TestProjectSnapshot_UnknownAgentWithoutRingDoesNotEcho(t *testing.T) {
	a := New()
	a.Record("s1", projectEvent(t, "not-an-agent", "/repo/a"))
	snap := a.ProjectSnapshot(time.Hour, time.Hour, "", "not-an-agent", "/repo/none", GroupModel)
	if snap.Project != "" {
		t.Errorf("Project = %q, want empty: no ring holds /repo/none", snap.Project)
	}
	if snap.Agent != "not-an-agent" {
		t.Errorf("Agent = %q, want the AgentSnapshot echo", snap.Agent)
	}
}

// In a minute with more projects than a bucket keeps labels, the all-sessions ring files the late
// ones under the overflow label and counts too few for them. Their own ring is still complete.
func TestProjectSnapshot_OverflowMinuteKeepsTheGroup(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 30, 0, time.UTC)
	a := New(WithClock(func() time.Time { return now }))
	var last string
	for i := 0; i <= maxLabelsPerBucket; i++ {
		last = fmt.Sprintf("/repo/%d", i)
		e := projectEvent(t, "not-an-agent", last)
		e.At = now
		a.Record("s1", e)
	}
	snap := a.ProjectSnapshot(time.Hour, time.Minute, "", "", last, GroupModel)
	if snap.Group != GroupModel {
		t.Errorf("Group = %q, want %q: the ring is complete", snap.Group, GroupModel)
	}
	if snap.Totals.Requests != 1 {
		t.Errorf("Totals.Requests = %d, want 1", snap.Totals.Requests)
	}
}

func TestProjectSnapshot_WithSessionIsNarrowed(t *testing.T) {
	a := New()
	a.Record("s1", projectEvent(t, "claude-code", "/repo/a"))
	a.Record("s1", projectEvent(t, "claude-code", "/repo/a"))
	a.Record("s1", projectEvent(t, "claude-code", "/repo/b"))
	a.Record("s2", projectEvent(t, "claude-code", "/repo/a"))

	snap := a.ProjectSnapshot(time.Hour, time.Hour, "s1", "claude-code", "/repo/a", GroupModel)
	if snap.Group != GroupNone {
		t.Errorf("Group = %q, want %q", snap.Group, GroupNone)
	}
	if snap.Totals.Requests != 2 {
		t.Errorf("Totals.Requests = %d, want 2 (s1 in /repo/a only)", snap.Totals.Requests)
	}
	if snap.Project != "/repo/a" {
		t.Errorf("Project = %q, want /repo/a", snap.Project)
	}
	if snap.Agent != "" {
		t.Errorf("Agent = %q, want empty: the session ring has no agent axis per project", snap.Agent)
	}
}

func TestProjectSnapshot_NoTrafficIsZeroNotError(t *testing.T) {
	a := New()
	a.Record("s1", projectEvent(t, "claude-code", "/repo/a"))
	snap := a.ProjectSnapshot(time.Hour, time.Hour, "", "claude-code", "/repo/none", GroupModel)
	if snap.Totals != (Counts{}) {
		t.Errorf("Totals = %+v, want zero", snap.Totals)
	}
	if snap.Project != "/repo/none" {
		t.Errorf("Project = %q, want /repo/none", snap.Project)
	}
}
