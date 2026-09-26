package tlsbridge

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFindSystemRoots_HonoursSSLCertFile: crypto/x509 does `files = []string{f}` when
// SSL_CERT_FILE is set, replacing its built-in list; OpenSSL honours it too. A host
// that sets it has told every tool where its trust lives, so assembling a bundle from
// a different list would hand tools a trust set they were not using.
func TestFindSystemRoots_HonoursSSLCertFile(t *testing.T) {
	pem := realPEM(t)
	dir := t.TempDir()
	custom := filepath.Join(dir, "custom-roots.pem")
	if err := os.WriteFile(custom, pem, 0o644); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	isolateRootSources(t) // outcome must not depend on the host's own store
	t.Setenv("SSL_CERT_FILE", custom)
	t.Setenv("SSL_CERT_DIR", "")

	path, data, err := findSystemRootsFrom("")
	if err != nil {
		t.Fatalf("findSystemRootsFrom: %v", err)
	}
	if path != custom {
		t.Errorf("path = %q, want the SSL_CERT_FILE path %q", path, custom)
	}
	if len(data) == 0 {
		t.Error("no data returned")
	}
}

// TestFindSystemRoots_HonoursSSLCertDir covers the case that previously failed on a
// complete host: hashed-symlink directories with no concatenated ca-certificates.crt
// matched nothing, produced ErrNoSystemRoots, and warned there was "no safe file to
// point at" — which reads as a Cortex bug.
func TestFindSystemRoots_HonoursSSLCertDir(t *testing.T) {
	pem := realPEM(t)
	dir := t.TempDir()
	for _, n := range []string{"aaa.pem", "bbb.pem"} {
		if err := os.WriteFile(filepath.Join(dir, n), pem, 0o644); err != nil { //nolint:gosec
			t.Fatal(err)
		}
	}
	// A CRL-ish file and an unparseable one must be skipped, not fatal.
	if err := os.WriteFile(filepath.Join(dir, "junk.0"), []byte("not a cert"), 0o644); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	isolateRootSources(t) // force the directory path to be the one that answers
	t.Setenv("SSL_CERT_FILE", "")
	t.Setenv("SSL_CERT_DIR", dir)

	path, data, err := findSystemRootsFrom("")
	if err != nil {
		t.Fatalf("findSystemRootsFrom: %v", err)
	}
	if path != dir {
		t.Errorf("path = %q, want the SSL_CERT_DIR %q", path, dir)
	}
	if !parseable(data) {
		t.Error("concatenated directory output is not parseable")
	}
}

// TestFindSystemRoots_NeverReadsItsOwnBundle is the trust-lifetime guard. A proxy
// started with SSL_CERT_FILE pointing at its own bundle.crt would otherwise re-embed
// the previous bundle every boot, so a CA rotated out of ca.crt would stay trusted
// indefinitely.
func TestFindSystemRoots_NeverReadsItsOwnBundle(t *testing.T) {
	pem := realPEM(t)
	dir := t.TempDir()
	bundle := filepath.Join(dir, TrustBundleName)
	if err := os.WriteFile(bundle, pem, 0o644); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	isolateRootSources(t)
	t.Setenv("SSL_CERT_FILE", bundle)
	t.Setenv("SSL_CERT_DIR", filepath.Join(dir, "nonexistent"))

	path, _, err := findSystemRootsFrom(bundle)
	if err == nil && path == bundle {
		t.Fatal("read its own bundle back as a platform root source")
	}
	// Either it found a real platform bundle on this host, or it found none. Both
	// are acceptable; returning OUR bundle is not.
	if path == bundle {
		t.Errorf("path = %q, which is the bundle being written", path)
	}
}

// TestFindSystemRoots_SkipsUnparseableSSLCertFile: a set-but-broken SSL_CERT_FILE must
// fall through to the platform list rather than producing a bundle with no usable
// roots, which would fail every public TLS call.
func TestFindSystemRoots_SkipsUnparseableSSLCertFile(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(bad, []byte("-----BEGIN CERTIFICATE-----\nnope\n"), 0o644); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	isolateRootSources(t)
	t.Setenv("SSL_CERT_FILE", bad)
	t.Setenv("SSL_CERT_DIR", "")

	path, _, err := findSystemRootsFrom("")
	if err == nil && path == bad {
		t.Error("accepted an unparseable SSL_CERT_FILE")
	}
}
