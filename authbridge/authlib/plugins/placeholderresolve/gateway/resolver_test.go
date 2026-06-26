// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/openshell"
)

// TestNextDelay exercises the refresh schedule: a short retry until primed,
// then a per-credential-expiry-derived delay clamped to [min, refreshInterval].
func TestNextDelay(t *testing.T) {
	r := &Resolver{refreshInterval: defaultRefreshInterval}

	// Not primed yet -> retry quickly.
	if d := r.nextDelay(); d != retryInterval {
		t.Errorf("unready nextDelay = %v, want %v", d, retryInterval)
	}

	r.ready.Store(true)

	// Primed, no expiry -> default refresh interval.
	r.env.Store(&openshell.Environment{Values: map[string]string{"K": "v"}})
	if d := r.nextDelay(); d != r.refreshInterval {
		t.Errorf("no-expiry nextDelay = %v, want %v", d, r.refreshInterval)
	}

	// Soonest expiry very near -> clamped up to minRefreshInterval.
	r.env.Store(&openshell.Environment{
		ExpiresAtMs: map[string]int64{"K": time.Now().Add(10 * time.Second).UnixMilli()},
	})
	if d := r.nextDelay(); d != minRefreshInterval {
		t.Errorf("near-expiry nextDelay = %v, want %v (clamped to min)", d, minRefreshInterval)
	}

	// Soonest expiry far out -> clamped down to refreshInterval.
	r.env.Store(&openshell.Environment{
		ExpiresAtMs: map[string]int64{"K": time.Now().Add(time.Hour).UnixMilli()},
	})
	if d := r.nextDelay(); d != r.refreshInterval {
		t.Errorf("far-expiry nextDelay = %v, want %v (clamped to refresh)", d, r.refreshInterval)
	}
}

// TestResolveStripsRevisionPrefix covers the bug fix: OpenShell injects
// "v<rev>_<NAME>" placeholders but the gateway returns the env keyed by the bare
// <NAME>, so the resolver must strip a single "v<digits>_" prefix on a miss.
func TestResolveStripsRevisionPrefix(t *testing.T) {
	ctx := context.Background()

	// Primed cache keyed by the bare name (as the gateway returns it).
	r := &Resolver{}
	r.env.Store(&openshell.Environment{
		Values: map[string]string{"ANTHROPIC_AUTH_TOKEN": "sk-real"},
	})

	cases := []struct {
		name   string
		key    string
		want   string
		wantOK bool
	}{
		{"revision-keyed resolves to bare value", "v7_ANTHROPIC_AUTH_TOKEN", "sk-real", true},
		{"bare key still resolves", "ANTHROPIC_AUTH_TOKEN", "sk-real", true},
		{"revision-keyed but absent fails closed", "v7_MISSING", "", false},
		{"non-numeric revision is not stripped", "vX_ANTHROPIC_AUTH_TOKEN", "", false},
		{"empty revision is not stripped", "v_ANTHROPIC_AUTH_TOKEN", "", false},
		{"empty key after prefix fails closed", "v7_", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := r.Resolve(ctx, tc.key)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("Resolve(%q) = (%q, %v), want (%q, %v)", tc.key, got, ok, tc.want, tc.wantOK)
			}
		})
	}

	// A literal key matching the revision shape wins over the bare fallback.
	t.Run("literal key takes precedence over strip", func(t *testing.T) {
		lit := &Resolver{}
		lit.env.Store(&openshell.Environment{
			Values: map[string]string{"v2_FOO": "literal", "FOO": "bare"},
		})
		if got, ok := lit.Resolve(ctx, "v2_FOO"); got != "literal" || !ok {
			t.Errorf("Resolve(v2_FOO) = (%q, %v), want (literal, true)", got, ok)
		}
	})

	// Expiry is checked against the bare key on the stripped path.
	t.Run("expired credential on stripped key fails closed", func(t *testing.T) {
		exp := &Resolver{}
		exp.env.Store(&openshell.Environment{
			Values:      map[string]string{"TOK": "x"},
			ExpiresAtMs: map[string]int64{"TOK": time.Now().Add(-time.Minute).UnixMilli()},
		})
		if got, ok := exp.Resolve(ctx, "v3_TOK"); ok {
			t.Errorf("Resolve(v3_TOK) = (%q, %v), want fail-closed", got, ok)
		}
	})

	// Cold cache fails closed regardless of key shape.
	t.Run("cold cache fails closed", func(t *testing.T) {
		cold := &Resolver{}
		if got, ok := cold.Resolve(ctx, "v1_ANTHROPIC_AUTH_TOKEN"); ok {
			t.Errorf("Resolve on cold cache = (%q, %v), want fail-closed", got, ok)
		}
	})
}
