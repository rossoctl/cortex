package tlsbridge

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
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
