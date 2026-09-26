package tlsbridge

import (
	"bytes"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// realPEM mints an actual certificate, because findSystemRoots parses rather
// than pattern-matches: a stand-in with the right BEGIN line no longer passes,
// which is the point of that check. Each call has its own key, so two results are
// distinguishable by bytes without re-parsing.
func realPEM(t *testing.T) []byte {
	t.Helper()
	_, _, certPEM, _, err := genSelfSignedCA()
	if err != nil {
		t.Fatalf("genSelfSignedCA: %v", err)
	}
	return certPEM
}

// withSystemRoots points the package at a temp root store for the duration of a
// test, so an outcome never depends on what the host happens to ship. Passing nil
// means "no root store on this machine".
// isolateRootSources detaches the package from the host's trust store for one test.
//
// findSystemRootsFrom consults three sources — SSL_CERT_FILE, systemRootFiles, then
// SSL_CERT_DIR or certDirectories — and a test that neutralises only one of them is
// at the mercy of whatever the runner ships. Overriding just systemRootFiles let a
// Linux runner's populated /etc/ssl/certs satisfy four tests that assert "no roots on
// this machine": they passed on macOS, where that directory holds nothing parseable,
// and failed in CI.
//
// Every test that pins root discovery calls this, so there is one place to extend
// when a fourth source appears.
func isolateRootSources(t *testing.T) {
	t.Helper()
	origFiles := systemRootFiles
	origDirs := certDirectories
	t.Cleanup(func() { systemRootFiles = origFiles; certDirectories = origDirs })
	certDirectories = []string{filepath.Join(t.TempDir(), "absent-dir")}
	systemRootFiles = []string{filepath.Join(t.TempDir(), "absent.pem")}
	t.Setenv("SSL_CERT_FILE", "")
	t.Setenv("SSL_CERT_DIR", "")
}

func withSystemRoots(t *testing.T, pem []byte) {
	t.Helper()
	isolateRootSources(t)
	if pem == nil {
		return // isolateRootSources already pointed every source at nothing
	}
	p := filepath.Join(t.TempDir(), "roots.pem")
	if err := os.WriteFile(p, pem, 0o644); err != nil {
		t.Fatal(err)
	}
	systemRootFiles = []string{p}
}

// caDirWith builds a ca_dir holding ca.crt, or an empty one when pem is nil.
func caDirWith(t *testing.T, pem []byte) string {
	t.Helper()
	dir := t.TempDir()
	if pem != nil {
		if err := os.WriteFile(filepath.Join(dir, "ca.crt"), pem, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func countCerts(t *testing.T, pem []byte) int {
	t.Helper()
	var n int
	rest := pem
	for {
		var block []byte
		block, rest = nextCertBlock(rest)
		if block == nil {
			return n
		}
		n++
	}
}

// nextCertBlock is a minimal PEM scanner: enough to count CERTIFICATE blocks
// without pulling encoding/pem semantics into an assertion.
func nextCertBlock(b []byte) (block, rest []byte) {
	const begin = "-----BEGIN CERTIFICATE-----"
	i := bytes.Index(b, []byte(begin))
	if i < 0 {
		return nil, nil
	}
	return b[i : i+len(begin)], b[i+len(begin):]
}

// TestEnsureTrustBundle_ContainsBothTrustSets is the whole point: a tool pointed
// at this file must trust the bridge AND the public internet. A bundle holding
// only one of them is the bug.
func TestEnsureTrustBundle_ContainsBothTrustSets(t *testing.T) {
	ca, roots := realPEM(t), realPEM(t)
	withSystemRoots(t, roots)
	dir := caDirWith(t, ca)

	path, err := EnsureTrustBundle(dir)
	if err != nil {
		t.Fatalf("EnsureTrustBundle: %v", err)
	}
	if got := filepath.Base(path); got != TrustBundleName {
		t.Errorf("bundle name = %q, want %q", got, TrustBundleName)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, ca) {
		t.Error("bundle is missing the bridge CA")
	}
	if !bytes.Contains(data, roots) {
		t.Error("bundle is missing the platform roots — every direct TLS call would fail")
	}
	if n := countCerts(t, data); n != 2 {
		t.Errorf("certificate count = %d, want 2", n)
	}
	// CA first, so a verifier finds it without walking the public roots.
	if bytes.Index(data, ca) > bytes.Index(data, roots) {
		t.Error("bridge CA should precede the platform roots")
	}
	// The result must be usable as a trust store, not merely well-formed text.
	if !x509.NewCertPool().AppendCertsFromPEM(data) {
		t.Error("assembled bundle does not parse as a cert pool")
	}
}

// TestEnsureTrustBundle_NoSystemRootsWritesNothing: the failure mode that matters.
// Falling back to a CA-only bundle would hand every tool a trust store containing
// one private CA — silently distrusting the public internet, which is far worse
// than not being able to verify the bridge.
func TestEnsureTrustBundle_NoSystemRootsWritesNothing(t *testing.T) {
	withSystemRoots(t, nil)
	dir := caDirWith(t, realPEM(t))

	_, err := EnsureTrustBundle(dir)
	if !errors.Is(err, ErrNoSystemRoots) {
		t.Fatalf("error = %v, want ErrNoSystemRoots", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, TrustBundleName)); serr == nil {
		t.Fatal("a CA-only bundle was written; it must not exist at all")
	}
}

// TestEnsureTrustBundle_KeepsAStaleBundleAndSaysSo: when the root store vanishes
// but a bundle from an earlier boot exists, that bundle is LEFT IN PLACE and the
// error names it.
//
// Deleting it would be worse than the staleness: four env vars point at this file
// and each replaces its tool's trust store, so an absent bundle does not fall back
// to the platform roots — it fails every TLS call. The error text is the only
// signal an operator gets, so it has to mention the file.
func TestEnsureTrustBundle_KeepsAStaleBundleAndSaysSo(t *testing.T) {
	ca, roots := realPEM(t), realPEM(t)
	withSystemRoots(t, roots)
	dir := caDirWith(t, ca)

	path, err := EnsureTrustBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// The host's root store disappears.
	withSystemRoots(t, nil)
	_, err = EnsureTrustBundle(dir)
	if !errors.Is(err, ErrNoSystemRoots) {
		t.Fatalf("error = %v, want it to wrap ErrNoSystemRoots", err)
	}
	if !strings.Contains(err.Error(), TrustBundleName) {
		t.Errorf("error does not name the stale bundle, so nothing surfaces it: %v", err)
	}
	after, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("the stale bundle was removed, which breaks every tool pointed at it: %v", rerr)
	}
	if !bytes.Equal(before, after) {
		t.Error("the stale bundle was modified")
	}
}

// TestEnsureTrustBundle_SkipsUnparseableRootFiles: a file carrying the BEGIN line
// but a corrupt body passed the old substring check and produced a bundle with no
// usable public roots — the CA-only trust store by another route. Some minimal
// images also ship an empty placeholder at a well-known path.
func TestEnsureTrustBundle_SkipsUnparseableRootFiles(t *testing.T) {
	// Isolate first: this test sets systemRootFiles by hand to control the candidate
	// ORDER, which withSystemRoots cannot express — but it still needs the env vars
	// and cert directories detached, or SSL_CERT_FILE from the host wins ahead of all
	// three candidates and the assertion tests nothing.
	isolateRootSources(t)
	dir := t.TempDir()

	empty := filepath.Join(dir, "empty.pem")
	if err := os.WriteFile(empty, []byte("# no certs here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Header present, body garbage: the case a substring check accepts.
	corrupt := filepath.Join(dir, "corrupt.pem")
	corruptPEM := "-----BEGIN CERTIFICATE-----\nnot base64 at all!!\n-----END CERTIFICATE-----\n"
	if err := os.WriteFile(corrupt, []byte(corruptPEM), 0o644); err != nil {
		t.Fatal(err)
	}
	roots := realPEM(t)
	good := filepath.Join(dir, "good.pem")
	if err := os.WriteFile(good, roots, 0o644); err != nil {
		t.Fatal(err)
	}
	systemRootFiles = []string{empty, corrupt, good}

	ca := realPEM(t)
	path, err := EnsureTrustBundle(caDirWith(t, ca))
	if err != nil {
		t.Fatalf("EnsureTrustBundle: %v", err)
	}
	data, _ := os.ReadFile(path)
	if !bytes.Contains(data, roots) {
		t.Error("skipped past the unusable files but the real roots are absent")
	}
	if bytes.Contains(data, []byte("not base64 at all")) {
		t.Error("the corrupt file was accepted as a root store")
	}

	// With ONLY unusable candidates, nothing is written.
	systemRootFiles = []string{empty, corrupt}
	if _, err := EnsureTrustBundle(caDirWith(t, ca)); !errors.Is(err, ErrNoSystemRoots) {
		t.Errorf("error = %v, want ErrNoSystemRoots when every candidate is unparseable", err)
	}
}

// TestEnsureTrustBundle_Idempotent: called on every boot, so an unchanged bundle
// must not be rewritten — that would churn the mtime and disturb a reader holding
// it open.
func TestEnsureTrustBundle_Idempotent(t *testing.T) {
	withSystemRoots(t, realPEM(t))
	dir := caDirWith(t, realPEM(t))

	path, err := EnsureTrustBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = EnsureTrustBundle(dir); err != nil {
		t.Fatal(err)
	}
	second, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !first.ModTime().Equal(second.ModTime()) {
		t.Error("unchanged bundle was rewritten")
	}
}

// TestEnsureTrustBundle_RewritesAfterCARotation: the flip side of idempotency. A
// stale bundle after the CA is regenerated would leave every tool unable to verify
// the bridge, with a file that looks present and correct.
func TestEnsureTrustBundle_RewritesAfterCARotation(t *testing.T) {
	withSystemRoots(t, realPEM(t))
	first := realPEM(t)
	dir := caDirWith(t, first)

	path, err := EnsureTrustBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	rotated := realPEM(t)
	if werr := os.WriteFile(filepath.Join(dir, "ca.crt"), rotated, 0o644); werr != nil {
		t.Fatal(werr)
	}
	if _, err = EnsureTrustBundle(dir); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !bytes.Contains(data, rotated) {
		t.Error("bundle still holds the old CA after rotation")
	}
	if bytes.Contains(data, first) {
		t.Error("bundle kept the superseded CA")
	}
}

// TestEnsureTrustBundle_MissingCAFails: without ca.crt there is nothing to graft
// in, and writing the platform roots alone would produce a file that verifies
// everything EXCEPT the bridge — passing silently while parsing nothing.
func TestEnsureTrustBundle_MissingCAFails(t *testing.T) {
	withSystemRoots(t, realPEM(t))
	dir := caDirWith(t, nil)

	if _, err := EnsureTrustBundle(dir); err == nil {
		t.Fatal("missing ca.crt should be an error")
	}
	if _, serr := os.Stat(filepath.Join(dir, TrustBundleName)); serr == nil {
		t.Error("a roots-only bundle was written")
	}
	if _, err := EnsureTrustBundle(""); err == nil {
		t.Error("empty ca_dir should be an error")
	}
}

// TestEnsureTrustBundle_SplicesPEMSafely: a CA file whose last line lacks a
// newline would otherwise join its END line to the next BEGIN, and every
// certificate after the splice silently fails to parse.
func TestEnsureTrustBundle_SplicesPEMSafely(t *testing.T) {
	roots := realPEM(t)
	withSystemRoots(t, roots)
	ca := realPEM(t)
	dir := caDirWith(t, bytes.TrimRight(ca, "\n")) // no trailing newline

	path, err := EnsureTrustBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if n := countCerts(t, data); n != 2 {
		t.Errorf("certificate count = %d, want 2", n)
	}
	// The real check: both certs still load. A splice leaves valid-looking text
	// that parses to fewer certs than it appears to hold.
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		t.Fatal("spliced bundle does not parse at all")
	}
	if !bytes.Contains(data, roots) {
		t.Error("roots lost across the splice boundary")
	}
}

// TestEnsureTrustBundle_ReadOnlyCADirIsClassified: in a cluster ca_dir is a
// mounted cert-manager Secret and Kubernetes mounts those read-only, so the write
// cannot succeed there. That is the normal outcome, not a fault — the caller keys
// off ErrCADirNotWritable to log it at Debug rather than warning about four
// laptop-only environment variables on every production boot.
func TestEnsureTrustBundle_ReadOnlyCADirIsClassified(t *testing.T) {
	withSystemRoots(t, realPEM(t))
	dir := caDirWith(t, realPEM(t))

	// 0500: readable and traversable, not writable — what a Secret mount looks
	// like to this process. Restored in cleanup so t.TempDir can remove it.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	_, err := EnsureTrustBundle(dir)
	if err == nil {
		t.Skip("write succeeded despite a 0500 ca_dir (running as root?)")
	}
	if !errors.Is(err, ErrCADirNotWritable) {
		t.Errorf("error = %v, want it to wrap ErrCADirNotWritable so the caller can "+
			"demote it; otherwise every in-cluster boot warns", err)
	}
	// It must NOT be mistaken for the missing-roots case, which is a real problem.
	if errors.Is(err, ErrNoSystemRoots) {
		t.Error("a read-only ca_dir must not report as missing system roots")
	}
}

// TestEnsureTrustBundle_OtherWriteErrorsStayLoud is the other side: only the
// filesystem refusing the write is demoted. A missing root store is a genuine
// misconfiguration and must keep warning.
func TestEnsureTrustBundle_OtherWriteErrorsStayLoud(t *testing.T) {
	withSystemRoots(t, nil)
	_, err := EnsureTrustBundle(caDirWith(t, realPEM(t)))
	if errors.Is(err, ErrCADirNotWritable) {
		t.Errorf("missing roots misclassified as unwritable ca_dir: %v", err)
	}
	if !errors.Is(err, ErrNoSystemRoots) {
		t.Errorf("error = %v, want ErrNoSystemRoots", err)
	}
}
