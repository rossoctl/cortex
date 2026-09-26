package tlsbridge

import (
	"bytes"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// TrustBundleName is the file EnsureTrustBundle writes inside a CA dir. It holds
// the bridge CA followed by the platform's trusted roots.
//
// It exists because ca.crt on its own is only usable by tools whose CA setting
// is ADDITIVE. Node's NODE_EXTRA_CA_CERTS is — the name says so — but almost
// every other tool REPLACES its trust store with what you point it at:
//
//	SSL_CERT_FILE      Go (gh, abctl, any Go CLI) — Linux/CI only, see below
//	GIT_SSL_CAINFO     git
//	REQUESTS_CA_BUNDLE Python requests
//	CURL_CA_BUNDLE     curl
//
// Pointing those at ca.crt makes the process trust the bridge CA and NOTHING
// else, so every direct (unproxied) TLS connection fails. Go's own loader is
// explicit about it — crypto/x509 root_unix.go does `files = []string{f}` when
// SSL_CERT_FILE is set, discarding the defaults.
//
// SSL_CERT_FILE DOES NOTHING ON macOS, so on that platform this bundle serves
// git, curl and Python but not Go. root_unix.go, the file quoted above, is built
// for `linux || freebsd || …` and excludes darwin; darwin's loadSystemRoots
// returns a `systemPool: true` sentinel that reads no file at all, and verify.go
// then routes any program that has not set RootCAs to systemVerify —
// Security.framework, keychain only. No environment variable reaches it.
//
// That is worth stating precisely because the obvious reading is the wrong way
// round: it is NOT that darwin honours the variable and a bare CA happens to be
// harmless there. The variable is ignored outright, which is why a bare ca.crt
// looks fine on a Mac and breaks every Linux machine and CI runner.
//
// A Go program that wants the bridge CA on macOS has to opt in in-process, via
// x509.SystemCertPool() plus AppendCertsFromPEM — verify.go falls through to the
// pure-Go verifier with those extra roots when the platform verifier fails. That
// is available to code we compile and not to a third-party binary like gh.
const TrustBundleName = "bundle.crt"

// systemRootFiles are the locations that ship a concatenated PEM of the
// platform's trusted roots: the concatenated bundles distributions ship. Consulted
// AFTER SSL_CERT_FILE and BEFORE the cert directories, matching the order
// crypto/x509 and OpenSSL use — see findSystemRootsFrom, which owns that order and
// is where the claim about matching the tools' own trust set actually holds.
//
// macOS has no such file for its keychain, but Apple ships LibreSSL's copy at
// /etc/ssl/cert.pem, which is the same list — and the tools that need this
// bundle (git, curl, Python) are the ones that read files rather than the
// keychain anyway.
var systemRootFiles = []string{
	"/etc/ssl/certs/ca-certificates.crt",                // Debian, Ubuntu, Gentoo, Alpine
	"/etc/pki/tls/certs/ca-bundle.crt",                  // Fedora, RHEL 6
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", // RHEL 7+, CentOS
	"/etc/ssl/ca-bundle.pem",                            // openSUSE
	"/etc/pki/tls/cacert.pem",                           // OpenELEC
	"/etc/ssl/cert.pem",                                 // macOS (LibreSSL), Alpine, OpenBSD
	"/usr/local/etc/ssl/cert.pem",                       // FreeBSD
}

// ErrNoSystemRoots means no platform root bundle could be located, so no trust
// bundle was written.
//
// Deliberately an error rather than a fallback to "the bridge CA alone": that
// file is the dangerous artifact this whole mechanism exists to avoid, and
// writing it would hand every tool a trust store containing one private CA. A
// missing bundle degrades to "tools keep their own trust and cannot verify the
// bridge", which is a visible, recoverable failure. The inverse — silently
// distrusting the public internet — is neither.
var ErrNoSystemRoots = errors.New("tlsbridge: no system root bundle found")

// ErrCADirNotWritable means the bundle could not be written because ca_dir is
// read-only or otherwise not writable by this process.
//
// Distinguished from every other failure because it is the NORMAL, expected
// outcome in-cluster and must not be reported as a problem there. In a cluster
// ca_dir is an operator-mounted cert-manager Secret, and Kubernetes mounts Secret
// volumes read-only, so the write cannot succeed — while a sidecar has no use for
// the bundle anyway: it exists so a developer's git/curl/Python can verify a
// laptop bridge. Without this distinction the caller warns about four
// laptop-only environment variables on every production boot.
var ErrCADirNotWritable = errors.New("tlsbridge: ca_dir is not writable")

// EnsureTrustBundle writes <caDir>/bundle.crt as the bridge CA (<caDir>/ca.crt)
// followed by the platform's trusted roots, and returns its path.
//
// Idempotent: a bundle whose content already matches is left alone, so calling
// this on every boot neither churns the file nor disturbs a process that has it
// open. It re-reads both inputs, so a rotated CA or an updated root store is
// picked up — but only AT A CALL, and the only caller is the proxy at startup.
// For a service documented to run for weeks, that makes this a snapshot taken at
// boot, not a view of the store.
//
// The direction that matters is root REMOVAL: once the OS distrusts a root,
// every tool pointed at bundle.crt keeps trusting it until the proxy restarts,
// and nothing surfaces that. Addition is the benign half — a root added after
// boot is simply absent, and absence fails closed. Re-assembling on an interval
// would close the gap, and the content comparison above already makes that free
// of churn; it is not done yet because nothing has needed it.
//
// Callers must treat failure as non-fatal. The bridge itself works without a
// bundle — only the clients' ability to verify it is affected — so a boot that
// cannot assemble one should warn and continue, not exit.
func EnsureTrustBundle(caDir string) (string, error) {
	if caDir == "" {
		return "", errors.New("tlsbridge: empty ca_dir")
	}
	caPath := filepath.Join(caDir, "ca.crt")
	caPEM, err := os.ReadFile(caPath) //nolint:gosec // operator-supplied ca_dir
	if err != nil {
		return "", fmt.Errorf("tlsbridge: read bridge CA %s: %w", caPath, err)
	}
	bundlePath := filepath.Join(caDir, TrustBundleName)
	rootsPath, rootsPEM, err := findSystemRootsFrom(bundlePath)
	if err != nil {
		// A bundle from an earlier boot is deliberately LEFT IN PLACE, and the
		// error names it so the caller's warning can too.
		//
		// Deleting it would be worse than the staleness it removes. Four env vars
		// point at this file, and each REPLACES its tool's trust store, so an
		// absent bundle does not fall back to the platform roots — git, curl and
		// Python fail every TLS call outright ("error setting certificate verify
		// locations"). That trades a slightly outdated root list for total loss of
		// egress, on a path that is already only reached when the host's own root
		// store cannot be found.
		if _, serr := os.Stat(bundlePath); serr == nil {
			return "", fmt.Errorf("%w; keeping the existing %s, whose platform roots are now "+
				"unverifiable and may include roots this host no longer trusts", err, bundlePath)
		}
		return "", err
	}

	// CA first: a verifier that stops at the first match spends no time walking
	// the public roots to find ours, and a human running `head` on the file sees
	// which CA was grafted in.
	var buf bytes.Buffer
	buf.Write(ensureTrailingNewline(caPEM))
	buf.WriteString("# --- platform roots from " + rootsPath + " ---\n")
	buf.Write(ensureTrailingNewline(rootsPEM))

	if existing, rerr := os.ReadFile(bundlePath); rerr == nil && bytes.Equal(existing, buf.Bytes()) {
		return bundlePath, nil
	}
	// 0644 like ca.crt: this is public trust material, and every tool reading it
	// runs as the user or as another service account.
	if werr := atomicWriteFile(bundlePath, buf.Bytes(), 0o644); werr != nil {
		// A read-only ca_dir is the in-cluster norm, not a fault: classify it so
		// the caller can report it at the right level instead of warning about
		// laptop tooling on every production start.
		if isNotWritable(werr) {
			return "", fmt.Errorf("%w: %s: %w", ErrCADirNotWritable, bundlePath, werr)
		}
		return "", fmt.Errorf("tlsbridge: write trust bundle %s: %w", bundlePath, werr)
	}
	return bundlePath, nil
}

// isNotWritable reports whether err is the filesystem refusing the write, as
// opposed to any other I/O failure.
//
// EROFS covers a read-only mount (a Kubernetes Secret volume), EACCES/EPERM a
// directory this user cannot write. fs.ErrPermission catches the latter pair
// portably; EROFS has no fs sentinel and is matched directly.
func isNotWritable(err error) bool {
	return errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EROFS)
}

// findSystemRoots returns the first entry in systemRootFiles that is readable
// AND holds at least one certificate Go can parse.
//
// Parsing, rather than looking for the BEGIN line: a file containing the header
// but truncated or corrupt body would pass a substring check and produce a
// bundle with the bridge CA and no usable public roots — which is the CA-only
// trust store this whole mechanism exists to avoid, arrived at by a different
// route. AppendCertsFromPEM reports whether anything parsed, which is exactly
// the question. Some minimal images also ship an empty placeholder at a
// well-known path, and that is caught by the same check.
func findSystemRoots() (string, []byte, error) {
	return findSystemRootsFrom("")
}

// findSystemRootsFrom is findSystemRoots with the bundle path we are about to write,
// so it can refuse to treat its own output as a platform root source. Pass "" when
// there is nothing to exclude.
//
// Order matters and mirrors the consumers rather than convenience:
//
//  1. SSL_CERT_FILE — crypto/x509 does `files = []string{f}` when it is set
//     (root_unix.go), replacing the built-in list entirely; OpenSSL and LibreSSL
//     honour it too. A host that sets it has told every tool where its trust lives.
//  2. systemRootFiles — the concatenated bundles distributions ship.
//  3. SSL_CERT_DIR, else the standard cert directories — hosts that populate
//     /etc/ssl/certs/ as hashed symlinks WITHOUT also shipping
//     ca-certificates.crt match nothing in step 2. Before this, such a host got
//     ErrNoSystemRoots and a warning that there was "no safe file to point at",
//     on a machine with a complete trust store — which reads as a Cortex bug.
func findSystemRootsFrom(exclude string) (string, []byte, error) {
	if f := os.Getenv("SSL_CERT_FILE"); f != "" && !sameFile(f, exclude) {
		if data, err := os.ReadFile(f); err == nil && parseable(data) { //nolint:gosec // operator-supplied
			return f, data, nil
		}
	}
	for _, p := range systemRootFiles {
		if sameFile(p, exclude) {
			continue
		}
		data, err := os.ReadFile(p) //nolint:gosec // fixed list of well-known paths
		if err != nil || !parseable(data) {
			continue
		}
		return p, data, nil
	}
	dirs := certDirectories
	if d := os.Getenv("SSL_CERT_DIR"); d != "" {
		// OpenSSL and BoringSSL both split on ":", and so does crypto/x509.
		dirs = strings.Split(d, ":")
	}
	for _, dir := range dirs {
		if data, err := concatCertDir(dir, exclude); err == nil && parseable(data) {
			return dir, data, nil
		}
	}
	return "", nil, ErrNoSystemRoots
}

// certDirectories mirrors crypto/x509's list for the same platforms. Only consulted
// when no concatenated bundle was found.
var certDirectories = []string{
	"/etc/ssl/certs",               // SLES10/11, Debian, Ubuntu, Alpine
	"/etc/pki/tls/certs",           // Fedora, RHEL
	"/system/etc/security/cacerts", // Android
}

// concatCertDir joins every parseable certificate file in dir. Files are sorted so
// the output is stable across boots — the caller skips the write when content is
// unchanged, and an unstable order would rewrite the bundle on every start.
func concatCertDir(dir, exclude string) ([]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	var buf bytes.Buffer
	for _, name := range names {
		full := filepath.Join(dir, name)
		if sameFile(full, exclude) {
			continue
		}
		data, rerr := os.ReadFile(full) //nolint:gosec // operator-supplied directory
		if rerr != nil || !parseable(data) {
			// Hashed-symlink dirs also hold .0 CRL files and dangling links.
			continue
		}
		buf.Write(ensureTrailingNewline(data))
	}
	if buf.Len() == 0 {
		return nil, ErrNoSystemRoots
	}
	return buf.Bytes(), nil
}

// parseable reports whether data holds at least one certificate x509 can use.
func parseable(data []byte) bool {
	return x509.NewCertPool().AppendCertsFromPEM(data)
}

// sameFile reports whether a and b name the same file, so the bundle we are about to
// write is never read back in as a "platform root" source. Without this, a proxy
// started with SSL_CERT_FILE pointing at its own bundle.crt would re-embed the
// previous bundle on every boot, and a CA rotated out of ca.crt would stay trusted
// indefinitely — the one trust-lifetime bug this file exists to prevent.
func sameFile(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

// ensureTrailingNewline guards the concatenation boundary: a PEM file whose last
// line lacks a newline would otherwise splice its END line onto the next BEGIN,
// and every certificate after that point silently fails to parse.
func ensureTrailingNewline(b []byte) []byte {
	if len(b) == 0 || b[len(b)-1] == '\n' {
		return b
	}
	return append(append([]byte(nil), b...), '\n')
}
