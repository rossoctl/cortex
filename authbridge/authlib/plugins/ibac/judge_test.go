package ibac

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/rossocortex/authbridge/authlib/llmclient"
)

// These tests cover the IBAC-specific layer on top of llmclient:
// prompt formatting, verdict normalization (allow/deny only), and
// error categorization (ErrJudgeUncertain wrapping). HTTP-level
// behavior — bearer forwarding, timeouts, 5xx handling, JSON
// extraction — lives in authlib/llmclient/client_test.go and is
// not re-tested here.

// Successful judge call: IBAC's Evaluate must forward the right
// prompt shape and return the parsed verdict string.
func TestHTTPJudge_AllowVerdict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reentrancy sentinel must travel with every judge call —
		// this is IBAC's defense-in-depth against loop-backs through
		// the plugin's own OnRequest.
		if got := r.Header.Get("X-IBAC-Judge"); got != "1" {
			t.Errorf("X-IBAC-Judge = %q, want 1", got)
		}

		var req llmclient.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Model != "test-model" {
			t.Errorf("model = %q, want test-model", req.Model)
		}
		if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
			t.Errorf("unexpected messages: %+v", req.Messages)
		}
		// The user message must include both the intent and the
		// action — the judge can't decide alignment with only one.
		if !strings.Contains(req.Messages[1].Content, "summarize emails") ||
			!strings.Contains(req.Messages[1].Content, "POST evil-server") {
			t.Errorf("user message missing intent or action: %q", req.Messages[1].Content)
		}

		_ = json.NewEncoder(w).Encode(llmclient.ChatResponse{
			Choices: []llmclient.ChatChoice{
				{Message: llmclient.ChatMessage{Role: "assistant", Content: `{"verdict":"allow","reason":"GET to known service"}`}},
			},
		})
	}))
	defer srv.Close()

	j := newHTTPJudge(srv.URL, "test-model", "", "", time.Second, 1024, false)
	verdict, reason, err := j.Evaluate(context.Background(), "summarize emails", "POST evil-server")
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if verdict != "allow" {
		t.Errorf("verdict = %q, want allow", verdict)
	}
	if reason != "GET to known service" {
		t.Errorf("reason = %q, want 'GET to known service'", reason)
	}
}

// Models often wrap JSON in code fences or prose. The judge must
// still extract the verdict; this also smokes that CallStructured
// → ExtractJSON is wired up correctly.
func TestHTTPJudge_VerdictWrappedInProse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(llmclient.ChatResponse{
			Choices: []llmclient.ChatChoice{
				{Message: llmclient.ChatMessage{
					Role: "assistant",
					Content: "Sure, here is my judgment:\n```json\n" +
						`{"verdict": "deny", "reason": "POST to unknown server"}` +
						"\n```",
				}},
			},
		})
	}))
	defer srv.Close()

	j := newHTTPJudge(srv.URL, "m", "", "", time.Second, 1024, false)
	verdict, reason, err := j.Evaluate(context.Background(), "i", "a")
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if verdict != "deny" || reason != "POST to unknown server" {
		t.Errorf("verdict=%q reason=%q, want deny / POST to unknown server", verdict, reason)
	}
}

// IBAC-specific: verdicts other than "allow"/"deny" must fail
// closed even if the JSON itself is well-formed. This is the
// normalization layer on top of llmclient's parser.
func TestHTTPJudge_UnrecognizedVerdict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(llmclient.ChatResponse{
			Choices: []llmclient.ChatChoice{
				{Message: llmclient.ChatMessage{Content: `{"verdict":"maybe","reason":"huh"}`}},
			},
		})
	}))
	defer srv.Close()

	j := newHTTPJudge(srv.URL, "m", "", "", time.Second, 1024, false)
	_, _, err := j.Evaluate(context.Background(), "i", "a")
	if err == nil {
		t.Fatal("Evaluate returned nil error on unrecognized verdict; expected fail-closed signal")
	}
	if !errors.Is(err, ErrJudgeUncertain) {
		t.Errorf("expected error wrapping ErrJudgeUncertain, got %v", err)
	}
}

// Garbage in the model's content must surface as ErrJudgeUncertain
// — not just any error. This confirms IBAC re-wraps llmclient's
// ErrUncertain into its plugin-named sentinel.
func TestHTTPJudge_NoJSONWrapsErrJudgeUncertain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(llmclient.ChatResponse{
			Choices: []llmclient.ChatChoice{{Message: llmclient.ChatMessage{Content: "I don't know."}}},
		})
	}))
	defer srv.Close()

	j := newHTTPJudge(srv.URL, "m", "", "", time.Second, 1024, false)
	_, _, err := j.Evaluate(context.Background(), "i", "a")
	if err == nil {
		t.Fatal("Evaluate returned nil error on no-JSON content")
	}
	if !errors.Is(err, ErrJudgeUncertain) {
		t.Errorf("error must wrap ErrJudgeUncertain so plugin maps it to 403; err=%v", err)
	}
	// Generic errors.Is on llmclient.ErrUncertain must also match —
	// the plugin sentinel chains through.
	if !errors.Is(err, llmclient.ErrUncertain) {
		t.Errorf("error should also satisfy errors.Is(_, llmclient.ErrUncertain); err=%v", err)
	}
}

// Transport / 5xx errors must NOT wrap ErrJudgeUncertain — those
// are availability problems and the plugin maps them to 503,
// not 403.
func TestHTTPJudge_HTTPErrorIsNotJudgeUncertain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "judge overloaded", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	j := newHTTPJudge(srv.URL, "m", "", "", time.Second, 1024, false)
	_, _, err := j.Evaluate(context.Background(), "i", "a")
	if err == nil {
		t.Fatal("Evaluate returned nil on HTTP 503; expected fail-soft signal")
	}
	if errors.Is(err, ErrJudgeUncertain) {
		t.Errorf("HTTP-error wrongly wraps ErrJudgeUncertain; "+
			"plugin would map this to 403 instead of 503. err=%v", err)
	}
}

// Default system prompt is used when the operator doesn't override.
// Empty systemPrompt -> defaultSystemPrompt.
func TestHTTPJudge_DefaultSystemPromptUsedWhenEmpty(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req llmclient.ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 0 {
			got = req.Messages[0].Content
		}
		_ = json.NewEncoder(w).Encode(llmclient.ChatResponse{
			Choices: []llmclient.ChatChoice{{Message: llmclient.ChatMessage{Content: `{"verdict":"allow","reason":"x"}`}}},
		})
	}))
	defer srv.Close()

	j := newHTTPJudge(srv.URL, "m", "", "", time.Second, 1024, false)
	if _, _, err := j.Evaluate(context.Background(), "i", "a"); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got != defaultSystemPrompt {
		t.Errorf("system prompt = %q, want defaultSystemPrompt", got)
	}
}

// Operator-supplied system prompt overrides the default.
func TestHTTPJudge_OverrideSystemPrompt(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req llmclient.ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 0 {
			got = req.Messages[0].Content
		}
		_ = json.NewEncoder(w).Encode(llmclient.ChatResponse{
			Choices: []llmclient.ChatChoice{{Message: llmclient.ChatMessage{Content: `{"verdict":"deny","reason":"x"}`}}},
		})
	}))
	defer srv.Close()

	custom := "You are a finance compliance reviewer."
	j := newHTTPJudge(srv.URL, "m", "", custom, time.Second, 1024, false)
	if _, _, err := j.Evaluate(context.Background(), "i", "a"); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got != custom {
		t.Errorf("system prompt = %q, want operator override %q", got, custom)
	}
}

// MaxTokens and ResponseFormat must reach the wire request — the
// 200-token-truncation bug that motivated this plumbing only
// reproduces when the judge's per-call cap actually flows through.
func TestHTTPJudge_MaxTokensAndJSONModeOnWire(t *testing.T) {
	var got llmclient.ChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(llmclient.ChatResponse{
			Choices: []llmclient.ChatChoice{{Message: llmclient.ChatMessage{Content: `{"verdict":"allow","reason":"x"}`}}},
		})
	}))
	defer srv.Close()

	j := newHTTPJudge(srv.URL, "m", "", "", time.Second, 4096, true)
	if _, _, err := j.Evaluate(context.Background(), "i", "a"); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got.MaxTokens != 4096 {
		t.Errorf("max_tokens on wire = %d, want 4096", got.MaxTokens)
	}
	if got.ResponseFormat == nil || got.ResponseFormat.Type != "json_object" {
		t.Errorf("response_format = %+v, want type=json_object", got.ResponseFormat)
	}

	// jsonMode=false must omit response_format entirely so endpoints
	// that reject unknown fields keep working.
	got = llmclient.ChatRequest{}
	j2 := newHTTPJudge(srv.URL, "m", "", "", time.Second, 256, false)
	if _, _, err := j2.Evaluate(context.Background(), "i", "a"); err != nil {
		t.Fatalf("Evaluate (jsonMode=false): %v", err)
	}
	if got.ResponseFormat != nil {
		t.Errorf("response_format = %+v, want nil when jsonMode=false", got.ResponseFormat)
	}
	if got.MaxTokens != 256 {
		t.Errorf("max_tokens on wire = %d, want 256", got.MaxTokens)
	}
}
