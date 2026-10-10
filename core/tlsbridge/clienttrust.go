package tlsbridge

import (
	"crypto/x509"
	"debug/buildinfo"
	"os"
	"runtime"
	"sync"
	"time"
)

// Trust predicts, before any leaf is shown, that a client program could only refuse
// one. The bridge otherwise learns that by trying, which costs the program a failed
// connection; a prediction costs nothing, so it is worth having wherever it is certain.
type Trust interface {
	// OSTrustOnly reports that exe trusts nothing but the operating system's
	// certificate store, and that the store does not trust the bridge CA.
	OSTrustOnly(exe string) bool
}

// ClientTrust is the Trust for this machine. Its one certain case is a Go program on
// darwin. Go reads SSL_CERT_FILE only in root_unix.go, whose build tag leaves darwin
// out, and on darwin hands verification to Security.framework, which reads the
// keychain alone. Such a program cannot be told about the bridge CA by any environment
// variable, so it trusts the CA exactly when macOS does. Everywhere else a Go program
// reads SSL_CERT_FILE like any other client, and OSTrustOnly is false.
//
// Gated on GOOS at run time rather than by a build tag, so every platform's tests can
// drive the darwin rule by injection.
type ClientTrust struct {
	goos      string
	isGo      func(exe string) bool
	osTrusted func() (trusted bool, checkedAt time.Time)
}

// osTrustWindow is how long a trust answer is reused, and so the longest it takes for
// adding the CA to the keychain to count, without a restart.
const osTrustWindow = time.Minute

// NewClientTrust builds the ClientTrust for this machine. m must be the minter whose
// leaves the bridge shows clients, since the trust check verifies one of them.
func NewClientTrust(m *Minter) *ClientTrust {
	return &ClientTrust{
		goos:      runtime.GOOS,
		isGo:      newGoExecutables(goExecutablesMax).isGo,
		osTrusted: newOSTrustCheck(m, osTrustWindow).trusted,
	}
}

// OSTrustOnly implements Trust. Nil-safe: a nil ClientTrust predicts nothing.
func (c *ClientTrust) OSTrustOnly(exe string) bool {
	if c == nil || c.goos != "darwin" || exe == "" {
		return false
	}
	if !c.isGo(exe) {
		return false
	}
	trusted, _ := c.osTrusted()
	return !trusted
}

// goExecutablesMax bounds the Go-executable cache. A laptop sends far fewer distinct
// programs through the proxy than this; past it, an evicted path costs one more read.
const goExecutablesMax = 512

// goExecutables caches whether a path is a Go executable.
type goExecutables struct {
	mu   sync.Mutex
	max  int
	m    map[string]goExe
	read func(path string) bool
}

type goExe struct {
	size int64
	mod  time.Time
	isGo bool
}

func newGoExecutables(max int) *goExecutables {
	return &goExecutables{max: max, m: make(map[string]goExe), read: readsAsGo}
}

// readsAsGo reports whether path is a Go executable. debug/buildinfo finds the build
// information every Go binary since 1.13 carries, universal Mach-O files included, and
// errors on anything else.
func readsAsGo(path string) bool {
	_, err := buildinfo.ReadFile(path)
	return err == nil
}

// isGo reports whether path is a Go executable, reading the file once per size and
// modification time so an upgrade in place is read again. A file that cannot be
// stat'ed or read is not Go: the bridge then shows it a leaf, and the program memory
// catches it if it refuses.
func (g *goExecutables) isGo(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	g.mu.Lock()
	e, ok := g.m[path]
	g.mu.Unlock()
	if ok && e.size == fi.Size() && e.mod.Equal(fi.ModTime()) {
		return e.isGo
	}
	// Read outside the lock: a large binary takes a while, and other programs' lookups
	// should not wait behind it.
	is := g.read(path)
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.m[path]; !ok && len(g.m) >= g.max {
		for k := range g.m { // any one: an evicted path only costs a read
			delete(g.m, k)
			break
		}
	}
	g.m[path] = goExe{size: fi.Size(), mod: fi.ModTime(), isGo: is}
	return is
}

// trustCheckHost is the name the trust check mints its leaf for. Nothing under
// .invalid can resolve (RFC 6761), so the leaf can never stand in for a real host's.
const trustCheckHost = "trust-check.cortex.invalid"

// osTrustCheck asks whether the platform trusts the bridge CA. It reads trust and
// never writes it: Cortex does not change the keychain.
type osTrustCheck struct {
	m      *Minter
	every  time.Duration
	verify func(leaf *x509.Certificate) error

	mu sync.Mutex
	at time.Time
	ok bool
}

func newOSTrustCheck(m *Minter, every time.Duration) *osTrustCheck {
	return &osTrustCheck{m: m, every: every, verify: verifyWithSystemRoots}
}

// verifyWithSystemRoots verifies leaf against the platform's roots. With no Roots set,
// Go on darwin hands this to Security.framework: the same evaluation a Go client runs
// on the leaf the bridge shows it, under the same user's trust settings, because the
// proxy runs as that user. Evaluating trust prompts for nothing and changes nothing.
func verifyWithSystemRoots(leaf *x509.Certificate) error {
	_, err := leaf.Verify(x509.VerifyOptions{DNSName: trustCheckHost})
	return err
}

// trusted reports whether the platform trusts the bridge CA, evaluated at most once per
// every, and when that answer was reached. The lock is held through an evaluation so
// concurrent connections wait for one answer rather than each asking.
func (o *osTrustCheck) trusted() (bool, time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.at.IsZero() || time.Since(o.at) >= o.every {
		o.ok = o.evaluate()
		o.at = time.Now()
	}
	return o.ok, o.at
}

func (o *osTrustCheck) evaluate() bool {
	if o.m == nil {
		return false
	}
	cert, err := o.m.GetCertificateForHost(trustCheckHost)
	if err != nil || cert.Leaf == nil {
		return false
	}
	return o.verify(cert.Leaf) == nil
}
