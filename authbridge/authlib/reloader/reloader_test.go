package reloader

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/rossocortex/authbridge/authlib/config"
	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
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

	// Mutate a non-unreloadable field so the swap is accepted. Session
	// TTL is a plain string field, easy to compare.
	newCfg := &config.Config{
		Mode:    "envoy-sidecar",
		Session: config.SessionConfig{TTL: "15m"},
	}
	b.set(builderResult{inbound: emptyPipeline(t), outbound: emptyPipeline(t), cfg: newCfg})

	writeConfig(t, cfgPath, "mode: envoy-sidecar\nsession: {ttl: 15m}\n")
	waitFor(t, 2*time.Second, func() bool { return r.Status().ReloadsOK >= 1 }, "reload to succeed")

	got := provider()
	if got.Session.TTL != "15m" {
		t.Errorf("ConfigProvider: got TTL=%q, want 15m", got.Session.TTL)
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
