package tlsbridge

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEphemeralSource_IssuesUsableCA(t *testing.T) {
	src, err := NewEphemeralSource()
	if err != nil {
		t.Fatalf("NewEphemeralSource: %v", err)
	}
	block, _ := pem.Decode(src.CACertPEM())
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("CACertPEM did not yield a CERTIFICATE PEM block")
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	if !caCert.IsCA {
		t.Errorf("issued cert is not a CA (IsCA=false)")
	}
	if caCert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Errorf("CA cert lacks KeyUsageCertSign")
	}
	cert, key := src.Issuer()
	if cert == nil || key == nil {
		t.Fatalf("Issuer() returned nil cert/key")
	}
}

func TestFileSource_LoadsPKCS1AndPKCS8(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pkcs8 bool
	}{{"ec-pkcs8", true}, {"ec-sec1", false}} {
		t.Run(tc.name, func(t *testing.T) {
			key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			tmpl := &x509.Certificate{
				SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "t"},
				NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
				IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true,
			}
			der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
			dir := t.TempDir()
			certP := filepath.Join(dir, "tls.crt")
			keyP := filepath.Join(dir, "tls.key")
			if err := os.WriteFile(certP, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
				t.Fatalf("write cert: %v", err)
			}
			var keyDER []byte
			keyType := "PRIVATE KEY"
			if tc.pkcs8 {
				keyDER, _ = x509.MarshalPKCS8PrivateKey(key)
			} else {
				// SEC1 EC keys conventionally use the "EC PRIVATE KEY" PEM
				// label (matches openssl / cert-manager artifacts).
				keyType = "EC PRIVATE KEY"
				keyDER, _ = x509.MarshalECPrivateKey(key)
			}
			if err := os.WriteFile(keyP, pem.EncodeToMemory(&pem.Block{Type: keyType, Bytes: keyDER}), 0o600); err != nil {
				t.Fatalf("write key: %v", err)
			}
			if _, err := NewFileSource(certP, keyP); err != nil {
				t.Fatalf("NewFileSource(%s): %v", tc.name, err)
			}
		})
	}
}

func TestFileSource_RejectsGarbage(t *testing.T) {
	// A valid cert paired with a garbage key, and a garbage cert, must both
	// surface a non-nil error rather than a panic or a silently-broken source.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "t"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true,
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	goodCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	for _, tc := range []struct {
		name    string
		certPEM []byte
		keyPEM  []byte
	}{
		{"garbage-cert", []byte("not a pem cert at all\n"), []byte("not a pem key at all\n")},
		{"good-cert-garbage-key", goodCertPEM, []byte("not a pem key at all\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			certP := filepath.Join(dir, "tls.crt")
			keyP := filepath.Join(dir, "tls.key")
			if err := os.WriteFile(certP, tc.certPEM, 0o600); err != nil {
				t.Fatalf("write cert: %v", err)
			}
			if err := os.WriteFile(keyP, tc.keyPEM, 0o600); err != nil {
				t.Fatalf("write key: %v", err)
			}
			if _, err := NewFileSource(certP, keyP); err == nil {
				t.Fatalf("NewFileSource(%s): expected error, got nil", tc.name)
			}
		})
	}
}

func TestFileSource_RejectsNonCAOrMismatch(t *testing.T) {
	// writePair builds a self-signed cert from certKey and writes it next to
	// fileKey (PKCS#8). When certKey != fileKey the cert/key public keys differ.
	writePair := func(t *testing.T, isCA bool, certKey, fileKey *ecdsa.PrivateKey) (string, string) {
		t.Helper()
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "t"},
			NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
			IsCA: isCA, BasicConstraintsValid: true,
		}
		if isCA {
			tmpl.KeyUsage = x509.KeyUsageCertSign
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, certKey.Public(), certKey)
		if err != nil {
			t.Fatalf("create cert: %v", err)
		}
		dir := t.TempDir()
		certP := filepath.Join(dir, "tls.crt")
		keyP := filepath.Join(dir, "tls.key")
		kd, _ := x509.MarshalPKCS8PrivateKey(fileKey)
		if err := os.WriteFile(certP, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
			t.Fatalf("write cert: %v", err)
		}
		if err := os.WriteFile(keyP, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}), 0o600); err != nil {
			t.Fatalf("write key: %v", err)
		}
		return certP, keyP
	}

	k1, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	k2, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	t.Run("non-CA cert", func(t *testing.T) {
		certP, keyP := writePair(t, false, k1, k1) // matching key, but IsCA=false
		if _, err := NewFileSource(certP, keyP); err == nil {
			t.Fatal("expected error for IsCA=false cert, got nil")
		}
	})
	t.Run("mismatched cert/key", func(t *testing.T) {
		certP, keyP := writePair(t, true, k1, k2) // cert carries k1's pubkey; file holds k2
		if _, err := NewFileSource(certP, keyP); err == nil {
			t.Fatal("expected error for cert/key mismatch, got nil")
		}
	})
}

func TestNewGeneratedFileSource_WritesAndReloads(t *testing.T) {
	dir := t.TempDir()
	certP := filepath.Join(dir, "tls.crt")
	keyP := filepath.Join(dir, "tls.key")
	trustP := filepath.Join(dir, "ca.crt")

	src, err := NewGeneratedFileSource(certP, keyP, trustP)
	if err != nil {
		t.Fatalf("NewGeneratedFileSource: %v", err)
	}

	// All three artifacts written.
	certPEM, err := os.ReadFile(certP)
	if err != nil {
		t.Fatalf("read tls.crt: %v", err)
	}
	if _, err := os.Stat(keyP); err != nil {
		t.Fatalf("stat tls.key: %v", err)
	}
	trustPEM, err := os.ReadFile(trustP)
	if err != nil {
		t.Fatalf("read ca.crt: %v", err)
	}
	// ca.crt is a copy of the signing cert (same trust anchor clients load).
	if !bytes.Equal(certPEM, trustPEM) {
		t.Error("ca.crt should be identical to tls.crt")
	}
	// The private key must not be group/other-readable.
	info, err := os.Stat(keyP)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("tls.key mode = %v, must not be group/other-accessible", info.Mode().Perm())
	}

	// The persisted pair reloads cleanly through the normal file path — proving
	// it's a valid, self-consistent signing CA.
	if _, err := NewFileSource(certP, keyP); err != nil {
		t.Fatalf("generated CA should reload via NewFileSource: %v", err)
	}
	// And the source's own CA cert is a usable CA.
	block, _ := pem.Decode(src.CACertPEM())
	if block == nil {
		t.Fatal("CACertPEM not a PEM block")
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	if !caCert.IsCA || caCert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("generated cert is not a signing CA")
	}
}

func TestEnsureFileSource_GenerateWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	src, generated, err := EnsureFileSource(dir, true)
	if err != nil {
		t.Fatalf("EnsureFileSource: %v", err)
	}
	if !generated {
		t.Error("expected generated=true for an empty ca_dir with generate=true")
	}
	if src == nil {
		t.Fatal("nil source")
	}
	for _, name := range []string{"tls.crt", "tls.key", "ca.crt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected %s to be written: %v", name, err)
		}
	}
}

func TestEnsureFileSource_NoGenerateFailsLoud(t *testing.T) {
	dir := t.TempDir() // empty, no CA files
	_, generated, err := EnsureFileSource(dir, false)
	if err == nil {
		t.Fatal("expected a load error when ca_dir is empty and generate=false")
	}
	if generated {
		t.Error("generated must be false on the no-generate path")
	}
}

func TestEnsureFileSource_LoadsExistingWithoutOverwrite(t *testing.T) {
	dir := t.TempDir()
	// First call generates.
	if _, generated, err := EnsureFileSource(dir, true); err != nil || !generated {
		t.Fatalf("first EnsureFileSource: generated=%v err=%v", generated, err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "tls.crt"))
	if err != nil {
		t.Fatalf("read tls.crt: %v", err)
	}
	// Second call with the files now present: load, do NOT regenerate/overwrite.
	_, generated, err := EnsureFileSource(dir, true)
	if err != nil {
		t.Fatalf("second EnsureFileSource: %v", err)
	}
	if generated {
		t.Error("expected generated=false when CA files already exist")
	}
	after, err := os.ReadFile(filepath.Join(dir, "tls.crt"))
	if err != nil {
		t.Fatalf("re-read tls.crt: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("existing CA was overwritten; must be left intact")
	}
}

func TestEnsureFileSource_PresentButInvalidNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	certP := filepath.Join(dir, "tls.crt")
	keyP := filepath.Join(dir, "tls.key")
	caP := filepath.Join(dir, "ca.crt")
	garbage := []byte("not a pem\n")
	// A COMPLETE set (all three present) that happens to be invalid.
	for _, p := range []string{certP, keyP, caP} {
		if err := os.WriteFile(p, garbage, 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	// A complete-but-invalid set must never be overwritten — return the loud
	// load error instead of silently minting a new CA (protects a real
	// mounted Secret). An INCOMPLETE set self-heals instead; see
	// TestEnsureFileSource_SelfHealsIncompleteSet.
	_, generated, err := EnsureFileSource(dir, true)
	if err == nil {
		t.Fatal("expected a load error for a present-but-invalid CA set")
	}
	if generated {
		t.Error("must not generate/overwrite when a complete CA set is present but invalid")
	}
	if got, _ := os.ReadFile(certP); !bytes.Equal(got, garbage) {
		t.Error("present-but-invalid tls.crt was overwritten")
	}
}

// An INCOMPLETE on-disk set (a partial write: some but not all of
// tls.crt/tls.key/ca.crt present) must self-heal under generate=true — mint a
// fresh complete set rather than fail. Closes the orphaned-key crash loop (key
// present, cert missing → NewFileSource would fatal on every boot) and the
// lost-ca.crt trust gap. Regression for the #707 review findings.
func TestEnsureFileSource_SelfHealsIncompleteSet(t *testing.T) {
	cases := []struct {
		name    string
		present []string // pre-seeded (stale) files
	}{
		{"orphaned key (cert+ca.crt missing)", []string{"tls.key"}},
		{"orphaned cert (key+ca.crt missing)", []string{"tls.crt"}},
		{"cert+key present, ca.crt missing", []string{"tls.crt", "tls.key"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tc.present {
				if err := os.WriteFile(filepath.Join(dir, f), []byte("stale\n"), 0o600); err != nil {
					t.Fatalf("seed %s: %v", f, err)
				}
			}
			src, generated, err := EnsureFileSource(dir, true)
			if err != nil {
				t.Fatalf("EnsureFileSource: %v", err)
			}
			if !generated {
				t.Error("expected generated=true (self-heal of an incomplete set)")
			}
			if src == nil {
				t.Fatal("nil source")
			}
			// A complete, valid, reloadable set now exists on disk.
			if _, err := NewFileSource(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")); err != nil {
				t.Fatalf("regenerated CA should reload via NewFileSource: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "ca.crt")); err != nil {
				t.Errorf("ca.crt should be present after self-heal: %v", err)
			}
		})
	}
}

func TestEnsureFileSource_CreatesMissingParentDirs(t *testing.T) {
	// ca_dir several levels deep with NONE of the intermediate parents present:
	// generation must os.MkdirAll the whole chain, not just the leaf.
	caDir := filepath.Join(t.TempDir(), "a", "b", "ca")
	src, generated, err := EnsureFileSource(caDir, true)
	if err != nil {
		t.Fatalf("EnsureFileSource on nested-nonexistent ca_dir: %v", err)
	}
	if !generated {
		t.Error("expected generated=true")
	}
	if src == nil {
		t.Fatal("nil source")
	}
	for _, name := range []string{"tls.crt", "tls.key", "ca.crt"} {
		if _, err := os.Stat(filepath.Join(caDir, name)); err != nil {
			t.Errorf("expected %s in the freshly-created nested ca_dir: %v", name, err)
		}
	}
	// Reload the persisted pair to prove it's a valid CA, not just files on disk.
	if _, err := NewFileSource(filepath.Join(caDir, "tls.crt"), filepath.Join(caDir, "tls.key")); err != nil {
		t.Fatalf("generated CA should reload via NewFileSource: %v", err)
	}
}

// writeCAWithValidity persists a complete, structurally VALID CA set into dir
// whose validity window is caller-chosen, so a test can present the exact
// on-disk state a laptop reaches after the generated CA's 365 days elapse.
// Deliberately writes all three files: the point is that the set is COMPLETE,
// which is what makes EnsureFileSource load it rather than self-heal.
func writeCAWithValidity(t *testing.T, dir string, notBefore, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "authbridge-tls-bridge-ca"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	kd, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd})
	for name, data := range map[string][]byte{"tls.crt": certPEM, "ca.crt": certPEM, "tls.key": keyPEM} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// caNotAfter reads back the persisted CA's expiry, to prove a replacement is
// actually a new certificate rather than the same one reloaded.
func caNotAfter(t *testing.T, dir string) time.Time {
	t.Helper()
	pemBytes, err := os.ReadFile(filepath.Join(dir, "tls.crt"))
	if err != nil {
		t.Fatalf("read tls.crt: %v", err)
	}
	blk, _ := pem.Decode(pemBytes)
	if blk == nil {
		t.Fatal("tls.crt is not PEM")
	}
	crt, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatalf("parse tls.crt: %v", err)
	}
	return crt.NotAfter
}

// TestEnsureFileSource_RenewsExpiredCA is the anniversary bug. The generated CA
// is valid 365 days and nothing renews it: reuse is keyed on file EXISTENCE and
// NewFileSource validates IsCA/KeyUsage/key-match but never expiry. So a year
// after a first install the same expired CA keeps loading and keeps forging
// leaves every client rejects — surfacing as client-rejected-ca, whose attached
// advice ("restart clients started before ca_not_before") cannot help and points
// at a cutoff ~a year old.
//
// Under generate=true an expired CA must be replaced, exactly as an incomplete
// set self-heals. The window is the laptop's; a mounted Secret is generate=false
// and is covered by TestEnsureFileSource_NoGenerateNeverTouchesExpiredCA.
func TestEnsureFileSource_RenewsExpiredCA(t *testing.T) {
	dir := t.TempDir()
	// A CA that was valid for its 365 days and lapsed yesterday.
	expiredAt := time.Now().Add(-24 * time.Hour)
	writeCAWithValidity(t, dir, expiredAt.Add(-365*24*time.Hour), expiredAt)

	src, generated, err := EnsureFileSource(dir, true)
	if err != nil {
		t.Fatalf("EnsureFileSource on an expired CA: %v", err)
	}
	if !generated {
		t.Fatal("an expired CA was reused; it will forge leaves every client rejects, " +
			"and the client-rejected-ca advice to restart clients cannot fix it")
	}
	if src == nil {
		t.Fatal("nil source")
	}
	if got := caNotAfter(t, dir); !got.After(time.Now()) {
		t.Errorf("persisted CA still expires in the past (%s); it was not actually replaced", got)
	}
	// The replacement must be loadable, not just newer.
	if _, err := NewFileSource(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")); err != nil {
		t.Fatalf("renewed CA should reload via NewFileSource: %v", err)
	}
}

// TestEnsureFileSource_RenewsCAExpiringSoon: waiting for expiry means every
// laptop gets a window of hard failures before the fix lands. A CA inside the
// renewal margin is replaced while it still works, so the swap is invisible.
func TestEnsureFileSource_RenewsCAExpiringSoon(t *testing.T) {
	dir := t.TempDir()
	// Still valid, but with less than caRenewBefore remaining.
	writeCAWithValidity(t, dir, time.Now().Add(-365*24*time.Hour), time.Now().Add(caRenewBefore/2))

	_, generated, err := EnsureFileSource(dir, true)
	if err != nil {
		t.Fatalf("EnsureFileSource on a soon-to-expire CA: %v", err)
	}
	if !generated {
		t.Error("a CA inside the renewal margin was reused; clients will start rejecting " +
			"its leaves before anything replaces it")
	}
	if got := caNotAfter(t, dir); got.Before(time.Now().Add(caRenewBefore)) {
		t.Errorf("persisted CA still expires within the renewal margin (%s)", got)
	}
}

// TestEnsureFileSource_KeepsHealthyCA guards the renewal against overreach: a CA
// with plenty of life left must still be loaded untouched. Regenerating a healthy
// CA is the very breakage this issue is about — it invalidates every running
// client's trust anchor.
func TestEnsureFileSource_KeepsHealthyCA(t *testing.T) {
	dir := t.TempDir()
	if _, generated, err := EnsureFileSource(dir, true); err != nil || !generated {
		t.Fatalf("seed EnsureFileSource: generated=%v err=%v", generated, err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "tls.crt"))
	if err != nil {
		t.Fatalf("read tls.crt: %v", err)
	}
	_, generated, err := EnsureFileSource(dir, true)
	if err != nil {
		t.Fatalf("second EnsureFileSource: %v", err)
	}
	if generated {
		t.Error("a healthy CA was regenerated; every running client's trust anchor just broke")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "tls.crt"))
	if !bytes.Equal(before, after) {
		t.Error("a healthy CA was overwritten on disk")
	}
}

// TestEnsureFileSource_NoGenerateNeverTouchesExpiredCA: in-cluster the CA is an
// operator-mounted cert-manager Secret and renewal is cert-manager's job. Minting
// a replacement there would substitute a self-signed CA for the mounted one and
// mask a real rotation failure, so generate=false must keep failing loud / loading
// as before, expiry notwithstanding.
func TestEnsureFileSource_NoGenerateNeverTouchesExpiredCA(t *testing.T) {
	dir := t.TempDir()
	expiredAt := time.Now().Add(-24 * time.Hour)
	writeCAWithValidity(t, dir, expiredAt.Add(-365*24*time.Hour), expiredAt)
	before, err := os.ReadFile(filepath.Join(dir, "tls.crt"))
	if err != nil {
		t.Fatalf("read tls.crt: %v", err)
	}

	_, generated, err := EnsureFileSource(dir, false)
	if err != nil {
		t.Fatalf("generate=false should still load an expired mounted CA: %v", err)
	}
	if generated {
		t.Error("generate=false minted a CA; a mounted Secret must never be replaced")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "tls.crt"))
	if !bytes.Equal(before, after) {
		t.Error("generate=false overwrote a mounted CA")
	}
}

// TestFingerprintSHA256_MatchesOpenSSL pins the rendering to the one form a user can
// actually compare against. The whole value of printing a fingerprint is that someone
// runs
//
//	openssl x509 -in ca.crt -noout -fingerprint -sha256
//
// and matches it by eye, so the encoding must be that command's: uppercase hex,
// colon-separated. A bare lowercase hex string would be correct and useless.
//
// The expectation is computed from the DER rather than from the implementation, so
// this fails if the function ever hashes the PEM (whose headers and whitespace give a
// different, uncomparable sum).
func TestFingerprintSHA256_MatchesOpenSSL(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := EnsureFileSource(dir, true); err != nil {
		t.Fatalf("EnsureFileSource: %v", err)
	}
	pemBytes, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatalf("read ca.crt: %v", err)
	}
	blk, _ := pem.Decode(pemBytes)
	if blk == nil {
		t.Fatal("ca.crt is not PEM")
	}
	crt, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	sum := sha256.Sum256(blk.Bytes)
	want := make([]string, 0, len(sum))
	for _, b := range sum {
		want = append(want, fmt.Sprintf("%02X", b))
	}
	if got, expected := FingerprintSHA256(crt), strings.Join(want, ":"); got != expected {
		t.Errorf("FingerprintSHA256() = %q, want %q", got, expected)
	}
}

// TestGenSelfSignedCA_SerialsAreDistinct: renewal now produces successive CAs in one
// directory, so a fixed serial would make old and new share BOTH subject and serial
// — the pair X.509 uses to name an issuer. A client whose bundle briefly holds both
// then hands OpenSSL two certificates it considers the same one.
func TestGenSelfSignedCA_SerialsAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		cert, _, _, _, err := genSelfSignedCA()
		if err != nil {
			t.Fatalf("genSelfSignedCA: %v", err)
		}
		s := cert.SerialNumber.String()
		if s == "1" {
			t.Fatal("serial is the fixed value 1; successive renewed CAs would be " +
				"indistinguishable by issuer name")
		}
		if seen[s] {
			t.Fatalf("serial %s repeated across mints", s)
		}
		seen[s] = true
	}
}
