package accounting

import (
	"bytes"
	"encoding/json"
	"fmt"
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

// The periodic save is skipped when nothing changed: the library's Snapshot holds its tracker
// lock for the whole marshal. A record marks the state changed; an idle host does not.
func TestSnapshot_OnlyARecordMarksTheStateChanged(t *testing.T) {
	h := New(Options{})
	t.Cleanup(func() { _ = h.Close() })
	if h.dirty.Load() {
		t.Fatal("a new host is marked changed")
	}
	pctx := request(t, h, "s1", "/v1/messages", clientBody("hi"), &operator{fwd: forwardedBody("hi")}, 200, coldUsage())
	if _, ok := h.Compute(pctx); !ok {
		t.Fatal("no Result")
	}
	if !h.dirty.Load() {
		t.Error("a priced request did not mark the state changed")
	}
}

// hostWithThreads is a Host at path with one priced request on each session, each a minute
// after the one before, so the sessions' threads are ordered oldest to newest.
func hostWithThreads(t *testing.T, path string, sessions ...string) *Host {
	t.Helper()
	at := time.Now().Add(-time.Hour)
	h := New(Options{SnapshotPath: path, Now: func() time.Time { return at }})
	t.Cleanup(func() { _ = h.Close() })
	for _, s := range sessions {
		at = at.Add(time.Minute)
		pctx := request(t, h, s, "/v1/messages", clientBody("hi"), &operator{fwd: forwardedBody("hi"), ops: []accountant.Operation{pruneOp()}}, 200, coldUsage())
		if _, ok := h.Compute(pctx); !ok {
			t.Fatalf("%s: no Result", s)
		}
	}
	return h
}

// Over snapshotMaxBytes the threads seen longest ago are left out of the file, and what is kept is
// a snapshot the library itself restores. One WARN per crossing.
func TestSnapshot_OverTheCapDropsTheOldestThreads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accountant-state.json")
	h := hostWithThreads(t, path, "s-old", "s-mid", "s-new")
	full, err := h.trackers.Snapshot()
	if err != nil {
		t.Fatal(err)
	}

	logs := captureWarn(t)
	prev := snapshotMaxBytes
	snapshotMaxBytes = len(full) - 1 // one thread too many
	t.Cleanup(func() { snapshotMaxBytes = prev })
	for i := 0; i < 3; i++ {
		if err := h.save(); err != nil {
			t.Fatalf("save over the cap: %v", err)
		}
	}
	if n := strings.Count(logs.String(), "over its size limit"); n != 1 {
		t.Errorf("%d WARNs for three saves over the cap, want 1:\n%s", n, logs)
	}

	again := New(Options{SnapshotPath: path}) // restores through the library's own Restore
	t.Cleanup(func() { _ = again.Close() })
	if strings.Contains(logs.String(), "not usable") {
		t.Fatalf("the trimmed file did not restore:\n%s", logs)
	}
	state, err := again.trackers.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(state) > snapshotMaxBytes {
		t.Errorf("restored state is %d bytes, over the %d cap", len(state), snapshotMaxBytes)
	}
	if strings.Contains(string(state), "s-old") {
		t.Error("the oldest thread was kept")
	}
	for _, s := range []string{"s-mid", "s-new"} {
		if !strings.Contains(string(state), s) {
			t.Errorf("thread %s was dropped, want it kept", s)
		}
	}
	// The host's memory still holds every thread: only the file was trimmed.
	if mem, _ := h.trackers.Snapshot(); !strings.Contains(string(mem), "s-old") {
		t.Error("the oldest thread left memory too")
	}
}

// A snapshot format this code does not know is never trimmed by guessing: the file is removed, so
// a restart starts fresh.
func TestTrimSnapshot_UnknownVersionIsRefused(t *testing.T) {
	for _, in := range []string{`{"version":2,"threads":[]}`, `{"version":1}`, `not json`, `{"version":1,"threads":[1]}`} {
		if _, _, err := trimSnapshot([]byte(in), 1<<20); err == nil {
			t.Errorf("trimSnapshot(%s) = no error, want a refusal", in)
		}
	}
}

func TestSnapshot_UntrimmableOverTheCapRemovesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accountant-state.json")
	h := hostWithThreads(t, path, "s1")
	if err := h.save(); err != nil {
		t.Fatal(err)
	}
	logs := captureWarn(t)
	prevMax, prevTrim := snapshotMaxBytes, trimSnapshotFn
	snapshotMaxBytes = 10
	trimSnapshotFn = func([]byte, int) ([]byte, int, error) {
		return nil, 0, fmt.Errorf("snapshot version 2, can trim only 1")
	}
	t.Cleanup(func() { snapshotMaxBytes, trimSnapshotFn = prevMax, prevTrim })
	for i := 0; i < 2; i++ {
		if err := h.save(); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the old file is still there (err %v), want it removed", err)
	}
	if n := strings.Count(logs.String(), "could not be trimmed"); n != 1 {
		t.Errorf("%d WARNs for two saves, want 1:\n%s", n, logs)
	}
}
