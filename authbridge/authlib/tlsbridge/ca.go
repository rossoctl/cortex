package tlsbridge

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"time"
)

// CASource supplies the signing CA used to mint per-origin leaves.
type CASource interface {
	// Issuer returns the CA certificate and its private key for signing leaves.
	Issuer() (cert *x509.Certificate, key crypto.Signer)
	// CACertPEM returns the CA certificate in PEM form (for the agent's trust store).
	CACertPEM() []byte
}

type staticSource struct {
	cert    *x509.Certificate
	key     crypto.Signer
	certPEM []byte
}

func (s *staticSource) Issuer() (*x509.Certificate, crypto.Signer) { return s.cert, s.key }
func (s *staticSource) CACertPEM() []byte                          { return s.certPEM }

// NewEphemeralSource generates an in-memory self-signed CA. Used as the
// standalone / no-cert-manager fallback and in tests.
func NewEphemeralSource() (CASource, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("tlsbridge: generate CA key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "authbridge-tls-bridge-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, fmt.Errorf("tlsbridge: self-sign CA: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("tlsbridge: parse self-signed CA: %w", err)
	}
	return &staticSource{
		cert:    cert,
		key:     key,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}, nil
}

// NewFileSource loads a CA (tls.crt/tls.key) from disk — the cert-manager /
// operator-coordinated path (Phase 2). Keys may be PKCS#8, PKCS#1 (RSA) or
// SEC1 (EC); cert-manager's DEFAULT encoding is PKCS#1, so all three are tried.
func NewFileSource(certPath, keyPath string) (CASource, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("tlsbridge: read CA cert %s: %w", certPath, err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("tlsbridge: read CA key %s: %w", keyPath, err)
	}
	cb, _ := pem.Decode(certPEM)
	if cb == nil {
		return nil, fmt.Errorf("tlsbridge: CA cert %s is not PEM", certPath)
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("tlsbridge: parse CA cert: %w", err)
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, fmt.Errorf("tlsbridge: CA key %s is not PEM", keyPath)
	}
	key, err := parsePrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	// Fail loud at load on a misissued Secret. Without these checks a non-CA or
	// mismatched cert/key loads "fine" and then silently fails to sign minted
	// leaves at request time → the agent rejects the chain → every call falls
	// open to tunnel, with no error. A cert-manager Secret can be misconfigured
	// (wrong issuerRef, leaf instead of CA, mid-rotation key mismatch), so verify.
	if !cert.IsCA {
		return nil, fmt.Errorf("tlsbridge: CA cert %s is not a CA (IsCA=false)", certPath)
	}
	if cert.KeyUsage != 0 && cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, fmt.Errorf("tlsbridge: CA cert %s lacks KeyUsageCertSign", certPath)
	}
	pub, ok := cert.PublicKey.(interface{ Equal(x crypto.PublicKey) bool })
	if !ok || !pub.Equal(key.Public()) {
		return nil, fmt.Errorf("tlsbridge: CA cert %s and key %s do not match", certPath, keyPath)
	}
	return &staticSource{cert: cert, key: key, certPEM: certPEM}, nil
}

// parsePrivateKey accepts PKCS#8, PKCS#1 (RSA) and SEC1 (EC) DER.
func parsePrivateKey(der []byte) (crypto.Signer, error) {
	if k, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		if s, ok := k.(crypto.Signer); ok {
			return s, nil
		}
		// A successful PKCS#8 parse means the bytes ARE PKCS#8, so falling
		// through to PKCS#1/SEC1 would be pointless — a non-Signer PKCS#8
		// key is a hard error, not a format we should keep guessing at.
		return nil, fmt.Errorf("tlsbridge: PKCS#8 key is not a crypto.Signer")
	}
	if k, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return k, nil
	}
	if k, err := x509.ParseECPrivateKey(der); err == nil {
		return k, nil
	}
	return nil, fmt.Errorf("tlsbridge: unsupported CA key format (tried PKCS#8, PKCS#1, SEC1)")
}
