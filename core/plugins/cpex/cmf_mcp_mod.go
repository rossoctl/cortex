package cpex

import (
	"encoding/json"
	"fmt"

	"github.com/rossoctl/cortex/core/pipeline"
)

// MCPRequestBodyMod describes what changes to apply to an MCP JSON-RPC
// request body. The cgo adapter extracts these values from a CPEX-
// modified CMF Message and hands them to applyMCPRequestBodyMod.
//
// Exactly one field is consumed per method:
//
//	tools/call     → NewArguments  → params.arguments
//	prompts/get    → NewArguments  → params.arguments
//	resources/read → NewURI        → params.uri
//
// ArgsSet / URISet are the explicit "this field carries a real
// modification" signals. We do NOT infer intent from emptiness: a
// policy whose redaction is "strip all arguments" legitimately returns
// an empty (or nil) arguments map, and a "blank the URI" redaction
// returns an empty URI. Treating empty-as-no-op would silently forward
// the ORIGINAL body (secret intact) while the policy believed it had
// redacted. The cgo adapter sets these flags when the corresponding
// CMF content part is present, regardless of value.
//
// Other methods are no-ops — the operator's APL policy can't rewrite
// protocol mechanics (initialize, tools/list, etc.) because there's
// no semantically meaningful target for the rewrite.
type MCPRequestBodyMod struct {
	NewArguments map[string]any
	ArgsSet      bool
	NewURI       string
	URISet       bool
}

// applyMCPRequestBodyMod rewrites pctx.Body, which is expected to be
// an MCP JSON-RPC request. Returns mutated=true when the body was
// changed and SetBody was called; returns mutated=false (with no
// error) when the original body didn't match the expected shape
// (e.g., parsed JSON has no `params` object, method is unsupported,
// or the mod struct's relevant field is empty for the method), or the new
// value equals the one already there.
func applyMCPRequestBodyMod(pctx *pipeline.Context, method string, mod MCPRequestBodyMod) (mutated bool, err error) {
	if len(pctx.Body) == 0 {
		return false, nil
	}
	var envelope map[string]any
	if err := json.Unmarshal(pctx.Body, &envelope); err != nil {
		return false, fmt.Errorf("decode request body as JSON: %w", err)
	}
	params, _ := envelope["params"].(map[string]any)
	if params == nil {
		return false, nil
	}

	switch method {
	case "tools/call", "prompts/get":
		// Gate on the explicit ArgsSet flag, NOT len()==0: a policy that
		// stripped all arguments returns an empty map and still means to
		// rewrite. Inferring no-op from emptiness would forward the
		// original (un-redacted) arguments.
		if !mod.ArgsSet || jsonEqual(params["arguments"], mod.NewArguments) {
			return false, nil
		}
		params["arguments"] = mod.NewArguments
	case "resources/read":
		if !mod.URISet || params["uri"] == mod.NewURI {
			return false, nil
		}
		params["uri"] = mod.NewURI
	default:
		return false, nil
	}

	newBody, err := json.Marshal(envelope)
	if err != nil {
		return false, fmt.Errorf("re-serialize request body: %w", err)
	}
	pctx.SetBody(newBody)
	return true, nil
}

// applyMCPResponseBodyMod rewrites pctx.ResponseBody for a tools/call
// JSON-RPC response. The new content replaces:
//
//   - result.content[].text — the canonical MCP text block, stringified
//     as JSON (pretty-printed when possible).
//   - result.structuredContent — only when the original response had it.
//     Don't introduce structuredContent on a response that didn't carry
//     it; clients sniffing for the new shape would be surprised.
//
// Fail-closed on multiple text blocks: the read side
// (extractToolResultContent) surfaces only the FIRST text block to the
// policy, and we hold a single redacted value. A result.content[] with
// >1 text block is therefore an ambiguous rewrite — blocks[1..] were
// never shown to the policy, so rewriting only block[0] would forward
// the others (potentially carrying the same secret) verbatim while
// reporting success. We return an error so the caller fails closed
// rather than leaking the un-inspected blocks (mirrors the A2A and
// inference response guards). Rewrite all blocks would be wrong too —
// we'd stamp one value over distinct blocks.
//
// Returns mutated=true when SetResponseBody was called; mutated=false
// when the body wasn't a tools/call response, had no result.content,
// the response had no replaceable text block, or newContent equals the
// result the policy was shown.
func applyMCPResponseBodyMod(pctx *pipeline.Context, method string, newContent any) (mutated bool, err error) {
	if method != "tools/call" {
		return false, nil
	}
	if len(pctx.ResponseBody) == 0 || newContent == nil {
		return false, nil
	}
	var envelope map[string]any
	if err := json.Unmarshal(pctx.ResponseBody, &envelope); err != nil {
		return false, fmt.Errorf("decode response body as JSON: %w", err)
	}
	result, ok := envelope["result"].(map[string]any)
	if !ok || jsonEqual(extractToolResultContent(result), newContent) {
		return false, nil
	}

	replaced := false

	// Primary path: rewrite the text block in result.content[]. Collect
	// all text blocks first so >1 fails closed rather than silently
	// leaving blocks[1..] unredacted (the read side only inspected the
	// first; we only hold one redacted value).
	if contentArr, ok := result["content"].([]any); ok {
		textBlocks := make([]map[string]any, 0, len(contentArr))
		for _, block := range contentArr {
			blockObj, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := blockObj["type"].(string); ok && t == "text" {
				textBlocks = append(textBlocks, blockObj)
			}
		}
		if len(textBlocks) > 1 {
			return false, fmt.Errorf(
				"MCP tools/call response has %d text blocks; single-value redaction rewrite is ambiguous",
				len(textBlocks))
		}
		if len(textBlocks) == 1 {
			textBlocks[0]["text"] = stringifyForTextBlock(newContent)
			replaced = true
		}
	}

	// Secondary path: update structuredContent when it was already
	// present. Operators can opt clients into the newer
	// structuredContent shape by populating it in the upstream
	// response; CPEX rewrites it consistently. We never introduce
	// it on a response that lacked it.
	if _, has := result["structuredContent"]; has {
		result["structuredContent"] = newContent
		replaced = true
	}

	if !replaced {
		return false, nil
	}

	newBody, err := json.Marshal(envelope)
	if err != nil {
		return false, fmt.Errorf("re-serialize response body: %w", err)
	}
	pctx.SetResponseBody(newBody)
	return true, nil
}

// stringifyForTextBlock turns the modified content value back into the
// string the MCP text block expects. We try pretty-printed JSON first
// (matches the upstream MCP server's typical output, makes diffs
// readable in session events); fall back to compact JSON on error;
// fall back to %v on impossible-to-marshal types.
func stringifyForTextBlock(v any) string {
	if s, ok := v.(string); ok {
		// Common case: the redactor handed back a string verbatim.
		return s
	}
	if b, err := json.MarshalIndent(v, "", "  "); err == nil {
		return string(b)
	}
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return fmt.Sprintf("%v", v)
}
