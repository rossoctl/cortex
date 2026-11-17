package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
)

const forkID = "44444444-4444-4444-8444-444444444444"

// writeFork writes a rollout that forks the plain fixture the way Codex copies a thread: the
// parent's lines after its session_meta, stamped with the fork time, then one new turn.
// session is the fork's session_id; parent "" leaves forked_from_id out.
func writeFork(t *testing.T, dir, session, parent string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, plainFile))
	if err != nil {
		t.Fatal(err)
	}
	stamp := func(at time.Time, typ, payload string) string {
		return `{"timestamp":"` + at.Format(time.RFC3339) + `","type":"` + typ + `","payload":` + payload + "}\n"
	}
	at := time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)
	meta := `{"id":"` + forkID + `","session_id":"` + session + `","cwd":"/tmp/demo","cli_version":"0.169.0"`
	if parent != "" {
		meta += `,"forked_from_id":"` + parent + `"`
	}
	var b strings.Builder
	b.WriteString(stamp(at, "session_meta", meta+"}"))
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		var l line
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.Type == "session_meta" {
			continue
		}
		b.WriteString(stamp(at, l.Type, string(l.Payload)))
	}
	b.WriteString(stamp(at, "event_msg", `{"type":"thread_settings_applied"}`))
	at = at.Add(time.Minute)
	b.WriteString(stamp(at, "response_item", `{"type":"message","role":"user","content":[{"type":"input_text","text":"again"}]}`))
	b.WriteString(stamp(at.Add(time.Second), "response_item", `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"forked"}]}`))
	b.WriteString(stamp(at.Add(2*time.Second), "event_msg",
		`{"type":"token_count","info":{"last_token_usage":{"input_tokens":1500,"cached_input_tokens":1200,"output_tokens":7,"total_tokens":1507}}}`))
	path := filepath.Join(dir, "sessions/2026/11/04/rollout-2026-11-04T08-00-00-"+forkID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	end := at.Add(2 * time.Second)
	if err := os.Chtimes(path, end, end); err != nil {
		t.Fatal(err)
	}
}

// A fork's copy of its parent's turns is conversation, not turns: the parent keeps them, and
// the fork has only its own turn, whose request carries the copied conversation.
func TestSource_ForkDoesNotRepeatParentTurns(t *testing.T) {
	for _, tc := range []struct{ name, session, subagent string }{
		{"thread fork", forkID, ""},
		{"forked subagent", plainID, forkID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := fixtures(t)
			writeFork(t, dir, tc.session, plainID)
			fork := sessionByID(t, dir, tc.session)
			resps := phase(fork, model.SessionResponse, tc.subagent)
			if len(resps) != 1 || resps[0].Inference.Completion != "forked" || resps[0].Inference.CompletionTokens != 7 {
				t.Fatalf("fork responses = %+v, want only the new turn", resps)
			}
			reqs := phase(fork, model.SessionRequest, tc.subagent)
			var texts []string
			for _, m := range reqs[0].Inference.Messages {
				texts = append(texts, m.Role)
			}
			if got := strings.Join(texts, ","); got != "developer,user,assistant,user" {
				t.Errorf("fork request roles = %s, want the copied conversation first", got)
			}
			if n := len(phase(sessionByID(t, dir, plainID), model.SessionResponse, "")); n != 1 {
				t.Errorf("parent has %d responses, want 1", n)
			}
		})
	}
}

// With no parent rollout to compare, the fork's turns are the only copy, so they stay.
func TestSource_ForkWithoutParentKeepsItsTurns(t *testing.T) {
	for _, parent := range []string{"", "55555555-5555-4555-8555-555555555555"} {
		dir := fixtures(t)
		writeFork(t, dir, forkID, parent)
		if n := len(phase(sessionByID(t, dir, forkID), model.SessionResponse, "")); n != 2 {
			t.Errorf("forked_from_id %q: %d responses, want 2", parent, n)
		}
	}
}
