package tui

// layout recomputes component sizes to fit the current terminal. Called on
// every WindowSizeMsg.
func (m *model) layout() {
	if m.width == 0 || m.height == 0 {
		return
	}
	// Reserve the title row, the body box's two edges and the footer.
	//
	// The box edges are UNCONDITIONAL, unlike the reservations below: paneView draws the box on
	// every pane that renders a body, so there is no pane-dependent condition here to get wrong
	// and none that can go stale between resizes. The two states that do NOT draw it — the edit
	// overlay and `m.width == 0` — are full-screen takeovers that never read bodyHeight; see
	// boxTop for what that means for anyone adding a third early return.
	//
	// TestLayout_EveryPaneFitsTheTerminal reports a reservation error as a view that is not
	// exactly as tall as the terminal, on every pane and every size.
	//
	// The spend band costs no row: it is the label of the box's top edge (see framePane).
	bodyH := m.height - 1 /* title */ - boxTopLines - boxBottomLines - footerLines
	// And the drawer's rows when it is open, plus its own box's bottom edge and the table box's
	// top edge. Reserving nothing for them made the view taller than the terminal the moment `$`
	// was pressed, pushing the footer off the bottom on every pane. This one reads the pane, and
	// Update re-runs layout() when it changes. See spendDrawerReservesRows.
	//
	// At the INNER width, because the drawer renders inside a box at that width: the reservation
	// and the render must use the same one.
	if m.spendDrawerReservesRows() {
		bodyH -= spendDrawerLinesFor(m.innerWidth()) + drawerBoxExtraLines
	}
	// And one more while the search prompt is open: View() prepends searchInput above the body,
	// so the line exists on screen whether or not the budget admits it. Unreserved, the view
	// came out one line taller than the terminal at every size, the terminal scrolled, and the
	// bottom row went missing for as long as the operator was typing — the same symptom as a
	// mis-sized table, from a line nobody counted.
	if m.searching && m.searchPane != targetHelp {
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
	m.pipelineTbl.SetColumns(fitTableColumns(pipelineColumns(), m.innerWidth()))
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
	// The agents table had no height set anywhere: it kept bubbles' table.New default for the
	// life of the process and rendered past the bottom of any shorter terminal. The plugin
	// catalog's own table (now the pipeline pane's available rows) had the same bug before it.
	// It is the class that will recur: TestLayout_EveryPaneFitsTheTerminal enumerates the panes
	// it measures in a HAND-WRITTEN map, and paneAgents was simply not in it — so adding a pane
	// means adding it to that map too.
	m.agentsTbl.SetHeight(bodyH)
	m.detailVp.Width = m.innerWidth()
	// The event detail's fixed line takes one row (#1681). Every pane but the plugin detail
	// gets the shorter viewport, because the event detail is the pane they return to, and
	// the usage and Timeline panes return to it without a re-render.
	m.detailVp.Height = bodyH
	if m.pane != panePluginDetail && bodyH > 1 {
		m.detailVp.Height = bodyH - 1
	}
	// Re-clamp the scroll offset to the new height, for the PLUGIN detail pane:
	// nothing re-renders that one on a resize, so this is the only place its offset can
	// be reconciled. The events detail pane is re-rendered just below and clamps itself
	// (see showDetail), which is why this line cannot simply move down there — the
	// re-render would land after it either way.
	m.detailVp.SetYOffset(m.detailVp.YOffset)

	m.searchInput.Width = m.innerWidth() - 4
	if m.searchPane == paneEvents {
		m.searchInput.Width = max(m.searchInput.Width-detailsBoxWidth, 1)
	}

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
