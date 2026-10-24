# moca-sandbox example: Cortex PR Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a moca repo run with no `.claude/settings.json`, so Claude Code loads `CLAUDE.md`, while a Cortex script keeps the model off local tools.

**Architecture:** Three changes to the script plugin's plumbing, then the example:
- a `fail_closed` slot refuses requests while one of its scripts did not start;
- the reloader watches slot folders that a reload adds;
- the default `~/.cortex/plugins` slot stays when the config names another slot, placed before it;
- `examples/script-plugins/moca-sandbox.py`, with pytest tests and an install section in `docs/script-plugin.md`.

**Tech Stack:** Go 1.26.10 (`core`, `cmd/cortex`), fsnotify, Python 3.12 + pytest, the script SDK (`core/plugins/script/sdk/cortex.py`).

**Spec:** `docs/superpowers/specs/2026-10-23-moca-sandbox-example-design.md` (the "Cortex PR" section; the moca PR is deferred).

## Global Constraints

- Work only in `.worktrees/moca-sandbox` (branch `feat/moca-sandbox-example`). Never touch the top-level checkout.
- **Do not restart the shared Cortex** on `:47600`: no `make dev-install`, no `agentop service restart`, no `agentop setup --restart`.
- Go commands run per module with `GOWORK=off`. `cmd/cortex` tests need build tags: `TAGS=$(go -C scripts/profile-tags run . full)` from the repo root.
- No new Go or Python dependencies. `pyyaml` is already installed by CI's Python job.
- The refusal is exactly: status `502`, code `plugin.script-failed`, reason `script <name> did not start`.
- The script's constants: `SANDBOX_PREFIX = "mcp__sandbox__"`, `BLOCKED_PREFIX = "blocked__"`, `LOCAL` = moca's `CLAUDE_DENY` (17 names).
- The install slot is exactly `{name: script, config: {dir: moca-sandbox, fail_closed: true}}`, the last outbound entry.
- Nothing in Cortex embeds, installs or references `examples/` at run time.
- `examples/script-plugins/moca-sandbox.py` is committed executable (mode `100755`).
- Prose: say "Cortex" for the product. Docs are short, problem first, lists over paragraphs.
- Every commit: `git commit -s`, ending with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. Never `Co-Authored-By`.
- Do not push or open a PR without asking.

## Review Focus

Failure modes the spec implies but does not test. Each has a test in the task named.

1. **A script that hangs before its handshake** in a `fail_closed` slot. It must refuse once the 5s start timeout passes, not pass traffic. Task 1.
2. **An edit that breaks a loaded script** in a `fail_closed` slot. It must refuse from that reload on, not fall back to the old worker. Task 1.
3. **A slot folder that already holds a script when the config adds it.** The script loads at that reload, and a later edit there reloads again. Task 2.
4. **A response in a known sandbox session whose request carried no tool list.** The local call must still be renamed. Task 4.
5. **The user's own default `script` entry** (no `dir`), listed after the named slot or with `on_error: off`. It is left exactly where it is, and nothing is added. Task 3.

Known gap, not fixed here (raise it with Hai): a `fail_closed` slot whose folder holds no script passes all traffic. A copy into the wrong folder, or one without the exec bit, therefore protects nothing. Task 4 pins the exec bit in the repo, and the docs' install step runs `chmod +x`.

---

### Task 1: A `fail_closed` slot refuses while a script did not start

**Files:**
- Modify: `core/plugins/script/plugin.go` (package doc, `config.FailClosed` description, `Script` struct, `Configure`, `OnRequest`, new `notStarted`, `Metrics`)
- Test: `core/plugins/script/plugin_test.go`
- Docs: `docs/plugin-catalog.md` (`script` section), `docs/script-plugin.md` ("A broken script" bullet, field table)

**Interfaces:**
- Consumes: nothing new.
- Produces: the metric `scripts not started` on `/v1/pipeline`. Task 5's live check reads it.

- [ ] **Step 1: Write the failing tests**

Add to `core/plugins/script/plugin_test.go`, after `TestOnRequest_FailClosedSparesShadowScripts`:

```go
// broken writes a script into dir that exits before its handshake.
func broken(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func metric(s *Script, name string) float64 {
	for _, m := range s.Metrics() {
		if m.Name == name {
			return m.Value
		}
	}
	return -1
}

// A fail_closed slot whose script did not start refuses every request: nothing can
// tell which ones that script would have stopped.
func TestOnRequest_FailClosedRefusesWhileAScriptDidNotStart(t *testing.T) {
	dir := home(t)
	script(t, dir, "10-mask", "mask")
	broken(t, dir, "20-guard")
	s := configured(t, true, `{"fail_closed": true}`)
	pctx := request("secret")
	a := s.OnRequest(context.Background(), pctx)
	if a.Type != pipeline.Reject || a.Violation.Code != "plugin.script-failed" || a.Violation.Status != 502 ||
		a.Violation.Reason != "script guard did not start" {
		t.Fatalf("OnRequest = %+v, want a 502 naming guard", a.Violation)
	}
	if string(pctx.Body) != "secret" {
		t.Errorf("a refused request was still edited: %s", pctx.Body)
	}
	if invs := invocations(pctx); len(invs) != 1 || invs[0].Plugin != "guard" || invs[0].Action != pipeline.ActionDeny {
		t.Errorf("invocations = %+v, want one deny row for guard", invs)
	}
	if got := metric(s, "scripts not started"); got != 1 {
		t.Errorf("scripts not started = %v, want 1", got)
	}
}

// Without fail_closed a script that did not start is skipped, as before, and the
// others still run.
func TestOnRequest_AScriptThatDidNotStartFailsOpen(t *testing.T) {
	dir := home(t)
	script(t, dir, "10-mask", "mask")
	broken(t, dir, "20-guard")
	pctx := request("secret")
	if a := configured(t, true, "").OnRequest(context.Background(), pctx); a.Type != pipeline.Continue {
		t.Fatalf("a slot without fail_closed refused: %+v", a.Violation)
	}
	if string(pctx.Body) != "***" {
		t.Errorf("body = %s, want the loaded script's edit", pctx.Body)
	}
}

// The refusal lasts until a build in which the script starts — an edit triggers
// one — or from which its file is gone. An edit that breaks a loaded script
// refuses from that build on.
func TestConfigure_ARebuildClearsAScriptThatDidNotStart(t *testing.T) {
	dir := home(t)
	slot := func() pipeline.ActionType {
		return configured(t, true, `{"fail_closed": true}`).OnRequest(context.Background(), request("x")).Type
	}
	broken(t, dir, "guard")
	if slot() != pipeline.Reject {
		t.Fatal("the broken script did not refuse")
	}
	script(t, dir, "guard", "count") // fixed
	if slot() != pipeline.Continue {
		t.Fatal("a rebuild with the script fixed still refused")
	}
	broken(t, dir, "guard") // broken again by an edit
	if slot() != pipeline.Reject {
		t.Fatal("an edit that broke a loaded script let requests through")
	}
	if err := os.Remove(filepath.Join(dir, "guard")); err != nil {
		t.Fatal(err)
	}
	if slot() != pipeline.Continue {
		t.Fatal("a slot whose broken script was deleted still refused")
	}
}

// A script that never sends its handshake is given up on at the start timeout, and
// counts as not started.
func TestOnRequest_FailClosedRefusesAScriptThatNeverAnswers(t *testing.T) {
	dir := home(t)
	if err := os.WriteFile(filepath.Join(dir, "guard"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	s := configured(t, true, `{"fail_closed": true}`)
	if d := time.Since(start); d > worker.DefaultStartTimeout+2*time.Second {
		t.Errorf("Configure took %v", d)
	}
	if a := s.OnRequest(context.Background(), request("x")); a.Type != pipeline.Reject {
		t.Fatal("a script that never answered let requests through")
	}
}

// Under on_error: observe the refusal is recorded, as a shadow row, and the request
// goes on.
func TestRun_ObservePolicyDoesNotRefuseForAScriptThatDidNotStart(t *testing.T) {
	dir := home(t)
	broken(t, dir, "guard")
	s := configured(t, true, `{"fail_closed": true}`)
	p, err := pipeline.New([]pipeline.Plugin{s}, pipeline.WithPolicies(pipeline.ErrorPolicyObserve))
	if err != nil {
		t.Fatal(err)
	}
	pctx := request("x")
	if a := p.Run(context.Background(), pctx); a.Type != pipeline.Continue {
		t.Fatalf("an observed slot refused: %+v", a.Violation)
	}
	if invs := invocations(pctx); len(invs) != 1 || invs[0].Plugin != "guard" || !invs[0].Shadow {
		t.Errorf("invocations = %+v, want one shadow row for guard", invs)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `cd core && GOWORK=off go test -count=1 -run 'DidNotStart|NeverAnswers' ./plugins/script/`
Expected: FAIL. `TestOnRequest_FailClosedRefusesWhileAScriptDidNotStart` reports `OnRequest = <nil>`, because today the slot continues. `TestOnRequest_AScriptThatDidNotStartFailsOpen` passes already; it pins today's behaviour.

- [ ] **Step 3: Implement**

In `core/plugins/script/plugin.go`:

Package doc, these three lines:

```go
// applied — so trying a script cannot break a request. A script that errors (a
// crash, a timeout, a malformed reply) lets the request through unless its slot is
// fail_closed.
```

become:

```go
// applied — so trying a script cannot break a request. A script that errors (a
// crash, a timeout, a malformed reply) or does not start lets the request through
// unless its slot is fail_closed.
```

`config.FailClosed`'s description:

```go
	FailClosed bool   `json:"fail_closed" description:"Deny the request when a script errors or did not start, instead of letting it through." default:"false"`
```

In `Script`, after `loaded []*loaded`:

```go
	// missing names the scripts in the folder that did not start, in file order.
	// A fail_closed slot refuses every request while it is not empty.
	missing []string
```

`Configure`'s doc comment becomes:

```go
// Configure loads the slot's scripts and starts their workers. A script that will
// not start is skipped with a WARN, never failing the build: one broken file must
// not stop the proxy from starting or a config edit from applying. A fail_closed
// slot remembers it and refuses requests until a build in which it starts; see
// notStarted.
```

The loop after `wg.Wait()` becomes:

```go
	for i, l := range results {
		if l == nil {
			s.missing = append(s.missing, files[i].name)
			continue
		}
		s.loaded = append(s.loaded, l)
		slog.Info("script plugin: loaded", "script", l.name, "hooks", l.hs.Hooks, "enforce", l.hs.Enforce)
	}
```

At the top of `OnRequest`, before `var msg map[string]any`:

```go
	if s.cfg.FailClosed && len(s.missing) > 0 {
		return s.notStarted(pctx)
	}
```

New method, after `failed`:

```go
// notStarted refuses a request in a fail_closed slot while one of its scripts did
// not start. The slot cannot tell which requests that script would have acted on,
// so it refuses them all: fail_closed means a request the script never saw does
// not go through. Under on_error: observe the refusal is recorded as shadow, as
// failed records one.
func (s *Script) notStarted(pctx *pipeline.Context) pipeline.Action {
	name := s.missing[0]
	inv := pipeline.Invocation{
		Plugin:  name,
		Action:  pipeline.ActionObserve,
		Reason:  "script_not_started",
		Details: map[string]string{"error": "did not start; see the Cortex log", "not_started": strings.Join(s.missing, ",")},
		Shadow:  pctx.Observing(),
	}
	if inv.Shadow {
		pctx.Record(inv)
		return pipeline.Action{}
	}
	inv.Action = pipeline.ActionDeny
	pctx.Record(inv)
	v := &pipeline.Violation{Code: "plugin.script-failed", Reason: "script " + name + " did not start", Status: 502, PluginName: name}
	return pipeline.Action{Type: pipeline.Reject, Violation: v}
}
```

In `Metrics`, after `scripts loaded`:

```go
		{Name: "scripts not started", Value: float64(len(s.missing)), Note: "a fail_closed slot refuses every request while this is not 0"},
```

- [ ] **Step 4: Run the package's tests**

Run: `cd core && GOWORK=off go test -race -count=1 ./plugins/script/...`
Expected: PASS. It takes about 15s, about 5s of which is the hang test's start timeout.

- [ ] **Step 5: Update the docs**

`docs/plugin-catalog.md`, `script` section. The sentence "...lets the request through, with the error on its timeline row, unless the slot sets `fail_closed: true`." gets one more sentence after it:

```markdown
Under `fail_closed`, a script that did not start makes the slot refuse every
request with a 502 until a reload in which it starts.
```

The same file's field table, and the table in `docs/script-plugin.md`. The `fail_closed` row becomes:

```markdown
| `fail_closed` | `false` | Block a request when an enforcing script fails on it, and every request while a script in the slot did not start |
```

`docs/script-plugin.md`, "Things worth knowing". The bullet "**A broken script does not break your agent.**" becomes:

```markdown
- **A broken script does not break your agent.** An exception, a crash or a
  timeout is shown on the request's row and the request continues. To block
  instead, see `fail_closed` below. Under `fail_closed`, a script that does not
  start at all — a syntax error, a missing interpreter — blocks every request
  through its slot. Cortex tries it again when you save a change to it, or when
  Cortex restarts.
```

- [ ] **Step 6: Commit**

The new `missing` field shifts the struct's alignment, so format before checking:

```bash
gofmt -w core/plugins/script/plugin.go && gofmt -l core/plugins/script
git add core/plugins/script/plugin.go core/plugins/script/plugin_test.go docs/plugin-catalog.md docs/script-plugin.md
git commit -s -m "feat: Refuse requests in a fail_closed script slot while a script did not start" \
  -m "A script that failed to start was skipped with a WARN, so a fail_closed slot passed every request a guard script never saw." \
  -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

`gofmt -l` must print nothing.

---
### Task 2: The reloader watches slot folders a reload adds

**Files:**
- Modify: `core/reloader/reloader.go` (`WithExtraWatch`, `Reloader` fields, `Start`, `watchLoop`, `reloadOnce`, new `watchExtra`)
- Modify: `cmd/cortex/main.go:612-619` (the `WithExtraWatch` call and its comment)
- Test: `core/reloader/reloader_test.go`

**Interfaces:**
- Consumes: `scriptFolders(*config.Config) []string` and `scriptFingerprint([]string) string`, both in `cmd/cortex` today.
- Produces: `func WithExtraWatch(dirs func(*config.Config) []string, fingerprint func(dirs []string) string) Option`. This replaces the old signature. The only caller is `cmd/cortex/main.go`, and no release ships the old one (it came in with the script plugin, after v0.9.1).

How it works:
- `extraDirs` is now a function from config to folders. `Start` calls it on the startup config.
- After a reload is accepted, the reloader calls it on the new config and adds a watch for each folder it has not seen, creating it as `Start` does.
- The add runs on the watch goroutine. Neither fsnotify backend holds its lock while sending an event, so this cannot deadlock.
- The fingerprint takes the folder list, so a newly watched folder counts in the next reload's hash.
- `reloadOnce` now returns the accepted config, or nil. `watchLoop` does the add, because it owns the watcher.
- Folders the config stops naming stay watched until restart. A stray event there leaves the hash unchanged, so it reloads nothing.

- [ ] **Step 1: Write the failing tests**

In `core/reloader/reloader_test.go`, replace `TestReloader_ExtraWatchFolderReloads` with:

```go
// withLateSlot is the fixture's config after an edit that adds a script slot.
func withLateSlot() *config.Config {
	c := &config.Config{Mode: "envoy-sidecar"}
	c.Pipeline.Outbound.Plugins = []config.PluginEntry{{Name: "script", Config: []byte(`{"dir":"late"}`)}}
	return c
}

// staticDirs is a WithExtraWatch folder function that ignores the config.
func staticDirs(dirs ...string) func(*config.Config) []string {
	return func(*config.Config) []string { return dirs }
}

// A change in a WithExtraWatch folder reloads with the config file untouched, and an
// event there that leaves the fingerprint unchanged does not.
func TestReloader_ExtraWatchFolderReloads(t *testing.T) {
	extra := filepath.Join(t.TempDir(), "plugins") // created by Start
	var fp atomic.Value
	fp.Store("one")
	r, b, _, _, _ := setupWith(t, WithExtraWatch(staticDirs(extra), func([]string) string { return fp.Load().(string) }))
	if _, err := os.Stat(extra); err != nil {
		t.Fatalf("Start did not create the watched folder: %v", err)
	}
	b.set(builderResult{inbound: emptyPipeline(t), outbound: emptyPipeline(t), cfg: &config.Config{Mode: "envoy-sidecar"}})

	// An event with nothing changed: no reload.
	if err := os.WriteFile(filepath.Join(extra, "scratch"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if got := r.Status().ReloadsOK; got != 0 {
		t.Fatalf("an unchanged fingerprint reloaded %d times", got)
	}

	fp.Store("two")
	if err := os.WriteFile(filepath.Join(extra, "mask.py"), []byte("x"), 0o700); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool { return r.Status().ReloadsOK >= 1 }, "a folder change to reload")
}

// A folder named by a config that a reload accepted is watched from then on: an
// edit in it reloads without a restart. The folder's content counts in the hash
// from that reload, so the edit is a change. A folder created by hand before the
// config names it — with the script already in it — is watched the same way.
func TestReloader_WatchesAFolderAReloadAdds(t *testing.T) {
	root := t.TempDir()
	early := filepath.Join(root, "early")
	late := filepath.Join(root, "late")
	if err := os.MkdirAll(late, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(late, "guard.py"), []byte("v1"), 0o700); err != nil {
		t.Fatal(err)
	}
	dirs := func(c *config.Config) []string {
		if c != nil && len(c.Pipeline.Outbound.Plugins) > 0 {
			return []string{early, late}
		}
		return []string{early}
	}
	// Each folder's fingerprint is the content of the file in it, as the script
	// plugin's is.
	fingerprint := func(ds []string) string {
		var s string
		for _, d := range ds {
			b, _ := os.ReadFile(filepath.Join(d, "guard.py"))
			s += d + "=" + string(b) + ";"
		}
		return s
	}
	r, b, cfgPath, _, _ := setupWith(t, WithExtraWatch(dirs, fingerprint))

	// The config edit that adds the slot: a reload the builder accepts. mode stays,
	// since a mode change is refused.
	b.set(builderResult{inbound: emptyPipeline(t), outbound: emptyPipeline(t), cfg: withLateSlot()})
	writeConfig(t, cfgPath, "mode: envoy-sidecar\n# with the late slot\n")
	waitFor(t, 2*time.Second, func() bool { return r.Status().ReloadsOK == 1 }, "the config edit to reload")

	// An edit in the folder that reload added.
	b.set(builderResult{inbound: emptyPipeline(t), outbound: emptyPipeline(t), cfg: withLateSlot()})
	if err := os.WriteFile(filepath.Join(late, "guard.py"), []byte("v2"), 0o700); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool { return r.Status().ReloadsOK == 2 }, "an edit in the added folder to reload")
}

// A refused reload adds no watch: the folders follow the config that runs.
func TestReloader_ARefusedReloadWatchesNothingNew(t *testing.T) {
	root := t.TempDir()
	late := filepath.Join(root, "late")
	var asked atomic.Int32
	dirs := func(c *config.Config) []string {
		if c != nil && len(c.Pipeline.Outbound.Plugins) > 0 {
			asked.Add(1)
			return []string{late}
		}
		return nil
	}
	_, b, cfgPath, _, _ := setupWith(t, WithExtraWatch(dirs, func([]string) string { return "" }))
	b.set(builderResult{err: errBuildRefused, cfg: withLateSlot()})
	writeConfig(t, cfgPath, "mode: envoy-sidecar\n# with the late slot\n")
	time.Sleep(200 * time.Millisecond)
	if asked.Load() != 0 {
		t.Fatal("a refused reload asked for the new config's folders")
	}
	if _, err := os.Stat(late); !os.IsNotExist(err) {
		t.Fatalf("a refused reload created a folder: %v", err)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `cd core && GOWORK=off go test -count=1 -run 'ExtraWatch|AReloadAdds|WatchesNothingNew' ./reloader/`
Expected: FAIL to compile: `cannot use staticDirs(extra) (value of type func(*config.Config) []string) as []string value`.

- [ ] **Step 3: Implement**

In `core/reloader/reloader.go`, replace `WithExtraWatch` and its comment with:

```go
// WithExtraWatch makes the pipelines depend on more than the config file: any event
// in one of the folders dirs names triggers a reload, and fingerprint's result over
// those folders joins the config's content in the hash that decides whether
// anything changed. The script plugin uses it for its folders, so adding or editing
// a script reloads exactly as a config edit does.
//
// dirs is called with the startup config at Start, and again with each config a
// reload accepts, so a folder a config edit adds is watched from that reload on.
// A folder that does not exist yet is created so it can be watched: one the user
// creates later would otherwise go unseen until the next config edit. A folder the
// config stops naming stays watched until restart; an event there leaves the
// fingerprint unchanged, so it reloads nothing. fingerprint is called on every
// candidate reload, with every folder watched so far.
func WithExtraWatch(dirs func(*config.Config) []string, fingerprint func(dirs []string) string) Option {
	return func(r *Reloader) { r.extraDirs, r.fingerprint = dirs, fingerprint }
}
```

In `Reloader`, replace the two `extraDirs`/`fingerprint` fields with:

```go
	// extraDirs and fingerprint widen what a reload depends on; see WithExtraWatch.
	extraDirs   func(*config.Config) []string
	fingerprint func(dirs []string) string
	// watched is every WithExtraWatch folder with a watch on it, cleaned, in the
	// order added. Written by Start and then only by the watch goroutine; hash
	// reads it on that goroutine, or in New before either exists.
	watched []string
```

In `Start`, replace the block from `watchedExtra := map[string]bool{}` through `go r.watchLoop(ctx, watcher, watchedExtra)` with:

```go
	if r.extraDirs != nil {
		r.watchExtra(watcher, r.extraDirs(r.activeCfg.Load()))
	}

	go r.watchLoop(ctx, watcher)
```

Add after `Start`:

```go
// watchExtra adds a watch on each of dirs not watched yet, creating it first. A
// folder that cannot be created or watched is skipped with a WARN: its scripts
// still load with each reload, and an edit in it reloads only with the config.
func (r *Reloader) watchExtra(watcher *fsnotify.Watcher, dirs []string) {
	for _, d := range dirs {
		d = filepath.Clean(d)
		if slices.Contains(r.watched, d) {
			continue
		}
		if err := os.MkdirAll(d, 0o700); err != nil {
			slog.Warn("reloader: cannot create a watched folder", "dir", d, "error", err)
			continue
		}
		if err := watcher.Add(d); err != nil {
			slog.Warn("reloader: cannot watch a folder; changes in it reload only with the config", "dir", d, "error", err)
			continue
		}
		r.watched = append(r.watched, d)
	}
}
```

Add `"slices"` to the imports.

`watchLoop`'s signature drops `extra`:

```go
func (r *Reloader) watchLoop(ctx context.Context, watcher *fsnotify.Watcher) {
```

and its filter's last clause becomes `!slices.Contains(r.watched, filepath.Dir(ev.Name))`. The `trigger` case becomes:

```go
		case <-trigger:
			// A config that names a new folder is watched from here. Added on this
			// goroutine, which owns the watcher; fsnotify sends no event while
			// holding the lock Add takes, so this cannot block on the channel
			// this loop drains.
			if cfg := r.reloadOnce(ctx); cfg != nil && r.extraDirs != nil {
				r.watchExtra(watcher, r.extraDirs(cfg))
			}
```

`reloadOnce` returns `*config.Config`:
- its doc gets a last sentence: `It returns the config it put into effect, or nil when it changed nothing.`;
- every bare `return` becomes `return nil`;
- the final line after `r.recordSuccess(attempt, hash)` is `return newCfg`.

`hash` passes the watched folders:

```go
		if fp := r.fingerprint(r.watched); fp != "" {
```

In `cmd/cortex/main.go`, replace the block from the comment `// The script plugin's folders are watched as well as the config` through the `reloader.New(...)` call with:

```go
	// The script plugin's folders are watched as well as the config, so adding or
	// editing a script reloads as a config edit does. The reloader asks each config
	// it puts into effect for its folders, so a slot a config edit adds is watched
	// from that reload on.
	rld := reloader.New(*configPath, inboundH, outboundH, buildPipelines, cfg,
		reloader.WithOnCommit(applyPricing),
		reloader.WithExtraWatch(scriptFolders, scriptFingerprint))
```

`scriptFolders(*config.Config) []string` and `scriptFingerprint([]string) string` already have these signatures.

The hash at `New` covers no folder yet, since `Start` adds them. That is the same as today: `New` already runs before any watch exists. With no script in any folder the fingerprint is empty, and `/reload/status` keeps reporting the config's own sha256.

- [ ] **Step 4: Run the tests**

Run: `cd core && GOWORK=off go test -race -count=1 ./reloader/`
Expected: PASS.

Run: `cd cmd/cortex && GOWORK=off go vet -tags "$(go -C ../../scripts/profile-tags run . full)" . && GOWORK=off go test -count=1 -tags "$(go -C ../../scripts/profile-tags run . full)" -run 'Script|LocalChain' .`
Expected: PASS.

Run: `cd cmd/cortex && GOWORK=off go vet . && GOWORK=off go build .`
Expected: no output. This is the build without the script plugin, where `scriptFingerprint` comes from `scripts_noplugin.go`.

- [ ] **Step 5: Commit**

```bash
gofmt -l core/reloader cmd/cortex
git add core/reloader/reloader.go core/reloader/reloader_test.go cmd/cortex/main.go
git commit -s -m "feat: Watch the script slot folders a config reload adds" \
  -m "The reloader watched only the folders named at startup, so a slot added by a reload ran its scripts but missed their edits until a restart." \
  -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 3: Keep the default slot, placed before the first named slot

**Files:**
- Modify: `cmd/cortex/scripts.go` (`addScriptSlot`)
- Test: `cmd/cortex/scripts_test.go` (`TestAddScriptSlot`), `cmd/cortex/scripts_build_test.go`
- Docs: `docs/script-plugin.md` ("Order, and running between Go plugins"), `docs/plugin-catalog.md` (`script` section)

**Interfaces:**
- Consumes: nothing new.
- Produces: `addScriptSlot(c *config.Config, local bool)`, same signature, new placement rule.

The rule:
- A chain whose `script` entries all name a `dir` gets the default slot (`{name: script}`) inserted **just before the first of them**.
- A chain with no `script` entry gets it appended, as today.
- A chain that already has a `script` entry with an empty `dir`, under any `on_error` including `off`, is the user's own arrangement: nothing is added.

Why before: the pipeline requires response readers before response rewriters. A user's script that reads responses, in a default slot after the moca slot, made the whole config fail to load (probed: `reads body after mutator "script"`). Before it, the chain builds.

- [ ] **Step 1: Write the failing tests**

In `cmd/cortex/scripts_test.go`, replace `outboundNames` and `TestAddScriptSlot` with:

```go
// outboundNames lists the outbound chain as name, or name:dir for a script slot
// that names one.
func outboundNames(c *config.Config) []string {
	var out []string
	for _, e := range c.Pipeline.Outbound.Plugins {
		if d := entryDir(e); d != "" {
			out = append(out, e.Name+":"+d)
		} else {
			out = append(out, e.Name)
		}
	}
	return out
}

func TestAddScriptSlot(t *testing.T) {
	chain := func(entries ...config.PluginEntry) *config.Config {
		c := &config.Config{}
		c.Pipeline.Outbound.Plugins = entries
		return c
	}
	parser := config.PluginEntry{Name: "inference-parser"}
	prune := config.PluginEntry{Name: "tool-prune"}
	moca := config.PluginEntry{Name: "script", Config: []byte(`{"dir":"moca-sandbox","fail_closed":true}`)}
	early := config.PluginEntry{Name: "script", Config: []byte(`{"dir":"early"}`)}
	def := config.PluginEntry{Name: "script"}
	defOff := config.PluginEntry{Name: "script", OnError: pipeline.ErrorPolicyOff}
	emptyDir := config.PluginEntry{Name: "script", Config: []byte(`{"dir":""}`)}

	for _, tc := range []struct {
		name  string
		local bool
		in    []config.PluginEntry
		want  []string
	}{
		{"no script entry: appended last", true, []config.PluginEntry{parser, prune}, []string{"inference-parser", "tool-prune", "script"}},
		{"not a local install: nothing added", false, []config.PluginEntry{parser}, []string{"inference-parser"}},
		{"a named slot: the default goes just before it", true, []config.PluginEntry{parser, prune, moca}, []string{"inference-parser", "tool-prune", "script", "script:moca-sandbox"}},
		{"two named slots: before the first", true, []config.PluginEntry{parser, early, prune, moca}, []string{"inference-parser", "script", "script:early", "tool-prune", "script:moca-sandbox"}},
		{"the user's default after a named slot: left alone", true, []config.PluginEntry{parser, moca, def}, []string{"inference-parser", "script:moca-sandbox", "script"}},
		{"the user's default switched off: left alone", true, []config.PluginEntry{defOff, parser, moca}, []string{"script", "inference-parser", "script:moca-sandbox"}},
		{"an explicit empty dir is the default slot", true, []config.PluginEntry{parser, moca, emptyDir}, []string{"inference-parser", "script:moca-sandbox", "script"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := chain(slices.Clone(tc.in)...)
			addScriptSlot(c, tc.local)
			if got := outboundNames(c); !slices.Equal(got, tc.want) {
				t.Errorf("chain = %v, want %v", got, tc.want)
			}
		})
	}
}
```

Add `"slices"` to that file's imports.

In `cmd/cortex/scripts_build_test.go`, add after `TestLocalChain_BuildsWithEachKindOfScript`:

```go
// A user's response-reading script in the default slot, next to a slot that
// rewrites responses (the moca-sandbox example's shape), still builds: the default
// slot goes first, because a response reader must precede every response writer.
func TestLocalChain_BuildsWithAResponseRewritingSlotAdded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "")
	root := filepath.Join(home, ".cortex", "plugins")
	scripts := map[string]string{
		filepath.Join(root, "watch"):                 `{"cortex":1,"hooks":["response"]}`,
		filepath.Join(root, "moca-sandbox", "guard"): `{"cortex":1,"hooks":["request","response"],"enforce":true,"rewrite_response":true}`,
	}
	for path, handshake := range scripts {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\necho '"+handshake+"'\ncat >/dev/null\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	p, err := writeBuiltinConfig(filepath.Join(home, ".cortex"), filepath.Join(home, ".cortex", "ca"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	config.ApplyPreset(cfg)
	cfg.Pipeline.Outbound.Plugins = append(cfg.Pipeline.Outbound.Plugins,
		config.PluginEntry{Name: "script", Config: []byte(`{"dir":"moca-sandbox","fail_closed":true}`)})
	addScriptSlot(cfg, true)
	pipe, err := plugins.BuildWithDeps(cfg.Pipeline.Outbound.Plugins,
		plugins.Deps{Listener: forwardproxy.Support(false), LocalInstall: true})
	if err != nil {
		t.Fatalf("the local chain did not build: %v", err)
	}
	pipe.Stop(t.Context())
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `cd cmd/cortex && GOWORK=off go test -count=1 -tags "$(go -C ../../scripts/profile-tags run . full)" -run 'AddScriptSlot|ResponseRewritingSlot' .`
Expected: FAIL. The "a named slot" and "two named slots" cases get no default slot. `TestLocalChain_BuildsWithAResponseRewritingSlotAdded` passes already, because today nothing is added; it pins that the new placement still builds.

- [ ] **Step 3: Implement**

In `cmd/cortex/scripts.go`, replace `addScriptSlot` and its comment with:

```go
// addScriptSlot puts the default slot, ~/.cortex/plugins itself, into the outbound
// chain of a local install, so a user who drops a file there has it running without
// editing YAML.
//
// A chain that names no script entry gets it appended. A chain whose script entries
// all name a dir — a slot another tool added, such as the moca-sandbox example's —
// gets it just before the first of them, so adding a slot does not switch the
// user's own scripts off. Before, not after: a slot that rewrites responses must
// come after every response reader, and the default slot's scripts may read them.
// A chain that already has an entry for the default slot, under any on_error,
// including off, is the user's own arrangement and is left alone. A binary built
// without the plugin adds nothing, since the entry could only fail to load.
func addScriptSlot(c *config.Config, local bool) {
	if !local || !slices.Contains(plugins.RegisteredPlugins(), scriptPlugin) {
		return
	}
	chain := c.Pipeline.Outbound.Plugins
	first := -1
	for i, e := range chain {
		if e.Name != scriptPlugin {
			continue
		}
		if entryDir(e) == "" {
			return
		}
		if first < 0 {
			first = i
		}
	}
	slot := config.PluginEntry{Name: scriptPlugin}
	if first < 0 {
		c.Pipeline.Outbound.Plugins = append(chain, slot)
		return
	}
	c.Pipeline.Outbound.Plugins = slices.Insert(chain, first, slot)
}
```

- [ ] **Step 4: Run the tests**

Run: `cd cmd/cortex && GOWORK=off go test -race -count=1 -tags "$(go -C ../../scripts/profile-tags run . full)" .`
Expected: PASS.

Run: `cd cmd/cortex && for p in local lite; do GOWORK=off go test -count=1 -tags "$(go -C ../../scripts/profile-tags run . $p)" . || break; done`
Expected: PASS twice. `lite` has no script plugin, so `addScriptSlot` adds nothing there.

- [ ] **Step 5: Update the docs**

`docs/script-plugin.md`, section "Order, and running between Go plugins":

The paragraph "All of `~/.cortex/plugins/` is one slot, which Cortex appends to the end of the outbound pipeline. To put scripts somewhere else..." becomes:

```markdown
All of `~/.cortex/plugins/` is one slot, which Cortex appends to the end of the
outbound pipeline. A folder inside it is a slot of its own once the config names
it. To put scripts somewhere else — between two Go plugins, for example — list
the slot yourself and give it a folder of its own:
```

The paragraph after the YAML block, "Once the config names `script` anywhere, Cortex appends nothing. A slot must come after the parsers: it rewrites requests, and readers go first.", becomes:

```markdown
- **Naming a slot keeps the default one.** When every `script` entry names a
  `dir`, Cortex adds the `~/.cortex/plugins/` slot just before the first of them.
  It goes first because a script that reads responses must come before one that
  rewrites them.
- **To place the default slot yourself,** list `- name: script` with no `dir`.
  Cortex then adds nothing. With `on_error: off` it is switched off.
- **A slot must come after the parsers:** it rewrites requests, and readers go
  first.
```

`docs/plugin-catalog.md`, `script` section. The sentence "List `script` yourself, with `config.dir`, to place a second slot elsewhere — ..." keeps its text; add after the paragraph it ends:

```markdown
A chain whose `script` entries all name a `dir` still gets the default slot,
inserted just before the first of them.
```

- [ ] **Step 6: Commit**

```bash
gofmt -l cmd/cortex
git add cmd/cortex/scripts.go cmd/cortex/scripts_test.go cmd/cortex/scripts_build_test.go docs/script-plugin.md docs/plugin-catalog.md
git commit -s -m "feat: Keep the default script slot when the config names another one" \
  -m "Adding a named slot used to switch the user's ~/.cortex/plugins scripts off. The default slot now goes just before the first named one, since a response reader must precede a response writer." \
  -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 4: The `moca-sandbox.py` example, its tests and its docs

**Files:**
- Create: `examples/script-plugins/moca-sandbox.py` (mode `100755`)
- Create: `tests/test_example_moca_sandbox.py`
- Modify: `docs/script-plugin.md` (new "Examples" section, before "Without the SDK")

**Interfaces:**
- Consumes: the SDK's `on.request`, `on.response(rewrite=True)`, `run(enforce=True)`, `handle(msg)`, and `Flow`'s `session`, `json`, `inference`, `response.body`, `response.headers`, `set_body`, `record`.
- Produces: the docs section `### moca-sandbox.py: keep Claude Code on moca's sandbox tools`, which the tests parse.

What changed from the spike's script (`/tmp/moca-spike/plugin/moca-sandbox.py`):
- `SANDBOX_PREFIX` is `mcp__sandbox__`, moca's server name.
- A stream is told from JSON by parsing the body, not by `Content-Type`, which a gateway may drop. A dropped header must not let a local call run.
- An SSE body is edited line by line, so CRLF line endings survive.
- `tool_result` content given as a list of blocks is replaced too.
- Non-dict entries anywhere in the body are skipped, not crashed on.

- [ ] **Step 1: Write the failing tests**

Create `tests/test_example_moca_sandbox.py`:

```python
"""Tests for examples/script-plugins/moca-sandbox.py."""

import importlib.util
import json
import os
import pathlib
import re
import subprocess
import sys
import textwrap

import pytest
import yaml

REPO = pathlib.Path(__file__).resolve().parent.parent
SDK = REPO / "core" / "plugins" / "script" / "sdk" / "cortex.py"
SCRIPT = REPO / "examples" / "script-plugins" / "moca-sandbox.py"
GUIDE = REPO / "docs" / "script-plugin.md"

SANDBOX_TOOL = {"name": "mcp__sandbox__bash", "input_schema": {"type": "object"}}
LOCAL_TOOLS = [{"name": n, "input_schema": {"type": "object"}} for n in ("Bash", "Read", "Skill")]


@pytest.fixture
def sdk(monkeypatch):
    """A fresh SDK with the example's hooks registered on it. run() is stubbed, so
    loading the script does not serve stdin."""
    spec = importlib.util.spec_from_file_location("cortex", SDK)
    cortex = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(cortex)
    cortex.run = lambda enforce=False: None
    monkeypatch.setitem(sys.modules, "cortex", cortex)
    spec = importlib.util.spec_from_file_location("moca_sandbox", SCRIPT)
    spec.loader.exec_module(importlib.util.module_from_spec(spec))
    return cortex


def request(tools, messages=None, session="s1"):
    body = {"model": "claude-opus-5-5", "messages": messages or [{"role": "user", "content": "hi"}]}
    if tools is not None:
        body["tools"] = tools
    return {"hook": "request", "session": {"id": session} if session else {}, "request": {"body": json.dumps(body)}}


def sent(reply):
    return json.loads(reply["request"]["body"])


def test_local_tools_are_hidden_in_a_sandbox_session(sdk):
    reply, _ = sdk.handle(request([SANDBOX_TOOL] + LOCAL_TOOLS))
    assert [t["name"] for t in sent(reply)["tools"]] == ["mcp__sandbox__bash", "Skill"]
    assert reply["invocation"]["details"]["hidden"] == "Bash,Read"


def test_a_session_without_the_sandbox_tools_is_untouched(sdk):
    reply, _ = sdk.handle(request(LOCAL_TOOLS))
    assert reply == {}


def test_a_request_without_a_tool_list_is_covered_once_the_session_is_known(sdk):
    history = [
        {"role": "assistant", "content": [{"type": "tool_use", "id": "t1", "name": "Read", "input": {}}]},
        {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t1", "content": "file text"}]},
    ]
    assert sdk.handle(request(None, history, session="s2"))[0] == {}  # session not known yet
    sdk.handle(request([SANDBOX_TOOL], session="s2"))
    reply, _ = sdk.handle(request(None, history, session="s2"))
    result = sent(reply)["messages"][1]["content"][0]
    assert result["is_error"] is True and "file text" not in result["content"]


def test_a_local_result_is_replaced_and_a_sandbox_result_kept(sdk):
    history = [
        {
            "role": "assistant",
            "content": [
                {"type": "tool_use", "id": "t1", "name": "Bash", "input": {}},
                {"type": "tool_use", "id": "t2", "name": "mcp__sandbox__bash", "input": {}},
            ],
        },
        {
            "role": "user",
            "content": [
                {"type": "tool_result", "tool_use_id": "t1", "content": "local secret"},
                {"type": "tool_result", "tool_use_id": "t2", "content": "sandbox output"},
            ],
        },
    ]
    reply, _ = sdk.handle(request([SANDBOX_TOOL], history))
    local, remote = sent(reply)["messages"][1]["content"]
    assert local["is_error"] is True
    assert "local secret" not in local["content"] and "mcp__sandbox__" in local["content"]
    assert remote == {"type": "tool_result", "tool_use_id": "t2", "content": "sandbox output"}
    assert reply["invocation"]["details"]["refused_results"] == "1"


# Claude Code sends a tool_result's content as blocks too, and a request may carry
# no session id; neither may let a local result through.
def test_a_block_list_result_without_a_session_is_replaced(sdk):
    history = [
        {"role": "assistant", "content": [{"type": "tool_use", "id": "t1", "name": "Read", "input": {}}]},
        {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t1", "content": [{"type": "text", "text": "file text"}]}]},
    ]
    reply, _ = sdk.handle(request([SANDBOX_TOOL], history, session=None))
    result = sent(reply)["messages"][1]["content"][0]
    assert result["is_error"] is True and "file text" not in json.dumps(result)
    assert sdk.handle(request(None, history, session=None))[0] == {}


def response(body, ctype, tools=(SANDBOX_TOOL,), session="r1"):
    return {
        "hook": "response",
        "session": {"id": session},
        "request": {"inference": {"tools": [{"name": t["name"]} for t in tools]}},
        "response": {"status": 200, "headers": {"content-type": ctype}, "body": body},
    }


def sse(*events):
    return "".join("event: %s\ndata: %s\n\n" % (e["type"], json.dumps(e)) for e in events)


START_BASH = {
    "type": "content_block_start",
    "index": 1,
    "content_block": {"type": "tool_use", "id": "t9", "name": "Bash", "input": {}},
}
STREAM = [
    {"type": "message_start", "message": {"id": "m", "content": []}},
    {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}},
    {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "running Bash"}},
    {"type": "content_block_stop", "index": 0},
    START_BASH,
    {"type": "content_block_delta", "index": 1, "delta": {"type": "input_json_delta", "partial_json": "{}"}},
    {"type": "content_block_stop", "index": 1},
    {"type": "message_stop"},
]


def test_a_streamed_local_call_is_renamed_and_nothing_else_changes(sdk):
    body = sse(*STREAM)
    reply, _ = sdk.handle(response(body, "text/event-stream"))
    out = reply["response"]["body"]
    renamed = "data: " + json.dumps(
        dict(START_BASH, content_block=dict(START_BASH["content_block"], name="blocked__Bash")),
        separators=(",", ":"),
    )
    assert out == body.replace("data: " + json.dumps(START_BASH), renamed)
    assert reply["invocation"]["details"]["tools"] == "Bash"


# The stream is told apart by not being JSON, not by its Content-Type, which a
# gateway may drop: without it the local call would run.
def test_a_stream_without_a_content_type_is_still_renamed(sdk):
    reply, _ = sdk.handle(response(sse(*STREAM), ""))
    assert '"name":"blocked__Bash"' in reply["response"]["body"]


def test_crlf_line_endings_are_kept(sdk):
    body = sse(*STREAM).replace("\n", "\r\n")
    reply, _ = sdk.handle(response(body, "text/event-stream"))
    out = reply["response"]["body"]
    assert '"name":"blocked__Bash"' in out
    assert out.count("\r\n") == body.count("\r\n") and "\n" not in out.replace("\r\n", "")


def test_a_json_local_call_is_renamed(sdk):
    body = {
        "content": [
            {"type": "text", "text": "ok"},
            {"type": "tool_use", "id": "t1", "name": "Read", "input": {}},
            {"type": "tool_use", "id": "t2", "name": "mcp__sandbox__read", "input": {}},
        ]
    }
    reply, _ = sdk.handle(response(json.dumps(body), "application/json"))
    names = [b.get("name") for b in json.loads(reply["response"]["body"])["content"]]
    assert names == [None, "blocked__Read", "mcp__sandbox__read"]


def test_sandbox_calls_and_other_sessions_are_untouched(sdk):
    sandbox_only = dict(START_BASH, content_block=dict(START_BASH["content_block"], name="mcp__sandbox__bash"))
    assert sdk.handle(response(sse(sandbox_only), "text/event-stream"))[0] == {}
    # Its own session: r1 is now known to be a sandbox session.
    assert sdk.handle(response(sse(START_BASH), "text/event-stream", tools=LOCAL_TOOLS, session="r2"))[0] == {}


# A request with no tool list (a count_tokens call, a compaction) still belongs to a
# sandbox session once one of its requests listed the sandbox tools.
def test_a_response_in_a_known_session_without_a_tool_list_is_renamed(sdk):
    sdk.handle(request([SANDBOX_TOOL], session="r3"))
    reply, _ = sdk.handle(response(sse(START_BASH), "text/event-stream", tools=(), session="r3"))
    assert '"name":"blocked__Bash"' in reply["response"]["body"]


def test_a_body_that_is_not_json_is_left_alone(sdk):
    assert sdk.handle(response("upstream error", "text/plain"))[0] == {}
    assert sdk.handle(request([SANDBOX_TOOL]) | {"request": {"body": "not json"}})[0] == {}


# cp keeps the mode the checkout gave the file. Without the exec bit the slot sees
# no script at all: it is empty, so even fail_closed refuses nothing.
def test_the_script_is_executable():
    assert os.access(SCRIPT, os.X_OK)


def test_handshake_enforces_and_rewrites_responses():
    env = dict(os.environ, PYTHONPATH=str(SDK.parent))
    proc = subprocess.run(
        [sys.executable, str(SCRIPT)],
        input="",
        capture_output=True,
        text=True,
        env=env,
        timeout=30,
    )
    hs = json.loads(proc.stdout.splitlines()[0])
    assert hs["enforce"] is True and hs["rewrite_response"] is True
    assert hs["hooks"] == ["request", "response"]


# The install steps in the guide must match the script: the folder it is copied
# to is the slot's dir, the slot fails closed, and the MCP server it registers is
# the one the script keys on.
def test_the_guides_install_steps_match_the_script():
    section = GUIDE.read_text().split("### moca-sandbox.py", 1)[1].split("\n## ", 1)[0]
    blocks = re.findall(r"```(\w+)\n(.*?)```", section, re.S)
    sh = [textwrap.dedent(b) for lang, b in blocks if lang == "sh"]
    (slot,) = yaml.safe_load(textwrap.dedent(next(b for lang, b in blocks if lang == "yaml")))
    assert slot == {"name": "script", "config": {"dir": "moca-sandbox", "fail_closed": True}}
    assert "~/.cortex/plugins/moca-sandbox/" in sh[0] and SCRIPT.name in sh[0] and "chmod +x" in sh[0]
    server = re.search(r"claude mcp add -s local (\S+) --", sh[1]).group(1)
    assert "mcp__%s__" % server in SCRIPT.read_text()
```

- [ ] **Step 2: Run them to verify they fail**

Run: `pytest tests/test_example_moca_sandbox.py -q`
Expected: FAIL. Every test errors with `FileNotFoundError` on `examples/script-plugins/moca-sandbox.py`, and the guide test with `IndexError`, since the section is missing.

- [ ] **Step 3: Write the script**

Create `examples/script-plugins/moca-sandbox.py`:

```python
#!/usr/bin/env python3
"""Keeps Claude Code on moca's sandbox tools without a project .claude/settings.json.

moca (github.com/rossoctl/moca) runs an agent's tools in a remote microVM and
offers them as the `sandbox` MCP server. `mocactl sbx up` denies Claude Code's
local tools in the project's .claude/settings.json, and that deny also stops
Claude Code loading CLAUDE.md (rossoctl/moca#568). This script does the deny's
job in Cortex instead, so the settings file is not needed.

It acts only in a session whose tool list carries an mcp__sandbox__ tool:

  request   the local tools are removed from the list the model sees, and the
            result of any local call that ran anyway is replaced with an error;
  response  a local tool call the model makes anyway is renamed blocked__<name>,
            so Claude Code answers "No such tool available" and runs nothing.

Both halves are needed: Claude Code runs a tool the model names even when that
tool was not in the list.

Install it as its own slot, failing closed — see "Examples" in
docs/script-plugin.md.
"""

import json

from cortex import on, run

# The tools `mocactl sbx up` denies: CLAUDE_DENY in moca's agent-config.ts.
LOCAL = {
    "Bash",
    "Read",
    "Edit",
    "Write",
    "Glob",
    "Grep",
    "NotebookEdit",
    "WebFetch",
    "WebSearch",
    "Agent",
    "Task",
    "EnterWorktree",
    "ExitWorktree",
    "Workflow",
    "Monitor",
    "PowerShell",
    "LSP",
}
SANDBOX_PREFIX = "mcp__sandbox__"
BLOCKED_PREFIX = "blocked__"
REFUSAL = (
    "Error: %s does not run in this session. The code is in a remote sandbox at /workspace: "
    "use the " + SANDBOX_PREFIX + " tools."
)

# Sessions seen carrying the sandbox tools, so a request with no tool list is still
# covered. Kept in memory: a restarted script relearns each session from its next
# request that lists tools.
sandbox_sessions = set()


def _dicts(v):
    return [x for x in v if isinstance(x, dict)] if isinstance(v, list) else []


def _is_sandbox(flow, tools):
    if any(str(t.get("name")).startswith(SANDBOX_PREFIX) for t in _dicts(tools)):
        if flow.session:
            sandbox_sessions.add(flow.session)
        return True
    return bool(flow.session) and flow.session in sandbox_sessions


@on.request
def hide_local_tools(flow):
    body = flow.json
    if not isinstance(body, dict) or not _is_sandbox(flow, body.get("tools")):
        return
    hidden = []
    if isinstance(body.get("tools"), list):
        hidden = sorted({t.get("name") for t in _dicts(body["tools"])} & LOCAL)
        body["tools"] = [t for t in body["tools"] if not (isinstance(t, dict) and t.get("name") in LOCAL)]
    messages = _dicts(body.get("messages"))
    ran = {}
    for m in messages:
        if m.get("role") == "assistant":
            for b in _dicts(m.get("content")):
                if b.get("type") == "tool_use" and b.get("name") in LOCAL and isinstance(b.get("id"), str):
                    ran[b["id"]] = b["name"]
    refused = 0
    for m in messages:
        if m.get("role") == "user":
            for b in _dicts(m.get("content")):
                use = b.get("tool_use_id")
                if b.get("type") == "tool_result" and isinstance(use, str) and use in ran:
                    b["content"] = REFUSAL % ran[use]
                    b["is_error"] = True
                    refused += 1
    if hidden or refused:
        flow.set_body(body)
        flow.record("modify", "local_tools_hidden", hidden=",".join(hidden), refused_results=refused)


def _block(block):
    """Renames block when it is a local tool call, and returns the name it had."""
    if isinstance(block, dict) and block.get("type") == "tool_use" and block.get("name") in LOCAL:
        name = block["name"]
        block["name"] = BLOCKED_PREFIX + name
        return name
    return None


def _block_sse(text):
    """Renames local tool calls in an Anthropic event stream. Only the data line
    that starts each one changes; every other byte, line endings included, is kept."""
    blocked = []
    lines = text.split("\n")
    for i, line in enumerate(lines):
        if not line.startswith("data:"):
            continue
        try:
            data = json.loads(line[5:])  # a trailing \r is whitespace to json
        except ValueError:
            continue
        if not isinstance(data, dict) or data.get("type") != "content_block_start":
            continue
        name = _block(data.get("content_block"))
        if name:
            blocked.append(name)
            lines[i] = "data: " + json.dumps(data, separators=(",", ":")) + ("\r" if line.endswith("\r") else "")
    return "\n".join(lines), blocked


def _block_json(text):
    """Renames local tool calls in a JSON response. None when text is not JSON,
    which is how a stream is told apart: its Content-Type may be missing."""
    try:
        body = json.loads(text)
    except ValueError:
        return None
    content = body.get("content") if isinstance(body, dict) else None
    blocked = [n for n in map(_block, _dicts(content)) if n]
    if not blocked:
        return text, []
    return json.dumps(body, ensure_ascii=False, separators=(",", ":")), blocked


@on.response(rewrite=True)
def block_local_calls(flow):
    if not _is_sandbox(flow, (flow.inference or {}).get("tools")):
        return
    body = flow.response.body
    if not body:
        return
    result = _block_json(body)
    new, blocked = result if result is not None else _block_sse(body)
    if blocked:
        flow.set_body(new)
        flow.record("modify", "local_call_blocked", tools=",".join(blocked))


run(enforce=True)
```

Then:

```bash
chmod +x examples/script-plugins/moca-sandbox.py
```

- [ ] **Step 4: Write the docs section**

In `docs/script-plugin.md`, insert before `## Without the SDK`:

````markdown
## Examples

Scripts written for a particular setup live in
[`examples/script-plugins/`](../examples/script-plugins/). Cortex does not install
them; copy the ones you want.

### moca-sandbox.py: keep Claude Code on moca's sandbox tools

[moca](https://github.com/rossoctl/moca) runs an agent's tools in a remote
sandbox. To keep Claude Code off its local tools, `mocactl sbx up` denies them in
the repo's `.claude/settings.json`. That deny also stops Claude Code loading
`CLAUDE.md` (rossoctl/moca#568).

This script does the deny's job instead, in sessions that carry moca's
`mcp__sandbox__` tools:

- it removes the local tools from the list the model sees;
- it renames a local tool call the model makes anyway, so Claude Code runs
  nothing;
- it replaces the result of a local call that already ran with an error.

Other sessions are untouched.

Install it in a slot of its own, failing closed:

1. Copy the script, from a checkout of this repo:

   ```sh
   mkdir -p ~/.cortex/plugins/moca-sandbox
   cp examples/script-plugins/moca-sandbox.py ~/.cortex/plugins/moca-sandbox/
   chmod +x ~/.cortex/plugins/moca-sandbox/moca-sandbox.py
   ```

2. Add the slot as the last outbound entry of `~/.cortex/config.yaml`:

   ```yaml
         - name: script
           config:
             dir: moca-sandbox
             fail_closed: true
   ```

   Cortex reloads it; no restart.

3. In the repo, create the sandbox without the settings file, and register the
   sandbox server for this repo only:

   ```sh
   mocactl sbx up --no-agent-config
   claude mcp add -s local sandbox -- mocactl sbx mcp
   ```

Check it in agentop's plugin pane: the `moca-sandbox` slot shows `scripts loaded`
1 and `scripts not started` 0.

What it costs, and what it does not cover:

- **Streams are held back for every session.** The script rewrites responses, so
  Cortex buffers each streamed response until it is complete, not only moca's.
- **No Cortex, no protection.** Claude Code must reach the model through Cortex
  (`HTTPS_PROXY`).
- **A script that does not start blocks all traffic** through its slot, by
  `fail_closed`, until it is fixed.
- **Claude Code's own tools stay visible:** `Skill`, `SendMessage`, the `Cron`
  tools and others are not on moca's list.
- **Approve each sandbox tool once,** or run in auto mode. "Don't ask again" in a
  prompt writes `.claude/settings.local.json` in the repo. Headless runs need
  `--allowedTools 'mcp__sandbox__*'`.

````

- [ ] **Step 5: Run the tests**

Run: `pytest tests/test_example_moca_sandbox.py tests/test_script_sdk.py -q`
Expected: PASS (15 + 71).

Run: `ruff check examples && ruff format --check examples`
Expected: `All checks passed!` and `1 file already formatted`.

Run: `pytest tests/ -q -x --ignore=tests/e2e`
Expected: PASS. This is CI's Python job.

- [ ] **Step 6: Commit**

```bash
git add examples/script-plugins/moca-sandbox.py tests/test_example_moca_sandbox.py docs/script-plugin.md
git ls-files -s examples/script-plugins/moca-sandbox.py   # must start with 100755
git commit -s -m "feat: Add a moca-sandbox example script plugin" \
  -m "Keeps Claude Code on moca's sandbox tools without a project .claude/settings.json, whose Read deny stops CLAUDE.md loading (rossoctl/moca#568)." \
  -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 5: Whole-branch verification and a live check

**Files:** none changed, unless a check fails. A failure goes back to the task that owns the code.

This runs a **scratch** Cortex under its own `HOME`, on ports 49700-49704. It never touches the shared proxy on `:47600`. The scratch Cortex from the spike (`/tmp/script-try`, 49600) is left alone.

- [ ] **Step 1: Run every check the PR's CI runs on these modules**

```bash
R=$(git rev-parse --show-toplevel)
(cd "$R/core" && GOWORK=off go vet ./... && GOWORK=off go test -race -count=1 ./plugins/script/... ./reloader/...)
(cd "$R/cmd/cortex" && GOWORK=off go vet . && GOWORK=off go build . && \
  for p in local full lite; do T=$(go -C ../../scripts/profile-tags run . $p); \
  GOWORK=off go vet -tags "$T" . && GOWORK=off go test -race -count=1 -tags "$T" . || exit 1; done)
gofmt -l "$R/core/plugins/script" "$R/core/reloader" "$R/cmd/cortex"
(cd "$R" && pytest tests/ -q -x --ignore=tests/e2e && ruff check examples && ruff format --check examples)
for m in core cmd/cortex; do (cd "$R/$m" && GOWORK=off go mod tidy -diff) || echo "tidy: $m"; done
```

Expected: every command passes, `gofmt -l` prints nothing, no `tidy:` line.

- [ ] **Step 2: Start a scratch Cortex with the example installed**

```bash
R=$(git rev-parse --show-toplevel); S=/tmp/moca-sandbox-live; rm -rf $S; mkdir -p $S/home
(cd "$R/cmd/cortex" && GOWORK=off go build -tags "$(go -C ../../scripts/profile-tags run . full)" -o $S/cortex .)
HOME=$S/home $S/cortex --local --write-config
C=$S/home/.cortex/config.yaml
sed -i 's/127\.0\.0\.1:4760\([0-4]\)/127.0.0.1:4970\1/' $C
grep -n '127.0.0.1:49' $C              # five lines, 49700-49704
HOME=$S/home nohup $S/cortex --config $C > $S/cortex.log 2>&1 &
sleep 2; curl -s --noproxy '*' http://127.0.0.1:49702/reload/status
```

Expected: `grep` prints five listener lines on 49700-49704, none on 4760x. `reload/status` answers JSON with `reloads_ok` 0.

Then install the example exactly as the docs say, with `HOME` pointing at the scratch folder:

```bash
mkdir -p $S/home/.cortex/plugins/moca-sandbox
cp "$R/examples/script-plugins/moca-sandbox.py" $S/home/.cortex/plugins/moca-sandbox/
chmod +x $S/home/.cortex/plugins/moca-sandbox/moca-sandbox.py
printf '      - name: script\n        config:\n          dir: moca-sandbox\n          fail_closed: true\n' >> $C
sleep 3
curl -s --noproxy '*' http://127.0.0.1:49701/v1/pipeline | python3 -c '
import json, sys
for p in json.load(sys.stdin)["outbound"]:
    if p["name"] == "script":
        m = {x["name"]: x["value"] for x in p.get("metrics") or []}
        print(p["position"], json.dumps(p.get("config")), m.get("scripts loaded"), m.get("scripts not started"))'
```

Expected: two `script` rows. First the default slot, config `null` and `scripts loaded` 0, then `{"dir": "moca-sandbox", "fail_closed": true}` with `scripts loaded` 1 and `scripts not started` 0. No restart happened: the added folder loaded on reload (Tasks 2 and 3).

- [ ] **Step 3: The folder is watched without a restart**

```bash
F=$S/home/.cortex/plugins/moca-sandbox/moca-sandbox.py
cp $F $S/good.py
printf 'syntax error here\n' >> $F
sleep 3
curl -s --noproxy '*' http://127.0.0.1:49701/v1/pipeline | grep -o '"scripts not started","value":[0-9]*' | tail -1
```

Expected: `"scripts not started","value":1`. The edit was seen without a restart (Task 2).

- [ ] **Step 4: A script that did not start refuses traffic**

The spike's mock LLM answers any Anthropic Messages request. Start it on 49710 and send one request through the scratch Cortex:

```bash
(python3 -I /tmp/moca-spike/mock_toolcall.py 49710 Bash '{"command": "touch /tmp/moca-sandbox-live/PWNED"}' $S/llm.jsonl > /dev/null 2>&1 &)
sleep 0.5
curl -s -x http://127.0.0.1:49700 -H 'content-type: application/json' -H 'x-api-key: dummy' \
  -w '\n[HTTP %{http_code}]\n' \
  -d '{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}' \
  http://127.0.0.1:49710/v1/messages
```

Expected: `[HTTP 502]` with a body naming `plugin.script-failed` and `script moca-sandbox did not start`. `$S/llm.jsonl` is empty: nothing reached the upstream (Task 1).

Restore the script and send the same request:

```bash
cp $S/good.py $F; sleep 3
curl -s -x http://127.0.0.1:49700 -H 'content-type: application/json' -H 'x-api-key: dummy' \
  -w '\n[HTTP %{http_code}]\n' \
  -d '{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}' \
  http://127.0.0.1:49710/v1/messages
```

Expected: `[HTTP 200]`. The request had no `mcp__sandbox__` tool, so the stream passes unchanged and still names `Bash`.

- [ ] **Step 5: A sandbox session's local call is renamed**

```bash
curl -s -x http://127.0.0.1:49700 -H 'content-type: application/json' -H 'x-api-key: dummy' \
  -H 'X-Claude-Code-Session-Id: live-1' -w '\n[HTTP %{http_code}]\n' \
  -d '{"model":"claude-sonnet-4-5","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}],
       "tools":[{"name":"mcp__sandbox__bash","input_schema":{"type":"object"}},{"name":"Bash","input_schema":{"type":"object"}}]}' \
  http://127.0.0.1:49710/v1/messages | grep -o '"name":"[^"]*"'
tail -1 $S/llm.jsonl | python3 -c 'import json,sys; print([t["name"] for t in json.load(sys.stdin)["tools"]])'
```

Expected: the response names `"name":"blocked__Bash"`, and the upstream saw only `['mcp__sandbox__bash']`.

- [ ] **Step 6: Tear the scratch setup down**

```bash
pkill -f "mock_toolcall.py 49710"; pkill -f "$S/cortex --config"; sleep 1; rm -rf $S
lsof -nP -iTCP:49700-49710 -sTCP:LISTEN || echo "ports free"
```

Expected: `ports free`.

- [ ] **Step 7: Report**

Give Hai the results of Steps 1-5 as a short list, and ask before pushing or opening the PR. The PR body ends with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`, is passed with `--body-file`, and lists the known gaps from this plan's Review Focus and the spec's "Known costs and open items".
