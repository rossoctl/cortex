package auth

import "strings"

// ExtractBearer pulls the token out of an HTTP `Authorization` header
// value. Returns the token (no "Bearer " prefix) on a well-formed
// bearer header, "" otherwise. The "Bearer" scheme match is
// case-insensitive per RFC 6750 §2.1.
//
// Used by every listener that touches request headers — extauthz,
// extproc, forwardproxy — so it lives here in authlib/auth (the home
// of the bearer/JWT composition layer) rather than being duplicated
// per listener.
func ExtractBearer(authHeader string) string {
	if len(authHeader) > 7 && strings.EqualFold(authHeader[:7], "bearer ") {
		return authHeader[7:]
	}
	return ""
}
