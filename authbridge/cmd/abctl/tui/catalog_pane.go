package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/table"

	"github.com/rossoctl/rossocortex/authbridge/cmd/abctl/apiclient"
)

// newCatalogTable builds the registered-plugin catalog table. Same
// styling as the pipeline table for visual consistency when switching
// between them via the `P` keybind.
func newCatalogTable() table.Model {
	t := table.New(
		table.WithColumns([]table.Column{
			{Title: "NAME", Width: 22},
			{Title: "REQUIRES", Width: 28},
			{Title: "DESCRIPTION", Width: 60},
		}),
		table.WithFocused(true),
	)
	t.SetStyles(tableStyles())
	return t
}

// rebuildCatalogTable populates the catalog table from m.catalog.
// Empty rows when the catalog hasn't loaded yet — caller renders
// "(loading…)" in the title.
func (m *model) rebuildCatalogTable() {
	if m.catalog == nil {
		m.catalogTbl.SetRows(nil)
		return
	}
	rows := make([]table.Row, 0, len(m.catalog.Plugins))
	for _, e := range m.catalog.Plugins {
		// Combine Requires and RequiresAny (the "any" group joined by " | ")
		// so operators see the full dependency picture in one column.
		reqs := []string{}
		reqs = append(reqs, e.Requires...)
		if len(e.RequiresAny) > 0 {
			reqs = append(reqs, strings.Join(e.RequiresAny, "|"))
		}
		rows = append(rows, table.Row{
			e.Name,
			strings.Join(reqs, ", "),
			e.Description,
		})
	}
	m.catalogTbl.SetRows(rows)
}

// selectedCatalogEntry returns the catalog entry under the cursor as
// a synthetic PipelinePlugin so showPluginDetail can render it. Direction
// is left blank and Position is 0 — showPluginDetail elides those fields
// when empty so the detail view degrades gracefully for catalog entries.
func (m *model) selectedCatalogEntry() *apiclient.PipelinePlugin {
	if m.catalog == nil {
		return nil
	}
	rows := m.catalogTbl.Rows()
	i := m.catalogTbl.Cursor()
	if i < 0 || i >= len(rows) {
		return nil
	}
	name := rows[i][0]
	for _, e := range m.catalog.Plugins {
		if e.Name == name {
			p := apiclient.PipelinePlugin{
				Name:        e.Name,
				ReadsBody:   e.ReadsBody,
				Requires:    e.Requires,
				RequiresAny: e.RequiresAny,
				Description: e.Description,
			}
			return &p
		}
	}
	return nil
}
