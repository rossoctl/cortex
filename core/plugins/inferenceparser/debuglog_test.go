package inferenceparser

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// With Debug off, the completion must not reach the log sanitizer. See
// inference.TestParseRequest_DebugOffDoesNotSanitizeMessages for the fixture runes.
func TestLogInferenceFinalized_DebugOffDoesNotSanitizeCompletion(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
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
