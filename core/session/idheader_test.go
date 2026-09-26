package session

import (
	"net/http"
	"strings"
	"testing"
)

// TestIDFromHeaders_FirstConfiguredHeaderWins pins the ordering contract of
// session.id_headers: the list is a precedence order, not a set. An operator
// running two different agents through one proxy relies on this to say which
// client's notion of a session wins when a request somehow carries both.
func TestIDFromHeaders_FirstConfiguredHeaderWins(t *testing.T) {
	h := http.Header{
		ClaudeCodeSessionHeader: []string{"claude-session"},
		"X-Other-Agent-Session": []string{"other-session"},
	}

	if got := IDFromHeaders(h, []string{ClaudeCodeSessionHeader, "X-Other-Agent-Session"}); got != "claude-session" {
		t.Errorf("IDFromHeaders() = %q, want %q", got, "claude-session")
	}
	// Reversing the configured order reverses the winner.
	if got := IDFromHeaders(h, []string{"X-Other-Agent-Session", ClaudeCodeSessionHeader}); got != "other-session" {
		t.Errorf("IDFromHeaders() reversed = %q, want %q", got, "other-session")
	}
	// A configured header that is absent is skipped, not fatal.
	if got := IDFromHeaders(h, []string{"X-Absent", ClaudeCodeSessionHeader}); got != "claude-session" {
		t.Errorf("IDFromHeaders() with absent first = %q, want %q", got, "claude-session")
	}
}

// TestIDFromHeaders_InvalidWinnerFallsThroughToNextHeader pins the interaction
// between validation and precedence — the one branch where the two rules meet.
// A present-but-unusable value in the higher-precedence header must not veto the
// runner-up: it is not a claim about which session this is, so attribution falls
// through rather than being abandoned. The alternative reading (a bad winner
// hard-stops the lookup) is deliberately NOT the behavior; see IDFromHeaders.
func TestIDFromHeaders_InvalidWinnerFallsThroughToNextHeader(t *testing.T) {
	names := []string{ClaudeCodeSessionHeader, "X-Other-Agent-Session"}

	t.Run("control characters in the winner", func(t *testing.T) {
		h := http.Header{
			ClaudeCodeSessionHeader: []string{"poisoned\nvalue"},
			"X-Other-Agent-Session": []string{"good-session"},
		}
		if got := IDFromHeaders(h, names); got != "good-session" {
			t.Errorf("IDFromHeaders() = %q, want %q", got, "good-session")
		}
	})

	t.Run("over-length winner", func(t *testing.T) {
		h := http.Header{
			ClaudeCodeSessionHeader: []string{strings.Repeat("a", MaxSessionIDLen+1)},
			"X-Other-Agent-Session": []string{"good-session"},
		}
		if got := IDFromHeaders(h, names); got != "good-session" {
			t.Errorf("IDFromHeaders() = %q, want %q", got, "good-session")
		}
	})

	t.Run("both unusable yields empty so the caller falls back", func(t *testing.T) {
		h := http.Header{
			ClaudeCodeSessionHeader: []string{"bad\nwinner"},
			"X-Other-Agent-Session": []string{"bad\rrunner-up"},
		}
		if got := IDFromHeaders(h, names); got != "" {
			t.Errorf("IDFromHeaders() = %q, want \"\"", got)
		}
	})
}

// TestIDFromHeaders_NoUsableIDReturnsEmpty covers every way the lookup comes up
// empty. All of them must return "" so the caller falls back to its previous
// bucketing rather than inventing a bucket.
func TestIDFromHeaders_NoUsableIDReturnsEmpty(t *testing.T) {
	cases := []struct {
		name    string
		headers http.Header
		names   []string
	}{
		{"no headers configured", http.Header{ClaudeCodeSessionHeader: []string{"x"}}, nil},
		{"configured header absent", http.Header{}, []string{ClaudeCodeSessionHeader}},
		{"nil header map", nil, []string{ClaudeCodeSessionHeader}},
		{"empty value", http.Header{ClaudeCodeSessionHeader: []string{""}}, []string{ClaudeCodeSessionHeader}},
		{"only an unusable value", http.Header{ClaudeCodeSessionHeader: []string{"bad\nvalue"}}, []string{ClaudeCodeSessionHeader}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IDFromHeaders(tc.headers, tc.names); got != "" {
				t.Errorf("IDFromHeaders() = %q, want \"\"", got)
			}
		})
	}
}

// TestIDFromHeaders_RejectsControlCharactersAboveASCII closes the gap between
// the documented rule and the check. "No control characters, printable bytes
// above ASCII left alone" cannot be enforced by inspecting bytes: a C1 control
// encodes as two bytes, neither of which looks like a control byte, so it slips
// through a byte loop. U+009B is the C1 CSI — a control character above ASCII,
// not a printable one — so the carve-out for other clients' id schemes was wider
// than intended.
func TestIDFromHeaders_RejectsControlCharactersAboveASCII(t *testing.T) {
	for _, bad := range []string{
		"abc\u009bdef", // C1 CSI, encodes as C2 9B — neither byte looks like a control
		"abc\u0085def", // C1 NEL
		"abc\u0080def", // C1 PAD, low end of the block
		"abc\u009fdef", // C1 APC, high end of the block
	} {
		h := http.Header{ClaudeCodeSessionHeader: []string{bad}}
		if got := IDFromHeaders(h, []string{ClaudeCodeSessionHeader}); got != "" {
			t.Errorf("IDFromHeaders(%q) = %q, want \"\" (control character above ASCII)", bad, got)
		}
	}
}

// TestIDFromHeaders_RejectsInvalidUTF8 keeps the bucket key round-trippable. The
// id is echoed in /v1/sessions JSON, and encoding/json substitutes U+FFFD for
// invalid UTF-8 on marshal — so an id the store keyed on raw bytes would come
// back out as a DIFFERENT string, and an operator could not match what they read
// to the bucket it names.
func TestIDFromHeaders_RejectsInvalidUTF8(t *testing.T) {
	for _, bad := range []string{
		"abc\xffdef",      // never valid in UTF-8
		"abc\xc2",         // truncated two-byte sequence
		"abc\xed\xa0\x80", // surrogate half, rejected by Go's UTF-8
	} {
		h := http.Header{ClaudeCodeSessionHeader: []string{bad}}
		if got := IDFromHeaders(h, []string{ClaudeCodeSessionHeader}); got != "" {
			t.Errorf("IDFromHeaders(%q) = %q, want \"\" (invalid UTF-8)", bad, got)
		}
	}
}

// TestIDFromHeaders_AcceptsPrintableNonASCII pins the carve-out the control check
// must NOT eat: a client with its own id scheme in a non-Latin script keeps
// working. This is the test that fails if the fix for C1 controls over-corrects
// into "ASCII only".
func TestIDFromHeaders_AcceptsPrintableNonASCII(t *testing.T) {
	for _, good := range []string{
		"café-session",   // Latin-1 supplement, printable
		"会话-42",          // CJK
		"sesión-emoji-🙂", // outside the BMP
	} {
		h := http.Header{ClaudeCodeSessionHeader: []string{good}}
		if got := IDFromHeaders(h, []string{ClaudeCodeSessionHeader}); got != good {
			t.Errorf("IDFromHeaders(%q) = %q, want it accepted", good, got)
		}
	}
}

// TestIDFromHeaders_LengthLimitIsBytesNotRunes guards the trap in fixing the
// control check: Store.Append truncates with sessionID[:MaxSessionIDLen], a BYTE
// slice. If the limit were counted in runes, a multi-byte id under the rune
// budget but over the byte budget would pass here and then be byte-truncated by
// the store, reintroducing the silent prefix-merge this validator exists to
// prevent.
func TestIDFromHeaders_LengthLimitIsBytesNotRunes(t *testing.T) {
	// 200 runes, 400 bytes: comfortably under MaxSessionIDLen in runes, over it
	// in bytes. Must be refused.
	id := strings.Repeat("é", 200)
	if len(id) <= MaxSessionIDLen {
		t.Fatalf("fixture is not over the byte limit: %d bytes", len(id))
	}
	h := http.Header{ClaudeCodeSessionHeader: []string{id}}
	if got := IDFromHeaders(h, []string{ClaudeCodeSessionHeader}); got != "" {
		t.Errorf("IDFromHeaders() accepted a %d-byte id (%d runes); the store would truncate it",
			len(id), len([]rune(id)))
	}
}

// TestIDFromHeaders_AcceptsIDAtMaxLength is the boundary companion to the
// over-length rejection: exactly MaxSessionIDLen is stored intact by
// Store.Append, so it must be accepted. An off-by-one here would silently push
// legitimate ids into the shared bucket.
func TestIDFromHeaders_AcceptsIDAtMaxLength(t *testing.T) {
	atLimit := strings.Repeat("a", MaxSessionIDLen)
	h := http.Header{ClaudeCodeSessionHeader: []string{atLimit}}
	if got := IDFromHeaders(h, []string{ClaudeCodeSessionHeader}); got != atLimit {
		t.Errorf("IDFromHeaders() rejected an id of exactly MaxSessionIDLen (%d)", MaxSessionIDLen)
	}
}

// TestBobSessionHeader_IsCanonicalAndResolves pins the two properties of the Bob
// header that a wrong constant would break silently rather than loudly.
//
// Casing first: IDFromHeaders reads through http.Header.Get, which canonicalizes
// whatever it is handed, so a lowercase constant would work in production and
// fail only here — where the fixture is a raw map literal, exactly as every other
// test in this file builds one. That asymmetry is the trap, so assert the
// constant's own form rather than relying on a round-trip to reveal it.
//
// Then that a Bob id actually resolves, and loses to a Claude Code id when both
// are present. The precedence machinery is already covered generically by
// TestIDFromHeaders_FirstConfiguredHeaderWins; what this adds is the real pair of
// constants rather than a stand-in literal.
func TestBobSessionHeader_IsCanonicalAndResolves(t *testing.T) {
	if got := http.CanonicalHeaderKey(BobSessionHeader); got != BobSessionHeader {
		t.Errorf("BobSessionHeader = %q, want canonical form %q; a raw http.Header literal will not match it",
			BobSessionHeader, got)
	}

	// A local copy of the shipped order, NOT a read of it: core/config imports
	// this package, so it cannot be imported back here to assert the real default.
	// That means a reorder in config.SessionIDHeaders would not fail this test —
	// TestSessionConfig_SessionIDHeaders over in that package is the guard for
	// that, and it asserts the list in order. What this fixture pins is the
	// behavior of the pair once ordered, not the ordering itself.
	names := []string{ClaudeCodeSessionHeader, BobSessionHeader}

	t.Run("a Bob id alone is used", func(t *testing.T) {
		h := http.Header{BobSessionHeader: []string{"task-42"}}
		if got := IDFromHeaders(h, names); got != "task-42" {
			t.Errorf("IDFromHeaders() = %q, want %q", got, "task-42")
		}
	})

	t.Run("a Claude Code id wins when both are present", func(t *testing.T) {
		h := http.Header{
			ClaudeCodeSessionHeader: []string{"claude-session"},
			BobSessionHeader:        []string{"task-42"},
		}
		if got := IDFromHeaders(h, names); got != "claude-session" {
			t.Errorf("IDFromHeaders() = %q, want %q", got, "claude-session")
		}
	})
}
