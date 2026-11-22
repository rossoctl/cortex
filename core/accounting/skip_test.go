package accounting

import "testing"

func TestSkipReason(t *testing.T) {
	cases := []struct {
		name string
		in   SkipInput
		want string
	}{
		{"200, with session", SkipInput{Status: 200, SessionID: "s1"}, ""},
		{"499 client-canceled", SkipInput{Status: 499, SessionID: "s1"}, "rejected_status"},
		{"502 bad gateway", SkipInput{Status: 502, SessionID: "s1"}, "rejected_status"},
		{"status 0 is not a rejection", SkipInput{Status: 0, SessionID: "s1"}, ""},
		{"more than one round", SkipInput{Status: 200, Rounds: 2, SessionID: "s1"}, "upstream_rounds=2"},
		{"empty session", SkipInput{Status: 200, SessionID: ""}, "no_record"},
		{"server-held history", SkipInput{Status: 200, SessionID: "s1", ServerHeldHistory: true}, "server_held_history"},
		{"unplaced deferral", SkipInput{Status: 200, SessionID: "s1", UnplacedDeferral: true}, "deferral_unplaced"},
		{"no session wins over rejection", SkipInput{Status: 502, SessionID: ""}, "no_record"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SkipReason(c.in); got != c.want {
				t.Errorf("SkipReason(%+v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
