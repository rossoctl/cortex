package tui

import (
	"testing"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/apiclient"
)

// TestRebuildPipelineTable_AllRowsMatchColumnCount is a regression guard:
// every row added to the pipeline table — including the inbound/outbound
// "(app)" divider — must have exactly as many cells as the table has columns.
// The divider previously carried 7 cells against 6 columns, which made
// bubbles' table.renderRow panic ("index out of range [6] with length 6")
// the moment abctl rendered any pipeline.
func TestRebuildPipelineTable_AllRowsMatchColumnCount(t *testing.T) {
	m := &model{
		pipeline: &apiclient.PipelineView{
			Inbound:  []apiclient.PipelinePlugin{{Name: "jwt-validation", Direction: "inbound", Position: 1}},
			Outbound: []apiclient.PipelinePlugin{{Name: "token-exchange", Direction: "outbound", Position: 1}},
		},
		pipelineTbl: newPipelineTable(),
	}

	// Must not panic: the buggy 7-cell divider panicked here via
	// SetRows → UpdateViewport → renderRow.
	m.rebuildPipelineTable()

	rows := m.pipelineTbl.Rows()
	if len(rows) != 3 { // inbound plugin + divider + outbound plugin
		t.Fatalf("rows = %d, want 3 (inbound + divider + outbound)", len(rows))
	}
	const wantCells = 6 // #, DIRECTION, PLUGIN, DEPS, BODY, EVENTS
	for i, r := range rows {
		if len(r) != wantCells {
			t.Errorf("row %d has %d cells, want %d (must match column count)", i, len(r), wantCells)
		}
	}
}
