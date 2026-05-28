package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/table"

	"github.com/kagenti/kagenti-extensions/authbridge/cmd/abctl/apiclient"
)

// newPipelineTable builds the plugins table shown on the Pipeline top-level
// view. Columns are sized to match the sessions table's compact width so
// Tab-switching doesn't feel layout-jarring.
func newPipelineTable() table.Model {
	t := table.New(
		table.WithColumns([]table.Column{
			{Title: "#", Width: 3},
			{Title: "DIRECTION", Width: 10},
			{Title: "PLUGIN", Width: 22},
			{Title: "WRITES", Width: 18},
			{Title: "BODY", Width: 6},
			{Title: "EVENTS", Width: 8},
		}),
		table.WithFocused(true),
	)
	t.SetStyles(tableStyles())
	return t
}

// rebuildPipelineTable renders the plugin list with a "(app)" divider row
// between inbound and outbound. eventsPerPlugin counts how many events in
// the cached data came from each plugin (by matching the event's written
// extension against the plugin's Writes).
func (m *model) rebuildPipelineTable() {
	if m.pipeline == nil {
		m.pipelineTbl.SetRows(nil)
		return
	}
	counts := m.countEventsPerPlugin()

	rows := make([]table.Row, 0, len(m.pipeline.Inbound)+len(m.pipeline.Outbound)+1)
	for _, p := range m.pipeline.Inbound {
		rows = append(rows, pipelineRow(p, counts[p.Name]))
	}
	// Divider between inbound and outbound.
	rows = append(rows, table.Row{"", "", "── (app) ──", "", "", ""})
	for _, p := range m.pipeline.Outbound {
		rows = append(rows, pipelineRow(p, counts[p.Name]))
	}
	m.pipelineTbl.SetRows(rows)
	// If cursor is on the divider row, nudge to the next plugin.
	if isDividerRow(rows, m.pipelineTbl.Cursor()) {
		m.pipelineTbl.SetCursor(m.pipelineTbl.Cursor() + 1)
	}
}

func pipelineRow(p apiclient.PipelinePlugin, events int) table.Row {
	body := "no"
	if p.BodyAccess {
		body = "yes"
	}
	eventsStr := ""
	if events > 0 {
		eventsStr = fmt.Sprintf("%d", events)
	}
	// Plugin names used to be colored by protocol but bubbles v1's
	// runewidth.Truncate miscounts ANSI escape bytes as visible width,
	// which truncated the closing \x1b[0m reset for longer names and
	// bled color into adjacent cells. Blocked on bubbles v2 upgrade.
	return table.Row{
		fmt.Sprintf("%d", p.Position),
		p.Direction,
		p.Name,
		strings.Join(p.Writes, ","),
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

// countEventsPerPlugin counts how many times each plugin actually ran
// across all cached events, by walking every event's Invocations list.
// This includes auth-gate plugins (jwt-validation, token-exchange, ibac)
// that don't write extension slots — they all show up in Invocations
// when they ran, so the pipeline view's per-plugin counts match what
// the events pane shows row-by-row.
func (m *model) countEventsPerPlugin() map[string]int {
	counts := map[string]int{}
	for _, events := range m.events {
		for _, e := range events {
			if e.Invocations == nil {
				continue
			}
			for _, inv := range e.Invocations.Inbound {
				counts[inv.Plugin]++
			}
			for _, inv := range e.Invocations.Outbound {
				counts[inv.Plugin]++
			}
		}
	}
	return counts
}

