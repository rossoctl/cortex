package jwtvalidation

import (
	"net/http"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/placeholder"
	"github.com/rossoctl/cortex/authbridge/authlib/shared"
)

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
