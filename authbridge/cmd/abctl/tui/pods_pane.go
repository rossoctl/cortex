package tui

import (
	"context"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/rossocortex/authbridge/cmd/abctl/cluster"
)

// newPodsTable builds an empty pods picker table.
func newPodsTable() table.Model {
	t := table.New(
		table.WithColumns([]table.Column{
			{Title: "POD", Width: 40},
			{Title: "PHASE", Width: 10},
			{Title: "READY", Width: 6},
		}),
		table.WithFocused(true),
	)
	t.SetStyles(tableStyles())
	return t
}

// rebuildPodsTable rebuilds rows from m.namespaces[selected].Pods.
func (m *model) rebuildPodsTable() {
	var pods []cluster.Pod
	for _, ns := range m.namespaces {
		if ns.Name == m.selectedNamespace {
			pods = ns.Pods
			break
		}
	}
	rows := make([]table.Row, 0, len(pods))
	for _, p := range pods {
		ready := "no"
		if p.Ready {
			ready = "yes"
		}
		rows = append(rows, table.Row{p.Name, p.Phase, ready})
	}
	m.podsTbl.SetRows(rows)
}

// currentPodsList returns the slice of pods backing the Pods pane,
// keyed by the currently-selected namespace. Used for selection lookup.
func (m *model) currentPodsList() []cluster.Pod {
	for _, ns := range m.namespaces {
		if ns.Name == m.selectedNamespace {
			return ns.Pods
		}
	}
	return nil
}

// startPortForwardCmd produces a Cmd that calls PortForwarder.Start and
// emits a portForwardReadyMsg. ctx bounds the readiness-wait only; the
// kubectl port-forward subprocess uses exec.Command (not CommandContext)
// so it survives ctx cancellation and is terminated explicitly via
// activePF.Close(). Threading the picker ctx in keeps quit responsive
// during the readiness window.
func startPortForwardCmd(ctx context.Context, pf cluster.PortForwarder, ns, pod string) tea.Cmd {
	return func() tea.Msg {
		conn, err := pf.Start(ctx, ns, pod)
		if err != nil {
			return portForwardReadyMsg{err: err}
		}
		return portForwardReadyMsg{pf: conn, endpoint: conn.Endpoint()}
	}
}
