package forwardproxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/attribution"
	"github.com/rossoctl/cortex/core/listener/skiphost"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/plugintesting"
	"github.com/rossoctl/cortex/core/session"
)

const hopTestToken = "hop-test-token"

// seenRequest is what the fake gateway got.
type seenRequest struct {
	host, uri string
	header    http.Header
}

// fakeGateway is an httptest.Server that keeps each request and answers with answer.
type fakeGateway struct {
	*httptest.Server
	mu   sync.Mutex
	seen []seenRequest
}

func newFakeGateway(t *testing.T, answer http.HandlerFunc) *fakeGateway {
	t.Helper()
	g := &fakeGateway{}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.seen = append(g.seen, seenRequest{host: r.Host, uri: r.RequestURI, header: r.Header.Clone()})
		g.mu.Unlock()
		if answer != nil {
			answer(w, r)
		}
	}))
	t.Cleanup(g.Close)
	return g
}

func (g *fakeGateway) requests() []seenRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]seenRequest(nil), g.seen...)
}

func (g *fakeGateway) only(t *testing.T) seenRequest {
	t.Helper()
	got := g.requests()
	if len(got) != 1 {
		t.Fatalf("gateway got %d requests, want 1", len(got))
	}
	return got[0]
}

// hopProxy is a forward proxy whose Hop dials gatewayAddr. The hop is ready unless
// notReady. configure, when non-nil, runs before the proxy serves.
func hopProxy(t *testing.T, gatewayAddr string, notReady bool, configure func(*Server), plugins ...pipeline.Plugin) (*httptest.Server, *session.Store) {
	t.Helper()
	p, err := plugintesting.BuildPipeline(plugins)
	if err != nil {
		t.Fatal(err)
	}
	store := session.New(5*time.Minute, 100, 0)
	t.Cleanup(store.Close)
	hop := NewHop(gatewayAddr, hopTestToken)
	hop.ready.Store(!notReady)
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Client:           &http.Client{Transport: &http.Transport{Proxy: nil}},
		SessionIDHeaders: []string{session.ClaudeCodeSessionHeader},
		Hop:              hop,
	}
	if configure != nil {
		configure(srv)
	}
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.Config.ConnContext = srv.ConnContext
	ts.Start()
	t.Cleanup(ts.Close)
	return ts, store
}

func gatewayAddr(g *fakeGateway) string { return strings.TrimPrefix(g.URL, "http://") }

// hopClient sends through the proxy. It keeps its own Accept-Encoding.
func hopClient(proxy *httptest.Server) *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL)), DisableCompression: true}}
}

func doVia(t *testing.T, c *http.Client, req *http.Request) (int, string, http.Header) {
	t.Helper()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func decodeWire(t *testing.T, v string) attribution.Wire {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		t.Fatalf("decode %s %q: %v", attribution.HeaderAttribution, v, err)
	}
	var w attribution.Wire
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	return w
}

func TestHop_DialsGatewayKeepsHostAndPath(t *testing.T) {
	gw := newFakeGateway(t, nil)
	proxy, _ := hopProxy(t, gatewayAddr(gw), false, nil)

	req, _ := http.NewRequest(http.MethodGet, "http://api.example.com/v1/x?q=1", nil)
	req.Header.Set(session.ClaudeCodeSessionHeader, "s1")
	if st, _, _ := doVia(t, hopClient(proxy), req); st != http.StatusOK {
		t.Fatalf("status = %d, want 200", st)
	}
	got := gw.only(t)
	if got.uri != "/v1/x?q=1" || got.host != "api.example.com" {
		t.Errorf("gateway saw %q Host %q, want /v1/x?q=1 Host api.example.com", got.uri, got.host)
	}
	if d := got.header.Get(headerDest); d != "http://api.example.com:80" {
		t.Errorf("%s = %q, want http://api.example.com:80", headerDest, d)
	}
	if v := got.header.Get(headerToken); v != hopTestToken {
		t.Errorf("%s = %q, want the hop's token", headerToken, v)
	}
	if got.header.Get(headerRequestID) == "" {
		t.Errorf("%s is empty", headerRequestID)
	}
	w := decodeWire(t, got.header.Get(attribution.HeaderAttribution))
	if w.Session != "s1" || w.RecordSession != "s1" || w.Source != string(pipeline.SessionSourceHeader) {
		t.Errorf("wire = %+v, want session and recordSession s1 from the header", w)
	}
}

// destWriterPlugin writes pctx.Scheme, pctx.Host and a Dest header, but does not redirect.
type destWriterPlugin struct{}

func (destWriterPlugin) Name() string { return "dest-writer" }
func (destWriterPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (destWriterPlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	pctx.Scheme, pctx.Host = "https", "elsewhere.example"
	pctx.Headers.Set(headerDest, "http://elsewhere.example:1")
	return pipeline.Action{Type: pipeline.Continue}
}
func (destWriterPlugin) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

func TestHop_OnlyTheDialTargetMoves(t *testing.T) {
	gw := newFakeGateway(t, nil)
	proxy, _ := hopProxy(t, gatewayAddr(gw), false, nil, destWriterPlugin{})

	req, _ := http.NewRequest(http.MethodGet, "http://api.example.com/p", nil)
	doVia(t, hopClient(proxy), req)
	got := gw.only(t)
	if d := got.header.Get(headerDest); d != "http://api.example.com:80" {
		t.Errorf("%s = %q, want the real destination http://api.example.com:80", headerDest, d)
	}
	if got.host != "api.example.com" || got.uri != "/p" {
		t.Errorf("gateway saw %q Host %q, want /p Host api.example.com", got.uri, got.host)
	}
}

func TestHop_OriginFormStaysDirect(t *testing.T) {
	gw := newFakeGateway(t, nil)
	proxy, _ := hopProxy(t, gatewayAddr(gw), false, nil)

	// No proxy: the forward proxy gets "GET /p", with no scheme or host to dial.
	resp, err := http.Get(proxy.URL + "/p")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want the direct-mode 502", resp.StatusCode)
	}
	if n := len(gw.requests()); n != 0 {
		t.Errorf("gateway got %d requests, want 0", n)
	}
}

// sessionHeaderWriterPlugin writes pctx.SessionHeader, which the Wire must not read.
type sessionHeaderWriterPlugin struct{}

func (sessionHeaderWriterPlugin) Name() string { return "session-header-writer" }
func (sessionHeaderWriterPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (sessionHeaderWriterPlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	pctx.SessionHeader = "X-Plugin-Wrote-This"
	return pipeline.Action{Type: pipeline.Continue}
}
func (sessionHeaderWriterPlugin) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

func TestHop_WireSessionHeaderIsPinned(t *testing.T) {
	gw := newFakeGateway(t, nil)
	proxy, _ := hopProxy(t, gatewayAddr(gw), false, nil, sessionHeaderWriterPlugin{})

	req, _ := http.NewRequest(http.MethodGet, "http://api.example.com/p", nil)
	req.Header.Set(session.ClaudeCodeSessionHeader, "s1")
	doVia(t, hopClient(proxy), req)
	w := decodeWire(t, gw.only(t).header.Get(attribution.HeaderAttribution))
	if w.SessionHeader == "X-Plugin-Wrote-This" || w.SessionHeader == "" {
		t.Errorf("wire SessionHeader = %q, want the header that named the session", w.SessionHeader)
	}
}

func TestHop_ClientHopHeadersNeverReachGateway(t *testing.T) {
	gw := newFakeGateway(t, nil)
	proxy, _ := hopProxy(t, gatewayAddr(gw), false, nil)

	req, _ := http.NewRequest(http.MethodGet, "http://api.example.com/p", nil)
	forged := attribution.Wire{Session: "forged"}.Encode()
	req.Header.Set(attribution.HeaderAttribution, forged)
	req.Header.Set(headerDest, "http://evil.example:1")
	req.Header.Set(headerToken, "forged-token")
	req.Header.Set(headerRequestID, "forged-id")
	req.Header.Set(headerProbe, "1")
	req.Header.Set("X-Cortex-Custom", "v")
	doVia(t, hopClient(proxy), req)

	h := gw.only(t).header
	if h.Get(headerDest) != "http://api.example.com:80" || h.Get(headerToken) != hopTestToken {
		t.Errorf("Dest %q Token %q, want the hop's own", h.Get(headerDest), h.Get(headerToken))
	}
	if id := h.Get(headerRequestID); id == "" || id == "forged-id" {
		t.Errorf("%s = %q, want the hop's own", headerRequestID, id)
	}
	if a := h.Get(attribution.HeaderAttribution); a == forged || decodeWire(t, a).Session == "forged" {
		t.Errorf("%s is the client's", attribution.HeaderAttribution)
	}
	if _, ok := h[headerProbe]; ok {
		t.Errorf("gateway got %s", headerProbe)
	}
	if v := h.Get("X-Cortex-Custom"); v != "v" {
		t.Errorf("X-Cortex-Custom = %q, want v", v)
	}
	for _, k := range clientHopHeaders {
		if n := len(h.Values(k)); n > 1 {
			t.Errorf("%s has %d values, want 1", k, n)
		}
	}
}

func TestHop_OwnsEncoding(t *testing.T) {
	gw := newFakeGateway(t, func(w http.ResponseWriter, _ *http.Request) {
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		_, _ = zw.Write([]byte("hello"))
		_ = zw.Close()
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(b.Bytes())
	})
	proxy, _ := hopProxy(t, gatewayAddr(gw), false, nil)

	req, _ := http.NewRequest(http.MethodGet, "http://api.example.com/p", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	_, body, _ := doVia(t, hopClient(proxy), req)
	if ae := gw.only(t).header.Get("Accept-Encoding"); ae != "gzip" {
		t.Errorf("gateway Accept-Encoding = %q, want gzip only", ae)
	}
	if body != "hello" {
		t.Errorf("client body = %q, want it decoded", body)
	}
}

func TestHop_NoRowsForHoppedRequest(t *testing.T) {
	gw := newFakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sse" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: one\n\ndata: two\n\n")
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	proxy, store := hopProxy(t, gatewayAddr(gw), false, nil)
	for _, p := range []string{"/json", "/sse"} {
		req, _ := http.NewRequest(http.MethodGet, "http://api.example.com"+p, nil)
		doVia(t, hopClient(proxy), req)
	}
	if ev := allEvents(t, store); len(ev) != 0 {
		t.Errorf("store holds %d rows, want none: the gateway records a hopped request", len(ev))
	}
}

// The process-attribution twin of TestProcessAttribution_AServicesToolJoinsTheSessionThatAnsweredLast:
// only touch makes the tool join a, the session that answered last.
func TestHop_TouchStillRuns(t *testing.T) {
	procs := newFakeProcs(fproc(500, 1, opencodeExe), fproc(600, 500, "/usr/bin/curl"))
	release, arrived := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	gw := newFakeGateway(t, func(_ http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/a" {
			close(arrived)
			<-release
		}
	})
	proxy, _ := hopProxy(t, gatewayAddr(gw), false, func(s *Server) {
		s.SessionIDHeaders = append(s.SessionIDHeaders, "X-Opencode-Session-Id")
		s.ClientAffinity = true
		s.Processes = procs
	})
	svc := procs.clientFor(proxy.URL, 500)

	done := make(chan struct{})
	go func() {
		defer close(done)
		sendAs(t, svc, "http://api.example.com/a", "opencode/latest/2.0.21/cli", "X-Opencode-Session-Id", "ses_a")
	}()
	<-arrived
	sendAs(t, svc, "http://api.example.com/b", "opencode/latest/2.0.21/cli", "X-Opencode-Session-Id", "ses_b")
	unblock()
	<-done
	sendAs(t, procs.clientFor(proxy.URL, 600), "http://api.example.com/tool", "curl/8.7.1", "", "")

	for _, r := range gw.requests() {
		if r.uri == "/tool" {
			if w := decodeWire(t, r.header.Get(attribution.HeaderAttribution)); w.Session != "ses_a" {
				t.Errorf("tool session = %q, want ses_a (the session that answered last)", w.Session)
			}
			return
		}
	}
	t.Fatal("the gateway did not get /tool")
}

func assertGatewayUnavailableRows(t *testing.T, store *session.Store) {
	t.Helper()
	ev := allEvents(t, store)
	if len(ev) != 2 {
		t.Fatalf("store holds %d rows, want the request row and its 503 row: %+v", len(ev), ev)
	}
	req, resp := ev[0], ev[1]
	if req.Phase != pipeline.SessionRequest || resp.Phase != pipeline.SessionResponse {
		t.Fatalf("phases = %q, %q", req.Phase, resp.Phase)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Error == nil || resp.Error.Kind != pipeline.ErrorKindGatewayUnavailable {
		t.Errorf("response row = %d %+v, want 503 %s", resp.StatusCode, resp.Error, pipeline.ErrorKindGatewayUnavailable)
	}
	if resp.RequestID == "" || resp.RequestID != req.RequestID {
		t.Errorf("response RequestID %q, want the request's %q", resp.RequestID, req.RequestID)
	}
}

func TestHop_NotReadyIsGatewayUnavailable(t *testing.T) {
	gw := newFakeGateway(t, nil)
	proxy, store := hopProxy(t, gatewayAddr(gw), true, nil)
	req, _ := http.NewRequest(http.MethodGet, "http://api.example.com/p", nil)
	if st, _, _ := doVia(t, hopClient(proxy), req); st != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", st)
	}
	if n := len(gw.requests()); n != 0 {
		t.Errorf("gateway got %d requests, want none", n)
	}
	assertGatewayUnavailableRows(t, store)
}

func TestHop_DialFailureIsGatewayUnavailable(t *testing.T) {
	proxy, store := hopProxy(t, closedAddr(t), false, nil)
	req, _ := http.NewRequest(http.MethodGet, "http://api.example.com/p", nil)
	if st, _, _ := doVia(t, hopClient(proxy), req); st != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (not 502 upstream_refused)", st)
	}
	assertGatewayUnavailableRows(t, store)
}

func TestHop_ProbeSetsReady(t *testing.T) {
	var n atomic.Int32
	gw := newFakeGateway(t, func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	hop := NewHop(gatewayAddr(gw), hopTestToken)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hop.Probe(ctx, 10*time.Millisecond)
	if !hop.Ready() {
		t.Fatal("Ready() = false after a 204")
	}
	got := gw.requests()
	if len(got) != 3 {
		t.Errorf("gateway got %d probes, want 3", len(got))
	}
	for _, r := range got {
		if r.header.Get(headerToken) != hopTestToken || r.header.Get(headerProbe) != "1" || r.header.Get(headerDest) != probeDest {
			t.Errorf("probe headers = %v", r.header)
		}
	}
}

func TestHop_SkipHostsAndUpgradesDoNotHop(t *testing.T) {
	gw := newFakeGateway(t, nil)
	var originHits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHits.Add(1)
		if r.Header.Get("Upgrade") != "" {
			wsEchoHandler(w, r)
		}
	}))
	t.Cleanup(origin.Close)

	skip, err := skiphost.New([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	proxy, _ := hopProxy(t, gatewayAddr(gw), false, func(s *Server) { s.SkipHosts = skip })
	req, _ := http.NewRequest(http.MethodGet, origin.URL+"/skipped", nil)
	if st, _, _ := doVia(t, hopClient(proxy), req); st != http.StatusOK {
		t.Errorf("skipped status = %d, want 200", st)
	}

	proxy2, _ := hopProxy(t, gatewayAddr(gw), false, nil)
	conn, err := net.Dial("tcp", strings.TrimPrefix(proxy2.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	u, _ := url.Parse(origin.URL)
	writeUpgradeRequest(conn, origin.URL+"/ws", u.Host)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("upgrade status = %d, want 101 from the origin", resp.StatusCode)
	}

	if n := len(gw.requests()); n != 0 {
		t.Errorf("gateway got %d requests, want none", n)
	}
	if n := originHits.Load(); n != 2 {
		t.Errorf("origin got %d requests, want 2", n)
	}
}
