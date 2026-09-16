package tui

import (
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/apiclient"
	"github.com/rossoctl/cortex/authbridge/cmd/abctl/edit"
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

// handleKey processes every key press. Modal overlays claim it first, in
// order: the key-help overlay, then the picker panes, then an in-flight
// pipeline edit, then the filter input. Only if none of those own the
// keyboard is the key dispatched based on the active pane.
func (m *model) handleKey(msg tea.KeyMsg) tea.Cmd {
	// A sticky flash (yank) stays up until the operator does something. Any key
	// dismisses it, including one that goes on to do unrelated work: reading the
	// path was the only thing pending, and pressing a key says they are done.
	// Only the flag is cleared, never m.flash, so timed messages are unaffected.
	m.flashSticky = false

	// The help overlay is modal: while it's up, it owns the keyboard so a
	// stray key can't navigate the pane hidden underneath. Checked before
	// every other handler, including the picker and edit overlays, so `?`
	// is genuinely available everywhere.
	if m.helpVisible {
		switch msg.String() {
		case "?", "esc", "q", "ctrl+c":
			// `q`/ctrl+c close the overlay rather than quitting abctl:
			// dismissing a help panel is the overwhelmingly likely intent,
			// and the overlay itself advertises how to quit.
			m.helpVisible = false
			return nil
		case "g":
			m.helpVp.GotoTop()
			return nil
		case "G":
			m.helpVp.GotoBottom()
			return nil
		}
		// Everything else goes to the viewport so the reference scrolls:
		// ↑↓/jk, pgup/pgdn, and the half-page keys the viewport binds by
		// default. Keys it doesn't recognize are harmlessly ignored, which
		// preserves the overlay's modality.
		var cmd tea.Cmd
		m.helpVp, cmd = m.helpVp.Update(msg)
		return cmd
	}
	// `?` opens the overlay from any pane, with two exceptions. While a
	// pipeline edit is in flight that overlay is already modal and owns
	// y/N/r/Esc, so help would swallow the apply confirmation. While the
	// filter input is focused `?` is a character the user is typing — a
	// session ID or host can contain one — and stealing it would make
	// those values unfilterable.
	if msg.String() == "?" && m.editState.phase == editPhaseDone && !m.filtering {
		m.helpVisible = true
		m.syncHelpViewport(true)
		return nil
	}

	// The Usage pane owns the keyboard while it is up, except for esc/q which
	// the shared handling above already routed.
	if m.pane == paneUsage && !m.filtering {
		switch msg.String() {
		case "m":
			// Metric. `t` (for "tokens") named one of the four values rather than
			// the axis, and every other binding here is the first letter of what it
			// changes.
			m.usage.cycleMetric()
			return nil
		case "w":
			m.usage.cycleWindow()
			return m.beginFetch()
		case "b":
			// Breakdown. NOT `g` for "group": `g` is globally "go to top" (see
			// goTop below), and shadowing a vim-style motion inside one pane is
			// worse than picking a second-choice mnemonic.
			//
			// Ignored while viewing latency: the aggregator holds no per-label
			// latency, so there is no per-status or per-model mean to plot. The
			// footer and the [?] overlay both say so, which is what keeps this from
			// reading as a broken binding — `b` reaches no other handler for this
			// pane (pageActivePane has no paneUsage case), so breaking here simply
			// drops it.
			if m.usage.metric.isLatency() {
				break
			}
			// Refetch: the breakdown is a server-side query parameter, not a
			// client-side filter, so the current snapshot has no series for the
			// newly selected dimension.
			m.usage.cycleGroup()
			return m.beginFetch()
		case "s":
			// Toggle scope between this session and all sessions. Only offered
			// when a session is selected; otherwise there is nothing to toggle to.
			if m.usage.session != "" {
				m.usage.session = ""
			} else if m.selectedSess != "" {
				m.usage.session = m.selectedSess
			}
			return m.beginFetch()
		}
	}

	// `u` opens the Usage pane. Scope depends on where it was pressed: from the
	// events timeline it charts the session being read, from the session picker
	// it charts everything. Suppressed while filtering, where `u` is a character
	// the user is typing — the same reasoning as the `?` overlay above.
	//
	// Gated on !m.colPicker as well: this handler sits above the picker block, so
	// without it `u` reached openUsage while m.colPicker stayed true — View() drew
	// the picker popup over the Usage pane, paneUsage's own m/w/b/s bindings went
	// live underneath it, and `esc` then closed the picker onto Usage instead of the
	// events timeline the picker was opened from. (`?` above is deliberately NOT
	// gated: it layers help over the picker without changing panes, and closing it
	// restores the picker.)
	if msg.String() == "u" && !m.filtering && !m.colPicker && m.editState.phase == editPhaseDone {
		switch m.pane {
		case paneEvents, paneDetail:
			if m.selectedSess != "" {
				return m.openUsage(m.selectedSess)
			}
		case paneSessions:
			return m.openUsage("")
		}
	}

	// The Cost pane owns the keyboard while it is up, except for esc/q which the
	// shared handling below routes.
	//
	// `g` for group, unlike the Usage pane's `b` for breakdown. The Usage pane chose a
	// second-best mnemonic to avoid shadowing the global `g` (go to top), and the
	// reasoning does not carry here: this pane has no cursor and no table, so goTop and
	// pageActivePane have no arm for it and both `g` and `G` are already inert. Taking
	// the right mnemonic for a key that does nothing is not a trade.
	//
	// Neither key persists directly. Settings is updated in place and written on the
	// way OUT of the pane, mirroring the column picker: a user cycling round to the
	// window they want would otherwise produce a write for every one they passed
	// through, each describing a state they rejected.
	if m.pane == paneCost && !m.filtering {
		switch msg.String() {
		case "w":
			m.costPane.cycleWindow()
			Settings.Cost.Window = m.costPane.window()
			// Refetch: the window is a server-side query parameter, not a client-side view of
			// the snapshot in hand, so the current answer has no data for the new span.
			return m.beginCostFetch()
		case "g":
			m.costPane.cycleGroup()
			Settings.Cost.Group = string(m.costPane.group)
			// Refetch for the same reason: the breakdown axis is chosen server-side, so the
			// snapshot in hand carries no series for the newly selected one.
			return m.beginCostFetch()
		}
	}

	// `$` opens the Cost pane, `C` is the alias.
	//
	// `$` because `c` is already the events-pane column picker and is unmistakable for
	// money; `? g G m w b s r l / p y e P q tab n f u` are all bound. `C` is offered
	// because a shifted letter is easier to find than a symbol on some layouts.
	//
	// Scoped on !m.filtering for the reason the column picker's `c` is: the events
	// pane's `/` filter takes arbitrary text — a model name or a shell-ish session id
	// can contain a `$` — and an unscoped handler would swallow the character and yank
	// the user to another pane mid-word.
	//
	// Gated on !m.colPicker for the reason `u` above is: the picker is modal, and
	// changing panes underneath it leaves its `esc` closing onto the wrong pane.
	if (msg.String() == "$" || msg.String() == "C") && !m.filtering && !m.colPicker &&
		m.editState.phase == editPhaseDone {
		switch m.pane {
		// Every session-view pane, and no scope argument: cost is asked all-sessions and
		// broken down by the selected axis. There is nothing to scope to — the pane's
		// session breakdown is one of its groupings.
		case paneSessions, paneEvents, paneDetail, panePipeline, panePluginDetail, paneUsage, paneCatalog:
			return m.openCostPane()
		}
	}

	// The column picker owns the keyboard while it is up, so ↑↓/space cannot also
	// move the table cursor underneath it. Checked before pane dispatch for the
	// same reason the help overlay is.
	//
	// Scoped to paneEvents, mirroring the condition that opens it: no KEY can change
	// panes underneath the picker, but a MESSAGE still can — the sessionsMsg handler
	// drops to paneSessions when the selected session disappears server-side, which
	// left this block swallowing enter/j/k over an inert sessions table. Gating on
	// the pane covers that and any future transition, where clearing the flag in one
	// handler would only fix today's path.
	if m.colPicker && m.pane == paneEvents && !m.filtering {
		switch msg.String() {
		case "q", "ctrl+c":
			// Quit stays live. A modal that traps the user until they find its exit
			// is worse than one that closes on the key they already reach for, and
			// `q` means quit everywhere else in abctl. Falls through to the global
			// handler rather than being reimplemented here.
		case "c", "esc", "enter":
			m.colPicker = false
			// Persist on close, not on each toggle: a user trying four columns on the
			// way to the two they want would otherwise produce three writes describing
			// states they rejected. `q` is not handled here — it falls through to the
			// global quit above, because quitting is not settling on a selection.
			Settings.Events.Columns = columnSettingsFrom(m.eventColumns)
			m.persistSettings()
			return nil
		case "up", "k":
			if m.colCursor > 0 {
				m.colCursor--
			}
			return nil
		case "down", "j":
			if m.colCursor < len(eventColumns)-1 {
				m.colCursor++
			}
			return nil
		case " ", "x":
			// Toggle. selectedColumns falls back to the defaults when the set is
			// empty, so turning everything off cannot leave an unrecoverable blank
			// pane.
			id := eventColumns[m.colCursor].id
			m.eventColumns[id] = !m.eventColumns[id]
			// Make that fallback visible in the checkboxes rather than only in the
			// table. Without this, emptying the selection drew twelve `[ ]` boxes over
			// a table showing twelve default columns — the one state the fallback
			// exists to rescue was also the state where the picker misreported what is
			// on screen, and the `(no room)` markers vanished too since they are gated
			// on the selection.
			if !anyColumnSelected(m.eventColumns) {
				m.eventColumns = defaultColumnSelection()
			}
			m.rebuildEventsTable()
			return nil
		case "r":
			m.eventColumns = defaultColumnSelection()
			m.rebuildEventsTable()
			return nil
		default:
			// Everything else is swallowed: the popup is modal, so a stray key must
			// not move the table cursor underneath it.
			return nil
		}
	}

	// `c` opens the column picker from the events timeline. Suppressed while
	// filtering, where `c` is a character being typed — the same reasoning as `?`
	// and `u`.
	if msg.String() == "c" && m.pane == paneEvents && !m.filtering &&
		m.editState.phase == editPhaseDone {
		m.colPicker = true
		return nil
	}

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
		case "l":
			// Skip the cluster entirely and talk to whatever session API
			// is already listening locally — an existing port-forward, an
			// in-mesh abctl, or a tunnel from a kubeconfig that can't list
			// pods. Probes before switching panes so a dead endpoint stays
			// an error in the picker rather than an empty session view.
			if m.loading {
				return nil
			}
			m.pickerErr = ""
			m.loading = true
			return connectLocalCmd(m.ctx, m.localEndpointOr())
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

	// Filter-mode: input box consumes most keys. Esc cancels (restores the filter as
	// it was at `/`), Enter commits and is the only key that persists.
	if m.filtering {
		switch msg.String() {
		case "esc":
			// Cancel, so it restores what was in effect when `/` was pressed and writes
			// nothing. It used to clear the filter instead — which, once filters began
			// persisting, meant one mis-keyed Esc permanently discarded a committed
			// filter, while the README and this file both called the key "cancel".
			//
			// Clearing has not been lost: empty the box and press Enter. That keeps Enter
			// as the only key that writes, which is the property worth having.
			m.filtering = false
			m.filter = m.filterBeforeEdit
			m.filterInput.SetValue(m.filterBeforeEdit)
			m.layout() // gives the body back the filter's line — see layout()
			m.refreshActivePane()
			return nil
		case "enter":
			m.filter = m.filterInput.Value()
			m.filtering = false
			m.layout() // gives the body back the filter's line — see layout()
			// Commit, not keystroke: the fallthrough below re-reads the input on every
			// character typed, and saving there would write once per keypress.
			//
			// The only key that persists a filter. An empty box committed here is how a
			// filter is cleared and the clearing made durable, now that Esc cancels.
			Settings.Filter = m.filter
			m.persistSettings()
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
		// The filter input takes a body line, so the height budget changes with this
		// flag — see layout(). Recomputed here rather than waiting for a WindowSizeMsg
		// that may never come.
		m.layout()
		// Snapshot for Esc. Taken here rather than derived on the way out, because by
		// then the input has already been edited and the original is gone.
		m.filterBeforeEdit = m.filter
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
			// Returning INTO a polling pane has to restart ITS chain. The tick
			// that was in flight when the catalog opened was dropped by that
			// pane's own `m.pane != …` guard, so without this nothing
			// reschedules and the 20s auto-refresh is silently dead until the
			// user backs all the way out and re-enters — `r` refetches once but
			// starts no chain.
			//
			// One arm per pane with a chain, and paneCost is the second: with
			// only the Usage arm here, `$` `P` `esc` came back to a Cost pane
			// whose chain had been dropped and never rescheduled, its freshness
			// line counting up ("updated 14m3s ago (every 20s)") against a
			// figure nothing would ever refresh.
			switch m.pane {
			case paneUsage:
				return m.resumeUsagePolling()
			case paneCost:
				return m.resumeCostPolling()
			}
		case paneUsage:
			// Return to whichever pane opened it, from usageState's own field —
			// model.previousPane is shared with the catalog overlay and gets
			// clobbered when the catalog is opened from here.
			if m.usage.returnPane != paneNone {
				m.pane = m.usage.returnPane
				m.usage.returnPane = paneNone
			} else {
				m.pane = paneSessions
			}
			// End the polling chain on the way out.
			m.usage.tickGen++
		case paneCost:
			// Return to whichever pane opened it, from costPaneState's own field, for the
			// reason usageState.returnPane records: model.previousPane is shared with the
			// catalog overlay and gets clobbered when the catalog is opened from here.
			if m.costPane.returnPane != paneNone {
				m.pane = m.costPane.returnPane
				m.costPane.returnPane = paneNone
			} else {
				m.pane = paneSessions
			}
			// End the polling chain on the way out, exactly as paneUsage does: a chain left
			// running against a backgrounded pane keeps issuing a request every 20s for the
			// life of the session, with nothing on screen to show for it.
			m.costPane.tickGen++
			// Persist here rather than on each `w` / `g` press. Leaving the pane is the
			// settled choice, the same way closing the column picker is — and it is one write
			// per visit instead of one per keystroke.
			m.persistSettings()
			// And restart the chain of the pane being returned TO, for the reason the
			// catalog arm above does it. `u` `$` `esc` is the live path: opening Cost from
			// Usage let the Usage tick in flight fall to its own `m.pane != paneUsage`
			// guard, so esc landed back on a Usage pane whose 20s refresh was dead — a
			// REGRESSION to an existing pane, introduced by adding paneCost to the openers,
			// and bit for bit the failure that arm exists to prevent.
			if m.pane == paneUsage {
				return m.resumeUsagePolling()
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
			live := make(map[string]bool, len(m.sessions))
			for _, s := range m.sessions {
				live[s.ID] = true
			}

			// The one and only place cached events are released.
			//
			// Picking a different session in the picker is the sole reliable
			// signal that the previous session's events have stopped being what
			// the user is looking at. A proxy restart, or any gap in
			// /v1/sessions, is not that signal — treating it as one is #870.
			//
			// Released only for sessions the server still LISTS: those are
			// recoverable, because snapshotCmd can fetch them again. A
			// cached-only session is kept — abctl's copy is the only copy, so
			// dropping it would be the same unrecoverable loss this PR exists to
			// stop. After a restart every previously-visited session is
			// cached-only, so opening one must not destroy the rest.
			//
			// The honest consequence: cached-only sessions are never released
			// while abctl runs, so the cache grows by one entry per restart the
			// user visited a session across. Measured, that is ~165 bytes per
			// event and 1000 events per session, so ~161 KB per session and a
			// few MB for a long debugging afternoon — worth it, given the
			// alternative is deleting the only copy of what someone is reading.
			if id != m.selectedSess {
				for cached := range m.events {
					if cached != id && live[cached] {
						delete(m.events, cached)
					}
				}
				// Clear only on an actual session change, so
				// re-entering the same session keeps the pin.
				m.selectedEventKey = eventKey{}
			}
			m.selectedSess = id
			m.pane = paneEvents
			m.rebuildEventsTable()
			if !live[id] {
				// Cached-only: there is no server-side session to snapshot, so the
				// fetch would 404 and flash an error over the very events this
				// change preserved.
				return nil
			}
			// Snapshot in case the stream hasn't yet delivered history.
			return m.snapshotCmd(id)
		case paneEvents:
			er, ok := m.selectedEventRow()
			if !ok {
				return nil
			}
			m.showDetail(er, true)
			m.pane = paneDetail
			return nil
		case panePipeline:
			p := m.selectedPlugin()
			if p == nil {
				return nil
			}
			m.previousPane = panePipeline
			m.showPluginDetail(p, true)
			m.pane = panePluginDetail
			// Fetch immediately rather than waiting for the next refresh tick:
			// opening the pane is exactly when someone wants current counters.
			return m.loadPipelineCmd()
		case paneCatalog:
			p := m.selectedCatalogEntry()
			if p == nil {
				return nil
			}
			m.previousPane = paneCatalog
			m.showPluginDetail(p, true)
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
			// Sticky, like the success case: checkYankDir's message names the
			// directory and the chmod that fixes it, which is useless if it
			// disappears after three seconds while the success path persists.
			m.setStickyFlash("yank failed: " + err.Error())
		} else {
			m.setStickyFlash("yanked → " + path)
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
		m.selectedEventKey = keyOf(m.selectedEvent())
		return cmd
	case paneDetail, panePluginDetail:
		var cmd tea.Cmd
		m.detailVp, cmd = m.detailVp.Update(msg)
		return cmd
	case panePipeline:
		prev := m.pipelineTbl.Cursor()
		var cmd tea.Cmd
		m.pipelineTbl, cmd = m.pipelineTbl.Update(msg)
		// Skip over the divider row when navigating. One more step in the direction
		// of travel, as a relative move so the offset stays reconciled — see
		// setCursorVisible for why SetCursor is not used for cursor placement.
		if isDividerRow(m.pipelineTbl.Rows(), m.pipelineTbl.Cursor()) {
			if m.pipelineTbl.Cursor() > prev {
				m.pipelineTbl.MoveDown(1)
			} else {
				m.pipelineTbl.MoveUp(1)
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

// goTop and goBottom place the cursor through setCursorVisible, not SetCursor: a
// jump to the last row is exactly the case where SetCursor leaves the highlight one
// line below the rendered window, so `G` on any list longer than the screen used to
// scroll to the bottom with nothing highlighted. The empty-table guards live in
// setCursorVisible now, and it clamps, so goBottom does not need the row count.
func (m *model) goTop() {
	switch m.pane {
	case paneCatalog:
		setCursorVisible(&m.catalogTbl, 0)
	case paneSessions:
		setCursorVisible(&m.sessionsTbl, 0)
	case paneEvents:
		setCursorVisible(&m.eventsTbl, 0)
		m.selectedEventKey = keyOf(m.selectedEvent())
	case panePipeline:
		setCursorVisible(&m.pipelineTbl, 0)
	case paneDetail, panePluginDetail:
		m.detailVp.GotoTop()
	}
}

func (m *model) goBottom() {
	switch m.pane {
	case paneSessions:
		setCursorVisible(&m.sessionsTbl, len(m.sessionsTbl.Rows())-1)
	case paneEvents:
		setCursorVisible(&m.eventsTbl, len(m.eventsTbl.Rows())-1)
		m.selectedEventKey = keyOf(m.selectedEvent())
	case panePipeline:
		setCursorVisible(&m.pipelineTbl, len(m.pipelineTbl.Rows())-1)
	case paneCatalog:
		setCursorVisible(&m.catalogTbl, len(m.catalogTbl.Rows())-1)
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
		m.selectedEventKey = keyOf(m.selectedEvent())
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
	// Explicitly clear: a timed message arriving after a sticky one must not
	// inherit its stickiness.
	m.flashSticky = false
}

// persistSettings hands the current Settings to the save hook, if one is wired.
//
// Failure is reported once through the footer flash and then dropped. Three
// constraints shape that: bubbletea owns the terminal via WithAltScreen, so
// writing to stderr here would corrupt the frame; the user cannot fix a read-only
// $HOME from inside the TUI, so an error that blocks or repeats is noise; and a
// preference that failed to save costs them one re-toggle next launch. Silence was
// the alternative, and it would leave a read-only home failing invisibly forever —
// the flash mechanism already exists for exactly this class of non-fatal problem.
//
// Never retries: a full disk would turn a retry loop into a redraw storm.
func (m *model) persistSettings() {
	if m.save == nil {
		return
	}
	if err := m.save(Settings); err != nil {
		m.setFlash("could not save settings: " + err.Error())
	}
}

// setStickyFlash shows a message that stays until the next keypress. For yank,
// where the whole point is giving the operator time to read or copy a path —
// flashDuration is shared with ten other producers, so lengthening it is not an
// option, and three seconds is not enough to transcribe a filename.
func (m *model) setStickyFlash(s string) {
	m.flash = s
	// Clear the deadline a previous timed flash may have left in the future.
	// Once a keypress clears flashSticky, footerView falls back to the
	// time.Now().Before(flashUntil) arm — so a stale deadline would keep the
	// yanked path on screen for the remainder of the old timer instead of
	// disappearing on the keypress.
	m.flashUntil = time.Time{}
	m.flashSticky = true
}

// helpView renders the keybinding hint line for the current pane. Short
// enough to fit on a single row at 80 cols.
func (m *model) helpView() string {
	if m.filtering {
		return "type to filter · [enter] commit · [esc] cancel"
	}
	switch m.pane {
	case paneNamespaces:
		return "[↑↓/jk] nav  [↵] open  [l] " + shortHost(m.localEndpointOr()) +
			"  [r] reload  [?] keys  [q] quit"
	case panePods:
		return "[↑↓/jk] nav  [↵] connect  [Esc] back  [r] reload  [?] keys  [q] quit"
	case paneSessions:
		if m.parentCtx != nil {
			return "[↑↓] nav  [↵] drill  [tab] pipeline  [u] usage  [$] cost  [/] filter  [esc] pods  [p] pause  [?] keys  [q] quit"
		}
		return "[↑↓] nav  [↵] drill  [tab] pipeline  [u] usage  [$] cost  [/] filter  [p] pause  [?] keys  [q] quit"
	case paneEvents:
		skipHint := "[s] hide passthru/skip"
		if m.hideInactive {
			skipHint = "[s] show all"
		}
		// Ordered by what must survive truncation, not by how the keys group. The
		// footer runs 135 columns and an 80-column terminal cuts the tail, so
		// anything after the cut is invisible — [?] keys and [q] quit were both
		// past it, which is the pair a stuck user reaches for. They now sit at the
		// end, and the escapable/discoverable keys ahead of them, with the
		// specialised ones first to be lost.
		base := "[↑↓] nav  [b/f] page  [↵] detail  [c] columns  [u] usage  [$] cost  " +
			skipHint + "  [p] pause  [/] filter  [esc] back"

		// Notices go BEFORE the essential hints, not after.
		//
		// fitHintLine drops from the front, so anything appended past "[q] quit"
		// outlives it — at width 40 the footer read "… · → 1 more column ([c] to
		// choose)" with no way to quit or reach help. A notice is worth less than
		// the keys that let a user act on it.
		//
		// Surface the hidden-message count so a filtered timeline doesn't look like
		// data loss. Only when hiding is on AND at least one message was hidden.
		if m.hideInactive && m.hiddenInactive > 0 {
			base = fmt.Sprintf("%s  ·  %d hidden", base, m.hiddenInactive)
		}
		// Columns that did not fit — the whole reason issue #866 was filed: HOST was
		// declared but never visible, and nothing said the table had been clipped.
		if m.eventColsDropped > 0 {
			base = fmt.Sprintf("%s  ·  → %d more column%s ([c] to choose)",
				base, m.eventColsDropped, plural(m.eventColsDropped))
		}
		return base + "  [?] keys  [q] quit"
	case paneDetail:
		return "[↑↓] scroll  [y] yank  [u] usage  [$] cost  [esc] back  [?] keys  [q] quit"
	case panePipeline:
		var base string
		if m.parentCtx != nil {
			base = "[↑↓] nav  [↵] plugin detail  [e] edit  [tab] sessions  [esc] pods"
		} else {
			base = "[↑↓] nav  [↵] plugin detail  [e] edit  [tab] sessions"
		}
		// Surface a count of plugins with unmet dependencies so a single "✗" in the
		// DEPS column doesn't get lost in a long list. Before the essential hints,
		// for the reason given in the paneEvents case: fitHintLine drops from the
		// front, so a notice appended past "[q] quit" outlives it.
		if n := m.unmetDepsCount(); n > 0 {
			base = fmt.Sprintf("%s  ·  %d plugin%s with unmet deps",
				base, n, plural(n))
		}
		return base + "  [?] keys  [q] quit"
	case panePluginDetail:
		return "[↑↓] scroll  [esc] back  [?] keys  [q] quit"
	case paneUsage:
		// [s] only appears when there is a session to scope to, so the footer
		// never advertises a key that would do nothing.
		scopeHint := ""
		if m.usage.session != "" {
			scopeHint = "  [s] all sessions"
		} else if m.selectedSess != "" {
			scopeHint = "  [s] this session"
		}
		// [b] is omitted under latency rather than shown as a no-op: a footer that
		// advertises an inert key is worse than a shorter footer.
		breakdownHint := "  [b] breakdown"
		if m.usage.metric.isLatency() {
			breakdownHint = ""
		}
		// No [r]: the pane polls every 20s on its own, so a manual refresh key
		// bought nothing but a line of footer.
		return "[m] metric  [w] window" + breakdownHint + scopeHint +
			"  [$] cost  [esc] back  [?] keys  [q] quit"
	case paneCost:
		// No [r]: the pane polls every 20s on its own, the same reasoning as the Usage
		// pane's footer. No [s] either — there is nothing to scope to, because the session
		// breakdown is one of [g]'s positions rather than a scope.
		return "[w] window  [g] breakdown  [esc] back  [?] keys  [q] quit"
	case paneCatalog:
		if m.catalog == nil {
			return "loading catalog…  [esc] back  [?] keys  [q] quit"
		}
		return "[↑↓] nav  [↵] plugin detail  [r] refresh  [esc] back  [?] keys  [q] quit"
	}
	return "[?] keys  [q] quit"
}

// layout recomputes component sizes to fit the current terminal. Called on
// every WindowSizeMsg.
//
// Up to FIVE rows can be reserved, not three: the title one, the footer two, the
// spend strip one more whenever the terminal is tall enough (spendStripReservesRow),
// and the filter input one more while it is open. This comment said "the footer
// reserves two lines; the title one" and stopped there — true and complete before
// either conditional row existed. The exact budget is in the body; keep the two in
// step, and keep app.go's bodyHeight comment in step with both.
func (m *model) layout() {
	if m.width == 0 || m.height == 0 {
		return
	}
	// Reserve 1 row for the title and 2 for the footer (status + hint). Two more
	// are conditional: the spend strip's, and the filter input's.
	//
	// Neither conditional row is BORROWED from the three. An earlier comment here
	// said "title + blank + footer", but the arithmetic was title(1) + footer(2) = 3
	// and there was never a blank row to take. Borrowing would leave every table one
	// row too tall and push the footer off the bottom of the terminal.
	//
	// The strip's row is reserved on height alone; see spendStripReservesRow for why
	// it must not read m.pane, and what that costs the two picker panes.
	reserved := 3
	if m.spendStripReservesRow() {
		reserved++
	}
	// The filter's row, whenever it is open: View() prepends filterInput above the
	// body, so the line exists on screen whether or not the budget admits it.
	// Unreserved, the view came out one line taller than the terminal at every size,
	// the terminal scrolled, and the bottom row went missing for as long as the
	// operator was typing a filter — the same symptom as a mis-sized table, from a
	// line nobody counted.
	if m.filtering {
		reserved++
	}
	bodyH := m.height - reserved
	if bodyH < 4 {
		bodyH = 4
	}

	// Fit every fixed-width table to the terminal. Only the events table fitted itself (see
	// fitColumns); these three declared their widths in their constructors and rendered wider
	// than an 80-column terminal, wrapping every row. Applied from the constructors' own
	// definitions each time rather than to the live columns, so widening the terminal back up
	// restores what a narrower one took away.
	m.sessionsTbl.SetColumns(fitTableColumns(sessionsColumns(), m.width))
	m.pipelineTbl.SetColumns(fitTableColumns(pipelineColumns(), m.width))
	m.catalogTbl.SetColumns(fitTableColumns(catalogColumns(), m.width))

	// Rebuild the sessions ROWS, not just re-fit the columns. AFTER SetColumns above, and
	// the order is the whole point: rebuildSessionsTable reads the width COST was
	// actually fitted to (fittedSessionsColumnWidth) to decide whether a dollar figure
	// fits or has to be elided, so running it first would read the previous width and
	// change nothing.
	//
	// Without this call the columns were re-fitted on every WindowSizeMsg while the rows
	// kept the formatting chosen for the PREVIOUS width, until the next
	// sessionsLoadedMsg or streamed event happened to repaint them. For that interval a
	// just-narrowed terminal showed one clipped figure and a just-widened one a needless
	// ellipsis — the seam fittedSessionsColumnWidth's own doc names and leaves to this
	// call site.
	//
	// Safe before the first fetch: m.sessions is empty, so it sets an empty row set on an
	// already-empty table and parks the cursor at 0. It is the same call
	// rebuildEventsTable a few lines down already makes unconditionally.
	m.rebuildSessionsTable()
	// Through setTableHeight, not SetHeight: a height change re-windows the rows while the
	// viewport keeps the offset it had for the old height, so something has to reconcile
	// it.
	//
	// AFTER the rebuild, and that ordering is load-bearing rather than incidental. Merging
	// #998 with the rebuild above put two cursor-touching calls next to each other:
	// rebuildSessionsTable reads Cursor() first, to re-select the same SESSION ID after the
	// rows are replaced, while setTableHeight moves the cursor (GotoTop, then
	// setCursorVisible). Height first would hand the rebuild a cursor that had already
	// moved, so it would preserve whichever row the re-windowing happened to land on
	// instead of the one the operator had selected. Rows first, then reconcile the offset
	// against the final row set.
	//
	// #998's own justification for this call — "these tables are not rebuilt from here" —
	// is no longer true of THIS table, though it still holds for the four below. The call
	// is still wanted here: the rebuild replaces rows, not the viewport's offset.
	setTableHeight(&m.sessionsTbl, bodyH)
	m.bodyHeight = bodyH
	// Picker tables share the same body area as the session tables so the
	// terminal real estate stays constant as the user navigates panes.
	setTableHeight(&m.namespacesTbl, bodyH)
	setTableHeight(&m.podsTbl, bodyH)
	// The events table's height depends on whether the IDENTITY banner
	// is rendered for the selected session. rebuildEventsTable() applies
	// the banner-aware adjustment; call it so the size is correct after
	// a window resize too.
	m.rebuildEventsTable()
	setTableHeight(&m.pipelineTbl, bodyH)
	// The catalog table had no height set anywhere: it kept bubbles' table.New
	// default of 20 rows for the life of the process, so on a terminal shorter than
	// that the pane rendered past the bottom (scrolling the title away) and on a
	// taller one it left the remaining rows unused. TestLayout_EveryPaneFitsTheTerminal
	// covered this pane but never populated m.catalog, so it only ever measured the
	// "loading catalog…" line.
	setTableHeight(&m.catalogTbl, bodyH)
	m.detailVp.Width = m.width
	m.detailVp.Height = bodyH
	// Re-clamp the scroll offset to the new height, for the PLUGIN detail pane:
	// nothing re-renders that one on a resize, so this is the only place its offset can
	// be reconciled. The events detail pane is re-rendered just below and clamps itself
	// (see showDetail), which is why this line cannot simply move down there — the
	// re-render would land after it either way.
	m.detailVp.SetYOffset(m.detailVp.YOffset)

	m.filterInput.Width = m.width - 4

	// Re-wrap the detail viewport to the new width so long JSON values continue to fit
	// after a terminal resize. Not a scroll reset: the reader stays where they were, as
	// in the help overlay.
	//
	// Only while that pane is the one on screen. detailVp is shared with the PLUGIN
	// detail pane, and m.detailEvent outlives the pane that set it — nothing clears it
	// on the way out, only the pod/session reset does — so read an event, esc, open a
	// plugin, resize, and this call replaced the plugin's content with the old event's
	// JSON under a "pipeline · tool-prune" title. The clamp above then reconciled the
	// offset against content the operator had not asked for.
	if m.pane == paneDetail && m.detailEvent != nil {
		m.showDetail(m.detailRow, false)
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
