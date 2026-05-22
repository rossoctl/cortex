package spiffe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

// DefaultSocketPath is the conventional SPIRE Workload API UDS path
// inside an authbridge sidecar. The agent socket is bind-mounted under
// /spiffe-workload-api/ by the operator-injected spec; this default
// matches that contract.
const DefaultSocketPath = "unix:///spiffe-workload-api/spire-agent.sock"

// DefaultMirrorDir is the directory the mirror writes svid.pem,
// svid_key.pem, svid_bundle.pem, and (when MirrorJWT is invoked)
// jwt_svid.token into. /opt matches the historical spiffe-helper
// layout that any remaining file-reading consumers expect.
const DefaultMirrorDir = "/opt"

// ProviderConfig parameterizes Provider construction.
//
// Empty SocketPath / MirrorDir resolve to DefaultSocketPath /
// DefaultMirrorDir at NewProvider time.
//
// MirrorFiles enables the file-mirror goroutine that copies the X.509
// SVID + bundle to MirrorDir for legacy file-reading consumers. The
// wider config layer (the spiffe block in authbridge-runtime) defaults
// this to true, so callers that omit the field get on-by-default
// mirroring at the surface they actually touch.
//
// JWT-SVID mirroring is plugin-driven: a plugin that needs an audience-
// specific token to be written to disk calls Provider.MirrorJWT. The
// framework no longer carries an audience here — only the tokenexchange
// plugin's spiffe identity path consumes the JWT-SVID.
type ProviderConfig struct {
	// SocketPath is the SPIRE Workload API endpoint URL. Empty
	// resolves to DefaultSocketPath.
	SocketPath string

	// MirrorFiles, when true, runs a background goroutine that
	// mirrors the in-memory X.509 SVIDs to disk under MirrorDir.
	// Defaults to true at the config layer above.
	MirrorFiles bool

	// MirrorDir is the directory the mirror writes to. Empty
	// resolves to DefaultMirrorDir.
	MirrorDir string
}

// Provider holds the SDK Workload API clients, exposes them through
// the framework's X509Source / JWTSource interfaces, and runs the
// X.509 file-mirror goroutine when MirrorFiles is set. NewProvider
// blocks until the first X.509-SVID arrives, so a successful return
// guarantees the X509Source is immediately usable for TLS handshakes.
//
// The JWT SDK client is opened lazily on the first JWTSource(audience)
// or MirrorJWT(ctx, audience) call — a Provider that no plugin ever
// asks for a JWT pays no SPIRE round-trip for it. Subsequent calls
// reuse the cached SDK client; new audiences just rebind the adapter.
//
// One Provider per process is the intended deployment shape.
type Provider struct {
	// cfg captures the resolved configuration for diagnostics.
	cfg ProviderConfig

	// x509SDK is the SDK-owned X.509 source. Owned by Provider —
	// closed in Close().
	x509SDK *workloadapi.X509Source

	// x509 is the framework-X509Source adapter wrapping x509SDK.
	// Always non-nil after a successful NewProvider.
	x509 *workloadX509

	// jwtMu guards lazy initialization of jwtSDK and the
	// mirrorAudiences map. Held only briefly (no I/O on the hot
	// path); workload_jwt's FetchToken takes its own SDK locks.
	jwtMu sync.Mutex

	// jwtSDK is the SDK-owned JWT source, opened lazily on the
	// first JWTSource() / MirrorJWT() call. nil until then. Owned
	// by Provider — closed in Close().
	jwtSDK *workloadapi.JWTSource

	// mirrorAudiences tracks the audiences for which a MirrorJWT
	// goroutine has already been spawned. Reentrant calls with the
	// same audience are a no-op.
	mirrorAudiences map[string]struct{}

	// mctx / mcancel scope every mirror goroutine (X.509 + per-
	// audience JWT). Cancelled by Close() so all mirror loops exit
	// before the SDK clients go away.
	mctx    context.Context
	mcancel context.CancelFunc
}

// NewProvider creates a Provider from cfg. It blocks until the first
// X.509-SVID arrives (the cold-start gate is delegated to
// workloadapi.NewX509Source's context). Pass a ctx with a deadline if
// you want to bound the agent-unreachable wait.
//
// The JWT SDK client is NOT opened here — it's opened on the first
// JWTSource(audience) / MirrorJWT(ctx, audience) call.
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

	// mirror lifecycle is decoupled from ctx — Provider owns it,
	// must outlive construction. Close() cancels mcancel.
	mctx, mcancel := context.WithCancel(context.Background())
	p := &Provider{
		cfg:             cfg,
		x509SDK:         x509SDK,
		x509:            newWorkloadX509(x509SDK, svid.ID.TrustDomain()),
		mirrorAudiences: make(map[string]struct{}),
		mctx:            mctx,
		mcancel:         mcancel,
	}

	if cfg.MirrorFiles {
		m := newMirror(mirrorConfig{
			Dir:  cfg.MirrorDir,
			X509: x509SDK,
		})
		// Synchronous initial write so the cold-start guarantee is
		// observable to anything that races with our return. Mirror
		// errors are non-fatal: a missing /opt mount must not fail
		// Provider construction.
		if err := m.writeX509(); err != nil {
			slog.Warn("spiffe.NewProvider: initial x509 mirror write", "err", err)
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

// JWTSource returns a framework JWTSource adapter bound to the given
// audience. The first call also opens the underlying SDK
// workloadapi.JWTSource (cached on the Provider); subsequent calls reuse
// that single SDK client and just bind a fresh adapter to the
// requested audience. Returns an error only if the SDK client cannot be
// opened (Workload API unreachable).
func (p *Provider) JWTSource(audience string) (JWTSource, error) {
	sdk, err := p.ensureJWTSDK()
	if err != nil {
		return nil, err
	}
	return newWorkloadJWT(sdk, audience), nil
}

// ensureJWTSDK lazily constructs the SDK JWT source on first use. Safe
// to call concurrently — returns the cached client on subsequent calls.
func (p *Provider) ensureJWTSDK() (*workloadapi.JWTSource, error) {
	p.jwtMu.Lock()
	defer p.jwtMu.Unlock()
	if p.jwtSDK != nil {
		return p.jwtSDK, nil
	}
	clientOpts := workloadapi.WithClientOptions(workloadapi.WithAddr(p.cfg.SocketPath))
	// Use a background context for SDK construction — the first
	// FetchJWTSVID call will retry if the agent isn't ready yet.
	// Bounding this to NewProvider's ctx would surface as a one-shot
	// failure on whichever request happens to be first.
	jwtSDK, err := workloadapi.NewJWTSource(context.Background(), clientOpts)
	if err != nil {
		return nil, fmt.Errorf("spiffe.Provider: jwt source: %w", err)
	}
	p.jwtSDK = jwtSDK
	return jwtSDK, nil
}

// MirrorJWT mirrors a JWT-SVID for the supplied audience to
// <MirrorDir>/jwt_svid.token. No-op when MirrorFiles is false.
//
// Performs a synchronous initial write so the file is observable on
// return, then spawns a goroutine that refreshes at 90% of the token's
// lifetime (or every 30s on error). The goroutine respects the
// Provider's mirror context and exits on Close().
//
// Reentrant: a second call with the same audience does NOT spawn a
// duplicate goroutine. Different audiences each get their own goroutine
// (and their own jwt_svid.token... but they all write to the same path,
// so callers should treat MirrorJWT as a single-audience contract).
func (p *Provider) MirrorJWT(ctx context.Context, audience string) error {
	if !p.cfg.MirrorFiles {
		return nil
	}
	if audience == "" {
		return errors.New("spiffe.Provider.MirrorJWT: audience is required")
	}

	sdk, err := p.ensureJWTSDK()
	if err != nil {
		return err
	}

	// Reentrancy guard. Hold the lock long enough to atomically
	// check-and-mark the audience as started.
	p.jwtMu.Lock()
	if _, already := p.mirrorAudiences[audience]; already {
		p.jwtMu.Unlock()
		return nil
	}
	p.mirrorAudiences[audience] = struct{}{}
	p.jwtMu.Unlock()

	// Synchronous initial write so the cold-start guarantee is
	// observable to anything that races with our return. Use the
	// caller's ctx for the initial write only; the refresh goroutine
	// uses Provider's mctx so it survives plugin Init.
	if _, err := writeJWT(ctx, sdk, audience, p.cfg.MirrorDir); err != nil {
		// Non-fatal at the framework level — the in-memory FetchToken
		// path is the source of truth for the plugin's hot path; the
		// mirror is an external-reader convenience.
		slog.Warn("spiffe.Provider.MirrorJWT: initial jwt mirror write", "audience", audience, "err", err)
	}

	go runJWTMirror(p.mctx, sdk, audience, p.cfg.MirrorDir)
	return nil
}

// Close shuts down the mirror goroutines and both SDK sources. Order:
// cancel mirror first (so the rotation loops exit before their SDK
// sources go away), then close the SDK sources. Errors from the SDK
// Close calls are joined; idempotent calls return nil because every
// step is nil-guarded.
func (p *Provider) Close() error {
	var errs []error
	if p.mcancel != nil {
		p.mcancel()
		p.mcancel = nil
	}
	p.jwtMu.Lock()
	jwtSDK := p.jwtSDK
	p.jwtSDK = nil
	p.jwtMu.Unlock()
	if jwtSDK != nil {
		if err := jwtSDK.Close(); err != nil {
			errs = append(errs, fmt.Errorf("jwt source close: %w", err))
		}
	}
	if p.x509SDK != nil {
		if err := p.x509SDK.Close(); err != nil {
			errs = append(errs, fmt.Errorf("x509 source close: %w", err))
		}
		p.x509SDK = nil
	}
	return errors.Join(errs...)
}
