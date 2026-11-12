package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/cost/usage"
)

// bandReply runs one band fetch for the hour and returns its reply.
func bandReply(t *testing.T, m *model) spendLoadedMsg {
	t.Helper()
	msg, ok := m.fetchSpendSpan(spanHour)().(spendLoadedMsg)
	if !ok {
		t.Fatal("the band fetch did not give a spendLoadedMsg")
	}
	return msg
}

func TestProjectScope_BandTotalEqualsTheSubRowCost(t *testing.T) {
	m, asked := projectsModel(t, defaultProjectSeries)
	openClaudeCode(t, m)
	onDesc(t, m, cortexRow)
	asked()
	msg := bandReply(t, m)
	if msg.err != nil || msg.snap == nil || msg.snap.Totals.CostMicros != 97_400_000 {
		t.Fatalf("band reply %+v (err %v), want the cortex sub-row COST 97_400_000", msg.snap, msg.err)
	}
	if q := asked(); !hasQuery(q, "agent=claude-code", "group=project") {
		t.Errorf("band queries %q, want agent=claude-code and group=project", q)
	}
}

func TestProjectScope_BandOnTheOtherSubRow(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	openClaudeCode(t, m)
	onDesc(t, m, otherRow)
	if msg := bandReply(t, m); msg.snap == nil || msg.snap.Totals.CostMicros != 15_520_000 {
		t.Errorf("band reply on Other %+v, want unknown + overflow 15_520_000", msg.snap)
	}
}

// With no overflow series in the reply, a project that is not a series has no traffic.
func TestProjectScope_BandOnAProjectWithNoTrafficIsEmpty(t *testing.T) {
	m, _ := projectsModel(t, map[string]map[string]int64{projAgent: {"~/works/cortex": 97_400_000}})
	m.agentScope, m.projectScope = projAgent, "~/gone"
	m.pane = paneSessions
	msg := bandReply(t, m)
	if msg.err != nil || msg.snap == nil || msg.snap.Totals != (usage.Counts{}) {
		t.Errorf("band reply %+v (err %v), want zero totals and no error", msg.snap, msg.err)
	}
}

// With an overflow series, the project can be inside it: the band reports an error, not "—".
func TestProjectScope_BandOnAProjectPastTheSeriesCapIsAnError(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	m.agentScope, m.projectScope = projAgent, "~/gone"
	m.pane = paneSessions
	msg := bandReply(t, m)
	if msg.err == nil || !strings.Contains(msg.err.Error(), "costliest") {
		t.Errorf("band reply %+v (err %v), want the not-among-the-costliest error", msg.snap, msg.err)
	}
}

// A mixed-unit window under a project scope makes no group=currency read: that read is the
// whole agent's, so its per-unit figures would show under the project's title.
func TestProjectScope_BandMakesNoCurrencyReadForAMixedWindow(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	var mu sync.Mutex
	var asked []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.RawQuery)
		mu.Unlock()
		q := r.URL.Query()
		_ = json.NewEncoder(w).Encode(usage.Snapshot{
			Window: q.Get("window"), Group: usage.Group(q.Get("group")), Agent: q.Get("agent"),
			Currencies: []string{"USD", "EUR"},
			Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
				"~/works/cortex": projectCounts(97_400_000),
				"EUR":            projectCounts(1),
				"USD":            projectCounts(1),
			}}},
		})
	}))
	t.Cleanup(ts.Close)
	m.client = apiclient.New(ts.URL)
	m.agentScope, m.projectScope = projAgent, "~/works/cortex"
	m.pane = paneSessions
	if msg := bandReply(t, m); msg.err != nil {
		t.Fatalf("band reply err %v", msg.err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, q := range asked {
		if strings.Contains(q, "group=currency") {
			t.Errorf("band asked %q under a project scope, want no group=currency read", q)
		}
	}
	if len(asked) != 1 {
		t.Errorf("band made %d reads %q, want 1", len(asked), asked)
	}
}

func TestProjectScope_SyncSpendScopeWatchesTheProject(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	openClaudeCode(t, m)
	onDesc(t, m, cortexRow)
	runCmds(m.syncSpendScope())
	if m.spend.bandProject != "~/works/cortex" {
		t.Fatalf("bandProject %q, want ~/works/cortex", m.spend.bandProject)
	}
	onDesc(t, m, mocaRow)
	cmd := m.syncSpendScope()
	if cmd == nil {
		t.Fatal("a move between two sub-rows of one agent gave no band refetch")
	}
	runCmds(cmd)
	if m.spend.bandProject != "~/moca" {
		t.Errorf("bandProject %q, want ~/moca", m.spend.bandProject)
	}
}

// openDrawerOnCortex opens the drawer on the cortex sub-row and delivers its reply.
func openDrawerOnCortex(t *testing.T, m *model) {
	t.Helper()
	openClaudeCode(t, m)
	onDesc(t, m, cortexRow)
	m.spend.expanded = true
	runCmds(m.syncSpendScope())
	m.Update(m.fetchSpendDrawer()())
}

func TestProjectScope_DrawerHasNoAxesAndSaysSo(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	openDrawerOnCortex(t, m)
	if axes := m.spendAxes(); len(axes) != 0 {
		t.Errorf("spendAxes %q, want none under a project", axes)
	}
	if m.spend.drawerProject != "~/works/cortex" {
		t.Errorf("drawerProject %q, want ~/works/cortex", m.spend.drawerProject)
	}
	axis, window := m.drawerLabels()
	lines := strings.Join(renderSpendDrawerAxes(m.spend.drawer.snap, m.spend.drawer.err, axis,
		m.spend.drawerAxis, m.spendAxes(), window, m.spend.drawerProject, m.width), "\n")
	if !strings.Contains(lines, "no breakdown for this scope in") || strings.Contains(lines, "[a]") {
		t.Errorf("drawer lines do not say there is no breakdown, or show [a]:\n%s", lines)
	}
}

func TestProjectScope_DrawerAxisKeyIsInert(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	openDrawerOnCortex(t, m)
	idx := m.spend.groupIdx
	pressCmd(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if cmd := m.cycleSpendAxis(); cmd != nil {
		t.Error("a under a project scope gave a command")
	}
	if m.spend.groupIdx != idx {
		t.Errorf("groupIdx %d, want %d", m.spend.groupIdx, idx)
	}
}
