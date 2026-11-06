package redact

import (
	"net/http"
	"net/url"
	"slices"
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

// Headers is a copy of h without credential headers, for storing.
func Headers(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vs := range h {
		if !CredentialName(k) {
			out[k] = append([]string(nil), vs...)
		}
	}
	return out
}

// credentialWords are the words of a name that make a header or a query parameter a credential.
var credentialWords = []string{"key", "apikey", "token", "auth", "authorization", "cookie",
	"secret", "signature", "password", "credential", "credentials"}

// CredentialName reports whether name, split at "-" and "_" with case ignored, has one of
// credentialWords. It covers every name in SecretHeaders.
func CredentialName(name string) bool {
	for _, w := range strings.FieldsFunc(strings.ToLower(name), func(r rune) bool { return r == '-' || r == '_' }) {
		if slices.Contains(credentialWords, w) {
			return true
		}
	}
	return false
}

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
		if n, err := url.QueryUnescape(name); err == nil && CredentialName(n) {
			continue
		}
		kept = append(kept, p)
	}
	return strings.Join(kept, "&")
}
