package extproc

import (
	"context"
	"strings"
	"sync"
	"testing"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// THE RESPONSE LIFECYCLE, OBSERVED WITH A SPY RATHER THAN WITH MONEY.
//
// Every claim in this file is about WHEN this listener dispatches and on what context: once per
// response, over a whole body, on a context the stream's end cannot cancel. Those are properties of
// the listener, and a spy plugin can see all of them — which keeps the tests here independent of
// whatever a cost owner would do with the same frames.

// lifecycleSpy is TWO plugins behind one accessor, and it has to be.
//
// RunResponse SKIPS streaming responders, so a plugin that implements OnResponseFrame never sees the
// response PHASE at all — a single spy would report zero phases and hide the very dispatch this file
// counts. Every shipped pipeline has both kinds, so the fixture does too: frameSpy takes the frames,
// phaseSpy takes the phase. phaseSpy also records an Invocation, because the listener's recorder
// appends a row only when something plugin-shaped happened, and a spy that observes nothing produces
// no telemetry to assert on.
type lifecycleSpy struct {
	frames *frameSpy
	phases *phaseSpy
}

type frameSpy struct {
	mu        sync.Mutex
	seen      []string
	terminals int
	lastCtx   error
	// lastFrame is the payload the TERMINAL dispatch carried. On the non-SSE arm that is the whole
	// body, and it is the only place the accumulation is observable — a spy that counts frames sees
	// nothing when a body arrives in fragments, because each fragment is delivered on the same one
	// terminal call.
	lastFrame string
}

func (s *frameSpy) Name() string { return "frame-spy" }
func (s *frameSpy) Capabilities() pipeline.PluginCapabilities {
	// ReadsBody is what makes this listener ask Envoy for a body at all — the dispatch shape under
	// test only exists for a pipeline that needs one.
	return pipeline.PluginCapabilities{ReadsBody: true}
}
func (s *frameSpy) OnRequest(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (s *frameSpy) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (s *frameSpy) OnResponseFrame(ctx context.Context, _ *pipeline.Context, frame []byte, last bool) pipeline.Action {
	s.mu.Lock()
	defer s.mu.Unlock()
	if last {
		s.terminals++
		s.lastCtx = ctx.Err()
		s.lastFrame = string(frame)
		return pipeline.Action{Type: pipeline.Continue}
	}
	s.seen = append(s.seen, string(frame))
	return pipeline.Action{Type: pipeline.Continue}
}

type phaseSpy struct {
	mu    sync.Mutex
	calls int
	// body is what pctx.ResponseBody held when the phase ran. Counting calls cannot see it, so the
	// two body arms could disagree about what "the response body" means without failing anything —
	// and they do disagree, deliberately: whole document on the non-SSE arm, trailing chunk on SSE.
	// This is the field that makes each of those a claim rather than an accident.
	body string
}

func (s *phaseSpy) Name() string { return "phase-spy" }
func (s *phaseSpy) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{ReadsBody: true}
}
func (s *phaseSpy) OnRequest(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (s *phaseSpy) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	s.mu.Lock()
	s.calls++
	s.body = string(pctx.ResponseBody)
	s.mu.Unlock()
	pctx.Observe("response phase")
	return pipeline.Action{Type: pipeline.Continue}
}

func (s *lifecycleSpy) snapshot() (phases, terminals int, frames []string, lastCtx error) {
	s.frames.mu.Lock()
	defer s.frames.mu.Unlock()
	s.phases.mu.Lock()
	defer s.phases.mu.Unlock()
	return s.phases.calls, s.frames.terminals, append([]string(nil), s.frames.seen...), s.frames.lastCtx
}

// terminalFrame returns what the terminal dispatch carried.
func (s *lifecycleSpy) terminalFrame() string {
	s.frames.mu.Lock()
	defer s.frames.mu.Unlock()
	return s.frames.lastFrame
}

// phaseBody returns what pctx.ResponseBody held when the response phase ran.
func (s *lifecycleSpy) phaseBody() string {
	s.phases.mu.Lock()
	defer s.phases.mu.Unlock()
	return s.phases.body
}

func lifecycleServer(t *testing.T) (*Server, *lifecycleSpy, *session.Store) {
	t.Helper()
	spy := &lifecycleSpy{frames: &frameSpy{}, phases: &phaseSpy{}}
	p, err := pipeline.New([]pipeline.Plugin{spy.frames, spy.phases})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	empty, err := pipeline.New(nil)
	if err != nil {
		t.Fatalf("pipeline.New(nil): %v", err)
	}
	store := session.New(0, 100, 100)
	t.Cleanup(func() { store.Close() })
	return &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		InboundPipeline:  pipeline.NewHolder(empty),
		Sessions:         store,
	}, spy, store
}

// sseTurn is one streamed turn: three complete events.
const sseTurn = "data: {\"seq\":1}\n\n" + "data: {\"seq\":2,\"tail\":\"x\"}\n\n" + "data: {\"seq\":3}\n\n"

// responseScript builds a request/response exchange. bodies are delivered as separate
// ResponseBody messages, and endOfStream is set on the last one unless torn is true.
func responseScript(contentType string, bodies []string, torn bool) []*extprocv3.ProcessingRequest {
	reqs := []*extprocv3.ProcessingRequest{
		{Request: &extprocv3.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extprocv3.HttpHeaders{
				Headers: makeHeaders(
					"x-authbridge-direction", "outbound",
					":method", "POST",
					":path", "/v1/messages",
					":authority", "gw.local",
					"content-length", "2",
				),
			},
		}},
		{Request: &extprocv3.ProcessingRequest_RequestBody{
			RequestBody: &extprocv3.HttpBody{Body: []byte("{}"), EndOfStream: true},
		}},
		{Request: &extprocv3.ProcessingRequest_ResponseHeaders{
			ResponseHeaders: &extprocv3.HttpHeaders{
				Headers: makeHeaders(":status", "200", "content-type", contentType),
				// Envoy sets this when the response ends on its headers, which it does not here.
				EndOfStream: len(bodies) == 0,
			},
		}},
	}
	for i, b := range bodies {
		last := i == len(bodies)-1 && !torn
		reqs = append(reqs, &extprocv3.ProcessingRequest{
			Request: &extprocv3.ProcessingRequest_ResponseBody{
				ResponseBody: &extprocv3.HttpBody{Body: []byte(b), EndOfStream: last},
			},
		})
	}
	return reqs
}

// TestExtProc_ResponsePhaseRunsOncePerResponse covers a per-RESPONSE dispatch that was written as a
// per-MESSAGE one.
//
// A statically configured STREAMED body mode delivers N messages, and the response phase ran on each
// of them — N decisions over N prefixes of a document, with every Invocation row landing in the
// recorded snapshot. It is the same "wait until the body is whole" rule the frame dispatch and the
// session record already follow.
func TestExtProc_ResponsePhaseRunsOncePerResponse(t *testing.T) {
	srv, spy, _ := lifecycleServer(t)
	reqs := responseScript("application/json", []string{`{"half":`, `1}`}, false)

	stream := &mockStream{ctx: context.Background(), requests: reqs}
	_ = srv.Process(stream)
	if stream.recvIdx != len(reqs) {
		t.Fatalf("consumed %d of %d messages; the listener bailed", stream.recvIdx, len(reqs))
	}

	phases, terminals, _, _ := spy.snapshot()
	if phases != 1 {
		t.Errorf("response phases = %d over a two-message body, want 1: each extra one decided over a partial document", phases)
	}
	if terminals != 1 {
		t.Errorf("terminal dispatches = %d, want 1", terminals)
	}
}

// TestExtProc_NonSSEBodyIsDispatchedWhole is the other half of that rule: a JSON body split across
// messages must reach a plugin as ONE document.
//
// Replaced per message, each fragment arrived as if it were the whole response — no fragment is
// valid JSON, so anything parsing it got nothing, and a plugin latching its first answer could not
// be corrected by a later message.
func TestExtProc_NonSSEBodyIsDispatchedWhole(t *testing.T) {
	srv, spy, _ := lifecycleServer(t)
	reqs := responseScript("application/json", []string{`{"a":1,`, `"b":2}`}, false)

	stream := &mockStream{ctx: context.Background(), requests: reqs}
	_ = srv.Process(stream)

	_, terminals, frames, _ := spy.snapshot()
	if terminals != 1 {
		t.Fatalf("terminal dispatches = %d, want 1", terminals)
	}
	if len(frames) != 0 {
		t.Errorf("non-terminal frames = %v, want none: a JSON body is dispatched once, whole", frames)
	}
	// AND THE TERMINAL FRAME CARRIES THE WHOLE DOCUMENT, which is the assertion this test was
	// missing: counting frames cannot see the accumulation, because every fragment arrives on the
	// same single terminal call — replacing rather than accumulating simply delivers the LAST
	// fragment there. Measured before the fix: a plugin parsing that gets nothing, so a JSON body
	// split across two messages produced TotalTokens 0 and no cost record at all.
	if got, want := spy.terminalFrame(), `{"a":1,"b":2}`; got != want {
		t.Errorf("terminal frame = %q, want %q: the body arrived in two messages and the parser was handed a fragment", got, want)
	}
}

// TestExtProc_WhatTheResponsePhaseSees pins the one thing the two body arms disagree about, in both
// directions.
//
// RunResponse skips only StreamingResponder plugins, so a shipped pipeline's opa, cpex, lineage and
// sparc DO run in the response phase and read pctx.ResponseBody there. On the non-SSE arm that is
// now the whole document — the point of accumulating. On SSE it is the carry plus the final message:
// the tail of the stream, not the turn, because a whole streamed turn is exactly the body that runs
// into the truncation cap, and its events reach a plugin as frames instead. Both shapes are
// deliberate and neither was observable while phaseSpy only counted its calls, so a change in
// either direction passed silently.
func TestExtProc_WhatTheResponsePhaseSees(t *testing.T) {
	cut := strings.Index(sseTurn, "\"tail\"") + 3 // mid-field, inside the second event
	for _, tc := range []struct {
		name        string
		contentType string
		bodies      []string
		want        string
	}{
		{
			name:        "a JSON body reaches the phase whole",
			contentType: "application/json",
			bodies:      []string{`{"a":1,`, `"b":2}`},
			want:        `{"a":1,"b":2}`,
		},
		{
			// The turn's first event completes inside the first message, so it is dispatched as a
			// frame and gone from the buffer; what reaches the phase is the event that straddled the
			// boundary plus the one after it.
			name:        "an SSE body reaches the phase as its trailing chunk",
			contentType: "text/event-stream",
			bodies:      []string{sseTurn[:cut], sseTurn[cut:]},
			want:        "data: {\"seq\":2,\"tail\":\"x\"}\n\ndata: {\"seq\":3}\n\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, spy, _ := lifecycleServer(t)
			reqs := responseScript(tc.contentType, tc.bodies, false)

			stream := &mockStream{ctx: context.Background(), requests: reqs}
			_ = srv.Process(stream)

			phases, _, _, _ := spy.snapshot()
			if phases != 1 {
				t.Fatalf("response phases = %d, want 1: this row asserts what the ONE phase was handed", phases)
			}
			if got := spy.phaseBody(); got != tc.want {
				t.Errorf("the phase saw %q, want %q", got, tc.want)
			}
		})
	}
}

// TestExtProc_SSEEventSplitAcrossMessagesIsReassembled is the undercount that hid behind an
// arithmetic-looking symptom.
//
// Envoy's ResponseBody messages are chunks of a byte stream, so nothing aligns them with SSE
// framing. Re-parsing each message alone splits any event that straddles a boundary into two halves
// that each look like a valid-but-uninteresting frame: sseframe delivers the first message's
// unterminated tail as a frame, and the remainder in the next message reads as a field nothing
// consumes. Whatever that event carried is then simply gone — on the Anthropic dialect it is the
// output tally, and the response still settles at a floor, so nothing looks wrong.
func TestExtProc_SSEEventSplitAcrossMessagesIsReassembled(t *testing.T) {
	cut := strings.Index(sseTurn, "\"tail\"") + 3 // mid-field, inside the second event
	srv, spy, _ := lifecycleServer(t)
	reqs := responseScript("text/event-stream", []string{sseTurn[:cut], sseTurn[cut:]}, false)

	stream := &mockStream{ctx: context.Background(), requests: reqs}
	_ = srv.Process(stream)

	_, terminals, frames, _ := spy.snapshot()
	if terminals != 1 {
		t.Errorf("terminal dispatches = %d, want 1", terminals)
	}
	want := []string{`{"seq":1}`, `{"seq":2,"tail":"x"}`, `{"seq":3}`}
	if len(frames) != len(want) {
		t.Fatalf("frames = %q, want %q: an event cut across two messages was delivered as halves", frames, want)
	}
	for i := range want {
		if frames[i] != want[i] {
			t.Errorf("frame[%d] = %q, want %q", i, frames[i], want[i])
		}
	}
}

// cancelOnEndStream cancels the stream's context when the script runs out, which is what Envoy
// tearing an ext_proc stream down looks like from here: a hangup, a filter timeout, a shutdown.
type cancelOnEndStream struct {
	*mockStream
	cancel context.CancelFunc
}

func (m *cancelOnEndStream) Recv() (*extprocv3.ProcessingRequest, error) {
	req, err := m.mockStream.Recv()
	if err != nil {
		m.cancel()
	}
	return req, err
}

// TestExtProc_TornStreamStillFinalizes covers the response Envoy never finishes.
//
// end_of_stream is not guaranteed — Envoy omits it when trailers follow, and this server asks for no
// trailer phase — so a body's last message can legitimately never be marked. Without a flush at
// stream end such a response never finalizes and is never recorded. And the flush has to run on a
// context the stream's death cannot cancel, or it is a no-op in exactly the case it exists for.
func TestExtProc_TornStreamStillFinalizes(t *testing.T) {
	srv, spy, store := lifecycleServer(t)
	reqs := responseScript("text/event-stream", []string{sseTurn}, true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &cancelOnEndStream{mockStream: &mockStream{ctx: ctx, requests: reqs}, cancel: cancel}
	_ = srv.Process(stream)
	if ctx.Err() == nil {
		t.Fatal("the stream context is not cancelled, so this test is not exercising teardown")
	}

	phases, terminals, _, lastCtx := spy.snapshot()
	if terminals != 1 {
		t.Errorf("terminal dispatches = %d, want 1: nothing finalized for a stream Envoy tore down", terminals)
	}
	if lastCtx != nil {
		t.Errorf("the terminal dispatch ran on a done context (%v): RunResponseFrame refuses one before calling any plugin, so the flush would be a no-op", lastCtx)
	}
	if phases != 1 {
		t.Errorf("response phases = %d, want 1", phases)
	}
	if n := responseRows(store); n != 1 {
		t.Errorf("outbound response rows = %d, want exactly 1: a response nobody recorded is telemetry that silently vanished, and two would pair one request with two responses", n)
	}
}

// TestExtProc_HeaderOnlyResponseFinishesOnItsHeaders covers the response with no body at all — a 204,
// a 304, an error status ended on headers.
//
// The header phase deferred to a body phase whenever the pipeline needed a body, which for any
// pipeline containing a body-reading plugin is unconditional. Envoy sends no ResponseBody message for
// a body-less response, whatever mode is asked for, so that response reached NEITHER branch: no
// terminal dispatch, no row. The assertion that discriminates is the ModeOverride — asking Envoy for
// a body it will never send — because the row alone can also come from the teardown flush.
func TestExtProc_HeaderOnlyResponseFinishesOnItsHeaders(t *testing.T) {
	srv, spy, store := lifecycleServer(t)
	reqs := responseScript("application/json", nil, false)

	stream := &mockStream{ctx: context.Background(), requests: reqs}
	_ = srv.Process(stream)

	// FOUND BY TYPE, NOT BY INDEX. Which position the response-header reply sits at depends on how
	// many request-phase messages there were, so an index made this assertion pass vacuously — the
	// control that reverts the guard caught it looking at the request-body reply instead.
	reply := responseHeadersReply(t, stream)
	if mo := reply.GetModeOverride(); mo != nil {
		t.Errorf("the response-header phase asked Envoy for a body (%v) on a response that has none: Envoy sends none, so nothing would finalize — and the row this test finds would come from the teardown flush instead, which is not the path under test",
			mo.GetResponseBodyMode())
	}
	if _, terminals, _, _ := spy.snapshot(); terminals != 1 {
		t.Errorf("terminal dispatches = %d, want 1: a body-less response still has to finalize", terminals)
	}
	if n := responseRows(store); n != 1 {
		t.Errorf("outbound response rows = %d, want exactly 1: a response nobody recorded is telemetry that silently vanished, and two would pair one request with two responses", n)
	}
}

// responseRows counts the outbound response rows in the default bucket. Exactly one is the claim
// everywhere in this file: zero means the response was never recorded, more than one means a
// consumer pairing rows sees the same response twice.
func responseRows(store *session.Store) int {
	v := store.View(session.DefaultSessionID)
	if v == nil {
		return 0
	}
	n := 0
	for i := range v.Events {
		if v.Events[i].Phase == pipeline.SessionResponse && v.Events[i].Direction == pipeline.Outbound {
			n++
		}
	}
	return n
}

// responseHeadersReply returns the listener's answer to the response-header phase.
func responseHeadersReply(t *testing.T, stream *mockStream) *extprocv3.ProcessingResponse {
	t.Helper()
	for _, r := range stream.responses {
		if r.GetResponseHeaders() != nil {
			return r
		}
	}
	t.Fatalf("no response-header reply among %d responses", len(stream.responses))
	return nil
}
