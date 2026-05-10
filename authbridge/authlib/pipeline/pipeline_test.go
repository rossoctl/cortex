package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubPlugin is a minimal Plugin implementation for testing.
type stubPlugin struct {
	name   string
	caps   PluginCapabilities
	onReq  func(ctx context.Context, pctx *Context) Action
	onResp func(ctx context.Context, pctx *Context) Action
}

func (s *stubPlugin) Name() string                     { return s.name }
func (s *stubPlugin) Capabilities() PluginCapabilities { return s.caps }
func (s *stubPlugin) OnRequest(ctx context.Context, pctx *Context) Action {
	if s.onReq != nil {
		return s.onReq(ctx, pctx)
	}
	return Action{Type: Continue}
}
func (s *stubPlugin) OnResponse(ctx context.Context, pctx *Context) Action {
	if s.onResp != nil {
		return s.onResp(ctx, pctx)
	}
	return Action{Type: Continue}
}

func TestPipelineRun_EmptyPipeline(t *testing.T) {
	p, err := New(nil)
	if err != nil {
		t.Fatalf("New(nil) returned error: %v", err)
	}
	pctx := &Context{}
	action := p.Run(context.Background(), pctx)
	if action.Type != Continue {
		t.Errorf("empty pipeline returned %v, want Continue", action.Type)
	}
}

func TestPipelineRun_Sequential(t *testing.T) {
	var order []string
	p1 := &stubPlugin{
		name: "first",
		onReq: func(_ context.Context, pctx *Context) Action {
			order = append(order, "first")
			pctx.Extensions.Custom = map[string]any{"key": "value"}
			return Action{Type: Continue}
		},
	}
	p2 := &stubPlugin{
		name: "second",
		caps: PluginCapabilities{Reads: []string{"custom"}},
		onReq: func(_ context.Context, pctx *Context) Action {
			order = append(order, "second")
			if pctx.Extensions.Custom["key"] != "value" {
				t.Error("second plugin did not see mutation from first")
			}
			return Action{Type: Continue}
		},
	}
	p1.caps = PluginCapabilities{Writes: []string{"custom"}}

	pipe, err := New([]Plugin{p1, p2})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	pctx := &Context{}
	action := pipe.Run(context.Background(), pctx)
	if action.Type != Continue {
		t.Errorf("got %v, want Continue", action.Type)
	}
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Errorf("execution order = %v, want [first second]", order)
	}
}

func TestPipelineRun_Reject(t *testing.T) {
	called := false
	p1 := &stubPlugin{
		name: "rejecter",
		onReq: func(_ context.Context, _ *Context) Action {
			return Deny("policy.forbidden", "forbidden")
		},
	}
	p2 := &stubPlugin{
		name: "never-called",
		onReq: func(_ context.Context, _ *Context) Action {
			called = true
			return Action{Type: Continue}
		},
	}
	pipe, err := New([]Plugin{p1, p2})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	pctx := &Context{}
	action := pipe.Run(context.Background(), pctx)
	if action.Type != Reject {
		t.Errorf("got %v, want Reject", action.Type)
	}
	if action.Violation == nil {
		t.Fatal("violation not populated")
	}
	if action.Violation.Code != "policy.forbidden" {
		t.Errorf("code = %q, want policy.forbidden", action.Violation.Code)
	}
	if action.Violation.Reason != "forbidden" {
		t.Errorf("reason = %q, want forbidden", action.Violation.Reason)
	}
	if action.Violation.PluginName != "rejecter" {
		t.Errorf("PluginName = %q, want rejecter (auto-stamped by pipeline)", action.Violation.PluginName)
	}
	status, _, _ := action.Violation.Render()
	if status != 403 {
		t.Errorf("status = %d, want 403", status)
	}
	if called {
		t.Error("second plugin was called after first rejected")
	}
}

func TestPipelineRunResponse_ReverseOrder(t *testing.T) {
	var order []string
	p1 := &stubPlugin{
		name: "first",
		onResp: func(_ context.Context, _ *Context) Action {
			order = append(order, "first")
			return Action{Type: Continue}
		},
	}
	p2 := &stubPlugin{
		name: "second",
		onResp: func(_ context.Context, _ *Context) Action {
			order = append(order, "second")
			return Action{Type: Continue}
		},
	}
	pipe, err := New([]Plugin{p1, p2})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	pctx := &Context{}
	action := pipe.RunResponse(context.Background(), pctx)
	if action.Type != Continue {
		t.Errorf("got %v, want Continue", action.Type)
	}
	if len(order) != 2 || order[0] != "second" || order[1] != "first" {
		t.Errorf("response order = %v, want [second first]", order)
	}
}

func TestPipelineRunResponse_Reject(t *testing.T) {
	called := false
	p1 := &stubPlugin{
		name: "first",
		onResp: func(_ context.Context, _ *Context) Action {
			called = true
			return Action{Type: Continue}
		},
	}
	p2 := &stubPlugin{
		name: "rejecter",
		onResp: func(_ context.Context, _ *Context) Action {
			return DenyStatus(500, "policy.content-blocked", "response blocked")
		},
	}
	pipe, err := New([]Plugin{p1, p2})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	pctx := &Context{}
	action := pipe.RunResponse(context.Background(), pctx)
	if action.Type != Reject {
		t.Errorf("got %v, want Reject", action.Type)
	}
	if called {
		t.Error("first plugin OnResponse was called after second rejected (reverse order)")
	}
}

func TestNew_ValidCapabilities(t *testing.T) {
	plugins := []Plugin{
		&stubPlugin{
			name: "writer",
			caps: PluginCapabilities{Writes: []string{"mcp"}},
		},
		&stubPlugin{
			name: "reader",
			caps: PluginCapabilities{Reads: []string{"mcp"}},
		},
	}
	_, err := New(plugins)
	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}
}

func TestNew_InvalidCapabilities_ReadBeforeWrite(t *testing.T) {
	plugins := []Plugin{
		&stubPlugin{
			name: "reader",
			caps: PluginCapabilities{Reads: []string{"mcp"}},
		},
		&stubPlugin{
			name: "writer",
			caps: PluginCapabilities{Writes: []string{"mcp"}},
		},
	}
	_, err := New(plugins)
	if err == nil {
		t.Fatal("expected error for read-before-write, got nil")
	}
}

func TestNew_InvalidCapabilities_UnknownSlot(t *testing.T) {
	plugins := []Plugin{
		&stubPlugin{
			name: "bad-reader",
			caps: PluginCapabilities{Reads: []string{"nonexistent"}},
		},
	}
	_, err := New(plugins)
	if err == nil {
		t.Fatal("expected error for unknown slot, got nil")
	}
}

func TestNew_MultipleWriters(t *testing.T) {
	plugins := []Plugin{
		&stubPlugin{
			name: "writer-1",
			caps: PluginCapabilities{Writes: []string{"security"}},
		},
		&stubPlugin{
			name: "writer-2",
			caps: PluginCapabilities{Writes: []string{"security"}},
		},
		&stubPlugin{
			name: "reader",
			caps: PluginCapabilities{Reads: []string{"security"}},
		},
	}
	_, err := New(plugins)
	if err != nil {
		t.Errorf("multiple writers should be valid, got: %v", err)
	}
}

func TestNew_NoCapabilities(t *testing.T) {
	plugins := []Plugin{
		&stubPlugin{name: "simple"},
	}
	_, err := New(plugins)
	if err != nil {
		t.Errorf("plugin with no capabilities should be valid, got: %v", err)
	}
}

func TestNew_CustomSlot(t *testing.T) {
	plugins := []Plugin{
		&stubPlugin{
			name: "custom-writer",
			caps: PluginCapabilities{Writes: []string{"custom"}},
		},
		&stubPlugin{
			name: "custom-reader",
			caps: PluginCapabilities{Reads: []string{"custom"}},
		},
	}
	_, err := New(plugins)
	if err != nil {
		t.Errorf("custom slot should be valid, got: %v", err)
	}
}

func TestNew_WithSlots(t *testing.T) {
	plugins := []Plugin{
		&stubPlugin{
			name: "cpex-bridge",
			caps: PluginCapabilities{Writes: []string{"cpex.completion"}},
		},
		&stubPlugin{
			name: "consumer",
			caps: PluginCapabilities{Reads: []string{"cpex.completion"}},
		},
	}
	// Without WithSlots, this should fail
	_, err := New(plugins)
	if err == nil {
		t.Fatal("expected error for unregistered slot without WithSlots")
	}

	// With WithSlots, this should succeed
	_, err = New(plugins, WithSlots("cpex.completion"))
	if err != nil {
		t.Errorf("expected no error with WithSlots, got: %v", err)
	}
}

func TestDelegationExtension_AppendHop(t *testing.T) {
	d := &DelegationExtension{}

	d.AppendHop(DelegationHop{SubjectID: "alice", Scopes: []string{"read", "write"}})
	if d.Depth() != 1 {
		t.Errorf("Depth() = %d, want 1", d.Depth())
	}
	if d.Origin != "alice" {
		t.Errorf("origin = %q, want %q", d.Origin, "alice")
	}
	if d.Actor != "alice" {
		t.Errorf("actor = %q, want %q", d.Actor, "alice")
	}

	d.AppendHop(DelegationHop{SubjectID: "bob", Scopes: []string{"read"}})
	if d.Depth() != 2 {
		t.Errorf("Depth() = %d, want 2", d.Depth())
	}
	if d.Origin != "alice" {
		t.Errorf("origin should stay %q, got %q", "alice", d.Origin)
	}
	if d.Actor != "bob" {
		t.Errorf("actor = %q, want %q", d.Actor, "bob")
	}
	chain := d.Chain()
	if len(chain) != 2 {
		t.Errorf("Chain() length = %d, want 2", len(chain))
	}
}

func TestDelegationExtension_ChainIsCopy(t *testing.T) {
	d := &DelegationExtension{}
	d.AppendHop(DelegationHop{SubjectID: "alice"})

	chain := d.Chain()
	chain[0].SubjectID = "tampered"

	original := d.Chain()
	if original[0].SubjectID != "alice" {
		t.Errorf("Chain() returned reference to backing slice, mutation leaked")
	}
}

func TestPipelineRun_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	called := false
	p1 := &stubPlugin{
		name: "should-not-run",
		onReq: func(_ context.Context, _ *Context) Action {
			called = true
			return Action{Type: Continue}
		},
	}
	pipe, err := New([]Plugin{p1})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	pctx := &Context{}
	action := pipe.Run(ctx, pctx)
	if action.Type != Reject {
		t.Errorf("got %v, want Reject for cancelled context", action.Type)
	}
	status, _, _ := action.Violation.Render()
	if status != 499 {
		t.Errorf("status = %d, want 499", status)
	}
	if action.Violation.Code != "pipeline.cancelled" {
		t.Errorf("code = %q, want pipeline.cancelled", action.Violation.Code)
	}
	if called {
		t.Error("plugin was called despite cancelled context")
	}
}

func TestPipeline_Plugins_ReturnsCopy(t *testing.T) {
	a := &stubPlugin{name: "a", caps: PluginCapabilities{Writes: []string{"custom"}}}
	b := &stubPlugin{name: "b", caps: PluginCapabilities{Reads: []string{"custom"}}}
	p, err := New([]Plugin{a, b})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got := p.Plugins()
	if len(got) != 2 {
		t.Fatalf("Plugins() len = %d, want 2", len(got))
	}
	if got[0].Name() != "a" || got[1].Name() != "b" {
		t.Errorf("Plugins() names = [%s %s], want [a b]", got[0].Name(), got[1].Name())
	}

	// Mutating the returned slice must not corrupt the pipeline's backing
	// storage — callers get a decorative accessor, not a handle.
	got[0] = nil
	again := p.Plugins()
	if again[0] == nil || again[0].Name() != "a" {
		t.Errorf("Plugins() returned aliased slice; backing data was mutated")
	}
}

// ----------------------------------------------------------------------------
// Lifecycle (Initializer / Shutdowner)
// ----------------------------------------------------------------------------

// lifecyclePlugin implements Initializer and Shutdowner so tests can
// verify the order and error handling of Pipeline.Start / Pipeline.Stop.
type lifecyclePlugin struct {
	stubPlugin
	initErr     error
	shutdownErr error
	onInit      func(ctx context.Context) // runs before returning initErr
	onShutdown  func(ctx context.Context)
}

func (l *lifecyclePlugin) Init(ctx context.Context) error {
	if l.onInit != nil {
		l.onInit(ctx)
	}
	return l.initErr
}
func (l *lifecyclePlugin) Shutdown(ctx context.Context) error {
	if l.onShutdown != nil {
		l.onShutdown(ctx)
	}
	return l.shutdownErr
}

// Init is called in declaration order, once per plugin. Plugins without
// Init are silently skipped.
func TestPipelineStart_InvokesInitInOrder(t *testing.T) {
	var order []string
	a := &lifecyclePlugin{
		stubPlugin: stubPlugin{name: "a"},
		onInit:     func(context.Context) { order = append(order, "a") },
	}
	// b has no Init — should be skipped, not cause a panic.
	b := &stubPlugin{name: "b"}
	c := &lifecyclePlugin{
		stubPlugin: stubPlugin{name: "c"},
		onInit:     func(context.Context) { order = append(order, "c") },
	}

	p, err := New([]Plugin{a, b, c})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(order) != 2 || order[0] != "a" || order[1] != "c" {
		t.Errorf("Init order = %v, want [a c]", order)
	}
}

// An Init error halts the sequence and the subsequent plugin's Init is
// never called. The returned error names the offending plugin so
// operators can debug a failed startup without reading the plugin's
// wrapped error message.
func TestPipelineStart_StopsOnInitError(t *testing.T) {
	aInitCalled := false
	cInitCalled := false
	a := &lifecyclePlugin{
		stubPlugin: stubPlugin{name: "a"},
		onInit:     func(context.Context) { aInitCalled = true },
	}
	b := &lifecyclePlugin{
		stubPlugin: stubPlugin{name: "b"},
		initErr:    errors.New("boom"),
	}
	c := &lifecyclePlugin{
		stubPlugin: stubPlugin{name: "c"},
		onInit:     func(context.Context) { cInitCalled = true },
	}

	p, err := New([]Plugin{a, b, c})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = p.Start(context.Background())
	if err == nil {
		t.Fatal("expected Start to return an error")
	}
	if !aInitCalled {
		t.Error("plugin a's Init should have been called before b failed")
	}
	if cInitCalled {
		t.Error("plugin c's Init should NOT have been called after b failed")
	}
	if !strings.Contains(err.Error(), `plugin "b"`) {
		t.Errorf("error = %q; expected it to name plugin b", err)
	}
}

// When a plugin's Init fails, plugins earlier in the chain whose Init
// already succeeded get their Shutdown called in reverse order. This
// prevents the common failure mode of "plugin A's Init spawns a
// background goroutine, plugin B's Init rejects its config, process
// exits via log.Fatalf, plugin A's goroutine is orphaned until the
// process dies." Not a correctness bug for the production flow (exit
// cleans up everything), but matters for embedded / multi-tenant
// callers.
func TestPipelineStart_UnwindsSuccessfulInitsOnFailure(t *testing.T) {
	var shutdownOrder []string
	a := &lifecyclePlugin{
		stubPlugin: stubPlugin{name: "a"},
		onShutdown: func(context.Context) { shutdownOrder = append(shutdownOrder, "a") },
	}
	b := &lifecyclePlugin{
		stubPlugin: stubPlugin{name: "b"},
		onShutdown: func(context.Context) { shutdownOrder = append(shutdownOrder, "b") },
	}
	// c fails its Init — a and b should be shut down in reverse order.
	c := &lifecyclePlugin{
		stubPlugin: stubPlugin{name: "c"},
		initErr:    errors.New("config rejected"),
		onShutdown: func(context.Context) { shutdownOrder = append(shutdownOrder, "c") },
	}
	// d is never Init'd, must not be Shutdown either.
	d := &lifecyclePlugin{
		stubPlugin: stubPlugin{name: "d"},
		onShutdown: func(context.Context) { shutdownOrder = append(shutdownOrder, "d") },
	}

	p, err := New([]Plugin{a, b, c, d})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.Start(context.Background()); err == nil {
		t.Fatal("expected Start to return an error")
	}
	// Expect [b, a]: reverse order of [a, b]. c's Init failed, so c is
	// not unwound (it never succeeded). d was never touched.
	if got, want := shutdownOrder, []string{"b", "a"}; !equalStrings(got, want) {
		t.Errorf("shutdown order = %v, want %v", got, want)
	}
}

// readyPlugin implements Readier so Pipeline.Ready / NotReadyPlugin
// can be exercised directly. Plugins without Readier are treated as
// always-ready, and this fixture proves both paths.
type readyPlugin struct {
	stubPlugin
	ready bool
}

func (p *readyPlugin) Ready() bool { return p.ready }

func TestPipelineReady_AllReadiersTrue(t *testing.T) {
	p, err := New([]Plugin{
		&readyPlugin{stubPlugin: stubPlugin{name: "a"}, ready: true},
		&stubPlugin{name: "no-opinion"},
		&readyPlugin{stubPlugin: stubPlugin{name: "b"}, ready: true},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !p.Ready() {
		t.Error("expected pipeline to be ready when all Readiers return true")
	}
	if name := p.NotReadyPlugin(); name != "" {
		t.Errorf("NotReadyPlugin() = %q, want empty", name)
	}
}

func TestPipelineReady_OneFalseBlocks(t *testing.T) {
	p, err := New([]Plugin{
		&readyPlugin{stubPlugin: stubPlugin{name: "a"}, ready: true},
		&readyPlugin{stubPlugin: stubPlugin{name: "b"}, ready: false},
		&readyPlugin{stubPlugin: stubPlugin{name: "c"}, ready: true},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.Ready() {
		t.Error("expected pipeline to be not-ready when one Readier returns false")
	}
	if got, want := p.NotReadyPlugin(), "b"; got != want {
		t.Errorf("NotReadyPlugin() = %q, want %q", got, want)
	}
}

// Pipelines containing only non-Readier plugins (parsers, stubs) are
// always-ready. This is the behavior Ready()-free plugins rely on —
// they must not block the pipeline's /readyz.
func TestPipelineReady_NoReadiersMeansReady(t *testing.T) {
	p, err := New([]Plugin{
		&stubPlugin{name: "a"},
		&stubPlugin{name: "b"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !p.Ready() {
		t.Error("pipeline with no Readiers should be ready")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Shutdown is called in reverse declaration order (LIFO), so plugins
// that depend on earlier plugins' resources can still use them while
// cleaning up.
func TestPipelineStop_InvokesShutdownInReverseOrder(t *testing.T) {
	var order []string
	a := &lifecyclePlugin{
		stubPlugin: stubPlugin{name: "a"},
		onShutdown: func(context.Context) { order = append(order, "a") },
	}
	b := &stubPlugin{name: "b"} // no Shutdown, skipped
	c := &lifecyclePlugin{
		stubPlugin: stubPlugin{name: "c"},
		onShutdown: func(context.Context) { order = append(order, "c") },
	}

	p, err := New([]Plugin{a, b, c})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.Stop(context.Background())
	if len(order) != 2 || order[0] != "c" || order[1] != "a" {
		t.Errorf("Shutdown order = %v, want [c a] (reverse)", order)
	}
}

// Shutdown is best-effort: an error from one plugin must not prevent
// the rest from being shut down. Otherwise a buggy plugin could strand
// in-flight work in a later plugin.
func TestPipelineStop_ContinuesOnShutdownError(t *testing.T) {
	var calls []string
	a := &lifecyclePlugin{
		stubPlugin: stubPlugin{name: "a"},
		onShutdown: func(context.Context) { calls = append(calls, "a") },
	}
	b := &lifecyclePlugin{
		stubPlugin:  stubPlugin{name: "b"},
		shutdownErr: errors.New("b failed"),
		onShutdown:  func(context.Context) { calls = append(calls, "b") },
	}

	p, err := New([]Plugin{a, b})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.Stop(context.Background()) // must not panic / abort
	// Reverse order: b runs first (and errors), a must still run.
	if len(calls) != 2 || calls[0] != "b" || calls[1] != "a" {
		t.Errorf("calls = %v, want [b a] — a must run even after b errored", calls)
	}
}

// Shutdown with no Shutdowner-implementing plugins is a no-op (not a
// panic). Important for default pipelines that don't declare
// background state.
func TestPipelineStop_NoShutdownersIsNoop(t *testing.T) {
	a := &stubPlugin{name: "a"}
	p, err := New([]Plugin{a})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.Stop(context.Background()) // must not panic
}

// --- on_error policy tests ---

// TestPipelineRun_ObservePolicyConvertsRejectToContinue is the
// headline safety property of observe mode: a plugin that intends to
// deny must not actually block the request. The downstream plugin
// still runs, the caller sees Continue, and the plugin's deny record
// lands in pctx with Shadow=true so operators can count would-have-
// blocked events on a rollout dashboard.
func TestPipelineRun_ObservePolicyConvertsRejectToContinue(t *testing.T) {
	var downstreamCalled bool
	shadow := &stubPlugin{
		name: "would-deny",
		onReq: func(_ context.Context, pctx *Context) Action {
			return pctx.DenyAndRecord("guardrail_triggered", "policy.content-blocked", "blocked")
		},
	}
	downstream := &stubPlugin{
		name: "downstream",
		onReq: func(_ context.Context, _ *Context) Action {
			downstreamCalled = true
			return Action{Type: Continue}
		},
	}
	p, err := New([]Plugin{shadow, downstream}, WithPolicies(ErrorPolicyObserve, ErrorPolicyEnforce))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{}
	got := p.Run(context.Background(), pctx)
	if got.Type != Continue {
		t.Fatalf("action type = %v, want Continue (observe suppresses Reject)", got.Type)
	}
	if !downstreamCalled {
		t.Error("downstream plugin not called; observe mode must not stop the pipeline")
	}
	if pctx.Extensions.Invocations == nil || len(pctx.Extensions.Invocations.Inbound) == 0 {
		t.Fatal("no invocations recorded")
	}
	deny := pctx.Extensions.Invocations.Inbound[0]
	if deny.Action != ActionDeny {
		t.Errorf("shadow invocation action = %q, want deny (plugin's own record is preserved)", deny.Action)
	}
	if !deny.Shadow {
		t.Error("shadow=false on the would-deny record; observe mode must mark it")
	}
}

// TestPipelineRun_EnforceIsDefault confirms that a plugin without a
// specified policy behaves exactly as before: Reject stops the
// pipeline and returns a Reject action upstream. Regression guard —
// the point of adding WithPolicies was to NOT change existing
// deployments.
func TestPipelineRun_EnforceIsDefault(t *testing.T) {
	plugin := &stubPlugin{
		name: "denier",
		onReq: func(_ context.Context, _ *Context) Action {
			return Deny("policy.forbidden", "no")
		},
	}
	// No WithPolicies — entries default to enforce.
	p, err := New([]Plugin{plugin})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := p.Run(context.Background(), &Context{})
	if got.Type != Reject {
		t.Errorf("action type = %v, want Reject (default is enforce)", got.Type)
	}
}

// TestPipelineRun_OffPolicySkipsDispatch verifies off-mode plugins
// don't run at all — not even their OnRequest. A kill-switched
// plugin must leave no trace in invocations, matching the "plugin
// absent from config" behavior.
func TestPipelineRun_OffPolicySkipsDispatch(t *testing.T) {
	var called bool
	disabled := &stubPlugin{
		name: "disabled",
		onReq: func(_ context.Context, _ *Context) Action {
			called = true
			return Action{Type: Continue}
		},
	}
	p, err := New([]Plugin{disabled}, WithPolicies(ErrorPolicyOff))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{}
	got := p.Run(context.Background(), pctx)
	if got.Type != Continue {
		t.Errorf("action = %v, want Continue", got.Type)
	}
	if called {
		t.Error("disabled plugin was dispatched; off must skip it")
	}
	if pctx.Extensions.Invocations != nil && len(pctx.Extensions.Invocations.Inbound) > 0 {
		t.Errorf("off-policy plugin appended invocations: %+v", pctx.Extensions.Invocations.Inbound)
	}
}

// TestPipelineRunResponse_ObservePolicySuppressesReject ensures the
// observe semantics apply symmetrically to the response phase.
// Response-side guardrails (DLP on tool output, jailbreak detection
// on model replies) are exactly the class that most needs shadow
// rollout — they inspect generated content the author can't preview.
func TestPipelineRunResponse_ObservePolicySuppressesReject(t *testing.T) {
	shadow := &stubPlugin{
		name: "would-deny-response",
		onResp: func(_ context.Context, pctx *Context) Action {
			return pctx.DenyAndRecord("pii_leak", "policy.content-blocked", "leak")
		},
	}
	p, err := New([]Plugin{shadow}, WithPolicies(ErrorPolicyObserve))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{}
	got := p.RunResponse(context.Background(), pctx)
	if got.Type != Continue {
		t.Errorf("action type = %v, want Continue (observe on response phase)", got.Type)
	}
	if pctx.Extensions.Invocations == nil || len(pctx.Extensions.Invocations.Inbound) == 0 {
		t.Fatal("no response-phase invocation recorded")
	}
	inv := pctx.Extensions.Invocations.Inbound[0]
	if inv.Phase != InvocationPhaseResponse {
		t.Errorf("phase = %q, want response", inv.Phase)
	}
	if !inv.Shadow {
		t.Error("response-phase shadow flag not set")
	}
}

// TestPipelineRun_ObserveSynthesizesRecordWhenPluginSkipsIt covers
// the defensive path: a plugin that returns Reject without first
// recording its own Invocation (programmer error, or use of the
// bare Deny helper instead of DenyAndRecord). The framework still
// produces a Shadow=true record so the would-have-blocked event is
// visible on the dashboard.
func TestPipelineRun_ObserveSynthesizesRecordWhenPluginSkipsIt(t *testing.T) {
	silent := &stubPlugin{
		name: "silent-rejecter",
		onReq: func(_ context.Context, _ *Context) Action {
			// Note: no pctx.Record / DenyAndRecord.
			return Deny("policy.forbidden", "no")
		},
	}
	p, err := New([]Plugin{silent}, WithPolicies(ErrorPolicyObserve))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{}
	got := p.Run(context.Background(), pctx)
	if got.Type != Continue {
		t.Errorf("action = %v, want Continue", got.Type)
	}
	if pctx.Extensions.Invocations == nil || len(pctx.Extensions.Invocations.Inbound) != 1 {
		t.Fatalf("want exactly one synthesized invocation, got: %+v", pctx.Extensions.Invocations)
	}
	inv := pctx.Extensions.Invocations.Inbound[0]
	if inv.Reason != "shadow_synthesized" {
		t.Errorf("reason = %q, want shadow_synthesized", inv.Reason)
	}
	if !inv.Shadow {
		t.Error("Shadow flag not set on synthesized record")
	}
	if inv.Details["would_deny_code"] != "policy.forbidden" {
		t.Errorf("would_deny_code = %q, want policy.forbidden", inv.Details["would_deny_code"])
	}
}

// TestSetBody_ObserveModeIsNoop verifies the body-mutation side of
// observe mode: SetBody records a shadow Invocation but does not
// replace the in-memory body or flip bodyMutated. Downstream readers
// and the wire both continue to see the original bytes, so a
// redaction plugin running in shadow can be trusted to leave
// production traffic unchanged.
func TestSetBody_ObserveModeIsNoop(t *testing.T) {
	mutator := &stubPlugin{
		name: "redactor",
		caps: PluginCapabilities{WritesBody: true},
		onReq: func(_ context.Context, pctx *Context) Action {
			pctx.SetBody([]byte("REDACTED"))
			return Action{Type: Continue}
		},
	}
	p, err := New([]Plugin{mutator}, WithPolicies(ErrorPolicyObserve))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pctx := &Context{Body: []byte("secret")}
	got := p.Run(context.Background(), pctx)
	if got.Type != Continue {
		t.Fatalf("action = %v, want Continue", got.Type)
	}
	if string(pctx.Body) != "secret" {
		t.Errorf("body = %q, want untouched (observe mode must not replace bytes)", pctx.Body)
	}
	if pctx.BodyMutated() {
		t.Error("BodyMutated=true under observe; wire must see original body")
	}
	if pctx.Extensions.Invocations == nil || len(pctx.Extensions.Invocations.Inbound) != 1 {
		t.Fatalf("want one invocation, got: %+v", pctx.Extensions.Invocations)
	}
	inv := pctx.Extensions.Invocations.Inbound[0]
	if !inv.Shadow || inv.Reason != "body_rewritten" {
		t.Errorf("expected shadow modify invocation, got action=%q reason=%q shadow=%v",
			inv.Action, inv.Reason, inv.Shadow)
	}
}

// TestNew_RejectsTooManyPolicies locks the defensive check in New:
// a caller that supplies more policies than plugins is making a
// parallel-slice mistake, not a well-formed pipeline. Fail loud at
// construction rather than silently truncate.
func TestNew_RejectsTooManyPolicies(t *testing.T) {
	_, err := New(
		[]Plugin{&stubPlugin{name: "a"}},
		WithPolicies(ErrorPolicyEnforce, ErrorPolicyObserve),
	)
	if err == nil {
		t.Fatal("expected error; 2 policies for 1 plugin is a parallel-slice bug")
	}
	if !strings.Contains(err.Error(), "WithPolicies") {
		t.Errorf("error should name WithPolicies: %q", err)
	}
}
