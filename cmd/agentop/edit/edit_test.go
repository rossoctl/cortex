package edit

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// cmStore wraps a kubectl stub in the store the picker would build, so these
// tests exercise FetchCmd through the same seam production uses.
func cmStore(stub Runner) ConfigMapStore {
	return ConfigMapStore{Run: stub, Namespace: "team1", Pod: "email-agent"}
}

func TestFetchCmd_Success(t *testing.T) {
	stub := func(ctx context.Context, args ...string) ([]byte, error) {
		return []byte(fixtureCMYAML), nil
	}
	cmd := FetchCmd(context.Background(), cmStore(stub), nil, nil)
	msg := cmd().(FetchedMsg)
	if msg.Err != nil {
		t.Fatalf("FetchedMsg.Err = %v", msg.Err)
	}
	if msg.Fetched == nil {
		t.Fatal("FetchedMsg.Fetched is nil")
	}
	if msg.TempPath == "" {
		t.Fatal("FetchedMsg.TempPath empty (should be the path to the subtree-only tempfile)")
	}
}

func TestFetchCmd_Error(t *testing.T) {
	stub := func(ctx context.Context, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("forbidden")
	}
	cmd := FetchCmd(context.Background(), cmStore(stub), nil, nil)
	msg := cmd().(FetchedMsg)
	if msg.Err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(msg.Err.Error(), "forbidden") {
		t.Fatalf("error: %v", msg.Err)
	}
}

func TestApplyCmd_Success(t *testing.T) {
	stub := func(ctx context.Context, args ...string) ([]byte, error) {
		return []byte("applied"), nil
	}
	cmd := ApplyCmd(context.Background(), cmStore(stub), "", nil, []byte("manifest"))
	msg := cmd().(AppliedMsg)
	if msg.Err != nil {
		t.Fatalf("err = %v", msg.Err)
	}
	if msg.ApplyTime.IsZero() {
		t.Fatal("ApplyTime not set")
	}
}

func TestPollCmd_Success(t *testing.T) {
	applyTime := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		ts := time.Now().Add(1 * time.Hour).Format(time.RFC3339Nano)
		_, _ = w.Write([]byte(`{"last_success":"` + ts + `"}`))
	}))
	defer srv.Close()
	cmd := PollCmd(context.Background(), srv.URL, applyTime, baselineFailedSentinel, Target{})
	msg := cmd().(PolledMsg)
	if msg.Result.Status != PollSuccess {
		t.Fatalf("status: %v", msg.Result.Status)
	}
}
