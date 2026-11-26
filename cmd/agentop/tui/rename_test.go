package tui

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/core/observe/claude"
	"github.com/rossoctl/cortex/core/session"
)

// `r` on the sessions pane names the highlighted session; Enter saves through the seam and the
// title shows the nickname; an empty name clears it; Esc keeps the old one.
func TestRenameSession(t *testing.T) {
	m := newSessionsModelForTest(t, "s1")
	var saved []string
	orig := saveNickname
	t.Cleanup(func() { saveNickname = orig })
	saveNickname = func(id, name string) error { saved = append(saved, id+"="+name); return nil }

	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if m.renameID != "s1" {
		t.Fatalf("r did not open the prompt: renameID %q", m.renameID)
	}
	for _, r := range "my build" {
		m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.renameID != "" || len(saved) != 1 || saved[0] != "s1=my build" || m.sessionTitleFor("s1", "served") != "my build" {
		t.Fatalf("after enter: saved %v, title %q", saved, m.sessionTitleFor("s1", "served"))
	}
	// Esc keeps the name.
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if len(saved) != 1 || m.sessionTitleFor("s1", "served") != "my build" {
		t.Errorf("esc: saved %v, title %q", saved, m.sessionTitleFor("s1", "served"))
	}
	// An empty name clears it.
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m.renameInput.SetValue("  ")
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if len(saved) != 2 || saved[1] != "s1=" || m.sessionTitleFor("s1", "served") != "served" {
		t.Errorf("clear: saved %v, title %q", saved, m.sessionTitleFor("s1", "served"))
	}
	// A failed save is reported, and the name is not shown.
	saveNickname = func(string, string) error { return errors.New("disk full") }
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m.renameInput.SetValue("nope")
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.sessionTitleFor("s1", "served") == "nope" || m.flash == "" {
		t.Errorf("a failed save: title %q, flash %q", m.sessionTitleFor("s1", "served"), m.flash)
	}
}

// A harvest that started before a rename or a clear carries the old nickname; the viewer keeps
// its own for a session it knows, and takes a harvest's only for one it does not.
func TestAStaleHarvestKeepsTheRename(t *testing.T) {
	m := newSessionsModelForTest(t, "s1")
	m.sessionsData = map[string]SessionMetadata{"s1": {Title: "t", Nickname: "B"}, "s2": {Title: "t2"}}
	m.Update(harvestedMsg{meta: map[string]SessionMetadata{
		"s1": {Title: "fresh", Nickname: "A"},
		"s2": {Title: "t2", Nickname: "stale"},
		"s3": {Title: "t3", Nickname: "from disk"},
	}})
	if got := m.sessionsData["s1"]; got.Nickname != "B" || got.Title != "fresh" {
		t.Errorf("s1 = %+v, want the viewer's nickname B with the harvest's title", got)
	}
	if got := m.sessionsData["s2"].Nickname; got != "" {
		t.Errorf("s2 nickname = %q, want the viewer's (cleared)", got)
	}
	if got := m.sessionsData["s3"].Nickname; got != "from disk" {
		t.Errorf("s3 nickname = %q, want the harvest's for a session the viewer did not know", got)
	}
}

// The prompt is drawn on Sessions only: a pane change that came from a message closes it, so the
// next key acts on the new pane instead of typing into a hidden box.
func TestRenamePromptClosesWhenThePaneChanges(t *testing.T) {
	m := newSessionsModelForTest(t, "s1")
	orig := saveNickname
	t.Cleanup(func() { saveNickname = orig })
	saveNickname = func(string, string) error { t.Error("saved a nickname from a hidden prompt"); return nil }
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m.pane = paneAgents // as agentRowsLoadedMsg does
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.renameID != "" {
		t.Errorf("the prompt is still open on another pane: renameID %q", m.renameID)
	}
}

// `r` works on Sessions even when the Events pane's column picker flag was left set.
func TestRenameWithALeftoverColumnPicker(t *testing.T) {
	m := newSessionsModelForTest(t, "s1")
	m.colPicker = true
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if m.renameID != "s1" {
		t.Errorf("r did not open the prompt: renameID %q", m.renameID)
	}
}

// newSessionsModelForTest is a model on the sessions pane showing the given sessions.
func newSessionsModelForTest(t *testing.T, ids ...string) *model {
	t.Helper()
	m := &model{pane: paneSessions, previousPane: paneNone, pipelineReturnPane: paneNone, width: 120, height: 40}
	m.searchInput = newSearchInput()
	m.sessionsTbl = newSessionsTable()
	for _, id := range ids {
		m.sessions = append(m.sessions, session.SessionSummary{ID: id, Title: "served"})
	}
	m.rebuildSessionsTable()
	if m.selectedSessionID() != ids[0] {
		t.Fatalf("selected %q, want %q", m.selectedSessionID(), ids[0])
	}
	return m
}

// The nickname is stored beside the harvested entry and survives a harvest that rewrites it.
func TestSetNicknameSurvivesAHarvest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-metadata.json")
	if err := claude.SaveMetadata(path, map[string]claude.SessionMetadata{"s1": {Title: "harvested"}}); err != nil {
		t.Fatal(err)
	}
	if err := claude.SetNickname(path, "s1", "  mine  "); err != nil {
		t.Fatal(err)
	}
	meta, err := claude.ReadMetadata(path)
	if err != nil || meta["s1"].Nickname != "mine" || meta["s1"].Title != "harvested" {
		t.Fatalf("after set: %+v, %v", meta["s1"], err)
	}
	if err := claude.SetNickname(path, "s2", ""); err != nil { // clearing an unknown one: no entry
		t.Fatal(err)
	}
	if meta, _ = claude.ReadMetadata(path); len(meta) != 1 {
		t.Errorf("clearing an unknown session made an entry: %+v", meta)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
