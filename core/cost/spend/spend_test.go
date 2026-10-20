package spend

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
)

// LogRecorder writes one log line per charge, carrying every attribution and token field, so the
// spend is visible before anything prices it into the ledger.
func TestLogRecorderLogsTheCharge(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	LogRecorder{}.RecordCost(Charge{
		Plugin: "context-guru", Purpose: PurposeKeepAlive, Session: "s1",
		Client: &pipeline.EventClient{Name: "claude-code", Raw: "claude-cli/2.1.14"},
		Host:   "api.example.com", Model: "claude-sonnet-5",
		Usage:  pricing.Usage{Input: 3, CacheWrite: 7, CacheRead: 48000, Output: 1},
		Status: 200, At: time.Unix(1_700_000_000, 0), Duration: 420 * time.Millisecond,
	})

	got := logs.String()
	for _, want := range []string{"plugin spend", "plugin=context-guru", "purpose=keep-alive", "session=s1",
		"agent=claude-code", "host=api.example.com", "model=claude-sonnet-5", "input=3", "cacheWrite=7",
		"cacheRead=48000", "output=1", "status=200", "ms=420"} {
		if !strings.Contains(got, want) {
			t.Errorf("log %q lacks %q", got, want)
		}
	}
	if strings.Count(got, "\n") != 1 {
		t.Errorf("want exactly one line, got %q", got)
	}
}

// A charge with no client still logs; the agent field is empty rather than a panic.
func TestLogRecorderToleratesNoClient(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	LogRecorder{}.RecordCost(Charge{Plugin: "context-guru", Purpose: PurposeCompaction, Session: "s1"})
	if !strings.Contains(logs.String(), `agent=""`) {
		t.Errorf("log %q, want an empty agent", logs.String())
	}
}
