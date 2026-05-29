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

func TestFetchCmd_Success(t *testing.T) {
	stub := func(ctx context.Context, args ...string) ([]byte, error) {
		return []byte(fixtureCMYAML), nil
	}
	cmd := FetchCmd(context.Background(), stub, "team1", "email-agent")
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
	cmd := FetchCmd(context.Background(), stub, "team1", "email-agent")
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
	cmd := ApplyCmd(context.Background(), stub, []byte("manifest"))
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
		_, _ = w.Write([]byte(`{"last_success_unix":99999999999}`))
	}))
	defer srv.Close()
	cmd := PollCmd(context.Background(), srv.URL, applyTime)
	msg := cmd().(PolledMsg)
	if msg.Result.Status != PollSuccess {
		t.Fatalf("status: %v", msg.Result.Status)
	}
}
