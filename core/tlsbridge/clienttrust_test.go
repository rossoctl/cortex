package tlsbridge

import (
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestClientTrust_OSTrustOnly(t *testing.T) {
	for _, c := range []struct {
		name          string
		goos          string
		isGo, trusted bool
		want          bool
	}{
		{"a Go program on darwin, CA not trusted", "darwin", true, false, true},
		{"a Go program on darwin, CA in the keychain", "darwin", true, true, false},
		{"not a Go program", "darwin", false, false, false},
		{"a Go program on linux reads SSL_CERT_FILE", "linux", true, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ct := &ClientTrust{
				goos:      c.goos,
				isGo:      func(string) bool { return c.isGo },
				osTrusted: func() (bool, time.Time) { return c.trusted, time.Now() },
			}
			if got := ct.OSTrustOnly("/bin/x"); got != c.want {
				t.Errorf("OSTrustOnly = %v, want %v", got, c.want)
			}
		})
	}
	var none *ClientTrust
	if none.OSTrustOnly("/bin/x") {
		t.Error("a nil ClientTrust predicted a refusal")
	}
	ct := &ClientTrust{goos: "darwin", isGo: func(string) bool { return true },
		osTrusted: func() (bool, time.Time) { return false, time.Now() }}
	if ct.OSTrustOnly("") {
		t.Error("a program with no known executable was predicted to refuse")
	}
}

// A program that is not Go never costs a Security.framework evaluation.
func TestClientTrust_AsksTheOSOnlyAboutGoPrograms(t *testing.T) {
	asked := 0
	ct := &ClientTrust{goos: "darwin", isGo: func(string) bool { return false },
		osTrusted: func() (bool, time.Time) { asked++; return false, time.Now() }}
	ct.OSTrustOnly("/usr/bin/curl")
	if asked != 0 {
		t.Errorf("asked the OS %d times about a program that is not Go", asked)
	}
}

func TestGoExecutables_RecognisesGo(t *testing.T) {
	self, err := os.Executable() // this test binary is a Go program
	if err != nil {
		t.Fatal(err)
	}
	g := newGoExecutables(8)
	if !g.isGo(self) {
		t.Errorf("%s, a Go test binary, did not read as Go", self)
	}
	if g.isGo("/bin/sh") {
		t.Error("/bin/sh read as Go")
	}
	if g.isGo(filepath.Join(t.TempDir(), "missing")) {
		t.Error("a file that does not exist read as Go")
	}
}

// An upgrade in place changes the file's size or modification time, and must be read
// again; an unchanged file must not be.
func TestGoExecutables_ReadsAFileAgainWhenItChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(path, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	g := newGoExecutables(8)
	reads := 0
	g.read = func(string) bool { reads++; return true }
	g.isGo(path)
	g.isGo(path)
	if reads != 1 {
		t.Fatalf("read an unchanged file %d times, want 1", reads)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	g.isGo(path)
	if reads != 2 {
		t.Fatalf("a changed file was not read again (reads = %d)", reads)
	}
}

func TestGoExecutables_IsBounded(t *testing.T) {
	g := newGoExecutables(2)
	g.read = func(string) bool { return false }
	dir := t.TempDir()
	for _, n := range []string{"a", "b", "c"} {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, nil, 0o755); err != nil {
			t.Fatal(err)
		}
		g.isGo(p)
	}
	if len(g.m) > 2 {
		t.Errorf("the cache holds %d entries; its bound is 2", len(g.m))
	}
}

// Review focus 4. The answer is reused inside its window, and a change of trust — the
// CA added to the keychain — is noticed once the window has passed, without a restart.
func TestOSTrustCheck_CachesForItsWindow(t *testing.T) {
	src, err := NewEphemeralSource()
	if err != nil {
		t.Fatal(err)
	}
	o := newOSTrustCheck(NewMinter(src, MinterOpts{}), time.Hour)
	calls := 0
	answer := errors.New("not trusted")
	o.verify = func(*x509.Certificate) error { calls++; return answer }

	if ok, _ := o.trusted(); ok || calls != 1 {
		t.Fatalf("first check: trusted = %v after %d evaluations, want false after 1", ok, calls)
	}
	answer = nil // the CA was added to the keychain
	if ok, _ := o.trusted(); ok || calls != 1 {
		t.Fatalf("inside the window: trusted = %v after %d evaluations, want the cached false after 1", ok, calls)
	}
	o.every = 0 // the window has passed
	if ok, _ := o.trusted(); !ok || calls != 2 {
		t.Fatalf("after the window: trusted = %v after %d evaluations, want true after 2", ok, calls)
	}
}

func TestOSTrustCheck_VerifiesALeafForTheReservedName(t *testing.T) {
	src, err := NewEphemeralSource()
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := src.Issuer()
	o := newOSTrustCheck(NewMinter(src, MinterOpts{}), time.Minute)
	var got *x509.Certificate
	o.verify = func(leaf *x509.Certificate) error { got = leaf; return nil }
	o.trusted()
	if got == nil {
		t.Fatal("the check verified nothing")
	}
	if !slices.Equal(got.DNSNames, []string{trustCheckHost}) {
		t.Errorf("the leaf names %v, want only %q", got.DNSNames, trustCheckHost)
	}
	if err := got.CheckSignatureFrom(ca); err != nil {
		t.Errorf("the leaf is not signed by the bridge CA: %v", err)
	}
}

// The real verifier, on whatever platform runs it: a CA generated a moment ago is in no
// trust store, so a leaf it signs must not verify. On darwin this is Security.framework,
// the case that matters; the macOS CI job runs it.
func TestOSTrustCheck_ARealVerifierDoesNotTrustAFreshCA(t *testing.T) {
	src, err := NewEphemeralSource()
	if err != nil {
		t.Fatal(err)
	}
	o := newOSTrustCheck(NewMinter(src, MinterOpts{}), time.Minute)
	if ok, _ := o.trusted(); ok {
		t.Fatal("the platform trusts a CA generated a moment ago")
	}
}

// The unread report says "not checked" off darwin, and must never ask the OS there;
// on darwin it reports the check's own answer and time.
func TestClientTrust_OSTrustState(t *testing.T) {
	at := time.Unix(1700000000, 0)
	asked := 0
	check := func() (bool, time.Time) { asked++; return true, at }
	if _, _, checked := (&ClientTrust{goos: "linux", osTrusted: check}).OSTrustState(); checked || asked != 0 {
		t.Errorf("off darwin: checked = %v after %d evaluations, want unchecked and none", checked, asked)
	}
	trusted, got, checked := (&ClientTrust{goos: "darwin", osTrusted: check}).OSTrustState()
	if !checked || !trusted || !got.Equal(at) {
		t.Errorf("on darwin = (%v, %v, %v), want (true, %v, true)", trusted, got, checked, at)
	}
	var none *ClientTrust
	if _, _, checked := none.OSTrustState(); checked {
		t.Error("a nil ClientTrust reported a check")
	}
}
