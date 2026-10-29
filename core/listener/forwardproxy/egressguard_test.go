package forwardproxy

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/auth"
	"github.com/rossoctl/cortex/core/listener/egressguard"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// guardProxy builds a forward proxy with the egress guard armed over the given
// permits, plus the session store its rows land in.
func guardProxy(t *testing.T, permits []string) (*httptest.Server, *session.Store) {
	t.Helper()
	g, err := egressguard.New(permits)
	if err != nil {
		t.Fatalf("egressguard.New: %v", err)
	}
	store := session.New(5*time.Minute, 100, 0)
	srv, err := NewServer(outboundPipelineFromAuth(t, auth.New(auth.Config{})), store, nil, WithEgressGuard(g))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)
	return proxy, store
}

// connectRaw speaks one CONNECT to the proxy over raw TCP and returns the
// response's status line and everything after it still buffered.
func connectRaw(t *testing.T, proxy *httptest.Server, target string) (string, *bufio.Reader, net.Conn) {
	t.Helper()
	proxyAddr := strings.TrimPrefix(proxy.URL, "http://")
	conn, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	if _, err := conn.Write([]byte("CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n\r\n")); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	// Drain the response headers up to the empty line, so a caller reading
	// tunnel bytes off br starts at the first application byte.
	for {
		hdr, herr := br.ReadString('\n')
		if herr != nil {
			t.Fatalf("read CONNECT headers: %v", herr)
		}
		if hdr == "\r\n" || hdr == "\n" {
			break
		}
	}
	_ = conn.SetReadDeadline(time.Time{})
	return line, br, conn
}

// TestForwardProxy_EgressGuardRefusesLoopbackCONNECT is the moca
// private_address_403 shape: a CONNECT to a loopback literal is answered 403
// destination_private by the proxy itself and the upstream is never dialed.
func TestForwardProxy_EgressGuardRefusesLoopbackCONNECT(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
	}))
	defer upstream.Close()
	target := strings.TrimPrefix(upstream.URL, "http://")

	proxy, store := guardProxy(t, nil)
	line, br, conn := connectRaw(t, proxy, target)
	defer conn.Close()

	if !strings.Contains(line, "403") {
		t.Fatalf("CONNECT to loopback = %q, want 403", line)
	}
	body, _ := io.ReadAll(br)
	if !strings.Contains(string(body), "destination_private") {
		t.Errorf("403 body = %q, want the destination_private code", body)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("upstream dialed %d times, want 0 — the refusal must precede the dial", n)
	}

	// The refusal leaves its two rows: an open that names the reason and a
	// 403 close carrying error.kind destination_private.
	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 2 {
		t.Fatalf("want a tunnel open and close row, got %+v", v)
	}
	open, closeRow := v.Events[0], v.Events[1]
	if !open.Tunnel || open.TunnelReason != pipeline.TunnelDestPrivate {
		t.Errorf("open row = tunnel:%v reason:%q, want dest-private", open.Tunnel, open.TunnelReason)
	}
	if closeRow.StatusCode != 403 || closeRow.Error == nil || closeRow.Error.Kind != "destination_private" {
		t.Errorf("close row = status:%d error:%+v, want 403 destination_private", closeRow.StatusCode, closeRow.Error)
	}
}

// TestForwardProxy_EgressGuardRefusesPrivateGET pins the request path: a
// plain GET to a loopback destination is refused by the guarded transport
// dial and answered 403 destination_private, with both rows recorded.
func TestForwardProxy_EgressGuardRefusesPrivateGET(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
	}))
	defer upstream.Close()

	proxy, store := guardProxy(t, nil)
	client := &http.Client{Transport: &http.Transport{Proxy: mustParseProxy(proxy)}}
	resp, err := client.Get(upstream.URL + "/path")
	if err != nil {
		t.Fatalf("GET through the guarded proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if !strings.Contains(string(body), "destination_private") {
		t.Errorf("body = %q, want the destination_private code", body)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("upstream hit %d times, want 0", n)
	}

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 2 {
		t.Fatalf("want a request and a response row, got %+v", v)
	}
	if v.Events[1].StatusCode != 403 || v.Events[1].Error == nil || v.Events[1].Error.Kind != "destination_private" {
		t.Errorf("response row = status:%d error:%+v, want 403 destination_private",
			v.Events[1].StatusCode, v.Events[1].Error)
	}
}

// TestForwardProxy_EgressGuardMetadataEvenIfPermitted is the moca
// metadata_address_403_even_if_permitted shape: 169.254.169.254 stays refused
// under permits that cover it (0.0.0.0/0 no less) — link-local is never
// permit-able.
func TestForwardProxy_EgressGuardMetadataEvenIfPermitted(t *testing.T) {
	proxy, _ := guardProxy(t, []string{"169.254.0.0/16", "0.0.0.0/0"})
	for _, target := range []string{"169.254.169.254:443", "169.254.0.9:80"} {
		line, _, conn := connectRaw(t, proxy, target)
		if !strings.Contains(line, "403") {
			t.Errorf("CONNECT %s with covering permits = %q, want 403 — link-local is never permit-able", target, line)
		}
		_ = conn.Close()
	}
}

// TestForwardProxy_EgressGuardPermitTunnelsAndRecordsDialAddr is the
// permissive half of the moca contract: with 127.0.0.1/32 permitted, a CONNECT
// to a loopback upstream tunnels, and the open row carries dialAddr — the
// audit trail that says the address checked is the address dialed.
func TestForwardProxy_EgressGuardPermitTunnelsAndRecordsDialAddr(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listen unavailable: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 1024)
				for {
					n, rerr := c.Read(buf)
					if rerr != nil {
						return
					}
					if _, werr := c.Write(buf[:n]); werr != nil {
						return
					}
				}
			}()
		}
	}()
	target := ln.Addr().String()

	proxy, store := guardProxy(t, []string{"127.0.0.1/32"})
	line, br, conn := connectRaw(t, proxy, target)
	defer conn.Close()
	if !strings.Contains(line, "200") {
		t.Fatalf("CONNECT to permitted loopback = %q, want 200", line)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write through tunnel: %v", err)
	}
	got := make([]byte, 4)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(br, got); err != nil || string(got) != "ping" {
		t.Fatalf("echo through tunnel = %q (%v), want ping", got, err)
	}
	_ = conn.Close()

	// The close row lands after the tunnel ends; poll for the pair.
	deadline := time.Now().Add(2 * time.Second)
	for {
		v := store.View(session.DefaultSessionID)
		if v != nil && len(v.Events) == 2 {
			if v.Events[0].DialAddr != target {
				t.Errorf("open row dialAddr = %q, want %q", v.Events[0].DialAddr, target)
			}
			if !v.Events[0].Tunnel {
				t.Errorf("open row is not a tunnel row: %+v", v.Events[0])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("want a tunnel open and close row, got %+v", v)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestForwardProxy_EgressGuardOffByDefault pins the compatibility contract:
// with no guard passed, a CONNECT to loopback tunnels exactly as before.
func TestForwardProxy_EgressGuardOffByDefault(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listen unavailable: %v", err)
	}
	defer ln.Close()

	srv, err := NewServer(outboundPipelineFromAuth(t, auth.New(auth.Config{})), nil, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	line, _, conn := connectRaw(t, proxy, ln.Addr().String())
	defer conn.Close()
	if !strings.Contains(line, "200") {
		t.Fatalf("CONNECT with the guard off = %q, want 200 — off must mean byte-for-byte unchanged", line)
	}
}

// The TRANSPARENT listener's refusal must reach Finishers as an allow with 403,
// the same as both CONNECT paths. handleConnect sets pctx.StatusCode before its
// tl.close; this path did not, and OutcomeFromContext reads a zero StatusCode as
// OutcomeError — so the three listener paths disagreed about the same refusal,
// this one reporting a failure for what the docs call a delivered 403, and
// lineage labelling a deliberate answer a failure (#1539 review).
//
// No status reaches the client here: a transparently redirected connection
// speaks no HTTP to the agent, so the field exists for the Finishers and the
// row, which is exactly why nothing else noticed it was missing.
func TestTransparent_EgressGuardRefusalReportsAnAllowToFinishers(t *testing.T) {
	g, err := egressguard.New(nil)
	if err != nil {
		t.Fatalf("egressguard.New: %v", err)
	}
	// Something to be refused: a loopback literal, which is what SO_ORIGINAL_DST
	// hands this path for a redirected connection to the host itself.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listen unavailable: %v", err)
	}
	defer func() { _ = ln.Close() }()

	fin := &outcomeRecorder{}
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	p, err := pipeline.New([]pipeline.Plugin{fin})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		EgressGuard:      g,
	}

	agentSide, proxySide := net.Pipe()
	go srv.HandleTransparentConn(proxySide, ln.Addr().String())
	defer func() { _ = agentSide.Close() }()

	// OnFinish runs on the handler's goroutine, after the refusal.
	var got *pipeline.Outcome
	for i := 0; i < 200; i++ {
		if got = fin.seen(); got != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got == nil {
		t.Fatal("OnFinish never ran, so the outcome is unasserted")
	}
	if got.FinalAction != pipeline.OutcomeAllow || got.StatusCode != http.StatusForbidden {
		t.Errorf("outcome = %v/%d, want allow/403 — the same shape handleConnect reports",
			got.FinalAction, got.StatusCode)
	}
}

func mustParseProxy(proxy *httptest.Server) func(*http.Request) (*url.URL, error) {
	return func(*http.Request) (*url.URL, error) { return mustParseURL(proxy.URL), nil }
}
