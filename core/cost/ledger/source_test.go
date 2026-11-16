package ledger

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/model"
)

func TestRecord_ImportedRowLandsInItsOwnDayWithSource(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, func() time.Time { return at })

	e := costedEvent(t, "api.anthropic.com", "claude-sonnet-4-5", 0.25, 1000, 100)
	e.At = at.Add(-72 * time.Hour)
	e.Source = model.ImportedSource("claude-code")
	w.Record("s1", e)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	b, err := os.ReadFile(w.store.path(e.At))
	if err != nil {
		t.Fatalf("read the day file of the event: %v", err)
	}
	if !bytesContains(b, `"source":"transcript"`) {
		t.Errorf("day file %s = %s, want a row with source transcript", filepath.Base(w.store.path(e.At)), b)
	}
	rows := readAllRows(t, dir)
	if len(rows) != 1 || rows[0].Source != event.SourceTranscript {
		t.Fatalf("rows = %+v, want one row with Source %q", rows, event.SourceTranscript)
	}
}

func TestRecord_LiveRowHasNoSource(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if b := readAllBytes(t, dir); bytesContains(b, `"source"`) {
		t.Errorf("live row = %s, want no source key", b)
	}
}

func TestRecord_SourceSplitsTheKey(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	live := costedEvent(t, "gw", "m", 0.25, 100, 50)
	imp := costedEvent(t, "gw", "m", 0.5, 100, 50)
	imp.Source = model.ImportedSource("claude-code")
	w.Record("s1", live)
	w.Record("s2", imp)
	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if rows := readAllRows(t, dir); len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: one live, one imported", len(rows))
	}
	rows, _, err := w.Window(context.Background(), at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil {
		t.Fatalf("Window: %v", err)
	}
	var micros int64
	for _, r := range rows {
		micros += r.CostMicros
	}
	if want := int64(750_000); micros != want {
		t.Errorf("Window total = %d micros, want %d (both rows)", micros, want)
	}
}
