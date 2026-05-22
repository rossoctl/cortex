package spiffe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

// DefaultSocketPath is the conventional SPIRE Workload API UDS path
// inside an authbridge sidecar. The agent socket is bind-mounted under
// /spiffe-workload-api/ by the operator-injected spec; this default
// matches that contract.
const DefaultSocketPath = "unix:///spiffe-workload-api/spire-agent.sock"

// DefaultMirrorDir is the directory the mirror writes svid.pem,
// svid_key.pem, svid_bundle.pem, and (when JWTAudience is set)
// jwt_svid.token into. /opt matches the historical spiffe-helper
// layout that any remaining file-reading consumers expect.
const DefaultMirrorDir = "/opt"

// ProviderConfig parameterizes Provider construction.
//
// Empty SocketPath / MirrorDir resolve to DefaultSocketPath /
// DefaultMirrorDir at NewProvider time. JWTAudience is the audience
// claim on the JWT-SVID minted for outbound RFC 8693 client-assertion
// JWTs; empty disables the JWT source entirely.
//
// MirrorFiles enables the file-mirror goroutine that copies SVID +
// bundle to MirrorDir for legacy file-reading consumers. The wider
// config layer (the spiffe ProviderConfig section in
// authbridge-runtime) defaults this to true, so callers that omit
// the field get on-by-default mirroring at the surface they actually
// touch.
type ProviderConfig struct {
	// SocketPath is the SPIRE Workload API endpoint URL. Empty
	// resolves to DefaultSocketPath.
	SocketPath string

	// JWTAudience fixes the audience claim on JWT-SVIDs minted by
	// the JWTSource. Empty disables the JWT source — JWTSource()
	// returns nil and no JWT mirror file is written.
	JWTAudience string

	// MirrorFiles, when true, runs a background goroutine that
	// mirrors the in-memory SVIDs to disk under MirrorDir. Defaults
	// to true at the config layer above.
	MirrorFiles bool

	// MirrorDir is the directory the mirror writes to. Empty
	// resolves to DefaultMirrorDir.
	MirrorDir string
}

// Provider holds the SDK Workload API clients, exposes them through
// the framework's X509Source / JWTSource interfaces, and runs the
// file-mirror goroutine when MirrorFiles is set. NewProvider blocks
// until the first X.509-SVID arrives, so a successful return
// guarantees the X509Source is immediately usable for TLS handshakes.
//
// One Provider per process is the intended deployment shape: the
// SDK's X509Source / JWTSource each open their own gRPC stream to
// the agent, and there's no benefit to multiplying them.
type Provider struct {
	// cfg captures the resolved configuration for diagnostics.
	cfg ProviderConfig

	// x509SDK is the SDK-owned X.509 source. Owned by Provider —
	// closed in Close().
	x509SDK *workloadapi.X509Source

	// jwtSDK is the SDK-owned JWT source. nil when JWTAudience is
	// empty. Owned by Provider — closed in Close().
	jwtSDK *workloadapi.JWTSource

	// x509 is the framework-X509Source adapter wrapping x509SDK.
	// Always non-nil after a successful NewProvider.
	x509 *workloadX509

	// jwt is the framework-JWTSource adapter wrapping jwtSDK. nil
	// when JWTAudience is empty; the JWTSource() method returns an
	// untyped nil interface in that case (typed-nil guard).
	jwt *workloadJWT

	// cancel cancels the mirror goroutine's context. nil when
	// MirrorFiles is false.
	cancel context.CancelFunc
}

// NewProvider creates a Provider from cfg. It blocks until the first
// X.509-SVID arrives (the cold-start gate is delegated to
// workloadapi.NewX509Source's context). Pass a ctx with a deadline if
// you want to bound the agent-unreachable wait.
//
// On error every SDK client constructed up to that point is closed
// before returning, so the caller doesn't have to chase partial
// initialization.
func NewProvider(ctx context.Context, cfg ProviderConfig) (*Provider, error) {
	if cfg.SocketPath == "" {
		cfg.SocketPath = DefaultSocketPath
	}
	if cfg.MirrorDir == "" {
		cfg.MirrorDir = DefaultMirrorDir
	}

	clientOpts := workloadapi.WithClientOptions(workloadapi.WithAddr(cfg.SocketPath))

	x509SDK, err := workloadapi.NewX509Source(ctx, clientOpts)
	if err != nil {
		return nil, fmt.Errorf("spiffe.NewProvider: x509 source: %w", err)
	}

	// Capture the trust domain from the first SVID. NewX509Source
	// already blocked until that SVID arrived, so this fetch
	// shouldn't fail; we still propagate the error rather than
	// asserting away a real failure mode.
	svid, err := x509SDK.GetX509SVID()
	if err != nil {
		_ = x509SDK.Close()
		return nil, fmt.Errorf("spiffe.NewProvider: get initial SVID: %w", err)
	}

	p := &Provider{
		cfg:     cfg,
		x509SDK: x509SDK,
		x509:    newWorkloadX509(x509SDK, svid.ID.TrustDomain()),
	}

	if cfg.JWTAudience != "" {
		jwtSDK, err := workloadapi.NewJWTSource(ctx, clientOpts)
		if err != nil {
			_ = x509SDK.Close()
			return nil, fmt.Errorf("spiffe.NewProvider: jwt source: %w", err)
		}
		p.jwtSDK = jwtSDK
		p.jwt = newWorkloadJWT(jwtSDK, cfg.JWTAudience)
	}

	if cfg.MirrorFiles {
		// Use a fresh context decoupled from ctx — Provider owns the
		// mirror lifecycle, so it must outlive the construction
		// context. Close() invokes cancel to terminate the mirror.
		mctx, cancel := context.WithCancel(context.Background())
		p.cancel = cancel
		m := newMirror(mirrorConfig{
			Dir:         cfg.MirrorDir,
			X509:        x509SDK,
			JWT:         p.jwtSDK, // nil when JWTAudience is empty; mirror handles that
			JWTAudience: cfg.JWTAudience,
		})
		// Synchronous initial write so the cold-start guarantee is
		// observable to anything that races with our return. Mirror
		// errors are non-fatal: a missing /opt mount must not fail
		// Provider construction.
		if err := m.writeX509(); err != nil {
			slog.Warn("spiffe.NewProvider: initial x509 mirror write", "err", err)
		}
		if p.jwtSDK != nil {
			if err := m.writeJWT(ctx); err != nil {
				slog.Warn("spiffe.NewProvider: initial jwt mirror write", "err", err)
			}
		}
		go m.run(mctx)
	}

	return p, nil
}

// X509Source returns the framework X509Source adapter. Never nil after
// a successful NewProvider — TLS listeners can capture this once at
// startup and call Certificate / TrustBundle on every handshake.
func (p *Provider) X509Source() X509Source {
	return p.x509
}

// JWTSource returns the framework JWTSource adapter, or untyped nil
// when JWTAudience was empty in the config. The explicit nil-return
// avoids the Go interface gotcha where (JWTSource)(nil) (a typed nil
// wrapping a nil *workloadJWT) compares != nil; callers checking
// `if p.JWTSource() == nil` get the result they expect.
func (p *Provider) JWTSource() JWTSource {
	if p.jwt == nil {
		return nil
	}
	return p.jwt
}

// Close shuts down the mirror goroutine and both SDK sources. Order:
// cancel mirror first (so the rotation loops exit before their SDK
// sources go away), then close the SDK sources. Errors from the SDK
// Close calls are joined; idempotent calls return nil because every
// step is nil-guarded.
func (p *Provider) Close() error {
	var errs []error
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	if p.jwtSDK != nil {
		if err := p.jwtSDK.Close(); err != nil {
			errs = append(errs, fmt.Errorf("jwt source close: %w", err))
		}
		p.jwtSDK = nil
	}
	if p.x509SDK != nil {
		if err := p.x509SDK.Close(); err != nil {
			errs = append(errs, fmt.Errorf("x509 source close: %w", err))
		}
		p.x509SDK = nil
	}
	return errors.Join(errs...)
}
