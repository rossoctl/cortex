package vmcontrol

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/listener/forwardproxy"
	"github.com/rossoctl/cortex/core/pipeline"
)

// newCappedStack is newVMEgressStack with limits.
func newCappedStack(t *testing.T, limits Limits) (*Registry, *captureSink) {
	t.Helper()
	p, err := pipeline.New([]pipeline.Plugin{&sessionProbe{}})
	if err != nil {
		t.Fatal(err)
	}
	fp := &forwardproxy.Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Client:           http.DefaultClient,
		TrustedSessionID: SessionIDFromRequest,
	}
	sink := &captureSink{}
	return NewRegistry(fp, sink, limits), sink
}

// countingOrigin answers 200 and counts the requests it saw.
func countingOrigin(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// wantRefusal checks the refusal shape: status, X-Moca-Egress-Error, a
// text/plain body "<code>: <name>\n", and the connection closed after it.
func wantRefusal(t *testing.T, resp *http.Response, code, name string) {
	t.Helper()
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests ||
		resp.Header.Get("X-Moca-Egress-Error") != code ||
		!strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") ||
		string(body) != code+": "+name+"\n" || !resp.Close {
		t.Fatalf("refusal = %d %v %q close=%v, want 429 %s naming %s, then close",
			resp.StatusCode, resp.Header, body, resp.Close, code, name)
	}
}

// eventOf is vm's audit event with decision.
func eventOf(t *testing.T, sink *captureSink, vm, decision string) Event {
	t.Helper()
	for _, e := range sink.forVM(vm) {
		if e.Decision == decision {
			return e
		}
	}
	t.Fatalf("no %s audit event for %s in %+v", decision, vm, sink.forVM(vm))
	return Event{}
}

// per_vm_conn_cap_429: with two live tunnels on vm-a and a cap of 2, a third
// connection on vm-a is refused before it is read, while vm-b of the same
// session is admitted: the cap is per VM, not per session.
func TestConnCapIsPerVM(t *testing.T) {
	reg, sink := newCappedStack(t, Limits{MaxConnsPerVM: 2})
	vmA := registerTCPVM(t, reg, "vm-a", "sid-a")
	vmB := registerTCPVM(t, reg, "vm-b", "sid-a")
	origin := echoTCPOrigin(t)

	for i := 0; i < 2; i++ {
		if resp := guestConnect(t, guest(t, vmA), origin); resp.StatusCode != http.StatusOK {
			t.Fatalf("tunnel %d: status %d", i+1, resp.StatusCode)
		}
	}
	third := guest(t, vmA)
	resp, err := http.ReadResponse(bufio.NewReader(third), nil)
	if err != nil {
		t.Fatalf("third conn on vm-a: %v", err)
	}
	wantRefusal(t, resp, DecisionConnCap, "vm-a")
	if e := eventOf(t, sink, "vm-a", DecisionConnCap); e.SID != "sid-a" || e.Status != 429 {
		t.Errorf("audit = %+v, want sid-a 429", e)
	}

	if resp := guestConnect(t, guest(t, vmB), origin); resp.StatusCode != http.StatusOK {
		t.Fatalf("vm-b CONNECT: status %d, want 200 under its own cap", resp.StatusCode)
	}
}

// A connection's slot frees when it closes, and only then.
func TestConnCapSlotFreesOnClose(t *testing.T) {
	inner := newPipeListener()
	var refused int
	l := newTrackedListener(inner, 1, func(c net.Conn, _ capRefusal) { refused++; _ = c.Close() })

	inner.dial()
	held, _ := l.Accept()
	inner.dial()
	inner.dial()
	inner.accepts <- nil // ends the next Accept once it has seen both
	_, _ = l.Accept()
	if refused != 2 {
		t.Fatalf("refused = %d with the slot held, want 2", refused)
	}

	_ = held.Close()
	inner.dial()
	inner.accepts <- nil // left unread when the slot is free
	if c, err := l.Accept(); err != nil || c == nil {
		t.Fatalf("Accept after the holder closed: %v, want the freed slot", err)
	}
}

// A guest looping on a full VM gets at most maxCapAnswersPerVM answers in
// flight, and a slot frees when its answer ends. Audit is one event per
// capEventEvery refusals, each carrying how many it stands for, and the
// first refusal after an admission is reported.
func TestConnCapBoundsAnswersAndEvents(t *testing.T) {
	inner := newPipeListener()
	var last capRefusal
	l := newTrackedListener(inner, 1, func(c net.Conn, d capRefusal) { last = d; _ = c.Close() })
	refuse := func() (net.Conn, net.Conn, capRefusal) {
		client, server := net.Pipe()
		return client, server, l.chargeCap(server)
	}

	var answered []net.Conn
	var reported int
	var coalesced uint64
	for i := 0; i < 2*capEventEvery+1; i++ {
		client, server, d := refuse()
		if d.answer {
			_ = client.Close() // so the answer ends at once
			answered = append(answered, server)
		}
		if d.report {
			reported++
			coalesced += d.coalesced
		}
	}
	if len(answered) != maxCapAnswersPerVM {
		t.Errorf("answers in flight = %d, want %d", len(answered), maxCapAnswersPerVM)
	}
	if reported != 3 || coalesced != 2*capEventEvery-2 {
		t.Errorf("events = %d standing for %d more, want 3 standing for %d",
			reported, coalesced, 2*capEventEvery-2)
	}
	l.answerCap(answered[0], "vm-a")
	if _, _, d := refuse(); !d.answer {
		t.Error("no answer after one ended, want its slot back")
	}

	inner.dial()
	if _, err := l.Accept(); err != nil {
		t.Fatal(err)
	}
	inner.dial()
	inner.accepts <- nil
	_, _ = l.Accept()
	// The refusal suppressed before the admission had no event yet, so this
	// one carries it.
	if !last.report || last.coalesced != 1 {
		t.Errorf("first refusal after an admission = %+v, want reported, carrying 1", last)
	}
}

// closeAll ends the conn_cap answers in flight, even to a guest that never
// reads, and returns only once they have ended.
func TestConnCapAnswersEndWithTheListener(t *testing.T) {
	l := newTrackedListener(newPipeListener(), 0, nil)
	client, pipe := net.Pipe()
	defer client.Close()
	server := noDeadlineConn{pipe} // so only closeAll can end the answer
	l.chargeCap(server)
	go l.answerCap(server, "vm-a") // blocks writing: client never reads
	l.closeAll()
	if n := len(l.answering); n != 0 {
		t.Errorf("answers in flight after closeAll = %d, want 0", n)
	}
}

// per_session_request_cap_429: the cap sums a session's requests across its
// VMs and connections; the request past it never reaches the upstream, and
// another session is unaffected.
func TestRequestCapIsPerSession(t *testing.T) {
	reg, sink := newCappedStack(t, Limits{MaxRequestsPerSession: 3})
	vmA := registerTCPVM(t, reg, "vm-a", "sid-s")
	vmB := registerTCPVM(t, reg, "vm-b", "sid-s")
	vmC := registerTCPVM(t, reg, "vm-c", "sid-t")
	origin, seen := countingOrigin(t)

	ga, gb := guest(t, vmA), guest(t, vmB)
	for _, s := range []struct {
		c    net.Conn
		path string
	}{{ga, "/1"}, {ga, "/2"}, {gb, "/3"}} {
		if resp := guestGet(t, s.c, origin.URL+s.path, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", s.path, resp.StatusCode)
		}
	}
	wantRefusal(t, guestGet(t, gb, origin.URL+"/4", nil), DecisionRequestCap, "127.0.0.1")
	if e := eventOf(t, sink, "vm-b", DecisionRequestCap); e.SID != "sid-s" || e.Status != 429 {
		t.Errorf("audit = %+v, want sid-s 429", e)
	}
	if n := seen.Load(); n != 3 {
		t.Errorf("upstream saw %d requests, want 3", n)
	}

	if resp := guestGet(t, guest(t, vmC), origin.URL+"/other", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("sid-t: status %d, want 200", resp.StatusCode)
	}
}

// A request decrypted inside a bridged tunnel counts like any other, so one
// CONNECT cannot carry requests past the cap.
func TestRequestCapCountsBridgedRequests(t *testing.T) {
	reg, sink, _, _, authority, roots, originSaw := newBridgedVMEgressStack(t)
	reg.limits.MaxRequestsPerSession = 2 // the CONNECT and one request inside it
	vmA := registerTCPVM(t, reg, "vm-a", "sid-a")

	raw := guest(t, vmA)
	if resp := guestConnect(t, raw, authority); resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status %d", resp.StatusCode)
	}
	tc := tls.Client(raw, &tls.Config{ServerName: hostOf(authority), RootCAs: roots})
	br := bufio.NewReader(tc)
	host := authority
	get := func(path string) *http.Response {
		t.Helper()
		if _, err := fmt.Fprintf(tc, "GET %s HTTP/1.1\r\nHost: %s\r\nX-Req: %s\r\n\r\n", path, host, path); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := get("/1")
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/1: status %d", resp.StatusCode)
	}
	// The guest's own Host does not name the refusal; the tunnel's does.
	host = "elsewhere.example"
	wantRefusal(t, get("/2"), DecisionRequestCap, hostOf(authority))
	if e := eventOf(t, sink, "vm-a", DecisionRequestCap); e.Host != hostOf(authority) || e.Path != "/2" {
		t.Errorf("audit = %+v, want the tunnel's host and /2", e)
	}
	if originSaw.Get("X-Req") != "/1" {
		t.Errorf("origin last saw %q, want /1", originSaw.Get("X-Req"))
	}
}

// The count outlives any one VM of a session and ends with its last.
func TestRequestCapLivesWithTheSessionsVMs(t *testing.T) {
	reg, _ := newCappedStack(t, Limits{MaxRequestsPerSession: 1})
	origin, _ := countingOrigin(t)
	get := func(addr string) int {
		resp := guestGet(t, guest(t, addr), origin.URL+"/", nil)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	vmA := registerTCPVM(t, reg, "vm-a", "sid-s")
	if got := get(vmA); got != http.StatusOK {
		t.Fatalf("first request: %d", got)
	}
	vmB := registerTCPVM(t, reg, "vm-b", "sid-s")
	_ = reg.Unregister("vm-a")
	vmC := registerTCPVM(t, reg, "vm-c", "sid-s")
	for vm, addr := range map[string]string{"vm-b": vmB, "vm-c": vmC} {
		if got := get(addr); got != http.StatusTooManyRequests {
			t.Errorf("%s after vm-a left: %d, want 429 (the session still has a VM)", vm, got)
		}
	}
	_ = reg.Unregister("vm-b")
	_ = reg.Unregister("vm-c")
	vmD := registerTCPVM(t, reg, "vm-d", "sid-s")
	if got := get(vmD); got != http.StatusOK {
		t.Errorf("vm-d after the session's last VM left: %d, want 200 (a fresh count)", got)
	}
}

// noDeadlineConn ignores deadlines.
type noDeadlineConn struct{ net.Conn }

func (noDeadlineConn) SetDeadline(time.Time) error { return nil }

// pipeListener is an in-memory net.Listener; dial queues one connection.
type pipeListener struct{ accepts chan net.Conn }

func newPipeListener() *pipeListener { return &pipeListener{accepts: make(chan net.Conn, 8)} }

func (l *pipeListener) dial() net.Conn {
	client, server := net.Pipe()
	l.accepts <- server
	return client
}

func (l *pipeListener) Accept() (net.Conn, error) {
	if c := <-l.accepts; c != nil {
		return c, nil
	}
	return nil, net.ErrClosed
}
func (l *pipeListener) Close() error   { return nil }
func (l *pipeListener) Addr() net.Addr { return &net.UnixAddr{Name: "pipe", Net: "unix"} }
