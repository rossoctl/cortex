package sparc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Reflector evaluates whether a proposed tool call is grounded in the
// conversation and tool specifications. The concrete implementation
// (httpReflector) calls the out-of-process SPARC reflection service; tests
// inject a fake.
//
// A returned error means the reflection could not be obtained (transport
// failure, timeout, non-2xx). It is distinct from a successful reflection
// whose Decision is "error" (SPARC ran but its own LLM/pipeline failed) —
// the plugin maps both to the configured fail policy, but keeps them
// distinguishable in session records.
type Reflector interface {
	Reflect(ctx context.Context, in ReflectInput) (ReflectVerdict, error)
}

// ReflectInput is SPARC's native input: the conversation, the available
// tools, and the proposed tool call(s) to vet.
type ReflectInput struct {
	Messages  []map[string]any `json:"messages"`
	ToolSpecs []map[string]any `json:"tool_specs"`
	ToolCalls []map[string]any `json:"tool_calls"`
	SessionID string           `json:"session_id,omitempty"`
	Track     string           `json:"track,omitempty"`
}

// ReflectIssue mirrors one SPARC issue (PyPI agent-lifecycle-toolkit 0.10.1
// shape: issue_type / metric_name / explanation / correction).
type ReflectIssue struct {
	IssueType   string `json:"issue_type"`
	MetricName  string `json:"metric_name"`
	Explanation string `json:"explanation"`
	Correction  any    `json:"correction"`
}

// ReflectVerdict is SPARC's decision plus its supporting issues and score.
type ReflectVerdict struct {
	Decision        string         `json:"decision"` // approve | reject | error
	Issues          []ReflectIssue `json:"issues"`
	OverallAvgScore *float64       `json:"overall_avg_score"` // 0=worst .. 1=best; nil when unavailable
	ExecutionTimeMs float64        `json:"execution_time_ms"`
}

// SPARC decision values.
const (
	DecisionApprove = "approve"
	DecisionReject  = "reject"
	DecisionError   = "error"
)

// ErrReflectorUnavailable is returned (wrapped) when the SPARC service could
// not be reached or returned a non-2xx status — an availability problem,
// distinct from a "reject" verdict.
var ErrReflectorUnavailable = errors.New("sparc reflector unavailable")

// sentinelHeader marks the plugin's own outbound HTTP call so that, if it
// ever loops back through the listener, OnRequest short-circuits instead of
// recursing (the same defense-in-depth pattern ibac's judge uses).
const sentinelHeader = "X-SPARC-Reflect"

// httpReflector POSTs a reflection request to {endpoint}/reflect.
//
// The call is made with a standalone http.Client that does NOT route back
// through the authbridge listener, structurally preventing reentrancy. The
// sentinel header is belt-and-suspenders.
type httpReflector struct {
	url     string
	bearer  string
	client  *http.Client
	timeout time.Duration
}

func newHTTPReflector(endpoint, bearer string, timeout time.Duration) *httpReflector {
	return &httpReflector{
		url:     endpoint + "/reflect",
		bearer:  bearer,
		client:  &http.Client{Timeout: timeout},
		timeout: timeout,
	}
}

func (r *httpReflector) Reflect(ctx context.Context, in ReflectInput) (ReflectVerdict, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return ReflectVerdict{}, fmt.Errorf("marshal reflect request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(body))
	if err != nil {
		return ReflectVerdict{}, fmt.Errorf("%w: %v", ErrReflectorUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(sentinelHeader, "1")
	if r.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+r.bearer)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return ReflectVerdict{}, fmt.Errorf("%w: %v", ErrReflectorUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ReflectVerdict{}, fmt.Errorf("%w: status %d: %s",
			ErrReflectorUnavailable, resp.StatusCode, preview(string(respBody), 240))
	}

	var v ReflectVerdict
	if err := json.Unmarshal(respBody, &v); err != nil {
		return ReflectVerdict{}, fmt.Errorf("%w: decode response: %v", ErrReflectorUnavailable, err)
	}
	if v.Decision == "" {
		return ReflectVerdict{}, fmt.Errorf("%w: empty decision in response", ErrReflectorUnavailable)
	}
	return v, nil
}
