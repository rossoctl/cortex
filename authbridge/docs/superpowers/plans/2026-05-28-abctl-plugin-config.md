# Plugin Config in abctl Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the placeholder text in abctl's plugin-detail pane with each plugin's actual runtime config (post-`${VAR}` expansion), surfaced via a new optional field on `/v1/pipeline`.

**Architecture:** The `pipeline` package gains a `configuredPlugin` wrapper that retains the raw JSON config bytes used to build a plugin. The wrapper forwards all five required + four optional Plugin interface methods so existing dispatchers continue to work unchanged. The `plugins` registry constructs the wrapper after a successful `Configure()`. `sessionapi/server.go` adds a `Config json.RawMessage` field to its pipeline view, populated via type-assertion. `abctl`'s apiclient mirrors the wire field; `plugin_detail_pane.go` renders it through the existing `ColorizeJSONBytes` helper.

**Tech Stack:** Go 1.24, `encoding/json`, existing authlib pipeline + sessionapi infrastructure, existing abctl bubbletea TUI + JSON colorizer.

**Spec:** [`docs/superpowers/specs/2026-05-28-abctl-plugin-config-design.md`](../specs/2026-05-28-abctl-plugin-config-design.md)

---

## File Structure

**New files:**
- `authbridge/authlib/pipeline/configured.go` — `configuredPlugin` wrapper struct, `RawConfig()` method, four optional-interface forwarding methods, package-level `wrapConfigured` constructor.
- `authbridge/authlib/pipeline/configured_test.go` — wrapper tests: `RawConfig`, `Plugin` pass-through, forwarding for each of `Initializer`/`Shutdowner`/`Finisher`/`Readier`, no-op fallback for plugins that don't implement those.

**Modified files:**
- `authbridge/authlib/plugins/registry.go` — wrap Configurable plugins after successful `Configure()` at the two existing call sites.
- `authbridge/authlib/plugins/registry_test.go` — verify a Configurable plugin built via the registry exposes `RawConfig()`; a non-Configurable one does not.
- `authbridge/authlib/sessionapi/server.go` — add `Config json.RawMessage` to `pipelinePluginView`; populate via type-assertion in `describePipeline`.
- `authbridge/authlib/sessionapi/server_test.go` — extend `TestHandlePipeline` to assert the `config` field round-trips for a Configurable plugin and is omitted for a non-Configurable one.
- `authbridge/cmd/abctl/apiclient/client.go` — add `Config json.RawMessage` to `PipelinePlugin`.
- `authbridge/cmd/abctl/apiclient/client_test.go` — extend the pipeline-decode test to assert `Config` round-trips.
- `authbridge/cmd/abctl/tui/plugin_detail_pane.go` — replace the placeholder text with `ColorizeJSONBytes`-rendered config (or `(none)`).
- `authbridge/cmd/abctl/tui/plugin_detail_pane_test.go` — extend the rendering test with both Config-set and Config-empty cases.

**Convention notes:**
- The `pipeline` package is a separate Go module (`authbridge/authlib/`); use `cd authbridge && go test ./authlib/...` for tests.
- The `cmd/abctl/` package is a separate Go module; use `cd authbridge/cmd/abctl && go test ./...`.
- Repo policy: DCO sign-off (`-s`), `Assisted-By` (NOT `Co-Authored-By`), conventional commit format.
- Branch: work proceeds on a fresh branch off `upstream/main`. The branch already exists (`feat/abctl-plugin-config-spec`) with the design doc already committed (`ec24f3f`).

---

## Task 1: Wrapper struct + RawConfig + Plugin pass-through

Build the wrapper with only the required `Plugin` interface methods + `RawConfig()`. The four optional interfaces come in Task 2.

**Files:**
- Create: `authbridge/authlib/pipeline/configured.go`
- Create: `authbridge/authlib/pipeline/configured_test.go`

- [ ] **Step 1: Write the failing tests**

Create `authbridge/authlib/pipeline/configured_test.go`:

```go
package pipeline

import (
	"context"
	"encoding/json"
	"testing"
)

// fakePlugin is a minimal Plugin implementation for testing the wrapper.
// It records call counts so pass-through can be asserted.
type fakePlugin struct {
	name      string
	caps      PluginCapabilities
	requests  int
	responses int
}

func (f *fakePlugin) Name() string                  { return f.name }
func (f *fakePlugin) Capabilities() PluginCapabilities { return f.caps }
func (f *fakePlugin) OnRequest(ctx context.Context, pctx *Context) Action {
	f.requests++
	return Action{}
}
func (f *fakePlugin) OnResponse(ctx context.Context, pctx *Context) Action {
	f.responses++
	return Action{}
}

func TestConfiguredPluginRawConfig(t *testing.T) {
	raw := json.RawMessage(`{"issuer":"http://idp"}`)
	cp := wrapConfigured(&fakePlugin{name: "jwt-validation"}, raw)
	rc, ok := cp.(interface{ RawConfig() json.RawMessage })
	if !ok {
		t.Fatal("wrapper should expose RawConfig() via type-assertion")
	}
	got := string(rc.RawConfig())
	if got != `{"issuer":"http://idp"}` {
		t.Fatalf("RawConfig: %q want %q", got, `{"issuer":"http://idp"}`)
	}
}

func TestConfiguredPluginPassesThroughPluginMethods(t *testing.T) {
	fake := &fakePlugin{
		name: "jwt-validation",
		caps: PluginCapabilities{Reads: []string{"a"}, Writes: []string{"security"}},
	}
	cp := wrapConfigured(fake, json.RawMessage(`{}`))

	if cp.Name() != "jwt-validation" {
		t.Fatalf("Name pass-through broken: %q", cp.Name())
	}
	caps := cp.Capabilities()
	if len(caps.Reads) != 1 || caps.Reads[0] != "a" {
		t.Fatalf("Capabilities pass-through broken: %+v", caps)
	}
	if len(caps.Writes) != 1 || caps.Writes[0] != "security" {
		t.Fatalf("Capabilities pass-through broken: %+v", caps)
	}
	cp.OnRequest(context.Background(), nil)
	cp.OnResponse(context.Background(), nil)
	if fake.requests != 1 || fake.responses != 1 {
		t.Fatalf("OnRequest/OnResponse pass-through broken: req=%d resp=%d",
			fake.requests, fake.responses)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail (compile error)**

Run:
```bash
cd authbridge
go test ./authlib/pipeline/ -run TestConfiguredPlugin -v
```
Expected: build failure — `wrapConfigured` undefined.

- [ ] **Step 3: Write minimal implementation**

Create `authbridge/authlib/pipeline/configured.go`:

```go
package pipeline

import (
	"context"
	"encoding/json"
)

// configuredPlugin wraps a plugin built via Configurable.Configure with the
// raw config bytes it was constructed from, so the session API can surface
// them on /v1/pipeline. All Plugin interface methods forward to the embedded
// plugin — zero observable behavior change at the request hot path.
//
// Optional plugin interfaces (Initializer, Shutdowner, Finisher, Readier)
// are NOT promoted through the embedded Plugin interface — Go does not
// promote method-set membership through an embedded interface. The wrapper
// implements each of those four interfaces explicitly and forwards
// conditionally; see the methods below.
type configuredPlugin struct {
	Plugin
	raw json.RawMessage
}

// wrapConfigured returns a Plugin whose dynamic type is *configuredPlugin.
// Callers (registry.Build) invoke this only after Configurable.Configure
// returns nil; non-Configurable plugins pass through unwrapped.
func wrapConfigured(p Plugin, raw json.RawMessage) Plugin {
	return &configuredPlugin{Plugin: p, raw: raw}
}

// RawConfig returns the raw config bytes the wrapped plugin was configured
// with. Used by sessionapi describePipeline to populate /v1/pipeline's
// Config field.
func (c *configuredPlugin) RawConfig() json.RawMessage { return c.raw }
```

Note: the `context` import isn't used yet — Go will reject unused imports. Drop it for now and re-add in Task 2 when forwarding methods need it. Or simply omit it from the import block until Task 2.

- [ ] **Step 4: Run tests to verify they pass**

Run:
```bash
cd authbridge
go test ./authlib/pipeline/ -run TestConfiguredPlugin -v
go vet ./authlib/pipeline/
```
Expected: both tests PASS, vet clean.

- [ ] **Step 5: Commit**

```bash
# (run from repo root)
git add authbridge/authlib/pipeline/configured.go authbridge/authlib/pipeline/configured_test.go
git commit -s -m "feat(authlib): Add configuredPlugin wrapper retaining raw config

The wrapper holds a json.RawMessage alongside an embedded Plugin so the
session API can surface per-plugin runtime config on /v1/pipeline.
Plugin interface methods (Name, Capabilities, OnRequest, OnResponse)
pass through via embedding. The four optional interfaces are added in
the next commit.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 2: Optional interface forwarding

Add explicit forwarding for `Initializer`, `Shutdowner`, `Finisher`, `Readier` so the framework dispatchers (which type-assert against these) continue to work for wrapped plugins.

**Files:**
- Modify: `authbridge/authlib/pipeline/configured.go` — append four forwarding methods.
- Modify: `authbridge/authlib/pipeline/configured_test.go` — append forwarding + no-op tests.

- [ ] **Step 1: Write the failing tests**

Append to `authbridge/authlib/pipeline/configured_test.go`:

```go
// fakeInitializer extends fakePlugin with Initializer.
type fakeInitializer struct {
	fakePlugin
	initCalls int
	initErr   error
}

func (f *fakeInitializer) Init(ctx context.Context) error {
	f.initCalls++
	return f.initErr
}

// fakeShutdowner extends fakePlugin with Shutdowner.
type fakeShutdowner struct {
	fakePlugin
	shutdownCalls int
}

func (f *fakeShutdowner) Shutdown(ctx context.Context) error {
	f.shutdownCalls++
	return nil
}

// fakeFinisher extends fakePlugin with Finisher.
type fakeFinisher struct {
	fakePlugin
	finishCalls int
}

func (f *fakeFinisher) OnFinish(ctx context.Context, pctx *Context) {
	f.finishCalls++
}

// fakeReadier extends fakePlugin with Readier.
type fakeReadier struct {
	fakePlugin
	ready bool
}

func (f *fakeReadier) Ready() bool { return f.ready }

func TestConfiguredPluginForwardsInit(t *testing.T) {
	fake := &fakeInitializer{fakePlugin: fakePlugin{name: "x"}}
	cp := wrapConfigured(fake, nil)
	init, ok := cp.(Initializer)
	if !ok {
		t.Fatal("wrapper should implement Initializer (unconditional forwarding)")
	}
	if err := init.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if fake.initCalls != 1 {
		t.Fatalf("Init not forwarded: calls=%d want 1", fake.initCalls)
	}
}

func TestConfiguredPluginInitNoOpForNonInitializer(t *testing.T) {
	cp := wrapConfigured(&fakePlugin{name: "x"}, nil)
	init, ok := cp.(Initializer)
	if !ok {
		t.Fatal("wrapper always implements Initializer")
	}
	if err := init.Init(context.Background()); err != nil {
		t.Fatalf("no-op Init should return nil, got %v", err)
	}
}

func TestConfiguredPluginForwardsShutdown(t *testing.T) {
	fake := &fakeShutdowner{fakePlugin: fakePlugin{name: "x"}}
	cp := wrapConfigured(fake, nil)
	sh, ok := cp.(Shutdowner)
	if !ok {
		t.Fatal("wrapper should implement Shutdowner")
	}
	if err := sh.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if fake.shutdownCalls != 1 {
		t.Fatalf("Shutdown not forwarded: calls=%d want 1", fake.shutdownCalls)
	}
}

func TestConfiguredPluginShutdownNoOpForNonShutdowner(t *testing.T) {
	cp := wrapConfigured(&fakePlugin{name: "x"}, nil)
	sh, ok := cp.(Shutdowner)
	if !ok {
		t.Fatal("wrapper always implements Shutdowner")
	}
	if err := sh.Shutdown(context.Background()); err != nil {
		t.Fatalf("no-op Shutdown should return nil, got %v", err)
	}
}

func TestConfiguredPluginForwardsFinish(t *testing.T) {
	fake := &fakeFinisher{fakePlugin: fakePlugin{name: "x"}}
	cp := wrapConfigured(fake, nil)
	fin, ok := cp.(Finisher)
	if !ok {
		t.Fatal("wrapper should implement Finisher")
	}
	fin.OnFinish(context.Background(), nil)
	if fake.finishCalls != 1 {
		t.Fatalf("OnFinish not forwarded: calls=%d want 1", fake.finishCalls)
	}
}

func TestConfiguredPluginFinishNoOpForNonFinisher(t *testing.T) {
	cp := wrapConfigured(&fakePlugin{name: "x"}, nil)
	fin, ok := cp.(Finisher)
	if !ok {
		t.Fatal("wrapper always implements Finisher")
	}
	// Should not panic.
	fin.OnFinish(context.Background(), nil)
}

func TestConfiguredPluginForwardsReady(t *testing.T) {
	// Plugin that reports not-ready: wrapper must report not-ready too.
	fake := &fakeReadier{fakePlugin: fakePlugin{name: "x"}, ready: false}
	cp := wrapConfigured(fake, nil)
	r, ok := cp.(Readier)
	if !ok {
		t.Fatal("wrapper should implement Readier")
	}
	if r.Ready() {
		t.Fatal("Ready should forward false from underlying plugin")
	}
	// Now set ready=true; wrapper reflects.
	fake.ready = true
	if !r.Ready() {
		t.Fatal("Ready should forward true from underlying plugin")
	}
}

func TestConfiguredPluginReadyDefaultsTrueForNonReadier(t *testing.T) {
	// Matches the existing Pipeline.Ready() semantics: plugins without
	// Readier are considered always-ready (pipeline.go:287-289).
	cp := wrapConfigured(&fakePlugin{name: "x"}, nil)
	r, ok := cp.(Readier)
	if !ok {
		t.Fatal("wrapper always implements Readier")
	}
	if !r.Ready() {
		t.Fatal("non-Readier wrapped plugin should default to ready=true")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:
```bash
cd authbridge
go test ./authlib/pipeline/ -run TestConfiguredPlugin -v
```
Expected: existing two tests still PASS, eight new tests FAIL with "wrapper should implement X" errors.

- [ ] **Step 3: Write minimal implementation**

Append to `authbridge/authlib/pipeline/configured.go` (and update the import block to include `"context"`):

```go
// Init forwards to the wrapped plugin if it implements Initializer; otherwise
// returns nil (no deferred initialization). Required because Initializer is
// not declared on the Plugin interface, so the embedded Plugin's Init method
// (if any) is NOT promoted to *configuredPlugin's method set — only methods
// declared on Plugin are. Without this explicit forwarding, the framework's
// Init dispatcher would silently skip wrapped plugins.
func (c *configuredPlugin) Init(ctx context.Context) error {
	if init, ok := c.Plugin.(Initializer); ok {
		return init.Init(ctx)
	}
	return nil
}

// Shutdown forwards to the wrapped plugin if it implements Shutdowner; otherwise
// returns nil (no resources to release).
func (c *configuredPlugin) Shutdown(ctx context.Context) error {
	if sh, ok := c.Plugin.(Shutdowner); ok {
		return sh.Shutdown(ctx)
	}
	return nil
}

// OnFinish forwards to the wrapped plugin if it implements Finisher; otherwise
// no-ops. The framework's OnFinish dispatcher type-asserts against Finisher,
// so wrapped plugins must implement it explicitly to participate.
func (c *configuredPlugin) OnFinish(ctx context.Context, pctx *Context) {
	if fin, ok := c.Plugin.(Finisher); ok {
		fin.OnFinish(ctx, pctx)
	}
}

// Ready forwards to the wrapped plugin if it implements Readier; otherwise
// returns true. This matches the existing semantics in Pipeline.Ready
// (pipeline.go:287-289): plugins without Readier are considered always-ready.
func (c *configuredPlugin) Ready() bool {
	if r, ok := c.Plugin.(Readier); ok {
		return r.Ready()
	}
	return true
}
```

The import block at the top of `configured.go` becomes:

```go
import (
	"context"
	"encoding/json"
)
```

- [ ] **Step 4: Run tests to verify they pass**

Run:
```bash
cd authbridge
go test ./authlib/pipeline/ -run TestConfiguredPlugin -v
go vet ./authlib/pipeline/
```
Expected: all 10 wrapper tests PASS, vet clean.

Also run the full pipeline package to verify no regression:
```bash
go test ./authlib/pipeline/
```
Expected: all tests PASS.

- [ ] **Step 5: Commit**

```bash
# (run from repo root)
git add authbridge/authlib/pipeline/configured.go authbridge/authlib/pipeline/configured_test.go
git commit -s -m "feat(authlib): Forward optional plugin interfaces in configuredPlugin

Initializer/Shutdowner/Finisher/Readier are not declared on the Plugin
interface, so methods on the embedded Plugin's concrete type aren't
promoted to *configuredPlugin's method set — Go only promotes methods
declared on the embedded interface. The framework's dispatchers
type-assert against these four interfaces, so without explicit
forwarding they would silently skip every wrapped plugin.

Each forwarding method conditionally invokes the wrapped plugin's
implementation if it has one, else no-ops with the documented default
(nil error / no-op call / Ready=true). End-to-end behavior is identical
to the pre-wrap world; only the dispatch chain grows by one frame.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 3: Registry wraps Configurable plugins

Hook the wrapper into the two existing `Configure()` call sites in `plugins/registry.go` so every Configurable plugin built via the registry gets wrapped. Promote `wrapConfigured` → `WrapConfigured` so the cross-package call works.

**Files:**
- Modify: `authbridge/authlib/pipeline/configured.go` — rename `wrapConfigured` to `WrapConfigured` (export).
- Modify: `authbridge/authlib/pipeline/configured_test.go` — update test references.
- Modify: `authbridge/authlib/plugins/registry.go` — wrap after Configure success at the two call sites in `Build` (line 119) and `BuildWithSPIFFE` (line 167).
- Modify: `authbridge/authlib/plugins/registry_test.go` — append `TestRegistryWrapsConfigurablePluginsForRawConfig`.

- [ ] **Step 1: Write the failing test**

The existing `registry_test.go` registers test plugins via `RegisterPlugin(name, factory)` and builds via `Build([]config.PluginEntry{...})`. It defines a non-Configurable test plugin `relPlugin` (lines 109-128). We extend it with a Configurable variant.

Append to `authbridge/authlib/plugins/registry_test.go`:

```go
// cfgPlugin is a Configurable wrapper around relPlugin used to verify
// that the registry wraps Configurable plugins in pipeline.WrapConfigured
// and that non-Configurable plugins (relPlugin alone) are NOT wrapped.
type cfgPlugin struct {
	relPlugin
	configured json.RawMessage
}

func (c *cfgPlugin) Configure(raw json.RawMessage) error {
	c.configured = raw
	return nil
}

// TestRegistryWrapsConfigurablePluginsForRawConfig verifies that plugins
// built through Build expose their raw config bytes via type-assertion
// to interface{ RawConfig() json.RawMessage }. This is the contract
// /v1/pipeline relies on.
func TestRegistryWrapsConfigurablePluginsForRawConfig(t *testing.T) {
	// Register a Configurable plugin and a non-Configurable plugin under
	// throwaway names so this test doesn't fight the global registry.
	cfgName := "rawcfg-test-configurable"
	relName := "rawcfg-test-relational"
	RegisterPlugin(cfgName, func() pipeline.Plugin {
		return &cfgPlugin{relPlugin: relPlugin{name: cfgName}}
	})
	defer UnregisterPlugin(cfgName)
	RegisterPlugin(relName, func() pipeline.Plugin {
		return &relPlugin{name: relName}
	})
	defer UnregisterPlugin(relName)

	configRaw := json.RawMessage(`{"hello":"world"}`)
	pipe, err := Build([]config.PluginEntry{
		{Name: cfgName, Config: configRaw},
		{Name: relName},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	plugins := pipe.Plugins()
	if len(plugins) != 2 {
		t.Fatalf("want 2 plugins, got %d", len(plugins))
	}

	// First plugin (Configurable) should expose RawConfig().
	rc, ok := plugins[0].(interface{ RawConfig() json.RawMessage })
	if !ok {
		t.Fatal("Configurable plugin should be wrapped (RawConfig type-assert)")
	}
	if string(rc.RawConfig()) != `{"hello":"world"}` {
		t.Fatalf("RawConfig: got %q want %q", string(rc.RawConfig()), `{"hello":"world"}`)
	}
	// Plugin's Name() still works through the wrapper.
	if plugins[0].Name() != cfgName {
		t.Fatalf("Name through wrapper: %q", plugins[0].Name())
	}

	// Second plugin (non-Configurable) must NOT be wrapped.
	_, ok = plugins[1].(interface{ RawConfig() json.RawMessage })
	if ok {
		t.Fatal("non-Configurable plugin should NOT be wrapped")
	}
}
```

If `encoding/json` isn't yet imported in `registry_test.go`, add it. The other imports (`config`, `pipeline`, `testing`) are already there.

- [ ] **Step 2: Run test to verify it fails**

Run:
```bash
cd authbridge
go test ./authlib/plugins/ -run TestRegistryWrapsConfigurablePluginsForRawConfig -v
```
Expected: FAIL — the test asserts `RawConfig` type-assertion succeeds for the Configurable plugin, but Build doesn't wrap yet, so the assertion fails on the first plugin.

It may also fail to compile if `pipeline.WrapConfigured` is referenced (it isn't in this test, since we type-assert through an interface literal). Should compile cleanly and just fail at the assertion.

- [ ] **Step 3: Implement — promote `wrapConfigured` to `WrapConfigured`**

In `authbridge/authlib/pipeline/configured.go`, rename the function:

```go
// Before:
func wrapConfigured(p Plugin, raw json.RawMessage) Plugin {
	return &configuredPlugin{Plugin: p, raw: raw}
}

// After:
// WrapConfigured returns a Plugin whose dynamic type retains the raw
// config bytes the plugin was built from, so the session API can
// surface them on /v1/pipeline. Callers (plugins.Build) invoke this
// only after Configurable.Configure returns nil; non-Configurable
// plugins pass through unwrapped.
func WrapConfigured(p Plugin, raw json.RawMessage) Plugin {
	return &configuredPlugin{Plugin: p, raw: raw}
}
```

In `authbridge/authlib/pipeline/configured_test.go`, find/replace every `wrapConfigured(` with `WrapConfigured(`:

```bash
sed -i.bak 's/wrapConfigured(/WrapConfigured(/g' \
  authbridge/authlib/pipeline/configured_test.go
rm authbridge/authlib/pipeline/configured_test.go.bak
```

- [ ] **Step 4: Implement — wrap in Build and BuildWithSPIFFE**

In `authbridge/authlib/plugins/registry.go`, modify `Build` (around line 117-122). The current code:

```go
		p := factory()
		if c, ok := p.(pipeline.Configurable); ok {
			if err := c.Configure(e.Config); err != nil {
				return nil, fmt.Errorf("configure %q: %w", e.Name, err)
			}
		} else if len(e.Config) > 0 {
			return nil, fmt.Errorf("plugin %q does not accept configuration", e.Name)
		}
		ps = append(ps, p)
```

Becomes:

```go
		p := factory()
		if c, ok := p.(pipeline.Configurable); ok {
			if err := c.Configure(e.Config); err != nil {
				return nil, fmt.Errorf("configure %q: %w", e.Name, err)
			}
			// Wrap so the session API can surface the raw config on /v1/pipeline.
			p = pipeline.WrapConfigured(p, e.Config)
		} else if len(e.Config) > 0 {
			return nil, fmt.Errorf("plugin %q does not accept configuration", e.Name)
		}
		ps = append(ps, p)
```

In the same file, modify `BuildWithSPIFFE` (around line 165-172). The local var there is named `plugin`, not `p`. The current code:

```go
		if c, ok := plugin.(pipeline.Configurable); ok {
			if err := c.Configure(e.Config); err != nil {
				return nil, fmt.Errorf("configure %q: %w", e.Name, err)
			}
		} else if len(e.Config) > 0 {
			return nil, fmt.Errorf("plugin %q does not accept configuration", e.Name)
		}
		ps = append(ps, plugin)
```

Becomes:

```go
		if c, ok := plugin.(pipeline.Configurable); ok {
			if err := c.Configure(e.Config); err != nil {
				return nil, fmt.Errorf("configure %q: %w", e.Name, err)
			}
			// Wrap so the session API can surface the raw config on /v1/pipeline.
			plugin = pipeline.WrapConfigured(plugin, e.Config)
		} else if len(e.Config) > 0 {
			return nil, fmt.Errorf("plugin %q does not accept configuration", e.Name)
		}
		ps = append(ps, plugin)
```

- [ ] **Step 5: Run tests to verify everything passes**

Run:
```bash
cd authbridge
go test ./authlib/...
go vet ./authlib/...
```
Expected: all tests PASS (existing + new), vet clean.

If the new test fails because `RawConfig` type-asserts as nil bytes for the Configurable plugin: verify that `pipeline.WrapConfigured(plugin, e.Config)` is being passed `e.Config` (the raw bytes), not a re-encoded form.

If the new test fails because the non-Configurable plugin's type-assert succeeds: check that the wrap line is INSIDE the `if c, ok := plugin.(pipeline.Configurable); ok { ... }` block, not outside.

- [ ] **Step 6: Commit**

```bash
# (run from repo root)
git add authbridge/authlib/pipeline/configured.go authbridge/authlib/pipeline/configured_test.go authbridge/authlib/plugins/registry.go authbridge/authlib/plugins/registry_test.go
git commit -s -m "feat(authlib): Wrap Configurable plugins in the registry

Registry now wraps each Configurable plugin in pipeline.WrapConfigured
after a successful Configure(), retaining the raw config bytes the
plugin was built from. Non-Configurable plugins pass through unwrapped.

Promoted wrapConfigured to WrapConfigured so the registry can call it
across packages.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 4: sessionapi exposes Config on /v1/pipeline

Add a `Config json.RawMessage` field to the wire shape and populate it via type-assertion. `omitempty` ensures non-Configurable plugins emit unchanged JSON.

**Files:**
- Modify: `authbridge/authlib/sessionapi/server.go` — add field to `pipelinePluginView`, populate in `describePipeline`.
- Modify: `authbridge/authlib/sessionapi/server_test.go` — extend `TestHandlePipeline`.

- [ ] **Step 1: Write the failing test**

The existing `TestHandlePipeline` (around line 288) already builds two pipelines with `&fakePlugin{...}` instances, wraps them in `pipeline.NewHolder`, constructs the server with `New(":0", store, WithPipelines(...))`, and `httptest.NewServer(srv.server.Handler)`. We mirror that shape for the new test.

Append to `authbridge/authlib/sessionapi/server_test.go` (next to `TestHandlePipeline`):

```go
// TestHandlePipelineSurfacesConfig verifies that the Config field on
// /v1/pipeline carries each Configurable plugin's raw config bytes
// (when wrapped by the registry's WrapConfigured), and that
// non-Configurable plugins emit no Config field.
func TestHandlePipelineSurfacesConfig(t *testing.T) {
	configRaw := json.RawMessage(`{"hello":"world"}`)
	wrapped := pipeline.WrapConfigured(&fakePlugin{name: "with-config"}, configRaw)
	plain := &fakePlugin{name: "without-config"}

	inbound, err := pipeline.New([]pipeline.Plugin{wrapped, plain})
	if err != nil {
		t.Fatal(err)
	}

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	srv := New(":0", store, WithPipelines(pipeline.NewHolder(inbound), nil))
	ts := httptest.NewServer(srv.server.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/pipeline")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body struct {
		Inbound []struct {
			Name   string          `json:"name"`
			Config json.RawMessage `json:"config,omitempty"`
		} `json:"inbound"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Inbound) != 2 {
		t.Fatalf("want 2 plugins, got %d", len(body.Inbound))
	}
	for _, p := range body.Inbound {
		switch p.Name {
		case "with-config":
			if string(p.Config) != `{"hello":"world"}` {
				t.Fatalf("with-config Config: got %q want %q",
					string(p.Config), `{"hello":"world"}`)
			}
		case "without-config":
			if len(p.Config) != 0 {
				t.Fatalf("without-config should emit no Config, got %q",
					string(p.Config))
			}
		default:
			t.Fatalf("unexpected plugin name: %q", p.Name)
		}
	}
}
```

`fakePlugin` is already defined in `server_test.go` at the top of the file (the existing `TestHandlePipeline` uses it). `json` and `http` and `httptest` and `time` and `session` and `pipeline` are already in the test file's imports.

- [ ] **Step 2: Run test to verify it fails**

Run:
```bash
cd authbridge
go test ./authlib/sessionapi/ -run TestHandlePipelineSurfacesConfig -v
```
Expected: FAIL — the response body has no `config` field, so the assertion `string(p.Config) != ""` fires for the Configurable plugin.

- [ ] **Step 3: Write minimal implementation**

In `authbridge/authlib/sessionapi/server.go`, locate the `pipelinePluginView` struct (around line 105) and add the new field:

```go
type pipelinePluginView struct {
	Name       string          `json:"name"`
	Direction  string          `json:"direction"`
	Position   int             `json:"position"`
	BodyAccess bool            `json:"bodyAccess"`
	Writes     []string        `json:"writes,omitempty"`
	Reads      []string        `json:"reads,omitempty"`
	Config     json.RawMessage `json:"config,omitempty"` // NEW
}
```

Locate `describePipeline` (around line 134) and modify the loop body that builds each `pipelinePluginView`:

```go
func describePipeline(h *pipeline.Holder, direction string) []pipelinePluginView {
	if h == nil {
		return []pipelinePluginView{}
	}
	plugins := h.Plugins()
	out := make([]pipelinePluginView, len(plugins))
	for i, pl := range plugins {
		caps := pl.Capabilities()
		view := pipelinePluginView{
			Name:       pl.Name(),
			Direction:  direction,
			Position:   i + 1,
			BodyAccess: caps.BodyAccess,
			Writes:     caps.Writes,
			Reads:      caps.Reads,
		}
		// Surface raw config when the plugin was wrapped by the registry.
		// Non-Configurable plugins type-assert false; Config stays nil and
		// json.Marshal omits it via omitempty.
		if rc, ok := pl.(interface{ RawConfig() json.RawMessage }); ok {
			view.Config = rc.RawConfig()
		}
		out[i] = view
	}
	return out
}
```

- [ ] **Step 4: Run tests to verify everything passes**

Run:
```bash
cd authbridge
go test ./authlib/sessionapi/ -v
go vet ./authlib/sessionapi/
```
Expected: all tests PASS including `TestHandlePipelineSurfacesConfig` and the existing `TestHandlePipeline`, vet clean.

- [ ] **Step 5: Commit**

```bash
# (run from repo root)
git add authbridge/authlib/sessionapi/server.go authbridge/authlib/sessionapi/server_test.go
git commit -s -m "feat(sessionapi): Surface plugin runtime config on /v1/pipeline

pipelinePluginView gains a Config json.RawMessage field. describePipeline
type-asserts each plugin against interface{ RawConfig() json.RawMessage }
— populated by the registry's configuredPlugin wrapper for Configurable
plugins. Non-Configurable plugins return no RawConfig method, so the
field stays nil and omitempty drops it from the JSON. Wire shape for
existing consumers is unchanged.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 5: apiclient mirrors Config field

Decode the new wire field on the abctl side.

**Files:**
- Modify: `authbridge/cmd/abctl/apiclient/client.go` — add field to `PipelinePlugin`.
- Modify: `authbridge/cmd/abctl/apiclient/client_test.go` — extend the pipeline-decode test.

- [ ] **Step 1: Write the failing test**

Find the existing pipeline-related test in `authbridge/cmd/abctl/apiclient/client_test.go` (look for `GetPipeline` or `TestPipeline` patterns). If one exists, extend it. If not, add a new test that exercises the JSON decode path:

```go
// TestPipelinePluginDecodesConfig verifies the new Config field on
// /v1/pipeline survives JSON round-trip through PipelinePlugin.
func TestPipelinePluginDecodesConfig(t *testing.T) {
	body := `{"inbound":[
	  {"name":"with-config","direction":"inbound","position":1,"bodyAccess":false,
	   "config":{"hello":"world"}},
	  {"name":"without-config","direction":"inbound","position":2,"bodyAccess":false}
	],"outbound":[]}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/pipeline" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := New(srv.URL)
	view, err := c.GetPipeline(context.Background())
	if err != nil {
		t.Fatalf("GetPipeline: %v", err)
	}
	if len(view.Inbound) != 2 {
		t.Fatalf("want 2 inbound, got %d", len(view.Inbound))
	}
	// First plugin: Config decoded.
	if string(view.Inbound[0].Config) != `{"hello":"world"}` {
		t.Fatalf("with-config Config: got %q want %q",
			string(view.Inbound[0].Config), `{"hello":"world"}`)
	}
	// Second plugin: Config absent → empty/nil.
	if len(view.Inbound[1].Config) != 0 {
		t.Fatalf("without-config Config should be empty, got %q",
			string(view.Inbound[1].Config))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:
```bash
cd authbridge/cmd/abctl
go test ./apiclient/ -run TestPipelinePluginDecodesConfig -v
```
Expected: build failure — `view.Inbound[0].Config` is undefined on `PipelinePlugin`.

- [ ] **Step 3: Write minimal implementation**

In `authbridge/cmd/abctl/apiclient/client.go`, locate the `PipelinePlugin` struct (around line 85) and add the new field:

```go
type PipelinePlugin struct {
	Name       string          `json:"name"`
	Direction  string          `json:"direction"`
	Position   int             `json:"position"`
	BodyAccess bool            `json:"bodyAccess"`
	Writes     []string        `json:"writes"`
	Reads      []string        `json:"reads"`
	Config     json.RawMessage `json:"config,omitempty"` // NEW
}
```

If `encoding/json` isn't imported in `client.go` yet (it likely is, but verify), add it.

- [ ] **Step 4: Run tests to verify they pass**

Run:
```bash
cd authbridge/cmd/abctl
go test ./apiclient/ -v
go vet ./apiclient/
```
Expected: all tests PASS including the new one, vet clean.

- [ ] **Step 5: Commit**

```bash
# (run from repo root)
git add authbridge/cmd/abctl/apiclient/client.go authbridge/cmd/abctl/apiclient/client_test.go
git commit -s -m "feat(abctl): Decode Config field on PipelinePlugin

Mirrors the new wire field added to /v1/pipeline server-side. Field is
omitempty so non-Configurable plugins stay backward-compatible.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 6: TUI renders the config

Replace the placeholder text in `plugin_detail_pane.go` with `ColorizeJSONBytes`-rendered config (or `(none)` for non-Configurable plugins).

**Files:**
- Modify: `authbridge/cmd/abctl/tui/plugin_detail_pane.go` — replace placeholder with rendered config.
- Modify: `authbridge/cmd/abctl/tui/detail_pane_test.go` (or wherever `showPluginDetail` is tested) — extend.

- [ ] **Step 1: Identify the existing detail-pane test file**

Run:
```bash
grep -l "showPluginDetail" authbridge/cmd/abctl/tui/*_test.go
```

There may not be a dedicated test for `showPluginDetail` yet — `detail_pane_test.go` is the closest neighbor. If neither tests `showPluginDetail`, create `plugin_detail_pane_test.go`. The plan below assumes a new test file.

- [ ] **Step 2: Write the failing tests**

Create (or extend) `authbridge/cmd/abctl/tui/plugin_detail_pane_test.go`:

```go
package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/apiclient"
)

func TestShowPluginDetailRendersConfig(t *testing.T) {
	m := newPickerModel(context.Background(), nil, nil)
	// The viewport defaults to 0×0 (sized by layout() on WindowSizeMsg);
	// in unit tests we set it manually so View() returns content.
	m.detailVp.Width = 80
	m.detailVp.Height = 20
	plugin := &apiclient.PipelinePlugin{
		Name:      "jwt-validation",
		Direction: "inbound",
		Position:  1,
		Writes:    []string{"security"},
		Config:    json.RawMessage(`{"issuer":"http://idp"}`),
	}
	m.showPluginDetail(plugin)
	view := m.detailVp.View()
	if !strings.Contains(view, "Config:") {
		t.Fatalf("rendered view missing Config section:\n%s", view)
	}
	if !strings.Contains(view, "issuer") {
		t.Fatalf("rendered view missing config key:\n%s", view)
	}
	if !strings.Contains(view, "http://idp") {
		t.Fatalf("rendered view missing config value:\n%s", view)
	}
}

func TestShowPluginDetailRendersNoneForEmptyConfig(t *testing.T) {
	m := newPickerModel(context.Background(), nil, nil)
	m.detailVp.Width = 80
	m.detailVp.Height = 20
	plugin := &apiclient.PipelinePlugin{
		Name:      "non-configurable",
		Direction: "inbound",
		Position:  1,
		Config:    nil,
	}
	m.showPluginDetail(plugin)
	view := m.detailVp.View()
	if !strings.Contains(view, "Config:") {
		t.Fatalf("rendered view missing Config section:\n%s", view)
	}
	if !strings.Contains(view, "(none)") {
		t.Fatalf("rendered view should say (none) for empty Config:\n%s", view)
	}
}
```

Add `"context"` to the imports if not already present.

- [ ] **Step 3: Run tests to verify they fail**

Run:
```bash
cd authbridge/cmd/abctl
go test ./tui/ -run TestShowPluginDetail -v
```
Expected: tests FAIL — current `showPluginDetail` renders the placeholder text "(per-plugin runtime config will be added when..." instead of "Config: ...".

- [ ] **Step 4: Write minimal implementation**

In `authbridge/cmd/abctl/tui/plugin_detail_pane.go`, locate the closing lines of `showPluginDetail`:

```go
fmt.Fprintln(&b)
b.WriteString(styleHint.Render("(per-plugin runtime config will be added when the Plugin interface\nexposes a Config() method — tracked as a follow-up.)"))
```

Replace those two lines with:

```go
fmt.Fprintln(&b)
b.WriteString(styleMuted.Render("Config:    "))
if len(p.Config) == 0 {
	b.WriteString(" (none)\n")
} else {
	b.WriteString("\n")
	b.WriteString(ColorizeJSONBytes(p.Config))
	b.WriteString("\n")
}
```

`ColorizeJSONBytes` is the existing helper in `tui/json_colorize.go`. No new imports needed if the file already imports `fmt` and `strings` (it does).

- [ ] **Step 5: Run tests to verify they pass**

Run:
```bash
cd authbridge/cmd/abctl
go test ./tui/
go vet ./tui/
go build ./...
```
Expected: all tests PASS, vet clean, binary builds.

Smoke-build the binary against the live IBAC cluster (optional but recommended):

```bash
go build -o ./abctl .
./abctl
# Pick team1 → email-agent → drill into Pipeline pane → Enter on a plugin
# (e.g. jwt-validation) → should see the plugin's actual config block
# rendered as colored JSON. Esc back, try ibac plugin (also Configurable).
# Try a non-Configurable plugin if any are visible — should show "(none)".
```

- [ ] **Step 6: Commit**

```bash
# (run from repo root)
git add authbridge/cmd/abctl/tui/plugin_detail_pane.go authbridge/cmd/abctl/tui/plugin_detail_pane_test.go
git commit -s -m "feat(abctl): Render plugin runtime config in plugin-detail pane

Replaces the (per-plugin runtime config will be added...) placeholder
with the plugin's actual config, rendered as colorized JSON via the
existing ColorizeJSONBytes helper. Plugins without runtime config
(non-Configurable) render Config: (none).

Closes the deferred follow-up tracked in PR #445.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Self-Review Checklist

After all six tasks land, before opening the PR:

**1. Spec coverage:**
- ✅ Goal: replace placeholder with actual config — Task 6.
- ✅ Server-side, post-${VAR} expansion — Tasks 1-4 (framework holds the bytes that came from the post-expansion YAML).
- ✅ Framework-wrap approach (zero plugin source changes) — Tasks 1-3.
- ✅ Optional-interface forwarding (load-bearing) — Task 2.
- ✅ Wire shape `Config json.RawMessage` with `omitempty` — Task 4.
- ✅ apiclient mirrors wire shape — Task 5.
- ✅ TUI renders via `ColorizeJSONBytes`, `(none)` for non-Configurable — Task 6.
- ✅ Acceptance criterion #1 (Configurable plugin's config visible) — Task 6 smoke-build.
- ✅ Acceptance criterion #2 (`Config: (none)` for non-Configurable) — Task 6 second test.
- ✅ Acceptance criterion #3 (hot-reload reflected) — Task 4 type-assert reads through `Holder.Plugins()` which is hot-swap-aware (no new code needed).
- ✅ Acceptance criterion #4 (additive wire shape) — Task 4 `omitempty`.
- ✅ Acceptance criterion #5 (zero plugin source changes) — verify with `git diff main..HEAD authbridge/authlib/plugins/*/` shows changes only in tests, not in plugin implementations.
- ✅ Acceptance criterion #6 (`go test ./...` passes) — every task ends with `go test`.

**2. Type consistency:**
- `WrapConfigured(p Plugin, raw json.RawMessage) Plugin` — Task 1 + Task 2 + Task 3 use the same signature.
- `interface{ RawConfig() json.RawMessage }` — Task 1 (test), Task 4 (sessionapi populate), all spell the method identically.
- `Config json.RawMessage` — Task 4 (server view), Task 5 (apiclient), Task 6 (TUI access via `p.Config`) — same name + type throughout.
- `pipelinePluginView` (sessionapi internal) vs `apiclient.PipelinePlugin` (cross-package wire view) — different types, deliberately, with the same wire shape.

**3. Placeholder scan:**
- No `TBD` / `TODO` / "fill in details" anywhere in the plan. Each task contains complete code.
- Task 1's note about omitting `context` from the import block until Task 2 is descriptive (Go's unused-import strictness), not a placeholder.
- Tasks 4 / 5 / 6 reference patterns from existing tests (e.g., "the surrounding TestHandlePipeline setup"). The patterns are spelled out in the task code; the references are pointers for diagnostic context, not gaps.
