package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/rossoctl/cortex/core/cost/usage"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
)

// recordSaves wires a save hook onto m that records every call, and returns a
// pointer to the recorded settings slice. Settings is package-global, so every
// caller resets it too.
func recordSaves(t *testing.T, m *model) *[]UserSettings {
	t.Helper()
	resetSettingsForTest(t)
	var got []UserSettings
	m.save = func(s UserSettings) error {
		got = append(got, s)
		return nil
	}
	return &got
}

// TestColumnPicker_ClosingSavesTheSelection: each of the three close keys is a
// deliberate "I am done choosing", so each one persists.
func TestColumnPicker_ClosingSavesTheSelection(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyMsg
	}{
		{"c", keyRune('c')},
		{"esc", tea.KeyMsg{Type: tea.KeyEsc}},
		{"enter", tea.KeyMsg{Type: tea.KeyEnter}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestEventsModel(t)
			saves := recordSaves(t, m)

			m.handleKey(keyRune('c'))
			// Move to TOKENS and turn it off, so the saved settings carry a real
			// deviation rather than an empty list that would pass trivially.
			for m.colCursor < len(eventColumns) && eventColumns[m.colCursor].id != colTokens {
				m.handleKey(keyRune('j'))
			}
			m.handleKey(keyRune(' '))
			if len(*saves) != 0 {
				t.Fatalf("toggling saved %d times; the save belongs on close", len(*saves))
			}

			m.handleKey(tc.key)
			if m.colPicker {
				t.Fatalf("%q did not close the picker", tc.name)
			}
			if len(*saves) != 1 {
				t.Fatalf("closing with %q produced %d saves, want exactly 1", tc.name, len(*saves))
			}
			want := []ColumnSetting{{Name: string(colTokens), Visible: false}}
			got := (*saves)[0].Events.Columns
			if len(got) != 1 || got[0] != want[0] {
				t.Errorf("saved columns = %v, want %v", got, want)
			}
		})
	}
}

// TestColumnPicker_TogglingDoesNotSave pins the timing decision: a user trying
// four columns on the way to the two they want must not produce three writes
// describing states they rejected.
func TestColumnPicker_TogglingDoesNotSave(t *testing.T) {
	m := newTestEventsModel(t)
	saves := recordSaves(t, m)

	m.handleKey(keyRune('c'))
	for i := 0; i < 4; i++ {
		m.handleKey(keyRune(' '))
		m.handleKey(keyRune('j'))
	}
	m.handleKey(keyRune('r')) // reset is a toggle too, not a commit
	if len(*saves) != 0 {
		t.Errorf("%d saves while the picker was still open, want 0", len(*saves))
	}
}

// TestColumnPicker_QuitDoesNotSave: `q` inside the picker falls through to the
// global quit handler. Quitting is not settling on a selection.
func TestColumnPicker_QuitDoesNotSave(t *testing.T) {
	m := newTestEventsModel(t)
	// `q` cancels the context on its way to tea.Quit.
	m.ctx, m.cancel = context.WithCancel(context.Background())
	saves := recordSaves(t, m)

	m.handleKey(keyRune('c'))
	m.handleKey(keyRune(' '))
	m.handleKey(keyRune('q'))
	if len(*saves) != 0 {
		t.Errorf("`q` in the picker saved %d times, want 0", len(*saves))
	}
}

// TestPersistSettings_NilHookIsSafe: nil means "no persistence", which is what
// every other test in this package relies on implicitly and what main passes when
// there is no resolvable home directory.
func TestPersistSettings_NilHookIsSafe(t *testing.T) {
	m := newTestEventsModel(t)
	resetSettingsForTest(t)
	m.save = nil

	m.handleKey(keyRune('c'))
	m.handleKey(keyRune(' '))
	m.handleKey(keyRune('c')) // close: would call the nil hook
	if m.colPicker {
		t.Error("picker did not close")
	}
}

// TestPersistSettings_FailureFlashesAndDoesNotCrash: the TUI owns the terminal, so
// a failed save cannot go to stderr. It goes to the footer once and is dropped.
func TestPersistSettings_FailureFlashesAndDoesNotCrash(t *testing.T) {
	m := newTestEventsModel(t)
	resetSettingsForTest(t)
	m.save = func(UserSettings) error { return errors.New("read-only file system") }

	m.handleKey(keyRune('c'))
	m.handleKey(keyRune(' '))
	m.handleKey(keyRune('c'))

	if m.colPicker {
		t.Error("a failed save left the picker open")
	}
	if !strings.Contains(m.flash, "could not save settings") {
		t.Errorf("flash = %q, want it to name the failed save", m.flash)
	}
	if !strings.Contains(m.flash, "read-only file system") {
		t.Errorf("flash = %q, want the underlying cause", m.flash)
	}
}

// TestSearch_NoKeySaves: the filter `/` replaced was saved on Enter and restored on the
// next start, which is how one forgotten in an earlier run made sessions look missing in
// the next (#1339). No key of the search writes the config file — not typing, not Esc,
// not Enter, not n.
func TestSearch_NoKeySaves(t *testing.T) {
	m := newTestEventsModel(t)
	m.searchInput = newSearchInput()
	saves := recordSaves(t, m)

	m.handleKey(keyRune('/'))
	for _, r := range "api" {
		m.handleKey(keyRune(r))
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	m.handleKey(keyRune('/'))
	for _, r := range "api" {
		m.handleKey(keyRune(r))
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m.handleKey(keyRune('n'))
	if m.search[paneEvents] != "api" {
		t.Fatalf("search = %q after Enter, want %q — the keys did not reach it", m.search[paneEvents], "api")
	}
	if len(*saves) != 0 {
		t.Errorf("searching saved settings %d times, want 0", len(*saves))
	}
}

// TestBackToPodsPane_ClearsTheSearch: a search names rows of the connection being left, so
// teardown drops it and the prompt's text with it. Left behind, the next pod's list would come
// up marked for a search typed against another one.
func TestBackToPodsPane_ClearsTheSearch(t *testing.T) {
	resetSettingsForTest(t)
	m := newPickerModel(context.Background(), nil, nil)
	m.parentCtx = context.Background()
	m.bodyHeight, m.width = 12, 200

	m.pane = paneSessions
	m.handleKey(keyRune('/'))
	for _, r := range "github-tool" {
		m.handleKey(keyRune(r))
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})

	m.backToPodsPane()

	if s := m.search[paneSessions]; s != "" {
		t.Fatalf("search = %q after teardown; it belongs to the pod that was left", s)
	}
	if v := m.searchInput.Value(); v != "" {
		t.Fatalf("searchInput = %q after teardown", v)
	}

	// The next pod: `/` then one character must mean that one character.
	m.pane = paneSessions
	m.handleKey(keyRune('/'))
	m.handleKey(keyRune('x'))
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})

	if s := m.search[paneSessions]; s != "x" {
		t.Errorf("search = %q, want %q — the previous pod's search leaked into this one", s, "x")
	}
}

// TestConstructors_SeedFromSettings: both constructors must honour a loaded
// config. newPickerModel historically set neither field, so the picker path
// ignored the user's saved selection entirely.
func TestConstructors_SeedFromSettings(t *testing.T) {
	resetSettingsForTest(t)
	Settings = UserSettings{
		Events: EventSettings{Columns: []ColumnSetting{{Name: string(colCost), Visible: false}}},
	}

	for _, tc := range []struct {
		name string
		m    *model
	}{
		{"New", New(context.Background(), apiclient.New("http://localhost:0")).(*model)},
		{"newPickerModel", newPickerModel(context.Background(), nil, nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.m.eventColumns == nil {
				t.Fatal("eventColumns is nil; the column picker would panic writing to it")
			}
			if tc.m.eventColumns[colCost] {
				t.Error("COST was saved off but the model has it on")
			}
			if !tc.m.eventColumns[colHost] {
				t.Error("HOST was not mentioned in the config and should default on")
			}
		})
	}
}

// cursorToColumn moves the picker cursor onto a named column. The picker opens at
// index 0, and `j` walks down.
func cursorToColumn(t *testing.T, m *model, id eventColumnID) {
	t.Helper()
	for i := 0; i < len(eventColumns); i++ {
		if eventColumns[m.colCursor].id == id {
			return
		}
		m.handleKey(keyRune('j'))
	}
	t.Fatalf("never reached column %s (cursor stuck at %d)", id, m.colCursor)
}

// `s` in the picker cycles one column through descending → ascending →
// chronological (#865). Descending first, because "longest duration / highest cost"
// wants the extreme at the top.
func TestColumnPicker_SortKeyCyclesThreeStates(t *testing.T) {
	m := newTestEventsModel(t)
	resetSettingsForTest(t)

	m.handleKey(keyRune('c'))
	cursorToColumn(t, m, colDuration)

	m.handleKey(keyRune('s'))
	if m.sortCol != colDuration || !m.sortDesc {
		t.Errorf("first press: sortCol=%q desc=%v, want DURATION descending", m.sortCol, m.sortDesc)
	}
	m.handleKey(keyRune('s'))
	if m.sortCol != colDuration || m.sortDesc {
		t.Errorf("second press: sortCol=%q desc=%v, want DURATION ascending", m.sortCol, m.sortDesc)
	}
	m.handleKey(keyRune('s'))
	if m.sortCol != "" {
		t.Errorf("third press: sortCol=%q, want chronological", m.sortCol)
	}
}

// Pressing `s` on a DIFFERENT column jumps straight to it, descending — a fresh
// column is a fresh question, not an inheritance of the previous direction.
func TestColumnPicker_SortOnNewColumnStartsDescending(t *testing.T) {
	m := newTestEventsModel(t)
	resetSettingsForTest(t)

	m.handleKey(keyRune('c'))
	cursorToColumn(t, m, colDuration)
	m.handleKey(keyRune('s'))
	m.handleKey(keyRune('s')) // DURATION ascending
	if m.sortDesc {
		t.Fatalf("setup: expected DURATION ascending")
	}

	cursorToColumn(t, m, colHost)
	m.handleKey(keyRune('s'))
	if m.sortCol != colHost || !m.sortDesc {
		t.Errorf("sortCol=%q desc=%v, want HOST descending", m.sortCol, m.sortDesc)
	}
}

// "#" has no sort key — its order IS arrival order — so `s` there says so rather
// than appearing to do nothing inside a modal that swallows every other key.
func TestColumnPicker_SortOnIndexColumnExplainsItself(t *testing.T) {
	m := newTestEventsModel(t)
	resetSettingsForTest(t)

	m.handleKey(keyRune('c'))
	cursorToColumn(t, m, colIndex)
	m.handleKey(keyRune('s'))

	if m.sortCol != "" {
		t.Errorf("sortCol = %q, want unchanged", m.sortCol)
	}
	if !strings.Contains(m.flash, string(colIndex)) {
		t.Errorf("flash = %q, want it to mention %s", m.flash, colIndex)
	}
}

// `r` resets the whole view, and chronological is part of the default view for the
// same reason the default column set is.
func TestColumnPicker_ResetClearsTheSort(t *testing.T) {
	m := newTestEventsModel(t)
	resetSettingsForTest(t)

	m.handleKey(keyRune('c'))
	cursorToColumn(t, m, colDuration)
	m.handleKey(keyRune('s'))
	if m.sortCol == "" {
		t.Fatal("setup: no sort applied")
	}
	m.handleKey(keyRune('r'))
	if m.sortCol != "" || m.sortDesc {
		t.Errorf("after reset: sortCol=%q desc=%v, want chronological", m.sortCol, m.sortDesc)
	}
}

// The sort rides the same save the column selection does, so it survives a restart.
func TestColumnPicker_ClosingSavesTheSort(t *testing.T) {
	m := newTestEventsModel(t)
	saves := recordSaves(t, m)

	m.handleKey(keyRune('c'))
	cursorToColumn(t, m, colCost)
	m.handleKey(keyRune('s')) // COST descending
	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})

	if len(*saves) != 1 {
		t.Fatalf("saves = %d, want 1", len(*saves))
	}
	got := (*saves)[0].Events
	if got.SortColumn != string(colCost) || !got.SortDesc {
		t.Errorf("saved sortColumn=%q sortDesc=%v, want COST/true", got.SortColumn, got.SortDesc)
	}
}

// A file with no sort — every file written before #865 — restores chronological
// order, and one naming a column this build does not have is ignored rather than
// leaving the table sorted by something unreachable.
func TestSortSelection_ValidatesAgainstTheDefinition(t *testing.T) {
	for _, tc := range []struct {
		name     string
		col      string
		desc     bool
		wantCol  eventColumnID
		wantDesc bool
	}{
		{"absent means chronological", "", false, "", false},
		{"known column", string(colDuration), true, colDuration, true},
		{"known column ascending", string(colHost), false, colHost, false},
		{"unknown column is ignored", "WIDGETS", true, "", false},
		{"# has no sort key", string(colIndex), true, "", false},
		// A stale direction with no column is not a sort: the zero SortColumn wins.
		{"direction without a column", "", true, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := UserSettings{Events: EventSettings{SortColumn: tc.col, SortDesc: tc.desc}}
			col, desc := u.sortSelection()
			if col != tc.wantCol || desc != tc.wantDesc {
				t.Errorf("sortSelection() = %q/%v, want %q/%v", col, desc, tc.wantCol, tc.wantDesc)
			}
		})
	}
}

// Hiding the sorted column keeps the sort — a visibility toggle must not silently
// reorder the table. Pinned because the state is only defensible while it stays
// discoverable: the footer names the ordering, the picker still lists the hidden
// column, and both `s` and `r` recover.
func TestColumnPicker_HidingTheSortedColumnKeepsTheSortRecoverable(t *testing.T) {
	m := newTestEventsModel(t)
	resetSettingsForTest(t)
	m.width = 200
	m.connState = connStateInfo{phase: connOpen}

	m.handleKey(keyRune('c'))
	cursorToColumn(t, m, colDuration)
	m.handleKey(keyRune('s')) // DURATION descending
	m.handleKey(keyRune(' ')) // hide it

	if m.eventColumns[colDuration] {
		t.Fatal("space did not hide DURATION")
	}
	if m.sortCol != colDuration {
		t.Errorf("sortCol = %q, want DURATION kept", m.sortCol)
	}
	// The footer is the only thing left naming the ordering.
	if st := stripANSI(strings.Split(m.footerView(), "\n")[0]); !strings.Contains(st, "[sort: DURATION") {
		t.Errorf("footer does not name the ordering of a hidden sorted column: %q", st)
	}
	// The picker still lists it, unchecked, so the cursor can reach it again.
	pk := stripANSI(renderColumnPicker(m.eventColumns, m.eventColWidths, m.colCursor, 160, 40, m.sortCol, m.sortDesc))
	if !strings.Contains(pk, "[ ] DURATION") {
		t.Errorf("picker does not offer the hidden sorted column: %q", pk)
	}
	// `s` cycles out of it, and `r` restores both the column and chronological order.
	m.handleKey(keyRune('s'))
	m.handleKey(keyRune('s'))
	if m.sortCol != "" {
		t.Errorf("s-cycle did not reach chronological: %q", m.sortCol)
	}
	m.handleKey(keyRune('s'))
	m.handleKey(keyRune('r'))
	if m.sortCol != "" || !m.eventColumns[colDuration] {
		t.Errorf("after r: sortCol=%q visible=%v, want chronological and visible",
			m.sortCol, m.eventColumns[colDuration])
	}
}

// The usage pane's three view choices must reach the file, or the operator picks
// them again on every start (#953).
//
// Each key is asserted separately because each writes a different field, and a
// helper that saved only the one just changed would leave the other two stale —
// the failure mode being "two thirds of the view I left".
func TestUsagePane_ViewChoicesArePersisted(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  rune
		want func(UsageSettings) bool
		desc string
	}{
		{"m cycles the metric", 'm', func(u UsageSettings) bool { return u.Metric != "" }, "Metric"},
		{"w cycles the window", 'w', func(u UsageSettings) bool { return u.Window != "" }, "Window"},
		{"o cycles the breakdown", 'o', func(u UsageSettings) bool { return u.Group != "" }, "Group"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestEventsModel(t)
			saves := recordSaves(t, m)
			m.pane = paneUsage

			m.handleKey(keyRune(tc.key))

			if len(*saves) == 0 {
				t.Fatalf("[%c] saved nothing; the choice is lost on restart", tc.key)
			}
			last := (*saves)[len(*saves)-1]
			if !tc.want(last.Usage) {
				t.Errorf("[%c] did not record %s: %+v", tc.key, tc.desc, last.Usage)
			}
		})
	}
}

// Whatever the pane holds must survive a round trip through the file, all three
// fields together: a reader returning expects the view they left.
func TestUsageSettings_RoundTripsEveryField(t *testing.T) {
	resetSettingsForTest(t)

	// Every field off its default, so nothing passes by coincidence.
	saved := captureUsage(metricErrors, 2, usage.GroupHost)
	if saved.Metric == "" || saved.Window == "" || saved.Group == "" {
		t.Fatalf("capture dropped a field: %+v", saved)
	}

	metric, windowIdx, group := UserSettings{Usage: saved}.usageSelection()
	if metric != metricErrors {
		t.Errorf("metric = %v, want %v", metric, metricErrors)
	}
	if windowIdx != 2 {
		t.Errorf("windowIdx = %d, want 2", windowIdx)
	}
	if group != usage.GroupHost {
		t.Errorf("group = %q, want %q", group, usage.GroupHost)
	}
}

// A pane left as it opened must add nothing to the file, so a config that was
// never customised stays empty — the same property the column list has.
func TestUsageSettings_DefaultsRecordNothing(t *testing.T) {
	if got := captureUsage(metricTokens, 0, usage.GroupNone); got != (UsageSettings{}) {
		t.Errorf("defaults recorded %+v, want the zero value", got)
	}
	// The zero value of the field, i.e. what an older file carries.
	if got := captureUsage(metricTokens, 0, ""); got != (UsageSettings{}) {
		t.Errorf("empty group recorded %+v, want the zero value", got)
	}
}

// A name this build does not have must fall back to the default rather than
// leaving the pane on a metric it cannot render or a window index out of range.
// That covers both an older file and a newer one written by a build that has since
// dropped a metric.
func TestUsageSettings_UnknownNamesFallBackToDefaults(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   UsageSettings
	}{
		{"unknown everything", UsageSettings{Metric: "nosuch", Window: "99h0m0s", Group: "bogus"}},
		{"empty everything", UsageSettings{}},
		{"window no longer offered", UsageSettings{Window: "3h0m0s"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metric, windowIdx, group := UserSettings{Usage: tc.in}.usageSelection()
			if metric != metricTokens {
				t.Errorf("metric = %v, want the tokens default", metric)
			}
			if windowIdx != 0 {
				t.Errorf("windowIdx = %d, want 0", windowIdx)
			}
			if windowIdx >= len(usageWindows) {
				t.Errorf("windowIdx %d is out of range for usageWindows", windowIdx)
			}
			if group != usage.GroupNone && group != "" {
				t.Errorf("group = %q, want ungrouped", group)
			}
		})
	}
}
