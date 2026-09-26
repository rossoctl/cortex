// Package llmclient is a small helper for calling OpenAI-compatible
// chat-completions endpoints from authbridge plugins.
//
// The package exists to lift the ~150 LOC of HTTP / wire-types /
// error-categorization boilerplate out of any plugin that needs to
// talk to an LLM (intent matchers, content scorers, policy judges,
// audit categorizers). Plugins keep ownership of the bits that are
// genuinely policy-specific: prompts, response schemas, and how to
// map the model's output to a pipeline action.
//
// Wire shape: OpenAI chat-completions (`POST /v1/chat/completions`).
// This is the lingua franca; ollama, vLLM, OpenAI itself, and most
// proxies all speak it. Anthropic-native and streaming responses are
// out of scope for the first version.
//
// Reentrancy: a plugin's outbound LLM call may itself be intercepted
// by the same plugin (e.g. IBAC's judge call passing through the
// IBAC outbound chain). Construct the Client with a
// SentinelHeaderName, then check that header at the top of OnRequest
// and short-circuit. Combined with the recommendation that LLM calls
// bypass the local listener entirely (a standalone http.Client),
// this gives defense-in-depth against loops.
//
// Error model:
//
//   - Transport / timeout / 5xx → returns a wrapped error that does
//     NOT match errors.Is(_, ErrUncertain). Plugins typically map
//     these to a 503 ("LLM unavailable") response.
//
//   - HTTP-200 but no choices, malformed content, JSON-extraction
//     failures → returns errors that DO wrap ErrUncertain. Plugins
//     typically map these to a fail-closed-deny (403): the LLM was
//     reachable, it just gave us nothing actionable.
//
// The ErrUncertain / "real failure" split mirrors the
// `ibac.judge_uncertain` (403) vs `ibac.judge_unavailable` (503)
// codes the IBAC plugin already exposes; future LLM-using plugins
// follow the same pattern by wrapping ErrUncertain in their own
// named sentinel.
package llmclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultTimeout is applied when Options.Timeout is zero.
const DefaultTimeout = 5 * time.Second

// errorBodyLimit caps how much of an upstream non-2xx body we
// surface in the error message. Misbehaving upstreams shouldn't
// be able to OOM the sidecar via giant 5xx pages.
const errorBodyLimit = 2 * 1024

// responseBodyLimit caps the JSON-decode read on a 2xx response.
// 64KB is generous for a chat completion (a 10K-token response
// is ~40KB of JSON) and bounds memory in the misbehaving-upstream
// case.
const responseBodyLimit = 64 * 1024

// Options configures a Client at construction time.
type Options struct {
	// Endpoint is the base URL of the LLM service, e.g.
	// "http://host.docker.internal:11434". The path
	// "/v1/chat/completions" is appended automatically.
	Endpoint string

	// Model is the model identifier passed to the chat-completions
	// API, e.g. "llama3.2:3b" or "gpt-4o-mini".
	Model string

	// Bearer, when non-empty, is sent as
	// "Authorization: Bearer <Bearer>" on every request.
	//
	// llmclient itself never logs request headers — error messages
	// surface only the status code and a truncated response body.
	// Plugin authors who plumb in a custom HTTPClient with verbose
	// HTTP-debug tracing (e.g. an httpdump RoundTripper) should
	// confirm that tracing redacts Authorization in production
	// builds; the bearer is sent on every call.
	Bearer string

	// Timeout bounds each call's whole request/response cycle.
	// Zero means DefaultTimeout. Contexts passed to Call /
	// CallRaw still apply on top.
	Timeout time.Duration

	// SentinelHeaderName, if non-empty, is set on every outgoing
	// request with value "1". Plugins use this as a reentrancy
	// breaker: their OnRequest checks for the header at the top
	// and short-circuits, so the LLM call doesn't loop back into
	// the plugin's own pipeline.
	SentinelHeaderName string

	// HTTPClient overrides the default http.Client. When nil,
	// llmclient constructs `&http.Client{Timeout: Timeout}`. Use
	// this to plumb in a transport with custom dialer / TLS /
	// observability — but keep in mind that http.Client.Timeout
	// is still authoritative if set.
	HTTPClient *http.Client
}

// ChatMessage is one message in a chat-completions request or
// response. Roles are typically "system", "user", "assistant".
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest is the OpenAI chat-completions request body. We model
// only the fields plugins actually pass; callers wanting more knobs
// (top_p, function calling) can use a custom http client and write
// their own request.
type ChatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens,omitempty"`

	// ResponseFormat is the OpenAI `response_format` field. The
	// common value is `{"type": "json_object"}`, which most hosted
	// models (and LiteLLM proxies) honor by suppressing the
	// markdown-fence wrapper they otherwise emit around structured
	// output. Leaving this nil preserves prior behavior — the field
	// is omitempty so locally-hosted endpoints that reject unknown
	// keys (some older Ollama builds) still see the same wire shape
	// when a plugin doesn't opt in.
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
}

// ResponseFormat is the OpenAI `response_format` request field.
// Construct with `&ResponseFormat{Type: "json_object"}` to ask the
// model for raw JSON without a markdown fence.
type ResponseFormat struct {
	Type string `json:"type"`
}

// ChatChoice is a single completion choice in the response.
type ChatChoice struct {
	Message ChatMessage `json:"message"`
}

// ChatResponse is the OpenAI chat-completions response body.
type ChatResponse struct {
	Choices []ChatChoice `json:"choices"`
}

// ErrUncertain signals "the LLM responded but its output is
// unparseable, ambiguous, or otherwise unactionable." It is
// distinct from transport / timeout / 5xx errors so callers can
// fail closed (typically HTTP 403) on uncertain output while
// failing soft (typically HTTP 503) on unavailability.
//
// Plugins SHOULD wrap this with their own named sentinel:
//
//	var ErrJudgeUncertain = fmt.Errorf("%w: judge produced bad output",
//	    llmclient.ErrUncertain)
//
// errors.Is then matches at both the generic
// (errors.Is(err, llmclient.ErrUncertain)) and plugin-specific
// (errors.Is(err, ibac.ErrJudgeUncertain)) levels.
var ErrUncertain = errors.New("LLM produced unparseable output")

// Client wraps an OpenAI-compatible chat-completions endpoint.
type Client struct {
	endpoint           string
	model              string
	bearer             string
	sentinelHeaderName string
	http               *http.Client
}

// New constructs a Client. The Endpoint is normalized
// (trailing slashes stripped); empty Endpoint or Model is permitted
// at construction but will cause Call/CallRaw to fail.
func New(opts Options) *Client {
	httpClient := opts.HTTPClient
	if httpClient == nil {
		timeout := opts.Timeout
		if timeout <= 0 {
			timeout = DefaultTimeout
		}
		httpClient = &http.Client{Timeout: timeout}
	}
	return &Client{
		endpoint:           strings.TrimRight(opts.Endpoint, "/"),
		model:              opts.Model,
		bearer:             opts.Bearer,
		sentinelHeaderName: opts.SentinelHeaderName,
		http:               httpClient,
	}
}

// Call sends a single-turn system+user prompt and returns the
// model's reply content. Caller parses the content (use
// ExtractJSON / CallStructured for JSON-shaped replies).
//
// Hardcoded knobs: Temperature is 0 (deterministic, suits
// structured-output use cases like policy verdicts) and MaxTokens
// is 1024 (room for one-sentence-reason JSON plus the markdown
// fence wrapper many hosted models — Gemini via LiteLLM in
// particular — emit reflexively around structured output). 200
// was the prior default and proved too tight: Gemini's
// "```json\n{...}\n```" preamble could exhaust the budget mid-key
// and produce unparseable truncated content. Plugins that need
// different values — higher temperature for free-form completions,
// larger MaxTokens for summarization — should call CallRaw with
// their own ChatRequest.
//
// Errors:
//   - transport / timeout / non-2xx → not wrapped with ErrUncertain
//   - 2xx with empty Choices → wrapped with ErrUncertain
func (c *Client) Call(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	resp, err := c.CallRaw(ctx, &ChatRequest{
		Model:       c.model,
		Temperature: 0,
		MaxTokens:   1024,
		Messages: []ChatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
	})
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("%w: response had no choices", ErrUncertain)
	}
	return resp.Choices[0].Message.Content, nil
}

// CallRaw is the lower-level entry point: caller provides the full
// ChatRequest. Use this for multi-turn prompts, custom temperature
// / token limits, or any field Call doesn't expose.
//
// When req.Model is empty, the Client's configured Model is used
// for the wire request. CallRaw does not mutate the caller's
// ChatRequest — plugins are free to reuse the same value across
// calls without observing model leak from a prior invocation.
func (c *Client) CallRaw(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	if c.endpoint == "" {
		return nil, errors.New("llmclient: endpoint not configured")
	}
	// Operate on a local copy so we can fill in defaults without
	// mutating the caller's request. A plugin reusing the same
	// ChatRequest across calls (e.g. a static "judge prompt"
	// template) would otherwise see the first call's resolved
	// Model leak into the next.
	effective := *req
	if effective.Model == "" {
		effective.Model = c.model
	}
	if effective.Model == "" {
		return nil, errors.New("llmclient: model not configured (and ChatRequest.Model is empty)")
	}

	body, err := json.Marshal(&effective)
	if err != nil {
		return nil, fmt.Errorf("llmclient: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.endpoint+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llmclient: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.bearer != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	if c.sentinelHeaderName != "" {
		httpReq.Header.Set(c.sentinelHeaderName, "1")
	}

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("llmclient: call failed: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(httpResp.Body, errorBodyLimit))
		return nil, fmt.Errorf("llmclient: HTTP %d: %s",
			httpResp.StatusCode, strings.TrimSpace(string(buf)))
	}

	var cr ChatResponse
	if err := json.NewDecoder(io.LimitReader(httpResp.Body, responseBodyLimit)).Decode(&cr); err != nil {
		return nil, fmt.Errorf("llmclient: decode response: %w", err)
	}
	return &cr, nil
}
