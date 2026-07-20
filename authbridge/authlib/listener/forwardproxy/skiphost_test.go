package forwardproxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/listener/skiphost"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins/plugintesting"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
)

// markerPlugin records one Invocation per OnRequest call. Tests use it to
// assert whether the pipeline ran on a given request — if SkipHosts
// short-circuits correctly, calls counts AND session events stay at zero.
type markerPlugin struct {
	calls atomic.Int32
}

func (p *markerPlugin) Name() string                                  { return "marker" }
func (p *markerPlugin) Capabilities() pipeline.PluginCapabilities     { return pipeline.PluginCapabilities{} }
func (p *markerPlugin) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

func (p *markerPlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	p.calls.Add(1)
	pctx.Record(pipeline.Invocation{
		Plugin: "marker",
		Action: pipeline.ActionObserve,
		Phase:  pipeline.InvocationPhaseRequest,
		Reason: "ran",
	})
	return pipeline.Action{Type: pipeline.Continue}
}

func newMarkerServer(t *testing.T, store *session.Store, skip *skiphost.Matcher) (*httptest.Server, *markerPlugin) {
	t.Helper()
	mp := &markerPlugin{}
	pp, err := plugintesting.BuildPipeline([]pipeline.Plugin{mp})
	if err != nil {
		t.Fatalf("build pipeline: %v", err)
	}
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(pp),
		Sessions:         store,
		Client:           http.DefaultClient,
		SkipHosts:        skip,
	}
	return httptest.NewServer(srv.Handler()), mp
}

// TestForwardProxy_SkipHosts_BypassesPipeline asserts the core property:
// a host matching SkipHosts produces NO plugin invocations and NO session
// events, while a non-matching host on the same proxy still runs both.
// This is the only behavioral guarantee that prevents OTel-style chatty
// infrastructure traffic from evicting the inbound A2A user intent out
// of the session buffer's FIFO eviction window.
func TestForwardProxy_SkipHosts_BypassesPipeline(t *testing.T) {
	upstreamHits := atomic.Int32{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream-ok"))
	}))
	defer backend.Close()

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	// Match the loopback IP that httptest backends listen on. A pattern
	// like "127.0.0.1" works because skiphost strips the port before
	// matching. We deliberately do NOT use a glob so the test is brittle
	// to behavior, not to glob semantics (those are exercised in the
	// skiphost package's own tests).
	skip, err := skiphost.New([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("skiphost.New: %v", err)
	}

	proxy, mp := newMarkerServer(t, store, skip)
	defer proxy.Close()

	proxyClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(proxy.URL)),
		},
	}

	// Skip path: backend URL is on 127.0.0.1, so it should match the
	// skip pattern. Pipeline must not run; session must remain empty.
	resp, err := proxyClient.Get(backend.URL + "/skip-me")
	if err != nil {
		t.Fatalf("skip request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("skip path: status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "upstream-ok" {
		t.Errorf("skip path: body = %q, want upstream-ok (transparent forward must still deliver upstream bytes)", string(body))
	}
	if upstreamHits.Load() != 1 {
		t.Errorf("skip path: upstream hit count = %d, want 1 (skip must not block the request)", upstreamHits.Load())
	}
	if mp.calls.Load() != 0 {
		t.Errorf("skip path: pipeline plugin ran %d times, want 0 (SkipHosts must short-circuit before pipeline.Run)", mp.calls.Load())
	}
	if sessions := store.ListSessions(); len(sessions) != 0 {
		t.Errorf("skip path: %d session(s) recorded, want 0 (SkipHosts must skip recording, otherwise OTel-style traffic still evicts the A2A intent)", len(sessions))
	}
}

// TestForwardProxy_SkipHosts_NonMatchingRunsPipeline is the regression
// guard: a Server with a SkipHosts list set must still run the pipeline
// + record events for hosts that DO NOT match the list. Without this
// pairing the skip test above could pass trivially with a globally
// disabled pipeline.
func TestForwardProxy_SkipHosts_NonMatchingRunsPipeline(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	skip, err := skiphost.New([]string{"otel-collector*"})
	if err != nil {
		t.Fatalf("skiphost.New: %v", err)
	}

	proxy, mp := newMarkerServer(t, store, skip)
	defer proxy.Close()

	proxyClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(proxy.URL)),
		},
	}
	resp, err := proxyClient.Get(backend.URL + "/run-me")
	if err != nil {
		t.Fatalf("non-skip request failed: %v", err)
	}
	resp.Body.Close()

	if mp.calls.Load() != 1 {
		t.Errorf("non-skip path: pipeline plugin ran %d times, want 1 (host did not match skip list)", mp.calls.Load())
	}
	// The marker plugin recorded an Invocation, so a session event
	// should land. Bucket is DefaultSessionID since no inbound primed
	// an active session for this proxy instance.
	if sessions := store.ListSessions(); len(sessions) != 1 {
		t.Errorf("non-skip path: session count = %d, want 1 (Invocation should drive event recording)", len(sessions))
	}
}

// TestForwardProxy_SkipHosts_NilMatcherPreservesBehavior asserts the
// zero-value default: a Server constructed without SkipHosts (nil
// Matcher) behaves like today's code — pipeline runs, sessions record.
// This is the upgrade-safety contract for existing deployments that
// don't opt into skip_hosts.
func TestForwardProxy_SkipHosts_NilMatcherPreservesBehavior(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	proxy, mp := newMarkerServer(t, store, nil) // SkipHosts: nil
	defer proxy.Close()

	proxyClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(proxy.URL)),
		},
	}
	resp, err := proxyClient.Get(backend.URL + "/")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()

	if mp.calls.Load() != 1 {
		t.Errorf("nil SkipHosts: pipeline ran %d times, want 1", mp.calls.Load())
	}
}

// TestForwardProxy_SkipHosts_MatchesAgainstHostHeaderNotURL locks the
// trust-model contract for the HTTP forward path: the skip match keys
// on the request `Host` header (`r.Host`), not on the dial target
// derived from `r.URL`. This is what huang195's review called out as
// the agent-influenceable input — the test exists to make the behavior
// observable so future maintainers see it when reading the test, and
// so a refactor that flips this boundary (e.g. switching to r.URL.Host)
// breaks loudly with a named test.
//
// Operators reading this: do NOT add a destination to skip_hosts that
// you'd want IBAC / token-exchange to deny on. The skip is keyed on
// agent-influenceable headers; the audit log emitted on each skip is
// the only after-the-fact signal.
//
// The test scopes itself to the trust contract — "skip fires when
// r.Host matches" — and does NOT assert what happens to the forwarded
// request afterward. Go's http.Transport behavior with a proxy and a
// forged Host header is implementation-detail that the trust model
// shouldn't depend on; what matters is that the skip decision was
// made on the agent-supplied value.
func TestForwardProxy_SkipHosts_MatchesAgainstHostHeaderNotURL(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	// Skip pattern matches a hostname that is NOT the dial target.
	// The dial target is the backend (127.0.0.1:N from backend.URL);
	// the pattern is a fictitious hostname.
	skip, err := skiphost.New([]string{"infrastructure-only.example"})
	if err != nil {
		t.Fatalf("skiphost.New: %v", err)
	}

	proxy, mp := newMarkerServer(t, store, skip)
	defer proxy.Close()

	// Send a request whose Host header is the skip-listed pattern
	// while r.URL points at the real backend. A normal Go http.Client
	// would set Host from URL; we override it explicitly to model the
	// adversarial case.
	proxyClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(proxy.URL)),
		},
	}
	req, err := http.NewRequest("GET", backend.URL+"/test", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Host = "infrastructure-only.example" // forged Host header

	// Don't assert on resp/err — the trust-model claim is solely
	// about the skip decision. Whatever Go's transport ends up doing
	// with the divergent Host vs URL is incidental.
	if resp, err := proxyClient.Do(req); err == nil && resp != nil {
		resp.Body.Close()
	}

	// The skip matched against the forged Host, so the pipeline
	// did NOT run. That's the trust-contract assertion: the agent
	// chose the skip-match value via the Host header, not via the
	// real dial target.
	if mp.calls.Load() != 0 {
		t.Errorf("forged Host: pipeline ran %d times, want 0 — skip matched on r.Host (the agent-supplied Host header), not r.URL.Host", mp.calls.Load())
	}
	if sessions := store.ListSessions(); len(sessions) != 0 {
		t.Errorf("forged Host: %d session(s) recorded, want 0 — skip path bypasses recording", len(sessions))
	}
}

// TestForwardProxy_SkipHosts_CONNECT_BypassesPipeline asserts that the
// CONNECT-tunnel path honors SkipHosts. CONNECT is safer-by-construction
// than the HTTP path because r.Host IS the dial target, but skip
// behavior must still be exercised so a regression that disables the
// CONNECT skip path is caught.
func TestForwardProxy_SkipHosts_CONNECT_BypassesPipeline(t *testing.T) {
	// Minimal echo server — stand-in for an HTTPS upstream we don't
	// want to do TLS to in tests. The skip path opens a tunnel and
	// shuttles bytes; we just need to prove bytes flow.
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen upstream: %v", err)
	}
	defer upstream.Close()
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 64)
		n, _ := conn.Read(buf)
		_, _ = conn.Write(append([]byte("echo:"), buf[:n]...))
	}()

	upstreamAddr := upstream.Addr().String()
	upstreamHost, _, _ := net.SplitHostPort(upstreamAddr)
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	skip, err := skiphost.New([]string{upstreamHost})
	if err != nil {
		t.Fatalf("skiphost.New: %v", err)
	}

	proxy, mp := newMarkerServer(t, store, skip)
	defer proxy.Close()

	// Drive CONNECT manually so we can observe the 200 + tunnel.
	proxyAddr := strings.TrimPrefix(proxy.URL, "http://")
	tunnel, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer tunnel.Close()
	if _, err := tunnel.Write([]byte("CONNECT " + upstreamAddr + " HTTP/1.1\r\nHost: " + upstreamAddr + "\r\n\r\n")); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	br := bufio.NewReader(tunnel)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if !strings.Contains(line, "200") {
		t.Fatalf("CONNECT response = %q, want 200 (skip path must still establish the tunnel)", line)
	}
	// Drain headers.
	for {
		hdr, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		if hdr == "\r\n" || hdr == "\n" {
			break
		}
	}

	// Tunnel up — write some bytes and confirm the echo round-trips.
	if _, err := tunnel.Write([]byte("hello")); err != nil {
		t.Fatalf("write through tunnel: %v", err)
	}
	got := make([]byte, 32)
	_ = tunnel.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := br.Read(got)
	if err != nil && err != io.EOF {
		t.Fatalf("read tunnel: %v", err)
	}
	if string(got[:n]) != "echo:hello" {
		t.Errorf("tunnel echo = %q, want echo:hello", got[:n])
	}

	// The skipped CONNECT must NOT have run the outbound pipeline
	// and must NOT have appended a session event.
	if mp.calls.Load() != 0 {
		t.Errorf("skipped CONNECT: pipeline ran %d times, want 0", mp.calls.Load())
	}
	if sessions := store.ListSessions(); len(sessions) != 0 {
		t.Errorf("skipped CONNECT: %d session(s) recorded, want 0", len(sessions))
	}
}

