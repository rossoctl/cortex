package forwardproxy

import (
	"bufio"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/plugintesting"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/tlsbridge"
)

func TestTunnelAuthorityMatches(t *testing.T) {
	cases := []struct {
		host, tunnel string
		want         bool
	}{
		{"support.example", "support.example:443", true},
		{"support.example:443", "support.example:443", true},
		{"support.example:443", "support.example", true},
		{"Support.Example.", "support.example:443", true},
		{"support.example.:443", "SUPPORT.EXAMPLE:443", true},
		{"support.example:", "support.example:443", true},
		{"support.example:8443", "support.example:8443", true},
		{"[::1]", "[::1]:443", true},
		{"[::1]:443", "[0:0::1]:443", true},
		{"[::1]:8443", "[::1]:8443", true},
		{"127.0.0.1:9000", "127.0.0.1:9000", true},

		{"support.example", "evil.example:443", false},
		{"support.example", "support.example:8443", false},
		{"support.example:8443", "support.example:443", false},
		{"support.example:0443x", "support.example:443", false},
		{"support.example:70000", "support.example:443", false},
		{"[::1]", "[::2]:443", false},
		{"[::1]:8443", "[::1]:443", false},
		{"127.0.0.1", "127.0.0.2:443", false},
		{"support.example.evil.example", "support.example:443", false},
		{"", "support.example:443", false},
	}
	for _, c := range cases {
		if got := tunnelAuthorityMatches(c.host, c.tunnel); got != c.want {
			t.Errorf("tunnelAuthorityMatches(%q, %q) = %v, want %v", c.host, c.tunnel, got, c.want)
		}
	}
}

// misdirectScene is a bridging proxy in front of a TLS origin that counts the
// requests it is sent, the bridge's own HEAD probe aside.
type misdirectScene struct {
	proxyAddr string
	target    string // the origin's host:port, as the CONNECT names it
	ca        []byte
	done      <-chan struct{}
	store     *session.Store
	served    *atomic.Int32
	probe     *hostRecorder
}

func newMisdirectScene(t *testing.T, connectHost string) *misdirectScene {
	t.Helper()
	served := &atomic.Int32{}
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			served.Add(1)
		}
		_, _ = w.Write([]byte("ok"))
	}))
	origin.Config.ErrorLog = log.New(io.Discard, "", 0)
	origin.StartTLS()
	t.Cleanup(origin.Close)
	u, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatalf("parse origin URL: %v", err)
	}
	_, port, _ := net.SplitHostPort(u.Host)
	target := net.JoinHostPort(connectHost, port)

	engine := bridgeEngine(t, portOf(u.Host), nil)
	// The origin's certificate names 127.0.0.1 and example.com, not every name these
	// tests CONNECT to; what is under test is the Host check, not upstream trust.
	insecure, err := tlsbridge.NewUpstreamClient(nil, true)
	if err != nil {
		t.Fatalf("NewUpstreamClient: %v", err)
	}
	engine.Upstream = insecure

	store := session.New(5*time.Minute, 100, 0)
	t.Cleanup(store.Close)
	probe := &hostRecorder{}
	p, err := plugintesting.BuildPipeline([]pipeline.Plugin{probe})
	if err != nil {
		t.Fatalf("BuildPipeline: %v", err)
	}
	s := connectServer(t, store, engine)
	s.OutboundPipeline = pipeline.NewHolder(p)
	proxyAddr, done := connectProxy(t, s)
	return &misdirectScene{proxyAddr: proxyAddr, target: target, ca: engine.CAPEM, done: done, store: store, served: served, probe: probe}
}

// send opens a bridged tunnel to the scene's target and sends one request whose Host
// header is host, returning the response and its body.
func (sc *misdirectScene) send(t *testing.T, host string) (*http.Response, string) {
	t.Helper()
	raw, br, cresp := sendConnect(t, sc.proxyAddr, sc.target)
	if cresp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status %d", cresp.StatusCode)
	}
	tc := bridgedTLS(t, raw, br, sc.target, sc.ca)
	req, _ := http.NewRequest(http.MethodGet, "https://"+sc.target+"/upload?token=secret", nil)
	req.Host = host
	go func() { _ = req.Write(tc) }()
	resp, err := http.ReadResponse(bufio.NewReader(tc), req)
	if err != nil {
		t.Fatalf("bridged request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	_ = tc.Close()
	waitDone(t, sc.done)
	return resp, string(body)
}

func TestBridged_MatchingHostIsForwarded(t *testing.T) {
	for _, tc := range []struct{ name, connect, host string }{
		{"exact", "127.0.0.1", ""},
		{"case and trailing dot", "localhost", "LocalHost."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := newMisdirectScene(t, tc.connect)
			host := sc.target
			if tc.host != "" {
				_, port, _ := net.SplitHostPort(sc.target)
				host = tc.host + ":" + port
			}
			resp, body := sc.send(t, host)
			if resp.StatusCode != http.StatusOK || body != "ok" {
				t.Fatalf("status %d body %q, want 200 ok", resp.StatusCode, body)
			}
			if n := sc.served.Load(); n != 1 {
				t.Errorf("origin served %d request(s), want 1", n)
			}
			if n := sc.probe.decrypted.Load(); n != 1 {
				t.Errorf("pipeline ran on %d decrypted request(s), want 1", n)
			}
		})
	}
}

func TestBridged_MismatchedHostIsRefused(t *testing.T) {
	sc := newMisdirectScene(t, "127.0.0.1")
	_, port, _ := net.SplitHostPort(sc.target)
	for _, host := range []string{
		"support.example",         // another name, default port
		"support.example:" + port, // another name, the tunnel's port
		"127.0.0.1",               // the tunnel's host, default port 443
		"127.0.0.1:1",             // the tunnel's host, another port
		"[::1]:" + port,           // another address family
	} {
		t.Run(host, func(t *testing.T) {
			before := sc.probe.decrypted.Load()
			resp, body := sc.send(t, host)
			if resp.StatusCode != http.StatusMisdirectedRequest {
				t.Fatalf("status %d, want 421", resp.StatusCode)
			}
			if strings.Contains(body, host) {
				t.Errorf("421 body %q echoes the client's Host", body)
			}
			if n := sc.served.Load(); n != 0 {
				t.Errorf("origin served %d request(s); a misdirected request must not be forwarded", n)
			}
			if sc.probe.decrypted.Load() != before {
				t.Error("the pipeline ran on a misdirected request")
			}
		})
	}
}

func TestBridged_MismatchedHostIsRecorded(t *testing.T) {
	sc := newMisdirectScene(t, "127.0.0.1")
	sc.send(t, "support.example")

	var req, resp *pipeline.SessionEvent
	for _, e := range allEvents(t, sc.store) {
		if e.Tunnel {
			continue
		}
		e := e
		switch e.Phase {
		case pipeline.SessionRequest:
			req = &e
		case pipeline.SessionResponse:
			resp = &e
		}
	}
	if req == nil || resp == nil {
		t.Fatalf("want a request and a response row; request %v response %v", req != nil, resp != nil)
	}
	for _, e := range []*pipeline.SessionEvent{req, resp} {
		if e.Host != sc.target {
			t.Errorf("%s row host %q, want the tunnel's %q", e.Phase, e.Host, sc.target)
		}
		if e.HTTPPath != "/upload" {
			t.Errorf("%s row path %q, want /upload", e.Phase, e.HTTPPath)
		}
	}
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("response row status %d, want 421", resp.StatusCode)
	}
	if resp.Error == nil || resp.Error.Kind != "misdirected_request" {
		t.Fatalf("response row error %+v, want kind misdirected_request", resp.Error)
	}
	if !strings.Contains(resp.Error.Message, "support.example") {
		t.Errorf("error message %q does not name the Host the client sent", resp.Error.Message)
	}
	if strings.Contains(resp.Error.Message, "secret") {
		t.Errorf("error message %q carries the query string", resp.Error.Message)
	}
}

// A plaintext request in absolute form names its destination in the request line, and
// net/http makes that r.Host and ignores the Host header (RFC 9112 §3.2.2), so the host
// the pipeline judges is the host the proxy dials. Pinned so a change that started
// reading the header would fail here.
func TestPlaintext_AbsoluteFormJudgesTheRequestLineHost(t *testing.T) {
	var gotHost atomic.Value
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost.Store(r.Host)
		_, _ = w.Write([]byte("ok"))
	}))
	defer origin.Close()
	target := strings.TrimPrefix(origin.URL, "http://")

	seen := &hostRecorder{}
	p, err := plugintesting.BuildPipeline([]pipeline.Plugin{seen})
	if err != nil {
		t.Fatalf("BuildPipeline: %v", err)
	}
	s := &Server{OutboundPipeline: pipeline.NewHolder(p), Client: http.DefaultClient}
	proxyAddr, done := connectProxy(t, s)

	raw, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = raw.Close() }()
	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = io.WriteString(raw, "GET http://"+target+"/x HTTP/1.1\r\nHost: support.example\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(raw), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	_ = resp.Body.Close()
	waitDone(t, done)

	if seen.host != target {
		t.Errorf("pipeline judged host %q, want the request line's %q", seen.host, target)
	}
	if h, _ := gotHost.Load().(string); h != target {
		t.Errorf("origin received Host %q, want %q", h, target)
	}
}

// hostRecorder records the host the pipeline judged and counts the requests it ran on,
// CONNECTs aside.
type hostRecorder struct {
	host      string
	decrypted atomic.Int32
}

func (*hostRecorder) Name() string                              { return "host-recorder" }
func (*hostRecorder) Capabilities() pipeline.PluginCapabilities { return pipeline.PluginCapabilities{} }
func (h *hostRecorder) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if pctx.Method != http.MethodConnect {
		h.host = pctx.Host
		h.decrypted.Add(1)
	}
	return pipeline.Action{Type: pipeline.Continue}
}
func (*hostRecorder) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
