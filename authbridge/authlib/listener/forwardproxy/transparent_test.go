package forwardproxy

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/plugintesting"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/session"
)

// TestRecordTunnelOpened_SetsTunnelMarker locks the explicit producer marker:
// a CONNECT / transparent-redirect tunnel-open is recorded with Tunnel=true so
// consumers (abctl) fold it into the decrypted inner request without inferring
// "tunnel" from host/extension shape.
func TestRecordTunnelOpened_SetsTunnelMarker(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	s.recordTunnelOpened(&pipeline.Context{Direction: pipeline.Outbound, Host: "example.com:443"})

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 1 {
		t.Fatalf("expected 1 tunnel-open event, got %+v", v)
	}
	ev := v.Events[0]
	if !ev.Tunnel {
		t.Error("tunnel-open event must have Tunnel=true")
	}
	if ev.Direction != pipeline.Outbound || ev.Phase != pipeline.SessionRequest {
		t.Errorf("tunnel-open = %v/%v, want Outbound/SessionRequest", ev.Direction, ev.Phase)
	}
}

// HandleTransparentConn gates then blind-tunnels: with an allow-all pipeline it
// must dial the recovered destination and copy bytes both ways, emitting no
// proxy-protocol bytes of its own (the agent thinks it's talking to dst).
func TestHandleTransparentConn_Tunnels(t *testing.T) {
	const banner = "UPSTREAM-HELLO\n"

	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen upstream: %v", err)
	}
	defer func() { _ = upstream.Close() }()
	go func() {
		c, err := upstream.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = c.Write([]byte(banner))
		buf := make([]byte, 4)
		if _, err := io.ReadFull(c, buf); err != nil {
			return
		}
		_, _ = c.Write(buf) // echo
	}()

	p, err := plugintesting.BuildPipeline(nil) // allow-all
	if err != nil {
		t.Fatalf("build pipeline: %v", err)
	}
	srv := &Server{OutboundPipeline: pipeline.NewHolder(p)}

	agentSide, proxySide := net.Pipe()
	go srv.HandleTransparentConn(proxySide, upstream.Addr().String())

	_ = agentSide.SetDeadline(time.Now().Add(5 * time.Second))

	// The first bytes the agent sees must be the upstream's banner, NOT a
	// "200 Connection Established" — proving no proxy protocol leaked.
	got := make([]byte, len(banner))
	if _, err := io.ReadFull(agentSide, got); err != nil {
		t.Fatalf("read banner: %v", err)
	}
	if string(got) != banner {
		t.Fatalf("first bytes = %q, want upstream banner %q", got, banner)
	}

	if _, err := agentSide.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(agentSide, echo); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(echo) != "ping" {
		t.Fatalf("echo = %q, want %q", echo, "ping")
	}
	_ = agentSide.Close()
}
