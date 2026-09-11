package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/apiclient"
)

// newTestFilterInput mirrors the textinput both constructors build. Duplicated
// here rather than extracted from them: this PR has no other reason to touch that
// initialization.
func newTestFilterInput() textinput.Model {
	ti := textinput.New()
	ti.Placeholder = "filter…"
	ti.Prompt = "/ "
	return ti
}

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

// TestFilter_EnterCommitsAndSaves: Enter is the commit, so that is where the
// filter persists.
func TestFilter_EnterCommitsAndSaves(t *testing.T) {
	m := newTestEventsModel(t)
	m.filterInput = newTestFilterInput()
	saves := recordSaves(t, m)

	m.handleKey(keyRune('/'))
	for _, r := range "github" {
		m.handleKey(keyRune(r))
	}
	if len(*saves) != 0 {
		t.Fatalf("typing saved %d times; the save belongs on commit", len(*saves))
	}

	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if len(*saves) != 1 {
		t.Fatalf("Enter produced %d saves, want 1", len(*saves))
	}
	if got := (*saves)[0].Filter; got != "github" {
		t.Errorf("saved filter = %q, want %q", got, "github")
	}
}

// TestFilter_KeystrokesDoNotSave: m.filter is re-read on every character typed
// (the fallthrough in the filtering block), so a save there would write once per
// keypress.
func TestFilter_KeystrokesDoNotSave(t *testing.T) {
	m := newTestEventsModel(t)
	m.filterInput = newTestFilterInput()
	saves := recordSaves(t, m)

	m.handleKey(keyRune('/'))
	for _, r := range "abcdefgh" {
		m.handleKey(keyRune(r))
	}
	if len(*saves) != 0 {
		t.Errorf("%d saves while typing, want 0", len(*saves))
	}
}

// TestFilter_EscCancelsAndPersistsNothing: Esc means cancel here as it does
// everywhere else in abctl, so it restores the filter that was in effect when `/`
// was pressed and writes nothing.
//
// It used to clear the filter outright. Harmless while filters were per-session;
// once they persisted, one mis-keyed Esc permanently discarded a committed filter —
// while the README and the code comment both called the key "cancel".
func TestFilter_EscCancelsAndPersistsNothing(t *testing.T) {
	m := newTestEventsModel(t)
	m.filterInput = newTestFilterInput()
	resetSettingsForTest(t)
	Settings.Filter = "committed"
	m.filter = "committed"
	m.filterInput.SetValue("committed")
	var got []UserSettings
	m.save = func(s UserSettings) error { got = append(got, s); return nil }

	// Edit, then change your mind.
	m.handleKey(keyRune('/'))
	m.handleKey(keyRune('x'))
	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})

	if len(got) != 0 {
		t.Errorf("Esc saved %d times, want 0 — cancelling is not a decision to record", len(got))
	}
	if Settings.Filter != "committed" {
		t.Errorf("Settings.Filter = %q, want the committed filter untouched", Settings.Filter)
	}
	if m.filter != "committed" {
		t.Errorf("m.filter = %q, want the pre-edit filter restored", m.filter)
	}
	if got := m.filterInput.Value(); got != "committed" {
		t.Errorf("filterInput = %q, want the pre-edit value restored so the next `/` is not "+
			"pre-loaded with the abandoned edit", got)
	}
}

// TestFilter_ClearingIsCommittedWithEnter: Esc no longer clears, so emptying the box
// and pressing Enter is the way to clear a filter and make that durable. Without
// this there would be no way to discard a saved filter from the TUI at all.
func TestFilter_ClearingIsCommittedWithEnter(t *testing.T) {
	m := newTestEventsModel(t)
	m.filterInput = newTestFilterInput()
	resetSettingsForTest(t)
	Settings.Filter = "committed"
	m.filter = "committed"
	m.filterInput.SetValue("committed")
	var got []UserSettings
	m.save = func(s UserSettings) error { got = append(got, s); return nil }

	m.handleKey(keyRune('/'))
	m.filterInput.SetValue("")
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})

	if len(got) != 1 {
		t.Fatalf("Enter on an empty box produced %d saves, want 1", len(got))
	}
	if got[0].Filter != "" {
		t.Errorf("saved filter = %q, want it cleared", got[0].Filter)
	}
	if m.filter != "" {
		t.Errorf("m.filter = %q, want it cleared", m.filter)
	}
}

// TestBackToPodsPane_ClearsTheFilterInputToo: the input is seeded from saved
// settings, which makes it a second source of truth that teardown has to reset.
//
// Leaving it behind meant that after backing out to the pod list and entering the
// next pod, the list was correctly unfiltered and the footer badge correctly gone,
// but `/` presented the OLD filter text already in the box — so one keystroke
// committed "github-toolx" and Enter persisted it.
func TestBackToPodsPane_ClearsTheFilterInputToo(t *testing.T) {
	resetSettingsForTest(t)
	m := newPickerModel(context.Background(), nil, nil)
	m.parentCtx = context.Background()
	m.bodyHeight, m.width = 12, 200
	var got []UserSettings
	m.save = func(s UserSettings) error { got = append(got, s); return nil }

	m.pane = paneSessions
	m.handleKey(keyRune('/'))
	for _, r := range "github-tool" {
		m.handleKey(keyRune(r))
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})

	m.backToPodsPane()

	if v := m.filterInput.Value(); v != "" {
		t.Fatalf("filterInput = %q after teardown; the next `/` starts pre-loaded with it", v)
	}

	// The next pod: `/` then one character must mean that one character.
	m.pane = paneSessions
	m.handleKey(keyRune('/'))
	m.handleKey(keyRune('x'))
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})

	if m.filter != "x" {
		t.Errorf("filter = %q, want %q — the previous pod's filter leaked into this one",
			m.filter, "x")
	}
	if Settings.Filter != "x" {
		t.Errorf("Settings.Filter = %q, want %q persisted", Settings.Filter, "x")
	}
}

// TestBackToPodsPane_DoesNotEraseTheSavedFilter is the trap in this design.
// backToPodsPane clears the ACTIVE filter so it cannot survive a pod switch and
// read as data loss — but that clear must not reach Settings, or backing out of a
// pane would silently discard the filter the user saved.
func TestBackToPodsPane_DoesNotEraseTheSavedFilter(t *testing.T) {
	m := newPickerModel(context.Background(), nil, nil)
	saves := recordSaves(t, m)
	Settings.Filter = "github"
	m.filter = "github"
	m.parentCtx = context.Background()

	m.backToPodsPane()

	if m.filter != "" {
		t.Errorf("m.filter = %q; the active filter should clear on teardown", m.filter)
	}
	if Settings.Filter != "github" {
		t.Errorf("Settings.Filter = %q, want the saved value untouched by teardown", Settings.Filter)
	}
	if len(*saves) != 0 {
		t.Errorf("teardown saved %d times, want 0 — it is not a user action", len(*saves))
	}
}

// TestConstructors_SeedFromSettings: both constructors must honour a loaded
// config. newPickerModel historically set neither field, so the picker path
// ignored the user's saved selection entirely.
func TestConstructors_SeedFromSettings(t *testing.T) {
	resetSettingsForTest(t)
	Settings = UserSettings{
		Events: EventSettings{Columns: []ColumnSetting{{Name: string(colCost), Visible: false}}},
		Filter: "seeded",
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
			if tc.m.filter != "seeded" {
				t.Errorf("filter = %q, want the saved %q", tc.m.filter, "seeded")
			}
			// The input, not just the value. Seeding only m.filter applied the filter
			// invisibly — the filter box renders only while filtering, so the list came
			// back truncated with nothing on screen saying why — and worse, `/` then one
			// character REPLACED the saved filter, while `/` then Esc persisted an empty
			// one and discarded it for good. Asserting m.filter alone passed throughout.
			if got := tc.m.filterInput.Value(); got != "seeded" {
				t.Errorf("filterInput = %q, want the saved filter pre-filled so `/` edits "+
					"it rather than replacing it", got)
			}
		})
	}
}

// TestRestoredFilter_IsEditableRatherThanReplaced: with only m.filter seeded, `/`
// opened an EMPTY box, so the first character typed replaced the saved filter
// instead of extending it. Pressing `/` on a restored filter must land the cursor
// in the value that is actually in effect.
func TestRestoredFilter_IsEditableRatherThanReplaced(t *testing.T) {
	resetSettingsForTest(t)
	Settings.Filter = "github"
	m := New(context.Background(), apiclient.New("http://127.0.0.1:1")).(*model)
	m.bodyHeight, m.width = 12, 200

	m.handleKey(keyRune('/'))
	m.handleKey(keyRune('-'))
	m.handleKey(keyRune('t'))
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})

	if got := m.filter; got != "github-t" {
		t.Errorf("filter = %q, want the restored value extended to %q", got, "github-t")
	}
}

// TestRestoredFilter_ShowsInTheFooterWhileInactive: the filter box renders only
// while filtering, so a restored filter silently truncated the list. The footer is
// the only place left to say so.
func TestRestoredFilter_ShowsInTheFooterWhileInactive(t *testing.T) {
	resetSettingsForTest(t)
	Settings.Filter = "github"
	m := New(context.Background(), apiclient.New("http://127.0.0.1:1")).(*model)
	m.width, m.height, m.bodyHeight = 200, 40, 12

	if got := m.footerView(); !strings.Contains(got, "github") {
		t.Errorf("footer does not mention the active filter, so a truncated list has no "+
			"explanation on screen:\n%s", got)
	}

	// While editing, the input box carries it and the indicator would be redundant.
	m.handleKey(keyRune('/'))
	if got := m.footerView(); strings.Contains(got, "[filter:") {
		t.Errorf("footer shows the indicator while the filter box is open:\n%s", got)
	}
}
