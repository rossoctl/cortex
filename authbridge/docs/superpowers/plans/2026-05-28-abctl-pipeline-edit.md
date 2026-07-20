# abctl Pipeline Editor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make abctl's Pipeline pane editable: pressing `e` opens the agent's runtime `pipeline:` subtree in `$EDITOR`, then on save shows a diff, applies via `kubectl apply --server-side`, polls `/reload/status` until the framework reloads, and refreshes the pane.

**Architecture:** New `cmd/abctl/edit/` package owns the kubectl wrappers, byte-range YAML splice, line-diff renderer, and reload-status poller — each behind a `Runner` injection seam (mirrors `cmd/abctl/cluster/`). The `cluster.PortForwarder` is extended to forward both `:9094` and `:9093` so abctl can reach `/reload/status` without a second port-forward. A new TUI overlay state machine drives the user through fetch → edit → validate → diff → apply → wait phases, using `tea.ExecProcess` to suspend bubbletea while `$EDITOR` runs.

**Tech Stack:** Go 1.24, `gopkg.in/yaml.v3` (new dependency, ~200KB), `os/exec`, bubbletea (`tea.ExecProcess`), existing lipgloss + `cluster.Runner` patterns.

**Spec:** [`docs/superpowers/specs/2026-05-28-abctl-pipeline-edit-design.md`](../specs/2026-05-28-abctl-pipeline-edit-design.md)

---

## File Structure

**New files:**
- `authbridge/cmd/abctl/edit/configmap.go` — `Runner` type, `FetchedPipeline` struct, `FindPipelineRange`, `Splice`, `Fetch`, `Apply`.
- `authbridge/cmd/abctl/edit/configmap_test.go` — unit tests with golden-YAML fixtures + stub `Runner`.
- `authbridge/cmd/abctl/edit/diff.go` — `Diff(old, new []byte) string` line-based, lipgloss-colored.
- `authbridge/cmd/abctl/edit/diff_test.go` — equal / one-line / multi-line / reorder cases.
- `authbridge/cmd/abctl/edit/status.go` — `ReloadStatus` struct, `PollUntilReloaded`.
- `authbridge/cmd/abctl/edit/status_test.go` — `httptest.Server` driving success / failure / timeout paths.
- `authbridge/cmd/abctl/edit/edit.go` — `tea.Cmd` factories that wrap each phase (used by the TUI Update handlers).
- `authbridge/cmd/abctl/edit/edit_test.go` — Cmd factory unit tests (each Cmd produces the expected msg with stubbed dependencies).
- `authbridge/cmd/abctl/tui/edit_overlay.go` — `editPhase` enum, `editState` struct, render functions.
- `authbridge/cmd/abctl/tui/edit_overlay_test.go` — render assertions per phase.

**Modified files:**
- `authbridge/cmd/abctl/cluster/portforward.go` — extend `kubectlPortForwarder.Start` to forward two ports. `PortForward` interface gains `StatusEndpoint() string`.
- `authbridge/cmd/abctl/cluster/portforward_test.go` — add `TestPortForwarderTwoPorts`.
- `authbridge/cmd/abctl/tui/picker_test.go` — extend `fakePortForward` with `StatusEndpoint() string`.
- `authbridge/cmd/abctl/tui/app.go` — new tea.Msg types; new `*model` fields (`editState`, `statusURL`); Update handlers per phase; `View` overlay branch when `editState.phase != editPhaseDone`.
- `authbridge/cmd/abctl/tui/keys.go` — `"e"` keybind on `panePipeline`.
- `authbridge/cmd/abctl/go.mod` — add `gopkg.in/yaml.v3` dependency.
- `authbridge/cmd/abctl/README.md` — document the `e` keybind, RBAC requirements, tempfile lifecycle.

**Convention notes:**
- The `cmd/abctl/` directory is its own Go module. All `go test` / `go build` invocations cd there: `cd authbridge/cmd/abctl`.
- Repo policy: DCO sign-off (`-s`), `Assisted-By` (NOT `Co-Authored-By`), conventional commits.
- Branch already exists (`feat/abctl-pipeline-edit-spec`) with the design doc committed. All implementation tasks add commits to that branch.

---

## Task 1: Two-port port-forward

Extend the picker's `kubectl port-forward` to forward both `:9094` (session API) and `:9093` (stat server). Add a `StatusEndpoint() string` accessor on `PortForward` so callers can reach `/reload/status`.

**Files:**
- Modify: `authbridge/cmd/abctl/cluster/portforward.go`
- Modify: `authbridge/cmd/abctl/cluster/portforward_test.go`
- Modify: `authbridge/cmd/abctl/tui/picker_test.go` (the `fakePortForward` now needs `StatusEndpoint`)

### Step 1: Write the failing test

Append to `authbridge/cmd/abctl/cluster/portforward_test.go`:

```go
func TestKubectlPortForward_StatusEndpoint(t *testing.T) {
	// Build-only check that the kubectlPortForward struct exposes a
	// StatusEndpoint() method returning a 127.0.0.1 URL with a non-zero
	// port. Doesn't spawn kubectl.
	pf := &kubectlPortForward{port: 12345, statusPort: 12346}
	want := "http://127.0.0.1:12346"
	if got := pf.StatusEndpoint(); got != want {
		t.Fatalf("StatusEndpoint: got %q want %q", got, want)
	}
	if got := pf.Endpoint(); got != "http://127.0.0.1:12345" {
		t.Fatalf("Endpoint: got %q", got)
	}
}

func TestPortForwarderInterfaceHasStatusEndpoint(t *testing.T) {
	// Compile-time assertion that the interface exposes StatusEndpoint.
	var _ interface {
		Endpoint() string
		StatusEndpoint() string
		Close() error
	} = (*kubectlPortForward)(nil)
}
```

### Step 2: Run test to verify it fails

```bash
cd authbridge/cmd/abctl
go test ./cluster/ -run "TestKubectlPortForward_StatusEndpoint|TestPortForwarderInterfaceHasStatusEndpoint" -v
```
Expected: build failure — `kubectlPortForward.statusPort` and `StatusEndpoint` undefined.

### Step 3: Write minimal implementation

In `authbridge/cmd/abctl/cluster/portforward.go`, modify `kubectlPortForward`:

```go
type kubectlPortForward struct {
	cmd        *exec.Cmd
	port       int // local 127.0.0.1 port forwarding to pod :9094
	statusPort int // local 127.0.0.1 port forwarding to pod :9093
	stderr     io.ReadCloser

	mu          sync.Mutex
	stderrLines []string
	stderrDone  chan struct{}
}
```

Add `StatusEndpoint`:

```go
func (p *kubectlPortForward) Endpoint() string {
	return "http://127.0.0.1:" + strconv.Itoa(p.port)
}

// StatusEndpoint is the URL of the agent's stat server (:9093) reached
// via the picker's port-forward. abctl's edit flow polls /reload/status
// here.
func (p *kubectlPortForward) StatusEndpoint() string {
	return "http://127.0.0.1:" + strconv.Itoa(p.statusPort)
}
```

Update the `PortForward` interface (it lives in the same file):

```go
type PortForward interface {
	Endpoint() string
	StatusEndpoint() string
	Close() error
}
```

Now extend `kubectlPortForwarder.Start` to allocate two free local ports and forward both. Replace the existing body with:

```go
func (k *kubectlPortForwarder) Start(ctx context.Context, namespace, pod string) (PortForward, error) {
	port, err := freeLocalPort()
	if err != nil {
		return nil, err
	}
	statusPort, err := freeLocalPort()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("kubectl", "port-forward",
		"-n", namespace,
		"pod/"+pod,
		strconv.Itoa(port)+":9094",
		strconv.Itoa(statusPort)+":9093",
	)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start kubectl port-forward: %w", err)
	}
	pf := &kubectlPortForward{
		cmd: cmd, port: port, statusPort: statusPort, stderr: stderr,
	}
	pf.startStderrDrain()

	readyCtx, cancel := context.WithTimeout(ctx, pfReadyTimeout)
	defer cancel()
	if err := waitForAccept(readyCtx, port); err != nil {
		_ = pf.Close()
		stderrTail := pf.stderrTail()
		if stderrTail != "" {
			return nil, fmt.Errorf("port-forward not ready: %w (stderr: %s)", err, stderrTail)
		}
		return nil, fmt.Errorf("port-forward not ready: %w", err)
	}
	if err := waitForAccept(readyCtx, statusPort); err != nil {
		_ = pf.Close()
		stderrTail := pf.stderrTail()
		if stderrTail != "" {
			return nil, fmt.Errorf("port-forward (:9093) not ready: %w (stderr: %s)", err, stderrTail)
		}
		return nil, fmt.Errorf("port-forward (:9093) not ready: %w", err)
	}
	return pf, nil
}
```

### Step 4: Update fakePortForward in picker_test.go

In `authbridge/cmd/abctl/tui/picker_test.go`, find the `fakePortForward` type. Add a `StatusEndpoint()` method:

```go
func (p *fakePortForward) Endpoint() string  { return p.endpoint }
func (p *fakePortForward) StatusEndpoint() string { return "" } // not used by current picker tests
func (p *fakePortForward) Close() error       { p.parent.closeCount++; return nil }
```

### Step 5: Run tests; expect PASS

```bash
cd authbridge/cmd/abctl
go test ./cluster/ -v
go test ./tui/ -v
go vet ./cluster/ ./tui/
```
Expected: all tests pass, vet clean. The new `TestKubectlPortForward_StatusEndpoint` and the build-only interface check pass.

### Step 6: Commit

```bash
cd /Users/haihuang/works/go/src/github.com/rossoctl/cortex
git add authbridge/cmd/abctl/cluster/portforward.go \
        authbridge/cmd/abctl/cluster/portforward_test.go \
        authbridge/cmd/abctl/tui/picker_test.go
git commit -s -m "feat(abctl): Forward :9093 alongside :9094 in the picker

The pipeline editor (incoming) needs to reach /reload/status on the
stat server (:9093) to know when the framework finished reloading.
Extend kubectlPortForwarder to forward both ports through one kubectl
subprocess. PortForward interface gains StatusEndpoint() returning the
:9093 URL.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 2: edit/configmap.go — FindPipelineRange

Pure-Go function that locates the `pipeline:` subtree's byte range inside a runtime config YAML. Uses `gopkg.in/yaml.v3` Node tree only to find the boundaries; the actual splice happens against raw bytes (preserves comments).

**Files:**
- Create: `authbridge/cmd/abctl/edit/configmap.go`
- Create: `authbridge/cmd/abctl/edit/configmap_test.go`
- Modify: `authbridge/cmd/abctl/go.mod` (will be auto-updated by `go mod tidy` after first import)

### Step 1: Write the failing test

Create `authbridge/cmd/abctl/edit/configmap_test.go`:

```go
package edit

import (
	"strings"
	"testing"
)

const fixtureMidYAML = `mode: proxy-sidecar

listener:
  forward_proxy_addr: ":8081"

pipeline:
  inbound:
    - name: jwt-validation
      config:
        issuer: http://idp
  outbound:
    - name: token-exchange

session:
  enabled: true
`

const fixtureLastYAML = `mode: proxy-sidecar

pipeline:
  inbound:
    - name: jwt-validation
`

const fixtureFirstYAML = `pipeline:
  inbound:
    - name: jwt-validation

mode: proxy-sidecar
`

const fixtureMissingYAML = `mode: proxy-sidecar

listener:
  forward_proxy_addr: ":8081"
`

func TestFindPipelineRange_Middle(t *testing.T) {
	start, end, err := FindPipelineRange([]byte(fixtureMidYAML))
	if err != nil {
		t.Fatalf("FindPipelineRange: %v", err)
	}
	got := fixtureMidYAML[start:end]
	// Range must include the full pipeline subtree.
	if !strings.Contains(got, "pipeline:") {
		t.Fatalf("range missing pipeline header: %q", got)
	}
	if !strings.Contains(got, "token-exchange") {
		t.Fatalf("range missing pipeline body: %q", got)
	}
	// Range must NOT include the next top-level key.
	if strings.Contains(got, "session:") {
		t.Fatalf("range includes next key: %q", got)
	}
	if strings.Contains(got, "listener:") {
		t.Fatalf("range includes prior key: %q", got)
	}
}

func TestFindPipelineRange_LastKey(t *testing.T) {
	start, end, err := FindPipelineRange([]byte(fixtureLastYAML))
	if err != nil {
		t.Fatalf("FindPipelineRange: %v", err)
	}
	if end != len(fixtureLastYAML) {
		t.Fatalf("end = %d, want len(yaml) = %d", end, len(fixtureLastYAML))
	}
	got := fixtureLastYAML[start:end]
	if !strings.Contains(got, "jwt-validation") {
		t.Fatalf("range missing pipeline body: %q", got)
	}
}

func TestFindPipelineRange_FirstKey(t *testing.T) {
	start, _, err := FindPipelineRange([]byte(fixtureFirstYAML))
	if err != nil {
		t.Fatalf("FindPipelineRange: %v", err)
	}
	if start != 0 {
		t.Fatalf("start = %d, want 0", start)
	}
}

func TestFindPipelineRange_Missing(t *testing.T) {
	_, _, err := FindPipelineRange([]byte(fixtureMissingYAML))
	if err == nil {
		t.Fatal("want error when pipeline key is absent")
	}
	if !strings.Contains(err.Error(), "pipeline") {
		t.Fatalf("error should mention pipeline: %v", err)
	}
}
```

### Step 2: Run test to verify it fails

```bash
cd authbridge/cmd/abctl
go test ./edit/ -run TestFindPipelineRange -v
```
Expected: build failure — `edit` package + `FindPipelineRange` undefined.

### Step 3: Write minimal implementation

Create `authbridge/cmd/abctl/edit/configmap.go`:

```go
// Package edit implements abctl's in-place pipeline editor. The flow is:
// fetch the agent's ConfigMap via kubectl, locate the pipeline: subtree,
// open just that subtree in the user's $EDITOR, splice the edit back into
// the original ConfigMap manifest, kubectl apply --server-side, then poll
// /reload/status until the framework reloads.
//
// All kubectl interaction goes through the Runner injection seam so tests
// can stub it out.
package edit

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// FindPipelineRange returns the byte offsets [start, end) in innerYAML
// that span the "pipeline:" subtree, including the "pipeline:" key line
// itself but not any following top-level keys. Used by the editor to
// extract just the pipeline subtree for the user, and by Splice to
// replace it with the user's edit.
//
// Returns an error if innerYAML is not valid YAML or if no top-level
// "pipeline" key exists.
func FindPipelineRange(innerYAML []byte) (start, end int, err error) {
	var root yaml.Node
	if err := yaml.Unmarshal(innerYAML, &root); err != nil {
		return 0, 0, fmt.Errorf("parse runtime YAML: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return 0, 0, fmt.Errorf("runtime YAML is not a document")
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return 0, 0, fmt.Errorf("runtime YAML root is not a mapping")
	}

	// Children of a MappingNode alternate key, value, key, value, ...
	// Find the index of the "pipeline" key, capture its line, and find
	// the next sibling's line (or end-of-document if it's the last key).
	pipelineKeyIdx := -1
	for i := 0; i < len(doc.Content); i += 2 {
		k := doc.Content[i]
		if k.Value == "pipeline" {
			pipelineKeyIdx = i
			break
		}
	}
	if pipelineKeyIdx == -1 {
		return 0, 0, fmt.Errorf("no top-level pipeline key in runtime YAML")
	}

	pipelineKeyLine := doc.Content[pipelineKeyIdx].Line // 1-indexed
	var nextKeyLine int                                  // 1-indexed; 0 if pipeline is last
	if pipelineKeyIdx+2 < len(doc.Content) {
		nextKeyLine = doc.Content[pipelineKeyIdx+2].Line
	}

	// Map line numbers to byte offsets. yaml.v3 Line is 1-indexed.
	lineStarts := []int{0} // lineStarts[i] = byte offset where line i+1 starts
	for i, b := range innerYAML {
		if b == '\n' {
			lineStarts = append(lineStarts, i+1)
		}
	}

	if pipelineKeyLine < 1 || pipelineKeyLine > len(lineStarts) {
		return 0, 0, fmt.Errorf("pipeline key line %d out of range", pipelineKeyLine)
	}
	start = lineStarts[pipelineKeyLine-1]

	if nextKeyLine == 0 {
		end = len(innerYAML)
	} else {
		if nextKeyLine < 1 || nextKeyLine > len(lineStarts) {
			return 0, 0, fmt.Errorf("next-key line %d out of range", nextKeyLine)
		}
		end = lineStarts[nextKeyLine-1]
	}
	return start, end, nil
}

// preserve unused-import safety while we bootstrap the file
var _ = bytes.Buffer{}
```

(The `var _ = bytes.Buffer{}` line keeps the `bytes` import alive; it'll be used by `Splice` in Task 3 and removed then.)

Run `go mod tidy` to add yaml.v3:

```bash
cd authbridge/cmd/abctl
go mod tidy
```

### Step 4: Run tests; expect PASS

```bash
cd authbridge/cmd/abctl
go test ./edit/ -v
go vet ./edit/
```
Expected: all 4 tests PASS, vet clean.

### Step 5: Commit

```bash
cd /Users/haihuang/works/go/src/github.com/rossoctl/cortex
git add authbridge/cmd/abctl/edit/configmap.go \
        authbridge/cmd/abctl/edit/configmap_test.go \
        authbridge/cmd/abctl/go.mod \
        authbridge/cmd/abctl/go.sum
git commit -s -m "feat(abctl): Add edit package with FindPipelineRange

Pure-Go function that locates the pipeline: subtree's byte range
inside a runtime config YAML. Uses yaml.v3 only to find boundaries;
the actual splice (Task 3) happens against raw bytes to preserve
comments outside the subtree.

Adds gopkg.in/yaml.v3 dependency.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 3: edit/configmap.go — Splice + manifest rebuild

Add `Splice` (byte-range surgery on the inner YAML) and `BuildManifest` (parse the outer ConfigMap YAML, replace `data["config.yaml"]`, re-emit). The two functions together produce the manifest passed to `kubectl apply`.

**Files:**
- Modify: `authbridge/cmd/abctl/edit/configmap.go`
- Modify: `authbridge/cmd/abctl/edit/configmap_test.go`

### Step 1: Write the failing tests

Append to `authbridge/cmd/abctl/edit/configmap_test.go`:

```go
const fixtureCMYAML = `apiVersion: v1
kind: ConfigMap
metadata:
  name: authbridge-config-email-agent
  namespace: team1
data:
  config.yaml: |
    mode: proxy-sidecar
    pipeline:
      inbound:
        - name: jwt-validation
          config:
            issuer: old
    session:
      enabled: true
`

func TestSplice_PreservesOutsideRange(t *testing.T) {
	const orig = `mode: proxy-sidecar
# this comment must survive
listener:
  forward_proxy_addr: ":8081"

pipeline:
  inbound:
    - name: jwt-validation

session:
  enabled: true
`
	start, end, err := FindPipelineRange([]byte(orig))
	if err != nil {
		t.Fatal(err)
	}
	const newSubtree = `pipeline:
  inbound:
    - name: jwt-validation
      config:
        issuer: new

`
	got := Splice([]byte(orig), start, end, []byte(newSubtree))
	gotS := string(got)
	if !strings.Contains(gotS, "# this comment must survive") {
		t.Fatalf("comment outside pipeline subtree was dropped:\n%s", gotS)
	}
	if !strings.Contains(gotS, "listener:") {
		t.Fatalf("listener section was dropped:\n%s", gotS)
	}
	if !strings.Contains(gotS, "session:") {
		t.Fatalf("session section was dropped:\n%s", gotS)
	}
	if !strings.Contains(gotS, "issuer: new") {
		t.Fatalf("new pipeline content not present:\n%s", gotS)
	}
	if strings.Contains(gotS, "issuer: old") {
		t.Fatalf("old pipeline content still present:\n%s", gotS)
	}
}

func TestBuildManifest_UpdatesDataField(t *testing.T) {
	const newInner = `mode: proxy-sidecar
pipeline:
  inbound:
    - name: jwt-validation
      config:
        issuer: new
session:
  enabled: true
`
	out, err := BuildManifest([]byte(fixtureCMYAML), []byte(newInner))
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	outS := string(out)
	// metadata is preserved.
	if !strings.Contains(outS, "name: authbridge-config-email-agent") {
		t.Fatalf("metadata.name lost:\n%s", outS)
	}
	if !strings.Contains(outS, "namespace: team1") {
		t.Fatalf("metadata.namespace lost:\n%s", outS)
	}
	// data.config.yaml carries the new content.
	if !strings.Contains(outS, "issuer: new") {
		t.Fatalf("new content not in manifest:\n%s", outS)
	}
	if strings.Contains(outS, "issuer: old") {
		t.Fatalf("old content still in manifest:\n%s", outS)
	}
}
```

### Step 2: Run tests; expect failure

```bash
cd authbridge/cmd/abctl
go test ./edit/ -run "TestSplice|TestBuildManifest" -v
```
Expected: build failure — `Splice` and `BuildManifest` undefined.

### Step 3: Write minimal implementation

In `authbridge/cmd/abctl/edit/configmap.go`, replace `var _ = bytes.Buffer{}` with the real functions:

```go
// Splice replaces the byte range [start, end) of innerYAML with newSubtree
// and returns the result. Used to apply the user's edit to just the pipeline
// subtree, leaving everything outside it byte-for-byte unchanged. Comments,
// blank lines, and field ordering outside the pipeline subtree all survive.
func Splice(innerYAML []byte, start, end int, newSubtree []byte) []byte {
	var b bytes.Buffer
	b.Grow(len(innerYAML) - (end - start) + len(newSubtree))
	b.Write(innerYAML[:start])
	b.Write(newSubtree)
	b.Write(innerYAML[end:])
	return b.Bytes()
}

// BuildManifest takes the original ConfigMap YAML manifest (as returned by
// kubectl get cm -o yaml) and a new inner runtime YAML (the contents that
// belong in data.config.yaml). Returns a manifest ready for kubectl apply.
//
// The manifest passes through yaml.v3 so the outer structure (apiVersion,
// kind, metadata, etc.) is preserved. Only data.config.yaml is replaced.
// Comments inside the inner runtime YAML survive because we set the
// data.config.yaml value to a literal block (|) string carrying newInner
// verbatim.
func BuildManifest(origCMYAML, newInner []byte) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(origCMYAML, &root); err != nil {
		return nil, fmt.Errorf("parse ConfigMap manifest: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, fmt.Errorf("ConfigMap manifest is not a document")
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("ConfigMap manifest root is not a mapping")
	}

	// Find data → config.yaml.
	var dataNode *yaml.Node
	for i := 0; i < len(doc.Content); i += 2 {
		if doc.Content[i].Value == "data" {
			dataNode = doc.Content[i+1]
			break
		}
	}
	if dataNode == nil || dataNode.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("ConfigMap has no data: mapping")
	}
	var configValueNode *yaml.Node
	for i := 0; i < len(dataNode.Content); i += 2 {
		if dataNode.Content[i].Value == "config.yaml" {
			configValueNode = dataNode.Content[i+1]
			break
		}
	}
	if configValueNode == nil {
		return nil, fmt.Errorf("ConfigMap data has no config.yaml key")
	}

	// Set the value to a literal-block scalar carrying newInner.
	configValueNode.Kind = yaml.ScalarNode
	configValueNode.Tag = "!!str"
	configValueNode.Style = yaml.LiteralStyle
	configValueNode.Value = string(newInner)

	out, err := yaml.Marshal(&root)
	if err != nil {
		return nil, fmt.Errorf("emit ConfigMap manifest: %w", err)
	}
	return out, nil
}
```

### Step 4: Run tests; expect PASS

```bash
cd authbridge/cmd/abctl
go test ./edit/ -v
go vet ./edit/
```
Expected: all 6 tests PASS, vet clean.

### Step 5: Commit

```bash
cd /Users/haihuang/works/go/src/github.com/rossoctl/cortex
git add authbridge/cmd/abctl/edit/configmap.go \
        authbridge/cmd/abctl/edit/configmap_test.go
git commit -s -m "feat(abctl): Add Splice and BuildManifest helpers

Splice does byte-range surgery to replace the pipeline subtree without
touching anything outside it (preserves comments, blank lines, field
ordering). BuildManifest parses the outer ConfigMap YAML, sets
data.config.yaml to a literal-block scalar carrying the new inner
content, and re-emits the manifest ready for kubectl apply.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 4: edit/configmap.go — Fetch + Apply (kubectl integration)

Add the `Runner` injection seam plus `Fetch` (wraps `kubectl get cm`) and `Apply` (wraps `kubectl apply --server-side`).

**Files:**
- Modify: `authbridge/cmd/abctl/edit/configmap.go`
- Modify: `authbridge/cmd/abctl/edit/configmap_test.go`

### Step 1: Write the failing tests

Append to `authbridge/cmd/abctl/edit/configmap_test.go`:

```go
import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestFetch_HappyPath(t *testing.T) {
	wantArgs := []string{
		"get", "cm", "authbridge-config-email-agent",
		"-n", "team1", "-o", "yaml",
	}
	stub := func(ctx context.Context, args ...string) ([]byte, error) {
		if !equalArgs(args, wantArgs) {
			t.Fatalf("kubectl args: got %v want %v", args, wantArgs)
		}
		return []byte(fixtureCMYAML), nil
	}
	fp, err := Fetch(context.Background(), stub, "team1", "email-agent")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(fp.ConfigMapYAML) == 0 {
		t.Fatal("ConfigMapYAML empty")
	}
	if len(fp.InnerYAML) == 0 {
		t.Fatal("InnerYAML empty")
	}
	if fp.PipelineEnd <= fp.PipelineStart {
		t.Fatalf("pipeline range invalid: [%d, %d)", fp.PipelineStart, fp.PipelineEnd)
	}
	subtree := fp.InnerYAML[fp.PipelineStart:fp.PipelineEnd]
	if !strings.Contains(string(subtree), "pipeline:") {
		t.Fatalf("subtree missing header: %q", subtree)
	}
	if !strings.Contains(string(subtree), "jwt-validation") {
		t.Fatalf("subtree missing body: %q", subtree)
	}
}

func TestFetch_KubectlError(t *testing.T) {
	stub := func(ctx context.Context, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("forbidden")
	}
	_, err := Fetch(context.Background(), stub, "team1", "email-agent")
	if err == nil {
		t.Fatal("want error from kubectl")
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("error should surface kubectl message: %v", err)
	}
}

func TestApply_PassesManifest(t *testing.T) {
	captured := make([]byte, 0)
	stub := func(ctx context.Context, args ...string) ([]byte, error) {
		// Args should be: apply --server-side --force-conflicts=false -f <path>
		if len(args) < 4 || args[0] != "apply" || args[1] != "--server-side" {
			t.Fatalf("kubectl args: %v", args)
		}
		// Read the manifest the orchestrator wrote to a tempfile.
		path := args[len(args)-1]
		b, err := readFileForTest(path)
		if err != nil {
			t.Fatalf("read manifest: %v", err)
		}
		captured = b
		return []byte("configmap/foo applied\n"), nil
	}
	manifest := []byte("apiVersion: v1\nkind: ConfigMap\n")
	at, err := Apply(context.Background(), stub, manifest)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if at.IsZero() {
		t.Fatal("apply time should be set")
	}
	if time.Since(at) > 5*time.Second {
		t.Fatalf("apply time too far in past: %v", at)
	}
	if string(captured) != string(manifest) {
		t.Fatalf("manifest captured by stub differs from input")
	}
}

// equalArgs checks two []string for equality.
func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// readFileForTest is a tiny helper so the test doesn't import os just
// to exercise Apply's tempfile path.
func readFileForTest(path string) ([]byte, error) {
	return os.ReadFile(path)
}
```

Add `"fmt"` and `"os"` to the test file's imports if not already present.

### Step 2: Run tests; expect failure

```bash
cd authbridge/cmd/abctl
go test ./edit/ -run "TestFetch|TestApply" -v
```
Expected: build failure — `Runner`, `FetchedPipeline`, `Fetch`, `Apply` undefined.

### Step 3: Write minimal implementation

Append to `authbridge/cmd/abctl/edit/configmap.go`. Update its import block to include `"context"`, `"os"`, `"os/exec"`, `"strings"`, `"time"`:

```go
// Runner abstracts a `kubectl <args>` invocation. Production uses os/exec;
// tests inject their own. Mirrors the Runner pattern in cmd/abctl/cluster.
type Runner func(ctx context.Context, args ...string) ([]byte, error)

// DefaultRunner shells out to the system `kubectl`.
func DefaultRunner(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("kubectl: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("kubectl: %w", err)
	}
	return out, nil
}

// FetchedPipeline is what Fetch returns: the full ConfigMap manifest, the
// inner runtime YAML extracted from data.config.yaml, and the byte range
// of the pipeline subtree within the inner YAML.
type FetchedPipeline struct {
	ConfigMapYAML []byte // raw kubectl get cm -o yaml output
	InnerYAML     []byte // value of data.config.yaml
	PipelineStart int    // byte offset in InnerYAML where pipeline: begins
	PipelineEnd   int    // byte offset where the subtree ends
}

// Fetch reads the per-agent ConfigMap (authbridge-config-<agent>), extracts
// the inner runtime YAML from data.config.yaml, and locates the pipeline
// subtree's byte range. Returns an error if the ConfigMap doesn't exist,
// has no data.config.yaml, or has no top-level pipeline: key.
func Fetch(ctx context.Context, run Runner, namespace, agent string) (*FetchedPipeline, error) {
	cmName := "authbridge-config-" + agent
	cmBytes, err := run(ctx, "get", "cm", cmName, "-n", namespace, "-o", "yaml")
	if err != nil {
		return nil, err
	}
	inner, err := extractInnerYAML(cmBytes)
	if err != nil {
		return nil, err
	}
	start, end, err := FindPipelineRange(inner)
	if err != nil {
		return nil, err
	}
	return &FetchedPipeline{
		ConfigMapYAML: cmBytes,
		InnerYAML:     inner,
		PipelineStart: start,
		PipelineEnd:   end,
	}, nil
}

// extractInnerYAML pulls data.config.yaml out of an outer ConfigMap manifest.
func extractInnerYAML(cmYAML []byte) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(cmYAML, &root); err != nil {
		return nil, fmt.Errorf("parse ConfigMap manifest: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, fmt.Errorf("ConfigMap manifest is not a document")
	}
	doc := root.Content[0]
	for i := 0; i+1 < len(doc.Content); i += 2 {
		if doc.Content[i].Value == "data" {
			dataNode := doc.Content[i+1]
			for j := 0; j+1 < len(dataNode.Content); j += 2 {
				if dataNode.Content[j].Value == "config.yaml" {
					return []byte(dataNode.Content[j+1].Value), nil
				}
			}
		}
	}
	return nil, fmt.Errorf("ConfigMap data has no config.yaml key")
}

// Apply writes manifest to a tempfile and runs kubectl apply --server-side.
// Returns the wall-clock time at which the apply call started; the caller
// uses this to compare against /reload/status's last_success_unix to know
// whether the framework has picked up the change yet.
func Apply(ctx context.Context, run Runner, manifest []byte) (time.Time, error) {
	tmp, err := os.CreateTemp("", "abctl-cm-*.yaml")
	if err != nil {
		return time.Time{}, fmt.Errorf("create temp manifest: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(manifest); err != nil {
		_ = tmp.Close()
		return time.Time{}, fmt.Errorf("write temp manifest: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return time.Time{}, fmt.Errorf("close temp manifest: %w", err)
	}
	applyTime := time.Now()
	if _, err := run(ctx, "apply", "--server-side", "--force-conflicts=false", "-f", tmp.Name()); err != nil {
		return time.Time{}, err
	}
	return applyTime, nil
}
```

### Step 4: Run tests; expect PASS

```bash
cd authbridge/cmd/abctl
go test ./edit/ -v
go vet ./edit/
```
Expected: all 9 tests pass, vet clean.

### Step 5: Commit

```bash
cd /Users/haihuang/works/go/src/github.com/rossoctl/cortex
git add authbridge/cmd/abctl/edit/configmap.go \
        authbridge/cmd/abctl/edit/configmap_test.go
git commit -s -m "feat(abctl): Add Fetch and Apply for the edit flow

Fetch reads authbridge-config-<agent> via kubectl, extracts the inner
runtime YAML from data.config.yaml, and locates the pipeline subtree's
byte range. Apply writes a manifest to a tempfile and runs kubectl
apply --server-side --force-conflicts=false; returns the apply
timestamp for downstream comparison against /reload/status.

All kubectl interaction goes through a Runner injection seam mirroring
cmd/abctl/cluster.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 5: edit/diff.go — Line-diff renderer

Pure-Go line-based LCS diff with lipgloss styling (green `+`, red `-`, dim context).

**Files:**
- Create: `authbridge/cmd/abctl/edit/diff.go`
- Create: `authbridge/cmd/abctl/edit/diff_test.go`

### Step 1: Write the failing tests

Create `authbridge/cmd/abctl/edit/diff_test.go`:

```go
package edit

import (
	"strings"
	"testing"
)

func TestDiff_Equal(t *testing.T) {
	got := Diff([]byte("a\nb\nc\n"), []byte("a\nb\nc\n"))
	if got != "" {
		t.Fatalf("equal inputs should produce empty diff, got %q", got)
	}
}

func TestDiff_OneLineChange(t *testing.T) {
	old := []byte("a\nb\nc\n")
	new := []byte("a\nB\nc\n")
	got := Diff(old, new)
	// We don't assert exact ANSI bytes (style-dependent); just check
	// the line markers + content are present.
	if !strings.Contains(got, "-b") {
		t.Fatalf("missing -b line:\n%s", got)
	}
	if !strings.Contains(got, "+B") {
		t.Fatalf("missing +B line:\n%s", got)
	}
}

func TestDiff_AddAndRemove(t *testing.T) {
	old := []byte("a\nb\nc\n")
	new := []byte("a\nc\nd\n")
	got := Diff(old, new)
	if !strings.Contains(got, "-b") {
		t.Fatalf("missing -b:\n%s", got)
	}
	if !strings.Contains(got, "+d") {
		t.Fatalf("missing +d:\n%s", got)
	}
}

func TestDiff_PreservesContext(t *testing.T) {
	old := []byte("line1\nline2\nline3\nline4\n")
	new := []byte("line1\nline2\nLINE3\nline4\n")
	got := Diff(old, new)
	// Both unchanged lines should appear (as context).
	if !strings.Contains(got, "line1") || !strings.Contains(got, "line4") {
		t.Fatalf("missing context lines:\n%s", got)
	}
	if !strings.Contains(got, "-line3") || !strings.Contains(got, "+LINE3") {
		t.Fatalf("missing change markers:\n%s", got)
	}
}
```

### Step 2: Run tests; expect failure

```bash
cd authbridge/cmd/abctl
go test ./edit/ -run TestDiff -v
```
Expected: build failure — `Diff` undefined.

### Step 3: Write minimal implementation

Create `authbridge/cmd/abctl/edit/diff.go`:

```go
package edit

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var (
	diffStyleAdd     = lipgloss.NewStyle().Foreground(lipgloss.Color("2")) // green
	diffStyleRemove  = lipgloss.NewStyle().Foreground(lipgloss.Color("1")) // red
	diffStyleContext = lipgloss.NewStyle().Faint(true)
)

// Diff renders a line-based diff of old vs new with lipgloss styling.
// Returns the empty string when inputs are byte-equal. The algorithm is
// LCS-on-lines; output is one rendered line per diff entry, terminated
// by newlines.
//
// LCS is O(N*M) in the line count; pipeline subtrees are typically <50
// lines so the cost is negligible. The diff is intended for the abctl
// edit confirmation overlay — it shows the user every line that
// changed, in original order, with two lines of context emit suppressed
// for compactness (every line is either ±something or context).
func Diff(old, new []byte) string {
	if string(old) == string(new) {
		return ""
	}
	oldLines := splitLinesKeepNewline(old)
	newLines := splitLinesKeepNewline(new)
	ops := lcsDiff(oldLines, newLines)
	var b strings.Builder
	for _, op := range ops {
		switch op.kind {
		case opEqual:
			b.WriteString(diffStyleContext.Render(" " + strings.TrimRight(op.line, "\n")))
			b.WriteByte('\n')
		case opRemove:
			b.WriteString(diffStyleRemove.Render("-" + strings.TrimRight(op.line, "\n")))
			b.WriteByte('\n')
		case opAdd:
			b.WriteString(diffStyleAdd.Render("+" + strings.TrimRight(op.line, "\n")))
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func splitLinesKeepNewline(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := string(b)
	out := strings.SplitAfter(s, "\n")
	// strings.SplitAfter on "a\n" returns ["a\n", ""] — drop trailing empty.
	if len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

type opKind int

const (
	opEqual opKind = iota
	opRemove
	opAdd
)

type diffOp struct {
	kind opKind
	line string
}

// lcsDiff is a textbook LCS-then-backtrack implementation. Returns the
// edit script as a sequence of equal/remove/add ops in order.
func lcsDiff(old, new []string) []diffOp {
	m, n := len(old), len(new)
	if m == 0 {
		out := make([]diffOp, n)
		for i, l := range new {
			out[i] = diffOp{opAdd, l}
		}
		return out
	}
	if n == 0 {
		out := make([]diffOp, m)
		for i, l := range old {
			out[i] = diffOp{opRemove, l}
		}
		return out
	}
	// dp[i][j] = LCS length of old[:i] vs new[:j]
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if old[i-1] == new[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}
	var rev []diffOp
	i, j := m, n
	for i > 0 && j > 0 {
		switch {
		case old[i-1] == new[j-1]:
			rev = append(rev, diffOp{opEqual, old[i-1]})
			i--
			j--
		case dp[i-1][j] >= dp[i][j-1]:
			rev = append(rev, diffOp{opRemove, old[i-1]})
			i--
		default:
			rev = append(rev, diffOp{opAdd, new[j-1]})
			j--
		}
	}
	for i > 0 {
		rev = append(rev, diffOp{opRemove, old[i-1]})
		i--
	}
	for j > 0 {
		rev = append(rev, diffOp{opAdd, new[j-1]})
		j--
	}
	// Reverse.
	out := make([]diffOp, len(rev))
	for k, op := range rev {
		out[len(rev)-1-k] = op
	}
	return out
}
```

### Step 4: Run tests; expect PASS

```bash
cd authbridge/cmd/abctl
go test ./edit/ -v
go vet ./edit/
```
Expected: all 13 tests pass.

### Step 5: Commit

```bash
cd /Users/haihuang/works/go/src/github.com/rossoctl/cortex
git add authbridge/cmd/abctl/edit/diff.go \
        authbridge/cmd/abctl/edit/diff_test.go
git commit -s -m "feat(abctl): Add line-diff renderer for edit confirmation

LCS-on-lines with lipgloss styling (green +, red -, dim context).
Returns empty string for byte-equal inputs. Used by the edit overlay's
confirm prompt to show the user what they're about to apply.

Hand-rolled rather than pulling a diff library; pipeline subtrees are
typically <50 lines so even quadratic LCS is trivially fast.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 6: edit/status.go — Poll /reload/status

Reload-status poller. Watches `last_success_unix` and `reloads_failed_total` to detect success or failure of the framework's reload after the user's apply. 1s cadence, 120s timeout.

**Files:**
- Create: `authbridge/cmd/abctl/edit/status.go`
- Create: `authbridge/cmd/abctl/edit/status_test.go`

### Step 1: Write the failing tests

Create `authbridge/cmd/abctl/edit/status_test.go`:

```go
package edit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// makeStatusServer returns a httptest.Server whose /reload/status
// endpoint reads its body from the supplied function — letting the
// test advance state between polls.
func makeStatusServer(t *testing.T, body func() ReloadStatus) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/reload/status" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body())
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPollUntilReloaded_Success(t *testing.T) {
	applyTime := time.Now()
	var calls atomic.Int32
	srv := makeStatusServer(t, func() ReloadStatus {
		c := calls.Add(1)
		// First call: no successful reload yet. Second call: success.
		if c == 1 {
			return ReloadStatus{LastSuccessUnix: applyTime.Add(-1 * time.Hour).Unix()}
		}
		return ReloadStatus{LastSuccessUnix: time.Now().Unix()}
	})
	res := PollUntilReloaded(context.Background(), srv.URL, applyTime)
	if res.Status != PollSuccess {
		t.Fatalf("status = %v, want PollSuccess", res.Status)
	}
}

func TestPollUntilReloaded_Failure(t *testing.T) {
	applyTime := time.Now()
	var calls atomic.Int32
	srv := makeStatusServer(t, func() ReloadStatus {
		c := calls.Add(1)
		// Pre-apply baseline on first call (ReloadsFailed = 5).
		// Increment on second call.
		if c == 1 {
			return ReloadStatus{ReloadsFailed: 5}
		}
		return ReloadStatus{ReloadsFailed: 6, LastError: "invalid YAML at line 3"}
	})
	res := PollUntilReloaded(context.Background(), srv.URL, applyTime)
	if res.Status != PollFailure {
		t.Fatalf("status = %v, want PollFailure", res.Status)
	}
	if res.LastError != "invalid YAML at line 3" {
		t.Fatalf("LastError: %q", res.LastError)
	}
}

func TestPollUntilReloaded_Timeout(t *testing.T) {
	applyTime := time.Now()
	srv := makeStatusServer(t, func() ReloadStatus {
		// Never advances; timeout path.
		return ReloadStatus{LastSuccessUnix: applyTime.Add(-1 * time.Hour).Unix()}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	// Override the package's poll deadline by using ctx — see implementation.
	res := PollUntilReloaded(ctx, srv.URL, applyTime)
	if res.Status != PollTimeout {
		t.Fatalf("status = %v, want PollTimeout", res.Status)
	}
}

func TestPollUntilReloaded_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal", http.StatusInternalServerError)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	res := PollUntilReloaded(ctx, srv.URL, time.Now())
	// On persistent errors we time out. (Transient errors are retried.)
	if res.Status != PollTimeout {
		t.Fatalf("status = %v, want PollTimeout", res.Status)
	}
}

// no-op: keep the imports list honest if this file ever drops fmt
var _ = fmt.Sprintf
```

### Step 2: Run tests; expect failure

```bash
cd authbridge/cmd/abctl
go test ./edit/ -run TestPollUntilReloaded -v
```
Expected: build failure — `ReloadStatus`, `PollUntilReloaded`, `PollSuccess`/`PollFailure`/`PollTimeout` undefined.

### Step 3: Write minimal implementation

Create `authbridge/cmd/abctl/edit/status.go`:

```go
package edit

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// ReloadStatus is the wire shape of the framework's /reload/status endpoint.
// Only the fields abctl uses are decoded.
type ReloadStatus struct {
	LastSuccessUnix int64  `json:"last_success_unix"`
	ReloadsOK       uint64 `json:"reloads_ok"`
	ReloadsFailed   uint64 `json:"reloads_failed"`
	LastError       string `json:"last_error"`
}

// PollResultStatus is a sum type for PollUntilReloaded outcomes.
type PollResultStatus int

const (
	PollUnknown PollResultStatus = iota
	PollSuccess
	PollFailure
	PollTimeout
)

// PollResult is what PollUntilReloaded returns.
type PollResult struct {
	Status    PollResultStatus
	LastError string // populated when Status == PollFailure
}

// pollInterval is the cadence between /reload/status fetches. 1s balances
// user-visible spinner progress with not hammering the cluster on slow
// reloads.
const pollInterval = 1 * time.Second

// PollUntilReloaded watches statusURL/reload/status until either:
//   - LastSuccessUnix > applyTime.Unix() → PollSuccess.
//   - ReloadsFailed exceeds the value at first successful poll → PollFailure
//     with LastError populated.
//   - ctx is done → PollTimeout. (Caller is expected to set a 120s timeout
//     via context.WithTimeout.)
//
// HTTP errors (network, non-200) are retried until ctx expires; we treat
// them as "framework not yet reachable, keep waiting."
func PollUntilReloaded(ctx context.Context, statusURL string, applyTime time.Time) PollResult {
	url := statusURL + "/reload/status"
	client := &http.Client{Timeout: 2 * time.Second}

	var baselineFailed uint64
	first := true

	for {
		select {
		case <-ctx.Done():
			return PollResult{Status: PollTimeout}
		default:
		}

		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return PollResult{Status: PollTimeout} // ctx-canceled mid-build
		}
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode == 200 {
			var rs ReloadStatus
			decodeErr := json.NewDecoder(resp.Body).Decode(&rs)
			resp.Body.Close()
			if decodeErr == nil {
				if first {
					baselineFailed = rs.ReloadsFailed
					first = false
				}
				if rs.LastSuccessUnix > applyTime.Unix() {
					return PollResult{Status: PollSuccess}
				}
				if rs.ReloadsFailed > baselineFailed {
					return PollResult{Status: PollFailure, LastError: rs.LastError}
				}
			}
		} else if resp != nil {
			resp.Body.Close()
		}

		select {
		case <-ctx.Done():
			return PollResult{Status: PollTimeout}
		case <-time.After(pollInterval):
		}
	}
}
```

### Step 4: Run tests; expect PASS

```bash
cd authbridge/cmd/abctl
go test ./edit/ -v
go vet ./edit/
```
Expected: all 17 tests pass, vet clean.

### Step 5: Commit

```bash
cd /Users/haihuang/works/go/src/github.com/rossoctl/cortex
git add authbridge/cmd/abctl/edit/status.go \
        authbridge/cmd/abctl/edit/status_test.go
git commit -s -m "feat(abctl): Poll /reload/status to detect framework reload

Watches last_success_unix advancing past applyTime (success) and
reloads_failed incrementing past the pre-apply baseline (failure with
the framework's error message). 1s cadence. Caller sets the 120s
timeout via context.WithTimeout.

Transient HTTP errors retry until ctx expires.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 7: tui/edit_overlay.go — State + render

Overlay state machine + per-phase render functions. No Cmd factories yet (Task 8); no Update wiring yet (Task 9). This task purely defines the shape.

**Files:**
- Create: `authbridge/cmd/abctl/tui/edit_overlay.go`
- Create: `authbridge/cmd/abctl/tui/edit_overlay_test.go`

### Step 1: Write the failing tests

Create `authbridge/cmd/abctl/tui/edit_overlay_test.go`:

```go
package tui

import (
	"strings"
	"testing"
)

func TestEditOverlayRender_Fetching(t *testing.T) {
	s := editState{phase: editPhaseFetching}
	out := renderEditOverlay(s, 80, 20)
	if !strings.Contains(out, "Fetching ConfigMap") {
		t.Fatalf("fetching phase missing message:\n%s", out)
	}
}

func TestEditOverlayRender_Diff(t *testing.T) {
	s := editState{
		phase: editPhaseDiff,
		diff:  "-old line\n+new line\n",
	}
	out := renderEditOverlay(s, 80, 20)
	if !strings.Contains(out, "old line") || !strings.Contains(out, "new line") {
		t.Fatalf("diff content missing:\n%s", out)
	}
	if !strings.Contains(out, "(y/N)") {
		t.Fatalf("confirm prompt missing:\n%s", out)
	}
}

func TestEditOverlayRender_Applying(t *testing.T) {
	s := editState{phase: editPhaseApplying}
	out := renderEditOverlay(s, 80, 20)
	if !strings.Contains(out, "Applying") {
		t.Fatalf("applying phase missing message:\n%s", out)
	}
}

func TestEditOverlayRender_Waiting(t *testing.T) {
	s := editState{phase: editPhaseWaiting}
	out := renderEditOverlay(s, 80, 20)
	if !strings.Contains(out, "reload") {
		t.Fatalf("waiting phase should mention reload:\n%s", out)
	}
}

func TestEditOverlayRender_Error(t *testing.T) {
	s := editState{phase: editPhaseError, err: "kubectl: forbidden"}
	out := renderEditOverlay(s, 80, 20)
	if !strings.Contains(out, "forbidden") {
		t.Fatalf("error message not surfaced:\n%s", out)
	}
}
```

### Step 2: Run tests; expect failure

```bash
cd authbridge/cmd/abctl
go test ./tui/ -run TestEditOverlayRender -v
```
Expected: build failure — `editState`, `editPhase*`, `renderEditOverlay` undefined.

### Step 3: Write minimal implementation

Create `authbridge/cmd/abctl/tui/edit_overlay.go`:

```go
package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/edit"
)

// editPhase tracks where the edit state machine currently sits.
type editPhase int

const (
	editPhaseDone editPhase = iota // not editing
	editPhaseFetching
	editPhaseEditing // $EDITOR is running; bubbletea is suspended
	editPhaseValidating
	editPhaseDiff
	editPhaseApplying
	editPhaseWaiting
	editPhaseError
)

// editState lives on *model when an edit is in flight.
type editState struct {
	phase     editPhase
	fetched   *edit.FetchedPipeline
	tempPath  string
	editedRaw []byte // bytes the user wrote in $EDITOR
	diff      string // colorized output from edit.Diff
	err       string // single-line message in editPhaseError
	applyTime time.Time
}

// renderEditOverlay returns the overlay content (rendered into a
// styled box) for the current edit phase. width/height are the
// terminal's full dimensions; the overlay sizes itself to fit
// comfortably inside.
func renderEditOverlay(s editState, width, height int) string {
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(1, 2).
		Width(min(width-4, 100))

	var b strings.Builder
	switch s.phase {
	case editPhaseFetching:
		b.WriteString(styleTitle.Render("Edit pipeline"))
		b.WriteString("\n\n")
		b.WriteString("Fetching ConfigMap…")
	case editPhaseEditing:
		b.WriteString(styleTitle.Render("Edit pipeline"))
		b.WriteString("\n\n")
		b.WriteString(fmt.Sprintf("Editor open at %s", s.tempPath))
	case editPhaseValidating:
		b.WriteString(styleTitle.Render("Edit pipeline"))
		b.WriteString("\n\n")
		b.WriteString("Validating YAML…")
	case editPhaseDiff:
		b.WriteString(styleTitle.Render("Edit pipeline — review diff"))
		b.WriteString("\n\n")
		b.WriteString(s.diff)
		b.WriteString("\n")
		b.WriteString(styleHint.Render("apply this change? (y/N)"))
	case editPhaseApplying:
		b.WriteString(styleTitle.Render("Edit pipeline"))
		b.WriteString("\n\n")
		b.WriteString("Applying to ConfigMap…")
	case editPhaseWaiting:
		b.WriteString(styleTitle.Render("Edit pipeline"))
		b.WriteString("\n\n")
		b.WriteString("Waiting for hot-reload…")
		b.WriteString("\n")
		b.WriteString(styleHint.Render("(this can take up to 120s while kubelet syncs the ConfigMap)"))
	case editPhaseError:
		b.WriteString(styleTitle.Render("Edit pipeline — error"))
		b.WriteString("\n\n")
		b.WriteString(s.err)
		b.WriteString("\n\n")
		b.WriteString(styleHint.Render("[r] re-edit  [Esc] back to Pipeline"))
	}
	return box.Render(b.String())
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
```

### Step 4: Run tests; expect PASS

```bash
cd authbridge/cmd/abctl
go test ./tui/ -v
go vet ./tui/
```
Expected: 5 new tests pass, all existing TUI tests still pass.

### Step 5: Commit

```bash
cd /Users/haihuang/works/go/src/github.com/rossoctl/cortex
git add authbridge/cmd/abctl/tui/edit_overlay.go \
        authbridge/cmd/abctl/tui/edit_overlay_test.go
git commit -s -m "feat(abctl): Add edit overlay state + render

editPhase enum, editState struct, renderEditOverlay function for each
phase (fetching/editing/validating/diff/applying/waiting/error). No
state-machine wiring yet (Task 8 adds Cmd factories, Task 9 wires
Update handlers + the e keybind).

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 8: edit/edit.go — tea.Cmd factories

Wrap each phase's work in a `tea.Cmd` factory that produces the corresponding `tea.Msg`. The factories live in the `edit` package alongside their implementation; the TUI Update handlers (Task 9) just dispatch them.

**Files:**
- Create: `authbridge/cmd/abctl/edit/edit.go`
- Create: `authbridge/cmd/abctl/edit/edit_test.go`

### Step 1: Write the failing tests

Create `authbridge/cmd/abctl/edit/edit_test.go`:

```go
package edit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestFetchCmd_Success(t *testing.T) {
	stub := func(ctx context.Context, args ...string) ([]byte, error) {
		return []byte(fixtureCMYAML), nil
	}
	cmd := FetchCmd(context.Background(), stub, "team1", "email-agent")
	msg := cmd().(FetchedMsg)
	if msg.Err != nil {
		t.Fatalf("FetchedMsg.Err = %v", msg.Err)
	}
	if msg.Fetched == nil {
		t.Fatal("FetchedMsg.Fetched is nil")
	}
	if msg.TempPath == "" {
		t.Fatal("FetchedMsg.TempPath empty (should be the path to the subtree-only tempfile)")
	}
}

func TestFetchCmd_Error(t *testing.T) {
	stub := func(ctx context.Context, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("forbidden")
	}
	cmd := FetchCmd(context.Background(), stub, "team1", "email-agent")
	msg := cmd().(FetchedMsg)
	if msg.Err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(msg.Err.Error(), "forbidden") {
		t.Fatalf("error: %v", msg.Err)
	}
}

func TestApplyCmd_Success(t *testing.T) {
	stub := func(ctx context.Context, args ...string) ([]byte, error) {
		return []byte("applied"), nil
	}
	cmd := ApplyCmd(context.Background(), stub, []byte("manifest"))
	msg := cmd().(AppliedMsg)
	if msg.Err != nil {
		t.Fatalf("err = %v", msg.Err)
	}
	if msg.ApplyTime.IsZero() {
		t.Fatal("ApplyTime not set")
	}
}

func TestPollCmd_Success(t *testing.T) {
	applyTime := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Always report last_success after applyTime.
		_, _ = w.Write([]byte(`{"last_success_unix":99999999999}`))
	}))
	defer srv.Close()
	cmd := PollCmd(context.Background(), srv.URL, applyTime)
	msg := cmd().(PolledMsg)
	if msg.Result.Status != PollSuccess {
		t.Fatalf("status: %v", msg.Result.Status)
	}
}

// Compile-time guard: each factory returns a tea.Cmd.
var (
	_ tea.Cmd = FetchCmd(context.Background(), nil, "", "")
	_ tea.Cmd = ApplyCmd(context.Background(), nil, nil)
	_ tea.Cmd = PollCmd(context.Background(), "", time.Time{})
)
```

Add `"fmt"` to the test file's imports if not already present.

### Step 2: Run tests; expect failure

```bash
cd authbridge/cmd/abctl
go test ./edit/ -run "TestFetchCmd|TestApplyCmd|TestPollCmd" -v
```
Expected: build failure — `FetchCmd`, `FetchedMsg`, `ApplyCmd`, `AppliedMsg`, `PollCmd`, `PolledMsg` undefined.

### Step 3: Write minimal implementation

Create `authbridge/cmd/abctl/edit/edit.go`:

```go
package edit

import (
	"context"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// FetchedMsg is the result of FetchCmd. On success: Fetched and TempPath
// are both set, Err is nil. On failure: Err is populated, others are zero.
type FetchedMsg struct {
	Fetched  *FetchedPipeline
	TempPath string // path to the tempfile holding just the pipeline subtree
	Err      error
}

// FetchCmd returns a tea.Cmd that fetches the agent's ConfigMap, locates
// the pipeline subtree, writes the subtree to a tempfile (ready for
// $EDITOR), and emits FetchedMsg. The tempfile lives in $TMPDIR; abctl
// leaves it in place on every exit path (success, error, abort) so users
// can recover an in-progress edit.
func FetchCmd(ctx context.Context, run Runner, namespace, agent string) tea.Cmd {
	return func() tea.Msg {
		fp, err := Fetch(ctx, run, namespace, agent)
		if err != nil {
			return FetchedMsg{Err: err}
		}
		tmp, err := os.CreateTemp("", "abctl-pipeline-*.yaml")
		if err != nil {
			return FetchedMsg{Err: err}
		}
		subtree := fp.InnerYAML[fp.PipelineStart:fp.PipelineEnd]
		if _, err := tmp.Write(subtree); err != nil {
			tmp.Close()
			return FetchedMsg{Err: err}
		}
		path := tmp.Name()
		if err := tmp.Close(); err != nil {
			return FetchedMsg{Err: err}
		}
		return FetchedMsg{Fetched: fp, TempPath: path}
	}
}

// AppliedMsg is the result of ApplyCmd.
type AppliedMsg struct {
	ApplyTime time.Time
	Err       error
}

// ApplyCmd returns a tea.Cmd that runs kubectl apply --server-side on
// the supplied manifest and emits AppliedMsg with the apply timestamp.
func ApplyCmd(ctx context.Context, run Runner, manifest []byte) tea.Cmd {
	return func() tea.Msg {
		at, err := Apply(ctx, run, manifest)
		return AppliedMsg{ApplyTime: at, Err: err}
	}
}

// PolledMsg is the result of PollCmd.
type PolledMsg struct {
	Result PollResult
}

// PollCmd returns a tea.Cmd that polls /reload/status until the framework
// reload completes (success or failure) or ctx expires. Emits PolledMsg.
//
// Caller should construct ctx with a 120s WithTimeout so the poll
// terminates if kubelet doesn't sync within a reasonable window.
func PollCmd(ctx context.Context, statusURL string, applyTime time.Time) tea.Cmd {
	return func() tea.Msg {
		return PolledMsg{Result: PollUntilReloaded(ctx, statusURL, applyTime)}
	}
}
```

### Step 4: Run tests; expect PASS

```bash
cd authbridge/cmd/abctl
go test ./edit/ -v
go vet ./edit/
```
Expected: all tests pass, vet clean.

### Step 5: Commit

```bash
cd /Users/haihuang/works/go/src/github.com/rossoctl/cortex
git add authbridge/cmd/abctl/edit/edit.go \
        authbridge/cmd/abctl/edit/edit_test.go
git commit -s -m "feat(abctl): Add tea.Cmd factories for the edit phases

FetchCmd / ApplyCmd / PollCmd wrap the existing Fetch/Apply/PollUntilReloaded
implementations in tea.Cmd shapes; each emits its corresponding tea.Msg
(FetchedMsg/AppliedMsg/PolledMsg). Tested with stub Runners and a fake
status server.

The validate + diff + edit phases don't need separate Cmd factories —
they're synchronous, small, and live inside the TUI Update handlers
(Task 9).

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 9: TUI integration — Update wiring + "e" keybind + end-to-end

This is the biggest task: wire the state machine into `tui/app.go`, add the `e` keybind on `panePipeline`, render the overlay when `m.editState.phase != editPhaseDone`, and add an end-to-end test that exercises the full flow with stubs.

**Files:**
- Modify: `authbridge/cmd/abctl/tui/app.go` — add `editState` field, `View` overlay branch, Update handlers.
- Modify: `authbridge/cmd/abctl/tui/keys.go` — `e` keybind on `panePipeline`.
- Modify: `authbridge/cmd/abctl/tui/picker_test.go` (extend existing tests if any reference the model shape).
- Create: `authbridge/cmd/abctl/tui/edit_e2e_test.go` — full state-machine drive with stubs.

### Step 1: Write the failing end-to-end test

Create `authbridge/cmd/abctl/tui/edit_e2e_test.go`:

```go
package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/edit"
)

const fixtureCMYAML = `apiVersion: v1
kind: ConfigMap
metadata:
  name: authbridge-config-email-agent
  namespace: team1
data:
  config.yaml: |
    mode: proxy-sidecar
    pipeline:
      inbound:
        - name: jwt-validation
    session:
      enabled: true
`

// fakeRunner records args + returns canned responses.
type fakeRunner struct {
	getResponse  []byte
	applyError   error
	captured     []string // args of each call, joined by " "
	applyManifest []byte
}

func (f *fakeRunner) run(ctx context.Context, args ...string) ([]byte, error) {
	f.captured = append(f.captured, strings.Join(args, " "))
	if len(args) > 0 && args[0] == "get" {
		return f.getResponse, nil
	}
	if len(args) > 0 && args[0] == "apply" {
		// Read the manifest the orchestrator wrote.
		path := args[len(args)-1]
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		f.applyManifest = b
		if f.applyError != nil {
			return nil, f.applyError
		}
		return []byte("applied"), nil
	}
	return nil, nil
}

// TestEditFlow_HappyPath drives the full state machine: e → fetch → (skip
// editor) → validate → diff → apply → poll → done.
//
// We bypass the editor by writing the "edited" content to the tempfile
// directly, then injecting the editorExitedMsg with err=nil.
func TestEditFlow_HappyPath(t *testing.T) {
	runner := &fakeRunner{getResponse: []byte(fixtureCMYAML)}

	// Stub /reload/status to report success on every poll.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"last_success_unix": 99999999999,
		})
	}))
	defer srv.Close()

	m := newPickerModel(context.Background(), nil, nil)
	m.statusURL = srv.URL
	m.editRunner = runner.run
	m.selectedNamespace = "team1"
	m.selectedPod = "email-agent"
	m.pane = panePipeline

	// Press "e".
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	mm := updated.(*model)
	if mm.editState.phase != editPhaseFetching {
		t.Fatalf("phase = %v, want editPhaseFetching", mm.editState.phase)
	}

	// Run FetchCmd.
	fetchedMsg := cmd().(edit.FetchedMsg)
	if fetchedMsg.Err != nil {
		t.Fatalf("Fetch failed: %v", fetchedMsg.Err)
	}

	// Bypass the editor: write a modified subtree to the tempfile
	// directly, then deliver editorExitedMsg.
	editedSubtree := []byte(`pipeline:
  inbound:
    - name: jwt-validation
      config:
        new_key: new_value
`)
	if err := os.WriteFile(fetchedMsg.TempPath, editedSubtree, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(fetchedMsg.TempPath)

	updated, _ = mm.Update(fetchedMsg)
	mm = updated.(*model)
	if mm.editState.phase != editPhaseEditing {
		t.Fatalf("phase = %v, want editPhaseEditing", mm.editState.phase)
	}

	// Skip past the ExecProcess (we'd normally suspend bubbletea here).
	updated, _ = mm.Update(editorExitedMsg{err: nil})
	mm = updated.(*model)
	if mm.editState.phase != editPhaseDiff {
		t.Fatalf("phase = %v, want editPhaseDiff (validate should pass)", mm.editState.phase)
	}
	if mm.editState.diff == "" {
		t.Fatal("diff should be populated")
	}

	// Press "y" to confirm.
	updated, cmd = mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	mm = updated.(*model)
	if mm.editState.phase != editPhaseApplying {
		t.Fatalf("phase = %v, want editPhaseApplying", mm.editState.phase)
	}

	// Run ApplyCmd.
	appliedMsg := cmd().(edit.AppliedMsg)
	if appliedMsg.Err != nil {
		t.Fatalf("apply failed: %v", appliedMsg.Err)
	}

	updated, cmd = mm.Update(appliedMsg)
	mm = updated.(*model)
	if mm.editState.phase != editPhaseWaiting {
		t.Fatalf("phase = %v, want editPhaseWaiting", mm.editState.phase)
	}

	// Run PollCmd.
	polledMsg := cmd().(edit.PolledMsg)
	if polledMsg.Result.Status != edit.PollSuccess {
		t.Fatalf("poll status = %v, want PollSuccess", polledMsg.Result.Status)
	}

	updated, _ = mm.Update(polledMsg)
	mm = updated.(*model)
	if mm.editState.phase != editPhaseDone {
		t.Fatalf("phase = %v, want editPhaseDone", mm.editState.phase)
	}

	// The applied manifest should contain the new content.
	if !strings.Contains(string(runner.applyManifest), "new_key: new_value") {
		t.Fatalf("manifest missing new content:\n%s", runner.applyManifest)
	}
}

// TestEditFlow_NCancelsAtDiff verifies "N" at the confirm prompt
// returns to panePipeline without applying.
func TestEditFlow_NCancelsAtDiff(t *testing.T) {
	runner := &fakeRunner{getResponse: []byte(fixtureCMYAML)}
	m := newPickerModel(context.Background(), nil, nil)
	m.editRunner = runner.run
	m.selectedNamespace = "team1"
	m.selectedPod = "email-agent"
	m.pane = panePipeline

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	fetchedMsg := cmd().(edit.FetchedMsg)
	mm := updated.(*model).Update(fetchedMsg)
	mm2 := mm.(*model)

	// Pretend the user edited.
	editedSubtree := []byte("pipeline:\n  inbound:\n    - name: jwt-validation\n      config: {x: 1}\n")
	_ = os.WriteFile(fetchedMsg.TempPath, editedSubtree, 0o600)
	defer os.Remove(fetchedMsg.TempPath)

	updated, _ = mm2.Update(editorExitedMsg{err: nil})
	mm2 = updated.(*model)

	// Press "N".
	updated, _ = mm2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'N'}})
	mm2 = updated.(*model)
	if mm2.editState.phase != editPhaseDone {
		t.Fatalf("phase = %v, want editPhaseDone (N should cancel)", mm2.editState.phase)
	}
	// No apply should have run.
	for _, c := range runner.captured {
		if strings.HasPrefix(c, "apply") {
			t.Fatalf("apply ran despite N: %q", c)
		}
	}
	_ = filepath.Base(fetchedMsg.TempPath) // silence unused
}
```

### Step 2: Run tests; expect failure

```bash
cd authbridge/cmd/abctl
go test ./tui/ -run TestEditFlow -v
```
Expected: build failure — `m.statusURL`, `m.editRunner`, `editorExitedMsg`, the e-keybind handler, etc. all undefined.

### Step 3: Wire the model + Update + keybind

In `authbridge/cmd/abctl/tui/app.go`, add fields to `*model`:

```go
// Edit state. editState.phase == editPhaseDone means "no edit in flight"
// and the rest of the fields are zero. statusURL is set by the picker
// when port-forward succeeds (PortForward.StatusEndpoint()). editRunner
// is the kubectl Runner for the edit package (set in main.go to
// edit.DefaultRunner; tests inject a stub).
editState  editState
statusURL  string
editRunner edit.Runner
```

Add to the imports:
```go
"github.com/rossoctl/cortex/authbridge/cmd/abctl/edit"
```

Add new `tea.Msg` types near the others:

```go
type editorExitedMsg struct{ err error }
```

(`edit.FetchedMsg`, `edit.AppliedMsg`, `edit.PolledMsg` are imported types.)

Wire up `Update` cases. Find the type switch on `msg` in `Update()` and add:

```go
case edit.FetchedMsg:
	if msg.Err != nil {
		m.editState.phase = editPhaseError
		m.editState.err = msg.Err.Error()
		return m, nil
	}
	m.editState.fetched = msg.Fetched
	m.editState.tempPath = msg.TempPath
	m.editState.phase = editPhaseEditing
	return m, openEditorCmd(msg.TempPath)

case editorExitedMsg:
	if msg.err != nil {
		m.editState.phase = editPhaseError
		m.editState.err = "editor exited: " + msg.err.Error()
		return m, nil
	}
	// Read the edited bytes; validate as YAML; compute diff.
	edited, err := os.ReadFile(m.editState.tempPath)
	if err != nil {
		m.editState.phase = editPhaseError
		m.editState.err = "read edited file: " + err.Error()
		return m, nil
	}
	m.editState.editedRaw = edited
	originalSubtree := m.editState.fetched.InnerYAML[m.editState.fetched.PipelineStart:m.editState.fetched.PipelineEnd]
	if string(edited) == string(originalSubtree) {
		// No changes — return to panePipeline.
		m.editState = editState{phase: editPhaseDone}
		return m, nil
	}
	if !validYAML(edited) {
		m.editState.phase = editPhaseError
		m.editState.err = "edited file is not valid YAML"
		return m, nil
	}
	m.editState.diff = edit.Diff(originalSubtree, edited)
	m.editState.phase = editPhaseDiff
	return m, nil

case edit.AppliedMsg:
	if msg.Err != nil {
		m.editState.phase = editPhaseError
		m.editState.err = "apply failed: " + msg.Err.Error()
		return m, nil
	}
	m.editState.applyTime = msg.ApplyTime
	m.editState.phase = editPhaseWaiting
	return m, edit.PollCmd(m.ctx, m.statusURL, msg.ApplyTime)

case edit.PolledMsg:
	switch msg.Result.Status {
	case edit.PollSuccess:
		m.editState = editState{phase: editPhaseDone}
		// Refresh /v1/pipeline so the pane shows the new state.
		return m, m.loadPipelineCmd()
	case edit.PollFailure:
		m.editState.phase = editPhaseError
		m.editState.err = "reload failed: " + msg.Result.LastError
		return m, nil
	case edit.PollTimeout:
		m.editState.phase = editPhaseError
		m.editState.err = "reload not observed in 120s; check kubectl logs"
		return m, nil
	}
```

Add helpers near the bottom of the file:

```go
// openEditorCmd returns a tea.Cmd that suspends bubbletea, runs $EDITOR
// (vi if unset) on path, and emits editorExitedMsg when the editor exits.
func openEditorCmd(path string) tea.Cmd {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	c := exec.Command("sh", "-c", editor+" "+path)
	return tea.ExecProcess(c, func(err error) tea.Msg {
		return editorExitedMsg{err: err}
	})
}

// validYAML returns true iff b parses as YAML.
func validYAML(b []byte) bool {
	var v any
	return yaml.Unmarshal(b, &v) == nil
}
```

Add to imports: `"os"`, `"os/exec"`, `"gopkg.in/yaml.v3"`.

Update `View()` to render the overlay when an edit is in flight. Find the existing view function and add this near the start:

```go
// Edit overlay takes over the screen while an edit is in flight.
if m.editState.phase != editPhaseDone {
	return renderEditOverlay(m.editState, m.width, m.height)
}
```

In `authbridge/cmd/abctl/tui/keys.go`, add the `e` keybind to the `panePipeline` arm of the key switch. Also add the `y`/`N`/`r`/`Esc` handlers for the edit overlay phases:

```go
// Edit overlay takes over key input while editing is in flight.
if m.editState.phase != editPhaseDone {
	return m.handleEditKey(msg)
}
```

Then define `handleEditKey` in `keys.go`:

```go
// handleEditKey is the keymap that takes over while an edit is in flight.
func (m *model) handleEditKey(msg tea.KeyMsg) tea.Cmd {
	switch m.editState.phase {
	case editPhaseDiff:
		switch msg.String() {
		case "y", "Y":
			m.editState.phase = editPhaseApplying
			newSubtree := m.editState.editedRaw
			newInner := edit.Splice(
				m.editState.fetched.InnerYAML,
				m.editState.fetched.PipelineStart,
				m.editState.fetched.PipelineEnd,
				newSubtree,
			)
			manifest, err := edit.BuildManifest(m.editState.fetched.ConfigMapYAML, newInner)
			if err != nil {
				m.editState.phase = editPhaseError
				m.editState.err = "build manifest: " + err.Error()
				return nil
			}
			return edit.ApplyCmd(m.ctx, m.editRunner, manifest)
		case "n", "N", "esc":
			m.editState = editState{phase: editPhaseDone}
			return nil
		}
		return nil
	case editPhaseError:
		switch msg.String() {
		case "r":
			// Re-open editor with the same tempfile.
			m.editState.phase = editPhaseEditing
			return openEditorCmd(m.editState.tempPath)
		case "esc":
			m.editState = editState{phase: editPhaseDone}
			return nil
		}
		return nil
	}
	// Other phases: only Esc cancels (best-effort; in-flight Cmds keep running but their msgs are dropped by the editPhaseDone gate).
	if msg.String() == "esc" {
		m.editState = editState{phase: editPhaseDone}
		return nil
	}
	return nil
}
```

In `keys.go`, in the `panePipeline` arm of the regular key switch, add:

```go
case "e":
	if m.editState.phase != editPhaseDone {
		return nil // already editing
	}
	m.editState = editState{phase: editPhaseFetching}
	return edit.FetchCmd(m.ctx, m.editRunner, m.selectedNamespace, m.selectedPod)
```

In `main.go`, wire the production runner + statusURL after the picker spawns the port-forward. Add to the model setup (or where the apiclient gets set):

```go
m.editRunner = edit.DefaultRunner
m.statusURL = portForward.StatusEndpoint()
```

(The exact splice depends on existing main.go shape. Read the file before patching.)

### Step 4: Run tests; expect PASS

```bash
cd authbridge/cmd/abctl
go test ./tui/ -v
go vet ./tui/
go build ./...
```
Expected: all tests pass, vet clean, binary builds.

### Step 5: Commit

```bash
cd /Users/haihuang/works/go/src/github.com/rossoctl/cortex
git add authbridge/cmd/abctl/tui/app.go \
        authbridge/cmd/abctl/tui/keys.go \
        authbridge/cmd/abctl/tui/edit_e2e_test.go \
        authbridge/cmd/abctl/main.go
git commit -s -m "feat(abctl): Wire edit state machine + e keybind

Adds editState/statusURL/editRunner fields on *model. Update handlers
for the four msgs (FetchedMsg, editorExitedMsg, AppliedMsg, PolledMsg)
advance the state machine. View() overlays the edit UI when an edit
is in flight; keys.go's handleEditKey takes over key input during
that time (y/N/r/Esc).

The e keybind on panePipeline kicks off the flow. main.go wires
edit.DefaultRunner + the picker's StatusEndpoint into the model
post-port-forward.

End-to-end test exercises the full flow with a stub kubectl Runner
and a fake /reload/status server.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Task 10: README + e2e

Document the keybind, RBAC requirement, and tempfile lifecycle. Add an opt-in e2e test if a live cluster is handy.

**Files:**
- Modify: `authbridge/cmd/abctl/README.md`
- Optional: `authbridge/cmd/abctl/edit/e2e_test.go` (build-tag `e2e`)

### Step 1: Update README

Add to the keybinds table:

```
| `e`   | pipeline | edit pipeline subtree in $EDITOR |
| `y`   | edit/diff | apply the edit |
| `N`   | edit/diff | abort the edit |
| `r`   | edit/error | re-open the editor with the same tempfile |
| `Esc` | edit/* | abort the edit, return to Pipeline pane |
```

Add a new section:

```markdown
## Editing the pipeline

Press `e` on the Pipeline pane to edit the agent's runtime `pipeline:`
subtree in `$EDITOR` (or `vi` if unset). On save, abctl shows a diff
and asks `apply this change? (y/N)`. Confirming runs
`kubectl apply --server-side` against the per-agent ConfigMap, then
polls the framework's `/reload/status` until the reload completes
(success or failure).

The single edit flow covers four operations:
- **Edit a value** — change a config field of an existing plugin
- **Reorder** — move a plugin's lines up or down
- **Remove** — delete a plugin's entry from `inbound:` or `outbound:`
- **Add** — append a new plugin entry

All four work because they're all just lines you change inside the
pipeline subtree.

### Permissions

abctl shells out to `kubectl`; kubectl uses your kubeconfig. Editing
requires `update` on `configmaps` in the agent's namespace (in
addition to `get pods` which the picker already needs). RBAC denial
surfaces verbatim in the overlay.

### Tempfile lifecycle

abctl writes the editable pipeline subtree to `$TMPDIR/abctl-pipeline-*.yaml`
on every edit. The tempfile is **left in place on every exit path**
(success, error, abort) so an interrupted edit is recoverable. Clean
up `/tmp/abctl-pipeline-*` periodically.

### Hot-reload window

The framework reloads via a config-file watcher; kubelet syncs
ConfigMap edits into the pod's mount within ~60s, then the framework
debounces and reloads. Total wall-clock from `apply` to reload is
typically under 90s. abctl shows a spinner during the wait. If
`/reload/status` doesn't observe a successful reload within 120s,
abctl gives up watching and tells you to check `kubectl logs deploy/<agent>`.
```

### Step 2: Verify markdown rendering

```bash
head -100 authbridge/cmd/abctl/README.md
```
Confirm the new section reads cleanly.

### Step 3: Commit

```bash
cd /Users/haihuang/works/go/src/github.com/rossoctl/cortex
git add authbridge/cmd/abctl/README.md
git commit -s -m "docs(abctl): Document the e keybind for pipeline editing

Adds a new section explaining the edit flow, the four operations it
covers (edit/reorder/remove/add), the RBAC requirement (update
configmaps), the tempfile lifecycle, and the hot-reload wait
window. Keybinds table updated.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

### Step 4 (optional): Live e2e test

If you have an IBAC cluster running and want belt-and-braces coverage:

Create `authbridge/cmd/abctl/edit/e2e_test.go`:

```go
//go:build e2e

package edit

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestE2E_FetchExistingAgent verifies Fetch works against a real cluster.
// Requires `make demo-ibac` to have run.
func TestE2E_FetchExistingAgent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fp, err := Fetch(ctx, DefaultRunner, "team1", "email-agent")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	subtree := fp.InnerYAML[fp.PipelineStart:fp.PipelineEnd]
	if !strings.Contains(string(subtree), "pipeline:") {
		t.Fatalf("subtree missing header: %s", subtree)
	}
}
```

Run: `go test -tags=e2e ./edit/ -run TestE2E_FetchExistingAgent -v`

### Step 5 (optional): Commit e2e test

```bash
git add authbridge/cmd/abctl/edit/e2e_test.go
git commit -s -m "test(abctl): Add opt-in e2e test for edit Fetch path

Build-tag-gated; requires the IBAC demo cluster running. Verifies the
real kubectl path against an actual ConfigMap.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Self-Review Checklist

**1. Spec coverage:**
- ✅ Two-port port-forward → Task 1
- ✅ Fetch ConfigMap, locate pipeline range → Tasks 2, 4
- ✅ Splice + manifest rebuild → Task 3
- ✅ Apply via kubectl --server-side → Task 4
- ✅ Diff renderer → Task 5
- ✅ Poll /reload/status → Task 6
- ✅ Overlay state + render → Task 7
- ✅ Cmd factories → Task 8
- ✅ Update wiring + e keybind + e2e test → Task 9
- ✅ README + RBAC docs + tempfile lifecycle → Task 10
- ✅ Acceptance criteria 1-9 covered by Task 9's e2e test
- ✅ Acceptance 10 (`go test ./...` + e2e) — Task 10

**2. Type consistency:**
- `Runner` is `func(ctx, args ...string) ([]byte, error)` everywhere (defined Task 4, used Tasks 4/8/9).
- `FetchedPipeline` fields: `ConfigMapYAML`, `InnerYAML`, `PipelineStart`, `PipelineEnd` — same name in Task 4 def + Task 8 (FetchCmd) + Task 9 (state machine).
- Cmd factory msg types: `FetchedMsg`, `AppliedMsg`, `PolledMsg` — referenced consistently. `editorExitedMsg` is TUI-local (not in `edit` package).
- `editPhase` enum values `editPhaseDone/Fetching/Editing/Validating/Diff/Applying/Waiting/Error` — same names in render (Task 7), state machine (Task 9), and tests.
- `PollResult.Status` is `PollSuccess/PollFailure/PollTimeout` — same names in Task 6, Task 8, Task 9.

**3. Placeholder scan:**
- Task 9 references `m.loadPipelineCmd()` for the post-edit refresh. This is the existing helper from the picker work — no new code needed.
- Task 9 says "the exact splice depends on existing main.go shape. Read the file before patching." This is necessary; the implementer needs to look at main.go's current model construction. Marked as such.
- No TBDs / TODOs.
