# Credential Placeholder Swap Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep the real user token out of the agent — `jwt-validation` mints an opaque placeholder on inbound, `token-exchange` resolves it back to the real token on egress before exchange.

**Architecture:** A generic process-scoped TTL store (`authlib/shared`) is injected into the listeners and exposed on `pipeline.Context.Shared`. A tiny `authlib/placeholder` package holds the handle convention. `jwt-validation` (mint mode) stores the real token under a random handle and forwards the handle to the agent; the reverseproxy/extproc inbound paths propagate that header change to the agent. `token-exchange` (resolve mode) looks the handle up and substitutes the real token before its normal RFC 8693 exchange.

**Tech Stack:** Go (module root `authbridge/`), `crypto/rand`, `encoding/base64`, standard `testing`.

**Spec:** `authbridge/docs/superpowers/specs/2026-06-02-credential-placeholder-swap-design.md`

**All commands run from** `/Users/haihuang/works/go/src/github.com/kagenti/kagenti-extensions/authbridge`.

**Scope (v1):** `authbridge-proxy` / `authbridge-lite` (reverseproxy + forwardproxy) and `authbridge-envoy` (extproc), all single-replica. `extauthz` (waypoint) and an external store are deferred — the waypoint topology needs the external store anyway (see spec compatibility table).

**Commit discipline:** DCO sign-off on every commit (`git commit -s`). Use the `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>` trailer, never `Co-Authored-By`. Run `golangci-lint run` before the final push.

---

## File Structure

| File | Responsibility | Create/Modify |
|------|----------------|---------------|
| `authlib/shared/store.go` | Generic process-scoped TTL key→value store (`any` values) | Create |
| `authlib/shared/store_test.go` | Store unit tests | Create |
| `authlib/placeholder/placeholder.go` | Handle convention: `Prefix`, `New()`, `Key()` | Create |
| `authlib/placeholder/placeholder_test.go` | Placeholder helper tests | Create |
| `authlib/pipeline/context.go` | Add `SharedStore` interface + `Context.Shared` field | Modify |
| `authlib/pipeline/sharedstore_test.go` | Interface-satisfaction assertion | Create |
| `authlib/plugins/jwtvalidation/plugin.go` | `placeholder_mode`/`placeholder_ttl` config + mint logic | Modify |
| `authlib/plugins/jwtvalidation/placeholder_test.go` | Mint-mode tests | Create |
| `authlib/plugins/tokenexchange/plugin.go` | `resolve_placeholders` config + resolve logic | Modify |
| `authlib/plugins/tokenexchange/placeholder_test.go` | Resolve tests | Create |
| `authlib/listener/reverseproxy/server.go` | `Shared` field + inbound Authorization propagation | Modify |
| `authlib/listener/reverseproxy/placeholder_test.go` | Inbound propagation test | Create |
| `authlib/listener/forwardproxy/server.go` | `Shared` field set on pctx | Modify |
| `authlib/listener/extproc/server.go` | `Shared` field + inbound propagation in `handleInbound`/`handleInboundBody` | Modify |
| `authlib/listener/extproc/placeholder_test.go` | Inbound propagation test | Create |
| `cmd/authbridge-proxy/main.go` | Create store, set on both servers | Modify |
| `cmd/authbridge-lite/main.go` | Create store, set on both servers | Modify |
| `cmd/authbridge-envoy/main.go` | Create store, set in extproc.Server literal | Modify |
| `authbridge/docs/plugin-reference.md` | Document the new mode | Modify |

---

## Design notes the implementer must respect

1. **Resolve happens in `token-exchange.OnRequest` BEFORE `p.inner.HandleOutbound`.** Reason: route matching is inside `HandleOutbound`, but the plugin only ever writes the `Authorization` header on `ActionReplaceToken` (a matched exchange route). So substituting the real token as the *subject* before `HandleOutbound` cannot leak it to an unmatched host — on passthrough/no-route the plugin leaves the header untouched (still the placeholder). Resolving first is also correct for the token cache, which keys on the real subject token.
2. **Passthrough hosts receive the placeholder, not the real token.** With mint on, the agent never holds the real token, so any non-exchange (passthrough) egress forwards the opaque handle. Hosts that need a real credential must be configured as exchange routes. Document this; consider `default_policy: exchange` or a deny default when mint is on.
3. **Fail-closed on lookup miss.** `resolve_placeholders` on + handle prefix present + store miss (expired/forged/restart) → deny. Never hand a placeholder to Keycloak as a subject token.
4. **Multi-use handle.** `Get` never deletes; the same handle resolves for every outbound call until its TTL expires.
5. **Listener wiring uses an exported `Shared` field set after construction** — not a `NewServer` signature change — to avoid churning every call site and test.

---

## Task 1: Generic shared store

**Files:**
- Create: `authlib/shared/store.go`
- Test: `authlib/shared/store_test.go`

- [ ] **Step 1: Write the failing tests**

Create `authlib/shared/store_test.go`:

```go
package shared

import (
	"sync"
	"testing"
	"time"
)

func TestStore_PutGet(t *testing.T) {
	s := New()
	s.Put("k", "v", time.Minute)
	got, ok := s.Get("k")
	if !ok || got.(string) != "v" {
		t.Fatalf("Get = %v, %v; want v, true", got, ok)
	}
}

func TestStore_GetMissing(t *testing.T) {
	s := New()
	if _, ok := s.Get("nope"); ok {
		t.Fatal("expected miss")
	}
}

func TestStore_Expiry(t *testing.T) {
	s := New()
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	s.Put("k", "v", time.Minute)
	now = now.Add(30 * time.Second)
	if _, ok := s.Get("k"); !ok {
		t.Fatal("should still be live at 30s")
	}
	now = now.Add(31 * time.Second)
	if _, ok := s.Get("k"); ok {
		t.Fatal("should be expired past 60s")
	}
}

func TestStore_Delete(t *testing.T) {
	s := New()
	s.Put("k", "v", time.Minute)
	s.Delete("k")
	if _, ok := s.Get("k"); ok {
		t.Fatal("expected deleted")
	}
}

func TestStore_ConcurrentAccess(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := string(rune('a' + n%26))
			s.Put(key, n, time.Minute)
			s.Get(key)
		}(i)
	}
	wg.Wait()
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./authlib/shared/ -v`
Expected: FAIL — `undefined: New`.

- [ ] **Step 3: Write the store**

Create `authlib/shared/store.go`:

```go
// Package shared provides a generic, process-scoped, TTL key→value store
// that plugins reach via pipeline.Context.Shared. It is intentionally
// semantics-free — feature-specific conventions (e.g. credential
// placeholders) live in their own packages and namespace their keys.
package shared

import (
	"sync"
	"time"
)

type entry struct {
	val     any
	expires time.Time
}

// Store is a thread-safe TTL map. The zero value is not usable; call New.
type Store struct {
	mu    sync.RWMutex
	items map[string]entry
	now   func() time.Time // injectable for tests
}

// New returns an empty Store.
func New() *Store {
	return &Store{items: make(map[string]entry), now: time.Now}
}

// Put stores val under key with the given time-to-live.
func (s *Store) Put(key string, val any, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[key] = entry{val: val, expires: s.now().Add(ttl)}
}

// Get returns the value for key if present and unexpired. Expired entries
// are evicted lazily.
func (s *Store) Get(key string) (any, bool) {
	s.mu.RLock()
	e, ok := s.items[key]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if s.now().After(e.expires) {
		s.Delete(key)
		return nil, false
	}
	return e.val, true
}

// Delete removes key.
func (s *Store) Delete(key string) {
	s.mu.Lock()
	delete(s.items, key)
	s.mu.Unlock()
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./authlib/shared/ -race -v`
Expected: PASS (all 5 tests, no data races).

- [ ] **Step 5: Commit**

```bash
git add authlib/shared/
git commit -s -m "feat(authbridge): add generic process-scoped TTL shared store"
```

---

## Task 2: Placeholder convention package

**Files:**
- Create: `authlib/placeholder/placeholder.go`
- Test: `authlib/placeholder/placeholder_test.go`

- [ ] **Step 1: Write the failing tests**

Create `authlib/placeholder/placeholder_test.go`:

```go
package placeholder

import (
	"strings"
	"testing"
)

func TestNew_HasPrefixAndIsUnique(t *testing.T) {
	a, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.HasPrefix(a, Prefix) {
		t.Fatalf("handle %q missing prefix %q", a, Prefix)
	}
	if len(a) <= len(Prefix)+10 {
		t.Fatalf("handle %q too short", a)
	}
	b, _ := New()
	if a == b {
		t.Fatal("two handles collided")
	}
}

func TestIsPlaceholder(t *testing.T) {
	h, _ := New()
	if !IsPlaceholder(h) {
		t.Fatalf("IsPlaceholder(%q) = false", h)
	}
	if IsPlaceholder("eyJhbGci-real-jwt") {
		t.Fatal("real token misclassified as placeholder")
	}
}

func TestKey_NamespacesHandle(t *testing.T) {
	if got := Key("abph_xyz"); got != "placeholder/abph_xyz" {
		t.Fatalf("Key = %q", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./authlib/placeholder/ -v`
Expected: FAIL — `undefined: New`.

- [ ] **Step 3: Write the package**

Create `authlib/placeholder/placeholder.go`:

```go
// Package placeholder defines the convention for opaque credential
// handles: a random "abph_" token the agent receives in place of the real
// Authorization value, plus the namespaced key used to store the real
// token in a shared store. The generic store itself (authlib/shared) holds
// no credential semantics.
package placeholder

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
)

// Prefix marks a value as a credential placeholder. token-exchange uses it
// as a cheap fast-path before attempting a store lookup.
const Prefix = "abph_"

// keyNamespace prefixes shared-store keys to avoid collisions with other
// shared-store consumers.
const keyNamespace = "placeholder/"

// New returns a fresh, unguessable handle (Prefix + 256 bits base64url).
func New() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// IsPlaceholder reports whether token is a placeholder handle.
func IsPlaceholder(token string) bool {
	return strings.HasPrefix(token, Prefix)
}

// Key returns the shared-store key for a handle.
func Key(handle string) string {
	return keyNamespace + handle
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./authlib/placeholder/ -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add authlib/placeholder/
git commit -s -m "feat(authbridge): add placeholder handle convention package"
```

---

## Task 3: SharedStore interface on the pipeline Context

**Files:**
- Modify: `authlib/pipeline/context.go` (struct at lines 82-132; add interface near the `Identity` interface ~line 26)
- Test: `authlib/pipeline/sharedstore_test.go`

- [ ] **Step 1: Write the failing test**

Create `authlib/pipeline/sharedstore_test.go`:

```go
package pipeline_test

import (
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/shared"
)

// shared.Store must satisfy pipeline.SharedStore so listeners can inject it.
func TestSharedStore_StoreSatisfiesInterface(t *testing.T) {
	var _ pipeline.SharedStore = shared.New()
}

// Context must expose a Shared field of the interface type.
func TestSharedStore_ContextField(t *testing.T) {
	pctx := &pipeline.Context{Shared: shared.New()}
	if pctx.Shared == nil {
		t.Fatal("Context.Shared not assignable")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./authlib/pipeline/ -run TestSharedStore -v`
Expected: FAIL — `undefined: pipeline.SharedStore` / unknown field `Shared`.

- [ ] **Step 3: Add the interface and field**

In `authlib/pipeline/context.go`, add the interface immediately after the `Identity` interface block (after line 30):

```go
// SharedStore is a process-scoped key→value store with TTL, injected by the
// listener so plugins can share state across the inbound→outbound request
// boundary (e.g. credential placeholders). Implemented by authlib/shared.Store.
type SharedStore interface {
	Put(key string, val any, ttl time.Duration)
	Get(key string) (any, bool)
	Delete(key string)
}
```

In the `Context` struct, add the field after the `TLS *tls.ConnectionState` field (around line 124):

```go
	// Shared is the process-scoped store the listener injects. May be nil
	// when no store is wired; plugins that require it must fail closed.
	Shared SharedStore
```

(`time` is already imported in context.go.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./authlib/pipeline/ -run TestSharedStore -v`
Expected: PASS (2 tests).

- [ ] **Step 5: Commit**

```bash
git add authlib/pipeline/context.go authlib/pipeline/sharedstore_test.go
git commit -s -m "feat(authbridge): expose SharedStore on the pipeline Context"
```

---

## Task 4: jwt-validation mint mode

**Files:**
- Modify: `authlib/plugins/jwtvalidation/plugin.go` (config struct 30-102; `JWTValidation` struct 187-208; `Configure` 233-294; `OnRequest` 349-414)
- Test: `authlib/plugins/jwtvalidation/placeholder_test.go`

- [ ] **Step 1: Write the failing tests**

Create `authlib/plugins/jwtvalidation/placeholder_test.go`:

```go
package jwtvalidation

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/placeholder"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/shared"
)

// fakeAuth lets us drive OnRequest's allow path without real JWKS.
// The real plugin uses p.inner; for these tests we configure a bypass so
// validation is skipped, then assert mint behavior on the allow path.
func mintTestContext(store pipeline.SharedStore) *pipeline.Context {
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Path:      "/work",
		Headers:   http.Header{"Authorization": []string{"Bearer real-user-token"}},
		Shared:    store,
	}
	pctx.SetCurrentPlugin("jwt-validation", pipeline.InvocationPhaseRequest)
	return pctx
}

func TestMint_ReplacesAuthAndStoresToken(t *testing.T) {
	st := shared.New()
	p := &JWTValidation{
		cfg:            jwtValidationConfig{PlaceholderMode: true},
		placeholderTTL: time.Hour,
	}
	pctx := mintTestContext(st)

	handle, real, ok := p.mint(pctx)
	if !ok {
		t.Fatal("mint returned not-ok")
	}
	if !placeholder.IsPlaceholder(handle) {
		t.Fatalf("handle %q not a placeholder", handle)
	}
	if real != "real-user-token" {
		t.Fatalf("stored token = %q", real)
	}
	if pctx.Headers.Get("Authorization") != "Bearer "+handle {
		t.Fatalf("header = %q, want Bearer %s", pctx.Headers.Get("Authorization"), handle)
	}
	got, present := st.Get(placeholder.Key(handle))
	if !present || got.(string) != "real-user-token" {
		t.Fatalf("store[%s] = %v, %v", handle, got, present)
	}
}

func TestMint_NilStoreFailsClosed(t *testing.T) {
	p := &JWTValidation{cfg: jwtValidationConfig{PlaceholderMode: true}, placeholderTTL: time.Hour}
	pctx := mintTestContext(nil)
	if _, _, ok := p.mint(pctx); ok {
		t.Fatal("mint must fail when Shared is nil")
	}
}

func TestConfigure_ParsesPlaceholderTTL(t *testing.T) {
	p := NewJWTValidation()
	raw := []byte(`{"issuer":"https://kc/realms/x","jwks_url":"https://kc/jwks","audience":"agent","placeholder_mode":true,"placeholder_ttl":"15m"}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if !p.cfg.PlaceholderMode {
		t.Fatal("placeholder_mode not parsed")
	}
	if p.placeholderTTL != 15*time.Minute {
		t.Fatalf("ttl = %v, want 15m", p.placeholderTTL)
	}
}

func TestConfigure_DefaultPlaceholderTTL(t *testing.T) {
	p := NewJWTValidation()
	raw := []byte(`{"issuer":"https://kc/realms/x","jwks_url":"https://kc/jwks","audience":"agent","placeholder_mode":true}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if p.placeholderTTL != time.Hour {
		t.Fatalf("default ttl = %v, want 1h", p.placeholderTTL)
	}
}

func _ctx() context.Context { return context.Background() }
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./authlib/plugins/jwtvalidation/ -run 'Mint|Placeholder' -v`
Expected: FAIL — unknown field `PlaceholderMode`, `placeholderTTL`, undefined `p.mint`.

- [ ] **Step 3: Add config fields**

In `authlib/plugins/jwtvalidation/plugin.go`, add to `jwtValidationConfig` (after the `BypassPaths` field, ~line 101):

```go
	PlaceholderMode bool   `json:"placeholder_mode" default:"false" description:"After validating the inbound token, replace it with an opaque placeholder before forwarding to the agent; the real token is held in the shared store for the outbound path to resolve. Requires a shared store and token-exchange resolve_placeholders downstream."`
	PlaceholderTTL  string `json:"placeholder_ttl" default:"1h" description:"How long the real token is retained for outbound resolution (Go duration, e.g. 30m). Default 1h."`
```

- [ ] **Step 4: Add the struct field and TTL parsing**

In the `JWTValidation` struct (lines 187-208), add a field:

```go
	placeholderTTL time.Duration
```

At the end of `Configure`, immediately before `return nil` (line 293), add:

```go
	p.placeholderTTL = time.Hour
	if c.PlaceholderTTL != "" {
		d, err := time.ParseDuration(c.PlaceholderTTL)
		if err != nil {
			return fmt.Errorf("jwt-validation: invalid placeholder_ttl %q: %w", c.PlaceholderTTL, err)
		}
		p.placeholderTTL = d
	}
```

Ensure `time` is imported in plugin.go (it is used elsewhere; if not, add to the import block). Add `"github.com/kagenti/kagenti-extensions/authbridge/authlib/placeholder"` and confirm `"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"` are imported (auth is already used for `p.inner`).

- [ ] **Step 5: Add the `mint` helper**

Add this method near `OnRequest` in plugin.go:

```go
// mint replaces the validated Authorization header with an opaque placeholder
// and stores the real bearer token in the shared store. Returns the handle,
// the stored token, and ok=false (fail closed) when no store is wired.
func (p *JWTValidation) mint(pctx *pipeline.Context) (handle, real string, ok bool) {
	if pctx.Shared == nil {
		return "", "", false
	}
	real = auth.ExtractBearer(pctx.Headers.Get("Authorization"))
	if real == "" {
		return "", "", false
	}
	h, err := placeholder.New()
	if err != nil {
		return "", "", false
	}
	pctx.Shared.Put(placeholder.Key(h), real, p.placeholderTTL)
	pctx.Headers.Set("Authorization", "Bearer "+h)
	return h, real, true
}
```

- [ ] **Step 6: Call `mint` from the allow path of `OnRequest`**

In `OnRequest`, after `pctx.Identity = claimsIdentity{c: result.Claims}` and the existing `pctx.Record(... ActionAllow ...)` block, but before `return pipeline.Action{Type: pipeline.Continue}` (after line 412), add:

```go
	if p.cfg.PlaceholderMode {
		handle, _, ok := p.mint(pctx)
		if !ok {
			pctx.Record(pipeline.Invocation{
				Action: pipeline.ActionDeny,
				Reason: "placeholder_mint_failed",
			})
			return pipeline.DenyStatus(503, "upstream.unreachable", "placeholder_mode requires a shared store")
		}
		pctx.Record(pipeline.Invocation{
			Action:  pipeline.ActionModify,
			Reason:  "placeholder_minted",
			Details: map[string]string{"handle_prefix": handle[:len(placeholder.Prefix)+6]},
		})
	}
```

(The `handle_prefix` records only the prefix plus 6 chars — never the full handle or token, per the security requirement.)

- [ ] **Step 7: Run tests to verify they pass**

Run: `go test ./authlib/plugins/jwtvalidation/ -v`
Expected: PASS (existing tests + new mint tests).

- [ ] **Step 8: Commit**

```bash
git add authlib/plugins/jwtvalidation/
git commit -s -m "feat(authbridge): add placeholder mint mode to jwt-validation"
```

---

## Task 5: token-exchange resolve step

**Files:**
- Modify: `authlib/plugins/tokenexchange/plugin.go` (config struct ~30-67; `OnRequest` 562-634)
- Test: `authlib/plugins/tokenexchange/placeholder_test.go`

- [ ] **Step 1: Write the failing tests**

Create `authlib/plugins/tokenexchange/placeholder_test.go`:

```go
package tokenexchange

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/placeholder"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/shared"
)

func resolveTestPlugin(t *testing.T, exchangeURL string) *TokenExchange {
	t.Helper()
	p := NewTokenExchange()
	raw := []byte(`{
	  "token_url":"` + exchangeURL + `",
	  "default_policy":"exchange",
	  "resolve_placeholders":true,
	  "identity":{"type":"client-secret","client_id":"agent","client_secret":"secret"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	return p
}

func exchangeStub(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "exchanged-token", "token_type": "Bearer", "expires_in": 300,
		})
	}))
}

// A valid handle on a matched route → resolved to the real token, exchanged.
func TestResolve_MatchedRouteExchanges(t *testing.T) {
	srv := exchangeStub(t)
	defer srv.Close()
	p := resolveTestPlugin(t, srv.URL)

	st := shared.New()
	handle, _ := placeholder.New()
	st.Put(placeholder.Key(handle), "real-user-token", time.Hour)

	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "target-svc",
		Headers:   http.Header{"Authorization": []string{"Bearer " + handle}},
		Shared:    st,
	}
	pctx.SetCurrentPlugin("token-exchange", pipeline.InvocationPhaseRequest)
	action := p.OnRequest(_ctx(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("action = %v, want Continue", action.Type)
	}
	if pctx.Headers.Get("Authorization") != "Bearer exchanged-token" {
		t.Fatalf("auth = %q, want Bearer exchanged-token", pctx.Headers.Get("Authorization"))
	}
}

// Unknown/expired handle on a matched route → deny (fail closed).
func TestResolve_MissDenies(t *testing.T) {
	srv := exchangeStub(t)
	defer srv.Close()
	p := resolveTestPlugin(t, srv.URL)

	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "target-svc",
		Headers:   http.Header{"Authorization": []string{"Bearer abph_unknownhandle"}},
		Shared:    shared.New(),
	}
	pctx.SetCurrentPlugin("token-exchange", pipeline.InvocationPhaseRequest)
	action := p.OnRequest(_ctx(), pctx)
	if action.Type != pipeline.Reject {
		t.Fatalf("action = %v, want Reject", action.Type)
	}
}

// A normal (non-placeholder) token is unaffected by resolve mode.
func TestResolve_NonPlaceholderPassThrough(t *testing.T) {
	srv := exchangeStub(t)
	defer srv.Close()
	p := resolveTestPlugin(t, srv.URL)

	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "target-svc",
		Headers:   http.Header{"Authorization": []string{"Bearer real-jwt"}},
		Shared:    shared.New(),
	}
	pctx.SetCurrentPlugin("token-exchange", pipeline.InvocationPhaseRequest)
	action := p.OnRequest(_ctx(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("action = %v, want Continue", action.Type)
	}
	if pctx.Headers.Get("Authorization") != "Bearer exchanged-token" {
		t.Fatalf("auth = %q, want Bearer exchanged-token (normal exchange)", pctx.Headers.Get("Authorization"))
	}
}

func _ctx() context.Context { return context.Background() }
```

Add `"context"` to the import block of this test file (and remove the duplicate `_ctx` if the package already defines one — if `go vet` reports a redeclaration, delete the local `_ctx` here and rely on the existing helper).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./authlib/plugins/tokenexchange/ -run TestResolve -v`
Expected: FAIL — unknown field `resolve_placeholders`.

- [ ] **Step 3: Add config field**

In `authlib/plugins/tokenexchange/plugin.go`, add to `tokenExchangeConfig` (after `AudienceFromHost`, ~line 67):

```go
	ResolvePlaceholders bool `json:"resolve_placeholders" default:"false" description:"Resolve an inbound bearer carrying the placeholder prefix from the shared store to the real token before exchange. Unresolvable placeholders are denied (fail closed)."`
```

- [ ] **Step 4: Add the resolve step to `OnRequest`**

Add the placeholder import: `"github.com/kagenti/kagenti-extensions/authbridge/authlib/placeholder"`.

In `OnRequest`, replace the opening lines:

```go
	authHeader := pctx.Headers.Get("Authorization")
	host := pctx.Host

	result := p.inner.HandleOutbound(ctx, authHeader, host)
```

with:

```go
	authHeader := pctx.Headers.Get("Authorization")
	host := pctx.Host

	if p.cfg.ResolvePlaceholders && placeholder.IsPlaceholder(auth.ExtractBearer(authHeader)) {
		real, ok := resolvePlaceholder(pctx, auth.ExtractBearer(authHeader))
		if !ok {
			return pctx.DenyAndRecord("placeholder_unresolved", "auth.unauthorized", "unresolvable credential placeholder")
		}
		authHeader = "Bearer " + real
	}

	result := p.inner.HandleOutbound(ctx, authHeader, host)
```

Add the helper near `OnRequest`:

```go
// resolvePlaceholder looks a handle up in the shared store. Returns ok=false
// (fail closed) when no store is wired or the handle is unknown/expired.
func resolvePlaceholder(pctx *pipeline.Context, handle string) (string, bool) {
	if pctx.Shared == nil {
		return "", false
	}
	v, ok := pctx.Shared.Get(placeholder.Key(handle))
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}
```

(`auth` is already imported — `auth.ExtractBearer` is used in the listeners and `p.inner` is `*auth.Auth`. Confirm the import is present in plugin.go; if not, add `"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"`.)

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./authlib/plugins/tokenexchange/ -v`
Expected: PASS (existing tests + 3 new resolve tests).

- [ ] **Step 6: Commit**

```bash
git add authlib/plugins/tokenexchange/
git commit -s -m "feat(authbridge): add placeholder resolve step to token-exchange"
```

---

## Task 6: reverseproxy inbound Authorization propagation

**Files:**
- Modify: `authlib/listener/reverseproxy/server.go` (`Server` struct 51-64; `handleRequest` 164-254)
- Test: `authlib/listener/reverseproxy/placeholder_test.go`

- [ ] **Step 1: Write the failing test**

Create `authlib/listener/reverseproxy/placeholder_test.go`:

```go
package reverseproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// rewritePlugin sets a new Authorization on the inbound context, simulating
// jwt-validation mint mode.
type rewritePlugin struct{}

func (rewritePlugin) Name() string                              { return "rewrite" }
func (rewritePlugin) Capabilities() pipeline.PluginCapabilities { return pipeline.PluginCapabilities{} }
func (rewritePlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	pctx.Headers.Set("Authorization", "Bearer abph_minted")
	return pipeline.Action{Type: pipeline.Continue}
}
func (rewritePlugin) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

func TestInboundPropagation_RewrittenAuthReachesBackend(t *testing.T) {
	var seen string
	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
	}))
	defer backend.Close()

	holder := pipeline.NewHolder([]pipeline.Plugin{rewritePlugin{}})
	srv, err := NewServer(holder, nil, backend.URL, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://agent.local/work", nil)
	req.Header.Set("Authorization", "Bearer real-user-token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if seen != "Bearer abph_minted" {
		t.Fatalf("backend saw Authorization=%q, want Bearer abph_minted", seen)
	}
}
```

Note: confirm the exact constructor for a `*pipeline.Holder` from a plugin slice (look for `NewHolder` or equivalent in `authlib/pipeline/`); if the name differs, use the existing helper the other listener tests use. Confirm `srv.ServeHTTP` is the handler entry (or use `srv.Handler()` / the method the other reverseproxy tests call).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./authlib/listener/reverseproxy/ -run TestInboundPropagation -v`
Expected: FAIL — backend sees `Bearer real-user-token` (mutation dropped).

- [ ] **Step 3: Add the `Shared` field**

In the `Server` struct (lines 51-64), add:

```go
	Shared pipeline.SharedStore // process-scoped store; set by main, may be nil
```

In the `pctx` literal at the top of `handleRequest` (lines 165-173), add the field:

```go
		Shared:    s.Shared,
```

- [ ] **Step 4: Propagate the inbound Authorization change**

In `handleRequest`, capture the original Authorization just before `action := s.InboundPipeline.Run(...)` (line 210):

```go
	originalAuth := pctx.Headers.Get("Authorization")
	action := s.InboundPipeline.Run(r.Context(), pctx)
```

Then, after the `Reject` handling and the `pctx.BodyMutated()` block (after line 225), before the session-recording block, add:

```go
	if newAuth := pctx.Headers.Get("Authorization"); newAuth != originalAuth {
		r.Header.Set("Authorization", newAuth)
	}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./authlib/listener/reverseproxy/ -v`
Expected: PASS (existing + new propagation test).

- [ ] **Step 6: Commit**

```bash
git add authlib/listener/reverseproxy/
git commit -s -m "feat(authbridge): propagate inbound Authorization mutation in reverseproxy"
```

---

## Task 7: forwardproxy Shared field

**Files:**
- Modify: `authlib/listener/forwardproxy/server.go` (`Server` struct 32-36; pctx build ~159)

- [ ] **Step 1: Add the field and set it on pctx**

In the `Server` struct (lines 32-36), add:

```go
	Shared pipeline.SharedStore // process-scoped store; set by main, may be nil
```

In the `pctx` literal in `handleRequest` (lines 159-167), add:

```go
		Shared:    s.Shared,
```

- [ ] **Step 2: Verify the package still builds and tests pass**

Run: `go test ./authlib/listener/forwardproxy/ -v`
Expected: PASS (no behavior change; the field is now available to outbound plugins).

- [ ] **Step 3: Commit**

```bash
git add authlib/listener/forwardproxy/
git commit -s -m "feat(authbridge): expose shared store on forwardproxy outbound context"
```

---

## Task 8: extproc Shared field + inbound propagation

**Files:**
- Modify: `authlib/listener/extproc/server.go` (`Server` 35-40; `handleInbound` 143-163; `handleInboundBody` 165-185; `handleOutbound` 447-483 and `handleOutboundBody` 485-521 pctx literals)
- Test: `authlib/listener/extproc/placeholder_test.go`

- [ ] **Step 1: Write the failing test**

Create `authlib/listener/extproc/placeholder_test.go`:

```go
package extproc

import (
	"context"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

type mintPlugin struct{}

func (mintPlugin) Name() string                              { return "mint" }
func (mintPlugin) Capabilities() pipeline.PluginCapabilities { return pipeline.PluginCapabilities{} }
func (mintPlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	pctx.Headers.Set("Authorization", "Bearer abph_minted")
	return pipeline.Action{Type: pipeline.Continue}
}
func (mintPlugin) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// On the inbound path, a rewritten Authorization must be emitted to Envoy as a
// header mutation so it reaches the agent.
func TestExtprocInbound_EmitsAuthMutation(t *testing.T) {
	s := &Server{InboundPipeline: pipeline.NewHolder([]pipeline.Plugin{mintPlugin{}})}
	headers := &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
		{Key: ":path", Value: "/work"},
		{Key: "authorization", Value: "Bearer real-user-token"},
	}}
	resp, _ := s.handleInbound(fakeStream(t), headers, nil)

	got := authMutationValue(t, resp)
	if got != "Bearer abph_minted" {
		t.Fatalf("emitted authorization = %q, want Bearer abph_minted", got)
	}
}
```

Implement the two test helpers (`fakeStream` returning a minimal `extprocv3.ExternalProcessor_ProcessServer` whose `Context()` returns `context.Background()`, and `authMutationValue` which walks `resp.GetRequestHeaders().GetResponse().GetHeaderMutation().GetSetHeaders()` for the `authorization` key) by copying the pattern from the existing extproc tests — check `authlib/listener/extproc/server_test.go` for an existing fake stream and mutation-reading helper and reuse them rather than reinventing.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./authlib/listener/extproc/ -run TestExtprocInbound -v`
Expected: FAIL — `handleInbound` returns `allowResponse()` with no auth mutation.

- [ ] **Step 3: Add the `Shared` field**

In the `Server` struct (lines 35-40), add:

```go
	Shared pipeline.SharedStore // process-scoped store; set by main, may be nil
```

Add `Shared: s.Shared,` to the `pctx` literal in all four handlers: `handleInbound` (line 145), `handleInboundBody` (line 167), `handleOutbound` (line 449), `handleOutboundBody` (line 487).

- [ ] **Step 4: Emit the inbound auth mutation**

In `handleInbound`, capture the original Authorization before `action := s.InboundPipeline.Run(...)` (line 155) and emit on change before the existing `return allowResponse(), pctx`:

```go
	originalAuth := pctx.Headers.Get("Authorization")
	action := s.InboundPipeline.Run(ctx, pctx)
	if action.Type == pipeline.Reject {
		s.recordInboundReject(pctx, action)
		return rejectFromAction(action), nil
	}

	s.recordInboundSession(pctx)

	if newAuth := pctx.Headers.Get("Authorization"); newAuth != originalAuth {
		return replaceTokenResponse(auth.ExtractBearer(newAuth)), pctx
	}
	return allowResponse(), pctx
```

Apply the same change in `handleInboundBody`, but emit via the body variant to preserve body handling:

```go
	originalAuth := pctx.Headers.Get("Authorization")
	action := s.InboundPipeline.Run(ctx, pctx)
	if action.Type == pipeline.Reject {
		s.recordInboundReject(pctx, action)
		return rejectFromAction(action), nil
	}

	s.recordInboundSession(pctx)

	if newAuth := pctx.Headers.Get("Authorization"); newAuth != originalAuth {
		return withBodyMutation(replaceTokenBodyResponse(auth.ExtractBearer(newAuth)), pctx), pctx
	}
	return withBodyMutation(allowBodyResponse(), pctx), pctx
```

(`auth`, `replaceTokenResponse`, `replaceTokenBodyResponse`, `withBodyMutation`, `allowBodyResponse` are all already used in this file.)

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./authlib/listener/extproc/ -v`
Expected: PASS (existing + new inbound mutation test).

- [ ] **Step 6: Commit**

```bash
git add authlib/listener/extproc/
git commit -s -m "feat(authbridge): propagate inbound Authorization mutation in extproc"
```

---

## Task 9: Wire the shared store in main

**Files:**
- Modify: `cmd/authbridge-proxy/main.go` (~lines 246-250), `cmd/authbridge-lite/main.go` (~lines 232-241), `cmd/authbridge-envoy/main.go` (`startGRPCExtProc` 299-320 + call site ~216)

- [ ] **Step 1: authbridge-proxy — create and inject the store**

In `cmd/authbridge-proxy/main.go`, immediately after the `rpSrv, err := reverseproxy.NewServer(...)` / `fpSrv, err := forwardproxy.NewServer(...)` blocks (after line 253) and before the `httpServers = append(...)` lines, add:

```go
	sharedStore := shared.New()
	rpSrv.Shared = sharedStore
	fpSrv.Shared = sharedStore
```

Add the import `"github.com/kagenti/kagenti-extensions/authbridge/authlib/shared"`.

- [ ] **Step 2: authbridge-lite — same injection**

In `cmd/authbridge-lite/main.go`, after the `rpSrv`/`fpSrv` construction (after line ~239), add the identical three lines and the `shared` import:

```go
	sharedStore := shared.New()
	rpSrv.Shared = sharedStore
	fpSrv.Shared = sharedStore
```

- [ ] **Step 3: authbridge-envoy — inject into the extproc.Server literal**

In `cmd/authbridge-envoy/main.go`, change `startGRPCExtProc` to take and set the store. Update the signature and the struct literal (lines 299-305):

```go
func startGRPCExtProc(inbound, outbound *pipeline.Holder, sessions *session.Store, store pipeline.SharedStore, addr string) *grpc.Server {
	srv := grpc.NewServer()
	extprocv3.RegisterExternalProcessorServer(srv, &extproc.Server{
		InboundPipeline:  inbound,
		OutboundPipeline: outbound,
		Sessions:         sessions,
		Shared:           store,
	})
```

Update the call site (line ~216):

```go
	grpcServers = append(grpcServers, startGRPCExtProc(inboundH, outboundH, sessions, shared.New(), cfg.Listener.ExtProcAddr))
```

Add the imports `"github.com/kagenti/kagenti-extensions/authbridge/authlib/shared"` and (if not present) `"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"`.

- [ ] **Step 4: Build everything**

Run: `go build ./...`
Expected: builds clean, no errors.

- [ ] **Step 5: Run the full test suite**

Run: `go test ./... 2>&1 | tail -30`
Expected: all packages PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/authbridge-proxy/main.go cmd/authbridge-lite/main.go cmd/authbridge-envoy/main.go
git commit -s -m "feat(authbridge): wire shared placeholder store into proxy, lite, and envoy"
```

---

## Task 10: Documentation

**Files:**
- Modify: `authbridge/docs/plugin-reference.md`

- [ ] **Step 1: Document the mode**

In `authbridge/docs/plugin-reference.md`, under the `jwt-validation` and `token-exchange` sections, add the new config fields and a short "Credential placeholder swap" subsection explaining: the two flags are a matched pair (mint without resolve → fail-closed deny; resolve without mint → no-op); passthrough hosts receive the placeholder, so hosts needing a real credential must be exchange routes; the store is in-memory and single-process (sidecar / single-replica extproc), with the external store as the multi-replica/waypoint follow-on. Link to the design spec `docs/superpowers/specs/2026-06-02-credential-placeholder-swap-design.md`.

- [ ] **Step 2: Commit**

```bash
git add authbridge/docs/plugin-reference.md
git commit -s -m "docs(authbridge): document credential placeholder swap mode"
```

---

## Final verification

- [ ] **Run the full suite with race detection**

Run: `go test ./... -race 2>&1 | tail -30`
Expected: all PASS, no races.

- [ ] **Lint**

Run: `golangci-lint run`
Expected: no new findings.

- [ ] **Manual smoke (optional, once deployed):** with `placeholder_mode: true` (jwt-validation) and `resolve_placeholders: true` (token-exchange), confirm via session events / logs that (a) the agent receives `Bearer abph_…`, (b) an exchange-route upstream receives the exchanged token, (c) an unknown/expired handle on an exchange route is denied.

---

## Spec coverage check

| Spec section | Task |
|--------------|------|
| Shared store (Layer 1) | 1 |
| Placeholder semantics (Layer 2) | 2 |
| `SharedStore` interface + `Context.Shared` | 3 |
| jwt-validation mint + config | 4 |
| token-exchange resolve + config | 5 |
| Inbound propagation: reverseproxy | 6 |
| forwardproxy store exposure | 7 |
| Inbound propagation: extproc | 8 |
| `main` wiring (proxy/lite/envoy) | 9 |
| Docs | 10 |
| Fail-closed branches (D, E) | 4 (mint nil-store), 5 (resolve miss) |
| Route-gating safety | 5 (design note: header only written on ActionReplaceToken) |
| External store / extauthz / waypoint | Out of scope (noted) |

## Attribution

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
