package inferenceparser

import (
	"context"
	"log/slog"
	"strings"

	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/cost/settle"
	"github.com/rossoctl/cortex/core/internal/parsercommon"
	"github.com/rossoctl/cortex/core/parse/inference"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
)

// InferenceParser parses outbound OpenAI-compatible LLM inference requests
// and populates pctx.Extensions.Inference for downstream policy plugins.
//
// It also prices the finalized response — see cost.go for why the component that produces
// the token counts is the one that turns them into money.
type InferenceParser struct {
	// rates is the process rate table, injected by plugins.BuildWithDeps. Nil when the
	// process has no pricing wired, which settle.Settle handles by reporting unpriced.
	rates pricing.Resolver
	// blind reports gateways whose cost header omits cache cost, once each. It lives here
	// rather than beside litellm-budget-track's drift reporter because that plugin is opt-in
	// and this one runs wherever anything is priced — see cacheBlindReporter.
	blind cacheBlindReporter
}

func NewInferenceParser() *InferenceParser { return &InferenceParser{} }

func init() {
	plugins.RegisterPlugin("inference-parser", func() pipeline.Plugin { return NewInferenceParser() })
}

func (p *InferenceParser) Name() string { return "inference-parser" }

func (p *InferenceParser) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		ReadsBody:   true,
		Description: "Parses LLM completions into pctx.Extensions.Inference.",
	}
}

// endpointPath returns pctx.Path with any query string removed.
//
// Every listener now guarantees Path is query-free (see pipeline.Context.Path),
// so this is defense in depth for contexts constructed outside a listener
// (tests, future transports). It stays because the failure mode it guards is
// silent: Claude Code posts to /v1/messages?beta=true, and with a query
// attached the path no longer ends the way dialectFor looks for, so it falls to
// the default arm and records no inference telemetry at all — or worse, sends an
// Anthropic stream to the OpenAI parser.
func endpointPath(pctx *pipeline.Context) string {
	path, _, _ := strings.Cut(pctx.Path, "?")
	return path
}

func (p *InferenceParser) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	// Dispatch by endpoint dialect: OpenAI chat/completions vs Anthropic
	// Messages. No Invocation is recorded when the parser doesn't apply (a path
	// dialectFor does not recognise, or a body not taken for inference) —
	// operators infer "inference-parser is in this pipeline" from config, not
	// per-event rows.
	//
	// LEAVING Extensions.Inference NIL IS THE POINT on both arms below, and it is not the
	// same thing as ignoring the request. Populating it for an endpoint this parser cannot
	// read would assert a model and a message list that were never on the wire, and every
	// downstream policy plugin reads those as facts. So the extension stays absent — and
	// the RESPONSE side settles the cost regardless, from the gateway's own header, which
	// needs neither a model nor a body. See the nil-extension guard in OnResponseFrame.
	ext := inference.ParseRequest(endpointPath(pctx), pctx.Headers, pctx.Body)
	if ext == nil {
		return pipeline.Action{Type: pipeline.Continue}
	}
	pctx.Extensions.Inference = ext

	pctx.Observe("matched_" + ext.Model)
	return pipeline.Action{Type: pipeline.Continue}
}

// OnResponse is the legacy buffered-path response hook. Because this
// plugin implements StreamingResponder, pipeline.RunResponse skips it
// and OnResponseFrame is the dispatch path under all listeners — this
// method is unreachable from a normal listener. Kept for tests and
// hypothetical pipelines that call OnResponse directly without going
// through RunResponse, with a defensive guard against re-recording if
// the streaming path has already populated state.
func (p *InferenceParser) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if pctx.Extensions.Inference == nil {
		// Priced anyway, on the rule OnResponseFrame's guard carries in full: whether this parser
		// understood the REQUEST decides what can be parsed, never what the gateway may charge.
		// Defence in depth rather than the live path — RunResponse skips this plugin, so the arm
		// that settles a nil-extension response under a real listener is OnResponseFrame's.
		p.settleCost(pctx)
		return pipeline.Action{Type: pipeline.Continue}
	}
	ext := pctx.Extensions.Inference
	if ext.Completion != "" || ext.FinishReason != "" || ext.TotalTokens > 0 {
		return pipeline.Action{Type: pipeline.Continue}
	}
	if len(pctx.ResponseBody) == 0 {
		pctx.Skip("no_response_body")
		// Priced anyway, because a body is not what makes a response cost money: the gateway
		// reports its figure in a RESPONSE HEADER and settle.Settle prefers it over anything
		// modelled, so a costed response with an empty or unrecognised body is real spend.
		// Returning without settling drops it from the cost record, the aggregate and the budget.
		//
		// It cannot flood the aggregate with settled zeros: with no body there are no counters, and
		// modelledCost short-circuits on an all-zero Usage, so settleCost publishes nothing unless
		// a header or a modelled figure exists. The Skip row above pairs the response with its
		// request; it is not a reason to stop charging.
		p.settleCost(pctx)
		return pipeline.Action{Type: pipeline.Continue}
	}

	inference.ReadResponse(endpointPath(pctx), ext.Stream, pctx.ResponseBody, ext)

	logInferenceFinalized(ext)
	p.settleCost(pctx)
	pctx.Observe("matched_" + ext.Model + "_response")
	return pipeline.Action{Type: pipeline.Continue}
}

// ParseResponse reads a buffered response body as OnResponse would, into a
// fresh extension, and settles nothing. It is for a plugin that runs before
// this one on the response pass and needs the normalized completion and tool
// calls. The bytes decide the shape: a body framed as SSE is read as a
// stream (and marks StreamedResponse), anything else as JSON. It returns nil
// when path is not an inference endpoint. It is inference.ParseResponse,
// kept here for the cpex plugin and for other users of this package.
func ParseResponse(path string, body []byte) *pipeline.InferenceExtension {
	return inference.ParseResponse(path, body)
}

// streamStateKey scopes the scratch state to this plugin in
// pctx.Extensions.Custom. Other plugins see pctx.Extensions.Custom
// keys but won't collide with this one.
const streamStateKey = "inference-parser/stream-state"

// OnResponseFrame folds each SSE-data chunk into the running
// completion. On last=true the finalized result is written to the
// public InferenceExtension fields (Completion / FinishReason /
// token counts) and the Observe row is recorded.
//
// Application/json responses are delivered as a single last=true
// frame containing the full JSON body — the dual path keeps one
// code path for both shapes.
func (p *InferenceParser) OnResponseFrame(_ context.Context, pctx *pipeline.Context, frame []byte, last bool) pipeline.Action {
	if pctx.Extensions.Inference == nil {
		// THE FOURTH BODY-LESS PATH, and the one not about a body at all: the guards below handle a
		// response whose BODY could not be read, this one a response whose REQUEST was never
		// parsed. OnRequest leaves Extensions.Inference nil for /v1/embeddings, /v1/rerank,
		// anything else the gateway mounts, and any body it does not take for inference — and
		// returning here made all of it free, with the gateway's figure sitting unread on the
		// response headers.
		//
		// EVERY path, not an allowlist of the ones that look priceable. The predicate that
		// decides whether money moves is "the gateway reported a cost", which
		// settle.headerCost evaluates off the response headers; a second list of paths
		// here would be the same omission this fixes, one release later. A plain proxied
		// response with no cost header publishes nothing, because with no extension there
		// is no usage to model and settleCost's own gate then finds neither a figure nor a
		// saving to report.
		//
		// THE DISPATCH IS PATH-AGNOSTIC; THE COVERAGE IS AS WIDE AS THE HEADER AND NO WIDER,
		// and the two are not the same claim. A streamed response on an unparsed endpoint
		// reports 0 — LiteLLM's placeholder, since the total is unknown when headers are
		// sent — and with the endpoint unparsed there is no usage to fall back to either, so
		// NO FIGURE EXISTS ANYWHERE for it and nothing is published. That is the boundary,
		// not an oversight: a settled zero would count unpriced traffic as free and a
		// modelled one would be invented. Streaming is not a carve-out — a POSITIVE header
		// on a stream is charged here like any other, and an implausible one is refused and
		// disclosed, because cost/settle's cap reads the nil extension and never the
		// Content-Type. The only thing that widens this row is teaching the parser the
		// dialect. See reverseproxy's StreamedUnparsedEndpoint_CoverageBoundary, which
		// states all three outcomes through a real listener.
		//
		// On last only, so an unparsed endpoint settles where every other path does — at
		// end of stream — rather than on whichever frame arrived first. Both proxy listeners
		// always terminate with RunResponseFrame(..., nil, true), so the arm is reached
		// whenever a response has any body phase at all.
		//
		// ON EXTPROC IT IS REACHED WITH OR WITHOUT A BODY. A response Envoy ends on headers
		// — a 204, a 304, an error status — has no body phase for the arm to hang off, so
		// that listener dispatches the terminal frame from its HEADERS phase when
		// end_of_stream is set (see its `NeedsBody() && !endOfStream` gate), and its deferred
		// flush covers a stream torn down before any end-of-stream arrives at all. Between
		// them the arm is reached on every shape either listener can produce.
		//
		// No Skip and no Observe row: the body may be perfectly fine and simply not ours,
		// so "no_response_body" would be a false diagnostic, and there is no model to name
		// in a matched_ row. OnRequest records nothing for this traffic either — the
		// invocation timeline stays as quiet as it was.
		if last {
			p.settleCost(pctx)
		}
		return pipeline.Action{Type: pipeline.Continue}
	}
	ext := pctx.Extensions.Inference

	// WHICH ARM — and why the request's stream flag no longer decides it.
	//
	// All three listeners choose their dispatch shape from the RESPONSE Content-Type:
	// text/event-stream gets one call per SSE event followed by a terminal empty
	// last=true, anything else gets a single last=true frame holding the whole body
	// (reverseproxy.modifyResponse, forwardproxy.serveOutbound,
	// extproc.dispatchBufferedFrames). DO NOT switch this on ext.Stream, which comes off the
	// REQUEST: the two disagree whenever the response's shape is not the one the request asked
	// for, and the parser then runs the wrong arm over the listener's frames:
	//
	//   - Streamed response, non-streaming request: every folded frame was thrown away.
	//     The terminal frame took the one-shot arm, found it empty, recorded a
	//     no_response_body Skip on a response that had carried a body, and never called
	//     finalize — so the usage never reached the extension. LiteLLM stamps 0 in the cost
	//     header on a stream, so the usage fallback is the ONLY figure available and the
	//     whole charge was lost.
	//   - Buffered response, streaming request: the complete envelope was folded as if it
	//     were one SSE chunk. On the OpenAI dialect that salvages the usage block by
	//     coincidence (chunk and envelope share the "usage" key) and still loses the
	//     completion; on the Anthropic dialect "type":"message" matches no stream event, so
	//     usage, completion and finish reason are all dropped and a false no_response_body
	//     Skip is recorded. A JSON reply to a streaming request is routine — it is what
	//     every gateway error page is.
	//
	// So the arm is taken from THE SHAPE THAT WAS DISPATCHED, which the listener states
	// unambiguously in how it calls: a non-terminal frame only ever comes from a per-frame
	// dispatch. That evidence needs no header and cannot disagree with the caller.
	if !last {
		// Mid-stream. Lazily allocate the per-stream scratch and fold this frame into it;
		// its existence is also what tells the terminal frame below that a stream ran.
		getOrCreateStreamState(pctx).Fold(frame, ext)
		return pipeline.Action{Type: pipeline.Continue}
	}

	// Terminal frame. Three shapes reach here and the state, the Content-Type and the
	// frame's own framing tell them apart — see inference.CarriesSSEFraming for the last of those.
	state := pipeline.GetState[inference.Stream](pctx, streamStateKey)
	if state == nil && len(frame) > 0 && settle.IsEventStream(pctx) && !inference.CarriesSSEFraming(frame) {
		// A streamed response whose whole content fitted in one event, delivered on the
		// terminal call: unframed payload, SSE Content-Type, no earlier frames. It folds
		// like any other chunk — allocate the scratch so it does.
		state = getOrCreateStreamState(pctx)
	}

	// Terminal frame of a streamed response. Read the state rather than the frame — the
	// usage arrived on an earlier chunk and Finish is the only thing that moves it onto
	// the extension.
	if state != nil {
		// A listener is free to carry the last data on the terminal call rather than
		// sending a separate empty one; fold it before finalizing so that shape is not a
		// silently dropped chunk.
		state.Fold(frame, ext)
		state.Finish(ext)
		// Empty stream with no body and no chunks — record Skip to
		// pair the response row with the request row.
		//
		// Tool calls count as a body. A turn cancelled while the model was
		// still emitting tool arguments has no completion text, no finish
		// reason, and no usage block, but Finish has copied the call —
		// so skipping here would label a stream that demonstrably carried
		// content as having none, and drop it out of any timeline filtered
		// on observe.
		if ext.Completion == "" && ext.FinishReason == "" && ext.TotalTokens == 0 &&
			len(ext.ToolCalls) == 0 {
			pctx.Skip("no_response_body")
			// Same as the guards above: an empty stream can still carry a gateway cost
			// header, and the Skip row is a diagnostic rather than a reason to stop
			// charging.
			p.settleCost(pctx)
			return pipeline.Action{Type: pipeline.Continue}
		}
		logInferenceFinalized(ext)
		p.settleCost(pctx)
		pctx.Observe("matched_" + ext.Model + "_response")
		return pipeline.Action{Type: pipeline.Continue}
	}

	// Terminal frame and nothing before it: the listener buffered the response and handed
	// it over in one piece.
	if len(frame) == 0 {
		pctx.Skip("no_response_body")
		// Charged before returning, for the reason spelled out on OnResponse's
		// identical guard: a positive gateway cost header needs no body at all.
		//
		// This is the arm a genuinely body-less response takes in production — a 204/304,
		// or an error status ended on headers. It is reached whatever the request asked for,
		// which is the point: keyed on the request instead, a header-only reply to a
		// streaming request lands on the streaming arm and is described as an "empty
		// stream".
		p.settleCost(pctx)
		return pipeline.Action{Type: pipeline.Continue}
	}
	// settle.IsEventStream(pctx) is the Content-Type check (text/event-stream). OR'd with
	// inference.CarriesSSEFraming(frame): the Responses API's chatgpt.com-hosted gateway (Codex)
	// sends real SSE wire bytes — event:/data: lines — under Content-Type:
	// application/json. Trusting the header alone would route that body to the JSON arm
	// below, where it fails to unmarshal as a single object and parses nothing. This is
	// the one dialect among the three known to mislabel itself this way; the other two
	// are only ever reached here with a correct header, so the fallback costs them
	// nothing — inference.CarriesSSEFraming is cheap (a prefix check) and false for an ordinary
	// JSON body.
	//
	// When sse is true, it is an SSE body delivered whole, with its wire framing intact — not
	// frame by frame. Both proxy listeners fall back to the buffered path for a text/event-stream
	// response when a plugin in the chain declares WritesResponseBody (a body they
	// must rewrite cannot also be forwarded as it arrives), and then deliver the
	// entire stream as this one frame. Folding it as a single chunk parses nothing; it
	// has to go through the SSE reader.
	sse := settle.IsEventStream(pctx) || inference.CarriesSSEFraming(frame)
	inference.ReadResponse(endpointPath(pctx), sse, frame, ext)
	logInferenceFinalized(ext)
	p.settleCost(pctx)
	pctx.Observe("matched_" + ext.Model + "_response")
	return pipeline.Action{Type: pipeline.Continue}
}

// getOrCreateStreamState returns the stream fold kept on pctx for this response.
// Its existence is what tells the terminal frame that a stream ran.
func getOrCreateStreamState(pctx *pipeline.Context) *inference.Stream {
	if s := pipeline.GetState[inference.Stream](pctx, streamStateKey); s != nil {
		return s
	}
	s := inference.NewStream(endpointPath(pctx))
	pipeline.SetState(pctx, streamStateKey, s)
	return s
}

// logInferenceFinalized emits the operator-facing INFO log once a
// response is finalized; shared by the buffered and streaming paths.
// Split counters render -1 when ext.PresentKinds says the provider
// did not expose that sub-kind, distinct from a reported 0.
func logInferenceFinalized(ext *pipeline.InferenceExtension) {
	tok := func(bit parsercommon.Kind, v int) int {
		if ext.PresentKinds&uint8(bit) == 0 {
			return -1
		}
		return v
	}
	slog.Info("inference-parser: response",
		"model", pipeline.SanitizeLogValue(ext.Model),
		"finishReason", pipeline.SanitizeLogValue(ext.FinishReason),
		"promptTokens", ext.PromptTokens,
		"completionTokens", ext.CompletionTokens,
		"inputTokens", tok(parsercommon.KindInput, ext.InputTokens),
		"cacheReadTokens", tok(parsercommon.KindCacheRead, ext.CacheReadTokens),
		"cacheWriteTokens", tok(parsercommon.KindCacheWrite, ext.CacheWriteTokens),
		"outputTokens", tok(parsercommon.KindOutput, ext.OutputTokens),
		"reasoningTokens", tok(parsercommon.KindReasoning, ext.ReasoningTokens),
	)
	slog.Debug("inference-parser: completion", "text", pipeline.SanitizeLogValue(parsercommon.Truncate(ext.Completion, parsercommon.DebugBodyMax)))
}
