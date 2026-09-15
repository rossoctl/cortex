package tui

import "github.com/charmbracelet/bubbles/table"

// setCursorVisible moves a table's cursor to row n, leaves it ON SCREEN, and
// scrolls the pane as little as it can to get there — including not at all when
// the cursor is already on row n.
//
// Use this instead of table.SetCursor everywhere a cursor is placed
// programmatically — following a tail, restoring a saved position, jumping to an
// end. SetCursor alone loses the highlight for any row at or past one screenful.
//
// WHY: bubbles' UpdateViewport renders a WINDOW of rows around the cursor —
// start = clamp(cursor−height, 0, cursor) — and the viewport then shows `height`
// lines of that window beginning at its own YOffset. SetCursor recomputes the
// window but never reconciles YOffset, and only MoveUp/MoveDown do. So with
// YOffset 0 and cursor ≥ height, start lands on cursor−height and the cursor sits
// at window line `height`: exactly one line past the last visible one. The row is
// rendered, the highlight is drawn on it, and it is off the bottom edge.
//
// So the move has to be expressed as RELATIVE movement, the only cursor API in
// bubbles that maintains the offset. Which relative move is not a free choice —
// each of the three has a range it is correct over:
//
//   - Not moving at all is the one that matters most. The panes that call this
//     restore their cursor on a two-second poll, so on a live session the common
//     case by far is "the cursor is already on row n". Returning here is what
//     leaves the operator's scroll position alone; every other branch re-anchors
//     the window. That re-anchoring is the reported bug: read up three rows from
//     the tail, and two seconds later the poll slid those three rows back off
//     the bottom edge.
//   - MoveDown(n − cursor) is safe for any distance. Whatever branch of its
//     switch fires, the cursor ends on or near the last visible line.
//   - MoveUp(cursor − n) is safe only while n ≥ height. It sets
//     YOffset = clamp(YOffset+distance, 1, height) — capped at height, and the
//     cursor's line within the new window is min(n, height) — so for n ≥ height
//     the cursor is always inside [YOffset, YOffset+height). Below that the cap
//     no longer covers the distance: MoveUp(38) into row 1 of a 40-row list
//     leaves YOffset at height with the cursor on line 1, and the pane renders a
//     window that begins ten lines BELOW the row it highlights.
//
// GotoTop is the fallback for that last case: it is a MoveUp to row 0, where the
// window is exactly one screenful and the viewport's own clamp pulls YOffset back
// to 0. One MoveDown then walks to a target inside the first screenful without
// scrolling at all. Distances, not loops — two O(height) re-renders at worst,
// however far the cursor travels.
//
// n is clamped to the row range, as SetCursor's own is. An empty table is left
// alone rather than parked on a row that does not exist.
func setCursorVisible(t *table.Model, n int) {
	rows := len(t.Rows())
	if rows == 0 {
		return
	}
	if n < 0 {
		n = 0
	}
	if n > rows-1 {
		n = rows - 1
	}
	// A table emptied by SetRows(nil) parks its cursor at −1, which is not a row
	// and cannot be moved relative to; GotoTop below is what normalizes it.
	switch cur := t.Cursor(); {
	case cur == n:
		return
	case cur >= 0 && n > cur:
		t.MoveDown(n - cur)
	case cur >= 0 && n >= t.Height():
		t.MoveUp(cur - n)
	default:
		t.GotoTop()
		if n > 0 {
			t.MoveDown(n)
		}
	}
}

// setTableHeight resizes a table and, when the height actually changed, rebuilds
// its scroll offset around the cursor.
//
// Use this instead of table.SetHeight. SetHeight re-windows the rendered rows for
// the new height — start = cursor − height moves — while the viewport keeps the
// offset it computed for the OLD height, and nothing in bubbles reconciles the two.
// Shrink a pane whose offset had been walked up by arrow keys and the window it
// renders can end up entirely BELOW the row it highlights: 40 rows, three arrows up
// from the tail, height 11 → 3, and the pane shows rows 37..38 with row 36 selected.
//
// This used to be covered by accident. setCursorVisible re-anchored on every call, so
// the two-second poll behind each pane's rebuild scrubbed the stale offset within a
// tick or two — at the cost of also scrubbing the operator's scroll position, which
// was the reported bug. Now that a restore to the row the cursor is already on is a
// no-op, a height change has to say so itself, at the point it happens rather than a
// poll later: GotoTop is the one state whose offset is knowable, and the cursor then walks
// back to where it was.
func setTableHeight(t *table.Model, h int) {
	was := t.Height()
	t.SetHeight(h)
	if t.Height() == was {
		return
	}
	// An empty table keeps the new height and nothing else. setCursorVisible promises
	// to leave one alone rather than park it on a row that does not exist, and the
	// GotoTop below would break that promise on this path: table's clamp is
	// min(max(v, low), high) with no swap for inverted bounds, so on a table with no
	// rows GotoTop resolves clamp(0, 0, -1) to −1 and moves a fresh table's cursor off
	// row 0. (viewport's clamp DOES swap, which is the confusing part — different
	// package, and not the one MoveUp uses.) Harmless in itself, since neither index
	// addresses a row and rebuildEventsTable's wasAtEnd reads both the same way, but
	// the invariant is worth keeping true package-wide.
	if len(t.Rows()) == 0 {
		return
	}
	n := t.Cursor()
	t.GotoTop()
	setCursorVisible(t, n)
}
