package forwardproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// frameProbe records what the terminal dispatch saw, and whether it saw one at all.
type frameProbe struct {
	mu        sync.Mutex
	frames    int
	terminals int
	payload   string
}

func (p *frameProbe) Name() string { return "frame-probe" }
func (p *frameProbe) Capabilities() pipeline.PluginCapabilities {
	// No capability flags needed: a StreamingResponder is an INTERFACE assertion — implementing
	// OnResponseFrame is what puts this plugin, and therefore this listener, on the
	// frame-dispatching path (see Pipeline.HasStreamingResponders).
	return pipeline.PluginCapabilities{}
}
func (p *frameProbe) OnRequest(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *frameProbe) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *frameProbe) OnResponseFrame(_ context.Context, _ *pipeline.Context, frame []byte, last bool) pipeline.Action {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.frames++
	if last {
		p.terminals++
		return pipeline.Action{Type: pipeline.Continue}
	}
	p.payload += string(frame)
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *frameProbe) seen() (frames, terminals int, payload string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.frames, p.terminals, p.payload
}

// noFlushWriter hides the recorder's Flush method.
//
// EMBEDDED AS THE INTERFACE, not as the concrete recorder: httptest.ResponseRecorder does
// implement http.Flusher, and embedding it would promote that method and keep the listener on
// its streaming path. Through the interface only the interface's methods exist, so the
// http.Flusher assertion in handleStreamingResponse fails and the buffered fallback runs —
// which is the whole shape under test.
type noFlushWriter struct{ http.ResponseWriter }

// TestForwardProxy_BufferedFallbackFinalizesAfterAHangup is suggestion 3 of review round 6.
//
// streamFallbackBuffered is the sibling of the streaming path whose finalization context this
// PR detached, and it was left running its dispatches on the REQUEST's context. The bug needs
// no exotic timing: the body is already fully read by io.ReadAll before any dispatch happens,
// so a client that hung up while it was being read leaves a cancelled context, and
// RunResponseFrame refuses a cancelled context before calling any plugin — it returns a Deny
// this loop cannot distinguish from a policy reject. The loop then writes a rejection and
// RETURNS, which skips recordOutboundResponseEvent: no settled cost, and no response row
// either, for a response that arrived complete and whose tokens are real spend.
//
// DRIVEN BY CALLING THE FALLBACK DIRECTLY, because the two conditions that reach it — a
// ResponseWriter with no Flush and a request whose context is already cancelled — are both
// properties of the caller, and a full proxy round trip cannot produce the second one at a
// deterministic moment. The claim is about which context the dispatches run on, so the fixture
// only has to supply a cancelled one; what it must not do is fake the pipeline, and it does
// not — a real Holder, a real SSE body, a real session store.
func TestForwardProxy_BufferedFallbackFinalizesAfterAHangup(t *testing.T) {
	probe := &frameProbe{}
	pipe, err := pipeline.New([]pipeline.Plugin{probe})
	if err != nil {
		t.Fatalf("New pipeline: %v", err)
	}
	store := session.New(5*time.Minute, 100, 0)
	s := &Server{OutboundPipeline: pipeline.NewHolder(pipe), Sessions: store}

	// The client is gone before the fallback runs — a cancelled turn, a closed tab, a timeout.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodPost, "http://gw.internal/v1/messages",
		strings.NewReader(`{"model":"claude-opus-5","stream":true}`)).WithContext(ctx)

	const body = "data: {\"type\":\"message_start\"}\n\ndata: {\"type\":\"message_stop\"}\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	pctx := &pipeline.Context{
		Direction:       pipeline.Outbound,
		Host:            "gw.internal",
		Method:          http.MethodPost,
		Path:            "/v1/messages",
		ResponseHeaders: resp.Header,
	}

	rec := httptest.NewRecorder()
	s.streamFallbackBuffered(noFlushWriter{rec}, r, resp, pctx)

	frames, terminals, payload := probe.seen()
	if terminals != 1 {
		t.Errorf("terminal dispatches = %d (of %d frames), want exactly 1: the hangup cancelled the context the fold runs on, so every plugin was skipped and nothing settled",
			terminals, frames)
	}
	if frames != 3 {
		t.Errorf("frames = %d, want 3 (two events plus the terminal): payload %q", frames, payload)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: a cancelled context produced a Deny this path cannot tell apart from a policy reject, so it rendered a rejection for a response that arrived complete",
			rec.Code)
	}
	// AND THE ROW, which the early return took with it. A response event that never lands is
	// the half of this failure an operator would notice: telemetry silently missing for
	// exactly the turns that ended in a hangup.
	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 1 {
		t.Fatalf("session events = %+v, want exactly one response row", v)
	}
	if got := v.Events[0].Phase; got != pipeline.SessionResponse {
		t.Errorf("Phase = %v, want %v", got, pipeline.SessionResponse)
	}
}
