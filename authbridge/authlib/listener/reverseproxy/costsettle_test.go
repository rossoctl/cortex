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

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/costing"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins/inferenceparser"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
)

// These tests exist because the listener and the inference parser used to disagree about
// what shape a response was, and nothing in either package could see the disagreement.
//
// The listener picks its dispatch arm from the RESPONSE Content-Type (see modifyResponse:
// text/event-stream => per-frame + a terminal last=true; anything else => one buffered
// last=true frame). The parser used to pick its arm from the REQUEST's stream flag. When
// those two differ the parser runs the wrong arm over the listener's frames, and every
// earlier test missed it by calling the parser's hook directly with a hand-built sequence
// that could not contradict itself.
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
	frames     int
	lastSeen   bool

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
func (p *costProbe) OnResponseFrame(_ context.Context, pctx *pipeline.Context, _ []byte, last bool) pipeline.Action {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.frames++
	if !last {
		return pipeline.Action{Type: pipeline.Continue}
	}
	p.lastSeen = true
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
			fmt.Fprintf(w, "data: %s\n\n", chunk)
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
	defer resp.Body.Close()
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
// delivers ONE last=true frame; the parser used to fold that envelope as if it were an
// SSE chunk, which yields no completion and a Skip row claiming there was no body.
func TestReverseProxy_BufferedResponseToStreamRequest_ParsesTheEnvelope(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(costing.ResponseCostHeader, "0.0004")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"buffered reply"},` +
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
				fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
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
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant",` +
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
// used to run on the REQUEST's context: cancelled by the disconnect, so
// pipeline.RunResponseFrame returned Deny("pipeline.cancelled") before calling any plugin
// (pipeline.go:203) and the charge was dropped on the floor. forwardproxy detaches the
// finalization context for exactly this reason (server.go:958).
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
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":"+
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
	br := bufio.NewReader(resp.Body)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading first event: %v", err)
		}
		if strings.HasPrefix(line, "data:") {
			break
		}
	}
	cancel()
	resp.Body.Close()

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
