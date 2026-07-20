package llmclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/llmclient"
)

// Smallest possible response shape — the server always returns
// 200 with a Choices[0].Message.Content set to whatever the test
// hands it.
func newServer(t *testing.T, content string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(llmclient.ChatResponse{
			Choices: []llmclient.ChatChoice{
				{Message: llmclient.ChatMessage{Role: "assistant", Content: content}},
			},
		})
	}))
}

// Happy path: Call returns the assistant content verbatim.
func TestCall_Success(t *testing.T) {
	srv := newServer(t, "hello world")
	defer srv.Close()

	c := llmclient.New(llmclient.Options{Endpoint: srv.URL, Model: "m", Timeout: time.Second})
	got, err := c.Call(context.Background(), "sys", "usr")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got != "hello world" {
		t.Errorf("Call = %q, want %q", got, "hello world")
	}
}

// The wire body and headers must match the OpenAI shape so any
// real chat-completions endpoint accepts what we send.
func TestCall_RequestShape(t *testing.T) {
	var (
		gotCT     string
		gotAuth   string
		gotSent   string
		gotReq    llmclient.ChatRequest
		decodeErr error
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		gotSent = r.Header.Get("X-Plugin-Sentinel")
		decodeErr = json.NewDecoder(r.Body).Decode(&gotReq)
		_ = json.NewEncoder(w).Encode(llmclient.ChatResponse{
			Choices: []llmclient.ChatChoice{{Message: llmclient.ChatMessage{Content: "ok"}}},
		})
	}))
	defer srv.Close()

	c := llmclient.New(llmclient.Options{
		Endpoint:           srv.URL,
		Model:              "test-model",
		Bearer:             "secret-token",
		SentinelHeaderName: "X-Plugin-Sentinel",
		Timeout:            time.Second,
	})
	if _, err := c.Call(context.Background(), "system text", "user text"); err != nil {
		t.Fatalf("Call: %v", err)
	}

	if decodeErr != nil {
		t.Fatalf("server failed to decode body: %v", decodeErr)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("Authorization = %q, want 'Bearer secret-token'", gotAuth)
	}
	if gotSent != "1" {
		t.Errorf("X-Plugin-Sentinel = %q, want 1 (reentrancy sentinel must be set)", gotSent)
	}
	if gotReq.Model != "test-model" {
		t.Errorf("model = %q, want test-model", gotReq.Model)
	}
	if len(gotReq.Messages) != 2 ||
		gotReq.Messages[0].Role != "system" || gotReq.Messages[0].Content != "system text" ||
		gotReq.Messages[1].Role != "user" || gotReq.Messages[1].Content != "user text" {
		t.Errorf("unexpected messages: %+v", gotReq.Messages)
	}
}

// No SentinelHeaderName configured → the helper does not set any
// reentrancy header (header value is empty string).
func TestCall_NoSentinelHeaderWhenUnset(t *testing.T) {
	var anySentinelSet bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Probe a couple of header names that look like reentrancy
		// markers; none should be present.
		if r.Header.Get("X-IBAC-Judge") != "" || r.Header.Get("X-LLMClient-Reentrancy") != "" {
			anySentinelSet = true
		}
		_ = json.NewEncoder(w).Encode(llmclient.ChatResponse{
			Choices: []llmclient.ChatChoice{{Message: llmclient.ChatMessage{Content: "ok"}}},
		})
	}))
	defer srv.Close()

	c := llmclient.New(llmclient.Options{Endpoint: srv.URL, Model: "m", Timeout: time.Second})
	if _, err := c.Call(context.Background(), "s", "u"); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if anySentinelSet {
		t.Errorf("expected no sentinel header when SentinelHeaderName is empty")
	}
}

// 5xx must surface as an error that does NOT wrap ErrUncertain.
// Plugins use this to distinguish "judge unavailable / 503" from
// "judge produced bad output / 403 fail-closed".
func TestCall_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream busy", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := llmclient.New(llmclient.Options{Endpoint: srv.URL, Model: "m", Timeout: time.Second})
	_, err := c.Call(context.Background(), "s", "u")
	if err == nil {
		t.Fatalf("Call: expected error on HTTP 503, got nil")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("err = %q, want it to mention HTTP 503", err)
	}
	if errors.Is(err, llmclient.ErrUncertain) {
		t.Errorf("HTTP-error wrongly wrapped ErrUncertain; "+
			"plugins would map this to 403 instead of 503. err=%v", err)
	}
}

// Connection-refused (no server listening) must also return an
// error that is NOT ErrUncertain.
func TestCall_NetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := llmclient.New(llmclient.Options{Endpoint: url, Model: "m", Timeout: 200 * time.Millisecond})
	_, err := c.Call(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("Call: expected error on connection refused, got nil")
	}
	if errors.Is(err, llmclient.ErrUncertain) {
		t.Errorf("network-error wrongly wrapped ErrUncertain; err=%v", err)
	}
}

// Per-call Timeout must actually be enforced.
func TestCall_TimeoutEnforced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	c := llmclient.New(llmclient.Options{Endpoint: srv.URL, Model: "m", Timeout: 200 * time.Millisecond})
	start := time.Now()
	_, err := c.Call(context.Background(), "s", "u")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Call: expected timeout error, got nil")
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("Call took %v, expected the 200ms timeout to trip well before 1.5s", elapsed)
	}
	if errors.Is(err, llmclient.ErrUncertain) {
		t.Errorf("timeout error wrongly wrapped ErrUncertain; err=%v", err)
	}
}

// HTTP 200 with no choices — the LLM was reachable but replied with
// nothing actionable. That's an "uncertain output" case.
func TestCall_EmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(llmclient.ChatResponse{Choices: nil})
	}))
	defer srv.Close()

	c := llmclient.New(llmclient.Options{Endpoint: srv.URL, Model: "m", Timeout: time.Second})
	_, err := c.Call(context.Background(), "s", "u")
	if !errors.Is(err, llmclient.ErrUncertain) {
		t.Errorf("expected error wrapping ErrUncertain on empty Choices, got %v", err)
	}
}

// Pathological response (200KB content) must not panic / OOM —
// 64KB LimitReader on the decode caps it. We expect an error
// (decoder hits EOF mid-string) rather than a hang or crash.
func TestCall_LargeResponseDoesNotCrash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"`+
			strings.Repeat("x", 200000)+`"}}]}`)
	}))
	defer srv.Close()

	c := llmclient.New(llmclient.Options{Endpoint: srv.URL, Model: "m", Timeout: 2 * time.Second})
	_, err := c.Call(context.Background(), "s", "u")
	if err == nil {
		t.Errorf("expected error from oversized response, got nil")
	}
}

// CallRaw without an Endpoint configured fails before any HTTP work.
func TestCallRaw_MissingEndpoint(t *testing.T) {
	c := llmclient.New(llmclient.Options{Model: "m"})
	_, err := c.CallRaw(context.Background(), &llmclient.ChatRequest{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "endpoint") {
		t.Errorf("expected endpoint-missing error, got %v", err)
	}
}

// CallRaw without a Model anywhere fails before any HTTP work.
func TestCallRaw_MissingModel(t *testing.T) {
	c := llmclient.New(llmclient.Options{Endpoint: "http://example"})
	_, err := c.CallRaw(context.Background(), &llmclient.ChatRequest{})
	if err == nil || !strings.Contains(err.Error(), "model") {
		t.Errorf("expected model-missing error, got %v", err)
	}
}

// CallRaw must not mutate the caller's *ChatRequest. A plugin
// reusing a static prompt template across calls would otherwise
// see the first call's resolved Model leak into subsequent calls.
func TestCallRaw_DoesNotMutateCallersRequest(t *testing.T) {
	srv := newServer(t, "ok")
	defer srv.Close()

	c := llmclient.New(llmclient.Options{Endpoint: srv.URL, Model: "client-default", Timeout: time.Second})
	req := &llmclient.ChatRequest{
		// Model deliberately empty: CallRaw fills it in for the
		// wire request, but the caller's struct must stay empty.
		Messages: []llmclient.ChatMessage{{Role: "user", Content: "x"}},
	}
	if _, err := c.CallRaw(context.Background(), req); err != nil {
		t.Fatalf("CallRaw: %v", err)
	}
	if req.Model != "" {
		t.Errorf("CallRaw mutated caller's req.Model = %q, want empty", req.Model)
	}
}

// CallRaw uses the request's Model when set, even if the Client's
// configured Model is different.
func TestCallRaw_RequestModelOverridesClientModel(t *testing.T) {
	var (
		gotModel  string
		decodeErr error
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req llmclient.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			decodeErr = err
			http.Error(w, "decode failed", http.StatusBadRequest)
			return
		}
		gotModel = req.Model
		_ = json.NewEncoder(w).Encode(llmclient.ChatResponse{
			Choices: []llmclient.ChatChoice{{Message: llmclient.ChatMessage{Content: "ok"}}},
		})
	}))
	defer srv.Close()

	c := llmclient.New(llmclient.Options{Endpoint: srv.URL, Model: "client-default", Timeout: time.Second})
	_, err := c.CallRaw(context.Background(), &llmclient.ChatRequest{
		Model:    "request-override",
		Messages: []llmclient.ChatMessage{{Role: "user", Content: "x"}},
	})
	if err != nil {
		t.Fatalf("CallRaw: %v", err)
	}
	if decodeErr != nil {
		t.Fatalf("server failed to decode request body: %v", decodeErr)
	}
	if gotModel != "request-override" {
		t.Errorf("model = %q, want 'request-override'", gotModel)
	}
}

// ExtractJSON unit tests — pure parsing, no HTTP server needed.

type sampleVerdict struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

func TestExtractJSON_Variants(t *testing.T) {
	cases := []struct {
		name        string
		content     string
		wantVerdict string
		wantErr     bool
	}{
		{"plain JSON", `{"verdict":"allow","reason":"ok"}`, "allow", false},
		{"prose prefix", `Sure: {"verdict":"deny","reason":"no"}`, "deny", false},
		{"code fence", "```json\n{\"verdict\":\"allow\",\"reason\":\"x\"}\n```", "allow", false},
		{"prose around fence", "Here you go:\n```\n{\"verdict\":\"deny\",\"reason\":\"x\"}\n```\nThanks", "deny", false},
		{"empty", "", "", true},
		{"no JSON", "I don't know", "", true},
		{"malformed", `{"verdict":"allow"`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := llmclient.ExtractJSON[sampleVerdict](tc.content)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr && got.Verdict != tc.wantVerdict {
				t.Errorf("verdict = %q, want %q", got.Verdict, tc.wantVerdict)
			}
		})
	}
}

func TestExtractJSON_FailureWrapsErrUncertain(t *testing.T) {
	_, err := llmclient.ExtractJSON[sampleVerdict]("not JSON")
	if !errors.Is(err, llmclient.ErrUncertain) {
		t.Errorf("ExtractJSON failure should wrap ErrUncertain, got %v", err)
	}
}

// CallStructured — end-to-end via httptest. Success path returns
// the parsed value; transport errors propagate; parse errors wrap
// ErrUncertain.

func TestCallStructured_Success(t *testing.T) {
	srv := newServer(t, `{"verdict":"allow","reason":"aligned"}`)
	defer srv.Close()

	c := llmclient.New(llmclient.Options{Endpoint: srv.URL, Model: "m", Timeout: time.Second})
	got, err := llmclient.CallStructured[sampleVerdict](context.Background(), c, "sys", "usr")
	if err != nil {
		t.Fatalf("CallStructured: %v", err)
	}
	if got.Verdict != "allow" || got.Reason != "aligned" {
		t.Errorf("got = %+v, want {allow, aligned}", got)
	}
}

func TestCallStructured_ParseErrorWrapsErrUncertain(t *testing.T) {
	srv := newServer(t, "I'm not sure.")
	defer srv.Close()

	c := llmclient.New(llmclient.Options{Endpoint: srv.URL, Model: "m", Timeout: time.Second})
	_, err := llmclient.CallStructured[sampleVerdict](context.Background(), c, "s", "u")
	if !errors.Is(err, llmclient.ErrUncertain) {
		t.Errorf("expected ErrUncertain, got %v", err)
	}
}

func TestCallStructured_TransportErrorIsNotUncertain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := llmclient.New(llmclient.Options{Endpoint: srv.URL, Model: "m", Timeout: time.Second})
	_, err := llmclient.CallStructured[sampleVerdict](context.Background(), c, "s", "u")
	if err == nil {
		t.Fatal("expected error on HTTP 503, got nil")
	}
	if errors.Is(err, llmclient.ErrUncertain) {
		t.Errorf("HTTP-error must not wrap ErrUncertain (would route to 403); err=%v", err)
	}
}
