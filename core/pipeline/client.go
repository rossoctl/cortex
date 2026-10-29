package pipeline

import (
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// EventClient identifies the coding agent that made a request, parsed from its
// User-Agent header.
//
// A DISPLAY AXIS, NOT A SECURITY BOUNDARY. The User-Agent is a request header, so
// this value is CLIENT-ASSERTED AND TRIVIALLY SPOOFABLE: it is an observability
// and cost-attribution key, never an authorization subject. Any client can claim
// to be any agent at any version. Nothing here may ever be used for an
// authorization decision, a rate-limit exemption, a pricing tier, or any other
// choice whose wrong answer costs something. What this is for is attribution in a
// cost breakdown, where a caller lying about itself mis-attributes that caller's
// own spend and nothing else.
//
// That is the same caveat Context.Session carries about client-asserted session ids,
// deliberately in the same words: the same property of a different field.
//
// NOT TO BE CONFUSED WITH Identity / EventIdentity, which sit beside it on the
// same context and the same event. Those are the AUTHENTICATED auth principal,
// established by an auth plugin and nil until one runs. This is a self-reported
// software label. The two answer different questions — "who is calling" versus
// "what program is calling" — and the proximity of the fields is the reason this
// paragraph exists: a reader who treats one as the other has built authorization
// on a request header.
//
// Nil means NO User-Agent was sent. That is distinct from a User-Agent that was
// sent but matched no known agent, which produces a non-nil value carrying only
// Raw. The distinction is load-bearing for #952, which has to tell an honest zero
// from missing data: an unrecognised agent we can still NAME is data, one we
// cannot is not.
type EventClient struct {
	// Name is the canonical agent name — "claude-code" — or empty when the
	// User-Agent matched nothing in knownClients. Empty is not a failure; see Raw.
	Name string `json:"name,omitempty"`
	// Version is the version string that followed the product token, or empty when
	// the agent was not recognised or sent no version.
	Version string `json:"version,omitempty"`
	// Raw is the User-Agent as it was sent, with tabs normalised to spaces, sanitised (see
	// SanitizeControlRunes) and capped at maxClientLen.
	//
	// "Verbatim" up to those three rules, which is as verbatim as a string that reaches a
	// terminal and a durable file can be. Worth knowing before comparing this against a
	// header captured somewhere else: a tab in the original is a space here, because RFC
	// 9110 allows HTAB as the separator and ParseUserAgent normalises it rather than letting
	// the sanitiser turn it into U+FFFD.
	//
	// Kept ALONGSIDE Name rather than only when parsing fails, so a new coding
	// agent appears in the breakdown the day someone runs it instead of after a
	// parser update ships — and so the detection work for the other agents can see
	// what strings to expect rather than guessing them.
	Raw string `json:"raw,omitempty"`
}

// maxClientLen bounds the retained User-Agent.
//
// Same reasoning as usage.maxLabelLen, and it is the load-bearing bound here
// because this value comes off a request header: its length and its cardinality
// are both chosen off-host. Label() feeds bucket label maps that free a slot only
// a full ring lap later (6h at one minute), so without a cap a caller parks
// arbitrary bytes in this process's memory, from off-host, on the synchronous
// session-append path. The cap is applied BEFORE anything is retained or matched,
// so no field on EventClient — Raw, Version, or the string Label() builds — can
// exceed it by more than the recognised name it is joined to.
//
// FORWARD REFERENCES BELOW. This lands ahead of the two packages that consume
// Label(): the usage aggregator's byAgent series and the durable cost ledger
// (`cost/ledger`, which does not exist yet). Both arrive later in this series, so
// `ledger.*` names in this file are the shape the rule takes there rather than
// symbols that resolve today. `usage.*` names do resolve.
//
// 128 is well beyond any real agent's product token and version. Cardinality is
// bounded separately, by usage.maxLabelsPerBucket, which applies to byAgent
// exactly as it does to every other label map.
//
// A BYTE cap cut on a RUNE boundary — see capUA. The bound has to be in bytes,
// because what it bounds is retained memory and the length of a persisted line; the
// cut has to be on a rune boundary, because the sanitiser below can put multi-byte
// U+FFFD runes in this string and half of one is invalid UTF-8 in a file other tools
// parse. The ledger's truncateLabel will apply the same rule to the same class of
// string for the same reason.
//
// usage.truncateLabel is a plain byte cut TODAY, and 128 here against its 96 is what would
// make that reachable: a User-Agent between 97 and 128 bytes survives capUA whole and is
// then byte-cut when it becomes a byAgent key, which can split one of this file's own
// 3-byte U+FFFD runes and leave an invalid fragment as the in-memory key while
// encoding/json substitutes on the way out — a label that is broken in the ring and clean
// on the wire. Neither half is reachable before the aggregate work later in this series,
// which is what wires byAgent AND makes usage.truncateLabel cut on a rune boundary. Should
// that order ever change, cap here at 96 rather than leaving the window open.
const maxClientLen = 128

// SanitizeControlRunes replaces every control character in s with U+FFFD. It
// is the rule that lived under sanitizeUA's name until #1539's CodeQL finding
// made the honest name necessary: it applies to any client-controlled string
// headed for a surface a terminal or a file renders, not only a User-Agent.
// Exported for the listeners — the forward proxy logs the destination Host on
// its egress-guard refusal paths, and a newline forged into that header would
// forge a log entry, the same CWE-150 shape the UA path guards against.
//
// THE PRIMARY CHOKE POINT FOR THIS STRING. ParseUserAgent is the one place a header
// becomes an EventClient, and Label() is what both consumers of the result read — the
// live usage aggregator and, later in this series, the durable cost ledger. Sanitising
// per consumer is strictly worse: whichever one does it neutralises the bytes on its own
// surface while the other serves them intact, and the next consumer added starts out
// unguarded too.
//
// THE HEADER IS NOT ALREADY CLEAN, and the reason it looked clean is worth stating
// because it is not a property of this package. On the forward proxy net/http rejects
// control bytes in a header value, so a hostile UA never reaches here through that
// listener — protection by accident, and only there. THE EXT_PROC PATH HAS NO SUCH
// PARSER: header values arrive from Envoy as protobuf bytes (HeaderValue.RawValue) and
// are handed over as-is, so on that path this function is the only bound on what a
// client can put in a string that ends up on a terminal and in a 30-day file. CWE-150.
//
// C0, DEL AND C1 — all three, because a C0-only filter is the version of this that
// looks right and is not. U+009B is the single-character CSI: a terminal decoding
// UTF-8 treats it exactly as it treats ESC [, so "\u009b2J" clears the screen with no
// ESC byte anywhere in the string. The C1 block is U+0080–U+009F and encodes as
// 0xC2 0x80–0xC2 0x9F, so nothing below 0x20 appears in it and a byte scan for control
// bytes steps straight past it.
//
// A BYTE THAT IS NOT VALID UTF-8 is replaced too. It is the same character in a pane
// that is not in UTF-8 mode (a lone 0x9B is CSI in Latin-1), and encoding/json would
// substitute U+FFFD for it on the way to disk regardless — doing it here means the
// label held in memory, the label served from /v1/usage and the label on disk are one
// string rather than three.
//
// REPLACED, NOT DROPPED, so tampering stays visible: "claude-cli/1\x1b[31m" reads as
// "claude-cli/1�[31m" rather than as "claude-cli/1[31m", which nobody would question.
//
// THE RULE WILL BE SHARED WITH THE LEDGER'S sanitizeLabel, AND THE TWO MUST STAY
// IDENTICAL. That copy is not redundant: it is the primary guard for Endpoint, Model
// and Provenance, which never pass through here, and it is defence in depth for this
// one on a file that cannot be edited after the fact. Two copies of a five-line rule
// is the right trade for a durable file; two DIFFERENT rules is not, so a change to
// either belongs in both.
func SanitizeControlRunes(s string) string {
	if !hasControlRunes(s) {
		// The overwhelmingly common case, and it must not allocate: this runs on the
		// request path, twice per turn.
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if IsControlRune(r) || (r == utf8.RuneError && size == 1) {
			b.WriteRune('\uFFFD')
			i += size
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String()
}

// hasControlRunes reports whether s carries anything SanitizeControlRunes would replace.
//
// A RUNE scan rather than a byte scan, because the C1 block cannot be seen from the
// bytes alone: 0xC2 0x9B is U+009B, and 0x9B on its own is a continuation byte of
// nothing. Allocation-free either way, and the strings are header-length.
//
// An INVALID byte reports true (RuneError at size 1 is the decoder saying "this is not
// UTF-8"); a legitimately encoded U+FFFD does not, because it decodes at size 3.
func hasControlRunes(s string) bool {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if IsControlRune(r) || (r == utf8.RuneError && size == 1) {
			return true
		}
		i += size
	}
	return false
}

// isControlRune reports whether r is a C0 control, DEL, a C1 control, or a rune that
// rewrites or hides the text around it without being a control character at all.
//
// The one predicate both the scan and the rewrite read, so they cannot disagree about
// what a control character is. The ledger's copy will carry the same clauses in the same
// order.
//
// THE FOURTH CLAUSE IS THE SAME ARGUMENT AS THE THIRD, applied to runes that need no escape
// sequence at all. C1 is here because U+009B opens one in a terminal, so a User-Agent could
// paint over a chart; a bidi override does the same job in any renderer, with no terminal
// involved — U+202E makes a label display right-to-left, so "claude-code/2.1.14" can be made
// to read as another agent's name in the column beside real spend, and a zero-width joiner or
// space hides the difference between two keys a reader is comparing. Display-spoofing runes
// reaching a 30-day file and a chart through a self-reported header are the same class.
//
// NOT A GENERAL UNICODE POLICY, and deliberately narrow: the members named are the ones with
// no legitimate use in a product token, and non-compliant clients are the interesting ones.
//
// ONE MEMBER IS REACHABLE BY A COMPLIANT CLIENT, and it is worth naming rather than implying
// otherwise: RFC 9110's User-Agent separator is RWS = 1*( SP / HTAB ), so a tab is legal
// there. It is still replaced here like any other C0 control — ParseUserAgent normalises tabs
// to spaces before this runs, so the legal-whitespace reading is honoured.
//
// EXPORTED, AND NOW THE ONLY COPY. usage and cost/ledger each held a byte-identical duplicate under a
// comment asserting "THE THREE COPIES MOVE TOGETHER" — which nothing enforced, and only this one had
// test coverage for the bidi marks. Both import this package, so there was never a reason for the
// other two to exist; a shared predicate is what makes the identical-rule rule true instead of
// aspirational.
func IsControlRune(r rune) bool {
	if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
		return true
	}
	switch r {
	case // Bidi overrides and isolates: reorder the glyphs around them.
		'\u202a', '\u202b', '\u202c', '\u202d', '\u202e',
		'\u2066', '\u2067', '\u2068', '\u2069',
		// Bidi MARKS, which are the same class and strictly easier to use: a mark needs no
		// matching pop, so one LRM reorders the neutral characters around it on its own.
		// U+200E LRM, U+200F RLM, U+061C ALM.
		'\u200e', '\u200f', '\u061c',
		// Zero-width: make two distinct labels render identically.
		'\u200b', '\u200c', '\u200d', '\u2060', '\ufeff':
		return true
	}
	return false
}

// capUA cuts s to at most maxClientLen BYTES, on a rune boundary.
//
// The cap stays in bytes because that is what it bounds — retained memory here, and
// through cost/ledger the length of an appended line. Cutting on a rune boundary only
// changes WHERE the byte cap lands (by up to three bytes), never that there is one.
//
// It matters because of the order in ParseUserAgent: SanitizeControlRunes runs first and can put
// 3-byte U+FFFD runes in this string, so a byte cut has a one-in-three chance of
// leaving a fragment of one — invalid UTF-8 in a label that goes to a durable file and
// to a chart, which is the failure the substitution was meant to avoid rather than
// cause.
func capUA(s string) string {
	if len(s) <= maxClientLen {
		return s
	}
	cut := maxClientLen
	// Walk back off a continuation byte. At most three steps: no UTF-8 sequence is
	// longer than four bytes.
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// knownClients maps a lowercased User-Agent product token to a canonical agent
// name.
//
// Deliberately small. Claude Code is the only agent with full support today —
// cmd/agentop/toolscan/known.go hardcodes its built-in tool names, so the
// tool-prune analysis only works for it — and Codex and the rest arrive
// with their own detection work rather than a speculative entry here. A guess
// that is wrong is worse than an unrecognised agent, because an unrecognised one
// still shows up under its Raw value and can be identified from the breakdown,
// whereas a mis-mapped one is silently filed under someone else's name.
//
// Keys are the token BEFORE the slash, lowercased. Claude Code sends
// "claude-cli/<version> (external, cli)"; the product token is "claude-cli",
// which is why the key is not the canonical name.
// Keys are matched against the FIRST product token, and — when that one is unknown —
// against the later ones, last to first. See ParseUserAgent's second rule and
// trailingKnownClient for which agents need that and why a map entry alone cannot express
// it.
var knownClients = map[string]string{
	"claude-cli": "claude-code",
	// THE SAME AGENT UNDER ITS OTHER PRODUCT TOKEN, which until #1363 was a value in this
	// map but never a key — so the lookup missed and ~13% of one capture's Claude Code
	// events pooled into agentop's catch-all Other row with their spend, while
	// IsKnownAgent said false for them wherever that gates. Claude Code sends both shapes
	// on live traffic, and they look like different COMPONENTS rather than
	// interactive-vs-headless, since both name claude-vscode and agent-sdk:
	//
	//	claude-cli/2.1.233 (external, claude-vscode, agent-sdk/0.3.233)
	//	claude-code/2.1.233 (claude-vscode, agent-sdk/0.3.233)
	//
	// Unlike claude-cli, this key IS the canonical name — the token Claude Code sends here
	// is already its product name, so there is no third spelling to invent.
	//
	// Captured 2026-10-13 from Claude Code 2.1.233. This does NOT claim the third shape,
	// "Claude-User (claude-code/…)"; see TestParseUserAgent_ClaudeUserIsNotClaimed and
	// AffinityName's doc comment for why that one stays unrecognised.
	"claude-code": "claude-code",
	// Bob Shell, the CLI — one of the two faces of the agent this repo folds under the
	// one canonical name "bob" (the other is the entry below). Its product token is
	// already its product name, but the canonical name is the SHORTER "bob" the binary
	// answers to, not "bob-shell": two products, one agent, one row in every per-agent
	// view — the same fold codex-mcp-client makes into "codex". It arrives with its own
	// detection work — inferenceparser parses its inference endpoint, its session header
	// is session.BobSessionHeader, and `agentop configure bob` sets it up — so this is a
	// supported agent rather than the speculative entry the paragraph above refuses.
	//
	// Earns its place in the FIRST-token map as well as the trailing scan because Bob
	// Shell sends the bare form too, on /admin/v1/profile and /inference/v1/model/info,
	// where bob-shell is the only token there is.
	//
	// "bob-shell" was this agent's canonical name until 2026-10-15; the spellings
	// already recorded — ledger rows, session labels, router pins — fold back to "bob"
	// through retiredAgents, which is what keeps the rename from splitting one agent's
	// spend into two rows beside each other.
	"bob-shell": "bob",
	// The IBM Bob IDE — the other face of the same agent: a VS Code fork with its own
	// settings file (`agentop configure bob` configures it and the shell function both)
	// and its own inference client. Canonical name "bob", the binary's name, shared
	// with the entry above; a canonical name must be one token, or AgentName cannot
	// fold its versions into one row.
	//
	// THE PRODUCT TOKEN IS "Bob", NOT "IBM". The IDE sends "IBM Bob/2.2.1", so the first
	// token is a bare "IBM" and the agent is the token after it — which is the trailing scan's
	// case, and the same one Bob Shell's inference client needs: the IDE's two inference
	// User-Agents are "ai-sdk/openai-compatible/… IBM Bob/2.2.1" and "ai/… IBM Bob/2.2.1".
	// Keying on "ibm" instead would claim every IBM product that names its vendor first, and
	// lose the version, which "IBM" does not carry. Captured 2026-10-01 from IBM Bob 2.2.1;
	// see #1210 for what leaving it unrecognised cost.
	//
	// NOT CLAIMED: the IDE's Electron shell ("Mozilla/5.0 … IBMBob/1.126.0+bob2.2.1 …") and
	// its update check ("Code/1.126.0+bob2.2.1 …"). Both carry the VS Code base version rather
	// than Bob's, and "code" is no agent's token. Neither carries inference.
	//
	// "ibm-bob" was this face's canonical name until 2026-10-15; recorded spellings fold
	// back to "bob" through retiredAgents.
	"bob": "bob",
	// OpenCode's one User-Agent, sent by its TUI, its background service and its inference
	// client alike: "opencode/<channel>/<version>/<client>", e.g. "opencode/latest/2.0.21/cli"
	// (captured from 2.0.21). The version is the field after the channel, not the first one —
	// see productVersion. It arrives with its own detection work: its session header is
	// session.SessionIDHeader, and `agentop configure opencode` sets it up.
	"opencode": "opencode",
	// Codex, which the paragraph above named as waiting for its own detection work. That work
	// is here: inferenceparser reads its Responses API dialect, and `agentop configure codex`
	// sets it up by writing ~/.codex/.env.
	//
	// THE PRODUCT TOKEN IS "codex_exec", WITH AN UNDERSCORE, not "codex" — which is why the key
	// is not the canonical name, as with claude-cli. Codex sends two forms of it, differing only
	// by a trailing "(codex_exec; <version>)" comment:
	//
	//	codex_exec/0.160.1 (Mac OS 26.3.0; arm64) unknown (codex_exec; 0.160.1)
	//	codex_exec/0.160.1 (Mac OS 26.3.0; arm64) unknown
	//
	// One entry covers both: the token is FIRST in each, so ParseUserAgent's first rule matches
	// and the trailing scan is never reached. Both carry the version where it is expected, so
	// productVersion needs no case for this agent. Captured 2026-10-08 from Codex 0.160.1 on
	// macOS, on /backend-api/codex/responses among others.
	"codex_exec": "codex",
	// Codex's MCP client, a separate product token on the same release, seen on
	// /backend-api/ps/mcp: "codex-mcp-client/0.160.1". Folded to the same canonical name
	// deliberately — it is one agent's component rather than a second agent, so its traffic
	// belongs in Codex's row and its session, the way OpenCode's service and TUI share one.
	"codex-mcp-client": "codex",
	// THE INTERACTIVE TUI, which is what `agentop configure codex enable` sends people to
	// ("run `codex` as usual"), so omitting these two covered the documented path and
	// missed the default one. Codex's User-Agent is "{originator}/{version} (…)", and the
	// originator is whatever client_name the app-server was initialized with: the TUI
	// passes "codex-tui", while anything sent BEFORE that initialize completes uses the
	// built-in default "codex_cli_rs". Both are Codex, so both fold here. Read off
	// Codex's source at rust-v0.160.1 (tui/src/lib.rs, app-server's
	// initialize_processor.rs, login/src/auth/default_client.rs), then CAPTURED
	// from a real TUI session driven through the proxy over a pty — including on
	// the TUI's own /backend-api/codex/responses inference requests, so this is
	// not inferred from source alone. That capture also carried 0.162.1 mid-
	// auto-update, which is the only evidence here that the shape holds across
	// releases rather than only on the build everything else was read from.
	"codex-tui":    "codex",
	"codex_cli_rs": "codex",
	// NOT CLAIMED, though the same binary carries them as originator names:
	// "codex_vscode", "codex_desktop", "codex-app-server", "codex_mcp_server",
	// "codex-cli". The first two plausibly belong to separate products the way IBM Bob
	// the IDE does to Bob Shell, and none of the five has been seen on live traffic or
	// traced to a client_name the way the four above have. An unrecognised agent still
	// shows under its raw User-Agent and can be added later; a wrong fold is silent.
	// NOT CLAIMED: "OTel-OTLP-Exporter-Rust/0.31.0", which Codex's telemetry sends to
	// ab.chatgpt.com/otlp/v1/metrics. The token names the generic Rust OTLP exporter, not
	// Codex, so claiming it would file every Rust program using that crate under Codex's
	// name — the mis-mapping this map's godoc calls worse than no answer. It carries no
	// inference either.
	//
	// Pi (pi.dev, earendil-works/pi): "pi/1.4.0 (darwin; node/v24.3.0; arm64)", captured
	// 2026-10-28 from Pi 1.4.0 on its /v1/chat/completions requests. Its "short" identity
	// mode sends a bare "pi", which matches too, with no version. Releases before 1.4.0
	// sent "pi (<platform> <release>; <arch>)", which also matches, as the bare token.
	// Its session header is session.SessionIDHeader, sent by the extension that `agentop
	// configure pi` installs.
	//
	// NOT CLAIMED: Pi signed in to an Anthropic subscription sends "claude-cli/<version>"
	// on that path (pi-ai's anthropic-messages.js), so that traffic files under Claude
	// Code. Telling the two apart needs a signal other than the User-Agent; see
	// docs/agents/pi.md.
	"pi": "pi",
}

// IsKnownAgent reports whether name is the canonical name of a coding agent ParseUserAgent
// recognises — every canonical name in knownClients — as AgentName folds a Label to it.
//
// FOR A CONSUMER THAT HAS TO TELL AN AGENT FROM A RAW User-Agent, which a label alone cannot
// say: "curl" is what AgentName answers for curl/8.4.0, and nothing in that string marks it
// as unrecognised. agentop's AGENTS pane pools every label this rejects under one row.
//
// False for UnknownClientLabel and for any label carrying a version: fold with AgentName first.
func IsKnownAgent(name string) bool {
	for _, known := range knownClients {
		if name == known {
			return true
		}
	}
	return false
}

// productVersion is the version in rest, what follows an agent's product token and its
// slash. For every agent but OpenCode that is all of rest. OpenCode puts a release channel
// first — "opencode/latest/2.0.21/cli" — so its version is the second field, and taking all
// of rest would label each release with its channel and client name too.
func productVersion(name, rest string) string {
	if name != "opencode" {
		return rest
	}
	if fields := strings.Split(rest, "/"); len(fields) >= 2 {
		return fields[1]
	}
	return rest
}

// ParseUserAgent derives an EventClient from a User-Agent header value.
//
// Returns nil when the header is absent or blank — absence, not a client named
// "unknown". Callers must not substitute a placeholder here: EventClient.Label()
// is where absence becomes the display string "unknown", and doing it at parse
// time would make a missing header indistinguishable from an agent that really
// called itself that.
//
// A value that IS present but matches no known agent returns non-nil with an
// empty Name and the (capped) header in Raw. That is an unrecognised agent, not
// an absent one, and Label() reports it under its raw value rather than folding
// it into "unknown" — otherwise every new coding agent would land in the same
// bucket as untagged traffic and the breakdown could not distinguish them.
func ParseUserAgent(ua string) *EventClient {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return nil
	}
	// SANITISED, THEN CAPPED, and both before ANY of it is retained or matched against,
	// so nothing derived from it downstream can carry a control character or exceed the
	// bound. See SanitizeControlRunes for what the first step removes and maxClientLen for what the
	// second one is for.
	//
	// THE ORDER IS LOAD-BEARING, in the same direction as ledger.rowLabel's: a
	// substitution can triple the string, so capping first would let 128 control bytes
	// become 384 in a label whose whole purpose is to be bounded. Capping last can land
	// mid-U+FFFD, which is why capUA cuts on a rune boundary. The transient cost of
	// sanitising before capping is a builder up to 3x the header — bounded by whatever
	// the listener already accepted as a header value, and paid only by a request that
	// sent control bytes in the first place.
	// TAB NORMALISED TO SPACE FIRST, ahead of the sanitiser, and it is not cosmetic. RFC
	// 9110 spells the separator RWS = 1*( SP / HTAB ), so a compliant client may separate
	// the product token from its comment with a tab — and a tab is a C0 control, so
	// SanitizeControlRunes turns it into U+FFFD and the split below then finds no whitespace at all.
	// Measured without this: "claude-cli/2.1\t(external)" yielded Version
	// "2.1�(external)", a byAgent series key of its own, so one agent's spend divided
	// in two.
	//
	// NORMALISATION, NOT SANITISATION, which is why it sits on this side of SanitizeControlRunes
	// instead of inside it. Mapping legal whitespace onto the canonical whitespace is not
	// the same job as neutralising what a terminal would act on. The rule inside SanitizeControlRunes is
	// shared rather than merely kept identical — every surface calls IsControlRune now — so this
	// side only has to leave it alone. Byte-length-neutral, so the cap arithmetic is untouched.
	ua = capUA(SanitizeControlRunes(strings.ReplaceAll(ua, "\t", " ")))
	c := &EventClient{Raw: ua}
	// The product token is the first SPACE-delimited word, so the trailing comment Claude
	// Code appends — "(external, cli)" — is ignored rather than having to be matched.
	// Parsed positionally rather than with a regexp: this runs twice per turn on the
	// request path, and the grammar being read is one slash in one word. Space alone is
	// enough because of the normalisation above; no tab reaches here.
	token := ua
	if i := strings.IndexByte(token, ' '); i >= 0 {
		token = token[:i]
	}
	product, version, _ := strings.Cut(token, "/")
	if name, ok := knownClients[strings.ToLower(product)]; ok {
		c.Name = name
		c.Version = productVersion(name, version)
		return c
	}
	// SECOND RULE, and a FALLBACK rather than an alternative: reached only when the first
	// token matched nothing. Some agents put a library's product token first and their own
	// last — bob-shell sends three User-Agents whose first tokens are `ai-sdk`, `ai` and
	// `bob-shell`, and only the last of those is the agent. Keyed on the first token alone,
	// one agent's spend divides into three series.
	//
	// It cannot be a knownClients entry instead. Two of those first tokens are a Vercel AI
	// SDK version, so claiming them would file every other program built on that SDK under
	// Bob's name — the mis-mapping knownClients' own godoc calls worse than no answer.
	//
	// ORDER IS LOAD-BEARING and this is why it runs second, not merged into one scan: a UA
	// carrying two known tokens must answer the same regardless of which end it is read
	// from, and "the first token, else the last one that matched" is a rule, where "the last
	// known token" would silently demote an agent that embedded another's name.
	if name, ver, ok := trailingKnownClient(ua); ok {
		c.Name = name
		c.Version = ver
	}
	return c
}

// trailingKnownClient scans the product tokens after the first, LAST TO FIRST, for a known
// agent, and reports the name and version of the one that matched.
//
// Last-to-first rather than first-to-last because the token being looked for is the agent's
// own and it sits at the END, behind however many libraries it wants to announce. Scanning
// forwards would find a library that later joins knownClients in preference to the agent
// that is actually calling.
//
// THE VERSION COMES FROM THE MATCHED TOKEN, not from the first one. Reading it off the front
// yields "openai-compatible/3.0.36" for Bob's inference client — a plausible-looking string
// that is not the agent's version, and worse than an empty one because nothing downstream
// can tell it is wrong.
//
// Allocates, via strings.Fields, on a path whose first rule deliberately does not. Paid only
// by a request whose first token is unrecognised, so the common case — claude-cli, which
// matches on the first rule and returns above — never reaches here. Starts at the LAST index
// and stops before index 0, since the caller has already tried that one.
func trailingKnownClient(ua string) (name, version string, ok bool) {
	fields := strings.Fields(ua)
	for i := len(fields) - 1; i >= 1; i-- {
		product, ver, _ := strings.Cut(fields[i], "/")
		if n, found := knownClients[strings.ToLower(product)]; found {
			return n, productVersion(n, ver), true
		}
	}
	return "", "", false
}

// UnknownClientLabel is the reserved key for traffic that carried no User-Agent.
//
// EXPORTED so there is exactly one definition of it. Both surfaces that serve
// group=agent have to agree on this string: the live aggregator keys its series on
// Label() directly, while the cost ledger stores absence losslessly as "" and maps it
// back at the query boundary (see ledger.labelFor). Those are two different code
// paths reaching the same bucket, so two spellings would surface as two rows in any
// client that merged a ring answer with a ledger answer — and each row would hold half the
// unattributed spend, which is worse than either alone. A matching literal in both packages
// would agree only by the comment saying it must; this makes it the compiler's to keep.
//
// NOT an agent name. See Label for what it means and why a consumer must not present
// it as one.
const UnknownClientLabel = "unknown"

// Label returns the display key for this client: "claude-code/2.1.14" for a
// recognised agent, the raw User-Agent for an unrecognised one, and
// UnknownClientLabel ("unknown") when there is no client at all.
//
// NIL-SAFE ON PURPOSE. Every consumer — the usage aggregator, the cost ledger —
// calls this on events that may carry no client, and a nil check at each call site
// is how one of them eventually gets forgotten. A forgotten one is not a panic
// caught in review either: it is a blank key in a breakdown table, which reads as
// a rendering bug rather than as unattributed traffic.
//
// "unknown" means THIS EVENT CARRIED NO USER-AGENT. It is a RESERVED BUCKET, not
// an agent name, and a consumer must not present it as one: a row labelled
// "unknown" in a cost breakdown is unattributed traffic, not a program that spent
// money. It is never the answer for a User-Agent that WAS sent and could not be
// recognised — that case answers with the raw value, so it stays nameable and does
// not pool with untagged traffic. The empty struct answers "unknown" for the same
// reason: it can name nothing, and inventing a name for it would put a value in a
// cost table that no request ever sent.
//
// CARRIES NO CONTROL CHARACTERS when the EventClient came from ParseUserAgent, which is the
// only path a request takes — a property of the parser, not of this method. A hand-built
// EventClient (a test, a future producer) can hold anything its author put there and this
// method will join it to a name and hand it on, which is the second reason cost/ledger
// sanitises again before writing a row: the guarantee here covers one code path, and the file
// is forever.
//
// A caller that sends literally "User-Agent: unknown" does land in that reserved
// bucket, and that collision is not defended against. Reserving the word would buy
// nothing: the axis is spoofable by construction, so the same caller could just as
// well claim "claude-cli/2.1.14" and land in a real agent's row instead. Anything
// that must not be spoofable belongs on Identity. Recorded here so the collision is
// a known property rather than a surprise to whoever first sees the row.
func (c *EventClient) Label() string {
	if c == nil {
		return UnknownClientLabel
	}
	if c.Name == "" {
		if c.Raw == "" {
			return UnknownClientLabel
		}
		return c.Raw
	}
	if c.Version == "" {
		return c.Name
	}
	return c.Name + "/" + c.Version
}

// versionedAgent matches a label that is exactly one name/version token.
var versionedAgent = regexp.MustCompile(`^([^\s/]+)/[^\s/]+$`)

// retiredAgents maps a canonical name this package no longer produces to the name
// that replaced it.
//
// A rename of a canonical agent name would otherwise split every per-agent figure in
// two: the ledger, the session store and the router's pins all hold labels written
// under the old spelling, and AgentName is the one seam all of them fold through on
// read — so folding the old spellings HERE is what keeps "bob-shell/2.2.1" and
// "bob/2.2.1" one row instead of two beside each other.
//
// EXACT MATCHES ONLY. An unrecognised agent's label is its raw User-Agent, which can
// be anything; a retired name must therefore be a whole label's or a stripped
// name's exact match, never a substring, or the fold would claim raw agents that
// merely contain the word.
//
// Kept FOREVER rather than cleaned up once the last pre-rename row expires: the cost
// is two map entries, and the ledger keeps rows far longer than any one retention
// window. Removing an entry is the same split-the-history bug the map exists to
// prevent, rediscovered later.
var retiredAgents = map[string]string{
	// Bob's two faces had two canonical names until 2026-10-15, when they converged
	// on the binary's name. See the two knownClients entries above.
	"bob-shell": "bob",
	"ibm-bob":   "bob",
}

// CanonicalAgent is the canonical agent name for name, folding the spellings a
// previous release produced to the name this one does. It is the identity function
// on every current name, which is what lets callers apply it to values they stored
// earlier without first having to know whether a rename ever happened.
//
// Takes a NAME, not a label: fold a label with AgentName first, or hand it a name a
// config file named (the inference router's `agents:` keys and stored pins are both
// canonicalized through this), never a raw User-Agent.
func CanonicalAgent(name string) string {
	if n, ok := retiredAgents[name]; ok {
		return n
	}
	return name
}

// FoldAgentNames is agents keyed by canonical agent name: every key folded through
// CanonicalAgent, so a map written before an agent-name rename — a router config's
// `agents:`, pinned under "bob-shell" before it converged into "bob" — names the
// agent requests report today.
//
// THE ONE IMPLEMENTATION OF THE MAP-LEVEL FOLD, shared by every reader and writer of
// such a map: the inference router keys its routing table with it, and agentop reads
// and writes the same `agents:` map through it, so the two cannot disagree about
// which spelling a config names — a disagreement that made `server reset` report
// "not routed" for an agent the proxy was routing.
//
// Sorted order with set-if-absent, so a config naming both a retired spelling and its
// replacement resolves deterministically whichever way the map iterates: whichever
// key sorts first wins. Deliberately the same rule on both sides rather than a
// "current beats retired" preference, because agreeing is the property that matters
// and this rule needs no second list of which spelling is which.
func FoldAgentNames(agents map[string]string) map[string]string {
	out := make(map[string]string, len(agents))
	for _, name := range slices.Sorted(maps.Keys(agents)) {
		canonical := CanonicalAgent(name)
		if _, exists := out[canonical]; !exists {
			out[canonical] = agents[name]
		}
	}
	return out
}

// AgentSpellings is name and every retired spelling that folds to it, sorted: for
// "bob", ["bob", "bob-shell", "ibm-bob"]. For every other name, just the name.
//
// FOR WRITERS THAT MUST CLEAR EVERY WAY A CONFIG MIGHT NAME THE AGENT: agentop's
// server commands remove each of an agent's spellings from a router config's
// agents: map, so a retired spelling cannot sit beside the canonical key and fold
// back into routing once the key is gone. Read-side callers want FoldAgentNames or
// CanonicalAgent instead.
func AgentSpellings(name string) []string {
	name = CanonicalAgent(name)
	out := []string{name}
	for retired, canonical := range retiredAgents {
		if canonical == name && retired != name {
			out = append(out, retired)
		}
	}
	slices.Sort(out)
	return out
}

// AgentName is the agent a Label names, with its version dropped: "claude-code/2.1.285" is
// "claude-code". It is the key every per-agent figure is filed under — the usage ring's agent
// axis, the ledger's on read, a session's agent, and /v1/usage?agent= — so an agent's releases
// are one row rather than one row each. The ledger stores the full Label; only reads fold.
//
// ONLY A WHOLE-LABEL name/version TOKEN FOLDS. An unrecognised agent's label is its raw
// User-Agent, and cutting "Claude-User (claude-code/2.1.284; …)" at its first slash would name a
// row after half a comment, so anything with a space or a second slash is returned unchanged.
// A single-token one does fold — "curl/8.4.0" is "curl" — which is the same rule, not an
// exception to it.
//
// RETIRED SPELLINGS FOLD TOO, through CanonicalAgent: a label recorded under a name this
// package no longer produces answers that name's replacement, so a rename does not split
// history. "bob-shell/2.2.1" and "ibm-bob/2.2.1" are both "bob".
func AgentName(label string) string {
	if m := versionedAgent.FindStringSubmatch(label); m != nil {
		return CanonicalAgent(m[1])
	}
	// A label with no version — Label() omits it when an agent's User-Agent carried none —
	// is the bare name, retired or current, so it folds through the same map.
	return CanonicalAgent(label)
}

// AffinityName is the coding agent a request belongs to for SESSION ATTRIBUTION: Name when
// the User-Agent was recognised, else a known agent's canonical name found in any token —
// comments included — else "". See session.Store.SessionForClient for what it decides.
//
// WIDER THAN Name, AND ONLY HERE. Claude Code's WebFetch sends
// "Claude-User (claude-code/2.1.284; …)", naming its agent only inside the comment the parser
// deliberately skips. Reading comments into Name would move those rows to a different agent in
// the ledger and the AGENTS pane than the history beside them; for attribution the only
// question is which agent's session a header-less call joins, and there the comment is right.
//
// Canonical names, not knownClients' keys: the comment says "claude-code", while the key is
// the product token "claude-cli". Nil-safe, like Label.
func (c *EventClient) AffinityName() string {
	if c == nil {
		return ""
	}
	if c.Name != "" {
		return c.Name
	}
	for _, f := range strings.Fields(c.Raw) {
		product, _, _ := strings.Cut(strings.Trim(f, "();,"), "/")
		product = strings.ToLower(product)
		for _, name := range knownClients {
			if product == name {
				return name
			}
		}
	}
	return ""
}
