package codex

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
)

const (
	meta = `{"timestamp":"2026-11-04T08:00:00Z","type":"session_meta","payload":{"id":"` + plainID + `","cwd":"/tmp/demo","cli_version":"0.169.0"}}`
	ctx  = `{"timestamp":"2026-11-04T08:00:01Z","type":"turn_context","payload":{"model":"gpt-5-codex"}}`
)

func user(sec int, text string) string {
	return `{"timestamp":"2026-11-04T08:00:` + pad(sec) + `Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]}}`
}

func assistant(sec int, text string) string {
	return `{"timestamp":"2026-11-04T08:00:` + pad(sec) + `Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + text + `"}]}}`
}

func pad(sec int) string { return fmt.Sprintf("%02d", sec) }

// writeRollout writes lines as a rollout file under home/sessions and sets its mtime.
func writeRollout(t *testing.T, home, name string, mod time.Time, lines ...string) string {
	t.Helper()
	path := filepath.Join(home, "sessions", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
	return path
}

func contents(e model.SessionEvent) []string {
	var out []string
	for _, m := range e.Inference.Messages {
		out = append(out, m.Role+":"+m.Content)
	}
	return out
}

// An input item closes an open turn with no usage; compacted closes one with no usage and
// replaces the conversation; token_usage_record closes one with its usage.
func TestSource_TurnBoundaries(t *testing.T) {
	home := t.TempDir()
	writeRollout(t, home, "rollout-a-"+plainID+".jsonl", time.Date(2026, 11, 4, 8, 1, 0, 0, time.UTC),
		meta, ctx,
		user(2, "q1"), assistant(3, "a1"),
		user(4, "q2"), assistant(5, "a2"),
		`{"timestamp":"2026-11-04T08:00:06Z","type":"compacted","payload":{"message":"","replacement_history":[{"type":"message","role":"user","content":[{"type":"input_text","text":"summary"}]}]}}`,
		user(7, "q3"), assistant(8, "a3"),
		`{"timestamp":"2026-11-04T08:00:09Z","type":"token_usage_record","payload":{"usage":{"input_tokens":300,"cached_input_tokens":100,"output_tokens":7,"reasoning_output_tokens":2,"total_tokens":307}}}`,
	)
	s := sessionByID(t, home, plainID)
	reqs, resps := phase(s, model.SessionRequest, ""), phase(s, model.SessionResponse, "")
	if len(reqs) != 3 || len(resps) != 3 {
		t.Fatalf("%d requests, %d responses, want 3 and 3", len(reqs), len(resps))
	}
	for i, want := range []string{"a1", "a2", "a3"} {
		if resps[i].Inference.Completion != want {
			t.Errorf("response %d Completion = %q, want %q", i, resps[i].Inference.Completion, want)
		}
	}
	// (c) q2 closes turn 1 with no usage, at its last output line.
	if r := resps[0]; r.Inference.TotalTokens != 0 || !r.At.Equal(time.Date(2026, 11, 4, 8, 0, 3, 0, time.UTC)) {
		t.Errorf("turn 1 = TotalTokens %d at %v", r.Inference.TotalTokens, r.At)
	}
	if got := contents(reqs[1]); !slices.Equal(got, []string{"user:q1", "assistant:a1", "user:q2"}) {
		t.Errorf("turn 2 input = %v", got)
	}
	// (a) compacted closes turn 2 with no usage and replaces the conversation.
	if resps[1].Inference.TotalTokens != 0 {
		t.Errorf("turn 2 TotalTokens = %d, want 0", resps[1].Inference.TotalTokens)
	}
	if got := contents(reqs[2]); !slices.Equal(got, []string{"user:summary", "user:q3"}) {
		t.Errorf("turn 3 input = %v, want the replacement history and q3", got)
	}
	// (b) token_usage_record closes turn 3 with its usage.
	in := resps[2].Inference
	if in.TotalTokens != 307 || in.InputTokens != 200 || in.CacheReadTokens != 100 || in.CompletionTokens != 7 || in.ReasoningTokens != 2 {
		t.Errorf("turn 3 usage = %+v", in)
	}
	if !resps[2].At.Equal(time.Date(2026, 11, 4, 8, 0, 9, 0, time.UTC)) {
		t.Errorf("turn 3 At = %v", resps[2].At)
	}
}

// Codex 0.142.3 writes a turn's token_count after its tool results, and no token_usage_record.
// The tool results do not close the turn, so the turn keeps its usage.
func TestSource_TokenCountAfterToolResults(t *testing.T) {
	home := t.TempDir()
	tokenCount := func(sec, total int) string {
		return `{"timestamp":"2026-11-04T08:00:` + pad(sec) + `Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":` +
			fmt.Sprint(total-1) + `,"output_tokens":1,"total_tokens":` + fmt.Sprint(total) + `}}}}`
	}
	writeRollout(t, home, "rollout-a-"+plainID+".jsonl", time.Date(2026, 11, 4, 8, 1, 0, 0, time.UTC),
		meta, ctx,
		user(2, "q"),
		`{"timestamp":"2026-11-04T08:00:03Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{}","call_id":"c1"}}`,
		`{"timestamp":"2026-11-04T08:00:04Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"ok"}}`,
		tokenCount(5, 100),
		assistant(6, "done"),
		tokenCount(7, 200),
	)
	s := sessionByID(t, home, plainID)
	reqs, resps := phase(s, model.SessionRequest, ""), phase(s, model.SessionResponse, "")
	if len(reqs) != 2 || len(resps) != 2 {
		t.Fatalf("%d requests, %d responses, want 2 and 2", len(reqs), len(resps))
	}
	if n := len(resps[0].Inference.ToolCalls); n != 1 || resps[0].Inference.TotalTokens != 100 {
		t.Errorf("turn 1 = %d tool calls, TotalTokens %d, want 1 and 100", n, resps[0].Inference.TotalTokens)
	}
	if r := reqs[1].Inference.ToolResults; len(r) != 1 || r[0].ToolUseID != "c1" {
		t.Errorf("turn 2 ToolResults = %+v, want c1", r)
	}
	if !reqs[1].At.Equal(time.Date(2026, 11, 4, 8, 0, 5, 0, time.UTC)) {
		t.Errorf("turn 2 request At = %v, want the end of turn 1", reqs[1].At)
	}
	if resps[1].Inference.TotalTokens != 200 {
		t.Errorf("turn 2 TotalTokens = %d, want 200", resps[1].Inference.TotalTokens)
	}
}

// Of two rollouts with one thread id, the newer mtime wins, whatever the path order.
func TestSource_NewerRolloutOfAThreadWins(t *testing.T) {
	home := t.TempDir()
	old, newer := time.Date(2026, 11, 4, 8, 1, 0, 0, time.UTC), time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	// The newer file sorts first, so a "last listed wins" rule would keep the older one.
	writeRollout(t, home, "a/rollout-a-"+plainID+".jsonl", newer, meta, ctx, user(2, "q"), assistant(3, "new"))
	writeRollout(t, home, "b/rollout-b-"+plainID+".jsonl", old, meta, ctx, user(2, "q"), assistant(3, "old"))
	all := readAll(t, New(home), time.Time{})
	if len(all) != 1 {
		t.Fatalf("got %d sessions, want 1", len(all))
	}
	if r := phase(all[0], model.SessionResponse, ""); len(r) != 1 || r[0].Inference.Completion != "new" {
		t.Fatalf("responses = %+v, want one from the newer file", r)
	}
}

// A child rollout that cannot be read counts as one skipped line; the session stays readable.
func TestSource_UnreadableChildIsSkipped(t *testing.T) {
	dir := fixtures(t)
	base := sessionByID(t, dir, toolsID)
	failOpens(t, func(path string, n int) bool { return strings.Contains(path, subID) && n == 2 })
	s := sessionByID(t, dir, toolsID)
	if s.Unreadable || s.Skipped != base.Skipped+1 {
		t.Fatalf("Unreadable %v, Skipped %d, want false and %d", s.Unreadable, s.Skipped, base.Skipped+1)
	}
	if n := len(phase(s, model.SessionRequest, subID)); n != 0 || len(s.Events) != len(base.Events)-2 {
		t.Errorf("%d subagent requests, %d events, want 0 and %d", n, len(s.Events), len(base.Events)-2)
	}
}

// A session whose root rollout is gone still yields its subagents' events.
func TestSource_ChildOnlySessionIsYielded(t *testing.T) {
	dir := fixtures(t)
	if err := os.Remove(filepath.Join(dir, toolsFile)); err != nil {
		t.Fatal(err)
	}
	s := sessionByID(t, dir, toolsID)
	if s.Unreadable || len(s.Events) != 2 || len(phase(s, model.SessionResponse, subID)) != 1 {
		t.Fatalf("session = %+v, want the 2 subagent events", s)
	}
}
