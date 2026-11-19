package tui

// setOpenAtOldest records which end of a session the operator last jumped to, and
// persists it.
//
// Called only from goTop/goBottom — `g` and `G` — because those are the two
// gestures that already mean "take me to an end". An arrow key that happens to
// reach row 0 is navigation, not a statement of preference, and treating it as one
// would flip the setting under an operator who was simply scrolling.
//
// Persisted immediately rather than on pane exit: there is no "closing" event for
// a session view (Esc, [L], a pod switch and quitting all leave it), so waiting for
// one means the common paths never save.
func (m *model) setOpenAtOldest(oldest bool) {
	if Settings.Events.OpenAtOldest == oldest {
		return // no write for a keypress that changed nothing
	}
	Settings.Events.OpenAtOldest = oldest
	m.persistSettings()
}

// openAtRow is the row a freshly opened session starts on: the oldest event, or
// the newest.
//
// rows is the number of rows the table now holds. An empty table is row 0 either
// way, which setCursorVisible already treats as a no-op.
func openAtRow(rows int, oldest bool) int {
	if oldest || rows <= 0 {
		return 0
	}
	return rows - 1
}
