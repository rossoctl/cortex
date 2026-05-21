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

	// spSubjectChanges counts how many times Store observed a NEW
	// subject overwriting an existing entry — the operator's
	// programmatic tripwire for "single-player mode is being used in a
	// multi-user context." Increments are atomic and lock-free; the
	// WARN log emitted by jwt-validation is a human-readable
	// counterpart but is unsuitable for alerting at high volume. Read
	// via SingleUserSubjectChangeCount; alerts should track the delta.
	spSubjectChanges atomic.Int64
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
//
// Side-effect: increments the package-level subject-change counter
// when prevSubject is non-empty AND differs from the new subject,
// surfacing the single-player-violation signal to /stats consumers
// and Prometheus exporters via SingleUserSubjectChangeCount.
//
// The spEnabled gate is checked outside the mutex deliberately. The
// gate is set once at startup by the binary; SetSingleUserModeEnabled
// is otherwise only called from tests. The microsecond window in
// which a concurrent SetSingleUserModeEnabled(false) could race a
// Store-in-flight is harmless — the worst case is one stale write
// before the next Get returns a miss anyway.
func StoreSingleUserToken(token, subject string, exp time.Time) (prevSubject string) {
	if !spEnabled.Load() {
		return ""
	}
	spMu.Lock()
	defer spMu.Unlock()
	prevSubject = spSubject
	if prevSubject != "" && prevSubject != subject {
		spSubjectChanges.Add(1)
	}
	spToken = token
	spSubject = subject
	spExp = exp
	return
}

// GetSingleUserToken returns the cached token if non-empty and not yet
// expired at `now`. Cache miss returns ok=false with empty strings.
// When the feature is disabled, always returns ok=false.
//
// See StoreSingleUserToken for the rationale on checking spEnabled
// outside the mutex.
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

// SingleUserSubjectChangeCount returns the lifetime count of times
// Store observed a new subject overwriting a different existing
// subject — i.e., the number of times the single-player assumption
// was violated by concurrent multi-user traffic. Operators should
// alert on the DELTA of this counter (not the absolute value); a
// nonzero rate indicates single-player mode is misapplied to a
// multi-user workload.
//
// Lifetime counter: not reset by ResetSingleUserToken. Always returns
// the live atomic value; safe to call concurrently.
func SingleUserSubjectChangeCount() int64 {
	return spSubjectChanges.Load()
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

// ResetSingleUserToken clears the cache. TEST-ONLY entry point —
// production code MUST NOT call this. Exported (rather than hidden in
// an export_test.go) because external test packages such as
// jwtvalidation_test, tokenexchange_test, and the plugins-level e2e
// suite need to reset state across tests, and Go's _test.go visibility
// scope wouldn't reach them.
//
// Pair with t.Cleanup(auth.ResetSingleUserToken) to avoid cross-test
// leakage. Does NOT change the enabled bit and does NOT reset the
// subject-change counter — tests that depend on either should restore
// them explicitly.
//
// If a runtime "wipe the cache mid-flight" capability is ever needed,
// add a separately-named function (e.g., InvalidateSingleUserToken)
// rather than overloading this one — the test-only contract here is
// load-bearing.
func ResetSingleUserToken() {
	spMu.Lock()
	defer spMu.Unlock()
	spToken = ""
	spSubject = ""
	spExp = time.Time{}
}
