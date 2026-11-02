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
	"slices"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/pipeline"
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

// WithOnCommit registers a hook run once a reload has been ACCEPTED — after the unreloadable-field
// check and after both pipelines have started — with the config that took effect.
//
// IT EXISTS BECAUSE A BUILDER'S SIDE EFFECTS OTHERWISE LAND TOO EARLY. build runs before
// validateReloadable, so anything it applies to live traffic is applied by a save this reloader is
// about to REFUSE: the pricing-table swap sat at the end of build, and a save touching both pricing.*
// and cost_ledger.* was rejected — pipelines untouched, /config still reporting the old rates — while
// requests were already being priced from the rejected file. A failed Start had the same shape.
//
// So the rule is: build may PREPARE, and only a hook here may APPLY. Called on the reload goroutine,
// which is serialised (see reloadOnce), so a hook needs no locking of its own.
func WithOnCommit(f func(*config.Config)) Option { return func(r *Reloader) { r.onCommit = f } }

// WithExtraWatch makes the pipelines depend on more than the config file: any event
// in one of the folders dirs names triggers a reload, and fingerprint's result over
// those folders joins the config's content in the hash that decides whether
// anything changed. The script plugin uses it for its folders, so adding or editing
// a script reloads exactly as a config edit does.
//
// dirs is called with the startup config at Start, and again with each config a
// reload accepts, so a folder a config edit adds is watched from that reload on.
// A folder that does not exist yet is created so it can be watched: one the user
// creates later would otherwise go unseen until the next config edit. A folder the
// config stops naming stays watched until restart. fingerprint is called on every
// candidate reload, with every folder watched so far.
func WithExtraWatch(dirs func(*config.Config) []string, fingerprint func(dirs []string) string) Option {
	return func(r *Reloader) { r.extraDirs, r.fingerprint = dirs, fingerprint }
}

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
	// onCommit is run after a reload is accepted; see WithOnCommit.
	onCommit func(*config.Config)
	// extraDirs and fingerprint widen what a reload depends on; see WithExtraWatch.
	extraDirs   func(*config.Config) []string
	fingerprint func(dirs []string) string
	// watched is every WithExtraWatch folder with a watch on it, cleaned, in the
	// order added. Written by Start and then only by the watch goroutine; hash
	// reads it on that goroutine, or in New before either exists.
	watched []string

	status    atomic.Pointer[Status]
	activeCfg atomic.Pointer[config.Config]

	// resolvedBase is the base name of configPath with symlinks followed, set
	// by Start when it differs enough to need its own watch, and read only by
	// the watch goroutine Start then launches. Empty when configPath is not a
	// symlink, or when the real file's directory could not be watched.
	resolvedBase string

	// serialized by the watch goroutine
	lastHash string
	// startData is the config file as New read it.
	startData []byte
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
		r.lastHash = r.hash(data)
		r.startData = data
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

	// Also watch the directory holding the real file when configPath is a
	// symlink pointing elsewhere.
	//
	// Watching only the link's directory means a writer that correctly writes
	// THROUGH the link — replacing the target rather than the link, which is
	// what preserves a config symlinked into a dotfiles repo — changes nothing
	// in this directory. On Linux, inotify reports directory-entry changes, so
	// no event arrives and the reload never happens: agentop's editor then times
	// out and rolls a correct edit back two minutes later, and `agentop tools scan
	// --write` / the migration `agentop service install` runs land silently unreloaded.
	// (macOS happens to survive this: kqueue watches the resolved file, so replacing
	// it reports on the link. Relying on that is relying on the platform.)
	//
	// ADDITIVE on purpose — the watch and filter above are untouched. A
	// ConfigMap mount resolves to a TIMESTAMPED directory (..2026_05_08_xx)
	// that is deleted on every update, so making the resolved path primary
	// would break cluster reloads; that case keeps working through the ..data
	// alias exactly as before.
	//
	// What the extra watch actually does under a ConfigMap, stated plainly
	// rather than as "harmless": it lands on the current generation directory,
	// and dies with it at the first update — Start runs once, so it is never
	// re-added. Before that it admits events for config.yaml INSIDE the
	// generation dir, which previously never arrived; those are redundant
	// rather than wrong, since reloadOnce hashes the content and dedups an
	// unchanged file. So the mechanism is "the watch dies on the first update",
	// and it costs nothing only because the cluster path never depended on it.
	// Re-arming it per generation would be real work for no benefit; if a
	// future change ever makes the cluster path rely on the resolved watch,
	// that has to be revisited, not assumed.
	if real, err := filepath.EvalSymlinks(r.configPath); err == nil {
		if rdir := filepath.Dir(real); rdir != dir {
			if aerr := watcher.Add(rdir); aerr != nil {
				// Non-fatal: the link's own directory is still watched, so this
				// is a degradation, not a reason to refuse to start.
				slog.Warn("reloader: cannot watch the symlink target's directory; "+
					"edits written through the link may not be observed",
					"link", r.configPath, "target", real, "error", aerr)
			} else {
				r.resolvedBase = filepath.Base(real)
				slog.Info("reloader: also watching the symlink target",
					"link", r.configPath, "target", real)
			}
		}
	}

	if r.extraDirs != nil {
		r.watchExtra(watcher, r.extraDirs(r.activeCfg.Load()))
		// New hashed before any folder was watched, so its hash left them out.
		// Seeded again with them, or the first event after startup would see a
		// change that is not there and rebuild every pipeline.
		r.seedHash()
	}

	go r.watchLoop(ctx, watcher)
	slog.Info("reloader: watching for config changes",
		"path", r.configPath, "dir", dir, "drainWindow", r.drainWindow)
	return nil
}

// watchExtra adds a watch on each of dirs not watched yet, creating it first. A
// folder that cannot be created or watched is skipped with a WARN: its scripts
// still load with each reload, and an edit in it reloads only with the config.
func (r *Reloader) watchExtra(watcher *fsnotify.Watcher, dirs []string) {
	for _, d := range dirs {
		d = filepath.Clean(d)
		if slices.Contains(r.watched, d) {
			continue
		}
		if err := os.MkdirAll(d, 0o700); err != nil {
			slog.Warn("reloader: cannot create a watched folder", "dir", d, "error", err)
			continue
		}
		if err := watcher.Add(d); err != nil {
			slog.Warn("reloader: cannot watch a folder; changes in it reload only with the config", "dir", d, "error", err)
			continue
		}
		r.watched = append(r.watched, d)
	}
}

// seedHash sets the hash a reload compares against from the config New read, and
// publishes it as the active config's.
func (r *Reloader) seedHash() {
	if r.startData == nil {
		return
	}
	r.lastHash = r.hash(r.startData)
	next := *r.status.Load()
	next.ActiveConfigSHA256 = r.lastHash
	r.status.Store(&next)
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
			//
			// resolvedBase admits the real file behind a symlinked config; see
			// Start. Empty unless such a watch was added, and filepath.Base
			// never returns "", so the comparison is inert in that case.
			evBase := filepath.Base(ev.Name)
			if evBase != base && evBase != "..data" &&
				(r.resolvedBase == "" || evBase != r.resolvedBase) &&
				!slices.Contains(r.watched, filepath.Dir(ev.Name)) {
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
			// A config that names a new folder is watched from here. Added on this
			// goroutine, which owns the watcher; fsnotify's inotify and kqueue
			// backends send no event while holding the lock Add takes, so this
			// cannot block on the channel this loop drains.
			r.reloadOnce(ctx, watcher)
		}
	}
}

// reloadOnce runs one reload attempt from start to finish. Any error
// leaves the currently-active pipelines untouched and is reported via
// Status. Only the debounced timer fires this, so two reloadOnce calls
// cannot overlap.
func (r *Reloader) reloadOnce(parent context.Context, watcher *fsnotify.Watcher) {
	attempt := time.Now()

	// Content-hash dedup: ignore mtime-only changes (e.g., a touch).
	data, err := os.ReadFile(r.configPath)
	if err != nil {
		r.recordFailure(attempt, fmt.Errorf("read: %w", err))
		return
	}
	hash := r.hash(data)
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
	// gRPC/HTTP servers; the cost ledger is a writer opened once and
	// held by the session store. Operator needs a pod restart for those.
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

	// After the swap, so a hook cannot observe a config that was refused, and before the log line, so
	// "pipelines swapped" means everything this reload applies has been applied.
	if r.onCommit != nil {
		r.onCommit(newCfg)
	}

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

	if r.extraDirs != nil {
		r.watchExtra(watcher, r.extraDirs(newCfg))
	}
	r.recordSuccess(attempt, hash)
}

// hash is what decides whether a reload has anything to do: the config's bytes,
// and, with WithExtraWatch, the fingerprint of everything else the pipelines are
// built from. While the fingerprint is empty it is the config's sha256, as it
// always was. Once there is something else to fingerprint, /reload/status's
// active_config_sha256 is the digest of both — it identifies what is running,
// which is no longer the config file alone.
func (r *Reloader) hash(data []byte) string {
	if r.fingerprint != nil {
		if fp := r.fingerprint(r.watched); fp != "" {
			data = append(append(append([]byte(nil), data...), 0), fp...)
		}
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
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
	// cost_ledger.* for the same reason, and it is the more dangerous of the two to
	// leave out. The ledger is a *ledger.Writer constructed once at startup and
	// handed to the session store as a Recorder, so nothing here can reach the
	// running writer. Without this branch an edit was ACCEPTED: ReloadsOK
	// incremented, /reload/status published a new ActiveConfigSHA256, and /config
	// served the new values while the writer kept appending to the old directory
	// under the old retention. An operator who edits `enabled: false` to stop
	// recording cost history reads all three of those as confirmation that it
	// stopped. Refusing the reload and asking for a restart is the honest answer.
	// Pointer-to-struct, so DeepEqual covers nil↔present as well as field drift.
	if !reflect.DeepEqual(active.CostLedger, next.CostLedger) {
		diffs = append(diffs, "cost_ledger.*")
	}
	// session.* has the same shape, and it was missing for the same reason: nobody
	// checked. Every consumer of the block reads it exactly once at startup —
	// session.New(lim.TTL, lim.MaxEvents, lim.MaxSessions) in each cmd main, and
	// forwardproxy.Server.SessionIDHeaders assigned before ListenAndServe — so there
	// is no live object for a reload to reach here either. Not one field of it is
	// reloadable, which is why the whole block is compared rather than a subset.
	// DeepEqual because Enabled is a *bool and IDHeaders a []string, and the
	// nil-versus-empty distinction on IDHeaders is load-bearing (see SessionConfig).
	if !reflect.DeepEqual(active.Session, next.Session) {
		diffs = append(diffs, "session.*")
	}
	// mtls.* also needs a restart: the TLS dialer is built once in main and handed to
	// the forward proxy listener. On an mTLS proxy, a reload that removes the mtls block
	// while adding a WritesDestination plugin would let that plugin through, violating
	// the TLS-in-TLS constraint forwardproxy.Support is built to enforce. Like cost_ledger
	// and session, mtls.* is unreloadable in any direction (present↔absent or field change).
	// Pointer-to-struct, so DeepEqual covers nil↔present as well as field drift.
	if !reflect.DeepEqual(active.MTLS, next.MTLS) {
		diffs = append(diffs, "mtls.*")
	}
	// tls_bridge.* is built once at boot (the bridge engine in each cmd main reads the
	// block at startup and nothing reloads it). Accepting an edit here is the
	// "worse than a refusal" case: ReloadsOK goes up and /config serves the new block
	// while the old bridge keeps decrypting — an operator who adds a host to
	// passthrough_hosts reads the reload as confirmation that it stopped being
	// intercepted, and it hasn't (#1561).
	if !reflect.DeepEqual(active.TLSBridge, next.TLSBridge) {
		diffs = append(diffs, "tls_bridge.*")
	}
	// spiffe.* likewise: the Provider is constructed once at boot and handed to the
	// plugins through Deps at build time. A reload that changes the spiffe block
	// rebuilds plugins against the NEW provider-shaped config while the running
	// Provider keeps its boot-time settings.
	if !reflect.DeepEqual(active.SPIFFE, next.SPIFFE) {
		diffs = append(diffs, "spiffe.*")
	}
	// memory.* too: the soft limit is applied once at boot (debug.SetMemoryLimit
	// before any listener starts). An accepted-but-unapplied edit here would be
	// the "worse than a refusal" shape an operator reads as confirmation (#1541).
	if !reflect.DeepEqual(active.Memory, next.Memory) {
		diffs = append(diffs, "memory.*")
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
