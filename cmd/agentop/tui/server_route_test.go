package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/cmd/agentop/edit"
	"github.com/rossoctl/cortex/cmd/agentop/servers"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
	"github.com/rossoctl/cortex/core/redact"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/sessionapi"
)

// routerYAML is a local config with the router's two servers and claude-code routed to ete.
const routerYAML = `mode: proxy-sidecar
pipeline:
  outbound:
    plugins:
      - name: inference-parser
      - name: inference-router
        config:
          servers:
            ete:
              url: https://ete.example.com
              key: sk-ete
              main: opus
              helper: haiku
            glm:
              url: https://glm.example.com:8443
              key: sk-glm
              main: glm
              helper: nemotron
          agents:
            claude-code: ete
`

// routerRaw is routerYAML's router config as /v1/pipeline's source holds it, before redaction.
const routerRaw = `{"servers":{"ete":{"url":"https://ete.example.com","key":"sk-ete","main":"opus","helper":"haiku"},` +
	`"glm":{"url":"https://glm.example.com:8443","key":"sk-glm","main":"glm","helper":"nemotron"}},"agents":{"claude-code":"ete"}}`

// routerPipeline is /v1/pipeline with the router configured as raw, its keys redacted as the
// proxy serves them.
func routerPipeline(raw string) *apiclient.PipelineView {
	return &apiclient.PipelineView{Outbound: []apiclient.PipelinePlugin{
		{Name: "inference-parser", Direction: "outbound", Position: 1},
		{Name: "inference-router", Direction: "outbound", Position: 2, Config: redact.JSON(json.RawMessage(raw))},
	}}
}

// fakeProxy is this machine's Cortex as S sees it: a config file, and one server answering
// /reload/status and /v1/pipeline. A proxy that reloads serves the router from the file; one
// that refuses reports a failed reload and keeps serving what it started with.
//
// The refusal is the one edit.PollUntilReloaded reads as PollFailure, so edit.WritePluginConfig
// ends it as WriteReloadFailed and puts the file back: the first poll is the baseline
// (reloads_failed 0), and the second reports one failure past it. last_success stays an hour
// old throughout, so no poll can read the refused write as a reload. It never stops answering,
// so it never models WriteStatusUnreachable.
type fakeProxy struct {
	srv    *httptest.Server
	path   string
	refuse bool
	polls  atomic.Int32
}

func newFakeProxy(t *testing.T, refuse bool) *fakeProxy {
	t.Helper()
	f := &fakeProxy{path: filepath.Join(t.TempDir(), "config.yaml"), refuse: refuse}
	if err := os.WriteFile(f.path, []byte(routerYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/pipeline":
			raw := json.RawMessage(routerRaw)
			if !f.refuse {
				cfg, err := config.Load(f.path)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				raw = cfg.Pipeline.Outbound.Plugins[1].Config
			}
			_ = json.NewEncoder(w).Encode(routerPipeline(string(raw)))
		case "/reload/status":
			st := edit.ReloadStatus{LastSuccess: time.Now(), ReloadsOK: 1}
			if f.refuse {
				st.LastSuccess = time.Now().Add(-time.Hour)
				if f.polls.Add(1) >= 2 {
					st.ReloadsFailed, st.LastError = 1, `configure "inference-router": refused`
				}
			}
			_ = json.NewEncoder(w).Encode(st)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeProxy) config(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// serverModel is a model attached to f as this machine's Cortex, on the AGENTS pane with
// claude-code (routed to ete) and opencode (not routed).
func serverModel(t *testing.T, f *fakeProxy) *model {
	t.Helper()
	m := &model{
		ctx:                context.Background(),
		pane:               paneAgents,
		previousPane:       paneNone,
		pipelineReturnPane: paneNone,
		agentsTbl:          newAgentsTable(),
		sessionsTbl:        newSessionsTable(),
		client:             apiclient.New(f.srv.URL),
		width:              120,
		height:             40,
		localEndpoint:      f.srv.URL,
		localConfigPath:    f.path,
		localStatsURL:      f.srv.URL,
		pipeline:           routerPipeline(routerRaw),
		agents: []agentRow{
			{label: "claude-code/2.1.270", Counts: usage.Counts{Requests: 10, PricedRequests: 10}},
			{label: "opencode/1.0.3", Counts: usage.Counts{Requests: 4}},
		},
	}
	m.rebuildAgentsTable()
	return m
}

// onRow puts the AGENTS cursor on the row labelled label, All agents being "".
func onRow(t *testing.T, m *model, label string) {
	t.Helper()
	for i, l := range m.agentRowLabels {
		if l == label {
			m.agentsTbl.SetCursor(i)
			return
		}
	}
	t.Fatalf("no AGENTS row %q in %q", label, m.agentRowLabels)
}

// agentsCell is the cell under the column titled title in the AGENTS row labelled label, and
// whether the column is there at all.
func agentsCell(t *testing.T, m *model, label, title string) (string, bool) {
	t.Helper()
	col := -1
	for i, c := range m.agentsTbl.Columns() {
		if headerTitle(c) == title {
			col = i
		}
	}
	if col < 0 {
		return "", false
	}
	for i, l := range m.agentRowLabels {
		if l == label {
			return m.agentsTbl.Rows()[i][col], true
		}
	}
	t.Fatalf("no AGENTS row %q", label)
	return "", false
}

// The column is there only while the proxy runs the router, and says each agent's server — or
// own choice — with nothing on All agents.
func TestAgentsPane_ServerColumnFollowsTheRouter(t *testing.T) {
	m := serverModel(t, newFakeProxy(t, false))
	for label, want := range map[string]string{"": "", "claude-code/2.1.270": "ete", "opencode/1.0.3": agentOwnChoice} {
		if got, ok := agentsCell(t, m, label, "SERVER"); !ok || got != want {
			t.Errorf("row %q: SERVER = %q (present %v), want %q", label, got, ok, want)
		}
	}
	m.pipeline = &apiclient.PipelineView{Outbound: []apiclient.PipelinePlugin{{Name: "inference-parser"}}}
	m.rebuildAgentsTable()
	if _, ok := agentsCell(t, m, "claude-code/2.1.270", "SERVER"); ok {
		t.Error("SERVER is shown with no inference-router in the pipeline")
	}
}

// The SERVER cells carry no ANSI: the table truncates an escape as text. "own choice" is the
// dimming, in words.
func TestServerCells_CarryNoANSI(t *testing.T) {
	restore := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(restore) })
	m := serverModel(t, newFakeProxy(t, false))
	for r, row := range m.agentsTbl.Rows() {
		for c, cell := range row {
			if strings.ContainsRune(cell, 0x1b) {
				t.Errorf("AGENTS row %d cell %d carries an escape: %q", r, c, cell)
			}
		}
	}
}

// The pane's rows bring the pipeline with them, so a switch made in a shell — `agentop server
// use` — shows on the next refresh, and S opens on the value the proxy runs.
func TestAgentsPane_ARowsRefreshAlsoRefetchesThePipeline(t *testing.T) {
	f := newFakeProxy(t, false)
	m := serverModel(t, f)
	routed := strings.Replace(routerYAML, "            claude-code: ete\n", "            claude-code: ete\n            opencode: glm\n", 1)
	if err := os.WriteFile(f.path, []byte(routed), 0o600); err != nil {
		t.Fatal(err)
	}
	_, cmd := m.Update(agentRowsLoadedMsg{rows: m.agents, open: agentsOpenNever})
	if cmd == nil {
		t.Fatal("the rows' reply fetched no pipeline")
	}
	m.Update(cmd())
	if got, _ := agentsCell(t, m, "opencode/1.0.3", "SERVER"); got != "glm" {
		t.Errorf("SERVER = %q after the refresh, want glm", got)
	}
}

// routerPlugin stands in for the inference-router in a real pipeline: the session API serves its
// name, and its config through pipeline.WrapConfigured, as it serves the real plugin's.
type routerPlugin struct{}

func (routerPlugin) Name() string                              { return servers.PluginName }
func (routerPlugin) Capabilities() pipeline.PluginCapabilities { return pipeline.PluginCapabilities{} }
func (routerPlugin) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (routerPlugin) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// servePipeline is the URL of a real sessionapi server whose outbound pipeline runs the router,
// configured as routerRaw, under policy: the wire the TUI reads, not a hand-built view of it.
func servePipeline(t *testing.T, policy pipeline.ErrorPolicy) string {
	t.Helper()
	p, err := pipeline.New([]pipeline.Plugin{pipeline.WrapConfigured(routerPlugin{}, json.RawMessage(routerRaw))},
		pipeline.WithPolicies(policy))
	if err != nil {
		t.Fatal(err)
	}
	store := session.New(time.Minute, 10, 0)
	t.Cleanup(store.Close)
	ts := httptest.NewServer(sessionapi.New(":0", store, sessionapi.WithPipelines(nil, pipeline.NewHolder(p))).Server().Handler)
	t.Cleanup(ts.Close)
	return ts.URL
}

// attachTo points m at the session API at url as this machine's Cortex — its config file and
// stats server stay f's — and loads the pipeline the way the TUI does. The view m holds is kept
// until the reply replaces it, as a refetch keeps it: pipelineLoadedMsg repaints against it.
func attachTo(t *testing.T, m *model, url string) {
	t.Helper()
	m.client, m.localEndpoint = apiclient.New(url), url
	prev := m.pipeline
	m.Update(m.loadPipelineCmd()())
	if m.pipeline == prev {
		t.Fatalf("no pipeline loaded from %s", url)
	}
}

// observing is v with the router's entry under on_error: observe, as /v1/pipeline serves it.
func observing(v *apiclient.PipelineView) *apiclient.PipelineView {
	for i := range v.Outbound {
		if v.Outbound[i].Name == servers.PluginName {
			v.Outbound[i].OnError = pipeline.ErrorPolicyObserve
		}
	}
	return v
}

// A router under on_error: observe stays in the running pipeline, but moves nothing: it only
// records observe/would_route. So it is no router to the SERVER column or to S, which says why in
// the words `agentop server` uses — and writes nothing. Through a real session API, because the
// policy reaches the TUI only on the wire.
func TestAgentsPane_AnObservingRouterRoutesNothing(t *testing.T) {
	f := newFakeProxy(t, false)
	m := serverModel(t, f)
	attachTo(t, m, servePipeline(t, pipeline.ErrorPolicyEnforce))
	if got, _ := agentsCell(t, m, "claude-code/2.1.270", "SERVER"); got != "ete" {
		t.Fatalf("under enforce, SERVER = %q, want ete", got)
	}

	attachTo(t, m, servePipeline(t, pipeline.ErrorPolicyObserve))
	if got, ok := agentsCell(t, m, "claude-code/2.1.270", "SERVER"); ok {
		t.Errorf("SERVER = %q for a router under on_error: observe, which routes nothing", got)
	}
	if m.serverKeyOffered() {
		t.Error("the footer offers S for a router that routes nothing")
	}
	before := f.config(t)
	onRow(t, m, "opencode/1.0.3")
	if cmd := m.handleKey(keyRune('S')); cmd != nil || m.serverPicker != nil {
		t.Fatalf("S opened something: cmd %v, picker %+v", cmd, m.serverPicker)
	}
	if want := servers.Inactive(pipeline.ErrorPolicyObserve, f.path); m.serverNotice != want {
		t.Errorf("notice %q\nwant   %q", m.serverNotice, want)
	}
	if f.config(t) != before {
		t.Error("the config was written")
	}
}

// ↵ asks again rather than trusting the picker's opening: the pane's rows refresh the pipeline
// while the picker is up, and a router that has since stopped routing must not be written to, nor
// a "New … sessions →" flash claim a route that will not happen.
func TestServerPicker_EnterRefusesARouterThatStoppedRoutingWhileItWasOpen(t *testing.T) {
	f := newFakeProxy(t, false)
	m := serverModel(t, f)
	before := f.config(t)
	onRow(t, m, "opencode/1.0.3")
	m.handleKey(keyRune('S'))
	if m.serverPicker == nil {
		t.Fatalf("S opened no picker; flash %q", m.flash)
	}
	m.handleKey(keyDown)
	m.Update(pipelineLoadedMsg(observing(routerPipeline(routerRaw))))
	if cmd := m.handleKey(keyEnter); cmd != nil {
		t.Fatal("↵ started a write to a router under on_error: observe")
	}
	if want := servers.Inactive(pipeline.ErrorPolicyObserve, f.path); m.serverNotice != want {
		t.Errorf("notice %q\nwant   %q", m.serverNotice, want)
	}
	if m.serverSwitch != nil || f.config(t) != before {
		t.Errorf("a switch began (%+v) or the config was written", m.serverSwitch)
	}
}

// With no router on /v1/pipeline, S cannot tell a config with none from one whose router is under
// on_error: off — the proxy does not build an off plugin, so it does not list it — and says both.
func TestServerPicker_NoRouterNamesBothReasonsItMayBeMissing(t *testing.T) {
	f := newFakeProxy(t, false)
	m := serverModel(t, f)
	m.pipeline = &apiclient.PipelineView{Outbound: []apiclient.PipelinePlugin{{Name: "inference-parser"}}}
	m.rebuildAgentsTable()
	onRow(t, m, "opencode/1.0.3")
	m.handleKey(keyRune('S'))
	for _, want := range []string{"runs no inference-router", "agentop server add", "on_error: off", f.path} {
		if !strings.Contains(m.serverNotice, want) {
			t.Errorf("notice %q, want it to contain %q", m.serverNotice, want)
		}
	}
}

// A router config this side cannot decode is no router to show: a SERVER column read off a
// half-decoded config would name servers it never decoded, and S would write against it.
func TestActiveRouter_AConfigItCannotReadIsNotOn(t *testing.T) {
	m := serverModel(t, newFakeProxy(t, false))
	m.pipeline = routerPipeline(`{"servers":5}`)
	m.rebuildAgentsTable()
	if _, on := m.activeRouter(); on {
		t.Error("activeRouter is on for a config it could not decode")
	}
	if _, ok := agentsCell(t, m, "claude-code/2.1.270", "SERVER"); ok {
		t.Error("SERVER is shown for a router config agentop could not read")
	}
	onRow(t, m, "opencode/1.0.3")
	m.handleKey(keyRune('S'))
	if m.serverPicker != nil || !strings.Contains(m.serverNotice, "cannot read the inference-router's running config") {
		t.Errorf("S: picker %+v, notice %q", m.serverPicker, m.serverNotice)
	}
}

// A rows reply while a pipeline fetch is out starts no second one: the guard that keeps the 2s
// tick from stacking fetches against a slow endpoint holds here too.
func TestAgentsPane_ARowsReplyDoesNotStackAPipelineFetch(t *testing.T) {
	m := serverModel(t, newFakeProxy(t, false))
	m.pipelineFetching = true
	if _, cmd := m.Update(agentRowsLoadedMsg{rows: m.agents, open: agentsOpenNever}); cmd != nil {
		t.Error("a rows reply started a pipeline fetch while one was in flight")
	}
	if !m.pipelineFetching {
		t.Error("the in-flight flag was cleared by a reply that was not the pipeline's")
	}
}

// A pipeline reply repaints the agents table only when the router in it changed — its routing or
// whether it routes — since one lands every 2s on the pipeline panes.
func TestPipelineLoaded_RepaintsTheAgentsOnlyWhenTheRouterChanged(t *testing.T) {
	m := serverModel(t, newFakeProxy(t, false))
	// Rows that are not drawn yet: they appear only when something repaints.
	m.agents = append(m.agents, agentRow{label: "bob-shell/1.0", Counts: usage.Counts{Requests: 2}})
	m.Update(pipelineLoadedMsg(routerPipeline(routerRaw)))
	if slices.Contains(m.agentRowLabels, "bob-shell/1.0") {
		t.Error("an unchanged router repainted the agents table")
	}
	m.Update(pipelineLoadedMsg(routerPipeline(strings.Replace(routerRaw, `"claude-code":"ete"`, `"claude-code":"glm"`, 1))))
	if !slices.Contains(m.agentRowLabels, "bob-shell/1.0") {
		t.Error("a changed route did not repaint the agents table")
	}
	if got, _ := agentsCell(t, m, "claude-code/2.1.270", "SERVER"); got != "glm" {
		t.Errorf("SERVER = %q after the route changed, want glm", got)
	}
	m.Update(pipelineLoadedMsg(observing(routerPipeline(routerRaw))))
	if _, ok := agentsCell(t, m, "claude-code/2.1.270", "SERVER"); ok {
		t.Error("a router that stopped routing left the SERVER column up")
	}
}

// A SERVER cell names a server only where one can be chosen. Other pools many agents, and the
// no-User-Agent row is one the router refuses to route, so both show nothing rather than an
// "own choice" S would then refuse. A server name is sanitised like every other served label.
func TestAgentServerCell_NamesNoServerWhereNoneCanBeChosen(t *testing.T) {
	router := routerconfig.Config{Agents: map[string]string{"claude-code": "ete", "opencode": "\x1b[31mglm"}}
	for label, want := range map[string]string{
		"claude-code/2.1.270": "ete",
		"bob-shell/1.0":       agentOwnChoice,
		otherAgents:           "",
		routerconfig.NoAgent:  "",
		"opencode/1.0.3":      "�[31mglm",
	} {
		if got := agentServerCell(router, label); got != want {
			t.Errorf("agentServerCell(%q) = %q, want %q", label, got, want)
		}
	}
}

// S's reasons name the config file as `agentop server` prints it, under the home directory as
// ~/…, so the panel and the command say the same thing about the same file.
func TestServerNotice_NamesTheConfigFileAsAgentopServerDoes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const shown = "~/.cortex/config.yaml"
	for _, pv := range []*apiclient.PipelineView{
		observing(routerPipeline(routerRaw)),
		{Outbound: []apiclient.PipelinePlugin{{Name: "inference-parser"}}},
	} {
		m := serverModel(t, newFakeProxy(t, false))
		m.localConfigPath = filepath.Join(home, ".cortex", "config.yaml")
		m.pipeline = pv
		m.rebuildAgentsTable()
		onRow(t, m, "claude-code/2.1.270")
		m.handleKey(keyRune('S'))
		if !strings.Contains(m.serverNotice, shown) || strings.Contains(m.serverNotice, home) {
			t.Errorf("notice %q, want it to name %s", m.serverNotice, shown)
		}
	}
	m := serverModel(t, newFakeProxy(t, false))
	m.localConfigPath = filepath.Join(home, ".cortex", "config.yaml")
	m.pipeline = observing(routerPipeline(routerRaw))
	if _, why := m.routerStatus(); why != servers.Inactive(pipeline.ErrorPolicyObserve, shown) {
		t.Errorf("reason %q, want agentop server's words", why)
	}
}

// At 80 columns, with SESSIONS up, the SERVER column costs AGENT fourteen columns, 30 to 16, and
// that is deliberate (rebuildAgentsTable): SERVER is the one place the pane shows where each
// agent's new sessions go, and what it and S act on is the agent's NAME, which survives with its
// minor version — two patch versions' rows that then read alike are one agent, with one server.
// Pinned so a later column cannot cut into the name as well.
func TestAgentsPane_TheAgentsNameSurvivesTheServerColumnAt80(t *testing.T) {
	m := serverModel(t, newFakeProxy(t, false))
	m.width = 80
	m.sessions = []session.SessionSummary{{ID: "a", Agent: "claude-code/2.1.270"}}
	m.rebuildAgentsTable()
	if _, ok := agentsCell(t, m, "claude-code/2.1.270", "SERVER"); !ok {
		t.Fatal("no SERVER column at 80")
	}
	if _, ok := agentsCell(t, m, "claude-code/2.1.270", "SESSIONS"); !ok {
		t.Fatal("no SESSIONS column: the fixture no longer measures the narrowest case")
	}
	if got, floor := sessionsColumnWidth(m.agentsTbl.Columns(), "AGENT"), len("claude-code/2.1")+1; got < floor {
		t.Errorf("AGENT is %d columns at 80, under the %d that keeps an agent's name and minor version", got, floor)
	}
	if view := m.View(); !strings.Contains(view, "claude-code/2.1") {
		t.Errorf("the row does not show the agent's name:\n%s", view)
	}
}
