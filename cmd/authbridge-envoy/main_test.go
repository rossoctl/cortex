package main

import (
	"bytes"
	"encoding/json"
	"go/parser"
	"go/token"
	"log/slog"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/config"
)

// captureWarns runs fn with a logger recording WARN records as JSON lines.
func captureWarns(t *testing.T, fn func(*slog.Logger)) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	fn(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
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

// A cost_ledger block in this binary's config is loaded, validated, and discarded:
// authbridge-envoy builds a session store and stops there, with no ledger and no
// usage aggregator, so nothing in the block can take effect. Inert is the right
// answer for an ext_proc sidecar (per-pod day files are the wrong sink for spend),
// but inert-and-unmentioned is not — an operator who set dir and retention_days had
// no way to learn that from anything other than reading main.go.
func TestWarnCostLedgerInert_SaysSoWhenTheBlockIsPresent(t *testing.T) {
	on := true
	recs := captureWarns(t, func(l *slog.Logger) {
		warnCostLedgerInert(&config.Config{
			Mode:       config.ModeEnvoySidecar,
			CostLedger: &config.CostLedgerConfig{Enabled: &on, Dir: "/var/lib/cortex/cost", RetentionDays: 8},
		}, l)
	})
	if len(recs) != 1 {
		t.Fatalf("expected exactly 1 warn, got %d: %#v", len(recs), recs)
	}
	msg, _ := recs[0]["msg"].(string)
	// The key as it is spelled in YAML, so a log search for the setting finds it.
	if !strings.Contains(msg, "cost_ledger") {
		t.Errorf("warn must name the key the operator wrote, got %q", msg)
	}
	// "Configured but does nothing" is the whole point of the line; a message that
	// only said "cost ledger disabled" would read as the ordinary Kubernetes default.
	if !strings.Contains(msg, "INERT") {
		t.Errorf("warn must say the block is inert, not merely off, got %q", msg)
	}
	// And it has to say what to do, since the operator's intent (durable cost
	// history) is achievable — just not in this binary.
	fix, _ := recs[0]["fix"].(string)
	if !strings.Contains(fix, "authbridge-proxy") {
		t.Errorf("warn must point at the binary that honours the block, got %q", fix)
	}
}

// A block that was never set must not produce a warning: every envoy sidecar in the
// fleet runs without one, and a line on all of them would train operators to ignore
// it.
func TestWarnCostLedgerInert_SilentWhenAbsent(t *testing.T) {
	recs := captureWarns(t, func(l *slog.Logger) {
		warnCostLedgerInert(&config.Config{Mode: config.ModeEnvoySidecar}, l)
	})
	if len(recs) != 0 {
		t.Errorf("expected silence when no cost_ledger block is set, got %#v", recs)
	}
}

// The inert-by-design claim rests on this binary having nowhere to put the ledger.
// Pin that: an aggregator or ledger appearing in authbridge-envoy makes the warning
// a lie, and this is the test that should fail when someone wires one.
//
// Asserted against the parsed import list rather than behaviour because there is no
// seam to observe — the absence IS the property, and main() is not decomposed. Parsed
// rather than grepped so a comment mentioning either package cannot trip it.
func TestCostLedgerInertClaim_NoLedgerOrAggregatorIsLinkedHere(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if strings.HasSuffix(path, "/core/cost/ledger") || strings.HasSuffix(path, "/core/cost/usage") {
			t.Errorf("main.go imports %s, so cost_ledger may no longer be inert in this binary — "+
				"wire the block through and delete warnCostLedgerInert, or narrow its message", path)
		}
	}
}
