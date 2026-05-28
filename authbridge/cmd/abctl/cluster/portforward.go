package cluster

import (
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// freeLocalPort returns an unused TCP port on 127.0.0.1.
//
// There is a TOCTOU race between this call and whoever binds next; in
// practice the kubectl subprocess binds within milliseconds and we accept
// the small window of risk for a much simpler design.
func freeLocalPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("pick free port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitForAccept blocks until 127.0.0.1:port accepts a TCP connection or
// the context is cancelled. Used to detect that `kubectl port-forward`
// has finished setting up its local listener.
func waitForAccept(ctx context.Context, port int) error {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	dialer := net.Dialer{Timeout: 250 * time.Millisecond}
	for {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err == nil {
			conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s: %w", addr, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// PortForwarder is the interface the picker uses to bring up a tunnel
// to a pod's session API. Implementations may shell out, use client-go,
// or be in-process fakes for tests.
type PortForwarder interface {
	// Start brings up a forward to the pod's :9094 (the session API).
	// The returned PortForward is ready to be dialed when Start returns
	// without error.
	Start(ctx context.Context, namespace, pod string) (PortForward, error)
}

// PortForward is a live tunnel to a pod. The caller MUST Close it.
type PortForward interface {
	// Endpoint is the URL abctl points its apiclient at.
	Endpoint() string
	// LocalPort is the ephemeral 127.0.0.1 port the tunnel listens on.
	LocalPort() int
	// Wait blocks until the underlying tunnel exits and returns the exit
	// error (nil on graceful Close).
	Wait() error
	// Close terminates the tunnel.
	Close() error
}

// NewPortForwarder returns a PortForwarder that spawns `kubectl
// port-forward` subprocesses.
func NewPortForwarder() PortForwarder { return &kubectlPortForwarder{} }

// pfReadyTimeout is how long we wait for the local port to start
// accepting after spawning kubectl. Generous enough for slow clusters,
// short enough that a typo doesn't hang the UI.
const pfReadyTimeout = 5 * time.Second

type kubectlPortForwarder struct{}

func (k *kubectlPortForwarder) Start(ctx context.Context, namespace, pod string) (PortForward, error) {
	port, err := freeLocalPort()
	if err != nil {
		return nil, err
	}
	// We do NOT bind ctx to the subprocess — kubectl port-forward should
	// outlive the per-call context (which is just for the readiness
	// check). The subprocess is terminated explicitly via Close.
	cmd := exec.Command("kubectl", "port-forward",
		"-n", namespace,
		"pod/"+pod,
		strconv.Itoa(port)+":9094",
	)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start kubectl port-forward: %w", err)
	}
	pf := &kubectlPortForward{cmd: cmd, port: port, stderr: stderr}
	pf.startStderrDrain()

	readyCtx, cancel := context.WithTimeout(ctx, pfReadyTimeout)
	defer cancel()
	if err := waitForAccept(readyCtx, port); err != nil {
		// Kill before surfacing — caller doesn't get a half-alive subprocess.
		_ = pf.Close()
		stderrTail := pf.stderrTail()
		if stderrTail != "" {
			return nil, fmt.Errorf("port-forward not ready: %v: %s", err, stderrTail)
		}
		return nil, fmt.Errorf("port-forward not ready: %v", err)
	}
	return pf, nil
}

type kubectlPortForward struct {
	cmd    *exec.Cmd
	port   int
	stderr io.ReadCloser

	mu          sync.Mutex
	stderrLines []string
	stderrDone  chan struct{}
}

func (p *kubectlPortForward) Endpoint() string {
	return "http://127.0.0.1:" + strconv.Itoa(p.port)
}

func (p *kubectlPortForward) LocalPort() int { return p.port }

func (p *kubectlPortForward) Wait() error { return p.cmd.Wait() }

func (p *kubectlPortForward) Close() error {
	if p.cmd.Process == nil {
		return nil
	}
	_ = p.cmd.Process.Kill()
	// Wait so the OS reaps the child; ignore exit error (Kill produces one).
	_ = p.cmd.Wait()
	return nil
}

// startStderrDrain consumes the subprocess stderr in a goroutine and
// retains the last few lines for error reporting.
func (p *kubectlPortForward) startStderrDrain() {
	p.stderrDone = make(chan struct{})
	go func() {
		defer close(p.stderrDone)
		buf := make([]byte, 4096)
		for {
			n, err := p.stderr.Read(buf)
			if n > 0 {
				p.mu.Lock()
				for _, line := range strings.Split(strings.TrimRight(string(buf[:n]), "\n"), "\n") {
					if line == "" {
						continue
					}
					p.stderrLines = append(p.stderrLines, line)
					// Cap retention.
					if len(p.stderrLines) > 16 {
						p.stderrLines = p.stderrLines[len(p.stderrLines)-16:]
					}
				}
				p.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
}

// stderrTail returns the last few stderr lines as a single string.
func (p *kubectlPortForward) stderrTail() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.stderrLines, " | ")
}
