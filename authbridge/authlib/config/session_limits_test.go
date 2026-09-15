package config

import (
	"slices"
	"testing"
	"time"
)

// Limits is the one place the session store's parameters are resolved, so this is
// where "max_events passes through unmodified" becomes assertable. It was not before:
// the same twenty lines lived in three main packages, where nothing reachable by a test
// could see them.
func TestSessionLimits(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   SessionConfig
		want SessionLimits
	}{
		{
			// The default that matters most: an operator who has not set max_events gets
			// a store that never evicts an event from a live session.
			name: "unset",
			want: SessionLimits{TTL: 0, MaxEvents: 0, MaxSessions: 100},
		},
		{
			name: "max_events set is honoured verbatim",
			in:   SessionConfig{MaxEvents: 250},
			want: SessionLimits{MaxEvents: 250, MaxSessions: 100},
		},
		{
			// Negative used to fall back to 500. It now means what unset means, and the
			// startup line reports the resolved value, so nothing claims a cap it lacks.
			name: "negative max_events means unlimited",
			in:   SessionConfig{MaxEvents: -1},
			want: SessionLimits{MaxEvents: -1, MaxSessions: 100},
		},
		{
			name: "max_sessions set is honoured verbatim",
			in:   SessionConfig{MaxSessions: 7},
			want: SessionLimits{MaxSessions: 7},
		},
		{
			// The case that matters, and the one the field comment used to get wrong:
			// max_sessions: 0 is indistinguishable from unset on a plain int, so it
			// resolves to the default rather than to the "unlimited" the store would
			// apply if it were ever handed a zero. Honouring it would delete the default
			// for everyone who left the field alone.
			name: "max_sessions zero resolves to the default, not unlimited",
			in:   SessionConfig{MaxSessions: 0},
			want: SessionLimits{MaxSessions: 100},
		},
		{
			name: "negative max_sessions resolves to the default too",
			in:   SessionConfig{MaxSessions: -5},
			want: SessionLimits{MaxSessions: 100},
		},
		{
			name: "ttl parses",
			in:   SessionConfig{TTL: "30m"},
			want: SessionLimits{TTL: 30 * time.Minute, MaxSessions: 100},
		},
		{
			name: "ttl zero is never",
			in:   SessionConfig{TTL: "0"},
			want: SessionLimits{TTL: 0, MaxSessions: 100},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.in.Limits()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("Limits() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A malformed ttl must not take the store down with it: the limits come back usable and
// the error is the caller's to log.
func TestSessionLimits_MalformedTTL(t *testing.T) {
	got, err := SessionConfig{TTL: "half an hour", MaxEvents: 9}.Limits()
	if err == nil {
		t.Fatal("expected an error for a malformed ttl")
	}
	if want := (SessionLimits{TTL: 0, MaxEvents: 9, MaxSessions: 100}); got != want {
		t.Errorf("limits alongside the error = %+v, want %+v", got, want)
	}
}

// The startup line spells out the zeros, which would otherwise read as "sessions expire
// instantly" and "the store keeps nothing".
func TestSessionLimits_LogAttrs(t *testing.T) {
	for _, tc := range []struct {
		name string
		lim  SessionLimits
		want []any
	}{
		{
			name: "defaults",
			lim:  SessionLimits{MaxSessions: 100},
			want: []any{"expiry", "never", "maxEvents", "unlimited", "maxSessions", "100"},
		},
		{
			name: "all set",
			lim:  SessionLimits{TTL: 30 * time.Minute, MaxEvents: 500, MaxSessions: 7},
			want: []any{"expiry", "30m0s", "maxEvents", "500", "maxSessions", "7"},
		},
		{
			// Only MaxEvents gets an "unlimited": Limits resolves every max_sessions
			// <= 0 to 100, so a zero there cannot reach LogAttrs and is not rendered
			// as anything.
			name: "defaults, as Limits produces them",
			lim:  SessionLimits{MaxEvents: 0, MaxSessions: 100},
			want: []any{"expiry", "never", "maxEvents", "unlimited", "maxSessions", "100"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.lim.LogAttrs(); !slices.Equal(got, tc.want) {
				t.Errorf("LogAttrs() = %v, want %v", got, tc.want)
			}
		})
	}
}
