// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
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
