package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestTrigger returns a trigger with a fake clock and a start that counts its calls.
func newTestTrigger(t *testing.T, startErr error) (*updateTrigger, *time.Time, *int) {
	t.Helper()
	now := time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)
	starts := 0
	tr := &updateTrigger{
		file:  filepath.Join(t.TempDir(), updateStatusFile),
		now:   func() time.Time { return now },
		start: func() error { starts++; return startErr },
	}
	return tr, &now, &starts
}

func writeStatusAged(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateTriggerStartsOnMissingFile(t *testing.T) {
	tr, _, starts := newTestTrigger(t, nil)
	if !tr.tick() || *starts != 1 {
		t.Fatalf("tick on a missing file: starts = %d, want 1", *starts)
	}
}

func TestUpdateTriggerStartsOnOldFile(t *testing.T) {
	tr, now, starts := newTestTrigger(t, nil)
	writeStatusAged(t, tr.file, now.Add(-25*time.Hour))
	if !tr.tick() || *starts != 1 {
		t.Fatalf("tick on a 25h-old file: starts = %d, want 1", *starts)
	}
}

func TestUpdateTriggerSkipsFreshFile(t *testing.T) {
	tr, now, starts := newTestTrigger(t, nil)
	writeStatusAged(t, tr.file, now.Add(-time.Hour))
	if tr.tick() || *starts != 0 {
		t.Fatalf("tick on a 1h-old file: starts = %d, want 0", *starts)
	}
}

func TestUpdateTriggerWaits24hAfterAStartWithNoFile(t *testing.T) {
	tr, now, starts := newTestTrigger(t, nil)
	base := *now
	for _, d := range []time.Duration{0, time.Hour, 23 * time.Hour} {
		*now = base.Add(d)
		tr.tick()
	}
	if *starts != 1 {
		t.Fatalf("ticks at +0, +1h, +23h with no file: starts = %d, want 1", *starts)
	}
	*now = base.Add(24 * time.Hour)
	tr.tick()
	if *starts != 2 {
		t.Fatalf("tick at +24h: starts = %d, want 2", *starts)
	}
}

func TestUpdateTriggerIgnoresStartError(t *testing.T) {
	tr, now, starts := newTestTrigger(t, errors.New("no such unit"))
	if !tr.tick() {
		t.Fatal("tick returned false, want true: it called start")
	}
	*now = now.Add(time.Hour)
	if tr.tick() || *starts != 1 {
		t.Fatalf("tick 1h after a failed start: starts = %d, want 1", *starts)
	}
}

// time.Now carries a monotonic reading, and a monotonic Sub leaves out the time a
// laptop sleeps. lastStart must hold the wall clock only.
func TestUpdateTriggerLastStartIsWallClock(t *testing.T) {
	tr, _, _ := newTestTrigger(t, nil)
	tr.now = time.Now
	tr.tick()
	if strings.Contains(tr.lastStart.String(), "m=") {
		t.Errorf("lastStart %s has a monotonic reading", tr.lastStart)
	}
}
