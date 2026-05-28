# abctl pod picker — built-in port-forward + namespace/pod navigation

**Status:** Design — pending user review
**Date:** 2026-05-28

## Goal

Eliminate the separate-terminal `kubectl port-forward` step that abctl
users do today. When invoked with no arguments, `abctl` opens a picker
that lets the user pick a namespace, then a pod. Selecting a pod
spawns a port-forward subprocess and drops the user into the existing
session-events TUI. The picker runs as part of the same bubbletea
program, so navigation, keybindings, and styling stay consistent with
the rest of abctl.

## Non-goals

- Multi-cluster / kube-context selection UI. abctl follows whatever
  context `kubectl` is using; switching contexts is `kubectl
  config use-context` outside abctl.
- A pod *creation* or *management* UI. abctl is read-only — picker
  only lists and connects.
- Background pod-list polling. The picker refreshes on entry and on
  the `r` keybind; there's no live stream of pod state.
- Multiple concurrent port-forwards. One pod is connected at a time.
- Replacing the `--endpoint` flag. It remains as a power-user / scripting
  shortcut that bypasses the picker.

## User-facing behavior

```
$ abctl                          # opens picker
$ abctl --endpoint http://...    # bypasses picker, today's behavior unchanged
```

### Pane stack

```
Namespaces   →   Pods   →   Sessions   →   Events   →   Detail
                                          ↘ Pipeline
   ↑              ↑          (existing panes, unchanged)
   └─ new ────────┘
```

Same Enter / Esc / q vocabulary throughout. Esc from Sessions backs
to Pods; Esc from Pods backs to Namespaces; Esc/q from Namespaces
quits. Switching pods (Esc → re-enter) tears down the old port-forward
and starts a new one for the new pod.

### What pods show up

A pod counts as an AuthBridge agent if **any container in the pod
spec** has a `name` matching one of:

- `authbridge-proxy`
- `authbridge-envoy`
- `authbridge-lite`

These are the operator-set sidecar names. Detection is client-side
from the JSON returned by `kubectl get pods -A -o json`.

Container name is chosen over labels because the operator may evolve
its label scheme but the sidecar container name is a stable contract
(it's referenced from the Helm chart, the operator, this repo, and
end-user demos).

The Namespaces pane lists only those namespaces that contain at least
one matching pod. Empty cluster → empty pane with a help hint pointing
at `--endpoint`.

### Port-forward lifecycle

- On entering the Pods pane, no port-forward yet.
- On Enter on a pod row:
  1. Ask the OS for a free local port (listen on `:0`, capture port,
     close the listener).
  2. Spawn `kubectl port-forward -n <ns> pod/<name> <localPort>:9094`
     as a subprocess; capture stderr.
  3. Wait until `127.0.0.1:<localPort>` accepts a TCP connect, with a
     5-second timeout.
  4. Construct the session-events bubbletea model with
     `endpoint = "http://127.0.0.1:<localPort>"` and run.
- On switching to a different pod, kill the previous subprocess
  (`cmd.Process.Kill()`) and start a new one. Only one port-forward
  alive at any time.
- On quit, kill the subprocess (deferred in `main`).
- The subprocess is a child of the Go process so an `abctl` crash
  takes it with it (cgroup / process-tree cleanup is the OS's job).

## Architecture

```
[abctl process]
   │
   ├──── exec  ─── kubectl get ns / pods         (one-shot, JSON output)
   │
   ├──── exec  ─── kubectl port-forward ...      (long-running, one at a time)
   │                       │
   │                       ▼
   │                  127.0.0.1:<ephemeral>
   │                       │
   └────── http ───────────┘ ──▶ session API on the agent pod
```

abctl orchestrates kubectl subprocesses; the existing TUI + apiclient
keep running unchanged inside the session view.

### New package: `cmd/abctl/cluster/`

Three responsibilities behind small interfaces, each with a kubectl
exec injection point so tests can stub:

```go
package cluster

// Pod is the slice of pod state the picker needs.
type Pod struct {
    Namespace string
    Name      string
    Phase     string  // "Running", "Pending", ...
    Ready     bool    // all containers ready
    AgeStart  time.Time
}

// AgentNamespace is a namespace + the pods in it that match the
// authbridge-sidecar detection rule.
type AgentNamespace struct {
    Name string
    Pods []Pod
}

// Lister enumerates AuthBridge-bearing pods, grouped by namespace.
// Implementation shells out to `kubectl get pods -A -o json`.
type Lister interface {
    ListAgents(ctx context.Context) ([]AgentNamespace, error)
}

// PortForwarder spawns a `kubectl port-forward` subprocess and returns
// a handle. Close() kills the subprocess.
type PortForwarder interface {
    Start(ctx context.Context, ns, pod string) (PortForward, error)
}

type PortForward interface {
    LocalPort() int      // ephemeral port on 127.0.0.1
    Endpoint()  string   // "http://127.0.0.1:<port>"
    Wait()      error    // returns when the subprocess exits
    Close()     error    // kills the subprocess
}
```

The exec-injection point is a single function variable on the package
so tests substitute their own `kubectl` runner without touching real
processes. Production calls `os/exec`.

### TUI integration (option A from brainstorm)

Two new panes prepended to the existing pane stack:

```go
const (
    paneNamespaces paneID = iota  // new
    panePods                      // new
    paneSessions
    paneEvents
    paneDetail
    panePipeline
    panePluginDetail
)
```

The `Run` entry point's signature changes from
`Run(ctx, endpoint string)` to `Run(ctx, opts RunOptions)`. `main.go`
constructs the `RunOptions` from the command-line flags:

```go
type RunOptions struct {
    // If non-empty, skip the picker and connect directly. This is the
    // surface --endpoint maps to and preserves today's behavior.
    Endpoint string

    // Lister + PortForwarder are picker dependencies. Unused when
    // Endpoint is non-empty. main.go wires the production
    // implementations; tests inject fakes.
    Lister        cluster.Lister
    PortForwarder cluster.PortForwarder
}

func Run(ctx context.Context, opts RunOptions) error {
    if opts.Endpoint != "" {
        return runSessionView(ctx, opts.Endpoint)
    }
    return runWithPicker(ctx, opts.Lister, opts.PortForwarder)
}
```

`runSessionView` is the existing `Run` body, factored unchanged.
`runWithPicker` builds the new picker model; on pod selection it
calls `PortForwarder.Start` and then transitions the same bubbletea
program into `runSessionView`'s state with the resulting endpoint —
no second program is launched.

`runWithPicker` constructs a model in the `paneNamespaces` pane and
hands the chosen `(ns, pod)` to `PortForwarder.Start`, which produces
the endpoint that `runSessionView` then uses. The session view is
rendered by the same model so Esc out of the Sessions pane returns to
the Pods pane in the same program.

### Refresh model

- Namespaces and Pods refresh on entry to the pane (one kubectl call
  each), not on a timer. The previous result is replaced atomically.
- `r` re-runs the kubectl call from either pane; useful in long
  sessions if pods rolled.
- Existing session-events polling stays as it is — the picker only
  handles the navigate-and-connect step.

## Error handling

| Failure | UX |
|---|---|
| `kubectl` not on PATH | Friendly error at startup, before any pane: `"kubectl not found on PATH; install it or use --endpoint"`. Exit 1. |
| `kubectl get pods` non-zero exit | Footer of the active picker pane: `"kubectl: <stderr first line>"`. List is empty until `r`. |
| No namespaces have AuthBridge pods | Empty Namespaces pane with: `"No AuthBridge agents found. Try \`abctl --endpoint http://...\` to connect manually."` |
| Port-forward subprocess fails to start (`exec` error) | Footer on Pods pane: `"port-forward failed to start: <err>"`. Stay on Pods. |
| Port-forward starts but local port doesn't accept within 5s | Same surface: `"port-forward did not become ready: <stderr>"`. The subprocess is killed before the message lands. |
| Port-forward subprocess dies mid-session | Existing TUI already handles connection loss with reconnect attempts. User sees the connection-lost state and can Esc → pick a different pod. |
| Selected pod is not Running / Ready | Detected client-side at Enter time — message in footer: `"pod not Ready"`. No subprocess spawned. |

All error messages are single-line and live in the footer of the
relevant pane; no modal dialogs.

## Backward compatibility

- `--endpoint` flag: unchanged behavior. Documented in README as a
  power-user / scripting bypass.
- The existing session-events keybindings (Enter / Esc / q / / / p /
  y / s / g / G) are unchanged.
- The new picker introduces no new global keys; `r` is local to the
  picker panes (not used in the existing TUI today).

## Testing

- `cluster/` package gets unit tests with a stub kubectl runner:
  - golden JSON inputs for `kubectl get pods -A -o json` covering: no
    matches, single match, multi-namespace, mixed sidecar names.
  - PortForwarder is tested with a stub that returns a fixed local
    port, plus an integration-style test that uses a real `nc -l`
    process to validate the "wait until accepting" loop.
- TUI tests for the new panes follow the existing
  `tui/e2e_test.go` pattern: drive `tea.Model` with a sequence of
  `KeyMsg` and assert on the rendered view. Picker-state tests don't
  invoke real kubectl — they use the `Lister` and `PortForwarder`
  fakes from the cluster-package tests.
- An end-to-end smoke test (gated behind a build tag, opt-in) drives a
  real kind cluster with the IBAC demo deployed and verifies that
  `abctl` (no flag) discovers the agent namespace, lists the pod,
  port-forwards on Enter, and successfully fetches `/v1/sessions`.

## Out-of-scope follow-ups

- A "favorite pod" memory file (e.g. `~/.config/abctl/recent.json`) so
  repeated invocations remember the last-picked pod. Easy to add later.
- Live pod-list streaming via the watch API. Adds complexity for
  marginal benefit given the picker is a short-lived screen.
- Embedded `client-go` for native port-forward via SPDY. Considered
  during brainstorming, rejected because shelling out to kubectl
  inherits auth/context for free and keeps the binary ~10 MB.

## Acceptance criteria

1. `abctl` (no flag) on a cluster with at least one AuthBridge agent
   running drops the user into the session view of that pod with no
   manual `kubectl port-forward`.
2. `abctl --endpoint http://localhost:9094` behaves exactly as today.
3. On a cluster with no AuthBridge agents, `abctl` shows an actionable
   empty state, not a stack trace or hang.
4. Quitting `abctl` (`q` or Ctrl+C) leaves no stray `kubectl
   port-forward` processes.
5. Tests in `cmd/abctl/cluster/` pass without a real kubectl available.
