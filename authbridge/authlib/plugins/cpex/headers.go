package cpex

import (
	"net/http"
	"strings"
)

// secretHeaderPrefixes lists header-name prefixes that are NEVER
// forwarded into CPEX. These are session cookies and platform-issued
// internal secrets that no CPEX plugin ever legitimately needs.
//
// This is NOT the only line of defense, and it does not replace
// CPEX's capability model. The `http` extension slot is
// capability-gated (`read_headers`); an audit/logger plugin SHOULD be
// configured without that capability so `filter_extensions` blinds it
// to the slot wholesale. Do that too. The boundary strip is
// complementary, for two gaps the capability gate cannot close:
//
//  1. `read_headers` is whole-slot — a plugin that legitimately
//     needs ONE header (the jwt plugin reading `Authorization`, a
//     router reading `Host`) must hold `read_headers` and thereby
//     sees EVERY header, including cookies and api-keys. The gate
//     can't say "Authorization yes, Cookie no"; this strip can.
//  2. Capability filtering happens at per-plugin dispatch only — the
//     canonical Extensions captured into the (unauthenticated)
//     session API are UNFILTERED. A secret that enters the `http`
//     slot is exposed there regardless of any plugin's caps.
//
// Stripping at this boundary keeps these secrets out of the canonical
// context entirely, so neither a misconfigured capability grant, a
// future plugin that declares `read_headers`, nor the session surface
// can leak them — the same posture CPEX itself uses for raw tokens
// (`#[serde(skip)]` AND a capability gate).
//
// NOTE on `Authorization`: deliberately NOT stripped. CPEX's
// identity/jwt plugins (jwt-client, etc.) read the bearer token from
// the Authorization header to validate signature, audience, expiry,
// and to extract role/perm/team/group claims that APL predicates
// (`require(role.hr)`, `redact(!perm.view_ssn)`, …) gate on. Strip
// it and you lose every gate that depends on the client identity —
// the request continues to evaluate against an empty client bag,
// which silently allows traffic the policy meant to deny.
//
// The audit-log risk this opens — bearer tokens reaching audit
// payloads — is mitigated by configuring audit-log to drop
// Authorization from its output (or by terminating TLS at the
// sidecar so tokens are short-lived and bound to mTLS).
//
// This table and the helpers below are tag-free (no //go:build cpex)
// so the default CGO_ENABLED=0 test build exercises them; the cgo
// adapter in manager_cpex.go is their only production caller.
var secretHeaderPrefixes = []string{
	"cookie",
	"set-cookie",
	"proxy-authorization",
	"x-amz-security-token",
}

// secretHeaderExact lists exact (case-insensitive) header names that
// must always be stripped. Used for one-off names that don't fit a
// prefix scheme.
var secretHeaderExact = map[string]struct{}{
	"x-api-key":           {},
	"x-auth-token":        {},
	"x-authorization":     {},
	"x-secret-token":      {},
	"x-session-token":     {},
	"x-csrf-token":        {},
	"x-platform-secret":   {},
	"x-authbridge-secret": {},
}

// flattenHeaders converts http.Header (multi-value) into the single-value
// map shape CPEX's HttpExtension.RequestHeaders requires. Multi-value
// headers are comma-joined per RFC 7230 §3.2.2 — the standard
// safe-merge for repeatable HTTP headers. (Set-Cookie, which doesn't
// follow §3.2.2, lands in secretHeaderPrefixes and is stripped before
// reaching here.)
//
// Sensitive headers (Authorization, Cookie, X-Api-Key, …) are
// dropped via secretHeaderPrefixes / secretHeaderExact, NOT silently
// truncated.
func flattenHeaders(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, vs := range h {
		if isSensitive(k) || len(vs) == 0 {
			continue
		}
		// RFC 7230 §3.2.2: repeatable HTTP headers combine with
		// comma; non-repeatable ones either have a single value
		// already or were already filtered (Set-Cookie).
		if len(vs) == 1 {
			out[k] = vs[0]
		} else {
			out[k] = strings.Join(vs, ", ")
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// isSensitive checks both the prefix and exact-name secret tables.
// Case-insensitive — HTTP header names are case-insensitive on the
// wire, and any policy that depends on the case of "Authorization"
// vs "authorization" is already broken.
func isSensitive(name string) bool {
	lower := strings.ToLower(name)
	if _, ok := secretHeaderExact[lower]; ok {
		return true
	}
	for _, prefix := range secretHeaderPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}
