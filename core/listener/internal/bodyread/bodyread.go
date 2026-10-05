// Package bodyread reports why buffering a request body failed.
//
// It exists because a single "request body too large or unreadable" warning
// collapsed two failure classes with different remedies — raise the limit, or
// look at the client — and named neither the true body size nor the cause. It
// also answered every failure with 413, telling an agent to shrink a body when
// the real problem was a dropped connection.
package bodyread

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/rossoctl/cortex/core/pipeline"
)

// LogError logs a failed body-buffering attempt. listener names the caller
// ("forward-proxy") so the message matches the surrounding lines.
//
// contentLength is the only value that says how large an over-limit body
// actually was: MaxBytesReader stops at the limit, so read is capped there and
// the rest is never seen. It is omitted when the client sent no Content-Length
// (chunked), where the true size is unknowable here.
func LogError(listener string, r *http.Request, read int, limit int64, err error) {
	attrs := []any{
		"host", r.Host,
		"method", r.Method,
		"path", r.URL.Path,
		"error", err,
		"read", read,
		"limit", limit,
	}
	if r.ContentLength >= 0 {
		attrs = append(attrs, "contentLength", r.ContentLength)
	}

	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		slog.Warn(listener+": request body exceeds size limit", attrs...)
		return
	}
	// Everything else — client disconnect, truncated upload, transport error —
	// is one class: the body never arrived. The error and the byte count say
	// which, and no further split changes what an operator does about it.
	slog.Warn(listener+": request body unreadable", attrs...)
}

// Rejection maps a body-read failure to the downstream status and JSON body.
// Only a genuine overrun is a 413; anything else is a 400, so a disconnect is
// not reported to the agent as an oversized payload.
func Rejection(err error) (int, string) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return http.StatusRequestEntityTooLarge, `{"error":"request body too large"}`
	}
	return http.StatusBadRequest, `{"error":"request body unreadable"}`
}

// Failure is what a listener records for a request whose body it could not
// buffer: the status for the row, and why. The pipeline never ran on such a
// request, so this row is the only trace it leaves; without one, a request the
// client saw fail did not appear in the session timeline at all.
//
// An overrun is proxy_error, the kind for a limit that is ours, and its message
// carries the announced size when there is one: that is the figure that says
// how far over the limit the client went. A body that broke off because the
// client went away is client_canceled with a 499, as for an exchange the client
// abandons — net/http cancels the request context on the read error, and nobody
// received the 400. Anything else is request_unreadable, named for the log
// line and the JSON error the client got.
func Failure(r *http.Request, limit int64, err error) (int, *pipeline.EventError) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		msg := fmt.Sprintf("request body over the %d-byte buffer limit", limit)
		if r.ContentLength >= 0 {
			msg = fmt.Sprintf("request body of %d bytes is over the %d-byte buffer limit", r.ContentLength, limit)
		}
		return http.StatusRequestEntityTooLarge, &pipeline.EventError{Kind: "proxy_error", Message: msg}
	}
	if r.Context().Err() != nil {
		return pipeline.StatusClientClosedRequest, &pipeline.EventError{Kind: "client_canceled", Message: err.Error()}
	}
	return http.StatusBadRequest, &pipeline.EventError{Kind: "request_unreadable", Message: err.Error()}
}
