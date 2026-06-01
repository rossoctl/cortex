package llmclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// truncateForLog caps content excerpts shown in error messages so a
// pathological 100KB completion doesn't end up in pod logs.
const truncateForLog = 200

// ExtractJSON extracts the first top-level JSON object from content
// and unmarshals it into T. Models commonly wrap JSON in markdown
// code fences, prefix it with prose ("Sure, here's the verdict:"),
// or sandwich it between explanation paragraphs — this helper
// strips through all of that with a `{...}` bracket scan.
//
// Failure (no `{...}` found, malformed JSON, JSON that doesn't fit
// T's shape) returns an error wrapping ErrUncertain.
//
// Limitation: the scan picks the first `{` and the last `}`, so a
// reply that contains multiple JSON objects (e.g. nested examples
// in prose: `for instance {"hint":"..."} you might emit
// {"verdict":"deny"}`) will produce a span covering both and fail
// to unmarshal. The failure is fail-closed (wraps ErrUncertain →
// caller maps to 403 fail-closed-deny), so security posture is
// preserved, but plugin authors should instruct the model to emit
// a single JSON object with no other braced content around it.
func ExtractJSON[T any](content string) (T, error) {
	var zero T
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start < 0 || end <= start {
		return zero, fmt.Errorf("%w: no JSON object in content: %q",
			ErrUncertain, truncate(content, truncateForLog))
	}
	var out T
	if err := json.Unmarshal([]byte(content[start:end+1]), &out); err != nil {
		return zero, fmt.Errorf("%w: unmarshal: %v (content %q)",
			ErrUncertain, err, truncate(content, truncateForLog))
	}
	return out, nil
}

// CallStructured wraps Call + ExtractJSON: send a system+user
// prompt pair and return a value of type T parsed from the model's
// reply content.
//
// Errors fall into two buckets:
//   - underlying Call errors (transport, 5xx, no choices) propagate
//     unchanged — they are NOT wrapped with ErrUncertain unless
//     Call itself wrapped them
//   - JSON extraction / unmarshal failures wrap ErrUncertain
//
// CallStructured does no schema validation beyond JSON unmarshal
// — semantic checks (e.g. "verdict must be allow or deny") are
// the caller's responsibility.
func CallStructured[T any](ctx context.Context, c *Client, systemPrompt, userPrompt string) (T, error) {
	var zero T
	content, err := c.Call(ctx, systemPrompt, userPrompt)
	if err != nil {
		return zero, err
	}
	return ExtractJSON[T](content)
}

// CallStructuredRaw is CallStructured with caller-controlled request
// shape — use this when a plugin needs to override MaxTokens, set
// ResponseFormat, or otherwise tune the wire request beyond what
// Call hardcodes. The same error model applies as CallStructured.
//
// req.Model defaults to the Client's configured Model when empty.
// CallStructuredRaw does not mutate req.
func CallStructuredRaw[T any](ctx context.Context, c *Client, req *ChatRequest) (T, error) {
	var zero T
	resp, err := c.CallRaw(ctx, req)
	if err != nil {
		return zero, err
	}
	if len(resp.Choices) == 0 {
		return zero, fmt.Errorf("%w: response had no choices", ErrUncertain)
	}
	return ExtractJSON[T](resp.Choices[0].Message.Content)
}

// truncate caps s at n runes (not bytes), appending an ellipsis when it
// truncates. Rune-safe matters here because the helper is used on model
// output that lands in slog / wrapped error messages — a byte-slice
// could split a multi-byte UTF-8 sequence and produce U+FFFD in logs.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
