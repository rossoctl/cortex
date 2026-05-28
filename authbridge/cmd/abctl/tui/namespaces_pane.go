package tui

import (
	"context"
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/cmd/abctl/cluster"
)

// newNamespacesTable builds an empty namespaces picker table.
func newNamespacesTable() table.Model {
	t := table.New(
		table.WithColumns([]table.Column{
			{Title: "NAMESPACE", Width: 30},
			{Title: "PODS", Width: 6},
		}),
		table.WithFocused(true),
	)
	t.SetStyles(tableStyles())
	return t
}

// rebuildNamespacesTable rebuilds rows from m.namespaces.
func (m *model) rebuildNamespacesTable() {
	rows := make([]table.Row, 0, len(m.namespaces))
	for _, ns := range m.namespaces {
		rows = append(rows, table.Row{ns.Name, fmt.Sprintf("%d", len(ns.Pods))})
	}
	m.namespacesTbl.SetRows(rows)
}

// loadAgentsCmd produces a tea.Cmd that calls Lister.ListAgents and
// emits an agentsLoadedMsg.
func loadAgentsCmd(ctx context.Context, lister cluster.Lister) tea.Cmd {
	return func() tea.Msg {
		ns, err := lister.ListAgents(ctx)
		return agentsLoadedMsg{namespaces: ns, err: err}
	}
}

// newPickerModel constructs a model already in the Namespaces pane,
// wired with the given Lister and PortForwarder. Used when --endpoint
// is not given. Mirrors the field initialization in New() so that
// transitioning to paneSessions after a port-forward is established
// finds all fields ready.
func newPickerModel(ctx context.Context, lister cluster.Lister, pf cluster.PortForwarder) *model {
	parentCtx := ctx
	ctx, cancel := context.WithCancel(ctx)

	ti := textinput.New()
	ti.Placeholder = "filter…"
	ti.Prompt = "/ "

	return &model{
		// endpoint and client are set later, when portForwardReadyMsg arrives.
		parentCtx:   parentCtx,
		ctx:         ctx,
		cancel:      cancel,
		events:      make(map[string][]pipeline.SessionEvent),
		pane:        paneNamespaces,
		sessionsTbl: newSessionsTable(),
		eventsTbl:   newEventsTable(),
		pipelineTbl: newPipelineTable(),
		detailVp:    viewport.New(0, 0),
		filterInput: ti,
		lastTick:    time.Now(),
		connState:   connStateInfo{phase: connConnecting},

		// Picker-only:
		lister:        lister,
		portForwarder: pf,
		namespacesTbl: newNamespacesTable(),
		podsTbl:       newPodsTable(),
	}
}
