package tui

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// handleKey processes every key press. The filter-input overlay takes
// precedence; otherwise keys are dispatched based on the active pane.
func (m *model) handleKey(msg tea.KeyMsg) tea.Cmd {
	// Picker panes handle their own keys before session-view logic.
	if m.pane == paneNamespaces {
		switch msg.String() {
		case "enter":
			if cur := m.namespacesTbl.Cursor(); cur < len(m.namespaces) {
				m.selectedNamespace = m.namespaces[cur].Name
				m.pane = panePods
				m.rebuildPodsTable()
			}
			return nil
		case "q", "esc", "ctrl+c":
			m.cancel()
			return tea.Quit
		}
		var cmd tea.Cmd
		m.namespacesTbl, cmd = m.namespacesTbl.Update(msg)
		return cmd
	}

	if m.pane == panePods {
		switch msg.String() {
		case "enter":
			pods := m.currentPodsList()
			if cur := m.podsTbl.Cursor(); cur < len(pods) {
				if !pods[cur].Ready {
					m.pickerErr = "pod not Ready"
					return nil
				}
				m.selectedPod = pods[cur].Name
				// Tear down the previous PF, if any, before starting a new one.
				if m.activePF != nil {
					_ = m.activePF.Close()
					m.activePF = nil
				}
				m.pickerErr = ""
				return startPortForwardCmd(m.ctx, m.portForwarder, m.selectedNamespace, m.selectedPod)
			}
			return nil
		case "esc":
			m.pane = paneNamespaces
			m.pickerErr = ""
			return nil
		case "q", "ctrl+c":
			m.cancel()
			return tea.Quit
		}
		var cmd tea.Cmd
		m.podsTbl, cmd = m.podsTbl.Update(msg)
		return cmd
	}

	// Filter-mode: input box consumes most keys. Esc cancels, Enter commits.
	if m.filtering {
		switch msg.String() {
		case "esc":
			m.filtering = false
			m.filter = ""
			m.filterInput.SetValue("")
			m.refreshActivePane()
			return nil
		case "enter":
			m.filter = m.filterInput.Value()
			m.filtering = false
			m.refreshActivePane()
			return nil
		}
		var cmd tea.Cmd
		m.filterInput, cmd = m.filterInput.Update(msg)
		m.filter = m.filterInput.Value()
		m.refreshActivePane()
		return cmd
	}

	switch msg.String() {
	case "ctrl+c", "q":
		m.cancel()
		return tea.Quit

	case "tab":
		// Toggle between top-level peers only. Sub-panes (events, detail,
		// plugin-detail) are addressed by their parent — Esc out first.
		switch m.pane {
		case paneSessions:
			m.pane = panePipeline
			m.rebuildPipelineTable()
		case panePipeline:
			m.pane = paneSessions
		}
		return nil

	case "/":
		m.filtering = true
		m.filterInput.Focus()
		return nil

	case "p":
		m.paused = !m.paused
		return nil

	case "s":
		// Toggle skip-row visibility. Only meaningful while the events
		// pane is active, but accepting the key on any pane keeps the
		// keybinding simple and lets operators "set their preference"
		// before drilling into a session. rebuildEventsTable is a no-op
		// when no session is selected.
		m.showSkips = !m.showSkips
		m.rebuildEventsTable()
		return nil

	case "esc", "left", "h":
		// Back-out: plugin-detail → pipeline; detail → events; events → sessions.
		// In picker mode, the top-level session tabs (paneSessions and
		// panePipeline are siblings) back out further to the Pods picker,
		// tearing down PF + SSE.
		switch m.pane {
		case panePluginDetail:
			m.pane = panePipeline
		case paneDetail:
			m.pane = paneEvents
		case paneEvents:
			m.pane = paneSessions
		case paneSessions, panePipeline:
			// Picker mode: back to Pods pane, tearing down the current
			// port-forward + SSE stream. Bypass mode: no-op (parentCtx
			// is nil; nowhere to go back to).
			if m.parentCtx != nil {
				m.backToPodsPane()
			}
		}
		return nil

	case "enter", "right", "l":
		switch m.pane {
		case paneSessions:
			id := m.selectedSessionID()
			if id == "" {
				return nil
			}
			m.selectedSess = id
			m.pane = paneEvents
			m.rebuildEventsTable()
			// Snapshot in case the stream hasn't yet delivered history.
			return m.snapshotCmd(id)
		case paneEvents:
			ev := m.selectedEvent()
			if ev == nil {
				return nil
			}
			m.showDetail(ev)
			m.pane = paneDetail
			return nil
		case panePipeline:
			p := m.selectedPlugin()
			if p == nil {
				return nil
			}
			m.showPluginDetail(p)
			m.pane = panePluginDetail
			return nil
		}
		return nil

	case "y":
		if m.pane != paneDetail || m.detailEvent == nil {
			return nil
		}
		path, err := yankEventToFile(m.detailEvent)
		if err != nil {
			m.setFlash("yank failed: " + err.Error())
		} else {
			m.setFlash("yanked → " + path)
		}
		return nil

	case "g":
		m.goTop()
		return nil

	case "G":
		m.goBottom()
		return nil

		// Dispatch j/k/up/down to the active component's Update.
	}

	// Fall through: let the active pane's component handle it.
	switch m.pane {
	case paneSessions:
		var cmd tea.Cmd
		m.sessionsTbl, cmd = m.sessionsTbl.Update(msg)
		return cmd
	case paneEvents:
		var cmd tea.Cmd
		m.eventsTbl, cmd = m.eventsTbl.Update(msg)
		return cmd
	case paneDetail, panePluginDetail:
		var cmd tea.Cmd
		m.detailVp, cmd = m.detailVp.Update(msg)
		return cmd
	case panePipeline:
		prev := m.pipelineTbl.Cursor()
		var cmd tea.Cmd
		m.pipelineTbl, cmd = m.pipelineTbl.Update(msg)
		// Skip over the divider row when navigating.
		if isDividerRow(m.pipelineTbl.Rows(), m.pipelineTbl.Cursor()) {
			if m.pipelineTbl.Cursor() > prev {
				m.pipelineTbl.SetCursor(m.pipelineTbl.Cursor() + 1)
			} else {
				m.pipelineTbl.SetCursor(m.pipelineTbl.Cursor() - 1)
			}
		}
		return cmd
	}
	return nil
}

// refreshActivePane rebuilds the current pane's component after a filter change.
func (m *model) refreshActivePane() {
	switch m.pane {
	case paneSessions:
		m.rebuildSessionsTable()
	case paneEvents:
		m.rebuildEventsTable()
	case panePipeline:
		m.rebuildPipelineTable()
	}
}

func (m *model) goTop() {
	switch m.pane {
	case paneSessions:
		m.sessionsTbl.SetCursor(0)
	case paneEvents:
		m.eventsTbl.SetCursor(0)
	case panePipeline:
		m.pipelineTbl.SetCursor(0)
	case paneDetail, panePluginDetail:
		m.detailVp.GotoTop()
	}
}

func (m *model) goBottom() {
	switch m.pane {
	case paneSessions:
		if n := len(m.sessionsTbl.Rows()); n > 0 {
			m.sessionsTbl.SetCursor(n - 1)
		}
	case paneEvents:
		if n := len(m.eventsTbl.Rows()); n > 0 {
			m.eventsTbl.SetCursor(n - 1)
		}
	case panePipeline:
		if n := len(m.pipelineTbl.Rows()); n > 0 {
			m.pipelineTbl.SetCursor(n - 1)
		}
	case paneDetail, panePluginDetail:
		m.detailVp.GotoBottom()
	}
}

// setFlash shows a transient message in the footer for flashDuration.
func (m *model) setFlash(s string) {
	m.flash = s
	m.flashUntil = time.Now().Add(flashDuration)
}

// helpView renders the keybinding hint line for the current pane. Short
// enough to fit on a single row at 80 cols.
func (m *model) helpView() string {
	if m.filtering {
		return "type to filter · [enter] commit · [esc] cancel"
	}
	switch m.pane {
	case paneNamespaces:
		return "[↑↓/jk] nav  [↵] open  [q] quit"
	case panePods:
		return "[↑↓/jk] nav  [↵] connect  [Esc] back  [q] quit"
	case paneSessions:
		if m.parentCtx != nil {
			return "[↑↓] nav  [↵] drill  [tab] pipeline  [/] filter  [esc] pods  [p] pause  [q] quit"
		}
		return "[↑↓] nav  [↵] drill  [tab] pipeline  [/] filter  [p] pause  [q] quit"
	case paneEvents:
		base := "[↑↓] nav  [↵] detail  [esc] back  [/] filter  [s] skips  [p] pause  [q] quit"
		// Surface the hidden-skip count so a sparse timeline doesn't
		// look like data loss. Only annotate when there's something
		// to say (skips off AND at least one row was hidden).
		if !m.showSkips && m.hiddenSkips > 0 {
			base = fmt.Sprintf("%s  ·  %d skip%s hidden",
				base, m.hiddenSkips, plural(m.hiddenSkips))
		}
		return base
	case paneDetail:
		return "[↑↓] scroll  [y] yank  [esc] back  [q] quit"
	case panePipeline:
		if m.parentCtx != nil {
			return "[↑↓] nav  [↵] plugin detail  [tab] sessions  [esc] pods  [q] quit"
		}
		return "[↑↓] nav  [↵] plugin detail  [tab] sessions  [q] quit"
	case panePluginDetail:
		return "[↑↓] scroll  [esc] back  [q] quit"
	}
	return "[q] quit"
}

// layout recomputes component sizes to fit the current terminal. Called on
// every WindowSizeMsg. The footer reserves two lines; the title one.
func (m *model) layout() {
	if m.width == 0 || m.height == 0 {
		return
	}
	// Reserve 3 rows for title + blank + footer lines.
	bodyH := m.height - 3
	if bodyH < 4 {
		bodyH = 4
	}

	m.sessionsTbl.SetHeight(bodyH)
	m.bodyHeight = bodyH
	// Picker tables share the same body area as the session tables so the
	// terminal real estate stays constant as the user navigates panes.
	m.namespacesTbl.SetHeight(bodyH)
	m.podsTbl.SetHeight(bodyH)
	// The events table's height depends on whether the IDENTITY banner
	// is rendered for the selected session. rebuildEventsTable() applies
	// the banner-aware adjustment; call it so the size is correct after
	// a window resize too.
	m.rebuildEventsTable()
	m.pipelineTbl.SetHeight(bodyH)
	m.detailVp.Width = m.width
	m.detailVp.Height = bodyH

	m.filterInput.Width = m.width - 4

	// Re-wrap the detail viewport to the new width so long JSON values
	// continue to fit after a terminal resize.
	if m.detailEvent != nil {
		m.showDetail(m.detailEvent)
	}
}
