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
//
// Back-to-back on a live entry is a parallel burst, not a pinned client over time: the
// proxy never forges for a host while it is skipped. TestSkipSet_PinnedHostEscalatesAcrossWindows
// covers the pinned client.
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

// setWindow sets host's remaining skip window to d, leaving its count alone.
func setWindow(t *testing.T, s *SkipSet, host string, d time.Duration) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[host]
	if !ok {
		t.Fatalf("no entry for %q", host)
	}
	e.expiry = time.Now().Add(d)
	s.m[host] = e
}

// expireWindow ends host's skip window the way the clock would, leaving its count alone.
func expireWindow(t *testing.T, s *SkipSet, host string) {
	t.Helper()
	setWindow(t, s, host, -time.Millisecond)
}

// TestSkipSet_PinnedHostEscalatesAcrossWindows drives the set the way the proxy does. Both
// bridging paths (the CONNECT handler and the transparent listener) check Contains before
// forging, so a skipped host is tunnelled and can't reject anything. Every rejection after
// the first therefore lands on an EXPIRED entry, and the window still has to grow.
//
// It used to restart at the base instead, on the theory that a window elapsing with no
// further rejection meant the problem might be gone. Under that gate a window always
// elapses that way, so a pinned client failed its handshake every 30s forever — the
// outcome the backoff's own comment rules out.
func TestSkipSet_PinnedHostEscalatesAcrossWindows(t *testing.T) {
	s := NewSkipSet()
	for i := 1; i <= 6; i++ {
		if s.Contains("h") {
			t.Fatalf("rejection %d: host is still skipped, so the proxy would not have forged", i)
		}
		s.Fail("h")
		if i < 6 {
			expireWindow(t, s, "h")
		}
	}
	if got := window(t, s, "h"); got < s.ttl-time.Second {
		t.Errorf("window after 6 rejections, each after the last window expired = %v, "+
			"want the %v ceiling; an expired entry must not restart the backoff", got, s.ttl)
	}
}

// TestSkipSet_TransientAfterExpiryGetsTheBaseAndKeepsTheCount: a hang-up, a cipher mismatch
// or our own minting failure says nothing about trust. On a host whose window has run out it
// must not re-arm the earned window for every client, but it must not wipe out the count
// either, or the next rejection would start a pinned host over from the base.
func TestSkipSet_TransientAfterExpiryGetsTheBaseAndKeepsTheCount(t *testing.T) {
	s := NewSkipSet()
	for i := 0; i < 6; i++ {
		s.Fail("h")
		expireWindow(t, s, "h")
	}

	s.FailTransient("h")
	if got := window(t, s, "h"); got > s.base {
		t.Errorf("window after a transient failure on an expired, escalated entry = %v, want at "+
			"most the %v base; it re-armed the earned window for every client", got, s.base)
	}

	expireWindow(t, s, "h")
	s.Fail("h")
	if got := window(t, s, "h"); got < s.ttl-time.Second {
		t.Errorf("window after the next rejection = %v, want the %v ceiling; the transient "+
			"failure reset the count", got, s.ttl)
	}
}

// TestSkipSet_TransientOnALiveWindow: a transient failure on a live entry keeps the later of
// the window already set and one base from now, whatever the count. Two pooled connections
// that both passed Contains before either failed reach this. If the first hang-up left a base
// window on an escalated count, the second must not stretch it to that count's window.
func TestSkipSet_TransientOnALiveWindow(t *testing.T) {
	s := NewSkipSet()
	for i := 0; i < 6; i++ {
		s.Fail("h")
		expireWindow(t, s, "h")
	}
	s.FailTransient("h") // a base window on a count of 6
	s.FailTransient("h") // the second pooled connection
	if got := window(t, s, "h"); got > s.base {
		t.Errorf("second transient failure inside a base window = %v, want at most the %v base; "+
			"it stretched the window to the count's", got, s.base)
	}

	setWindow(t, s, "h", time.Minute) // an earned window, partly elapsed
	s.FailTransient("h")
	if got := window(t, s, "h"); got > time.Minute || got < time.Minute-time.Second {
		t.Errorf("transient failure on a live window with 1m left = %v, want it left at about 1m", got)
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

// TestDefaultPassthrough_ClaudeCodeUpdater: the Claude Code updater fetches a
// ~226MB binary from downloads.claude.ai, which belongs in the same category as
// every other entry here — a developer tool pulling a large artifact that no
// plugin can read. It is neither an inference nor a tool endpoint, so the
// "NEVER add" rule above does not reach it.
//
// Bridging it was not merely wasteful: it put the download on the buffered
// response path, where the 10MB cap answered 502 and `claude update` could not
// succeed through the proxy at all.
func TestDefaultPassthrough_ClaudeCodeUpdater(t *testing.T) {
	d := mustDecision(t, DecisionOpts{}) // nil SkipHosts -> DefaultPassthroughHosts
	tlsHello := []byte{0x16, 0x03, 0x01, 0x00, 0x05}
	if v, reason := d.Classify("downloads.claude.ai", 443, tlsHello); v != Passthrough || reason != "skip" {
		t.Errorf("downloads.claude.ai: got (%v,%q), want (%v,%q)", v, reason, Passthrough, "skip")
	}
	// The inference endpoint must stay bridged — that is where the parsers and
	// the token accounting live.
	if v, _ := d.Classify("api.anthropic.com", 443, tlsHello); v != Terminate {
		t.Errorf("api.anthropic.com: got %v, want %v (must stay bridged)", v, Terminate)
	}
}

// refuseAcrossWindows records n rejections of key, each after the window the one before
// it earned has ended: a program refusing again when it is next tried, which is the only
// rejection the program set counts. s's windows must be a few milliseconds for this to
// be quick.
func refuseAcrossWindows(s *SkipSet, key string, n int) {
	for i := 0; i < n; i++ {
		if i > 0 {
			time.Sleep(s.ttl + 5*time.Millisecond)
		}
		s.Fail(key)
	}
}

// TestProgramSkipSet_StopsAfterThreeRejections: a program that keeps refusing the
// leaf is evidence it will not change during this run, so the third consecutive
// rejection stops the retries. The window would otherwise re-arm forever (#912).
func TestProgramSkipSet_StopsAfterThreeRejections(t *testing.T) {
	s := NewProgramSkipSet()
	s.base, s.ttl = time.Millisecond, 4*time.Millisecond

	refuseAcrossWindows(s, "p", 2)
	time.Sleep(10 * time.Millisecond) // past any window two rejections earn
	if s.Contains("p") {
		t.Fatal("two rejections stopped the retries; three are required")
	}
	s.Fail("p")
	time.Sleep(10 * time.Millisecond) // past the ceiling
	if !s.Contains("p") {
		t.Fatal("three rejections in a row did not stop the retries: the program will fail again when the window ends")
	}
}

// TestProgramSkipSet_ABurstCountsOnce: the program set counts a rejection only after a
// wait. Connections that all passed Contains before the first rejection was recorded
// land on its still-open window. That is one burst, not three chances the program had
// to be fixed, so it must not stop the program in under a second.
func TestProgramSkipSet_ABurstCountsOnce(t *testing.T) {
	s := NewProgramSkipSet()
	for i := 0; i < programStopAfter; i++ {
		s.Fail("p")
	}
	s.mu.RLock()
	e := s.m["p"]
	s.mu.RUnlock()
	if e.stopped {
		t.Fatal("one burst of rejections stopped the program")
	}
	if e.failures != 1 {
		t.Errorf("failures = %d after one burst of %d rejections, want 1", e.failures, programStopAfter)
	}
	if got := window(t, s, "p"); got > s.base {
		t.Errorf("the burst lengthened the window to %v, more than one base (%v)", got, s.base)
	}

	// Nor does a burst undo a longer window an earlier, counted rejection earned.
	s.Fail("q")
	expireWindow(t, s, "q")
	s.Fail("q") // counted: a 2x window
	earned := window(t, s, "q")
	s.Fail("q") // the rest of that connection's burst
	if got := window(t, s, "q"); got < earned-time.Second {
		t.Errorf("a burst cut the earned window from %v to %v", earned, got)
	}
}

// A transient failure is not evidence about trust, so it must not walk a program
// toward being passed through for the rest of the run. Each one lands after the window
// ends, where a rejection would count.
func TestProgramSkipSet_TransientFailuresDoNotCountTowardStopping(t *testing.T) {
	s := NewProgramSkipSet()
	s.base, s.ttl = time.Millisecond, 4*time.Millisecond

	s.Fail("p")
	for i := 0; i < 5; i++ {
		time.Sleep(10 * time.Millisecond)
		s.FailTransient("p")
	}
	time.Sleep(10 * time.Millisecond)
	if s.Contains("p") {
		t.Fatal("transient failures stopped the retries")
	}
}

// A transient failure never adds to the count, but as a program's first failure it
// starts the count at one, so a hang-up followed by two spaced rejections stops the
// program — one rejection sooner than three would. The program-refused row in
// docs/laptop-service.md says so; this pins it.
func TestProgramSkipSet_AHangUpFirstStartsTheCount(t *testing.T) {
	s := NewProgramSkipSet()
	s.base, s.ttl = time.Millisecond, 4*time.Millisecond

	s.FailTransient("p")
	time.Sleep(10 * time.Millisecond) // past the hang-up's window
	refuseAcrossWindows(s, "p", 2)
	time.Sleep(10 * time.Millisecond) // past the ceiling: only a stop still holds it
	if !s.Contains("p") {
		t.Fatal("a hang-up and two spaced rejections did not stop the program")
	}
}

func TestProgramSkipSet_SucceedClearsAStop(t *testing.T) {
	s := NewProgramSkipSet()
	s.base, s.ttl = time.Millisecond, 4*time.Millisecond
	refuseAcrossWindows(s, "p", programStopAfter)
	time.Sleep(10 * time.Millisecond) // past the ceiling: only a stop still holds it
	if !s.Contains("p") {
		t.Fatal("precondition: the program is not stopped")
	}
	s.Succeed("p")
	if s.Contains("p") {
		t.Fatal("a completed handshake did not clear a stopped program")
	}
}

// The host set keeps today's behaviour: it never stops, because a host is shared by
// every client and the next one may well trust the CA.
func TestSkipSet_HostSetNeverStops(t *testing.T) {
	s := NewSkipSet()
	s.base, s.ttl = time.Millisecond, 4*time.Millisecond
	for i := 0; i < 10; i++ {
		s.Fail("h")
	}
	time.Sleep(10 * time.Millisecond)
	if s.Contains("h") {
		t.Fatal("the host set stopped retrying; only the program set may")
	}
}

// Review focus 5: when the set is full, a stopped program must not be the one evicted
// while entries that will expire anyway remain — eviction would make it fail again.
func TestSkipSet_StoppedEntriesAreEvictedLast(t *testing.T) {
	s := NewProgramSkipSet()
	s.max = 2
	// The stopped entry earns a window that has ended by the time the set fills, so it
	// is both the one expiring soonest and one the sweep would purge as expired. Left at
	// the default windows its three rejections outlast live's one, and ordering by
	// expiry alone would pass.
	s.base, s.ttl = time.Millisecond, time.Millisecond
	refuseAcrossWindows(s, "stopped", programStopAfter)
	time.Sleep(5 * time.Millisecond)
	s.base, s.ttl = skipBackoffBase, skipTTL
	s.Fail("live")
	s.Fail("newcomer") // full: one entry must go
	if !s.Contains("stopped") {
		t.Error("the stopped entry was evicted while a live window remained")
	}
	if s.Contains("live") {
		t.Error("the live window was kept and something else evicted")
	}
	if !s.Contains("newcomer") {
		t.Error("the new entry was not recorded")
	}
}

func TestSkipSet_EntriesListsWhatContainsReports(t *testing.T) {
	s := NewProgramSkipSet()
	// Tiny windows to stop one entry across them and let another expire, then long
	// ones so the live entry stays live for the rest of the test.
	s.base, s.ttl = time.Millisecond, 2*time.Millisecond
	refuseAcrossWindows(s, "stopped", programStopAfter)
	s.Fail("expired")
	s.base, s.ttl = time.Hour, time.Hour
	s.Fail("live")
	time.Sleep(5 * time.Millisecond)

	got := map[string]SkipEntry{}
	for _, e := range s.Entries() {
		got[e.Key] = e
	}
	if e, ok := got["stopped"]; !ok || !e.Stopped || !e.Until.IsZero() || e.Failures != programStopAfter {
		t.Errorf("stopped = %+v, %v; want Stopped, no Until, %d failures", e, ok, programStopAfter)
	}
	if e, ok := got["live"]; !ok || e.Stopped || e.Until.IsZero() || e.Failures != 1 {
		t.Errorf("live = %+v, %v; want a window and 1 failure", e, ok)
	}
	if _, ok := got["expired"]; ok {
		t.Error("an expired entry was listed; Contains no longer reports it")
	}
	for k := range got {
		if !s.Contains(k) {
			t.Errorf("Entries listed %q, which Contains does not report", k)
		}
	}
}
