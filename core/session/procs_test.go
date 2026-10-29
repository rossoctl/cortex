package session

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// named is a request from proc that named its session through a header, at t.
func named(proc string, t time.Time) pipeline.SessionEvent {
	return pipeline.SessionEvent{At: t, Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		Proc: proc, SessionHeader: ClaudeCodeSessionHeader}
}

// A session records the processes that NAMED it, each with its first and last such request.
// A response, a row filed by process, and a pending bucket's rows name nothing.
func TestListSessions_RecordsTheProcessesThatNamedTheSession(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	t0 := time.Unix(1_700_000_000, 0)
	s.Append("s1", named("100.5", t0))
	s.Append("s1", named("100.5", t0.Add(time.Minute)))
	s.Append("s1", named("200.7", t0.Add(2*time.Minute)))
	resp := named("300.9", t0.Add(3*time.Minute))
	resp.Phase = pipeline.SessionResponse
	s.Append("s1", resp)
	byProcess := named("400.1", t0.Add(4*time.Minute))
	byProcess.SessionHeader = ""
	s.Append("s1", byProcess)

	sum := summaryOf(t, s, "s1")
	if !slices.Equal(sum.Procs, []string{"100.5", "200.7"}) {
		t.Errorf("Procs = %v, want the two processes whose headers named s1", sum.Procs)
	}
	if got := sum.procSpans["100.5"]; !got.First.Equal(t0) || !got.Last.Equal(t0.Add(time.Minute)) {
		t.Errorf("100.5's span = %+v, want its first and last headered request", got)
	}
	if !sum.LastRequestAt.Equal(t0.Add(4 * time.Minute)) {
		t.Errorf("LastRequestAt = %v, want the newest request", sum.LastRequestAt)
	}
	s.Append(PendingID("claude-code", "100.5"), named("100.5", t0))
	if got := summaryOf(t, s, PendingID("claude-code", "100.5")).Procs; got != nil {
		t.Errorf("a pending bucket's Procs = %v, want none: a bucket names no session", got)
	}
}

// The fold persists procs, owner and lastRequestAt in session.json, and a document written
// before they existed decodes to none of them.
func TestSummaryFold_PersistsProcsAndDecodesAnOldDocument(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0).UTC()
	f := NewSummaryFold()
	e := named("100.5", t0)
	e.Client = &pipeline.EventClient{Name: "claude-code"}
	f.Add("s1", &e)
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var back SummaryFold
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	sum := back.Summary("s1")
	if !slices.Equal(sum.Procs, []string{"100.5"}) || !sum.LastRequestAt.Equal(t0) || back.owner != "claude-code" {
		t.Errorf("restored: Procs %v LastRequestAt %v owner %q", sum.Procs, sum.LastRequestAt, back.owner)
	}

	var old SummaryFold
	if err := json.Unmarshal([]byte(`{"eventCount":3,"title":"x"}`), &old); err != nil {
		t.Fatal(err)
	}
	if got := old.Summary("s1"); got.Procs != nil || !got.LastRequestAt.IsZero() {
		t.Errorf("an old document: Procs %v LastRequestAt %v, want neither", got.Procs, got.LastRequestAt)
	}
}

// A session resumed after a restart reports the processes that named it before, widened by
// the ones naming it now, so Link can reach calls made before the restart.
func TestListSessions_ProcsContinueFromTheEarlierHistory(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	earlier := NewSummaryFold()
	e := named("100.5", t0)
	earlier.Add("s1", &e)
	st := New(0, 0, 0)
	defer st.Close()
	st.AddRecorder(&priorKeeper{last: map[string]uint64{"s1": 1}, prior: map[string]Prior{"s1": {Fold: earlier}}})
	st.Append("s1", named("100.5", t0.Add(time.Hour)))
	st.Append("s1", named("200.7", t0.Add(2*time.Hour)))
	sum := summaryOf(t, st, "s1")
	if !slices.Equal(sum.Procs, []string{"100.5", "200.7"}) {
		t.Fatalf("Procs = %v", sum.Procs)
	}
	if got := sum.procSpans["100.5"]; !got.First.Equal(t0) || !got.Last.Equal(t0.Add(time.Hour)) {
		t.Errorf("100.5's span = %+v, want it to run from before the restart to after", got)
	}
}

// The procs map is bounded: a client that sends a new process key on every request cannot
// grow one session's summary without limit.
func TestApplyProcs_IsBounded(t *testing.T) {
	var procs map[string]ProcSpan
	t0 := time.Unix(1_700_000_000, 0)
	for i := range maxProcsPerSession + 10 {
		e := named(string(rune('A'+i%26))+time.Duration(i).String(), t0.Add(time.Duration(i)*time.Second))
		procs = applyProcs(procs, "s1", &e)
	}
	if len(procs) != maxProcsPerSession {
		t.Errorf("%d processes held, want %d", len(procs), maxProcsPerSession)
	}
}
