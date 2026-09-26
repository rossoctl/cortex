package pricing

import "github.com/rossoctl/cortex/core/pipeline"

// UsageFromInference reads the per-tier token split that the inference parsers
// publish onto pctx.Extensions.Inference.
//
// This is the one conversion between the parsers' vocabulary and this package's,
// and it lives here so both consumers share it rather than each writing their own
// — the D3 duplication this consolidation exists to remove.
//
// The split counters are preferred over the legacy PromptTokens / CompletionTokens
// aggregates, never the reverse: parsercommon.TokenUsage.Fill derives PromptTokens
// FROM the split, so reading the aggregate would fold the cache tiers into
// uncached input and price cache reads at up to 10x their real rate on
// cache-heavy traffic (which is what Claude Code produces).
//
// The aggregates are the fallback for a provider that reports only totals. Without
// it such traffic has a zero split, and Cost treats zero usage as UNPRICED — so
// the requests would drop out of the dollar total silently instead of being priced
// approximately. Attributing the aggregate to uncached input is the same fallback
// toolprune's OnFinish already makes (plugins/toolprune/plugin.go:705-709): it
// over-prices a cache-heavy request, but a provider that hides its cache split has
// given us no way to do better, and being visibly approximate beats being
// invisibly absent.
//
// The two fallbacks are independent because a provider can report a prompt split
// and only a legacy completion total.
func UsageFromInference(inf *pipeline.InferenceExtension) Usage {
	if inf == nil {
		return Usage{}
	}
	u := Usage{
		Input:      inf.InputTokens,
		CacheWrite: inf.CacheWriteTokens,
		CacheRead:  inf.CacheReadTokens,
		Output:     inf.OutputTokens,
	}
	if u.PromptTotal() == 0 && inf.PromptTokens > 0 {
		u.Input = inf.PromptTokens
	}
	if u.Output == 0 && inf.CompletionTokens > 0 {
		u.Output = inf.CompletionTokens
	}
	// Last resort: a gateway that reports ONLY total_tokens. parsercommon records it
	// in TotalTokens and leaves prompt/completion at zero, so without this the
	// request has no usage at all and Cost treats it as unpriced — the doc above
	// promised this fallback and did not have it.
	//
	// Attributed wholly to uncached input, which OVERSTATES a cache-heavy request
	// and cannot distinguish prompt from completion. That is the only reading
	// available when the provider reports one number, and being visibly approximate
	// beats dropping the request out of the total.
	if u == (Usage{}) && inf.TotalTokens > 0 {
		u.Input = inf.TotalTokens
	}
	return u
}

// Reasons a figure modelled from an inference extension's counters is not an exact
// total. Returned by IncompleteReason and carried per-request on the cost record.
//
// Separate reasons rather than one "incomplete" boolean because a consumer acts on them
// differently: a floor is known-LOW and bounded on one side, an approximation has no known
// direction. Collapsed, a totals-only gateway — a permanent property of that gateway — would be
// indistinguishable from a truncated stream, which is a transient failure worth chasing.
//
// Wire strings, travelling on event.Event.IncompleteReason: hyphenated lowercase, like
// Provenance.
const (
	// ReasonOutputUncounted: the prompt was counted and whatever was generated never
	// was, so the figure is a FLOOR — the real cost is this plus an unknown completion.
	//
	// The case it exists for is a truncated Anthropic stream: prompt counts land on
	// message_start and the output count only on message_delta, so a stream dying in between
	// finalizes with real prompt tokens and output at zero.
	//
	// KNOWN BOUNDARY, deliberately not closed. The discriminator is the stop reason (see
	// outputUncounted), so a response carrying a stop_reason but no usage block reads as EXACT
	// even though its output was never tallied. Closing that would flag every response whose
	// output legitimately WAS zero — an immediate refusal, a max_tokens of zero — because those
	// are indistinguishable by counters alone, and a false caveat on ordinary traffic is how an
	// operator learns to ignore the real signal. A test row pins it ("stop reason with zero
	// output is exact"). If the omitted-usage shape is ever OBSERVED, the fix is a new reason
	// keyed on something that distinguishes it, not a weakening of this one.
	ReasonOutputUncounted = "output-uncounted"

	// ReasonSplitUnreported: the provider reported a total and no per-kind split, so the figure
	// is APPROXIMATE rather than low. UsageFromInference attributes such a total wholly to
	// uncached input, which over-prices a cache-heavy request (a cache read bills at ~0.1x) and
	// under-prices a generation-heavy one (~5x), with no way to say which from one number.
	//
	// A property of the gateway rather than the request, so a consumer presents it as a standing
	// caveat on precision, never as an incident.
	ReasonSplitUnreported = "split-unreported"

	// ReasonCountersBelowTotal: the response's OWN total exceeds the counters that were priced,
	// so tokens were reported and attributed to no tier. A floor, like ReasonOutputUncounted, on
	// different evidence — there a stop reason is missing, here the parts do not add up to the
	// stated total.
	//
	// NAMED FOR THE EVIDENCE, not the tier, because the tier is what is unknown: a
	// short-reported prompt produces identical arithmetic, and a wrong tier in the label sends
	// an operator to the wrong side of the request.
	ReasonCountersBelowTotal = "counters-below-total"
)

// NO PREDICATE IN THIS FILE READS pipeline.InferenceExtension.PresentKinds, and none should: it
// is dialect-unreliable for both questions asked here — see outputUncounted. The bit names the
// tests need live in incompletereason_test.go, beside the assertions that use them.

// IncompleteReason names why a figure modelled from inf's counters is not an exact
// total, or "" when the counters support one.
//
// Only ever consulted for a MODELLED figure. A gateway's own cost header is what the
// call actually charged whatever our counters managed to observe, so completeness there
// is the gateway's assertion and not an inference from a token tally — settle.Settle
// gates this call on the usage-fallback arm for that reason.
//
// The reasons are decided by DIFFERENT instruments, and that asymmetry is what to read before
// changing anything here: the presence mask is the right test for ReasonSplitUnreported and the
// wrong one for ReasonOutputUncounted, because the mask's reliability is DIALECT-specific —
// OpenAI gates each bit on a pointer, Anthropic asserts Input|Output unconditionally. See the
// block comment at the discriminator.
//
// Takes the extension rather than a Usage because none of the signals it needs — the stop reason,
// the legacy aggregates, the presence mask — is inside Usage's four tiers.
func IncompleteReason(inf *pipeline.InferenceExtension) string {
	if inf == nil {
		return ""
	}
	// Partial first. It is the stronger statement and the two are mutually exclusive in
	// practice, but ordering it first means a hypothetical extension satisfying both is
	// reported as a floor rather than as merely approximate — under-claiming precision
	// rather than over-claiming it.
	if outputUncounted(inf) {
		return ReasonOutputUncounted
	}
	// No counter of any kind carried a figure, yet a total arrived: a gateway reporting
	// only total_tokens. Gated on the total because with no counters at all there is no
	// figure to qualify — costing publishes nothing for that request.
	//
	// THE COUNTERS ARE THE INSTRUMENT HERE, NOT THE PRESENCE MASK. Do not rewrite this as
	// `PresentKinds&(presentInput|presentOutput) == 0`: that asks a question neither the
	// reason nor the arithmetic is about, and gets it wrong in both directions:
	//
	//	FALSE POSITIVE  A response reporting only cache counts has the Input and Output bits
	//	                clear, so the mask called it "no split at all" — while a split
	//	                plainly existed and UsageFromInference priced it, no total-to-input
	//	                attribution anywhere in sight.
	//	FALSE NEGATIVE  Anthropic asserts Input|Output unconditionally (see the block comment
	//	                in outputUncounted), so on the dialect Claude Code speaks the bits are
	//	                set even when both tallies are zero. A total arriving with them WAS
	//	                attributed wholly to input and would have been published as exact.
	//
	// Both are out of reach on today's dialects — OpenAI gates each bit on a pointer, and
	// Anthropic's Fill derives TotalTokens FROM the split, so all-zero tiers give a zero
	// total — which is why nothing observable moves and why the rows pinning it have to
	// synthesize an extension. The mask is dialect-unreliable in BOTH branches of this
	// function; it just happened to be harmless in this one.
	if totalsOnly(inf) {
		return ReasonSplitUnreported
	}
	// THE GATEWAY'S OWN TOTAL, AGAINST THE COUNTERS THAT WERE PRICED. A response reporting
	// input 600, output 0, total 1000 and a stop reason of "stop" prices 600 tokens and, before
	// this check, called that figure EXACT: outputUncounted returns false on any stop reason,
	// and totalsOnly needs every counter empty, so 400 reported tokens fell between the two
	// predicates and the floor was published as a total.
	//
	// A CROSS-CHECK RATHER THAN A THIRD GUESS AT THE WIRE, which is what makes it safe here.
	// The comparison uses figures the response itself supplied, so it cannot false-positive on
	// a genuinely zero-output call — a refusal, a max_tokens of zero — where the total equals
	// the prompt and the two sides agree. That is the failure mode outputUncounted's comment
	// warns against, and the reason this is a separate branch rather than a weakening of it.
	//
	// Only ONE DIRECTION is a claim. A total SMALLER than the counters is a gateway
	// contradicting itself, not a figure we can call low, and nothing here would know which
	// side to believe.
	if counted := countersBelowTotal(inf); counted {
		return ReasonCountersBelowTotal
	}
	return ""
}

// countersBelowTotal reports a response whose stated total exceeds what UsageFromInference
// could attribute to tiers.
//
// Reads the same fields UsageFromInference does, and in the same shape, so the label cannot
// drift from the arithmetic: PromptTotal() is the prompt side it priced, Output the completion
// side. The legacy aggregates are covered because UsageFromInference folds them into those two.
func countersBelowTotal(inf *pipeline.InferenceExtension) bool {
	if inf.TotalTokens <= 0 {
		return false
	}
	u := UsageFromInference(inf)
	// Guarded against the totals-only attribution, which is the one case where the total was
	// itself turned INTO a counter: UsageFromInference puts the whole total in uncached input,
	// so the two sides agree by construction and the branch above already named it.
	return u.PromptTotal()+u.Output < inf.TotalTokens
}

// totalsOnly reports the shape ReasonSplitUnreported names: every per-tier counter and both
// legacy aggregates empty, with a total present.
//
// It mirrors UsageFromInference's own last-resort guard deliberately, down to the fields it
// reads. IncompleteReason's whole job is to say what UsageFromInference DID, so it has to ask
// that function's question: this is exactly the input on which the whole total is attributed
// to uncached input, and nothing else is. Asking anything else — a presence mask, a single
// aggregate — is how the label and the arithmetic drift apart while both look reasonable.
func totalsOnly(inf *pipeline.InferenceExtension) bool {
	return inf.InputTokens == 0 && inf.CacheWriteTokens == 0 && inf.CacheReadTokens == 0 &&
		inf.OutputTokens == 0 && inf.PromptTokens == 0 && inf.CompletionTokens == 0 &&
		inf.TotalTokens > 0
}

// outputUncounted reports the floor case: a prompt was counted, the output tally cannot be
// shown to be final, and the provider never said why it stopped.
//
// "CANNOT BE SHOWN TO BE FINAL" RATHER THAN "IS ZERO", and the difference is the whole
// predicate. Returning "complete" on any non-zero output tally answers "was anything
// counted" when the question is "is what was counted all there is" — which calls a truncated
// stream exact.
func outputUncounted(inf *pipeline.InferenceExtension) bool {
	// THE DISCRIMINATOR, and it is deliberately not the Output presence bit.
	//
	// Do not "simplify" this to `PresentKinds & KindOutput == 0`. The mask reads like the
	// obvious mechanism, and the trap is that it DISCRIMINATES IN ONE DIALECT AND NOT THE
	// OTHER — and the broken one is the dialect Claude Code speaks:
	//
	//	OpenAI     inferenceUsage.toNeutral gates every bit on a pointer, so an absent
	//	           completion_tokens leaves KindOutput unset. The mask works.
	//	Anthropic  anthropicUsage.toNeutral sets `Present: KindInput | KindOutput` in the
	//	           struct literal, unconditionally, because the Messages API always
	//	           carries both keys. The mask cannot work.
	//
	// So checking the mask against the OpenAI path finds it correct and invites the
	// conclusion that it generalizes. It does not, and Anthropic is where the bug lives.
	//
	// The mechanism, on a truncated Anthropic stream: message_start's usage goes through
	// mergeAnthropicUsageMaxSeen, which ORs Present and merges Input, CacheRead and
	// CacheWrite — but never assigns Output. Output is assigned only in the
	// message_delta arm. So the Output BIT arrives on the first frame while the Output
	// TALLY only ever arrives on the last, nothing records that they came from different
	// events, and bit-set-count-zero is therefore reachable and identical to a genuine
	// reported zero.
	//
	// The stop reason works because it rides the SAME frame as the tally: message_delta
	// carries `delta.stop_reason` and the cumulative `usage.output_tokens` together, so
	// its absence is the wire's own statement that the tally never completed rather than
	// an inference about it.
	//
	// And it generalizes where the mask does not: foldOpenAIFrame sets FinishReason from
	// `choices[].finish_reason`, so a truncated OpenAI stream leaves it empty too. One
	// predicate, both dialects.
	//
	// It also does not over-flag. A response that legitimately generated nothing still
	// gets a message_delta with a stop reason — an immediate refusal, a max_tokens of
	// zero — so its total stays exact.
	if inf.FinishReason != "" {
		return false
	}
	// A COUNTED OUTPUT IS NOT A FINAL ONE ON A STREAM, and treating the two as identical
	// re-entered the defect this whole function exists to catch — from the opposite side.
	//
	// DO NOT HOIST THIS ABOVE THE STOP REASON, and do not make it unconditional:
	// `OutputTokens > 0 -> return false` ends the question on a non-zero tally, which is
	// right for a body that arrived whole and wrong for one still arriving. foldOpenAIFrame
	// REPLACES the accumulated usage with each usage-bearing chunk's totals, under a comment
	// stating why — "OpenAI streams cumulative usage: each usage-bearing chunk restates the
	// full totals" — so on that shape a mid-generation tally is a RUNNING total. A stream of
	// that dialect that dies before the end therefore had output > 0 and no stop reason, and
	// was published as exact: a floor labelled whole, which is the failure this function's
	// zero-output branch was written to prevent.
	//
	// GATED ON Stream RATHER THAN REQUIRING A STOP REASON OUTRIGHT. Demanding
	// `FinishReason != ""` of every counted output would be simpler and is the wrong trade:
	// a NON-streamed response arrived in one piece, so its tally is final whether or not the
	// dialect bothers to name a stop reason, and flagging those would print the "+ partial"
	// marker and the "the real total is higher" language over money that is exact. A caveat
	// that appears on correct figures is one a reader learns to ignore, which costs more than
	// the case it was added for.
	//
	// AND THE SHAPE READ IS THE RESPONSE'S, NOT THE REQUEST'S. StreamedResponse is set by whoever
	// folded the frames; Stream is what the request asked for, and the two come apart in both
	// directions. A gateway answering a NON-streaming request with SSE was the case that mattered:
	// keyed on the request flag, a truncated stream of that shape had a running tally, no stop
	// reason, and Stream false — so it was classified EXACT, a floor labelled whole, which is the
	// failure this function exists to prevent. The parser already refuses to take its dispatch arm
	// from the request flag for the same reason; this is that argument applied one level up.
	//
	// False whenever nothing observed a stream — an unparsed endpoint, a body past the size cap —
	// which is the safe direction: a caveat can only be added to traffic seen streaming, never to
	// traffic nobody read (see IncompleteReason's ordering).
	//
	// ANTHROPIC IS UNAFFECTED either way: Output is assigned only in the message_delta arm, which
	// carries delta.stop_reason on the same frame, so a truncated Anthropic stream has no tally to
	// be misread. This closes the OpenAI-shaped half.
	if inf.OutputTokens > 0 || inf.CompletionTokens > 0 {
		return inf.StreamedResponse
	}
	// A prompt-side count is what makes this a FLOOR rather than simply unpriced:
	// without one there is nothing for the figure to be a lower bound OF, and costing
	// publishes nothing at all. TotalTokens is deliberately NOT accepted here — a bare
	// total includes the completion, so it is the approximate case above, not this one.
	// PromptTokens is checked alongside the split because Fill derives it from them, and
	// a producer that set only one of the two should still be read.
	//
	// NOT GATED ON Stream, UNLIKE THE BRANCH ABOVE, and the asymmetry is deliberate rather
	// than an oversight. Up there a counted output on a non-streamed response is FINAL — the
	// body arrived whole — so flagging it would print the caveat over exact money. Here there
	// is no output count at all and no stop reason, on a response that reported what the
	// prompt cost: whether or not the request asked for a stream, the completion is unaccounted
	// for, and "at least this much" is the only claim the data supports. A predicate whose job
	// is to under-claim precision takes the safe direction when the two arguments disagree.
	return inf.PromptTokens > 0 || inf.InputTokens > 0 ||
		inf.CacheReadTokens > 0 || inf.CacheWriteTokens > 0
}
