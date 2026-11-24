package cluster

import (
	"context"
	"errors"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

func TestFreeLocalPortReturnsUsablePort(t *testing.T) {
	port, err := freeLocalPort()
	if err != nil {
		t.Fatalf("freeLocalPort: %v", err)
	}
	if port <= 0 || port > 65535 {
		t.Fatalf("port out of range: %d", port)
	}
	// We should be able to listen on it (the listener inside freeLocalPort
	// has been closed by now).
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("could not bind to returned port %d: %v", port, err)
	}
	l.Close()
}

func TestWaitForAcceptReturnsWhenListenerOpens(t *testing.T) {
	port, err := freeLocalPort()
	if err != nil {
		t.Fatalf("freeLocalPort: %v", err)
	}
	// waitForAccept dials a port nothing listens on yet, and returns nil once the
	// listener below opens after that refused dial.
	saved := dialAccept
	t.Cleanup(func() { dialAccept = saved })
	refused := make(chan struct{})
	var once sync.Once
	var connected atomic.Bool
	dialAccept = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := saved(ctx, network, addr)
		if err != nil {
			once.Do(func() { close(refused) })
		} else {
			connected.Store(true)
		}
		return c, err
	}
	// The dialing goroutine has returned before dialAccept is restored.
	ctx, cancel := context.WithCancel(t.Context())
	done, returned := make(chan error, 1), make(chan struct{})
	go func() { defer close(returned); done <- waitForAccept(ctx, port) }()
	t.Cleanup(func() { cancel(); <-returned })
	<-refused
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("listen on %d: %v", port, err)
	}
	defer l.Close()
	if err := <-done; err != nil {
		t.Fatalf("waitForAccept: %v", err)
	}
	if !connected.Load() {
		t.Fatal("waitForAccept returned nil without a dial that connected")
	}
}

func TestWaitForAcceptRespectsContext(t *testing.T) {
	port, err := freeLocalPort()
	if err != nil {
		t.Fatalf("freeLocalPort: %v", err)
	}
	// No listener, and a context already done: waitForAccept returns ctx.Err().
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitForAccept(ctx, port); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestPortForwarderBuildOnly(t *testing.T) {
	// This test exists only to ensure the production constructor compiles
	// and the returned value satisfies the interface. It does NOT spawn
	// kubectl.
	var _ PortForwarder = NewPortForwarder()
}

func TestKubectlPortForward_StatusEndpoint(t *testing.T) {
	pf := &kubectlPortForward{port: 12345, statusPort: 12346}
	want := "http://127.0.0.1:12346"
	if got := pf.StatusEndpoint(); got != want {
		t.Fatalf("StatusEndpoint: got %q want %q", got, want)
	}
	if got := pf.Endpoint(); got != "http://127.0.0.1:12345" {
		t.Fatalf("Endpoint: got %q", got)
	}
}
