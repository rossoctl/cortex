package parsercommon

import "github.com/rossoctl/cortex/core/pipeline"

// Kind is a bitmask naming which sub-kinds the provider populated.
// A zero on a set bit is "reported zero"; a zero on an unset bit is
// "not exposed."
type Kind uint8

const (
	KindInput Kind = 1 << iota
	KindCacheRead
	KindCacheWrite
	KindOutput
	KindReasoning
)

// TokenUsage is the provider-neutral token accounting shape. Parsers
// normalize their wire format into these fields before publishing via
// Fill, and set Present to name which sub-kinds the wire carried.
type TokenUsage struct {
	Input         int  // uncached prompt tokens
	CacheRead     int  // prompt tokens served from cache
	CacheWrite    int  // prompt tokens written to cache
	Output        int  // generated completion tokens
	Reasoning     int  // reasoning-only output (subset of Output)
	ReportedTotal int  // provider's own total_tokens if reported, else 0
	Present       Kind // which sub-kinds the provider reported
}

// PromptTotal is the sum of all prompt-side sub-kinds.
func (u TokenUsage) PromptTotal() int {
	return u.Input + u.CacheRead + u.CacheWrite
}

// Total is PromptTotal + Output. Reasoning is a subset of Output and
// intentionally not added.
func (u TokenUsage) Total() int {
	return u.PromptTotal() + u.Output
}

// Fill writes the split counters, the Present bitmask, and the derived
// legacy aggregates onto ext. TotalTokens prefers the provider's own
// reported total when present — a gateway that reports only total_tokens
// (with prompt/completion zero) would otherwise record 0 here.
func (u TokenUsage) Fill(ext *pipeline.InferenceExtension) {
	ext.InputTokens = u.Input
	ext.CacheReadTokens = u.CacheRead
	ext.CacheWriteTokens = u.CacheWrite
	ext.OutputTokens = u.Output
	ext.ReasoningTokens = u.Reasoning
	ext.PresentKinds = uint8(u.Present)

	ext.PromptTokens = u.PromptTotal()
	ext.CompletionTokens = u.Output
	if u.ReportedTotal > 0 {
		ext.TotalTokens = u.ReportedTotal
	} else {
		ext.TotalTokens = u.Total()
	}
}
