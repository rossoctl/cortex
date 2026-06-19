package tui

import (
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/kagenti/kagenti-extensions/authbridge/cmd/abctl/apiclient"
	"github.com/kagenti/kagenti-extensions/authbridge/cmd/abctl/edit"
)

// catalogPlugins extracts the plugin slice from a (possibly nil)
// catalog snapshot so FetchCmd can render templates inline. Nil-safe:
// if the catalog hasn't loaded yet (--endpoint mode without
// /v1/plugins, or first-edit-before-poll), the edit just opens
// without templates rather than blocking.
func catalogPlugins(c *apiclient.PluginCatalog) []apiclient.PluginCatalogEntry {
	if c == nil {
		return nil
	}
	return c.Plugins
}

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
		case "r":
			if m.loading {
				return nil
			}
			m.pickerErr = ""
			m.loading = true
			return loadAgentsCmd(m.ctx, m.lister)
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
		case "r":
			if m.loading {
				return nil
			}
			m.pickerErr = ""
			m.loading = true
			return loadAgentsCmd(m.ctx, m.lister)
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

	// Edit overlay takes over key input while an edit is in flight.
	if m.editState.phase != editPhaseDone {
		return m.handleEditKey(msg)
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
		// Toggle hiding of passthrough / skip-only messages. Default is
		// off (show everything); turning it on focuses the timeline on
		// plugin activity. Only meaningful while the events pane is
		// active, but accepting the key on any pane keeps the keybinding
		// simple and lets operators set their preference before drilling
		// into a session. rebuildEventsTable is a no-op when no session
		// is selected.
		m.hideInactive = !m.hideInactive
		m.rebuildEventsTable()
		return nil

	case "esc", "left", "h":
		// Back-out: plugin-detail → pipeline (or catalog if we came from
		// there); detail → events; events → sessions; catalog → previous.
		// In picker mode, the top-level session tabs (paneSessions and
		// panePipeline are siblings) back out further to the Pods picker,
		// tearing down PF + SSE.
		switch m.pane {
		case panePluginDetail:
			// Return to whichever pane invoked the detail (Pipeline or Catalog).
			if m.previousPane == paneCatalog {
				m.pane = paneCatalog
				m.previousPane = paneNone
			} else {
				m.pane = panePipeline
			}
		case paneCatalog:
			// Return to whichever pane the user pressed P from.
			if m.previousPane != paneNone {
				m.pane = m.previousPane
				m.previousPane = paneNone
			} else {
				m.pane = panePipeline
			}
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
			er, ok := m.selectedEventRow()
			if !ok {
				return nil
			}
			m.showDetail(er)
			m.pane = paneDetail
			return nil
		case panePipeline:
			p := m.selectedPlugin()
			if p == nil {
				return nil
			}
			m.previousPane = panePipeline
			m.showPluginDetail(p)
			m.pane = panePluginDetail
			return nil
		case paneCatalog:
			p := m.selectedCatalogEntry()
			if p == nil {
				return nil
			}
			m.previousPane = paneCatalog
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

	case "e":
		if m.pane != panePipeline {
			return nil
		}
		// `e` requires the picker-mode cluster fields. In --endpoint
		// mode none of these are set, so the keypath would crash later
		// trying to kubectl-fetch with an empty pod/namespace. Surface
		// the limitation in the footer instead of opening a broken edit.
		if m.editRunner == nil || m.statusURL == "" || m.selectedNamespace == "" || m.selectedPod == "" {
			m.setFlash("pipeline editing requires the picker (no --endpoint)")
			return nil
		}
		if m.editState.phase != editPhaseDone {
			return nil // already editing
		}
		gen := m.editState.generation + 1
		m.editState = editState{phase: editPhaseFetching, generation: gen}
		return withGen(gen, edit.FetchCmd(m.ctx, m.editRunner, m.client, m.selectedNamespace, m.selectedPod, catalogPlugins(m.catalog)))

	case "g":
		m.goTop()
		return nil

	case "G":
		m.goBottom()
		return nil

	case "pgup", "pgdown", "pgdn", "b", "f":
		// Page the active pane. Sessions can hold up to session.max_events
		// (500) rows, so one-row-at-a-time nav isn't enough. b/f (less/vim
		// style) work on any keyboard; PgUp/PgDn map to fn+↑/fn+↓ on Mac
		// laptops. Explicit here (rather than the table component's own
		// binding) so all of them share a one-row overlap for context and the
		// detail viewport pages too.
		return m.pageActivePane(msg)

	case "P":
		// Open the registered-plugin catalog. Available from any
		// session-view pane; in --endpoint mode the picker fields
		// don't matter — the catalog comes via the same /v1/* endpoint
		// abctl is already pointed at.
		if m.client == nil {
			return nil
		}
		switch m.pane {
		case paneNamespaces, panePods:
			return nil
		}
		m.previousPane = m.pane
		m.pane = paneCatalog
		// Fetch on first open; cached afterward (refresh via `r`).
		if m.catalog == nil {
			return m.loadCatalogCmd()
		}
		m.rebuildCatalogTable()
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
	case paneCatalog:
		// `r` here refreshes the catalog (in the catalog pane only — the
		// top-level `r` is reserved for the picker). All other keys go to
		// the table for navigation.
		if msg.String() == "r" {
			return m.loadCatalogCmd()
		}
		var cmd tea.Cmd
		m.catalogTbl, cmd = m.catalogTbl.Update(msg)
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
	case paneCatalog:
		m.catalogTbl.SetCursor(0)
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
	case paneCatalog:
		if n := len(m.catalogTbl.Rows()); n > 0 {
			m.catalogTbl.SetCursor(n - 1)
		}
	case paneDetail, panePluginDetail:
		m.detailVp.GotoBottom()
	}
}

// pageActivePane moves the active pane by one page. Tables move the cursor by
// (visible height − 1) rows — one row of overlap keeps context across the
// jump — clamped to the row range by table.MoveUp/MoveDown. The detail/
// plugin-detail viewport delegates to its built-in page scrolling. Picker
// panes (namespaces/pods) never reach here; they return early in handleKey
// and page via their own component's binding.
func (m *model) pageActivePane(msg tea.KeyMsg) tea.Cmd {
	up := msg.String() == "pgup" || msg.String() == "b"
	page := func(t *table.Model) {
		n := t.Height() - 1
		if n < 1 {
			n = 1
		}
		if up {
			t.MoveUp(n)
		} else {
			t.MoveDown(n)
		}
	}
	switch m.pane {
	case paneEvents:
		page(&m.eventsTbl)
	case paneSessions:
		page(&m.sessionsTbl)
	case panePipeline:
		page(&m.pipelineTbl)
	case paneCatalog:
		page(&m.catalogTbl)
	case paneDetail, panePluginDetail:
		var cmd tea.Cmd
		m.detailVp, cmd = m.detailVp.Update(msg)
		return cmd
	}
	return nil
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
		return "[↑↓/jk] nav  [↵] open  [r] reload  [q] quit"
	case panePods:
		return "[↑↓/jk] nav  [↵] connect  [Esc] back  [r] reload  [q] quit"
	case paneSessions:
		if m.parentCtx != nil {
			return "[↑↓] nav  [↵] drill  [tab] pipeline  [/] filter  [esc] pods  [p] pause  [q] quit"
		}
		return "[↑↓] nav  [↵] drill  [tab] pipeline  [/] filter  [p] pause  [q] quit"
	case paneEvents:
		skipHint := "[s] hide skips"
		if m.hideInactive {
			skipHint = "[s] show all"
		}
		base := "[↑↓] nav  [b/f] page  [↵] detail  [esc] back  [/] filter  " + skipHint + "  [p] pause  [q] quit"
		// Surface the hidden-message count so a filtered timeline doesn't
		// look like data loss. Only annotate when hiding is on AND at
		// least one message was hidden.
		if m.hideInactive && m.hiddenInactive > 0 {
			base = fmt.Sprintf("%s  ·  %d hidden",
				base, m.hiddenInactive)
		}
		return base
	case paneDetail:
		return "[↑↓] scroll  [y] yank  [esc] back  [q] quit"
	case panePipeline:
		var base string
		if m.parentCtx != nil {
			base = "[↑↓] nav  [↵] plugin detail  [e] edit  [tab] sessions  [esc] pods  [q] quit"
		} else {
			base = "[↑↓] nav  [↵] plugin detail  [e] edit  [tab] sessions  [q] quit"
		}
		// Surface a count of plugins with unmet dependencies so a single
		// "✗" in the DEPS column doesn't get lost in a long list.
		if n := m.unmetDepsCount(); n > 0 {
			base = fmt.Sprintf("%s  ·  %d plugin%s with unmet deps",
				base, n, plural(n))
		}
		return base
	case panePluginDetail:
		return "[↑↓] scroll  [esc] back  [q] quit"
	case paneCatalog:
		if m.catalog == nil {
			return "loading catalog…  [esc] back  [q] quit"
		}
		return "[↑↓] nav  [↵] plugin detail  [r] refresh  [esc] back  [q] quit"
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
		m.showDetail(m.detailRow)
	}
}

// handleEditKey is the keymap that takes over while an edit is in flight.
func (m *model) handleEditKey(msg tea.KeyMsg) tea.Cmd {
	switch m.editState.phase {
	case editPhaseDiff:
		switch msg.String() {
		case "y", "Y":
			m.editState.phase = editPhaseApplying
			newSubtree := m.editState.editedRaw
			newInner := edit.Splice(
				m.editState.fetched.InnerYAML,
				m.editState.fetched.PipelineStart,
				m.editState.fetched.PipelineEnd,
				newSubtree,
			)
			manifest, err := edit.BuildManifest(m.editState.fetched.ConfigMapYAML, newInner)
			if err != nil {
				m.editState.phase = editPhaseError
				m.editState.err = "build manifest: " + err.Error()
				return nil
			}
			return withGen(m.editState.generation, edit.ApplyCmd(m.ctx, m.editRunner, manifest))
		case "n", "N", "esc":
			m.editState = editState{phase: editPhaseDone}
			return nil
		}
		return nil
	case editPhaseError:
		switch msg.String() {
		case "r":
			// If the fetch never completed (tempPath empty), retry the
			// fetch instead of opening $EDITOR on "" (which leaves the
			// user with nothing to edit and a misleading flow). A retry
			// bumps gen so any straggling messages from the failed
			// attempt are dropped.
			if m.editState.tempPath == "" {
				gen := m.editState.generation + 1
				m.editState = editState{phase: editPhaseFetching, generation: gen}
				return withGen(gen, edit.FetchCmd(m.ctx, m.editRunner, m.client, m.selectedNamespace, m.selectedPod, catalogPlugins(m.catalog)))
			}
			m.editState.phase = editPhaseEditing
			return openEditorCmd(m.editState.generation, m.editState.tempPath)
		case "esc":
			m.editState = editState{phase: editPhaseDone}
			return nil
		}
		return nil
	}
	// Other phases: Esc backgrounds (Waiting / Rollback) so the in-flight
	// Cmd's eventual result lands as a footer flash, or cancels outright
	// (Fetching / Editing / Applying — phases where the result is still
	// safe to drop).
	if msg.String() == "esc" {
		switch m.editState.phase {
		case editPhaseWaiting, editPhaseRollback:
			m.editState.phase = editPhaseBackground
			m.setFlash("hot-reload watch moved to background; you'll be notified")
		default:
			m.editState = editState{phase: editPhaseDone}
		}
		return nil
	}
	return nil
}
