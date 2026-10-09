package tui

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/cmd/agentop/edit"
	"github.com/rossoctl/cortex/cmd/agentop/tui/table"
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
// pipeline edit, then the search prompt. Only if none of those own the
// keyboard is the key dispatched based on the active pane.
func (m *model) handleKey(msg tea.KeyMsg) tea.Cmd {
	// A sticky flash (yank) stays up until the operator does something. Any key
	// dismisses it, including one that goes on to do unrelated work: reading the
	// path was the only thing pending, and pressing a key says they are done.
	// Only the flag is cleared, never m.flash, so timed messages are unaffected.
	m.flashSticky = false

	// The search prompt consumes every key while it is open, on every pane — checked first,
	// before the pickers and the overlays, so a `q`, `?` or `u` in a query is typed rather
	// than acted on. Nothing below can open while the prompt is: every key that would open
	// one is a character here.
	if m.searching {
		return m.searchKey(msg)
	}

	// The help overlay is modal: while it's up, it owns the keyboard so a
	// stray key can't navigate the pane hidden underneath. Checked before
	// every other handler, including the picker and edit overlays, so `?`
	// is genuinely available everywhere.
	if m.helpVisible {
		switch msg.String() {
		case "/":
			m.openSearch(targetHelp)
			return nil
		case "n", "N":
			m.searchStep(targetHelp, msg.String() == "n")
			return nil
		case "?", "esc", "q", "ctrl+c":
			// `q`/ctrl+c close the overlay rather than quitting agentop:
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
	// X's confirmation is modal too, and checked before `?` and every pane's keys: it is the one
	// dialog whose wrong answer cannot be undone, so nothing else may act while it is up.
	if m.clearConfirm != nil {
		return m.clearConfirmKey(msg)
	}
	// S's picker is modal too, and for the same pane-scoping reason as the column picker: a
	// message can move the pane under it, and then it is inert rather than eating keys.
	if m.serverPicker != nil && m.pane == paneAgents {
		return m.serverPickerKey(msg)
	}
	if m.serverNotice != "" && m.pane == paneAgents {
		return m.serverNoticeKey(msg)
	}

	// `/`, n and N search the pane, on every pane. Above the pickers, which handle their own
	// keys and return. Not over the column picker or an edit, which own their keys.
	if m.editState.phase == editPhaseDone && !(m.colPicker && m.pane == paneEvents) {
		switch msg.String() {
		case "/":
			m.openSearch(m.pane)
			return nil
		case "n", "N":
			if m.surface(m.pane) != nil {
				m.searchStep(m.pane, msg.String() == "n")
				return nil
			}
		}
	}

	// `?` opens the overlay from any pane, with two exceptions. While a
	// pipeline edit is in flight that overlay is already modal and owns
	// y/N/r/Esc, so help would swallow the apply confirmation. While the
	// search prompt is open `?` is a character the user is typing — a
	// session ID or host can contain one — and stealing it would make
	// those values unsearchable.
	if msg.String() == "?" && m.editState.phase == editPhaseDone && !m.searching {
		m.helpVisible = true
		m.syncHelpViewport(true)
		return nil
	}

	// The Usage pane owns the keyboard while it is up, except for esc/q which
	// the shared handling above already routed.
	if m.pane == paneUsage && !m.searching {
		switch msg.String() {
		case "m":
			// Metric. `t` (for "tokens") named one of the four values rather than
			// the axis, and every other binding here is the first letter of what it
			// changes.
			m.usage.cycleMetric()
			m.persistUsage()
			return nil
		case "w":
			m.usage.cycleWindow()
			m.persistUsage()
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
			// Inert under an agent scope, which the footer and the [?] overlay both say. The
			// scope needs group=agent on the wire, so there is no axis left for this key to
			// move; dropping it here is what keeps it from refetching against an axis the
			// scoped narrowing would immediately discard.
			if m.agentScope != "" {
				break
			}
			// Refetch: the breakdown is a server-side query parameter, not a
			// client-side filter, so the current snapshot has no series for the
			// newly selected dimension.
			m.usage.cycleGroup()
			m.persistUsage()
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
	// it charts everything. Suppressed while searching, where `u` is a character
	// the user is typing — the same reasoning as the `?` overlay above.
	//
	// Gated on !m.colPicker as well: this handler sits above the picker block, so
	// without it `u` reached openUsage while m.colPicker stayed true — View() drew
	// the picker popup over the Usage pane, paneUsage's own m/w/b/s bindings went
	// live underneath it, and `esc` then closed the picker onto Usage instead of the
	// events timeline the picker was opened from. (`?` above is deliberately NOT
	// gated: it layers help over the picker without changing panes, and closing it
	// restores the picker.)
	if msg.String() == "u" && !m.searching && !m.colPicker && m.editState.phase == editPhaseDone {
		switch m.pane {
		case paneEvents, paneDetail:
			if m.selectedSess != "" {
				return m.openUsage(m.selectedSess)
			}
		case paneSessions:
			return m.openUsage("")
		}
	}

	// The spend drawer's three keys, handled where `u` is and gated the same way: not
	// while searching (they are characters the user is typing), not under the column
	// picker, not mid-edit.
	//
	// `$` BOTH OPENS AND CLOSES IT, and esc does neither. esc used to close the drawer before
	// reaching the pane, so with the drawer open, leaving a pane took two presses, and which
	// press did what depended on whether the drawer was open. The drawer is not modal (it
	// stays open alongside the table, and `a`/`w` are the only keys it takes), so esc keeps
	// its one meaning on every pane: go back.
	//
	// GLOBAL, unlike every pane binding below, because the strip is global — it draws on
	// every pane except the two pickers, and a breakdown of it that only opened on one
	// pane would be a pane's feature wearing the strip's clothes.
	//
	// `a` and `w` are live ONLY while the drawer is open. They are ordinary letters, and
	// claiming them permanently would take them from any future pane binding for the sake of a
	// surface that is closed most of the time; scoped to the open drawer they are discoverable
	// from its own hint line and inert otherwise.
	//
	// `a`, NOT `g`: `g` is globally "go to top", and the drawer stays open alongside the table,
	// so it would shadow that motion for most of a session. See cycleSpendAxis.
	//
	// `w` IS TAKEN ON paneUsage, whose handler runs above this one and returns — so the drawer
	// does not open on that pane at all (see spendDrawerHost) and its hint line never advertises
	// a key that belongs to something else.
	if !m.searching && !m.colPicker && m.editState.phase == editPhaseDone {
		switch msg.String() {
		case "$":
			return m.toggleSpendDrawer()
		case "a":
			if m.spendDrawerVisible() {
				return m.cycleSpendAxis()
			}
		case "w":
			if m.spendDrawerVisible() {
				return m.cycleSpendWindow()
			}
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
	if m.colPicker && m.pane == paneEvents && !m.searching {
		switch msg.String() {
		case "q", "ctrl+c":
			// Quit stays live. A modal that traps the user until they find its exit
			// is worse than one that closes on the key they already reach for, and
			// `q` means quit everywhere else in agentop. Falls through to the global
			// handler rather than being reimplemented here.
		case "c", "esc", "enter":
			m.colPicker = false
			// Persist on close, not on each toggle: a user trying four columns on the
			// way to the two they want would otherwise produce three writes describing
			// states they rejected. `q` is not handled here — it falls through to the
			// global quit above, because quitting is not settling on a selection.
			Settings.Events.Columns = columnSettingsFrom(m.eventColumns)
			// The sort rides along on the same save, for the same reason: it is a
			// deliberate view choice made in this modal, and it would be odd for the
			// column set to survive a restart while the ordering did not.
			Settings.Events.SortColumn = string(m.sortCol)
			Settings.Events.SortDesc = m.sortDesc
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
			// Hiding the column the table is SORTED by leaves the sort in place, so the
			// rows stay ordered by a column that is no longer on screen. Deliberate
			// rather than overlooked: the operator may well want the ordering without
			// the column taking up width, and clearing the sort here would make a
			// visibility toggle silently reorder the whole table. Three things keep it
			// discoverable — the footer still names the ordering, the picker still lists
			// the hidden column so the cursor can reach it, and both the `s` cycle and
			// `r` recover from here.
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
		case "s":
			// Sort by the column under the cursor (#865). One key cycles all three
			// states for that column: descending → ascending → chronological.
			//
			// Descending FIRST because the question the issue asks — "the events with
			// the longest duration or highest token cost" — wants the extreme at the
			// top; ascending is the follow-up, not the opening move.
			//
			// Cycling back through "off" on the same key is how the operator recovers
			// arrival order without a second binding to discover. Pressing `s` on a
			// DIFFERENT column jumps straight to that column, descending, rather than
			// inheriting the previous column's direction — a fresh column is a fresh
			// question.
			//
			// `s` does not collide with the global hideInactive toggle: this modal block
			// returns before the global switch is reached, the same way its `r` shadows
			// nothing.
			id := eventColumns[m.colCursor].id
			switch {
			case eventColumns[m.colCursor].sortKey == nil:
				// "#" has no sort key: its order IS arrival order, so sorting by it is
				// what sorting by nothing already does. Flash rather than silently
				// ignoring the key, so a modal that swallows everything else does not
				// read as broken here.
				m.setFlash(string(id) + " is the chronological order")
			case m.sortCol != id:
				m.sortCol, m.sortDesc = id, true
			case m.sortDesc:
				m.sortDesc = false
			default:
				m.sortCol, m.sortDesc = "", false
			}
			m.rebuildEventsTable()
			return nil
		case "r":
			m.eventColumns = defaultColumnSelection()
			// Reset means the default VIEW, which is chronological — the same reasoning
			// that makes this key restore the default column set.
			m.sortCol, m.sortDesc = "", false
			m.rebuildEventsTable()
			return nil
		default:
			// Everything else is swallowed: the popup is modal, so a stray key must
			// not move the table cursor underneath it.
			return nil
		}
	}

	// `c` opens the column picker from the events timeline. Suppressed while
	// searching, where `c` is a character being typed — the same reasoning as `?`
	// and `u`.
	if msg.String() == "c" && m.pane == paneEvents && !m.searching &&
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
			// in-mesh agentop, or a tunnel from a kubeconfig that can't list
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

	switch msg.String() {
	case "ctrl+c", "q":
		m.cancel()
		return tea.Quit

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
		// Back-out: plugin-detail → pipeline (or catalog if we came from there);
		// detail → events; events → sessions; pipeline, catalog and usage → the
		// pane that opened them. Sessions is the only top-level pane now, and in
		// picker mode it backs out further to the Pods picker, tearing down PF +
		// SSE. It is the ONLY pane that does that — a key-opened surface returning
		// to its caller must never cost the connection.
		switch m.pane {
		case panePluginDetail:
			// Return to whichever pane invoked the detail (Pipeline or Catalog).
			if m.previousPane == paneCatalog {
				m.pane = paneCatalog
				m.previousPane = paneNone
			} else {
				m.pane = panePipeline
			}
		case paneAgents:
			// Leaving WITHOUT touching the scope, which is the difference between this and the
			// Enter arm below. esc means "back out" on every other pane here, and a key that
			// silently discarded a scope on the way out would be the one exception.
			return m.leaveAgentsPane()
		case paneCatalog:
			// Return to whichever pane the user pressed `C` from.
			//
			// The fallback is SESSIONS, not the pipeline. It was the pipeline while the
			// pipeline was a top-level pane; now that it is a key-opened surface, being
			// dropped into it is being dropped somewhere the operator never asked to be,
			// with a return pane of paneNone underneath. Sessions is the only pane that
			// is always a defensible place to land.
			//
			// The branch is reachable, not just defensive: drilling catalog → plugin
			// detail overwrites previousPane with paneCatalog and the detail's esc
			// clears it, so the catalog backs out with no caller recorded.
			if m.previousPane != paneNone {
				m.pane = m.previousPane
				m.previousPane = paneNone
			} else {
				m.pane = paneSessions
			}
			// Returning INTO Usage has to restart its polling chain. The tick
			// that was in flight when the catalog opened was dropped by the
			// `m.pane != paneUsage` guard, so without this nothing reschedules
			// and the 20s auto-refresh is silently dead until the user backs all
			// the way out and re-enters with `u` — `r` refetches once but starts
			// no chain.
			if m.pane == paneUsage {
				return m.resumeUsagePolling()
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
		case panePipeline:
			// Back to the pane `P` was pressed on, from the pipeline's OWN field —
			// model.previousPane is the catalog's and gets clobbered when the catalog
			// is opened from here, exactly as it did for Usage.
			//
			// IT USED TO BE GROUPED WITH paneSessions BELOW, which meant esc here tore
			// down the port-forward and the SSE stream and dropped the operator at the
			// pods picker. That was defensible while `tab` made the pipeline a top-level
			// peer with nowhere above it; now that it is opened by a key from somewhere,
			// esc has a caller to return to, and "show me the config" must not cost a
			// connection.
			if m.pipelineReturnPane != paneNone {
				m.pane = m.pipelineReturnPane
				m.pipelineReturnPane = paneNone
			} else {
				m.pane = paneSessions
			}
		case paneDetail:
			m.pane = paneEvents
		case paneEvents:
			m.pane = paneSessions
		case paneSessions:
			if m.sessionsViaAgents {
				return m.returnToAgentsPane()
			}
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
		case paneAgents:
			// Pick the agent the views are narrowed to, then LIST ITS SESSIONS: the pane is a
			// picker, and picking an agent is asking for its sessions.
			//
			// NOT A TOGGLE: Enter on the agent already scoped keeps it, and the All agents row is
			// what clears it. See allAgentsLabel.
			scope, ok := m.selectedAgentScope()
			if !ok {
				return nil
			}
			m.agentScope = scope
			// SESSIONS, NOT THE PANE `A` WAS PRESSED ON. This used to share esc's exit and return
			// there, and from a session's events that hid the pick: the events pane shows one
			// session whatever the scope, so the reader landed back in the session they were
			// reading — under a title naming an agent it might not belong to. esc keeps that exit;
			// it changes nothing, so going back is all it can mean. previousPane is spent as
			// leaveAgentsPane spends it, since the catalog shares the field. The spend band and
			// drawer restart under the new scope, so no figure from the old one stays on screen
			// beneath it. esc on that list comes back here.
			m.pane = paneSessions
			m.previousPane = paneNone
			m.sessionsViaAgents = true
			m.rebuildSessionsTable()
			return m.startSpendPolling()
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
			// cached-only session is kept — agentop's copy is the only copy, so
			// dropping it would be the same unrecoverable loss this PR exists to
			// stop. After a restart every previously-visited session is
			// cached-only, so opening one must not destroy the rest.
			//
			// The honest consequence: cached-only sessions are never released
			// while agentop runs, so the cache grows by one entry per restart the
			// user visited a session across. Measured, that is ~165 bytes per
			// event and 1000 events per session, so ~161 KB per session and a
			// few MB for a long debugging afternoon — worth it, given the
			// alternative is deleting the only copy of what someone is reading.
			if id != m.selectedSess {
				for cached := range m.events {
					if cached != id && live[cached] {
						delete(m.events, cached)
						// contextRun is deliberately NOT deleted, unlike the two
						// below. This releases a still-listed session's events for
						// memory — a storage decision, not news about the session —
						// and the gauge's figure is one int. Dropping it would turn a
						// live session's context into a dash; sessionContextFor
						// re-folds whatever is left on top of the figure it kept.
						//
						// Same reason as the wholesale reset in backToPodsPane: the
						// count belongs to the events it describes.
						delete(m.olderNotFetched, cached)
						// And so does the paging window. Left behind, it described a
						// window that no longer existed: a page still in flight would
						// land on a session with no events, resurrect it, and leave
						// pageSizes describing pages that were never there.
						delete(m.paging, cached)
					}
				}
				// Clear only on an actual session change, so
				// re-entering the same session keeps the pin.
				m.selectedEventKey = eventKey{}
				// And the events and message-detail searches, for the same reason: they were
				// typed for that session's events, and re-entering it keeps them.
				delete(m.search, paneEvents)
				delete(m.search, paneDetail)
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
			// Render from the summary immediately, then fill the message bodies in
			// when they arrive — the timeline no longer carries them. See
			// fetchDetailEventCmd for why this is not a blocking spinner.
			m.showDetail(er, true)
			m.pane = paneDetail
			if m.needsFullEvent(er.event) && m.client != nil {
				return fetchDetailEventCmd(m, m.selectedSess, er.event.Seq)
			}
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
			// Say so when the bodies are not in the file. `y` exists to hand an
			// event to somebody for debugging, and a body-less event that LOOKS
			// complete is the kind of surprise that wastes an afternoon — the
			// timeline is projected now, so the messages arrive a moment after the
			// row does, and not at all if that fetch failed.
			note := ""
			if m.detailIsProjected() {
				note = "  (message bodies not loaded yet — re-yank in a moment)"
			}
			m.setStickyFlash("yanked → " + path + note)
		}
		return nil

	case "e":
		if m.pane != panePipeline {
			return nil
		}
		// Two targets can back an edit — this machine's config file, or a
		// pod's ConfigMap via the picker. pipelineStore says which, if either;
		// a nil store is the only case there is nothing to edit.
		store, statusURL := m.pipelineStore()
		if store == nil {
			// Name what is missing and how to get it. The old text said
			// "requires the picker (no --endpoint)", which described neither:
			// a bare `agentop` that auto-connected to a local Cortex passes no
			// --endpoint at all, so it accused the operator of a flag they had
			// not used and omitted the one that would have helped.
			m.setFlash("no pipeline to edit here — run `agentop --kubernetes` to pick a pod, or point agentop at a Cortex running on this machine")
			return nil
		}
		if m.editState.phase != editPhaseDone {
			return nil // already editing
		}
		gen := m.editState.generation + 1
		m.editState = editState{phase: editPhaseFetching, generation: gen, store: store, statusURL: statusURL}
		return withGen(gen, edit.FetchCmd(m.ctx, store, m.client, catalogPlugins(m.catalog)))

	case "g":
		m.goTop()
		return nil

	case "G":
		m.goBottom()
		return nil

	case "o":
		// Load the page before the oldest event held. Only in the timeline: the other
		// panes have nothing to page.
		if m.pane != paneEvents {
			return nil
		}
		return m.loadOlderPage()

	case "t":
		// Back to the live tail from a paged-back timeline.
		if m.pane != paneEvents {
			return nil
		}
		return m.returnToTail()

	case "pgup", "pgdown", "pgdn", "b", "f":
		// Page the active pane. Sessions can hold up to session.max_events
		// (500) rows, so one-row-at-a-time nav isn't enough. b/f (less/vim
		// style) work on any keyboard; PgUp/PgDn map to fn+↑/fn+↓ on Mac
		// laptops. Explicit here (rather than the table component's own
		// binding) so all of them share a one-row overlap for context and the
		// detail viewport pages too.
		return m.pageActivePane(msg)

	case "X":
		// Clear all history, after a confirmation. Capital, and off the footer like `A`: a
		// destructive key should not be one stray press of a common letter, nor advertised on
		// the always-visible line. The `?` overlay names it.
		if m.client == nil || m.pane != paneSessions {
			return nil
		}
		return m.openClearConfirm()

	case "P":
		// Open the pipeline. `P` REPLACED `tab` HERE, and took the letter off the
		// plugin catalog (now `C`) to do it.
		//
		// `P` because the house rule is the first letter of the thing, and the only
		// other spelling of it — lowercase `p` — pauses the stream, is printed in
		// two footers, and has no replacement worth the trade: `space` and `d` are
		// bubbles' page-down and half-page-down, and every free letter is a worse
		// pause mnemonic than `p` is a pipeline one. The catalog's claim on `P` was
		// the weaker of the two (it only fit by borrowing the "Plugin" in "plugin
		// catalog") and `C` for Catalog is a better key than it gave up.
		//
		// The allowlist is the same three panes `u` opens Usage from, deliberately:
		// two top-level surfaces reached from the same places is a rule an operator
		// can hold, whereas two overlapping sets is a lookup. It also keeps the
		// pipeline out of the nested-overlay cases (opened from Usage or from the
		// catalog) where the pane to return to is ambiguous.
		//
		// The panes match; the CONDITIONS do not, and the difference is deliberate.
		// `u` additionally requires a selected session on Events/Detail, because its
		// charts are scoped to that session and there is nothing to chart without
		// one. The pipeline is the proxy's plugin chain — the same on every session —
		// so it needs no session and asks for none.
		//
		// DO NOT MOVE THIS UP BESIDE `u` ON THE STRENGTH OF THAT SYMMETRY. `u`, `$`,
		// `c` and `?` sit ABOVE the modal blocks and each carries explicit
		// `!m.searching && !m.colPicker && m.editState.phase == editPhaseDone` guards
		// to compensate. `P` and `C` carry none, and do not need to, because they sit
		// BELOW all three: the column picker ends in `default: return nil`, the search
		// prompt returns unconditionally after feeding the input, and an in-flight edit
		// returns via handleEditKey. The safety here is POSITIONAL. Hoisting either
		// key above those blocks without adopting the guards would make `P` change
		// panes under a modal popup and swallow a `P` someone was typing into the
		// search — silently, since nothing in the type system notices.
		if m.client == nil {
			return nil
		}
		switch m.pane {
		case paneSessions, paneEvents, paneDetail:
		default:
			return nil
		}
		m.pipelineReturnPane = m.pane
		m.pane = panePipeline
		// The composition is fetched eagerly in Init, so this normally has rows to
		// build from already; rebuild rather than refetch. A nil pipeline renders
		// "(loading pipeline…)" and rebuildPipelineTable is a no-op on it.
		m.rebuildPipelineTable()
		return nil

	case "S":
		// Choose where the highlighted agent's new sessions go. Capital, for the reason `A` and
		// `P` are: lowercase `s` hides skip rows on every pane. On the AGENTS pane only, the one
		// surface whose rows are agents.
		if m.pane == paneAgents {
			return m.openServerPicker()
		}
		return nil

	case "A":
		// Open the per-agent cost breakdown.
		//
		// `A` CAPITALISED, for the reason `P` is: lowercase `a` is the spend drawer's axis
		// cycle. Available wherever `C` is — anywhere past the pickers, since it needs a
		// connection.
		//
		// ALWAYS REFETCHED, never opened from cached rows: how many agents have been seen changes
		// while agentop runs. The reply enters the pane.
		if m.client == nil {
			return nil
		}
		switch m.pane {
		case paneNamespaces, panePods:
			return nil
		}
		// On the pane itself it is a refresh, leaving the pane's caller and place as they are.
		if m.pane == paneAgents {
			return m.fetchAgentRowsCmd(agentsOpenNever, paneNone)
		}
		// THE CALLER IS RESOLVED HERE, AT PRESS TIME, and carried on the message: by the time the
		// reply lands the reader may have navigated elsewhere.
		return m.fetchAgentRowsCmd(agentsOpenOnPress, m.pane)

	case "C":
		// Open the registered-plugin catalog. Available from any
		// session-view pane; in --endpoint mode the picker fields
		// don't matter — the catalog comes via the same /v1/* endpoint
		// agentop is already pointed at.
		//
		// `C` for Catalog, moved off `P` when the pipeline took that letter. See
		// the `P` case above for why this side of the swap is the cheap one — and
		// note this key's own doc in globalKeys, which records that `P` was never
		// advertised in any footer, so the muscle memory being broken is thin.
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
		// Skip over the divider row when navigating: one more step in the direction
		// of travel.
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
	case paneAgents:
		// Without this arm the pane's navigation keys fell off the end of the switch and
		// returned nil, while its footer printed "[↑↓] nav" and the help overlay listed
		// "↑↓ / jk  navigate" — an advertised key that does nothing. The table is built
		// WithFocused(true), so it drew a selection highlight on a cursor nothing could move.
		//
		// NO `r` BRANCH, unlike the catalog above: the rows are refetched by every `A` press,
		// which is why the footer advertises no refresh key either.
		var cmd tea.Cmd
		m.agentsTbl, cmd = m.agentsTbl.Update(msg)
		return cmd
	}
	return nil
}

// goTop and goBottom place the cursor through setCursorVisible, which carries the
// empty-table guard and clamps, so goBottom does not need the row count.
func (m *model) goTop() {
	switch m.pane {
	case paneCatalog:
		setCursorVisible(&m.catalogTbl, 0)
	case paneSessions:
		setCursorVisible(&m.sessionsTbl, 0)
	case paneEvents:
		setCursorVisible(&m.eventsTbl, 0)
		m.selectedEventKey = keyOf(m.selectedEvent())
		// `g` is an explicit "take me to the oldest end", so it also becomes where
		// the next session opens — see EventSettings.OpenAtOldest.
		m.setOpenAtOldest(true)
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
		// The mirror of `g` above: back to the tail, and that is where the next
		// session opens.
		m.setOpenAtOldest(false)
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

// persistUsage records the usage pane's view, then saves.
//
// Its own helper rather than three copies of the two lines: [m], [w] and [b] each
// change one of the three, and all three belong in the file together — a reader
// returning to the pane expects the view they left, not two thirds of it.
func (m *model) persistUsage() {
	Settings.Usage = captureUsage(m.usage.metric, m.usage.windowIdx, m.usage.group)
	m.persistSettings()
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
	if m.searching {
		return "type to search · [enter] done · [esc] cancel"
	}
	switch m.pane {
	case paneNamespaces:
		return "[↑↓/jk] nav  [↵] open  [l] " + shortHost(m.localEndpointOr()) +
			"  [r] reload  " + m.searchHints(m.pane) + "  [?] keys  [q] quit"
	case panePods:
		return "[↑↓/jk] nav  [↵] connect  [Esc] back  [r] reload  " + m.searchHints(m.pane) + "  [?] keys  [q] quit"
	case paneSessions:
		// THE PER-SESSION SCOPE IS NOT SAID HERE. It was, as a leading notice; then the pane's
		// title carried it as " · lifetime totals"; now neither does. See paneView's sessions
		// case for why the note was dropped rather than reworded — "lifetime" named a span this
		// table does not have, and named the shortest one on screen when it named any.
		//
		// A notice here would have been the wrong place for it regardless: fitHintLine drops
		// whole hints from the FRONT, so anything added here is paid for by the hints ahead of
		// it — and the two keys that reach cost, [u] and [$], were deliberately placed to
		// survive an 80-column cut. Spending them to explain a cost column is a bad trade at
		// any width. The [?] overlay is the surface that cannot run out of room.
		// [$] spend BESIDE [u] usage, because the footer is where a key gets discovered. It was
		// documented in the [?] overlay and in the README's own footer sample and was missing
		// from the line those two describe — so the drawer existed only for a reader who went
		// looking for it. Placed after [u] so fitHintLine, which drops from the front, gives up
		// the navigation keys before either of the two that reach cost.
		// [P] pipeline sits at the TAIL, not where [tab] pipeline used to sit third
		// from the left. The tab strip in the title bar survived every width; a footer
		// hint does not, and fitHintLine drops from the front — so the key that
		// replaced the strip is placed behind the hints it is willing to outlive.
		//
		// MEASURED, because the intuition here is off by more than it looks: the
		// un-drilled line is 98 columns, and [P] survives down to 34 columns at the
		// tail versus 79 third from the left. So the two placements are equivalent at
		// the classic 80-column floor — 79 still fits — and the tail only starts
		// paying below that, in a tmux split or a half-screen terminal. It is bought
		// cheaply (the hints it outlives are the two most guessable on the line) so it
		// is worth having, but it is not the difference between visible and invisible
		// at 80 that the first draft of this comment claimed.
		// [A] agents is absent for width: the line is already 98 columns, and with one agent —
		// the common case — the breakdown is a single row. The [?] overlay names it. esc names
		// the picker instead when this list was reached through it.
		search := m.searchHints(m.pane)
		if m.sessionsViaAgents {
			return "[↑↓] nav  [↵] drill  [u] usage  [$] spend  " + search + "  [esc] agents  [p] pause  [P] pipeline  [?] keys  [q] quit"
		}
		if m.parentCtx != nil {
			return "[↑↓] nav  [↵] drill  [u] usage  [$] spend  " + search + "  [esc] pods  [p] pause  [P] pipeline  [?] keys  [q] quit"
		}
		return "[↑↓] nav  [↵] drill  [u] usage  [$] spend  " + search + "  [p] pause  [P] pipeline  [?] keys  [q] quit"
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
		base := "[↑↓] nav  [b/f] page  [↵] detail  [c] columns  [u] usage  " +
			skipHint + "  [p] pause  " + m.searchHints(m.pane) + "  [esc] back"

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
		// Older events the snapshot did not ask for. Same reasoning as the hidden count
		// one line up — a partial timeline should not read as the whole one — and it now
		// names the key that fetches them, because there is one.
		if n := m.olderNotFetched[m.selectedSess]; n > 0 {
			base = fmt.Sprintf("%s  ·  %d older ([o] to load)", base, n)
		}
		// Paging backward suspends this session's live appends, and dropping a page at
		// the cap means the timeline no longer reaches the present. Both are states the
		// operator did not ask for explicitly and cannot otherwise see — a timeline that
		// silently stopped updating is the same class of lie as one that silently
		// omitted its beginning.
		if st := m.paging[m.selectedSess]; st != nil {
			note := "paged back, live paused ([t] for tail)"
			if st.droppedNewer {
				note = "paged back, newer dropped ([t] for tail)"
			}
			base = fmt.Sprintf("%s  ·  %s", base, note)
		}
		// Columns that did not fit — the whole reason issue #866 was filed: HOST was
		// declared but never visible, and nothing said the table had been clipped.
		if m.eventColsDropped > 0 {
			base = fmt.Sprintf("%s  ·  → %d more column%s ([c] to choose)",
				base, m.eventColsDropped, plural(m.eventColsDropped))
		}
		return base + "  [?] keys  [q] quit"
	case paneDetail:
		return "[↑↓] scroll  [y] yank  [u] usage  " + m.searchHints(m.pane) + "  [esc] back  [?] keys  [q] quit"
	case panePipeline:
		// ONE SPELLING NOW, where there used to be two. The picker/bypass split
		// existed only because esc meant different things in the two modes — "back
		// to pods" with a parent context, nothing without one. esc now returns to
		// whichever pane opened this one in both modes, so there is nothing left for
		// the branch to say differently.
		base := "[↑↓] nav  [↵] plugin detail  [e] edit  [esc] back"
		// Surface a count of plugins with unmet dependencies so a single "✗" in the
		// DEPS column doesn't get lost in a long list. Before the essential hints,
		// for the reason given in the paneEvents case: fitHintLine drops from the
		// front, so a notice appended past "[q] quit" outlives it.
		if n := m.unmetDepsCount(); n > 0 {
			base = fmt.Sprintf("%s  ·  %d plugin%s with unmet deps",
				base, n, plural(n))
		}
		return base + "  " + m.searchHints(m.pane) + "  [?] keys  [q] quit"
	case panePluginDetail:
		return "[↑↓] scroll  " + m.searchHints(m.pane) + "  [esc] back  [?] keys  [q] quit"
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
		// Omitted under an agent scope for the same reason as under latency: the scope has taken
		// the wire axis, so there is no second one to break down by, and a footer that advertises
		// an inert key is worse than a shorter footer.
		if m.agentScope != "" {
			breakdownHint = ""
		}
		if m.usage.metric.isLatency() {
			breakdownHint = ""
		}
		// No [r]: the pane polls every 20s on its own, so a manual refresh key
		// bought nothing but a line of footer.
		return "[m] metric  [w] window" + breakdownHint + scopeHint +
			"  " + m.searchHints(m.pane) + "  [esc] back  [?] keys  [q] quit"
	case paneCatalog:
		if m.catalog == nil {
			return "loading catalog…  [esc] back  [?] keys  [q] quit"
		}
		return "[↑↓] nav  [↵] plugin detail  [r] refresh  [esc] back  " + m.searchHints(m.pane) + "  [?] keys  [q] quit"
	case paneAgents:
		// [↵] LABELLED BY WHAT IT WILL SHOW: the highlighted agent, or every agent on the All
		// agents row.
		//
		// STILL NO [r]: the rows are refetched by every `A` press, so a refresh key would
		// duplicate the way in, and advertising it would be the inert-key problem the usage
		// pane's breakdownHint above avoids.
		enterHint := "  [↵] scope to this agent"
		if scope, ok := m.selectedAgentScope(); ok && scope == "" {
			enterHint = "  [↵] all agents"
		}
		escHint := "  [esc] back"
		if m.agentsAboveSessions {
			escHint = ""
			if m.parentCtx != nil {
				escHint = "  [esc] pods"
			}
		}
		// [S] only on a row where it opens, so the footer never advertises a key that would only
		// say why not. After esc, so fitHintLine — which drops from the front — gives up nav first.
		serverHint := ""
		if m.serverKeyOffered() {
			serverHint = "  [S] server"
		}
		return "[↑↓] nav" + enterHint + escHint + serverHint + "  " + m.searchHints(m.pane) + "  [?] keys  [q] quit"
	}
	return "[?] keys  [q] quit"
}

// layout recomputes component sizes to fit the current terminal. Called on
// every WindowSizeMsg. The footer reserves two lines; the title one.
func (m *model) layout() {
	if m.width == 0 || m.height == 0 {
		return
	}
	// Reserve 3 rows: the title row and the footer's two.
	//
	// NOT "title + blank + footer", which is what this said and was never true — there is
	// no blank row to borrow, and a reader who believes there is one reclaims a row the
	// footer is standing on.
	bodyH := m.height - 3
	// And one for the rule that closes the top block.
	//
	// UNCONDITIONAL, unlike the two reservations below it: paneView draws the rule on every pane
	// that renders a body, so there is no pane-dependent condition here to get wrong and none
	// that can go stale between resizes.
	//
	// The two states that do NOT draw it — the edit overlay and `m.width == 0` — are full-screen
	// takeovers that never read bodyHeight, so this row cannot be stranded in a body that does
	// not exist. That is the exemption's reason, and it is narrower than "it always draws"; see
	// renderDivider for what it means for anyone adding a third early return.
	//
	// TestLayout_EveryPaneFitsTheTerminal reports the reservation's absence as "one line too
	// many", on every pane and every size.
	bodyH -= dividerLines
	// And one for the spend strip, which View() draws directly under the title.
	//
	// BY HEIGHT ALONE, deliberately blind to the pane: layout() is called from exactly one
	// place, the WindowSizeMsg handler, and no pane transition re-runs it. A reservation
	// that read m.pane would go stale the moment the user moved between a picker and a data
	// pane, leaving bodyHeight wrong until the next resize — which on a pane the strip DOES
	// draw means a body one row too tall and a footer pushed off the bottom. The cost is
	// that the two picker panes render one row shorter than they need, which is invisible
	// next to that. See spendStripReservesRow and spendStripVisible for the same asymmetry
	// stated from the other side.
	if m.spendStripReservesRow() {
		// spendBandLines, not one row: the band stacks labels over values. paneView fills
		// exactly this many, and the two must move together — see paneView.
		bodyH -= spendBandLines
	}
	// And the drawer's rows when it is open. Reserving nothing for them made the view
	// spendDrawerLines taller than the terminal the moment `$` was pressed, pushing the footer
	// off the bottom on every pane — the failure the strip's own reservation exists to prevent,
	// five rows at a time instead of one. See spendDrawerReservesRows.
	if m.spendDrawerReservesRows() {
		bodyH -= spendDrawerLinesFor(m.width)
	}
	// And one more while the search prompt is open: View() prepends searchInput above the body,
	// so the line exists on screen whether or not the budget admits it. Unreserved, the view
	// came out one line taller than the terminal at every size, the terminal scrolled, and the
	// bottom row went missing for as long as the operator was typing — the same symptom as a
	// mis-sized table, from a line nobody counted.
	if m.searching {
		bodyH--
	}
	if bodyH < 4 {
		bodyH = 4
	}

	// Fit every fixed-width table to the terminal. Only the events table fitted itself (see
	// fitColumns); these three declared their widths in their constructors and rendered wider
	// than an 80-column terminal, wrapping every row. Applied from the constructors' own
	// definitions each time rather than to the live columns, so widening the terminal back up
	// restores what a narrower one took away.
	// The sessions table is NOT fitted here. Its header and its rows have to change together —
	// the money columns come and go with the width, and TITLE grows into a wide terminal's
	// slack — and setting columns with the old rows still loaded panics inside bubbles'
	// SetColumns. rebuildSessionsTable owns both; see its doc. It is called below, after the
	// heights are set.
	m.pipelineTbl.SetColumns(fitTableColumns(pipelineColumns(), m.width))
	m.catalogTbl.SetColumns(fitTableColumns(catalogColumns(), m.width))
	m.rebuildAgentsTable()

	m.sessionsTbl.SetHeight(bodyH)
	m.bodyHeight = bodyH
	// AFTER the height, so the cursor-visibility maths inside it uses the new window. This is
	// what fits the sessions header to the new width, rows included — which is also what
	// re-truncates the TITLE cells, since rebuildSessionsTable computes the new TITLE width and
	// passes it to sessionTitleCell. It does NOT read the live table — doing that was the
	// stale-width bug, where a narrowing resize truncated against the previous width. An
	// earlier revision added a second rebuild above for that, claiming nothing else
	// reconciled them; this call already did, and only the height changes in between.
	m.rebuildSessionsTable()
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
	// The catalog table had no height set anywhere: it kept bubbles' table.New
	// default of 20 rows for the life of the process, so on a terminal shorter than
	// that the pane rendered past the bottom (scrolling the title away) and on a
	// taller one it left the remaining rows unused. TestLayout_EveryPaneFitsTheTerminal
	// covered this pane but never populated m.catalog, so it only ever measured the
	// "loading catalog…" line.
	m.catalogTbl.SetHeight(bodyH)
	// The agents table had the catalog's bug, whose comment above is this one's history: nothing
	// sized it anywhere, so it kept bubbles' table.New default for the life of the process and
	// rendered past the bottom of any shorter terminal. The catalog's blind spot was an empty
	// fixture; this one was a size larger, and it is the class that will recur:
	// TestLayout_EveryPaneFitsTheTerminal enumerates the panes it measures in a HAND-WRITTEN
	// map, and paneAgents was simply not in it — so no fixture could have helped, and adding a
	// pane means adding it to that map too.
	m.agentsTbl.SetHeight(bodyH)
	m.detailVp.Width = m.width
	m.detailVp.Height = bodyH
	// Re-clamp the scroll offset to the new height, for the PLUGIN detail pane:
	// nothing re-renders that one on a resize, so this is the only place its offset can
	// be reconciled. The events detail pane is re-rendered just below and clamps itself
	// (see showDetail), which is why this line cannot simply move down there — the
	// re-render would land after it either way.
	m.detailVp.SetYOffset(m.detailVp.YOffset)

	m.searchInput.Width = m.width - 4

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
			payload, err := m.editState.store.Build(m.editState.fetched, newInner)
			if err != nil {
				m.editState.phase = editPhaseError
				m.editState.err = "build payload: " + err.Error()
				return nil
			}
			return withGen(m.editState.generation, edit.ApplyCmd(
				m.ctx, m.editState.store, m.editState.fetched, payload))
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
				// The store the failed attempt used, not a fresh
				// pipelineStore(): a retry is the same transaction, so
				// re-resolving could silently retarget it.
				store, statusURL := m.editState.store, m.editState.statusURL
				m.editState = editState{phase: editPhaseFetching, generation: gen, store: store, statusURL: statusURL}
				return withGen(gen, edit.FetchCmd(m.ctx, store, m.client, catalogPlugins(m.catalog)))
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
