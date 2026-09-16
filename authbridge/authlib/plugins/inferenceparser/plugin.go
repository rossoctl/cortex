package inferenceparser

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/rossoctl/cortex/authbridge/authlib/costing"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins/internal/parsercommon"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
)

// InferenceParser parses outbound OpenAI-compatible LLM inference requests
// and populates pctx.Extensions.Inference for downstream policy plugins.
//
// It also prices the finalized response — see cost.go for why the component that produces
// the token counts is the one that turns them into money.
type InferenceParser struct {
	// rates is the process rate table, injected by plugins.BuildWithDeps. Nil when the
	// process has no pricing wired, which costing.Settle handles by reporting unpriced.
	rates pricing.Resolver
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
// attached the exact-match dialect dispatch below falls to the default arm and
// records no inference telemetry at all — or worse, sends an Anthropic stream
// to the OpenAI parser.
func endpointPath(pctx *pipeline.Context) string {
	path, _, _ := strings.Cut(pctx.Path, "?")
	return path
}

// bobPath is IBM Bob's inference endpoint. It speaks the OPENAI dialect — the
// body is {model, messages, ...} — despite the path not matching any of the
// OpenAI spellings above, because Bob mounts the API under an /inference prefix.
//
// A named const rather than another string in the switch: the prefix is what
// makes it non-obvious that this is OpenAI-shaped, and the name is where that
// gets said. It lives here, beside parseOpenAIRequest, rather than in
// anthropic.go — routing an Anthropic-file const to the OpenAI parser reads as a
// mistake even when it is not.
const bobPath = "/inference/v1/chat/completions"

func (p *InferenceParser) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	// Dispatch by endpoint dialect: OpenAI chat/completions vs Anthropic
	// Messages. No Invocation is recorded when the parser doesn't apply
	// (unrecognized path, empty body, or non-JSON body) — operators infer
	// "inference-parser is in this pipeline" from config, not per-event rows.
	//
	// LEAVING Extensions.Inference NIL IS THE POINT on both arms below, and it is not the
	// same thing as ignoring the request. Populating it for an endpoint this parser cannot
	// read would assert a model and a message list that were never on the wire, and every
	// downstream policy plugin reads those as facts. So the extension stays absent — and
	// the RESPONSE side settles the cost regardless, from the gateway's own header, which
	// needs neither a model nor a body. See the nil-extension guard in OnResponseFrame.
	var ext *pipeline.InferenceExtension
	switch endpointPath(pctx) {
	case "/v1/chat/completions", "/v1/completions", "/chat/completions", "/completions", bobPath:
		ext = parseOpenAIRequest(pctx.Body)
	case anthropicMessagesPath:
		ext = parseAnthropicRequest(pctx.Body)
	default:
		return pipeline.Action{Type: pipeline.Continue}
	}
	if ext == nil {
		// "no telemetry", not "skipping": this request is still charged if the gateway
		// says it cost something. The old wording ("no/invalid body, skipping") described
		// the whole request as dropped, which is what the code used to do and is now the
		// wrong half of what it does — an operator reading it while hunting for missing
		// spend would rule out the one path that still records some.
		slog.Debug("inference-parser: no/invalid body; no request telemetry, response still priced from the gateway header", "path", pctx.Path)
		return pipeline.Action{Type: pipeline.Continue}
	}

	pctx.Extensions.Inference = ext

	slog.Info("inference-parser", "model", ext.Model)
	slog.Debug("inference-parser: extracted", "model", ext.Model, "messages", len(ext.Messages), "stream", ext.Stream, "tools", len(ext.Tools))
	for i, m := range ext.Messages {
		slog.Debug("inference-parser: message", "index", i, "role", m.Role, "content", parsercommon.Truncate(m.Content, parsercommon.DebugBodyMax))
	}

	pctx.Observe("matched_" + ext.Model)
	return pipeline.Action{Type: pipeline.Continue}
}

// parseOpenAIRequest builds an InferenceExtension from an OpenAI
// chat/completions (or completions) request body. Returns nil for an empty or
// non-JSON body. Every populated extension is an outbound LLM call — an agent
// action (IsAction); the "don't judge inference by default" choice is operator
// policy in IBAC, independent of this classification.
func parseOpenAIRequest(body []byte) *pipeline.InferenceExtension {
	if len(body) == 0 {
		return nil
	}
	var req inferenceRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	ext := &pipeline.InferenceExtension{
		Model:       req.Model,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		TopP:        req.TopP,
		Stream:      req.Stream,
		ToolChoice:  req.ToolChoice,
		IsAction:    true,
	}
	for _, msg := range req.Messages {
		ext.Messages = append(ext.Messages, pipeline.InferenceMessage{
			Role:         msg.Role,
			Content:      msg.Content,
			ContentBytes: msg.ContentBytes,
		})
	}
	for _, tool := range req.Tools {
		if tool.Function.Name == "" {
			continue
		}
		ext.Tools = append(ext.Tools, pipeline.InferenceTool{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			Parameters:  tool.Function.paramsMap(),
		})
	}
	return ext
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
		// Priced anyway. Same rule as the OnResponseFrame guard below, which carries the
		// full argument: whether this parser understood the REQUEST decides what can be
		// parsed, never what the gateway may charge.
		p.settleCost(pctx)
		return pipeline.Action{Type: pipeline.Continue}
	}
	ext := pctx.Extensions.Inference
	if ext.Completion != "" || ext.FinishReason != "" || ext.TotalTokens > 0 {
		return pipeline.Action{Type: pipeline.Continue}
	}
	if len(pctx.ResponseBody) == 0 {
		pctx.Skip("no_response_body")
		// Priced anyway, because a body is not what makes a response cost money. The
		// gateway reports its own post-discount figure in a RESPONSE HEADER, and
		// costing.Settle prefers that figure over anything modelled from token
		// counters — so a LiteLLM-costed response whose body was empty or
		// unrecognised is real spend. Returning before settleCost dropped it
		// entirely: no cost record, nothing in the aggregator's dollar total, and
		// nothing in litellm-budget-track's ledger, so the money escaped the budget
		// as well as the chart.
		//
		// This cannot flood the aggregate with settled zeros. With no body there are
		// no token counters, and pricing.Cost refuses an all-zero Usage as UNPRICED
		// rather than as free (costing.modelledCost short-circuits on it first), so
		// settleCost's own gate publishes nothing when neither a header nor a
		// modelled figure exists. A body-less response with no cost header therefore
		// records only the Skip row above, exactly as before.
		//
		// The Skip is a diagnostic that pairs the response row with the request row;
		// it is not a reason to stop charging. Both happen, in that order.
		p.settleCost(pctx)
		return pipeline.Action{Type: pipeline.Continue}
	}

	if ext.Stream {
		if endpointPath(pctx) == anthropicMessagesPath {
			parseAnthropicSSE(pctx.ResponseBody, ext)
		} else {
			parseInferenceSSE(pctx.ResponseBody, ext)
		}
	} else {
		if endpointPath(pctx) == anthropicMessagesPath {
			parseAnthropicJSON(pctx.ResponseBody, ext)
		} else {
			parseInferenceJSON(pctx.ResponseBody, ext)
		}
	}

	logInferenceFinalized(ext)
	p.settleCost(pctx)
	pctx.Observe("matched_" + ext.Model + "_response")
	return pipeline.Action{Type: pipeline.Continue}
}

// inferenceStreamState is the scratch state kept on pctx.Extensions.Custom
// for the duration of a streaming response. Provider-specific fold
// functions normalize their wire format into the neutral usage field;
// hasUsage flags whether any event carried usage counts (some providers
// omit the block unless the client opts in).
//
// A streamed Anthropic tool call is spread over many frames — id and name
// on the opening frame, arguments as fragments after it — so it has to be
// assembled here rather than read off any single frame. toolCalls keeps
// emission order; toolsByIndex resolves a fragment to its call, since
// interleaved blocks (a text block and two tool calls) are only
// distinguishable by the block index the provider stamps on each frame.
// openTool is the fallback for a provider that omits the index.
type inferenceStreamState struct {
	completion strings.Builder
	usage      parsercommon.TokenUsage
	hasUsage   bool

	toolCalls    []*anthropicToolCallState
	toolsByIndex map[int]*anthropicToolCallState
	openTool     *anthropicToolCallState
}

// finalize copies the accumulated stream state onto the public extension
// fields. Every write is an assignment rather than an accumulation, so a
// second finalize on the same state (the buffered OnResponse path running
// after a streaming pass) is a no-op instead of a double-count.
func (s *inferenceStreamState) finalize(ext *pipeline.InferenceExtension) {
	ext.Completion = s.completion.String()
	if s.hasUsage {
		s.usage.Fill(ext)
	}
	if len(s.toolCalls) == 0 {
		return
	}
	calls := make([]pipeline.InferenceToolCall, 0, len(s.toolCalls))
	for _, tc := range s.toolCalls {
		calls = append(calls, pipeline.InferenceToolCall{
			ID:        tc.id,
			Name:      tc.name,
			Arguments: tc.args.String(),
		})
	}
	ext.ToolCalls = calls
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
		// THE FOURTH BODY-LESS PATH, and the one that is not about a body at all.
		//
		// The three guards further down handle a response whose BODY we could not read.
		// This one handles a response whose REQUEST we never parsed: OnRequest populates
		// Extensions.Inference only for the six OpenAI spellings and the Anthropic
		// Messages path, and leaves it nil for /v1/embeddings, /v1/rerank,
		// /v1/moderations, anything else the gateway mounts, and any request whose body
		// was empty or not JSON. Returning here made all of that free — the gateway's
		// figure was on the response headers and nothing read it, so the spend was in no
		// /v1/usage total, no ledger and no budget. See settleCost for why nil is safe.
		//
		// EVERY path, not an allowlist of the ones that look priceable. The predicate that
		// decides whether money moves is "the gateway reported a cost", which
		// costing.headerCost evaluates off the response headers; a second list of paths
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
		// disclosed, because costing's cap reads the nil extension and never the
		// Content-Type. The only thing that widens this row is teaching the parser the
		// dialect. See reverseproxy's StreamedUnparsedEndpoint_CoverageBoundary, which
		// states all three outcomes through a real listener.
		//
		// On last only, so an unparsed endpoint settles where every other path does — at
		// end of stream — rather than on whichever frame arrived first. Both proxy listeners
		// always terminate with RunResponseFrame(..., nil, true), so the arm is reached
		// whenever a response has any body phase at all. On extproc it is reached from the
		// body phase only: for a response with NO body that listener dispatches nothing —
		// see TestCapabilities_ReadsBodyDecidesTheExtprocBranch, which names the gap and
		// where it has to be closed.
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
	// extproc.dispatchBufferedFrames). This used to switch on ext.Stream, which comes off
	// the REQUEST — so the two disagreed whenever the response's shape was not the one the
	// request asked for, and the parser then ran the wrong arm over the listener's frames:
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
		state := getOrCreateStreamState(pctx)
		if len(frame) > 0 {
			foldResponseFrame(pctx, frame, state, ext)
		}
		return pipeline.Action{Type: pipeline.Continue}
	}

	// Terminal frame. Three shapes reach here and the state, the Content-Type and the
	// frame's own framing tell them apart — see carriesSSEFraming for the last of those.
	state := pipeline.GetState[inferenceStreamState](pctx, streamStateKey)
	if state == nil && len(frame) > 0 && costing.IsEventStream(pctx) && !carriesSSEFraming(frame) {
		// A streamed response whose whole content fitted in one event, delivered on the
		// terminal call: unframed payload, SSE Content-Type, no earlier frames. It folds
		// like any other chunk — allocate the scratch so it does.
		state = getOrCreateStreamState(pctx)
	}

	// Terminal frame of a streamed response. Read the state rather than the frame — the
	// usage arrived on an earlier chunk and finalize is the only thing that moves it onto
	// the extension.
	if state != nil {
		// A listener is free to carry the last data on the terminal call rather than
		// sending a separate empty one; fold it before finalizing so that shape is not a
		// silently dropped chunk.
		if len(frame) > 0 {
			foldResponseFrame(pctx, frame, state, ext)
		}
		state.finalize(ext)
		// Empty stream with no body and no chunks — record Skip to
		// pair the response row with the request row.
		//
		// Tool calls count as a body. A turn cancelled while the model was
		// still emitting tool arguments has no completion text, no finish
		// reason, and no usage block, but finalize has captured the call —
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
		// or an error status ended on headers. It is reached whatever the request asked
		// for, which is the point: a header-only reply to a streaming request used to land
		// on the streaming arm and be described there as an "empty stream".
		p.settleCost(pctx)
		return pipeline.Action{Type: pipeline.Continue}
	}
	if costing.IsEventStream(pctx) {
		// An SSE body delivered whole, with its wire framing intact — not frame by frame.
		// Both proxy listeners fall back to the buffered path for a text/event-stream
		// response when a plugin in the chain declares WritesResponseBody (a body they
		// must rewrite cannot also be forwarded as it arrives), and then deliver the
		// entire stream as this one frame. Folding it as a single chunk parses nothing; it
		// has to go through the SSE reader.
		if endpointPath(pctx) == anthropicMessagesPath {
			parseAnthropicSSE(frame, ext)
		} else {
			parseInferenceSSE(frame, ext)
		}
	} else if endpointPath(pctx) == anthropicMessagesPath {
		parseAnthropicJSON(frame, ext)
	} else {
		parseInferenceJSON(frame, ext)
	}
	logInferenceFinalized(ext)
	p.settleCost(pctx)
	pctx.Observe("matched_" + ext.Model + "_response")
	return pipeline.Action{Type: pipeline.Continue}
}

// carriesSSEFraming reports whether a frame holds RAW SSE WIRE BYTES — it has at least one
// `data:` field — rather than one payload with the framing already stripped.
//
// The question only arises on the terminal call, and the answer decides which parser can
// read it. A listener that buffered a text/event-stream response hands over the whole wire
// body, framing included; every per-frame dispatch goes through sseframe.Reader, which
// strips the framing and hands over the payload alone. Feeding wire bytes to the chunk fold
// parses nothing, and feeding a bare payload to the SSE reader parses nothing — each of
// them silently, which is why the shapes are told apart here instead.
func carriesSSEFraming(frame []byte) bool {
	if bytes.HasPrefix(bytes.TrimLeft(frame, " \t\r\n"), []byte("data:")) {
		return true
	}
	return bytes.Contains(frame, []byte("\ndata:"))
}

// foldResponseFrame folds one streamed frame into the running state via the dialect the
// endpoint speaks. Extracted so the mid-stream and terminal call sites cannot drift apart
// on which parser a path gets.
func foldResponseFrame(pctx *pipeline.Context, frame []byte, state *inferenceStreamState, ext *pipeline.InferenceExtension) {
	if endpointPath(pctx) == anthropicMessagesPath {
		foldAnthropicFrame(frame, state, ext)
		return
	}
	foldOpenAIFrame(frame, state, ext)
}

// foldOpenAIFrame folds one OpenAI streaming chunk (data: {choices,usage}) into
// the running stream state. The "[DONE]" sentinel and malformed chunks are
// skipped. Usage arrives (cumulative) when the client set
// stream_options.include_usage.
func foldOpenAIFrame(frame []byte, state *inferenceStreamState, ext *pipeline.InferenceExtension) {
	if bytes.Equal(bytes.TrimSpace(frame), []byte("[DONE]")) {
		return
	}
	var chunk inferenceStreamChunk
	if err := json.Unmarshal(frame, &chunk); err != nil {
		slog.Debug("inference-parser: malformed streaming chunk, skipping", "error", err)
		return
	}
	for _, c := range chunk.Choices {
		if c.Delta.Content != "" {
			state.completion.WriteString(c.Delta.Content)
		}
		if c.FinishReason != "" {
			ext.FinishReason = c.FinishReason
		}
	}
	// OpenAI streams cumulative usage: each usage-bearing chunk restates
	// the full totals, so replacing state.usage with the latest chunk's
	// neutral form is correct. Gate on hasAny so chunks with no usage
	// block (every non-final chunk) don't clear an accumulator that a
	// prior chunk populated.
	if chunk.Usage.hasAny() {
		state.usage = chunk.Usage.toNeutral()
		state.hasUsage = true
	}
}

func getOrCreateStreamState(pctx *pipeline.Context) *inferenceStreamState {
	if s := pipeline.GetState[inferenceStreamState](pctx, streamStateKey); s != nil {
		return s
	}
	s := &inferenceStreamState{}
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
		"model", ext.Model,
		"finishReason", ext.FinishReason,
		"promptTokens", ext.PromptTokens,
		"completionTokens", ext.CompletionTokens,
		"inputTokens", tok(parsercommon.KindInput, ext.InputTokens),
		"cacheReadTokens", tok(parsercommon.KindCacheRead, ext.CacheReadTokens),
		"cacheWriteTokens", tok(parsercommon.KindCacheWrite, ext.CacheWriteTokens),
		"outputTokens", tok(parsercommon.KindOutput, ext.OutputTokens),
		"reasoningTokens", tok(parsercommon.KindReasoning, ext.ReasoningTokens),
	)
	slog.Debug("inference-parser: completion", "text", parsercommon.Truncate(ext.Completion, parsercommon.DebugBodyMax))
}

// parseInferenceJSON parses a non-streaming OpenAI chat/completions response.
func parseInferenceJSON(body []byte, ext *pipeline.InferenceExtension) {
	var resp inferenceResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		slog.Debug("inference-parser: invalid response JSON", "error", err)
		return
	}
	if len(resp.Choices) > 0 {
		c := resp.Choices[0]
		ext.Completion = c.Message.Content
		ext.FinishReason = c.FinishReason
		for _, tc := range c.Message.ToolCalls {
			ext.ToolCalls = append(ext.ToolCalls, pipeline.InferenceToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
	}
	// No usage block: leave PresentKinds at 0 (matches SSE path).
	if resp.Usage.hasAny() {
		resp.Usage.toNeutral().Fill(ext)
	}
}

// parseInferenceSSE concatenates content deltas across SSE events and captures
// the last finish_reason and usage block (sent when stream_options.include_usage
// is set). The stream terminates with a "data: [DONE]" marker which is skipped.
//
// OpenAI streams cumulative usage: each usage-bearing chunk restates the full
// totals, so the latest chunk's neutral form is authoritative. Accumulate into
// a local TokenUsage and Fill once at the end — matching foldOpenAIFrame's
// contract, so PresentKinds and ReportedTotal reflect only the final chunk.
func parseInferenceSSE(body []byte, ext *pipeline.InferenceExtension) {
	var completion strings.Builder
	var usage parsercommon.TokenUsage
	var hasUsage bool
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		var chunk inferenceStreamChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			slog.Debug("inference-parser: skipping malformed SSE data frame", "error", err, "data", parsercommon.Truncate(string(data), 128))
			continue
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				completion.WriteString(c.Delta.Content)
			}
			if c.FinishReason != "" {
				ext.FinishReason = c.FinishReason
			}
		}
		if chunk.Usage.hasAny() {
			usage = chunk.Usage.toNeutral()
			hasUsage = true
		}
	}
	ext.Completion = completion.String()
	if hasUsage {
		usage.Fill(ext)
	}
}

type inferenceResponse struct {
	Choices []inferenceChoice `json:"choices"`
	Usage   inferenceUsage    `json:"usage"`
}

type inferenceChoice struct {
	Message      inferenceRespMessage `json:"message"`
	FinishReason string               `json:"finish_reason"`
}

// inferenceRespMessage is the response-side message shape. Separate from
// the request-side inferenceMessage (which has the multi-part content
// Unmarshaler) because responses only carry plain-string content + an
// optional tool_calls array.
type inferenceRespMessage struct {
	Role      string                  `json:"role"`
	Content   string                  `json:"content"`
	ToolCalls []inferenceRespToolCall `json:"tool_calls"`
}

// inferenceRespToolCall matches OpenAI's tool-call shape:
//
//	{"id":"call_123","type":"function","function":{"name":"...","arguments":"..."}}
type inferenceRespToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // raw JSON string
	} `json:"function"`
}

type inferenceStreamChunk struct {
	Choices []inferenceStreamChoice `json:"choices"`
	Usage   inferenceUsage          `json:"usage"`
}

type inferenceStreamChoice struct {
	Delta        inferenceDelta `json:"delta"`
	FinishReason string         `json:"finish_reason"`
}

type inferenceDelta struct {
	Content string `json:"content"`
}

// inferenceUsage decodes the OpenAI usage block. All fields are pointers
// so "key absent" is distinguishable from "key present with value 0" —
// a total-only response must not assert KindInput/KindOutput.
type inferenceUsage struct {
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
	TotalTokens      *int `json:"total_tokens"`

	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// hasAny reports whether any recognized field was on the wire. Call
// sites gate toNeutral on this so an omitted or empty usage block
// doesn't overwrite a prior chunk's state.
func (u inferenceUsage) hasAny() bool {
	return u.PromptTokens != nil || u.CompletionTokens != nil || u.TotalTokens != nil ||
		u.PromptTokensDetails != nil || u.CompletionTokensDetails != nil
}

// toNeutral maps OpenAI's usage onto TokenUsage. prompt_tokens includes
// cached_tokens on the wire — subtract to get uncached input and clamp
// at 0 for malformed responses. CacheWrite stays absent (OpenAI bills
// cache writes as ordinary input). Each Present bit is set only when
// its key was on the wire, so an absent key stays "not exposed"
// (-1 sentinel) rather than "reported zero."
func (u inferenceUsage) toNeutral() parsercommon.TokenUsage {
	usage := parsercommon.TokenUsage{}
	if u.TotalTokens != nil {
		usage.ReportedTotal = *u.TotalTokens
	}
	if u.PromptTokens != nil {
		usage.Input = *u.PromptTokens
		usage.Present |= parsercommon.KindInput
	}
	if u.CompletionTokens != nil {
		usage.Output = *u.CompletionTokens
		usage.Present |= parsercommon.KindOutput
	}
	if u.PromptTokensDetails != nil {
		cached := u.PromptTokensDetails.CachedTokens
		usage.Input -= cached
		if usage.Input < 0 {
			usage.Input = 0
		}
		usage.CacheRead = cached
		usage.Present |= parsercommon.KindCacheRead
	}
	if u.CompletionTokensDetails != nil {
		usage.Reasoning = u.CompletionTokensDetails.ReasoningTokens
		usage.Present |= parsercommon.KindReasoning
	}
	return usage
}

type inferenceRequest struct {
	Model       string             `json:"model"`
	Messages    []inferenceMessage `json:"messages"`
	Temperature *float64           `json:"temperature"`
	MaxTokens   *int               `json:"max_tokens"`
	TopP        *float64           `json:"top_p"`
	Stream      bool               `json:"stream"`
	Tools       []inferenceTool    `json:"tools"`
	ToolChoice  any                `json:"tool_choice"` // "auto"/"none" or object
}

// inferenceMessage accepts both OpenAI content shapes:
//   - "content": "plain string"
//   - "content": [{"type":"text","text":"..."}, {"type":"image_url",...}, ...]
//
// The array form is used for multi-modal input and tool-result messages.
// Non-text parts (image_url, tool_use objects, etc.) are dropped since the
// parser only exposes text for downstream policy plugins.
//
// ContentBytes records the size of the content value before that reduction,
// so a message the model was billed for doesn't read as empty just because
// none of it was text.
type inferenceMessage struct {
	Role         string
	Content      string
	ContentBytes int
}

func (m *inferenceMessage) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	m.Content = flattenContent(raw.Content)
	m.ContentBytes = contentBytes(raw.Content)
	return nil
}

// contentBytes is the wire size of a message's content value, and the source
// of InferenceMessage.ContentBytes. Absent and null content report 0 rather
// than the 4 bytes the literal `null` occupies — the field is a size signal
// for content that exists, and an assistant turn that carries only tool_calls
// has none.
//
// raw is the client's bytes verbatim, so the count includes any whitespace the
// client's serializer emitted. That is deliberate: this measures what was
// sent. Compacting first would buy comparability across clients at the cost of
// an allocation per message on every request-body parse, and would no longer
// answer "how big was this on the wire".
func contentBytes(raw json.RawMessage) int {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0
	}
	return len(raw)
}

// flattenContent returns the text representation of an OpenAI content value.
// Returns "" when content is absent, null, or contains no text parts.
func flattenContent(raw json.RawMessage) string {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" && p.Text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

type inferenceTool struct {
	Type     string            `json:"type"`
	Function inferenceFunction `json:"function"`
}

// inferenceFunction decodes the function object within an OpenAI tool
// definition. Parameters is deliberately a json.RawMessage rather than a
// map[string]any so a non-object value (string / number / null) does not
// fail the whole request decode — we fall back to nil parameters but still
// capture the tool name and description.
type inferenceFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// paramsMap decodes Parameters into a map. Returns nil if the value is
// absent or not a JSON object (e.g. a string or number); callers treat nil
// as "no schema captured" without failing the whole inference parse.
func (f inferenceFunction) paramsMap() map[string]any {
	if len(f.Parameters) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(f.Parameters, &m); err != nil {
		return nil
	}
	return m
}
