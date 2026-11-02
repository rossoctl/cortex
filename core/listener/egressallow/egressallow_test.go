package egressallow

import (
	"strings"
	"testing"
)

// TestNew_RejectsWideningEntries pins the construction guards: the entries that
// would silently widen — or silently narrow to nothing — the boundary the list
// exists to hold are startup errors, not interpreted.
func TestNew_RejectsWideningEntries(t *testing.T) {
	for _, e := range []string{
		"",
		"   ",
		"example.com",      // portless: the contract is host:port
		"example.com:",     // empty port
		"example.com:http", // service names are not the contract either
		"example.com:0",
		"example.com:65536",
		"example.com:notaport",
		":443",  // no host
		"*:443", // wildcard host
		"**:443",
	} {
		if _, err := New([]string{e}); err == nil {
			t.Errorf("New(%q) = nil error, want a startup error", e)
		}
	}
}

// TestNew_AcceptsExactEntries: the shapes the contract names compile.
func TestNew_AcceptsExactEntries(t *testing.T) {
	if _, err := New([]string{"allowed.test:443", "Example.COM:80", "[::1]:8443"}); err != nil {
		t.Fatalf("New: %v", err)
	}
}

// TestAllows_ExactMatch is the core contract: on-list allows, everything else
// — other host, other port, other case spelling of the same pair — is denied.
func TestAllows_ExactMatch(t *testing.T) {
	m, err := New([]string{"allowed.test:443", "allowed.test:8080", "other.test:80"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, allowed := range []string{
		"allowed.test:443",
		"ALLOWED.test:443",  // host compares lowercased
		"allowed.test.:443", // trailing dot is the same name
		"allowed.test:8080",
		"other.test:80",
	} {
		if !m.Allows(allowed, "https") {
			t.Errorf("Allows(%q) = false, want true", allowed)
		}
	}
	for _, denied := range []string{
		"off.test:443",
		"allowed.test:8443", // right host, wrong port
		"allowed.test:80",   // portless defaults by scheme, and 80 is not listed for it
		"sub.allowed.test:443",
		"",
		"allowed.test:notaport",
	} {
		if m.Allows(denied, "https") {
			t.Errorf("Allows(%q) = true, want false", denied)
		}
	}
}

// TestAllows_PortlessDestinationDefaultsByScheme: a destination naming no port
// is read at its scheme's — http 80, https and CONNECT's tcp 443 — and judged
// against the entry for that pair.
func TestAllows_PortlessDestinationDefaultsByScheme(t *testing.T) {
	m, err := New([]string{"host.test:80"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !m.Allows("host.test", "http") {
		t.Error("Allows(host.test, http) = false, want true — portless http is host.test:80")
	}
	if m.Allows("host.test", "https") {
		t.Error("Allows(host.test, https) = true, want false — portless https is host.test:443, not listed")
	}
	if m.Allows("host.test", "tcp") {
		t.Error("Allows(host.test, tcp) = true, want false — CONNECT's portless default is 443, not listed")
	}
}

// TestAllows_IPv6Literals: bracketed v6 destinations — the only spelling a
// URL host, Host header or CONNECT authority carries them in — match their
// entries. An unbracketed v6 literal cannot be told from a host:port split
// gone wrong, so it is denied rather than guessed at (fail closed, same rule
// as any unparseable destination).
func TestAllows_IPv6Literals(t *testing.T) {
	m, err := New([]string{"[2001:db8::1]:443", "203.0.113.7:80"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !m.Allows("[2001:db8::1]:443", "https") {
		t.Error("Allows([2001:db8::1]:443) = false, want true")
	}
	if !m.Allows("[2001:0db8::1]:443", "https") {
		t.Error("Allows([2001:0db8::1]:443) = false, want true — same address, longer spelling")
	}
	if !m.Allows("203.0.113.7:80", "http") {
		t.Error("Allows(203.0.113.7:80) = false, want true")
	}
	if m.Allows("::ffff:203.0.113.7", "http") {
		t.Error("Allows(unbracketed v6) = true, want false — fail closed, not guessed")
	}
}

// TestAllows_NilMatcherAllowsEverything: nil is the feature off — today's
// behavior, byte for byte. The zero-value matcher (empty entry set) allows
// nothing, which is why cmd/cortex only builds one for a non-empty list.
func TestAllows_NilMatcherAllowsEverything(t *testing.T) {
	var m *Matcher
	if !m.Allows("anything.test:1", "https") {
		t.Error("nil Matcher denied a destination; nil means the feature is off")
	}
	empty, err := New(nil)
	if err != nil {
		t.Fatalf("New(nil): %v", err)
	}
	if empty.Allows("anything.test:443", "https") {
		t.Error("empty matcher allowed a destination; built = deny-by-default")
	}
}

// TestAllows_StrayBracketDeniedNotPanic: the destination comes off the wire,
// so a hostile spelling must be denied rather than panic. A bare "]" used to
// take the bracketed-literal branch on a LastIndex hit at 0 and index s[1:0];
// only a LEADING "[" takes that branch now, and a stray "]" is not host:port.
func TestAllows_StrayBracketDeniedNotPanic(t *testing.T) {
	m, err := New([]string{"allowed.test:443"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, dest := range []string{
		"]:443",
		"]",
		"]]:443",
		"a]b:443",
		"[::1", // opened, never closed
		"[]:443",
		"[::1]x", // closed, then junk instead of :port
		"allowed.test]:443",
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Allows(%q) panicked: %v", dest, r)
				}
			}()
			if m.Allows(dest, "https") {
				t.Errorf("Allows(%q) = true, want false", dest)
			}
		}()
	}
	// The spelling that is genuinely bracketed still parses.
	v6, err := New([]string{"[::1]:443"})
	if err != nil {
		t.Fatalf("New([::1]:443): %v", err)
	}
	if !v6.Allows("[::1]:443", "https") {
		t.Error("Allows([::1]:443) = false, want true — the bracketed branch must still work")
	}
}

// TestNew_RejectsUnbracketedIPv6Entry: an unbracketed v6 entry parses into a
// host:port that no destination can match ("2001:db8::1" → host "2001:db8:",
// port 1), so the operator would believe they allowed a host and have allowed
// nothing. It fails closed, but the contract is that entries which silently
// empty the boundary are startup errors — so it is one, naming the bracketed
// spelling that works.
func TestNew_RejectsUnbracketedIPv6Entry(t *testing.T) {
	for _, e := range []string{"::1", "2001:db8::1", "fd00::2"} {
		_, err := New([]string{e})
		if err == nil {
			t.Errorf("New(%q) = nil error, want a startup error", e)
			continue
		}
		if !strings.Contains(err.Error(), "IPv6") {
			t.Errorf("New(%q) error = %q, want it to name the IPv6 spelling", e, err)
		}
	}
	// A v6 entry whose last segment is not numeric trips the port check
	// first — a zone id, or an embedded v4 address. Still a startup error,
	// just reported as the bad port rather than as the bracket spelling.
	for _, e := range []string{"fe80::1%eth0", "::ffff:203.0.113.7"} {
		if _, err := New([]string{e}); err == nil {
			t.Errorf("New(%q) = nil error, want a startup error", e)
		}
	}
	// The bracketed spellings the message points at are accepted.
	if _, err := New([]string{"[::1]:443", "[2001:db8::1]:8443"}); err != nil {
		t.Errorf("New(bracketed v6) = %v, want nil", err)
	}
}

// TestAllows_UnparseableDestinationDenied: deny-by-default means a destination
// that cannot be parsed is refused, not guessed at.
func TestAllows_UnparseableDestinationDenied(t *testing.T) {
	m, err := New([]string{"allowed.test:443"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, dest := range []string{"allowed.test:0", "allowed.test:99999", ":443"} {
		if m.Allows(dest, "https") {
			t.Errorf("Allows(%q) = true, want false — unparseable is denied", dest)
		}
	}
}
