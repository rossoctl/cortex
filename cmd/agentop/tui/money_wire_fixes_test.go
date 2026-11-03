package tui

import (
	"math"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
)

// A clamped total is a floor, so the AGENTS cell marks it the way the sessions pane does.
func TestAgentCostCellIn_MarksAClampedTotal(t *testing.T) {
	c := usage.Counts{Requests: 1, PricedRequests: 1, CostMicros: math.MaxInt64, Saturated: true}
	got := agentCostCellIn(c, nil, agentsCostWidth)
	if !strings.HasSuffix(got, partialMarker) {
		t.Errorf("agentCostCellIn(saturated) = %q, want a %q suffix: a clamped total must not read as exact", got, partialMarker)
	}
	unclamped := c
	unclamped.Saturated = false
	if got := agentCostCellIn(unclamped, nil, agentsCostWidth); strings.HasSuffix(got, partialMarker) {
		t.Errorf("agentCostCellIn(not saturated) = %q, want no %q suffix", got, partialMarker)
	}
}

// A negative cost is a broken producer, and every other money surface refuses to print one.
func TestAgentCostCell_RefusesANegativeCost(t *testing.T) {
	c := usage.Counts{Requests: 1, PricedRequests: 1, CostMicros: -5_000_000}
	if got := agentCostCell(c); got != emptyCell {
		t.Errorf("agentCostCell(negative) = %q, want %q", got, emptyCell)
	}
}

// METHOD carries the model name from the request body, so its cut must land on a rune
// boundary and fit the column by display width, not by bytes.
func TestEventMethod_TruncatesByDisplayWidth(t *testing.T) {
	for _, model := range []string{
		"モデル名モデル名モデル名モデル名モデル名",                // 3 bytes and 2 columns per rune
		"héllo-wörld-ünïcode-model-name-long", // 2-byte runes, 1 column each
	} {
		e := pipeline.SessionEvent{Inference: &pipeline.InferenceExtension{Model: model}}
		got := eventMethod(e)
		if !utf8.ValidString(got) {
			t.Errorf("eventMethod(%q) = %q, not valid UTF-8", model, got)
		}
		if w := lipgloss.Width(got); w > methodColWidth {
			t.Errorf("eventMethod(%q) = %q, %d columns wide, want <= %d", model, got, w, methodColWidth)
		}
	}
}

// The timeline hides this machine's own hosts only, by address, not by name prefix.
func TestIsLoopback_MatchesAddressesNotPrefixes(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1:47600":    true,
		"localhost:8080":     true,
		"localhost":          true,
		"[::1]":              true,
		"[::1]:47600":        true,
		"::1":                true,
		"127.0.0.2":          true,
		"localhost.evil.com": false,
		"127.evil.com":       false,
		"api.anthropic.com":  false,
		"0.0.0.0:80":         false,
	} {
		if got := isLoopback(host); got != want {
			t.Errorf("isLoopback(%q) = %v, want %v", host, got, want)
		}
	}
}
