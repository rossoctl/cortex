package cpex

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferenceparser"
)

// Inference (OpenAI chat/completions) body write-back. The cgo adapter
// (manager_cpex.go) extracts the redacted text from a CPEX-modified CMF
// Message and hands it here; these re-serializers re-parse the ORIGINAL
// request/response envelope and replace only the message/completion text,
// so every other field the upstream cares about (model, temperature,
// tools, usage, …) survives untouched. Mirrors the MCP re-serializers in
// cmf_body.go; kept tag-free so it's unit-tested under CGO_ENABLED=0.

// inferenceResponse is what cpex reads from a buffered inference response.
// Response hooks run in reverse pipeline order, so cpex runs before
// inference-parser's OnResponse and reads the body itself.
type inferenceResponse struct {
	// parts are the CMF content parts: completion text, then tool calls.
	parts []cmfPart
	// calls are the tool calls as the model sent them, arguments unparsed.
	calls []pipeline.InferenceToolCall
	// decoded reports that the body was read as a response of its dialect.
	decoded bool
	// rewritable reports that a changed completion can be written back:
	// a non-streaming OpenAI chat response. Elsewhere only deny applies.
	rewritable bool
}

// readInferenceResponse reads pctx.ResponseBody. A non-streaming OpenAI
// chat response yields a text part per choice with string content plus its
// tool calls; every other dialect and every stream is read with
// inference-parser's own normalizer, so a streamed tool call is the one it
// assembles, and yields the completion text and the tool calls.
func readInferenceResponse(pctx *pipeline.Context) inferenceResponse {
	body := pctx.ResponseBody
	if len(body) == 0 {
		return inferenceResponse{}
	}
	if isOpenAIDialect(pctx.Path) {
		if r, ok := openAIChatResponse(body); ok {
			return r
		}
	}
	ext := inferenceparser.ParseResponse(pctx.Path, body)
	if ext == nil || (!ext.StreamedResponse && !json.Valid(body)) {
		return inferenceResponse{}
	}
	r := inferenceResponse{decoded: true, calls: ext.ToolCalls}
	if ext.Completion != "" {
		r.parts = append(r.parts, cmfPart{Kind: cmfPartText, Text: ext.Completion})
	}
	for _, tc := range ext.ToolCalls {
		if tc.Name != "" {
			r.parts = append(r.parts, toolCallPart(tc))
		}
	}
	return r
}

// openAIChatResponse reads a non-streaming OpenAI chat/completions body,
// which ok reports it is.
func openAIChatResponse(body []byte) (r inferenceResponse, ok bool) {
	var envelope map[string]any
	if json.Unmarshal(body, &envelope) != nil {
		return r, false
	}
	r.decoded, r.rewritable = true, true
	choices, _ := envelope["choices"].([]any)
	for _, ch := range choices {
		choice, ok := ch.(map[string]any)
		if !ok {
			continue
		}
		msg, ok := choice["message"].(map[string]any)
		if !ok {
			// A legacy completions choice: readable, not rewritable.
			if c, _ := choice["text"].(string); c != "" {
				r.parts = append(r.parts, cmfPart{Kind: cmfPartText, Text: c})
				r.rewritable = false
			}
			continue
		}
		if c, ok := msg["content"].(string); ok && c != "" {
			r.parts = append(r.parts, cmfPart{Kind: cmfPartText, Text: c})
		}
		toolCalls, _ := msg["tool_calls"].([]any)
		for _, t := range toolCalls {
			to, ok := t.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := to["function"].(map[string]any)
			name, _ := fn["name"].(string)
			if name == "" {
				continue
			}
			args, _ := fn["arguments"].(string)
			id, _ := to["id"].(string)
			tc := pipeline.InferenceToolCall{ID: id, Name: name, Arguments: args}
			r.calls = append(r.calls, tc)
			r.parts = append(r.parts, toolCallPart(tc))
		}
	}
	return r, true
}

func toolCallPart(tc pipeline.InferenceToolCall) cmfPart {
	return cmfPart{
		Kind:          cmfPartToolCall,
		Name:          tc.Name,
		Arguments:     parseToolArgs(tc.Arguments),
		CorrelationID: tc.ID,
	}
}

// applyInferenceRequestBodyMod rewrites pctx.Body — an OpenAI-style
// chat/completions request — replacing each message's string `content`
// positionally from newContents. newContents is the ordered list of
// redacted text parts CPEX returned, which the read path
// (inferenceToCMFParts) produced one-per-message for every message with
// non-empty string content, in document order. Messages with role "tool"
// are skipped: the read path sends them as tool-result parts.
//
// Alignment is enforced, not assumed: the count of body messages with
// non-empty string content MUST equal len(newContents). A mismatch means
// the body shape diverged from what the read path saw (e.g. multimodal
// array content, a message added/removed mid-pipeline), so rather than
// risk landing a redaction on the wrong message we return an error and
// let the caller fail closed. Messages whose content isn't a non-empty
// string (array/multimodal content, tool-call assistant turns with null
// content) are skipped on both sides and never counted.
//
// Returns mutated=true only when at least one content value actually
// changed; an all-identical rewrite is reported as mutated=false (no
// SetBody call), matching the MCP re-serializers' no-op contract.
func applyInferenceRequestBodyMod(pctx *pipeline.Context, newContents []string) (mutated bool, err error) {
	if len(pctx.Body) == 0 {
		if len(newContents) > 0 {
			return false, fmt.Errorf("inference request has no body to apply %d text parts to", len(newContents))
		}
		return false, nil
	}
	var envelope map[string]any
	if err := json.Unmarshal(pctx.Body, &envelope); err != nil {
		return false, fmt.Errorf("decode inference request body as JSON: %w", err)
	}
	// Without a messages array (a legacy completions prompt) there is no
	// target, which the count check below refuses unless nothing came back.
	rawMsgs, _ := envelope["messages"].([]any)

	// Collect the string-content messages in order, so the count check
	// and the assignment loop see exactly the same set.
	targets := make([]map[string]any, 0, len(rawMsgs))
	for _, m := range rawMsgs {
		mo, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := mo["role"].(string); role == roleTool {
			continue
		}
		if c, ok := mo["content"].(string); ok && c != "" {
			targets = append(targets, mo)
		}
	}

	if len(targets) != len(newContents) {
		return false, fmt.Errorf(
			"inference request message count drift: body has %d string-content messages, CPEX returned %d text parts",
			len(targets), len(newContents))
	}

	changed := false
	for i, mo := range targets {
		if mo["content"] != newContents[i] {
			mo["content"] = newContents[i]
			changed = true
		}
	}
	if !changed {
		return false, nil
	}

	newBody, err := json.Marshal(envelope)
	if err != nil {
		return false, fmt.Errorf("re-serialize inference request body: %w", err)
	}
	pctx.SetBody(newBody)
	return true, nil
}

// applyInferenceResponseBodyMod rewrites pctx.ResponseBody — a
// non-streaming OpenAI chat/completions response — replacing the
// assistant completion text with newCompletion (the single redacted text
// part CPEX returned on the response phase).
//
// It rewrites the single string-content choice's `message.content`.
// Because the write side receives only one redacted completion while the
// read side (inferenceResponseParts) emits one text part per choice, a
// response carrying more than one string-content choice (n>1) is an
// ambiguous single-value rewrite — we fail closed rather than overwrite
// the other choices' distinct completions with one value. Streaming
// responses (SSE: a sequence of `data:` frames, not a single JSON object)
// are not rewritable here — the body won't parse as one JSON object, so we
// return an error and let the caller fail closed rather than forward an
// unredacted stream.
//
// Returns mutated=false (no error) when there's nothing to change: no
// choices, no string content to replace, or newCompletion equal to it. An
// empty newCompletion is a redaction like any other.
func applyInferenceResponseBodyMod(pctx *pipeline.Context, newCompletion string) (mutated bool, err error) {
	if len(pctx.ResponseBody) == 0 {
		return false, nil
	}
	var envelope map[string]any
	if err := json.Unmarshal(pctx.ResponseBody, &envelope); err != nil {
		// Likely an SSE stream or otherwise non-JSON body. A redaction
		// was requested but can't be applied — fail closed.
		return false, fmt.Errorf("decode inference response body as JSON (streaming responses are not rewritable): %w", err)
	}

	choices, ok := envelope["choices"].([]any)
	if !ok {
		return false, nil
	}

	// Collect the choices carrying string content, matching what the read
	// side surfaced. We only hold one redacted completion, so >1 such
	// choice is ambiguous: fail closed instead of stamping the same value
	// over distinct completions.
	targets := make([]map[string]any, 0, len(choices))
	for _, ch := range choices {
		choice, ok := ch.(map[string]any)
		if !ok {
			continue
		}
		msg, ok := choice["message"].(map[string]any)
		if !ok {
			continue
		}
		if c, ok := msg["content"].(string); ok && c != "" {
			targets = append(targets, msg)
		}
	}
	if len(targets) == 0 {
		return false, nil
	}
	if len(targets) > 1 {
		return false, fmt.Errorf(
			"inference response has %d string-content choices; single-value redaction rewrite is ambiguous",
			len(targets))
	}
	if targets[0]["content"] == newCompletion {
		return false, nil
	}
	targets[0]["content"] = newCompletion

	newBody, err := json.Marshal(envelope)
	if err != nil {
		return false, fmt.Errorf("re-serialize inference response body: %w", err)
	}
	pctx.SetResponseBody(newBody)
	return true, nil
}

// applyInferenceRequestMod applies the text parts and tool-result contents
// CPEX returned for an inference request. It reports no change when they
// equal what was sent, so a label- or security-only result is not a
// modification. A real change is applied only to an OpenAI chat request's
// message text; a changed tool result, or any change to another dialect's
// body, has no re-serializer and is an error the caller fails closed on.
func applyInferenceRequestMod(pctx *pipeline.Context, texts []string, toolResults []any) (mutated bool, err error) {
	var sentTexts []string
	var sentResults []any
	for _, part := range inferenceToCMFParts(pctx.Extensions.Inference) {
		switch part.Kind {
		case cmfPartText:
			sentTexts = append(sentTexts, part.Text)
		case cmfPartToolResult:
			sentResults = append(sentResults, part.Content)
		}
	}
	resultsChanged := !jsonEqual(sentResults, toolResults)
	if !resultsChanged && jsonEqual(sentTexts, texts) {
		return false, nil
	}
	if resultsChanged {
		return false, fmt.Errorf("tool result rewrite on an inference request is not supported")
	}
	if !isOpenAIDialect(pctx.Path) {
		return false, fmt.Errorf("body modification is supported on OpenAI chat requests only, not %q", pctx.Path)
	}
	return applyInferenceRequestBodyMod(pctx, texts)
}

// applyInferenceResponseMod applies the content parts CPEX returned for an
// inference response, compared with what readInferenceResponse sent. No
// change is a no-op. A changed completion text is rewritten on a
// non-streaming OpenAI chat response; any other change (a tool call
// altered, added or removed, a text part removed, or any change to another
// dialect or a stream) has no re-serializer and is an error the caller
// fails closed on.
func applyInferenceResponseMod(pctx *pipeline.Context, returned []cmfPart) (mutated bool, err error) {
	sent := readInferenceResponse(pctx)
	sentTexts, sentCalls := splitResponseParts(sent.parts)
	texts, calls := splitResponseParts(returned)
	callsChanged := !jsonEqual(sentCalls, calls)
	if !callsChanged && jsonEqual(sentTexts, texts) {
		return false, nil
	}
	if callsChanged {
		return false, fmt.Errorf("tool call rewrite on an inference response is not supported")
	}
	if !sent.rewritable {
		return false, fmt.Errorf("body modification is supported on non-streaming OpenAI chat responses only, not %q", pctx.Path)
	}
	if len(texts) != len(sentTexts) || len(texts) != 1 {
		return false, fmt.Errorf("inference response sent %d text parts, CPEX returned %d; only a single completion is rewritable",
			len(sentTexts), len(texts))
	}
	return applyInferenceResponseBodyMod(pctx, texts[0])
}

// splitResponseParts separates response parts into completion texts and
// comparable tool calls.
func splitResponseParts(parts []cmfPart) (texts []string, calls []any) {
	for _, p := range parts {
		switch p.Kind {
		case cmfPartText:
			texts = append(texts, p.Text)
		case cmfPartToolCall:
			calls = append(calls, map[string]any{"id": p.CorrelationID, "name": p.Name, "arguments": p.Arguments})
		}
	}
	return texts, calls
}

// isOpenAIDialect reports whether path is an OpenAI completions endpoint,
// matched by its ending as inference-parser matches it.
func isOpenAIDialect(path string) bool {
	return strings.HasSuffix(path, "/completions")
}

// jsonEqual reports whether a and b encode to the same JSON. Values decoded
// from MessagePack and from JSON differ in Go type (int64 against float64)
// while meaning the same thing; nil and empty slices compare equal.
func jsonEqual(a, b any) bool {
	ja, errA := json.Marshal(normalizeEmpty(a))
	jb, errB := json.Marshal(normalizeEmpty(b))
	return errA == nil && errB == nil && string(ja) == string(jb)
}

func normalizeEmpty(v any) any {
	switch x := v.(type) {
	case []string:
		if len(x) == 0 {
			return nil
		}
	case []any:
		if len(x) == 0 {
			return nil
		}
	}
	return v
}

// Custom extension keys carrying an inference request's tool results and
// an inference response's tool calls.
const (
	customToolResults     = "tool_results"
	customToolResultCount = "tool_result_count"
	customToolCalls       = "tool_calls"
	customToolCallCount   = "tool_call_count"
)

// inferenceToolResultsCustom returns an inference request's tool results
// for the custom extension: an OpenAI chat message with role "tool" and
// each Anthropic tool_result block with text, in that order, keyed by
// position ("0", "1", ...) because CPEX's attribute bag cannot hold a list
// of objects. Each carries tool_use_id, is_error, and its content cut to
// egressExcerptBytes with truncated set when it was longer. results is nil
// when there are none. This is policy input only and is never recorded.
func inferenceToolResultsCustom(inf *pipeline.InferenceExtension) (count int, results map[string]any) {
	if inf == nil {
		return 0, nil
	}
	add := func(id, content string, isError bool) {
		if content == "" {
			return
		}
		excerpt, truncated := bodyExcerpt([]byte(content))
		if results == nil {
			results = map[string]any{}
		}
		results[strconv.Itoa(count)] = map[string]any{
			"tool_use_id": id,
			"content":     excerpt,
			"is_error":    isError,
			"truncated":   truncated,
		}
		count++
	}
	for _, m := range inf.Messages {
		// A chat-completions result the parser also recorded in ToolResults (named by its
		// ToolUseID) is added by the loop below; adding the message copy too would count it
		// twice in the policy input.
		if m.Role == roleTool && m.ToolUseID == "" {
			add("", m.Content, false)
		}
	}
	for _, r := range inf.ToolResults {
		add(r.ToolUseID, r.Content, r.IsError)
	}
	return count, results
}

// inferenceToolCallsCustom returns an inference response's tool calls for
// the custom extension, keyed by position as inferenceToolResultsCustom
// keys results: CEL on an llm route reads neither tool-call parts nor more
// than the first one's arguments. Each carries id, name, and arguments as
// the JSON text the model sent, cut to egressExcerptBytes with truncated
// set when it was longer. This is policy input only and is never recorded.
func inferenceToolCallsCustom(calls []pipeline.InferenceToolCall) (count int, out map[string]any) {
	for _, tc := range calls {
		args, truncated := bodyExcerpt([]byte(tc.Arguments))
		if out == nil {
			out = map[string]any{}
		}
		out[strconv.Itoa(count)] = map[string]any{
			"id":        tc.ID,
			"name":      tc.Name,
			"arguments": args,
			"truncated": truncated,
		}
		count++
	}
	return count, out
}
