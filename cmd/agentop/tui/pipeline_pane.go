package tui

import (
	"fmt"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/cmd/agentop/tui/table"
)

// newPipelineTable builds the plugins table shown on the Pipeline top-level
// view. Columns are sized to match the sessions table's compact width so
// Tab-switching doesn't feel layout-jarring.
// pipelineColumns is the full-width column set, fitted to the terminal by layout().
//
// The cell COUNT here is load-bearing beyond width: rebuildPipelineTable writes its divider as
// a six-cell literal and bubbles' renderRow indexes columns by cell position, so adding a
// column without adding a cell there panics. Fitting changes widths only, never the count,
// which is what makes it safe to re-apply on every resize.
func pipelineColumns() []table.Column {
	return []table.Column{
		{Title: "#", Width: 3},
		{Title: "DIRECTION", Width: 10},
		{Title: "PLUGIN", Width: 22},
		{Title: "DEPS", Width: 5},
		{Title: "BODY", Width: 6},
		{Title: "EVENTS", Width: 8},
	}
}

func newPipelineTable() table.Model {
	t := table.New(
		table.WithColumns(pipelineColumns()),
		table.WithFocused(true),
	)
	t.SetStyles(tableStyles())
	return t
}

// rebuildPipelineTable renders the plugin list with a "(app)" divider row
// between inbound and outbound.
func (m *model) rebuildPipelineTable() {
	if m.pipeline == nil {
		m.pipelineTbl.SetRows(nil)
		return
	}
	counts := m.pluginRuns // per-plugin run counts, folded at stream arrival — see model.pluginRuns

	rows := make([]table.Row, 0, len(m.pipeline.Inbound)+len(m.pipeline.Outbound)+1)
	for _, p := range m.pipeline.Inbound {
		rows = append(rows, pipelineRow(p, counts[p.Name], m.pipeline.Inbound))
	}
	// Divider between inbound and outbound. Cell count MUST match the 6
	// columns defined in newPipelineTable (#, DIRECTION, PLUGIN, DEPS, BODY,
	// EVENTS) — bubbles' table.renderRow indexes columns by cell position and
	// panics on a mismatch.
	rows = append(rows, table.Row{"", "", "── (app) ──", "", "", ""})
	for _, p := range m.pipeline.Outbound {
		rows = append(rows, pipelineRow(p, counts[p.Name], m.pipeline.Outbound))
	}
	m.pipelineTbl.SetRows(rows)
	// If cursor is on the divider row, nudge to the next plugin. A relative move so
	// the viewport offset stays reconciled — see setCursorVisible.
	if isDividerRow(rows, m.pipelineTbl.Cursor()) {
		m.pipelineTbl.MoveDown(1)
	}
}

func pipelineRow(p apiclient.PipelinePlugin, events int, chain []apiclient.PipelinePlugin) table.Row {
	body := "no"
	if p.ReadsBody {
		body = "yes"
	}
	eventsStr := ""
	if events > 0 {
		eventsStr = fmt.Sprintf("%d", events)
	}
	// DEPS column: ✓ when all declared dependencies are met, ✗ when any
	// fail, blank when the plugin declares no Requires/RequiresAny.
	// Blank vs ✓ avoids a misleading "looks fine" mark on plugins that
	// have nothing to verify in the first place.
	deps := ""
	if pluginHasAnyDeps(&p) {
		if pluginDepsAllSatisfied(&p, chain) {
			deps = "✓"
		} else {
			deps = "✗"
		}
	}
	return table.Row{
		fmt.Sprintf("%d", p.Position),
		p.Direction,
		p.Name,
		deps,
		body,
		eventsStr,
	}
}

func isDividerRow(rows []table.Row, i int) bool {
	if i < 0 || i >= len(rows) {
		return false
	}
	return rows[i][2] == "── (app) ──"
}

// selectedPlugin returns the PipelinePlugin under the cursor, or nil when
// the cursor sits on the divider or the table is empty.
func (m *model) selectedPlugin() *apiclient.PipelinePlugin {
	if m.pipeline == nil {
		return nil
	}
	rows := m.pipelineTbl.Rows()
	i := m.pipelineTbl.Cursor()
	if i < 0 || i >= len(rows) {
		return nil
	}
	if isDividerRow(rows, i) {
		return nil
	}
	// Rows are inbound, divider, outbound. Map the table index back to the
	// source slices by name rather than arithmetic — safer against future
	// divider changes.
	name := rows[i][2]
	for j := range m.pipeline.Inbound {
		if m.pipeline.Inbound[j].Name == name {
			return &m.pipeline.Inbound[j]
		}
	}
	for j := range m.pipeline.Outbound {
		if m.pipeline.Outbound[j].Name == name {
			return &m.pipeline.Outbound[j]
		}
	}
	return nil
}

// unmetDepsCount returns how many active plugins have at least one
// unsatisfied Requires/RequiresAny/After dependency. Pulls from the
// active pipeline view so a hot-reload that lands a new chain
// immediately reflects in the count.
func (m *model) unmetDepsCount() int {
	if m.pipeline == nil {
		return 0
	}
	n := 0
	for i := range m.pipeline.Inbound {
		p := &m.pipeline.Inbound[i]
		if pluginHasAnyDeps(p) && !pluginDepsAllSatisfied(p, m.pipeline.Inbound) {
			n++
		}
	}
	for i := range m.pipeline.Outbound {
		p := &m.pipeline.Outbound[i]
		if pluginHasAnyDeps(p) && !pluginDepsAllSatisfied(p, m.pipeline.Outbound) {
			n++
		}
	}
	return n
}
