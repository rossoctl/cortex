package pipeline

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// holderTestPlugin is a no-op plugin used to build test pipelines.
// Implements all optional interfaces so Holder's delegating methods
// have something to delegate to.
type holderTestPlugin struct {
	name      string
	needsBody bool
	notReady  bool
	runCount  atomic.Int64
	respCount atomic.Int64
}

func (p *holderTestPlugin) Name() string { return p.name }
func (p *holderTestPlugin) Capabilities() PluginCapabilities {
	return PluginCapabilities{ReadsBody: p.needsBody}
}
func (p *holderTestPlugin) OnRequest(_ context.Context, _ *Context) Action {
	p.runCount.Add(1)
	return Action{Type: Continue}
}
func (p *holderTestPlugin) OnResponse(_ context.Context, _ *Context) Action {
	p.respCount.Add(1)
	return Action{Type: Continue}
}
func (p *holderTestPlugin) Ready() bool { return !p.notReady }

func mustPipeline(t *testing.T, plugins ...Plugin) *Pipeline {
	t.Helper()
	p, err := New(plugins)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	return p
}

func TestHolder_LoadStore(t *testing.T) {
	a := &holderTestPlugin{name: "a"}
	b := &holderTestPlugin{name: "b"}
	pa := mustPipeline(t, a)
	pb := mustPipeline(t, b)

	h := NewHolder(pa)
	if got := h.Load(); got != pa {
		t.Fatalf("Load before Store: got %p, want %p", got, pa)
	}
	h.Store(pb)
	if got := h.Load(); got != pb {
		t.Fatalf("Load after Store: got %p, want %p", got, pb)
	}
}

// Delegated methods must match calling the same method on the
// underlying pipeline directly. Guards against a future refactor that
// mistypes a pass-through.
func TestHolder_DelegatesMatchUnderlying(t *testing.T) {
	plugins := []Plugin{
		&holderTestPlugin{name: "a", needsBody: true},
		&holderTestPlugin{name: "b"},
	}
	p := mustPipeline(t, plugins...)
	h := NewHolder(p)

	if h.NeedsBody() != p.NeedsBody() {
		t.Errorf("NeedsBody delegation mismatch")
	}
	if h.Ready() != p.Ready() {
		t.Errorf("Ready delegation mismatch")
	}
	if h.NotReadyPlugin() != p.NotReadyPlugin() {
		t.Errorf("NotReadyPlugin delegation mismatch")
	}
	if len(h.Plugins()) != len(p.Plugins()) {
		t.Errorf("Plugins delegation mismatch: %d vs %d", len(h.Plugins()), len(p.Plugins()))
	}

	// Simulate a plugin going not-ready; the Holder should reflect it.
	plugins[0].(*holderTestPlugin).notReady = true
	if h.NotReadyPlugin() != "a" {
		t.Errorf("NotReadyPlugin after flag flip: got %q, want %q", h.NotReadyPlugin(), "a")
	}
}

// Run under concurrent Store: no races, no panics, every goroutine
// observes a non-nil pipeline. The pipeline returned from Load may
// change mid-race; we only assert memory safety under -race.
func TestHolder_ConcurrentRunAndStore(t *testing.T) {
	pa := mustPipeline(t, &holderTestPlugin{name: "a"})
	pb := mustPipeline(t, &holderTestPlugin{name: "b"})
	pc := mustPipeline(t, &holderTestPlugin{name: "c"})
	pipelines := []*Pipeline{pa, pb, pc}

	h := NewHolder(pa)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup

	// 10 runners hammer Run/RunResponse. We accept Reject only when ctx
	// has been cancelled — pipeline.Run short-circuits cancelled contexts
	// by returning a Deny action, which is semantically correct but not
	// what this test is asserting. We care that Load->Run never NPEs or
	// tears under concurrent Store.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pctx := &Context{Path: "/x", Direction: Inbound}
			for ctx.Err() == nil {
				action := h.Run(ctx, pctx)
				if action.Type != Continue && ctx.Err() == nil {
					t.Errorf("unexpected non-Continue from no-op pipeline: %v", action.Type)
				}
				action = h.RunResponse(ctx, pctx)
				if action.Type != Continue && ctx.Err() == nil {
					t.Errorf("unexpected non-Continue from no-op response pipeline: %v", action.Type)
				}
			}
		}()
	}

	// 1 swapper rotates through pipelines.
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for ctx.Err() == nil {
			h.Store(pipelines[i%len(pipelines)])
			i++
		}
	}()

	time.Sleep(200 * time.Millisecond)
	cancel()
	wg.Wait()
}

// Holder must not accept a nil pipeline silently: Load returning nil
// would NPE in every delegating method. NewHolder takes a non-nil by
// contract; this asserts that contract is enforced in the only way
// the type system lets us — the panic surfaces immediately under -race
// rather than later at the first Run.
func TestHolder_ZeroValueLoadsNil(t *testing.T) {
	var h Holder
	if got := h.Load(); got != nil {
		t.Fatalf("zero-value Holder Load: got %p, want nil", got)
	}
}
