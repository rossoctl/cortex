package codex

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// sameNewest checks that the fast scan of each session gives the Updated of its parse. Rule 2
// can then skip an unchanged session before the parse.
func sameNewest(t *testing.T, dir string) {
	t.Helper()
	files, _ := list(filepath.Join(dir, "sessions"))
	groups := group(files)
	for _, s := range readAll(t, New(dir), time.Time{}) {
		if at, ok := newestAt(groups[s.ID]); !ok || !at.Equal(s.Updated) {
			t.Errorf("session %s: newestAt = %v, %v; want Updated %v", s.ID, at, ok, s.Updated)
		}
	}
}

// Codex writes a line after it takes the line's timestamp, so the mtime of a rollout is after
// its newest event. A second import must still skip each unchanged session before the parse.
func TestSource_UnchangedSessionIsNotParsedAgain(t *testing.T) {
	dir := fixtures(t)
	now := time.Now()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		return os.Chtimes(path, now, now)
	})
	if err != nil {
		t.Fatal(err)
	}
	sameNewest(t, dir)
	archived := map[string]time.Time{}
	for _, s := range readAll(t, New(dir), time.Time{}) {
		archived[s.ID] = s.Updated
	}
	parsed := func() []string {
		var out []string
		for s, err := range New(dir).Sessions(time.Time{}, func(id string, at time.Time) bool {
			return !at.After(archived[id]) // Rule 2, as importer.Run applies it
		}, func(int, int) {}) {
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, s.ID)
		}
		return out
	}
	if got := parsed(); len(got) != 0 {
		t.Fatalf("parsed again: %v, want none", got)
	}

	f, err := os.OpenFile(filepath.Join(dir, plainFile), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`{"timestamp":"2026-11-02T10:00:20Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"q2"}]}}` + "\n" +
		`{"timestamp":"2026-11-02T10:00:21Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a2"}]}}` + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed(); !slices.Equal(got, []string{plainID}) {
		t.Fatalf("parsed again after a new turn: %v, want %s", got, plainID)
	}
}

// The scan closes a turn where the parse closes it.
func TestSource_NewestAtFollowsTheParse(t *testing.T) {
	tokenCount := func(sec int) string {
		return `{"timestamp":"2026-11-04T08:00:` + pad(sec) + `Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":9,"output_tokens":1,"total_tokens":10}}}}`
	}
	call := `{"timestamp":"2026-11-04T08:00:03Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{}","call_id":"c1"}}`
	result := `{"timestamp":"2026-11-04T08:00:04Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"ok"}}`
	refused := `{"timestamp":"2026-11-04T08:00:05Z","type":"response_item","payload":{"type":"message","role":"user","tools":"x","content":"x"}}`
	for name, lines := range map[string][]string{
		"usage after the tool results":   {user(2, "q"), call, result, tokenCount(6)},
		"refused item in an open turn":   {user(2, "q"), assistant(3, "a"), refused, tokenCount(6)},
		"usage with no open turn":        {user(2, "q"), assistant(3, "a"), tokenCount(4), tokenCount(6)},
		"open turn at the end":           {user(2, "q"), call, result},
		"input after the output":         {user(2, "q"), assistant(3, "a"), user(6, "q2")},
		"input closes, usage comes late": {user(2, "q"), assistant(3, "a"), user(4, "q2"), tokenCount(6)},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			writeRollout(t, home, "rollout-a-"+plainID+".jsonl", time.Date(2026, 11, 4, 8, 1, 0, 0, time.UTC),
				append([]string{meta, ctx}, lines...)...)
			sameNewest(t, home)
		})
	}
}
