package edit

import (
	"bytes"
	"context"
	"fmt"
	"os"

	"github.com/rossoctl/cortex/core/config"
)

// ConfigWrite is one write of plugin config changes to a local Cortex config file.
type ConfigWrite struct {
	// Path is the config file the proxy was started with and watches.
	Path string
	// StatsURL is the base URL of that proxy's stats server, where /reload/status
	// says whether it took the write. Empty when no proxy answers there; the file is
	// then written and nothing is polled.
	StatsURL string
	// Changes are applied in order, each to the result of the one before.
	Changes []ConfigChange
	// Verify, when set, checks the result after config.Load has accepted it. This is
	// where a caller puts its plugin's own rules, which config.Load does not know:
	// a result Verify refuses is never written.
	Verify func(*config.Config) error
}

// WriteOutcome is how a write ended once the file was, or was not, written.
type WriteOutcome int

const (
	// WriteUnchanged: the file already held every change. Nothing was written and
	// nothing polled: the reloader ignores a byte-identical file, so its
	// last_success would never move and the poll would wait out its deadline.
	WriteUnchanged WriteOutcome = iota
	// WriteReloaded: written, and the proxy reloaded it.
	WriteReloaded
	// WriteReloadFailed: written, and the proxy refused it, keeping the
	// configuration it had. ReloadError says why. The file is put back as it was
	// (RolledBack), since a refused config left on disk is what the proxy next starts
	// from, and it may not start from it.
	WriteReloadFailed
	// WriteReloadTimedOut: written, and the proxy reported neither a reload nor a
	// failure within LocalPollDeadline. The file is left as written; the reload may
	// be slow or the state uncertain.
	WriteReloadTimedOut
	// WriteNotRunning: StatsURL was empty (no proxy to poll). The file was written.
	WriteNotRunning
	// WriteStatusUnreachable: written, and then the proxy stopped answering at
	// StatsURL before it reported a reload or a refusal. Nothing refused the change;
	// whether the proxy took it is unknown. The file is put back as after a refusal
	// (RolledBack), as the TUI's RollbackCmd does: a proxy that went away next
	// starts from the file, and this keeps that the configuration it last ran.
	// ReloadError is the poller's account of what failed.
	WriteStatusUnreachable
)

// WriteResult is what WritePluginConfig reports.
type WriteResult struct {
	Outcome WriteOutcome
	// ReloadError is the proxy's error for WriteReloadFailed, and the poller's for
	// WriteStatusUnreachable.
	ReloadError string
	// RolledBack reports, for those two outcomes, that the file was put back as it
	// was. It is false beside an error saying why it was not.
	RolledBack bool
}

// WritePluginConfig applies w.Changes to the file at w.Path and waits for the proxy
// to reload it, for at most LocalPollDeadline or until ctx is done.
//
// An error from before the write means nothing was written: a change cannot be
// made, the result does not load or fails Verify, or the file changed while it
// worked. Once the file is written the error is nil and the WriteResult says what
// the proxy did, with one exception: the proxy refused the change or stopped
// answering, and the file could not be put back, because someone changed it in the
// meantime (it is then left as found) or because the restore failed. The WriteResult
// still says what the proxy did, RolledBack is false, and the error says why the
// file was not put back, with ReloadError in it. No error quotes the file, which
// holds API keys.
func WritePluginConfig(ctx context.Context, w ConfigWrite) (WriteResult, error) {
	orig, err := os.ReadFile(w.Path)
	if err != nil {
		return WriteResult{}, fmt.Errorf("read %s: %w", w.Path, err)
	}
	updated := orig
	for _, ch := range w.Changes {
		if updated, err = SetPluginConfig(updated, ch); err != nil {
			return WriteResult{}, fmt.Errorf("%s: %w", w.Path, err)
		}
	}
	if bytes.Equal(updated, orig) {
		return WriteResult{Outcome: WriteUnchanged}, nil
	}
	if err := checkLoads(updated, w.Verify); err != nil {
		return WriteResult{}, fmt.Errorf("not writing %s: %w", w.Path, err)
	}

	store := FileStore{Path: w.Path}
	if err := store.CheckUnchanged(ctx, &FetchedPipeline{Original: orig}); err != nil {
		return WriteResult{}, err
	}
	baseline := ReloadBaseline(ctx, w.StatsURL)
	applyTime, err := store.Apply(ctx, updated)
	if err != nil {
		return WriteResult{}, err
	}
	if w.StatsURL == "" {
		return WriteResult{Outcome: WriteNotRunning}, nil
	}

	pctx, cancel := context.WithTimeout(ctx, LocalPollDeadline)
	defer cancel()
	res := PollUntilReloaded(pctx, w.StatsURL, applyTime, baseline, store.Describe().UnreachableHint)
	switch res.Status {
	case PollSuccess:
		return WriteResult{Outcome: WriteReloaded}, nil
	case PollFailure:
		out := WriteResult{Outcome: WriteReloadFailed, ReloadError: res.LastError}
		what := "the proxy refused the change"
		if res.Unreachable {
			out.Outcome, what = WriteStatusUnreachable, "the proxy stopped answering before it reported the reload"
		}
		if err := putBack(store, updated, orig); err != nil {
			return out, fmt.Errorf("%s (%s), and %w", what, res.LastError, err)
		}
		out.RolledBack = true
		return out, nil
	default:
		return WriteResult{Outcome: WriteReloadTimedOut}, nil
	}
}

// putBack restores orig over written after a reload that did not take, but only
// while the file still holds exactly written. A person who edited it meanwhile, in
// an editor or another agentop, made a change of their own, which a restore would
// silently undo; that file is left as found. Its errors name the path and never the
// file's bytes.
func putBack(store FileStore, written, orig []byte) error {
	current, err := os.ReadFile(store.Path)
	if err != nil {
		return fmt.Errorf("re-reading %s to put it back failed, so it may still hold the change; check it by hand: %w", store.Path, err)
	}
	if !bytes.Equal(current, written) {
		return fmt.Errorf("%s changed while the proxy was reloading it, so it was left as found; "+
			"check it by hand before the proxy next starts", store.Path)
	}
	if _, err := store.Apply(context.Background(), orig); err != nil {
		return fmt.Errorf("putting %s back failed, so it still holds the change; fix it by hand before the proxy next starts: %w", store.Path, err)
	}
	return nil
}

// checkLoads reports whether b loads as a Cortex config and passes verify, as the
// proxy would load it. Checked from a private temp file, never beside the config:
// the proxy watches that directory, and b can hold API keys.
func checkLoads(b []byte, verify func(*config.Config) error) error {
	f, err := os.CreateTemp("", "agentop-config-check-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	cfg, err := config.Load(f.Name())
	if err != nil {
		return fmt.Errorf("the result would not load: %w", err)
	}
	if verify != nil {
		return verify(cfg)
	}
	return nil
}
