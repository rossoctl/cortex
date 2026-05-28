package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kagenti/kagenti-extensions/authbridge/cmd/abctl/apiclient"
	"github.com/kagenti/kagenti-extensions/authbridge/cmd/abctl/cluster"
)

// fakeLister returns a fixed []AgentNamespace.
type fakeLister struct{ namespaces []cluster.AgentNamespace }

func (f *fakeLister) ListAgents(ctx context.Context) ([]cluster.AgentNamespace, error) {
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

// fakePortForwarder returns a no-op PortForward.
type fakePortForwarder struct {
	startedNs  string
	startedPod string
	endpoint   string
	startErr   error
	closeCount int
}

func (f *fakePortForwarder) Start(ctx context.Context, ns, pod string) (cluster.PortForward, error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	f.startedNs, f.startedPod = ns, pod
	return &fakePortForward{endpoint: f.endpoint, parent: f}, nil
}

type fakePortForward struct {
	endpoint string
	parent   *fakePortForwarder
}

func (p *fakePortForward) Endpoint() string { return p.endpoint }
func (p *fakePortForward) Close() error     { p.parent.closeCount++; return nil }

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
