package litellm_budgettrack

import (
	"encoding/json"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// Verifies the plugin is recognized as a StreamingResponder through the same
// wrapping the real build applies — the gap that unit tests calling
// OnResponseFrame directly cannot catch.
func TestPipelineDetectsStreamingResponder(t *testing.T) {
	raw, _ := json.Marshal(budgetTrackConfig{SpendFile: t.TempDir() + "/s.json", MaxBudget: 5})
	p := New()
	if err := p.Configure(raw); err != nil {
		t.Fatal(err)
	}
	wrapped := pipeline.WrapConfigured(p, raw)
	if _, ok := wrapped.(pipeline.StreamingResponder); !ok {
		t.Fatal("wrapped plugin is NOT a StreamingResponder")
	}
	pl, err := pipeline.New([]pipeline.Plugin{wrapped})
	if err != nil {
		t.Fatal(err)
	}
	if !pl.HasStreamingResponders() {
		t.Fatal("pipeline.HasStreamingResponders() = false; forward proxy will use the buffered path and never call OnResponseFrame")
	}
}
