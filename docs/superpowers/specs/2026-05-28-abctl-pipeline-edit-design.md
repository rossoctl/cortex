# abctl pipeline editor — in-place ConfigMap editing with hot-reload

**Status:** Design — pending user review
**Date:** 2026-05-28

## Goal

Make abctl's Pipeline pane editable. Pressing `e` opens the agent's
runtime `pipeline:` subtree in `$EDITOR`; on save, abctl shows a diff,
applies the edit to the per-agent ConfigMap via `kubectl apply
--server-side`, polls `/reload/status` until the framework reloads, and
returns the user to the Pipeline pane now showing the new state.

This single flow covers four operations the user can perform inside the
editor — **edit a plugin's config, reorder plugins, remove a plugin,
add a plugin** — because each is just lines the user changes inside the
pipeline subtree.

## Non-goals

- Editing config OUTSIDE the `pipeline:` subtree (`mode`, `listener`,
  `session`, `mtls`, `spiffe`). Typos there can make the pod
  unschedulable or break the listener; locking the editor to the
  pipeline subtree limits the blast radius.
- Schema-aware form editor. The user types raw YAML and the framework
  validates on reload. A schema endpoint + structured form is a
  follow-up if it's wanted.
- Multi-pod fanout. The picker already constrains the user to one
  agent; the edit flow operates on that one ConfigMap.
- Undo / audit trail beyond the per-edit diff preview. The cluster's
  audit log + git are the right places for that.
- Inline secret redaction in the diff. The convention (mirrored from
  `:9093/config` and the new `:9094/v1/pipeline`) is `*_file` paths,
  never inline secrets. The diff shows what the user typed.

## User-facing behavior

```
Pipeline pane         press "e"
    │                     │
    ▼                     ▼
[fetching ConfigMap...]               (overlay, ~1s)
    │
    ▼
$EDITOR opens on /tmp/abctl-pipeline-XXXXXX.yaml
(content: only the pipeline: subtree of data.config.yaml,
 indented at the subtree's natural indent)
    │ user edits, saves, exits
    ▼
[validating YAML...]                  (overlay, instant)
    │
    │   if invalid: error + "r" to re-edit
    ▼
┌────────── DIFF OVERLAY ──────────┐
│  pipeline:                       │
│    inbound:                      │
│      - name: jwt-validation      │
│  -     config: {issuer: a}       │
│  +     config: {issuer: b}       │
│    outbound:                     │
│      ...                         │
│                                  │
│  apply this change? (y/N)        │
└──────────────────────────────────┘
    │ y
    ▼
[applying to ConfigMap...]            (overlay, ~1-2s)
    │
    ▼
[waiting for hot-reload...]           (overlay, up to 120s)
   spinner; updates as `last_success` advances on /reload/status
    │
    ▼
Pipeline pane (refreshed; new state)
```

Cancel paths: `Esc` from any overlay returns to the Pipeline pane.
`N` at the diff prompt is the same as Esc. `r` from a recoverable
error (validation, apply conflict) re-opens the editor with the same
tempfile. The pipeline state is read-only until the user explicitly
confirms apply.

## Foundation choices (resolved during brainstorming)

These are constraints the design takes as given:

- **A1 — write via `kubectl`.** abctl shells out to kubectl (matching
  the picker's pattern). The user's kubeconfig + RBAC controls writes.
  No new write API on the sidecar.
- **B1 — free-form YAML.** No schema discovery; the framework's
  reload-time validation is the source of truth. Bad YAML keeps the
  active pipeline serving (per `authbridge/CLAUDE.md` §"Config
  Hot-Reload") and reports the error on `/reload/status`.
- **Editor model — `$EDITOR` via `tea.ExecProcess`.** Bubbletea
  suspends, the user's chosen editor takes the terminal, abctl
  resumes. Mirrors `kubectl edit`, `git commit`, `K9s edit`.
- **Edit scope — pipeline subtree.** Listener / session / mtls etc.
  are out of scope; the editor only sees and only mutates the
  `pipeline:` block.
- **Apply mechanism — `kubectl apply --server-side`.** Uses
  `resourceVersion` for optimistic concurrency; concurrent edits
  surface as a 409 the user sees verbatim.
- **Reload feedback — poll `/reload/status` on `:9093`.** Watches
  `last_success_unix` advancing past the apply timestamp (success) or
  `reloads_failed_total` incrementing (failure with the framework's
  error message). 120s timeout.
- **Port-forward both `:9094` and `:9093`.** The picker's
  `kubectl port-forward` is extended to map two ephemeral local
  ports. abctl uses `:9094` for sessions/pipeline and `:9093` for
  reload-status.

## Architecture

```text
abctl picker mode
    │
    ▼ user picks pod; PortForward forwards both :9094 and :9093
    │
panePipeline
    │ press "e"
    ▼
edit state machine (cmd/abctl/edit/ + cmd/abctl/tui/edit_overlay.go)

  ① Fetch       kubectl get cm authbridge-config-<agent> -n <ns> -o yaml
                → parse → extract data.config.yaml string
                → locate "pipeline:" byte range inside
                → write subtree bytes to /tmp/abctl-pipeline-XXXX.yaml

  ② Edit        tea.ExecProcess($EDITOR tempPath)
                — bubbletea suspends; resumes on editor exit

  ③ Validate    parse the edited file as YAML
                — on error, surface line/col, offer "r" to re-edit

  ④ Diff        line-diff old vs new pipeline subtree, overlay,
                "apply? (y/N)" prompt

  ⑤ Apply       splice edited subtree back into original config.yaml
                (text byte-range surgery — preserves comments outside
                the pipeline subtree); rebuild ConfigMap manifest;
                kubectl apply --server-side --force-conflicts=false

  ⑥ Wait        poll http://127.0.0.1:<status-port>/reload/status:
                  last_success_unix > applyTime    → success
                  reloads_failed_total incremented → failure (body has the error)
                  120s elapsed                     → timeout

  ⑦ Refresh     re-fetch /v1/pipeline; rebuild pipeline table; close overlay
```

The state machine lives entirely on the bubbletea model. Each phase
emits one `tea.Msg`; the corresponding `Update` handler advances state
and dispatches the next `Cmd`. Cancel from any phase returns the user
to `panePipeline` cleanly.

## Components

### New package `cmd/abctl/edit/`

Pure logic, kubectl shell-out behind an injection seam (same pattern
as `cmd/abctl/cluster/`).

#### `edit/configmap.go`

```go
package edit

// Runner abstracts a `kubectl <args>` invocation. Production uses
// os/exec; tests inject their own.
type Runner func(ctx context.Context, args ...string) ([]byte, error)

// FetchedPipeline holds the result of a successful fetch + extract:
// the original ConfigMap bytes (untouched, for splice-back) and the
// pipeline-subtree byte range within data.config.yaml (so the editor
// only sees + mutates that subtree).
type FetchedPipeline struct {
    ConfigMapYAML []byte // raw `kubectl get cm -o yaml` output
    InnerYAML     []byte // value of data.config.yaml (the runtime config)
    PipelineStart int    // byte offset in InnerYAML where "pipeline:" begins
    PipelineEnd   int    // byte offset where the subtree ends
}

func Fetch(ctx context.Context, run Runner, namespace, agent string) (*FetchedPipeline, error)

// Splice replaces the pipeline subtree in fp.InnerYAML with newSubtree
// and returns a new ConfigMap YAML manifest ready for `kubectl apply`.
func Splice(fp *FetchedPipeline, newSubtree []byte) ([]byte, error)

// Apply writes manifest to a tempfile and runs kubectl apply --server-side.
// Returns the apply's wall-clock time (used to compare against
// /reload/status's last_success).
func Apply(ctx context.Context, run Runner, manifest []byte) (applyTime time.Time, err error)
```

The pipeline-subtree byte range is found by parsing `InnerYAML` with
`gopkg.in/yaml.v3` to a Node tree, finding the `pipeline:` key node,
and reading its `Line`/`Column` info to locate end-of-subtree
(start-of-next-top-level-key, or end-of-document). The splice is then
a `bytes.Buffer` rebuild: bytes before `PipelineStart`, then
`newSubtree`, then bytes after `PipelineEnd`. **No round-trip through
yaml.v3 emit** — that would lose comments and re-format whitespace.

#### `edit/diff.go`

```go
// Diff returns a colorized line-diff of old vs new (lipgloss-styled
// "+", "-", and dim context lines). Implementation is hand-rolled
// LCS-on-lines; the typical pipeline subtree is < 50 lines so even
// quadratic LCS is trivially fast.
func Diff(old, new []byte) string
```

No external diff library. yaml.v3's diff helpers are tree-aware and
hide line-by-line changes; users want to see the lines they typed.

#### `edit/status.go`

```go
type ReloadStatus struct {
    LastSuccessUnix     int64
    ReloadsOK           uint64
    ReloadsFailed       uint64
    LastError           string // body when ReloadsFailed advanced
}

// PollUntilReloaded watches /reload/status until either:
//   - LastSuccessUnix > applyTime.Unix(), OR
//   - ReloadsFailed exceeds the value at apply time (failure: error in LastError),
//   - 120s timeout.
// Returns sum-typed result.
func PollUntilReloaded(ctx context.Context, statusURL string, applyTime time.Time) PollResult
```

Polls every 1s. The 1s cadence is a balance between user-visible
spinner progress and avoiding pointless cluster chatter.

#### `edit/edit.go`

Top-level orchestrator:

```go
type Result struct {
    Status   ResultStatus       // applied / aborted / yamlInvalid /
                                // applyError / reloadError / timeout
    Diff     string             // populated for any post-validate state
    Error    error              // present for any *Error status
    NewYAML  []byte             // populated for applied
}

type ResultStatus int

const (
    StatusUnknown ResultStatus = iota
    StatusApplied
    StatusAborted
    StatusYAMLInvalid
    StatusApplyError
    StatusReloadError
    StatusTimeout
    StatusNoChanges
)
```

Each phase fires one tea.Msg; a state machine on the bubbletea model
tracks where we are. The orchestrator is split between
`edit/edit.go` (pure phase functions, returning results) and
`tui/edit_overlay.go` (UI state, drives the phases via tea.Cmds).

### Modified `cluster/portforward.go`

The `kubectlPortForwarder.Start` method is extended to forward two
ports. The `PortForward` interface gains:

```go
type PortForward interface {
    Endpoint() string        // existing — http://127.0.0.1:<sessionPort>
    StatusEndpoint() string  // NEW   — http://127.0.0.1:<statusPort>
    Close() error            // existing
}
```

`kubectl port-forward` accepts multiple `host:remote` pairs, e.g.
`9094:9094 9093:9093`, so this is one subprocess. Both ports use
`waitForAccept` to confirm readiness.

### TUI integration

`tui/edit_overlay.go` (new) — owns the overlay UI:

```go
type editPhase int

const (
    editPhaseFetching editPhase = iota
    editPhaseEditing       // $EDITOR is running; abctl is suspended
    editPhaseValidating
    editPhaseDiff
    editPhaseApplying
    editPhaseWaiting
    editPhaseError
    editPhaseDone
)

// editState lives on *model.
type editState struct {
    phase     editPhase
    fetched   *edit.FetchedPipeline
    tempPath  string
    diff      string  // colorized
    err       string  // single-line message in editPhaseError
    applyTime time.Time
}

func (m *model) showEditOverlay() string  // rendered
```

`tui/keys.go` — add `case "e":` in the `panePipeline` arm; sets
`m.editState.phase = editPhaseFetching` and dispatches `editFetchCmd`.

`tui/app.go` — five new `tea.Msg` types (one per phase transition):
`editFetchedMsg`, `editorExitedMsg`, `editValidatedMsg`,
`editAppliedMsg`, `editReloadedMsg`. Each handler in `Update` advances
`m.editState.phase` and dispatches the next Cmd.

The overlay renders on top of the existing Pipeline pane View when
`m.editState.phase != editPhaseDone`; otherwise the Pipeline pane
renders normally.

## Error handling

Every error / cancel path returns the user to `panePipeline` cleanly.
No stuck states.

| Failure | Surface | Recovery |
|---|---|---|
| `kubectl get cm` fails (RBAC, missing CM) | `"fetch failed: <stderr>"` | `Esc` |
| `$EDITOR` exits non-zero | `"editor exited <code>"` | `Esc`; tempfile is left on disk |
| Edited file == original | `"no changes; nothing to apply"` | auto-dismiss after 1s |
| YAML parse error on edited file | `"invalid YAML at line N: <msg>"` | `r` re-opens editor; `Esc` aborts |
| User says `N` at diff confirm | drop overlay | n/a |
| `kubectl apply` fails (RBAC denied / 409 conflict / validation) | stderr verbatim | `r` re-fetch + re-edit; `Esc` aborts |
| `/reload/status` reports `reloads_failed_total++` | `"reload failed: <error from status body>"` | `Esc`. Pod is still serving the OLD pipeline (framework keeps active on reload failure — `authbridge/CLAUDE.md` §"Config Hot-Reload"). User fixes their YAML and re-edits |
| 120s timeout polling /reload/status | `"reload not observed in 120s; check kubectl logs deploy/<agent>"` | `Esc`. Apply succeeded; kubelet just hasn't synced. Next /v1/pipeline fetch eventually shows the new state |
| User quits abctl mid-edit (q / Ctrl+C / SIGTERM) | program exits | `m.cancel()` propagates ctx, kubectl subprocesses die, port-forward closes, tempfile is left for recovery |

## Concurrent edits

`kubectl apply --server-side --force-conflicts=false` rejects
conflicting field changes with a clear 409 message naming the
conflicting field manager. We surface verbatim. `r` triggers a
re-fetch (which gets the updated state) and re-edit.

This is sufficient for the demo / single-operator case. Multi-operator
contention with diverging concurrent edits is rare in practice; if it
becomes a problem, a follow-up could read-after-apply to confirm.

## Trust & permissions

- abctl shells out to kubectl, which uses the user's kubeconfig.
  RBAC enforcement is the cluster's job.
- Caller needs `get` and `update` on `configmaps` in the agent's
  namespace. `get` was already needed for the picker; `update` is
  new for this PR.
- The edit endpoint is the cluster's API server, not the sidecar.
  No change to the sidecar's existing trust model (`:9094` and
  `:9093` are still in-cluster, no auth).
- Edits go to a ConfigMap that may already contain references to
  secrets via `*_file` paths. abctl doesn't introspect or redact —
  what the user types is what gets applied. Same model as
  `kubectl edit configmap`.

## Testing

| File | What |
|---|---|
| `edit/configmap_test.go` | Find pipeline byte range in fixture inner YAML; splice modified subtree; verify byte-for-byte that comments outside the pipeline subtree survive. Table-driven. |
| `edit/configmap_test.go` | End-to-end fetch → splice → apply with a stub kubectl Runner that captures the manifest written. Stub records args + final manifest bytes. |
| `edit/diff_test.go` | Diff renderer: equal inputs → empty diff; one-line edit; multi-line; reorder. Assert specific `+`/`-`/context line markers. |
| `edit/status_test.go` | `PollUntilReloaded` against an `httptest.Server` that returns successive ReloadStatus snapshots; verify success, failure, and timeout paths. |
| `edit/edit_test.go` | State machine end-to-end with fakes: stub kubectl, stub `/reload/status`, fake "editor" (writes pre-canned bytes to the tempfile). Drive the bubbletea model via `Update` directly. |
| `cluster/portforward_test.go` | Two-port mode of `kubectlPortForwarder` — wait for both local ports to accept. Same `freeLocalPort` + Go-listener pattern as existing tests. |
| `tui/edit_overlay_test.go` | Render the overlay in each phase (fetching / editing / validating / diff / applying / waiting / error). Drive `m.Update` with each new msg, assert overlay text. |
| `cmd/abctl/cluster/e2e_test.go` (existing, build-tag `e2e`) | New subtest drives the real edit flow against the live IBAC cluster: pick a Configurable plugin, append a no-op key to its config, apply, wait for reload, assert /v1/pipeline reflects the new value. |

## Acceptance criteria

1. Pressing `e` on the Pipeline pane opens `$EDITOR` with the agent's
   `pipeline:` subtree.
2. On editor exit with no changes, abctl shows "no changes" and returns.
3. On editor exit with changes, abctl shows a colorized diff and
   prompts `apply this change? (y/N)`.
4. `y` writes the edit to the per-agent ConfigMap via `kubectl apply
   --server-side`. `N` discards.
5. After apply, abctl polls `/reload/status` until the framework
   reports the reload succeeded (or failed); both outcomes are
   surfaced clearly.
6. After successful reload, the Pipeline pane refreshes from
   `/v1/pipeline` and shows the new state.
7. Add / remove / reorder a plugin all work because they're just
   line-edits inside the pipeline subtree.
8. RBAC denial on apply surfaces verbatim and the user can `Esc` back
   to the Pipeline pane without abctl losing state.
9. Bad YAML on save surfaces a clear error with line/col and offers
   to re-open the editor.
10. `go test ./...` passes including the new tests; the e2e build-tag
    test passes against a live IBAC cluster.
