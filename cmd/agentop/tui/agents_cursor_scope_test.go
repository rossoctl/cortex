package tui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
)

// cursorScopeModel is a connected agents pane over navRows, cursor on claude-code's row, with
// claude-code committed as the scope. Requests are recorded as their query strings.
func cursorScopeModel(t *testing.T) (*model, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var asked []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.RawQuery)
		mu.Unlock()
		// project= is sent but not echoed, so this server acts as one older than project=.
		// agentop's own fold labels must never go on the wire.
		if p := r.URL.Query().Get("project"); p == otherProjects || p == projectOverflowLabel {
			t.Errorf("query %q carries project=%s", r.URL.RawQuery, p)
		}
		// The agent is ECHOED, as the real server does, so fetchUsageScoped does not take its
		// old-server fallback and re-ask unscoped.
		q := r.URL.Query()
		_, _ = w.Write([]byte(`{"window":"` + q.Get("window") + `","group":"` + q.Get("group") +
			`","agent":"` + q.Get("agent") + `","buckets":[],"totals":{}}`))
	}))
	t.Cleanup(ts.Close)
	m := &model{
		width:      120,
		height:     40,
		pane:       paneAgents,
		client:     apiclient.New(ts.URL),
		agentScope: "claude-code/2.1.270",
		agents:     navRows(),
		agentsTbl:  newAgentsTable(),
	}
	m.rebuildAgentsTable()
	setCursorVisible(&m.agentsTbl, 1)
	return m, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := asked
		asked = nil
		return out
	}
}

// pressKey drives one key through Update and runs every command it returned, batches included.
// Replies are not fed back: these tests measure what was ASKED for.
func pressKey(m *model, key tea.KeyMsg) {
	_, cmd := m.Update(key)
	runCmds(cmd)
}

// runCmds runs a command tree concurrently and returns once every member has returned. Ticks
// fire at once in this package's tests (see TestMain).
func runCmds(cmd tea.Cmd) {
	var wg sync.WaitGroup
	var run func(c tea.Cmd)
	run = func(c tea.Cmd) {
		if c == nil {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if batch, ok := c().(tea.BatchMsg); ok {
				for _, cc := range batch {
					run(cc)
				}
			}
		}()
	}
	run(cmd)
	wg.Wait()
}

// bandAgents is the agent each band request carried — the band's requests are the ungrouped
// ones; the drawer and the rows each name a group.
func bandAgents(queries []string) []string {
	var out []string
	for _, q := range queries {
		if strings.Contains(q, "group=") {
			continue
		}
		out = append(out, agentOf(q))
	}
	return out
}

func agentOf(rawQuery string) string {
	for _, kv := range strings.Split(rawQuery, "&") {
		if v, ok := strings.CutPrefix(kv, "agent="); ok {
			return strings.ReplaceAll(v, "%2F", "/")
		}
	}
	return ""
}

// THE BAND FOLLOWS THE CURSOR ON THE AGENTS PANE, as the drawer under it already did. The band
// used to keep the committed scope, so a reader who moved the highlight to bob-shell saw
// claude-code's spend above a bob-shell breakdown, under a title saying "scoped to claude-code"
// — three agents' worth of captions for two agents' figures. Now the whole screen describes the
// highlighted row; Enter still commits it, and leaving the pane puts the band back on the
// committed scope.
func TestSpendBand_FollowsTheAgentsCursor(t *testing.T) {
	m, asked := cursorScopeModel(t)

	pressKey(m, tea.KeyMsg{Type: tea.KeyDown})
	got := bandAgents(asked())
	if len(got) != int(numSpendSpans) {
		t.Fatalf("after the cursor moved, band requests carried %v; want one per span (%d), each for bob-shell", got, numSpendSpans)
	}
	for _, a := range got {
		if a != "bob-shell/2.0.5" {
			t.Errorf("band request scoped to %q after the cursor moved to bob-shell", a)
		}
	}

	pressKey(m, tea.KeyMsg{Type: tea.KeyDown})
	if got := bandAgents(asked()); len(got) != 0 {
		t.Errorf("a clamped cursor move refetched the band: %v; a key that moves nothing must fetch nothing", got)
	}

	pressKey(m, tea.KeyMsg{Type: tea.KeyUp})
	pressKey(m, tea.KeyMsg{Type: tea.KeyUp})
	for _, a := range bandAgents(asked()) {
		if a != "" && a != "claude-code/2.1.270" {
			t.Errorf("band request scoped to %q on the way up through claude-code to All agents", a)
		}
	}
	if m.spend.bandAgent != "" {
		t.Errorf("on All agents the band is scoped to %q; that row is every agent", m.spend.bandAgent)
	}

	pressKey(m, tea.KeyMsg{Type: tea.KeyEsc})
	for _, a := range bandAgents(asked()) {
		if a != "claude-code/2.1.270" {
			t.Errorf("after leaving the pane, band request scoped to %q; want the committed claude-code/2.1.270", a)
		}
	}
	if m.agentScope != "claude-code/2.1.270" {
		t.Errorf("moving the cursor changed the committed scope to %q; only Enter writes it", m.agentScope)
	}
}

// THE TITLE NAMES THE ROW THE SCREEN DESCRIBES. It said "scoped to <committed>", which stopped
// being the screen's subject once the band and the drawer followed the cursor.
func TestAgentsTitle_NamesTheHighlightedRow(t *testing.T) {
	m, _ := cursorScopeModel(t)
	setCursorVisible(&m.agentsTbl, 2)
	title := strings.SplitN(m.paneView(), "\n", 2)[0]
	if !strings.Contains(title, "agent=bob-shell/2.0.5") {
		t.Errorf("title %q does not name the highlighted bob-shell row", title)
	}
	if strings.Contains(title, "claude-code") {
		t.Errorf("title %q names the committed scope, which nothing on the pane now shows", title)
	}
	setCursorVisible(&m.agentsTbl, 0)
	if title := strings.SplitN(m.paneView(), "\n", 2)[0]; strings.Contains(title, "agent=") {
		t.Errorf("title %q names an agent on the All agents row", title)
	}
}

// `w` ON THE AGENTS PANE MOVES THE PANE'S WINDOW, drawer open or closed: the rows, the COST
// header and the drawer all answer for one span. The title does not name the window: the COST
// header and the footer's `w` hint already do.
func TestAgentsPane_WCyclesOneWindowForRowsHeaderAndDrawer(t *testing.T) {
	m, asked := cursorScopeModel(t)
	view := func() (title, rest string) {
		parts := strings.SplitN(m.paneView(), "\n", 2)
		return parts[0], parts[1]
	}
	if title, rest := view(); !strings.Contains(rest, "COST TODAY") || strings.Contains(title, "today") {
		t.Fatalf("title %q: the pane must open on today, named in the COST header and not in the title", title)
	}

	pressKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}})
	rows := groupedWindows(asked(), "agent")
	if len(rows) != 1 || rows[0] != "7d" {
		t.Errorf("after `w` with the drawer closed, rows requested windows %v; want one, for 7d", rows)
	}
	// runCmds drops the rows reply, so rebuild the table as agentRowsLoadedMsg does.
	m.rebuildAgentsTable()
	if title, rest := view(); !strings.Contains(rest, "COST 7 DAYS") || strings.Contains(title, "7 days") {
		t.Errorf("after `w` the COST header must name the 7-day window and the title %q must not", title)
	}

	m.spend.expanded = true
	runCmds(m.syncSpendScope())
	if got := groupedWindows(asked(), "model"); len(got) != 1 || got[0] != "7d" {
		t.Errorf("the drawer opened on the agents pane asked for %v; want the pane's 7d", got)
	}

	pressKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}})
	q := asked()
	if got := groupedWindows(q, "agent"); len(got) != 1 || got[0] != "month" {
		t.Errorf("after a second `w`, rows requested %v; want month", got)
	}
	if got := groupedWindows(q, "model"); len(got) != 1 || got[0] != "month" {
		t.Errorf("after a second `w`, the drawer requested %v; want month, the rows' window", got)
	}

	// Off the pane the drawer keeps its own cycle, which the agents pane did not move.
	pressKey(m, tea.KeyMsg{Type: tea.KeyEsc})
	if got := groupedWindows(asked(), "model"); len(got) != 1 || got[0] != "1h" {
		t.Errorf("after leaving the pane the drawer requested %v; want its own 1h", got)
	}
}

// groupedWindows is the window of each request folded by group.
func groupedWindows(queries []string, group string) []string {
	var out []string
	for _, q := range queries {
		if !strings.Contains(q, "group="+group) {
			continue
		}
		for _, kv := range strings.Split(q, "&") {
			if v, ok := strings.CutPrefix(kv, "window="); ok {
				out = append(out, v)
			}
		}
	}
	return out
}

// AN OPENING FETCH STARTS ON TODAY: an `A` press from another pane, and the startup gate a new
// connection runs. `w` lasts for one visit — `A` on the pane itself, the pane's poll and esc
// back from the sessions list it led to keep it — so leaving and re-entering, or connecting to
// another pod, does not inherit a window the reader picked for a different look.
func TestAgentsPane_AnOpeningFetchStartsOnToday(t *testing.T) {
	m, asked := cursorScopeModel(t)
	pressKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}})
	asked()

	pressKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'A'}})
	if got := groupedWindows(asked(), "agent"); len(got) != 1 || got[0] != "7d" {
		t.Errorf("`A` on the pane itself requested %v; want the visit's 7d kept", got)
	}

	m.pane = paneSessions
	pressKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'A'}})
	if got := groupedWindows(asked(), "agent"); len(got) != 1 || got[0] != "today" {
		t.Errorf("`A` from Sessions requested %v; want today", got)
	}

	m.pane = paneAgents
	pressKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}})
	if m.agentsSpan() == spanToday {
		t.Fatal("`w` on the pane left the window on today; the gate case below would prove nothing")
	}
	m.pane = paneSessions
	asked()
	runCmds(m.startupAgentsGateCmd())
	if got := groupedWindows(asked(), "agent"); len(got) != 1 || got[0] != "today" {
		t.Errorf("the startup gate requested %v; want today, whatever the last connection's `w` left", got)
	}
}
