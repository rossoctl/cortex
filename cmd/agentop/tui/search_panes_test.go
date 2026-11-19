package tui

import (
	"context"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/cmd/agentop/cluster"
	"github.com/rossoctl/cortex/cmd/agentop/tui/table"
)

func newPanesModel(t *testing.T) *model {
	t.Helper()
	resetSettingsForTest(t)
	m := newPickerModel(context.Background(), nil, nil)
	m.width, m.height = 160, 40
	m.namespaces = []cluster.AgentNamespace{
		{Name: "team1", Pods: []cluster.Pod{{Name: "weather-agent-7d9", Phase: "Running", Ready: true}}},
		{Name: "team2"},
	}
	m.selectedNamespace = "team1"
	m.pipeline = &apiclient.PipelineView{
		Inbound:  []apiclient.PipelinePlugin{{Name: "jwt-validation", Direction: "inbound", Position: 0}},
		Outbound: []apiclient.PipelinePlugin{{Name: "token-exchange", Direction: "outbound", Position: 0, ReadsBody: true}},
	}
	m.catalog = &apiclient.PluginCatalog{Plugins: []apiclient.PluginCatalogEntry{
		{Name: "alpha", Description: "First plugin"},
		{Name: "beta", Requires: []string{"alpha"}, Description: "Second"},
	}}
	m.agents = []agentRow{{label: "claude-code/2.1"}, {label: "opencode/1.0"}}
	m.layout()
	m.rebuildNamespacesTable()
	m.rebuildPodsTable()
	m.rebuildPipelineTable()
	m.rebuildAgentsTable()
	return m
}

func TestSearch_EveryCellOfEveryOtherTableIsSearchable(t *testing.T) {
	m := newPanesModel(t)
	for _, tc := range []struct {
		pane paneID
		tbl  *table.Model
	}{
		{paneNamespaces, &m.namespacesTbl},
		{panePods, &m.podsTbl},
		{panePipeline, &m.pipelineTbl},
		{paneAgents, &m.agentsTbl},
	} {
		t.Run(paneName(tc.pane), func(t *testing.T) {
			assertEveryCellSearchable(t, tc.tbl, func(q string) {
				m.search = map[paneID]string{tc.pane: q}
				m.surface(tc.pane).redraw()
			})
		})
	}
}

// TestSearch_SlashOnThePickers: the pickers handle their keys before anything else did,
// so `/` never reached the search there, and q quit agentop mid-query.
func TestSearch_SlashOnThePickers(t *testing.T) {
	for _, p := range []paneID{paneNamespaces, panePods} {
		m := newPanesModel(t)
		m.pane = p
		typeKeys(m, "/team1q")
		if !m.searching || m.searchInput.Value() != "team1q" {
			t.Fatalf("pane %v: searching=%v value=%q, want the prompt holding team1q", p, m.searching, m.searchInput.Value())
		}
		press(m, tea.KeyBackspace)
		press(m, tea.KeyEnter)
		if got := m.search[p]; got != "team1" {
			t.Errorf("pane %v: committed %q, want team1", p, got)
		}
		if !strings.Contains(stripANSI(m.View()), "[/team1") {
			t.Errorf("pane %v: no search status on screen:\n%s", p, stripANSI(m.View()))
		}
	}
}

// TestSearch_OnAPaneWithNoRows: the pipeline before it loads has no table rows at all.
func TestSearch_OnAPaneWithNoRows(t *testing.T) {
	m := newPanesModel(t)
	m.pipeline = nil
	m.rebuildPipelineTable()
	m.pane = panePipeline
	typeKeys(m, "/x")
	if st := m.searchStatus(panePipeline); st != "[/x: no match]" {
		t.Errorf("status = %q, want [/x: no match]", st)
	}
	press(m, tea.KeyEnter)
	m.Update(keyRune('n'))
	m.Update(keyRune('N'))
	typeKeys(m, "/y")
	press(m, tea.KeyEsc)
	_ = m.View()
}

// TestSearch_ThePaneChangesUnderTheOpenPrompt: a port-forward finishing moves the picker
// to the sessions pane while the operator is typing. The keys still go to the prompt, and
// the query is committed to the pane it was typed on.
func TestSearch_ThePaneChangesUnderTheOpenPrompt(t *testing.T) {
	m := newPanesModel(t)
	m.pane = panePods
	typeKeys(m, "/wea")
	m.pane = paneSessions
	typeKeys(m, "therq")
	if !m.searching || m.searchInput.Value() != "weatherq" {
		t.Fatalf("searching=%v value=%q, want the prompt holding weatherq", m.searching, m.searchInput.Value())
	}
	press(m, tea.KeyEnter)
	if m.search[panePods] != "weatherq" || m.search[paneSessions] != "" {
		t.Errorf("search = %v, want weatherq on pods only", m.search)
	}
}

func TestSearch_NextAndPreviousOnAnotherTable(t *testing.T) {
	m := newPanesModel(t)
	m.pane = panePipeline
	typeKeys(m, "/alpha")
	press(m, tea.KeyEnter)
	// Rows: jwt-validation, (app), token-exchange, available, alpha, beta.
	if c := m.pipelineTbl.Cursor(); c != 4 {
		t.Fatalf("cursor = %d, want 4 (the available row alpha)", c)
	}
	m.Update(keyRune('n'))
	if c := m.pipelineTbl.Cursor(); c != 4 {
		t.Errorf("n with one match moved the cursor to %d", c)
	}
	if got := m.pipelineTbl.Matches(); !slices.Equal(got, []int{4}) {
		t.Errorf("Matches = %v, want [4]", got)
	}
}

// TestSearch_SlashOpensOnEveryPane walks every pane, as TestPaneKeysCoverAllPanes does, so a
// pane added later cannot ship without a search.
func TestSearch_SlashOpensOnEveryPane(t *testing.T) {
	m := newPanesModel(t)
	for p := paneID(0); p <= lastPaneID; p++ {
		m.pane = p
		if m.surface(p) == nil {
			t.Errorf("pane %v has no search surface", p)
			continue
		}
		m.Update(keyRune('/'))
		if !m.searching || m.searchPane != p {
			t.Errorf("pane %v: `/` did not open the prompt on it", p)
		}
		press(m, tea.KeyEsc)
	}
	m.pane = paneSessions
	m.Update(keyRune('?'))
	m.Update(keyRune('/'))
	if !m.searching || m.searchPane != targetHelp {
		t.Error("help overlay: `/` did not open the prompt on it")
	}
}

// TestHelp_SearchIsAnAnywhereKey: every pane searches, so the overlay names `/` once, with
// the keys that work everywhere, and no pane names it again.
func TestHelp_SearchIsAnAnywhereKey(t *testing.T) {
	has := func(g keyGroup, key string) bool {
		for _, b := range g.bindings {
			if b.keys == key {
				return true
			}
		}
		return false
	}
	if !has(anywhereKeys, "/") || !has(anywhereKeys, "n / N") {
		t.Error("anywhereKeys does not name / and n / N")
	}
	for p, g := range paneKeys {
		if has(g, "/") || has(g, "n / N") {
			t.Errorf("pane %v names / or n / N itself", p)
		}
	}
}

// TestSearch_LeavingTheConnectionClearsItsSearches: a search on a pane that shows the
// connection's data goes when the connection does — and off the table too, which keeps its own
// copy of the query: a map entry deleted under it left the old highlights on screen and n moving
// between them. The pickers list the cluster, not the connection, and help's search lasts until
// agentop exits, so those three stay.
func TestSearch_LeavingTheConnectionClearsItsSearches(t *testing.T) {
	m := newPanesModel(t)
	for p, q := range map[paneID]string{panePipeline: "jwt", paneAgents: "claude", panePods: "weather"} {
		m.pane = p
		typeKeys(m, "/"+q)
		press(m, tea.KeyEnter)
	}
	m.Update(keyRune('?'))
	typeKeys(m, "/quit")
	press(m, tea.KeyEnter)
	m.Update(keyRune('?'))
	if len(m.pipelineTbl.Matches()) == 0 || len(m.podsTbl.Matches()) == 0 {
		t.Fatal("setup: the searches did not match")
	}

	m.backToPodsPane()
	for p, tbl := range map[paneID]*table.Model{panePipeline: &m.pipelineTbl, paneAgents: &m.agentsTbl} {
		if m.search[p] != "" || len(tbl.Matches()) != 0 {
			t.Errorf("pane %v kept its search: query %q, matches %v", p, m.search[p], tbl.Matches())
		}
	}
	if m.search[panePods] != "weather" || len(m.podsTbl.Matches()) == 0 {
		t.Errorf("the pods picker lost its search: query %q", m.search[panePods])
	}
	if m.search[targetHelp] != "quit" {
		t.Errorf("help lost its search: %q", m.search[targetHelp])
	}
}
