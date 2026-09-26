package tui

import (
	"context"
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/cmd/abctl/apiclient"
	"github.com/rossoctl/cortex/cmd/abctl/cluster"
	"github.com/rossoctl/cortex/cmd/abctl/edit"
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

// localConnectedMsg carries the result of probing localEndpoint for the
// `[l]` shortcut. client is non-nil only when the probe succeeded.
type localConnectedMsg struct {
	client   *apiclient.Client
	endpoint string
	err      error
}

// connectLocalCmd probes localEndpoint's /v1/sessions and, on success,
// hands back a ready client. The probe matters because there's no
// port-forward subprocess to fail loudly here — without it, a wrong
// guess would drop the operator into a silently empty session view.
func connectLocalCmd(ctx context.Context, endpoint string) tea.Cmd {
	return func() tea.Msg {
		c := apiclient.New(endpoint)
		probeCtx, cancel := context.WithTimeout(ctx, localProbeTimeout)
		defer cancel()
		if _, err := c.ListSessions(probeCtx); err != nil {
			return localConnectedMsg{err: err}
		}
		return localConnectedMsg{client: c, endpoint: endpoint}
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
	// Seed the input, not just m.filter: the filter box renders only while filtering,
	// so a restored filter was applied invisibly — the list came back truncated with
	// nothing on screen saying why. Worse, `/` then one character replaced the saved
	// filter with that character, and `/` then Esc persisted an empty one, discarding
	// it for good.
	ti.SetValue(Settings.Filter)

	// Resolved once, as in New: see the note there.
	sortCol, sortDesc := Settings.sortSelection()

	// The usage pane's view, restored once at startup rather than in openUsage:
	// re-entering the pane must not reset a choice made during the session.
	usageMetric, usageWindowIdx, usageGroup := Settings.usageSelection()

	return &model{
		// endpoint and client are set later, when portForwardReadyMsg arrives.
		parentCtx: parentCtx,
		ctx:       ctx,
		cancel:    cancel,
		events:    make(map[string][]pipeline.SessionEvent),
		pane:      paneNamespaces,
		// Seeded here as well as in New: this constructor's doc comment promises it
		// mirrors New's field initialization, and the events table it reaches after a
		// port-forward reads both of these.
		eventColumns: Settings.columnSelection(),
		sortCol:      sortCol,
		sortDesc:     sortDesc,
		usage:        usageState{metric: usageMetric, windowIdx: usageWindowIdx, group: usageGroup},
		filter:       Settings.Filter,
		sessionsData: loadSessionMetadataForModel(),
		sessionsTbl:  newSessionsTable(),
		eventsTbl:    newEventsTable(),
		pipelineTbl:  newPipelineTable(),
		catalogTbl:   newCatalogTable(),
		previousPane: paneNone,
		// Mirrors New, per this constructor's doc comment. paneNone rather than
		// the zero value, which is paneNamespaces — see New.
		pipelineReturnPane: paneNone,
		detailVp:           viewport.New(0, 0),
		filterInput:        ti,
		lastTick:           time.Now(),
		connState:          connStateInfo{phase: connConnecting},

		// Picker-only:
		lister:        lister,
		portForwarder: pf,
		namespacesTbl: newNamespacesTable(),
		podsTbl:       newPodsTable(),

		// Edit flow:
		editRunner: edit.DefaultRunner,
	}
}
