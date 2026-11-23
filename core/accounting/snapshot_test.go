package accounting

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
)

// captureWarn sends WARN and above to a buffer for the rest of the test.
func captureWarn(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logs
}

// hostWithState is a Host at path whose Trackers hold one thread with an active change.
func hostWithState(t *testing.T, path string, now func() time.Time) *Host {
	t.Helper()
	h := New(Options{SnapshotPath: path, Now: now})
	pctx := request(t, h, "s1", "/v1/messages", clientBody("hi"), &operator{fwd: forwardedBody("hi"), ops: []accountant.Operation{pruneOp()}}, 200, coldUsage())
	if _, ok := h.Compute(pctx); !ok {
		t.Fatal("no Result")
	}
	return h
}

func TestSnapshot_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "accountant-state.json")
	h := hostWithState(t, path, nil)
	want, err := h.trackers.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	var f map[string]any
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &f); err != nil || f["savedAt"] == nil || f["state"] == nil {
		t.Fatalf("file = %s, want {savedAt, state}", data)
	}
	if _, err := time.Parse(time.RFC3339, f["savedAt"].(string)); err != nil {
		t.Errorf("savedAt is not RFC3339: %v", err)
	}

	again := New(Options{SnapshotPath: path})
	t.Cleanup(func() { _ = again.Close() })
	got, err := again.trackers.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("restored state differs:\n got %s\nwant %s", got, want)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("directory holds %d files, want only the snapshot (no temp file left)", len(entries))
	}
}

func TestSnapshot_OlderThanHourIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accountant-state.json")
	old := time.Now().Add(-61 * time.Minute)
	h := hostWithState(t, path, func() time.Time { return old })
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	logs := captureWarn(t)
	fresh := New(Options{SnapshotPath: path})
	t.Cleanup(func() { _ = fresh.Close() })
	state, _ := fresh.trackers.Snapshot()
	if strings.Contains(string(state), "test:big") {
		t.Errorf("a snapshot older than 1h was restored: %s", state)
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Errorf("WARN lines = %d, want 1:\n%s", n, logs)
	}
}

// Review (r4659043710): a SavedAt in the future (clock stepped back) must be rejected too, with
// its own message rather than the over-age one.
func TestSnapshot_FutureClockIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accountant-state.json")
	future := time.Now().Add(10 * time.Minute)
	h := hostWithState(t, path, func() time.Time { return future })
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	logs := captureWarn(t)
	fresh := New(Options{SnapshotPath: path})
	t.Cleanup(func() { _ = fresh.Close() })
	state, _ := fresh.trackers.Snapshot()
	if strings.Contains(string(state), "test:big") {
		t.Errorf("a snapshot saved in the future was restored: %s", state)
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Errorf("WARN lines = %d, want 1:\n%s", n, logs)
	}
	if strings.Contains(logs.String(), "older than") {
		t.Errorf("future SavedAt logged the over-age message, not the future-clock one:\n%s", logs)
	}
}

// Review Focus 5: a corrupt snapshot starts fresh with one WARN and does not stop the host.
func TestSnapshot_CorruptIgnored(t *testing.T) {
	for name, content := range map[string]string{
		"not JSON":        "{{{",
		"no savedAt":      `{"state":"e30="}`,
		"state not state": `{"savedAt":"` + time.Now().UTC().Format(time.RFC3339) + `","state":"bm90IGpzb24="}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "accountant-state.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			logs := captureWarn(t)
			h := New(Options{SnapshotPath: path})
			t.Cleanup(func() { _ = h.Close() })
			if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
				t.Errorf("WARN lines = %d, want 1:\n%s", n, logs)
			}
			pctx := request(t, h, "s1", "/v1/messages", clientBody("hi"), &operator{fwd: forwardedBody("hi")}, 200, coldUsage())
			if _, ok := h.Compute(pctx); !ok {
				t.Error("the host does not work after a corrupt snapshot")
			}
		})
	}
}

func TestSnapshot_MissingFileIsSilent(t *testing.T) {
	logs := captureWarn(t)
	h := New(Options{SnapshotPath: filepath.Join(t.TempDir(), "accountant-state.json")})
	t.Cleanup(func() { _ = h.Close() })
	if logs.Len() != 0 {
		t.Errorf("a first start logged:\n%s", logs)
	}
}
