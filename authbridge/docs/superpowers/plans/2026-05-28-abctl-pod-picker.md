# abctl Pod Picker Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the manual `kubectl port-forward` step in front of `abctl` with a built-in Namespaces → Pods picker that auto-port-forwards on selection.

**Architecture:** New `cmd/abctl/cluster/` package shells out to `kubectl` for pod listing and port-forward subprocess management. New `paneNamespaces` and `panePods` panes are prepended to the existing pane stack so navigation feels identical to today. The `tui.Run` entry point grows a `RunOptions` struct so `main.go` can wire production implementations and tests can inject fakes.

**Tech Stack:** Go 1.24, bubbletea (existing), `os/exec` (subprocess), `kubectl` (system-installed).

**Spec:** [`docs/superpowers/specs/2026-05-28-abctl-pod-picker-design.md`](../specs/2026-05-28-abctl-pod-picker-design.md)

**Working directory for all paths below:** `authbridge/cmd/abctl/`

---

## File Structure

**New files:**
- `cluster/cluster.go` — types (`Pod`, `AgentNamespace`), `Lister` interface, kubectl impl, JSON parser.
- `cluster/cluster_test.go` — parser test with golden JSON fixture, Lister test with stub runner.
- `cluster/portforward.go` — `PortForwarder` / `PortForward` interfaces, kubectl impl, `freeLocalPort` and `waitForAccept` helpers.
- `cluster/portforward_test.go` — unit tests for `freeLocalPort` and `waitForAccept` using a real Go listener.
- `tui/namespaces_pane.go` — namespaces table model + render helpers.
- `tui/pods_pane.go` — pods table model + render helpers.
- `tui/picker_test.go` — TUI tests for both new panes using a fake `Lister` and fake `PortForwarder`.

**Modified files:**
- `tui/app.go` — add `paneNamespaces` and `panePods` to the pane enum, add `RunOptions`, refactor `Run`, add picker-mode entry path.
- `main.go` — switch to `RunOptions`, construct production cluster impls.
- `README.md` — lead with picker mode; demote `--endpoint` to power-user shortcut.

**Convention:** picker pane code follows the existing `tui/sessions_pane.go` pattern (one file per pane, each owns its `bubbles/table.Model` constructor and rebuild function; the central state machine in `tui/app.go` orchestrates).

---

## Task 1: Cluster types and JSON parser

Add the data types and a pure JSON-parsing function. No subprocess yet — this task only proves we can decode `kubectl get pods -A -o json` output, filter by container name, and group by namespace.

**Files:**
- Create: `authbridge/cmd/abctl/cluster/cluster.go`
- Create: `authbridge/cmd/abctl/cluster/cluster_test.go`

- [ ] **Step 1: Write the failing test**

Create `cluster/cluster_test.go`:

```go
package cluster

import (
	"strings"
	"testing"
)

// fixturePodsJSON is a stripped-down `kubectl get pods -A -o json` payload
// covering: a matching pod (authbridge-proxy sidecar), a non-matching pod
// (no authbridge container), and a pod in a second namespace with a
// different sidecar variant (authbridge-envoy).
const fixturePodsJSON = `{
  "items": [
    {
      "metadata": {"namespace": "team1", "name": "weather-agent-1"},
      "spec": {"containers": [{"name": "agent"}, {"name": "authbridge-proxy"}]},
      "status": {"phase": "Running",
                 "containerStatuses": [{"ready": true}, {"ready": true}]}
    },
    {
      "metadata": {"namespace": "team1", "name": "unrelated-1"},
      "spec": {"containers": [{"name": "app"}]},
      "status": {"phase": "Running",
                 "containerStatuses": [{"ready": true}]}
    },
    {
      "metadata": {"namespace": "team2", "name": "billing-agent-1"},
      "spec": {"containers": [{"name": "agent"}, {"name": "authbridge-envoy"}]},
      "status": {"phase": "Pending",
                 "containerStatuses": [{"ready": false}, {"ready": false}]}
    }
  ]
}`

func TestParseAgentPods(t *testing.T) {
	got, err := parseAgentPods([]byte(fixturePodsJSON))
	if err != nil {
		t.Fatalf("parseAgentPods: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 namespaces, got %d", len(got))
	}
	// Namespaces must be sorted alphabetically.
	if got[0].Name != "team1" || got[1].Name != "team2" {
		t.Fatalf("namespace order wrong: %q, %q", got[0].Name, got[1].Name)
	}
	if len(got[0].Pods) != 1 {
		t.Fatalf("team1: want 1 pod, got %d", len(got[0].Pods))
	}
	if got[0].Pods[0].Name != "weather-agent-1" {
		t.Fatalf("team1 pod name: got %q", got[0].Pods[0].Name)
	}
	if !got[0].Pods[0].Ready {
		t.Fatalf("team1 pod should be Ready")
	}
	if got[1].Pods[0].Ready {
		t.Fatalf("team2 pod should NOT be Ready")
	}
}

func TestParseAgentPodsRejectsBadJSON(t *testing.T) {
	_, err := parseAgentPods([]byte("not json"))
	if err == nil {
		t.Fatal("want error on bad JSON")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Fatalf("error should mention decode failure, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:
```bash
cd authbridge/cmd/abctl
go test ./cluster/ -run TestParseAgentPods -v
```
Expected: build failure (`cluster` package does not exist yet).

- [ ] **Step 3: Write minimal implementation**

Create `cluster/cluster.go`:

```go
// Package cluster talks to a Kubernetes cluster on behalf of abctl's
// picker UI. The production implementation shells out to `kubectl`;
// tests inject a stub command runner.
package cluster

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// sidecarContainerNames is the set of container names that mark a pod as
// an AuthBridge agent. Container names are the operator's stable contract;
// labels are not (the operator may evolve them).
var sidecarContainerNames = map[string]struct{}{
	"authbridge-proxy":  {},
	"authbridge-envoy":  {},
	"authbridge-lite":   {},
}

// Pod is the slice of pod state the picker UI cares about.
type Pod struct {
	Namespace string
	Name      string
	Phase     string
	Ready     bool      // true when every container in containerStatuses is ready
	StartedAt time.Time // status.startTime; zero if absent
}

// AgentNamespace is a namespace plus the AuthBridge-bearing pods inside it.
type AgentNamespace struct {
	Name string
	Pods []Pod
}

// kubectlPodList mirrors the JSON shape of `kubectl get pods -A -o json`.
// Only the fields we use are decoded.
type kubectlPodList struct {
	Items []struct {
		Metadata struct {
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Containers []struct {
				Name string `json:"name"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			Phase             string    `json:"phase"`
			StartTime         time.Time `json:"startTime"`
			ContainerStatuses []struct {
				Ready bool `json:"ready"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

// parseAgentPods filters `kubectl get pods -A -o json` output to AuthBridge
// agents and groups them by namespace, sorted alphabetically.
func parseAgentPods(raw []byte) ([]AgentNamespace, error) {
	var list kubectlPodList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decode kubectl pod list: %w", err)
	}
	byNs := map[string][]Pod{}
	for _, item := range list.Items {
		hasSidecar := false
		for _, c := range item.Spec.Containers {
			if _, ok := sidecarContainerNames[c.Name]; ok {
				hasSidecar = true
				break
			}
		}
		if !hasSidecar {
			continue
		}
		ready := len(item.Status.ContainerStatuses) > 0
		for _, cs := range item.Status.ContainerStatuses {
			if !cs.Ready {
				ready = false
				break
			}
		}
		byNs[item.Metadata.Namespace] = append(byNs[item.Metadata.Namespace], Pod{
			Namespace: item.Metadata.Namespace,
			Name:      item.Metadata.Name,
			Phase:     item.Status.Phase,
			Ready:     ready,
			StartedAt: item.Status.StartTime,
		})
	}
	out := make([]AgentNamespace, 0, len(byNs))
	for ns, pods := range byNs {
		sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
		out = append(out, AgentNamespace{Name: ns, Pods: pods})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run:
```bash
cd authbridge/cmd/abctl
go test ./cluster/ -v
```
Expected: `TestParseAgentPods` and `TestParseAgentPodsRejectsBadJSON` both PASS.

- [ ] **Step 5: Commit**

```bash
git add authbridge/cmd/abctl/cluster/cluster.go authbridge/cmd/abctl/cluster/cluster_test.go
git commit -s -m "feat(abctl): Add cluster package with kubectl pod-list parser

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 2: Lister interface with kubectl shelling

Wrap the parser in a `Lister` interface that shells out to `kubectl get pods -A -o json`. Production wiring uses `os/exec`; the test uses an injected runner closure that returns the golden JSON.

**Files:**
- Modify: `authbridge/cmd/abctl/cluster/cluster.go` (append)
- Modify: `authbridge/cmd/abctl/cluster/cluster_test.go` (append)

- [ ] **Step 1: Write the failing test**

Append to `cluster/cluster_test.go`:

```go
import "context"

func TestKubectlListerListAgents(t *testing.T) {
	stub := func(ctx context.Context, args ...string) ([]byte, error) {
		// Verify the args we pass kubectl.
		want := []string{"get", "pods", "-A", "-o", "json"}
		if len(args) != len(want) {
			t.Fatalf("kubectl args: got %v want %v", args, want)
		}
		for i := range want {
			if args[i] != want[i] {
				t.Fatalf("kubectl args[%d]: got %q want %q", i, args[i], want[i])
			}
		}
		return []byte(fixturePodsJSON), nil
	}
	l := &kubectlLister{run: stub}
	got, err := l.ListAgents(context.Background())
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 namespaces, got %d", len(got))
	}
}

func TestKubectlListerSurfacesRunnerError(t *testing.T) {
	stub := func(ctx context.Context, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("kubectl: forbidden")
	}
	l := &kubectlLister{run: stub}
	_, err := l.ListAgents(context.Background())
	if err == nil {
		t.Fatal("want error from runner to surface")
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("error should include runner output, got %v", err)
	}
}
```

Update the import block at the top of the test file to include `"context"` and `"fmt"`.

- [ ] **Step 2: Run test to verify it fails**

Run:
```bash
cd authbridge/cmd/abctl
go test ./cluster/ -run TestKubectlLister -v
```
Expected: build failure (`kubectlLister` undefined).

- [ ] **Step 3: Write minimal implementation**

Append to `cluster/cluster.go`:

```go
import (
	"context"
	"os/exec"
)

// runner abstracts a `kubectl <args>` invocation. Production uses os/exec;
// tests inject their own.
type runner func(ctx context.Context, args ...string) ([]byte, error)

// defaultRunner shells out to the system `kubectl`.
func defaultRunner(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	out, err := cmd.Output()
	if err != nil {
		// exec.ExitError carries the stderr we want to surface.
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("kubectl: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("kubectl: %w", err)
	}
	return out, nil
}

// Lister enumerates AuthBridge-bearing pods in the cluster, grouped by
// namespace. Implementations are safe to call concurrently from a single
// goroutine context — the picker calls ListAgents once per pane entry.
type Lister interface {
	ListAgents(ctx context.Context) ([]AgentNamespace, error)
}

// NewLister returns a Lister that shells out to the system `kubectl`.
func NewLister() Lister { return &kubectlLister{run: defaultRunner} }

type kubectlLister struct{ run runner }

func (l *kubectlLister) ListAgents(ctx context.Context) ([]AgentNamespace, error) {
	out, err := l.run(ctx, "get", "pods", "-A", "-o", "json")
	if err != nil {
		return nil, err
	}
	return parseAgentPods(out)
}
```

Note: the existing imports in `cluster.go` (`encoding/json`, `fmt`, `sort`, `time`) need `"context"`, `"os/exec"`, and `"strings"` added.

- [ ] **Step 4: Run test to verify it passes**

Run:
```bash
cd authbridge/cmd/abctl
go test ./cluster/ -v
```
Expected: all four tests PASS.

- [ ] **Step 5: Commit**

```bash
git add authbridge/cmd/abctl/cluster/cluster.go authbridge/cmd/abctl/cluster/cluster_test.go
git commit -s -m "feat(abctl): Wrap pod-list parser in Lister interface

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 3: Local-port helpers

Two pure helpers for the port-forward path: `freeLocalPort` (asks the OS for an unused TCP port) and `waitForAccept` (blocks until 127.0.0.1:port accepts a connection or context is done). Both have unit tests using a real Go listener — no subprocess needed.

**Files:**
- Create: `authbridge/cmd/abctl/cluster/portforward.go`
- Create: `authbridge/cmd/abctl/cluster/portforward_test.go`

- [ ] **Step 1: Write the failing test**

Create `cluster/portforward_test.go`:

```go
package cluster

import (
	"context"
	"net"
	"testing"
	"time"
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
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
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
	// Open a listener on the port after a short delay; waitForAccept
	// should return nil once the dial succeeds.
	go func() {
		time.Sleep(50 * time.Millisecond)
		l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
		if err == nil {
			defer l.Close()
			// Accept one connection from waitForAccept and close.
			c, _ := l.Accept()
			if c != nil {
				c.Close()
			}
			time.Sleep(100 * time.Millisecond) // give waitForAccept room to return
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := waitForAccept(ctx, port); err != nil {
		t.Fatalf("waitForAccept: %v", err)
	}
}

func TestWaitForAcceptRespectsContext(t *testing.T) {
	port, err := freeLocalPort()
	if err != nil {
		t.Fatalf("freeLocalPort: %v", err)
	}
	// No listener — waitForAccept should return ctx.Err() once we cancel.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := waitForAccept(ctx, port); err == nil {
		t.Fatal("want context-deadline error, got nil")
	}
}

// itoa is a tiny helper to avoid importing strconv in the test file just
// for one call.
func itoa(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var buf [10]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	return string(buf[i:])
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:
```bash
cd authbridge/cmd/abctl
go test ./cluster/ -run TestFreeLocalPort -v
```
Expected: build failure (`freeLocalPort`, `waitForAccept` undefined).

- [ ] **Step 3: Write minimal implementation**

Create `cluster/portforward.go`:

```go
package cluster

import (
	"context"
	"fmt"
	"net"
	"strconv"
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
```

- [ ] **Step 4: Run test to verify it passes**

Run:
```bash
cd authbridge/cmd/abctl
go test ./cluster/ -v
```
Expected: all six tests PASS (two parser, two Lister, three port-helper).

- [ ] **Step 5: Commit**

```bash
git add authbridge/cmd/abctl/cluster/portforward.go authbridge/cmd/abctl/cluster/portforward_test.go
git commit -s -m "feat(abctl): Add freeLocalPort and waitForAccept helpers

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 4: PortForwarder interface and kubectl impl

Define the public interfaces the picker calls. The kubectl-spawning implementation orchestrates `freeLocalPort` → spawn → `waitForAccept`. The implementation itself is not unit-tested (it requires a real subprocess); the picker tests use a fake. The end-to-end smoke test (Task 9, optional) covers the real path.

**Files:**
- Modify: `authbridge/cmd/abctl/cluster/portforward.go` (append)

- [ ] **Step 1: Write the failing test**

No new test for this task — the interface is exercised by the TUI tests in Task 5 / 6 via a fake. We only need the production impl to compile.

Append a build-only test that imports the symbols, to catch obvious compile breakage:

```go
// In cluster/portforward_test.go, append:

func TestPortForwarderBuildOnly(t *testing.T) {
	// This test exists only to ensure the production constructor compiles
	// and the returned value satisfies the interface. It does NOT spawn
	// kubectl.
	var _ PortForwarder = NewPortForwarder()
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:
```bash
cd authbridge/cmd/abctl
go test ./cluster/ -run TestPortForwarderBuildOnly -v
```
Expected: build failure (`PortForwarder`, `NewPortForwarder` undefined).

- [ ] **Step 3: Write minimal implementation**

Append to `cluster/portforward.go`:

```go
import (
	"io"
	"os/exec"
	"strings"
	"sync"
)

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
```

Note: ensure the `import` block is one consolidated `import (...)` group at the top of the file.

- [ ] **Step 4: Run test to verify it passes**

Run:
```bash
cd authbridge/cmd/abctl
go test ./cluster/ -v
go vet ./cluster/
```
Expected: all tests PASS, vet clean.

- [ ] **Step 5: Commit**

```bash
git add authbridge/cmd/abctl/cluster/portforward.go authbridge/cmd/abctl/cluster/portforward_test.go
git commit -s -m "feat(abctl): Add PortForwarder interface and kubectl impl

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 5: TUI Namespaces pane

Add the first picker pane: a table of namespaces that contain at least one AuthBridge agent. Selecting a row drills into the Pods pane for that namespace. The model loads namespaces by calling `Lister.ListAgents(ctx)` once on entry.

This task introduces:
- New pane IDs `paneNamespaces`, `panePods` in the existing pane enum (so subsequent tasks don't re-edit).
- Two new fields on `*model`: a `cluster.Lister`, a `cluster.PortForwarder`, the namespaces table, and the loaded `[]cluster.AgentNamespace`.
- A new tea message `agentsLoadedMsg` carrying the result of `ListAgents`.

**Files:**
- Modify: `authbridge/cmd/abctl/tui/app.go`
- Create: `authbridge/cmd/abctl/tui/namespaces_pane.go`
- Create: `authbridge/cmd/abctl/tui/picker_test.go`

- [ ] **Step 1: Write the failing test**

Create `tui/picker_test.go`:

```go
package tui

import (
	"context"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kagenti/kagenti-extensions/authbridge/cmd/abctl/cluster"
)

// fakeLister returns a fixed []AgentNamespace.
type fakeLister struct{ namespaces []cluster.AgentNamespace }

func (f *fakeLister) ListAgents(ctx context.Context) ([]cluster.AgentNamespace, error) {
	return f.namespaces, nil
}

// fixtureNamespaces is a small, deterministic dataset for picker tests.
var fixtureNamespaces = []cluster.AgentNamespace{
	{Name: "team1", Pods: []cluster.Pod{
		{Namespace: "team1", Name: "weather-agent-1", Phase: "Running", Ready: true},
	}},
	{Name: "team2", Pods: []cluster.Pod{
		{Namespace: "team2", Name: "billing-agent-1", Phase: "Pending", Ready: false},
	}},
}

func TestNamespacesPaneLoadsAndRenders(t *testing.T) {
	m := newPickerModel(context.Background(), &fakeLister{namespaces: fixtureNamespaces}, nil)
	// Init returns a Cmd that loads the agents.
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init returned nil cmd; want loader cmd")
	}
	msg := cmd()
	loaded, ok := msg.(agentsLoadedMsg)
	if !ok {
		t.Fatalf("loader cmd produced %T, want agentsLoadedMsg", msg)
	}
	updated, _ := m.Update(loaded)
	mm := updated.(*model)
	if len(mm.namespaces) != 2 {
		t.Fatalf("model should hold 2 namespaces, got %d", len(mm.namespaces))
	}
	view := mm.View()
	if !contains(view, "team1") || !contains(view, "team2") {
		t.Fatalf("rendered view missing namespaces:\n%s", view)
	}
}

func TestNamespacesPaneDrillsIntoPods(t *testing.T) {
	m := newPickerModel(context.Background(), &fakeLister{namespaces: fixtureNamespaces}, nil)
	loaded := m.Init()()
	updated, _ := m.Update(loaded)
	mm := updated.(*model)
	// Press Enter on the first row.
	updated, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mm = updated.(*model)
	if mm.pane != panePods {
		t.Fatalf("after Enter, active pane should be panePods, got %v", mm.pane)
	}
	if mm.selectedNamespace != "team1" {
		t.Fatalf("selected namespace should be team1, got %q", mm.selectedNamespace)
	}
}

// contains is a thin wrapper over strings.Contains used to keep test
// assertions readable.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// silence unused-import nag if test build trims this file later
var _ = time.Second
```

- [ ] **Step 2: Run test to verify it fails**

Run:
```bash
cd authbridge/cmd/abctl
go test ./tui/ -run TestNamespacesPane -v
```
Expected: build failure (`newPickerModel`, `agentsLoadedMsg`, etc. undefined).

- [ ] **Step 3: Write minimal implementation**

**3a. Modify `tui/app.go`** — add pane IDs, `RunOptions`, the `agentsLoadedMsg`, the `namespaces` and picker-related fields on `model`.

In `tui/app.go`, find the `paneID` constant block (currently lines ~26-32):

```go
const (
	paneSessions paneID = iota
	paneEvents
	paneDetail
	panePipeline
	panePluginDetail
)
```

Replace it with:

```go
const (
	paneNamespaces paneID = iota
	panePods
	paneSessions
	paneEvents
	paneDetail
	panePipeline
	panePluginDetail
)
```

Find the `model` struct definition. Add these fields at the end (preserve existing fields):

```go
	// Picker dependencies and state. nil + empty when --endpoint bypasses
	// the picker.
	lister        cluster.Lister
	portForwarder cluster.PortForwarder
	namespaces    []cluster.AgentNamespace
	namespacesTbl table.Model
	podsTbl       table.Model

	selectedNamespace string // set on Enter from Namespaces pane
	selectedPod       string // set on Enter from Pods pane

	pickerErr string // single-line picker error shown in footer
```

Add the import for `cluster` to the import block:

```go
"github.com/kagenti/kagenti-extensions/authbridge/cmd/abctl/cluster"
```

Add tea messages near the other tea-message types:

```go
type agentsLoadedMsg struct {
	namespaces []cluster.AgentNamespace
	err        error
}
```

**3b. Create `tui/namespaces_pane.go`**:

```go
package tui

import (
	"context"
	"fmt"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/kagenti/kagenti-extensions/authbridge/cmd/abctl/cluster"
)

// newNamespacesTable builds an empty namespaces picker table.
func newNamespacesTable() table.Model {
	t := table.New(
		table.WithColumns([]table.Column{
			{Title: "NAMESPACE", Width: 30},
			{Title: "PODS", Width: 6},
		}),
		table.WithFocused(true),
	)
	t.SetStyles(tableStyles())
	return t
}

// rebuildNamespacesTable rebuilds rows from m.namespaces.
func (m *model) rebuildNamespacesTable() {
	rows := make([]table.Row, 0, len(m.namespaces))
	for _, ns := range m.namespaces {
		rows = append(rows, table.Row{ns.Name, fmt.Sprintf("%d", len(ns.Pods))})
	}
	m.namespacesTbl.SetRows(rows)
}

// loadAgentsCmd produces a tea.Cmd that calls Lister.ListAgents and
// emits an agentsLoadedMsg.
func loadAgentsCmd(lister cluster.Lister) tea.Cmd {
	return func() tea.Msg {
		ns, err := lister.ListAgents(context.Background())
		return agentsLoadedMsg{namespaces: ns, err: err}
	}
}

// newPickerModel constructs a model already in the Namespaces pane,
// wired with the given Lister and PortForwarder. Used when --endpoint
// is not given.
func newPickerModel(ctx context.Context, lister cluster.Lister, pf cluster.PortForwarder) *model {
	m := &model{
		lister:        lister,
		portForwarder: pf,
		pane:    paneNamespaces,
		namespacesTbl: newNamespacesTable(),
		podsTbl:       newPodsTable(),
	}
	return m
}
```

**3c. Modify `tui/app.go`** — extend `Init` and `Update` for picker mode.

Find `func (m *model) Init() tea.Cmd`. At the top of the function body, add:

```go
	if m.pane == paneNamespaces {
		// Picker mode — load agents, then idle until user picks a pod.
		return loadAgentsCmd(m.lister)
	}
```

Find `func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd)`. Add a new case in the type switch on `msg`, alongside the existing message handlers:

```go
	case agentsLoadedMsg:
		if msg.err != nil {
			m.pickerErr = msg.err.Error()
			return m, nil
		}
		m.namespaces = msg.namespaces
		m.rebuildNamespacesTable()
		return m, nil
```

Find the existing key handler (likely under `case tea.KeyMsg`). Inside, before any pane-agnostic key handling (q, ctrl+c), add a branch:

```go
		if m.pane == paneNamespaces {
			switch msg.String() {
			case "enter":
				if cur := m.namespacesTbl.Cursor(); cur < len(m.namespaces) {
					m.selectedNamespace = m.namespaces[cur].Name
					m.pane = panePods
					m.rebuildPodsTable()
				}
				return m, nil
			case "q", "esc", "ctrl+c":
				return m, tea.Quit
			}
			var cmd tea.Cmd
			m.namespacesTbl, cmd = m.namespacesTbl.Update(msg)
			return m, cmd
		}
```

Find `func (m *model) View() string`. At the top of the body, before the existing rendering, add:

```go
	if m.pane == paneNamespaces {
		title := "abctl · pick namespace"
		body := m.namespacesTbl.View()
		footer := "[↑↓/jk] nav  [↵] open  [q] quit"
		if m.pickerErr != "" {
			footer = "error: " + m.pickerErr + "    " + footer
		}
		return title + "\n\n" + body + "\n" + footer
	}
```

(The exact wrappers should mirror the existing `View()` style — wrap with `lipgloss.JoinVertical` or the helper used by the other panes. Match the convention you see in the surrounding code.)

**3d. Add a stub `newPodsTable` and `rebuildPodsTable` so the test compiles.** These get fleshed out in Task 6.

In `tui/pods_pane.go` (create file):

```go
package tui

import "github.com/charmbracelet/bubbles/table"

// newPodsTable is the stub for the Pods picker table; populated in Task 6.
func newPodsTable() table.Model {
	t := table.New(
		table.WithColumns([]table.Column{{Title: "POD", Width: 40}}),
		table.WithFocused(true),
	)
	t.SetStyles(tableStyles())
	return t
}

// rebuildPodsTable populates pod rows for m.selectedNamespace; fleshed
// out in Task 6.
func (m *model) rebuildPodsTable() {
	// Stub — Task 6 fills this in.
}
```

- [ ] **Step 4: Run test to verify it passes**

Run:
```bash
cd authbridge/cmd/abctl
go build ./...
go test ./tui/ -run TestNamespacesPane -v
```
Expected: build clean, both `TestNamespacesPaneLoadsAndRenders` and `TestNamespacesPaneDrillsIntoPods` PASS.

If existing TUI tests break (e.g. e2e_test.go expected `paneSessions` to be `iota = 0`), update those tests to use named constants instead of integer literals where applicable.

- [ ] **Step 5: Commit**

```bash
git add authbridge/cmd/abctl/tui/app.go authbridge/cmd/abctl/tui/namespaces_pane.go authbridge/cmd/abctl/tui/pods_pane.go authbridge/cmd/abctl/tui/picker_test.go
git commit -s -m "feat(abctl): Add Namespaces picker pane

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 6: TUI Pods pane

Flesh out the Pods pane: list the pods in the selected namespace, allow selection. Selection in this task only sets `m.selectedPod` and remembers user intent — the actual port-forward + transition to the session view comes in Task 7.

**Files:**
- Modify: `authbridge/cmd/abctl/tui/pods_pane.go`
- Modify: `authbridge/cmd/abctl/tui/app.go` (add panePods key handler + view branch)
- Modify: `authbridge/cmd/abctl/tui/picker_test.go` (append)

- [ ] **Step 1: Write the failing test**

Append to `tui/picker_test.go`:

```go
func TestPodsPaneListsPods(t *testing.T) {
	m := newPickerModel(context.Background(), &fakeLister{namespaces: fixtureNamespaces}, nil)
	loaded := m.Init()()
	updated, _ := m.Update(loaded)
	mm := updated.(*model)
	// Drill into team1.
	updated, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mm = updated.(*model)
	view := mm.View()
	if !contains(view, "weather-agent-1") {
		t.Fatalf("Pods view missing pod name:\n%s", view)
	}
	if !contains(view, "Running") {
		t.Fatalf("Pods view missing phase column:\n%s", view)
	}
}

func TestPodsPaneEscBacksOut(t *testing.T) {
	m := newPickerModel(context.Background(), &fakeLister{namespaces: fixtureNamespaces}, nil)
	updated, _ := m.Update(m.Init()())
	mm := updated.(*model)
	updated, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEnter}) // → panePods
	updated, _ = updated.(*model).Update(tea.KeyMsg{Type: tea.KeyEsc})
	mm = updated.(*model)
	if mm.pane != paneNamespaces {
		t.Fatalf("Esc should back out to Namespaces, got pane %v", mm.pane)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:
```bash
cd authbridge/cmd/abctl
go test ./tui/ -run TestPodsPane -v
```
Expected: tests fail (Pods pane render is stub; Esc handler missing).

- [ ] **Step 3: Write minimal implementation**

**3a. Replace `tui/pods_pane.go` body:**

```go
package tui

import (
	"github.com/charmbracelet/bubbles/table"

	"github.com/kagenti/kagenti-extensions/authbridge/cmd/abctl/cluster"
)

// newPodsTable builds an empty pods picker table.
func newPodsTable() table.Model {
	t := table.New(
		table.WithColumns([]table.Column{
			{Title: "POD", Width: 40},
			{Title: "PHASE", Width: 10},
			{Title: "READY", Width: 6},
		}),
		table.WithFocused(true),
	)
	t.SetStyles(tableStyles())
	return t
}

// rebuildPodsTable rebuilds rows from m.namespaces[selected].Pods.
func (m *model) rebuildPodsTable() {
	var pods []cluster.Pod
	for _, ns := range m.namespaces {
		if ns.Name == m.selectedNamespace {
			pods = ns.Pods
			break
		}
	}
	rows := make([]table.Row, 0, len(pods))
	for _, p := range pods {
		ready := "no"
		if p.Ready {
			ready = "yes"
		}
		rows = append(rows, table.Row{p.Name, p.Phase, ready})
	}
	m.podsTbl.SetRows(rows)
}

// currentPodsList returns the slice of pods backing the Pods pane,
// keyed by the currently-selected namespace. Used for selection lookup.
func (m *model) currentPodsList() []cluster.Pod {
	for _, ns := range m.namespaces {
		if ns.Name == m.selectedNamespace {
			return ns.Pods
		}
	}
	return nil
}
```

**3b. Modify `tui/app.go`** — add the Pods pane key handler. Find the picker-mode `case tea.KeyMsg` branch from Task 5 and extend it with a panePods branch:

```go
		if m.pane == panePods {
			switch msg.String() {
			case "enter":
				pods := m.currentPodsList()
				if cur := m.podsTbl.Cursor(); cur < len(pods) {
					if !pods[cur].Ready {
						m.pickerErr = "pod not Ready"
						return m, nil
					}
					m.selectedPod = pods[cur].Name
					// Task 7 wires the port-forward + session-view transition.
					return m, nil
				}
				return m, nil
			case "esc":
				m.pane = paneNamespaces
				m.pickerErr = ""
				return m, nil
			case "q", "ctrl+c":
				return m, tea.Quit
			}
			var cmd tea.Cmd
			m.podsTbl, cmd = m.podsTbl.Update(msg)
			return m, cmd
		}
```

**3c. Modify `tui/app.go`** — add the Pods pane View branch. After the Namespaces View branch from Task 5, add:

```go
	if m.pane == panePods {
		title := "abctl · " + m.selectedNamespace + " · pick pod"
		body := m.podsTbl.View()
		footer := "[↑↓/jk] nav  [↵] connect  [Esc] back  [q] quit"
		if m.pickerErr != "" {
			footer = "error: " + m.pickerErr + "    " + footer
		}
		return title + "\n\n" + body + "\n" + footer
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run:
```bash
cd authbridge/cmd/abctl
go test ./tui/ -run TestPodsPane -v
go test ./tui/ -run TestNamespacesPane -v
```
Expected: all four picker tests PASS. Existing TUI tests remain green:

```bash
go test ./tui/ -v
```

- [ ] **Step 5: Commit**

```bash
git add authbridge/cmd/abctl/tui/pods_pane.go authbridge/cmd/abctl/tui/app.go authbridge/cmd/abctl/tui/picker_test.go
git commit -s -m "feat(abctl): Add Pods picker pane

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 7: Port-forward + transition into session view

Wire the Enter-on-pod action to (a) call `PortForwarder.Start`, (b) record the resulting endpoint, (c) transition the model to `paneSessions` reusing all existing TUI state — same program, no second `tea.Program`.

**Files:**
- Modify: `authbridge/cmd/abctl/tui/app.go`
- Modify: `authbridge/cmd/abctl/tui/pods_pane.go`
- Modify: `authbridge/cmd/abctl/tui/picker_test.go` (append)

- [ ] **Step 1: Write the failing test**

Append to `tui/picker_test.go`:

```go
// fakePortForwarder returns a no-op PortForward.
type fakePortForwarder struct {
	startedNs  string
	startedPod string
	endpoint   string
	startErr   error
	closeCount int
}

func (f *fakePortForwarder) Start(ctx context.Context, ns, pod string) (cluster.PortForward, error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	f.startedNs, f.startedPod = ns, pod
	return &fakePortForward{endpoint: f.endpoint, parent: f}, nil
}

type fakePortForward struct {
	endpoint string
	parent   *fakePortForwarder
}

func (p *fakePortForward) Endpoint() string  { return p.endpoint }
func (p *fakePortForward) LocalPort() int    { return 0 }
func (p *fakePortForward) Wait() error       { return nil }
func (p *fakePortForward) Close() error      { p.parent.closeCount++; return nil }

func TestPodEnterStartsPortForwardAndTransitions(t *testing.T) {
	pf := &fakePortForwarder{endpoint: "http://127.0.0.1:60000"}
	m := newPickerModel(context.Background(), &fakeLister{namespaces: fixtureNamespaces}, pf)
	updated, _ := m.Update(m.Init()())
	mm := updated.(*model)
	updated, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEnter}) // → panePods
	mm = updated.(*model)
	updated, cmd := mm.Update(tea.KeyMsg{Type: tea.KeyEnter}) // start PF
	mm = updated.(*model)
	if cmd == nil {
		t.Fatal("Enter on pod should produce a Cmd to start the PF")
	}
	msg := cmd()
	conn, ok := msg.(portForwardReadyMsg)
	if !ok {
		t.Fatalf("PF cmd produced %T, want portForwardReadyMsg", msg)
	}
	updated, _ = mm.Update(conn)
	mm = updated.(*model)
	if mm.pane != paneSessions {
		t.Fatalf("after PF ready, pane should be paneSessions, got %v", mm.pane)
	}
	if mm.endpoint != "http://127.0.0.1:60000" {
		t.Fatalf("model endpoint not set: %q", mm.endpoint)
	}
	if pf.startedNs != "team1" || pf.startedPod != "weather-agent-1" {
		t.Fatalf("PortForwarder.Start not called with selection: ns=%q pod=%q", pf.startedNs, pf.startedPod)
	}
}

func TestPodEnterSurfacesPortForwardError(t *testing.T) {
	pf := &fakePortForwarder{startErr: fmt.Errorf("forbidden")}
	m := newPickerModel(context.Background(), &fakeLister{namespaces: fixtureNamespaces}, pf)
	updated, _ := m.Update(m.Init()())
	mm := updated.(*model)
	updated, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEnter}) // → panePods
	mm = updated.(*model)
	updated, cmd := mm.Update(tea.KeyMsg{Type: tea.KeyEnter}) // start PF
	mm = updated.(*model)
	updated, _ = mm.Update(cmd())
	mm = updated.(*model)
	if mm.pane != panePods {
		t.Fatalf("PF error should keep us on panePods, got %v", mm.pane)
	}
	if !contains(mm.pickerErr, "forbidden") {
		t.Fatalf("error not surfaced in pickerErr: %q", mm.pickerErr)
	}
}
```

The test imports `"fmt"` — add it to the import block.

- [ ] **Step 2: Run test to verify it fails**

Run:
```bash
cd authbridge/cmd/abctl
go test ./tui/ -run TestPodEnter -v
```
Expected: `portForwardReadyMsg` undefined; tests fail to build.

- [ ] **Step 3: Write minimal implementation**

**3a. Modify `tui/app.go`** — add the new tea message:

```go
type portForwardReadyMsg struct {
	pf       cluster.PortForward
	endpoint string
	err      error
}
```

Add a field to `*model` to hold the live PF (so we can Close on quit / pod-switch):

```go
	activePF cluster.PortForward
```

**3b. Modify `tui/pods_pane.go`** — add a Cmd factory:

```go
// startPortForwardCmd produces a Cmd that calls PortForwarder.Start and
// emits a portForwardReadyMsg.
func startPortForwardCmd(pf cluster.PortForwarder, ns, pod string) tea.Cmd {
	return func() tea.Msg {
		conn, err := pf.Start(context.Background(), ns, pod)
		if err != nil {
			return portForwardReadyMsg{err: err}
		}
		return portForwardReadyMsg{pf: conn, endpoint: conn.Endpoint()}
	}
}
```

(Add `"context"` and `tea "github.com/charmbracelet/bubbletea"` to the imports if not already present.)

**3c. Modify `tui/app.go`** — replace the placeholder Enter-on-pod handler from Task 6:

```go
			case "enter":
				pods := m.currentPodsList()
				if cur := m.podsTbl.Cursor(); cur < len(pods) {
					if !pods[cur].Ready {
						m.pickerErr = "pod not Ready"
						return m, nil
					}
					m.selectedPod = pods[cur].Name
					// Tear down the previous PF, if any, before starting a new one.
					if m.activePF != nil {
						_ = m.activePF.Close()
						m.activePF = nil
					}
					m.pickerErr = ""
					return m, startPortForwardCmd(m.portForwarder, m.selectedNamespace, m.selectedPod)
				}
				return m, nil
```

Add a `portForwardReadyMsg` case to the `Update` type switch:

```go
	case portForwardReadyMsg:
		if msg.err != nil {
			m.pickerErr = "port-forward: " + msg.err.Error()
			return m, nil
		}
		m.activePF = msg.pf
		m.endpoint = msg.endpoint
		m.pane = paneSessions
		// Re-init the session-view side of the model so SSE/refresh kick in.
		return m, m.initSessionView()
```

The current `Init()` body in `tui/app.go` is:

```go
func (m *model) Init() tea.Cmd {
	m.streamCh = m.client.Stream(m.ctx, "")
	return tea.Batch(
		m.loadSessionsCmd(),
		m.loadPipelineCmd(),
		streamPump(m.streamCh),
		tickCmd(),
		refreshTickCmd(),
	)
}
```

Refactor it into two functions — `initSessionView` does the work, and `Init` dispatches by pane:

```go
// initSessionView fires the session-view bootstrap: SSE pump, first
// fetch, ticks. Caller must have set m.client and m.ctx.
func (m *model) initSessionView() tea.Cmd {
	m.streamCh = m.client.Stream(m.ctx, "")
	return tea.Batch(
		m.loadSessionsCmd(),
		m.loadPipelineCmd(),
		streamPump(m.streamCh),
		tickCmd(),
		refreshTickCmd(),
	)
}

func (m *model) Init() tea.Cmd {
	if m.pane == paneNamespaces {
		return loadAgentsCmd(m.lister)
	}
	return m.initSessionView()
}
```

The session-view path needs `m.client` and `m.ctx` set before `initSessionView` runs. In bypass mode `New(ctx, c)` sets both today (preserved by Task 8). In picker mode, the `portForwardReadyMsg` handler must do it before transitioning — adjust the handler shown above:

```go
	case portForwardReadyMsg:
		if msg.err != nil {
			m.pickerErr = "port-forward: " + msg.err.Error()
			return m, nil
		}
		m.activePF = msg.pf
		m.endpoint = msg.endpoint
		m.client = apiclient.New(m.endpoint)
		m.pane = paneSessions
		return m, m.initSessionView()
```

The picker model also needs the session-view tables and caches initialized up front — otherwise the first render after transition crashes on a nil map. Update `newPickerModel` in Task 5 (or here, if you're catching it now) to set every field that the existing `New(ctx, *apiclient.Client)` constructor sets, EXCEPT `client`, `streamCh`, and `endpoint`. The relevant body of `New` lives at `tui/app.go` ~lines 152-174:

```go
ti := textinput.New()
ti.Placeholder = "filter…"
ti.Prompt = "/ "

return &model{
    // endpoint and client set later by portForwardReadyMsg handler in picker mode
    ctx:         ctx,
    cancel:      cancel,
    events:      make(map[string][]pipeline.SessionEvent),
    pane:        paneNamespaces,  // picker mode entry; bypass uses paneSessions
    sessionsTbl: newSessionsTable(),
    eventsTbl:   newEventsTable(),
    pipelineTbl: newPipelineTable(),
    detailVp:    viewport.New(0, 0),
    filterInput: ti,
    lastTick:    time.Now(),
    connState:   connStateInfo{phase: connConnecting},

    // Picker-only:
    lister:        lister,
    portForwarder: pf,
    namespacesTbl: newNamespacesTable(),
    podsTbl:       newPodsTable(),
}
```

Mirror this exactly in `newPickerModel` and pass `ctx` through (extend its signature: `newPickerModel(ctx context.Context, lister, pf)`).

**3d. Ensure cleanup on quit.** In `Run` (Task 8 will refactor this in detail), make sure that whenever the program returns, `m.activePF.Close()` runs if non-nil. For now, add a `defer` inside the existing `Run` function before `tea.NewProgram(...)` returns:

```go
defer func() {
	// m is closed over from outer scope where the model is constructed.
	if m.activePF != nil {
		_ = m.activePF.Close()
	}
}()
```

(This works because the model's `activePF` is the live one when `tea.NewProgram` returns.)

- [ ] **Step 4: Run test to verify it passes**

Run:
```bash
cd authbridge/cmd/abctl
go test ./tui/ -run TestPodEnter -v
go test ./tui/ -v
```
Expected: both new tests PASS, all other TUI tests still PASS.

- [ ] **Step 5: Commit**

```bash
git add authbridge/cmd/abctl/tui/app.go authbridge/cmd/abctl/tui/pods_pane.go authbridge/cmd/abctl/tui/picker_test.go
git commit -s -m "feat(abctl): Wire pod selection to port-forward and transition to session view

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 8: RunOptions + main.go wiring

Refactor `tui.Run` to take `RunOptions`. `main.go` constructs the production `cluster.Lister` / `cluster.PortForwarder` and decides which mode to enter based on whether `--endpoint` was given.

**Files:**
- Modify: `authbridge/cmd/abctl/tui/app.go` (signature of `Run`)
- Modify: `authbridge/cmd/abctl/main.go`
- Modify: `authbridge/cmd/abctl/tui/picker_test.go` (append a sanity test)

- [ ] **Step 1: Write the failing test**

Append to `tui/picker_test.go`:

```go
func TestRunOptionsWiringEndpointBypass(t *testing.T) {
	// Endpoint set → no Lister/PF needed; the function should not panic
	// and should return promptly when the context is cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	opts := RunOptions{Endpoint: "http://127.0.0.1:1"}
	// Run will exit because ctx is already cancelled; we just verify
	// it doesn't dereference nil Lister/PortForwarder.
	_ = Run(ctx, opts)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:
```bash
cd authbridge/cmd/abctl
go build ./...
```
Expected: `RunOptions` undefined.

- [ ] **Step 3: Write minimal implementation**

**3a. Modify `tui/app.go`** — replace the existing `Run` signature:

```go
// RunOptions selects the entry mode for abctl's TUI.
//
// If Endpoint is non-empty, abctl skips the picker and connects directly
// to that URL — preserving the pre-picker behavior (and the documented
// `--endpoint` flag).
//
// Otherwise, abctl uses Lister + PortForwarder to render the picker.
// Both must be non-nil in picker mode.
type RunOptions struct {
	Endpoint      string
	Lister        cluster.Lister
	PortForwarder cluster.PortForwarder
}

// Run starts the bubbletea program. See RunOptions.
func Run(ctx context.Context, opts RunOptions) error {
	var m *model
	if opts.Endpoint != "" {
		m = &model{
			endpoint:   opts.Endpoint,
			pane: paneSessions,
			// ... whatever the existing Run() initialized; preserve that ...
		}
	} else {
		m = newPickerModel(ctx, opts.Lister, opts.PortForwarder)
	}
	defer func() {
		if m.activePF != nil {
			_ = m.activePF.Close()
		}
	}()
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	return err
}
```

The bypass-mode model construction must mirror what the *old* `Run` did (the existing body of the function before this PR). Read the current `Run` body and copy its `model{...}` initialization here.

**3b. Modify `main.go`** — wire `RunOptions`:

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/kagenti/kagenti-extensions/authbridge/cmd/abctl/cluster"
	"github.com/kagenti/kagenti-extensions/authbridge/cmd/abctl/tui"
)

func main() {
	endpoint := flag.String("endpoint", "",
		"AuthBridge session API URL (e.g. http://localhost:9094). When omitted, abctl opens a Namespaces → Pods picker.")
	flag.Parse()

	// Friendly check: if picker mode and no kubectl, fail fast with a
	// clear message instead of a stack trace later.
	if *endpoint == "" {
		if _, err := exec.LookPath("kubectl"); err != nil {
			fmt.Fprintln(os.Stderr, "abctl: kubectl not found on PATH; install it or pass --endpoint http://...")
			os.Exit(1)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		cancel()
	}()

	opts := tui.RunOptions{Endpoint: *endpoint}
	if *endpoint == "" {
		opts.Lister = cluster.NewLister()
		opts.PortForwarder = cluster.NewPortForwarder()
	}
	if err := tui.Run(ctx, opts); err != nil {
		fmt.Fprintf(os.Stderr, "abctl: %v\n", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 4: Run tests + build the binary**

Run:
```bash
cd authbridge/cmd/abctl
go build ./...
go test ./...
```
Expected: all tests PASS, `go build` clean.

Smoke-build the binary and check `--help`:
```bash
go build -o /tmp/abctl-pickertest .
/tmp/abctl-pickertest --help 2>&1 | head -5
```
Expected: usage line mentions `--endpoint` with the new "When omitted..." text.

- [ ] **Step 5: Commit**

```bash
git add authbridge/cmd/abctl/tui/app.go authbridge/cmd/abctl/main.go authbridge/cmd/abctl/tui/picker_test.go
git commit -s -m "feat(abctl): Wire picker mode through RunOptions

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 9: README update

Update the user-facing docs to lead with picker mode and demote `--endpoint` to a power-user shortcut.

**Files:**
- Modify: `authbridge/cmd/abctl/README.md`

- [ ] **Step 1: Read the current README to identify sections to rewrite**

```bash
cd authbridge/cmd/abctl
cat README.md
```

The "Run" section is the main thing to rewrite; the ASCII screenshot + "Quit with q or Ctrl+C" survive unchanged.

- [ ] **Step 2: Rewrite the Run section**

Replace the existing "## Run" section with:

```markdown
## Run

`abctl` discovers AuthBridge agents in your current `kubectl` context
and lets you pick one:

```sh
./abctl
```

You'll see a Namespaces pane listing each namespace that contains an
AuthBridge agent. Enter drills into the Pods pane for that namespace;
Enter on a pod starts a `kubectl port-forward` automatically and drops
you into the session-events view. Esc backs out. `q` (or Ctrl+C) quits
and tears the port-forward down.

The picker shells out to `kubectl` — whatever context you're in is the
context abctl uses. There's no separate auth.

### Power-user / scripting bypass

Pass `--endpoint` to skip the picker entirely:

```sh
kubectl port-forward -n team1 pod/weather-agent-xxxx 9094:9094 &
./abctl --endpoint http://localhost:9094
```

This preserves the pre-picker behavior for scripts, CI, or remote
session APIs that aren't in your kube context.
```

Update the keybindings table to add the picker keys near the top:

```markdown
| `↑ ↓` / `k j` | picker, list | navigate rows |
| `Enter` | namespaces | open the namespace |
| `Enter` | pods | port-forward + connect |
| `Esc` | pods | back to namespaces |
| `r` | namespaces, pods | reload from cluster |
```

(Insert these as the first rows of the existing table.)

- [ ] **Step 3: Verify the rendered Markdown reads cleanly**

```bash
cd authbridge/cmd/abctl
head -80 README.md
```

Confirm the new Run section flows well and the keybindings table is intact.

- [ ] **Step 4: Commit**

```bash
git add authbridge/cmd/abctl/README.md
git commit -s -m "docs(abctl): Lead with built-in pod picker; demote --endpoint

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 10 (optional): End-to-end smoke test

Behind a build tag, drive the full path: real cluster (kind), real `kubectl`, real port-forward, real session API. Skipped by default; run manually when needed.

**Files:**
- Create: `authbridge/cmd/abctl/cluster/e2e_test.go`

- [ ] **Step 1: Write the test**

```go
//go:build e2e

package cluster

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestE2ESmoke runs against the active kubectl context. Set up by
// running the IBAC demo (make demo-ibac) before invoking:
//
//   go test -tags=e2e ./cluster/ -run TestE2ESmoke -v
//
// The test fails clearly if no AuthBridge agent is found.
func TestE2ESmoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	lister := NewLister()
	groups, err := lister.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(groups) == 0 {
		t.Fatal("no AuthBridge namespaces found; run `make demo-ibac` first")
	}
	var ns, pod string
	for _, g := range groups {
		for _, p := range g.Pods {
			if p.Ready {
				ns, pod = g.Name, p.Name
				break
			}
		}
		if pod != "" {
			break
		}
	}
	if pod == "" {
		t.Fatal("no Ready AuthBridge pod found")
	}

	pf, err := NewPortForwarder().Start(ctx, ns, pod)
	if err != nil {
		t.Fatalf("Start port-forward: %v", err)
	}
	defer pf.Close()

	resp, err := http.Get(pf.Endpoint() + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("/healthz status: %d", resp.StatusCode)
	}
	if !strings.HasPrefix(pf.Endpoint(), "http://127.0.0.1:") {
		t.Fatalf("unexpected endpoint shape: %s", pf.Endpoint())
	}
}
```

- [ ] **Step 2: Run with the build tag**

```bash
cd authbridge/cmd/abctl
go test -tags=e2e ./cluster/ -run TestE2ESmoke -v
```
Expected on a cluster with the IBAC demo running: PASS. On a bare cluster: clear failure message about running `make demo-ibac`.

Skip this task if no kind cluster with a demo is available — the design's correctness is covered by Tasks 1-7.

- [ ] **Step 3: Commit**

```bash
git add authbridge/cmd/abctl/cluster/e2e_test.go
git commit -s -m "test(abctl): Add opt-in e2e smoke test for picker path

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Self-Review Checklist (run mentally before claiming the plan is done)

**Spec coverage:**
- ✅ Pane stack (Namespaces → Pods → Sessions...): Tasks 5, 6, 7 build it.
- ✅ AuthBridge-pod detection by container name: Task 1's `sidecarContainerNames`.
- ✅ Free ephemeral port: Task 3's `freeLocalPort`.
- ✅ One PF alive at a time, kill on switch: Task 7's `m.activePF` + Close on Enter.
- ✅ PF cleanup on quit: Task 7's `defer` in `Run` and Task 8's `RunOptions`-aware version.
- ✅ 5s readiness timeout: Task 4's `pfReadyTimeout`.
- ✅ `--endpoint` bypass: Task 8's `RunOptions.Endpoint` short-circuit.
- ✅ kubectl-not-found preflight: Task 8 main.go.
- ✅ Empty-cluster empty state: implicit in Task 5's namespaces table — pickup an explicit footer hint as part of polishing in Task 5 step 3 if missing.
- ✅ `r` reload key: documented in README (Task 9). NOTE: the implementation must add this key to the namespaces and pods key handlers in Task 5/6 — verify it lands.
- ✅ Tests for cluster package: Tasks 1, 2, 3 unit tests; Task 4 build-only test.
- ✅ TUI picker tests with fakes: Tasks 5, 6, 7.
- ✅ Optional e2e: Task 10.

**Type consistency:**
- `Pod`, `AgentNamespace`, `Lister`, `PortForwarder`, `PortForward`, `RunOptions`, `agentsLoadedMsg`, `portForwardReadyMsg` are spelled identically across all tasks.
- `freeLocalPort` (lowercase) and `waitForAccept` (lowercase, package-private) are consistent.

**Placeholder scan:**
- Step 3 of Task 7 references "the existing logic that today's `Init()` runs" — the engineer is expected to read `tui/app.go` to extract it. This is intentional (the existing code is in-tree) but flagged as the only "go look at the file" step.
- Step 3 of Task 8 references "whatever the existing `Run()` initialized; preserve that" — same situation.

These are not abstract placeholders; the source of truth is the current `tui/app.go`. The engineer should literally copy the existing init body into `initSessionView` rather than invent.
