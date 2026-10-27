package tui

import "github.com/rossoctl/cortex/core/cost/usage"

// Settings is agentop's live user settings, global to the process.
//
// One struct rather than the fields it replaces (eventColumns, filter) scattered
// across the model: persisting a new setting then means adding a field here and a
// save call at the keypress that changes it, rather than finding every place the
// value is read. It is also what gets marshalled, so the file's shape and the
// in-memory shape cannot drift.
//
// Package-level because there is exactly one agentop TUI per process — bubbletea
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
// an older agentop must not disable a setting it never knew about, so the zero value
// has to mean "use the default" for each one.
type UserSettings struct {
	Events EventSettings `yaml:"events,omitempty"`
	Usage  UsageSettings `yaml:"usage,omitempty"`
	// Timeline is the Timeline pane's window.
	Timeline TimelineSettings `yaml:"timeline,omitempty"`
	// No filter: `/` searches now and a search is not saved (#1339). A `filter:` key an
	// older agentop wrote loads as an unknown key — ignored — and the next save drops it.
}

// EventSettings is the events-table view state.
type EventSettings struct {
	// Columns records only DEVIATIONS from the defaults — a column absent here takes
	// its own default, which since #1309 is visible for every column there is. Two
	// consequences, both deliberate:
	//
	// A column added in a later build shows up for everyone who already has a
	// config file, instead of starting hidden because their file predates it. And
	// the file stays legible: turn two columns off and it has two entries, not
	// thirteen.
	//
	// The alternative (a list of what is ON) reads more obviously but inverts both
	// of those, since every column is defaultOn. Note the scheme still has to
	// round-trip a default-OFF column — a future one, or BYTES again — which is why
	// the tests for that exercise a synthetic column rather than a real one.
	Columns []ColumnSetting `yaml:"columns,omitempty"`
	// SortColumn is the events-table sort column, by the same stable id the picker
	// shows as a header (#865). Empty — the zero value, and what every file written
	// before this existed carries — means CHRONOLOGICAL, so an older config restores
	// exactly today's behaviour.
	SortColumn string `yaml:"sortColumn,omitempty"`
	// SortDesc is that sort's direction. Only meaningful with SortColumn set; false
	// on its own is simply the ascending half of a sort that is not active.
	SortDesc bool `yaml:"sortDesc,omitempty"`
	// OpenAtOldest places the cursor on the OLDEST event when a session is opened,
	// instead of the newest.
	//
	// Set by `g` and cleared by `G` — the two keys that already mean "go to an end",
	// so the preference is whichever end the operator last deliberately jumped to.
	// That needs no new binding and teaches itself: press g, leave the session, come
	// back, and it opened where you were.
	//
	// False is the default, and what every file written before this carries, which is
	// the behaviour agentop has always had: a session opens at the newest event and
	// follows the tail from there.
	//
	// The ENTRY point only. Once open, tail-follow is still governed by whether the
	// cursor sits on the newest row (see rebuildEventsTable), so opening at the
	// oldest simply means the operator is not at the tail and arriving events append
	// below them instead of dragging the cursor along.
	OpenAtOldest bool `yaml:"openAtOldest,omitempty"`
}

// UsageSettings is the usage pane's view state: which metric, window and
// breakdown the operator last chose.
//
// Stored by NAME, not by the int the pane holds. usageMetric is an iota and the
// window is an index into usageWindows, so a raw number would silently change
// meaning if either list were ever reordered or had an entry inserted — the same
// trap ColumnSetting avoids by keying on the column id. A name this build does not
// recognise falls back to the default, which is also what an older file with none
// of these keys gets.
type UsageSettings struct {
	// Metric is the metric name as usageMetric.String() renders it: tokens,
	// requests, errors or latency. Empty means tokens, the zero value the pane
	// starts with.
	Metric string `yaml:"metric,omitempty"`
	// Window is the window duration as time.Duration.String() renders it (10m0s,
	// 1h0m0s, 6h0m0s). Empty, or a duration no longer offered, means the first
	// entry in usageWindows.
	Window string `yaml:"window,omitempty"`
	// Group is the breakdown, by the same string the /v1/usage group parameter
	// takes. Empty means ungrouped.
	//
	// Deliberately not enumerated here, and not validated against a local list:
	// usage.ParseGroup owns which values are legal, and it accepts more than the
	// [b] cycle visits — a hand-edited file naming one of those is honoured. A copy
	// of the set here would be a second place to update, and would have been wrong
	// already: the API gained groupings after this comment would have been written.
	Group string `yaml:"group,omitempty"`
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
// column is defaultOn since #1309, which is what makes "absent means visible" true
// of them today — but the fallback is the column's default and not a literal true,
// so the next default-off column needs no change here, exactly as in
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
		// is. BYTES was the first column with defaultOn:false and is why hardcoding true
		// here would be a trap — it would render such a column ON for every user, fresh
		// installs included, and disagree with defaultColumnSelection. BYTES is on by
		// default now (#1309), so nothing exercises that today but the tests.
		if v, ok := set[string(c.id)]; ok {
			out[c.id] = v
			continue
		}
		out[c.id] = c.defaultOn
	}
	// A file turning every column off would render a table with no columns. The
	// picker cannot produce that state (its toggle re-seeds the defaults), but a
	// hand-edited file can, and selectedColumns' fallback only rescues the render —
	// the picker's checkboxes would still show thirteen empty boxes. Re-seed here so
	// the two agree, matching what the toggle handler already does.
	if !anyColumnSelected(out) {
		return defaultColumnSelection()
	}
	return out
}

// sortSelection resolves the persisted sort into the column id the model holds.
//
// Validated against eventColumns rather than trusted, the same way columnSelection
// refuses to build a map key for a name this build does not have: a file naming a
// column that was renamed or removed — or one hand-edited to a typo — falls back to
// chronological instead of leaving the table ordered by a column that cannot be
// found to un-sort it.
//
// A column with no sortKey ("#") is refused for the same reason the keypress
// refuses it: its order already IS chronological.
func (u UserSettings) sortSelection() (eventColumnID, bool) {
	if u.Events.SortColumn == "" {
		return "", false
	}
	for _, c := range eventColumns {
		if string(c.id) == u.Events.SortColumn && c.sortKey != nil {
			return c.id, u.Events.SortDesc
		}
	}
	return "", false
}

// columnSettingsFrom is the inverse: the deviations worth writing down.
//
// Emits only the columns that differ from their own defaultOn, so an untouched
// selection serialises to nothing rather than to a `visible: true` entry per column —
// the file should record what the user changed: the default columns turned off, and
// any default-off column turned on.
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
		// unpersistable. BYTES was that column until #1309 turned it on by default;
		// the round-trip is still required, and is covered with a synthetic column.
		if sel[c.id] != c.defaultOn {
			out = append(out, ColumnSetting{Name: string(c.id), Visible: sel[c.id]})
		}
	}
	return out
}

// usageSelection resolves the persisted usage view into the three values the pane
// holds. Anything absent or unrecognised yields that field's default, so an older
// config file — or one naming a metric this build dropped — restores today's
// opening view rather than an empty chart.
func (u UserSettings) usageSelection() (metric usageMetric, windowIdx int, group usage.Group) {
	for m := usageMetric(0); m < usageMetricCount; m++ {
		if m.String() == u.Usage.Metric {
			metric = m
			break
		}
	}
	// Matched against the window's own String(), not stored as the index: an entry
	// inserted into usageWindows would otherwise silently move every saved choice.
	for i, w := range usageWindows {
		if w.window.String() == u.Usage.Window {
			windowIdx = i
			break
		}
	}
	// ParseGroup owns the valid set; its error case is exactly "not a grouping this
	// build has", which is the fallback this function promises.
	if g, err := usage.ParseGroup(u.Usage.Group); err == nil {
		group = g
	}
	return metric, windowIdx, group
}

// TimelineSettings is the Timeline pane's view. Window is a label from timelineWindows
// (5m, 1h, 24h, all); empty or unknown means the first.
type TimelineSettings struct {
	Window string `yaml:"window,omitempty"`
}

// timelineWindow is the persisted [w] choice as an index into timelineWindows, matched by
// label so a window inserted into the list does not move a saved choice.
func (u UserSettings) timelineWindow() int {
	for i, w := range timelineWindows {
		if w.label == u.Timeline.Window {
			return i
		}
	}
	return 0
}

// captureTimeline records the window, empty for the default, as captureUsage does.
func captureTimeline(windowIdx int) TimelineSettings {
	if windowIdx > 0 && windowIdx < len(timelineWindows) {
		return TimelineSettings{Window: timelineWindows[windowIdx].label}
	}
	return TimelineSettings{}
}

// captureUsage records the pane's current view for the next start.
//
// Writes the empty string for each default rather than the default's name, so a
// pane left as it opened adds nothing to the file — the same "only deviations are
// recorded" property the columns have, which keeps a file that was never
// customised empty.
func captureUsage(metric usageMetric, windowIdx int, group usage.Group) UsageSettings {
	var out UsageSettings
	if metric != usageMetric(0) {
		out.Metric = metric.String()
	}
	if windowIdx > 0 && windowIdx < len(usageWindows) {
		out.Window = usageWindows[windowIdx].window.String()
	}
	if group != "" && group != usage.GroupNone {
		out.Group = string(group)
	}
	return out
}
