package forwardproxy

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/listener/internal/wsframe"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferenceparser"
	"github.com/rossoctl/cortex/core/plugins/plugintesting"
)

// Tests for the review of #1467: each pins one defect a reviewer reproduced.

// scriptedOrigin replaces the harness origin's handler with one that, after the
// handshake, runs script with a reader for client frames and a writer for its own.
func scriptedOrigin(h *wsHarness, script func(read func() *wsframe.Frame, write func(string))) {
	h.origin.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			return
		}
		conn, brw, _ := w.(http.Hijacker).Hijack()
		defer conn.Close()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + wsAccept(r.Header.Get("Sec-Websocket-Key")) + "\r\n\r\n")
		_ = brw.Flush()
		fr := wsframe.NewReader(brw.Reader, 64<<20, 64<<20)
		read := func() *wsframe.Frame {
			f, err := fr.ReadFrame()
			if err != nil {
				return nil
			}
			return f
		}
		write := func(ev string) { _, _ = conn.Write(wsframe.Encode(wsframe.OpText, []byte(ev), false)) }
		script(read, write)
		for read() != nil {
		}
	})
}

// turnRows is each turn's response row, in order.
func turnRows(events []pipeline.SessionEvent) []pipeline.SessionEvent {
	var out []pipeline.SessionEvent
	for _, e := range events {
		if e.HTTPMethod == http.MethodPost && e.Phase == pipeline.SessionResponse {
			out = append(out, e)
		}
	}
	return out
}

// Turn 1's terminal event arrives after the client has already sent turn 2 — what Codex
// does after a fast tool. Each answer belongs to the turn it answers, in order: turn 1
// keeps its own usage, and turn 2 is not ended by turn 1's terminal event.
func TestResponsesWebSocket_LateTerminalEventEndsItsOwnTurn(t *testing.T) {
	h := newWSHarness(t, config.ResponsesWebSocketsParse)
	scriptedOrigin(h, func(read func() *wsframe.Frame, write func(string)) {
		read() // create 1
		one, two := turnEvents(1), turnEvents(2)
		for _, ev := range one[:len(one)-1] {
			write(ev)
		}
		read() // create 2, before turn 1's terminal event
		write(one[len(one)-1])
		for _, ev := range two {
			write(ev)
		}
	})
	c := h.dial(t, nil)
	c.send(responseCreate("one"))
	c.readUntil("response.output_item.done")
	c.send(responseCreate("two"))
	c.readUntil("response.completed")
	c.readUntil("response.completed")
	c.close()

	rows := turnRows(h.events(t, codexSID, true))
	if len(rows) != 2 {
		t.Fatalf("%d turn rows, want 2", len(rows))
	}
	for i, want := range []int{12867, 13007} {
		r := rows[i]
		if r.Error != nil {
			t.Errorf("turn %d recorded error %+v; it completed", i+1, r.Error)
		}
		if r.Inference == nil || r.Inference.TotalTokens != want {
			t.Errorf("turn %d tokens = %+v, want %d: an answer went to the wrong turn", i+1, r.Inference, want)
		}
	}
}

// The handshake's Finishers run on the pipeline the handshake ran on, whatever a reload
// installs while the socket is open. A reload to a shorter pipeline used to index past
// its end.
func TestResponsesWebSocket_HandshakeFinishesOnItsOwnPipeline(t *testing.T) {
	first := &outcomeRecorder{}
	extra := &outcomeRecorder{}
	h := newWSHarness(t, config.ResponsesWebSocketsParse, first, extra)
	c := h.dial(t, nil)
	if c.status != http.StatusSwitchingProtocols {
		t.Fatalf("handshake answered %d", c.status)
	}
	shorter, err := plugintesting.BuildPipeline([]pipeline.Plugin{inferenceparser.NewInferenceParser()})
	if err != nil {
		t.Fatal(err)
	}
	h.holder.Store(shorter)
	c.close()
	h.events(t, codexSID, true)
	if first.seen() == nil || extra.seen() == nil {
		t.Error("the handshake's own Finishers did not run once its socket closed")
	}
}

// A destination writer (inference-router) cannot steer a turn on a socket already
// connected to OpenAI, so the handshake is declined with a 426 and Codex takes HTTPS,
// where it can.
func TestResponsesWebSocket_DestinationWriterIsDeclined(t *testing.T) {
	h := newWSHarness(t, config.ResponsesWebSocketsParse, &redirector{})
	c := h.dial(t, nil)
	if c.status != http.StatusUpgradeRequired {
		t.Fatalf("handshake answered %d, want 426 with a redirecting plugin in the chain", c.status)
	}
}

// A reload that adds a plugin the socket cannot honour ends the socket at the next
// turn, with "try again later", rather than ignoring the plugin for the socket's life.
func TestResponsesWebSocket_ReloadAddingAnUncarriablePluginEndsTheSocket(t *testing.T) {
	h := newWSHarness(t, config.ResponsesWebSocketsParse)
	c := h.dial(t, nil)
	if c.status != http.StatusSwitchingProtocols {
		t.Fatalf("handshake answered %d", c.status)
	}
	fresh, err := plugintesting.BuildPipeline([]pipeline.Plugin{inferenceparser.NewInferenceParser(), &responseRewriter{}})
	if err != nil {
		t.Fatal(err)
	}
	h.holder.Store(fresh)
	c.send(responseCreate("hello"))
	f, err := c.fr.ReadFrame()
	if err != nil || f.Opcode != wsframe.OpClose || len(f.Payload) < 2 || int(f.Payload[0])<<8|int(f.Payload[1]) != wsframe.CloseTryAgainLater {
		t.Fatalf("next frame = %+v, %v; want a close with code %d", f, err, wsframe.CloseTryAgainLater)
	}
	c.close()
	h.origin.mu.Lock()
	n := len(h.origin.creates)
	h.origin.mu.Unlock()
	if n != 0 {
		t.Errorf("origin received %d response.create; the turn must not be sent without its plugin", n)
	}
}

// A turn over the 32 MiB a request body may be ends the socket with 1009, as HTTPS
// answers 413: it is never forwarded unread, past every plugin.
func TestResponsesWebSocket_OverLimitTurnEndsTheSocket(t *testing.T) {
	h := newWSHarness(t, config.ResponsesWebSocketsParse, &turnDenier{})
	c := h.dial(t, nil)
	_ = c.conn.SetDeadline(time.Now().Add(60 * time.Second))
	half := strings.Repeat("x", (wsClientMessageLimit/2)+1)
	_, _ = c.conn.Write(fragmentFrame(wsframe.OpText, false, []byte(half)))
	_, _ = c.conn.Write(fragmentFrame(wsframe.OpContinuation, true, []byte(half)))
	f, err := c.fr.ReadFrame()
	if err != nil || f.Opcode != wsframe.OpClose || len(f.Payload) < 2 || int(f.Payload[0])<<8|int(f.Payload[1]) != wsframe.CloseMessageTooBig {
		t.Fatalf("answer = %+v, %v; want a close with code %d", f, err, wsframe.CloseMessageTooBig)
	}
	h.events(t, codexSID, true)
	h.origin.mu.Lock()
	n := len(h.origin.creates)
	h.origin.mu.Unlock()
	if n != 0 {
		t.Errorf("origin received %d response.create; an over-limit turn must not be forwarded", n)
	}
}

// A prewarm carries the prompt the next turn builds on, so it runs through the request
// phase: a plugin that refuses the content refuses the prewarm too. It records no turn.
func TestResponsesWebSocket_PrewarmRunsTheRequestPhase(t *testing.T) {
	h := newWSHarness(t, config.ResponsesWebSocketsParse, &turnDenier{})
	c := h.dial(t, nil)
	c.send(`{"type":"response.create","generate":false,"model":"gpt-6-luna","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"secret"}]}]}`)
	msgs := c.readUntil("error")
	if msgs[len(msgs)-1]["status"] != float64(http.StatusForbidden) {
		t.Errorf("prewarm answer = %v, want the plugin's refusal", msgs[len(msgs)-1])
	}
	c.close()
	events := h.events(t, codexSID, true)
	h.origin.mu.Lock()
	n := len(h.origin.creates)
	h.origin.mu.Unlock()
	if n != 0 {
		t.Errorf("origin received %d response.create; the refused prewarm must not be forwarded", n)
	}
	if rows := turnRows(events); len(rows) != 0 {
		t.Errorf("%d turn rows for a prewarm; a prewarm is not a turn", len(rows))
	}
}

// Each turn sees the session as it is when the turn starts, as an HTTPS request does,
// not the view taken at the handshake before any turn was recorded.
func TestResponsesWebSocket_EachTurnSeesTheSessionSoFar(t *testing.T) {
	seen := &sessionLenRecorder{}
	h := newWSHarness(t, config.ResponsesWebSocketsParse, seen)
	c := h.dial(t, nil)
	c.send(responseCreate("one"))
	c.readUntil("response.completed")
	c.send(responseCreate("two"))
	c.readUntil("response.completed")
	c.close()
	h.events(t, codexSID, true)
	got := seen.lens()
	if len(got) < 3 || got[len(got)-1] <= got[len(got)-2] {
		t.Errorf("session lengths seen = %v; the second turn should see more of the session than the first", got)
	}
}

// An origin's refusal is relayed whole: the handshake's deadline must not cut its body.
func TestResponsesWebSocket_OriginRefusalBodyIsRelayedWhole(t *testing.T) {
	h := newWSHarness(t, config.ResponsesWebSocketsParse)
	body := strings.Repeat("r", 200<<10)
	h.origin.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			return
		}
		w.WriteHeader(http.StatusForbidden)
		bw := bufio.NewWriterSize(w, 4096)
		for i := 0; i < len(body); i += 4096 {
			_, _ = bw.WriteString(body[i:min(i+4096, len(body))])
			_ = bw.Flush()
			w.(http.Flusher).Flush()
		}
	})
	c := h.dial(t, nil)
	if c.status != http.StatusForbidden || len(c.body) != len(body) {
		t.Errorf("refusal = %d with %d body bytes, want 403 with %d", c.status, len(c.body), len(body))
	}
}

// fragmentFrame is one masked client frame, FIN as given.
func fragmentFrame(op byte, fin bool, payload []byte) []byte {
	f := wsframe.Encode(op, payload, true)
	if !fin {
		f[0] &^= 0x80
	}
	return f
}

// redirector declares WritesDestination, as inference-router does.
type redirector struct{}

func (redirector) Name() string { return "destination-writer" }
func (redirector) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{WritesDestination: true}
}
func (redirector) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (redirector) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// sessionLenRecorder records how many events the session view held at each request.
type sessionLenRecorder struct {
	mu  sync.Mutex
	got []int
}

func (*sessionLenRecorder) Name() string { return "session-len-recorder" }
func (*sessionLenRecorder) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (r *sessionLenRecorder) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := -1
	if pctx.Session != nil {
		n = pctx.Session.Len()
	}
	r.got = append(r.got, n)
	return pipeline.Action{Type: pipeline.Continue}
}
func (*sessionLenRecorder) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (r *sessionLenRecorder) lens() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.got...)
}
