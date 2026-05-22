# Replace spiffe-helper with go-spiffe SDK — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the bundled `spiffe-helper` binary in authbridge images with an in-process `go-spiffe v2` SDK integration that exposes in-memory `X509Source` / `JWTSource` to authbridge code and mirrors SVIDs to `/opt/svid*.pem` and `/opt/jwt_svid.token` for external readers.

**Architecture:** A new `spiffe.Provider` per container owns one `workloadapi.Client`, exposing in-memory sources for the hot path and an opt-in file-mirror goroutine for back-compat. A small dependency-injection mechanism in `authlib/plugins.Build` lets the `tokenexchange` plugin receive the same Provider rather than building its own client.

**Tech Stack:** Go 1.24, `github.com/spiffe/go-spiffe/v2 v2.6.0` (already in `go.mod`), `gopkg.in/yaml.v3`, standard library only for the mirror.

**Spec:** [`docs/superpowers/specs/2026-05-22-authbridge-go-spiffe-sdk-design.md`](../specs/2026-05-22-authbridge-go-spiffe-sdk-design.md)

**Branch:** `feat/spiffe-go-sdk-replace-helper` (branched from `upstream/main`)

---

## File Structure

### New files

| Path | Responsibility |
|---|---|
| `authlib/spiffe/provider.go` | `Provider`, `ProviderConfig`, `NewProvider`, `X509Source()`, `JWTSource()`, `Close()`. Composition only — no SDK calls directly. |
| `authlib/spiffe/workload_x509.go` | Unexported `workloadX509` type implementing existing `X509Source` interface. Wraps `*workloadapi.X509Source`. |
| `authlib/spiffe/workload_jwt.go` | Unexported `workloadJWT` type implementing the `JWTSource` interface used by `tokenexchange`. Wraps `*workloadapi.JWTSource`, captures audience. |
| `authlib/spiffe/mirror.go` | `atomicWrite` helper + `mirror` goroutine that watches X.509 `Updated()` and runs a JWT refresh loop. Called only from `provider.go`. |
| `authlib/spiffe/consumer.go` | One-line `ProviderConsumer` interface used by `plugins.BuildWithSPIFFE` for DI into plugins. |
| `authlib/spiffe/provider_test.go` | Tests for `NewProvider` construction, `Close()`, `JWTAudience=""` skip path. |
| `authlib/spiffe/workload_x509_test.go` | Tests against the in-process fake workload API server (`workloadapi/fakeworkloadapi`). |
| `authlib/spiffe/workload_jwt_test.go` | Same, for JWT side. |
| `authlib/spiffe/mirror_test.go` | Atomic-write test, mirror enable/disable test, JWT refresh schedule test (using fakes). |
| `authlib/spiffe/integration_test.go` | End-to-end test booting Provider against a fake workload API and exercising rotation. |

### Files modified

| Path | Change |
|---|---|
| `authlib/config/config.go` | Drop `MTLSConfig.{CertFile,KeyFile,BundleFile}`. Add top-level `SPIFFEConfig`. Cross-block validation. Defaults. |
| `authlib/config/config_test.go` | Update mTLS tests; add SPIFFE block tests; add unknown-field tolerance pinning test. |
| `authlib/plugins/registry.go` | Add `BuildWithSPIFFE(entries, *spiffe.Provider, opts...)` calling `SetSPIFFEProvider` on consumers. |
| `authlib/plugins/registry_test.go` | Tests for the new function. |
| `authlib/plugins/tokenexchange/plugin.go` | Drop `JWTSVIDPath` field. Implement `SetSPIFFEProvider`. Use injected `JWTSource` in `buildClientAuthFrom`. |
| `authlib/plugins/tokenexchange/plugin_test.go` | Drop file-path assertions; add provider-injection tests. |
| `cmd/authbridge-proxy/main.go` | Build Provider, pass `X509Source()` to listener MTLSOptions, switch to `BuildWithSPIFFE`, drop file-existence wait loop. |
| `cmd/authbridge-envoy/main.go` | Same. |
| `cmd/authbridge-lite/main.go` | Same. |
| `cmd/authbridge-proxy/entrypoint.sh` | Remove Phase 1 spiffe-helper launch. |
| `cmd/authbridge-envoy/entrypoint.sh` | Same. |
| `cmd/authbridge-lite/entrypoint.sh` | Same. |
| `cmd/authbridge-proxy/Dockerfile` | Drop `spiffe-helper` binary install layer + helper.conf copy. |
| `cmd/authbridge-envoy/Dockerfile` | Same. |
| `cmd/authbridge-lite/Dockerfile` | Same. |
| `CLAUDE.md` | Update credential-files table around lines 247-250 to say "written by spiffe.Provider mirror". |
| `demos/mtls/README.md` | Update line 6 (spiffe-helper reference). |

### Files deleted

| Path | Reason |
|---|---|
| `authlib/spiffe/x509source.go` | `FileX509Source` no longer constructed anywhere. |
| `authlib/spiffe/x509source_test.go` | Same. |
| `authlib/plugins/tokenexchange/spiffe/file.go` | `FileJWTSource` no longer constructed anywhere. |
| `authlib/plugins/tokenexchange/spiffe/file_test.go` | Same. |

---

## Phase 1 — Build the SPIFFE library

### Task 1: Workload X.509 source

**Files:**
- Create: `authlib/spiffe/workload_x509.go`
- Create: `authlib/spiffe/workload_x509_test.go`

The existing `X509Source` interface in `authlib/spiffe/source.go` already has `Certificate()` and `TrustBundle()`. Wait — verify with Read first; the spec says the file is `source.go` but earlier exploration found `x509source.go`. Update path references to whatever the actual interface file is called. The interface signature must remain unchanged.

- [ ] **Step 1: Write failing test for `workloadX509.Certificate()`**

```go
// authlib/spiffe/workload_x509_test.go
package spiffe

import (
    "context"
    "crypto/x509"
    "testing"

    "github.com/spiffe/go-spiffe/v2/spiffeid"
    "github.com/spiffe/go-spiffe/v2/svid/x509svid"
    "github.com/spiffe/go-spiffe/v2/workloadapi"
    "github.com/spiffe/go-spiffe/v2/workloadapi/fakeworkloadapi"
)

func TestWorkloadX509_Certificate(t *testing.T) {
    ctx := context.Background()
    fake := fakeworkloadapi.New(t)
    defer fake.Stop()

    td := spiffeid.RequireTrustDomainFromString("example.org")
    id := spiffeid.RequireFromString("spiffe://example.org/workload")
    svid, _ := x509svid.New(id, fake.GenerateLeafCertificate(t, id), nil)
    bundle := fake.GenerateBundle(t, td)
    fake.SetX509SVID(svid)
    fake.SetX509Bundle(bundle)

    sdk, err := workloadapi.NewX509Source(ctx, workloadapi.WithClientOptions(workloadapi.WithAddr(fake.Addr())))
    if err != nil { t.Fatalf("NewX509Source: %v", err) }
    defer sdk.Close()

    src := newWorkloadX509(sdk, td)

    cert, err := src.Certificate()
    if err != nil { t.Fatalf("Certificate: %v", err) }
    if cert == nil || len(cert.Certificate) == 0 {
        t.Fatal("Certificate returned empty tls.Certificate")
    }

    pool, err := src.TrustBundle()
    if err != nil { t.Fatalf("TrustBundle: %v", err) }
    if pool == (*x509.CertPool)(nil) || len(pool.Subjects()) == 0 {
        t.Fatal("TrustBundle returned empty pool")
    }
}
```

> If `fakeworkloadapi` is unavailable in v2.6.0 under that import path, use `github.com/spiffe/go-spiffe/v2/spiffetest` instead. Check `go doc github.com/spiffe/go-spiffe/v2/workloadapi/fakeworkloadapi` first; if not found run `go doc github.com/spiffe/go-spiffe/v2/spiffetest`.

- [ ] **Step 2: Run test to verify it fails**

```
cd authbridge/authlib && go test ./spiffe/ -run TestWorkloadX509_Certificate -v
```
Expected: FAIL with `undefined: newWorkloadX509`.

- [ ] **Step 3: Implement `workloadX509`**

```go
// authlib/spiffe/workload_x509.go
package spiffe

import (
    "crypto/tls"
    "crypto/x509"
    "errors"
    "fmt"

    "github.com/spiffe/go-spiffe/v2/spiffeid"
    "github.com/spiffe/go-spiffe/v2/workloadapi"
)

// workloadX509 adapts a *workloadapi.X509Source to the framework
// X509Source interface. The trust domain is captured at construction time
// because TrustBundle() must look up bundles per trust domain — we always
// return the bundle for the workload's own domain (which includes federated
// domains automatically when the SPIRE server is configured for them).
type workloadX509 struct {
    sdk *workloadapi.X509Source
    td  spiffeid.TrustDomain
}

func newWorkloadX509(sdk *workloadapi.X509Source, td spiffeid.TrustDomain) *workloadX509 {
    return &workloadX509{sdk: sdk, td: td}
}

func (w *workloadX509) Certificate() (*tls.Certificate, error) {
    svid, err := w.sdk.GetX509SVID()
    if err != nil {
        return nil, fmt.Errorf("workloadX509: GetX509SVID: %w", err)
    }
    if svid == nil || len(svid.Certificates) == 0 {
        return nil, errors.New("workloadX509: SVID has no certificates")
    }
    raw := make([][]byte, 0, len(svid.Certificates))
    for _, c := range svid.Certificates {
        raw = append(raw, c.Raw)
    }
    return &tls.Certificate{
        Certificate: raw,
        PrivateKey:  svid.PrivateKey,
        Leaf:        svid.Certificates[0],
    }, nil
}

func (w *workloadX509) TrustBundle() (*x509.CertPool, error) {
    bundle, err := w.sdk.GetX509BundleForTrustDomain(w.td)
    if err != nil {
        return nil, fmt.Errorf("workloadX509: GetX509BundleForTrustDomain(%s): %w", w.td, err)
    }
    pool := x509.NewCertPool()
    for _, c := range bundle.X509Authorities() {
        pool.AddCert(c)
    }
    if len(pool.Subjects()) == 0 {
        return nil, errors.New("workloadX509: trust bundle is empty")
    }
    return pool, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

```
go test ./spiffe/ -run TestWorkloadX509_Certificate -v
```
Expected: PASS.

- [ ] **Step 5: Add error-path tests**

Add tests in `workload_x509_test.go`:
- `TestWorkloadX509_Certificate_AfterClose` — closes the SDK source, asserts `Certificate()` returns a non-nil error.
- `TestWorkloadX509_TrustBundle_EmptyBundle` — sets a bundle with zero authorities, asserts `TrustBundle()` returns the "trust bundle is empty" error.

- [ ] **Step 6: Run all spiffe tests**

```
go test ./spiffe/ -v
```
Expected: all PASS.

- [ ] **Step 7: Commit**

```
git add authbridge/authlib/spiffe/workload_x509.go authbridge/authlib/spiffe/workload_x509_test.go
git commit -s -m "feat(spiffe): add workload X.509 source backed by go-spiffe SDK

Implements the existing X509Source interface against
*workloadapi.X509Source. Used by the upcoming Provider type to feed
authbridge's TLS listeners directly from the SPIRE Workload API
without spiffe-helper.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 2: Workload JWT source

**Files:**
- Create: `authlib/spiffe/workload_jwt.go`
- Create: `authlib/spiffe/workload_jwt_test.go`

The `JWTSource` interface lives in `authlib/plugins/tokenexchange/spiffe/source.go`. The new `workloadJWT` type implements that interface but lives in the framework `authlib/spiffe` package — so the framework Provider can return it. The `tokenexchange/spiffe` package keeps its interface declaration and gets its `FileJWTSource` deleted later.

To avoid an import cycle, the `JWTSource` interface declaration is duplicated as a one-liner in `authlib/spiffe/workload_jwt.go` (interfaces are cheap; the duplicate just states "this thing has FetchToken"). The `tokenexchange` plugin keeps consuming through its own interface declaration; both interfaces have identical signatures so the same value satisfies both via Go's structural typing.

- [ ] **Step 1: Write failing test for `workloadJWT.FetchToken()`**

```go
// authlib/spiffe/workload_jwt_test.go
package spiffe

import (
    "context"
    "testing"

    "github.com/spiffe/go-spiffe/v2/spiffeid"
    "github.com/spiffe/go-spiffe/v2/workloadapi"
    "github.com/spiffe/go-spiffe/v2/workloadapi/fakeworkloadapi"
)

func TestWorkloadJWT_FetchToken(t *testing.T) {
    ctx := context.Background()
    fake := fakeworkloadapi.New(t)
    defer fake.Stop()

    id := spiffeid.RequireFromString("spiffe://example.org/workload")
    fake.SetJWTSVID(t, id, "https://keycloak/realms/test")

    sdk, err := workloadapi.NewJWTSource(ctx, workloadapi.WithClientOptions(workloadapi.WithAddr(fake.Addr())))
    if err != nil { t.Fatalf("NewJWTSource: %v", err) }
    defer sdk.Close()

    src := newWorkloadJWT(sdk, "https://keycloak/realms/test")

    tok, err := src.FetchToken(ctx)
    if err != nil { t.Fatalf("FetchToken: %v", err) }
    if tok == "" { t.Fatal("FetchToken returned empty token") }
}
```

- [ ] **Step 2: Run test to verify it fails**

```
go test ./spiffe/ -run TestWorkloadJWT_FetchToken -v
```
Expected: FAIL with `undefined: newWorkloadJWT`.

- [ ] **Step 3: Implement `workloadJWT`**

```go
// authlib/spiffe/workload_jwt.go
package spiffe

import (
    "context"
    "fmt"

    "github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
    "github.com/spiffe/go-spiffe/v2/workloadapi"
)

// JWTSource is the per-fetch JWT-SVID interface satisfied by workloadJWT.
// Identical signature to authlib/plugins/tokenexchange/spiffe.JWTSource —
// they are kept in separate packages to avoid an import cycle, and Go's
// structural typing lets one implementation satisfy both.
type JWTSource interface {
    FetchToken(ctx context.Context) (string, error)
}

// workloadJWT fetches a JWT-SVID for a fixed audience via the SPIRE
// Workload API. The SDK's *workloadapi.JWTSource caches and refreshes
// internally, so repeated FetchToken calls within a token's validity
// avoid the agent round-trip.
type workloadJWT struct {
    sdk      *workloadapi.JWTSource
    audience string
}

func newWorkloadJWT(sdk *workloadapi.JWTSource, audience string) *workloadJWT {
    return &workloadJWT{sdk: sdk, audience: audience}
}

func (w *workloadJWT) FetchToken(ctx context.Context) (string, error) {
    svid, err := w.sdk.FetchJWTSVID(ctx, jwtsvid.Params{Audience: w.audience})
    if err != nil {
        return "", fmt.Errorf("workloadJWT: FetchJWTSVID(audience=%q): %w", w.audience, err)
    }
    return svid.Marshal(), nil
}
```

- [ ] **Step 4: Run test to verify it passes**

```
go test ./spiffe/ -run TestWorkloadJWT_FetchToken -v
```
Expected: PASS.

- [ ] **Step 5: Add error-path test**

`TestWorkloadJWT_FetchToken_AgentError` — close the SDK source, call `FetchToken`, expect a wrapped error mentioning the audience.

- [ ] **Step 6: Run all spiffe tests**

```
go test ./spiffe/ -v
```
Expected: PASS.

- [ ] **Step 7: Commit**

```
git add authbridge/authlib/spiffe/workload_jwt.go authbridge/authlib/spiffe/workload_jwt_test.go
git commit -s -m "feat(spiffe): add workload JWT source backed by go-spiffe SDK

workloadJWT fetches JWT-SVIDs for a fixed audience via
*workloadapi.JWTSource. Will be returned by the upcoming Provider so
the tokenexchange plugin can produce client-assertion JWTs without
spiffe-helper.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 3: Atomic file write helper

**Files:**
- Create: `authlib/spiffe/mirror.go` (helper portion only this task)
- Create: `authlib/spiffe/mirror_test.go`

`atomicWrite` is the building block for the file mirror. Standard tmp+rename pattern; idempotent.

- [ ] **Step 1: Write failing tests for `atomicWrite`**

```go
// authlib/spiffe/mirror_test.go
package spiffe

import (
    "os"
    "path/filepath"
    "testing"
)

func TestAtomicWrite_NewFile(t *testing.T) {
    dir := t.TempDir()
    path := filepath.Join(dir, "svid.pem")
    if err := atomicWrite(path, []byte("contents"), 0o644); err != nil {
        t.Fatalf("atomicWrite: %v", err)
    }
    got, _ := os.ReadFile(path)
    if string(got) != "contents" {
        t.Errorf("got %q, want %q", got, "contents")
    }
}

func TestAtomicWrite_OverwritesExisting(t *testing.T) {
    dir := t.TempDir()
    path := filepath.Join(dir, "svid.pem")
    _ = os.WriteFile(path, []byte("old"), 0o644)
    if err := atomicWrite(path, []byte("new"), 0o644); err != nil {
        t.Fatalf("atomicWrite: %v", err)
    }
    got, _ := os.ReadFile(path)
    if string(got) != "new" {
        t.Errorf("got %q, want %q", got, "new")
    }
}

func TestAtomicWrite_NoTempLeftOnSuccess(t *testing.T) {
    dir := t.TempDir()
    path := filepath.Join(dir, "svid.pem")
    if err := atomicWrite(path, []byte("contents"), 0o644); err != nil {
        t.Fatalf("atomicWrite: %v", err)
    }
    entries, _ := os.ReadDir(dir)
    if len(entries) != 1 {
        t.Errorf("expected 1 entry, got %d: %v", len(entries), entries)
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

```
go test ./spiffe/ -run TestAtomicWrite -v
```
Expected: FAIL with `undefined: atomicWrite`.

- [ ] **Step 3: Implement `atomicWrite`**

```go
// authlib/spiffe/mirror.go
package spiffe

import (
    "fmt"
    "os"
    "path/filepath"
)

// atomicWrite writes data to path via a tmp file in the same directory
// followed by os.Rename. Same-directory rename is atomic on POSIX, which
// guarantees external readers always see either the old or the new file
// content — never a partial write.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
    dir := filepath.Dir(path)
    tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+".*")
    if err != nil {
        return fmt.Errorf("atomicWrite: create temp in %s: %w", dir, err)
    }
    tmpName := tmp.Name()
    cleanup := func() { _ = os.Remove(tmpName) }
    if _, err := tmp.Write(data); err != nil {
        _ = tmp.Close()
        cleanup()
        return fmt.Errorf("atomicWrite: write %s: %w", tmpName, err)
    }
    if err := tmp.Chmod(mode); err != nil {
        _ = tmp.Close()
        cleanup()
        return fmt.Errorf("atomicWrite: chmod %s: %w", tmpName, err)
    }
    if err := tmp.Close(); err != nil {
        cleanup()
        return fmt.Errorf("atomicWrite: close %s: %w", tmpName, err)
    }
    if err := os.Rename(tmpName, path); err != nil {
        cleanup()
        return fmt.Errorf("atomicWrite: rename %s -> %s: %w", tmpName, path, err)
    }
    return nil
}
```

- [ ] **Step 4: Run tests to verify pass**

```
go test ./spiffe/ -run TestAtomicWrite -v
```
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```
git add authbridge/authlib/spiffe/mirror.go authbridge/authlib/spiffe/mirror_test.go
git commit -s -m "feat(spiffe): add atomicWrite helper for SVID file mirror

POSIX same-directory tmp+rename so external readers of /opt/svid*.pem
during rotation always observe either the old or the new file, never
a partial write.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 4: File mirror goroutine

**Files:**
- Modify: `authlib/spiffe/mirror.go` (add `mirror` struct + `start`)
- Modify: `authlib/spiffe/mirror_test.go` (add goroutine tests)

The mirror writes:
- `<dir>/svid.pem`, `<dir>/svid_key.pem`, `<dir>/svid_bundle.pem` on every X.509 `Updated()` event.
- `<dir>/jwt_svid.token` once on start, then refreshes at 90% of token lifetime.

If the X.509 source has no JWTAudience, the JWT mirror loop is skipped.

- [ ] **Step 1: Write failing tests for mirror**

```go
// Add to authlib/spiffe/mirror_test.go
func TestMirror_X509_WritesAllThreeFiles(t *testing.T) {
    // Set up fake workload API serving an X.509 SVID + bundle
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()
    fake := fakeworkloadapi.New(t)
    defer fake.Stop()
    /* set up SVID + bundle */

    sdk, err := workloadapi.NewX509Source(ctx, workloadapi.WithClientOptions(workloadapi.WithAddr(fake.Addr())))
    if err != nil { t.Fatalf("NewX509Source: %v", err) }
    defer sdk.Close()

    dir := t.TempDir()
    m := newMirror(mirrorConfig{
        Dir:        dir,
        X509:       sdk,
        // JWTAudience left zero — no JWT mirror
    })
    go m.run(ctx)

    // Wait for first write
    waitFile(t, filepath.Join(dir, "svid.pem"), 2*time.Second)

    for _, f := range []string{"svid.pem", "svid_key.pem", "svid_bundle.pem"} {
        info, err := os.Stat(filepath.Join(dir, f))
        if err != nil {
            t.Errorf("missing %s: %v", f, err)
            continue
        }
        if info.Size() == 0 {
            t.Errorf("%s is empty", f)
        }
    }
}

func TestMirror_JWT_WritesAndRefreshes(t *testing.T) {
    // Fake JWT source returning a token with a short lifetime; assert
    // /opt/jwt_svid.token is written, then re-written after refresh window.
}

func TestMirror_DisabledByCancellation(t *testing.T) {
    ctx, cancel := context.WithCancel(context.Background())
    /* set up mirror, cancel ctx, assert no goroutine leak */
    cancel()
    /* time.Sleep small + check goroutine count via runtime.NumGoroutine before/after */
}
```

`waitFile(t, path, timeout)` is a small test helper that polls `os.Stat` until the file exists or the timeout fires.

- [ ] **Step 2: Run to verify failure**

```
go test ./spiffe/ -run TestMirror -v
```
Expected: FAIL with `undefined: newMirror, mirrorConfig`.

- [ ] **Step 3: Implement mirror**

Append to `authlib/spiffe/mirror.go`:

```go
import (
    "context"
    "encoding/pem"
    "log/slog"
    "time"

    "github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
    "github.com/spiffe/go-spiffe/v2/workloadapi"
)

type mirrorConfig struct {
    Dir          string
    X509         *workloadapi.X509Source
    JWT          *workloadapi.JWTSource // nil = no JWT mirror
    JWTAudience  string                 // ignored if JWT is nil
}

type mirror struct {
    cfg mirrorConfig
}

func newMirror(cfg mirrorConfig) *mirror { return &mirror{cfg: cfg} }

// run blocks until ctx is cancelled. Spawns one writer for X.509 and,
// if JWTAudience is set, one for JWT. Errors are logged and never
// propagate — the mirror is a best-effort companion to the in-memory
// hot path.
func (m *mirror) run(ctx context.Context) {
    if err := m.writeX509(); err != nil {
        slog.Warn("spiffe.mirror: initial x509 write", "err", err)
    }
    if m.cfg.JWT != nil && m.cfg.JWTAudience != "" {
        if err := m.writeJWT(ctx); err != nil {
            slog.Warn("spiffe.mirror: initial jwt write", "err", err)
        }
        go m.runJWT(ctx)
    }
    m.runX509(ctx)
}

func (m *mirror) runX509(ctx context.Context) {
    ch := m.cfg.X509.Updated()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ch:
            if err := m.writeX509(); err != nil {
                slog.Warn("spiffe.mirror: x509 rotation write", "err", err)
            }
        }
    }
}

func (m *mirror) writeX509() error {
    svid, err := m.cfg.X509.GetX509SVID()
    if err != nil { return err }

    // cert chain (PEM concat, leaf first)
    var certPEM []byte
    for _, c := range svid.Certificates {
        certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{
            Type: "CERTIFICATE", Bytes: c.Raw,
        })...)
    }
    if err := atomicWrite(m.cfg.Dir+"/svid.pem", certPEM, 0o644); err != nil { return err }

    // private key
    keyDER, err := svid.Marshal()
    _ = keyDER // svid.Marshal is for x509svid.SVID — verify exact API
    // Use svid.PrivateKey + x509.MarshalPKCS8PrivateKey
    keyBytes, err := marshalPKCS8(svid.PrivateKey)
    if err != nil { return err }
    keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
    if err := atomicWrite(m.cfg.Dir+"/svid_key.pem", keyPEM, 0o600); err != nil { return err }

    // trust bundle
    bundle, err := m.cfg.X509.GetX509BundleForTrustDomain(svid.ID.TrustDomain())
    if err != nil { return err }
    var bundlePEM []byte
    for _, c := range bundle.X509Authorities() {
        bundlePEM = append(bundlePEM, pem.EncodeToMemory(&pem.Block{
            Type: "CERTIFICATE", Bytes: c.Raw,
        })...)
    }
    return atomicWrite(m.cfg.Dir+"/svid_bundle.pem", bundlePEM, 0o644)
}

// marshalPKCS8 wraps x509.MarshalPKCS8PrivateKey for testability.
func marshalPKCS8(key any) ([]byte, error) {
    return x509MarshalPKCS8PrivateKey(key) // import "crypto/x509" and use x509.MarshalPKCS8PrivateKey
}

func (m *mirror) runJWT(ctx context.Context) {
    for {
        if err := m.writeJWT(ctx); err != nil {
            slog.Warn("spiffe.mirror: jwt refresh write", "err", err)
            select {
            case <-ctx.Done(): return
            case <-time.After(30 * time.Second):
            }
            continue
        }
        // Sleep until 90% of remaining lifetime, then re-fetch.
        // writeJWT stored expiry on the mirror — read it.
        sleep := m.cfg.nextJWTRefreshAfter
        if sleep <= 0 { sleep = 30 * time.Second }
        select {
        case <-ctx.Done(): return
        case <-time.After(sleep):
        }
    }
}

func (m *mirror) writeJWT(ctx context.Context) error {
    svid, err := m.cfg.JWT.FetchJWTSVID(ctx, jwtsvid.Params{Audience: m.cfg.JWTAudience})
    if err != nil { return err }
    if err := atomicWrite(m.cfg.Dir+"/jwt_svid.token", []byte(svid.Marshal()), 0o644); err != nil {
        return err
    }
    // Schedule next refresh at 90% lifetime (writeJWT doesn't loop; runJWT does).
    m.cfg.nextJWTRefreshAfter = time.Until(svid.Expiry) * 9 / 10
    return nil
}
```

> **Refactor note for the engineer:** the sketch above stores `nextJWTRefreshAfter` on `mirrorConfig` for simplicity; in the real implementation, move it to `mirror` (the receiver) so config stays immutable. Also fix the import for `crypto/x509.MarshalPKCS8PrivateKey` directly — the `marshalPKCS8` indirection in the sketch is to make the import obvious.

- [ ] **Step 4: Iterate until tests pass**

```
go test ./spiffe/ -run TestMirror -v
```
Expected: PASS. Likely 1-2 iterations to get the imports and field placement right.

- [ ] **Step 5: Commit**

```
git add authbridge/authlib/spiffe/mirror.go authbridge/authlib/spiffe/mirror_test.go
git commit -s -m "feat(spiffe): add file-mirror goroutine for SVID rotation

Subscribes to X.509 Updated() and runs a JWT refresh loop at 90%
lifetime, writing svid.pem / svid_key.pem / svid_bundle.pem /
jwt_svid.token via atomic tmp+rename. Mirror failures are logged at
WARN; the in-memory hot path remains the source of truth.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 5: Provider type composing all the pieces

**Files:**
- Create: `authlib/spiffe/provider.go`
- Create: `authlib/spiffe/provider_test.go`

The Provider holds the SDK clients, returns the framework `X509Source` / `JWTSource` adapters, and starts the mirror goroutine. `NewProvider` blocks until the first X.509-SVID arrives (cold-start gate).

- [ ] **Step 1: Write failing test**

```go
// authlib/spiffe/provider_test.go
package spiffe

import (
    "context"
    "testing"
)

func TestProvider_BasicLifecycle(t *testing.T) {
    fake := fakeworkloadapi.New(t)
    defer fake.Stop()
    /* serve X.509 SVID + bundle */

    p, err := NewProvider(context.Background(), ProviderConfig{
        SocketPath:  fake.Addr(),
        JWTAudience: "https://keycloak/realms/test",
        MirrorFiles: false, // tested separately in mirror_test.go
    })
    if err != nil { t.Fatalf("NewProvider: %v", err) }
    defer p.Close()

    if p.X509Source() == nil { t.Error("X509Source() returned nil") }
    if p.JWTSource() == nil { t.Error("JWTSource() returned nil with audience set") }
}

func TestProvider_NoJWTAudience(t *testing.T) {
    fake := fakeworkloadapi.New(t)
    defer fake.Stop()
    /* serve X.509 only */

    p, err := NewProvider(context.Background(), ProviderConfig{
        SocketPath:  fake.Addr(),
        // JWTAudience empty
        MirrorFiles: false,
    })
    if err != nil { t.Fatalf("NewProvider: %v", err) }
    defer p.Close()

    if p.JWTSource() != nil { t.Error("JWTSource() should be nil when audience empty") }
}

func TestProvider_AgentUnreachable(t *testing.T) {
    ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
    defer cancel()
    _, err := NewProvider(ctx, ProviderConfig{
        SocketPath:  "unix:///nonexistent/socket",
        MirrorFiles: false,
    })
    if err == nil { t.Fatal("expected error for unreachable agent") }
}
```

- [ ] **Step 2: Run to verify fail**

```
go test ./spiffe/ -run TestProvider -v
```
Expected: FAIL.

- [ ] **Step 3: Implement Provider**

```go
// authlib/spiffe/provider.go
package spiffe

import (
    "context"
    "errors"
    "fmt"

    "github.com/spiffe/go-spiffe/v2/workloadapi"
)

const (
    DefaultSocketPath = "unix:///spiffe-workload-api/spire-agent.sock"
    DefaultMirrorDir  = "/opt"
)

type ProviderConfig struct {
    // SocketPath is the SPIRE agent socket. Defaults to DefaultSocketPath.
    SocketPath string

    // JWTAudience is the audience for the JWT-SVID used as a client
    // assertion in token-exchange. Empty disables JWT entirely (no
    // workloadapi.JWTSource constructed, no JWT mirror).
    JWTAudience string

    // MirrorFiles enables the file-mirror goroutine (default true via
    // config.SPIFFEConfig). Disable for proxy-sidecar deployments that
    // don't need on-disk SVIDs.
    MirrorFiles bool

    // MirrorDir defaults to DefaultMirrorDir. Used only when MirrorFiles.
    MirrorDir string
}

type Provider struct {
    cfg     ProviderConfig
    x509SDK *workloadapi.X509Source
    jwtSDK  *workloadapi.JWTSource // may be nil
    x509    *workloadX509
    jwt     *workloadJWT // may be nil
    cancel  context.CancelFunc
}

// NewProvider constructs a Provider, blocking until the first
// X.509-SVID arrives or ctx is cancelled. JWT source is only
// constructed when cfg.JWTAudience is non-empty.
func NewProvider(ctx context.Context, cfg ProviderConfig) (*Provider, error) {
    if cfg.SocketPath == "" { cfg.SocketPath = DefaultSocketPath }
    if cfg.MirrorDir == "" { cfg.MirrorDir = DefaultMirrorDir }

    clientOpts := workloadapi.WithClientOptions(workloadapi.WithAddr(cfg.SocketPath))

    x509SDK, err := workloadapi.NewX509Source(ctx, clientOpts)
    if err != nil {
        return nil, fmt.Errorf("spiffe.NewProvider: x509 source: %w", err)
    }

    // Capture trust domain from first SVID.
    svid, err := x509SDK.GetX509SVID()
    if err != nil {
        _ = x509SDK.Close()
        return nil, fmt.Errorf("spiffe.NewProvider: get initial SVID: %w", err)
    }

    p := &Provider{
        cfg:     cfg,
        x509SDK: x509SDK,
        x509:    newWorkloadX509(x509SDK, svid.ID.TrustDomain()),
    }

    if cfg.JWTAudience != "" {
        jwtSDK, err := workloadapi.NewJWTSource(ctx, clientOpts)
        if err != nil {
            _ = x509SDK.Close()
            return nil, fmt.Errorf("spiffe.NewProvider: jwt source: %w", err)
        }
        p.jwtSDK = jwtSDK
        p.jwt = newWorkloadJWT(jwtSDK, cfg.JWTAudience)
    }

    if cfg.MirrorFiles {
        mctx, cancel := context.WithCancel(context.Background())
        p.cancel = cancel
        m := newMirror(mirrorConfig{
            Dir:         cfg.MirrorDir,
            X509:        x509SDK,
            JWT:         p.jwtSDK,
            JWTAudience: cfg.JWTAudience,
        })
        // Synchronous initial write so the cold-start guarantee is
        // observable to anything that races with our return.
        if err := m.writeX509(); err != nil {
            // Mirror errors are non-fatal — log only.
            // (Provider construction succeeds even if mirror dir is
            // unwriteable; the in-memory path is the source of truth.)
        }
        if p.jwtSDK != nil {
            _ = m.writeJWT(ctx)
        }
        go m.run(mctx)
    }

    return p, nil
}

func (p *Provider) X509Source() X509Source { return p.x509 }

// JWTSource returns nil when the Provider was constructed with
// JWTAudience == "". Callers that get nil unexpectedly indicate a
// config-validation gap (cross-block check should have rejected the
// config at startup).
func (p *Provider) JWTSource() JWTSource {
    if p.jwt == nil { return nil }
    return p.jwt
}

func (p *Provider) Close() error {
    var errs []error
    if p.cancel != nil { p.cancel() }
    if p.jwtSDK != nil {
        if err := p.jwtSDK.Close(); err != nil { errs = append(errs, err) }
    }
    if p.x509SDK != nil {
        if err := p.x509SDK.Close(); err != nil { errs = append(errs, err) }
    }
    return errors.Join(errs...)
}
```

> The framework `X509Source` interface is referenced as `X509Source` here. If the existing interface in `authlib/spiffe/source.go` is in a different file, this code stays put — it's the same package.

- [ ] **Step 4: Run tests**

```
go test ./spiffe/ -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```
git add authbridge/authlib/spiffe/provider.go authbridge/authlib/spiffe/provider_test.go
git commit -s -m "feat(spiffe): add Provider tying together SDK sources and mirror

NewProvider blocks until the first X.509-SVID arrives (cold-start
gate), constructs the JWT source only when JWTAudience is non-empty,
and starts the file-mirror goroutine when MirrorFiles is true. Close
shuts down both SDK sources and the mirror via context cancellation.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Phase 2 — Config schema

### Task 6: Add SPIFFEConfig

**Files:**
- Modify: `authlib/config/config.go`
- Modify: `authlib/config/config_test.go`

Read `authlib/config/config.go` first to see how `MTLSConfig`, `Load`, `applyDefaults`, and `Validate` are structured. Add `SPIFFEConfig` following the same pattern.

- [ ] **Step 1: Add failing test**

```go
// authlib/config/config_test.go
func TestSPIFFEConfig_Defaults(t *testing.T) {
    yaml := `
spiffe:
  jwt_audience: "https://keycloak/realms/test"
`
    cfg, err := Load([]byte(yaml))
    if err != nil { t.Fatalf("Load: %v", err) }
    if cfg.SPIFFE == nil { t.Fatal("SPIFFE block missing") }
    if cfg.SPIFFE.Socket != "unix:///spiffe-workload-api/spire-agent.sock" {
        t.Errorf("Socket = %q, want default", cfg.SPIFFE.Socket)
    }
    if !cfg.SPIFFE.MirrorFiles {
        t.Error("MirrorFiles should default true")
    }
    if cfg.SPIFFE.MirrorDir != "/opt" {
        t.Errorf("MirrorDir = %q, want /opt", cfg.SPIFFE.MirrorDir)
    }
}

func TestSPIFFEConfig_Validate_BadSocket(t *testing.T) {
    yaml := `
spiffe:
  socket: "tcp://oops"
  jwt_audience: "x"
`
    if _, err := Load([]byte(yaml)); err == nil {
        t.Fatal("expected validation error")
    }
}
```

- [ ] **Step 2: Run, expect fail**

```
go test ./config/ -run TestSPIFFEConfig -v
```
Expected: FAIL with `cfg.SPIFFE` undefined.

- [ ] **Step 3: Add `SPIFFEConfig` to config.go**

In `authlib/config/config.go` (placement: top-level Config struct):

```go
type SPIFFEConfig struct {
    Socket       string `yaml:"socket" json:"socket"`
    JWTAudience  string `yaml:"jwt_audience" json:"jwt_audience"`
    MirrorFiles  *bool  `yaml:"mirror_files" json:"mirror_files"` // pointer to distinguish unset
    MirrorDir    string `yaml:"mirror_dir" json:"mirror_dir"`
}

// And on the top-level Config struct, add:
//   SPIFFE *SPIFFEConfig `yaml:"spiffe" json:"spiffe"`
```

In `applyDefaults` (or wherever defaults run during Load):

```go
if cfg.SPIFFE != nil {
    if cfg.SPIFFE.Socket == "" {
        cfg.SPIFFE.Socket = "unix:///spiffe-workload-api/spire-agent.sock"
    }
    if cfg.SPIFFE.MirrorFiles == nil {
        t := true
        cfg.SPIFFE.MirrorFiles = &t
    }
    if cfg.SPIFFE.MirrorDir == "" {
        cfg.SPIFFE.MirrorDir = "/opt"
    }
}
```

In `Validate`:

```go
if cfg.SPIFFE != nil {
    if !strings.HasPrefix(cfg.SPIFFE.Socket, "unix://") {
        return fmt.Errorf("spiffe.socket must be a unix:// URL, got %q", cfg.SPIFFE.Socket)
    }
}
```

- [ ] **Step 4: Run tests, expect pass**

```
go test ./config/ -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```
git add authbridge/authlib/config/config.go authbridge/authlib/config/config_test.go
git commit -s -m "feat(config): add top-level spiffe config block

New SPIFFEConfig with Socket, JWTAudience, MirrorFiles, MirrorDir.
Defaults match today's helper.conf-driven setup. Cross-block
validation against tokenexchange identity arrives in the next commit.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 7: Drop old MTLS file fields

**Files:**
- Modify: `authlib/config/config.go`
- Modify: `authlib/config/config_test.go`
- Modify: `authlib/listener/reverseproxy/server.go` (and forwardproxy if it reads cfg.MTLS.CertFile)

The yaml decoder in this codebase ignores unknown keys (verify by inspecting `yaml.Unmarshal` call site — should not have `KnownFields(true)`). Removing these fields silently drops them from chart-rendered configs, per spec decision.

- [ ] **Step 1: Add a pinning test for unknown-field tolerance**

```go
// config_test.go
func TestLoad_UnknownMTLSFields_Ignored(t *testing.T) {
    // Old-style chart config: cert_file/key_file/bundle_file present.
    yaml := `
mtls:
  mode: permissive
  cert_file: /opt/svid.pem
  key_file: /opt/svid_key.pem
  bundle_file: /opt/svid_bundle.pem
spiffe:
  jwt_audience: "x"
`
    cfg, err := Load([]byte(yaml))
    if err != nil {
        t.Fatalf("Load should ignore unknown mtls.cert_file/key_file/bundle_file, got %v", err)
    }
    if cfg.MTLS == nil || cfg.MTLS.Mode != MTLSModePermissive {
        t.Error("mtls.mode missing after load")
    }
}
```

- [ ] **Step 2: Run, expect fail (still has CertFile/KeyFile/BundleFile)**

```
go test ./config/ -run TestLoad_UnknownMTLSFields_Ignored -v
```
This may PASS already (if loader is permissive); if so, skip Step 3 below and proceed to field removal. If it fails, the loader needs `KnownFields(false)` or `DisallowUnknownFields` removed.

- [ ] **Step 3: Drop fields from `MTLSConfig`**

In `authlib/config/config.go`:

```go
// Before
type MTLSConfig struct {
    Mode       MTLSMode `yaml:"mode" json:"mode"`
    CertFile   string   `yaml:"cert_file" json:"cert_file"`
    KeyFile    string   `yaml:"key_file" json:"key_file"`
    BundleFile string   `yaml:"bundle_file" json:"bundle_file"`
}

// After
type MTLSConfig struct {
    Mode MTLSMode `yaml:"mode" json:"mode"`
}
```

Also remove the corresponding lines in `applyDefaults` (the `cert_file` / `key_file` / `bundle_file` defaults around lines 384-390 in current file), and the `CheckPathsReadable` method — it has no callers without those fields.

- [ ] **Step 4: Update existing tests in config_test.go**

Search for `CertFile`, `KeyFile`, `BundleFile` in `config_test.go` and remove or rewrite assertions that referenced those fields. The pinning test from Step 1 covers the back-compat case.

- [ ] **Step 5: Update forwardproxy/reverseproxy listener references**

Grep for `cfg.MTLS.CertFile` (or similar) in the listener packages — if any code references those fields, replace with the X509Source obtained from the Provider (deferred wiring is in Phase 4; for now, make it compile-time clean by removing the references).

- [ ] **Step 6: Run all unit tests**

```
go test ./...
```
Expected: PASS — except possibly the `cmd/*/main.go` tests that haven't been rewired yet. If `go build ./...` fails for cmd/*, that's expected and addressed in Phase 4.

- [ ] **Step 7: Commit**

```
git add authbridge/authlib/config/
git commit -s -m "refactor(config): drop mtls cert/key/bundle file fields

Replaced by the new top-level spiffe.socket + Provider. Old fields
are silently ignored by the yaml loader (pinning test added) so
chart-rendered configs continue to boot during the cross-repo
migration.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 8: Drop tokenexchange JWTSVIDPath; cross-block validation

**Files:**
- Modify: `authlib/plugins/tokenexchange/plugin.go`
- Modify: `authlib/plugins/tokenexchange/plugin_test.go`
- Modify: `authlib/config/config.go` (cross-block validation)

- [ ] **Step 1: Failing tests**

In `authlib/config/config_test.go`:

```go
func TestValidate_SpiffeIdentity_RequiresAudience(t *testing.T) {
    yaml := `
plugins:
  - name: token-exchange
    config:
      identity:
        type: spiffe
spiffe:
  socket: "unix:///spire/socket"
  # jwt_audience missing
`
    if _, err := Load([]byte(yaml)); err == nil {
        t.Fatal("expected validation error: spiffe identity requires spiffe.jwt_audience")
    }
}
```

In `authlib/plugins/tokenexchange/plugin_test.go` — remove the assertion at lines around 56-59 that checks `JWTSVIDPath == "/opt/jwt_svid.token"`.

- [ ] **Step 2: Run, expect fail**

- [ ] **Step 3: Drop `JWTSVIDPath` from `tokenExchangeIdentity`**

In `authlib/plugins/tokenexchange/plugin.go`:

```go
// Remove
//   JWTSVIDPath string `json:"jwt_svid_path"`
// And remove the default-application around line 136:
//   if c.Identity.JWTSVIDPath == "" {
//       c.Identity.JWTSVIDPath = "/opt/jwt_svid.token"
//   }
```

Also at line 343 (the `buildClientAuthFrom` function), the `jwtSVIDPath` parameter is removed; it no longer constructs `spiffe.NewFileJWTSource`. Defer the actual JWTSource wiring to Task 11 (plugin DI) — for now, leave a panic with a clear message:

```go
case "spiffe":
    panic("tokenexchange: spiffe identity requires SPIFFE provider injection (see Task 11)")
```

This panic intentionally fails any test that exercises the spiffe path; Task 11 wires the real source in.

- [ ] **Step 4: Add cross-block validation to config.go**

In `authlib/config/config.go`, add to the top-level Validate (or wherever plugin entries are walked):

```go
// After other validation:
hasSpiffeIdentity := false
for _, e := range cfg.Pipeline.Inbound.Plugins {
    if e.Name == "token-exchange" {
        var id struct {
            Identity struct{ Type string `json:"type"` } `json:"identity"`
        }
        if err := json.Unmarshal(e.Config, &id); err == nil && id.Identity.Type == "spiffe" {
            hasSpiffeIdentity = true
        }
    }
}
// Repeat for cfg.Pipeline.Outbound.Plugins
if hasSpiffeIdentity {
    if cfg.SPIFFE == nil || cfg.SPIFFE.JWTAudience == "" {
        return errors.New("token-exchange identity.type=spiffe requires top-level spiffe.jwt_audience to be set")
    }
}
```

- [ ] **Step 5: Run tests**

```
go test ./...
```
Expected: PASS for config; tokenexchange tests that exercised the spiffe identity path may panic — those are addressed in Task 11.

- [ ] **Step 6: Commit**

```
git add authbridge/authlib/config/ authbridge/authlib/plugins/tokenexchange/
git commit -s -m "refactor(tokenexchange): drop jwt_svid_path; require spiffe.jwt_audience

Token-exchange's spiffe identity path no longer reads a file path from
its own config; the audience comes from the framework spiffe block
and the JWTSource is injected by plugins.BuildWithSPIFFE (next commit).
Cross-block validation rejects configs that set identity.type=spiffe
without spiffe.jwt_audience.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Phase 3 — Plugin dependency injection

### Task 9: Add ProviderConsumer interface

**Files:**
- Create: `authlib/spiffe/consumer.go`

- [ ] **Step 1: Write the file**

```go
// authlib/spiffe/consumer.go
package spiffe

// ProviderConsumer is implemented by plugins that need access to the
// process-wide SPIFFE Provider. plugins.BuildWithSPIFFE invokes
// SetSPIFFEProvider on every plugin that satisfies this interface
// before Configure runs, so plugin configuration code can use the
// Provider's sources directly.
//
// Plugins that don't need SPIFFE simply don't implement this
// interface and are unaffected.
type ProviderConsumer interface {
    SetSPIFFEProvider(p *Provider)
}
```

- [ ] **Step 2: Commit (no test — pure interface declaration tested via consumers)**

```
git add authbridge/authlib/spiffe/consumer.go
git commit -s -m "feat(spiffe): add ProviderConsumer interface for plugin DI

Plugins implement this to receive the framework Provider before
Configure runs. Used by the upcoming plugins.BuildWithSPIFFE.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 10: Add `BuildWithSPIFFE` to plugins package

**Files:**
- Modify: `authlib/plugins/registry.go`
- Modify: `authlib/plugins/registry_test.go`

- [ ] **Step 1: Failing test**

```go
// authlib/plugins/registry_test.go
func TestBuildWithSPIFFE_InjectsProviderIntoConsumer(t *testing.T) {
    // Register a fake plugin that implements ProviderConsumer.
    var got *spiffe.Provider
    type fakePlugin struct{ pipeline.Plugin }
    fake := &consumerPlugin{record: func(p *spiffe.Provider) { got = p }}
    plugins.RegisterPlugin("test-spiffe-consumer", func() pipeline.Plugin { return fake })
    defer plugins.UnregisterPlugin("test-spiffe-consumer")

    p := &spiffe.Provider{} // OK to use zero-value here for identity test
    _, err := plugins.BuildWithSPIFFE([]config.PluginEntry{{Name: "test-spiffe-consumer"}}, p)
    if err != nil { t.Fatalf("BuildWithSPIFFE: %v", err) }
    if got != p { t.Errorf("provider not injected; got %v want %v", got, p) }
}

type consumerPlugin struct{ record func(*spiffe.Provider) }
// implement minimal pipeline.Plugin and spiffe.ProviderConsumer
```

- [ ] **Step 2: Run, expect fail**

```
go test ./plugins/ -run TestBuildWithSPIFFE -v
```

- [ ] **Step 3: Implement `BuildWithSPIFFE`**

Modify `authlib/plugins/registry.go` — add a new function alongside `Build`:

```go
// BuildWithSPIFFE is Build plus dependency injection of the framework
// SPIFFE Provider. Every plugin satisfying spiffe.ProviderConsumer
// receives p via SetSPIFFEProvider before its Configure is invoked.
// Pass nil for p in builds that don't need SPIFFE (equivalent to Build).
func BuildWithSPIFFE(entries []config.PluginEntry, p *spiffe.Provider, opts ...pipeline.Option) (*pipeline.Pipeline, error) {
    ps := make([]pipeline.Plugin, 0, len(entries))
    policies := make([]pipeline.ErrorPolicy, 0, len(entries))
    for _, e := range entries {
        if e.OnError.Resolved() == pipeline.ErrorPolicyOff { continue }
        factory, ok := factoryFor(e.Name)
        if !ok {
            return nil, fmt.Errorf("unknown plugin %q (registered: %v)", e.Name, RegisteredPlugins())
        }
        plugin := factory()
        // Inject framework deps BEFORE Configure so configure logic can
        // use the provider directly.
        if c, ok := plugin.(spiffe.ProviderConsumer); ok && p != nil {
            c.SetSPIFFEProvider(p)
        }
        if c, ok := plugin.(pipeline.Configurable); ok {
            if err := c.Configure(e.Config); err != nil {
                return nil, fmt.Errorf("configure %q: %w", e.Name, err)
            }
        } else if len(e.Config) > 0 {
            return nil, fmt.Errorf("plugin %q does not accept configuration", e.Name)
        }
        ps = append(ps, plugin)
        policies = append(policies, e.OnError.Resolved())
    }
    if err := validateRelationships(ps); err != nil { return nil, err }
    opts = append(opts, pipeline.WithPolicies(policies...))
    return pipeline.New(ps, opts...)
}
```

The existing `Build` function is unchanged; new wiring uses `BuildWithSPIFFE`.

> Import for `spiffe` package: `import "github.com/kagenti/kagenti-extensions/authbridge/authlib/spiffe"`. Verify there's no cycle (spiffe should not import plugins; plugins importing spiffe is fine).

- [ ] **Step 4: Run tests**

```
go test ./plugins/ -v
```

- [ ] **Step 5: Commit**

```
git add authbridge/authlib/plugins/
git commit -s -m "feat(plugins): add BuildWithSPIFFE for Provider DI

New build entrypoint that injects a *spiffe.Provider into every
plugin satisfying spiffe.ProviderConsumer before Configure runs.
Existing Build() is unchanged and still used by call sites that
don't need SPIFFE.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 11: TokenExchange consumes injected Provider

**Files:**
- Modify: `authlib/plugins/tokenexchange/plugin.go`
- Modify: `authlib/plugins/tokenexchange/plugin_test.go`

- [ ] **Step 1: Failing test**

```go
// plugin_test.go
func TestTokenExchange_SpiffeIdentity_UsesInjectedProvider(t *testing.T) {
    fake := fakeworkloadapi.New(t)
    defer fake.Stop()
    /* serve JWT-SVID for audience X */

    p, _ := spiffe.NewProvider(context.Background(), spiffe.ProviderConfig{
        SocketPath: fake.Addr(), JWTAudience: "X", MirrorFiles: false,
    })
    defer p.Close()

    plugin := NewTokenExchange()
    plugin.SetSPIFFEProvider(p)
    err := plugin.Configure(json.RawMessage(`{
        "token_url": "http://example/token",
        "identity": {"type": "spiffe", "client_id": "agent-1"}
    }`))
    if err != nil { t.Fatalf("Configure: %v", err) }

    if !plugin.Ready() { t.Error("expected Ready=true after spiffe configure with provider") }
}
```

- [ ] **Step 2: Run, expect fail**

- [ ] **Step 3: Implement**

In `authlib/plugins/tokenexchange/plugin.go`:

```go
import (
    fwspiffe "github.com/kagenti/kagenti-extensions/authbridge/authlib/spiffe"
)

// Add field to TokenExchange struct:
type TokenExchange struct {
    // ... existing fields ...
    provider *fwspiffe.Provider
}

// Add setter:
func (p *TokenExchange) SetSPIFFEProvider(prov *fwspiffe.Provider) {
    p.provider = prov
}

// Replace buildClientAuthFrom signature to accept a provider:
func buildClientAuthFrom(identityType, clientID, clientSecret string, prov *fwspiffe.Provider) (exchange.ClientAuth, error) {
    switch identityType {
    case "spiffe":
        if prov == nil {
            return nil, errors.New("tokenexchange: spiffe identity requires SPIFFE provider (none injected)")
        }
        src := prov.JWTSource()
        if src == nil {
            return nil, errors.New("tokenexchange: spiffe identity requires spiffe.jwt_audience to be set")
        }
        return &exchange.JWTAssertionAuth{
            ClientID:      clientID,
            AssertionType: "urn:ietf:params:oauth:client-assertion-type:jwt-spiffe",
            TokenSource:   src.FetchToken,
        }, nil
    case "client-secret":
        return &exchange.ClientSecretAuth{ClientID: clientID, ClientSecret: clientSecret}, nil
    default:
        return nil, fmt.Errorf("unknown identity.type %q", identityType)
    }
}
```

Update all `buildClientAuthFrom` callers (Configure, pollCredentials) to pass `p.provider`.

Also update `credentialsAreReady`:

```go
func credentialsAreReady(id tokenExchangeIdentity, prov *fwspiffe.Provider) bool {
    if id.ClientID == "" { return false }
    switch id.Type {
    case "client-secret":
        return id.ClientSecret != ""
    case "spiffe":
        return prov != nil && prov.JWTSource() != nil
    }
    return false
}
```

- [ ] **Step 4: Update plugin_test.go assertions**

Remove the panic from Task 8. Run the full plugin test suite:

```
go test ./plugins/tokenexchange/ -v
```

- [ ] **Step 5: Commit**

```
git add authbridge/authlib/plugins/tokenexchange/
git commit -s -m "feat(tokenexchange): consume injected SPIFFE Provider

TokenExchange now implements spiffe.ProviderConsumer. The spiffe
identity path uses provider.JWTSource() (a workload-api-backed JWT
source) instead of constructing a FileJWTSource from a config field.
The framework's plugins.BuildWithSPIFFE injects the Provider before
Configure runs.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Phase 4 — Wire main.go for each binary

### Task 12: Update authbridge-proxy main.go

**Files:**
- Modify: `cmd/authbridge-proxy/main.go`

The wiring change has four parts:
1. Drop the file-existence wait loop around line 209 (waiting for `/opt/svid.pem` etc.).
2. Build `*spiffe.Provider` from `cfg.SPIFFE`.
3. Pass `provider.X509Source()` to `MTLSOptions` instead of `spiffe.NewFileX509Source(...)`.
4. Switch `plugins.Build` to `plugins.BuildWithSPIFFE(..., provider, opts...)` for both inbound and outbound chains.

- [ ] **Step 1: Read main.go to understand current flow**

```
sed -n '170,230p' authbridge/cmd/authbridge-proxy/main.go
```

- [ ] **Step 2: Replace the mTLS construction block**

Find the section that today reads:

```go
if cfg.MTLS != nil {
    strict := cfg.MTLS.ResolvedMode() == config.MTLSModeStrict
    src := spiffe.NewFileX509Source(cfg.MTLS.CertFile, cfg.MTLS.KeyFile, cfg.MTLS.BundleFile)
    // ... wait loop for files ...
}
```

Replace with:

```go
var provider *spiffe.Provider
if cfg.SPIFFE != nil {
    var err error
    provider, err = spiffe.NewProvider(context.Background(), spiffe.ProviderConfig{
        SocketPath:  cfg.SPIFFE.Socket,
        JWTAudience: cfg.SPIFFE.JWTAudience,
        MirrorFiles: cfg.SPIFFE.MirrorFiles == nil || *cfg.SPIFFE.MirrorFiles,
        MirrorDir:   cfg.SPIFFE.MirrorDir,
    })
    if err != nil { log.Fatalf("spiffe provider: %v", err) }
    defer provider.Close()
}

if cfg.MTLS != nil {
    if provider == nil {
        log.Fatal("mtls requires spiffe block to be configured")
    }
    strict := cfg.MTLS.ResolvedMode() == config.MTLSModeStrict
    src := provider.X509Source()
    mtlsMetrics = authtls.NewMetrics()
    rpMTLS = &reverseproxy.MTLSOptions{Source: src, Strict: strict, Metrics: mtlsMetrics}
    fpMTLS = &forwardproxy.MTLSOptions{Source: src, Strict: strict, Metrics: mtlsMetrics}
    slog.Info("mTLS enabled", "mode", cfg.MTLS.ResolvedMode())
}
```

The file-existence wait loop is gone — `NewProvider` already blocks until the first SVID arrives.

- [ ] **Step 3: Switch plugin Build calls**

```go
// Before:
in, err := plugins.Build(c.Pipeline.Inbound.Plugins)
out, err := plugins.Build(c.Pipeline.Outbound.Plugins)

// After:
in, err := plugins.BuildWithSPIFFE(c.Pipeline.Inbound.Plugins, provider)
out, err := plugins.BuildWithSPIFFE(c.Pipeline.Outbound.Plugins, provider)
```

- [ ] **Step 4: Build**

```
cd authbridge && go build ./cmd/authbridge-proxy/
```
Expected: success.

- [ ] **Step 5: Run unit tests**

```
go test ./...
```
Expected: PASS.

- [ ] **Step 6: Commit**

```
git add authbridge/cmd/authbridge-proxy/main.go
git commit -s -m "refactor(authbridge-proxy): wire SPIFFE Provider into main

Replaces the FileX509Source + helper.conf-driven file-wait pattern
with spiffe.NewProvider, which blocks until the first X.509-SVID
arrives in memory. mTLS listeners get provider.X509Source(); the
plugin pipeline is built via BuildWithSPIFFE so token-exchange's
spiffe identity path receives an injected JWTSource.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 13: Update authbridge-envoy main.go

Same pattern as Task 12. Note: envoy-sidecar mode has no mTLS today, so the X.509 source path may not be exercised; but the JWT path matters for token-exchange. Build the Provider unconditionally when `cfg.SPIFFE` is set, switch to `BuildWithSPIFFE`.

- [ ] **Step 1: Apply the same pattern**
- [ ] **Step 2: `go build ./cmd/authbridge-envoy/`**
- [ ] **Step 3: `go test ./...`**
- [ ] **Step 4: Commit (use same message body, replace binary name)**

---

### Task 14: Update authbridge-lite main.go

Same pattern as Task 12. authbridge-lite is the proxy-sidecar lite variant — wire identically.

- [ ] **Step 1: Apply the same pattern**
- [ ] **Step 2: `go build ./cmd/authbridge-lite/`**
- [ ] **Step 3: `go test ./...`**
- [ ] **Step 4: Commit**

---

## Phase 5 — Drop spiffe-helper from images

### Task 15: Remove spiffe-helper from entrypoint scripts

**Files:**
- Modify: `cmd/authbridge-proxy/entrypoint.sh`
- Modify: `cmd/authbridge-envoy/entrypoint.sh`
- Modify: `cmd/authbridge-lite/entrypoint.sh`

For each entrypoint.sh, delete the Phase 1 block (lines 27-32 in proxy and lite, lines 28-33 in envoy):

```bash
# Delete:
# --- Phase 1: spiffe-helper (conditional) ---
if [ "${SPIRE_ENABLED:-}" = "true" ]; then
  echo "[entrypoint] Starting spiffe-helper..."
  /usr/local/bin/spiffe-helper -config=/etc/spiffe-helper/helper.conf run &
  CRITICAL_PIDS="$CRITICAL_PIDS $!"
fi
```

Renumber subsequent phase comments (Phase 2 → Phase 1 in proxy/lite; Phase 2/3 → Phase 1/2 in envoy).

- [ ] **Step 1: Edit all three files**
- [ ] **Step 2: Verify shellcheck passes (if used in CI)**: `shellcheck cmd/authbridge-*/entrypoint.sh`
- [ ] **Step 3: Commit**

```
git add authbridge/cmd/authbridge-{proxy,envoy,lite}/entrypoint.sh
git commit -s -m "refactor(images): remove spiffe-helper launch from entrypoints

Authbridge processes now talk to the SPIRE agent socket directly via
the in-process Provider; the bundled spiffe-helper binary is no
longer needed. Phase 1 of each entrypoint is gone.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 16: Drop spiffe-helper from Dockerfiles

**Files:**
- Modify: `cmd/authbridge-proxy/Dockerfile`
- Modify: `cmd/authbridge-envoy/Dockerfile`
- Modify: `cmd/authbridge-lite/Dockerfile`

Read each Dockerfile to identify:
- The `COPY --from=spiffe-helper-stage /opt/...` (or similar) layer that pulls the binary.
- The `COPY .../helper.conf /etc/spiffe-helper/helper.conf` layer (if any).
- Any apt/apk install of `spiffe-helper` package.

Delete those layers.

- [ ] **Step 1: Read each Dockerfile**
- [ ] **Step 2: Remove spiffe-helper-related layers**
- [ ] **Step 3: Build all three images locally**

```
cd authbridge
docker build -f cmd/authbridge-proxy/Dockerfile -t authbridge-proxy:test .
docker build -f cmd/authbridge-envoy/Dockerfile -t authbridge-envoy:test .
docker build -f cmd/authbridge-lite/Dockerfile -t authbridge-lite:test .
```

- [ ] **Step 4: Verify spiffe-helper binary is gone**

```
docker run --rm authbridge-proxy:test ls /usr/local/bin/ | grep -i spiffe-helper && echo "STILL PRESENT" || echo "OK: removed"
```
Repeat for envoy and lite. Expected: `OK: removed` for each.

- [ ] **Step 5: Commit**

```
git add authbridge/cmd/authbridge-{proxy,envoy,lite}/Dockerfile
git commit -s -m "refactor(images): drop spiffe-helper binary from authbridge images

Each combined image shrinks by ~15-20MB compressed. The agent socket
mount stays in place — it's now consumed by authbridge's in-process
Provider instead of spiffe-helper.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Phase 6 — Cleanup

### Task 17: Delete `FileX509Source` and tests

**Files:**
- Delete: `authlib/spiffe/x509source.go`
- Delete: `authlib/spiffe/x509source_test.go`

- [ ] **Step 1: `git rm` both files**

```
git rm authbridge/authlib/spiffe/x509source.go authbridge/authlib/spiffe/x509source_test.go
```

- [ ] **Step 2: Run all tests**

```
cd authbridge && go test ./...
```
Expected: PASS. If any package still imports `FileX509Source` / `NewFileX509Source` / `DefaultCertPath` / `DefaultKeyPath` / `DefaultBundlePath`, fix the imports — those should already be removed by Phase 4 wiring changes.

- [ ] **Step 3: Commit**

```
git commit -s -m "refactor(spiffe): delete FileX509Source

No runtime caller after the Provider/workloadX509 migration. Deletion
follows the kagenti CLAUDE.md guidance to remove unused code rather
than leave it behind.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 18: Delete `FileJWTSource` and tests

**Files:**
- Delete: `authlib/plugins/tokenexchange/spiffe/file.go`
- Delete: `authlib/plugins/tokenexchange/spiffe/file_test.go`

The interface declaration in `authlib/plugins/tokenexchange/spiffe/source.go` stays — it's the contract the tokenexchange plugin consumes.

- [ ] **Step 1: `git rm` both files**

```
git rm authbridge/authlib/plugins/tokenexchange/spiffe/file.go authbridge/authlib/plugins/tokenexchange/spiffe/file_test.go
```

- [ ] **Step 2: Run all tests**

```
go test ./...
```
Expected: PASS.

- [ ] **Step 3: Commit**

```
git commit -s -m "refactor(tokenexchange): delete FileJWTSource

No runtime caller after the Provider/workloadJWT migration. Plugin
now consumes a JWTSource injected via plugins.BuildWithSPIFFE.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 19: Update CLAUDE.md and demos/mtls/README.md

**Files:**
- Modify: `authbridge/CLAUDE.md`
- Modify: `authbridge/demos/mtls/README.md`

- [ ] **Step 1: Edit `authbridge/CLAUDE.md` lines 247-250**

Today (lines 247-250 from earlier exploration):

```
| `/opt/jwt_svid.token` | spiffe-helper | authbridge (token-exchange) | JWT SVID from SPIRE |
| `/opt/svid.pem` | spiffe-helper | authbridge (mTLS) | X.509 SVID leaf cert (PEM) |
| `/opt/svid_key.pem` | spiffe-helper | authbridge (mTLS) | X.509 SVID private key (PEM) |
| `/opt/svid_bundle.pem` | spiffe-helper | authbridge (mTLS) | SPIRE trust bundle (PEM, may concatenate multiple CAs) |
```

After:

```
| `/opt/jwt_svid.token` | spiffe.Provider mirror | authbridge (token-exchange) + external readers | JWT SVID, audience from spiffe.jwt_audience |
| `/opt/svid.pem` | spiffe.Provider mirror | external readers (debugging, future Envoy SDS) | X.509 SVID leaf cert (PEM) |
| `/opt/svid_key.pem` | spiffe.Provider mirror | external readers | X.509 SVID private key (PEM) |
| `/opt/svid_bundle.pem` | spiffe.Provider mirror | external readers | SPIRE trust bundle (PEM) |
```

Also update the surrounding paragraph that mentions spiffe-helper.

- [ ] **Step 2: Edit `authbridge/demos/mtls/README.md` line 6**

Today: `- spiffe-helper writes /opt/svid.pem / _key.pem / _bundle.pem on ...`

After: `- authbridge's spiffe.Provider mirror writes /opt/svid.pem / _key.pem / _bundle.pem on every rotation; the in-process X.509 source is what the listener actually uses.`

- [ ] **Step 3: Commit**

```
git add authbridge/CLAUDE.md authbridge/demos/mtls/README.md
git commit -s -m "docs: update spiffe-helper references to spiffe.Provider mirror

The credential files are now written by an in-process goroutine
instead of the spiffe-helper binary. Authbridge's hot path reads the
SVIDs from memory; the files exist for external readers (e2e tests,
debugging shells, future Envoy filesystem SDS).

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Self-Review Checklist (run before opening PR)

- [ ] All unit tests pass: `cd authbridge && go test ./...`
- [ ] All three binaries build: `go build ./cmd/...`
- [ ] All three Docker images build and contain no spiffe-helper binary
- [ ] `kind-full-test.sh` deploy succeeds and:
  - `/opt/svid.pem`, `/opt/svid_key.pem`, `/opt/svid_bundle.pem`, `/opt/jwt_svid.token` exist after pod ready
  - mTLS handshake between two authbridge-proxy pods succeeds
  - Token exchange against Keycloak returns a valid token
  - After ~3min, `/opt/svid.pem` mtime updates
- [ ] `verify-spire-keycloak.sh` passes
- [ ] `make lint` clean
- [ ] PR description includes:
  - Spec link: `authbridge/docs/superpowers/specs/2026-05-22-authbridge-go-spiffe-sdk-design.md`
  - Manual-verification checklist results
  - Note on chart sequencing: "kagenti chart PR adding `spiffe.jwt_audience` rendering must merge first"
  - Image-size delta (~15-20MB compressed reduction per image)
  - `Assisted-By: Claude Code` footer

## Spec Coverage Check

Cross-reference every spec section against this plan:

| Spec section | Tasks |
|---|---|
| Architecture (one Provider per container) | Task 5 |
| Components: provider.go, workload_x509.go, workload_jwt.go, mirror.go | Tasks 1-5 |
| Public surface: NewProvider/X509Source/JWTSource/Close | Task 5 |
| File deletions: FileX509Source, FileJWTSource | Tasks 17, 18 |
| Config: drop MTLS file paths, add SPIFFEConfig, drop JWTSVIDPath | Tasks 6-8 |
| Cross-block validation | Task 8 |
| Old field tolerance pinning test | Task 7 |
| Plugin DI mechanism | Tasks 9-11 |
| cmd/*/main.go wiring (3 binaries) | Tasks 12-14 |
| entrypoint.sh changes (3 binaries) | Task 15 |
| Dockerfile changes (3 binaries) | Task 16 |
| Documentation updates | Task 19 |
| Hot path: in-memory X509Source/JWTSource | Tasks 1, 2, 5, 12-14 |
| File mirror with atomic writes | Tasks 3, 4 |
| Cold-start gate (NewProvider blocks on first SVID) | Task 5 |
| Mirror failures non-fatal | Task 4 |
| 90% lifetime JWT refresh | Task 4 |

All spec items have a task. No coverage gaps.
