package sessionapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// fetchUsage GETs the endpoint and returns status plus raw body.
func fetchUsage(t *testing.T, base, query string) (int, string) {
	t.Helper()
	resp, err := http.Get(base + "/v1/usage" + query)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(body)
}

// Without WithUsage the endpoint must 404, not serve an empty snapshot: zeroed
// buckets would render as a flat chart implying idle traffic, hiding the fact
// that aggregation is not running at all.
func TestHandleUsage_NoAggregator404s(t *testing.T) {
	ts, _ := newTestServer(t)
	status, body := fetchUsage(t, ts.URL, "")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
	if !strings.Contains(body, "not enabled") {
		t.Errorf("body should say why: %q", body)
	}
}

func TestHandleUsage_Defaults(t *testing.T) {
	agg := usage.New()
	ts, _ := newTestServer(t, WithUsage(agg))

	status, body := fetchUsage(t, ts.URL, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var snap usage.Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(snap.Buckets) != 10 {
		t.Errorf("default window should be 10 buckets, got %d", len(snap.Buckets))
	}
	if snap.BucketSeconds != 60 {
		t.Errorf("bucketSeconds = %d, want 60", snap.BucketSeconds)
	}
	if snap.Priced {
		t.Error("priced = true when no request carried a cost event")
	}
}

// Query parameters must actually reach Snapshot — a handler that parsed them and
// then ignored them would pass every validation test while serving the default
// view.
func TestHandleUsage_ParamsReachSnapshot(t *testing.T) {
	agg := usage.New()
	ts, store := newTestServer(t, WithUsage(agg))
	store.AddRecorder(agg)

	store.Append("alice", pipeline.SessionEvent{
		At:         time.Now(),
		Direction:  pipeline.Outbound,
		Phase:      pipeline.SessionResponse,
		StatusCode: 200,
		Duration:   time.Second,
		Host:       "api.anthropic.com",
		Inference:  &pipeline.InferenceExtension{Model: "claude-sonnet-5", TotalTokens: 300},
	})

	// resolution: 1h at 5m must fold to 12 buckets, not 60.
	status, body := fetchUsage(t, ts.URL, "?window=1h&resolution=5m")
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var snap usage.Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(snap.Buckets) != 12 || snap.BucketSeconds != 300 {
		t.Errorf("1h@5m gave %d buckets at %ds, want 12 at 300s",
			len(snap.Buckets), snap.BucketSeconds)
	}

	// group: the series must be keyed by the requested dimension.
	_, body = fetchUsage(t, ts.URL, "?group=method")
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Group != usage.GroupMethod {
		t.Errorf("group = %q, want method", snap.Group)
	}
	found := false
	for _, b := range snap.Buckets {
		if _, ok := b.Series["claude-sonnet-5"]; ok {
			found = true
		}
	}
	if !found {
		t.Error("group=method did not key the series by model")
	}

	// group=host: the same request, keyed by the upstream it went to. Asserted
	// through the real handler because the bucket's series lookup returns nil for a
	// group it does not know, and abctl falls back to ungrouped bars on an empty
	// series set — so a missing arm looks like "this host had no traffic" rather
	// than like an error.
	_, body = fetchUsage(t, ts.URL, "?group=host")
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Group != usage.GroupHost {
		t.Errorf("group = %q, want host", snap.Group)
	}
	found = false
	for _, b := range snap.Buckets {
		if _, ok := b.Series["api.anthropic.com"]; ok {
			found = true
		}
	}
	if !found {
		t.Error("group=host did not key the series by host")
	}

	// session: scoping must filter, and echo back which session it covers.
	_, body = fetchUsage(t, ts.URL, "?session=alice")
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Session != "alice" {
		t.Errorf("session = %q, want alice", snap.Session)
	}
	if snap.Totals.Tokens != 300 {
		t.Errorf("alice tokens = %d, want 300", snap.Totals.Tokens)
	}

	// Fresh value: Counts fields are omitempty, so an all-zero Totals is absent
	// from the JSON entirely. Decoding into the struct reused above would leave
	// alice's 300 in place and the assertion would pass for the wrong reason.
	var bobSnap usage.Snapshot
	_, body = fetchUsage(t, ts.URL, "?session=bob")
	if err := json.Unmarshal([]byte(body), &bobSnap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if bobSnap.Totals.Tokens != 0 {
		t.Errorf("bob should have no traffic, got %d tokens", bobSnap.Totals.Tokens)
	}
}

func TestHandleUsage_BadParams400(t *testing.T) {
	ts, _ := newTestServer(t, WithUsage(usage.New()))

	for _, q := range []string{
		"?window=30s",               // finer than a bucket
		"?window=7h",                // beyond retention
		"?window=90s",               // not a multiple
		"?window=nonsense",          // unparseable
		"?group=bogus",              // unknown grouping
		"?window=1h&resolution=90s", // resolution not a multiple
		"?window=1h&resolution=2h",  // resolution wider than the window
		"?resolution=30s",           // finer than storage
	} {
		status, body := fetchUsage(t, ts.URL, q)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body %q)", q, status, body)
		}
		if !strings.Contains(body, `"error"`) {
			t.Errorf("%s: body is not a JSON error: %q", q, body)
		}
	}
}

// The endpoint is unauthenticated, so an error body must never reflect
// caller-supplied bytes back — that is a reflection primitive.
func TestHandleUsage_ErrorsDoNotEchoInput(t *testing.T) {
	ts, _ := newTestServer(t, WithUsage(usage.New()))
	const probe = "<script>alert(1)</script>"

	for _, q := range []string{"?group=" + probe, "?window=" + probe, "?resolution=" + probe} {
		status, body := fetchUsage(t, ts.URL, q)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, status)
		}
		if strings.Contains(body, "script") {
			t.Errorf("error body echoes caller input: %q", body)
		}
	}
}

// An over-long session id is rejected rather than truncated or echoed. Bounded
// at the store's own cap so the two cannot drift.
func TestHandleUsage_SessionIDLengthCap(t *testing.T) {
	ts, _ := newTestServer(t, WithUsage(usage.New()))

	ok := strings.Repeat("a", session.MaxSessionIDLen)
	if status, body := fetchUsage(t, ts.URL, "?session="+ok); status != http.StatusOK {
		t.Errorf("id at exactly the cap should be accepted, got %d: %s", status, body)
	}

	tooLong := strings.Repeat("a", session.MaxSessionIDLen+1)
	status, body := fetchUsage(t, ts.URL, "?session="+tooLong)
	if status != http.StatusBadRequest {
		t.Errorf("over-long id: status = %d, want 400", status)
	}
	if strings.Contains(body, tooLong) {
		t.Error("error body echoes the over-long id back")
	}
}
