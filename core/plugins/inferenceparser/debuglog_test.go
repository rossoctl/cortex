package inferenceparser

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// infoOnly is the production default level, Info, with no output. Debug is off, Info
// is on, so a guard on the wrong level fails the test. A text handler is not used:
// under -race its buffer pool drops items at random and changes the allocation count.
type infoOnly struct{ slog.Handler }

func (infoOnly) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelInfo }

// With Debug off, the completion must not reach the log sanitizer. See
// inference.TestParseRequest_DebugOffDoesNotSanitizeMessages for the fixture runes.
func TestLogInferenceFinalized_DebugOffDoesNotSanitizeCompletion(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(infoOnly{slog.DiscardHandler}))
	t.Cleanup(func() { slog.SetDefault(prev) })

	allocs := func(completion string) float64 {
		ext := &pipeline.InferenceExtension{Model: "m", Completion: completion}
		return testing.AllocsPerRun(20, func() { logInferenceFinalized(ext) })
	}
	control, plain := allocs("a\u0085b"), allocs("aéb")
	if control != plain {
		t.Errorf("allocs with a control rune = %v, without = %v; want equal with Debug off", control, plain)
	}
}

// The guard must not drop the line it guards.
func TestLogInferenceFinalized_DebugOnLogsCompletion(t *testing.T) {
	prev := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	logInferenceFinalized(&pipeline.InferenceExtension{Model: "m", Completion: "hi\nthere"})
	if !strings.Contains(buf.String(), `msg="inference-parser: completion" text=`) {
		t.Errorf("no completion line in the Debug log:\n%s", buf.String())
	}
}
