package sparc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPReflector_DecodesVerdict(t *testing.T) {
	var gotPath, gotSentinel string
	var gotBody ReflectInput
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotSentinel = r.Header.Get(sentinelHeader)
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		score := 2.0
		_ = json.NewEncoder(w).Encode(ReflectVerdict{
			Decision:        DecisionReject,
			OverallAvgScore: &score,
			Issues: []ReflectIssue{
				{IssueType: "semantic_function", MetricName: "m", Explanation: "ungrounded id"},
			},
		})
	}))
	defer srv.Close()

	r := newHTTPReflector(srv.URL, "", time.Second)
	v, err := r.Reflect(context.Background(), ReflectInput{
		ToolCalls: []map[string]any{{"function": map[string]any{"name": "get_transaction"}}},
		SessionID: "s1",
	})
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if gotPath != "/reflect" {
		t.Errorf("path = %q, want /reflect", gotPath)
	}
	if gotSentinel != "1" {
		t.Errorf("sentinel header = %q, want 1", gotSentinel)
	}
	if gotBody.SessionID != "s1" {
		t.Errorf("forwarded session_id = %q, want s1", gotBody.SessionID)
	}
	if v.Decision != DecisionReject {
		t.Errorf("decision = %q, want reject", v.Decision)
	}
	if v.OverallAvgScore == nil || *v.OverallAvgScore != 2.0 {
		t.Errorf("score = %v, want 2.0", v.OverallAvgScore)
	}
	if len(v.Issues) != 1 || v.Issues[0].Explanation != "ungrounded id" {
		t.Errorf("issues = %+v", v.Issues)
	}
}

func TestHTTPReflector_Non2xxIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"boom"}`, http.StatusBadGateway)
	}))
	defer srv.Close()

	r := newHTTPReflector(srv.URL, "", time.Second)
	_, err := r.Reflect(context.Background(), ReflectInput{ToolCalls: []map[string]any{{}}})
	if !errors.Is(err, ErrReflectorUnavailable) {
		t.Fatalf("expected ErrReflectorUnavailable, got %v", err)
	}
}

func TestHTTPReflector_EmptyDecisionIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"issues":[]}`))
	}))
	defer srv.Close()

	r := newHTTPReflector(srv.URL, "", time.Second)
	_, err := r.Reflect(context.Background(), ReflectInput{ToolCalls: []map[string]any{{}}})
	if !errors.Is(err, ErrReflectorUnavailable) {
		t.Fatalf("expected ErrReflectorUnavailable for empty decision, got %v", err)
	}
}

func TestHTTPReflector_TransportErrorIsUnavailable(t *testing.T) {
	// Point at a closed port to force a dial failure.
	r := newHTTPReflector("http://127.0.0.1:1", "", 200*time.Millisecond)
	_, err := r.Reflect(context.Background(), ReflectInput{ToolCalls: []map[string]any{{}}})
	if !errors.Is(err, ErrReflectorUnavailable) {
		t.Fatalf("expected ErrReflectorUnavailable, got %v", err)
	}
}
