package edit

import (
	"context"
	"os"
	"path/filepath"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/apiclient"
)

// tempFileMaxAge is how long a stale edit tempfile is allowed to sit
// in $TMPDIR before SweepStaleTempfiles deletes it. 24h covers crash
// recovery (a user can re-open last day's edit) without letting the
// directory grow without bound.
const tempFileMaxAge = 24 * time.Hour

// SweepStaleTempfiles deletes abctl-pipeline-*.yaml tempfiles older
// than tempFileMaxAge from os.TempDir(). Errors are non-fatal — a
// best-effort cleanup at startup; the editor still works without it.
// Returns the number of files removed (for diagnostics).
func SweepStaleTempfiles() int {
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "abctl-pipeline-*.yaml"))
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

// FetchCmd returns a tea.Cmd that resolves the pod's agent name (via the
// app.kubernetes.io/name label), fetches the agent's ConfigMap, locates
// the pipeline subtree, writes the subtree to a tempfile (ready for
// $EDITOR), and emits FetchedMsg. The tempfile lives in $TMPDIR; abctl
// leaves it in place on every exit path (success, error, abort) so users
// can recover an in-progress edit.
//
// cachedCatalog is the catalog the TUI has already fetched (e.g. via
// the catalog pane). When non-nil it's used as-is to render templates.
// When nil and client is non-nil, FetchCmd fetches it inline so the
// edit experience works on first 'e' press without requiring the
// operator to open the catalog pane first. Both nil → no templates
// (used by tests and degraded server paths).
func FetchCmd(
	ctx context.Context,
	run Runner,
	client *apiclient.Client,
	namespace, pod string,
	cachedCatalog []apiclient.PluginCatalogEntry,
) tea.Cmd {
	return func() tea.Msg {
		agent, err := ResolveAgentName(ctx, run, namespace, pod)
		if err != nil {
			return FetchedMsg{Err: err}
		}
		fp, err := Fetch(ctx, run, namespace, agent)
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

		tmp, err := os.CreateTemp("", "abctl-pipeline-*.yaml")
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

// ApplyCmd returns a tea.Cmd that runs kubectl apply --server-side on
// the supplied manifest and emits AppliedMsg with the apply timestamp.
func ApplyCmd(ctx context.Context, run Runner, manifest []byte) tea.Cmd {
	return func() tea.Msg {
		at, err := Apply(ctx, run, manifest)
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

// RollbackCmd re-applies the supplied (original) manifest to undo a
// successful API write whose subsequent in-pod reload failed. The
// running pipeline never moved (the framework keeps the previous
// pipeline on build failure), so this just reconciles the ConfigMap
// on disk back to what's actually serving.
func RollbackCmd(ctx context.Context, run Runner, manifest []byte, reloadErr string) tea.Cmd {
	return func() tea.Msg {
		_, err := Apply(ctx, run, manifest)
		return RolledBackMsg{ReloadErr: reloadErr, Err: err}
	}
}

// PolledMsg is the result of PollCmd.
type PolledMsg struct {
	Result PollResult
}

// pollDeadline bounds how long PollCmd waits for an in-pod reload to
// reach a terminal state. Picked to outlast the worst-case kubelet
// ConfigMap sync (~60s) plus the framework's drain window (30s) plus
// jitter, while still surfacing a stuck reload in a reasonable time.
const pollDeadline = 120 * time.Second

// PollCmd returns a tea.Cmd that polls /reload/status until the framework
// reload completes (success or failure) or pollDeadline elapses. Emits
// PolledMsg. The deadline is enforced internally; the caller's ctx is
// only used for parent-cancellation (e.g. process shutdown).
func PollCmd(ctx context.Context, statusURL string, applyTime time.Time) tea.Cmd {
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, pollDeadline)
		defer cancel()
		return PolledMsg{Result: PollUntilReloaded(c, statusURL, applyTime)}
	}
}
