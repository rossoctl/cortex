package edit

import (
	"context"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// FetchedMsg is the result of FetchCmd. On success: Fetched and TempPath
// are both set, Err is nil. On failure: Err is populated, others are zero.
type FetchedMsg struct {
	Fetched  *FetchedPipeline
	TempPath string // path to the tempfile holding just the pipeline subtree
	Err      error
}

// FetchCmd returns a tea.Cmd that fetches the agent's ConfigMap, locates
// the pipeline subtree, writes the subtree to a tempfile (ready for
// $EDITOR), and emits FetchedMsg. The tempfile lives in $TMPDIR; abctl
// leaves it in place on every exit path (success, error, abort) so users
// can recover an in-progress edit.
func FetchCmd(ctx context.Context, run Runner, namespace, agent string) tea.Cmd {
	return func() tea.Msg {
		fp, err := Fetch(ctx, run, namespace, agent)
		if err != nil {
			return FetchedMsg{Err: err}
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
		path := tmp.Name()
		if err := tmp.Close(); err != nil {
			return FetchedMsg{Err: err}
		}
		return FetchedMsg{Fetched: fp, TempPath: path}
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

// PolledMsg is the result of PollCmd.
type PolledMsg struct {
	Result PollResult
}

// PollCmd returns a tea.Cmd that polls /reload/status until the framework
// reload completes (success or failure) or ctx expires. Emits PolledMsg.
//
// Caller should construct ctx with a 120s WithTimeout so the poll
// terminates if kubelet doesn't sync within a reasonable window.
func PollCmd(ctx context.Context, statusURL string, applyTime time.Time) tea.Cmd {
	return func() tea.Msg {
		return PolledMsg{Result: PollUntilReloaded(ctx, statusURL, applyTime)}
	}
}
