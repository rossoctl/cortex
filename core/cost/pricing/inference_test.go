package pricing

import (
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

func TestUsageFromInference_SplitCounters(t *testing.T) {
	got := UsageFromInference(&pipeline.InferenceExtension{
		InputTokens:      100,
		CacheWriteTokens: 200,
		CacheReadTokens:  300,
		OutputTokens:     400,
	})
	want := Usage{Input: 100, CacheWrite: 200, CacheRead: 300, Output: 400}
	if got != want {
		t.Errorf("UsageFromInference = %+v, want %+v", got, want)
	}
}

func TestUsageFromInference_Nil(t *testing.T) {
	if got := UsageFromInference(nil); got != (Usage{}) {
		t.Errorf("UsageFromInference(nil) = %+v, want zero", got)
	}
}

func TestUsageFromInference_FallsBackToLegacyAggregates(t *testing.T) {
	// A gateway that reports only prompt/completion totals carries no split, and
	// zero-token usage is UNPRICED in Cost — so without this fallback such traffic
	// would silently vanish from the dollar total rather than being priced.
	got := UsageFromInference(&pipeline.InferenceExtension{
		PromptTokens:     1000,
		CompletionTokens: 250,
	})
	want := Usage{Input: 1000, Output: 250}
	if got != want {
		t.Errorf("UsageFromInference = %+v, want %+v", got, want)
	}
}

func TestUsageFromInference_SplitWinsOverAggregate(t *testing.T) {
	// PromptTokens is derived from the split (parsercommon.TokenUsage.Fill sets
	// both), so preferring the aggregate would double-count cache tiers into input
	// and price cache reads at the uncached rate — a ~10x overstatement on
	// cache-heavy traffic.
	got := UsageFromInference(&pipeline.InferenceExtension{
		InputTokens:     100,
		CacheReadTokens: 900,
		PromptTokens:    1000,
		OutputTokens:    50,
	})
	want := Usage{Input: 100, CacheRead: 900, Output: 50}
	if got != want {
		t.Errorf("UsageFromInference = %+v, want %+v", got, want)
	}
}

func TestUsageFromInference_OutputFallbackIsIndependent(t *testing.T) {
	// A provider can report a prompt split but only a legacy completion total.
	got := UsageFromInference(&pipeline.InferenceExtension{
		InputTokens:      100,
		CompletionTokens: 250,
	})
	want := Usage{Input: 100, Output: 250}
	if got != want {
		t.Errorf("UsageFromInference = %+v, want %+v", got, want)
	}
}
