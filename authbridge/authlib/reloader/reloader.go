// Package reloader watches a config file and atomically swaps the
// inbound / outbound plugin pipelines when the file changes.
//
// The watcher targets the *directory* containing the config file,
// not the file itself, because Kubernetes ConfigMap mounts use
// symlink swap (..data → ..<timestamp>). A direct file watch misses
// the symlink retarget. Directory watch + basename filter sees
// plain writes, atomic renames, and symlink swaps uniformly.
//
// On a detected change the reloader debounces, hashes the new
// content, validates it against the last-active config (refusing
// changes to unreloadable fields like mode / listener addresses),
// builds fresh pipelines via the caller-supplied PipelineBuilder,
// runs their Start hooks under a bounded context, and Stores them
// into the Holders. The old pipelines keep running until a drain
// window expires, then Stop is invoked in a background goroutine.
//
// Validation failure at any stage leaves the current pipelines
// untouched and records the error in Status so operators can see
// the cause via the /reload/status endpoint.
package reloader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/rossoctl/rossocortex/authbridge/authlib/config"
	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
)

// PipelineBuilder loads the config file, validates it, and returns the
// built (but not-yet-Started) pipelines plus the parsed Config. The
// reloader invokes Start on the returned pipelines; on any Start failure
// it calls Stop to unwind.
//
// Implementations should mirror main.go's startup sequence exactly:
// config.Load → mode override → config.ApplyPreset → config.Validate →
// plugins.Build. Returning distinct errors for each stage lets the
// reloader surface a precise message on /reload/status.
type PipelineBuilder func() (inbound, outbound *pipeline.Pipeline, cfg *config.Config, err error)

// Option configures a Reloader at construction time.
type Option func(*Reloader)

// WithDrainWindow overrides the delay before old pipelines are Stopped
// after a successful swap. Default 30s. Tests pass 0 to avoid sleeping.
func WithDrainWindow(d time.Duration) Option { return func(r *Reloader) { r.drainWindow = d } }

// WithDebounce overrides the debounce interval used to coalesce rapid
// fsnotify events (e.g., the REMOVE+CREATE+CHMOD sequence that fires
// on a k8s atomic symlink swap). Default 250ms. Tests pass a smaller
// value to keep suites fast.
func WithDebounce(d time.Duration) Option { return func(r *Reloader) { r.debounce = d } }

// WithStartTimeout overrides the bound applied to the new pipelines'
// Start context. Default 60s (same as main.go startup).
func WithStartTimeout(d time.Duration) Option { return func(r *Reloader) { r.startTimeout = d } }

// Reloader owns the fsnotify watcher and the reload orchestration.
// Safe for concurrent use — all internal state transitions happen
// inside the single watch goroutine or are guarded by atomic.Pointer.
type Reloader struct {
	configPath string
	inbound    *pipeline.Holder
	outbound   *pipeline.Holder
	build      PipelineBuilder

	drainWindow  time.Duration
	debounce     time.Duration
	startTimeout time.Duration

	status    atomic.Pointer[Status]
	activeCfg atomic.Pointer[config.Config]

	// serialized by the watch goroutine
	lastHash string
}

// New constructs a Reloader. initialCfg must be the config the caller
// already loaded and built pipelines from — the reloader uses it as
// the baseline for detecting unreloadable-field changes.
func New(configPath string, inbound, outbound *pipeline.Holder, build PipelineBuilder, initialCfg *config.Config, opts ...Option) *Reloader {
	r := &Reloader{
		configPath:   configPath,
		inbound:      inbound,
		outbound:     outbound,
		build:        build,
		drainWindow:  30 * time.Second,
		debounce:     250 * time.Millisecond,
		startTimeout: 60 * time.Second,
	}
	for _, opt := range opts {
		opt(r)
	}
	r.activeCfg.Store(initialCfg)

	// Seed the hash with the bytes main.go just loaded, so a spurious
	// fsnotify event that fires immediately after startup (e.g., the
	// kubelet touching the mount) doesn't trigger a redundant rebuild.
	if data, err := os.ReadFile(configPath); err == nil {
		sum := sha256.Sum256(data)
		r.lastHash = hex.EncodeToString(sum[:])
	}

	init := Status{LastAttempt: time.Time{}, LastSuccess: time.Now(), ActiveConfigSHA256: r.lastHash}
	r.status.Store(&init)
	return r
}

// Start arms the watcher. Blocks until the watcher is ready or fails
// to arm (invalid directory, fsnotify init error). Returns nil on
// success; the watch goroutine then runs until ctx is cancelled.
//
// Safe to call at most once. A Reloader whose Start returned an error
// is not usable — construct a new one.
func (r *Reloader) Start(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("fsnotify.NewWatcher: %w", err)
	}

	dir := filepath.Dir(r.configPath)
	if err := watcher.Add(dir); err != nil {
		watcher.Close()
		return fmt.Errorf("fsnotify.Add(%s): %w", dir, err)
	}

	go r.watchLoop(ctx, watcher)
	slog.Info("reloader: watching for config changes",
		"path", r.configPath, "dir", dir, "drainWindow", r.drainWindow)
	return nil
}

// Status returns a snapshot of the most recent reload outcome.
func (r *Reloader) Status() Status { return *r.status.Load() }

// ConfigProvider returns a closure the StatServer can call to render
// /config against the currently-active (post-swap) configuration.
func (r *Reloader) ConfigProvider() func() *config.Config {
	return func() *config.Config { return r.activeCfg.Load() }
}

// watchLoop drains fsnotify events, debounces them, and dispatches
// each debounced burst to reloadOnce. Exits when ctx is cancelled or
// the watcher channel closes.
func (r *Reloader) watchLoop(ctx context.Context, watcher *fsnotify.Watcher) {
	defer watcher.Close()

	base := filepath.Base(r.configPath)
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	trigger := make(chan struct{}, 1)

	for {
		select {
		case <-ctx.Done():
			return

		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			slog.Warn("reloader: fsnotify error", "error", err)

		case ev, ok := <-watcher.Events:
			if !ok {
				return
			}
			// Only care about events naming our file (or its ConfigMap
			// alias directory ..data that symlink-swaps to it). The
			// underlying timestamped dirs (..2026_05_08_xx) produce
			// events too, but we catch the swap via the ..data rewrite.
			if filepath.Base(ev.Name) != base && filepath.Base(ev.Name) != "..data" {
				continue
			}
			slog.Debug("reloader: fs event", "name", ev.Name, "op", ev.Op.String())

			// Debounce: reset the timer on every event. Fire once when
			// the burst quiesces for `debounce`.
			if timer == nil {
				timer = time.AfterFunc(r.debounce, func() {
					select {
					case trigger <- struct{}{}:
					default:
					}
				})
			} else {
				timer.Reset(r.debounce)
			}

		case <-trigger:
			r.reloadOnce(ctx)
		}
	}
}

// reloadOnce runs one reload attempt from start to finish. Any error
// leaves the currently-active pipelines untouched and is reported via
// Status. Only the debounced timer fires this, so two reloadOnce calls
// cannot overlap.
func (r *Reloader) reloadOnce(parent context.Context) {
	attempt := time.Now()

	// Content-hash dedup: ignore mtime-only changes (e.g., a touch).
	data, err := os.ReadFile(r.configPath)
	if err != nil {
		r.recordFailure(attempt, fmt.Errorf("read: %w", err))
		return
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if hash == r.lastHash {
		slog.Debug("reloader: content unchanged, skipping reload", "sha256", hash[:12])
		return
	}

	// Build + validate a new pair of pipelines. build() does Load +
	// ApplyPreset + Validate + plugins.Build in the same order main.go
	// used, so behavior is identical to process startup.
	newIn, newOut, newCfg, err := r.build()
	if err != nil {
		r.recordFailure(attempt, fmt.Errorf("build: %w", err))
		return
	}

	// Refuse changes to unreloadable fields. Listener addresses and
	// mode bind to sockets that can't be rebound under the running
	// gRPC/HTTP servers; operator needs a pod restart for those.
	if err := validateReloadable(r.activeCfg.Load(), newCfg); err != nil {
		r.recordFailure(attempt, err)
		return
	}

	// Start the new pipelines. On failure, Stop any partial state so
	// background goroutines from the aborted build don't leak.
	startCtx, cancel := context.WithTimeout(parent, r.startTimeout)
	defer cancel()
	if err := newIn.Start(startCtx); err != nil {
		r.recordFailure(attempt, fmt.Errorf("inbound Start: %w", err))
		return
	}
	if err := newOut.Start(startCtx); err != nil {
		// Inbound started cleanly; unwind it before bailing.
		unwindCtx, unwindCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer unwindCancel()
		newIn.Stop(unwindCtx)
		r.recordFailure(attempt, fmt.Errorf("outbound Start: %w", err))
		return
	}

	// Commit: swap Holders and update active-config snapshot. From
	// here on, requests drain onto the new pipelines; old in-flight
	// requests keep their existing *Pipeline reference.
	oldIn := r.inbound.Load()
	oldOut := r.outbound.Load()
	r.inbound.Store(newIn)
	r.outbound.Store(newOut)
	r.activeCfg.Store(newCfg)
	r.lastHash = hash

	slog.Info("reloader: pipelines swapped",
		"sha256", hash[:12],
		"drainWindow", r.drainWindow)

	// Drain old pipelines in the background. Using context.Background
	// deliberately: parent may cancel during shutdown, and we still
	// want a bounded Stop on the old plugins so they flush cleanly.
	go func() {
		if r.drainWindow > 0 {
			time.Sleep(r.drainWindow)
		}
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer stopCancel()
		if oldIn != nil {
			oldIn.Stop(stopCtx)
		}
		if oldOut != nil {
			oldOut.Stop(stopCtx)
		}
		slog.Info("reloader: drained old pipelines", "sha256", hash[:12])
	}()

	r.recordSuccess(attempt, hash)
}

// validateReloadable returns an error when a newly-loaded config
// differs from the active one on a field that cannot be reloaded
// under the running listeners.
func validateReloadable(active, next *config.Config) error {
	if active == nil {
		return nil // first load
	}
	var diffs []string
	if active.Mode != next.Mode {
		diffs = append(diffs, fmt.Sprintf("mode (%s→%s)", active.Mode, next.Mode))
	}
	// reflect.DeepEqual rather than `!=` because ListenerConfig now
	// contains a SkipHosts []string field, and slices are not
	// comparable with `!=`. The semantic stays the same: any field
	// difference under listener.* fails reload and asks for a restart,
	// because these addresses (and skip-host patterns) are bound at
	// listener construction.
	if !reflect.DeepEqual(active.Listener, next.Listener) {
		diffs = append(diffs, "listener.*")
	}
	if len(diffs) > 0 {
		return fmt.Errorf("unreloadable field changed, pod restart required: %v", diffs)
	}
	return nil
}

func (r *Reloader) recordFailure(attempt time.Time, err error) {
	slog.Warn("reloader: reload failed", "error", err)
	cur := r.status.Load()
	next := *cur
	next.LastAttempt = attempt
	next.LastError = err.Error()
	next.ReloadsFailed++
	r.status.Store(&next)
}

func (r *Reloader) recordSuccess(attempt time.Time, hash string) {
	cur := r.status.Load()
	next := *cur
	next.LastAttempt = attempt
	next.LastSuccess = attempt
	next.LastError = ""
	next.ReloadsOK++
	next.ActiveConfigSHA256 = hash
	r.status.Store(&next)
}
