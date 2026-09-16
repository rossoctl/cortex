package reloader

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/config"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// buildFn captures the caller-supplied pipeline results so tests can
// steer each reload attempt's outcome (success with a given plugin
// list, build-time failure, Start-time failure).
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

// setup creates a temp dir with an initial config file, builds a
// Reloader with short debounce + no drain window, and Starts it.
func setup(t *testing.T) (*Reloader, *fakeBuilder, string, *pipeline.Holder, *pipeline.Holder) {
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
		WithDebounce(20*time.Millisecond),
		WithDrainWindow(0),
		WithStartTimeout(5*time.Second),
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
	b.set(builderResult{inbound: newIn, outbound: newOut, cfg: &config.Config{Mode: "waypoint"}})

	writeConfig(t, cfgPath, "mode: waypoint\n")

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
// This is the defect the guard was added for: the ledger is a *costledger.Writer
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
