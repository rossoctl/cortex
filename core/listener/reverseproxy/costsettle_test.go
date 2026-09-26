package reverseproxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/costevent"
	"github.com/rossoctl/cortex/core/costing"
	"github.com/rossoctl/cortex/core/listener/httpx"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferenceparser"
	"github.com/rossoctl/cortex/core/pricing"
)

// These tests exist because the listener and the inference parser can disagree about what
// shape a response was, and nothing in either package sees the disagreement on its own.
//
// The listener picks its dispatch arm from the RESPONSE Content-Type (see modifyResponse:
// text/event-stream => per-frame + a terminal last=true; anything else => one buffered
// last=true frame). The parser must take its arm from the same evidence — a non-terminal
// frame only ever comes from a per-frame dispatch — and NOT from the REQUEST's stream flag:
// when those two differ the parser runs the wrong arm over the listener's frames. A test that
// calls the parser's hook directly with a hand-built sequence cannot contradict itself, which
// is why these drive a real listener.
//
// So the assertions below go through the real listener: a real backend sets the
// Content-Type, a real request body sets (or omits) the stream flag, and the cost is read
// off the same pipeline.Context the listener built.

// costProbe reads what the parser settled, in the parser's own response pass.
//
// Ordered FIRST in the plugin list on purpose. Response passes run in reverse
// (pipeline.RunResponseFrame counts down), so first-in-request means last-in-response and
// this probe observes a context the parser has already finished with. Reading
// pctx.Extensions from the test goroutine instead would be a data race with the server
// goroutine that wrote it — the mutex here is the synchronisation edge, which is why the
// snapshot is taken in-pass rather than after the client's ReadAll.
type costProbe struct {
	mu         sync.Mutex
	settled    costing.Settled
	loaded     bool
	prompt     int
	output     int
	total      int
	completion string
	skips      int
	record     bool

	// frames counts every dispatch, terminals only the last=true ones. Both are ASSERTED,
	// not merely collected: exactly-once on this listener was pinned nowhere, so a second
	// terminal dispatch — a double charge — passed every test in this package.
	frames    int
	terminals int
	// terminalBudget is how much of httpx.TeardownTimeout was still left when the terminal
	// dispatch reached the plugins, and terminalDeadline whether that context carried one at
	// all. See TestReverseProxy_LongStreamGetsAFullTeardownBudget.
	terminalBudget   time.Duration
	terminalDeadline bool

	// terminal is signalled once when the terminal frame reaches the plugins. Buffered so
	// the server goroutine never blocks on a test that has stopped listening.
	terminal chan struct{}
}

func (p *costProbe) Name() string { return "cost-probe" }
func (p *costProbe) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (p *costProbe) OnRequest(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *costProbe) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *costProbe) OnResponseFrame(ctx context.Context, pctx *pipeline.Context, _ []byte, last bool) pipeline.Action {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.frames++
	if !last {
		return pipeline.Action{Type: pipeline.Continue}
	}
	p.terminals++
	if dl, ok := ctx.Deadline(); ok {
		p.terminalDeadline, p.terminalBudget = true, time.Until(dl)
	}
	p.settled, p.loaded = costing.Load(pctx)
	if ext := pctx.Extensions.Inference; ext != nil {
		p.prompt, p.output, p.total = ext.PromptTokens, ext.CompletionTokens, ext.TotalTokens
		p.completion = ext.Completion
	}
	p.skips = noBodySkips(pctx)
	_, p.record = pctx.Extensions.Custom[costevent.Key+pipeline.PluginEventSuffix]
	if p.terminal != nil {
		select {
		case p.terminal <- struct{}{}:
		default:
		}
	}
	return pipeline.Action{Type: pipeline.Continue}
}

// published reports whether a cost record reached the context. Read from the canonical
// concern-named key, which is the one a consumer is meant to read.
func (p *costProbe) published() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.record
}

func (p *costProbe) snapshotCost() (costing.Settled, bool, int, int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.settled, p.loaded, p.prompt, p.output, p.total
}

// dispatches returns the frame counts, so a test can pin how many times the parsers were
// finalized rather than only what the last one produced.
func (p *costProbe) dispatches() (frames, terminals int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.frames, p.terminals
}

// teardownBudget returns what was left of the finalization deadline when the terminal frame
// arrived, and whether there was a deadline at all.
func (p *costProbe) teardownBudget() (time.Duration, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.terminalBudget, p.terminalDeadline
}

func (p *costProbe) snapshotBody() (string, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.completion, p.skips
}

// noBodySkips counts the parser's "no_response_body" Skip rows. A response that carried a
// body must not produce one: the row pairs a response with its request in abctl, and a
// false one sends whoever is hunting missing telemetry after a body that was there.
func noBodySkips(pctx *pipeline.Context) int {
	if pctx.Extensions.Invocations == nil {
		return 0
	}
	var n int
	for _, list := range [][]pipeline.Invocation{
		pctx.Extensions.Invocations.Outbound, pctx.Extensions.Invocations.Inbound,
	} {
		for _, iv := range list {
			if iv.Action == pipeline.ActionSkip && iv.Reason == "no_response_body" {
				n++
			}
		}
	}
	return n
}

// flatRates prices every tier at $1/Mtok so a modelled figure is arithmetically obvious:
// one micro-dollar per token, whatever the tier.
func flatRates(t *testing.T) pricing.Resolver {
	t.Helper()
	var r pricing.Rates
	for _, tier := range []pricing.Tier{
		pricing.TierInput, pricing.TierCacheWrite, pricing.TierCacheRead, pricing.TierOutput,
	} {
		r.Base[tier], r.Set[tier] = 1e-6, true
	}
	tab, err := pricing.NewTable([]pricing.Entry{
		{Host: "*", Model: "*", Rates: r, Prov: pricing.ProvConfigured},
	})
	if err != nil {
		t.Fatal(err)
	}
	return pricing.NewRegistry(tab)
}

// bodyMutator declares WritesResponseBody without touching anything. Its only job is to
// make the listener take its buffered fallback for a text/event-stream response — a body a
// plugin may rewrite cannot also be forwarded as it arrives. It sorts LAST because the
// pipeline requires body readers to precede a mutator, which also puts it first in the
// reverse-order response pass, where it does nothing at all.
type bodyMutator struct{}

func (bodyMutator) Name() string { return "body-mutator" }
func (bodyMutator) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{ReadsBody: true, WritesResponseBody: true}
}
func (bodyMutator) OnRequest(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (bodyMutator) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// costPipeline wires the real inference parser behind the probe, with rates injected the
// way plugins.BuildWithDeps injects them in a real process. The probe is FIRST so that the
// reverse-order response pass reaches it LAST — after the parser has settled.
func costPipeline(t *testing.T, probe *costProbe, extra ...pipeline.Plugin) *pipeline.Holder {
	t.Helper()
	parser := inferenceparser.NewInferenceParser()
	parser.SetPricingResolver(flatRates(t))
	pipe, err := pipeline.New(append([]pipeline.Plugin{probe, parser}, extra...))
	if err != nil {
		t.Fatalf("New pipeline: %v", err)
	}
	return pipeline.NewHolder(pipe)
}

// sseBackend replies as LiteLLM does to a streamed chat completion: an SSE Content-Type,
// a cost header of ZERO (the placeholder it stamps because the total is unknown when
// headers are sent), delta chunks, then a usage-bearing chunk and [DONE].
func litellmSSEBackend(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set(costing.ResponseCostHeader, "0")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, chunk := range []string{
			`{"choices":[{"delta":{"content":"Hel"}}]}`,
			`{"choices":[{"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`,
			`[DONE]`,
		} {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
			flusher.Flush()
		}
	}))
}

func postThrough(t *testing.T, proxyURL, path, body string) {
	t.Helper()
	resp, err := http.Post(proxyURL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
}

// TestReverseProxy_StreamedResponseToNonStreamRequest_StillCosts is finding 1.
//
// The request body carries NO stream flag; the backend answers with text/event-stream.
// The listener therefore runs its SSE arm — per-frame dispatch and a terminal empty
// last=true — over a request whose ext.Stream is false. The usage the stream reported
// must still reach the cost record: LiteLLM's header says 0 on a stream, so the usage
// fallback is the ONLY figure available, and discarding the folded state loses the whole
// charge.
func TestReverseProxy_StreamedResponseToNonStreamRequest_StillCosts(t *testing.T) {
	backend := litellmSSEBackend(t)
	defer backend.Close()

	probe := &costProbe{}
	srv, err := NewServer(costPipeline(t, probe), nil, backend.URL, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	// No "stream" key at all: ext.Stream == false while the response streams.
	postThrough(t, proxy.URL, "/v1/chat/completions",
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

	settled, loaded, prompt, output, total := probe.snapshotCost()
	if !loaded {
		t.Fatal("no Settled stored: the parser's response pass never ran")
	}
	if prompt != 5 || output != 3 || total != 8 {
		t.Errorf("usage on the extension = (%d,%d,%d), want (5,3,8): the stream's usage chunk was folded and then discarded, so finalize never ran",
			prompt, output, total)
	}
	if !settled.Priced {
		t.Fatalf("settled = %+v; want Priced from the usage fallback — a stream's 0 header is a placeholder, so nothing else can pay for this response", settled)
	}
	if got, want := settled.CostUSD, 8e-6; got != want {
		t.Errorf("CostUSD = %v, want %v (8 tokens at $1/Mtok)", got, want)
	}
}

// TestReverseProxy_BufferedResponseToStreamRequest_ParsesTheEnvelope is the other
// direction of the same disagreement, and the one a gateway produces routinely: the
// client asked for a stream and got a single application/json envelope back (a 200 from a
// gateway that ignored the flag, or any 4xx/5xx error page). The listener buffers it and
// delivers ONE last=true frame, and folding that envelope as if it were an SSE chunk yields
// no completion and a Skip row claiming there was no body.
func TestReverseProxy_BufferedResponseToStreamRequest_ParsesTheEnvelope(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(costing.ResponseCostHeader, "0.0004")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"buffered reply"},` +
			`"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`))
	}))
	defer backend.Close()

	probe := &costProbe{}
	srv, err := NewServer(costPipeline(t, probe), nil, backend.URL, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	postThrough(t, proxy.URL, "/v1/chat/completions",
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	_, loaded, prompt, output, total := probe.snapshotCost()
	if !loaded {
		t.Fatal("no Settled stored: the parser's response pass never ran")
	}
	if prompt != 7 || output != 2 || total != 9 {
		t.Errorf("usage on the extension = (%d,%d,%d), want (7,2,9): a buffered JSON envelope was folded as an SSE chunk",
			prompt, output, total)
	}
	completion, skips := probe.snapshotBody()
	if completion != "buffered reply" {
		t.Errorf("Completion = %q, want %q: choices[].message is not a delta, so folding the envelope as a chunk drops it",
			completion, "buffered reply")
	}
	if skips != 0 {
		t.Errorf("no_response_body Skip rows = %d, want 0 — the response carried a body", skips)
	}
}

// TestReverseProxy_StreamedUnparsedEndpoint_CoverageBoundary states, through the real
// listener, exactly how far the "settle every path" fix reaches on a STREAMED response to
// an endpoint the parser cannot read — /v1/responses, /v1/complete, an Azure deployment
// path, anything off the dialect list.
//
// The dispatch is path-agnostic: the extension is nil, the terminal frame settles, and
// what happens next is decided by the cost header alone. So coverage is exactly "did the
// gateway report a figure", and streaming does not narrow it:
//
//   - A positive figure on a stream is charged. Streaming is NOT a carve-out.
//   - An implausible figure on a stream is refused and DISCLOSED. The cap reads the nil
//     extension, never the Content-Type, so it covers streamed and buffered alike.
//   - A ZERO on a stream buys nothing, and nothing is published. That is not a gap in this
//     code: 0 is the placeholder LiteLLM stamps because the total is unknown when headers
//     are sent, and with the endpoint unparsed there is no usage to model either. No figure
//     exists ANYWHERE for that response. Publishing a settled zero would count unpriced
//     traffic as free; modelling one would invent it. The honest record is none, and the
//     only thing that would widen coverage here is teaching the parser the dialect.
func TestReverseProxy_StreamedUnparsedEndpoint_CoverageBoundary(t *testing.T) {
	for _, tc := range []struct {
		name         string
		costHeader   string
		wantPriced   bool
		wantUSD      float64
		wantRejected string
	}{{
		name:       "positive figure on a stream is charged",
		costHeader: "0.0025",
		wantPriced: true,
		wantUSD:    0.0025,
	}, {
		name:         "implausible figure on a stream is refused and disclosed",
		costHeader:   "50000",
		wantRejected: costevent.RejectedImplausible,
	}, {
		name:       "placeholder zero on a stream is no figure at all",
		costHeader: "0",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set(costing.ResponseCostHeader, tc.costHeader)
				w.WriteHeader(http.StatusOK)
				flusher := w.(http.Flusher)
				_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
				flusher.Flush()
			}))
			defer backend.Close()

			probe := &costProbe{}
			srv, err := NewServer(costPipeline(t, probe), nil, backend.URL, nil)
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			proxy := httptest.NewServer(srv.Handler())
			defer proxy.Close()

			// /v1/responses is off the parser's dialect list, so Extensions.Inference stays
			// nil and this is the unparsed-endpoint path.
			postThrough(t, proxy.URL, "/v1/responses", `{"model":"gpt-4o","input":"hi","stream":true}`)

			settled, loaded, _, _, _ := probe.snapshotCost()
			if !loaded {
				t.Fatal("no Settled stored: the unparsed-endpoint path did not settle at all")
			}
			if settled.Priced != tc.wantPriced {
				t.Errorf("Priced = %v, want %v (settled = %+v)", settled.Priced, tc.wantPriced, settled)
			}
			if settled.CostUSD != tc.wantUSD {
				t.Errorf("CostUSD = %v, want %v", settled.CostUSD, tc.wantUSD)
			}
			if settled.RejectedReason != tc.wantRejected {
				t.Errorf("RejectedReason = %q, want %q", settled.RejectedReason, tc.wantRejected)
			}
			// A record is published when there is something to say — a figure or a refusal —
			// and only then. The zero row is the coverage boundary: no figure, no record.
			wantRecord := tc.wantPriced || tc.wantRejected != ""
			if got := probe.published(); got != wantRecord {
				t.Errorf("published a cost record = %v, want %v", got, wantRecord)
			}
		})
	}
}

// TestReverseProxy_BufferedSSEBodyStillCosts pins the third dispatch shape, the one that
// makes "read the listener, not the request" more than a swap of one flag for another.
//
// A plugin that rewrites response bodies cannot also forward them as they arrive, so both
// proxy listeners fall back to the buffered path for a text/event-stream response when one
// is in the chain (modifyResponse logs exactly that) and then deliver the WHOLE STREAM,
// wire framing included, as a single last=true frame. Folding that as one chunk parses
// nothing, so it has to be read by the SSE parser — and the frame's own framing is the only
// thing that says so, since the Content-Type is identical to the frame-by-frame case.
func TestReverseProxy_BufferedSSEBodyStillCosts(t *testing.T) {
	backend := litellmSSEBackend(t)
	defer backend.Close()

	probe := &costProbe{}
	srv, err := NewServer(costPipeline(t, probe, bodyMutator{}), nil, backend.URL, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	postThrough(t, proxy.URL, "/v1/chat/completions",
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	settled, loaded, prompt, output, total := probe.snapshotCost()
	if !loaded {
		t.Fatal("no Settled stored: the parser's response pass never ran")
	}
	if prompt != 5 || output != 3 || total != 8 {
		t.Errorf("usage on the extension = (%d,%d,%d), want (5,3,8): a buffered SSE body has to go through the SSE parser, not the chunk fold",
			prompt, output, total)
	}
	if !settled.Priced {
		t.Fatalf("settled = %+v; want Priced from the usage fallback", settled)
	}
	completion, skips := probe.snapshotBody()
	if completion != "Hello" {
		t.Errorf("Completion = %q, want %q", completion, "Hello")
	}
	if skips != 0 {
		t.Errorf("no_response_body Skip rows = %d, want 0 — the response carried a body", skips)
	}
}

// TestReverseProxy_BufferedAnthropicEnvelopeToStreamRequest is the same disagreement on
// the dialect where it costs money. The OpenAI fold above happens to salvage the usage
// block (a chat.completion envelope and a chat.completion.chunk share the "usage" key);
// the Anthropic fold does not, because it dispatches on the event type and a complete
// envelope's "type":"message" matches no stream event. Every token count is then lost, and
// with no cost header on the response there is nothing else to charge from — so this
// publishes NOTHING for a fully-formed, usage-bearing reply, and labels it as having had
// no body at all.
func TestReverseProxy_BufferedAnthropicEnvelopeToStreamRequest(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant",` +
			`"content":[{"type":"text","text":"buffered reply"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":7,"output_tokens":2}}`))
	}))
	defer backend.Close()

	probe := &costProbe{}
	srv, err := NewServer(costPipeline(t, probe), nil, backend.URL, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	postThrough(t, proxy.URL, "/v1/messages",
		`{"model":"claude-opus-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	settled, loaded, prompt, output, _ := probe.snapshotCost()
	if !loaded {
		t.Fatal("no Settled stored: the parser's response pass never ran")
	}
	if prompt != 7 || output != 2 {
		t.Errorf("usage on the extension = (%d,%d), want (7,2)", prompt, output)
	}
	if !settled.Priced {
		t.Fatalf("settled = %+v; want Priced from the usage fallback", settled)
	}
	if got, want := settled.CostUSD, 9e-6; got != want {
		t.Errorf("CostUSD = %v, want %v (9 tokens at $1/Mtok)", got, want)
	}
	completion, skips := probe.snapshotBody()
	if completion != "buffered reply" {
		t.Errorf("Completion = %q, want %q", completion, "buffered reply")
	}
	if skips != 0 {
		t.Errorf("no_response_body Skip rows = %d, want 0 — the response carried a body", skips)
	}
}

// TestReverseProxy_ClientDisconnectMidStreamStillSettles is finding 4.
//
// A client that hangs up mid-stream is a normal event — a cancelled turn, a closed tab, a
// timeout — and the tokens the model already reported are real spend. The terminal
// last=true dispatch is the only thing that turns folded state into a settled cost, and it
// must not run on the REQUEST's context: that context is cancelled by the disconnect, so
// pipeline.RunResponseFrame returns Deny("pipeline.cancelled") before calling any plugin and
// the charge is dropped on the floor. Every listener detaches its finalization context for
// this reason — see httpx.TeardownContext, which also bounds it.
//
// The Anthropic shape is what makes this cost money rather than telemetry: message_start
// carries the whole prompt split, including cache reads, so the expensive half of a long
// agent turn is already on the wire before the client goes away.
func TestReverseProxy_ClientDisconnectMidStreamStillSettles(t *testing.T) {
	released := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":"+
			"{\"input_tokens\":1000,\"cache_read_input_tokens\":30000,\"output_tokens\":1}}}\n\n")
		flusher.Flush()
		// Never send message_stop: the turn is still generating when the client leaves.
		<-released
	}))
	defer backend.Close()

	probe := &costProbe{terminal: make(chan struct{}, 1)}
	srv, err := NewServer(costPipeline(t, probe), nil, backend.URL, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()
	// Registered LAST so it runs FIRST: httptest.Server.Close waits for in-flight handlers,
	// and this test deliberately leaves one parked mid-stream.
	defer close(released)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxy.URL+"/v1/messages",
		strings.NewReader(`{"model":"claude-opus-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	// Wait until the first event is downstream, so the fold has definitely happened, then
	// hang up exactly as a real client does.
	// BOUNDED. A read error fails cleanly, but a backend that holds the connection open
	// without writing would otherwise park here until the package timeout — a test that hangs
	// instead of failing, and takes every other test's output with it.
	firstEvent := make(chan error, 1)
	go func() {
		br := bufio.NewReader(resp.Body)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				firstEvent <- err
				return
			}
			if strings.HasPrefix(line, "data:") {
				firstEvent <- nil
				return
			}
		}
	}()
	select {
	case err := <-firstEvent:
		if err != nil {
			t.Fatalf("reading first event: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no SSE event downstream within 5s; the fold this test depends on never happened")
	}
	cancel()
	_ = resp.Body.Close()

	select {
	case <-probe.terminal:
	case <-time.After(5 * time.Second):
		t.Fatal("no terminal frame reached the plugins within 5s: the disconnect cancelled the context the finalization dispatch runs on, so RunResponseFrame denied it and nothing settled")
	}

	settled, loaded, prompt, _, _ := probe.snapshotCost()
	if !loaded {
		t.Fatal("no Settled stored on the terminal frame")
	}
	if prompt != 31000 {
		t.Errorf("PromptTokens = %d, want 31000: the counts message_start already reported were folded and then lost", prompt)
	}
	if !settled.Priced {
		t.Fatalf("settled = %+v; want the prompt-side spend charged — the client leaving does not refund the tokens the model already read", settled)
	}
	if !settled.Incomplete {
		t.Error("Incomplete = false; a stream that died before its output count must say the figure is a floor")
	}
}

// TestReverseProxy_LongStreamGetsAFullTeardownBudget is the must-fix from review round 6.
//
// The finalization context is DETACHED so a client hangup cannot cancel it, and BOUNDED so a
// wedged plugin cannot pin a goroutine forever. Those two properties fight each other on one
// axis — WHEN the bound starts — and a context built where the earlier fix built it, at
// installStreamingResponseBody, starts its clock when the RESPONSE HEADERS arrive. Every
// consumer of it fires at end-of-stream. A turn that streams for longer than
// httpx.TeardownTimeout therefore reaches its terminal frame holding an already-expired
// context, RunResponseFrame refuses an expired context exactly as it refuses a cancelled one,
// and nothing settles: the same defect the disconnect test above pins, with a different cause
// and a worse blast radius, because it drops the LONG turns — the expensive ones. Agent turns
// past ten seconds are ordinary.
//
// WHAT IS ASSERTED IS THE BUDGET, NOT A REAL OVERRUN. Waiting out a ten-second stream would
// put ten seconds into every run of this package, and TeardownTimeout is a const because
// nothing should be tuning it at runtime — including a test. The discriminating question is
// cheaper than the wait anyway: how much of the budget is LEFT when the terminal frame
// arrives. Built at finalization it is the whole of it; built at header time it is short by
// however long the stream ran, so a stream held well past the slack below fails.
func TestReverseProxy_LongStreamGetsAFullTeardownBudget(t *testing.T) {
	// hold is how long the response stays open after its first event, and slack is what the
	// assertion allows for scheduling. hold must exceed slack by enough that the failure is
	// unambiguous rather than flaky in either direction.
	const hold = 400 * time.Millisecond
	const slack = 150 * time.Millisecond

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":"+
			"{\"input_tokens\":1000,\"cache_read_input_tokens\":30000,\"output_tokens\":1}}}\n\n")
		flusher.Flush()
		// The turn keeps generating. This is the whole point: on the shipped path the gap
		// between the first event and the last is the model's thinking time, which for a long
		// agent turn is minutes, not milliseconds.
		time.Sleep(hold)
		_, _ = fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":500}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
		flusher.Flush()
	}))
	defer backend.Close()

	probe := &costProbe{}
	srv, err := NewServer(costPipeline(t, probe), nil, backend.URL, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	postThrough(t, proxy.URL, "/v1/messages",
		`{"model":"claude-opus-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	budget, hasDeadline := probe.teardownBudget()
	if !hasDeadline {
		t.Fatal("the terminal dispatch ran on a context with no deadline: detached is only half of it — a wedged plugin would pin this goroutine for the life of the process")
	}
	if want := httpx.TeardownTimeout - slack; budget < want {
		t.Errorf("teardown budget left at the terminal frame = %v, want >= %v (of %v): the deadline started when the response headers arrived, not at finalization, so a stream longer than %v settles nothing",
			budget, want, httpx.TeardownTimeout, httpx.TeardownTimeout)
	}

	// AND STILL EXACTLY ONCE. The lazy context is per-finalization, so the guard that used to
	// come from "one context, one cancel" is gone; what stops a second charge is b.finished.
	if frames, terminals := probe.dispatches(); terminals != 1 {
		t.Errorf("terminal dispatches = %d (of %d frames), want exactly 1: a second one settles the same turn twice", terminals, frames)
	}

	settled, loaded, prompt, output, _ := probe.snapshotCost()
	if !loaded {
		t.Fatal("no Settled stored: the parser's terminal pass never ran")
	}
	if prompt != 31000 || output != 500 {
		t.Errorf("usage = (%d,%d), want (31000,500)", prompt, output)
	}
	if !settled.Priced {
		t.Errorf("settled = %+v; want Priced: a long turn is exactly the one that must be charged", settled)
	}
}

// TestReverseProxy_BufferedResponseAfterAHangupStillSettles is must-fix 2 of review round 7.
//
// THE ONE ARM THAT WAS MISSED. The streaming path's finalize() detaches, the forward proxy's
// buffered fallback detaches, and modifyResponse's buffered arm — the ordinary
// application/json response, which is most inference traffic — still ran both of its dispatches
// on the request context. Both happen AFTER io.ReadAll has the whole body, so a client that
// hung up during the read leaves a done context: RunResponse and the terminal frame each come
// back Deny("pipeline.cancelled"), and this call site only tests action.Type, so a cancellation
// is indistinguishable from a policy reject. modifyResponse returns responseRejectedError, the
// cost never settles, and the SessionResponse append at the bottom of it never runs — for a
// response that arrived complete and whose tokens are real spend.
//
// DRIVEN BY CALLING modifyResponse DIRECTLY, for the same reason the forward proxy's fallback
// test does: the condition is "the request context is already done when the response is
// complete", which a live round trip cannot produce at a deterministic moment. Everything else
// is real — a real pipeline holding the real parser, a real body, a real pctx off the request
// context the way the handler puts it there.
func TestReverseProxy_BufferedResponseAfterAHangupStillSettles(t *testing.T) {
	probe := &costProbe{}
	srv, err := NewServer(costPipeline(t, probe), nil, "http://backend.invalid", nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	const requestBody = `{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`
	pctx := &pipeline.Context{Direction: pipeline.Inbound, Host: "gw.internal",
		Method: http.MethodPost, Path: "/v1/messages", Body: []byte(requestBody)}
	// The REQUEST phase first, as the handler runs it: the parser populates
	// Extensions.Inference there, from the path and the request body, and its response pass
	// fills in that extension rather than creating one. Skipping it left the fixture settling
	// zero for a reason that had nothing to do with the context under test.
	if action := srv.InboundPipeline.Run(context.Background(), pctx); action.Type == pipeline.Reject {
		t.Fatalf("request phase rejected: %+v", action)
	}
	if pctx.Extensions.Inference == nil {
		t.Fatal("no Inference extension after the request phase; this fixture would then prove nothing about the response path")
	}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), pctxKey{}, pctx))
	req := httptest.NewRequest(http.MethodPost, "http://gw.internal/v1/messages",
		strings.NewReader(requestBody)).WithContext(ctx)
	// The client leaves while the body is being read. Everything below is already on the wire.
	cancel()

	const body = `{"model":"claude-opus-5","usage":{"input_tokens":1000,"output_tokens":500},` +
		`"content":[{"type":"text","text":"buffered reply"}]}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}

	if err := srv.modifyResponse(resp); err != nil {
		t.Fatalf("modifyResponse: %v — a cancelled request context produced a Deny this arm cannot tell apart from a policy reject, so a complete response was rendered as a rejection", err)
	}

	settled, loaded, prompt, output, _ := probe.snapshotCost()
	if !loaded {
		t.Fatal("no Settled stored: the parser's response pass never ran, so this turn's spend reaches no aggregate, ledger or budget")
	}
	if prompt != 1000 || output != 500 {
		t.Errorf("usage = (%d,%d), want (1000,500)", prompt, output)
	}
	if !settled.Priced {
		t.Errorf("settled = %+v; want Priced", settled)
	}
	if _, terminals := probe.dispatches(); terminals != 1 {
		t.Errorf("terminal dispatches = %d, want exactly 1", terminals)
	}
}

// TestReverseProxy_TruncatedStreamToANonStreamRequest_IsAFloor is round 8's instrument finding,
// driven through a real listener because that is where the two facts come apart.
//
// The request does NOT ask for a stream; the gateway answers with SSE anyway and the stream dies
// before its stop reason — an OpenAI-shaped body, where each usage-bearing chunk RESTATES the
// running totals, so the last tally seen is not final. Keyed on the REQUEST's stream flag, this
// response was classified exact: a floor published as a whole figure, which is the failure the
// incompleteness machinery exists to prevent, reached by a route it could not see.
//
// The shape that decides finality is the RESPONSE's, and the parser learns it the same way it
// picks its dispatch arm — by being handed frames.
func TestReverseProxy_TruncatedStreamToANonStreamRequest_IsAFloor(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		// A running total and no finish_reason: the generation was still going.
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}],"+
			"\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":12,\"total_tokens\":1012}}\n\n")
		flusher.Flush()
	}))
	defer backend.Close()

	probe := &costProbe{}
	srv, err := NewServer(costPipeline(t, probe), nil, backend.URL, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	// NO stream flag on the request, which is the whole point.
	postThrough(t, proxy.URL, "/v1/chat/completions",
		`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`)

	settled, loaded, _, output, _ := probe.snapshotCost()
	if !loaded {
		t.Fatal("no Settled stored: the parser's terminal pass never ran")
	}
	if !settled.Priced {
		t.Fatalf("settled = %+v; want Priced — a floor is still the best figure available", settled)
	}
	if !settled.Incomplete || settled.IncompleteReason != pricing.ReasonOutputUncounted {
		t.Errorf("Incomplete = %v / reason = %q, want true / %q: the tally of %d output tokens was a RUNNING total, and the stream died before its stop reason",
			settled.Incomplete, settled.IncompleteReason, pricing.ReasonOutputUncounted, output)
	}
	// AND THE RECORD CARRIES IT, since the caveat only matters where the money is read.
	if rec := costing.NewRecord(settled, nil); !rec.Incomplete || rec.Trust() != costevent.TrustFloor {
		t.Errorf("record Incomplete = %v / Trust = %q, want true / %q", rec.Incomplete, rec.Trust(), costevent.TrustFloor)
	}
}

// truncatedSSEBackend is a stream that dies mid-generation: a usage-bearing chunk restating the
// running totals, and then nothing — no finish_reason and no [DONE], which is what a dropped
// upstream connection or a gateway timeout leaves behind.
func truncatedSSEBackend(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set(costing.ResponseCostHeader, "0")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, chunk := range []string{
			`{"choices":[{"delta":{"content":"Hel"}}]}`,
			`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`,
		} {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
			flusher.Flush()
		}
	}))
}

// TestReverseProxy_BufferedSSEFallbackStillFlagsATruncatedStream proves the buffered arm is
// REACHED, not just that the parser handles it.
//
// bodyMutator declares WritesResponseBody, which makes this listener buffer an event-stream
// response rather than dispatching it frame by frame — a body a plugin may rewrite cannot also be
// forwarded as it arrives. Everything about the response is then identical to the streamed case
// except the shape it was handed over in, and that shape was the whole defect: the flag
// pricing.IncompleteReason reads was set only on the per-frame path, so this arm published a
// running total as an exact figure. A rewriting chain is an ordinary deployment, so this is the
// path a cost consumer sees, not a corner.
func TestReverseProxy_BufferedSSEFallbackStillFlagsATruncatedStream(t *testing.T) {
	backend := truncatedSSEBackend(t)
	defer backend.Close()

	probe := &costProbe{}
	srv, err := NewServer(costPipeline(t, probe, bodyMutator{}), nil, backend.URL, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	postThrough(t, proxy.URL, "/v1/chat/completions",
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	settled, loaded, prompt, output, total := probe.snapshotCost()
	if !loaded {
		t.Fatal("no Settled stored: the parser's response pass never ran")
	}
	// Precondition: the counters that make this the case under test — a tally with no stop reason.
	if prompt != 5 || output != 3 || total != 8 {
		t.Fatalf("usage = (%d,%d,%d), want (5,3,8): the buffered body did not reach the SSE parser",
			prompt, output, total)
	}
	if !settled.Priced {
		t.Fatalf("settled = %+v; want Priced: a floor is still money owed", settled)
	}
	if !settled.Incomplete || settled.IncompleteReason != pricing.ReasonOutputUncounted {
		t.Errorf("Incomplete = %v (%q), want true (%q): an OpenAI stream restates a RUNNING total on every usage chunk, so a tally with no stop reason is a floor — and delivering it whole must not make it read as exact",
			settled.Incomplete, settled.IncompleteReason, pricing.ReasonOutputUncounted)
	}
}
