package reloader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/pipeline"
)

// buildFn captures the caller-supplied pipeline results so tests can
// steer each reload attempt's outcome (success with a given plugin
// list, build-time failure, Start-time failure).
// errBuildRefused is a build failure with a recognisable message.
var errBuildRefused = errors.New("build refused by the fixture")

type fakeBuilder struct {
	// next is the result returned by the next build() call. Tests
	// replace this before writing to the config file. The PipelineBuilder
	// closure returned by (*fakeBuilder).build captures `b` and reads
	// `next` at call time.
	next atomic.Value // holds builderResult
	cfg  atomic.Value // holds *config.Config
}

type builderResult struct {
	inbound  *pipeline.Pipeline
	outbound *pipeline.Pipeline
	cfg      *config.Config
	err      error
}

func (b *fakeBuilder) set(r builderResult) {
	b.next.Store(r)
	if r.cfg != nil {
		b.cfg.Store(r.cfg)
	}
}

func (b *fakeBuilder) build() (*pipeline.Pipeline, *pipeline.Pipeline, *config.Config, error) {
	r := b.next.Load().(builderResult)
	return r.inbound, r.outbound, r.cfg, r.err
}

// emptyPipeline returns a no-plugin pipeline, used for tests that
// don't exercise plugin behavior — only swap plumbing.
func emptyPipeline(t *testing.T) *pipeline.Pipeline {
	t.Helper()
	p, err := pipeline.New(nil)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	return p
}

// writeConfig writes `content` atomically into path via rename, the
// same sequence an editor / kubectl apply ultimately performs.
func writeConfig(t *testing.T, path string, content string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("Rename: %v", err)
	}
}

// waitFor polls `cond` up to `timeout` at 10ms intervals. Used to
// observe async reload completion without baked-in sleeps.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", msg)
}

func setup(t *testing.T) (*Reloader, *fakeBuilder, string, *pipeline.Holder, *pipeline.Holder) {
	t.Helper()
	return setupWith(t)
}

// setup creates a temp dir with an initial config file, builds a
// Reloader with short debounce + no drain window, and Starts it.
// setupWith is setup with extra options, for the tests that need to observe a hook.
func setupWith(t *testing.T, extra ...Option) (*Reloader, *fakeBuilder, string, *pipeline.Holder, *pipeline.Holder) {
	t.Helper()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("mode: envoy-sidecar\n"), 0o600); err != nil {
		t.Fatalf("initial WriteFile: %v", err)
	}

	initialCfg := &config.Config{Mode: "envoy-sidecar"}
	inP := emptyPipeline(t)
	outP := emptyPipeline(t)
	inH := pipeline.NewHolder(inP)
	outH := pipeline.NewHolder(outP)

	b := &fakeBuilder{}
	// Seed with a result for the first reload. Tests overwrite this
	// before the config file is next written.
	b.set(builderResult{inbound: emptyPipeline(t), outbound: emptyPipeline(t), cfg: initialCfg})

	r := New(cfgPath, inH, outH, b.build, initialCfg,
		append([]Option{
			WithDebounce(20 * time.Millisecond),
			WithDrainWindow(0),
			WithStartTimeout(5 * time.Second),
		}, extra...)...,
	)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	return r, b, cfgPath, inH, outH
}

// Plain file rewrite → reload fires; the pipeline Holders point at the
// new pipelines after the swap.
func TestReloader_PlainRewrite(t *testing.T) {
	r, b, cfgPath, inH, outH := setup(t)

	newIn := emptyPipeline(t)
	newOut := emptyPipeline(t)
	b.set(builderResult{inbound: newIn, outbound: newOut, cfg: &config.Config{Mode: "envoy-sidecar"}})

	writeConfig(t, cfgPath, "mode: envoy-sidecar\n# edited\n")

	waitFor(t, 2*time.Second, func() bool { return r.Status().ReloadsOK >= 1 }, "reload to succeed")
	if inH.Load() != newIn {
		t.Errorf("inbound holder: not swapped")
	}
	if outH.Load() != newOut {
		t.Errorf("outbound holder: not swapped")
	}
}

// Two writes within the debounce window coalesce into a single reload.
func TestReloader_DebouncesBurst(t *testing.T) {
	r, b, cfgPath, _, _ := setup(t)
	b.set(builderResult{inbound: emptyPipeline(t), outbound: emptyPipeline(t), cfg: &config.Config{Mode: "envoy-sidecar"}})

	writeConfig(t, cfgPath, "mode: envoy-sidecar\n# edit 1\n")
	writeConfig(t, cfgPath, "mode: envoy-sidecar\n# edit 2\n")

	waitFor(t, 2*time.Second, func() bool { return r.Status().ReloadsOK >= 1 }, "first reload")
	// Give the debounce a chance to fire a second time if it's going to.
	time.Sleep(100 * time.Millisecond)

	if got := r.Status().ReloadsOK; got > 1 {
		t.Errorf("expected coalesced single reload, got %d", got)
	}
}

// A write that produces identical bytes is a no-op — no reload, no
// failed-counter bump.
func TestReloader_ContentHashDedup(t *testing.T) {
	r, _, cfgPath, _, _ := setup(t)
	before := r.Status()

	// Touch the file with the same content it already had.
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	writeConfig(t, cfgPath, string(data))

	time.Sleep(200 * time.Millisecond)

	after := r.Status()
	if after.ReloadsOK != before.ReloadsOK || after.ReloadsFailed != before.ReloadsFailed {
		t.Errorf("counters changed on no-op write: before=%+v after=%+v", before, after)
	}
}

// Mode change is unreloadable; reload is refused; holders unchanged.
func TestReloader_RefusesModeChange(t *testing.T) {
	r, b, cfgPath, inH, _ := setup(t)

	oldInPipeline := inH.Load()
	newIn := emptyPipeline(t)
	newOut := emptyPipeline(t)
	b.set(builderResult{inbound: newIn, outbound: newOut, cfg: &config.Config{Mode: "proxy-sidecar"}})

	writeConfig(t, cfgPath, "mode: proxy-sidecar\n")

	waitFor(t, 2*time.Second, func() bool { return r.Status().ReloadsFailed >= 1 }, "reload to fail")
	if inH.Load() != oldInPipeline {
		t.Errorf("inbound holder mutated despite refused reload")
	}
	if r.Status().LastError == "" || !contains(r.Status().LastError, "mode") {
		t.Errorf("error should mention mode, got %q", r.Status().LastError)
	}
}

// Listener address change is unreloadable.
func TestReloader_RefusesListenerChange(t *testing.T) {
	r, b, cfgPath, inH, _ := setup(t)

	oldInPipeline := inH.Load()
	newCfg := &config.Config{
		Mode:     "envoy-sidecar",
		Listener: config.ListenerConfig{ExtProcAddr: ":19090"}, // different from active (empty)
	}
	b.set(builderResult{inbound: emptyPipeline(t), outbound: emptyPipeline(t), cfg: newCfg})

	writeConfig(t, cfgPath, "mode: envoy-sidecar\nlistener: {ext_proc_addr: :19090}\n")

	waitFor(t, 2*time.Second, func() bool { return r.Status().ReloadsFailed >= 1 }, "reload to fail")
	if inH.Load() != oldInPipeline {
		t.Errorf("inbound holder mutated despite refused reload")
	}
	if got := r.Status().LastError; !contains(got, "listener") {
		t.Errorf("error should mention listener, got %q", got)
	}
}

// A cost_ledger edit must be REFUSED, not accepted-and-discarded.
//
// This is the defect the guard was added for: the ledger is a *ledger.Writer
// opened once at startup and handed to the session store as a Recorder, so a reload
// cannot reach it. Before the guard, editing `enabled: true` to `enabled: false`
// incremented ReloadsOK, published a new ActiveConfigSHA256 on /reload/status, and
// made /config serve `enabled: false` while the writer kept appending — three
// independent signals all telling an operator the edit took effect.
//
// Deliberately driven by a REAL config.Load builder rather than the fakeBuilder the
// tests above use. A fake would only prove validateReloadable compares the field it
// is handed; this proves the yaml key reaches that comparison, so a wiring mistake
// (block renamed, field dropped from the struct, guard called with the wrong config)
// fails here too. The four assertions are the four things an operator would read as
// confirmation.
func TestReloader_RefusesCostLedgerChange(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	const before = "mode: envoy-sidecar\ncost_ledger:\n  enabled: true\n  retention_days: 9\n"
	if err := os.WriteFile(cfgPath, []byte(before), 0o600); err != nil {
		t.Fatalf("initial WriteFile: %v", err)
	}

	initialCfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !initialCfg.CostLedger.LedgerEnabled(false) {
		t.Fatalf("fixture is wrong: the ledger must start ENABLED for the edit below to be the dangerous one")
	}

	// Built up front, not inside the closure: build() runs on the watch goroutine,
	// and t.Fatalf from a non-test goroutine is not allowed.
	newIn, newOut := emptyPipeline(t), emptyPipeline(t)
	build := func() (*pipeline.Pipeline, *pipeline.Pipeline, *config.Config, error) {
		c, lerr := config.Load(cfgPath)
		if lerr != nil {
			return nil, nil, nil, lerr
		}
		return newIn, newOut, c, nil
	}

	inH := pipeline.NewHolder(emptyPipeline(t))
	outH := pipeline.NewHolder(emptyPipeline(t))
	oldIn, oldOut := inH.Load(), outH.Load()

	r := New(cfgPath, inH, outH, build, initialCfg,
		WithDebounce(20*time.Millisecond), WithDrainWindow(0), WithStartTimeout(5*time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	shaBefore := r.Status().ActiveConfigSHA256

	// The edit an operator makes to stop writing cost history.
	writeConfig(t, cfgPath, "mode: envoy-sidecar\ncost_ledger:\n  enabled: false\n  retention_days: 9\n")

	waitFor(t, 2*time.Second, func() bool { return r.Status().ReloadsFailed >= 1 }, "reload to be refused")

	st := r.Status()
	if !contains(st.LastError, "cost_ledger") {
		t.Errorf("error must name the section the operator has to restart for, got %q", st.LastError)
	}
	if st.ReloadsOK != 0 {
		t.Errorf("ReloadsOK = %d; a refused reload must not report success", st.ReloadsOK)
	}
	if st.ActiveConfigSHA256 != shaBefore {
		t.Errorf("ActiveConfigSHA256 moved to %q on a refused reload; /reload/status would claim the edit is live",
			st.ActiveConfigSHA256)
	}
	// The one an operator is most likely to check: /config must still show the
	// ledger the running writer is actually using.
	if got := r.ConfigProvider(); !got().CostLedger.LedgerEnabled(false) {
		t.Errorf("/config now reports the ledger disabled while the startup writer is still recording")
	}
	if inH.Load() != oldIn || outH.Load() != oldOut {
		t.Errorf("holders swapped despite a refused reload")
	}
}

// PipelineBuilder error (e.g., config.Validate rejects) → reload fails;
// holders unchanged; error reflected in Status.
func TestReloader_BuilderError(t *testing.T) {
	r, b, cfgPath, inH, _ := setup(t)
	oldInPipeline := inH.Load()

	b.set(builderResult{err: &fakeErr{"synthetic build error"}})
	writeConfig(t, cfgPath, "mode: envoy-sidecar\n# any edit\n")

	waitFor(t, 2*time.Second, func() bool { return r.Status().ReloadsFailed >= 1 }, "reload to fail")
	if inH.Load() != oldInPipeline {
		t.Errorf("holder mutated despite failed build")
	}
	if !contains(r.Status().LastError, "synthetic build error") {
		t.Errorf("error should name the builder's error, got %q", r.Status().LastError)
	}
}

// Config provider returns the latest-active *Config after a swap.
func TestReloader_ConfigProviderReflectsSwap(t *testing.T) {
	r, b, cfgPath, _, _ := setup(t)
	provider := r.ConfigProvider()

	// Mutate a genuinely reloadable field so the swap is accepted. The plugin
	// pipeline is what the reloader exists to swap, so it is the honest choice
	// here. This test used session.ttl until session.* joined the unreloadable
	// set — which is the point: a TTL edit is now refused, because the store was
	// constructed with the old one and nothing rebuilds it.
	newCfg := &config.Config{
		Mode: "envoy-sidecar",
		Pipeline: config.PipelineConfig{
			Outbound: config.PipelineStageConfig{Plugins: []config.PluginEntry{{Name: "inference-parser"}}},
		},
	}
	b.set(builderResult{inbound: emptyPipeline(t), outbound: emptyPipeline(t), cfg: newCfg})

	writeConfig(t, cfgPath, "mode: envoy-sidecar\npipeline: {outbound: {plugins: [{name: inference-parser}]}}\n")
	waitFor(t, 2*time.Second, func() bool { return r.Status().ReloadsOK >= 1 }, "reload to succeed")

	got := provider()
	if len(got.Pipeline.Outbound.Plugins) != 1 || got.Pipeline.Outbound.Plugins[0].Name != "inference-parser" {
		t.Errorf("ConfigProvider: got outbound plugins %+v, want one inference-parser", got.Pipeline.Outbound.Plugins)
	}
}

// A session.* edit must be REFUSED, for the same reason cost_ledger.* is.
//
// This one is a correction to the PR description, which claimed session.* was
// already excluded from reload validation. It was not: validateReloadable guarded
// mode and listener.* only, and the test above deliberately used session.ttl as its
// example of a field a reload ACCEPTS. Every consumer reads the block once at
// startup (session.New in each cmd main, forwardproxy.Server.SessionIDHeaders before
// ListenAndServe), so an accepted edit changed /config and nothing else.
//
// The subtests cover the two fields with the most misleading failure mode: a ttl
// that appears to bound how long raw prompts sit in memory, and an id_headers list
// emptied to stop bucketing on a client-asserted header — a trust-boundary change
// (see SessionConfig.IDHeaders) that would have reported success and kept honouring
// the header.
func TestReloader_RefusesSessionChange(t *testing.T) {
	off := false
	for _, tc := range []struct {
		name string
		yaml string
		cfg  config.SessionConfig
	}{
		{"ttl", "session: {ttl: 15m}\n", config.SessionConfig{TTL: "15m"}},
		{"max_events", "session: {max_events: 500}\n", config.SessionConfig{MaxEvents: 500}},
		{"id_headers emptied", "session: {id_headers: []}\n", config.SessionConfig{IDHeaders: []string{}}},
		{"enabled false", "session: {enabled: false}\n", config.SessionConfig{Enabled: &off}},
		// Unset means on, so writing the off switch into a running config is a change
		// in behaviour, not a no-op — and nothing can reach the running listener.
		{"client_affinity false", "session: {client_affinity: false}\n", config.SessionConfig{ClientAffinity: &off}},
		{"process_attribution off", "session: {process_attribution: off}\n", config.SessionConfig{ProcessAttribution: config.ProcessAttributionOff}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, b, cfgPath, inH, outH := setup(t)
			oldIn, oldOut := inH.Load(), outH.Load()
			shaBefore := r.Status().ActiveConfigSHA256

			b.set(builderResult{
				inbound:  emptyPipeline(t),
				outbound: emptyPipeline(t),
				cfg:      &config.Config{Mode: "envoy-sidecar", Session: tc.cfg},
			})
			writeConfig(t, cfgPath, "mode: envoy-sidecar\n"+tc.yaml)

			waitFor(t, 2*time.Second, func() bool { return r.Status().ReloadsFailed >= 1 }, "reload to be refused")

			st := r.Status()
			if !contains(st.LastError, "session") {
				t.Errorf("error must name the section that needs a restart, got %q", st.LastError)
			}
			if st.ReloadsOK != 0 {
				t.Errorf("ReloadsOK = %d; a refused reload must not report success", st.ReloadsOK)
			}
			if st.ActiveConfigSHA256 != shaBefore {
				t.Errorf("ActiveConfigSHA256 moved on a refused reload")
			}
			if inH.Load() != oldIn || outH.Load() != oldOut {
				t.Errorf("holders swapped despite a refused reload")
			}
		})
	}
}

// --- helpers --------------------------------------------------------

type fakeErr struct{ msg string }

func (e *fakeErr) Error() string { return e.msg }

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// setupAt is setup() with the config path chosen by the caller, for layouts
// where the watched path is not a plain file in its own directory.
func setupAt(t *testing.T, cfgPath string) (*Reloader, *fakeBuilder, *pipeline.Holder, *pipeline.Holder) {
	t.Helper()

	initialCfg := &config.Config{Mode: "envoy-sidecar"}
	inH := pipeline.NewHolder(emptyPipeline(t))
	outH := pipeline.NewHolder(emptyPipeline(t))

	b := &fakeBuilder{}
	b.set(builderResult{inbound: emptyPipeline(t), outbound: emptyPipeline(t), cfg: initialCfg})

	r := New(cfgPath, inH, outH, b.build, initialCfg,
		WithDebounce(20*time.Millisecond),
		WithDrainWindow(0),
		WithStartTimeout(5*time.Second),
	)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return r, b, inH, outH
}

// A symlinked config, written THROUGH the link — the shape agentop's editor
// produces for a config pointed at a dotfiles repo, and what `agentop tools scan
// --write` / `agentop service install`'s config migration do too.
//
// Watching only filepath.Dir(configPath) misses this on Linux, where inotify
// reports directory-entry changes and the link's directory has none: the reload
// never fires, so agentop's editor times out and rolls a correct edit back two
// minutes later. macOS survives it by accident (kqueue watches the resolved
// file), which is exactly why this is pinned rather than left to the platform.
func TestReloader_SymlinkedConfigWrittenThroughTheLink(t *testing.T) {
	linkDir := t.TempDir()
	realDir := t.TempDir() // a different directory, as a dotfiles repo would be
	realPath := filepath.Join(realDir, "cortex-config.yaml")
	if err := os.WriteFile(realPath, []byte("mode: envoy-sidecar\n"), 0o600); err != nil {
		t.Fatalf("write real: %v", err)
	}
	linkPath := filepath.Join(linkDir, "config.yaml")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	r, b, inH, _ := setupAt(t, linkPath)

	// Assert the MECHANISM, not just the outcome. The outcome below passes on
	// macOS even without the extra watch, because kqueue happens to report the
	// replacement against the link — so on this platform this assertion is the
	// only thing that would catch someone removing it, and on Linux nothing
	// else would fire at all.
	if r.resolvedBase == "" {
		t.Fatal("Start added no watch for the symlink target; on Linux the reload would never fire")
	}
	if r.resolvedBase != filepath.Base(realPath) {
		t.Errorf("resolvedBase = %q, want %q", r.resolvedBase, filepath.Base(realPath))
	}

	newIn := emptyPipeline(t)
	b.set(builderResult{inbound: newIn, outbound: emptyPipeline(t), cfg: &config.Config{Mode: "envoy-sidecar"}})

	// Replace the TARGET, leaving the link intact — write-temp-then-rename, as
	// edit.FileStore.Apply does.
	writeConfig(t, realPath, "mode: envoy-sidecar\n# edited through the link\n")

	waitFor(t, 3*time.Second, func() bool { return r.Status().ReloadsOK >= 1 },
		"a reload after the symlink target was replaced")
	if inH.Load() != newIn {
		t.Error("inbound holder was not swapped")
	}
	// The link must still be a link.
	fi, err := os.Lstat(linkPath)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink did not survive")
	}
}

// The ConfigMap layout must keep working: config.yaml → ..data → a TIMESTAMPED
// directory that is deleted on every update. Making the resolved path primary
// would put the watch on a directory that vanishes, so the symlink support
// above is additive and this pins that it stayed so.
func TestReloader_ConfigMapDataSymlinkSwapStillReloads(t *testing.T) {
	root := t.TempDir()
	gen1 := filepath.Join(root, "..2026_05_08_01")
	if err := os.MkdirAll(gen1, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gen1, "config.yaml"), []byte("mode: envoy-sidecar\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dataLink := filepath.Join(root, "..data")
	if err := os.Symlink(gen1, dataLink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cfgPath := filepath.Join(root, "config.yaml")
	if err := os.Symlink(filepath.Join("..data", "config.yaml"), cfgPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	r, b, inH, _ := setupAt(t, cfgPath)

	newIn := emptyPipeline(t)
	b.set(builderResult{inbound: newIn, outbound: emptyPipeline(t), cfg: &config.Config{Mode: "envoy-sidecar"}})

	// kubelet's update: new generation dir, then swap ..data onto it.
	gen2 := filepath.Join(root, "..2026_05_08_02")
	if err := os.MkdirAll(gen2, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gen2, "config.yaml"),
		[]byte("mode: envoy-sidecar\n# generation 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tmpLink := filepath.Join(root, "..data_tmp")
	if err := os.Symlink(gen2, tmpLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmpLink, dataLink); err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(gen1)

	waitFor(t, 3*time.Second, func() bool { return r.Status().ReloadsOK >= 1 },
		"a reload after the ..data symlink swap")
	if inH.Load() != newIn {
		t.Error("inbound holder was not swapped")
	}
}

// TestReloader_ARefusedReloadCommitsNothing is the ordering the pricing swap fell through.
//
// build() runs BEFORE the unreloadable-field check, so anything it applies to live traffic is applied
// by a save this reloader then refuses. That is how a config touching both pricing.* and cost_ledger.*
// left requests priced from a rejected file while /config still served the old rates and the holders
// still held the old pipelines — the reload reported failure and the process had already changed
// behaviour. A failed pipeline Start has the same shape.
//
// The hook is the fix and this is its contract: build may PREPARE, only a commit may APPLY. Asserted
// through the reloader rather than through main's pricing wiring, because the ordering is the
// reloader's to guarantee for every side effect anyone hangs off it later.
func TestReloader_ARefusedReloadCommitsNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		newCfg   *config.Config
		buildErr error
		wantWhy  string
	}{
		{
			// Unreloadable field: build succeeds, the check refuses it.
			name:    "an unreloadable field",
			newCfg:  &config.Config{Mode: "proxy-sidecar"},
			wantWhy: "mode",
		},
		{
			// The build itself fails, so there is nothing to commit either.
			name:     "a build failure",
			newCfg:   &config.Config{Mode: "envoy-sidecar"},
			buildErr: errBuildRefused,
			wantWhy:  "build",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var commits int
			r, b, cfgPath, inH, _ := setupWith(t, WithOnCommit(func(*config.Config) { commits++ }))

			oldIn := inH.Load()
			b.set(builderResult{
				inbound: emptyPipeline(t), outbound: emptyPipeline(t),
				cfg: tc.newCfg, err: tc.buildErr,
			})
			writeConfig(t, cfgPath, "mode: "+tc.newCfg.Mode+"\n# refused\n")

			waitFor(t, 2*time.Second, func() bool { return r.Status().ReloadsFailed >= 1 }, "reload to fail")

			if commits != 0 {
				t.Errorf("commit hook ran %d times on a refused reload: whatever it applies is now live from a config this reloader rejected", commits)
			}
			if inH.Load() != oldIn {
				t.Errorf("inbound holder mutated despite a refused reload")
			}
			if !contains(r.Status().LastError, tc.wantWhy) {
				t.Errorf("LastError = %q, want it to mention %q", r.Status().LastError, tc.wantWhy)
			}
		})
	}
}

// TestReloader_AnAcceptedReloadCommitsOnce is the other half: the hook is not merely never called.
func TestReloader_AnAcceptedReloadCommitsOnce(t *testing.T) {
	var commits int
	var gotMode string
	r, b, cfgPath, _, _ := setupWith(t, WithOnCommit(func(c *config.Config) {
		commits++
		if c != nil {
			gotMode = c.Mode
		}
	}))

	b.set(builderResult{inbound: emptyPipeline(t), outbound: emptyPipeline(t),
		cfg: &config.Config{Mode: "envoy-sidecar"}})
	writeConfig(t, cfgPath, "mode: envoy-sidecar\n# accepted\n")

	waitFor(t, 2*time.Second, func() bool { return r.Status().ReloadsOK >= 1 }, "reload to succeed")
	if commits != 1 {
		t.Errorf("commit hook ran %d times, want 1", commits)
	}
	if gotMode != "envoy-sidecar" {
		t.Errorf("hook saw Mode %q, want the config that took effect", gotMode)
	}
}

// mtls needs a restart: the TLS dialer is constructed once in main and handed to the
// forward proxy listener, and nothing re-reads it. More critically, a reload that removes
// the mtls block while adding a WritesDestination plugin would let that plugin through on
// an mTLS proxy that was built with the opposite constraint — so validateReloadable blocks
// any change to mtls.* (present↔absent or field change), exactly like cost_ledger and session.
func TestValidateReloadable_RefusesMTLSEdit(t *testing.T) {
	// Test 1: adding an mtls block to a config that had none
	active := &config.Config{MTLS: nil}
	next := &config.Config{MTLS: &config.MTLSConfig{Mode: "strict"}}
	if err := validateReloadable(active, next); err == nil {
		t.Fatal("adding an mtls block was accepted")
	} else if !bytes.Contains([]byte(err.Error()), []byte("mtls.*")) {
		t.Errorf("error doesn't mention mtls.*: %v", err)
	}

	// Test 2: removing an mtls block from a config that had one
	active = &config.Config{MTLS: &config.MTLSConfig{Mode: "strict"}}
	next = &config.Config{MTLS: nil}
	if err := validateReloadable(active, next); err == nil {
		t.Fatal("removing an mtls block was accepted")
	} else if !bytes.Contains([]byte(err.Error()), []byte("mtls.*")) {
		t.Errorf("error doesn't mention mtls.*: %v", err)
	}

	// Test 3: unchanged mtls block should be accepted
	active = &config.Config{MTLS: &config.MTLSConfig{Mode: "strict"}}
	next = &config.Config{MTLS: &config.MTLSConfig{Mode: "strict"}}
	if err := validateReloadable(active, next); err != nil {
		t.Errorf("unchanged mtls block was refused: %v", err)
	}

	// Test 4: nil mtls in both should be accepted
	active = &config.Config{MTLS: nil}
	next = &config.Config{MTLS: nil}
	if err := validateReloadable(active, next); err != nil {
		t.Errorf("nil mtls in both was refused: %v", err)
	}
}

// session.archive needs a restart like the rest of `session`: the archive is constructed once
// in main and registered on the store, and nothing re-reads it. validateReloadable already
// refuses any change under Session; this pins that the archive's block is inside that net.
func TestValidateReloadable_RefusesASessionArchiveEdit(t *testing.T) {
	on, off := true, false
	active := &config.Config{Session: config.SessionConfig{Archive: &config.SessionArchiveConfig{Enabled: &on}}}
	next := &config.Config{Session: config.SessionConfig{Archive: &config.SessionArchiveConfig{Enabled: &off}}}
	if err := validateReloadable(active, next); err == nil {
		t.Fatal("a live edit of session.archive was accepted")
	}
}

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
// edit in it reloads without a restart. A folder created by hand before the
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
			// Slow, so an edit made as soon as the reload reports success would
			// land before the folder is watched if success came first.
			time.Sleep(100 * time.Millisecond)
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

// A folder watched from startup counts in the startup hash: an event that changes
// nothing does not reload, and /reload/status names what is running.
func TestReloader_StartupHashCoversTheWatchedFolders(t *testing.T) {
	extra := t.TempDir()
	if err := os.WriteFile(filepath.Join(extra, "guard.py"), []byte("v1"), 0o700); err != nil {
		t.Fatal(err)
	}
	fingerprint := func(ds []string) string {
		var s string
		for _, d := range ds {
			b, _ := os.ReadFile(filepath.Join(d, "guard.py"))
			s += d + "=" + string(b) + ";"
		}
		return s
	}
	r, b, cfgPath, _, _ := setupWith(t, WithExtraWatch(staticDirs(extra), fingerprint))
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.Status().ActiveConfigSHA256, r.hash(data); got != want {
		t.Fatalf("startup hash %q leaves out the watched folder, want %q", got, want)
	}
	b.set(builderResult{inbound: emptyPipeline(t), outbound: emptyPipeline(t), cfg: &config.Config{Mode: "envoy-sidecar"}})
	if err := os.WriteFile(filepath.Join(extra, "scratch"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if got := r.Status().ReloadsOK; got != 0 {
		t.Fatalf("an event that changed nothing reloaded %d times", got)
	}
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
	r, b, cfgPath, _, _ := setupWith(t, WithExtraWatch(dirs, func([]string) string { return "" }))
	b.set(builderResult{err: errBuildRefused, cfg: withLateSlot()})
	writeConfig(t, cfgPath, "mode: envoy-sidecar\n# with the late slot\n")
	waitFor(t, 2*time.Second, func() bool { return r.Status().ReloadsFailed >= 1 }, "the refused reload to run")
	time.Sleep(50 * time.Millisecond)
	if asked.Load() != 0 {
		t.Fatal("a refused reload asked for the new config's folders")
	}
	if _, err := os.Stat(late); !os.IsNotExist(err) {
		t.Fatalf("a refused reload created a folder: %v", err)
	}
}

// Without WithExtraWatch the hash is the config file's own sha256, which is what
// /reload/status has always published as active_config_sha256.
func TestReloader_HashIsTheConfigSHA256WithoutAFingerprint(t *testing.T) {
	r, _, cfgPath, _, _ := setup(t)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.Status().ActiveConfigSHA256, r.hash(data); got != want {
		t.Fatalf("status hash %q, want %q", got, want)
	}
	sum := sha256.Sum256(data)
	if got := r.hash(data); got != hex.EncodeToString(sum[:]) {
		t.Errorf("hash = %q, not the config's sha256", got)
	}
}

// An edit that lands while Start adds its watches is reloaded: the hash Start
// seeds comes from the config New read, not from the file as Start finds it.
func TestReloader_AnEditDuringStartIsReloaded(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("mode: envoy-sidecar\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(dir, "plugins")
	var edited atomic.Bool
	// Start asks for the folders after it watches the config's directory, so the
	// edit made here is one made while Start runs.
	dirs := func(*config.Config) []string {
		if !edited.Swap(true) {
			writeConfig(t, cfgPath, "mode: envoy-sidecar\n# edited during Start\n")
		}
		return []string{extra}
	}
	initialCfg := &config.Config{Mode: "envoy-sidecar"}
	b := &fakeBuilder{}
	b.set(builderResult{inbound: emptyPipeline(t), outbound: emptyPipeline(t), cfg: initialCfg})
	r := New(cfgPath, pipeline.NewHolder(emptyPipeline(t)), pipeline.NewHolder(emptyPipeline(t)), b.build, initialCfg,
		WithDebounce(20*time.Millisecond), WithDrainWindow(0), WithStartTimeout(5*time.Second),
		WithExtraWatch(dirs, func([]string) string { return "" }))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool { return r.Status().ReloadsOK == 1 }, "the edit made during Start to reload")
}

// tls_bridge.* and spiffe.* are built once at boot — the bridge engine in each
// cmd main and the in-process SPIFFE Provider. Before these checks, an edit to
// either block was ACCEPTED (ReloadsOK up, /config serving the new values) while
// the boot-time objects kept running: an operator adding a host to
// tls_bridge.passthrough_hosts read the reload as confirmation that it stopped
// being intercepted, and it hadn't (#1561). Same refusal shape as mtls/session.
func TestValidateReloadable_RefusesTLSBridgeAndSPIFFEEdits(t *testing.T) {
	// Adding a tls_bridge block.
	active := &config.Config{}
	next := &config.Config{TLSBridge: &config.TLSBridgeConfig{Mode: "enabled"}}
	if err := validateReloadable(active, next); err == nil {
		t.Fatal("adding a tls_bridge block was accepted")
	} else if !bytes.Contains([]byte(err.Error()), []byte("tls_bridge.*")) {
		t.Errorf("error doesn't mention tls_bridge.*: %v", err)
	}

	// The passthrough_hosts case from the issue: same block, one host added.
	active = &config.Config{TLSBridge: &config.TLSBridgeConfig{Mode: "enabled", PassthroughHosts: []string{"a.example.com"}}}
	next = &config.Config{TLSBridge: &config.TLSBridgeConfig{Mode: "enabled", PassthroughHosts: []string{"a.example.com", "b.example.com"}}}
	if err := validateReloadable(active, next); err == nil {
		t.Fatal("a passthrough_hosts edit was accepted")
	} else if !bytes.Contains([]byte(err.Error()), []byte("tls_bridge.*")) {
		t.Errorf("error doesn't mention tls_bridge.*: %v", err)
	}

	// An unchanged tls_bridge block is fine.
	active = &config.Config{TLSBridge: &config.TLSBridgeConfig{Mode: "enabled", PassthroughHosts: []string{"a.example.com"}}}
	next = &config.Config{TLSBridge: &config.TLSBridgeConfig{Mode: "enabled", PassthroughHosts: []string{"a.example.com"}}}
	if err := validateReloadable(active, next); err != nil {
		t.Errorf("unchanged tls_bridge block was refused: %v", err)
	}

	// A spiffe.* edit.
	active = &config.Config{SPIFFE: &config.SPIFFEConfig{Socket: "unix:///old.sock"}}
	next = &config.Config{SPIFFE: &config.SPIFFEConfig{Socket: "unix:///new.sock"}}
	if err := validateReloadable(active, next); err == nil {
		t.Fatal("a spiffe.* edit was accepted")
	} else if !bytes.Contains([]byte(err.Error()), []byte("spiffe.*")) {
		t.Errorf("error doesn't mention spiffe.*: %v", err)
	}

	// nil spiffe in both is fine.
	if err := validateReloadable(&config.Config{}, &config.Config{}); err != nil {
		t.Errorf("nil tls_bridge and spiffe in both was refused: %v", err)
	}
}
