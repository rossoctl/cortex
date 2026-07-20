package jwtvalidation

import (
	"reflect"
	"testing"
	"time"

	"github.com/rossoctl/rossocortex/authbridge/authlib/contracts"
	"github.com/rossoctl/rossocortex/authbridge/authlib/plugins/jwtvalidation/validation"
)

func TestClaimsIdentity_BasicAccessors(t *testing.T) {
	id := claimsIdentity{c: &validation.Claims{
		Subject:  "alice",
		ClientID: "agent-x",
		Scopes:   []string{"hr.read", "hr.write"},
	}}
	if id.Subject() != "alice" {
		t.Errorf("Subject = %q", id.Subject())
	}
	if id.ClientID() != "agent-x" {
		t.Errorf("ClientID = %q", id.ClientID())
	}
	if !reflect.DeepEqual(id.Scopes(), []string{"hr.read", "hr.write"}) {
		t.Errorf("Scopes = %v", id.Scopes())
	}
}

func TestClaimsIdentity_ClaimsCarrier(t *testing.T) {
	// Compile-time + runtime: the adapter is a ClaimsCarrier.
	var cc contracts.ClaimsCarrier = claimsIdentity{c: &validation.Claims{
		Subject:   "alice",
		Issuer:    "https://kc/realms/rossoctl",
		Audience:  []string{"hr-agent", "github-tool"},
		ExpiresAt: time.Unix(1893456000, 0),
	}}

	if cc.AuthMethod() != "jwt" {
		t.Errorf("AuthMethod = %q, want jwt", cc.AuthMethod())
	}
	if cc.Issuer() != "https://kc/realms/rossoctl" {
		t.Errorf("Issuer = %q", cc.Issuer())
	}
	if !reflect.DeepEqual(cc.Audience(), []string{"hr-agent", "github-tool"}) {
		t.Errorf("Audience = %v", cc.Audience())
	}

	claims := cc.Claims()
	want := map[string]string{
		"issuer":   "https://kc/realms/rossoctl",
		"audience": "hr-agent,github-tool",
		"exp":      "1893456000",
	}
	if !reflect.DeepEqual(claims, want) {
		t.Errorf("Claims = %v, want %v", claims, want)
	}
}

func TestClaimsIdentity_CuratedClaimsOmitsEmpty(t *testing.T) {
	// Only subject present — Claims() returns nil rather than a map of
	// empty strings, and never leaks a raw Extra map.
	id := claimsIdentity{c: &validation.Claims{
		Subject: "bob",
		Extra:   map[string]any{"secret": "do-not-forward"},
	}}
	if got := id.Claims(); got != nil {
		t.Fatalf("Claims with no issuer/aud/exp should be nil, got %v", got)
	}
}
