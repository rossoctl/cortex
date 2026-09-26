package tlsbridge

import (
	"testing"
	"time"
)

// mustDecision fails the test on an invalid pattern list, so each case reads as
// the single call it was before NewDecision grew an error return.
func mustDecision(t *testing.T, o DecisionOpts) *Decision {
	t.Helper()
	d, err := NewDecision(o)
	if err != nil {
		t.Fatalf("NewDecision(%+v): %v", o, err)
	}
	return d
}

func TestDecision_Classify(t *testing.T) {
	d := mustDecision(t, DecisionOpts{
		Ports:     map[int]bool{443: true, 8443: true},
		SkipHosts: []string{"pinned.example.com"},
	})
	tlsHello := []byte{0x16, 0x03, 0x01, 0x00, 0x05} // handshake, TLS1.0 record, len
	cases := []struct {
		name   string
		host   string
		port   int
		first  []byte
		expect Verdict
		reason string
	}{
		{"happy https", "api.example.com", 443, tlsHello, Terminate, ""},
		{"happy 8443", "api.example.com", 8443, tlsHello, Terminate, ""},
		{"non-tls first byte", "api.example.com", 443, []byte("GET / "), Passthrough, "non-tls"},
		{"unlisted port", "api.example.com", 9999, tlsHello, Passthrough, "port"},
		{"skip-listed host", "pinned.example.com", 443, tlsHello, Passthrough, "skip"},
		{"short record (<5 bytes)", "api.example.com", 443, []byte{0x16, 0x03}, Passthrough, "non-tls"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, reason := d.Classify(tc.host, tc.port, tc.first)
			if v != tc.expect || reason != tc.reason {
				t.Errorf("got (%v,%q), want (%v,%q)", v, reason, tc.expect, tc.reason)
			}
		})
	}
}

func TestDecision_DefaultPortsWhenNil(t *testing.T) {
	d := mustDecision(t, DecisionOpts{}) // nil Ports -> {443,8443}
	tlsHello := []byte{0x16, 0x03, 0x01, 0x00, 0x05}
	if v, reason := d.Classify("api.example.com", 443, tlsHello); v != Terminate || reason != "" {
		t.Errorf("port 443: got (%v,%q), want (%v,%q)", v, reason, Terminate, "")
	}
	if v, reason := d.Classify("api.example.com", 80, tlsHello); v != Passthrough || reason != "port" {
		t.Errorf("port 80: got (%v,%q), want (%v,%q)", v, reason, Passthrough, "port")
	}
}

func TestSkipSet_AutoSkip(t *testing.T) {
	s := NewSkipSet()
	if s.Contains("h") {
		t.Fatal("empty set should not contain h")
	}
	s.Fail("h")
	if !s.Contains("h") {
		t.Error("Fail then Contains failed")
	}
}

func TestSkipSet_TTLExpires(t *testing.T) {
	s := NewSkipSet()
	s.ttl = 20 * time.Millisecond // same-package test can tighten the CAP
	s.Fail("h")
	if !s.Contains("h") {
		t.Fatal("should contain immediately after Fail")
	}
	time.Sleep(40 * time.Millisecond)
	if s.Contains("h") {
		t.Error("entry should have expired (self-healing re-attempt)")
	}
}

func TestSkipSet_Bounded(t *testing.T) {
	s := NewSkipSet()
	s.max = 2 // cap small; a flood of distinct SNIs must not grow it unbounded
	s.Fail("a")
	s.Fail("b")
	s.Fail("c")
	if len(s.m) > 2 {
		t.Errorf("SkipSet grew past max: len=%d, want <=2", len(s.m))
	}
}

func TestDecision_HandlesPort(t *testing.T) {
	// Default set when Ports is nil.
	d := mustDecision(t, DecisionOpts{})
	if !d.HandlesPort(443) || !d.HandlesPort(8443) {
		t.Error("default set must handle 443 and 8443")
	}
	if d.HandlesPort(9443) {
		t.Error("default set must not handle 9443")
	}
	// Custom set replaces the default.
	c := mustDecision(t, DecisionOpts{Ports: map[int]bool{9443: true}})
	if !c.HandlesPort(9443) {
		t.Error("custom set must handle 9443")
	}
	if c.HandlesPort(443) {
		t.Error("custom set must not handle 443 (it replaces, not augments, the default)")
	}
}

// TestDefaultPassthrough_CoversTheToolsThatCannotBeConfigured is the point of the
// default list: `gh` has no CA option at all and Go on macOS honours no CA
// environment variable, so intercepting these hosts breaks them with no fix
// available to the user. Every host below appeared in a real proxy log as
// reason=handshake-fail.
func TestDefaultPassthrough_CoversTheToolsThatCannotBeConfigured(t *testing.T) {
	d := mustDecision(t, DecisionOpts{}) // nil SkipHosts -> defaults
	tlsHello := []byte{0x16, 0x03, 0x01, 0x00, 0x05}
	for _, host := range []string{
		"api.github.com", "github.com", "raw.githubusercontent.com",
		"codeload.github.com", "uploads.github.com", "cafe.github.com",
		"ghcr.io",
		"proxy.golang.org", "sum.golang.org", "google.golang.org", "golang.org",
		"go.googlesource.com", "go.opentelemetry.io", "go.yaml.in", "gopkg.in",
		"pypi.org", "files.pythonhosted.org", "registry.npmjs.org", "crates.io",
		// proxy.golang.org redirects module zips here; without it `go mod download`
		// still failed on one host after every other Go host was skipped.
		"storage.googleapis.com",
	} {
		if v, reason := d.Classify(host, 443, tlsHello); v != Passthrough || reason != "skip" {
			t.Errorf("%s: got (%v,%q), want (Passthrough,\"skip\")", host, v, reason)
		}
	}
	// Ports are still honoured ahead of the host check.
	if v, reason := d.Classify("api.github.com", 9999, tlsHello); reason != "port" {
		t.Errorf("port gate should win: got (%v,%q)", v, reason)
	}
	// The matcher strips the port, so host:port forms skip too.
	if v, _ := d.Classify("api.github.com:443", 443, tlsHello); v != Passthrough {
		t.Error("host:port form should still match the skip list")
	}
}

// TestDefaultPassthrough_NeverSkipsInferenceOrToolEndpoints is the guard that
// matters most. A default that quietly stopped bridging an LLM endpoint would
// remove the parsing and the tool-prune savings — the whole reason the bridge
// exists — with no error anywhere to notice it by.
//
// generativelanguage.googleapis.com is listed explicitly: it is why the Go module
// hosts are enumerated per-family instead of as a blanket *.googleapis.com.
func TestDefaultPassthrough_NeverSkipsInferenceOrToolEndpoints(t *testing.T) {
	d := mustDecision(t, DecisionOpts{})
	tlsHello := []byte{0x16, 0x03, 0x01, 0x00, 0x05}
	for _, host := range []string{
		"api.anthropic.com",
		"api.openai.com",
		"generativelanguage.googleapis.com",
		"us-central1-aiplatform.googleapis.com",
		"ete-litellm.ai-models.vpc-int.res.ibm.com",
		"github-tool-mcp",
		"github-tool-mcp.team1.svc.cluster.local",
		"weather-agent.team1.svc.cluster.local",
	} {
		if v, reason := d.Classify(host, 443, tlsHello); v != Terminate {
			t.Errorf("%s must stay bridged, got (%v,%q)", host, v, reason)
		}
	}
}

// TestDecisionOpts_ExplicitEmptyDisablesDefaults: nil means "unset, use the
// defaults" and an empty non-nil slice means "skip nothing". YAML distinguishes an
// absent key from `passthrough_hosts: []`, which is what lets an operator bridge
// everything without needing a second config field to turn defaults off.
func TestDecisionOpts_ExplicitEmptyDisablesDefaults(t *testing.T) {
	tlsHello := []byte{0x16, 0x03, 0x01, 0x00, 0x05}

	def := mustDecision(t, DecisionOpts{SkipHosts: nil})
	if v, _ := def.Classify("api.github.com", 443, tlsHello); v != Passthrough {
		t.Error("nil SkipHosts should apply the defaults")
	}
	none := mustDecision(t, DecisionOpts{SkipHosts: []string{}})
	if v, _ := none.Classify("api.github.com", 443, tlsHello); v != Terminate {
		t.Error("explicit empty SkipHosts should skip nothing, not fall back to defaults")
	}
	// An explicit list replaces the defaults rather than adding to them.
	own := mustDecision(t, DecisionOpts{SkipHosts: []string{"pinned.example.com"}})
	if v, _ := own.Classify("api.github.com", 443, tlsHello); v != Terminate {
		t.Error("an explicit list should replace the defaults")
	}
	if v, _ := own.Classify("pinned.example.com", 443, tlsHello); v != Passthrough {
		t.Error("an explicit list should still be honoured")
	}
}

// TestNewDecision_RejectsUnusablePatterns: a pattern that cannot match is a typo,
// and ignoring it presents as "the bridge broke my tool" with nothing connecting
// the symptom to the cause. Match-all is rejected for a different reason — it
// would disable interception wholesale while looking like a narrowing.
func TestNewDecision_RejectsUnusablePatterns(t *testing.T) {
	for _, bad := range [][]string{
		{"*"},                  // match-all
		{"**"},                 // match-all, other spelling
		{"api.github.com:443"}, // port-bearing: Match strips ports, so it could never fire
		{""},                   // empty
		{"github.com", "  "},   // one good, one blank
	} {
		if _, err := NewDecision(DecisionOpts{SkipHosts: bad}); err == nil {
			t.Errorf("NewDecision(%q) should have failed", bad)
		}
	}
	// The shipped defaults must themselves compile — a bad entry here would be
	// a fatal boot error for every user.
	if _, err := NewDecision(DecisionOpts{SkipHosts: DefaultPassthroughHosts}); err != nil {
		t.Fatalf("DefaultPassthroughHosts does not compile: %v", err)
	}
}

// window returns the remaining skip window for host, for asserting on backoff.
//
// A test helper rather than a method on SkipSet: nothing in the proxy needs to read a
// window — it only ever asks Contains — so a method would be non-test code that nothing
// calls. There WAS such a method briefly, left behind when an exported accessor was
// unexported instead of deleted; it sat dead until review caught it, because no linter
// here reports unused methods.
func window(t *testing.T, s *SkipSet, host string) time.Duration {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.m[host]
	if !ok {
		t.Fatalf("no entry for %q", host)
	}
	return time.Until(e.expiry)
}

// TestSkipSet_SucceedClears is the signal the set never had.
//
// It only ever added entries and waited them out, so a client that demonstrably
// trusted the CA bridging successfully taught it nothing: the next connection to that
// host was still tunnelled. On a machine where one agent holds a stale CA and the rest
// are fine, that is how the healthy ones lost their observability for ten minutes at a
// time, repeatedly.
func TestSkipSet_SucceedClears(t *testing.T) {
	s := NewSkipSet()
	s.Fail("h")
	if !s.Contains("h") {
		t.Fatal("Fail did not skip the host")
	}
	s.Succeed("h")
	if s.Contains("h") {
		t.Error("Succeed did not clear the entry; a trusting client's proof was discarded")
	}
}

// TestSkipSet_BackoffEscalates: consecutive rejections lengthen the window, so a host
// where NOTHING ever succeeds settles at the cap — today's behaviour, which is the
// case the skip was built for and must not regress.
func TestSkipSet_BackoffEscalates(t *testing.T) {
	s := NewSkipSet()
	var prev time.Duration
	for i := 1; i <= 6; i++ {
		s.Fail("h")
		got := window(t, s, "h")
		if got <= prev && got < s.ttl {
			t.Errorf("failure %d: window %v did not grow past %v", i, got, prev)
		}
		if got > s.ttl {
			t.Errorf("failure %d: window %v exceeds the cap %v", i, got, s.ttl)
		}
		prev = got
	}
	// Far past the doubling range: still capped, never negative from a shift overflow.
	for i := 0; i < 40; i++ {
		s.Fail("h")
	}
	if got := window(t, s, "h"); got > s.ttl || got <= 0 {
		t.Errorf("window after many failures = %v, want 0 < w <= %v", got, s.ttl)
	}
}

// TestSkipSet_SucceedResetsBackoff: clearing must discard the COUNT too, or a host that
// recovered would keep escalating from wherever it left off and the mixed-client case
// would drift toward the cap it is meant to avoid.
func TestSkipSet_SucceedResetsBackoff(t *testing.T) {
	s := NewSkipSet()
	for i := 0; i < 4; i++ {
		s.Fail("h")
	}
	escalated := window(t, s, "h")
	s.Succeed("h")
	s.Fail("h")
	if got := window(t, s, "h"); got >= escalated {
		t.Errorf("after Succeed the window is %v, want back near the base (was %v)", got, escalated)
	}
}

// TestSkipSet_ExpiredEntryRestartsBackoff: a window that elapsed with no further
// failure is evidence the problem may be gone, so the next failure starts over rather
// than continuing to climb.
//
// base is tightened as well as ttl, which the earlier version of this test did not do:
// with base fixed at 30s every window capped to the tiny ttl regardless of the failure
// count, so it exercised cap-and-reset and proved nothing about restarting a backoff its
// name claimed to cover. Driving both lets it escalate for real and then reset.
func TestSkipSet_ExpiredEntryRestartsBackoff(t *testing.T) {
	s := NewSkipSet()
	s.base = 10 * time.Millisecond
	s.ttl = 200 * time.Millisecond

	s.Fail("h")
	s.Fail("h")
	s.Fail("h")
	s.mu.RLock()
	climbed := s.m["h"].failures
	s.mu.RUnlock()
	if climbed != 3 {
		t.Fatalf("failures = %d after 3 consecutive rejections, want 3 (no real escalation "+
			"happened, so the reset below would prove nothing)", climbed)
	}
	if w := window(t, s, "h"); w <= s.base {
		t.Errorf("window %v did not grow beyond the base %v", w, s.base)
	}

	time.Sleep(120 * time.Millisecond) // longer than the 3rd window (40ms), shorter than ttl
	if s.Contains("h") {
		t.Fatal("entry should have expired")
	}
	s.Fail("h")
	s.mu.RLock()
	n := s.m["h"].failures
	s.mu.RUnlock()
	if n != 1 {
		t.Errorf("failures after an expired window = %d, want 1 (a fresh start)", n)
	}
}

// TestSkipSet_TransientFailureDoesNotEscalate is the review's point made executable.
//
// Only a rejected leaf is evidence of a persistent trust problem. A hang-up or a cipher
// mismatch still has to seed a skip — the forged handshake killed that connection — but
// escalating on it lets a client that merely cancels a lot walk the host to the ceiling
// and take every other client's observability with it.
func TestSkipSet_TransientFailureDoesNotEscalate(t *testing.T) {
	s := NewSkipSet()

	s.FailTransient("h")
	if !s.Contains("h") {
		t.Fatal("a transient failure must still seed a skip; the retry needs the tunnel")
	}
	for i := 0; i < 5; i++ {
		s.FailTransient("h")
	}
	// Asserted against base, not against the previous reading: every Fail re-dates the
	// window from now, so two readings differ by microseconds even with no escalation
	// at all. What matters is that the window never exceeds ONE base — an escalation
	// would put it at 2x or more.
	if got := window(t, s, "h"); got > s.base {
		t.Errorf("window is %v after six transient failures, more than one base (%v); "+
			"only a rejection should escalate", got, s.base)
	}
	s.mu.RLock()
	n := s.m["h"].failures
	s.mu.RUnlock()
	if n != 1 {
		t.Errorf("transient failures drove the count to %d, want it held at 1", n)
	}
}

// TestSkipSet_TransientDoesNotShortenAnEarnedWindow: a transient failure must not
// escalate, but it must not undo a longer window a real rejection already earned either
// — otherwise a cancel-happy client could keep resetting a genuinely pinned host back to
// the base and make it forge repeatedly.
func TestSkipSet_TransientDoesNotShortenAnEarnedWindow(t *testing.T) {
	s := NewSkipSet()
	for i := 0; i < 4; i++ {
		s.Fail("h")
	}
	earned := window(t, s, "h")

	s.FailTransient("h")
	if got := window(t, s, "h"); got < earned/2 {
		t.Errorf("a transient failure cut the earned window from %v to %v", earned, got)
	}
	s.mu.RLock()
	n := s.m["h"].failures
	s.mu.RUnlock()
	if n != 4 {
		t.Errorf("failures = %d after a transient failure, want the earned 4", n)
	}
}
