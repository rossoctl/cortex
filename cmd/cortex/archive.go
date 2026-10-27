package main

import (
	"cmp"
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"time"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
)

// sessionArchiveDirName is the archive's directory under ~/.cortex, beside the cost ledger's.
const sessionArchiveDirName = "sessions"

// sessionArchiveDir is where the session archive lives: always ~/.cortex/sessions. There is no
// config for it, because in this release the archive is a laptop feature — see
// config.SessionArchiveConfig.
func sessionArchiveDir() (string, error) {
	dir, err := defaultCortexDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sessionArchiveDirName), nil
}

// sessionArchiveRuns decides whether the session archive runs, and says why.
//
// ONLY ON A LOCAL INSTALL. A laptop is where a restart costs a user their history and where the
// user can read it back (agentop lists it) and erase it (X, or DELETE /v1/sessions). Elsewhere it is
// off even when asked for — raw prompts on a cluster's volume need a decision of their own, and a
// mounted path is not one. On a local install, config.ArchiveRunsOnLocalInstall decides.
func sessionArchiveRuns(cfg *config.Config, configPath string) (bool, string) {
	a := cfg.Session.Archive
	explicit := a != nil && a.Enabled != nil
	if !startedFromLocalInstall(configPath) {
		switch {
		case explicit && !*a.Enabled:
			return false, "session.archive.enabled is false"
		case explicit:
			return false, "the session archive is a laptop feature in this release and runs only from a config inside ~/.cortex"
		}
		return false, "not a local install"
	}
	return cfg.ArchiveRunsOnLocalInstall()
}

// closeArchiveOnFatal finishes the archive's open segments before a fatal exit, for the reason
// closeLedgerOnFatal flushes the ledger: log.Fatalf ends in os.Exit, which runs no defers. Nil
// whenever no archive is open.
var closeArchiveOnFatal func()

// openSessionArchive opens the archive and registers it on the store when sessionArchiveRuns
// says it should, and returns nil otherwise. A failure to open is a warning, not a fatal error:
// the archive is observability, and refusing to start the proxy over it would trade a nicety for
// an outage.
func openSessionArchive(cfg *config.Config, configPath string, sessions *session.Store) *archive.Archive {
	run, why := sessionArchiveRuns(cfg, configPath)
	if !run {
		if a := cfg.Session.Archive; a != nil && a.Enabled != nil && *a.Enabled {
			slog.Warn("session archive will NOT run despite session.archive.enabled: true", "reason", why)
		} else {
			slog.Info("session archive disabled — sessions do not survive a restart or eviction", "reason", why)
		}
		return nil
	}
	dir, err := sessionArchiveDir()
	if err != nil {
		slog.Warn("session archive disabled — cannot determine where to write it", "error", err)
		return nil
	}
	var ac config.SessionArchiveConfig
	if cfg.Session.Archive != nil {
		ac = *cfg.Session.Archive
	}
	arch, err := archive.Open(dir, archive.WithRetentionDays(ac.RetentionDays), archive.WithMaxBytes(ac.MaxBytes))
	if err != nil {
		slog.Warn("session archive disabled — could not open it", "dir", dir, "error", err,
			"effect", "sessions do not survive a restart or eviction")
		return nil
	}
	sessions.AddRecorder(arch)
	closeArchiveOnFatal = func() {
		if cerr := arch.Close(); cerr != nil {
			slog.Warn("session archive: final flush failed during a fatal startup error", "error", cerr)
		}
	}
	st := arch.Stats()
	slog.Info("session archive enabled — sessions survive a restart and the store's eviction",
		"dir", dir, "retentionDays", st.RetentionDays, "maxBytes", st.MaxBytes, "bytes", st.Bytes,
		"holds", "raw prompts, completions and tool results; readable by anyone who can read this directory",
		"default", why)
	return arch
}

// usageReplayBudget bounds the replay of the session archive into the usage ring. The replay runs
// beside live traffic, so this delays nothing; it only stops a replay that cannot finish from
// holding six hours of reduced events indefinitely. Over it the replay is abandoned whole; see
// replayUsage.
const usageReplayBudget = 60 * time.Second

// replayUsage refills the usage ring — LAST 1H, the usage pane's duration windows — from the
// session archive, so a restart leaves them as they were. It reads the last usage.MaxWindow of
// archived events in set, reduces each with usage.ReplayCopy, sorts them by time and feeds them to
// agg.Replay.
//
// BESIDE LIVE TRAFFIC, so it costs no readiness. Three things keep that correct. set was taken
// before any listener started, and a process never appends to a segment it did not open, so
// nothing this process records is read back and counted twice. agg is the private ring
// usage.Aggregator.BeginReplay returned, so feeding in time order makes eviction and request
// pairing choose as they did live. And the live aggregator logs every clear, delete and rename
// until FinishReplay applies them to agg, so an erased session does not come back with the merge.
//
// NEVER THROUGH THE STORE: the store would hand every event to the cost ledger and the archive
// again, writing durable rows twice. agg.Replay touches the ring and nothing else, and this takes
// no store or ledger to reach either.
//
// ALL OR NOTHING. A read that does not finish within ctx is abandoned before anything is fed, and
// the ring keeps only live traffic: a partial replay would show a LAST 1H that is too low as if it
// were real.
func replayUsage(ctx context.Context, set archive.ReplaySet, agg *usage.Aggregator) (events, sessions, incomplete int, err error) {
	type replayed struct {
		id string
		e  pipeline.SessionEvent
	}
	// About 1KB a kept event (see usage.ReplayCopy), held until this returns. Keeping the decoded
	// event instead would hold six hours of conversations at once.
	var evs []replayed
	incomplete, err = set.Read(ctx, func(id string, e *pipeline.SessionEvent) {
		evs = append(evs, replayed{id, usage.ReplayCopy(e)})
	})
	if err != nil {
		return 0, 0, incomplete, err
	}
	// Seq breaks a tie within one session, where it is the order the store appended in; across
	// sessions a tie has no order to keep.
	slices.SortStableFunc(evs, func(x, y replayed) int {
		if c := x.e.At.Compare(y.e.At); c != 0 {
			return c
		}
		return cmp.Compare(x.e.Seq, y.e.Seq)
	})
	seen := make(map[string]bool)
	for i := range evs {
		if agg.Replay(evs[i].id, &evs[i].e) {
			events++
			seen[evs[i].id] = true
		}
	}
	return events, len(seen), incomplete, nil
}

// startUsageReplay takes the replay's segment list — before any listener starts, which is what
// keeps live traffic out of it — and replays it into live in the background, within
// usageReplayBudget. The returned channel closes when the replay has merged or given up.
func startUsageReplay(arch *archive.Archive, live *usage.Aggregator) <-chan struct{} {
	start := time.Now()
	set := arch.ReplaySources(start.Add(-usage.MaxWindow))
	priv := live.BeginReplay()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), usageReplayBudget)
		defer cancel()
		events, sessions, incomplete, err := replayUsage(ctx, set, priv)
		took := time.Since(start)
		if err != nil {
			live.AbandonReplay()
			slog.Warn("usage: did not replay the session archive into the usage ring; LAST 1H and the usage pane count only traffic since the start",
				"error", err, "budget", usageReplayBudget, "took", took)
			return
		}
		live.FinishReplay(priv)
		slog.Info("usage: replayed the session archive into the usage ring", "window", usage.MaxWindow,
			"events", events, "sessions", sessions, "incompleteSegments", incomplete, "took", took)
	}()
	return done
}
