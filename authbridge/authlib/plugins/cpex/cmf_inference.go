package cpex

import (
	"encoding/json"
	"fmt"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// Inference (OpenAI chat/completions) body write-back. The cgo adapter
// (manager_cpex.go) extracts the redacted text from a CPEX-modified CMF
// Message and hands it here; these re-serializers re-parse the ORIGINAL
// request/response envelope and replace only the message/completion text,
// so every other field the upstream cares about (model, temperature,
// tools, usage, …) survives untouched. Mirrors the MCP re-serializers in
// cmf_body.go; kept tag-free so it's unit-tested under CGO_ENABLED=0.

// inferenceResponseParts extracts the structured CMF content parts from a
// non-streaming OpenAI chat/completions response body. Used on the
// response phase, where cpex runs before inference-parser and so must
// parse the body itself rather than read the not-yet-populated
// inf.Completion / inf.ToolCalls (mirrors extractToolResultFromBody for
// MCP).
//
// Emits an assistant text part per choice with string content (redactable
// via cmf.llm_output) plus a tool_call part per tool call the model
// emitted. Streaming (SSE) bodies don't parse as one JSON object and yield
// nil — there's no structured content to redact, and the matching
// write-back fails closed on SSE.
func inferenceResponseParts(body []byte) []cmfPart {
	if len(body) == 0 {
		return nil
	}
	var envelope map[string]any
	if json.Unmarshal(body, &envelope) != nil {
		return nil
	}
	choices, _ := envelope["choices"].([]any)
	var parts []cmfPart
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
			parts = append(parts, cmfPart{Kind: cmfPartText, Text: c})
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
			argsStr, _ := fn["arguments"].(string)
			id, _ := to["id"].(string)
			parts = append(parts, cmfPart{
				Kind:          cmfPartToolCall,
				Name:          name,
				Arguments:     parseToolArgs(argsStr),
				CorrelationID: id,
			})
		}
	}
	return parts
}

// applyInferenceRequestBodyMod rewrites pctx.Body — an OpenAI-style
// chat/completions request — replacing each message's string `content`
// positionally from newContents. newContents is the ordered list of
// redacted text parts CPEX returned, which the read path
// (inferenceToCMFParts) produced one-per-message for every message with
// non-empty string content, in document order.
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
		return false, nil
	}
	var envelope map[string]any
	if err := json.Unmarshal(pctx.Body, &envelope); err != nil {
		return false, fmt.Errorf("decode inference request body as JSON: %w", err)
	}
	rawMsgs, ok := envelope["messages"].([]any)
	if !ok {
		// No messages array — nothing we know how to rewrite. Not an
		// error: a request without messages simply has no redaction
		// target (mirrors MCP's "no params" no-op).
		return false, nil
	}

	// Collect the string-content messages in order, so the count check
	// and the assignment loop see exactly the same set.
	targets := make([]map[string]any, 0, len(rawMsgs))
	for _, m := range rawMsgs {
		mo, ok := m.(map[string]any)
		if !ok {
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
// It rewrites `choices[].message.content` for every choice that currently
// carries a string content. Streaming responses (SSE: a sequence of
// `data:` frames, not a single JSON object) are not rewritable here — the
// body won't parse as one JSON object, so we return an error and let the
// caller fail closed rather than forward an unredacted stream.
//
// Returns mutated=false (no error) when there's nothing to change:
// newCompletion empty, no choices, or no string content to replace.
func applyInferenceResponseBodyMod(pctx *pipeline.Context, newCompletion string) (mutated bool, err error) {
	if len(pctx.ResponseBody) == 0 || newCompletion == "" {
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
	changed := false
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
			msg["content"] = newCompletion
			changed = true
		}
	}
	if !changed {
		return false, nil
	}

	newBody, err := json.Marshal(envelope)
	if err != nil {
		return false, fmt.Errorf("re-serialize inference response body: %w", err)
	}
	pctx.SetResponseBody(newBody)
	return true, nil
}
