package claude

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session/importer"
)

const (
	plainID = "11111111-1111-4111-8111-111111111111"
	toolsID = "22222222-2222-4222-8222-222222222222"
)

// fixtures copies testdata to a temporary config directory, so that a test can change mtimes.
func fixtures(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("testdata")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func readAll(t *testing.T, src importer.Source, since time.Time) []importer.Session {
	t.Helper()
	var out []importer.Session
	for s, err := range src.Sessions(since) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func sessionByID(t *testing.T, dir, id string) importer.Session {
	t.Helper()
	for _, s := range readAll(t, New(dir), time.Time{}) {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no session %s", id)
	return importer.Session{}
}

// requests gives the request events of agent ("" is the main agent), in order.
func requests(s importer.Session, agent string) []model.SessionEvent {
	var out []model.SessionEvent
	for _, e := range s.Events {
		if e.Phase == model.SessionRequest && e.Subagent == agent {
			out = append(out, e)
		}
	}
	return out
}

func responses(s importer.Session, agent string) []model.SessionEvent {
	var out []model.SessionEvent
	for _, e := range s.Events {
		if e.Phase == model.SessionResponse && e.Subagent == agent {
			out = append(out, e)
		}
	}
	return out
}

func TestSource_AgentAndHost(t *testing.T) {
	src := New(t.TempDir())
	if src.Agent() != "claude-code" || src.PricingHost() != "api.anthropic.com" {
		t.Fatalf("Agent %q, PricingHost %q", src.Agent(), src.PricingHost())
	}
}

func TestSource_NoProjectsDirYieldsNothing(t *testing.T) {
	if got := readAll(t, New(t.TempDir()), time.Time{}); len(got) != 0 {
		t.Fatalf("got %d sessions, want 0", len(got))
	}
}

func TestSource_PlainSession(t *testing.T) {
	s := sessionByID(t, fixtures(t), plainID)
	if len(s.Events) != 4 {
		t.Fatalf("got %d events, want 4", len(s.Events))
	}
	if s.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", s.Skipped)
	}
	wantPhases := []model.SessionPhase{model.SessionRequest, model.SessionResponse, model.SessionRequest, model.SessionResponse}
	for i, e := range s.Events {
		if e.Phase != wantPhases[i] || e.Direction != model.Outbound {
			t.Errorf("event %d: phase %v direction %v", i, e.Phase, e.Direction)
		}
		if e.Client == nil || e.Client.Name != "claude-code" || e.Client.Version != "2.1.300" {
			t.Errorf("event %d: Client %+v", i, e.Client)
		}
		if e.Project == "" {
			t.Errorf("event %d: no Project", i)
		}
		if e.Source != "" || e.Host != "" {
			t.Errorf("event %d: Source %q Host %q, want both empty", i, e.Source, e.Host)
		}
		if e.Inference == nil || e.Inference.AgentRole != model.AgentRoleMain {
			t.Fatalf("event %d: Inference %+v", i, e.Inference)
		}
	}
	if got, want := s.Events[0].At, time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("request At = %v, want %v", got, want)
	}
	if got, want := s.Events[1].At, time.Date(2026, 11, 1, 10, 0, 5, 0, time.UTC); !got.Equal(want) {
		t.Errorf("response At = %v, want %v", got, want)
	}
	if got, want := s.Updated, time.Date(2026, 11, 1, 10, 1, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("Updated = %v, want %v", got, want)
	}
	if s.Events[0].RequestID != "req_011AAAAAAAAAAAAAAAAAAAAA" || s.Events[1].RequestID != s.Events[0].RequestID {
		t.Errorf("RequestIDs %q %q", s.Events[0].RequestID, s.Events[1].RequestID)
	}
	if fi, err := os.Stat(filepath.Join("testdata", "projects", "-tmp-demo", plainID+".jsonl")); err != nil || s.SizeHint != fi.Size() {
		t.Errorf("SizeHint = %d, want the file size (%v)", s.SizeHint, err)
	}

	r1 := s.Events[1].Inference
	if r1.Model != "claude-sonnet-4-5-20250929" || r1.Completion != "Hello." || r1.FinishReason != "end_turn" {
		t.Errorf("first response: Model %q Completion %q FinishReason %q", r1.Model, r1.Completion, r1.FinishReason)
	}
	if r1.InputTokens != 10 || r1.CacheWriteTokens != 200 || r1.CacheReadTokens != 3000 || r1.CompletionTokens != 5 {
		t.Errorf("first response usage: %+v", r1)
	}
	r2 := s.Events[3].Inference
	if r2.Model != "claude-opus-4-1-20250805" || r2.Completion != "Goodbye." {
		t.Errorf("second response: Model %q Completion %q", r2.Model, r2.Completion)
	}
	// The second request carries the whole conversation so far.
	if n := len(s.Events[2].Inference.Messages); n != 3 {
		t.Errorf("second request has %d messages, want 3", n)
	}
}

func TestSource_ToolCallsAndResults(t *testing.T) {
	s := sessionByID(t, fixtures(t), toolsID)
	resp := responses(s, "")
	reqs := requests(s, "")
	if len(resp) < 2 || len(reqs) < 2 {
		t.Fatalf("got %d responses, %d requests", len(resp), len(reqs))
	}
	calls := resp[0].Inference.ToolCalls
	if len(calls) != 1 || calls[0].Name != "Read" {
		t.Fatalf("first response ToolCalls = %+v", calls)
	}
	results := reqs[1].Inference.ToolResults
	if len(results) == 0 || results[0].ToolUseID != calls[0].ID {
		t.Fatalf("second request ToolResults = %+v, want the id %q", results, calls[0].ID)
	}
}

func TestSource_SplitAssistantLinesAreOneTurn(t *testing.T) {
	s := sessionByID(t, fixtures(t), toolsID)
	// Main file: 4 assistant messages (one split over 2 lines). Subagent: 2.
	if got := len(responses(s, "")); got != 4 {
		t.Fatalf("got %d main turns, want 4", got)
	}
	r := responses(s, "")[0].Inference
	// Usage and stop_reason come from the last line of the group.
	if r.CompletionTokens != 60 || r.CacheWriteTokens != 1500 || r.FinishReason != "tool_use" {
		t.Errorf("split turn: CompletionTokens %d CacheWriteTokens %d FinishReason %q", r.CompletionTokens, r.CacheWriteTokens, r.FinishReason)
	}
	// The content blocks of both lines are joined.
	if r.ResponseThinking == nil || len(r.ToolCalls) != 1 {
		t.Errorf("split turn: ResponseThinking %+v ToolCalls %+v", r.ResponseThinking, r.ToolCalls)
	}
}

func TestSource_CompactBoundaryResetsConversation(t *testing.T) {
	s := sessionByID(t, fixtures(t), toolsID)
	reqs := requests(s, "")
	before, after := reqs[len(reqs)-2].Inference, reqs[len(reqs)-1].Inference
	if len(after.Messages) >= len(before.Messages) {
		t.Fatalf("after the boundary %d messages, before %d: want fewer", len(after.Messages), len(before.Messages))
	}
	if len(after.Messages) == 0 || after.Messages[0].Role != "user" {
		t.Fatalf("after the boundary Messages = %+v", after.Messages)
	}
}

func TestSource_SubagentEventsInRootSession(t *testing.T) {
	dir := fixtures(t)
	for _, s := range readAll(t, New(dir), time.Time{}) {
		if s.ID == "a1b2c3" || s.ID == "agent-a1b2c3" {
			t.Fatalf("subagent file read as its own session %q", s.ID)
		}
	}
	s := sessionByID(t, dir, toolsID)
	sub := append(requests(s, "a1b2c3"), responses(s, "a1b2c3")...)
	if len(sub) != 4 {
		t.Fatalf("got %d subagent events, want 4", len(sub))
	}
	for _, e := range sub {
		if e.Inference == nil || e.Inference.AgentRole != model.AgentRoleSubagent {
			t.Errorf("subagent event: Inference %+v", e.Inference)
		}
	}
	// Events are merged by At: the subagent request at 11:00:07 is after the main response at
	// 11:00:06 and before the main request at 11:00:30.
	for i := 1; i < len(s.Events); i++ {
		if s.Events[i].At.Before(s.Events[i-1].At) {
			t.Fatalf("event %d At %v is before event %d At %v", i, s.Events[i].At, i-1, s.Events[i-1].At)
		}
	}
	main, err := os.Stat(filepath.Join(dir, "projects", "-tmp-demo", toolsID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	subFile, err := os.Stat(filepath.Join(dir, "projects", "-tmp-demo", toolsID, "subagents", "agent-a1b2c3.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if s.SizeHint != main.Size()+subFile.Size() {
		t.Errorf("SizeHint = %d, want %d", s.SizeHint, main.Size()+subFile.Size())
	}
}

func TestSource_WorkflowSubagent(t *testing.T) {
	dir := fixtures(t)
	from := filepath.Join(dir, "projects", "-tmp-demo", toolsID, "subagents")
	wf := filepath.Join(from, "workflows", "run1")
	if err := os.MkdirAll(wf, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(from, "agent-a1b2c3.jsonl"), filepath.Join(wf, "agent-d4e5f6.jsonl")); err != nil {
		t.Fatal(err)
	}
	s := sessionByID(t, dir, toolsID)
	if got := len(requests(s, "d4e5f6")); got != 2 {
		t.Fatalf("got %d workflow subagent requests, want 2", got)
	}
}

func TestSource_UnreadableSubagentIsSkipped(t *testing.T) {
	dir := fixtures(t)
	sub := filepath.Join(dir, "projects", "-tmp-demo", toolsID, "subagents", "agent-a1b2c3.jsonl")
	if err := os.Chmod(sub, 0); err != nil {
		t.Fatal(err)
	}
	if f, err := os.Open(sub); err == nil {
		f.Close()
		t.Skip("the file is readable without permission (root)")
	}
	s := sessionByID(t, dir, toolsID)
	if s.Skipped != 1 || len(requests(s, "a1b2c3")) != 0 {
		t.Fatalf("Skipped = %d, subagent requests %d", s.Skipped, len(requests(s, "a1b2c3")))
	}
}

func TestSource_NewestFirstAndSince(t *testing.T) {
	dir := fixtures(t)
	proj := filepath.Join(dir, "projects", "-tmp-demo")
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mid := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for path, at := range map[string]time.Time{
		filepath.Join(proj, toolsID+".jsonl"):                               old,
		filepath.Join(proj, toolsID, "subagents", "agent-a1b2c3.jsonl"):     old,
		filepath.Join(proj, toolsID, "subagents", "agent-a1b2c3.meta.json"): old,
		filepath.Join(proj, plainID+".jsonl"):                               recent,
	} {
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	got := readAll(t, New(dir), time.Time{})
	if len(got) != 2 || got[0].ID != plainID || got[1].ID != toolsID {
		t.Fatalf("order = %v, want newest first", ids(got))
	}
	got = readAll(t, New(dir), mid)
	if len(got) != 1 || got[0].ID != plainID {
		t.Fatalf("since: %v, want only %s", ids(got), plainID)
	}

	// A subagent file newer than since keeps the session.
	if err := os.Chtimes(filepath.Join(proj, toolsID, "subagents", "agent-a1b2c3.jsonl"), recent, recent); err != nil {
		t.Fatal(err)
	}
	if got = readAll(t, New(dir), mid); len(got) != 2 {
		t.Fatalf("since with a new subagent file: %v, want 2 sessions", ids(got))
	}
}

func ids(ss []importer.Session) []string {
	var out []string
	for _, s := range ss {
		out = append(out, s.ID)
	}
	return out
}

func TestSource_HonoursClaudeConfigDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", fixtures(t))
	if got := readAll(t, New(""), time.Time{}); len(got) != 2 {
		t.Fatalf("got %v, want 2 sessions", ids(got))
	}
}

// A parallel tool call puts the tool_result lines between the lines of one message.
// They must not split the message into two turns: that counts one API call twice.
func TestFile_InterleavedParallelToolCallIsOneTurn(t *testing.T) {
	p := &parser{projects: map[string]string{}}
	res, err := p.file(filepath.Join("testdata", "parallel-tools.jsonl"), "")
	if err != nil {
		t.Fatal(err)
	}
	s := importer.Session{Events: res.events}
	resp, reqs := responses(s, ""), requests(s, "")
	if len(resp) != 2 || len(reqs) != 2 {
		t.Fatalf("got %d responses, %d requests, want 2 turns", len(resp), len(reqs))
	}
	r := resp[0].Inference
	if resp[0].RequestID != "req_011XXXXXXXXXXXXXXXXXXXXX" || r.CompletionTokens != 144 || len(r.ToolCalls) != 2 || r.FinishReason != "tool_use" {
		t.Errorf("parallel turn: RequestID %q CompletionTokens %d ToolCalls %+v FinishReason %q",
			resp[0].RequestID, r.CompletionTokens, r.ToolCalls, r.FinishReason)
	}
	// The held user lines follow the assistant message: user, assistant, 3 user lines.
	next := reqs[1].Inference
	if len(next.Messages) != 5 || next.Messages[1].Role != "assistant" {
		t.Fatalf("second request Messages = %+v", next.Messages)
	}
	if len(next.ToolResults) != 2 {
		t.Errorf("second request ToolResults = %+v, want 2", next.ToolResults)
	}
	if !reqs[1].At.Equal(time.Date(2026, 11, 1, 12, 0, 5, 0, time.UTC)) {
		t.Errorf("second request At = %v, want the last held user line", reqs[1].At)
	}
}
