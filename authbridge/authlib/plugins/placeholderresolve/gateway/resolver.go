// Package gateway provides a placeholderresolve.Resolver backed by the
// OpenShell gateway: it fetches the sandbox's resolved provider environment
// once (and on a refresh schedule) and serves placeholder lookups from an
// in-memory cache, so per-request resolution does no network I/O.
//
// It satisfies the plugin's Resolver and lifecycle interfaces structurally —
// this package does not import placeholderresolve, avoiding an import cycle.
package gateway

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/openshell"
)

const (
	// defaultRefreshInterval bounds how often the cache is refreshed when no
	// shorter per-credential expiry applies.
	defaultRefreshInterval = 5 * time.Minute
	// retryInterval is how often Start/refresh retries before the first
	// successful fetch (the gateway may not be reachable at pod boot).
	retryInterval = 3 * time.Second
	// minRefreshInterval floors the expiry-derived refresh delay.
	minRefreshInterval = 30 * time.Second
)

// Config locates the gateway and the sandbox identity material.
type Config struct {
	Endpoint    string
	MTLSCertDir string
	SATokenPath string
	SandboxID   string
}

// Resolver caches a sandbox's resolved provider environment from the gateway.
type Resolver struct {
	client          *openshell.Client
	refreshInterval time.Duration

	env      atomic.Pointer[openshell.Environment]
	ready    atomic.Bool
	bgCancel atomic.Pointer[context.CancelFunc]
}

// New dials the gateway and returns an unstarted Resolver. Call Start to prime
// the cache and begin background refresh.
func New(cfg Config) (*Resolver, error) {
	client, err := openshell.Dial(openshell.Config{
		Endpoint:    cfg.Endpoint,
		MTLSCertDir: cfg.MTLSCertDir,
		SATokenPath: cfg.SATokenPath,
		SandboxID:   cfg.SandboxID,
	})
	if err != nil {
		return nil, err
	}
	return &Resolver{client: client, refreshInterval: defaultRefreshInterval}, nil
}

// Resolve returns the real value for an env-var key from the cached
// environment. It fails closed (ok=false) when the cache is not yet primed,
// the key is absent, or the key's credential has expired.
func (r *Resolver) Resolve(_ context.Context, key string) (string, bool) {
	env := r.env.Load()
	if env == nil {
		return "", false
	}
	v, ok := env.Values[key]
	if !ok {
		return "", false
	}
	if exp, has := env.ExpiresAtMs[key]; has && exp > 0 && time.Now().UnixMilli() >= exp {
		return "", false
	}
	return v, true
}

// Start launches the refresh loop on a process-lifetime context (so it
// outlives the pipeline's Init budget) and returns immediately. The loop
// fetches right away; Ready() flips true once the first fetch succeeds.
func (r *Resolver) Start(_ context.Context) error {
	if r.bgCancel.Load() != nil {
		return nil // already started
	}
	bgCtx, cancel := context.WithCancel(context.Background())
	r.bgCancel.Store(&cancel)
	go r.refreshLoop(bgCtx)
	return nil
}

// Ready reports whether the cache has been primed at least once.
func (r *Resolver) Ready() bool { return r.ready.Load() }

// Stop cancels the refresh loop and closes the gateway connection.
func (r *Resolver) Stop(_ context.Context) error {
	if cancel := r.bgCancel.Swap(nil); cancel != nil {
		(*cancel)()
	}
	return r.client.Close()
}

func (r *Resolver) fetch(ctx context.Context) error {
	env, err := r.client.FetchEnvironment(ctx)
	if err != nil {
		return err
	}
	r.env.Store(env)
	r.ready.Store(true)
	return nil
}

func (r *Resolver) refreshLoop(ctx context.Context) {
	for {
		if err := r.fetch(ctx); err != nil {
			slog.Warn("gateway resolver: provider-environment fetch failed", "error", err)
		}
		timer := time.NewTimer(r.nextDelay())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// nextDelay schedules the next refresh: a short retry until the cache is
// primed, then 80% of the soonest per-credential expiry (clamped), or the
// default interval when nothing expires.
func (r *Resolver) nextDelay() time.Duration {
	if !r.ready.Load() {
		return retryInterval
	}
	env := r.env.Load()
	if env == nil || len(env.ExpiresAtMs) == 0 {
		return r.refreshInterval
	}
	now := time.Now().UnixMilli()
	soonest := int64(-1)
	for _, exp := range env.ExpiresAtMs {
		if exp <= 0 {
			continue
		}
		if soonest < 0 || exp < soonest {
			soonest = exp
		}
	}
	if soonest < 0 {
		return r.refreshInterval
	}
	d := time.Duration(soonest-now) * time.Millisecond * 8 / 10
	if d < minRefreshInterval {
		d = minRefreshInterval
	}
	if d > r.refreshInterval {
		d = r.refreshInterval
	}
	return d
}
