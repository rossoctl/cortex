package tui

// Settings is abctl's live user settings, global to the process.
//
// One struct rather than the fields it replaces (eventColumns, filter) scattered
// across the model: persisting a new setting then means adding a field here and a
// save call at the keypress that changes it, rather than finding every place the
// value is read. It is also what gets marshalled, so the file's shape and the
// in-memory shape cannot drift.
//
// Package-level because there is exactly one abctl TUI per process — bubbletea
// owns the terminal, so a second model in the same process has nowhere to render.
// Threading this through both constructors and every handler would buy testability
// the callback in RunOptions already provides.
//
// The cost is real and worth naming: tests share it, so each one that touches
// Settings must reset it (resetSettingsForTest), and t.Parallel is unavailable in
// this package. Nothing here is parallel today, and t.Setenv in the yank tests
// already forbids it independently.
var Settings UserSettings

// UserSettings is the persisted shape. Every field is optional: a file written by
// an older abctl must not disable a setting it never knew about, so the zero value
// has to mean "use the default" for each one.
type UserSettings struct {
	Events EventSettings `yaml:"events,omitempty"`
	// Filter is the committed substring filter. One field because there is one
	// m.filter: the sessions and events panes share it (sessions_pane.go, and
	// events_pane.go's matchEventRow).
	Filter string `yaml:"filter,omitempty"`
}

// EventSettings is the events-table view state.
type EventSettings struct {
	// Columns records only DEVIATIONS from the defaults — a column absent here is
	// visible. Two consequences, both deliberate:
	//
	// A column added in a later build shows up for everyone who already has a
	// config file, instead of starting hidden because their file predates it. And
	// the file stays legible: turn two columns off and it has two entries, not
	// twelve.
	//
	// The alternative (a list of what is ON) reads more obviously but inverts both
	// of those, since every column is defaultOn today.
	Columns []ColumnSetting `yaml:"columns,omitempty"`
}

// ColumnSetting is one column's visibility, keyed by the stable id the picker
// shows as its header (#, TIME, DIR, …). Keyed by name rather than position
// because eventColumns' order is a display decision that may change; see the
// eventColumnID doc comment.
type ColumnSetting struct {
	Name    string `yaml:"name"`
	Visible bool   `yaml:"visible"`
}

// columnSelection converts the persisted deviations into the selection map the
// table renders from.
//
// Iterates eventColumns — the definition — rather than the file, so a name this
// build does not have cannot create a map key with no column behind it. That
// matters because anyColumnSelected and selectedColumns both walk the definition:
// a phantom key would make the former report a selection the latter cannot render.
//
// Absent means visible, which is the rule this whole shape exists for.
func (u UserSettings) columnSelection() map[eventColumnID]bool {
	off := make(map[string]bool, len(u.Events.Columns))
	for _, c := range u.Events.Columns {
		if !c.Visible {
			off[c.Name] = true
		}
	}
	out := make(map[eventColumnID]bool, len(eventColumns))
	for _, c := range eventColumns {
		out[c.id] = !off[string(c.id)]
	}
	// A file turning every column off would render a table with no columns. The
	// picker cannot produce that state (its toggle re-seeds the defaults), but a
	// hand-edited file can, and selectedColumns' fallback only rescues the render —
	// the picker's checkboxes would still show twelve empty boxes. Re-seed here so
	// the two agree, matching what the toggle handler already does.
	if !anyColumnSelected(out) {
		return defaultColumnSelection()
	}
	return out
}

// columnSettingsFrom is the inverse: the deviations worth writing down.
//
// Emits only the columns that are OFF, so an untouched selection serialises to
// nothing rather than to twelve `visible: true` entries — the file should record
// what the user changed.
//
// Ordered by eventColumns, not by map iteration: Go randomises the latter, which
// would rewrite the file with the same entries reshuffled on every save. That
// churn is invisible in the TUI and obvious in a diff.
func columnSettingsFrom(sel map[eventColumnID]bool) []ColumnSetting {
	var out []ColumnSetting
	for _, c := range eventColumns {
		if !sel[c.id] {
			out = append(out, ColumnSetting{Name: string(c.id), Visible: false})
		}
	}
	return out
}
