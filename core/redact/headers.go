package redact

import (
	"net/http"
	"strings"
)

// SecretHeaders are the request and response headers that carry credentials. They never reach
// a script, and they are removed from a request before the session archive captures it: a
// credential has no business in a log, on the timeline or on disk. Keys are lower-case.
var SecretHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"set-cookie":          true,
	"x-api-key":           true,
	"api-key":             true,
	"x-goog-api-key":      true,
}

// Headers is a copy of h without the SecretHeaders.
func Headers(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vs := range h {
		if !SecretHeaders[strings.ToLower(k)] {
			out[k] = append([]string(nil), vs...)
		}
	}
	return out
}
