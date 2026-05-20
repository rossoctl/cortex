package auth

import (
	"sync"
	"testing"
	"time"
)

// Helper: future-dated expiry that's safely above the MinTTL floor and
// below the MaxTTL cap, so the happy-path tests don't accidentally
// trigger floor/cap policy.
func futureExp(t *testing.T) time.Time {
	t.Helper()
	return time.Now().Add(2 * time.Minute)
}

// withEnabled flips the package-level feature gate for a test and
// restores the prior state on cleanup. Tests assume the feature is
// enabled by default (matching the package init), but explicit gating
// makes the intent visible.
func withEnabled(t *testing.T, enabled bool) {
	t.Helper()
	prev := IsSingleUserModeEnabled()
	SetSingleUserModeEnabled(enabled)
	t.Cleanup(func() { SetSingleUserModeEnabled(prev) })
}

func TestSingleUserMode_DefaultEnabled(t *testing.T) {
	// Package init() must set the gate to true so out-of-the-box
	// behavior bridges inbound→outbound. Validate that here so a future
	// regression flips it without anyone noticing.
	if !IsSingleUserModeEnabled() {
		t.Fatal("expected single-user mode enabled by default at init time")
	}
}

func TestStoreSingleUserToken_RoundTrip(t *testing.T) {
	withEnabled(t, true)
	t.Cleanup(ResetSingleUserToken)

	exp := futureExp(t)
	prev := StoreSingleUserToken("tok-1", "alice", exp)
	if prev != "" {
		t.Errorf("prev subject on first store = %q, want empty", prev)
	}

	tok, sub, ok := GetSingleUserToken(time.Now())
	if !ok {
		t.Fatalf("GetSingleUserToken: ok = false, want true")
	}
	if tok != "tok-1" {
		t.Errorf("token = %q, want tok-1", tok)
	}
	if sub != "alice" {
		t.Errorf("subject = %q, want alice", sub)
	}
}

func TestSingleUserMode_DisabledIsNoOp(t *testing.T) {
	t.Cleanup(ResetSingleUserToken)

	withEnabled(t, false)

	// Store should no-op; prevSubject empty.
	if got := StoreSingleUserToken("tok", "alice", futureExp(t)); got != "" {
		t.Errorf("StoreSingleUserToken when disabled: prev = %q, want empty (no-op)", got)
	}

	// Get should report miss.
	if _, _, ok := GetSingleUserToken(time.Now()); ok {
		t.Error("GetSingleUserToken when disabled: ok = true, want false")
	}

	// Re-enable: cache should still be empty (Store was a no-op, not a
	// queued write).
	SetSingleUserModeEnabled(true)
	if _, _, ok := GetSingleUserToken(time.Now()); ok {
		t.Error("after re-enable: cache should be empty (Store was no-op while disabled)")
	}
}

func TestGetSingleUserToken_ExpiredReturnsMiss(t *testing.T) {
	t.Cleanup(ResetSingleUserToken)

	past := time.Now().Add(-1 * time.Second)
	StoreSingleUserToken("tok-1", "alice", past)

	tok, sub, ok := GetSingleUserToken(time.Now())
	if ok {
		t.Errorf("ok = true, want false (token expired)")
	}
	if tok != "" || sub != "" {
		t.Errorf("expected empty token/subject on miss, got %q / %q", tok, sub)
	}
}

func TestGetSingleUserToken_EmptyCacheReturnsMiss(t *testing.T) {
	t.Cleanup(ResetSingleUserToken)

	tok, sub, ok := GetSingleUserToken(time.Now())
	if ok {
		t.Errorf("ok = true, want false (cache empty)")
	}
	if tok != "" || sub != "" {
		t.Errorf("expected empty token/subject, got %q / %q", tok, sub)
	}
}

func TestStoreSingleUserToken_PrevSubjectOnOverwrite(t *testing.T) {
	t.Cleanup(ResetSingleUserToken)

	exp := futureExp(t)
	StoreSingleUserToken("tok-1", "alice", exp)

	prev := StoreSingleUserToken("tok-2", "bob", exp)
	if prev != "alice" {
		t.Errorf("prev subject on overwrite = %q, want alice", prev)
	}

	prev2 := StoreSingleUserToken("tok-3", "bob", exp)
	if prev2 != "bob" {
		t.Errorf("prev subject on same-subject overwrite = %q, want bob", prev2)
	}
}

func TestCapSingleUserExpiry(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name      string
		exp       time.Time
		wantOK    bool
		wantCap   bool // true if returned exp should be capped at now+MaxTTL
		wantPass  bool // true if returned exp should equal input exp
	}{
		{
			name:     "below floor returns false",
			exp:      now.Add(10 * time.Second), // < MinTTL (30s)
			wantOK:   false,
		},
		{
			name:     "exactly at floor returns true",
			exp:      now.Add(singleUserMinTTL),
			wantOK:   true,
			wantPass: true,
		},
		{
			name:     "above floor below cap passes through",
			exp:      now.Add(2 * time.Minute),
			wantOK:   true,
			wantPass: true,
		},
		{
			name:    "above cap is capped",
			exp:     now.Add(1 * time.Hour),
			wantOK:  true,
			wantCap: true,
		},
		{
			name:   "in the past returns false",
			exp:    now.Add(-1 * time.Second),
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := CapSingleUserExpiry(now, tt.exp)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				if !got.IsZero() {
					t.Errorf("expected zero time on false, got %v", got)
				}
				return
			}
			if tt.wantCap {
				want := now.Add(singleUserMaxTTL)
				if !got.Equal(want) {
					t.Errorf("capped exp = %v, want %v", got, want)
				}
			}
			if tt.wantPass {
				if !got.Equal(tt.exp) {
					t.Errorf("passthrough exp = %v, want %v", got, tt.exp)
				}
			}
		})
	}
}

func TestResetSingleUserToken(t *testing.T) {
	exp := futureExp(t)
	StoreSingleUserToken("tok-1", "alice", exp)

	ResetSingleUserToken()

	_, _, ok := GetSingleUserToken(time.Now())
	if ok {
		t.Errorf("ok = true after reset, want false")
	}
}

func TestSingleUserToken_ConcurrentSafe(t *testing.T) {
	t.Cleanup(ResetSingleUserToken)

	const goroutines = 50
	const iterations = 100

	exp := futureExp(t)

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	// Writers
	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				StoreSingleUserToken("tok", "user", exp)
			}
		}(i)
	}

	// Readers
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				_, _, _ = GetSingleUserToken(time.Now())
			}
		}()
	}

	wg.Wait()

	// Final state should still be readable.
	tok, sub, ok := GetSingleUserToken(time.Now())
	if !ok || tok != "tok" || sub != "user" {
		t.Errorf("post-concurrency state = (%q, %q, %v), want (tok, user, true)", tok, sub, ok)
	}
}
