package bodyread

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
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

// LogError must not panic on the shapes it sees in production: a chunked
// upload (ContentLength -1, so the field is omitted) and a nil-bodied request.
func TestLogErrorHandlesChunkedAndEmpty(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.ContentLength = -1
	LogError("forward-proxy", r, 0, 1<<20, &http.MaxBytesError{Limit: 1 << 20})
	LogError("forward-proxy", r, 17, 1<<20, errors.New("connection reset"))
}
