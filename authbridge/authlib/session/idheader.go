package session

import (
	"log/slog"
	"net/http"
	"unicode"
	"unicode/utf8"
)

// ClaudeCodeSessionHeader is the request header the Claude Code CLI sets on
// every inference request, carrying a UUID that is stable for the life of one
// session and reused when a session is resumed (`claude --resume <id>`). It is
// the same id the CLI names its transcript after
// (~/.claude/projects/<slug>/<uuid>.jsonl), so a bucket keyed on it lines up
// with what the user sees in their own client.
//
// Verified against claude-cli/2.1.266. The value is also duplicated inside the
// request body as metadata.user_id, which is why losing the header is not
// silently fatal — see IDFromHeaders for why we read the header only.
const ClaudeCodeSessionHeader = "X-Claude-Code-Session-Id"

// BobSessionHeader is the request header the Bob coding agent sets to name the
// task a request belongs to. Two differences from ClaudeCodeSessionHeader are
// worth knowing before reading a bucket keyed on it: it is not verified against a
// released client, and the value is a task id rather than a session uuid — so it
// is stable for as long as Bob reuses that task, which may span what a user would
// call several sessions, or none.
//
// UNNAMESPACED, and in the default list, which is the one property here worth a
// second thought. X-Task-Id is a name any client might plausibly send for its own
// reasons, and nothing ties it to Bob — so unrelated traffic that happens to use
// it captures a bucket named after its value, rather than falling through to
// ActiveSession() as it did before this header was consulted. That is a
// mis-grouping, not an escalation: IDFromHeaders already treats every id as
// client-asserted and untrusted (see its TRUST section), and a caller able to
// send this header could equally send the Claude Code one. Accepted because the
// alternative — Bob traffic silently pooling in the shared bucket — is the
// problem this solves, and session.id_headers is the per-deployment override
// either way.
//
// Canonical HTTP casing is required here even though IDFromHeaders reads via
// http.Header.Get, which canonicalizes: tests construct http.Header map literals
// directly from these constants, and a raw map literal does not.
const BobSessionHeader = "X-Task-Id"

// IDFromHeaders returns the first usable session id found in h among names,
// in order, or "" when none is present. Callers treat "" as "fall back to
// whatever bucketing you did before" — never as an error, so a client that
// stops sending the header degrades to the old shared bucket instead of
// dropping telemetry.
//
// The header is read rather than the equivalent body field (Claude Code
// duplicates the id in metadata.user_id) because a header costs nothing to
// reach: bucketing must work for every request, including ones no parser
// matched and ones whose body was never buffered.
//
// TRUST: the returned id is CLIENT-ASSERTED AND UNAUTHENTICATED. validity
// checks below stop a value from corrupting a log line or a terminal; nothing
// establishes that the client owns the session it names. Two distinct effects
// follow, and the second is the cheaper one:
//
//   - Targeting. A client may name another session's id and write into that
//     bucket, or name "default" and become indistinguishable from unattributed
//     traffic. Since these buckets now feed cost attribution, a poisoned key
//     mis-attributes spend, not just a TUI row.
//   - Cardinality. Store.Append evicts the oldest bucket once the store exceeds
//     session.max_sessions (default 100), so a client that sends distinct random
//     ids evicts every real bucket without needing to know a single one of them.
//     Memory stays bounded; the telemetry does not survive. That destroys history
//     rather than mis-filing it, and it reaches the buckets this comment calls
//     trustworthy below — an agent can evict the A2A-correlated buckets, not only
//     redirect its own. session.max_sessions is the only knob that bounds it, and
//     raising it trades eviction for memory rather than removing the effect.
//
// The blast radius is set by one property only: the session store is
// in-process, so the bucket namespace is shared by exactly those clients that
// can reach this proxy. Nothing here narrows that further — the listener role
// says which pipeline runs, not how many workloads a process serves, and
// in-cluster the forward proxy binds a wildcard address by default
// (ListenerConfig.BindLoopbackOnly is off), so "only my own pod reaches it" is a
// property of the deployment — network namespace, NetworkPolicy, who sets
// HTTP_PROXY — and not of this code.
//
// So: a laptop with bind_loopback_only is a single trust domain and the user owns
// every session. A sidecar serving one workload confines mis-filing to that
// workload's own telemetry. A shared or standalone forward proxy serving several
// workloads shares one bucket namespace, and there a poisoned id does cross
// workloads. Even within a single pod there is a residual worth naming: an agent
// serving several users can redirect telemetry away from the inbound A2A turn
// that caused it, and that A2A correlation is the trustworthy signal in-cluster.
//
// Set session.id_headers to an empty list wherever attribution is a trust
// boundary rather than a convenience. Compare DefaultSessionID, which carries the
// mirror-image caveat for the shared bucket.
func IDFromHeaders(h http.Header, names []string) string {
	for _, name := range names {
		id := h.Get(name)
		if id == "" {
			continue
		}
		if !validHeaderSessionID(id) {
			// Debug, not Warn: on a shared proxy any client can send anything,
			// and a rejected id already degrades visibly to the old bucket.
			// Logging the value itself is what the check exists to prevent, so
			// report only its shape.
			slog.Debug("session: ignoring unusable session id header",
				"header", name, "len", len(id))
			// continue, not return "": an unusable value is not a claim about
			// which session this is, so it does not get to veto the next
			// configured header. names is a precedence order over CLIENTS —
			// "if both a Claude Code and a Bob id are present, prefer this one"
			// — and a malformed winner means that client said nothing
			// intelligible, not that attribution should be abandoned. Pinned by
			// TestIDFromHeaders_InvalidWinnerFallsThroughToNextHeader.
			continue
		}
		return id
	}
	return ""
}

// validHeaderSessionID reports whether a client-supplied id is safe to use as
// a bucket key.
//
// Three rules, each for a concrete reason:
//
//   - No longer than MaxSessionIDLen, counted in BYTES. Store.Append truncates
//     with sessionID[:MaxSessionIDLen], a byte slice, and two overlong ids
//     sharing a prefix would then silently merge into one bucket — precisely the
//     confusion per-session bucketing exists to remove. Counting runes here
//     instead would let a multi-byte id under the rune budget through and hand it
//     to the store to byte-truncate, reintroducing that merge. Refusing falls
//     back to the old shared bucket, which is wrong in a way the operator can
//     see, rather than wrong in a way they cannot.
//   - Valid UTF-8. The id is echoed in /v1/sessions JSON, and encoding/json
//     substitutes U+FFFD for invalid bytes on marshal — so an id keyed on raw
//     bytes would come back out as a DIFFERENT string, and an operator could not
//     match what they read to the bucket it names.
//   - No control characters, tested per RUNE. The id is rendered in abctl's TUI,
//     interpolated into structured log lines, and returned in that JSON. A
//     newline forges a log line and an ESC sequence rewrites the operator's
//     terminal. Printable characters above ASCII are deliberately left alone so a
//     non-Anthropic client with its own id scheme still works — but that carve-out
//     has to be about PRINTABLE characters, and a byte loop cannot draw the line:
//     a C1 control (U+0080–U+009F) encodes as two bytes, neither of which looks
//     like a control byte, so it passed a check whose own stated rule excluded it.
//     unicode.IsControl covers C0, DEL and C1 together and implements the rule as
//     written.
//
// Deliberately NOT rejected: bidi overrides (U+202E) and zero-width characters,
// which are category Cf rather than Cc. Both can make two distinct buckets look
// alike in the TUI, so there is an argument for requiring unicode.IsPrint
// instead. Left out because it widens the rule beyond "no control characters"
// and risks refusing a legitimate client's ids; revisit if bucket names ever
// become something a human types rather than something a client generates.
func validHeaderSessionID(id string) bool {
	if len(id) > MaxSessionIDLen {
		return false
	}
	if !utf8.ValidString(id) {
		return false
	}
	for _, r := range id {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
