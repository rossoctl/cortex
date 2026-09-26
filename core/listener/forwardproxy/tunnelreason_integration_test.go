package forwardproxy

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/plugintesting"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/tlsbridge"
)

// bridgeForRejectTest builds a Server whose upstream verification SUCCEEDS (so
// bridgeServe reaches the forge step) against a throwaway TLS origin.
func bridgeForRejectTest(t *testing.T) (*Server, *session.Store, string) {
	t.Helper()

	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(origin.Close)
	originCAPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: origin.Certificate().Raw})

	src, err := tlsbridge.NewEphemeralSource()
	if err != nil {
		t.Fatalf("NewEphemeralSource: %v", err)
	}
	up, err := tlsbridge.NewUpstreamClient(originCAPEM, false)
	if err != nil {
		t.Fatalf("NewUpstreamClient: %v", err)
	}
	store := session.New(5*time.Minute, 100, 0)
	t.Cleanup(store.Close)

	s := &Server{
		Sessions: store,
		TLSBridge: &tlsbridge.Engine{
			Term:     tlsbridge.NewTerminator(tlsbridge.NewMinter(src, tlsbridge.MinterOpts{})),
			Skip:     tlsbridge.NewSkipSet(),
			Upstream: up,
			CAPEM:    src.CACertPEM(),
		},
	}
	return s, store, strings.TrimPrefix(origin.URL, "https://")
}

// clientConnPair returns the PROXY-side conn of a TCP pair, having handed the client
// end to fn in a goroutine. Real TCP rather than net.Pipe because clientAddr /
// clientPort read RemoteAddr, and the port is the whole point of those.
func clientConnPair(t *testing.T, fn func(net.Conn)) net.Conn {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	accepted := make(chan net.Conn, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			accepted <- nil
			return
		}
		accepted <- c
	}()
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	go fn(raw)

	srv := <-accepted
	if srv == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// rejectingClient is a real TLS client that trusts nothing, so it refuses the forged
// leaf and sends a bad_certificate alert. That alert is what lets the proxy claim CA
// distrust rather than guess at it.
//
// The earlier version of this helper just wrote "nope" and closed, which produces
// "unexpected EOF" — a hang-up, not a rejection. It passed against a classifier that
// labelled every handshake error client-rejected-ca, and stopped passing the moment
// that claim was narrowed to what the evidence supports. The test was the
// counterexample to its own assertion.
func rejectingClient(t *testing.T) net.Conn {
	t.Helper()
	return clientConnPair(t, func(raw net.Conn) {
		tc := tls.Client(raw, &tls.Config{ServerName: "example.com", RootCAs: x509.NewCertPool()})
		_ = tc.Handshake() // fails, and sends the alert on its way out
		_ = tc.Close()
	})
}

// hangUpClient sends bytes that are not a ClientHello and vanishes, which is what a
// cancelled request or a dead socket looks like. Distinct from a rejection, and it
// must NOT be told to restart anything.
func hangUpClient(t *testing.T) net.Conn {
	t.Helper()
	return clientConnPair(t, func(raw net.Conn) {
		_, _ = raw.Write([]byte("nope"))
		_ = raw.Close()
	})
}

// TestClientRejectedCA_WarnsEvenAfterOtherTrafficBridged is the regression test
// for the bug this change exists to fix.
//
// The guidance ("does not trust the bridge CA") used to sit behind
// bridgedRequests == 0, which treats CA trust as a property of the deployment.
// It is a property of each client. On a machine running several agents they
// disagree — one predates the CA, the rest do not — so the counter was non-zero
// and the one message that explains the failure never printed. Diagnosing it
// then took the proxy log, the CA's NotBefore and a process listing.
func TestClientRejectedCA_WarnsEvenAfterOtherTrafficBridged(t *testing.T) {
	s, store, authority := bridgeForRejectTest(t)

	// The condition that used to suppress everything: something has bridged.
	s.bridgedRequests.Store(7)

	var logbuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	client := rejectingClient(t)
	pctx := &pipeline.Context{Direction: pipeline.Outbound, Host: authority}
	rec := func(reason pipeline.TunnelReason) { s.recordTunnelOpened(pctx, reason) }

	if handled := s.bridgeServe(client, authority, hostOnly(authority), rec); !handled {
		t.Fatal("bridgeServe returned false; the connection is dead post-forge and must be reported handled")
	}

	got := logbuf.String()
	if !strings.Contains(got, string(pipeline.TunnelClientRejectedCA)) {
		t.Errorf("warning did not name the reason %q despite bridgedRequests=7:\n%s",
			pipeline.TunnelClientRejectedCA, got)
	}
	// The client address is the discriminator — the host cannot tell two clients
	// apart because they all dial the same host.
	if !strings.Contains(got, "client=") {
		t.Errorf("warning did not name the client, so the offender is unattributable:\n%s", got)
	}
	if !strings.Contains(got, "ca_not_before=") {
		t.Errorf("warning did not state the CA cutoff, which is what makes it actionable:\n%s", got)
	}
	// The lsof recipe deliberately does NOT appear here: review asked for a log line
	// short enough to read unwrapped, so mapping a client port to a process lives in
	// docs/laptop-service.md instead of being repeated on every occurrence. What the
	// line must still carry is the client and the cutoff, asserted above.
	if strings.Contains(got, "lsof") {
		t.Errorf("the warning re-grew the lsof recipe; it belongs in the docs:\n%s", got)
	}

	// And the timeline carries it, so this is visible without reading a log file.
	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 1 {
		t.Fatalf("want exactly 1 tunnel event, got %+v", v)
	}
	if ev := v.Events[0]; !ev.Tunnel || ev.TunnelReason != pipeline.TunnelClientRejectedCA {
		t.Errorf("event = {Tunnel:%v Reason:%q}, want {true %q}",
			ev.Tunnel, ev.TunnelReason, pipeline.TunnelClientRejectedCA)
	}
}

// TestClientRejectedCA_SkipsHostAfterwards pins the existing self-healing
// behaviour: the failure is remembered so the client's retry tunnels instead of
// dying again. Unchanged by this work, asserted because the reason vocabulary
// now distinguishes that later tunnel (skip-cached) from this one.
func TestClientRejectedCA_SkipsHostAfterwards(t *testing.T) {
	s, _, authority := bridgeForRejectTest(t)
	host := hostOnly(authority)
	if s.TLSBridge.Skip.Contains(host) {
		t.Fatal("host skipped before any failure")
	}
	s.bridgeServe(rejectingClient(t), authority, host, noopRecorder)
	if !s.TLSBridge.Skip.Contains(host) {
		t.Error("host not skipped after a rejected forge; the client's retry would fail again")
	}
}

// TestClientHungUp_GetsNoRestartAdvice: an EOF mid-handshake is not proof of anything
// about trust, so it must not carry the restart-your-agents hint. Sending someone to
// restart agents over a cancelled request wastes their time and teaches them to
// distrust the message that matters.
func TestClientHungUp_GetsNoRestartAdvice(t *testing.T) {
	s, store, authority := bridgeForRejectTest(t)

	var logbuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	pctx := &pipeline.Context{Direction: pipeline.Outbound, Host: authority}
	s.bridgeServe(hangUpClient(t), authority, hostOnly(authority),
		func(reason pipeline.TunnelReason) { s.recordTunnelOpened(pctx, reason) })

	got := logbuf.String()
	if !strings.Contains(got, string(pipeline.TunnelClientHungUp)) {
		t.Errorf("want reason %q, got:\n%s", pipeline.TunnelClientHungUp, got)
	}
	if strings.Contains(got, "fix=") || strings.Contains(got, "ca_not_before") {
		t.Errorf("a hang-up was given CA-trust advice it cannot justify:\n%s", got)
	}
	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 1 || v.Events[0].TunnelReason != pipeline.TunnelClientHungUp {
		t.Errorf("event reason = %+v, want %q", v, pipeline.TunnelClientHungUp)
	}
}

// connectThrough drives a real CONNECT through handleConnect against a throwaway
// origin and returns the tunnel-open event that was recorded, or nil.
//
// This exists because the tests above call bridgeServe directly, which leaves the
// recorder closure in handleConnect — recOnce, the skipped guard, and five of the nine
// reasons — with no coverage at all. Reading the control flow is not the same as
// pinning it.
func connectThrough(t *testing.T, s *Server, store *session.Store, target string, first []byte) *pipeline.SessionEvent {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleRequest(w, r)
	}))
	t.Cleanup(srv.Close)

	raw, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = raw.Close() }()

	if _, err := fmt.Fprintf(raw, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(raw)
	if _, err := http.ReadResponse(br, nil); err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if len(first) > 0 {
		_, _ = raw.Write(first)
	}
	// The event is recorded on the CONNECT path before any copying; give it a moment.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v := store.View(session.DefaultSessionID); v != nil && len(v.Events) > 0 {
			return &v.Events[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

// connectServer builds a Server wired the way handleConnect needs: an outbound
// pipeline holder (it runs the gate on the CONNECT itself) plus a session store.
func connectServer(t *testing.T, store *session.Store, bridge *tlsbridge.Engine) *Server {
	t.Helper()
	p, err := plugintesting.BuildPipeline(nil)
	if err != nil {
		t.Fatalf("BuildPipeline: %v", err)
	}
	return &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Client:           http.DefaultClient,
		Sessions:         store,
		TLSBridge:        bridge,
	}
}

// tlsRecordHead is the first five bytes of a TLS handshake record: content type 22,
// version 3.x, length. Enough for looksLikeTLSRecord, and enough to unblock the
// listener's Peek(5) — which waits for five bytes before it can classify anything, so
// a test that sends nothing after CONNECT hangs until its own deadline rather than
// exercising the branch it names.
var tlsRecordHead = []byte{0x16, 0x03, 0x01, 0x00, 0x00}

// TestHandleConnect_RecordsReason covers the reasons only reachable through
// handleConnect's own wiring. Each is a different branch of the decision, and none was
// exercised end to end before.
func TestHandleConnect_RecordsReason(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()
	target := strings.TrimPrefix(origin.URL, "http://")

	t.Run("bridge-disabled", func(t *testing.T) {
		store := session.New(5*time.Minute, 100, 0)
		defer store.Close()
		s := connectServer(t, store, nil) // no TLSBridge at all
		ev := connectThrough(t, s, store, target, nil)
		if ev == nil || ev.TunnelReason != pipeline.TunnelBridgeDisabled {
			t.Fatalf("reason = %v, want %q", ev, pipeline.TunnelBridgeDisabled)
		}
	})

	t.Run("skip-cached", func(t *testing.T) {
		store := session.New(5*time.Minute, 100, 0)
		defer store.Close()
		d, err := tlsbridge.NewDecision(tlsbridge.DecisionOpts{Ports: map[int]bool{portOf(target): true}})
		if err != nil {
			t.Fatal(err)
		}
		s := connectServer(t, store, &tlsbridge.Engine{Decision: d, Skip: tlsbridge.NewSkipSet()})
		s.TLSBridge.Skip.Fail(hostOnly(target)) // a previous client's rejection
		ev := connectThrough(t, s, store, target, tlsRecordHead)
		if ev == nil || ev.TunnelReason != pipeline.TunnelSkipCached {
			t.Fatalf("reason = %v, want %q", ev, pipeline.TunnelSkipCached)
		}
	})

	t.Run("passthrough-port", func(t *testing.T) {
		store := session.New(5*time.Minute, 100, 0)
		defer store.Close()
		// A port the bridge does not watch.
		d, err := tlsbridge.NewDecision(tlsbridge.DecisionOpts{Ports: map[int]bool{1: true}})
		if err != nil {
			t.Fatal(err)
		}
		s := connectServer(t, store, &tlsbridge.Engine{Decision: d, Skip: tlsbridge.NewSkipSet()})
		ev := connectThrough(t, s, store, target, tlsRecordHead)
		if ev == nil || ev.TunnelReason != pipeline.TunnelPassthroughPort {
			t.Fatalf("reason = %v, want %q", ev, pipeline.TunnelPassthroughPort)
		}
	})

	t.Run("passthrough-nontls", func(t *testing.T) {
		store := session.New(5*time.Minute, 100, 0)
		defer store.Close()
		d, err := tlsbridge.NewDecision(tlsbridge.DecisionOpts{Ports: map[int]bool{portOf(target): true}})
		if err != nil {
			t.Fatal(err)
		}
		s := connectServer(t, store, &tlsbridge.Engine{Decision: d, Skip: tlsbridge.NewSkipSet()})
		// Plain HTTP bytes: watched port, but not a TLS record.
		ev := connectThrough(t, s, store, target, []byte("GET / HTTP/1.1\r\n\r\n"))
		if ev == nil || ev.TunnelReason != pipeline.TunnelPassthroughNonTLS {
			t.Fatalf("reason = %v, want %q", ev, pipeline.TunnelPassthroughNonTLS)
		}
	})
}

// TestHandleConnect_RecordsExactlyOnce: the recorder is a closure with a recOnce
// guard, so a CONNECT must produce ONE tunnel-open however it is decided. A double
// record would double-count every tunnel in the timeline.
func TestHandleConnect_RecordsExactlyOnce(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer origin.Close()
	target := strings.TrimPrefix(origin.URL, "http://")

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := connectServer(t, store, nil)
	if ev := connectThrough(t, s, store, target, nil); ev == nil {
		t.Fatal("no event recorded")
	}
	if v := store.View(session.DefaultSessionID); v == nil || len(v.Events) != 1 {
		t.Errorf("want exactly 1 tunnel-open event, got %d", len(v.Events))
	}
}

// TestHangUpAlsoSeedsTheSkip pins that a hang-up skips the host too, and that the
// second connection therefore reports skip-cached rather than client-rejected-ca.
//
// Raised in review as a possible defect — Skip is seeded before the failure is
// classified, so a hang-up caches the same state a confirmed rejection does. Keeping
// it deliberately: in EVERY failure class the forged handshake already killed that
// connection, so the client's retry needs a tunnel to work at all. Skipping only on
// client-rejected-ca would leave a client that closes without sending an alert — which
// is a real way to refuse a certificate — failing forever.
//
// What WAS wrong is what skip-cached claimed. It said "another client rejected the CA",
// which is true only sometimes; the seeding failure logs its own specific reason. This
// test exists so the behaviour is a choice on the record rather than an accident.
func TestHangUpAlsoSeedsTheSkip(t *testing.T) {
	s, store, authority := bridgeForRejectTest(t)
	host := hostOnly(authority)
	pctx := &pipeline.Context{Direction: pipeline.Outbound, Host: authority}

	s.bridgeServe(hangUpClient(t), authority, host,
		func(r pipeline.TunnelReason) { s.recordTunnelOpened(pctx, r) })

	// The first failure reports what actually happened, not a CA rejection.
	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 1 {
		t.Fatalf("want 1 event, got %+v", v)
	}
	if got := v.Events[0].TunnelReason; got != pipeline.TunnelClientHungUp {
		t.Errorf("first failure reason = %q, want %q — a hang-up must not be reported as "+
			"a CA rejection", got, pipeline.TunnelClientHungUp)
	}
	// And it seeds the skip, so the client's retry can tunnel.
	if !s.TLSBridge.Skip.Contains(host) {
		t.Error("a hang-up did not seed the skip; the client's retry would forge again and " +
			"die again")
	}
}

// TestPassthroughReasonNeverEmpty: the fallback must be the sentinel, never "".
// An empty reason is how a BRIDGED row is marked, so an unmapped passthrough would
// render as an em dash and read as "we decrypted this" — the opposite of the truth.
func TestPassthroughReasonNeverEmpty(t *testing.T) {
	// Every reason Classify can DECLINE with must map to something non-empty. "" is
	// excluded on purpose: Classify pairs it with Terminate, so it is not a
	// passthrough at all and empty is the right answer there — asserted separately
	// below rather than lumped in here, which is what made this test contradict the
	// behaviour it was written to protect.
	for _, why := range append([]string{"a-reason-nobody-mapped"}, tlsbridge.ClassifyReasons...) {
		if got := passthroughReason(why); got == "" {
			t.Errorf("passthroughReason(%q) = %q; an empty reason renders as BRIDGED", why, got)
		}
	}
	if got := passthroughReason(""); got != "" {
		t.Errorf(`passthroughReason("") = %q, want "" — Classify pairs "" with Terminate, `+
			"so the caller bridges and bridgeServe records the outcome", got)
	}
	if got := passthroughReason("a-reason-nobody-mapped"); got != pipeline.TunnelPassthroughUnknown {
		t.Errorf("unmapped reason = %q, want the sentinel %q", got, pipeline.TunnelPassthroughUnknown)
	}
}

// trustingClient is a real TLS client that DOES trust the bridge CA, so it completes
// the forged handshake. It sends a request and reads the reply so ServeConn has
// something to serve, then closes.
func trustingClient(t *testing.T, caPEM []byte) net.Conn {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("could not load the bridge CA into a pool")
	}
	return clientConnPair(t, func(raw net.Conn) {
		tc := tls.Client(raw, &tls.Config{ServerName: "example.com", RootCAs: pool})
		if err := tc.Handshake(); err != nil {
			return
		}
		_, _ = tc.Write([]byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"))
		_, _ = io.ReadFull(tc, make([]byte, 1))
		_ = tc.Close()
	})
}

// TestOneStaleClientDoesNotSuppressAHealthyOne is the scenario this change exists for.
//
// The skip set is keyed by host, and there is no durable client identity to key it by,
// so one client's rejection tunnels every OTHER client's traffic to that host as well.
// With a fixed ten-minute window re-armed on each attempt, a single agent holding a
// stale CA cost a correctly-configured one nearly all of its observability — measured
// on a real laptop as four rejections over a hundred minutes with one bridged request
// in between.
//
// A success now clears the entry, so the healthy client restores interception itself
// rather than waiting out a window it did not cause.
func TestOneStaleClientDoesNotSuppressAHealthyOne(t *testing.T) {
	s, _, authority := bridgeForRejectTest(t)
	host := hostOnly(authority)

	// 1. The stale client rejects our leaf; the host is skipped for everyone.
	s.bridgeServe(rejectingClient(t), authority, host, noopRecorder)
	if !s.TLSBridge.Skip.Contains(host) {
		t.Fatal("a rejected forge did not skip the host")
	}

	// 2. The healthy client bridges. bridgeServe blocks serving the decrypted
	//    connection, so run it and wait for the skip to clear.
	go s.bridgeServe(trustingClient(t, s.TLSBridge.CAPEM), authority, host, noopRecorder)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !s.TLSBridge.Skip.Contains(host) {
			return // cleared: the next connection from either client is intercepted again
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("a successful bridge did not clear the skip; the healthy client stays blind " +
		"until a window it did not cause elapses")
}

// Escalation itself — that a rejection lengthens the window and a transient failure
// does not — is asserted in tlsbridge, where SkipSet's internals are reachable without
// exporting a window accessor purely for a test. What belongs HERE is that bridgeServe
// routes each failure class to the right call, which TestHangUpAlsoSeedsTheSkip and
// TestClientRejectedCA_SkipsHostAfterwards cover between them.

// TestClientRejectedCA_NamesTheCAItself is the diagnostic gap behind issue #1033.
//
// ca_not_before answers "is this client older than the CA", which is the right
// question when the CA was regenerated in place. It is the wrong question when the
// CA MOVED: `--local` derives ca_dir from $HOME, so a sandbox or any redirected
// $HOME gets its own CA, and every one of them is spelled ~/.cortex/ca and carries
// the same CN=authbridge-tls-bridge-ca. Two CAs, one name, and a ca_not_before that
// looks perfectly recent for both — nothing in the line distinguishes "your client
// is stale" from "your client trusts a different file".
//
// The fingerprint does, and it is the only thing that does. ca_file names which
// anchor the client was supposed to load, which is the other half of the answer.
func TestClientRejectedCA_NamesTheCAItself(t *testing.T) {
	s, _, authority := bridgeForRejectTest(t)
	s.TLSBridge.CAFile = "/tmp/sandbox-a/.cortex/ca/ca.crt"

	var logbuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s.bridgeServe(rejectingClient(t), authority, hostOnly(authority), noopRecorder)

	got := logbuf.String()
	if !strings.Contains(got, "ca_fingerprint=") {
		t.Errorf("warning did not fingerprint the CA, so a MOVED CA is indistinguishable "+
			"from a merely stale client (both share CN=authbridge-tls-bridge-ca):\n%s", got)
	}
	if !strings.Contains(got, "ca_file=") {
		t.Errorf("warning did not name the trust anchor the client should hold:\n%s", got)
	}
	if !strings.Contains(got, "/tmp/sandbox-a/.cortex/ca/ca.crt") {
		t.Errorf("ca_file did not carry the RESOLVED path, which is the part that differs "+
			"between sandboxes:\n%s", got)
	}
}

// TestCAFingerprint_MatchesOpenSSL pins the fingerprint to the one form a user can
// actually compare against. The whole value of printing it is that someone runs
//
//	openssl x509 -in ca.crt -noout -fingerprint -sha256
//
// and matches it by eye, so the encoding must be that command's: uppercase hex,
// colon-separated. A bare lowercase hex string would be correct and useless.
func TestCAFingerprint_MatchesOpenSSL(t *testing.T) {
	s, _, _ := bridgeForRejectTest(t)

	got := s.caFingerprint()

	sum := sha256.Sum256(caCertDER(t, s.TLSBridge.CAPEM))
	want := make([]string, 0, len(sum))
	for _, b := range sum {
		want = append(want, fmt.Sprintf("%02X", b))
	}
	if expected := strings.Join(want, ":"); got != expected {
		t.Errorf("caFingerprint() = %q, want %q (openssl -fingerprint -sha256 form)", got, expected)
	}
}

// caCertDER pulls the DER out of a PEM CA, so the test computes its expectation
// from the same bytes openssl would hash rather than from the implementation.
func caCertDER(t *testing.T, caPEM []byte) []byte {
	t.Helper()
	blk, _ := pem.Decode(caPEM)
	if blk == nil {
		t.Fatal("CAPEM is not PEM")
	}
	return blk.Bytes
}

// TestCAFingerprint_UnknownWithoutCA: diagnostics must never panic or invent a
// value. Mirrors caNotBefore's "unknown" contract for the same reason.
func TestCAFingerprint_UnknownWithoutCA(t *testing.T) {
	s := &Server{}
	if got := s.caFingerprint(); got != "unknown" {
		t.Errorf("caFingerprint() with no bridge = %q, want %q", got, "unknown")
	}
	s2 := &Server{TLSBridge: &tlsbridge.Engine{CAPEM: []byte("not pem")}}
	if got := s2.caFingerprint(); got != "unknown" {
		t.Errorf("caFingerprint() on garbage PEM = %q, want %q", got, "unknown")
	}
}

// TestClientHungUp_GetsNoCAIdentity: the same gating that keeps restart advice off
// a hang-up must keep the CA identity off it too. An EOF says nothing about trust,
// and a fingerprint on that line invites someone to go compare certificates over
// what was probably a cancelled request.
func TestClientHungUp_GetsNoCAIdentity(t *testing.T) {
	s, _, authority := bridgeForRejectTest(t)
	s.TLSBridge.CAFile = "/tmp/sandbox-a/.cortex/ca/ca.crt"

	var logbuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s.bridgeServe(hangUpClient(t), authority, hostOnly(authority), noopRecorder)

	if got := logbuf.String(); strings.Contains(got, "ca_fingerprint=") || strings.Contains(got, "ca_file=") {
		t.Errorf("a hang-up was given CA identity it cannot justify:\n%s", got)
	}
}
