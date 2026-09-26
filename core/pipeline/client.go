package pipeline

import (
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
	// sanitizeUA) and capped at maxClientLen.
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
// (`costledger`, which does not exist yet). Both arrive later in this series, so
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

// sanitizeUA replaces every control character in a User-Agent with U+FFFD.
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
func sanitizeUA(s string) string {
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

// hasControlRunes reports whether s carries anything sanitizeUA would replace.
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
// EXPORTED, AND NOW THE ONLY COPY. usage and costledger each held a byte-identical duplicate under a
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
// through costledger the length of an appended line. Cutting on a rune boundary only
// changes WHERE the byte cap lands (by up to three bytes), never that there is one.
//
// It matters because of the order in ParseUserAgent: sanitizeUA runs first and can put
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
// cmd/abctl/toolscan/known.go hardcodes its built-in tool names, so the
// tool-prune analysis only works for it — and OpenCode, Codex and the rest arrive
// with their own detection work rather than a speculative entry here. A guess
// that is wrong is worse than an unrecognised agent, because an unrecognised one
// still shows up under its Raw value and can be identified from the breakdown,
// whereas a mis-mapped one is silently filed under someone else's name.
//
// Keys are the token BEFORE the slash, lowercased. Claude Code sends
// "claude-cli/<version> (external, cli)"; the product token is "claude-cli",
// which is why the key is not the canonical name.
var knownClients = map[string]string{
	"claude-cli": "claude-code",
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
	// bound. See sanitizeUA for what the first step removes and maxClientLen for what the
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
	// sanitizeUA turns it into U+FFFD and the split below then finds no whitespace at all.
	// Measured without this: "claude-cli/2.1\t(external)" yielded Version
	// "2.1�(external)", a byAgent series key of its own, so one agent's spend divided
	// in two.
	//
	// NORMALISATION, NOT SANITISATION, which is why it sits on this side of sanitizeUA
	// instead of inside it. Mapping legal whitespace onto the canonical whitespace is not
	// the same job as neutralising what a terminal would act on. The rule inside sanitizeUA is
	// shared rather than merely kept identical — every surface calls IsControlRune now — so this
	// side only has to leave it alone. Byte-length-neutral, so the cap arithmetic is untouched.
	ua = capUA(sanitizeUA(strings.ReplaceAll(ua, "\t", " ")))
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
		c.Version = version
	}
	return c
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
// method will join it to a name and hand it on, which is the second reason costledger
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
