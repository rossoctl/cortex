package bodyread

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// An over-limit body must be reported as such, and answered with 413 so the
// caller learns the remedy is a smaller body (or a larger limit).
func TestOverLimit(t *testing.T) {
	err := &http.MaxBytesError{Limit: 1 << 20}
	status, body := Rejection(err)
	if status != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", status)
	}
	if body == "" {
		t.Error("empty rejection body")
	}
}

// A wrapped MaxBytesError must still be recognized: net/http returns it
// wrapped in some paths, and a missed unwrap silently downgrades a real
// overrun to a generic 400.
func TestOverLimitWrapped(t *testing.T) {
	err := errors.Join(errors.New("read tcp: connection reset"),
		&http.MaxBytesError{Limit: 1 << 20})
	if status, _ := Rejection(err); status != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d for a wrapped MaxBytesError, want 413", status)
	}
}

// Anything that is not an overrun must be a 400, not a 413. Reporting a
// dropped connection as an oversized payload sends the agent chasing the
// wrong remedy — the reason this package exists.
func TestNonOverLimitIsNot413(t *testing.T) {
	for _, err := range []error{
		errors.New("unexpected EOF"),
		errors.New("context canceled"),
	} {
		if status, _ := Rejection(err); status != http.StatusBadRequest {
			t.Errorf("status = %d for %v, want 400", status, err)
		}
	}
}

// Failure classifies the row a listener records. An overrun names the announced
// size and the limit when Content-Length is known, and only the limit when the
// upload was chunked; a read error under a canceled context is the client
// leaving; anything else is the body itself.
func TestFailure(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	overrun := &http.MaxBytesError{Limit: 1 << 20}

	sized := httptest.NewRequest("POST", "/v1/messages", nil)
	sized.ContentLength = 2 << 20
	chunked := httptest.NewRequest("POST", "/v1/messages", nil)
	chunked.ContentLength = -1
	gone := chunked.WithContext(canceled)

	tests := []struct {
		name    string
		r       *http.Request
		err     error
		status  int
		kind    string
		message string
	}{
		{"overrun with Content-Length", sized, overrun, http.StatusRequestEntityTooLarge, "proxy_error",
			"request body of 2097152 bytes is over the 1048576-byte buffer limit"},
		{"overrun, chunked", chunked, overrun, http.StatusRequestEntityTooLarge, "proxy_error",
			"request body over the 1048576-byte buffer limit"},
		// An overrun wins over a canceled context: the limit is what stopped the read.
		{"overrun, client gone", gone, overrun, http.StatusRequestEntityTooLarge, "proxy_error",
			"request body over the 1048576-byte buffer limit"},
		{"client gone", gone, io.ErrUnexpectedEOF, pipeline.StatusClientClosedRequest, "client_canceled", "unexpected EOF"},
		{"unreadable", chunked, errors.New("invalid byte in chunk length"), http.StatusBadRequest, "request_unreadable",
			"invalid byte in chunk length"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, fail := Failure(tc.r, 1<<20, tc.err)
			if status != tc.status {
				t.Errorf("status = %d, want %d", status, tc.status)
			}
			if fail == nil {
				t.Fatal("nil EventError")
			}
			if fail.Kind != tc.kind || fail.Message != tc.message {
				t.Errorf("got %s %q, want %s %q", fail.Kind, fail.Message, tc.kind, tc.message)
			}
		})
	}
}

// LogError must not panic on the shapes it sees in production: a chunked
// upload (ContentLength -1, so the field is omitted) and a nil-bodied request.
func TestLogErrorHandlesChunkedAndEmpty(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.ContentLength = -1
	LogError("forward-proxy", r, 0, 1<<20, &http.MaxBytesError{Limit: 1 << 20})
	LogError("forward-proxy", r, 17, 1<<20, errors.New("connection reset"))
}
