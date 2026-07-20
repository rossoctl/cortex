package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/rossoctl/rossocortex/authbridge/cmd/abctl/apiclient"
)

func TestRebuildCatalogTable_RendersEntries(t *testing.T) {
	m := newPickerModel(context.Background(), nil, nil)
	m.catalog = &apiclient.PluginCatalog{
		Plugins: []apiclient.PluginCatalogEntry{
			{Name: "alpha", Description: "First plugin"},
			{Name: "beta", Requires: []string{"alpha"}, Description: "Second"},
		},
	}
	m.rebuildCatalogTable()
	rows := m.catalogTbl.Rows()
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0][0] != "alpha" || !strings.Contains(rows[0][2], "First plugin") {
		t.Errorf("rows[0] = %+v", rows[0])
	}
	if rows[1][0] != "beta" || !strings.Contains(rows[1][1], "alpha") {
		t.Errorf("rows[1] = %+v (expected requires=alpha)", rows[1])
	}
}

func TestSelectedCatalogEntry_ReturnsCursorRow(t *testing.T) {
	m := newPickerModel(context.Background(), nil, nil)
	m.catalog = &apiclient.PluginCatalog{
		Plugins: []apiclient.PluginCatalogEntry{
			{Name: "alpha", Description: "First", Requires: []string{"x"}},
			{Name: "beta", Description: "Second"},
		},
	}
	m.rebuildCatalogTable()
	m.catalogTbl.SetCursor(1)
	got := m.selectedCatalogEntry()
	if got == nil {
		t.Fatal("expected entry, got nil")
	}
	if got.Name != "beta" {
		t.Fatalf("Name = %q, want beta", got.Name)
	}
}

func TestRebuildCatalogTable_NilCatalogClearsRows(t *testing.T) {
	m := newPickerModel(context.Background(), nil, nil)
	m.catalog = nil
	m.rebuildCatalogTable()
	if rows := m.catalogTbl.Rows(); len(rows) != 0 {
		t.Fatalf("rows should be empty when catalog nil, got %d", len(rows))
	}
}

// TestSelectedCatalogEntry_AsPipelinePlugin verifies the converter
// preserves the metadata fields the plugin-detail pane reads, so
// pressing Enter on a catalog row renders the full detail.
func TestSelectedCatalogEntry_AsPipelinePlugin(t *testing.T) {
	m := newPickerModel(context.Background(), nil, nil)
	m.catalog = &apiclient.PluginCatalog{
		Plugins: []apiclient.PluginCatalogEntry{
			{
				Name:        "ibac",
				Description: "Judge",
				Requires:    []string{"mcp-parser"},
			},
		},
	}
	m.rebuildCatalogTable()
	got := m.selectedCatalogEntry()
	if got == nil {
		t.Fatal("nil entry")
	}
	if got.Description != "Judge" {
		t.Errorf("Description = %q", got.Description)
	}
	if len(got.Requires) != 1 || got.Requires[0] != "mcp-parser" {
		t.Errorf("Requires = %v", got.Requires)
	}
	// Direction and Position deliberately blank for catalog entries —
	// showPluginDetail elides them.
	if got.Direction != "" || got.Position != 0 {
		t.Errorf("Direction/Position should be zero for catalog entry, got %q/%d", got.Direction, got.Position)
	}
}
