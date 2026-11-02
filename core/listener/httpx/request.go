package httpx

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// SetRequestBody replaces r's body with body and fixes the headers that describe it:
// Content-Length to the new size, Content-Encoding dropped (a rewritten body is
// plaintext — the caller decompressed or rebuilt it).
//
// ONE definition for every listener (#1567): the forward proxy carried this as setBody
// and the reverse proxy inlined the same four lines, which is the copy pattern that
// let every other duplicated helper drift.
func SetRequestBody(r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	r.Header.Del("Content-Encoding")
}

// SyncRequestHeaders propagates every header mutation a pipeline made back onto the
// request that will be sent: h started as a clone of r.Header, so entries missing from
// h were deleted by a plugin, empty entries are deletes, and the rest are set /
// overwritten. Content-Length and Content-Encoding are skipped — the body-rewrite
// path and the transport own those.
//
// Shared for the same reason as SetRequestBody: the forward proxy's syncHeaders and
// the reverse proxy's inline copy of it were identical only because someone kept
// them that way.
func SyncRequestHeaders(r *http.Request, h http.Header) {
	skip := func(k string) bool {
		return strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Content-Encoding")
	}
	for k := range r.Header {
		if skip(k) {
			continue
		}
		if _, ok := h[k]; !ok {
			r.Header.Del(k) // plugin removed it
		}
	}
	for k, vv := range h {
		if skip(k) {
			continue
		}
		if len(vv) == 0 {
			r.Header.Del(k) // h[k] = nil is a delete, same as Del(k)
			continue
		}
		r.Header[k] = append([]string(nil), vv...) // set / overwrite
	}
}
