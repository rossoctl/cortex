package spiffe

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

// atomicWrite writes data to path via a tmp file in the same directory
// followed by os.Rename. Same-directory rename is atomic on POSIX, which
// guarantees external readers always see either the old or the new file
// content — never a partial write.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("atomicWrite: create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("atomicWrite: write %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("atomicWrite: chmod %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("atomicWrite: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("atomicWrite: rename %s -> %s: %w", tmpName, path, err)
	}
	return nil
}

// mirrorX509Source is the minimal surface of *workloadapi.X509Source that
// the mirror needs. Defining it here lets tests substitute a hand-rolled
// fake, mirroring the seam pattern in workload_x509.go's x509SVIDFetcher.
// *workloadapi.X509Source satisfies this implicitly via structural typing.
type mirrorX509Source interface {
	GetX509SVID() (*x509svid.SVID, error)
	GetX509BundleForTrustDomain(trustDomain spiffeid.TrustDomain) (*x509bundle.Bundle, error)
	Updated() <-chan struct{}
}

// mirrorJWTSource is the minimal surface of *workloadapi.JWTSource that
// the JWT-mirror path needs. *workloadapi.JWTSource satisfies this
// implicitly. The X.509 mirror struct does NOT embed a JWT source —
// JWT mirroring is plugin-driven via Provider.MirrorJWT.
type mirrorJWTSource interface {
	FetchJWTSVID(ctx context.Context, params jwtsvid.Params) (*jwtsvid.SVID, error)
}

// mirrorConfig is the immutable configuration for the X.509 file mirror.
type mirrorConfig struct {
	Dir  string
	X509 mirrorX509Source
}

// mirror copies SPIFFE X.509 credentials from in-memory sources to disk
// for readers that still consume files (e.g. legacy spiffe-helper
// layouts). All filesystem failures are best-effort: errors are logged
// at WARN and never propagate, since the in-memory hot path is the
// source of truth.
//
// JWT-SVID mirroring is plugin-driven (see Provider.MirrorJWT) — only
// the tokenexchange plugin's spiffe identity path knows the audience to
// mint, so the framework no longer mirrors JWTs unconditionally.
type mirror struct {
	cfg mirrorConfig
}

// newMirror constructs a mirror from cfg. The mirror does no I/O until
// run() is called.
func newMirror(cfg mirrorConfig) *mirror {
	return &mirror{cfg: cfg}
}

// run blocks until ctx is cancelled. It performs an initial X.509 write
// then enters the X.509 rotation loop.
func (m *mirror) run(ctx context.Context) {
	if err := m.writeX509(); err != nil {
		slog.Warn("spiffe.mirror: initial x509 write", "err", err)
	}
	m.runX509(ctx)
}

// runX509 subscribes to the X.509 source's Updated channel and writes
// the SVID + bundle on every signal. Blocks until ctx is done.
func (m *mirror) runX509(ctx context.Context) {
	ch := m.cfg.X509.Updated()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			if err := m.writeX509(); err != nil {
				slog.Warn("spiffe.mirror: x509 rotation write", "err", err)
			}
		}
	}
}

// writeX509 fetches the latest X.509 SVID and bundle, encodes them as
// PEM, and writes svid.pem / svid_key.pem / svid_bundle.pem atomically.
// Returns the first error encountered; partial writes are possible if a
// later step fails after an earlier success (acceptable: the next
// rotation overwrites).
func (m *mirror) writeX509() error {
	svid, err := m.cfg.X509.GetX509SVID()
	if err != nil {
		return fmt.Errorf("GetX509SVID: %w", err)
	}
	if svid == nil || len(svid.Certificates) == 0 {
		return errors.New("X509 SVID has no certificates")
	}

	// Cert chain (leaf-first, per X.509 SVID convention).
	var certPEM []byte
	for _, c := range svid.Certificates {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: c.Raw,
		})...)
	}
	if err := atomicWrite(filepath.Join(m.cfg.Dir, "svid.pem"), certPEM, 0o644); err != nil {
		return fmt.Errorf("write svid.pem: %w", err)
	}

	// Private key (PKCS#8).
	keyDER, err := x509.MarshalPKCS8PrivateKey(svid.PrivateKey)
	if err != nil {
		return fmt.Errorf("MarshalPKCS8PrivateKey: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := atomicWrite(filepath.Join(m.cfg.Dir, "svid_key.pem"), keyPEM, 0o600); err != nil {
		return fmt.Errorf("write svid_key.pem: %w", err)
	}

	// Bundle for the workload's own trust domain.
	bundle, err := m.cfg.X509.GetX509BundleForTrustDomain(svid.ID.TrustDomain())
	if err != nil {
		return fmt.Errorf("GetX509BundleForTrustDomain: %w", err)
	}
	var bundlePEM []byte
	for _, c := range bundle.X509Authorities() {
		bundlePEM = append(bundlePEM, pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: c.Raw,
		})...)
	}
	if err := atomicWrite(filepath.Join(m.cfg.Dir, "svid_bundle.pem"), bundlePEM, 0o644); err != nil {
		return fmt.Errorf("write svid_bundle.pem: %w", err)
	}
	return nil
}

// writeJWT fetches a fresh JWT-SVID for the supplied audience and writes
// it to <dir>/jwt_svid.token, returning 90% of the token's remaining
// lifetime as the recommended next-refresh delay (clamped to a 100ms
// minimum so a near-expired token can't drive a tight loop).
//
// Used by Provider.MirrorJWT (and its background refresh goroutine).
func writeJWT(ctx context.Context, jwtSrc mirrorJWTSource, audience, dir string) (time.Duration, error) {
	const minRefresh = 100 * time.Millisecond
	svid, err := jwtSrc.FetchJWTSVID(ctx, jwtsvid.Params{Audience: audience})
	if err != nil {
		return 0, fmt.Errorf("FetchJWTSVID: %w", err)
	}
	tokenPath := filepath.Join(dir, "jwt_svid.token")
	if err := atomicWrite(tokenPath, []byte(svid.Marshal()), 0o644); err != nil {
		return 0, fmt.Errorf("write jwt_svid.token: %w", err)
	}
	sleep := time.Until(svid.Expiry) * 9 / 10
	if sleep < minRefresh {
		sleep = minRefresh
	}
	return sleep, nil
}

// runJWTMirror loops calling writeJWT and sleeping until the next refresh
// time (90% of token lifetime) or ctx cancellation. On error, retries
// every 30s. Used by Provider.MirrorJWT for the background refresh
// goroutine.
func runJWTMirror(ctx context.Context, jwtSrc mirrorJWTSource, audience, dir string) {
	const errRetry = 30 * time.Second
	for {
		var sleep time.Duration
		if d, err := writeJWT(ctx, jwtSrc, audience, dir); err == nil {
			sleep = d
		} else {
			sleep = errRetry
			slog.Warn("spiffe.mirror: jwt write", "audience", audience, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(sleep):
		}
	}
}
