package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
)

const (
	parentID = "44444444-4444-4444-8444-444444444444"
	forkID   = "55555555-5555-4555-8555-555555555555"
)

// parentLines is a rollout with a tool-call turn and an answer turn, both with usage.
var parentLines = []string{
	`{"type":"session_meta","payload":{"id":"` + parentID + `","cwd":"/tmp/demo","cli_version":"0.169.0"}}`,
	`{"type":"turn_context","payload":{"cwd":"/tmp/demo","model":"gpt-5-codex"}}`,
	`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"list"}]}}`,
	`{"type":"response_item","payload":{"type":"reasoning","summary":[],"encrypted_content":"x"}}`,
	`{"type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{}","call_id":"c1"}}`,
	`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":500,"output_tokens":30,"total_tokens":530}}}}`,
	`{"type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"a.txt"}}`,
	`{"type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"listed"}]}}`,
	`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":800,"cached_input_tokens":500,"output_tokens":10,"total_tokens":810}}}}`,
}

// kept is Codex's filter for a fork without its tool calls (keep_forked_rollout_item).
func kept(l string) bool {
	var v struct {
		Type    string `json:"type"`
		Payload struct {
			Type, Role, Phase string
		} `json:"payload"`
	}
	_ = json.Unmarshal([]byte(l), &v)
	if v.Type != "response_item" {
		return true
	}
	p := v.Payload
	return p.Type == "message" && (p.Role != "assistant" || p.Phase == "final_answer")
}

// stampRollout writes lines as the rollout of thread under dir/sub, each stamped at at.
func stampRollout(t *testing.T, dir, sub, thread string, at time.Time, lines []string) {
	t.Helper()
	var b strings.Builder
	for _, l := range lines {
		at = at.Add(time.Second)
		b.WriteString(`{"timestamp":"` + at.Format(time.RFC3339) + `",` + l[1:] + "\n")
	}
	path := filepath.Join(dir, sub, "2026/11/04/rollout-2026-11-04T08-00-00-"+thread+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// writeFork writes a fork of parentLines the way Codex does: its session_meta, then the copy
// of the parent's lines (session_meta included) that copy picks, then one turn of its own.
func writeFork(t *testing.T, dir, session string, copy func([]string) []string) {
	t.Helper()
	lines := []string{`{"type":"session_meta","payload":{"id":"` + forkID + `","session_id":"` + session +
		`","forked_from_id":"` + parentID + `","cwd":"/tmp/demo","cli_version":"0.169.0"}}`}
	lines = append(lines, copy(parentLines)...)
	lines = append(lines,
		`{"type":"event_msg","payload":{"type":"thread_settings_applied"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"again"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"forked"}]}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":900,"output_tokens":7,"total_tokens":907}}}}`)
	stampRollout(t, dir, "sessions", forkID, time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC), lines)
}

// A fork's copy of its parent's turns is conversation, not turns: the fork has only its own
// turn, and the parent keeps its two.
func TestSource_ForkDoesNotRepeatParentTurns(t *testing.T) {
	all := func(l []string) []string { return l }
	filtered := func(l []string) []string {
		var out []string
		for _, s := range l {
			if kept(s) {
				out = append(out, s)
			}
		}
		return out
	}
	// The parent was in its tool call when it forked: no token_count closes that turn.
	midTurn := func(l []string) []string {
		return append(append([]string{}, l[:5]...), `{"type":"event_msg","payload":{"type":"turn_aborted"}}`)
	}
	for _, tc := range []struct {
		name, session, subagent, parentDir string
		copy                               func([]string) []string
	}{
		{"thread fork", forkID, "", "sessions", all},
		{"forked subagent", parentID, forkID, "sessions", all},
		{"fork without tool calls", forkID, "", "sessions", filtered},
		{"fork in the middle of a turn", parentID, forkID, "sessions", midTurn},
		{"archived parent", forkID, "", "archived_sessions", all},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			stampRollout(t, dir, tc.parentDir, parentID, time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC), parentLines)
			writeFork(t, dir, tc.session, tc.copy)
			fork := sessionByID(t, dir, tc.session)
			resps := phase(fork, model.SessionResponse, tc.subagent)
			if len(resps) != 1 || resps[0].Inference.Completion != "forked" {
				var got []string
				for _, r := range resps {
					got = append(got, r.RequestID+"="+r.Inference.Completion)
				}
				t.Fatalf("fork responses = %v, want only the new turn", got)
			}
			reqs := phase(fork, model.SessionRequest, tc.subagent)
			if msgs := reqs[0].Inference.Messages; len(msgs) == 0 || msgs[0].Role != "user" {
				t.Errorf("fork request messages = %+v, want the copied conversation first", msgs)
			}
			if tc.parentDir == "sessions" {
				if n := len(phase(sessionByID(t, dir, parentID), model.SessionResponse, "")); n != 2 {
					t.Errorf("parent has %d responses, want 2", n)
				}
			}
		})
	}
}

// With no parent rollout to compare, the fork's turns are the only copy, so they stay.
func TestSource_ForkWithoutParentKeepsItsTurns(t *testing.T) {
	dir := t.TempDir()
	writeFork(t, dir, forkID, func(l []string) []string { return l })
	if n := len(phase(sessionByID(t, dir, forkID), model.SessionResponse, "")); n != 3 {
		t.Errorf("%d responses, want 3", n)
	}
}
