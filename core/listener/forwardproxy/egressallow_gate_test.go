package forwardproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/auth"
	"github.com/rossoctl/cortex/core/listener/egressallow"
	"github.com/rossoctl/cortex/core/listener/skiphost"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// The egress allow list (#1483, MOCA MI1 §8.5): when on, an off-list
// destination — CONNECT target or absolute-form http:// host — is refused
// with 403 destination_denied naming the host, in the moca egress
// conformance suite's refusal shape (header X-Moca-Egress-Error, text/plain
// body "destination_denied: <host>\n"), before the pipeline, before DNS and
// before any dial. The in-tree pin for "the upstream was never contacted" is
// the origin's accept counter plus the gate's pre-dial placement; the
// conformance suite's zero-DNS-queries assertion needs its Target adapter for
// cortex (moca main has only conformance/), so upstream-accepts-0 is the
// in-tree equivalent.

// allowHarness is a forward proxy with the egress allow list armed over
// entries, in front of an origin that counts what reaches it.
type allowHarness struct {
	store  *session.Store
	proxy  *httptest.Server
	origin *httptest.Server
	// allow is the harness's matcher, reusable by tests that build their own
	// proxy server over the same entries (skip-hosts, redirect).
	allow *egressallow.Matcher
	// originAddr is the origin's host:port — the on-list spelling.
	originAddr string
	reached    atomic.Int32
}

// newAllowHarness arms the allow list over the origin's own address, so
// requests naming the origin pass and anything else is off-list.
func newAllowHarness(t *testing.T, entries ...string) *allowHarness {
	t.Helper()
	h := &allowHarness{store: session.New(5*time.Minute, 100, 0)}
	t.Cleanup(h.store.Close)
	h.origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h.reached.Add(1)
		_, _ = io.WriteString(w, "origin")
	}))
	t.Cleanup(h.origin.Close)
	h.originAddr = h.origin.Listener.Addr().String()

	if len(entries) == 0 {
		entries = []string{h.originAddr}
	}
	allow, err := egressallow.New(entries)
	if err != nil {
		t.Fatalf("egressallow.New: %v", err)
	}
	h.allow = allow
	srv := &Server{
		OutboundPipeline: outboundPipelineFromAuth(t, auth.New(auth.Config{})),
		Sessions:         h.store,
		Client:           http.DefaultClient,
		EgressAllow:      allow,
	}
	h.proxy = httptest.NewServer(srv.Handler())
	t.Cleanup(h.proxy.Close)
	return h
}

// assertMocaRefusal pins the conformance suite's wantRefusal shape on a
// response: status, X-Moca-Egress-Error, text/plain, and the exact body.
func assertMocaRefusal(t *testing.T, resp *http.Response, host string) {
	t.Helper()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Moca-Egress-Error"); got != "destination_denied" {
		t.Errorf("X-Moca-Egress-Error = %q, want destination_denied", got)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain*", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if want := "destination_denied: " + host + "\n"; string(body) != want {
		t.Errorf("body = %q, want %q", body, want)
	}
}

// deniedRow finds the session's denied-phase row and pins that it carries the
// egress-allowlist invocation — the audit event the conformance suite counts
// one of per refusal (#1482's JSON audit channel reads the same row).
func deniedRow(t *testing.T, h *allowHarness) *pipeline.SessionEvent {
	t.Helper()
	v := h.store.View(session.DefaultSessionID)
	if v == nil {
		t.Fatal("no session recorded for the refusal")
	}
	for i := range v.Events {
		if v.Events[i].Phase == pipeline.SessionDenied {
			return &v.Events[i]
		}
	}
	t.Fatalf("no denied row; events = %+v", v.Events)
	return nil
}

// TestEgressAllow_RefusesOffListCONNECT is the moca off_allowlist_named_403
// shape, CONNECT half: an off-list target is answered 403 by name, the origin
// is never contacted, and the denial leaves its audit row.
func TestEgressAllow_RefusesOffListCONNECT(t *testing.T) {
	h := newAllowHarness(t)
	line, br, conn := connectRaw(t, h.proxy, "off.test:9443")
	defer conn.Close()
	if !strings.Contains(line, "403") {
		t.Fatalf("CONNECT off-list = %q, want 403", line)
	}
	body, _ := io.ReadAll(br)
	if want := "destination_denied: off.test\n"; string(body) != want {
		t.Errorf("body = %q, want %q", body, want)
	}
	if n := h.reached.Load(); n != 0 {
		t.Errorf("origin accepted %d connection(s), want 0 — the refusal must precede the dial", n)
	}
	row := deniedRow(t, h)
	// The row's Host is the authority, as every CONNECT row's is; the
	// host-only name the conformance audit reads is in Details and the body.
	if row.Host != "off.test:9443" {
		t.Errorf("denied row host = %q, want off.test:9443", row.Host)
	}
	if row.Invocations == nil {
		t.Fatal("denied row carries no invocations")
	}
	var inv *pipeline.Invocation
	for i := range row.Invocations.Outbound {
		if row.Invocations.Outbound[i].Plugin == "egress-allowlist" {
			inv = &row.Invocations.Outbound[i]
		}
	}
	if inv == nil {
		t.Fatalf("no egress-allowlist invocation on the denied row: %+v", row.Invocations)
	}
	if inv.Action != pipeline.ActionDeny || inv.Reason != "destination_denied" ||
		inv.Details["host"] != "off.test" {
		t.Errorf("invocation = %+v, want deny/destination_denied naming off.test", inv)
	}
}

// TestEgressAllow_RefusesOffListGET is the absolute-form half: a plain GET to
// an off-list host:port gets the same named 403 and the same audit row, and
// the origin is never contacted.
func TestEgressAllow_RefusesOffListGET(t *testing.T) {
	h := newAllowHarness(t)
	resp, err := proxyClient(h.proxy, nil).Get("http://off.test:9443/x")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	defer resp.Body.Close()
	assertMocaRefusal(t, resp, "off.test")
	if n := h.reached.Load(); n != 0 {
		t.Errorf("origin reached %d time(s), want 0", n)
	}
	if row := deniedRow(t, h); row.Host != "off.test:9443" {
		t.Errorf("denied row host = %q, want off.test:9443", row.Host)
	}
}

// TestEgressAllow_PortlessURLDefaultsTo80: a portless absolute-form URL is
// judged at its scheme's port (http → 80), so it is denied when only another
// port of the host is listed — deny-by-default, not guess-the-port.
func TestEgressAllow_PortlessURLDefaultsTo80(t *testing.T) {
	h := newAllowHarness(t) // lists only originAddr
	resp, err := proxyClient(h.proxy, nil).Get("http://off.test/refused?x=1")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	defer resp.Body.Close()
	assertMocaRefusal(t, resp, "off.test")
}

// TestEgressAllow_OnListFlowsThrough: the permissive half — a CONNECT to the
// listed origin tunnels (200), and a GET naming it is forwarded and answered.
func TestEgressAllow_OnListFlowsThrough(t *testing.T) {
	h := newAllowHarness(t)

	line, _, conn := connectRaw(t, h.proxy, h.originAddr)
	defer conn.Close()
	if !strings.Contains(line, "200") {
		t.Fatalf("CONNECT on-list = %q, want 200", line)
	}

	resp, err := proxyClient(h.proxy, nil).Get("http://" + h.originAddr + "/x")
	if err != nil {
		t.Fatalf("GET on-list through the proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if n := h.reached.Load(); n != 1 {
		t.Errorf("origin reached %d time(s), want 1 (the GET; the CONNECT tunnel carries no HTTP)", n)
	}
}

// TestEgressAllow_NilKeepsBehavior pins the compatibility contract: with no
// allow list, a CONNECT to a dead loopback port is the old dial-failure 502,
// not a 403 — off must mean byte-for-byte unchanged.
func TestEgressAllow_NilKeepsBehavior(t *testing.T) {
	srv := &Server{
		OutboundPipeline: outboundPipelineFromAuth(t, auth.New(auth.Config{})),
		Sessions:         session.New(5*time.Minute, 100, 0),
		Client:           http.DefaultClient,
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()
	line, _, conn := connectRaw(t, proxy, "127.0.0.1:1")
	defer conn.Close()
	if !strings.Contains(line, "502") {
		t.Fatalf("CONNECT with the allow list off = %q, want the dial-failure 502", line)
	}
	if strings.Contains(line, "403") {
		t.Fatalf("CONNECT with the allow list off = %q, must not be a destination_denied 403", line)
	}
}

// TestEgressAllow_WinsOverSkipHosts: the allowlist is the outer boundary — a
// skip_hosts pattern matching the off-list host does not exempt it, because a
// bypass must not become an egress bypass.
func TestEgressAllow_WinsOverSkipHosts(t *testing.T) {
	h := newAllowHarness(t)
	skip, err := skiphost.New([]string{"off.test"})
	if err != nil {
		t.Fatalf("skiphost.New: %v", err)
	}
	srv := &Server{
		OutboundPipeline: outboundPipelineFromAuth(t, auth.New(auth.Config{})),
		Sessions:         h.store,
		Client:           http.DefaultClient,
		EgressAllow:      h.allow,
		SkipHosts:        skip,
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	line, _, conn := connectRaw(t, proxy, "off.test:9443")
	defer conn.Close()
	if !strings.Contains(line, "403") {
		t.Fatalf("CONNECT off-list under a matching skip pattern = %q, want 403 — the allowlist is checked first", line)
	}
	if n := h.reached.Load(); n != 0 {
		t.Errorf("origin accepted %d connection(s), want 0", n)
	}
}

// TestEgressAllow_RefusesOffListRedirect: a plugin's redirect can move the
// dial target after the top gate passed it; deny-by-default means the dial
// target is what counts, so an off-list redirect is refused before the
// exchange. The request row has landed; the denial row follows it.
func TestEgressAllow_RefusesOffListRedirect(t *testing.T) {
	h := newAllowHarness(t)
	away := &redirectPlugin{target: "http://off.test:9443"}
	p, err := pipeline.New([]pipeline.Plugin{away})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         h.store,
		Client:           http.DefaultClient,
		EgressAllow:      h.allow,
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	// The request names the on-list origin; the plugin steers it off-list.
	resp, err := proxyClient(proxy, nil).Get("http://" + h.originAddr + "/x")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	defer resp.Body.Close()
	assertMocaRefusal(t, resp, "off.test")
	if n := h.reached.Load(); n != 0 {
		t.Errorf("origin reached %d time(s), want 0 — the steered request must not dial", n)
	}
	// Both rows: the request row for the on-list host, then the denial.
	v := h.store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) < 2 {
		t.Fatalf("want a request row and a denial row, got %+v", v)
	}
}

// TestEgressAllow_RefusesStrayBracketDestination: the destination is
// client-chosen, so a parser on this boundary must not panic on a hostile
// spelling. A bare "]" authority used to index s[1:0] inside splitPort and
// panic — net/http recovered it, so the proxy stayed up and nothing was
// dialed, but any client could write a stack trace into the log on every
// request and the refusal row never landed. It is now simply not host:port,
// which deny-by-default refuses by name like any other unparseable
// destination — with the audit row that a panic skipped.
func TestEgressAllow_RefusesStrayBracketDestination(t *testing.T) {
	// "[::1" (opened, never closed) is not in this list: net/http rejects
	// that request line with a 400 before the handler runs, so the gate
	// never sees it. Refused either way; only these reach the gate.
	for _, target := range []string{"]:443", "]", "]]:443", "a]b:443"} {
		t.Run(target, func(t *testing.T) {
			h := newAllowHarness(t)
			line, br, conn := connectRaw(t, h.proxy, target)
			defer conn.Close()
			if !strings.Contains(line, "403") {
				t.Fatalf("CONNECT %q = %q, want 403", target, line)
			}
			body, _ := io.ReadAll(br)
			if !strings.HasPrefix(string(body), "destination_denied: ") {
				t.Errorf("body = %q, want a destination_denied refusal", body)
			}
			if n := h.reached.Load(); n != 0 {
				t.Errorf("origin accepted %d connection(s), want 0", n)
			}
			// The row a panic used to cost: a recovered panic unwound
			// before anything was recorded.
			if row := deniedRow(t, h); row.Invocations == nil {
				t.Error("denied row carries no invocations")
			}
		})
	}
}

// TestEgressAllow_StrayBracketGETRefused is the absolute-form half of the
// same hostile input: url.ParseRequestURI accepts "http://]:80/", so it
// reaches the gate too.
func TestEgressAllow_StrayBracketGETRefused(t *testing.T) {
	h := newAllowHarness(t)
	resp, err := proxyClient(h.proxy, nil).Get("http://]:80/x")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if n := h.reached.Load(); n != 0 {
		t.Errorf("origin reached %d time(s), want 0", n)
	}
}
