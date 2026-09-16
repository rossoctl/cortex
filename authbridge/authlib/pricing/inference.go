package pricing

import "github.com/rossoctl/cortex/authbridge/authlib/pipeline"

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
// Two, not one, because they are different claims and a consumer acts on them
// differently: a partial figure is known-LOW and bounded on one side, while an
// approximate one has no known direction at all. Collapsing them into a single
// "incomplete" boolean would make a totals-only gateway — a permanent, unfixable
// property of that gateway — indistinguishable from a truncated stream, which is a
// transient failure worth chasing.
//
// Values are wire strings: they travel on costevent.Event.IncompleteReason. Hyphenated
// lowercase, matching Provenance's spellings ("authoritative", "configured").
const (
	// ReasonOutputUncounted: the prompt was counted and whatever was generated never
	// was, so the figure is a FLOOR — the real cost is this plus an unknown completion.
	//
	// The case it exists for is a truncated Anthropic stream. Prompt counts land on
	// message_start; the output count only ever arrives on message_delta. A stream that
	// dies in between — an upstream `error` event, a client disconnect, a proxy restart
	// mid-turn — finalizes with real prompt tokens and output at zero, and the parser
	// ALREADY knows: foldAnthropicFrame logs "token counts will be incomplete". Nothing
	// consumed that knowledge, so costing priced the prompt, published Settled: true,
	// and every consumer read a lower bound as a complete figure.
	//
	// KNOWN BOUNDARY, deliberately not closed. The discriminator is the stop reason (see
	// the block comment in outputUncounted), so a response that carries a stop_reason but
	// no usage block reads as EXACT even though its output was never tallied — a
	// message_delta whose usage the wire omitted, or an intermediary that strips usage
	// while forwarding the stop reason. Such a response is classified complete with
	// OutputTokens == 0.
	//
	// That is the intended trade, not an oversight, and there is a test row pinning it
	// ("stop reason with zero output is exact"). Closing it would mean flagging every
	// response whose output legitimately WAS zero — an immediate refusal, a max_tokens of
	// zero — because those are indistinguishable from it by counters alone. A false
	// caveat on ordinary traffic is worse than a missed one on a wire shape we have no
	// evidence of: it becomes a permanent warning with nothing to act on, which is how an
	// operator learns to ignore the real signal. Do not "fix" this row into a false
	// positive; if the omitted-usage shape is ever OBSERVED, the fix is a new reason
	// keyed on something that actually distinguishes it, not a weakening of this one.
	ReasonOutputUncounted = "output-uncounted"

	// ReasonSplitUnreported: the provider reported a total and no per-kind split at
	// all, so the figure is APPROXIMATE rather than low. UsageFromInference above
	// attributes such a total wholly to uncached input, which over-prices a cache-heavy
	// request (a cache read bills at ~0.1x) and under-prices a generation-heavy one (an
	// output token bills at ~5x), with no way to say which from one number.
	//
	// A property of the gateway, not of the request: it will hold for every request
	// that gateway answers. A consumer should present it as a standing caveat on the
	// total's precision, never as an incident.
	ReasonSplitUnreported = "split-unreported"
)

// Presence bits, mirrored from parsercommon.Kind.
//
// Mirrored rather than imported because parsercommon lives under plugins/internal and
// is unreachable from here — the same constraint that made
// pipeline.InferenceExtension.PresentKinds a plain uint8 with the layout written into
// its doc comment.
//
// NOTHING IN THIS FILE READS THE MASK ANY MORE. Both predicates that used to were
// dialect-unreliable — see outputUncounted's block comment and the call site of
// totalsOnly. The two bits stay declared because the tests that assert what the mask
// CANNOT discriminate need names for them, and a fixture built from bare shifts would
// stop saying which kinds it means.
const (
	presentInput  uint8 = 1 << 0 // parsercommon.KindInput
	presentOutput uint8 = 1 << 3 // parsercommon.KindOutput
)

// IncompleteReason names why a figure modelled from inf's counters is not an exact
// total, or "" when the counters support one.
//
// Only ever consulted for a MODELLED figure. A gateway's own cost header is what the
// call actually charged whatever our counters managed to observe, so completeness there
// is the gateway's assertion and not an inference from a token tally — costing.Settle
// gates this call on the usage-fallback arm for that reason.
//
// The two reasons are decided by DIFFERENT instruments, and the asymmetry is the part
// worth reading before changing anything here: the presence mask is the right test for
// ReasonSplitUnreported and the wrong one for ReasonOutputUncounted. Not because one
// reason is special, but because the mask's reliability is DIALECT-specific — OpenAI
// gates each bit on a pointer while Anthropic asserts Input|Output unconditionally. See
// the block comment at the discriminator itself, which is the single most important
// comment in this file.
//
// Takes the extension rather than a Usage because none of the three signals it needs —
// the stop reason, the legacy aggregates, the presence mask — is inside Usage's four
// tiers.
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
	// THE COUNTERS ARE THE INSTRUMENT HERE, NOT THE PRESENCE MASK. This used to read
	// `PresentKinds&(presentInput|presentOutput) == 0`, which asked a question neither the
	// reason nor the arithmetic is about, and got it wrong in both directions:
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
	return ""
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
// "CANNOT BE SHOWN TO BE FINAL" RATHER THAN "IS ZERO", which is the correction below. The
// function used to return "complete" on any non-zero output tally, before consulting the
// stop reason at all — so it answered "was anything counted" when the question is "is what
// was counted all there is".
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
	// mergeAnthropicPromptMaxSeen, which ORs Present and merges Input, CacheRead and
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
	// The early return here used to be unconditional: `OutputTokens > 0 -> return false`,
	// placed ABOVE the stop reason, so a non-zero tally ended the question. That is right
	// for a body that arrived whole and wrong for one still arriving. foldOpenAIFrame
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
	// Stream comes from the REQUEST body, so it is false whenever this process did not read
	// the request — an unparsed endpoint, extproc without a request body, a body past the
	// size cap. The change can only ever add a caveat to traffic we know asked for a stream,
	// and never to traffic we did not read: the safe direction for a predicate whose job is
	// to under-claim precision rather than over-claim it (see IncompleteReason's ordering).
	//
	// ANTHROPIC WAS ALREADY SAFE and stays unaffected: Output is assigned only in the
	// message_delta arm, which carries delta.stop_reason on the same frame, so a truncated
	// Anthropic stream has no tally to be misread. This closes the OpenAI-shaped half, which
	// is the half nothing was watching.
	if inf.OutputTokens > 0 || inf.CompletionTokens > 0 {
		return inf.Stream
	}
	// A prompt-side count is what makes this a FLOOR rather than simply unpriced:
	// without one there is nothing for the figure to be a lower bound OF, and costing
	// publishes nothing at all. TotalTokens is deliberately NOT accepted here — a bare
	// total includes the completion, so it is the approximate case above, not this one.
	// PromptTokens is checked alongside the split because Fill derives it from them, and
	// a producer that set only one of the two should still be read.
	return inf.PromptTokens > 0 || inf.InputTokens > 0 ||
		inf.CacheReadTokens > 0 || inf.CacheWriteTokens > 0
}
