package tui

import (
	"slices"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

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
	// Cost is the Cost pane's view state.
	Cost CostSettings `yaml:"cost,omitempty"`
}

// CostSettings is the Cost pane's remembered view.
//
// Both fields record a DEVIATION from the pane's default, following EventSettings'
// convention: empty means "whatever the pane defaults to", not "no window". A build
// that changes the default therefore moves existing users with it, rather than
// pinning them to a choice they never made — the same reason an events column added
// in a later build shows up rather than starting hidden.
//
// Strings rather than the pane's own types, because this is a FILE format. An index
// into costPaneWindows would silently mean a different window the day that list grew,
// and a usage.Group would put a wire enum into a hand-editable file with nothing
// checking it on the way back in. costView is that check.
type CostSettings struct {
	Window string `yaml:"window,omitempty"`
	Group  string `yaml:"group,omitempty"`
}

// costView returns the remembered Cost view with anything unusable dropped.
//
// Field by field, never the whole section: a hand-edited file with one bad line is not
// a bad file, and discarding the neighbour would punish the user twice for one typo.
// A dropped field falls back to the pane's default, which is exactly what an absent
// field already means.
//
// Validated against THIS PANE'S cycles, not merely against usage.ParseGroup.
// ParseGroup accepts "status" and "plugin" — real axes, for the Usage pane — and this
// pane has no series for either, so honouring one would leave a permanently empty
// breakdown under a heading naming it. The same argument covers a window the Usage
// pane cycles and this one does not: [w] could never return to it, so the pane would
// sit on a view its own key cannot reach.
//
// Validated HERE, at the read, rather than in the loader. The loader lives in package
// main (loadUserConfig) and is shared with settings that need no validation, and a
// check there would not cover the other way a bad value arrives — Settings is a
// package-level var that any future writer can assign to.
func (u UserSettings) costView() CostSettings {
	out := u.Cost
	if out.Window != "" && !slices.Contains(costPaneWindows, out.Window) {
		out.Window = ""
	}
	if out.Group != "" {
		// ParseGroup first, so an axis no build knows is rejected by the wire vocabulary
		// rather than by a membership test that cannot explain itself.
		g, err := usage.ParseGroup(out.Group)
		if err != nil || !slices.Contains(costPaneGroups, g) {
			out.Group = ""
		}
	}
	return out
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
// Absent means "unchanged", so the column falls back to its own defaultOn. Every
// column is defaultOn today, which is what makes "absent means visible" a true
// description of the file format — but the fallback is the default, not a literal
// true, so a future defaultOn:false column behaves the same way here as it does in
// defaultColumnSelection.
func (u UserSettings) columnSelection() map[eventColumnID]bool {
	// Both directions, not just the off-list: a column whose defaultOn is false has
	// to be turnable ON by the file, or the picker could not persist enabling it.
	set := make(map[string]bool, len(u.Events.Columns))
	for _, c := range u.Events.Columns {
		set[c.Name] = c.Visible
	}
	out := make(map[eventColumnID]bool, len(eventColumns))
	for _, c := range eventColumns {
		// The fallback is c.defaultOn, not an unconditional true: "absent from the file"
		// means "I never changed this", so the answer is whatever the built-in default
		// is. The two agree today because all twelve entries are defaultOn — which is
		// exactly why hardcoding true here would be a trap. The first column added with
		// defaultOn:false would otherwise render ON for every user, fresh installs
		// included, and disagree with defaultColumnSelection.
		if v, ok := set[string(c.id)]; ok {
			out[c.id] = v
			continue
		}
		out[c.id] = c.defaultOn
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
// Emits only the columns that differ from their own defaultOn, so an untouched
// selection serialises to nothing rather than to twelve `visible: true` entries —
// the file should record what the user changed. With every column defaultOn, that
// means the off entries and nothing else.
//
// Ordered by eventColumns, not by map iteration: Go randomises the latter, which
// would rewrite the file with the same entries reshuffled on every save. That
// churn is invisible in the TUI and obvious in a diff.
func columnSettingsFrom(sel map[eventColumnID]bool) []ColumnSetting {
	var out []ColumnSetting
	for _, c := range eventColumns {
		// Compared against the column's own default, in both directions: turning ON a
		// defaultOn:false column is as much a deviation as turning off a default one,
		// and recording only the off-list would make enabling such a column
		// unpersistable. Every column is defaultOn today, so in practice this still
		// writes only the off entries.
		if sel[c.id] != c.defaultOn {
			out = append(out, ColumnSetting{Name: string(c.id), Visible: sel[c.id]})
		}
	}
	return out
}
