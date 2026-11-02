package httpx

import "testing"

// LoopbackHost is the one loopback-Host predicate behind the session API's
// erase guards, the stats server's pprof endpoints and the agentop viewer's —
// three call sites that each used to carry their own copy. The table is the
// union of what those sites' tests pinned, so a regression here is caught once
// instead of per binary. It is an allowlist on purpose: see the function's doc.
func TestLoopbackHost(t *testing.T) {
	for _, tc := range []struct {
		hostport string
		want     bool
	}{
		{"localhost", true},
		{"localhost:9094", true},
		{"LOCALHOST:9094", true},
		{"127.0.0.1", true},
		{"127.0.0.1:47603", true},
		{"::1", true},
		{"[::1]:9094", true},
		{"", false},
		{"evil.example:47603", false},
		{"localhost.evil.example", false},
		{"127.0.0.1.evil.example", false},
		{"localhost.", false},
		{"127.0.0.2:9094", false},
		{"0177.0.0.1:80", false},
		{"127.1:80", false},
		{"::1.evil.example", false},
		{"[::ffff:127.0.0.1]:80", false},
	} {
		if got := LoopbackHost(tc.hostport); got != tc.want {
			t.Errorf("LoopbackHost(%q) = %v, want %v", tc.hostport, got, tc.want)
		}
	}
}
