package egressallow

import "testing"

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
