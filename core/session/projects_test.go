package session

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

func TestApplyProject(t *testing.T) {
	fold := func(id string, labels ...string) []string {
		var p []string
		for _, l := range labels {
			p = applyProject(p, id, l)
		}
		return p
	}
	if got := fold("s1", ""); got != nil {
		t.Errorf("empty label: %v, want nil", got)
	}
	if got := fold(DefaultSessionID, "a"); got != nil {
		t.Errorf("default bucket: %v, want nil", got)
	}
	if got := fold(PendingSessionID("claude-code"), "a"); got != nil {
		t.Errorf("pending bucket: %v, want nil", got)
	}
	if got := fold("s1", "a", "b", "a"); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("a,b,a: %v, want [a b]", got)
	}
	if got := fold("s1", "a", "b"); !slices.Equal(got, []string{"b", "a"}) {
		t.Errorf("a,b: %v, want [b a]", got)
	}
	var ten []string
	for i := range 10 {
		ten = append(ten, fmt.Sprintf("p%d", i))
	}
	got := fold("s1", ten...)
	if len(got) != maxSessionProjects || got[0] != "p9" || got[maxSessionProjects-1] != "p2" {
		t.Errorf("p0..p9: %v, want 8 entries p9..p2", got)
	}
}

func projectEvents() []pipeline.SessionEvent {
	return []pipeline.SessionEvent{
		{Phase: pipeline.SessionRequest, Project: "~/a"},
		{Phase: pipeline.SessionRequest, Project: "~/b"},
		{Phase: pipeline.SessionRequest, Project: "~/a"},
	}
}

func TestStoreListSessions_Projects(t *testing.T) {
	st := New(0, 0, 0)
	defer st.Close()
	for _, e := range projectEvents() {
		st.Append("s1", e)
	}
	st.Append(DefaultSessionID, pipeline.SessionEvent{Phase: pipeline.SessionRequest, Project: "~/c"})

	sum := summaryOf(t, st, "s1")
	if !slices.Equal(sum.Projects, []string{"~/a", "~/b"}) {
		t.Fatalf("s1 Projects = %v, want [~/a ~/b]", sum.Projects)
	}
	// The summary holds a copy: a caller that changes it does not change the store.
	sum.Projects[0] = "x"
	if got := summaryOf(t, st, "s1").Projects[0]; got != "~/a" {
		t.Fatalf("store changed through a summary: %q", got)
	}
	if p := summaryOf(t, st, DefaultSessionID).Projects; p != nil {
		t.Fatalf("default Projects = %v, want nil", p)
	}
}

func TestSummaryFold_Projects(t *testing.T) {
	f := NewSummaryFold()
	for _, e := range projectEvents() {
		f.Add("s1", &e)
	}
	if got := f.Summary("s1").Projects; !slices.Equal(got, []string{"~/a", "~/b"}) {
		t.Fatalf("Projects = %v, want [~/a ~/b]", got)
	}
}

func TestSummaryFold_JSONRoundTripKeepsProjects(t *testing.T) {
	f := NewSummaryFold()
	for _, e := range projectEvents() {
		f.Add("s1", &e)
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var back SummaryFold
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if got := back.Summary("s1").Projects; !slices.Equal(got, []string{"~/a", "~/b"}) {
		t.Fatalf("round trip Projects = %v, want [~/a ~/b]", got)
	}
	var old SummaryFold
	if err := json.Unmarshal([]byte(`{"eventCount":3}`), &old); err != nil {
		t.Fatal(err)
	}
	if got := old.Summary("s1").Projects; got != nil {
		t.Fatalf("no projects key: %v, want nil", got)
	}
}

// A resumed session lists the history's projects behind the entry's, as one fold over both would.
func TestListSessions_ProjectsMergeTheEarlierHistory(t *testing.T) {
	earlier := NewSummaryFold()
	for _, p := range []string{"~/old", "~/a"} {
		earlier.Add("s1", &pipeline.SessionEvent{Phase: pipeline.SessionRequest, Project: p})
	}
	st := New(0, 0, 0)
	defer st.Close()
	st.AddRecorder(&priorKeeper{last: map[string]uint64{"s1": 2}, prior: map[string]Prior{"s1": {Fold: earlier}}})
	st.Append("s1", pipeline.SessionEvent{Phase: pipeline.SessionRequest, Project: "~/old"})
	st.Append("s1", pipeline.SessionEvent{Phase: pipeline.SessionRequest, Project: "~/b"})
	if got := summaryOf(t, st, "s1").Projects; !slices.Equal(got, []string{"~/b", "~/old", "~/a"}) {
		t.Fatalf("Projects = %v, want [~/b ~/old ~/a]", got)
	}
}

// A rename out of a bucket that applyProject skips keeps the labels of the events it moves.
func TestStoreRekey_Projects(t *testing.T) {
	st := New(0, 0, 0)
	defer st.Close()
	for _, e := range projectEvents() {
		st.Append(DefaultSessionID, e)
	}
	st.Rekey(DefaultSessionID, "ctx-1")
	if got := summaryOf(t, st, "ctx-1").Projects; !slices.Equal(got, []string{"~/a", "~/b"}) {
		t.Fatalf("ctx-1 Projects = %v, want [~/a ~/b]", got)
	}
}

func TestApplyParent(t *testing.T) {
	for _, c := range []struct{ cur, id, parent, want string }{
		{"", "c1", "s1", "s1"},
		{"s0", "c1", "s1", "s0"},
		{"", "c1", "", ""},
		{"", DefaultSessionID, "s1", ""},
		{"", PendingPrefix + "claude-code@1.2", "s1", ""},
	} {
		if got := applyParent(c.cur, c.id, c.parent); got != c.want {
			t.Errorf("applyParent(%q, %q, %q) = %q, want %q", c.cur, c.id, c.parent, got, c.want)
		}
	}
}

// childEvents are the events of a headless run that session s1 started, in the directory ~/child.
func childEvents() []pipeline.SessionEvent {
	e := pipeline.SessionEvent{Phase: pipeline.SessionRequest, Project: "~/child", ParentSession: "s1", ParentProject: "~/parent"}
	return []pipeline.SessionEvent{e, e}
}

func TestStoreListSessions_ParentSession(t *testing.T) {
	st := New(0, 0, 0)
	defer st.Close()
	for _, e := range childEvents() {
		st.Append("c1", e)
	}
	st.Append("x1", pipeline.SessionEvent{Phase: pipeline.SessionRequest, Project: "~/x"})
	sum := summaryOf(t, st, "c1")
	if !slices.Equal(sum.Projects, []string{"~/parent"}) || sum.ParentSession != "s1" {
		t.Fatalf("c1 Projects = %v, ParentSession = %q; want [~/parent], s1", sum.Projects, sum.ParentSession)
	}
	other := summaryOf(t, st, "x1")
	if !slices.Equal(other.Projects, []string{"~/x"}) || other.ParentSession != "" {
		t.Fatalf("x1 Projects = %v, ParentSession = %q; want [~/x], none", other.Projects, other.ParentSession)
	}
}

func TestSummaryFold_ParentSessionMatchesTheStore(t *testing.T) {
	st := New(0, 0, 0)
	defer st.Close()
	f := NewSummaryFold()
	for _, e := range childEvents() {
		st.Append("c1", e)
		f.Add("c1", &e)
	}
	want, got := summaryOf(t, st, "c1"), f.Summary("c1")
	if !slices.Equal(got.Projects, want.Projects) || got.ParentSession != want.ParentSession || got.ParentSession != "s1" {
		t.Fatalf("fold Projects = %v, ParentSession = %q; store %v, %q", got.Projects, got.ParentSession, want.Projects, want.ParentSession)
	}
}

func TestSummaryFold_JSONRoundTripKeepsParentSession(t *testing.T) {
	f := NewSummaryFold()
	for _, e := range childEvents() {
		f.Add("c1", &e)
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var back SummaryFold
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if got := back.Summary("c1").ParentSession; got != "s1" {
		t.Fatalf("round trip ParentSession = %q, want s1", got)
	}
	var old SummaryFold
	if err := json.Unmarshal([]byte(`{"eventCount":3}`), &old); err != nil {
		t.Fatal(err)
	}
	if got := old.Summary("c1").ParentSession; got != "" {
		t.Fatalf("no parentSession key: %q, want none", got)
	}
}

// A pending bucket sets no parent; the rename into the named session applies its events' parent.
func TestStoreRekey_ParentSession(t *testing.T) {
	st := New(0, 0, 0)
	defer st.Close()
	pending := PendingSessionID("claude-code")
	for _, e := range childEvents() {
		st.Append(pending, e)
	}
	if got := summaryOf(t, st, pending).ParentSession; got != "" {
		t.Fatalf("pending ParentSession = %q, want none", got)
	}
	st.Rekey(pending, "c1")
	sum := summaryOf(t, st, "c1")
	if sum.ParentSession != "s1" || !slices.Equal(sum.Projects, []string{"~/parent"}) {
		t.Fatalf("c1 ParentSession = %q, Projects = %v; want s1, [~/parent]", sum.ParentSession, sum.Projects)
	}
}

// A resumed session keeps the parent its earlier history named, over one its entry names.
func TestListSessions_ParentSessionFromTheEarlierHistory(t *testing.T) {
	earlier := NewSummaryFold()
	for _, e := range childEvents() {
		earlier.Add("c1", &e)
	}
	st := New(0, 0, 0)
	defer st.Close()
	st.AddRecorder(&priorKeeper{last: map[string]uint64{"c1": 2}, prior: map[string]Prior{"c1": {Fold: earlier}}})
	st.Append("c1", pipeline.SessionEvent{Phase: pipeline.SessionRequest, Project: "~/child"})
	if got := summaryOf(t, st, "c1").ParentSession; got != "s1" {
		t.Fatalf("ParentSession = %q, want s1 from the history", got)
	}
	st.Append("c1", pipeline.SessionEvent{Phase: pipeline.SessionRequest, Project: "~/child", ParentSession: "s9", ParentProject: "~/p9"})
	if got := summaryOf(t, st, "c1").ParentSession; got != "s1" {
		t.Fatalf("ParentSession = %q, want s1: the earlier history wins", got)
	}
}
