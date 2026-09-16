package pipeline

import (
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestParseUserAgent(t *testing.T) {
	for _, tc := range []struct {
		name, ua          string
		wantName, wantVer string
		wantNil           bool
	}{
		{"absent is nil", "", "", "", true},
		{"claude code", "claude-cli/2.1.14 (external, cli)", "claude-code", "2.1.14", false},
		{"claude code bare", "claude-cli/2.1.14", "claude-code", "2.1.14", false},
		{"unrecognised keeps raw only", "SomeNewAgent/9.9", "", "", false},
		{"curl is not a coding agent", "curl/8.4.0", "", "", false},
		{"whitespace only is nil", "   ", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseUserAgent(tc.ua)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("ParseUserAgent(%q) = %+v, want nil", tc.ua, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("ParseUserAgent(%q) = nil, want a client", tc.ua)
			}
			if got.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tc.wantName)
			}
			if got.Version != tc.wantVer {
				t.Errorf("Version = %q, want %q", got.Version, tc.wantVer)
			}
			// Raw is always kept: an unrecognised agent we can still NAME is data.
			if got.Raw == "" {
				t.Error("Raw is empty; the verbatim UA must be kept")
			}
		})
	}
}

func TestParseUserAgent_CapsTheRetainedValue(t *testing.T) {
	// The header is caller-controlled and the value is retained in bucket label
	// maps for a full ring lap. Without a cap a caller parks arbitrary bytes in
	// memory from off-host, on the synchronous session-append path — the same
	// reasoning as usage.maxLabelLen.
	long := strings.Repeat("A", 10_000)
	got := ParseUserAgent(long)
	if got == nil {
		t.Fatal("ParseUserAgent returned nil for a long UA")
	}
	if len(got.Raw) > maxClientLen {
		t.Errorf("Raw retained %d bytes, want <= %d", len(got.Raw), maxClientLen)
	}
}

func TestParseUserAgent_CapsBeforeMatching(t *testing.T) {
	// A caller cannot buy an unbounded Version either: the version is cut out of
	// the already-capped string, so no field on EventClient can exceed the cap.
	got := ParseUserAgent("claude-cli/" + strings.Repeat("9", 10_000))
	if got == nil {
		t.Fatal("ParseUserAgent returned nil")
	}
	if len(got.Version) > maxClientLen {
		t.Errorf("Version retained %d bytes, want <= %d", len(got.Version), maxClientLen)
	}
	if len(got.Label()) > maxClientLen+len(got.Name)+1 {
		t.Errorf("Label() is %d bytes; the cap must bound every derived string", len(got.Label()))
	}
}

// A control character in the User-Agent never reaches Label().
//
// THROUGH Label(), not through the sanitiser in isolation, because Label() is what the
// consumers call: the live usage aggregator keys its byAgent series on it and the cost
// ledger writes it into a durable row. A test that only exercised sanitizeUA would still
// pass if ParseUserAgent stopped calling it.
//
// The forward proxy is protected by net/http, which rejects control bytes in a header
// value — accidentally, and only there. THE EXT_PROC PATH HAS NO SUCH PARSER: Envoy hands
// header values over as protobuf bytes, so on that path these strings are exactly what a
// client sent. CWE-150.
func TestParseUserAgent_SanitisesControlCharactersBeforeLabel(t *testing.T) {
	for _, tc := range []struct {
		name, ua string
		// want is the exact label, so the assertion pins the substitution rather than
		// merely the absence of the byte.
		want string
	}{
		// The C0 case: ESC [ 2J clears the screen, ESC [ 31m recolours it.
		{"esc", "claude-cli/2.1.14\x1b[2J\x1b[31mPWNED", "claude-code/2.1.14\uFFFD[2J\uFFFD[31mPWNED"},
		// C1, and the reason a C0-only filter is not enough. U+009B IS the CSI: a terminal
		// decoding UTF-8 acts on "\u009b2J" exactly as it acts on "\x1b[2J", and there is no
		// byte below 0x20 anywhere in it. It encodes as 0xC2 0x9B, so a scan for control
		// BYTES walks straight past it.
		{"c1 CSI in the version", "claude-cli/2.1.14\u009b2J", "claude-code/2.1.14\uFFFD2J"},
		{"c1 CSI in an unrecognised agent", "newagent/1\u009b31m", "newagent/1\uFFFD31m"},
		// DEL, and a raw byte that is not valid UTF-8 at all — the same character in a pane
		// that is not in UTF-8 mode.
		{"del", "newagent/1\x7f", "newagent/1\uFFFD"},
		{"invalid byte", "newagent/1\x9b", "newagent/1\uFFFD"},
		// A newline breaks a table apart wherever the label is rendered in one.
		{"newline", "newagent/1\nfake-row", "newagent/1\uFFFDfake-row"},
		// NO ESCAPE SEQUENCE AND NO TERMINAL NEEDED, which is why these belong in the same
		// set as C1 rather than in a separate "cosmetic" one. U+202E reorders the glyphs that
		// follow it, so a self-reported agent string can be made to RENDER as another agent's
		// name in the column beside real spend — in a browser, in a chart, anywhere at all,
		// not only in a pane that decodes escape sequences.
		{"bidi override", "newagent/1\u202egnitcepsus", "newagent/1\uFFFDgnitcepsus"},
		{"bidi isolate", "newagent/1\u2066x\u2069", "newagent/1\uFFFDx\uFFFD"},
		// Zero-width: two DISTINCT keys that render identically, so a reader comparing rows
		// cannot see that they are different agents — and the aggregate keeps them apart.
		{"zero-width space", "newagent/1\u200b", "newagent/1\uFFFD"},
		{"zero-width joiner", "newagent/1\u200d2", "newagent/1\uFFFD2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := ParseUserAgent(tc.ua)
			if c == nil {
				t.Fatal("ParseUserAgent returned nil for a UA that was sent")
			}
			if got := c.Label(); got != tc.want {
				t.Errorf("Label() = %q, want %q", got, tc.want)
			}
			// And every field, since Raw is persisted and Version is joined into the label
			// for a recognised agent.
			for name, got := range map[string]string{"Raw": c.Raw, "Version": c.Version, "Label": c.Label()} {
				if hasControlRunes(got) {
					t.Errorf("%s = %q still carries a control character", name, got)
				}
			}
		})
	}
}

// The version a recognised agent reports is sanitised too, and that matters more than the
// raw string: Label() joins it to a name we control, so "claude-code/<escape sequence>" is
// a row that looks trustworthy up to the slash.
func TestParseUserAgent_SanitisationSurvivesTheProductTokenParse(t *testing.T) {
	c := ParseUserAgent("claude-cli/2.1.14\x1b[31m (external, cli)")
	if c == nil {
		t.Fatal("ParseUserAgent returned nil")
	}
	if c.Name != "claude-code" {
		t.Errorf("Name = %q; sanitising must not break recognition of a real agent", c.Name)
	}
	if c.Version != "2.1.14\uFFFD[31m" {
		t.Errorf("Version = %q, want the ESC replaced and the rest kept", c.Version)
	}
}

// SANITISE THEN CAP, and the cap is still in BYTES.
//
// The order is what makes the byte cap mean anything: a substitution triples the string,
// so capping first would let 128 control bytes become 384 in the field that bounds a
// persisted line. Capping last is what makes the rune boundary necessary — a byte cut has
// a two-in-three chance of leaving a fragment of a 3-byte U+FFFD.
func TestParseUserAgent_SanitisesBeforeCappingAndStillHonoursTheByteCap(t *testing.T) {
	// Every byte a control byte, past the cap: 3x maxClientLen after substitution.
	c := ParseUserAgent(strings.Repeat("\x1b", maxClientLen+32))
	if c == nil {
		t.Fatal("ParseUserAgent returned nil for a UA that was sent")
	}
	if len(c.Raw) > maxClientLen {
		t.Errorf("Raw is %d bytes, want <= %d; the cap bounds retained memory and the "+
			"length of a persisted line, so it has to be a BYTE cap", len(c.Raw), maxClientLen)
	}
	if hasControlRunes(c.Raw) {
		t.Errorf("Raw = %q: capping must not be able to reintroduce a control character", c.Raw)
	}
	// A cut mid-U+FFFD is invalid UTF-8 in a string that goes to a durable file and to a
	// chart — the failure the substitution exists to avoid rather than to cause.
	if !utf8.ValidString(c.Raw) {
		t.Errorf("Raw = %q is not valid UTF-8; the byte cap split a rune", c.Raw)
	}
	if !utf8.ValidString(c.Label()) {
		t.Errorf("Label() = %q is not valid UTF-8", c.Label())
	}
}

// A multi-byte rune straddling the cap is not halved, and the byte bound still holds.
//
// Stated separately from the hostile case because this one is ordinary: a UA is usually
// ASCII, but nothing makes it so, and the cut lands wherever the client's bytes put it.
func TestParseUserAgent_CapCutsOnARuneBoundary(t *testing.T) {
	// 2-byte runes, so 64 of them is exactly maxClientLen: a few more puts a rune across
	// the boundary at byte 128, where a byte cut leaves half of it behind.
	c := ParseUserAgent(strings.Repeat("é", maxClientLen/2+8))
	if c == nil {
		t.Fatal("ParseUserAgent returned nil")
	}
	if len(c.Raw) > maxClientLen {
		t.Errorf("Raw is %d bytes, want <= %d", len(c.Raw), maxClientLen)
	}
	if !utf8.ValidString(c.Raw) {
		t.Errorf("Raw = %q is not valid UTF-8; the cut split a rune in half", c.Raw)
	}
	// And the cap is not quietly loosened to a rune count: 64 two-byte runes is the most
	// that fits, so the cut must land ON the byte cap rather than one rune past it.
	if len(c.Raw) != maxClientLen {
		t.Errorf("Raw is %d bytes; a rune-boundary cut must still honour the byte cap "+
			"exactly when the boundary allows it", len(c.Raw))
	}
}

// Valid non-ASCII is NOT mangled. The rule filters control characters, not "anything the
// author did not expect to see"; a sanitiser that ate legitimate text would make the
// breakdown wrong for every non-English label rather than for a hostile one.
func TestParseUserAgent_LeavesLegitimateNonASCIIAlone(t *testing.T) {
	const ua = "wetter-agent/1.0 (Zürich, ✓)"
	c := ParseUserAgent(ua)
	if c == nil {
		t.Fatal("ParseUserAgent returned nil")
	}
	if c.Raw != ua {
		t.Errorf("Raw = %q, want it untouched: %q", c.Raw, ua)
	}
}

func TestEventClient_Label(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    *EventClient
		want string
	}{
		{"nil is unknown", nil, "unknown"},
		{"recognised", &EventClient{Name: "claude-code", Version: "2.1.14"}, "claude-code/2.1.14"},
		{"no version", &EventClient{Name: "claude-code"}, "claude-code"},
		{"unrecognised falls back to raw", &EventClient{Raw: "SomeNewAgent/9.9"}, "SomeNewAgent/9.9"},
		// The zero value cannot name anything, so it must not invent a name. This is
		// the case the "unknown" label exists for: absence, never a parse failure
		// wearing a plausible-looking agent name.
		{"empty struct is unknown", &EventClient{}, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.c.Label(); got != tc.want {
				t.Errorf("Label() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestContextClientInfo_ParsesFromTheRequestHeaders(t *testing.T) {
	c := &Context{Headers: http.Header{}}
	c.Headers.Set("User-Agent", "claude-cli/2.1.14 (external, cli)")

	got := c.ClientInfo()

	if got == nil {
		t.Fatal("ClientInfo() = nil for a request carrying a User-Agent")
	}
	if got.Name != "claude-code" || got.Version != "2.1.14" {
		t.Errorf("ClientInfo() = %+v, want claude-code/2.1.14", got)
	}
}

func TestContextClientInfo_NilWhenNoUserAgent(t *testing.T) {
	c := &Context{Headers: http.Header{}}
	if got := c.ClientInfo(); got != nil {
		t.Errorf("ClientInfo() = %+v, want nil", got)
	}
}

func TestContextClientInfo_NilHeadersDoesNotPanic(t *testing.T) {
	// A Context built by a listener that never set Headers. This runs on the
	// request hot path; a nil map read is fine but a nil *Context is not, and the
	// event sites call this unconditionally.
	c := &Context{}
	if got := c.ClientInfo(); got != nil {
		t.Errorf("ClientInfo() = %+v, want nil", got)
	}
}

func TestContextClientInfo_MemoizesIncludingTheNilAnswer(t *testing.T) {
	// The subtle one. A nil result is a VALID memoized answer, so a bare nil check
	// as the memo guard would re-parse on every call for exactly the requests that
	// have nothing to parse — and every turn calls this at least twice, once per
	// event.
	c := &Context{Headers: http.Header{}}
	c.Headers.Set("User-Agent", "claude-cli/2.1.14")

	first := c.ClientInfo()
	// Mutating the header after the first call must not change the answer; if it
	// does, the value is being re-derived rather than memoized.
	c.Headers.Set("User-Agent", "something-else/9.9")
	second := c.ClientInfo()

	// VALUE EQUALITY, NOT POINTER IDENTITY, and the change is the point rather than an
	// accommodation. This asserted `first != second` on the pointers, which is the wrong
	// instrument for the property the test is named for: "memoized" is a claim about WHICH
	// ANSWER, and the pointer was standing in for it. It also pinned the aliasing bug —
	// ClientInfo handed out the memo itself, so ten recording sites shared one mutable
	// struct and a single write through it relabelled events already appended.
	if *first != *second {
		t.Errorf("ClientInfo() gave different answers across calls: %+v then %+v", *first, *second)
	}
	if second.Name != "claude-code" {
		t.Errorf("second call re-parsed the mutated header: %+v", second)
	}
	// And the copies really are distinct, which is the new half: a caller storing this on a
	// SessionEvent must not be able to reach any other event's label through it.
	if first == second {
		t.Error("ClientInfo() returned the memo itself: the label an event is attributed to " +
			"is then mutable by anyone holding another event's copy — see SnapshotClient")
	}
	first.Name = "impostor"
	if third := c.ClientInfo(); third.Name != "claude-code" {
		t.Errorf("writing through one caller's copy changed the memo: %+v", third)
	}
}

func TestContextClientInfo_MemoizesTheNilCase(t *testing.T) {
	c := &Context{Headers: http.Header{}}
	_ = c.ClientInfo() // nil
	c.Headers.Set("User-Agent", "claude-cli/2.1.14")
	if got := c.ClientInfo(); got != nil {
		t.Errorf("ClientInfo() = %+v after a memoized nil; want the nil to stick", got)
	}
}
