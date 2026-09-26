package config

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// captureWarns runs fn with a logger that records warning records into a
// JSON-line buffer, then returns the captured records.
func captureWarns(t *testing.T, fn func(*slog.Logger)) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	fn(logger)
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func TestWarnEmptyPipelines_BothEmpty(t *testing.T) {
	cfg := &Config{Mode: ModeEnvoySidecar}
	recs := captureWarns(t, func(l *slog.Logger) { WarnEmptyPipelines(cfg, l) })
	if len(recs) != 2 {
		t.Fatalf("expected 2 warn records, got %d: %#v", len(recs), recs)
	}
	if !strings.Contains(recs[0]["msg"].(string), "inbound pipeline has no plugins") {
		t.Errorf("first record should be the inbound warn, got: %v", recs[0]["msg"])
	}
	if !strings.Contains(recs[1]["msg"].(string), "outbound pipeline has no plugins") {
		t.Errorf("second record should be the outbound warn, got: %v", recs[1]["msg"])
	}
}

func TestWarnEmptyPipelines_OnlyInboundEmpty(t *testing.T) {
	cfg := &Config{
		Mode: ModeEnvoySidecar,
		Pipeline: PipelineConfig{
			Outbound: PipelineStageConfig{Plugins: []PluginEntry{{Name: "token-exchange"}}},
		},
	}
	recs := captureWarns(t, func(l *slog.Logger) { WarnEmptyPipelines(cfg, l) })
	if len(recs) != 1 {
		t.Fatalf("expected 1 warn record, got %d", len(recs))
	}
	if !strings.Contains(recs[0]["msg"].(string), "inbound pipeline has no plugins") {
		t.Errorf("record should be the inbound warn, got: %v", recs[0]["msg"])
	}
}

func TestWarnEmptyPipelines_OnlyOutboundEmpty(t *testing.T) {
	cfg := &Config{
		Mode: ModeEnvoySidecar,
		Pipeline: PipelineConfig{
			Inbound: PipelineStageConfig{Plugins: []PluginEntry{{Name: "jwt-validation"}}},
		},
	}
	recs := captureWarns(t, func(l *slog.Logger) { WarnEmptyPipelines(cfg, l) })
	if len(recs) != 1 {
		t.Fatalf("expected 1 warn record, got %d", len(recs))
	}
	if !strings.Contains(recs[0]["msg"].(string), "outbound pipeline has no plugins") {
		t.Errorf("record should be the outbound warn, got: %v", recs[0]["msg"])
	}
}

func TestWarnEmptyPipelines_NeitherEmpty(t *testing.T) {
	cfg := &Config{
		Mode: ModeEnvoySidecar,
		Pipeline: PipelineConfig{
			Inbound:  PipelineStageConfig{Plugins: []PluginEntry{{Name: "jwt-validation"}}},
			Outbound: PipelineStageConfig{Plugins: []PluginEntry{{Name: "token-exchange"}}},
		},
	}
	recs := captureWarns(t, func(l *slog.Logger) { WarnEmptyPipelines(cfg, l) })
	if len(recs) != 0 {
		t.Fatalf("expected 0 warn records when both stages are populated, got %d: %#v", len(recs), recs)
	}
}

func TestWarnEmptyPipelines_NilLoggerUsesDefault(t *testing.T) {
	// Smoke test: passing nil should not panic. We don't assert on the
	// default logger's output here (it goes wherever slog.Default is
	// pointed).
	cfg := &Config{Mode: ModeEnvoySidecar}
	WarnEmptyPipelines(cfg, nil)
}
