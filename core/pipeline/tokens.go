package pipeline

// PromptTokensOf is the request's own billed token count: what the provider counted for
// everything we sent. It lives on the response because the provider is the only party that
// tokenizes, but it is a request-side quantity — which is what lets a request row show a
// total at all.
//
// NAMED ...Of BECAUSE THE BARE NAME IS ALSO A FIELD, and they do not agree: this function is
// Input+CacheRead+CacheWrite, while InferenceExtension.PromptTokens is an aggregate nothing in
// tree writes. `x.PromptTokens` against `PromptTokens(x)` was one character from a silent zero,
// with the compiler content either way. PromptContextOf in this package already reads as the
// house style for "the figure for this thing".
//
// OUTPUT IS DELIBERATELY EXCLUDED, which the prompt-context rule measured on the same
// sessions: output is 0.003%-2.2% of the prompt and 0.2% on conversations near the context
// limit, so folding it in would change no rendered pixel while making the figure mean
// something else.
//
// The aggregate fallback cannot currently fire: parsercommon.TokenUsage.Fill is the sole
// production writer of these fields and sets PromptTokens to Input+CacheRead+CacheWrite — the
// same sum computed here — so when the split is zero the aggregate is zero too. It costs
// nothing and would start earning its keep if a parser ever published the aggregate directly.
// An earlier version of this paragraph said the fallback was "kept only to mirror
// savedTokensAndCost", an identifier that exists nowhere at this branch or upstream; it was
// already dangling in the cost_event.go this branch deletes.
func PromptTokensOf(resp *InferenceExtension) int {
	if resp == nil {
		return 0
	}
	if n := resp.InputTokens + resp.CacheReadTokens + resp.CacheWriteTokens; n > 0 {
		return n
	}
	return resp.PromptTokens
}
