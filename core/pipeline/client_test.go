package pipeline

import (
	"fmt"
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
		// RFC 9110 spells the separator RWS = 1*( SP / HTAB ), so a tab is legal here and has
		// to yield the same agent and version a space does. Without ParseUserAgent's
		// normalisation the tab is a C0 control, sanitizeUA replaces it with U+FFFD, nothing
		// splits, and Version comes back as "2.1.14�(external, cli)" — the same agent in a
		// byAgent series key of its own, with its spend divided between the two.
		{"tab-separated comment (RFC 9110 RWS)", "claude-cli/2.1.14\t(external, cli)", "claude-code", "2.1.14", false},
		// INTERNAL, past TrimSpace's reach. A TRAILING tab is stripped before the
		// normalisation runs, so a row ending in one passes whether or not the
		// normalisation exists — it looks like coverage and is not.
		{"tab inside the value", "claude-cli/2.1.14\tx", "claude-code", "2.1.14", false},
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

// Bob Shell is identified by the LAST product token in its User-Agent, not the first.
//
// This is the one identification rule knownClients cannot express, and the reason is in the
// three values below rather than in any preference: bob-shell sends the SAME agent under
// three different first tokens, and only one of them is its own. Keyed on the first token,
// one agent's spend divides into three series — one per raw User-Agent — and no map entry
// closes that, because two of the three first tokens (`ai-sdk`, `ai`) are a Vercel AI SDK
// version, not an agent. Claiming those for Bob would file every other program built on the
// same SDK under Bob's name.
//
// CAPTURED 2026-09-25 from bob-shell 2.0.5 against api.us-east.bob.ibm.com, verbatim. The
// version is asserted too: it has to come from the token that MATCHED, and reading it off
// the first token instead yields "openai-compatible/3.0.36" or "7.0.16" — a plausible-looking
// string that is not Bob's version, which is worse than an empty one.
func TestParseUserAgent_BobShellIsIdentifiedByItsTrailingProductToken(t *testing.T) {
	for _, tc := range []struct {
		name, ua string
	}{
		// The inference client — the only one of the three that carries token spend.
		{"inference client", "ai-sdk/openai-compatible/3.0.36 ai-sdk/provider-utils/5.0.29 runtime/node.js/24 bob-shell/2.0.5"},
		// Same agent, different SDK entry point. Nothing but the first token differs.
		{"secondary client", "ai/7.0.16 ai-sdk/provider-utils/5.0.29 runtime/node.js/24 bob-shell/2.0.5"},
		// The bare form, sent on /admin/v1/profile and /inference/v1/model/info. This is the
		// ONE of the three a plain knownClients entry would already fix, which is why it is
		// here: without it, a first-token-only implementation passes two thirds of this test
		// and the rule looks less necessary than it is.
		{"bare control", "bob-shell/2.0.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseUserAgent(tc.ua)
			if got == nil {
				t.Fatalf("ParseUserAgent(%q) = nil, want a client", tc.ua)
			}
			if got.Name != "bob-shell" {
				t.Errorf("Name = %q, want %q", got.Name, "bob-shell")
			}
			if got.Version != "2.0.5" {
				t.Errorf("Version = %q, want %q", got.Version, "2.0.5")
			}
			// Label is the assertion that matters downstream: it is what the usage
			// aggregator keys its series on and what ledger.Row.Agent stores, so this is
			// the field that decides whether three User-Agents are one row or three.
			if want := "bob-shell/2.0.5"; got.Label() != want {
				t.Errorf("Label() = %q, want %q — this is the series key, so a mismatch here IS the fragmentation", got.Label(), want)
			}
		})
	}
}

// A User-Agent whose FIRST token is already known is never re-resolved by a later one.
//
// The trailing scan is a FALLBACK, and this is the test that keeps it one. Without the
// ordering, the rule becomes "last known token wins" — and then a UA carrying two known
// tokens answers differently depending on which end you read from, which is a coin flip
// dressed as a rule. Both orders are asserted because a single order passes under either
// implementation.
//
// Neither value is traffic anyone has seen; they are constructed precisely because the
// ordering is not otherwise reachable from real input. That is the point — the rule has to
// be decided here rather than by whichever agent first happens to embed another's token.
func TestParseUserAgent_AKnownFirstTokenIsNotReResolvedByALaterToken(t *testing.T) {
	for _, tc := range []struct {
		name, ua, wantName, wantVer string
	}{
		{"claude first, bob last", "claude-cli/2.1.270 bob-shell/2.0.5", "claude-code", "2.1.270"},
		{"bob first, claude last", "bob-shell/2.0.5 claude-cli/2.1.270", "bob-shell", "2.0.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseUserAgent(tc.ua)
			if got == nil {
				t.Fatalf("ParseUserAgent(%q) = nil, want a client", tc.ua)
			}
			if got.Name != tc.wantName {
				t.Errorf("Name = %q, want %q — the first token must win", got.Name, tc.wantName)
			}
			if got.Version != tc.wantVer {
				t.Errorf("Version = %q, want %q", got.Version, tc.wantVer)
			}
		})
	}
}

// The trailing scan reads LAST to FIRST, and this is the only test that can tell.
//
// Worth stating why it needs a constructed value. On all three of Bob's real User-Agents a
// first-to-last scan returns the same answer as a last-to-first one, because bob-shell is
// the only known token in any of them — so the documented direction is invisible to every
// realistic fixture, and a scan written the other way round would ship green. Two known
// tokens after an unknown first one is the smallest input that distinguishes them.
//
// The direction matters beyond the pin: the agent's own token sits at the END, behind
// however many libraries it announces. Should a library's token ever join knownClients —
// `ai-sdk` is one plausible future entry, since it is a real product — a forwards scan would
// start answering "ai-sdk" for every agent built on it, silently reassigning Bob's spend to
// its SDK. Backwards, the agent keeps its own name.
func TestParseUserAgent_TheTrailingScanReadsLastToFirst(t *testing.T) {
	const ua = "ai-sdk/openai-compatible/3.0.36 claude-cli/2.1.270 bob-shell/2.0.5"
	got := ParseUserAgent(ua)
	if got == nil {
		t.Fatalf("ParseUserAgent(%q) = nil, want a client", ua)
	}
	if got.Name != "bob-shell" {
		t.Errorf("Name = %q, want %q — the scan must read backwards, so the LAST known token wins once the first token is unknown", got.Name, "bob-shell")
	}
	if got.Version != "2.0.5" {
		t.Errorf("Version = %q, want %q — the version must come from the token that matched", got.Version, "2.0.5")
	}
}

// The IBM Bob IDE is one agent under every User-Agent it sends inference or account traffic
// with, and the version is Bob's, not its SDK's.
//
// CAPTURED 2026-10-01 from IBM Bob 2.2.1 against api.us-east.bob.ibm.com, verbatim (#1210).
// Unrecognised, these were three AGENTS rows — and the one reading "IBM Bob/2.2.1" carried
// only the /admin/v1 calls, so it showed 0 tokens while the inference sat under two AI-SDK
// rows nobody would read as Bob.
func TestParseUserAgent_IBMBobIDEIsOneAgent(t *testing.T) {
	for _, tc := range []struct {
		name, ua string
	}{
		{"inference client", "ai-sdk/openai-compatible/3.0.36 ai-sdk/provider-utils/5.0.29 runtime/node.js/v24.15.0 IBM Bob/2.2.1"},
		{"secondary client", "ai/7.0.16 ai-sdk/provider-utils/5.0.29 runtime/node.js/v24.15.0 IBM Bob/2.2.1"},
		// /admin/v1/profile and /admin/v1/teams/…: the vendor first, as a bare token.
		{"bare form", "IBM Bob/2.2.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseUserAgent(tc.ua)
			if got == nil {
				t.Fatalf("ParseUserAgent(%q) = nil, want a client", tc.ua)
			}
			if got.Name != "ibm-bob" || got.Version != "2.2.1" {
				t.Errorf("Name, Version = %q, %q, want %q, %q", got.Name, got.Version, "ibm-bob", "2.2.1")
			}
			if got.AffinityName() != "ibm-bob" {
				t.Errorf("AffinityName() = %q, want ibm-bob — without it the session is no agent's", got.AffinityName())
			}
			if want := "ibm-bob"; AgentName(got.Label()) != want {
				t.Errorf("AgentName(Label()) = %q, want %q — this is the AGENTS row", AgentName(got.Label()), want)
			}
		})
	}
}

// The IDE's Electron shell and its update check stay unrecognised: both carry the VS Code base
// version where Bob's would go, and neither sends inference.
func TestParseUserAgent_IBMBobIDEShellIsNotClaimed(t *testing.T) {
	for _, ua := range []string{
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) IBMBob/1.126.0+bob2.2.1 Chrome/148.0.7778",
		"Code/1.126.0+bob2.2.1 Darwin/24.6.0",
	} {
		if got := ParseUserAgent(ua); got.Name != "" {
			t.Errorf("ParseUserAgent(%q).Name = %q, want unrecognised", ua, got.Name)
		}
	}
}

// Codex's two product tokens both fold to one canonical name, and its telemetry exporter
// stays unrecognised. All four strings captured live from Codex 0.160.1 on macOS.
//
// The two codex_exec forms differ only by a trailing "(codex_exec; <version>)" comment, so
// this pins that the comment changes nothing: the token is first in both, which is the
// parser's first rule, and the version comes off that token rather than out of the comment.
func TestParseUserAgent_Codex(t *testing.T) {
	for _, tc := range []struct{ ua, name, version string }{
		{"codex_exec/0.160.1 (Mac OS 26.3.0; arm64) unknown (codex_exec; 0.160.1)", "codex", "0.160.1"},
		{"codex_exec/0.160.1 (Mac OS 26.3.0; arm64) unknown", "codex", "0.160.1"},
		// Codex's MCP client: a different token, the same agent, so the same row.
		{"codex-mcp-client/0.160.1", "codex", "0.160.1"},
		// Its telemetry exporter names the generic Rust OTLP crate, not Codex. Claiming it
		// would file every Rust program using that crate under Codex's name.
		{"OTel-OTLP-Exporter-Rust/0.31.0", "", ""},
	} {
		got := ParseUserAgent(tc.ua)
		if got == nil {
			t.Fatalf("ParseUserAgent(%q) = nil, want a client", tc.ua)
		}
		if got.Name != tc.name || got.Version != tc.version {
			t.Errorf("ParseUserAgent(%q) = {Name: %q, Version: %q}, want {%q, %q}",
				tc.ua, got.Name, got.Version, tc.name, tc.version)
		}
	}
}

// Session attribution follows the recognised name rather than the comment scan for Codex:
// AffinityName short-circuits on Name, so the "(codex_exec; 0.160.1)" comment — whose token
// is the map KEY and not the canonical name — never has to match for this to work.
func TestAffinityName_CodexUsesTheRecognisedName(t *testing.T) {
	c := ParseUserAgent("codex_exec/0.160.1 (Mac OS 26.3.0; arm64) unknown (codex_exec; 0.160.1)")
	if got := c.AffinityName(); got != "codex" {
		t.Errorf("AffinityName() = %q, want codex", got)
	}
}

// IsKnownAgent answers for canonical names only: what AgentName folds a recognised Label to.
func TestIsKnownAgent(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"claude-code", true},
		{"bob-shell", true},
		{"ibm-bob", true},
		{"opencode", true},
		{"codex", true},
		// A product token is not a canonical name: claude-cli is how Claude Code is RECOGNISED.
		{"claude-cli", false},
		{"bob", false},
		{"codex_exec", false},
		{"codex-mcp-client", false},
		// Versioned labels have to be folded first; the godoc says so.
		{"claude-code/2.1.285", false},
		{"curl", false},
		{UnknownClientLabel, false},
		{"(other)", false},
		{"", false},
	} {
		if got := IsKnownAgent(tc.name); got != tc.want {
			t.Errorf("IsKnownAgent(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
	// Every value ParseUserAgent can put in Name is one this accepts, so a new knownClients
	// entry cannot be recognised by the parser and rejected here.
	for product, name := range knownClients {
		if !IsKnownAgent(name) {
			t.Errorf("knownClients[%q] = %q, which IsKnownAgent rejects", product, name)
		}
	}
}

// OpenCode sends one User-Agent from its TUI, its background service and its inference
// client alike, with a release channel before the version. Captured from OpenCode 2.0.21.
func TestParseUserAgent_OpenCodeVersionIsTheFieldAfterItsChannel(t *testing.T) {
	for _, tc := range []struct{ ua, version string }{
		{"opencode/latest/2.0.21/cli", "2.0.21"},
		{"opencode/beta/2.1.0-beta.3/tui", "2.1.0-beta.3"},
		{"opencode/2.0.21", "2.0.21"},
	} {
		got := ParseUserAgent(tc.ua)
		if got == nil || got.Name != "opencode" || got.Version != tc.version {
			t.Errorf("ParseUserAgent(%q) = %+v, want opencode %s", tc.ua, got, tc.version)
		}
	}
}

// Every other agent's version is all of what follows its product token's slash.
func TestParseUserAgent_OnlyOpenCodeHasAChannelField(t *testing.T) {
	if got := ParseUserAgent("bob-shell/2.0.5/extra"); got.Version != "2.0.5/extra" {
		t.Errorf("bob-shell version = %q, want the whole rest", got.Version)
	}
}

// An unrecognised User-Agent with several tokens still reports no name.
//
// The trailing scan must not turn "nothing matched" into a guess. curl/8.4.0 is already
// covered as a single token in TestParseUserAgent; the multi-token case is the one the new
// loop makes reachable, and a loop that fell through to "last token" rather than "last token
// that MATCHED" would name this agent "node.js" — filing unrelated traffic under a program
// name, which is the failure knownClients' own godoc calls worse than no answer at all.
func TestParseUserAgent_TheTrailingScanNeverGuessesAnUnknownAgent(t *testing.T) {
	const ua = "some-tool/1.2 runtime/node.js/24 libcurl/8.4.0"
	got := ParseUserAgent(ua)
	if got == nil {
		t.Fatalf("ParseUserAgent(%q) = nil, want a client", ua)
	}
	if got.Name != "" {
		t.Errorf("Name = %q, want empty — no token matched, so nothing may be claimed", got.Name)
	}
	if got.Version != "" {
		t.Errorf("Version = %q, want empty", got.Version)
	}
	// Unrecognised reports under its raw value, so it stays nameable from a breakdown
	// instead of pooling with untagged traffic. See EventClient.Label.
	if got.Label() != ua {
		t.Errorf("Label() = %q, want the raw UA %q", got.Label(), ua)
	}
}

// A tab is NORMALISED to a space, not replaced with U+FFFD.
//
// The distinction is the whole point of doing it before sanitizeUA rather than inside it: a
// tab is legal whitespace in this header (RFC 9110's RWS), so the honest answer is the
// canonical whitespace rather than a substitution mark. Asserted on Raw, which is the field
// that reaches a durable row and a chart — a U+FFFD here would mean the two spellings of one
// agent stay distinct everywhere downstream.
func TestParseUserAgent_ATabIsNormalisedNotSubstituted(t *testing.T) {
	c := ParseUserAgent("claude-cli/2.1.14\t(external, cli)")
	if c == nil {
		t.Fatal("ParseUserAgent returned nil for a UA that was sent")
	}
	if want := "claude-cli/2.1.14 (external, cli)"; c.Raw != want {
		t.Errorf("Raw = %q, want %q — a tab is legal whitespace here, so it is normalised "+
			"rather than marked as tampering", c.Raw, want)
	}
	if strings.ContainsRune(c.Raw, '\uFFFD') {
		t.Errorf("Raw = %q carries U+FFFD; the tab was substituted instead of normalised, so "+
			"this caller keeps a byAgent key of its own", c.Raw)
	}
	// And the label a consumer keys on is identical to the space-separated spelling's.
	if space := ParseUserAgent("claude-cli/2.1.14 (external, cli)"); space == nil || c.Label() != space.Label() {
		t.Errorf("Label() = %q, want it identical to the space-separated spelling's", c.Label())
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
	// The bound deliberately does NOT carry len(got.Name) on its right-hand side: with it,
	// this reduces to `len(Version) > maxClientLen`, which is the assertion directly above.
	// maxClientLen*2 is the honest ceiling for a joined "name/version" — every component is
	// separately capped — and it is what would catch a future field escaping the cap.
	if len(got.Label()) > 2*maxClientLen {
		t.Errorf("Label() is %d bytes, want <= %d: the cap has to bound every DERIVED string, "+
			"not only the fields it is applied to", len(got.Label()), 2*maxClientLen)
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
//
// THE INPUT HAS TO PUT A RUNE ACROSS BYTE 128, which an even-width rune at an even cap never
// does: 2-byte runes all start at even offsets, so byte 128 is always a rune start, the
// walk-back never executes, and `return s[:maxClientLen]` passes every assertion. Both rows
// below straddle it, and each names the byte the cut must land on — an exact figure rather
// than `<= maxClientLen`, because the loose form is also what a cap loosened to a rune count
// would satisfy.
func TestParseUserAgent_CapCutsOnARuneBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		ua   string
		// wantLen is where the walk-back has to stop: the largest rune boundary at or below
		// maxClientLen for this input.
		wantLen int
	}{
		// One ASCII byte shifts the 2-byte runes onto odd offsets, so byte 128 is a
		// continuation byte and the cut walks back one to 127.
		{"2-byte runes behind one ASCII byte", "x" + strings.Repeat("é", 70), maxClientLen - 1},
		// 3-byte runes: 42 of them end at 126, the 43rd spans 126..128, so the cut walks
		// back two to 126.
		{"3-byte runes", strings.Repeat("✓", 60), maxClientLen - 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := ParseUserAgent(tc.ua)
			if c == nil {
				t.Fatal("ParseUserAgent returned nil")
			}
			if !utf8.ValidString(c.Raw) {
				t.Errorf("Raw = %q is not valid UTF-8; the cut split a rune in half", c.Raw)
			}
			if len(c.Raw) != tc.wantLen {
				t.Errorf("Raw is %d bytes, want exactly %d — the cut must land on the last rune "+
					"boundary at or below the %d-byte cap, neither splitting a rune nor "+
					"loosening the cap to a rune count", len(c.Raw), tc.wantLen, maxClientLen)
			}
		})
	}
	// And the ordinary case still cuts exactly at the cap when the boundary allows it, so
	// the walk-back is not silently shortening every label.
	if c := ParseUserAgent(strings.Repeat("é", maxClientLen/2+8)); c == nil {
		t.Fatal("ParseUserAgent returned nil")
	} else if len(c.Raw) != maxClientLen {
		t.Errorf("Raw is %d bytes for an even-width input, want exactly %d", len(c.Raw), maxClientLen)
	}
}

// EVERY MEMBER OF isControlRune's SWITCH, with a literal expectation for each.
//
// The predicate cannot be its own oracle: hasControlRunes calls isControlRune, so a loop
// asserting !hasControlRunes(got) stays green when a rune is deleted from the switch —
// verified by deleting U+200C, which no test in this package noticed. A member missing from
// the switch is a rune that reaches a durable row and a chart intact, so each one is pinned
// against the exact string it must become.
//
// A NEW MEMBER NEEDS A ROW HERE. That is the point of the literal want: this list is the only
// thing standing between the switch and a silent regression.
func TestParseUserAgent_EveryControlRuneIsReplaced(t *testing.T) {
	for _, r := range []rune{
		// Bidi overrides and isolates.
		'\u202a', '\u202b', '\u202c', '\u202d', '\u202e',
		'\u2066', '\u2067', '\u2068', '\u2069',
		// Bidi marks.
		'\u200e', '\u200f', '\u061c',
		// Zero-width.
		'\u200b', '\u200c', '\u200d', '\u2060', '\ufeff',
		// C0, DEL and C1, which the range test above the switch covers.
		'\x00', '\t', '\n', '\r', '\x1b', '\x7f', '\u0085', '\u009b',
	} {
		t.Run(fmt.Sprintf("U+%04X", r), func(t *testing.T) {
			// MID-STRING, PAST TrimSpace's REACH. ParseUserAgent trims leading and trailing
			// whitespace first, and \n, \r and U+0085 are all Unicode space — so a row that
			// appended the rune would assert nothing whatever for those three, however the
			// switch reads. Round 3 of this review had exactly that defect with a trailing tab.
			c := ParseUserAgent("newagent/1" + string(r) + "x")
			if c == nil {
				t.Fatal("ParseUserAgent returned nil for a UA that was sent")
			}
			// A tab is NORMALISED to a space rather than replaced — see
			// TestParseUserAgent_ATabIsNormalisedNotSubstituted — so it is the one member
			// whose literal answer is not the replacement character.
			want := "newagent/1\uFFFDx"
			if r == '\t' {
				want = "newagent/1 x"
			}
			if c.Raw != want {
				t.Errorf("Raw = %q, want %q: U+%04X reaches a durable row and a chart intact, "+
					"so it is either in the switch or it is not filtered at all", c.Raw, want, r)
			}
		})
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

	// VALUE EQUALITY, NOT POINTER IDENTITY. "Memoized" is a claim about WHICH ANSWER, so a
	// pointer comparison is the wrong instrument for it — and asserting pointer identity
	// here would pin the aliasing that ClientInfo deliberately avoids: every caller sharing
	// one mutable struct, where a single write relabels events already appended.
	// Guarded before dereferencing: nil is a legitimate answer from this method (see
	// TestContextClientInfo_NilWhenNoUserAgent), so the regression where the memo starts
	// answering nil for a UA that WAS sent would panic the test binary — taking the rest of
	// the package's output with it — instead of failing this assertion.
	if first == nil || second == nil {
		t.Fatalf("ClientInfo() returned nil for a User-Agent that was sent: first=%v second=%v",
			first, second)
	}
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
			"is then mutable by anyone holding another event's copy — see snapshotClient")
	}
	first.Name = "impostor"
	third := c.ClientInfo()
	if third == nil {
		t.Fatal("ClientInfo() returned nil after a caller wrote through its own copy")
	}
	if third.Name != "claude-code" {
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

// TestAffinityName pins which coding agent a request is filed with when it carries no
// session header. The Claude-User row is the reason this is not just Name: Claude Code's
// WebFetch announces itself only inside a comment, which the parser deliberately does
// not read.
func TestAffinityName(t *testing.T) {
	cases := []struct {
		ua   string
		want string
	}{
		{"claude-cli/2.1.284 (external, cli)", "claude-code"},
		{"Claude-User (claude-code/2.1.284; +https://support.anthropic.com/)", "claude-code"},
		{"ai-sdk/5.0.1 openai-compatible/3.0.36 bob-shell/2.0.5", "bob-shell"},
		{"bob-shell/2.0.5", "bob-shell"},
		{"Go-http-client/1.1", ""},
		{"python-httpx/0.27.0", ""},
		{"axios/1.15.2", ""},
		{"node", ""},
		// A comment naming a product that is not a coding agent claims nothing.
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)", ""},
	}
	for _, tc := range cases {
		if got := ParseUserAgent(tc.ua).AffinityName(); got != tc.want {
			t.Errorf("AffinityName(%q) = %q, want %q", tc.ua, got, tc.want)
		}
	}
	if got := (*EventClient)(nil).AffinityName(); got != "" {
		t.Errorf("nil client: AffinityName() = %q, want \"\"", got)
	}
}

// TestAffinityName_LeavesTheLabelAlone pins the other half: reading claude-code out of
// the Claude-User comment is for session attribution ONLY. Label is the ledger's agent key
// and the AGENTS pane's row name, and changing it would re-file new cost rows under a
// different agent than the history beside them.
func TestAffinityName_LeavesTheLabelAlone(t *testing.T) {
	const ua = "Claude-User (claude-code/2.1.284; +https://support.anthropic.com/)"
	c := ParseUserAgent(ua)
	if c.Name != "" || c.Label() != ua {
		t.Fatalf("ParseUserAgent(%q) = {Name:%q Label:%q}, want an unrecognised client labelled by its raw value",
			ua, c.Name, c.Label())
	}
}

// An agent's releases are one agent: the version is dropped wherever the label is a single
// name/version token, and nothing else is touched — a multi-token User-Agent is not a name and
// a version, so cutting it at its first slash would name a row after half of a comment.
func TestAgentName(t *testing.T) {
	cases := []struct{ label, want string }{
		{"claude-code/2.1.285", "claude-code"},
		{"bob-shell/2.0.5", "bob-shell"},
		{"claude-code", "claude-code"},
		{"curl/8.4.0", "curl"},
		{UnknownClientLabel, UnknownClientLabel},
		{"Claude-User (claude-code/2.1.284; +https://support.anthropic.com/)",
			"Claude-User (claude-code/2.1.284; +https://support.anthropic.com/)"},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)"},
		{"ai-sdk/5.0.1 openai-compatible/3.0.36", "ai-sdk/5.0.1 openai-compatible/3.0.36"},
		{"a/b/c", "a/b/c"},
		{"/2.1", "/2.1"},
		{"claude-code/", "claude-code/"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := AgentName(tc.label); got != tc.want {
			t.Errorf("AgentName(%q) = %q, want %q", tc.label, got, tc.want)
		}
	}
}
