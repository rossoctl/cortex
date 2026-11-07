package tui

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
	// BY HEIGHT ALONE, deliberately blind to the pane: there is no setPane choke point to
	// hook (the pane is assigned in twenty-odd places, and layout() runs on resizes, drawer
	// toggles and pane transitions alike), so a reservation that read m.pane would go stale
	// the moment the user moved between a picker and a data pane, leaving bodyHeight wrong
	// until the next layout() — which on a pane the strip DOES draw means a body one row too
	// tall and a footer pushed off the bottom. The cost is that the two picker panes render
	// one row shorter than they need, which is invisible next to that. See
	// spendStripReservesRow and spendStripVisible for the same asymmetry stated from the
	// other side.
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
