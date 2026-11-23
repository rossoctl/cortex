package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/cost/usage"
	coremodel "github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session"
)

// mixedSplit is a cost in two units: $1065.52 and 0.95 Bobcoins. The cell has no thousands
// separator, as bandAmount prints it.
var mixedSplit = map[string]int64{"USD": 1_065_520_000, "Bobcoins": 950_000}

const mixedCell = "$1065.52 + 0.95 Bobcoins"

func TestUnitsAmount_DollarsFirst(t *testing.T) {
	got, ok := unitsAmount([]string{"Bobcoins", "USD"}, mixedSplit)
	if !ok || got != mixedCell {
		t.Errorf("unitsAmount = %q, %v, want %q, true", got, ok, mixedCell)
	}
	if _, ok := unitsAmount([]string{"Bobcoins", "USD", "credits"}, mixedSplit); ok {
		t.Error("a unit missing from byUnit gave an amount")
	}
	if _, ok := unitsAmount([]string{"Bobcoins", "USD"}, nil); ok {
		t.Error("a nil byUnit gave an amount")
	}
}

func TestAgentCostCellIn_EachUnit(t *testing.T) {
	c := usage.Counts{CostMicros: 1_066_470_000, PricedRequests: 3}
	units := []string{"Bobcoins", "USD"}
	if got := agentCostCellIn(c, units, mixedSplit, 40); got != mixedCell {
		t.Errorf("budget 40: %q, want %q", got, mixedCell)
	}
	if got := agentCostCellIn(c, units, mixedSplit, 12); got != "(mixed)" {
		t.Errorf("budget 12: %q, want (mixed)", got)
	}
	if got := agentCostCellIn(c, units, nil, 40); got != "(mixed)" {
		t.Errorf("no split: %q, want (mixed)", got)
	}
}

// unitsServer answers group=currency with mixedSplit, echoing agent= and project=, and group=agent
// with nothing. It records each query.
func unitsServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var asked []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.RawQuery)
		mu.Unlock()
		q := r.URL.Query()
		snap := usage.Snapshot{Window: q.Get("window"), Group: usage.Group(q.Get("group")),
			Agent: q.Get("agent"), Project: coremodel.LenientString(q.Get("project"))}
		if snap.Group == usage.GroupCurrency {
			snap.Currencies = []string{"Bobcoins", "USD"}
			b := usage.Bucket{Series: map[string]usage.Counts{}}
			for u, micros := range mixedSplit {
				b.Series[u] = usage.Counts{CostMicros: micros, PricedRequests: 1}
			}
			snap.Buckets = []usage.Bucket{b}
		}
		_ = json.NewEncoder(w).Encode(snap)
	}))
	t.Cleanup(ts.Close)
	return ts, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), asked...)
	}
}

// unitsModel is a connected agents pane width columns wide, router on, over a claude-code row in
// two units and a bob row in Bobcoins only, with sessions that name both agents.
func unitsModel(t *testing.T, width int) (*model, func() []string) {
	t.Helper()
	ts, asked := unitsServer(t)
	now := time.Now()
	m := &model{
		ctx:       context.Background(),
		width:     width,
		height:    40,
		pane:      paneAgents,
		client:    apiclient.New(ts.URL),
		agentsTbl: newAgentsTable(),
		pipeline:  routerPipeline(routerRaw),
		sessions: []session.SessionSummary{
			{ID: "a", Agent: "claude-code/2.1.270", UpdatedAt: now},
			{ID: "b", Agent: "bob-shell/2.0.5", UpdatedAt: now},
		},
		now: func() time.Time { return now },
	}
	return m, asked
}

func loadUnitsRows(m *model) {
	_, cmd := m.Update(agentRowsLoadedMsg{window: m.agentsSpanDef().window, rows: []agentRow{
		{label: "claude-code/2.1.270", Counts: usage.Counts{CostMicros: 1_066_470_000, PricedRequests: 3},
			units: []string{"Bobcoins", "USD"}},
		{label: "bob-shell/2.0.5", Counts: usage.Counts{CostMicros: 15_900_000, PricedRequests: 2},
			units: []string{"Bobcoins"}},
	}})
	deliverSplits(m, cmd)
}

// deliverSplits runs cmd and passes each split reply it gives through Update.
func deliverSplits(m *model, cmd tea.Cmd) {
	var mu sync.Mutex
	var splits []agentsUnitSplitLoadedMsg
	var wg sync.WaitGroup
	var run func(c tea.Cmd)
	run = func(c tea.Cmd) {
		if c == nil {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch msg := c().(type) {
			case tea.BatchMsg:
				for _, cc := range msg {
					run(cc)
				}
			case agentsUnitSplitLoadedMsg:
				mu.Lock()
				splits = append(splits, msg)
				mu.Unlock()
			}
		}()
	}
	run(cmd)
	wg.Wait()
	for _, s := range splits {
		m.Update(s)
	}
}

func agentRowText(t *testing.T, m *model, label string) string {
	t.Helper()
	i := agentRowIndex(m, label)
	if i < 0 {
		t.Fatalf("no row %q", label)
	}
	return strings.Join(m.agentsTbl.Rows()[i], " ")
}

func TestAgentsPane_MixedRowShowsEachUnitWhenWide(t *testing.T) {
	m, _ := unitsModel(t, 160)
	loadUnitsRows(m)
	if got := agentRowText(t, m, "claude-code/2.1.270"); !strings.Contains(got, mixedCell) {
		t.Errorf("row %q, want %q", got, mixedCell)
	}
	if !strings.Contains(m.View(), mixedCell) {
		t.Errorf("the view cuts the cell:\n%s", m.View())
	}
}

func TestAgentsPane_ForeignUnitIsNotCut(t *testing.T) {
	m, _ := unitsModel(t, 160)
	loadUnitsRows(m)
	got := agentRowText(t, m, "bob-shell/2.0.5")
	if !strings.Contains(got, "15.90 Bobcoins") || strings.Contains(got, "…") {
		t.Errorf("row %q, want 15.90 Bobcoins uncut", got)
	}
}

func TestAgentsPane_MixedCellFallsBackAt80(t *testing.T) {
	m, _ := unitsModel(t, 80)
	m.agents = []agentRow{{label: "claude-code/2.1.270", Counts: usage.Counts{CostMicros: 1, PricedRequests: 1}}}
	m.rebuildAgentsTable()
	for _, title := range []string{"SESSIONS", "LAST SEEN", "SERVER"} {
		if _, ok := agentsCell(t, m, "claude-code/2.1.270", title); !ok {
			t.Fatalf("no %s column at 80", title)
		}
	}
	before := sessionsColumnWidth(m.agentsTbl.Columns(), "AGENT")
	loadUnitsRows(m)
	if got := sessionsColumnWidth(m.agentsTbl.Columns(), "AGENT"); got != before {
		t.Errorf("AGENT is %d columns with the split, %d without", got, before)
	}
	if got := agentRowText(t, m, "claude-code/2.1.270"); !strings.Contains(got, "(mixed)") {
		t.Errorf("row %q, want (mixed)", got)
	}
}

func TestAgentsPane_ProjectRowSplitUsesProject(t *testing.T) {
	m, asked := unitsModel(t, 160)
	m.sessions[0].Projects = []string{"~/works/cortex"}
	loadUnitsRows(m)
	m.agentsExpanded = map[string]bool{"claude-code/2.1.270": true}
	_, cmd := m.Update(agentProjectsLoadedMsg{agent: "claude-code/2.1.270", window: m.agentsSpanDef().window,
		data: agentProjectsData{rows: []agentRow{
			{label: "~/works/cortex", Counts: usage.Counts{CostMicros: 1_066_470_000, PricedRequests: 3},
				units: []string{"Bobcoins", "USD"}},
			{label: otherProjects, Counts: usage.Counts{CostMicros: 5, PricedRequests: 1},
				units: []string{"Bobcoins", "USD"}},
		}}})
	deliverSplits(m, cmd)
	found := false
	for _, q := range asked() {
		if strings.Contains(q, "project=") {
			if !strings.Contains(q, "project=~%2Fworks%2Fcortex") || !strings.Contains(q, "group=currency") {
				t.Errorf("split query %q, want project=~/works/cortex and group=currency", q)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("no project split request in %q", asked())
	}
	d := agentRowDesc{agent: "claude-code/2.1.270", kind: agentRowProject, project: "~/works/cortex"}
	if got := strings.Join(m.agentsTbl.Rows()[indexOfDesc(t, m, d)], " "); !strings.Contains(got, mixedCell) {
		t.Errorf("project row %q, want %q", got, mixedCell)
	}
	other := agentRowDesc{agent: "claude-code/2.1.270", kind: agentRowProjectOther}
	if got := strings.Join(m.agentsTbl.Rows()[indexOfDesc(t, m, other)], " "); !strings.Contains(got, "(mixed)") {
		t.Errorf("Other row %q, want (mixed)", got)
	}
}

func TestAgentsPane_StaleSplitIsDropped(t *testing.T) {
	m, _ := unitsModel(t, 160)
	m.agentsWindowStep = 2 // month
	m.agents = []agentRow{{label: "claude-code/2.1.270", Counts: usage.Counts{CostMicros: 1, PricedRequests: 1},
		units: []string{"Bobcoins", "USD"}}}
	m.agentsRowsWindow = m.agentsSpanDef().window
	m.Update(agentsUnitSplitLoadedMsg{key: agentsSplitKey{agent: "claude-code/2.1.270", window: usage.Window7d},
		byUnit: mixedSplit})
	if len(m.agentsSplits) != 0 {
		t.Errorf("a 7d split was kept in the %s window: %v", m.agentsSpanDef().window, m.agentsSplits)
	}
	m.rebuildAgentsTable()
	if got := agentRowText(t, m, "claude-code/2.1.270"); !strings.Contains(got, "(mixed)") {
		t.Errorf("row %q, want (mixed)", got)
	}
}

func indexOfDesc(t *testing.T, m *model, d agentRowDesc) int {
	t.Helper()
	for i, x := range m.agentRowDescs {
		if x == d {
			return i
		}
	}
	t.Fatalf("no row %+v in %+v", d, m.agentRowDescs)
	return -1
}

func TestAgentsPane_SplitDropsOtherWindows(t *testing.T) {
	m, _ := unitsModel(t, 160)
	window := m.agentsSpanDef().window
	old := agentsSplitKey{agent: "claude-code/2.1.270", window: "old-window"}
	m.agentsSplits = map[agentsSplitKey]map[string]int64{old: mixedSplit}
	cur := agentsSplitKey{agent: "claude-code/2.1.270", window: window}
	m.Update(agentsUnitSplitLoadedMsg{key: cur, byUnit: mixedSplit})
	if _, ok := m.agentsSplits[old]; ok || len(m.agentsSplits) != 1 || m.agentsSplits[cur] == nil {
		t.Errorf("splits %v, want only %+v", m.agentsSplits, cur)
	}
}

func TestAgentsPane_FailedSplitChangesNothing(t *testing.T) {
	m, _ := unitsModel(t, 160)
	window := m.agentsSpanDef().window
	kept := agentsSplitKey{agent: "bob-shell/2.0.5", window: window}
	m.agentsSplits = map[agentsSplitKey]map[string]int64{kept: mixedSplit}
	m.Update(agentsUnitSplitLoadedMsg{key: agentsSplitKey{agent: "claude-code/2.1.270", window: window},
		err: context.DeadlineExceeded})
	if len(m.agentsSplits) != 1 || m.agentsSplits[kept] == nil {
		t.Errorf("a failed split changed the map: %v", m.agentsSplits)
	}
}

func TestAgentsPane_AllProjectsUnderAllAgentsFetchesItsSplit(t *testing.T) {
	m, asked := unitsModel(t, 160)
	m.sessions[0].Projects = []string{"~/works/cortex"}
	loadUnitsRows(m)
	m.agentsExpanded = map[string]bool{"": true}
	_, cmd := m.Update(agentProjectsLoadedMsg{agent: "", window: m.agentsSpanDef().window,
		data: agentProjectsData{all: agentRow{Counts: usage.Counts{CostMicros: 1_066_470_000, PricedRequests: 3},
			units: []string{"Bobcoins", "USD"}}}})
	deliverSplits(m, cmd)
	found := false
	for _, q := range asked() {
		if strings.Contains(q, "group=currency") && !strings.Contains(q, "agent=") && !strings.Contains(q, "project=") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no unscoped split request in %q", asked())
	}
	d := agentRowDesc{kind: agentRowAllProjects}
	if got := strings.Join(m.agentsTbl.Rows()[indexOfDesc(t, m, d)], " "); !strings.Contains(got, mixedCell) {
		t.Errorf("All projects row %q, want %q", got, mixedCell)
	}
}

func TestAgentsPane_AllProjectsUnderAnAgentReadsItsSplit(t *testing.T) {
	m, _ := unitsModel(t, 160)
	m.sessions[0].Projects = []string{"~/works/cortex"}
	loadUnitsRows(m)
	m.agentsExpanded = map[string]bool{"claude-code/2.1.270": true}
	m.rebuildAgentsTable()
	d := agentRowDesc{agent: "claude-code/2.1.270", kind: agentRowAllProjects}
	if got := strings.Join(m.agentsTbl.Rows()[indexOfDesc(t, m, d)], " "); !strings.Contains(got, mixedCell) {
		t.Errorf("All projects row %q, want %q", got, mixedCell)
	}
}
