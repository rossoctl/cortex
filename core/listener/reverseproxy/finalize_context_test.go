package reverseproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/listener/httpx"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// frameProbe records what the terminal dispatch was handed: how many times it happened, and what
// state the context was in when it did.
//
// Deliberately knows nothing about cost. What these tests are about is the CONTEXT a finalization
// dispatch runs on, which every aggregating plugin depends on and none can check from the inside.
type frameProbe struct {
	mu        sync.Mutex
	frames    int
	terminals int
	ctxErr    error
	budget    time.Duration
	hadDeadli bool
}

func (p *frameProbe) Name() string { return "frame-probe" }
func (p *frameProbe) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (p *frameProbe) OnRequest(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *frameProbe) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *frameProbe) OnResponseFrame(ctx context.Context, _ *pipeline.Context, _ []byte, last bool) pipeline.Action {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.frames++
	if !last {
		return pipeline.Action{Type: pipeline.Continue}
	}
	p.terminals++
	p.ctxErr = ctx.Err()
	if dl, ok := ctx.Deadline(); ok {
		p.hadDeadli, p.budget = true, time.Until(dl)
	}
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *frameProbe) snapshot() (terminals int, err error, budget time.Duration, hadDeadline bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.terminals, p.ctxErr, p.budget, p.hadDeadli
}

// plainProbe implements no OnResponseFrame, so it is NOT a StreamingResponder and RunResponse is
// the only hook that reaches it.
//
// In the pipeline beside frameProbe deliberately: RunResponse SKIPS streaming responders, so a
// pipeline holding only frameProbe never dispatches that phase at all — and a test built that way
// cannot notice which context the response phase runs on. Every shipped pipeline has both kinds.
type plainProbe struct {
	mu     sync.Mutex
	calls  int
	ctxErr error
	budget time.Duration
	// spend is how long OnResponse takes, so a test can consume part of a finalization budget and
	// see whether the NEXT dispatch still has its own.
	spend time.Duration
}

func (p *plainProbe) Name() string { return "plain-probe" }
func (p *plainProbe) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (p *plainProbe) OnRequest(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *plainProbe) OnResponse(ctx context.Context, _ *pipeline.Context) pipeline.Action {
	if p.spend > 0 {
		time.Sleep(p.spend)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.ctxErr = ctx.Err()
	if dl, ok := ctx.Deadline(); ok {
		p.budget = time.Until(dl)
	}
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *plainProbe) seen() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.ctxErr
}

func (p *plainProbe) phaseBudget() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.budget
}

func probePipeline(t *testing.T, probe *frameProbe, extra ...pipeline.Plugin) *pipeline.Holder {
	t.Helper()
	p, err := pipeline.New(append([]pipeline.Plugin{probe}, extra...))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	return pipeline.NewHolder(p)
}

// TestFinalize_LongStreamGetsAFullTeardownBudget covers the axis on which "detached" and "bounded"
// fight each other: WHEN the bound starts.
//
// The terminal dispatch is the only one that lets an aggregating plugin finalize, and it runs at
// end-of-stream. Build its context when the RESPONSE HEADERS arrive and the deadline is already
// running, so a turn that streams for longer than httpx.TeardownTimeout reaches that dispatch on an
// expired context — and RunResponseFrame refuses an expired context exactly as it refuses a
// cancelled one, so the finalization silently does not happen. Agent turns past ten seconds are
// ordinary.
//
// The budget LEFT at the dispatch is what discriminates, and it is cheaper than waiting out a real
// overrun: built at finalization it is the whole of it, built at header time it is short by however
// long the stream ran.
func TestFinalize_LongStreamGetsAFullTeardownBudget(t *testing.T) {
	const hold = 400 * time.Millisecond
	const slack = 150 * time.Millisecond

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "data: {\"one\":1}\n\n")
		flusher.Flush()
		// The turn keeps generating: on the shipped path this gap is the model thinking.
		time.Sleep(hold)
		_, _ = fmt.Fprint(w, "data: {\"two\":2}\n\n")
		flusher.Flush()
	}))
	defer backend.Close()

	probe := &frameProbe{}
	srv, err := NewServer(probePipeline(t, probe), nil, backend.URL, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	terminals, ctxErr, budget, hadDeadline := probe.snapshot()
	if terminals != 1 {
		t.Fatalf("terminal dispatches = %d, want 1", terminals)
	}
	if ctxErr != nil {
		t.Errorf("the terminal dispatch ran on a context already done (%v): every aggregating plugin is skipped when that happens", ctxErr)
	}
	if !hadDeadline {
		t.Fatal("no deadline on the finalization context: detached is only half of it — a wedged plugin would pin this goroutine for the life of the process")
	}
	if want := httpx.TeardownTimeout - slack; budget < want {
		t.Errorf("budget left at the terminal frame = %v, want >= %v (of %v): the deadline started when the response headers arrived, not at finalization, so any stream longer than %v finalizes nothing",
			budget, want, httpx.TeardownTimeout, httpx.TeardownTimeout)
	}
}

// TestModifyResponse_BufferedArmSurvivesAHangup covers the ordinary application/json response, which
// is most traffic and was the third place this defect lived.
//
// Both dispatches on that arm run AFTER io.ReadAll has the whole body, so a client that hung up
// during the read leaves a done context: RunResponse and the terminal frame each come back
// Deny("pipeline.cancelled"), and the call site only tests action.Type — so a cancellation is
// indistinguishable from a policy reject. modifyResponse returns responseRejectedError, no plugin
// finalizes, and the SessionResponse row at the bottom of it never happens, for a response that
// arrived complete.
//
// Driven by calling modifyResponse directly, because "the request context is already done when the
// response is complete" is a property of the caller that a live round trip cannot produce at a
// deterministic moment.
func TestModifyResponse_BufferedArmSurvivesAHangup(t *testing.T) {
	probe := &frameProbe{}
	plain := &plainProbe{}
	store := session.New(5*time.Minute, 100, 0)
	t.Cleanup(store.Close)
	srv, err := NewServer(probePipeline(t, probe, plain), store, "http://backend.invalid", nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	pctx := &pipeline.Context{Direction: pipeline.Inbound, Host: "gw.internal",
		Method: http.MethodPost, Path: "/v1/messages"}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), pctxKey{}, pctx))
	req := httptest.NewRequest(http.MethodPost, "http://gw.internal/v1/messages",
		strings.NewReader(`{"hello":"world"}`)).WithContext(ctx)
	// The client leaves while the body is being read; everything below is already on the wire.
	cancel()

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		Request:    req,
	}

	if err := srv.modifyResponse(resp); err != nil {
		t.Fatalf("modifyResponse: %v — a cancelled request context produced a Deny this arm cannot tell from a policy reject, so a complete response was rendered as a rejection", err)
	}
	if terminals, ctxErr, _, _ := probe.snapshot(); terminals != 1 || ctxErr != nil {
		t.Errorf("terminal dispatches = %d, ctx.Err = %v, want 1 and nil: nothing finalized for a response that arrived complete", terminals, ctxErr)
	}
	// AND THE RESPONSE PHASE, which is a separate dispatch on a separate context. It reaches only
	// non-streaming plugins, so a pipeline without one cannot see this at all.
	if calls, ctxErr := plain.seen(); calls != 1 || ctxErr != nil {
		t.Errorf("response phases = %d, ctx.Err = %v, want 1 and nil: the phase was skipped or ran on a done context", calls, ctxErr)
	}
	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 1 || v.Events[0].Phase != pipeline.SessionResponse {
		t.Errorf("session events = %+v, want one response row: the early return took the telemetry with it", v)
	}
}

// TestModifyResponse_TheSettleDoesNotInheritASpentBudget pins the one rule this tree follows at every
// finalization site: a bounded context PER DISPATCH.
//
// The alternative reads simpler — one context for the whole finalization, "two halves of one thing" —
// and gets the priority backwards. The dispatches run in order, so whatever the response phase spends
// comes out of the TERMINAL frame's budget, and that is the dispatch that turns folded state into a
// settled figure. A slow plugin in the phase would lose the charge: the same failure this file exists
// to prevent, wearing a different hat.
//
// Both rules were in the tree at once before this — shared at two sites, split at a third, each
// written as a general rule — and nothing failed when the split was collapsed. This is that missing
// test. The response phase spends a measurable slice of its own budget; the terminal dispatch must
// still see a full one.
func TestModifyResponse_TheSettleDoesNotInheritASpentBudget(t *testing.T) {
	const spend = 300 * time.Millisecond
	const slack = 150 * time.Millisecond

	probe := &frameProbe{}
	plain := &plainProbe{spend: spend}
	store := session.New(5*time.Minute, 100, 0)
	t.Cleanup(store.Close)
	srv, err := NewServer(probePipeline(t, probe, plain), store, "http://backend.invalid", nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	pctx := &pipeline.Context{Direction: pipeline.Inbound, Host: "gw.internal",
		Method: http.MethodPost, Path: "/v1/messages"}
	ctx := context.WithValue(context.Background(), pctxKey{}, pctx)
	req := httptest.NewRequest(http.MethodPost, "http://gw.internal/v1/messages",
		strings.NewReader(`{"hello":"world"}`)).WithContext(ctx)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		Request:    req,
	}

	if err := srv.modifyResponse(resp); err != nil {
		t.Fatalf("modifyResponse: %v", err)
	}

	// The phase spent its own budget, which is what makes the next assertion mean something.
	if got := plain.phaseBudget(); got > httpx.TeardownTimeout-spend/2 {
		t.Fatalf("the response phase saw %v of budget, so it did not spend a measurable slice: this test would prove nothing", got)
	}
	_, ctxErr, budget, hadDeadline := probe.snapshot()
	if ctxErr != nil {
		t.Errorf("the terminal dispatch ran on a done context: %v", ctxErr)
	}
	if !hadDeadline {
		t.Fatal("no deadline on the terminal dispatch")
	}
	if want := httpx.TeardownTimeout - slack; budget < want {
		t.Errorf("the terminal dispatch had %v of budget left, want >= %v: it inherited what the response phase spent, so a slow plugin in the phase would take the settle's time",
			budget, want)
	}
}
