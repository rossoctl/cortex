package vmcontrol

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/listener/forwardproxy"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// These are the in-tree stand-ins for moca's egress conformance suite
// (remote-worker/egress/conformance), which cannot be imported while moca's
// modules are unpublished. The three done-when cases live here:
// identity_from_connection, unregister_aborts_inflight, grant_refused_under_single.
//
// They drive the Registry directly, with TCP listeners standing in for the
// handed-over Unix ones: everything under test — identity from the
// connection, abort semantics, audit shape — is listener-type-agnostic, and
// TCP keeps the tests runnable where Unix binds are unavailable. The
// SCM_RIGHTS handover itself is covered by server_test.go.

// sessionProbe records the session identity the pipeline handed a plugin —
// the side every session-aware plugin keys on, as distinct from the store.
type sessionProbe struct {
	mu      sync.Mutex
	ids     []string
	sources []pipeline.SessionSource
	headers []string
}

func (p *sessionProbe) Name() string { return "session-probe" }
func (p *sessionProbe) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (p *sessionProbe) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pctx.Session == nil {
		p.ids = append(p.ids, "")
		p.sources = append(p.sources, "")
		p.headers = append(p.headers, "")
	} else {
		p.ids = append(p.ids, pctx.Session.ID)
		p.sources = append(p.sources, pctx.SessionSource)
		p.headers = append(p.headers, pctx.SessionHeader)
	}
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *sessionProbe) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// captureSink collects audit events for assertions.
type captureSink struct {
	mu     sync.Mutex
	events []Event
}

func (s *captureSink) Emit(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func (s *captureSink) forVM(vm string) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Event
	for _, e := range s.events {
		if e.VMID == vm {
			out = append(out, e)
		}
	}
	return out
}

// newVMEgressStack wires the real handler stack a VM's traffic crosses: a
// forward proxy with header bucketing on (so a forged header would otherwise
// win) and the trusted-session hook set the way cmd/cortex sets it.
func newVMEgressStack(t *testing.T) (*Registry, *captureSink, *session.Store, *sessionProbe) {
	t.Helper()
	store := session.New(5*time.Minute, 100, 0)
	t.Cleanup(store.Close)
	probe := &sessionProbe{}
	p, err := pipeline.New([]pipeline.Plugin{probe})
	if err != nil {
		t.Fatal(err)
	}
	fp := &forwardproxy.Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Client:           http.DefaultClient,
		// Forged-header coverage needs the headers to matter when not trusted.
		SessionIDHeaders: []string{session.ClaudeCodeSessionHeader, session.SessionIDHeader},
		TrustedSessionID: SessionIDFromRequest,
	}
	sink := &captureSink{}
	return NewRegistry(fp, sink), sink, store, probe
}

// registerTCPVM binds a listener and hands it to the registry as a VM's
// egress socket, returning the address its guests dial.
func registerTCPVM(t *testing.T, reg *Registry, vm, sid string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	if err := reg.Register(vm, sid, nil, ln); err != nil {
		t.Fatal(err)
	}
	return ln.Addr().String()
}

// guest dials a VM's socket the way a microVM's egress does.
func guest(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// guestRequest writes one absolute-form GET through a VM socket without
// reading the response.
func guestRequest(conn net.Conn, target string) {
	host := target
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	_, _ = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, host)
}

// guestGet sends one absolute-form GET through a VM socket and returns the
// response, leaving the connection open for the caller to close.
func guestGet(t *testing.T, conn net.Conn, target string, headers map[string]string) *http.Response {
	t.Helper()
	host := target
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\nHost: %s\r\n", target, host)
	for k, v := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	b.WriteString("\r\n")
	if _, err := conn.Write([]byte(b.String())); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// guestConnect opens an opaque CONNECT tunnel through a VM socket and returns
// the established connection plus its response.
func guestConnect(t *testing.T, conn net.Conn, target string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodConnect, "http://"+target, nil)
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// echoTCPOrigin is a TCP server echoing every byte, the far end of an opaque
// tunnel.
func echoTCPOrigin(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

// identity_from_connection: two VMs, two sids; every audit event and every
// recorded row names the registered vm_id/sid and no other, and nothing the
// guest sent — session headers, X-Moca-*, Proxy-Authorization — moves a
// request to another session or reaches the upstream.
func TestRegistryIdentityFromConnection(t *testing.T) {
	reg, sink, store, probe := newVMEgressStack(t)
	vmA := registerTCPVM(t, reg, "vm-a", "sid-a")
	vmB := registerTCPVM(t, reg, "vm-b", "sid-b")

	var backendMu sync.Mutex
	var backendSaw http.Header
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendMu.Lock()
		backendSaw = r.Header.Clone()
		backendMu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	target := backend.URL + "/from-vm"

	for _, vm := range []struct{ addr, sid, wantVM string }{
		{vmA, "sid-a", "vm-a"},
		{vmB, "sid-b", "vm-b"},
	} {
		conn := guest(t, vm.addr)
		resp := guestGet(t, conn, target, map[string]string{
			"X-Moca-Session":                "sid-b",
			"X-Moca-Vm-Id":                  "vm-b",
			"X-Moca-Anything":               "x",
			"Proxy-Authorization":           "Basic Zm9yZ2Vk",
			session.ClaudeCodeSessionHeader: "forged-cc",
			session.SessionIDHeader:         "forged-x",
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", vm.wantVM, resp.StatusCode)
		}
		_ = resp.Body.Close()
		_ = conn.Close()

		events := sink.forVM(vm.wantVM)
		if len(events) == 0 {
			t.Fatalf("%s: no audit event", vm.wantVM)
		}
		e := events[len(events)-1]
		if e.SID != vm.sid || e.Decision != DecisionForward || e.Status != http.StatusOK {
			t.Errorf("%s: audit = %+v, want sid %s forward 200", vm.wantVM, e, vm.sid)
		}
		if e.Method != http.MethodGet || e.Path != "/from-vm" {
			t.Errorf("%s: audit = %+v, want GET /from-vm", vm.wantVM, e)
		}
	}

	// The pipeline heard the registered sid, resolved from the connection.
	probe.mu.Lock()
	for i, id := range probe.ids {
		want := "sid-a"
		if i > 0 {
			want = "sid-b"
		}
		if id != want || probe.sources[i] != pipeline.SessionSourceConnection || probe.headers[i] != "" {
			t.Errorf("plugin saw session %q via %q/%q, want %q via connection",
				id, probe.sources[i], probe.headers[i], want)
		}
	}
	probe.mu.Unlock()

	// Rows are filed under the registered sids, and under nothing the guest named.
	for _, sid := range []string{"sid-a", "sid-b"} {
		if v := store.View(sid); v == nil || len(v.Events) == 0 {
			t.Errorf("session %s has no recorded rows", sid)
		}
	}
	for _, forged := range []string{"forged-cc", "forged-x", "sid-b", "default"} {
		if sid := forged; sid != "sid-b" && store.View(sid) != nil {
			t.Errorf("forged session %q has rows", forged)
		}
	}

	// The upstream saw none of the guest's identity assertions.
	backendMu.Lock()
	for _, name := range []string{
		"X-Moca-Session", "X-Moca-Vm-Id", "X-Moca-Anything",
		"Proxy-Authorization", session.ClaudeCodeSessionHeader, session.SessionIDHeader,
	} {
		if _, ok := backendSaw[http.CanonicalHeaderKey(name)]; ok {
			t.Errorf("upstream saw %s", name)
		}
	}
	backendMu.Unlock()
}

// A CONNECT tunnel through a VM records one terminal tunnel line with the
// CONNECT's own status, and the bytes cross.
func TestRegistryTunnelAudit(t *testing.T) {
	reg, sink, _, _ := newVMEgressStack(t)
	vmB := registerTCPVM(t, reg, "vm-b", "sid-b")
	origin := echoTCPOrigin(t)

	conn := guest(t, vmB)
	resp := guestConnect(t, conn, origin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status %d", resp.StatusCode)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("tunnel echo: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("tunnel echoed %q", buf)
	}
	_ = conn.Close()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if evs := sink.forVM("vm-b"); len(evs) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	events := sink.forVM("vm-b")
	if len(events) != 1 {
		t.Fatalf("audit events = %+v, want exactly one tunnel line", events)
	}
	e := events[0]
	if e.Decision != DecisionTunnel || e.Method != http.MethodConnect || e.Status != http.StatusOK {
		t.Errorf("tunnel audit = %+v, want CONNECT tunnel 200", e)
	}
	if e.Path != "" {
		t.Errorf("tunnel audit path = %q, want none (opaque bytes carry no request line)", e.Path)
	}
}

// unregister_aborts_inflight: Unregister closes the VM's listener and every
// live connection — an opaque tunnel and a slow in-flight GET, hijacked
// tunnels included — before returning; each live request emits exactly one
// aborted line, no tunnel/forward line is emitted for the VM, and the other
// VM's tunnel is untouched.
func TestRegistryUnregisterAbortsInflight(t *testing.T) {
	reg, sink, _, _ := newVMEgressStack(t)
	vmA := registerTCPVM(t, reg, "vm-a", "sid-a")
	vmB := registerTCPVM(t, reg, "vm-b", "sid-b")
	origin := echoTCPOrigin(t)

	// A live tunnel on vm-a, established and proven.
	tunnelA := guest(t, vmA)
	if resp := guestConnect(t, tunnelA, origin); resp.StatusCode != http.StatusOK {
		t.Fatalf("vm-a CONNECT status %d", resp.StatusCode)
	}
	if _, err := tunnelA.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	ping := make([]byte, 4)
	if _, err := io.ReadFull(tunnelA, ping); err != nil {
		t.Fatalf("vm-a tunnel echo: %v", err)
	}

	// A slow in-flight GET on vm-a: the backend holds the response until the
	// test releases it, so the request is provably inside the proxy when
	// Unregister runs.
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		arrived <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)
	slowA := guest(t, vmA)
	go func() {
		// The response is expected to die with the connection when vm-a is
		// unregistered; only the request needs to get in, so no failure is
		// reported from in here.
		guestRequest(slowA, backend.URL+"/slow")
		resp, err := http.ReadResponse(bufio.NewReader(slowA), nil)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("slow request never reached the backend")
	}

	// A live tunnel on vm-b, which must survive vm-a's Unregister.
	tunnelB := guest(t, vmB)
	if resp := guestConnect(t, tunnelB, origin); resp.StatusCode != http.StatusOK {
		t.Fatalf("vm-b CONNECT status %d", resp.StatusCode)
	}

	if err := reg.Unregister("vm-a"); err != nil {
		t.Fatal(err)
	}
	close(release)

	// vm-a's connections are closed.
	if _, err := tunnelA.Read(make([]byte, 1)); err == nil {
		t.Error("vm-a tunnel still readable after Unregister")
	}
	if _, err := slowA.Read(make([]byte, 1)); err == nil {
		t.Error("vm-a slow request still readable after Unregister")
	}
	// The VM's listener refuses new guests.
	if c, err := net.Dial("tcp", vmA); err == nil {
		_ = c.Close()
		t.Error("vm-a socket still accepts after Unregister")
	}

	// Exactly one aborted line per live request, and nothing else, for vm-a.
	events := sink.forVM("vm-a")
	methods := map[string]bool{}
	for _, e := range events {
		if e.Decision != DecisionAborted {
			t.Errorf("vm-a audit = %+v, want only aborted lines", e)
		}
		methods[e.Method] = true
	}
	if len(events) != 2 || !methods[http.MethodConnect] || !methods[http.MethodGet] {
		t.Errorf("vm-a audit = %+v, want one aborted CONNECT and one aborted GET", events)
	}

	// vm-b's tunnel still carries bytes.
	if _, err := tunnelB.Write([]byte("pong")); err != nil {
		t.Fatalf("vm-b tunnel write after vm-a Unregister: %v", err)
	}
	pong := make([]byte, 4)
	if _, err := io.ReadFull(tunnelB, pong); err != nil || string(pong) != "pong" {
		t.Fatalf("vm-b tunnel echo after vm-a Unregister: %q %v", pong, err)
	}
}

// grant_refused_under_single: a Register carrying a grant is refused with
// bad_request and binds nothing; the same vm_id registers cleanly after.
func TestRegistryGrantRefusedUnderSingle(t *testing.T) {
	reg, _, _, _ := newVMEgressStack(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	err = reg.Register("vm-g", "sid-g", []byte("a-grant"), ln)
	if err == nil {
		t.Fatal("grant accepted under single tenancy")
	}
	re, ok := err.(*RefusalError)
	if !ok || re.Code != CodeBadRequest {
		t.Fatalf("grant refusal = %v, want bad_request", err)
	}
	if want := "grants are not supported in this slice (MI1 S2)"; re.Message != want {
		t.Errorf("grant refusal message = %q, want %q", re.Message, want)
	}
	// Nothing was bound: Unregister of the refused VM is a no-op, and the
	// socket the refused fd served is closed (the server's contract for a
	// refused Register; the Registry never adopted it).
	if err := reg.Unregister("vm-g"); err != nil {
		t.Errorf("unregister of refused vm: %v", err)
	}

	// The same vm_id registers cleanly once the grant is gone.
	if err := reg.Register("vm-g", "sid-g", nil, ln); err != nil {
		t.Fatalf("clean register after refusal: %v", err)
	}
}

// The audit line is moca's JSON shape: same keys, via forced, path without a
// query string.
func TestAuditEventJSONShape(t *testing.T) {
	var buf syncBuffer
	sink := NewJSONSink(&buf)
	sink.Emit(Event{
		VMID: "vm-a", SID: "sid-a",
		Host: "example.com", Method: http.MethodGet,
		Path: "/x", Status: 200, Decision: DecisionForward,
	})

	var got map[string]any
	if err := json.Unmarshal(buf.bytes(), &got); err != nil {
		t.Fatalf("not one JSON line: %v (%q)", err, buf.bytes())
	}
	for _, key := range []string{"time", "via", "vm_id", "sid", "host", "method", "path", "status", "bytes_up", "bytes_down", "decision"} {
		if _, ok := got[key]; !ok {
			t.Errorf("audit line missing %q: %v", key, got)
		}
	}
	if got["via"] != "moca-egress" {
		t.Errorf("via = %v, want moca-egress", got["via"])
	}
	// Fields this slice leaves empty are absent, not zero: a reader must not
	// mistake an unset value for a real one.
	for _, key := range []string{"sub", "dial_addr", "credential", "dropped", "coalesced"} {
		if _, ok := got[key]; ok {
			t.Errorf("audit line carries %q, which this slice never sets", key)
		}
	}
}

// syncBuffer is a locked byte buffer for the JSON sink test.
type syncBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b = append(s.b, p...)
	return len(p), nil
}

func (s *syncBuffer) bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.b...)
}
