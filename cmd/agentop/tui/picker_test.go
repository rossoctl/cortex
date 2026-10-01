package tui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/cmd/agentop/cluster"
	"github.com/rossoctl/cortex/cmd/agentop/edit"
)

// fakeLister returns a fixed []AgentNamespace and counts ListAgents calls.
type fakeLister struct {
	namespaces []cluster.AgentNamespace
	calls      int
}

func (f *fakeLister) ListAgents(ctx context.Context) ([]cluster.AgentNamespace, error) {
	f.calls++
	return f.namespaces, nil
}

// fixtureNamespaces is a small, deterministic dataset for picker tests.
var fixtureNamespaces = []cluster.AgentNamespace{
	{Name: "team1", Pods: []cluster.Pod{
		{Namespace: "team1", Name: "weather-agent-1", Phase: "Running", Ready: true},
	}},
	{Name: "team2", Pods: []cluster.Pod{
		{Namespace: "team2", Name: "billing-agent-1", Phase: "Pending", Ready: false},
	}},
}

func TestNamespacesPaneLoadsAndRenders(t *testing.T) {
	m := newPickerModel(context.Background(), &fakeLister{namespaces: fixtureNamespaces}, nil)
	// Init returns a Cmd that loads the agents.
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init returned nil cmd; want loader cmd")
	}
	msg := cmd()
	loaded, ok := msg.(agentsLoadedMsg)
	if !ok {
		t.Fatalf("loader cmd produced %T, want agentsLoadedMsg", msg)
	}
	updated, _ := m.Update(loaded)
	mm := updated.(*model)
	if len(mm.namespaces) != 2 {
		t.Fatalf("model should hold 2 namespaces, got %d", len(mm.namespaces))
	}
	view := mm.View()
	if !strings.Contains(view, "team1") || !strings.Contains(view, "team2") {
		t.Fatalf("rendered view missing namespaces:\n%s", view)
	}
}

func TestNamespacesPaneEmptyState(t *testing.T) {
	// Lister returns no agents — the pane should render an actionable hint
	// instead of an empty table.
	m := newPickerModel(context.Background(), &fakeLister{namespaces: []cluster.AgentNamespace{}}, nil)
	loaded := m.Init()()
	updated, _ := m.Update(loaded)
	mm := updated.(*model)
	view := mm.View()
	if !strings.Contains(view, "No Cortex agents found") {
		t.Fatalf("empty-state hint missing from view:\n%s", view)
	}
	if !strings.Contains(view, "--endpoint") {
		t.Fatalf("empty-state hint should mention --endpoint:\n%s", view)
	}
}

func TestNamespacesPaneDrillsIntoPods(t *testing.T) {
	m := newPickerModel(context.Background(), &fakeLister{namespaces: fixtureNamespaces}, nil)
	loaded := m.Init()()
	updated, _ := m.Update(loaded)
	mm := updated.(*model)
	// Press Enter on the first row.
	updated, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mm = updated.(*model)
	if mm.pane != panePods {
		t.Fatalf("after Enter, active pane should be panePods, got %v", mm.pane)
	}
	if mm.selectedNamespace != "team1" {
		t.Fatalf("selected namespace should be team1, got %q", mm.selectedNamespace)
	}
}

func TestPodsPaneListsPods(t *testing.T) {
	m := newPickerModel(context.Background(), &fakeLister{namespaces: fixtureNamespaces}, nil)
	loaded := m.Init()()
	updated, _ := m.Update(loaded)
	mm := updated.(*model)
	// Drill into team1.
	updated, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mm = updated.(*model)
	view := mm.View()
	if !strings.Contains(view, "weather-agent-1") {
		t.Fatalf("Pods view missing pod name:\n%s", view)
	}
	if !strings.Contains(view, "Running") {
		t.Fatalf("Pods view missing phase column:\n%s", view)
	}
}

func TestPodsPaneEscBacksOut(t *testing.T) {
	m := newPickerModel(context.Background(), &fakeLister{namespaces: fixtureNamespaces}, nil)
	updated, _ := m.Update(m.Init()())
	mm := updated.(*model)
	updated, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEnter}) // → panePods
	updated, _ = updated.(*model).Update(tea.KeyMsg{Type: tea.KeyEsc})
	mm = updated.(*model)
	if mm.pane != paneNamespaces {
		t.Fatalf("Esc should back out to Namespaces, got pane %v", mm.pane)
	}
}

func TestRefreshKeybindReloadsAgents(t *testing.T) {
	lister := &fakeLister{namespaces: fixtureNamespaces}
	m := newPickerModel(context.Background(), lister, nil)
	// Initial load: 1 call.
	updated, _ := m.Update(m.Init()())
	mm := updated.(*model)
	if lister.calls != 1 {
		t.Fatalf("after Init: lister.calls = %d, want 1", lister.calls)
	}
	mm.pickerErr = "stale error from earlier"

	// `r` from paneNamespaces should re-run loadAgentsCmd and clear the error.
	_, cmd := mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if cmd == nil {
		t.Fatal("`r` on paneNamespaces should produce a Cmd to reload")
	}
	if mm.pickerErr != "" {
		t.Fatalf("`r` should clear pickerErr, got %q", mm.pickerErr)
	}
	// Run the Cmd and feed its message through Update so the post-reload
	// transition (clear m.loading, rebuild table) actually runs.
	updated, _ = mm.Update(cmd())
	mm = updated.(*model)
	if lister.calls != 2 {
		t.Fatalf("after r on paneNamespaces: lister.calls = %d, want 2", lister.calls)
	}
	if mm.loading {
		t.Fatal("agentsLoadedMsg should clear m.loading")
	}

	// Drill into team1, press `r` from panePods. Same effect.
	updated, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mm = updated.(*model)
	if mm.pane != panePods {
		t.Fatalf("setup: not on panePods, got %v", mm.pane)
	}
	_, cmd = mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if cmd == nil {
		t.Fatal("`r` on panePods should produce a Cmd to reload")
	}
	updated, _ = mm.Update(cmd())
	mm = updated.(*model)
	if lister.calls != 3 {
		t.Fatalf("after r on panePods: lister.calls = %d, want 3", lister.calls)
	}
	// Pane stays on panePods (selectedNamespace still exists in the new data).
	if mm.pane != panePods {
		t.Fatalf("after r on panePods with stable data: pane = %v, want panePods", mm.pane)
	}
}

func TestRefreshKeybindIgnoredWhileLoading(t *testing.T) {
	lister := &fakeLister{namespaces: fixtureNamespaces}
	m := newPickerModel(context.Background(), lister, nil)
	// Init dispatches loadAgentsCmd and sets m.loading = true. We don't
	// execute the Cmd yet — that simulates the load being in flight.
	initCmd := m.Init()
	if initCmd == nil {
		t.Fatal("Init should return a Cmd")
	}
	if !m.loading {
		t.Fatal("Init should set m.loading = true")
	}
	// `r` while loading: the gate is purely on m.loading, regardless of
	// whether the in-flight Cmd has run yet. No new dispatch.
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if cmd != nil {
		t.Fatal("`r` while loading should return nil Cmd")
	}
	// Complete the original load; loading flag clears.
	_, _ = m.Update(initCmd())
	if m.loading {
		t.Fatal("agentsLoadedMsg should clear m.loading")
	}
	if lister.calls != 1 {
		t.Fatalf("only the original load should have run: lister.calls = %d, want 1", lister.calls)
	}
	// Now `r` works again.
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if cmd == nil {
		t.Fatal("`r` after load completes should produce a Cmd")
	}
	_ = cmd()
	if lister.calls != 2 {
		t.Fatalf("after second r: lister.calls = %d, want 2", lister.calls)
	}
}

func TestRefreshDropsBackOutWhenSelectedNamespaceVanishes(t *testing.T) {
	// Drill into team1, then the lister's underlying data shifts to
	// only-team2. After `r`, the user should land back on paneNamespaces
	// since team1 no longer exists.
	lister := &fakeLister{namespaces: fixtureNamespaces}
	m := newPickerModel(context.Background(), lister, nil)
	updated, _ := m.Update(m.Init()())
	mm := updated.(*model)
	updated, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEnter}) // → panePods on team1
	mm = updated.(*model)
	if mm.pane != panePods {
		t.Fatalf("setup: not on panePods, got %v", mm.pane)
	}

	// Cluster state changes — only team2 remains.
	lister.namespaces = []cluster.AgentNamespace{
		{Name: "team2", Pods: []cluster.Pod{
			{Namespace: "team2", Name: "billing-agent-1", Phase: "Pending", Ready: false},
		}},
	}
	// Press `r`, deliver the new agentsLoadedMsg.
	_, cmd := mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	updated, _ = mm.Update(cmd())
	mm = updated.(*model)

	if mm.pane != paneNamespaces {
		t.Fatalf("after vanished-namespace reload, pane should be paneNamespaces, got %v", mm.pane)
	}
	if mm.selectedNamespace != "" {
		t.Fatalf("selectedNamespace should be cleared, got %q", mm.selectedNamespace)
	}
}

// fakePortForwarder returns a no-op PortForward.
type fakePortForwarder struct {
	startedNs  string
	startedPod string
	endpoint   string
	// statusEndpoint is what the forward reports as its :9093 stats URL. Empty
	// for most tests, which never read it; set where the model's statusURL
	// matters — see TestLocalConnectClearsPodIdentity.
	statusEndpoint string
	startErr       error
	closeCount     int
}

func (f *fakePortForwarder) Start(ctx context.Context, ns, pod string) (cluster.PortForward, error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	f.startedNs, f.startedPod = ns, pod
	return &fakePortForward{endpoint: f.endpoint, statusEndpoint: f.statusEndpoint, parent: f}, nil
}

type fakePortForward struct {
	endpoint       string
	statusEndpoint string
	parent         *fakePortForwarder
}

func (p *fakePortForward) Endpoint() string       { return p.endpoint }
func (p *fakePortForward) StatusEndpoint() string { return p.statusEndpoint }
func (p *fakePortForward) Close() error           { p.parent.closeCount++; return nil }

func TestPodEnterStartsPortForwardAndTransitions(t *testing.T) {
	pf := &fakePortForwarder{endpoint: "http://127.0.0.1:60000"}
	m := newPickerModel(context.Background(), &fakeLister{namespaces: fixtureNamespaces}, pf)
	updated, _ := m.Update(m.Init()())
	mm := updated.(*model)
	updated, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEnter}) // → panePods
	mm = updated.(*model)
	updated, cmd := mm.Update(tea.KeyMsg{Type: tea.KeyEnter}) // start PF
	mm = updated.(*model)
	if cmd == nil {
		t.Fatal("Enter on pod should produce a Cmd to start the PF")
	}
	msg := cmd()
	conn, ok := msg.(portForwardReadyMsg)
	if !ok {
		t.Fatalf("PF cmd produced %T, want portForwardReadyMsg", msg)
	}
	updated, _ = mm.Update(conn)
	mm = updated.(*model)
	if mm.pane != paneSessions {
		t.Fatalf("after PF ready, pane should be paneSessions, got %v", mm.pane)
	}
	if mm.endpoint != "http://127.0.0.1:60000" {
		t.Fatalf("model endpoint not set: %q", mm.endpoint)
	}
	if pf.startedNs != "team1" || pf.startedPod != "weather-agent-1" {
		t.Fatalf("PortForwarder.Start not called with selection: ns=%q pod=%q", pf.startedNs, pf.startedPod)
	}
}

func TestPodEnterSurfacesPortForwardError(t *testing.T) {
	pf := &fakePortForwarder{startErr: fmt.Errorf("forbidden")}
	m := newPickerModel(context.Background(), &fakeLister{namespaces: fixtureNamespaces}, pf)
	updated, _ := m.Update(m.Init()())
	mm := updated.(*model)
	updated, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEnter}) // → panePods
	mm = updated.(*model)
	updated, cmd := mm.Update(tea.KeyMsg{Type: tea.KeyEnter}) // start PF
	mm = updated.(*model)
	updated, _ = mm.Update(cmd())
	mm = updated.(*model)
	if mm.pane != panePods {
		t.Fatalf("PF error should keep us on panePods, got %v", mm.pane)
	}
	if !strings.Contains(mm.pickerErr, "forbidden") {
		t.Fatalf("error not surfaced in pickerErr: %q", mm.pickerErr)
	}
}

func TestRunOptionsWiringEndpointBypass(t *testing.T) {
	// Endpoint set → no Lister/PF needed; the function should not panic
	// and should return promptly when the context is cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	opts := RunOptions{Endpoint: "http://127.0.0.1:1"}
	// Run will exit because ctx is already cancelled; we just verify
	// it doesn't dereference nil Lister/PortForwarder.
	_ = Run(ctx, opts)
}

func TestEscFromSessionsReturnsToPods(t *testing.T) {
	pf := &fakePortForwarder{endpoint: "http://127.0.0.1:60001"}
	m := newPickerModel(context.Background(), &fakeLister{namespaces: fixtureNamespaces}, pf)
	// Drill: agents loaded → namespaces → pods → port-forward → sessions
	updated, _ := m.Update(m.Init()())
	mm := updated.(*model)
	updated, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mm = updated.(*model)
	updated, cmd := mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mm = updated.(*model)
	updated, _ = mm.Update(cmd())
	mm = updated.(*model)
	if mm.pane != paneSessions {
		t.Fatalf("setup failed: not in paneSessions, got %v", mm.pane)
	}
	if mm.activePF == nil {
		t.Fatal("setup failed: activePF should be set")
	}

	// Esc from Sessions should return to Pods.
	updated, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEsc})
	mm = updated.(*model)
	if mm.pane != panePods {
		t.Fatalf("after Esc, pane should be panePods, got %v", mm.pane)
	}
	if mm.activePF != nil {
		t.Fatal("activePF should have been closed and cleared")
	}
	if pf.closeCount != 1 {
		t.Fatalf("PortForward.Close should have been called once, got %d", pf.closeCount)
	}
	if mm.client != nil {
		t.Fatal("m.client should have been cleared")
	}
	if mm.cancel == nil {
		t.Fatal("m.cancel should have been re-derived from parentCtx, not left nil")
	}
}

func TestEscFromSessionsNoOpInBypassMode(t *testing.T) {
	// Build a bypass-mode model directly (no picker).
	ctx := context.Background()
	c := apiclient.New("http://127.0.0.1:1")
	m := New(ctx, c).(*model)
	// No parentCtx. Esc on paneSessions should be a no-op.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	mm := updated.(*model)
	if mm.pane != paneSessions {
		t.Fatalf("bypass mode Esc should NOT change pane, got %v", mm.pane)
	}
}

func TestRefreshTickAfterEscDoesNotPanic(t *testing.T) {
	pf := &fakePortForwarder{endpoint: "http://127.0.0.1:60002"}
	m := newPickerModel(context.Background(), &fakeLister{namespaces: fixtureNamespaces}, pf)
	// Drill all the way to paneSessions.
	updated, _ := m.Update(m.Init()())
	mm := updated.(*model)
	updated, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mm = updated.(*model)
	updated, cmd := mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mm = updated.(*model)
	updated, _ = mm.Update(cmd())
	mm = updated.(*model)
	if mm.pane != paneSessions {
		t.Fatalf("setup failed: not in paneSessions, got %v", mm.pane)
	}
	// Esc back to Pods. m.client is now nil.
	updated, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEsc})
	mm = updated.(*model)
	if mm.pane != panePods {
		t.Fatalf("after Esc, expected panePods, got %v", mm.pane)
	}
	// A late refreshTickMsg should not panic.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("tick/stream msg in picker pane panicked: %v", r)
		}
	}()
	_, _ = mm.Update(refreshTickMsg(time.Now()))
	// A late tickMsg should not panic.
	_, _ = mm.Update(tickMsg(time.Now()))
	// A late streamMsg should not panic.
	_, _ = mm.Update(streamMsg{})
	// A late streamClosedMsg should not panic.
	_, _ = mm.Update(streamClosedMsg{})
}

// silence unused-import nag if test build trims this file later
var _ = time.Second

// --- [l] localhost:9094 shortcut -----------------------------------------

// sessionAPIStub serves the minimum /v1/sessions response the `[l]` probe
// needs, so the shortcut can be exercised end-to-end against a real HTTP
// endpoint rather than a mocked client.
func sessionAPIStub(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sessions":[]}`))
	})
	mux.HandleFunc("/v1/pipeline", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"inbound":[],"outbound":[]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestLocalhostKeybindConnects(t *testing.T) {
	srv := sessionAPIStub(t)
	m := newPickerModel(context.Background(), &fakeLister{namespaces: fixtureNamespaces}, nil)
	updated, _ := m.Update(m.Init()())
	mm := updated.(*model)

	// `l` should dispatch a probe Cmd and mark the model loading.
	_, cmd := mm.Update(keyRune('l'))
	if cmd == nil {
		t.Fatal("`l` on paneNamespaces should produce a connect Cmd")
	}
	if !mm.loading {
		t.Fatal("`l` should set m.loading while the probe is in flight")
	}

	// Run the probe against the stub (not the real localhost:9094) and feed
	// the result back through Update.
	msg := connectLocalCmd(context.Background(), srv.URL)()
	lc, ok := msg.(localConnectedMsg)
	if !ok {
		t.Fatalf("connectLocalCmd produced %T, want localConnectedMsg", msg)
	}
	if lc.err != nil {
		t.Fatalf("probe against stub failed: %v", lc.err)
	}
	updated, _ = mm.Update(lc)
	mm = updated.(*model)

	if mm.pane != paneSessions {
		t.Fatalf("after `l` connect, pane should be paneSessions, got %v", mm.pane)
	}
	if mm.client == nil {
		t.Fatal("after `l` connect, m.client should be set")
	}
	if mm.endpoint != srv.URL {
		t.Fatalf("endpoint = %q, want %q", mm.endpoint, srv.URL)
	}
	if !mm.localDirect {
		t.Fatal("localDirect should be true after connecting via `l`")
	}
	if mm.activePF != nil {
		t.Fatal("`l` must not create a port-forward")
	}
	if mm.loading {
		t.Fatal("localConnectedMsg should clear m.loading")
	}
}

func TestLocalhostKeybindProbeFailureStaysInPicker(t *testing.T) {
	// Point at a closed port so the probe fails fast.
	srv := sessionAPIStub(t)
	dead := srv.URL
	srv.Close()

	m := newPickerModel(context.Background(), &fakeLister{namespaces: fixtureNamespaces}, nil)
	updated, _ := m.Update(m.Init()())
	mm := updated.(*model)

	msg := connectLocalCmd(context.Background(), dead)()
	lc := msg.(localConnectedMsg)
	if lc.err == nil {
		t.Fatal("probe against a closed port should fail")
	}
	updated, _ = mm.Update(lc)
	mm = updated.(*model)

	if mm.pane != paneNamespaces {
		t.Fatalf("failed probe should leave the user in the picker, got pane %v", mm.pane)
	}
	if mm.client != nil {
		t.Fatal("failed probe must not set a client")
	}
	if mm.pickerErr == "" {
		t.Fatal("failed probe should surface an error in the footer")
	}
	if !strings.Contains(mm.pickerErr, "localhost:9094") {
		t.Fatalf("picker error should name the endpoint it tried, got %q", mm.pickerErr)
	}
	if mm.loading {
		t.Fatal("failed probe should clear m.loading")
	}
}

// Esc out of a session entered via `[l]` has no pod to return to, so it
// must land on Namespaces rather than an empty Pods table.
func TestLocalhostEscReturnsToNamespaces(t *testing.T) {
	srv := sessionAPIStub(t)
	m := newPickerModel(context.Background(), &fakeLister{namespaces: fixtureNamespaces}, nil)
	updated, _ := m.Update(m.Init()())
	mm := updated.(*model)
	updated, _ = mm.Update(connectLocalCmd(context.Background(), srv.URL)())
	mm = updated.(*model)
	if mm.pane != paneSessions {
		t.Fatalf("setup: expected paneSessions, got %v", mm.pane)
	}
	updated, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEsc})
	mm = updated.(*model)
	if mm.pane != paneNamespaces {
		t.Fatalf("Esc from an `l`-entered session should return to Namespaces, got %v", mm.pane)
	}
	if mm.localDirect {
		t.Fatal("localDirect should be cleared on back-out")
	}
}

// The namespaces footer and empty state must advertise `[l]`, otherwise
// the shortcut is undiscoverable.
func TestLocalhostKeybindIsAdvertised(t *testing.T) {
	m := newPickerModel(context.Background(), &fakeLister{namespaces: fixtureNamespaces}, nil)
	updated, _ := m.Update(m.Init()())
	mm := updated.(*model)
	updated, _ = mm.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	mm = updated.(*model)
	if !strings.Contains(mm.View(), "[l] localhost:9094") {
		t.Fatalf("namespaces footer should advertise [l]:\n%s", mm.View())
	}

	// Empty-state hint: the no-agents case is exactly when `l` is most
	// useful, so it should be mentioned there too.
	m2 := newPickerModel(context.Background(), &fakeLister{namespaces: []cluster.AgentNamespace{}}, nil)
	u2, _ := m2.Update(m2.Init()())
	mm2 := u2.(*model)
	if !strings.Contains(mm2.View(), "[l]") {
		t.Fatalf("empty-state hint should mention [l]:\n%s", mm2.View())
	}
}

// `l` must not be swallowed when the picker is mid-load, and must not
// hijack the vim-right binding in the session panes.
func TestLocalhostKeybindScoping(t *testing.T) {
	srv := sessionAPIStub(t)
	m := newPickerModel(context.Background(), &fakeLister{namespaces: fixtureNamespaces}, nil)
	updated, _ := m.Update(m.Init()())
	mm := updated.(*model)
	mm.loading = true
	_, cmd := mm.Update(keyRune('l'))
	if cmd != nil {
		t.Fatal("`l` while loading should be a no-op")
	}
	mm.loading = false

	// In the sessions pane, `l` is vim-right (drill in), not connect-local.
	updated, _ = mm.Update(connectLocalCmd(context.Background(), srv.URL)())
	mm = updated.(*model)
	mm.pane = paneSessions
	before := mm.endpoint
	_, _ = mm.Update(keyRune('l'))
	if mm.endpoint != before {
		t.Fatal("`l` in the sessions pane should not re-trigger a local connect")
	}
}

// localConnected drives a picker model through an `[l]` connect to srv and
// leaves it on the pipeline pane, ready for an `e` press.
func localConnected(t *testing.T, srvURL string) *model {
	t.Helper()
	m := newPickerModel(context.Background(), &fakeLister{namespaces: fixtureNamespaces}, nil)
	updated, _ := m.Update(m.Init()())
	mm := updated.(*model)
	updated, _ = mm.Update(connectLocalCmd(context.Background(), srvURL)())
	mm = updated.(*model)
	mm.pane = panePipeline
	return mm
}

// A connection with no pod/namespace AND no known local config has nothing to
// edit, so `e` must say so rather than open a broken edit. The message has to
// name a remedy: the old one blamed `--endpoint`, which a bare `agentop` that
// auto-connected to a local Cortex never passed.
func TestEditUnavailableWithoutAStore(t *testing.T) {
	srv := sessionAPIStub(t)
	mm := localConnected(t, srv.URL)

	updated, cmd := mm.Update(keyRune('e'))
	mm = updated.(*model)
	if cmd != nil {
		t.Fatal("`e` with no store should not start an edit")
	}
	if mm.editState.phase != editPhaseDone {
		t.Fatalf("`e` should not enter an edit phase, got %v", mm.editState.phase)
	}
	if !strings.Contains(mm.flash, "--kubernetes") {
		t.Errorf("flash should point at a remedy, got %q", mm.flash)
	}
	if strings.Contains(mm.flash, "--endpoint") {
		t.Errorf("flash blames a flag the user did not pass, got %q", mm.flash)
	}
}

// The point of the local store: when the endpoint on screen IS this machine's
// Cortex, `e` edits its config file — no picker, no pod, no kubectl. Nothing
// about how agentop was launched gates this, only what it is connected to.
func TestEditUsesTheLocalFileStoreWhenConnectedLocally(t *testing.T) {
	srv := sessionAPIStub(t)
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("mode: proxy-sidecar\npipeline:\n  outbound: []\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	mm := localConnected(t, srv.URL)
	// What main.go passes when a local Cortex answered. localEndpoint must equal
	// the connected endpoint — that equality is the whole discriminator.
	mm.localEndpoint = mm.client.Endpoint()
	mm.localConfigPath = cfgPath
	mm.localStatsURL = "http://127.0.0.1:47602"

	updated, cmd := mm.Update(keyRune('e'))
	mm = updated.(*model)
	if cmd == nil {
		t.Fatal("`e` should start an edit against the local config")
	}
	if mm.editState.phase != editPhaseFetching {
		t.Fatalf("phase = %v, want fetching", mm.editState.phase)
	}
	fs, ok := mm.editState.store.(edit.FileStore)
	if !ok {
		t.Fatalf("store is %T, want edit.FileStore", mm.editState.store)
	}
	if fs.Path != cfgPath {
		t.Errorf("FileStore.Path = %q, want %q", fs.Path, cfgPath)
	}
	// The store and the polled endpoint must describe the same proxy, or a
	// local write would be confirmed against somebody else's reload status.
	if mm.editState.statusURL != "http://127.0.0.1:47602" {
		t.Errorf("statusURL = %q, want the local stats URL", mm.editState.statusURL)
	}
}

// dialURL emits http://127.0.0.1:47601 for the built-in config, but an
// operator types --endpoint http://localhost:47601. An exact string compare
// refused that and told them to "point agentop at a Cortex running on this
// machine" — which they had just done.
func TestSameEndpoint(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"http://127.0.0.1:47601", "http://127.0.0.1:47601", true},
		{"http://localhost:47601", "http://127.0.0.1:47601", true},
		{"http://127.0.0.1:47601", "http://localhost:47601", true},
		{"http://[::1]:47601", "http://127.0.0.1:47601", true},
		// Path is not part of the comparison, so a trailing slash still names
		// the same server. apiclient.New trims it anyway; this holds even if it
		// stops.
		{"http://localhost:47601", "http://localhost:47601/", true},
		// A different port is a different proxy, loopback or not.
		{"http://localhost:9094", "http://127.0.0.1:47601", false},
		// A non-loopback host must match outright: a config bound to a LAN
		// address is not "this machine" for anyone else's purposes.
		{"http://192.168.1.10:47601", "http://127.0.0.1:47601", false},
		{"http://192.168.1.10:47601", "http://192.168.1.10:47601", true},
		{"http://cortex.example:47601", "http://localhost:47601", false},
		{"://nonsense", "http://localhost:47601", false},
	} {
		if got := sameEndpoint(tc.a, tc.b); got != tc.want {
			t.Errorf("sameEndpoint(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// The end-to-end consequence of the above: connecting with the human spelling
// still gets the local file store.
func TestEditUsesLocalStoreAcrossLoopbackSpellings(t *testing.T) {
	srv := sessionAPIStub(t)
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("mode: proxy-sidecar\npipeline:\n  outbound: []\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	mm := localConnected(t, srv.URL)
	// The connected endpoint is 127.0.0.1:<port> (httptest); the config-derived
	// localEndpoint spells the same server as localhost.
	u, err := url.Parse(mm.client.Endpoint())
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	mm.localEndpoint = "http://localhost:" + u.Port()
	mm.localConfigPath = cfgPath
	mm.localStatsURL = "http://127.0.0.1:47602"

	updated, cmd := mm.Update(keyRune('e'))
	mm = updated.(*model)
	if cmd == nil {
		t.Fatal("`e` should start an edit despite the differing loopback spelling")
	}
	if _, ok := mm.editState.store.(edit.FileStore); !ok {
		t.Fatalf("store is %T, want edit.FileStore", mm.editState.store)
	}
}

// Visit a pod, Esc out, then connect to the local Cortex with `[l]`. The pod's
// identity must not survive into that session.
//
// backToPodsPane clears ~20 fields but not selectedPod / selectedNamespace /
// statusURL, so before this was fixed all three still named the pod and its
// now-closed port-forward. pipelineStore tries local first and falls through to
// the ConfigMap branch — whose conditions those three stale fields satisfy — so
// `e` would fetch and kubectl-apply against a pod the screen was not showing,
// polling a port-forward that `[l]` never established. Latent until this became
// a store selector; a wrong-target write once it did.
func TestLocalConnectClearsPodIdentity(t *testing.T) {
	srv := sessionAPIStub(t)
	pf := &fakePortForwarder{
		endpoint:       "http://127.0.0.1:60001",
		statusEndpoint: "http://127.0.0.1:60002",
	}
	m := newPickerModel(context.Background(), &fakeLister{namespaces: fixtureNamespaces}, pf)
	updated, _ := m.Update(m.Init()())
	mm := updated.(*model)
	// Drill namespaces → pods → port-forward → sessions.
	updated, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mm = updated.(*model)
	updated, cmd := mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mm = updated.(*model)
	updated, _ = mm.Update(cmd())
	mm = updated.(*model)
	if mm.selectedPod == "" || mm.statusURL == "" {
		t.Fatalf("setup failed: expected a pod and a statusURL, got %q / %q", mm.selectedPod, mm.statusURL)
	}

	// Esc back to Pods, then `[l]` to the local Cortex.
	updated, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEsc})
	mm = updated.(*model)
	updated, _ = mm.Update(connectLocalCmd(context.Background(), srv.URL)())
	mm = updated.(*model)

	if mm.selectedPod != "" || mm.selectedNamespace != "" {
		t.Errorf("pod identity survived `[l]`: namespace=%q pod=%q", mm.selectedNamespace, mm.selectedPod)
	}
	if mm.statusURL != "" {
		t.Errorf("statusURL survived `[l]`: %q — it names a closed port-forward", mm.statusURL)
	}

	// The consequence: `e` must not silently pick the pod's ConfigMap. With no
	// local config path configured there is no store at all, so it refuses.
	mm.pane = panePipeline
	updated, ecmd := mm.Update(keyRune('e'))
	mm = updated.(*model)
	if store, _ := mm.pipelineStore(); store != nil {
		t.Errorf("pipelineStore returned %T after `[l]`, want nil", store)
	}
	if ecmd != nil {
		t.Error("`e` started an edit against stale pod state")
	}
}

// A local Cortex running on this machine must not make `e` edit its file while
// the operator is looking at a POD. The endpoint decides, not mere presence of
// a local install.
func TestEditPrefersThePodWhenViewingAPod(t *testing.T) {
	srv := sessionAPIStub(t)
	mm := localConnected(t, srv.URL)
	// A local install exists and answered, but at a different address than the
	// one on screen — which is what viewing a pod through a port-forward looks
	// like.
	mm.localEndpoint = "http://127.0.0.1:47601"
	mm.localConfigPath = "/Users/somebody/.cortex/config.yaml"
	mm.localStatsURL = "http://127.0.0.1:47602"
	mm.editRunner = func(context.Context, ...string) ([]byte, error) { return nil, nil }
	mm.selectedNamespace, mm.selectedPod = "team1", "email-agent"
	mm.statusURL = "http://127.0.0.1:19093"

	updated, _ := mm.Update(keyRune('e'))
	mm = updated.(*model)
	if _, ok := mm.editState.store.(edit.ConfigMapStore); !ok {
		t.Fatalf("store is %T, want edit.ConfigMapStore — a local install must not hijack a pod edit", mm.editState.store)
	}
	if mm.editState.statusURL != "http://127.0.0.1:19093" {
		t.Errorf("statusURL = %q, want the port-forward's", mm.editState.statusURL)
	}
}
