package tui

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
)

// availableModel is a pipeline that runs mcp-parser, and a catalog that also offers ibac.
func availableModel() *model {
	m := newPickerModel(context.Background(), nil, nil)
	m.client = deadClient()
	m.pipeline = &apiclient.PipelineView{Outbound: []apiclient.PipelinePlugin{
		{Name: "mcp-parser", Direction: "outbound", Position: 1},
	}}
	m.catalog = &apiclient.PluginCatalog{Plugins: []apiclient.PluginCatalogEntry{
		{Name: "mcp-parser", Description: "Parses MCP"},
		{Name: "ibac", Description: "Judge", Requires: []string{"mcp-parser"}, ReadsBody: true},
	}}
	m.rebuildPipelineTable()
	return m
}

// The catalog's plugins that the chain does not run follow an "available" divider; a plugin
// that runs is listed once, in the chain.
func TestPipelinePane_ListsAvailablePluginsAfterTheChain(t *testing.T) {
	m := availableModel()
	rows := m.pipelineTbl.Rows()
	// (app) divider, mcp-parser, available divider, ibac.
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4: %v", len(rows), rows)
	}
	if !isDividerRow(rows, 2) || rows[2][2] != availableDivider {
		t.Errorf("rows[2] = %v, want the available divider", rows[2])
	}
	if rows[3][2] != "ibac" || rows[3][4] != "yes" {
		t.Errorf("rows[3] = %v, want ibac with BODY yes", rows[3])
	}
	for _, p := range m.availablePlugins() {
		if p.Name == "mcp-parser" {
			t.Error("a plugin the chain runs is also listed as available")
		}
	}
}

// No catalog yet: no divider and no available rows.
func TestPipelinePane_NoCatalogNoAvailableSection(t *testing.T) {
	m := availableModel()
	m.catalog = nil
	m.rebuildPipelineTable()
	for i := range m.pipelineTbl.Rows() {
		if m.pipelineTbl.Rows()[i][2] == availableDivider {
			t.Fatal("the available divider shows with no catalog")
		}
	}
}

// An available row's plugin carries the catalog's metadata, with no Direction or Position.
func TestSelectedPlugin_AvailableRowIsACatalogEntry(t *testing.T) {
	m := availableModel()
	m.pipelineTbl.SetCursor(3)
	got := m.selectedPlugin()
	if got == nil || got.Name != "ibac" {
		t.Fatalf("selectedPlugin = %+v, want ibac", got)
	}
	if got.Description != "Judge" || len(got.Requires) != 1 || got.Requires[0] != "mcp-parser" {
		t.Errorf("metadata lost: %+v", got)
	}
	if got.Direction != "" || got.Position != 0 {
		t.Errorf("Direction/Position = %q/%d, want blank for an available row", got.Direction, got.Position)
	}
}

// Enter on an available row opens the plugin detail, and esc comes back to the pipeline.
func TestPipelinePane_EnterOnAvailableRowOpensDetail(t *testing.T) {
	m := availableModel()
	m.pane = panePipeline
	m.pipelineTbl.SetCursor(3)
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.pane != panePluginDetail {
		t.Fatalf("Enter on an available row landed on %v, want panePluginDetail", m.pane)
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.pane != panePipeline {
		t.Errorf("esc from the plugin detail landed on %v, want panePipeline", m.pane)
	}
}
