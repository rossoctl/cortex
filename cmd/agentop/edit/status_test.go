package edit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// makeStatusServer returns a httptest.Server whose /reload/status
// endpoint reads its body from the supplied function — letting the
// test advance state between polls.
func makeStatusServer(t *testing.T, body func() ReloadStatus) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/reload/status" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body())
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPollUntilReloaded_Success(t *testing.T) {
	applyTime := time.Now()
	var calls atomic.Int32
	srv := makeStatusServer(t, func() ReloadStatus {
		c := calls.Add(1)
		if c == 1 {
			return ReloadStatus{LastSuccess: applyTime.Add(-1 * time.Hour)}
		}
		return ReloadStatus{LastSuccess: time.Now()}
	})
	res := PollUntilReloaded(context.Background(), srv.URL, applyTime, baselineFailedSentinel, "")
	if res.Status != PollSuccess {
		t.Fatalf("status = %v, want PollSuccess", res.Status)
	}
}

func TestPollUntilReloaded_Failure(t *testing.T) {
	applyTime := time.Now()
	var calls atomic.Int32
	srv := makeStatusServer(t, func() ReloadStatus {
		c := calls.Add(1)
		if c == 1 {
			return ReloadStatus{ReloadsFailed: 5}
		}
		return ReloadStatus{ReloadsFailed: 6, LastError: "invalid YAML at line 3"}
	})
	res := PollUntilReloaded(context.Background(), srv.URL, applyTime, baselineFailedSentinel, "")
	if res.Status != PollFailure {
		t.Fatalf("status = %v, want PollFailure", res.Status)
	}
	if res.LastError != "invalid YAML at line 3" {
		t.Fatalf("LastError: %q", res.LastError)
	}
}

func TestPollUntilReloaded_Timeout(t *testing.T) {
	applyTime := time.Now()
	srv := makeStatusServer(t, func() ReloadStatus {
		return ReloadStatus{LastSuccess: applyTime.Add(-1 * time.Hour)}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	res := PollUntilReloaded(ctx, srv.URL, applyTime, baselineFailedSentinel, "")
	if res.Status != PollTimeout {
		t.Fatalf("status = %v, want PollTimeout", res.Status)
	}
}

func TestPollUntilReloaded_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal", http.StatusInternalServerError)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	res := PollUntilReloaded(ctx, srv.URL, time.Now(), baselineFailedSentinel, "")
	if res.Status != PollTimeout {
		t.Fatalf("status = %v, want PollTimeout", res.Status)
	}
}

// A refusal and an endpoint that stopped answering are both PollFailure; only the
// second is Unreachable, which is what lets a caller say which happened.
func TestPollUntilReloaded_MarksAnEndpointThatStoppedAnsweringUnreachable(t *testing.T) {
	shortenPollSchedule(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			_ = json.NewEncoder(w).Encode(ReloadStatus{})
			return
		}
		http.Error(w, "gone", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	res := PollUntilReloaded(context.Background(), srv.URL, time.Now(), baselineFailedSentinel, "")
	if res.Status != PollFailure || !res.Unreachable || !strings.Contains(res.LastError, "unreachable") {
		t.Fatalf("result = %+v, want an Unreachable PollFailure", res)
	}

	var polls atomic.Int32
	refused := makeStatusServer(t, func() ReloadStatus {
		if polls.Add(1) == 1 {
			return ReloadStatus{ReloadsFailed: 0}
		}
		return ReloadStatus{ReloadsFailed: 1, LastError: "bad"}
	})
	if res := PollUntilReloaded(context.Background(), refused.URL, time.Now(), baselineFailedSentinel, ""); res.Status != PollFailure || res.Unreachable {
		t.Fatalf("refusal = %+v, want a PollFailure that is not Unreachable", res)
	}
}

// The TUI editor path: ApplyCmd reads the baseline before its write, so PollCmd
// reports a refusal that the first poll already counts. A failed baseline read
// (no proxy) keeps the old behaviour: the first poll sets the baseline.
func TestApplyCmd_ReadsTheBaselineBeforeTheWrite(t *testing.T) {
	path := writeFixture(t, 0o600)
	srv := makeStatusServer(t, func() ReloadStatus {
		if !strings.Contains(readFile(t, path), "pipeline: {}") {
			return ReloadStatus{ReloadsFailed: 4}
		}
		return ReloadStatus{ReloadsFailed: 5, LastError: "refused before the first poll"}
	})
	applied := ApplyCmd(context.Background(), FileStore{Path: path}, srv.URL, nil, []byte("pipeline: {}\n"))().(AppliedMsg)
	if applied.Err != nil || applied.BaselineFailed != 4 {
		t.Fatalf("applied = %+v, want BaselineFailed 4 from before the write", applied)
	}
	polled := PollCmd(context.Background(), srv.URL, applied.ApplyTime, applied.BaselineFailed,
		Target{PollDeadline: 2 * time.Second})().(PolledMsg)
	if polled.Result.Status != PollFailure || polled.Result.LastError != "refused before the first poll" {
		t.Errorf("poll = %+v, want the refusal", polled.Result)
	}

	if got := ReloadBaseline(context.Background(), ""); got != baselineFailedSentinel {
		t.Errorf("ReloadBaseline with no status URL = %d, want the sentinel", got)
	}
}

// A proxy that restarts after the baseline read starts its count again at 0, below
// the baseline. The poll then lowers the baseline to that count, so a refusal on
// the restarted proxy is still seen, as with a baseline read at the first poll.
func TestPollUntilReloaded_SeesARefusalAfterTheProxyRestarted(t *testing.T) {
	var calls atomic.Int32
	srv := makeStatusServer(t, func() ReloadStatus {
		if calls.Add(1) == 1 {
			return ReloadStatus{ReloadsFailed: 0}
		}
		return ReloadStatus{ReloadsFailed: 1, LastError: "refused after a restart"}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res := PollUntilReloaded(ctx, srv.URL, time.Now(), 3, "")
	if res.Status != PollFailure || res.LastError != "refused after a restart" {
		t.Fatalf("result = %+v, want the refusal", res)
	}
}
