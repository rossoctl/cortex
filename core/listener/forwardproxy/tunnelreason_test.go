package forwardproxy

import (
	"github.com/rossoctl/cortex/core/session"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/tlsbridge"
)

// TestPassthroughReason pins the mapping onto Classify's own vocabulary. If
// Classify gains a reason and this is not extended, the event silently carries
// "" — which reads as "bridged" and is the opposite of the truth.
func TestPassthroughReason(t *testing.T) {
	for _, tc := range []struct {
		why  string
		want pipeline.TunnelReason
	}{
		{"port", pipeline.TunnelPassthroughPort},
		{"non-tls", pipeline.TunnelPassthroughNonTLS},
		{"skip", pipeline.TunnelPassthroughHost},
		{"", ""},
	} {
		if got := passthroughReason(tc.why); got != tc.want {
			t.Errorf("passthroughReason(%q) = %q, want %q", tc.why, got, tc.want)
		}
	}
}

// TestPassthroughReasonCoversEveryClassifyReason is a real tripwire, not a restated
// list. It iterates tlsbridge.ClassifyReasons — the vocabulary Classify itself uses —
// so adding a reason there, or renaming one, fails HERE instead of silently producing
// an unmapped "" downstream. An unmapped reason renders as an em dash, which reads as
// "bridged": the opposite of what happened.
func TestPassthroughReasonCoversEveryClassifyReason(t *testing.T) {
	if len(tlsbridge.ClassifyReasons) == 0 {
		t.Fatal("tlsbridge.ClassifyReasons is empty; this test would assert nothing")
	}
	for _, why := range tlsbridge.ClassifyReasons {
		got := passthroughReason(why)
		if got == "" {
			t.Errorf("Classify reason %q maps to \"\", which renders as a BRIDGED row", why)
			continue
		}
		if len(got) > pluginCellWidth {
			t.Errorf("reason %q is %d chars; it truncates in abctl's %d-wide PLUGIN cell, "+
				"so the timeline token stops matching the log token", got, len(got), pluginCellWidth)
		}
	}
}

// pluginCellWidth mirrors abctl's PLUGIN column width. Duplicated deliberately:
// core must not import the TUI, and a reason that does not fit is a defect in the
// reason, not in the column.
const pluginCellWidth = 18

// TestClientAddrNeverPanics: these helpers exist only to build a log line, so a
// nil conn must degrade rather than take the proxy down on a diagnostic path.
func TestClientAddrNeverPanics(t *testing.T) {
	if got := clientAddr(nil); got != "unknown" {
		t.Errorf("clientAddr(nil) = %q, want %q", got, "unknown")
	}
	if got := clientPort(nil); got != "<port>" {
		t.Errorf("clientPort(nil) = %q, want a template placeholder, got %q", got, got)
	}
}

// TestClientPortIsTheDiscriminator is the point of the whole change: the host
// cannot tell two clients apart because they all dial the same host, so the
// source port is what identifies the offender.
func TestClientPortIsTheDiscriminator(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	srv, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer func() { _ = srv.Close() }()

	// srv's RemoteAddr is the client end — the same thing bridgeServe holds.
	addr := clientAddr(srv)
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("clientAddr = %q, want a loopback host:port", addr)
	}
	port := clientPort(srv)
	if port == "<port>" || !strings.HasSuffix(addr, ":"+port) {
		t.Errorf("clientPort = %q, not the port of %q", port, addr)
	}
	// It must be the CLIENT's ephemeral port, not the listener's: the listener
	// port is shared by every client and would identify nothing.
	if _, lport, _ := net.SplitHostPort(ln.Addr().String()); port == lport {
		t.Errorf("clientPort returned the listener port %q — that cannot discriminate between clients", port)
	}
}

// TestCANotBeforeWithoutBridge: the value appears in a log line on a failure
// path, so its absence must read as "unknown" rather than crash or print junk.
func TestCANotBeforeWithoutBridge(t *testing.T) {
	s := &Server{}
	if got := s.caNotBefore(); got != "unknown" {
		t.Errorf("caNotBefore with no bridge = %q, want %q", got, "unknown")
	}
}

// TestTunnelRecorderRecordsAtMostOnce exercises the invariant directly.
//
// The integration test that claimed to cover this did not: no current path calls the
// recorder twice, so bypassing the guard left every test green. Verified by doing
// exactly that. The guard exists for a future exit, so only a direct call can hold it.
func TestTunnelRecorderRecordsAtMostOnce(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}
	pctx := &pipeline.Context{Direction: pipeline.Outbound, Host: "example.com:443"}

	rec := s.tunnelRecorderFor(pctx, false)
	rec(pipeline.TunnelSkipCached)
	rec(pipeline.TunnelClientRejectedCA) // a second exit firing must be swallowed
	rec("")

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 1 {
		t.Fatalf("want exactly 1 event after 3 calls, got %d", len(v.Events))
	}
	if got := v.Events[0].TunnelReason; got != pipeline.TunnelSkipCached {
		t.Errorf("recorded reason = %q, want the FIRST call's %q", got, pipeline.TunnelSkipCached)
	}
}

// TestTunnelRecorderSkipsSkipHosts: a SkipHosts destination ran no plugins, so there is
// nothing to attribute an event to and none must be written.
func TestTunnelRecorderSkipsSkipHosts(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}
	rec := s.tunnelRecorderFor(&pipeline.Context{Direction: pipeline.Outbound, Host: "h:443"}, true)
	rec(pipeline.TunnelPassthroughHost)

	if v := store.View(session.DefaultSessionID); v != nil && len(v.Events) != 0 {
		t.Errorf("a SkipHosts tunnel recorded %d event(s); want none", len(v.Events))
	}
}
