package cpex

import (
	"encoding/json"
	"fmt"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// A2A (JSON-RPC) body write-back. Like the inference re-serializers, the
// cgo adapter extracts redacted text from the CPEX-modified CMF Message
// and these functions splice it back into the ORIGINAL A2A envelope so
// unrelated fields survive. Only kind=="text" message/artifact parts
// participate — matching a2aToCMFParts on the read side — so positional
// alignment is exact and structured data/file parts are never corrupted.
// Tag-free for CGO_ENABLED=0 unit tests.

// a2aResponseParts extracts the artifact text parts from a non-streaming
// A2A JSON-RPC response body. Used on the response phase, where cpex runs
// before a2a-parser and so must parse the body itself rather than read the
// not-yet-populated a2a.Artifact (mirrors inferenceResponseParts /
// extractToolResultFromBody).
//
// Emits one assistant text part per text-kind artifact part. Streaming
// (SSE) bodies don't parse as one JSON object and yield nil — consistent
// with the response write-back failing closed on SSE.
func a2aResponseParts(body []byte) []cmfPart {
	if len(body) == 0 {
		return nil
	}
	var envelope map[string]any
	if json.Unmarshal(body, &envelope) != nil {
		return nil
	}
	result, ok := envelope["result"].(map[string]any)
	if !ok {
		return nil
	}
	artifacts, ok := result["artifacts"].([]any)
	if !ok {
		return nil
	}
	var parts []cmfPart
	for _, a := range artifacts {
		artifact, ok := a.(map[string]any)
		if !ok {
			continue
		}
		ps, ok := artifact["parts"].([]any)
		if !ok {
			continue
		}
		for _, p := range ps {
			po, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if kind, _ := po["kind"].(string); kind != "text" {
				continue
			}
			if t, ok := po["text"].(string); ok && t != "" {
				parts = append(parts, cmfPart{Kind: cmfPartText, Text: t})
			}
		}
	}
	return parts
}

// applyA2ARequestBodyMod rewrites pctx.Body — an A2A JSON-RPC request —
// replacing `params.message.parts[].text` for each text-kind part,
// positionally from newTexts (the ordered redacted text parts CPEX
// returned, which the read path produced one-per-text-part in order).
//
// Alignment is enforced: the count of text-kind parts in the body MUST
// equal len(newTexts); a mismatch returns an error so the caller fails
// closed rather than mapping a redaction onto the wrong part. Non-text
// parts (data/file) are skipped on both sides and never counted.
//
// Returns mutated=false (no error) when the body has no
// params.message.parts to rewrite; mutated=true only when a value
// actually changed.
func applyA2ARequestBodyMod(pctx *pipeline.Context, newTexts []string) (mutated bool, err error) {
	if len(pctx.Body) == 0 {
		return false, nil
	}
	var envelope map[string]any
	if err := json.Unmarshal(pctx.Body, &envelope); err != nil {
		return false, fmt.Errorf("decode A2A request body as JSON: %w", err)
	}
	parts, ok := a2aMessageParts(envelope)
	if !ok {
		// No params.message.parts — nothing to rewrite (e.g. a
		// non-message method). Not an error.
		return false, nil
	}

	// Gather the text-kind parts in order so the count check and the
	// assignment loop see the same set.
	targets := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		po, ok := p.(map[string]any)
		if !ok {
			continue
		}
		// Only non-empty text parts participate, matching the read side
		// (a2aToCMFParts / a2aResponseParts both drop empty text). Counting
		// empty text-kind parts here would drift against newTexts and
		// trip a false count-mismatch error.
		if kind, _ := po["kind"].(string); kind == "text" {
			if t, ok := po["text"].(string); ok && t != "" {
				targets = append(targets, po)
			}
		}
	}

	if len(targets) != len(newTexts) {
		return false, fmt.Errorf(
			"A2A request part count drift: body has %d text parts, CPEX returned %d text parts",
			len(targets), len(newTexts))
	}

	changed := false
	for i, po := range targets {
		if po["text"] != newTexts[i] {
			po["text"] = newTexts[i]
			changed = true
		}
	}
	if !changed {
		return false, nil
	}

	newBody, err := json.Marshal(envelope)
	if err != nil {
		return false, fmt.Errorf("re-serialize A2A request body: %w", err)
	}
	pctx.SetBody(newBody)
	return true, nil
}

// applyA2AResponseBodyMod rewrites pctx.ResponseBody — a non-streaming
// A2A JSON-RPC response — replacing the single artifact text part with
// newArtifact (the redacted text CPEX returned on the response phase).
//
// Because the write side receives only one redacted text while the read
// side (a2aResponseParts) emits one text part per non-empty text-kind
// artifact part, a response carrying more than one such part is an
// ambiguous single-value rewrite — we fail closed rather than overwrite
// only the first part and forward the rest unredacted. Streaming (SSE)
// responses don't parse as one JSON object and also fail closed.
//
// Returns mutated=false (no error) when newArtifact is empty or there's
// no artifact text part to replace.
func applyA2AResponseBodyMod(pctx *pipeline.Context, newArtifact string) (mutated bool, err error) {
	if len(pctx.ResponseBody) == 0 || newArtifact == "" {
		return false, nil
	}
	var envelope map[string]any
	if err := json.Unmarshal(pctx.ResponseBody, &envelope); err != nil {
		return false, fmt.Errorf("decode A2A response body as JSON (streaming responses are not rewritable): %w", err)
	}
	result, ok := envelope["result"].(map[string]any)
	if !ok {
		return false, nil
	}
	artifacts, ok := result["artifacts"].([]any)
	if !ok {
		return false, nil
	}

	// Collect all non-empty text-kind artifact parts, matching what the
	// read side surfaced. We only hold one redacted text, so >1 such
	// part is ambiguous: fail closed instead of stamping the same value
	// over distinct parts or rewriting only the first.
	targets := make([]map[string]any, 0, 4)
	for _, a := range artifacts {
		artifact, ok := a.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := artifact["parts"].([]any)
		if !ok {
			continue
		}
		for _, p := range parts {
			po, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if kind, _ := po["kind"].(string); kind != "text" {
				continue
			}
			if t, ok := po["text"].(string); ok && t != "" {
				targets = append(targets, po)
			}
		}
	}

	if len(targets) == 0 {
		return false, nil
	}
	if len(targets) > 1 {
		return false, fmt.Errorf(
			"A2A response has %d text parts; single-value redaction rewrite is ambiguous",
			len(targets))
	}
	if targets[0]["text"] == newArtifact {
		return false, nil
	}
	targets[0]["text"] = newArtifact

	newBody, err := json.Marshal(envelope)
	if err != nil {
		return false, fmt.Errorf("re-serialize A2A response body: %w", err)
	}
	pctx.SetResponseBody(newBody)
	return true, nil
}

// a2aMessageParts navigates a decoded A2A JSON-RPC request envelope to
// params.message.parts, returning the parts slice and ok=false when the
// path is absent or the wrong shape.
func a2aMessageParts(envelope map[string]any) ([]any, bool) {
	params, ok := envelope["params"].(map[string]any)
	if !ok {
		return nil, false
	}
	message, ok := params["message"].(map[string]any)
	if !ok {
		return nil, false
	}
	parts, ok := message["parts"].([]any)
	return parts, ok
}
