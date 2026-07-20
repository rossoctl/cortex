package ibac

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/llmclient"
)

// Judge evaluates whether a proposed agent action aligns with a recorded
// user intent. Implementations are responsible for whatever policy logic
// they want — calling an LLM, consulting a rules engine, etc.
//
// Verdict is "allow" or "deny"; any other value is treated as "deny" by
// the plugin (fail-closed). Reason is a short human-readable explanation
// surfaced in Invocation.Details so operators can see why a request was
// blocked without re-running the judge.
//
// Errors are categorized so the plugin can surface the right code:
//
//   - errors.Is(err, ErrJudgeUncertain) — judge ran but emitted an
//     unparseable / ambiguous response. Treated as a fail-closed deny
//     (HTTP 403 ibac.judge_uncertain) — different from "judge down"
//     so operators don't conflate model-output bugs with infra outages.
//
//   - any other error — transport failure, timeout, 5xx from the
//     judge endpoint. Treated as judge unavailable (HTTP 503
//     ibac.judge_unavailable).
//
// Implementations only return verdict="deny" for actual policy denials.
type Judge interface {
	Evaluate(ctx context.Context, intent, action string) (verdict, reason string, err error)
}

// ErrJudgeUncertain is the sentinel returned (wrapped) when the judge
// produced output we couldn't extract a verdict from. It also wraps
// llmclient.ErrUncertain so generic LLM-output checks
// (errors.Is(err, llmclient.ErrUncertain)) match too.
var ErrJudgeUncertain = fmt.Errorf("%w: judge produced unparseable output", llmclient.ErrUncertain)

// httpJudge calls an OpenAI chat-completions-compatible endpoint with a
// system+user prompt asking the model to compare intent vs action and
// return a structured verdict.
//
// The judge's outbound HTTP call is made directly via llmclient — NOT
// routed back through the authbridge listener. That structurally
// prevents the reentrancy that would otherwise occur (IBAC's judge call
// triggering IBAC's OnRequest in a loop). Defense-in-depth: every
// outbound judge request also carries `X-IBAC-Judge: 1`, which the
// plugin's OnRequest checks at the very top and short-circuits on. So
// even if a misconfiguration ever sent the judge call back through the
// proxy, IBAC would skip itself.
type httpJudge struct {
	client       *llmclient.Client
	systemPrompt string
	maxTokens    int
	jsonMode     bool
}

// newHTTPJudge constructs a judge that POSTs chat-completion requests to
// endpoint+"/v1/chat/completions". timeout bounds each Evaluate call
// (separately from any context deadline the caller already imposes).
// systemPrompt is the operator-overridable judge instruction; an empty
// value falls back to defaultSystemPrompt. maxTokens caps the reply
// length on every judge call. jsonMode, when true, sets
// response_format: {"type": "json_object"} so hosted models suppress
// the markdown-fence wrapper they otherwise emit around JSON.
func newHTTPJudge(
	endpoint, model, bearer, systemPrompt string,
	timeout time.Duration,
	maxTokens int,
	jsonMode bool,
) *httpJudge {
	if systemPrompt == "" {
		systemPrompt = defaultSystemPrompt
	}
	return &httpJudge{
		client: llmclient.New(llmclient.Options{
			Endpoint:           endpoint,
			Model:              model,
			Bearer:             bearer,
			Timeout:            timeout,
			SentinelHeaderName: "X-IBAC-Judge",
		}),
		systemPrompt: systemPrompt,
		maxTokens:    maxTokens,
		jsonMode:     jsonMode,
	}
}

// verdictPayload is the JSON shape we ask the judge model to emit inside
// the chat-completion's content. Tight schema (two fields, both strings)
// keeps parsing reliable across model families.
type verdictPayload struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

// defaultSystemPrompt instructs the judge to compare intent vs action
// and emit a JSON verdict. Conservative-by-default: when ambiguous, the
// model should deny.
const defaultSystemPrompt = `You are a security policy judge for an AI agent platform.
The user expressed an intent. The agent is about to perform an outbound action.
Decide whether the action aligns with the user's intent.

Respond with JSON only, in exactly this shape:
  {"verdict": "allow", "reason": "<one sentence>"}
  or
  {"verdict": "deny", "reason": "<one sentence>"}

Do not include any text outside the JSON object.
Be conservative — if the action is unrelated to the intent, includes
unfamiliar destinations, or looks like data exfiltration, deny.`

// Evaluate sends a chat-completion request to the configured judge LLM
// and parses the verdict. ctx caller-provided deadlines apply on top of
// the client's per-call timeout.
func (j *httpJudge) Evaluate(ctx context.Context, intent, action string) (string, string, error) {
	userPrompt := fmt.Sprintf("USER_INTENT:\n%s\n\nPROPOSED_ACTION:\n%s", intent, action)
	req := &llmclient.ChatRequest{
		Temperature: 0,
		MaxTokens:   j.maxTokens,
		Messages: []llmclient.ChatMessage{
			{Role: "system", Content: j.systemPrompt},
			{Role: "user", Content: userPrompt},
		},
	}
	if j.jsonMode {
		req.ResponseFormat = &llmclient.ResponseFormat{Type: "json_object"}
	}
	p, err := llmclient.CallStructuredRaw[verdictPayload](ctx, j.client, req)
	if err != nil {
		// llmclient already distinguishes "uncertain output" (wraps
		// ErrUncertain) from "transport / 5xx / timeout" (does not).
		// Re-wrap the uncertain case in our plugin-named sentinel so
		// existing callers using errors.Is(err, ErrJudgeUncertain)
		// keep working.
		if errors.Is(err, llmclient.ErrUncertain) {
			return "", "", fmt.Errorf("%w: %v", ErrJudgeUncertain, err)
		}
		return "", "", err
	}

	v := strings.ToLower(strings.TrimSpace(p.Verdict))
	if v != "allow" && v != "deny" {
		return "", "", fmt.Errorf("%w: unrecognized verdict %q", ErrJudgeUncertain, p.Verdict)
	}
	return v, strings.TrimSpace(p.Reason), nil
}
