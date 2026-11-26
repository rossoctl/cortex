package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/core/observe/claude"
)

// SESSION NICKNAMES. `r` on the sessions pane opens a prompt over the table, filled with the
// session's nickname; Enter saves what it holds, an empty name clears it, Esc keeps the old one.
// The nickname is stored in ~/.cortex/session-metadata.json beside the harvested title
// (claude.SetNickname), wins over it and over the served title (sessionTitleFor), and survives
// a re-harvest. It is local: nothing sends it anywhere.

// saveNickname writes a nickname; a test seam, so a test never writes the real file.
var saveNickname = func(id, name string) error {
	path, err := SessionMetadataPath()
	if err != nil {
		return err
	}
	return claude.SetNickname(path, id, name)
}

// openRename opens the rename prompt for session id.
func (m *model) openRename(id string) {
	ti := textinput.New()
	ti.Prompt = "rename: "
	ti.Placeholder = "a name for this session (empty clears it)"
	ti.CharLimit = claude.MaxTitleLen
	ti.SetValue(m.sessionsData[id].Nickname)
	ti.CursorEnd()
	ti.Focus()
	m.renameInput, m.renameID = ti, id
}

// renameKey handles a key while the rename prompt is open.
func (m *model) renameKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		m.renameID = ""
		return nil
	case tea.KeyEnter:
		id, name := m.renameID, strings.TrimSpace(m.renameInput.Value())
		m.renameID = ""
		if err := saveNickname(id, name); err != nil {
			m.setFlash("rename failed: " + err.Error())
			return nil
		}
		if m.sessionsData == nil {
			m.sessionsData = map[string]SessionMetadata{}
		}
		meta := m.sessionsData[id]
		meta.Nickname = name
		m.sessionsData[id] = meta
		m.rebuildSessionsTable()
		if name == "" {
			m.setFlash("nickname cleared")
		} else {
			m.setFlash("renamed to " + name)
		}
		return nil
	}
	var cmd tea.Cmd
	m.renameInput, cmd = m.renameInput.Update(msg)
	return cmd
}
