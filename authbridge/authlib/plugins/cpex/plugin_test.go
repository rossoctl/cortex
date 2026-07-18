package cpex

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// Canonical test-config strings. Most dispatch tests want a single
// request-phase hook wired so the chain has something to fire; a few
// configure tests use the empty config explicitly.
const (
	cfgOneRequestHook = `{"hooks":{"on_request":["cmf.tool_pre_invoke"]}}`
	cfgEmpty          = `{}`
)

// setupFake returns a CPEX plugin pre-wired to construct the given
// FakeManager from Configure. Tests use this to bypass the
// production NewManager path (which under no-tag builds is the
// errNoCpexBuild stub).
func setupFake(fake *FakeManager) *CPEX {
	p := NewCPEX()
	p.newManager = func(_ ManagerOptions) (Manager, error) { return fake, nil }
	return p
}

// --- Configure ---

func TestConfigure_EmptyConfigSucceeds(t *testing.T) {
	// A cpex plugin with no hooks/apl/pipelines is a valid no-op
	// install — operator can ship a config update later.
	fake := &FakeManager{}
	p := setupFake(fake)
	if err := p.Configure([]byte(cfgEmpty)); err != nil {
		t.Fatalf("Configure(empty): %v", err)
	}
	if fake.LoadedYAML != "" {
		t.Fatalf("LoadedYAML = %q, want empty (no apl/pipelines blocks)", fake.LoadedYAML)
	}
}

func TestConfigure_RejectsUnknownField(t *testing.T) {
	p := setupFake(&FakeManager{})
	err := p.Configure([]byte(`{"not_a_field":1}`))
	if err == nil {
		t.Fatal("Configure: expected error on unknown field")
	}
	if !strings.Contains(err.Error(), "not_a_field") {
		t.Fatalf("error doesn't name the bad field: %v", err)
	}
}

func TestConfigure_RejectsNegativeWorkerThreads(t *testing.T) {
	p := setupFake(&FakeManager{})
	err := p.Configure([]byte(`{"worker_threads":-1}`))
	if err == nil {
		t.Fatal("Configure: expected error on negative worker_threads")
	}
}

func TestConfigure_RejectsEmptyHookName(t *testing.T) {
	p := setupFake(&FakeManager{})
	err := p.Configure([]byte(`{"hooks":{"on_request":["cmf.tool_pre_invoke",""]}}`))
	if err == nil {
		t.Fatal("Configure: expected error on empty hook name in chain")
	}
}

func TestConfigure_LoadConfigErrorPropagates(t *testing.T) {
	fake := &FakeManager{LoadErr: errors.New("bad yaml")}
	p := setupFake(fake)
	err := p.Configure([]byte(`{"config":"plugins: garbage"}`))
	if err == nil || !strings.Contains(err.Error(), "bad yaml") {
		t.Fatalf("Configure: want LoadConfig error to propagate, got %v", err)
	}
}

func TestConfigure_InlineConfigLoadedIntoCPEX(t *testing.T) {
	// Operator supplies the full CPEX YAML inline via `config:`. We
	// hand it to Manager.LoadConfig verbatim — no re-shaping. This
	// matches what operators read in CPEX's own docs (`plugins:`,
	// `global:`, `plugin_settings:` at the top level).
	fake := &FakeManager{}
	p := setupFake(fake)
	yaml := "plugins:\n  - name: identity-jwt\n    kind: identity/jwt\n"
	raw := `{"config":"` + strings.ReplaceAll(yaml, "\n", `\n`) + `"}`
	if err := p.Configure([]byte(raw)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if fake.LoadedYAML != yaml {
		t.Fatalf("LoadedYAML = %q, want %q (verbatim passthrough)", fake.LoadedYAML, yaml)
	}
}

func TestConfigure_ConfigFileReadsFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cpex.yaml")
	want := "plugins:\n  - name: noop\n    kind: identity/none\n"
	if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
		t.Fatalf("write tempfile: %v", err)
	}
	fake := &FakeManager{}
	p := setupFake(fake)
	raw := []byte(`{"config_file":"` + path + `"}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure config_file: %v", err)
	}
	if fake.LoadedYAML != want {
		t.Fatalf("LoadedYAML = %q, want %q (read from %s)", fake.LoadedYAML, want, path)
	}
}

func TestConfigure_RejectsBothConfigAndFile(t *testing.T) {
	p := setupFake(&FakeManager{})
	err := p.Configure([]byte(`{"config":"plugins: []","config_file":"/dev/null"}`))
	if err == nil {
		t.Fatal("Configure: expected error when both config and config_file set")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("error message lost 'mutually exclusive' hint: %v", err)
	}
}

func TestConfigure_ConfigFileMissingPropagatesError(t *testing.T) {
	p := setupFake(&FakeManager{})
	err := p.Configure([]byte(`{"config_file":"/definitely/not/a/path"}`))
	if err == nil {
		t.Fatal("Configure: expected error when config_file missing")
	}
}

func TestConfigure_NoConfigSkipsLoadConfig(t *testing.T) {
	// When the operator hasn't supplied config / config_file, we
	// don't call LoadConfig at all — saves a YAML parse cycle on the
	// CPEX side for the "install with bypass only" case.
	fake := &FakeManager{}
	p := setupFake(fake)
	if err := p.Configure([]byte(cfgOneRequestHook)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if fake.LoadedYAML != "" {
		t.Fatalf("LoadedYAML = %q, want empty (no config supplied)", fake.LoadedYAML)
	}
}

func TestConfigure_PassesWorkerThreadsToFactory(t *testing.T) {
	var seenOpts ManagerOptions
	fake := &FakeManager{}
	p := NewCPEX()
	p.newManager = func(opts ManagerOptions) (Manager, error) {
		seenOpts = opts
		return fake, nil
	}
	if err := p.Configure([]byte(`{"worker_threads":4}`)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if seenOpts.WorkerThreads != 4 {
		t.Fatalf("WorkerThreads = %d, want 4", seenOpts.WorkerThreads)
	}
}

func TestConfigure_RejectsWildcardBypassHost(t *testing.T) {
	p := setupFake(&FakeManager{})
	err := p.Configure([]byte(`{"bypass_hosts":["*"]}`))
	if err == nil {
		t.Fatal("Configure: expected error for wildcard bypass_hosts pattern")
	}
}

func TestConfigure_RejectsWildcardBypassPath(t *testing.T) {
	p := setupFake(&FakeManager{})
	err := p.Configure([]byte(`{"bypass_paths":["/*"]}`))
	if err == nil {
		t.Fatal("Configure: expected error for wildcard bypass_paths pattern")
	}
}

// --- Init / Shutdown / Ready ---

func TestInit_FlipsReadyTrue(t *testing.T) {
	fake := &FakeManager{}
	p := setupFake(fake)
	mustConfigure(t, p, cfgOneRequestHook)
	if p.Ready() {
		t.Fatal("Ready true before Init")
	}
	if err := p.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !fake.Initialized {
		t.Fatal("manager.Initialize not called")
	}
	if !p.Ready() {
		t.Fatal("Ready false after Init")
	}
}

func TestInit_PropagatesManagerError(t *testing.T) {
	fake := &FakeManager{InitErr: errors.New("upstream down")}
	p := setupFake(fake)
	mustConfigure(t, p, cfgOneRequestHook)
	err := p.Init(context.Background())
	if err == nil || !strings.Contains(err.Error(), "upstream down") {
		t.Fatalf("Init: want upstream error, got %v", err)
	}
	if p.Ready() {
		t.Fatal("Ready true after failed Init")
	}
}

func TestInit_WithoutConfigureErrors(t *testing.T) {
	p := NewCPEX()
	if err := p.Init(context.Background()); err == nil {
		t.Fatal("Init: expected error when Configure not called")
	}
}

func TestShutdown_FlipsReadyFalse(t *testing.T) {
	fake := &FakeManager{}
	p := setupFake(fake)
	mustConfigure(t, p, cfgOneRequestHook)
	if err := p.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !fake.ShutdownCalled {
		t.Fatal("manager.Shutdown not called")
	}
	if p.Ready() {
		t.Fatal("Ready still true after Shutdown")
	}
}

func TestShutdown_WithoutConfigureIsNoOp(t *testing.T) {
	p := NewCPEX()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown without Configure should be no-op, got %v", err)
	}
}

// --- Capabilities / Name / ConfigSchema ---

func TestName(t *testing.T) {
	if name := NewCPEX().Name(); name != "cpex" {
		t.Fatalf("Name = %q, want %q", name, "cpex")
	}
}

func TestCapabilities_RequiresAnyParser(t *testing.T) {
	caps := NewCPEX().Capabilities()
	if !caps.ReadsBody || !caps.WritesBody {
		t.Fatal("ReadsBody/WritesBody must be true: CPEX policies routinely mutate payloads")
	}
	want := []string{"mcp-parser", "inference-parser", "a2a-parser"}
	if len(caps.RequiresAny) != len(want) {
		t.Fatalf("RequiresAny = %v, want %v", caps.RequiresAny, want)
	}
	for i, p := range want {
		if caps.RequiresAny[i] != p {
			t.Fatalf("RequiresAny[%d] = %q, want %q", i, caps.RequiresAny[i], p)
		}
	}
	if caps.Description == "" {
		t.Fatal("Description empty — abctl catalog renders blank")
	}
}

func TestConfigSchema_IncludesExpectedFields(t *testing.T) {
	schema := NewCPEX().ConfigSchema()
	if len(schema) == 0 {
		t.Fatal("ConfigSchema empty")
	}
	have := map[string]bool{}
	for _, f := range schema {
		have[f.Name] = true
	}
	for _, name := range []string{
		"hooks", "config", "config_file",
		"fail_open", "worker_threads",
		"bypass_hosts", "bypass_paths",
	} {
		if !have[name] {
			t.Errorf("ConfigSchema missing field %q", name)
		}
	}
}

// --- OnRequest / OnResponse dispatch ---

func TestDispatch_EmptyHooksContinues(t *testing.T) {
	// No hooks configured → phase is a no-op. Manager is never called.
	fake := &FakeManager{}
	p := setupAndInit(t, fake, cfgEmpty)
	a := p.OnRequest(context.Background(), &pipeline.Context{})
	if a.Type != pipeline.Continue {
		t.Fatalf("Type = %d, want Continue", a.Type)
	}
	if len(fake.Invokes) != 0 {
		t.Fatalf("Invoke called %d times, want 0 (no hooks)", len(fake.Invokes))
	}
}

func TestDispatch_UnknownHookSkipsAndContinuesChain(t *testing.T) {
	// Operator listed hook in config but no sub-plugin is wired for it
	// on CPEX. We skip that hook (recording Invocation.Skip) and proceed
	// to the next hook in the chain. The second hook here is wired and
	// returns Allow.
	fake := &FakeManager{
		KnownHooks: []string{HookToolPostInvoke},
		Hooks: map[string]Result{
			HookToolPostInvoke: {Decision: DecisionAllow, Reason: "ok"},
		},
	}
	cfg := `{"hooks":{"on_request":["cmf.tool_pre_invoke","cmf.tool_post_invoke"]}}`
	p := setupAndInit(t, fake, cfg)
	a := p.OnRequest(context.Background(), &pipeline.Context{})
	if a.Type != pipeline.Continue {
		t.Fatalf("Type = %d, want Continue", a.Type)
	}
	if len(fake.Invokes) != 1 || fake.Invokes[0].Hook != HookToolPostInvoke {
		t.Fatalf("Invokes = %#v, want one call to %s", fake.Invokes, HookToolPostInvoke)
	}
}

func TestDispatch_AllowDecisionContinues(t *testing.T) {
	fake := &FakeManager{
		KnownHooks: []string{HookToolPreInvoke},
		Hooks: map[string]Result{
			HookToolPreInvoke: {Decision: DecisionAllow, Reason: "ok"},
		},
	}
	p := setupAndInit(t, fake, cfgOneRequestHook)
	a := p.OnRequest(context.Background(), &pipeline.Context{})
	if a.Type != pipeline.Continue {
		t.Fatalf("Type = %d, want Continue", a.Type)
	}
	if len(fake.Invokes) != 1 || fake.Invokes[0].Hook != HookToolPreInvoke {
		t.Fatalf("Invoke called with %#v, want one call to %s", fake.Invokes, HookToolPreInvoke)
	}
}

func TestDispatch_DenyDecisionRejectsAndShortCircuits(t *testing.T) {
	// CPEX-emitted codes get namespaced under cpex.* and sanitized
	// to lower_underscore form. Input "cedar.denied" → "cpex.cedar_denied".
	// The chain stops after deny — the second hook never fires.
	fake := &FakeManager{
		KnownHooks: []string{HookToolPreInvoke, HookToolPostInvoke},
		Hooks: map[string]Result{
			HookToolPreInvoke:  {Decision: DecisionDeny, Code: "cedar.denied", Reason: "policy says no"},
			HookToolPostInvoke: {Decision: DecisionAllow, Reason: "should never run"},
		},
	}
	cfg := `{"hooks":{"on_request":["cmf.tool_pre_invoke","cmf.tool_post_invoke"]}}`
	p := setupAndInit(t, fake, cfg)
	a := p.OnRequest(context.Background(), &pipeline.Context{})
	if a.Type != pipeline.Reject {
		t.Fatalf("Type = %d, want Reject", a.Type)
	}
	if a.Violation == nil || a.Violation.Code != "cpex.cedar_denied" || a.Violation.Reason != "policy says no" {
		t.Fatalf("Violation = %#v, want code=cpex.cedar_denied reason=\"policy says no\"", a.Violation)
	}
	// A policy deny is an authorization decision → 403, not the 500 a
	// namespaced cpex.* code would otherwise default to via StatusFromCode.
	if a.Violation.Status != http.StatusForbidden {
		t.Fatalf("Violation.Status = %d, want 403 (policy deny)", a.Violation.Status)
	}
	if len(fake.Invokes) != 1 {
		t.Fatalf("Invokes = %d, want 1 (chain should short-circuit on first deny)", len(fake.Invokes))
	}
}

func TestDispatch_DenyWithoutCodeDefaultsToCpexDenied(t *testing.T) {
	fake := &FakeManager{
		KnownHooks: []string{HookToolPreInvoke},
		Hooks: map[string]Result{
			HookToolPreInvoke: {Decision: DecisionDeny, Reason: "no code"},
		},
	}
	p := setupAndInit(t, fake, cfgOneRequestHook)
	a := p.OnRequest(context.Background(), &pipeline.Context{})
	if a.Type != pipeline.Reject || a.Violation == nil || a.Violation.Code != "cpex.denied" {
		t.Fatalf("want Reject with code=cpex.denied, got %#v", a)
	}
	if a.Violation.Status != http.StatusForbidden {
		t.Fatalf("Violation.Status = %d, want 403 (policy deny)", a.Violation.Status)
	}
}

func TestDispatch_ChainAllowedHooksAllFire(t *testing.T) {
	// Allow on every hook → all hooks in the chain fire in order.
	fake := &FakeManager{
		KnownHooks: []string{HookToolPreInvoke, HookToolPostInvoke},
		Hooks: map[string]Result{
			HookToolPreInvoke:  {Decision: DecisionAllow, Reason: "ok"},
			HookToolPostInvoke: {Decision: DecisionAllow, Reason: "ok"},
		},
	}
	cfg := `{"hooks":{"on_request":["cmf.tool_pre_invoke","cmf.tool_post_invoke"]}}`
	p := setupAndInit(t, fake, cfg)
	a := p.OnRequest(context.Background(), &pipeline.Context{})
	if a.Type != pipeline.Continue {
		t.Fatalf("Type = %d, want Continue", a.Type)
	}
	if len(fake.Invokes) != 2 {
		t.Fatalf("Invokes = %d, want 2", len(fake.Invokes))
	}
	if fake.Invokes[0].Hook != HookToolPreInvoke || fake.Invokes[1].Hook != HookToolPostInvoke {
		t.Fatalf("Hook order = %s,%s, want %s,%s",
			fake.Invokes[0].Hook, fake.Invokes[1].Hook,
			HookToolPreInvoke, HookToolPostInvoke)
	}
}

func TestDispatch_ModifyDecisionContinuesChain(t *testing.T) {
	// Modify is a non-terminal outcome — the next hook in the chain
	// still fires. Headers/labels modifications are applied by
	// manager_cpex.go inside Invoke before we see the Result.
	fake := &FakeManager{
		KnownHooks: []string{HookToolPreInvoke, HookToolPostInvoke},
		Hooks: map[string]Result{
			HookToolPreInvoke:  {Decision: DecisionModify, Reason: "redacted email"},
			HookToolPostInvoke: {Decision: DecisionAllow, Reason: "ok"},
		},
	}
	cfg := `{"hooks":{"on_request":["cmf.tool_pre_invoke","cmf.tool_post_invoke"]}}`
	p := setupAndInit(t, fake, cfg)
	a := p.OnRequest(context.Background(), &pipeline.Context{})
	if a.Type != pipeline.Continue {
		t.Fatalf("Type = %d, want Continue", a.Type)
	}
	if len(fake.Invokes) != 2 {
		t.Fatalf("Invokes = %d, want 2 (Modify shouldn't stop the chain)", len(fake.Invokes))
	}
}

func TestDispatch_ObserveContinues(t *testing.T) {
	fake := &FakeManager{
		KnownHooks: []string{HookToolPreInvoke},
		Hooks: map[string]Result{
			HookToolPreInvoke: {Decision: DecisionObserve, Reason: "logged"},
		},
	}
	p := setupAndInit(t, fake, cfgOneRequestHook)
	a := p.OnRequest(context.Background(), &pipeline.Context{})
	if a.Type != pipeline.Continue {
		t.Fatalf("Type = %d, want Continue", a.Type)
	}
}

func TestDispatch_InvokeErrorFailClosed(t *testing.T) {
	fake := &FakeManager{
		KnownHooks: []string{HookToolPreInvoke},
		InvokeErr:  errors.New("ffi exploded"),
	}
	cfg := `{"hooks":{"on_request":["cmf.tool_pre_invoke"]},"fail_open":false}`
	p := setupAndInit(t, fake, cfg)
	a := p.OnRequest(context.Background(), &pipeline.Context{})
	if a.Type != pipeline.Reject {
		t.Fatalf("Type = %d, want Reject (fail-closed default)", a.Type)
	}
	if a.Violation == nil || a.Violation.Code != "cpex.error" {
		t.Fatalf("want Violation code=cpex.error, got %#v", a.Violation)
	}
	// A CPEX-internal failure is an upstream fault → 502.
	if a.Violation.Status != http.StatusBadGateway {
		t.Fatalf("Violation.Status = %d, want 502 (cpex error)", a.Violation.Status)
	}
}

func TestDispatch_InvokeErrorFailOpen(t *testing.T) {
	fake := &FakeManager{
		KnownHooks: []string{HookToolPreInvoke},
		InvokeErr:  errors.New("ffi exploded"),
	}
	cfg := `{"hooks":{"on_request":["cmf.tool_pre_invoke"]},"fail_open":true}`
	p := setupAndInit(t, fake, cfg)
	a := p.OnRequest(context.Background(), &pipeline.Context{})
	if a.Type != pipeline.Continue {
		t.Fatalf("Type = %d, want Continue (fail_open=true), got %#v", a.Type, a)
	}
}

func TestDispatch_ErrorDecisionFailClosed(t *testing.T) {
	// A Manager that returns DecisionError WITHOUT a Go error (e.g. an
	// unappliable body modification mapped to DecisionError, or a future
	// code path) must fail closed when fail_open=false — never silently
	// allow. Mirrors the returned-error fail-closed path.
	fake := &FakeManager{
		KnownHooks: []string{HookToolPreInvoke},
		Hooks: map[string]Result{
			HookToolPreInvoke: {Decision: DecisionError, Reason: "redaction unappliable"},
		},
	}
	cfg := `{"hooks":{"on_request":["cmf.tool_pre_invoke"]},"fail_open":false}`
	p := setupAndInit(t, fake, cfg)
	a := p.OnRequest(context.Background(), &pipeline.Context{})
	if a.Type != pipeline.Reject {
		t.Fatalf("Type = %d, want Reject (DecisionError fail-closed)", a.Type)
	}
	if a.Violation == nil || a.Violation.Code != "cpex.error" {
		t.Fatalf("want Violation code=cpex.error, got %#v", a.Violation)
	}
	if a.Violation.Status != http.StatusBadGateway {
		t.Fatalf("Violation.Status = %d, want 502 (cpex error)", a.Violation.Status)
	}
}

func TestDispatch_ErrorDecisionFailOpen(t *testing.T) {
	// Same DecisionError, but fail_open=true → Observe + Continue.
	fake := &FakeManager{
		KnownHooks: []string{HookToolPreInvoke},
		Hooks: map[string]Result{
			HookToolPreInvoke: {Decision: DecisionError, Reason: "redaction unappliable"},
		},
	}
	cfg := `{"hooks":{"on_request":["cmf.tool_pre_invoke"]},"fail_open":true}`
	p := setupAndInit(t, fake, cfg)
	a := p.OnRequest(context.Background(), &pipeline.Context{})
	if a.Type != pipeline.Continue {
		t.Fatalf("Type = %d, want Continue (DecisionError fail_open=true), got %#v", a.Type, a)
	}
}

func TestDispatch_UnknownDecisionFailsClosed(t *testing.T) {
	// A Decision value the switch doesn't recognise (including the
	// zero-value DecisionUnknown) must fail closed via the default arm.
	fake := &FakeManager{
		KnownHooks: []string{HookToolPreInvoke},
		Hooks: map[string]Result{
			HookToolPreInvoke: {Decision: DecisionUnknown, Reason: "zero-value sentinel"},
		},
	}
	cfg := `{"hooks":{"on_request":["cmf.tool_pre_invoke"]},"fail_open":false}`
	p := setupAndInit(t, fake, cfg)
	a := p.OnRequest(context.Background(), &pipeline.Context{})
	if a.Type != pipeline.Reject {
		t.Fatalf("Type = %d, want Reject (DecisionUnknown must fail closed)", a.Type)
	}
	if a.Violation == nil || a.Violation.Code != "cpex.error" {
		t.Fatalf("want Violation code=cpex.error, got %#v", a.Violation)
	}
}

func TestSanitizeReason(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "unspecified"},
		{"CEDAR.DENIED", "cedar_denied"},
		{"pii.redacted-field", "pii_redacted_field"},
		{"café", "caf_"}, // non-ASCII rune → single underscore
	}
	for _, tc := range cases {
		if got := sanitizeReason(tc.in); got != tc.want {
			t.Errorf("sanitizeReason(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDispatch_PathBypassSkipsInvoke(t *testing.T) {
	fake := &FakeManager{
		KnownHooks: []string{HookToolPreInvoke},
		Hooks: map[string]Result{
			HookToolPreInvoke: {Decision: DecisionDeny, Reason: "would deny"},
		},
	}
	// /healthz must skip CPEX entirely (default bypass pattern), even
	// with a deny configured for the hook.
	p := setupAndInit(t, fake, cfgOneRequestHook)
	pctx := &pipeline.Context{Path: "/healthz"}
	a := p.OnRequest(context.Background(), pctx)
	if a.Type != pipeline.Continue {
		t.Fatalf("Type = %d, want Continue (path bypass)", a.Type)
	}
	if len(fake.Invokes) != 0 {
		t.Fatalf("Invoke called %d times, want 0 (path bypass should skip FFI)", len(fake.Invokes))
	}
}

func TestDispatch_HostBypassSkipsInvokeOutbound(t *testing.T) {
	fake := &FakeManager{
		KnownHooks: []string{HookToolPreInvoke},
		Hooks: map[string]Result{
			HookToolPreInvoke: {Decision: DecisionDeny, Reason: "would deny"},
		},
	}
	// keycloak.* is in defaultBypassHosts; port stripping handles
	// "keycloak.local:8081" cleanly. Host bypass is honored on OUTBOUND
	// only (Finding 1), so the direction must be set for it to apply.
	p := setupAndInit(t, fake, cfgOneRequestHook)
	pctx := &pipeline.Context{Direction: pipeline.Outbound, Host: "keycloak.local:8081"}
	a := p.OnRequest(context.Background(), pctx)
	if a.Type != pipeline.Continue {
		t.Fatalf("Type = %d, want Continue (host bypass)", a.Type)
	}
	if len(fake.Invokes) != 0 {
		t.Fatalf("Invoke called %d times, want 0 (host bypass should skip FFI)", len(fake.Invokes))
	}
}

// Finding 1: on the inbound reverse-proxy phase the Host header is
// attacker-controlled and identity is resolved inside the CPEX chain, so a
// spoofed Host matching bypass_hosts must NOT skip CPEX. The hook still fires.
func TestDispatch_HostBypassIgnoredInbound(t *testing.T) {
	fake := &FakeManager{
		KnownHooks: []string{HookToolPreInvoke},
		Hooks: map[string]Result{
			HookToolPreInvoke: {Decision: DecisionDeny, Reason: "policy says no"},
		},
	}
	p := setupAndInit(t, fake, cfgOneRequestHook)
	// Same bypass-matching Host as the outbound test, but inbound: the
	// gate is skipped and the (deny) hook runs.
	pctx := &pipeline.Context{Direction: pipeline.Inbound, Host: "keycloak.local:8081"}
	a := p.OnRequest(context.Background(), pctx)
	if a.Type != pipeline.Reject {
		t.Fatalf("Type = %d, want Reject (inbound must not honor host bypass)", a.Type)
	}
	if len(fake.Invokes) != 1 {
		t.Fatalf("Invoke called %d times, want 1 (inbound must not skip CPEX on a spoofable Host)", len(fake.Invokes))
	}
}

func TestConfigure_CustomBypassListsHonored(t *testing.T) {
	fake := &FakeManager{
		KnownHooks: []string{HookToolPreInvoke},
		Hooks: map[string]Result{
			HookToolPreInvoke: {Decision: DecisionAllow, Reason: "ok"},
		},
	}
	cfg := `{"hooks":{"on_request":["cmf.tool_pre_invoke"]},"bypass_hosts":["internal.svc"],"bypass_paths":["/api/internal/*"]}`
	p := setupAndInit(t, fake, cfg)
	// /healthz NO LONGER bypasses (operator narrowed the set), so the
	// hook fires for it.
	p.OnRequest(context.Background(), &pipeline.Context{Path: "/healthz"})
	if len(fake.Invokes) != 1 {
		t.Fatalf("Invoke called %d times, want 1 (custom bypass list shouldn't include /healthz)", len(fake.Invokes))
	}
}

func TestDispatch_OnResponseUsesResponseHooks(t *testing.T) {
	// OnRequest and OnResponse use distinct hook lists. Verify they're
	// dispatched independently.
	fake := &FakeManager{
		KnownHooks: []string{HookToolPreInvoke, HookToolPostInvoke},
		Hooks: map[string]Result{
			HookToolPreInvoke:  {Decision: DecisionAllow, Reason: "req"},
			HookToolPostInvoke: {Decision: DecisionAllow, Reason: "resp"},
		},
	}
	cfg := `{"hooks":{"on_request":["cmf.tool_pre_invoke"],"on_response":["cmf.tool_post_invoke"]}}`
	p := setupAndInit(t, fake, cfg)
	p.OnRequest(context.Background(), &pipeline.Context{})
	p.OnResponse(context.Background(), &pipeline.Context{})
	if len(fake.Invokes) != 2 {
		t.Fatalf("Invokes = %d, want 2", len(fake.Invokes))
	}
	if fake.Invokes[0].Hook != HookToolPreInvoke || fake.Invokes[1].Hook != HookToolPostInvoke {
		t.Fatalf("Invokes = %v, want [%s, %s]",
			fake.Invokes, HookToolPreInvoke, HookToolPostInvoke)
	}
}

func TestDispatch_NotReadyIsNoOp(t *testing.T) {
	// p.Init never called → p.ready == false → dispatch returns Continue
	// without touching the manager.
	fake := &FakeManager{
		KnownHooks: []string{HookToolPreInvoke},
		Hooks: map[string]Result{
			HookToolPreInvoke: {Decision: DecisionDeny, Reason: "would deny"},
		},
	}
	p := setupFake(fake)
	mustConfigure(t, p, cfgOneRequestHook)
	a := p.OnRequest(context.Background(), &pipeline.Context{})
	if a.Type != pipeline.Continue {
		t.Fatalf("Type = %d, want Continue when not ready", a.Type)
	}
	if len(fake.Invokes) != 0 {
		t.Fatalf("Invoke called %d times, want 0 when not ready", len(fake.Invokes))
	}
}

// --- helpers ---

func mustConfigure(t *testing.T, p *CPEX, raw string) {
	t.Helper()
	if err := p.Configure([]byte(raw)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
}

// setupAndInit returns a plugin that's Configure'd against the given
// fake and Init'd (so p.ready is true). Most dispatch tests need this
// — Configure alone leaves the plugin no-op until Init flips ready.
func setupAndInit(t *testing.T, fake *FakeManager, raw string) *CPEX {
	t.Helper()
	p := setupFake(fake)
	mustConfigure(t, p, raw)
	if err := p.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return p
}
