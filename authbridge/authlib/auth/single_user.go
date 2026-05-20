package auth

import (
	"sync"
	"sync/atomic"
	"time"
)

// Process-level cache for the most recent validated inbound user JWT.
// Bridges inbound (jwt-validation captures) → outbound (token-exchange
// reads when the agent didn't propagate the Authorization header).
//
// The whole feature is gated on a single atomic.Bool — set once at
// startup from the top-level Config.SingleUserMode by the binary's
// main.go. Plugins call Store/Get unconditionally; when disabled,
// Store is a no-op and Get returns ok=false. This keeps the operator
// control surface to one knob and the plugin code branch-free.
//
// Safe only when the process handles one user at a time. Multi-tenant
// concurrent agents need correlation-ID-based routing — the helpers
// here will last-write-wins, and jwt-validation logs a WARN whenever
// the cached subject changes so operators have a tripwire.
//
// Each replica has its own cache. Cluster-scaled deployments need
// stickiness or a future shared-state implementation.

const (
	singleUserMaxTTL = 5 * time.Minute   // cap regardless of token exp
	singleUserMinTTL = 30 * time.Second  // skip capture below this floor
)

var (
	// spEnabled gates the entire feature. Default-true: out-of-the-box
	// behavior is to bridge inbound→outbound. Operators opt out by
	// setting `single_user_mode: false` at the top level of the runtime
	// YAML; the binary then calls SetSingleUserModeEnabled(false) at
	// startup. The atomic.Bool zero value is false, so we explicitly
	// initialize to true in init() to match the documented default.
	spEnabled atomic.Bool

	spMu      sync.RWMutex
	spToken   string
	spSubject string
	spExp     time.Time
)

func init() {
	spEnabled.Store(true)
}

// SetSingleUserModeEnabled toggles the inbound→outbound bridge. Called
// once at startup by the binary's main.go from Config.SingleUserModeEnabled().
// Tests use this to flip the feature on/off without restarting.
func SetSingleUserModeEnabled(enabled bool) {
	spEnabled.Store(enabled)
}

// IsSingleUserModeEnabled reports whether the bridge is on. Exported
// for tests and for the binary's startup banner.
func IsSingleUserModeEnabled() bool {
	return spEnabled.Load()
}

// StoreSingleUserToken caches the most recent validated inbound token.
// Returns the previous subject so callers can detect when the
// single-player assumption is violated (subject changed between
// captures); empty string when there was no prior entry, or when the
// feature is disabled (no-op).
//
// Caller is responsible for applying TTL caps before calling — see
// CapSingleUserExpiry which encapsulates the floor/cap policy.
func StoreSingleUserToken(token, subject string, exp time.Time) (prevSubject string) {
	if !spEnabled.Load() {
		return ""
	}
	spMu.Lock()
	defer spMu.Unlock()
	prevSubject = spSubject
	spToken = token
	spSubject = subject
	spExp = exp
	return
}

// GetSingleUserToken returns the cached token if non-empty and not yet
// expired at `now`. Cache miss returns ok=false with empty strings.
// When the feature is disabled, always returns ok=false.
func GetSingleUserToken(now time.Time) (token, subject string, ok bool) {
	if !spEnabled.Load() {
		return "", "", false
	}
	spMu.RLock()
	defer spMu.RUnlock()
	if spToken == "" || now.After(spExp) {
		return "", "", false
	}
	return spToken, spSubject, true
}

// CapSingleUserExpiry applies the package's cap/floor policy to a
// proposed cache expiry. Returns (capped, true) when exp is at or after
// now+MinTTL; (zero, false) when below the floor (caller should skip
// capture). The capped value is min(exp, now+MaxTTL).
func CapSingleUserExpiry(now, exp time.Time) (time.Time, bool) {
	if exp.Before(now.Add(singleUserMinTTL)) {
		return time.Time{}, false
	}
	if cap := now.Add(singleUserMaxTTL); exp.After(cap) {
		return cap, true
	}
	return exp, true
}

// ResetSingleUserToken clears the cache. Exported for tests; pair with
// t.Cleanup(auth.ResetSingleUserToken) to avoid cross-test leakage.
// Does not change the enabled bit — tests that toggle that should
// restore it explicitly.
func ResetSingleUserToken() {
	spMu.Lock()
	defer spMu.Unlock()
	spToken = ""
	spSubject = ""
	spExp = time.Time{}
}
