package redact

import (
	"net/http"
	"net/url"
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

// storedSecretHeaders are credentials Headers also drops, beyond SecretHeaders: the cpex
// plugin's list. Not in SecretHeaders because a script may set these, and SecretHeaders is also
// the set a script is refused.
var storedSecretHeaders = map[string]bool{
	"x-amz-security-token": true,
	"x-auth-token":         true,
	"x-authorization":      true,
	"x-session-token":      true,
	"x-secret-token":       true,
	"x-csrf-token":         true,
	"x-platform-secret":    true,
	"x-authbridge-secret":  true,
}

// Headers is a copy of h without credential headers, for storing.
func Headers(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vs := range h {
		if lk := strings.ToLower(k); !SecretHeaders[lk] && !storedSecretHeaders[lk] {
			out[k] = append([]string(nil), vs...)
		}
	}
	return out
}

// secretParams are query parameters that carry credentials: some APIs take the key in the URL,
// as Gemini's ?key= does.
var secretParams = map[string]bool{"key": true, "api_key": true, "apikey": true, "access_token": true, "token": true}

// Query is raw without credential parameters, for storing. The rest is kept as it was, in its
// order and escaping, since a replay must send the same bytes. A query that does not parse is
// dropped whole: what it holds cannot be told apart.
func Query(raw string) string {
	if _, err := url.ParseQuery(raw); err != nil {
		return ""
	}
	var kept []string
	for _, p := range strings.Split(raw, "&") {
		name, _, _ := strings.Cut(p, "=")
		if n, err := url.QueryUnescape(name); err == nil && secretParams[strings.ToLower(n)] {
			continue
		}
		kept = append(kept, p)
	}
	return strings.Join(kept, "&")
}
