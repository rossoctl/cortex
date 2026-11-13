//go:build praxis_e2e

package main

// End-to-end tests of praxis mode through the real cortex-gateway binary (#1721, plan
// Task 8). Each test starts the binary that CORTEX_GATEWAY_BIN names. A test dialer
// sends every origin address to one mock origin, so names such as chatgpt.com never
// reach DNS.

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/listener/forwardproxy"
	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/sessionapi"
	"github.com/rossoctl/cortex/core/tlsbridge"
)

const e2eSession = "00000000-0000-4000-8000-0000000000e2"

// allHopHeaders are the twelve hop headers (praxis/filters/src/record/attribution.rs HOP_HEADERS).
var allHopHeaders = []string{
	"X-Cortex-Agent", "X-Cortex-Proc", "X-Cortex-Client-Proc", "X-Cortex-Session", "X-Cortex-Source",
	"X-Cortex-Session-Header", "X-Cortex-Chain-Session", "X-Cortex-Attribution", "X-Cortex-Request-Id",
	"X-Cortex-Probe", "X-Cortex-Dest", "X-Cortex-Token",
}

// mockOrigin serves the handler a test sets and keeps each request's headers.
type mockOrigin struct {
	*httptest.Server
	mu      sync.Mutex
	handler http.HandlerFunc
	headers []http.Header
}

func newMockOrigin(t *testing.T) *mockOrigin {
	o := &mockOrigin{}
	o.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		o.headers = append(o.headers, r.Header.Clone())
		h := o.handler
		o.mu.Unlock()
		if h == nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		h(w, r)
	}))
	t.Cleanup(o.Close)
	return o
}

func (o *mockOrigin) serve(h http.HandlerFunc) {
	o.mu.Lock()
	o.handler = h
	o.mu.Unlock()
}

func (o *mockOrigin) lastHeader(t *testing.T) http.Header {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.headers) == 0 {
		t.Fatal("the origin got no request")
	}
	return o.headers[len(o.headers)-1]
}

// dial sends every address to the origin (Decision 15).
func (o *mockOrigin) dial(ctx context.Context, network, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, o.Listener.Addr().String())
}

// e2eRates prices every host and model at one rate, for both chains.
func e2eRates(t *testing.T) *pricing.Registry {
	t.Helper()
	var r pricing.Rates
	for tier, perToken := range map[pricing.Tier]float64{
		pricing.TierInput: 7e-6, pricing.TierCacheWrite: 11e-6, pricing.TierCacheRead: 3e-6, pricing.TierOutput: 23e-6,
	} {
		r.Base[tier], r.Set[tier] = perToken, true
	}
	tab, err := pricing.NewTable([]pricing.Entry{{Host: "*", Model: "*", Rates: r, Prov: pricing.ProvConfigured}})
	if err != nil {
		t.Fatal(err)
	}
	return pricing.NewRegistry(tab)
}

// chain is a forward proxy and its session store, in praxis mode or in direct mode.
type chain struct {
	origin *mockOrigin
	store  *session.Store
	proxy  string // the forward proxy, host:port
	client *http.Client
	cfg    *config.Config
	hop    *forwardproxy.Hop // nil in direct mode
	// startGateway starts cortex-gateway. newPraxisChain calls it unless noGateway is set.
	startGateway func()
}

type chainOpts struct {
	noGateway bool
	policy    string // policy file content; when set, praxis.policy.config_path names it
}

// e2eConfig has inference-parser on the outbound chain. In praxis mode outboundEntries
// builds none of it, as in main.
func e2eConfig(praxis bool) *config.Config {
	cfg := &config.Config{}
	cfg.Pipeline.Outbound.Plugins = []config.PluginEntry{{Name: "inference-parser"}}
	if praxis {
		cfg.Praxis = &config.PraxisConfig{Enabled: true}
	}
	return cfg
}

// newForwardProxy builds the forward proxy as main does, for cfg.
func newForwardProxy(t *testing.T, cfg *config.Config, store *session.Store, rates *pricing.Registry) *forwardproxy.Server {
	t.Helper()
	p, err := plugins.BuildWithDeps(outboundEntries(cfg, slog.Default()),
		plugins.Deps{Pricing: rates, Listener: forwardproxy.Support(false), LocalInstall: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Stop(context.Background()) })
	fp, err := forwardproxy.NewServer(pipeline.NewHolder(p), store, nil)
	if err != nil {
		t.Fatal(err)
	}
	fp.ResponsesWebSockets = cfg.Listener.ResponsesWebSocketsMode()
	fp.SessionIDHeaders = cfg.Session.SessionIDHeadersFor(true)
	fp.ClientAffinity = cfg.Session.ClientAffinityEnabled()
	return fp
}

func (c *chain) serveProxy(t *testing.T, fp *forwardproxy.Server) {
	t.Helper()
	ts := httptest.NewUnstartedServer(fp.Handler())
	ts.Config.ConnContext = fp.ConnContext
	ts.Start()
	t.Cleanup(ts.Close)
	c.proxy = ts.Listener.Addr().String()
	c.client = &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: c.proxy})}}
}

func newStore(t *testing.T) *session.Store {
	store := session.New(time.Hour, 0, 0)
	t.Cleanup(store.Close)
	return store
}

// newDirectChain is the direct-mode twin: the same origin and dialer, praxis off.
func newDirectChain(t *testing.T, origin *mockOrigin) *chain {
	t.Helper()
	c := &chain{origin: origin, store: newStore(t), cfg: e2eConfig(false)}
	fp := newForwardProxy(t, c.cfg, c.store, e2eRates(t))
	fp.Client.Transport.(*http.Transport).DialContext = origin.dial
	c.serveProxy(t, fp)
	return c
}

// newPraxisChain wires praxis mode with startPraxis, as main does, and starts the gateway.
func newPraxisChain(t *testing.T, origin *mockOrigin, opts chainOpts) *chain {
	t.Helper()
	bin := os.Getenv("CORTEX_GATEWAY_BIN")
	if bin == "" {
		t.Skip("CORTEX_GATEWAY_BIN is not set")
	}
	// A set but wrong path fails: a skip there would make CI's blocking step pass with no test run.
	if st, err := os.Stat(bin); err != nil || st.IsDir() {
		t.Fatalf("CORTEX_GATEWAY_BIN=%q does not name the cortex-gateway binary: %v", bin, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := &chain{origin: origin, store: newStore(t), cfg: e2eConfig(true)}
	apiLn, egressLn := listen(t), listen(t)
	c.cfg.Listener.SessionAPIAddr = apiLn.Addr().String()
	c.cfg.Praxis.Listen = freeAddr(t)
	c.cfg.Praxis.Egress = egressLn.Addr().String()

	dir := t.TempDir()
	if opts.policy != "" {
		path := dir + "/policy.yaml"
		if err := os.WriteFile(path, []byte(opts.policy), 0o600); err != nil {
			t.Fatal(err)
		}
		c.cfg.Praxis.Policy = &config.PraxisPolicyConfig{ConfigPath: path}
	}
	tokenPath, yamlPath, err := writeGatewayFiles(dir, c.cfg)
	if err != nil {
		t.Fatal(err)
	}
	token, err := readGatewayToken(func(string) string { return tokenPath }, os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}

	rates := e2eRates(t)
	fp := newForwardProxy(t, c.cfg, c.store, rates)
	up, err := tlsbridge.NewUpstreamClient(nil, false, tlsbridge.WithDialContext(origin.dial))
	if err != nil {
		t.Fatal(err)
	}
	hop, egressSrv := startPraxis(ctx, c.cfg, fp, token, up.Transport, egressLn)
	t.Cleanup(func() { egressSrv.Close() })
	c.hop = hop

	api := sessionapi.New("", c.store, ingestOptions(c.cfg, rates, token, hop)...)
	go func() { _ = api.Server().Serve(apiLn) }()
	t.Cleanup(func() { api.Server().Close() })
	c.serveProxy(t, fp)

	c.startGateway = func() {
		cmd := exec.Command(bin, "-c", yamlPath, "--exit-on-stdin-eof") //nolint:gosec // the test's own binary
		cmd.Env = []string{}
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			stdin.Close()
			done := make(chan struct{})
			go func() { _ = cmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
			if t.Failed() {
				t.Logf("cortex-gateway output:\n%s", out.String())
			}
		})
	}
	if !opts.noGateway {
		c.startGateway()
		c.waitReady(t, 20*time.Second)
	}
	return c
}

func (c *chain) waitReady(t *testing.T, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !c.hop.Ready() {
		if time.Now().After(deadline) {
			t.Fatalf("the hop is not ready after %s", d)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// request makes a request with the e2e session header.
func request(t *testing.T, ctx context.Context, method, rawURL string, body []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(session.ClaudeCodeSessionHeader, e2eSession)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func (c *chain) do(t *testing.T, req *http.Request) (int, []byte, http.Header) {
	t.Helper()
	resp, err := c.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b, resp.Header
}

func responseCount(evs []model.SessionEvent) int {
	n := 0
	for _, ev := range evs {
		if ev.Phase == pipeline.SessionResponse {
			n++
		}
	}
	return n
}

// waitResponses waits until the session holds n response rows, and returns its rows.
func (c *chain) waitResponses(t *testing.T, n int, d time.Duration) []model.SessionEvent {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		var evs []model.SessionEvent
		if v := c.store.View(e2eSession); v != nil {
			evs = v.Events
		}
		if responseCount(evs) >= n {
			return evs
		}
		if time.Now().After(deadline) {
			t.Fatalf("the session has %d response rows after %s, want %d: %+v", responseCount(evs), d, n, evs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// lastResponse is the newest response row.
func lastResponse(t *testing.T, evs []model.SessionEvent) model.SessionEvent {
	t.Helper()
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Phase == pipeline.SessionResponse {
			return evs[i]
		}
	}
	t.Fatal("no response row")
	return model.SessionEvent{}
}

const chatURL = "http://api.openai.com/v1/chat/completions"

var chatRequest = []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)

const chatJSON = `{"id":"c1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":5,"total_tokens":16}}`

var chatStreamRequest = []byte(`{"model":"gpt-4o","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hello"}]}`)

var chatFrames = []string{
	`data: {"id":"c2","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}` + "\n\n",
	`data: {"id":"c2","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
	`data: {"id":"c2","object":"chat.completion.chunk","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":5,"total_tokens":16}}` + "\n\n",
	"data: [DONE]\n\n",
}

func serveJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

// serveFrames writes frames[0], then waits for release (when not nil), then the rest.
// gz sends them gzip-encoded, with a flush after each frame.
func serveFrames(contentType string, gz bool, frames []string, release <-chan struct{}, pause time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", contentType)
		var out io.Writer = w
		var zw *gzip.Writer
		if gz {
			w.Header().Set("Content-Encoding", "gzip")
			zw = gzip.NewWriter(w)
			out = zw
		}
		w.WriteHeader(http.StatusOK)
		flush := func() {
			if zw != nil {
				_ = zw.Flush()
			}
			w.(http.Flusher).Flush()
		}
		for i, f := range frames {
			if i == 1 {
				if release != nil {
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				time.Sleep(pause)
			}
			_, _ = io.WriteString(out, f)
			flush()
		}
		if zw != nil {
			_ = zw.Close()
		}
	}
}

// readFrame reads one SSE frame (to the blank line), or fails after 10 s.
func readFrame(t *testing.T, br *bufio.Reader) string {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		var b strings.Builder
		for {
			line, err := br.ReadString('\n')
			b.WriteString(line)
			if err != nil || line == "\n" {
				got <- b.String()
				return
			}
		}
	}()
	select {
	case s := <-got:
		return s
	case <-time.After(10 * time.Second):
		t.Fatal("no frame in 10s: the response is buffered")
		return ""
	}
}

// W1 1: the response row's host and cost equal the direct run's.
func TestPraxisE2E_HostAndCostMatchDirect(t *testing.T) {
	origin := newMockOrigin(t)
	origin.serve(serveJSON(chatJSON))
	var rows [2]model.SessionEvent
	for i, c := range []*chain{newDirectChain(t, origin), newPraxisChain(t, origin, chainOpts{})} {
		if st, b, _ := c.do(t, request(t, t.Context(), http.MethodPost, chatURL, chatRequest)); st != http.StatusOK {
			t.Fatalf("status %d: %s", st, b)
		}
		rows[i] = lastResponse(t, c.waitResponses(t, 1, 10*time.Second))
	}
	direct, praxis := rows[0], rows[1]
	if praxis.Host != direct.Host || praxis.Host == "" {
		t.Errorf("host = %q, want the direct run's %q", praxis.Host, direct.Host)
	}
	if direct.Plugins["cost"] == nil {
		t.Fatalf("the direct run has no cost: %+v", direct.Plugins)
	}
	if !jsonEqual(praxis.Plugins["cost"], direct.Plugins["cost"]) {
		t.Errorf("cost = %s, want the direct run's %s", praxis.Plugins["cost"], direct.Plugins["cost"])
	}
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// streamFirstFrame sends req, reads frame 1 before it releases the origin, and returns
// the whole body.
func streamFirstFrame(t *testing.T, c *chain, req *http.Request, release chan struct{}) string {
	t.Helper()
	resp, err := c.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	br := bufio.NewReader(resp.Body)
	first := readFrame(t, br)
	close(release)
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	return first + string(rest)
}

// W1 3: a gzip SSE response arrives frame by frame.
func TestPraxisE2E_GzipSSEArrivesAsFrames(t *testing.T) {
	origin := newMockOrigin(t)
	c := newPraxisChain(t, origin, chainOpts{})
	release := make(chan struct{})
	origin.serve(serveFrames("text/event-stream", true, chatFrames, release, 0))
	got := streamFirstFrame(t, c, request(t, t.Context(), http.MethodPost, chatURL, chatStreamRequest), release)
	if want := strings.Join(chatFrames, ""); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// W1 5: a client that hangs up mid-stream gives client_canceled, and the origin sees it.
func TestPraxisE2E_HangUpIsClientCanceled(t *testing.T) {
	origin := newMockOrigin(t)
	c := newPraxisChain(t, origin, chainOpts{})
	originDone := make(chan struct{})
	origin.serve(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chatFrames[0])
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(originDone)
	})
	ctx, cancel := context.WithCancel(t.Context())
	resp, err := c.client.Do(request(t, ctx, http.MethodPost, chatURL, chatStreamRequest))
	if err != nil {
		t.Fatal(err)
	}
	readFrame(t, bufio.NewReader(resp.Body))
	cancel()
	resp.Body.Close()
	select {
	case <-originDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the origin's request context did not end within 5s")
	}
	row := lastResponse(t, c.waitResponses(t, 1, 10*time.Second))
	if row.Error == nil || row.Error.Kind != "client_canceled" {
		t.Errorf("error = %+v, want client_canceled", row.Error)
	}
}

// W1 7: with no gateway, a request gets 503 gateway_unavailable. Once the gateway runs, 200.
func TestPraxisE2E_FailClosedAtStartup(t *testing.T) {
	origin := newMockOrigin(t)
	origin.serve(serveJSON(chatJSON))
	c := newPraxisChain(t, origin, chainOpts{noGateway: true})
	if st, b, _ := c.do(t, request(t, t.Context(), http.MethodPost, chatURL, chatRequest)); st != http.StatusServiceUnavailable {
		t.Fatalf("status = %d (%s), want 503", st, b)
	}
	row := lastResponse(t, c.waitResponses(t, 1, 5*time.Second))
	if row.Error == nil || row.Error.Kind != model.ErrorKindGatewayUnavailable {
		t.Errorf("error = %+v, want %s", row.Error, model.ErrorKindGatewayUnavailable)
	}
	c.startGateway()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, _, _ := c.do(t, request(t, t.Context(), http.MethodPost, chatURL, chatRequest))
		if st == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %d 10s after the gateway started, want 200", st)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// W1 8: 65 s of silence mid-stream does not cut the stream.
func TestPraxisE2E_SilenceMidStream(t *testing.T) {
	origin := newMockOrigin(t)
	c := newPraxisChain(t, origin, chainOpts{})
	origin.serve(serveFrames("text/event-stream", false, chatFrames, nil, 65*time.Second))
	st, b, _ := c.do(t, request(t, t.Context(), http.MethodPost, chatURL, chatStreamRequest))
	if want := strings.Join(chatFrames, ""); st != http.StatusOK || string(b) != want {
		t.Fatalf("got %d %q, want 200 %q", st, b, want)
	}
	if row := lastResponse(t, c.waitResponses(t, 1, 10*time.Second)); row.Error != nil {
		t.Errorf("error = %+v, want none", row.Error)
	}
}

// W1 9: an 11 MiB body each way passes, and the row counts both. The response streams
// as SSE frames of 64 KiB: a buffered response over 10 MiB is a 502 in direct mode too.
func TestPraxisE2E_OverTenMiBEachWay(t *testing.T) {
	const size = 11 << 20
	origin := newMockOrigin(t)
	c := newPraxisChain(t, origin, chainOpts{})
	got := make(chan int64, 1)
	origin.serve(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		got <- n
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := []byte("data: " + strings.Repeat("x", 64<<10-8) + "\n\n")
		for sent := 0; sent < size; sent += len(chunk) {
			_, _ = w.Write(chunk)
			w.(http.Flusher).Flush()
		}
	})
	req := request(t, t.Context(), http.MethodPost, "http://files.example.com/upload", bytes.Repeat([]byte("u"), size))
	req.Header.Set("Content-Type", "application/octet-stream")
	st, b, _ := c.do(t, req)
	if st != http.StatusOK || len(b) != size {
		t.Fatalf("got %d with %d bytes, want 200 with %d", st, len(b), size)
	}
	if n := <-got; n != size {
		t.Errorf("the origin read %d bytes, want %d", n, size)
	}
	// One direction per row (docs/session-api.md): bytesUp on the request row, bytesDown on the response row.
	evs := c.waitResponses(t, 1, 10*time.Second)
	var up int64
	for _, ev := range evs {
		if ev.Phase == pipeline.SessionRequest {
			up = ev.BytesUp
		}
	}
	if down := lastResponse(t, evs).BytesDown; up != size || down != size {
		t.Errorf("bytesUp, bytesDown = %d, %d, want %d each", up, down, size)
	}
}

const codexURL = "http://chatgpt.com/backend-api/codex/responses"

var codexRequest = []byte(`{"model":"gpt-5","stream":true,"input":[{"role":"user","content":"hello"}]}`)

// Codex's endpoint labels its SSE as application/json: it still streams. A 405 keeps its body.
func TestPraxisE2E_CodexMislabeledSSE(t *testing.T) {
	origin := newMockOrigin(t)
	c := newPraxisChain(t, origin, chainOpts{})
	frames := []string{
		`data: {"type":"response.created","response":{"id":"r1","model":"gpt-5"}}` + "\n\n",
		`data: {"type":"response.completed","response":{"id":"r1","model":"gpt-5","usage":{"input_tokens":11,"output_tokens":5,"total_tokens":16}}}` + "\n\n",
	}
	release := make(chan struct{})
	origin.serve(serveFrames("application/json", false, frames, release, 0))
	if got := streamFirstFrame(t, c, request(t, t.Context(), http.MethodPost, codexURL, codexRequest), release); got != strings.Join(frames, "") {
		t.Errorf("body = %q", got)
	}
	c.waitResponses(t, 1, 10*time.Second)

	const detail = `{"detail":"Method Not Allowed"}`
	origin.serve(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = io.WriteString(w, detail)
	})
	st, b, _ := c.do(t, request(t, t.Context(), http.MethodPost, codexURL, codexRequest))
	if st != http.StatusMethodNotAllowed || string(b) != detail {
		t.Fatalf("got %d %q, want 405 %q", st, b, detail)
	}
	row := lastResponse(t, c.waitResponses(t, 2, 10*time.Second))
	// The provider's text must not go into the store, which has no authentication.
	if row.StatusCode != http.StatusMethodNotAllowed || row.Error == nil || row.Error.Kind != "backend_error" || row.Error.Code != "405" || row.Error.Message != "" {
		t.Errorf("row = %d %+v, want 405 backend_error/405 with no message", row.StatusCode, row.Error)
	}
}

// Cortex owns the encoding: the origin sees only gzip, and the client gets plain JSON.
func TestPraxisE2E_AcceptEncodingDecoded(t *testing.T) {
	origin := newMockOrigin(t)
	c := newPraxisChain(t, origin, chainOpts{})
	origin.serve(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		_, _ = io.WriteString(zw, chatJSON)
		_ = zw.Close()
	})
	req := request(t, t.Context(), http.MethodPost, chatURL, chatRequest)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	st, b, h := c.do(t, req)
	if st != http.StatusOK || string(b) != chatJSON || h.Get("Content-Encoding") != "" {
		t.Fatalf("got %d %q (Content-Encoding %q), want 200 and the plain JSON", st, b, h.Get("Content-Encoding"))
	}
	if ae := origin.lastHeader(t).Get("Accept-Encoding"); ae != "gzip" {
		t.Errorf("the origin saw Accept-Encoding %q, want gzip", ae)
	}
	row := lastResponse(t, c.waitResponses(t, 1, 10*time.Second))
	if row.Inference == nil || row.Inference.InputTokens != 11 || row.Inference.OutputTokens != 5 {
		t.Errorf("inference = %+v, want 11 input and 5 output tokens", row.Inference)
	}
}

// No hop header reaches the origin, whoever sent it. Other X-Cortex-* headers pass.
func TestPraxisE2E_NoHopHeaderReachesOrigin(t *testing.T) {
	origin := newMockOrigin(t)
	origin.serve(serveJSON(chatJSON))
	c := newPraxisChain(t, origin, chainOpts{})
	req := request(t, t.Context(), http.MethodPost, chatURL, chatRequest)
	for _, k := range []string{"X-Cortex-Attribution", "X-Cortex-Dest", "X-Cortex-Token", "X-Cortex-Request-Id", "X-Cortex-Probe"} {
		req.Header.Set(k, "from-the-client")
	}
	req.Header.Set("X-Cortex-Custom", "keep")
	if st, b, _ := c.do(t, req); st != http.StatusOK {
		t.Fatalf("status %d: %s", st, b)
	}
	h := origin.lastHeader(t)
	for _, k := range allHopHeaders {
		if v := h.Values(k); len(v) > 0 {
			t.Errorf("the origin saw %s: %q", k, v)
		}
	}
	if h.Get("X-Cortex-Custom") != "keep" {
		t.Errorf("the origin did not see X-Cortex-Custom: %v", h)
	}
}

// A request straight to praxis.listen with no token gets 403 and records nothing.
func TestPraxisE2E_TokenlessRequestTo47605(t *testing.T) {
	origin := newMockOrigin(t)
	origin.serve(serveJSON(chatJSON))
	c := newPraxisChain(t, origin, chainOpts{})
	before := len(c.store.ListSessions())
	req := request(t, t.Context(), http.MethodPost, "http://"+c.cfg.Praxis.ListenAddr()+"/v1/chat/completions", chatRequest)
	req.Host = "api.openai.com"
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	time.Sleep(time.Second) // a row, if one were sent, lands well within this
	if after := len(c.store.ListSessions()); after != before || c.store.View(e2eSession) != nil {
		t.Errorf("sessions went from %d to %d; e2e session = %+v", before, after, c.store.View(e2eSession))
	}
}

// eventsJSON reads the session's rows from the session API's JSON form.
func eventsJSON(t *testing.T, c *chain) []map[string]any {
	t.Helper()
	srv := httptest.NewServer(sessionapi.New("", c.store).Server().Handler)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/sessions/" + e2eSession)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v.Events
}

// The JSON and SSE requests record the same rows in both modes.
func TestPraxisE2E_SameEventsAsDirect(t *testing.T) {
	origin := newMockOrigin(t)
	// ttfbMs and durationMs are omitempty. The delay keeps both at 5 ms or more, so a
	// missing key means the field is missing, not a 0 ms value.
	slow := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { time.Sleep(5 * time.Millisecond); h(w, r) }
	}
	var got [2][]map[string]any
	for i, c := range []*chain{newDirectChain(t, origin), newPraxisChain(t, origin, chainOpts{})} {
		origin.serve(slow(serveJSON(chatJSON)))
		if st, b, _ := c.do(t, request(t, t.Context(), http.MethodPost, chatURL, chatRequest)); st != http.StatusOK {
			t.Fatalf("status %d: %s", st, b)
		}
		c.waitResponses(t, 1, 10*time.Second)
		origin.serve(slow(serveFrames("text/event-stream", false, chatFrames, nil, 0)))
		if st, b, _ := c.do(t, request(t, t.Context(), http.MethodPost, chatURL, chatStreamRequest)); st != http.StatusOK {
			t.Fatalf("status %d: %s", st, b)
		}
		c.waitResponses(t, 2, 10*time.Second)
		got[i] = eventsJSON(t, c)
	}
	direct, praxis := got[0], got[1]
	if len(direct) != len(praxis) {
		t.Fatalf("direct has %d rows, praxis %d:\ndirect %v\npraxis %v", len(direct), len(praxis), direct, praxis)
	}
	for i := range direct {
		d, p := direct[i], praxis[i]
		for _, k := range []string{"seq", "at", "sessionId", "requestId"} {
			delete(d, k)
			delete(p, k)
		}
		for _, k := range []string{"ttfbMs", "durationMs"} {
			dv, dok := d[k].(float64)
			pv, pok := p[k].(float64)
			// A missing key reads as 0, which is within 50 ms of the other side's value.
			if dok != pok {
				t.Errorf("row %d: %s = %s, direct %s (on one side only)", i, k, compact(p[k]), compact(d[k]))
			} else if dv-pv > 50 || pv-dv > 50 {
				t.Errorf("row %d: %s = %v, direct %v (over 50 ms apart)", i, k, pv, dv)
			}
			delete(d, k)
			delete(p, k)
		}
		// Known difference, praxis/README.md "Gateway decisions" 6: the gateway runs no
		// Cortex plugin, so its rows have no invocations.
		if p["invocations"] != nil {
			t.Errorf("row %d: praxis invocations = %s, want none", i, compact(p["invocations"]))
		}
		delete(d, "invocations")
		for _, k := range unionKeys(d, p) {
			if !reflect.DeepEqual(d[k], p[k]) {
				t.Errorf("row %d (%v): %s = %s, direct %s", i, d["phase"], k, compact(p[k]), compact(d[k]))
			}
		}
	}
}

func unionKeys(a, b map[string]any) []string {
	var keys []string
	for k := range a {
		keys = append(keys, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	return keys
}

func compact(v any) string {
	if v == nil {
		return "<absent>"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// e2ePolicy permits model-ok and denies every other model.
const e2ePolicy = `routes:
  - llm: model-ok
    authorization:
      pre_invocation:
        - "llm.provider == 'never': deny('unreachable', 'never')"
  - llm: "*"
    authorization:
      pre_invocation:
        - "deny('model is not permitted', 'model_not_allowed')"
`

func (o *mockOrigin) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.headers)
}

func modelRequest(t *testing.T, model string) *http.Request {
	t.Helper()
	return request(t, t.Context(), http.MethodPost, chatURL,
		[]byte(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
}

// The policy filter permits model-ok and refuses model-bad before the origin.
func TestPraxisE2E_PolicyDeniesModel(t *testing.T) {
	origin := newMockOrigin(t)
	origin.serve(serveJSON(chatJSON))
	c := newPraxisChain(t, origin, chainOpts{policy: e2ePolicy})

	if st, b, _ := c.do(t, modelRequest(t, "model-ok")); st != http.StatusOK {
		t.Fatalf("model-ok: status %d, body %s", st, b)
	}
	if n := origin.count(); n != 1 {
		t.Fatalf("model-ok: the origin got %d requests, want 1", n)
	}
	st, b, h := c.do(t, modelRequest(t, "model-bad"))
	if st != http.StatusForbidden {
		t.Fatalf("model-bad: status %d, body %s", st, b)
	}
	if got := h.Get("X-Policy-Violation"); got != "model_not_allowed" {
		t.Fatalf("model-bad: X-Policy-Violation = %q, want model_not_allowed", got)
	}
	if n := origin.count(); n != 1 {
		t.Fatalf("model-bad: the origin got %d requests, want 1", n)
	}
	c.waitResponses(t, 2, 10*time.Second)
}

// A policy with llm: routes passes requests that are not inference calls.
func TestPraxisE2E_PolicyPassesNonInference(t *testing.T) {
	origin := newMockOrigin(t)
	origin.serve(serveJSON(chatJSON))
	c := newPraxisChain(t, origin, chainOpts{policy: e2ePolicy})

	form := request(t, t.Context(), http.MethodPost, "http://oauth.example.com/token",
		[]byte("grant_type=refresh_token&refresh_token=r"))
	form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Over the policy filter's 10 MiB default, under the gateway's 32 MiB.
	upload := request(t, t.Context(), http.MethodPost, "http://files.example.com/upload", bytes.Repeat([]byte("u"), 11<<20))
	upload.Header.Set("Content-Type", "application/octet-stream")
	reqs := map[string]*http.Request{
		"upload over 10 MiB": upload,
		"json without model": request(t, t.Context(), http.MethodPost, "http://telemetry.example.com/v1/events", []byte(`{"events":[{"n":1}]}`)),
		"form body":          form,
		"put":                request(t, t.Context(), http.MethodPut, "http://api.example.com/repos/x", []byte(`{"name":"x"}`)),
		"mcp tools/call":     request(t, t.Context(), http.MethodPost, "http://mcp.example.com/mcp", []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t","arguments":{}}}`)),
	}
	want := 0
	for name, req := range reqs {
		want++
		if st, b, h := c.do(t, req); st != http.StatusOK {
			t.Errorf("%s: status %d, X-Policy-Violation %q, body %s", name, st, h.Get("X-Policy-Violation"), b)
		}
		if n := origin.count(); n != want {
			t.Errorf("%s: the origin got %d requests, want %d", name, n, want)
			want = n
		}
	}
}

// e2eToolPolicy permits safe_tool and denies every other MCP tool.
// The llm: route admits the hop's readiness probe (GET /, no MCP row): a policy
// with only tool: routes refuses every request that has no mcp.method (500).
// The llm: route also admits any bodyless request, so the GET and DELETE /mcp
// cases below do not prove that mcp.method is set; the observe.rs unit test does.
const e2eToolPolicy = `routes:
  - llm: "*"
    authorization:
      pre_invocation:
        - "llm.provider == 'never': deny('unreachable', 'never')"
  - tool: safe_tool
    authorization:
      pre_invocation:
        - "http.method == 'NEVER': deny('unreachable', 'never')"
  - tool: "*"
    authorization:
      pre_invocation:
        - "deny('tool is not permitted', 'tool_not_allowed')"
`

// cortex_observe writes mcp.method and mcp.name, so the policy filter
// permits safe_tool, refuses danger_tool before the origin, and passes tools/list.
func TestPraxisE2E_PolicyDeniesTool(t *testing.T) {
	origin := newMockOrigin(t)
	origin.serve(serveJSON(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	c := newPraxisChain(t, origin, chainOpts{policy: e2eToolPolicy})
	const mcpURL = "http://mcp.example.com/mcp"
	call := func(id int, tool string) *http.Request {
		return request(t, t.Context(), http.MethodPost, mcpURL,
			fmt.Appendf(nil, `{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"%s","arguments":{}}}`, id, tool))
	}

	if st, b, _ := c.do(t, call(1, "safe_tool")); st != http.StatusOK {
		t.Fatalf("safe_tool: status %d, body %s", st, b)
	}
	if n := origin.count(); n != 1 {
		t.Fatalf("safe_tool: the origin got %d requests, want 1", n)
	}

	st, b, h := c.do(t, call(7, "danger_tool"))
	if st != http.StatusOK {
		t.Fatalf("danger_tool: status %d, body %s", st, b)
	}
	if got := h.Get("X-Policy-Violation"); got != "tool_not_allowed" {
		t.Fatalf("danger_tool: X-Policy-Violation = %q, want tool_not_allowed", got)
	}
	var rpc struct {
		ID    int `json:"id"`
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &rpc); err != nil {
		t.Fatalf("danger_tool: body %s: %v", b, err)
	}
	if rpc.Error.Code != -32001 || rpc.ID != 7 {
		t.Fatalf("danger_tool: error.code %d, id %d, want -32001 and 7; body %s", rpc.Error.Code, rpc.ID, b)
	}
	if n := origin.count(); n != 1 {
		t.Fatalf("danger_tool: the origin got %d requests, want 1", n)
	}

	list := request(t, t.Context(), http.MethodPost, mcpURL, []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`))
	if st, b, _ := c.do(t, list); st != http.StatusOK {
		t.Fatalf("tools/list: status %d, body %s", st, b)
	}
	if n := origin.count(); n != 2 {
		t.Fatalf("tools/list: the origin got %d requests, want 2", n)
	}

	// A bodyless GET (SSE stream) and DELETE (session end) carry a synthetic
	// mcp.method, so the policy admits them with no tool hook.
	stream := request(t, t.Context(), http.MethodGet, mcpURL, nil)
	end := request(t, t.Context(), http.MethodDelete, mcpURL, nil)
	end.Header.Set("Mcp-Session-Id", "s1")
	for i, req := range []*http.Request{stream, end} {
		if st, b, _ := c.do(t, req); st != http.StatusOK {
			t.Fatalf("%s /mcp: status %d, body %s", req.Method, st, b)
		}
		if n := origin.count(); n != 3+i {
			t.Fatalf("%s /mcp: the origin got %d requests, want %d", req.Method, n, 3+i)
		}
	}
}

// Without a policy, a model-bad request reaches the origin.
func TestPraxisE2E_NoPolicyNoChange(t *testing.T) {
	origin := newMockOrigin(t)
	origin.serve(serveJSON(chatJSON))
	c := newPraxisChain(t, origin, chainOpts{})
	if st, b, _ := c.do(t, modelRequest(t, "model-bad")); st != http.StatusOK {
		t.Fatalf("status %d, body %s", st, b)
	}
	if n := origin.count(); n != 1 {
		t.Fatalf("the origin got %d requests, want 1", n)
	}
}
