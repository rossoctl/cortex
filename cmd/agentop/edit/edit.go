package edit

import (
	"context"
	"os"
	"path/filepath"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
)

// tempFileMaxAge is how long a stale edit tempfile is allowed to sit
// in $TMPDIR before SweepStaleTempfiles deletes it. 24h covers crash
// recovery (a user can re-open last day's edit) without letting the
// directory grow without bound.
const tempFileMaxAge = 24 * time.Hour

// SweepStaleTempfiles deletes agentop-pipeline-*.yaml tempfiles older
// than tempFileMaxAge from os.TempDir(). Errors are non-fatal — a
// best-effort cleanup at startup; the editor still works without it.
// Returns the number of files removed (for diagnostics).
func SweepStaleTempfiles() int {
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "agentop-pipeline-*.yaml"))
	if err != nil {
		return 0
	}
	cutoff := time.Now().Add(-tempFileMaxAge)
	n := 0
	for _, p := range matches {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if fi.ModTime().Before(cutoff) {
			if err := os.Remove(p); err == nil {
				n++
			}
		}
	}
	return n
}

// FetchedMsg is the result of FetchCmd. On success: Fetched and TempPath
// are both set, Err is nil. On failure: Err is populated, others are zero.
//
// Catalog carries the catalog used to render templates, so the TUI can
// cache it into m.catalog after a fresh fetch. Nil when the caller
// supplied a cached catalog or no client.
type FetchedMsg struct {
	Fetched  *FetchedPipeline
	TempPath string // path to the tempfile holding just the pipeline subtree
	Catalog  *apiclient.PluginCatalog
	Err      error
}

// FetchCmd returns a tea.Cmd that fetches the runtime YAML from store,
// locates the pipeline subtree, writes the subtree to a tempfile (ready for
// $EDITOR), and emits FetchedMsg. The tempfile lives in $TMPDIR; agentop
// leaves it in place on every exit path (success, error, abort) so users
// can recover an in-progress edit.
//
// Where the YAML comes from is the store's business: a pod's ConfigMap via
// kubectl, or the config file of a local Cortex. Everything after the fetch
// is identical either way.
//
// cachedCatalog is the catalog the TUI has already fetched (e.g. via
// the catalog pane). When non-nil it's used as-is to render templates.
// When nil and client is non-nil, FetchCmd fetches it inline so the
// edit experience works on first 'e' press without requiring the
// operator to open the catalog pane first. Both nil → no templates
// (used by tests and degraded server paths).
func FetchCmd(
	ctx context.Context,
	store Store,
	client *apiclient.Client,
	cachedCatalog []apiclient.PluginCatalogEntry,
) tea.Cmd {
	return func() tea.Msg {
		fp, err := store.Fetch(ctx)
		if err != nil {
			return FetchedMsg{Err: err}
		}

		// Resolve the catalog: prefer cached, otherwise fetch inline.
		// Catalog-fetch failure is non-fatal — the edit still opens
		// without templates, mirroring the older "no catalog" path.
		catalog := cachedCatalog
		var freshCatalog *apiclient.PluginCatalog
		if catalog == nil && client != nil {
			if c, err := client.GetPluginCatalog(ctx); err == nil && c != nil {
				freshCatalog = c
				catalog = c.Plugins
			}
		}

		tmp, err := os.CreateTemp("", "agentop-pipeline-*.yaml")
		if err != nil {
			return FetchedMsg{Err: err}
		}
		subtree := fp.InnerYAML[fp.PipelineStart:fp.PipelineEnd]
		if _, err := tmp.Write(subtree); err != nil {
			tmp.Close()
			return FetchedMsg{Err: err}
		}
		if templates := RenderTemplates(catalog); len(templates) > 0 {
			if _, err := tmp.Write(templates); err != nil {
				tmp.Close()
				return FetchedMsg{Err: err}
			}
		}
		path := tmp.Name()
		if err := tmp.Close(); err != nil {
			return FetchedMsg{Err: err}
		}
		return FetchedMsg{Fetched: fp, TempPath: path, Catalog: freshCatalog}
	}
}

// AppliedMsg is the result of ApplyCmd.
type AppliedMsg struct {
	ApplyTime time.Time
	Err       error
}

// ApplyCmd returns a tea.Cmd that writes the supplied payload back through
// store and emits AppliedMsg with the apply timestamp.
//
// orig is the fetch this payload was derived from. When the store implements
// ConflictChecker, it is consulted first and a concurrent write aborts the apply
// instead of overwriting it. Checked here rather than at the keypress so the
// re-read happens off the Update loop, next to the write it guards.
func ApplyCmd(ctx context.Context, store Store, orig *FetchedPipeline, payload []byte) tea.Cmd {
	return func() tea.Msg {
		if cc, ok := store.(ConflictChecker); ok && orig != nil {
			if err := cc.CheckUnchanged(ctx, orig); err != nil {
				return AppliedMsg{Err: err}
			}
		}
		at, err := store.Apply(ctx, payload)
		return AppliedMsg{ApplyTime: at, Err: err}
	}
}

// RolledBackMsg is the result of RollbackCmd. ReloadErr is the error
// from the failed in-pod reload (the reason we're rolling back); Err
// is any error from the rollback Apply itself.
type RolledBackMsg struct {
	ReloadErr string
	Err       error
}

// RollbackCmd re-applies the supplied (original) payload to undo a
// successful write whose subsequent reload failed. The running pipeline
// never moved (the framework keeps the previous pipeline on build
// failure), so this just reconciles the stored config back to what's
// actually serving.
//
// Deliberately skips ConflictChecker: the file has changed since the fetch —
// this apply changed it — so the check would refuse every rollback. That leaves
// the documented caveat below intact for the narrow case of a third party
// writing between the forward apply and this one.
func RollbackCmd(ctx context.Context, store Store, payload []byte, reloadErr string) tea.Cmd {
	return func() tea.Msg {
		_, err := store.Apply(ctx, payload)
		return RolledBackMsg{ReloadErr: reloadErr, Err: err}
	}
}

// PolledMsg is the result of PollCmd.
type PolledMsg struct {
	Result PollResult
}

// ClusterPollDeadline bounds a ConfigMap edit's wait: the worst-case kubelet
// sync (~60s) plus jitter, doubled. The verdict does not wait on the drain —
// the reloader records success right after the Holder swap and drains the old
// pipelines in a goroutine.
const ClusterPollDeadline = 120 * time.Second

// LocalPollDeadline bounds a local file edit's wait. The proxy is already
// watching the file, so the path is fsnotify → a 250ms debounce → build and
// start the pipelines → swap → record success, and it lands in about a second.
//
// 30s is ~30x that, which leaves generous room for plugin Start doing real work
// (a JWKS fetch, say) while not making a failure take two minutes to report.
// Only the failure path is affected: the poll returns the moment last_success
// advances, so a healthy reload never waits on this at all.
const LocalPollDeadline = 30 * time.Second

// PollCmd returns a tea.Cmd that polls /reload/status until the framework
// reload completes (success or failure) or the target's deadline elapses.
// Emits PolledMsg. The deadline is enforced internally; the caller's ctx is
// only used for parent-cancellation (e.g. process shutdown).
//
// Takes the whole Target rather than a growing list of scalars — the deadline
// and the unreachable wording both come from it, and passing it entire keeps
// them from being sourced from different stores.
func PollCmd(ctx context.Context, statusURL string, applyTime time.Time, tg Target) tea.Cmd {
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, tg.Deadline())
		defer cancel()
		return PolledMsg{Result: PollUntilReloaded(c, statusURL, applyTime, tg.UnreachableHint)}
	}
}

// Deadline is PollDeadline with a floor, so a zero-valued Target (a test, or a
// nil store) waits the conservative cluster ceiling rather than returning
// instantly and reporting a timeout that never happened.
//
// Exported because the TUI reports the value it waited in the timeout message,
// and that has to be the same number PollCmd used.
func (t Target) Deadline() time.Duration {
	if t.PollDeadline <= 0 {
		return ClusterPollDeadline
	}
	return t.PollDeadline
}
