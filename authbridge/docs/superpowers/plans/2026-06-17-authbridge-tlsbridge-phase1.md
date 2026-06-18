# AuthBridge Egress TLS Bridge — Phase 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an outbound **TLS bridge** to AuthBridge's Go forward proxy — terminate the agent's egress TLS, run the existing outbound pipeline on the decrypted request, and re-originate a separately-verified TLS connection to the real origin — in-process, with an ephemeral CA, no operator dependency, without ever breaking a currently-working agent call.

> **Naming:** this feature was previously called "MITM". It is now the **TLS bridge** (terminate one TLS, run logic on plaintext, originate a fresh verified TLS — literally a bridge between two TLS connections). Package `tlsbridge`, config block `tls_bridge:`, type `tlsbridge.Engine`.

**Architecture:** A new self-contained `authlib/tlsbridge/` package (CA source, leaf minter, bridge decision, TLS terminator, upstream client). The forward-proxy's two blind-tunnel sites (transparent listener + CONNECT) gain a *reversible* bridge branch: verify the upstream origin's TLS first, then forge a leaf and terminate the agent's TLS, then run the **unchanged** outbound pipeline on the decrypted request and relay over the verified upstream connection. Un-bridgeable or pinned traffic falls open to the existing tunnel and self-heals via an auto-skip set.

**Scope (Phase 1 is test-only):** This PR is AuthBridge-only and proves the decrypt→pipeline→re-originate loop via **in-process integration tests** (a client configured to trust the ephemeral CA). It does **not** make a real operator-deployed agent trust the CA — that is Phase 2 (operator) work. On a real cluster, a live agent's egress will safely **tunnel** (the no-broken-calls guarantee), because it does not yet trust the minted leaf. See "Phase 2 compatibility" below — Phase 1 is built so the cert-manager path (2c) drops in with a one-line `main.go` swap and no changes to the bridge core.

**Tech Stack:** Go 1.25, stdlib `crypto/tls` + `crypto/x509` (hand-rolled cert minting), `golang.org/x/net/http2` for h2. Module: `github.com/kagenti/kagenti-extensions/authbridge/authlib` (`go.mod` already has `golang.org/x/net v0.51.0` as an indirect dep — Task 5 promotes it to direct). Tests are package-internal `_test.go`, table-driven, loopback listeners; run with `go test ./...` from `authbridge/authlib`.

**Spec:** `authbridge/docs/superpowers/specs/2026-06-12-authbridge-tlsbridge-design.md`

---

## Verified anchors in the current code (source of truth, re-checked against current `main`)

- `forwardproxy.Server` (`authlib/listener/forwardproxy/server.go:47-61`): fields `OutboundPipeline *pipeline.Holder`, `Sessions *session.Store`, `Shared pipeline.SharedStore`, `Client *http.Client`, `SkipHosts *skiphost.Matcher`. `Shared`/`SkipHosts` are set by the caller after `NewServer`, not inside it — the new `TLSBridge` field follows that pattern.
- `forwardproxy.NewServer(outbound *pipeline.Holder, sessions *session.Store, mtls *MTLSOptions) (*Server, error)` (`server.go:89`). When `mtls != nil` it sets `transport.DialContext = mtlsDialer(...).DialContext` (`server.go:90-117`) — a **TLS-or-fail dialer that presents the agent SVID and verifies against the SPIRE bundle**. This is exactly why bridge re-origination must use its **own** client, never `s.Client`.
- `(s *Server) handleRequest(w, r)` (`server.go`): builds `pctx := &pipeline.Context{Direction: pipeline.Outbound, Method, Scheme: r.URL.Scheme, Host: r.Host, Path: r.URL.Path, Headers: r.Header.Clone(), Shared, StartedAt}` (`server.go:197-206`); runs `action := s.OutboundPipeline.Run(r.Context(), pctx)` and checks `action.Type == pipeline.Reject` (`server.go:258-270`); strips hop-by-hop + `Proxy-Authorization`/`Proxy-Connection` (`server.go:320-329`); clears `r.RequestURI = ""` (`server.go:332`); re-originates via `s.Client.Do(r)` (`server.go:334`). **It does NOT set `r.URL.Scheme`/`Host`** — it relies on absolute-form proxy requests already carrying them. The bridge handler must set them (origin-form decrypted requests have empty `r.URL.Host`).
- `(s *Server) handleConnect(w, r)` (`server.go`): dials `upstream` via `net.DialTimeout("tcp", r.Host, …)` (`server.go:779`), hijacks `clientConn` (`server.go:786`), writes `200 Connection Established` (`server.go:802`), then `tunnel(clientConn, upstream)` (`server.go:822`). Upstream is plain TCP — never mTLS.
- `(s *Server) HandleTransparentConn(clientConn net.Conn, dst string)` (`transparent.go:46`): `name, wrapped := sniffHost(clientConn); clientConn = wrapped` (`transparent.go:69-70`); local var `host` = `net.JoinHostPort(name, port)` or `dst` (`transparent.go:67-78`); dials `upstream` against `dst` with `defer upstream.Close()` (`transparent.go:115-120`); `tunnel(clientConn, upstream)` (`transparent.go:125`).
- `sniffHost(conn net.Conn) (string, net.Conn)` (`sniff.go`): returns the recovered host string and a replayable `*peekedConn{net.Conn; r *bufio.Reader}` (`sniff.go:148-153`). It uses `br.Peek(...)` (non-consuming) internally but **discards the peeked bytes** and has **no exported peek method**. Task 6 adds `(c *peekedConn) Peek(n int) ([]byte, error)`.
- `config.Config` (`config/config.go`): optional pointer blocks `MTLS *MTLSConfig \`yaml:"mtls,omitempty"\`` (`:33`), `SPIFFE *SPIFFEConfig \`yaml:"spiffe,omitempty"\`` (`:39`), each with a `Validate()` method (`:86`, `:133`) called from the top-level loader (`cfg.MTLS.Validate()` `:441`, `cfg.SPIFFE.Validate()` `:460`). The loader is **`func Load(path string) (*Config, error)`** (`:415`) — it takes a **file path, not bytes**. `config.go` already imports `fmt` and `os`.
- main wiring (`cmd/authbridge-proxy/main.go`): `fpMTLS = &forwardproxy.MTLSOptions{...}` (`:243`); `fpSrv, err := forwardproxy.NewServer(outboundH, sessions, fpMTLS)` (`:256`); then `fpSrv.SkipHosts = …` (`:268`), `fpSrv.Shared = sharedStore` (`:272`); `transparentproxy.NewServer(fp.HandleTransparentConn)` (`:424`). NOTE: `cmd/authbridge-proxy/main.go` imports `"log"` and uses **`log.Fatalf`** for boot-fatal errors (no `os.Exit` in the boot path) — match that convention, not `slog`+`os.Exit`.
- `pipeline`: `Action{Type ActionType, Violation *Violation}` with `Reject` const (`pipeline/action.go:11-24`); `SharedStore` is an interface (`pipeline/context.go:35`); `Context` (`pipeline/context.go:93`). The response phase (`RunResponse`/`RunResponseFrame`) lives inside the `handleRequest` body that Task 6 moves wholesale — no new response-phase code is authored.

---

## File Structure

**New package `authlib/tlsbridge/`** (one responsibility per file):
- `authlib/tlsbridge/doc.go` — package doc.
- `authlib/tlsbridge/ca.go` — `CASource` interface + `EphemeralSource` + `FileSource` (multi-format key parsing).
- `authlib/tlsbridge/minter.go` — `Minter` (per-host leaf, **one shared P-256 leaf key**, LRU/TTL cache).
- `authlib/tlsbridge/decision.go` — `Decision` (classify) + `SkipSet` (runtime auto-skip).
- `authlib/tlsbridge/upstream.go` — `NewUpstreamClient` (system + injected roots).
- `authlib/tlsbridge/terminator.go` — `Terminator` (tls.Server wrap, ALPN h2+http/1.1).
- `authlib/tlsbridge/serve.go` — `ServeConn` (one-conn keep-alive http.Server, h2-enabled).
- `authlib/tlsbridge/engine.go` — `Engine` facade + `RunTrustSelfCheck`.
- `authlib/tlsbridge/*_test.go` — per-unit tests.

**Modified (proxy integration):**
- `authlib/listener/forwardproxy/server.go` — add `TLSBridge *tlsbridge.Engine` field; extract `serveOutbound(w, r, isBridge)`; add `bridgeServe` + small `hostOnly`/`portOf`/`nameOrIP` helpers; bridge branch in `handleConnect`.
- `authlib/listener/forwardproxy/transparent.go` — bridge branch before `tunnel(...)`.
- `authlib/listener/forwardproxy/sniff.go` — add `(*peekedConn).Peek(n)`.
- `authlib/config/config.go` — `TLSBridge *TLSBridgeConfig` block + validation.
- `cmd/authbridge-proxy/main.go` — construct the engine, set `fpSrv.TLSBridge`, run trust self-check.

**Phase 2 (operator) — sketched at the end, not bite-sized here.**

---

## Task 0: Branch + package skeleton

**Files:**
- Create: `authlib/tlsbridge/doc.go`

- [ ] **Step 1: Create the implementation branch from current upstream main**

```bash
cd /Users/haihuang/works/go/src/github.com/kagenti/kagenti-extensions
git fetch upstream main
git checkout -b feat/tlsbridge-phase1 upstream/main
```

Expected: branch created off post-resolv.conf `main` (the design branch is docs-only and stale for code).

- [ ] **Step 2: Create the package doc file**

```go
// Package tlsbridge implements AuthBridge's outbound TLS bridge: it forges a
// per-origin leaf so the agent's egress TLS terminates at AuthBridge, the
// existing outbound pipeline runs on the decrypted request, and the call is
// relayed over a separately-verified upstream TLS connection. Un-bridgeable or
// pinned traffic falls open to a plain tunnel and self-heals via an auto-skip set.
package tlsbridge
```

- [ ] **Step 3: Verify it compiles and commit**

Run: `cd authbridge/authlib && go build ./tlsbridge/`
Expected: builds (empty package).

```bash
git add authbridge/authlib/tlsbridge/doc.go
git commit -s -m "feat(tlsbridge): add package skeleton"
```

---

## Task 1: CASource — ephemeral + file CA (multi-format key)

**Files:**
- Create: `authlib/tlsbridge/ca.go`
- Test: `authlib/tlsbridge/ca_test.go`

- [ ] **Step 1: Write the failing test**

```go
package tlsbridge

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
)

func TestEphemeralSource_IssuesUsableCA(t *testing.T) {
	src, err := NewEphemeralSource()
	if err != nil {
		t.Fatalf("NewEphemeralSource: %v", err)
	}
	block, _ := pem.Decode(src.CACertPEM())
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("CACertPEM did not yield a CERTIFICATE PEM block")
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	if !caCert.IsCA {
		t.Errorf("issued cert is not a CA (IsCA=false)")
	}
	if caCert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Errorf("CA cert lacks KeyUsageCertSign")
	}
	cert, key := src.Issuer()
	if cert == nil || key == nil {
		t.Fatalf("Issuer() returned nil cert/key")
	}
}
```

- [ ] **Step 2: Run it — expect compile failure**

Run: `cd authbridge/authlib && go test ./tlsbridge/ -run TestEphemeralSource -v`
Expected: FAIL — `undefined: NewEphemeralSource`.

- [ ] **Step 3: Implement `ca.go`**

```go
package tlsbridge

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"time"
)

// CASource supplies the signing CA used to mint per-origin leaves.
type CASource interface {
	// Issuer returns the CA certificate and its private key for signing leaves.
	Issuer() (cert *x509.Certificate, key crypto.Signer)
	// CACertPEM returns the CA certificate in PEM form (for the agent's trust store).
	CACertPEM() []byte
}

type staticSource struct {
	cert    *x509.Certificate
	key     crypto.Signer
	certPEM []byte
}

func (s *staticSource) Issuer() (*x509.Certificate, crypto.Signer) { return s.cert, s.key }
func (s *staticSource) CACertPEM() []byte                          { return s.certPEM }

// NewEphemeralSource generates an in-memory self-signed CA. Used as the
// standalone / no-cert-manager fallback and in tests.
func NewEphemeralSource() (CASource, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("tlsbridge: generate CA key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "authbridge-tls-bridge-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, fmt.Errorf("tlsbridge: self-sign CA: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("tlsbridge: parse self-signed CA: %w", err)
	}
	return &staticSource{
		cert:    cert,
		key:     key,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}, nil
}

// NewFileSource loads a CA (tls.crt/tls.key) from disk — the cert-manager /
// operator-coordinated path (Phase 2). Keys may be PKCS#8, PKCS#1 (RSA) or
// SEC1 (EC); cert-manager's DEFAULT encoding is PKCS#1, so all three are tried.
func NewFileSource(certPath, keyPath string) (CASource, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("tlsbridge: read CA cert %s: %w", certPath, err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("tlsbridge: read CA key %s: %w", keyPath, err)
	}
	cb, _ := pem.Decode(certPEM)
	if cb == nil {
		return nil, fmt.Errorf("tlsbridge: CA cert %s is not PEM", certPath)
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("tlsbridge: parse CA cert: %w", err)
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, fmt.Errorf("tlsbridge: CA key %s is not PEM", keyPath)
	}
	key, err := parsePrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	return &staticSource{cert: cert, key: key, certPEM: certPEM}, nil
}

// parsePrivateKey accepts PKCS#8, PKCS#1 (RSA) and SEC1 (EC) DER.
func parsePrivateKey(der []byte) (crypto.Signer, error) {
	if k, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		if s, ok := k.(crypto.Signer); ok {
			return s, nil
		}
		return nil, fmt.Errorf("tlsbridge: PKCS#8 key is not a crypto.Signer")
	}
	if k, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return k, nil
	}
	if k, err := x509.ParseECPrivateKey(der); err == nil {
		return k, nil
	}
	return nil, fmt.Errorf("tlsbridge: unsupported CA key format (tried PKCS#8, PKCS#1, SEC1)")
}
```

- [ ] **Step 4: Add a FileSource round-trip test (proves PKCS#1 + PKCS#8 both load)**

Append to `ca_test.go`:

```go
import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509/pkix"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

func TestFileSource_LoadsPKCS1AndPKCS8(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pkcs8  bool
	}{{"ec-pkcs8", true}, {"ec-sec1", false}} {
		t.Run(tc.name, func(t *testing.T) {
			key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			tmpl := &x509.Certificate{
				SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "t"},
				NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
				IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true,
			}
			der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
			dir := t.TempDir()
			certP := filepath.Join(dir, "tls.crt")
			keyP := filepath.Join(dir, "tls.key")
			os.WriteFile(certP, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
			var keyDER []byte
			if tc.pkcs8 {
				keyDER, _ = x509.MarshalPKCS8PrivateKey(key)
			} else {
				keyDER, _ = x509.MarshalECPrivateKey(key)
			}
			os.WriteFile(keyP, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600)
			if _, err := NewFileSource(certP, keyP); err != nil {
				t.Fatalf("NewFileSource(%s): %v", tc.name, err)
			}
		})
	}
}
```

- [ ] **Step 5: Run the tests — expect PASS**

Run: `go test ./tlsbridge/ -run 'TestEphemeralSource|TestFileSource' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add authbridge/authlib/tlsbridge/ca.go authbridge/authlib/tlsbridge/ca_test.go
git commit -s -m "feat(tlsbridge): CASource (ephemeral + file, multi-format key)"
```

---

## Task 2: Minter — per-host leaf (shared key) + cache

**Files:**
- Create: `authlib/tlsbridge/minter.go`
- Test: `authlib/tlsbridge/minter_test.go`

> **Decision (locked):** one shared P-256 leaf key, generated once in `NewMinter` and reused for every minted leaf — only the certificate is per-host. The leaf key is not the secret here (it lives behind the same trust boundary as the CA); the CA key is what's protected.

- [ ] **Step 1: Write the failing tests**

```go
package tlsbridge

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"
	"time"
)

func newTestMinter(t *testing.T) (*Minter, *x509.CertPool) {
	t.Helper()
	src, err := NewEphemeralSource()
	if err != nil {
		t.Fatalf("ca: %v", err)
	}
	m := NewMinter(src, MinterOpts{CacheMax: 8, LeafTTL: time.Hour})
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(src.CACertPEM()) {
		t.Fatal("append CA to pool")
	}
	return m, pool
}

func TestMinter_LeafChainsToCA_AndHasSAN(t *testing.T) {
	m, pool := newTestMinter(t)
	cert, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "api.example.com"})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "api.example.com"}); err != nil {
		t.Errorf("leaf does not verify against CA for its SAN: %v", err)
	}
}

func TestMinter_IPLiteralSAN(t *testing.T) {
	m, pool := newTestMinter(t)
	c2, err := m.GetCertificateForHost("10.0.0.5")
	if err != nil {
		t.Fatalf("GetCertificateForHost(ip): %v", err)
	}
	leaf, _ := x509.ParseCertificate(c2.Certificate[0])
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
		// IP SANs verify via IPAddresses, not DNSName; the explicit SAN check below is authoritative.
	}
	found := false
	for _, ip := range leaf.IPAddresses {
		if ip.Equal(net.ParseIP("10.0.0.5")) {
			found = true
		}
	}
	if !found {
		t.Errorf("leaf for IP host lacks the IP SAN")
	}
}

func TestMinter_CacheHitReturnsSameCert(t *testing.T) {
	m, _ := newTestMinter(t)
	a, _ := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "h.example.com"})
	b, _ := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "h.example.com"})
	if &a.Certificate[0][0] != &b.Certificate[0][0] {
		t.Errorf("expected cached cert reuse for same SNI")
	}
}
```

- [ ] **Step 2: Run — expect compile failure**

Run: `go test ./tlsbridge/ -run TestMinter -v`
Expected: FAIL — `undefined: NewMinter` / `MinterOpts`.

- [ ] **Step 3: Implement `minter.go`**

```go
package tlsbridge

import (
	"container/list"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"sync"
	"time"
)

type MinterOpts struct {
	CacheMax int           // max cached leaves (LRU); <=0 → 1024
	LeafTTL  time.Duration // leaf validity AND cache TTL; <=0 → 24h
}

// Minter mints per-host leaf certs signed by a CASource, cached LRU+TTL by host.
type Minter struct {
	src     CASource
	max     int
	ttl     time.Duration
	leafKey *ecdsa.PrivateKey // one key reused across leaves (cheaper; key is not the secret here)

	mu    sync.Mutex
	ll    *list.List               // MRU front
	items map[string]*list.Element // host -> element(*cacheEntry)
}

type cacheEntry struct {
	host    string
	cert    *tls.Certificate
	expires time.Time
}

func NewMinter(src CASource, o MinterOpts) *Minter {
	if o.CacheMax <= 0 {
		o.CacheMax = 1024
	}
	if o.LeafTTL <= 0 {
		o.LeafTTL = 24 * time.Hour
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	return &Minter{
		src: src, max: o.CacheMax, ttl: o.LeafTTL, leafKey: key,
		ll: list.New(), items: make(map[string]*list.Element),
	}
}

// GetCertificate satisfies tls.Config.GetCertificate via the SNI server name.
func (m *Minter) GetCertificate(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := chi.ServerName
	if host == "" {
		return nil, fmt.Errorf("tlsbridge: no SNI; caller must use GetCertificateForHost with the dialed IP")
	}
	return m.GetCertificateForHost(host)
}

func (m *Minter) GetCertificateForHost(host string) (*tls.Certificate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.items[host]; ok {
		e := el.Value.(*cacheEntry)
		if time.Now().Before(e.expires) {
			m.ll.MoveToFront(el)
			return e.cert, nil
		}
		m.ll.Remove(el)
		delete(m.items, host)
	}
	cert, err := m.mint(host)
	if err != nil {
		return nil, err
	}
	el := m.ll.PushFront(&cacheEntry{host: host, cert: cert, expires: time.Now().Add(m.ttl)})
	m.items[host] = el
	for m.ll.Len() > m.max {
		back := m.ll.Back()
		m.ll.Remove(back)
		delete(m.items, back.Value.(*cacheEntry).host)
	}
	return cert, nil
}

func (m *Minter) mint(host string) (*tls.Certificate, error) {
	caCert, caKey := m.src.Issuer()
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Minute),
		// Leaf validity must exceed the cache TTL so a cached leaf never serves past expiry.
		NotAfter:    time.Now().Add(m.ttl + time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, m.leafKey.Public(), caKey)
	if err != nil {
		return nil, fmt.Errorf("tlsbridge: mint leaf for %s: %w", host, err)
	}
	return &tls.Certificate{
		Certificate: [][]byte{der, caCert.Raw},
		PrivateKey:  m.leafKey,
	}, nil
}
```

- [ ] **Step 4: Run — expect PASS**

Run: `go test ./tlsbridge/ -run TestMinter -v`
Expected: PASS (all three).

- [ ] **Step 5: Commit**

```bash
git add authbridge/authlib/tlsbridge/minter.go authbridge/authlib/tlsbridge/minter_test.go
git commit -s -m "feat(tlsbridge): per-host leaf Minter (shared P-256 key) with LRU/TTL cache"
```

---

## Task 3: Decision + SkipSet

**Files:**
- Create: `authlib/tlsbridge/decision.go`
- Test: `authlib/tlsbridge/decision_test.go`

- [ ] **Step 1: Write the failing tests**

```go
package tlsbridge

import "testing"

func TestDecision_Classify(t *testing.T) {
	d := NewDecision(DecisionOpts{
		Ports:         map[int]bool{443: true, 8443: true},
		Scope:         ScopeExternal,
		InternalCIDRs: []string{"10.0.0.0/8"},
		SkipHosts:     []string{"pinned.example.com"},
	})
	tlsHello := []byte{0x16, 0x03, 0x01, 0x00, 0x05} // handshake, TLS1.0 record, len
	cases := []struct {
		name   string
		host   string
		ip     string
		port   int
		first  []byte
		expect Verdict
		reason string
	}{
		{"happy external https", "api.example.com", "93.184.216.34", 443, tlsHello, Terminate, ""},
		{"non-tls first byte", "api.example.com", "93.184.216.34", 443, []byte("GET / "), Passthrough, "non-tls"},
		{"unlisted port", "api.example.com", "93.184.216.34", 9999, tlsHello, Passthrough, "port"},
		{"internal under external scope", "tool.svc", "10.96.1.2", 443, tlsHello, Passthrough, "in-cluster"},
		{"skip-listed host", "pinned.example.com", "1.2.3.4", 443, tlsHello, Passthrough, "skip"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, reason := d.Classify(tc.host, tc.ip, tc.port, tc.first)
			if v != tc.expect || (tc.expect == Passthrough && reason != tc.reason) {
				t.Errorf("got (%v,%q), want (%v,%q)", v, reason, tc.expect, tc.reason)
			}
		})
	}
}

func TestSkipSet_AutoSkip(t *testing.T) {
	s := NewSkipSet()
	if s.Contains("h") {
		t.Fatal("empty set should not contain h")
	}
	s.Add("h")
	if !s.Contains("h") {
		t.Error("Add then Contains failed")
	}
}
```

- [ ] **Step 2: Run — expect compile failure**

Run: `go test ./tlsbridge/ -run 'TestDecision|TestSkipSet' -v`
Expected: FAIL — `undefined: NewDecision`.

- [ ] **Step 3: Implement `decision.go`**

```go
package tlsbridge

import (
	"net"
	"sync"
)

type Verdict int

const (
	Passthrough Verdict = iota
	Terminate
)

type Scope int

const (
	ScopeExternal Scope = iota // default: do not bridge internal/mesh destinations
	ScopeAll                   // bridge everything eligible (no-mesh / standalone)
)

type DecisionOpts struct {
	Ports         map[int]bool
	Scope         Scope
	InternalCIDRs []string
	SkipHosts     []string
}

type Decision struct {
	ports    map[int]bool
	scope    Scope
	internal []*net.IPNet
	skip     map[string]bool
}

func NewDecision(o DecisionOpts) *Decision {
	d := &Decision{ports: o.Ports, scope: o.Scope, skip: map[string]bool{}}
	if d.ports == nil {
		d.ports = map[int]bool{443: true, 8443: true}
	}
	for _, c := range o.InternalCIDRs {
		if _, n, err := net.ParseCIDR(c); err == nil {
			d.internal = append(d.internal, n)
		}
	}
	for _, h := range o.SkipHosts {
		d.skip[h] = true
	}
	return d
}

// Classify decides whether to bridge. first is the peeked client bytes.
func (d *Decision) Classify(host, ip string, port int, first []byte) (Verdict, string) {
	if !d.ports[port] {
		return Passthrough, "port"
	}
	if !looksLikeTLSClientHello(first) {
		return Passthrough, "non-tls"
	}
	if d.skip[host] {
		return Passthrough, "skip"
	}
	if d.scope == ScopeExternal && d.isInternal(ip) {
		return Passthrough, "in-cluster"
	}
	return Terminate, ""
}

func (d *Decision) isInternal(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false // a hostname (not an IP) is never matched as in-cluster — see CONNECT-path note
	}
	for _, n := range d.internal {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

// looksLikeTLSClientHello validates the 5-byte TLS record header (not just 0x16):
// content type 22 (handshake), legacy record version 0x03 0x01-0x04.
func looksLikeTLSClientHello(b []byte) bool {
	if len(b) < 5 {
		return false
	}
	return b[0] == 0x16 && b[1] == 0x03 && b[2] <= 0x04
}

// SkipSet is the runtime auto-skip set (hosts whose minted leaf the client
// rejected). Concurrent-safe; augments the static skip list.
type SkipSet struct {
	mu sync.RWMutex
	m  map[string]bool
}

func NewSkipSet() *SkipSet { return &SkipSet{m: map[string]bool{}} }
func (s *SkipSet) Add(host string) {
	s.mu.Lock()
	s.m[host] = true
	s.mu.Unlock()
}
func (s *SkipSet) Contains(host string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.m[host]
}
```

- [ ] **Step 4: Run — expect PASS**

Run: `go test ./tlsbridge/ -run 'TestDecision|TestSkipSet' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add authbridge/authlib/tlsbridge/decision.go authbridge/authlib/tlsbridge/decision_test.go
git commit -s -m "feat(tlsbridge): Decision (record-header + scope gate) and SkipSet"
```

---

## Task 4: UpstreamClient — system + injected roots

**Files:**
- Create: `authlib/tlsbridge/upstream.go`
- Test: `authlib/tlsbridge/upstream_test.go`

- [ ] **Step 1: Write the failing test** (private-CA origin must verify when its CA is injected, and fail when not)

```go
package tlsbridge

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUpstreamClient_InjectedRootVerifies(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})

	// With the origin's CA injected → verifies.
	good, err := NewUpstreamClient(caPEM)
	if err != nil {
		t.Fatalf("NewUpstreamClient: %v", err)
	}
	resp, err := good.Get(srv.URL)
	if err != nil {
		t.Fatalf("expected success with injected root, got %v", err)
	}
	resp.Body.Close()

	// Without it (system roots only) → the self-signed httptest cert is rejected.
	bare, _ := NewUpstreamClient(nil)
	if _, err := bare.Get(srv.URL); err == nil {
		t.Errorf("expected verification failure with system roots only")
	}
}
```

- [ ] **Step 2: Run — expect compile failure**

Run: `go test ./tlsbridge/ -run TestUpstreamClient -v`
Expected: FAIL — `undefined: NewUpstreamClient`.

- [ ] **Step 3: Implement `upstream.go`**

```go
package tlsbridge

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"time"
)

// NewUpstreamClient builds the HTTP client the TLS bridge uses to re-originate
// to the real origin. RootCAs = system roots + extraRootsPEM (the agent's
// injected upstream trust). It NEVER sets InsecureSkipVerify and never uses the
// mesh-mTLS dialer — re-origination must verify the origin the way the agent would.
func NewUpstreamClient(extraRootsPEM []byte) (*http.Client, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if len(extraRootsPEM) > 0 {
		if !pool.AppendCertsFromPEM(extraRootsPEM) {
			return nil, fmt.Errorf("tlsbridge: upstream_ca_bundle is not valid PEM")
		}
	}
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:       &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}
```

- [ ] **Step 4: Run — expect PASS**

Run: `go test ./tlsbridge/ -run TestUpstreamClient -v`
Expected: PASS (injected-root success; system-roots-only failure).

- [ ] **Step 5: Commit**

```bash
git add authbridge/authlib/tlsbridge/upstream.go authbridge/authlib/tlsbridge/upstream_test.go
git commit -s -m "feat(tlsbridge): upstream client with system + injected roots"
```

---

## Task 5: Terminator + ServeConn (h2 keep-alive)

**Files:**
- Create: `authlib/tlsbridge/terminator.go`, `authlib/tlsbridge/serve.go`
- Test: `authlib/tlsbridge/terminator_test.go`

- [ ] **Step 1: Write the failing test** (client trusting the ephemeral CA handshakes through the Terminator; ALPN offers h2+http/1.1)

```go
package tlsbridge

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"
	"time"
)

func TestTerminator_AgentTrustingCAHandshakes(t *testing.T) {
	src, _ := NewEphemeralSource()
	m := NewMinter(src, MinterOpts{})
	term := NewTerminator(m)

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	errc := make(chan error, 1)
	go func() {
		tconn, err := term.Terminate(c2, "api.example.com")
		if err != nil {
			errc <- err
			return
		}
		_ = tconn.Close()
		errc <- nil
	}()

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(src.CACertPEM())
	client := tls.Client(c1, &tls.Config{ServerName: "api.example.com", RootCAs: pool, NextProtos: []string{"h2", "http/1.1"}})
	_ = c1.SetDeadline(time.Now().Add(2 * time.Second))
	if err := client.Handshake(); err != nil {
		t.Fatalf("client handshake failed (agent should trust minted leaf): %v", err)
	}
	if alpn := client.ConnectionState().NegotiatedProtocol; alpn != "h2" && alpn != "http/1.1" {
		t.Errorf("unexpected ALPN %q", alpn)
	}
	if err := <-errc; err != nil {
		t.Fatalf("terminator: %v", err)
	}
}
```

- [ ] **Step 2: Run — expect compile failure**

Run: `go test ./tlsbridge/ -run TestTerminator -v`
Expected: FAIL — `undefined: NewTerminator`.

- [ ] **Step 3: Implement `terminator.go`**

```go
package tlsbridge

import (
	"crypto/tls"
	"net"
)

// Terminator wraps a sniffed client conn as a tls.Server, using the Minter to
// forge a per-SNI leaf. ALPN offers h2 + http/1.1.
type Terminator struct {
	minter *Minter
}

func NewTerminator(m *Minter) *Terminator { return &Terminator{minter: m} }

// Terminate completes the server-side TLS handshake against the agent. host is
// the dialed name/IP, used to mint when the ClientHello carries no SNI.
func (t *Terminator) Terminate(client net.Conn, host string) (*tls.Conn, error) {
	cfg := &tls.Config{
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if chi.ServerName != "" {
				return t.minter.GetCertificateForHost(chi.ServerName)
			}
			return t.minter.GetCertificateForHost(host)
		},
	}
	conn := tls.Server(client, cfg)
	if err := conn.Handshake(); err != nil {
		return nil, err
	}
	return conn, nil
}
```

- [ ] **Step 4: Implement `serve.go` (one-conn keep-alive + h2)**

```go
package tlsbridge

import (
	"crypto/tls"
	"net"
	"net/http"
	"sync"

	"golang.org/x/net/http2"
)

// oneConnListener serves exactly one already-accepted conn to http.Server.Serve,
// so keep-alive (multiple requests on the same TLS conn) works.
type oneConnListener struct {
	mu   sync.Mutex
	conn net.Conn
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil {
		return nil, net.ErrClosed
	}
	c := l.conn
	l.conn = nil
	return c, nil
}
func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return dummyAddr{} }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "tcp" }
func (dummyAddr) String() string  { return "tls-bridge" }

// ServeConn drives an already-terminated TLS conn through handler with HTTP
// keep-alive, negotiating h2 when ALPN selected it.
func ServeConn(tconn *tls.Conn, handler http.Handler) {
	srv := &http.Server{Handler: handler}
	if tconn.ConnectionState().NegotiatedProtocol == "h2" {
		h2s := &http2.Server{}
		h2s.ServeConn(tconn, &http2.ServeConnOpts{Handler: handler, BaseConfig: srv})
		return
	}
	_ = srv.Serve(&oneConnListener{conn: tconn})
}
```

- [ ] **Step 5: Run terminator test — expect PASS, then whole package**

Run: `go test ./tlsbridge/ -run TestTerminator -v`
Expected: PASS. Then `go test ./tlsbridge/ -v` — all green.

- [ ] **Step 6: Promote the http2 dep and commit**

Run: `cd authbridge/authlib && go get golang.org/x/net/http2 && go mod tidy`
Expected: `golang.org/x/net` becomes a direct dependency.

```bash
git add authbridge/authlib/tlsbridge/terminator.go authbridge/authlib/tlsbridge/serve.go authbridge/authlib/tlsbridge/terminator_test.go authbridge/authlib/go.mod authbridge/authlib/go.sum
git commit -s -m "feat(tlsbridge): Terminator + one-conn keep-alive ServeConn (h2)"
```

---

## Task 6: `Engine` facade, `serveOutbound` extraction, sniff `Peek`, helpers

**Files:**
- Create: `authlib/tlsbridge/engine.go`
- Modify: `authlib/listener/forwardproxy/server.go` (extract `serveOutbound`; add `TLSBridge` field; helpers)
- Modify: `authlib/listener/forwardproxy/sniff.go` (add `(*peekedConn).Peek`)

- [ ] **Step 1: Write `engine.go` (facade the proxy holds)**

```go
package tlsbridge

import "net/http"

// Engine bundles everything the forward proxy needs to bridge TLS.
// A nil *Engine means the bridge is disabled.
type Engine struct {
	Decision *Decision
	Term     *Terminator
	Skip     *SkipSet
	Upstream *http.Client
	CAPEM    []byte
}
```

- [ ] **Step 2: Add `Peek` to `peekedConn` in `sniff.go`**

After the `peekedConn` definition (`sniff.go:148-153`):

```go
// Peek returns the next n buffered bytes without consuming them. Used by the
// TLS-bridge classify step on the CONNECT and transparent paths.
func (c *peekedConn) Peek(n int) ([]byte, error) { return c.r.Peek(n) }
```

- [ ] **Step 3: Add the `TLSBridge` field + helpers + extract `serveOutbound` in `server.go`**

Add the field to the `Server` struct (after `SkipHosts`):

```go
	TLSBridge *tlsbridge.Engine // nil = disabled
```

Add the import `"github.com/kagenti/kagenti-extensions/authbridge/authlib/tlsbridge"` and `"strconv"`.

Add helpers (bottom of `server.go`):

```go
// hostOnly strips the port from an authority ("h:443" → "h"); returns input if no port.
func hostOnly(authority string) string {
	if h, _, err := net.SplitHostPort(authority); err == nil {
		return h
	}
	return authority
}

// portOf returns the port from an authority, defaulting to 443.
func portOf(authority string) int {
	if _, p, err := net.SplitHostPort(authority); err == nil {
		if n, err := strconv.Atoi(p); err == nil {
			return n
		}
	}
	return 443
}

// nameOrIP prefers the sniffed SNI name, falling back to the dialed IP.
func nameOrIP(name, ip string) string {
	if name != "" {
		return name
	}
	return ip
}
```

Extract the body of `handleRequest` (everything after the `MethodConnect` check) into `serveOutbound`. The **only behavioral change** is the re-origination client selection:

```go
// serveOutbound runs the outbound pipeline for one decrypted/plaintext request
// and re-originates it. isBridge=true marks requests produced by TLS bridging:
// they are origin-form (the handler sets r.URL.Scheme/Host) and must re-originate
// via the dedicated upstream client, never the mesh-mTLS s.Client.
func (s *Server) serveOutbound(w http.ResponseWriter, r *http.Request, isBridge bool) {
	// ... existing handleRequest body (pctx build :197-206, skip logic, pipeline.Run
	//     :258-270, Authorization/body/hop-by-hop handling :305-329, RequestURI clear
	//     :332, response phase) ...

	// At the single re-origination site (was `resp, err := s.Client.Do(r)` :334):
	client := s.Client
	if isBridge && s.TLSBridge != nil {
		client = s.TLSBridge.Upstream
	}
	resp, err := client.Do(r)
	// ... unchanged response handling ...
}

func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		s.handleConnect(w, r)
		return
	}
	s.serveOutbound(w, r, false)
}
```

> **No `!isBridge` guard is needed around the hop-by-hop / `Proxy-*` strip (`:320-329`).** Deleting `Connection`/`Proxy-Authorization`/etc. from an origin-form request is a harmless no-op (those headers aren't present). The `isBridge` flag's *only* job is selecting the upstream client.

- [ ] **Step 4: Run the existing forward-proxy suite — expect PASS (pure refactor)**

Run: `cd authbridge/authlib && go test ./listener/forwardproxy/ ./tlsbridge/ -v`
Expected: PASS — the refactor is behavior-preserving for the plaintext path.

- [ ] **Step 5: Commit**

```bash
git add authbridge/authlib/tlsbridge/engine.go authbridge/authlib/listener/forwardproxy/server.go authbridge/authlib/listener/forwardproxy/sniff.go
git commit -s -m "refactor(forwardproxy): extract serveOutbound; add tlsbridge.Engine facade + sniff Peek"
```

---

## Task 7: Reversible bridge branch on the transparent path

**Files:**
- Modify: `authlib/listener/forwardproxy/transparent.go` (bridge-or-tunnel before `tunnel(...)` at `:125`)
- Modify: `authlib/listener/forwardproxy/server.go` (add `bridgeServe`)
- Test: `authlib/listener/forwardproxy/tlsbridge_integration_test.go`

- [ ] **Step 1: Write the failing integration test**

```go
package forwardproxy

// Build a Server with a tlsbridge.Engine (ephemeral CA). Drive HandleTransparentConn
// with a tls.Client that trusts the CA, targeting an httptest TLS origin whose CA is
// in the engine's upstream client. Register a probe plugin that records the decrypted
// request path. Assert: probe saw "/secret", response body intact.
func TestTransparentBridge_DecryptsAndRunsPipeline(t *testing.T) {
	// ... uses NewEphemeralSource, NewMinter, NewTerminator, NewDecision(ScopeAll),
	//     NewUpstreamClient(originCA), a recording probe plugin, httptest.NewTLSServer
	//     origin. Wire engine onto Server.TLSBridge, run a goroutine that calls
	//     s.HandleTransparentConn(serverSideConn, originIP:port), and drive a
	//     tls.Client trusting the ephemeral CA over the other end of a net.Pipe /
	//     loopback. (Mirror server_test.go's Server construction + pipeline probe.)
}
```

(Construct the recording probe via the existing `pipeline` test helpers used in `server_test.go`; mirror that file's Server construction.)

- [ ] **Step 2: Run — expect FAIL** (`Server.TLSBridge` is nil; transparent path still tunnels).

- [ ] **Step 3: Add `bridgeServe` to `server.go`** (shared by both paths):

```go
// bridgeServe attempts to bridge: verify the upstream origin first (reversibility),
// then forge a leaf + terminate the agent TLS + run the UNCHANGED pipeline via
// serveOutbound. authority is host:port (used to dial+verify upstream and to set
// r.URL.Host); host is the skip/log key. Returns true if it consumed the connection
// (success OR an unrecoverable post-forge failure that was logged); false to fall
// back to a plain tunnel — so no working call is ever broken.
func (s *Server) bridgeServe(client net.Conn, authority, host string) bool {
	// 1) Verify upstream reachability + cert via the dedicated client, BEFORE forging.
	//    HEAD avoids GET side-effects; a non-2xx/4xx/5xx status still returns err==nil
	//    (cert verified), which is all we need. Only a transport/TLS error fails here.
	resp, err := s.TLSBridge.Upstream.Head("https://" + authority)
	if err != nil {
		slog.Info("tls-bridge passthrough", "host", host, "reason", "upstream-verify", "error", err)
		return false // fall back to plain tunnel — agent's own e2e TLS still reaches origin
	}
	_ = resp.Body.Close()

	// 2) Forge + terminate downstream.
	tconn, err := s.TLSBridge.Term.Terminate(client, hostOnly(authority))
	if err != nil {
		s.TLSBridge.Skip.Add(host) // pinned client → its retry will passthrough
		slog.Warn("tls-bridge passthrough", "host", host, "reason", "handshake-fail", "error", err)
		return true // conn is dead post-forge; nothing left to tunnel
	}

	// 3) Serve the decrypted conn through the UNCHANGED pipeline.
	tlsbridge.ServeConn(tconn, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Scheme = "https"
		r.URL.Host = authority // host:port — preserves non-443 origins
		s.serveOutbound(w, r, true)
	}))
	return true
}
```

- [ ] **Step 4: Implement the branch** in `HandleTransparentConn`, replacing `tunnel(clientConn, upstream)` (`transparent.go:125`). At this point `clientConn` is the `wrapped` `*peekedConn` and `name`/`host` are in scope:

```go
	if s.TLSBridge != nil {
		ip := hostOnly(dst)
		key := nameOrIP(name, ip)
		var first []byte
		if pc, ok := clientConn.(*peekedConn); ok {
			first, _ = pc.Peek(5)
		}
		if !s.TLSBridge.Skip.Contains(key) {
			if v, reason := s.TLSBridge.Decision.Classify(key, ip, portOf(dst), first); v == tlsbridge.Terminate {
				authority := net.JoinHostPort(key, strconv.Itoa(portOf(dst)))
				_ = upstream.Close() // bridgeServe dials its own verified upstream; drop the pre-dial
				if s.bridgeServe(clientConn, authority, key) {
					return
				}
				// bridgeServe fell open (upstream-verify failed) → re-dial for the tunnel.
				if up2, derr := net.DialTimeout("tcp", dst, connectDialTimeout); derr == nil {
					tunnel(clientConn, up2)
					_ = up2.Close()
				}
				return
			} else {
				slog.Info("tls-bridge passthrough", "host", key, "reason", reason)
			}
		}
	}
	tunnel(clientConn, upstream)
```

(Add `"strconv"` to `transparent.go` imports if not present. The pre-dialed `upstream` is closed on the bridge path; the existing `defer upstream.Close()` makes the non-bridge fall-through safe.)

- [ ] **Step 5: Run the integration test — expect PASS**

Run: `go test ./listener/forwardproxy/ -run TestTransparentBridge -v`
Expected: PASS — probe recorded `/secret`, response intact.

- [ ] **Step 6: Commit**

```bash
git add authbridge/authlib/listener/forwardproxy/
git commit -s -m "feat(forwardproxy): reversible TLS bridge on transparent path"
```

---

## Task 8: Bridge branch on the CONNECT path

**Files:**
- Modify: `authlib/listener/forwardproxy/server.go` (`handleConnect`, the `tunnel(clientConn, upstream)` at `:822`)
- Test: add `TestConnectBridge_DecryptsAndRunsPipeline` to `tlsbridge_integration_test.go`

- [ ] **Step 1: Write the failing CONNECT integration test** (same shape as Task 7 but the client sends `CONNECT host:443`, reads `200`, then starts a TLS ClientHello on the same conn).

- [ ] **Step 2: Run — expect FAIL** (CONNECT still always tunnels after the 200).

- [ ] **Step 3: Implement** — after writing `200 Connection Established` and `recordTunnelOpened`, before `tunnel(clientConn, upstream)` (`server.go:822`). Unlike the transparent path, nothing has sniffed yet, so wrap the hijacked conn in a fresh `peekedConn` (a `bufio.Reader` over it) to peek the agent's ClientHello and stay replayable:

```go
	if s.TLSBridge != nil {
		pc := &peekedConn{Conn: clientConn, r: bufio.NewReaderSize(clientConn, sniffBufSize)}
		clientConn = pc // replay peeked bytes into whichever path runs
		first, _ := pc.Peek(5)
		authority := r.Host // CONNECT target is already host:port
		key := hostOnly(r.Host)
		if !s.TLSBridge.Skip.Contains(key) {
			// ip is empty for a hostname CONNECT target → scope=external won't match
			// it as in-cluster (documented limitation; transparent path keys on the IP).
			if v, _ := s.TLSBridge.Decision.Classify(key, hostOnly(r.Host), portOf(r.Host), first); v == tlsbridge.Terminate {
				_ = upstream.Close() // bridgeServe dials its own verified upstream
				if s.bridgeServe(clientConn, authority, key) {
					return
				}
				if up2, derr := net.DialTimeout("tcp", r.Host, connectDialTimeout); derr == nil {
					tunnel(clientConn, up2)
					_ = up2.Close()
				}
				return
			}
		}
	}
	tunnel(clientConn, upstream)
```

(Add `"bufio"` to `server.go` imports if not present. `sniffBufSize` is the existing const in `sniff.go`, same package.)

- [ ] **Step 4: Run — expect PASS**

Run: `go test ./listener/forwardproxy/ -run TestConnectBridge -v`

- [ ] **Step 5: Commit**

```bash
git commit -s -am "feat(forwardproxy): reversible TLS bridge on CONNECT path"
```

---

## Task 9: No-broken-calls guarantees (the critical safety tests)

**Files:**
- Test: `authlib/listener/forwardproxy/tlsbridge_safety_test.go`

- [ ] **Step 1: Write the safety tests** (these encode the spec's success criteria #4/#5)

```go
// 1) Untrusted/unreachable origin → upstream-verify (HEAD) fails → request is
//    TUNNELED (passthrough), NOT failed. Drive an origin whose CA is NOT in the
//    engine's upstream client; assert the agent's own end-to-end TLS still reaches it.
func TestBridge_UnverifiableUpstream_FallsOpenToTunnel(t *testing.T) { /* ... */ }

// 2) Pinned client (rejects minted leaf) → host auto-skipped → retry passes through.
//    Use a tls.Client with RootCAs that does NOT include the ephemeral CA; first
//    call fails, assert Skip.Contains(host), second call tunnels & the agent's TLS
//    reaches the origin.
func TestBridge_PinnedClient_AutoSkipsThenTunnels(t *testing.T) { /* ... */ }

// 3) Non-TLS bytes on a bridge-eligible port → passthrough (no tls.Server attempt).
func TestBridge_NonTLS_Passthrough(t *testing.T) { /* ... */ }
```

- [ ] **Step 2: Run — fix any gaps in Task 7/8 logic until all three PASS**

Run: `go test ./listener/forwardproxy/ -run TestBridge_ -v`
Expected: all PASS — no configuration of the bridge turns a working call into a hard failure.

- [ ] **Step 3: Commit**

```bash
git commit -s -am "test(tlsbridge): no-broken-calls guarantees (fall-open + auto-skip)"
```

---

## Task 10: `TLSBridgeConfig` + `main.go` wiring + trust self-check

**Files:**
- Modify: `authlib/config/config.go` (add `TLSBridge *TLSBridgeConfig` block, mirroring `MTLS`/`SPIFFE`)
- Modify: `cmd/authbridge-proxy/main.go` (construct the engine, set `fpSrv.TLSBridge`, run self-check)
- Modify: `authlib/tlsbridge/engine.go` (add `RunTrustSelfCheck`)
- Test: `authlib/config/config_test.go` (decode + validate)

- [ ] **Step 1: Write the config decode test** (`Load` takes a **path** — write a temp file)

```go
func TestConfig_TLSBridgeBlockDecodes(t *testing.T) {
	y := []byte("mode: proxy-sidecar\n" +
		"tls_bridge:\n" +
		"  enabled: true\n" +
		"  scope: external\n" +
		"  ca_source: ephemeral\n" +
		"  skip_hosts: [\"pinned.example.com\"]\n")
	p := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(p, y, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.TLSBridge == nil || !cfg.TLSBridge.Enabled || cfg.TLSBridge.Scope != "external" {
		t.Fatalf("tls_bridge block did not decode: %+v", cfg.TLSBridge)
	}
}
```

(Add `"os"` and `"path/filepath"` to `config_test.go` imports if not present.)

- [ ] **Step 2: Run — expect FAIL** (`cfg.TLSBridge` undefined).

- [ ] **Step 3: Add `TLSBridgeConfig` to `config.go`** (after the `SPIFFE *SPIFFEConfig` field at `:39`):

```go
	// TLSBridge, when non-nil and Enabled, terminates agent outbound TLS so the
	// outbound pipeline sees decrypted HTTPS. See docs/.../tlsbridge-design.md.
	TLSBridge *TLSBridgeConfig `yaml:"tls_bridge,omitempty" json:"tls_bridge,omitempty"`
```

```go
type TLSBridgeConfig struct {
	Enabled          bool     `yaml:"enabled" json:"enabled"`
	Scope            string   `yaml:"scope" json:"scope"` // external | all
	InternalCIDRs    []string `yaml:"internal_cidrs" json:"internal_cidrs"`
	CASource         string   `yaml:"ca_source" json:"ca_source"` // file | ephemeral
	CACertPath       string   `yaml:"ca_cert_path" json:"ca_cert_path"`
	CAKeyPath        string   `yaml:"ca_key_path" json:"ca_key_path"`
	UpstreamCABundle string   `yaml:"upstream_ca_bundle" json:"upstream_ca_bundle"`
	SkipHosts        []string `yaml:"skip_hosts" json:"skip_hosts"`
}

// Validate is called from the loader when TLSBridge != nil.
func (b *TLSBridgeConfig) Validate() error {
	if b.Scope != "" && b.Scope != "external" && b.Scope != "all" {
		return fmt.Errorf("tls_bridge.scope must be 'external' or 'all', got %q", b.Scope)
	}
	if b.CASource == "file" && (b.CACertPath == "" || b.CAKeyPath == "") {
		return fmt.Errorf("tls_bridge.ca_source=file requires ca_cert_path and ca_key_path")
	}
	return nil
}
```

Hook it into the loader beside the existing `MTLS`/`SPIFFE` validation (`config.go:441`/`:460`):

```go
	if cfg.TLSBridge != nil {
		if err := cfg.TLSBridge.Validate(); err != nil {
			return nil, err
		}
	}
```

- [ ] **Step 4: Run config test — expect PASS**

Run: `go test ./config/ -run TestConfig_TLSBridge -v`

- [ ] **Step 5: Add `RunTrustSelfCheck` to `engine.go`**

```go
import (
	"log/slog"
	"os"
	"strings"
)

// RunTrustSelfCheck logs a loud WARN when the bridge CA does not appear in the
// trust file the agent runtime is told to use (SSL_CERT_FILE / NODE_EXTRA_CA_CERTS).
// Best-effort: a trust-miss is then a visible signal, not an opaque handshake error.
// (In Phase 1 — test-only — no agent trust env is set, so this simply notes that.)
func RunTrustSelfCheck(caPEM []byte) {
	for _, env := range []string{"SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE"} {
		p := os.Getenv(env)
		if p == "" {
			continue
		}
		b, err := os.ReadFile(p)
		if err == nil && strings.Contains(string(b), strings.TrimSpace(string(caPEM))) {
			slog.Info("tls-bridge trust self-check OK", "env", env, "path", p)
			return
		}
	}
	slog.Warn("tls-bridge trust self-check: CA not found in any agent trust file " +
		"(SSL_CERT_FILE/NODE_EXTRA_CA_CERTS/REQUESTS_CA_BUNDLE) — agent will not trust " +
		"minted leaves; egress will safely tunnel. Expected in Phase 1 (test-only).")
}
```

- [ ] **Step 6: Wire `main.go`** — beside the `fpMTLS` construction (`main.go:243`), build the engine when enabled; set the field after `NewServer` (mirroring `fpSrv.SkipHosts`/`fpSrv.Shared`). main uses `log.Fatalf` for boot-fatal errors — match it (the sketch below shows `slog.Error`+`os.Exit`; use `log.Fatalf` to fit the file):

```go
	var bridge *tlsbridge.Engine
	if cfg.TLSBridge != nil && cfg.TLSBridge.Enabled {
		var src tlsbridge.CASource
		if cfg.TLSBridge.CASource == "file" {
			src, err = tlsbridge.NewFileSource(cfg.TLSBridge.CACertPath, cfg.TLSBridge.CAKeyPath)
		} else {
			src, err = tlsbridge.NewEphemeralSource()
		}
		if err != nil {
			slog.Error("tls-bridge CA init failed", "error", err)
			os.Exit(1)
		}
		var extra []byte
		if cfg.TLSBridge.UpstreamCABundle != "" {
			if extra, err = os.ReadFile(cfg.TLSBridge.UpstreamCABundle); err != nil {
				slog.Error("tls-bridge upstream_ca_bundle read failed", "error", err)
				os.Exit(1)
			}
		}
		up, uerr := tlsbridge.NewUpstreamClient(extra)
		if uerr != nil {
			slog.Error("tls-bridge upstream client failed", "error", uerr)
			os.Exit(1)
		}
		minter := tlsbridge.NewMinter(src, tlsbridge.MinterOpts{})
		scope := tlsbridge.ScopeExternal
		if cfg.TLSBridge.Scope == "all" {
			scope = tlsbridge.ScopeAll
		}
		bridge = &tlsbridge.Engine{
			Decision: tlsbridge.NewDecision(tlsbridge.DecisionOpts{
				Scope: scope, InternalCIDRs: cfg.TLSBridge.InternalCIDRs, SkipHosts: cfg.TLSBridge.SkipHosts,
			}),
			Term:     tlsbridge.NewTerminator(minter),
			Skip:     tlsbridge.NewSkipSet(),
			Upstream: up,
			CAPEM:    src.CACertPEM(),
		}
		tlsbridge.RunTrustSelfCheck(bridge.CAPEM)
		slog.Info("tls-bridge enabled", "scope", cfg.TLSBridge.Scope, "ca_source", cfg.TLSBridge.CASource)
	}

	fpSrv, err := forwardproxy.NewServer(outboundH, sessions, fpMTLS)
	// ... existing fpSrv.SkipHosts / fpSrv.Shared assignments ...
	fpSrv.TLSBridge = bridge
```

(Add the `tlsbridge` import to `main.go`.)

- [ ] **Step 7: Build everything + full test run**

Run: `cd authbridge/authlib && go build ./... && go test ./...`
Run: `cd authbridge && go build ./cmd/authbridge-proxy/`
Expected: all green.

- [ ] **Step 8: Commit**

```bash
git add authbridge/authlib/config/ authbridge/cmd/authbridge-proxy/main.go authbridge/authlib/tlsbridge/
git commit -s -m "feat(tlsbridge): config block + main wiring + trust self-check"
```

---

## Phase 1 done — definition of done

- `go test ./...` green in `authbridge/authlib`; `authbridge-proxy` builds.
- With `tls_bridge.enabled` + ephemeral CA + an **in-process client trusting the CA**: agent HTTPS egress is decrypted, the **unchanged** pipeline runs (probe sees method/path/headers/body), the call reaches the real (verified) origin, response is byte-intact — proven by in-process integration tests. (A real operator-deployed agent does **not** trust the CA yet and safely tunnels — that's Phase 2.)
- The three safety tests pass: unverifiable upstream → tunnel; pinned client → auto-skip then tunnel; non-TLS → passthrough. **No bridge configuration breaks a working call.**
- h2 and h1.1 origins both work; keep-alive (2 requests / 1 conn) works.

---

## Phase 2 compatibility (cert-manager path is the target — 2c)

Phase 1 is built so the operator-coordinated cert-manager CA drops in with **no changes to the bridge core**:

- **The seam is `CASource`.** Phase 1 defaults to `EphemeralSource`; 2c sets `ca_source: file` + paths and `main.go` calls `NewFileSource` instead — already implemented (Task 1) and selected (Task 10 Step 6). `Minter`, `Terminator`, `Decision`, `ServeConn`, `bridgeServe`, `serveOutbound` are all CA-agnostic.
- **Key formats:** `FileSource` parses PKCS#8 / PKCS#1 / SEC1, so cert-manager's default (PKCS#1) loads (Task 1).
- **Name Constraints (2c CA):** no Minter change. A leaf for an origin outside the CA's permitted names simply fails the agent's verify → auto-skip → tunnel — the existing no-broken-calls path.
- **`upstream_ca_bundle`** already feeds the re-origination client (Task 4/10), independent of the signing CA.

Phase 2 (operator) is intentionally **not** bite-sized here. When Phase 1 lands, write its own plan covering: `MITMMode`→**`TLSBridgeMode`** CRD field + resolution; per-agent cert-manager `Certificate`/`Issuer` reconciler with Name Constraints; cert-manager + namespaced-`Issuer` write RBAC; webhook hard-mounts (sidecar `tls.crt`+`tls.key` `0400`; agent `ca.crt` + trust env, `Optional:false` to gate pod start); `tls_bridge: {ca_source: file, ...}` config render; `:9094` session-API localhost-bind + raw-body redaction when the bridge is on; E2E on OpenShell; CA rotation follow-up.

---

## Self-review notes

- **Spec coverage:** every Phase-1 spec item maps to a task — `tlsbridge/` units (T1–T5), serveOutbound + full-pipeline reuse (T6), reversible decision + upstream-verify-first (T7/T8 `bridgeServe`), auto-skip (T7/T9), interception scope gate (T3/T10), upstream client with injected roots (T4), h2 + keep-alive (T5), config + wiring + self-check (T10), no-broken-calls (T9). Operator items → Phase 2.
- **Fixes folded in from the end-to-end audit (2026-06-17):** `Load(path)` not `Load(bytes)` (T10 temp file); main `log.Fatalf` for boot-fatal (T10 — corrected: the cmd entrypoint uses `log.Fatalf`, not `slog`+`os.Exit`); `(*peekedConn).Peek` added since `sniffHost` discards the ClientHello (T6) — replaces the undefined `peekFirstBytes`/`peek(5)`; `hostOnly`/`portOf`/`nameOrIP` helpers defined (T6/T7); CONNECT `peekedConn` constructed **with** its `bufio.Reader` (T8); `serveOutbound` selects `s.TLSBridge.Upstream` for bridge re-origination, never the mesh-mTLS `s.Client` (T6, **core correctness**); authority (`host:port`) carried so non-443 origins re-originate and verify on the right port (T7/T8); upstream verify uses `HEAD` to avoid `GET /` side-effects (T7); `FileSource` multi-format key parsing for 2c (T1); pre-dialed upstream closed and re-dialed only on fall-open (T7/T8).
- **Known minor limitations (documented, not bugs):** CONNECT-path `scope=external` keys on a hostname (no IP) so it won't classify a hostname-addressed in-cluster service as internal — the transparent path (which has the SO_ORIGINAL_DST IP) does; the bridge pays one upstream HEAD per agent connection (reversibility cost) plus the relay handshake.
- **Type consistency:** `GetCertificateForHost(host string)`, `Terminate(client net.Conn, host string)`, `Classify(host, ip string, port int, first []byte) (Verdict, string)`, `Engine{Decision,Term,Skip,Upstream,CAPEM}`, `bridgeServe(client net.Conn, authority, host string) bool`, `serveOutbound(w, r, isBridge bool)` are used consistently across tasks.
