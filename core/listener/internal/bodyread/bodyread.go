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
	"log/slog"
	"net/http"
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
