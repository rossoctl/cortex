package tui

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/sessionapi"
)

// oneServerRaw is a router with a single server: nothing for a SERVER column to tell apart.
const oneServerRaw = `{"servers":{"ete":{"url":"https://ete.example.com","key":"sk-ete","main":"opus","helper":"haiku"}}}`

// serverCellOf is the cell under the column titled title in session id's row, and whether the
// column is there at all.
func serverCellOf(t *testing.T, m *model, id, title string) (string, bool) {
	t.Helper()
	col := -1
	for i, c := range m.sessionsTbl.Columns() {
		if headerTitle(c) == title {
			col = i
		}
	}
	if col < 0 {
		return "", false
	}
	for i, rid := range m.sessionRowIDs {
		if rid == id {
			return m.sessionsTbl.Rows()[i][col], true
		}
	}
	t.Fatalf("no sessions row %q in %q", id, m.sessionRowIDs)
	return "", false
}

func serverSessions() []session.SessionSummary {
	now := time.Now()
	return []session.SessionSummary{
		{ID: "on-glm", UpdatedAt: now, Agent: "claude-code", InferenceHost: "GLM.example.com:8443"},
		{ID: "on-ete", UpdatedAt: now.Add(-time.Second), Agent: "claude-code", InferenceHost: "ete.example.com:443"},
		{ID: "elsewhere", UpdatedAt: now.Add(-2 * time.Second), Agent: "opencode", InferenceHost: "api.anthropic.com"},
		{ID: "quiet", UpdatedAt: now.Add(-3 * time.Second), Agent: "opencode"},
	}
}

func sessionsServerModel(raw string) *model {
	m := &model{width: 200, height: 40, pane: paneSessions, sessionsTbl: newSessionsTable(), sessions: serverSessions()}
	if raw != "" {
		m.pipeline = routerPipeline(raw)
	}
	m.rebuildSessionsTable()
	return m
}

// SERVER names the server each session's inference went to, port and case ignored; a host no
// server has, in parentheses; and an em dash for a session with no inference traffic, or for a
// cached-only row, which has no summary to read a host from.
func TestSessionsTable_ServerColumnNamesEachSessionsServer(t *testing.T) {
	m := sessionsServerModel(routerRaw)
	// Held by agentop, no longer listed by the server: its events went to glm, but a row with no
	// summary names no server rather than one guessed from whatever events agentop kept.
	m.events = map[string][]pipeline.SessionEvent{"gone": {{At: time.Now(), Phase: pipeline.SessionRequest,
		Host: "glm.example.com:8443", Inference: &pipeline.InferenceExtension{Model: "glm-4.6"}}}}
	m.rebuildSessionsTable()
	for id, want := range map[string]string{
		"on-glm":    "glm",
		"on-ete":    "ete",
		"elsewhere": "(api.anthro…",
		"quiet":     emptyCell,
		"gone":      emptyCell,
	} {
		if got, ok := serverCellOf(t, m, id, "SERVER"); !ok || got != want {
			t.Errorf("%s: SERVER = %q (present %v), want %q", id, got, ok, want)
		}
	}
}

// With one server, or none, every session is on it or on its own provider, and the column
// would say nothing.
func TestSessionsTable_ServerColumnNeedsTwoServers(t *testing.T) {
	for name, raw := range map[string]string{"no router": "", "one server": oneServerRaw} {
		if _, ok := serverCellOf(t, sessionsServerModel(raw), "on-glm", "SERVER"); ok {
			t.Errorf("%s: SERVER is shown", name)
		}
	}
}

// A router under on_error: observe keeps its config on /v1/pipeline but moves nothing, so the
// sessions table hides SERVER as the agents pane does: a session there went where its client
// sent it, and naming a server off the config would claim a route that never happened.
func TestSessionsTable_ServerColumnHidesForARouterThatRoutesNothing(t *testing.T) {
	m := sessionsServerModel("")
	m.pipeline = observing(routerPipeline(routerRaw))
	m.rebuildSessionsTable()
	if got, ok := serverCellOf(t, m, "on-glm", "SERVER"); ok {
		t.Errorf("SERVER = %q for a router under on_error: observe, which routes nothing", got)
	}
}

// Read-only: S on the sessions table opens nothing and writes nothing.
func TestSessionsTable_NoKeyChangesASessionsServer(t *testing.T) {
	m := sessionsServerModel(routerRaw)
	if cmd := m.handleKey(keyRune('S')); cmd != nil || m.serverPicker != nil {
		t.Errorf("S on the sessions table: cmd %v, picker %+v", cmd, m.serverPicker)
	}
}

// Dropped whole, never squeezed, on a terminal too narrow for it: with SERVER, no other column
// is narrower than it is without SERVER, or than its declared width. TITLE's growth into slack is
// shared, not taken.
func TestSessionsTable_ServerColumnNeverNarrowsAnother(t *testing.T) {
	declared := map[string]int{"AGENT": sessionsAgentWidth}
	for _, c := range sessionsColumns() {
		declared[headerTitle(c)] = c.Width
	}
	for _, width := range []int{200, 120, 100, 90, 80, 60} {
		with := fitTableColumns(sessionsColumnsWith(width, true, true), width)
		without := fitTableColumns(sessionsColumnsWith(width, true, false), width)
		for _, c := range without {
			title := headerTitle(c)
			if w := sessionsColumnWidth(with, title); w < min(c.Width, declared[title]) {
				t.Errorf("width %d: SERVER narrowed %s to %d", width, title, w)
			}
		}
	}
	if sessionsColumnWidth(fitTableColumns(sessionsColumnsWith(200, true, true), 200), "SERVER") == 0 {
		t.Error("no SERVER column at 200 columns")
	}
}

func TestSessionsServerCells_CarryNoANSI(t *testing.T) {
	restore := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(restore) })
	m := sessionsServerModel(routerRaw)
	for r, row := range m.sessionsTbl.Rows() {
		for c, cell := range row {
			if strings.ContainsRune(cell, 0x1b) {
				t.Errorf("row %d cell %d carries an escape: %q", r, c, cell)
			}
		}
	}
}

// The cell draws request-controlled text and a name off an unauthenticated API, so it shows a
// server name sanitised, and of a host only a host: never a key a client put before an '@', a
// path or query after one, or a control. A host outside a hostname's alphabet is not shown at
// all — nor are parentheses inside one, which would let a host read as two set-apart hosts.
func TestSessionServerCell_ShowsOnlyAHostOfAHost(t *testing.T) {
	router := routerconfig.Config{Servers: map[string]routerconfig.Server{
		"ete":         {URL: "https://ete.example.com"},
		"\x1b[31mglm": {URL: "https://glm.example.com:8443"},
	}}
	for host, want := range map[string]string{
		"glm.example.com:8443":           "�[31mglm",
		"localhost:11434":                "(localhost)",
		"[::1]:8080":                     "(::1)",
		"sk-ant-api03-secret:x@evil.com": sessionsNotAHost,
		"evil.com/v1/messages?key=sk":    sessionsNotAHost,
		"\x1b[2Jevil.com":                sessionsNotAHost,
		"evil.com)(ete.example.com":      sessionsNotAHost,
		":8443":                          sessionsNotAHost,
	} {
		got := sessionServerCell(router, host, sessionsServerWidth)
		if got != want {
			t.Errorf("sessionServerCell(%q) = %q, want %q", host, got, want)
		}
		for _, leak := range []string{"secret", "key=", "/v1", "\x1b"} {
			if strings.Contains(got, leak) {
				t.Errorf("sessionServerCell(%q) = %q shows %q", host, got, leak)
			}
		}
	}
}

// serveSessionsAndRouter is a real sessionapi server over store, whose outbound pipeline runs the
// router configured as routerRaw under policy: both halves of what the SERVER column reads, as
// the proxy serves them.
func serveSessionsAndRouter(t *testing.T, store *session.Store, policy pipeline.ErrorPolicy) string {
	t.Helper()
	p, err := pipeline.New([]pipeline.Plugin{pipeline.WrapConfigured(routerPlugin{}, json.RawMessage(routerRaw))},
		pipeline.WithPolicies(policy))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(sessionapi.New(":0", store, sessionapi.WithPipelines(nil, pipeline.NewHolder(p))).Server().Handler)
	t.Cleanup(ts.Close)
	return ts.URL
}

// THROUGH THE WIRE, both halves: inferenceHost as /v1/sessions folds and serves it, and the
// router's config as /v1/pipeline serves it — keys redacted, and its policy with it. A hand-built
// summary or view would supply what the server might not.
func TestServerColumns_FromTheWire(t *testing.T) {
	resetSettingsForTest(t)
	store := session.New(5*time.Minute, 100, 0)
	t.Cleanup(store.Close)
	claude := &pipeline.EventClient{Name: "claude-code", Version: "2.1.284"}
	store.Append("on-glm", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest, Client: claude,
		Host: "glm.example.com:8443", Inference: &pipeline.InferenceExtension{Model: "claude-opus-5-5"}})
	// A tunnel row after the turn, as live traffic interleaves them: it must not move the host.
	store.Append("on-glm", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest, Client: claude,
		Tunnel: true, HTTPMethod: "CONNECT", Host: "api.anthropic.com:443"})

	// Each order reaches a different rebuild: sessions first, and it is the pipeline reply's
	// repaint that has to bring SERVER up; pipeline first, and the sessions reply's own rebuild
	// has to read it.
	load := func(policy pipeline.ErrorPolicy, pipelineFirst bool) *model {
		m := New(context.Background(), apiclient.New(serveSessionsAndRouter(t, store, policy))).(*model)
		m.width, m.height, m.pane = 200, 40, paneSessions
		if pipelineFirst {
			m.Update(m.loadPipelineCmd()())
		}
		m.Update(m.loadSessionsCmd()())
		if !pipelineFirst {
			m.Update(m.loadPipelineCmd()())
		}
		return m
	}

	m := load(pipeline.ErrorPolicyEnforce, false)
	if got, ok := serverCellOf(t, m, "on-glm", "SERVER"); !ok || got != "glm" {
		t.Errorf("SERVER = %q (present %v), want glm from the session's inferenceHost", got, ok)
	}
	router, on := m.activeRouter()
	if !on || router.Agents["claude-code"] != "ete" || router.Servers["ete"].Key != "[REDACTED]" {
		t.Errorf("router off the wire = %+v (on %v), want claude-code → ete and the key redacted", router, on)
	}

	if got, ok := serverCellOf(t, load(pipeline.ErrorPolicyEnforce, true), "on-glm", "SERVER"); !ok || got != "glm" {
		t.Errorf("pipeline first: SERVER = %q (present %v), want glm", got, ok)
	}

	// The control: the same store and config under observe, which only the wire carries.
	// Pipeline first, so the sessions rebuild runs with the observing router in hand: the other
	// order passes however SERVER is decided, since a router that was off and is still off
	// repaints nothing.
	if got, ok := serverCellOf(t, load(pipeline.ErrorPolicyObserve, true), "on-glm", "SERVER"); ok {
		t.Errorf("SERVER = %q off the wire for a router under on_error: observe", got)
	}
}

// Widening the terminal never takes AGENT or SERVER away, and never narrows TITLE, whichever of
// the two are asked for. Both used to come and go: at 90 they fitted in the room COST and SAVED
// leave while TITLE holds them out, at 93 those returned and took it back, and at 113 or so the
// optional columns fitted again — present, absent, present as the window widened. TITLE then
// grew into the room AGENT and SERVER would claim and shrank when they arrived, the money
// columns' old harm by another door (TestSessionsTitle_WidthNeverShrinksAsTheTerminalGrows).
func TestSessionsTable_WideningNeverDropsAColumnOrNarrowsTitle(t *testing.T) {
	for _, flags := range []struct{ agent, server bool }{{true, false}, {false, true}, {true, true}} {
		seen := map[string]int{}
		prevTitle := 0
		for w := 40; w < 250; w++ {
			cols := fitTableColumns(sessionsColumnsWith(w, flags.agent, flags.server), w)
			for _, title := range []string{"AGENT", "SERVER", "TITLE", "COST"} {
				if sessionsColumnWidth(cols, title) > 0 {
					seen[title] = w
				} else if at, ok := seen[title]; ok && at == w-1 {
					t.Errorf("agent=%v server=%v: widening %d to %d dropped %s", flags.agent, flags.server, w-1, w, title)
				}
			}
			if got := sessionsColumnWidth(cols, "TITLE"); got > 0 {
				if prevTitle > 0 && got < prevTitle {
					t.Errorf("agent=%v server=%v: widening %d to %d shrank TITLE from %d to %d",
						flags.agent, flags.server, w-1, w, prevTitle, got)
				}
				prevTitle = got
			}
		}
		if seen["SERVER"] == 0 && flags.server || seen["AGENT"] == 0 && flags.agent {
			t.Errorf("agent=%v server=%v: never shown below 250 columns: %v", flags.agent, flags.server, seen)
		}
	}
}
