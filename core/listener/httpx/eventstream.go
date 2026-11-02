package httpx

import "strings"

// IsEventStream reports whether a Content-Type header value names the SSE media
// type. Tolerates parameters ("text/event-stream; charset=utf-8") and ASCII case
// differences.
//
// ONE definition for all three listeners (#1567): the forward proxy, reverse proxy
// and ext_proc listeners each carried their own copy, and a fix to one — the
// parameter tolerance — reached the others only by re-editing every copy. The
// copies were identical only because someone kept them that way.
func IsEventStream(contentType string) bool {
	if contentType == "" {
		return false
	}
	// Strip parameters: "text/event-stream; charset=utf-8" → "text/event-stream".
	if idx := strings.IndexByte(contentType, ';'); idx >= 0 {
		contentType = contentType[:idx]
	}
	return strings.EqualFold(strings.TrimSpace(contentType), "text/event-stream")
}
