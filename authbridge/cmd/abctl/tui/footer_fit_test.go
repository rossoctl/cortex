package tui

import (
	"strings"
	"testing"
)

// Quit and help must survive at every terminal width. The events footer runs ~135
// columns, so an 80-column terminal cut the tail — and the tail is exactly where
// [?] and [q] were, the pair a stuck user reaches for.
func TestFitHintLine_KeepsQuitAndHelpAtEveryWidth(t *testing.T) {
	m := &model{pane: paneEvents, eventColumns: defaultColumnSelection()}
	full := m.helpView()

	for _, width := range []int{200, 140, 120, 100, 80, 60, 40, 24, 20} {
		got := fitHintLine(full, width)
		if len([]rune(got)) > width {
			t.Errorf("width %d: hint is %d columns:\n%q", width, len([]rune(got)), got)
		}
		if !strings.Contains(got, "[q] quit") {
			t.Errorf("width %d: lost [q] quit:\n%q", width, got)
		}
		// Help is the complete reference, so it outlives every specialised hint.
		if width >= 24 && !strings.Contains(got, "[?] keys") {
			t.Errorf("width %d: lost [?] keys:\n%q", width, got)
		}
	}
}

// A truncated line says so. Without the marker, a missing hint reads as a key that
// does not exist rather than one that did not fit.
func TestFitHintLine_MarksTruncation(t *testing.T) {
	m := &model{pane: paneEvents, eventColumns: defaultColumnSelection()}
	full := m.helpView()

	if got := fitHintLine(full, 60); !strings.HasPrefix(got, "…") {
		t.Errorf("truncated line does not start with an ellipsis: %q", got)
	}
	// And an untruncated line does not claim to be.
	wide := fitHintLine(full, 400)
	if strings.HasPrefix(wide, "…") {
		t.Errorf("untruncated line marked as truncated: %q", wide)
	}
	if wide != full {
		t.Error("a line that fits was altered")
	}
}

// Dropping is from the front, so the hints that survive are the ones helpView put
// last. Ordering and trimming are two halves of one mechanism.
func TestFitHintLine_DropsFromTheFront(t *testing.T) {
	m := &model{pane: paneEvents, eventColumns: defaultColumnSelection()}
	got := fitHintLine(m.helpView(), 60)

	// The specialised, pane-specific keys go first.
	for _, gone := range []string{"[↑↓] nav", "[c] columns", "[u] usage"} {
		if strings.Contains(got, gone) {
			t.Errorf("width 60 kept %q, which should have been dropped: %q", gone, got)
		}
	}
}

// Every pane's footer must fit, not only the events one — the others are shorter
// but the guard should hold for all of them.
func TestFitHintLine_EveryPaneFitsAt80(t *testing.T) {
	for _, pane := range []paneID{
		paneNamespaces, panePods, paneSessions, paneEvents,
		paneDetail, paneUsage, panePipeline, panePluginDetail, paneCatalog,
	} {
		m := &model{pane: pane, eventColumns: defaultColumnSelection()}
		got := fitHintLine(m.helpView(), 80)
		if len([]rune(got)) > 80 {
			t.Errorf("pane %v: hint is %d columns at 80:\n%q", pane, len([]rune(got)), got)
		}
		if !strings.Contains(got, "[q] quit") {
			t.Errorf("pane %v: lost [q] quit at 80 columns:\n%q", pane, got)
		}
	}
}

// A zero width means the size is not known yet (before the first WindowSizeMsg);
// pass the line through rather than trimming it to nothing.
func TestFitHintLine_UnknownWidthIsUnchanged(t *testing.T) {
	m := &model{pane: paneEvents, eventColumns: defaultColumnSelection()}
	full := m.helpView()
	if got := fitHintLine(full, 0); got != full {
		t.Error("zero width altered the hint line")
	}
}
