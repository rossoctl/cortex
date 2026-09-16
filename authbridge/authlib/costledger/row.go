// Package costledger persists per-minute cost and token totals to disk so a
// question like "what did today cost" survives a restart.
//
// It exists because the usage aggregator's ring is 6 hours of in-memory buckets
// (usage.NumBuckets x usage.BucketWidth) and dies with the process. A coding
// session spans days; the release bar asks for numbers that are still there
// tomorrow.
//
// It is a SECOND session.Recorder, registered alongside the usage aggregator,
// rather than a reader of it. The aggregator keeps independent marginals —
// by-model, by-endpoint, by-plugin — not a joint distribution, so reading it
// could never reconstruct "this endpoint x this model x this provenance", and
// summing marginals would double-count. Recording independently also means this
// package cannot regress charting.
//
// TWO HALVES, ONE ANSWER. Closed minutes live on disk; the minute still
// accumulating lives in the Writer. Window returns both, and that is the only
// entry point a reader should use — see its doc for how non-overlap is enforced.
//
// The open minute comes from THIS package's accumulator and not from the usage
// aggregator's ring, though the ring also holds it. Four reasons, because an
// earlier draft of this doc claimed the ring and it would have been wrong:
//
//   - The ring prices independently. usage.Aggregator.costOf falls back to the
//     process rate table where this package does not (see the divergence paragraph
//     below), so a total assembled from both would have one minute priced by one
//     rule and the rest by another — an internally inconsistent figure, which is
//     harder to explain than either rule on its own.
//   - The ring's request denominator is different. It counts every response
//     carrying a Host, including MCP and health traffic; this package counts
//     inference only. Requests would jump for exactly one minute of the window.
//   - The ring is 6 hours. A minute held while the proxy sits idle overnight has
//     rotated out of the ring, but is still in the map right here.
//   - Ownership would become a timing question. The ring knows nothing about what
//     has been flushed, so any periodic flush could put a minute in both halves.
//     Reading the open minute from the writer that owns it makes the boundary exact
//     and provable rather than probable.
//
// It prices NOTHING. Every figure here is the one authlib/costing settled and
// inference-parser published on the event; this package only decodes and adds.
// That is the invariant cortex #972 exists to protect: one component turns
// tokens into dollars.
//
// CONSEQUENCES of pricing nothing, stated because they are real differences a reader
// will otherwise discover by comparing two totals on one screen. THREE of them, not
// one — the first is about dollars and the other two about the denominator those
// dollars are a fraction of:
//
//   - usage.Aggregator.costOf falls back to the process rate table for a request that
//     arrives with no settled record, and this package does not. Where that fallback
//     fires, /v1/usage over a ring window reports dollars the ledger records as
//     priceable-but-unpriced. On the live pipeline inference-parser settles every
//     inference response, so the two agree; a composition without it would show the
//     gap rather than a wrong number, which is the failure mode to prefer.
//
//   - PriceableRequests is counted differently. costOf sets priceable for ANY request
//     carrying a settled cost record, whatever its token counts, while the writer here
//     requires Model != "" and Tokens > 0. A settled record over zero tokens — a
//     gateway that reported a cost and no usage — therefore lands in the ring's
//     coverage denominator and not in the ledger's. It is priced in both, so the
//     dollars match and only the ratio differs.
//
//   - A REFUSED FIGURE is counted as priceable here and nowhere in the ring. A record
//     carrying costevent.RejectedImplausible says a cost was on the wire and this proxy
//     declined it, and the writer records that as priceable-and-unpriced so the coverage
//     gap survives to tomorrow. usage.Aggregator.costOf reaches the record through
//     costevent.Decode, which reports nothing at all for an unpriced one, so the ring
//     counts the response in Requests and in no other counter. The dollars are zero in
//     both and only the ratio differs — the same shape as the bullet above, in the
//     opposite direction. It is deliberate: the refusal exists to keep a coverage gap
//     nameable, and this is the surface that is still there tomorrow to name it on.
//
//   - The token fields read here are the modern ones only. pricing.UsageFromInference
//     still falls back to InferenceExtension.PromptTokens and CompletionTokens when the
//     split counters are absent, so a producer emitting only the legacy pair yields a
//     ring figure with usage and a ledger row with Tokens == 0 — which then fails the
//     priceable test above. Not fixed by adding the legacy fields to Row: the schema
//     rule at the bottom of this doc is add-never-rename, and adding two columns for a
//     shape parsercommon.Fill no longer produces would put them on every future row.
//     The right fix is upstream, where the legacy pair is normalised into the split.
//
// On disk it holds hosts, model names, counts, dollars and timestamps. No prompt
// content, no completions, no tool arguments — ever. That is a user-facing promise
// and TestWriter_HoldsNoPromptContent asserts it against the serialized bytes.
//
// SCHEMA STABILITY: Row embeds usage.Counts, so a field added there changes what
// lands on disk with no edit here. That is the point — one vocabulary — but it
// means the ledger's JSON is only as stable as Counts'. Additive changes keep old
// files readable, because an absent field decodes to its zero; a RENAME would
// silently read every historical row as zero for that column. Add, never rename.
package costledger

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// Row is one minute's totals for one (endpoint, model, agent, provenance) key.
//
// usage.Counts is EMBEDDED rather than restated so the ledger, /v1/usage and any
// future collector share one vocabulary: a field added there appears here, and a
// divergence is a compile error instead of a review question. It also means
// Counts.Add is the summation for both — including IncompleteRequests, which is
// how the "this total is a floor, not an exact figure" caveat survives a restart
// instead of being the one thing a persisted total silently loses.
type Row struct {
	At time.Time `json:"at"`
	// Endpoint is the target host, taken from SessionEvent.Host.
	//
	// Named endpoint rather than host because that is what it MEANS to a reader of
	// a cost row: half of the rate key, since the same model bills differently on a
	// discounted gateway than on the vendor endpoint. The rename from the event's
	// field name is deliberate and matches usage.GroupEndpoint, which reads the
	// same field under the same name.
	Endpoint string `json:"endpoint,omitempty"`
	Model    string `json:"model,omitempty"`
	// Agent identifies the calling coding agent, as "name/version" —
	// "claude-code/2.1.14" — or as its raw User-Agent when the parser did not
	// recognise it. Populated from pipeline.EventClient.Label; see Writer.Record.
	//
	// CLIENT-ASSERTED AND TRIVIALLY SPOOFABLE, because it is derived from a request
	// header: a cost-attribution and display key, never an authorization subject.
	// Nothing may branch on it.
	//
	// EMPTY means the request carried no User-Agent. Stored as "" rather than as the
	// display string "unknown" deliberately, and this is the one representation choice
	// in this file a reader joining the ledger to /v1/usage has to know about:
	//
	//   - This is a DURABLE file. Writing "unknown" into it would permanently destroy
	//     the difference between "no agent was recorded" and "an agent that reported
	//     itself as unknown", for every future reader of every retained day. "" plus
	//     omitempty is lossless and costs no bytes.
	//   - The live aggregator's series uses "unknown" instead, because its keys are
	//     display strings a client renders directly, where a "" key is a blank row that
	//     reads as a rendering bug.
	//   - labelFor maps "" to "unknown" at the query boundary, so group=agent answers
	//     IDENTICALLY whether it was served from the ring or from this file. The two
	//     sources differ in representation and agree in meaning; nothing a client sees
	//     differs.
	Agent string `json:"agent,omitempty"`
	// Provenance is part of the KEY, not a summary of the row: one minute can mix a
	// gateway's own figures with modelled ones, and a single provenance per row
	// would have to pick a winner. Keying on it keeps /v1/usage's pricedBy
	// reconstructible from the ledger.
	//
	// EXPORTED BECAUSE encoding/json REQUIRES IT, like every other field here, and not
	// because a caller reads it: nothing outside this package does today. Unexporting it
	// would drop the column from every persisted row and from the accumulation key with
	// it, which is the opposite of tidying up. See Fold for what is served from it.
	Provenance string `json:"provenance,omitempty"`

	usage.Counts
}

// key is the in-memory accumulation key. Mirrors the JSON identity fields exactly.
type key struct {
	endpoint, model, agent, provenance string
}

func (r Row) key() key {
	return key{r.Endpoint, r.Model, r.Agent, r.Provenance}
}

// maxLabelLen bounds one label ON DISK, and it is the same 96 bytes
// usage.maxLabelLen applies to the same two request-controlled fields — the value
// is matched deliberately rather than chosen again, so a model name is spelled
// identically in the ring and in the ledger and group=model answers the same from
// either source.
//
// Load-bearing here for a reason the ring does not have: these strings are WRITTEN
// TO AN APPEND-ONLY FILE that is retained for retentionDays. Model comes straight
// off the parsed request body, so a workload chooses its length. Uncapped, one
// request with a model name longer than maxLineBytes produced a line readDay cannot
// buffer and cannot step over, which ENDED that day's read at that offset — every
// row appended after it, for the rest of the day, unreadable from then on, on every
// future read, invisible in the API's totals and unfixable because the file is
// append-only. Measured: $3.00 of a $3.25 day gone to one 1 MiB model name.
//
// So the cap is on the WRITE side, where it is absolute. Four labels at 96 bytes
// plus a timestamp and the numeric counters put the longest line this package can
// emit at well under a kilobyte, three orders of magnitude below maxLineBytes —
// which is what turns readDay's over-long-line guard from a live failure mode into
// the last-resort guard for a file some other process corrupted.
// TestRecord_LabelsAreCappedSoALineCanNeverExceedTheReadLimit pins the arithmetic.
const maxLabelLen = 96

// truncateLabel caps one label at maxLabelLen BYTES, cut on a RUNE boundary.
//
// THE CAP STAYS IN BYTES because bytes are what it bounds: the length of a line in an
// append-only file, and through that the reachability of readDay's unskippable-line
// path. Cutting on a rune boundary moves where that cap lands by at most three bytes
// and does not weaken it.
//
// IT USED TO BE A PLAIN BYTE CUT, which halved whatever rune straddled byte 96 and left
// an invalid UTF-8 sequence in a DURABLE file that other tools parse — and it did so
// most readily on exactly the labels this package rewrites, since sanitizeLabel emits
// 3-byte U+FFFD runes and rowLabel cuts afterwards. The old comment defended the byte
// cut on the grounds that a model name is "effectively always ASCII" and that one
// truncation rule beats two; the first is a caller's choice rather than a fact, and the
// second is now satisfied the other way round — pipeline.capUA cuts on a rune boundary
// too.
//
// usage.truncateLabel is the remaining byte cut, on the ring's in-memory copies of the
// same labels. Named rather than silently diverged from: it is a different package's
// file, its strings die with the process, and the fix belongs with its owner.
func truncateLabel(s string) string {
	if len(s) <= maxLabelLen {
		return s
	}
	cut := maxLabelLen
	// Walk back off a continuation byte; at most three steps, since no UTF-8 sequence
	// is longer than four bytes.
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// sanitizeLabel replaces C0 controls, DEL and C1 controls with U+FFFD.
//
// These strings are request- or upstream-controlled — Endpoint is the host the
// workload asked for and Model comes straight off the parsed request body — and they
// are written to a DURABLE file that an operator cats and that other tools parse. An
// escape sequence in a model name repositions the cursor, recolours the pane or erases
// the line that reports it, for every future read of a file that is retained for
// retentionDays and cannot be edited. CWE-150.
//
// REPLACED, NOT DROPPED, so tampering is visible instead of collapsing into a
// plausible-looking label: "m\x1b[31mx" reads as "m�[31mx" rather than as "m[31mx",
// which nobody would question.
//
// C1 IS INCLUDED — U+0080–U+009F — and leaving it out was the gap this comment used to
// describe as complete. U+009B is the single-character CSI: a terminal decoding UTF-8
// acts on it exactly as it acts on ESC [, so "2J" clears the pane with no ESC byte in
// the label at all. C1 encodes as 0xC2 0x80–0xC2 0x9F, so nothing in it is below 0x20
// and the byte scan this function used to gate on stepped straight past it. An invalid
// byte is replaced too, so the label held in memory, the label served, and the label
// encoding/json would have written are one string rather than three.
//
// SHARED RULE, THREE COPIES, AND THE FIRST TWO MUST STAY IDENTICAL:
//
//   - pipeline.sanitizeUA is the PRIMARY guard for the Agent label. It sits in
//     ParseUserAgent, where a User-Agent header becomes a value, so it covers the live
//     usage aggregator as well as this file — one choke point for both consumers of
//     EventClient.Label rather than one fix per consumer.
//   - THIS one is the primary guard for Endpoint, Model and Provenance, which never pass
//     through that parser, and defence in depth for Agent. It stays for that reason
//     rather than out of habit: it is the copy in front of a file that is retained for
//     retentionDays and cannot be edited afterwards, so it is the one that has to hold
//     even if a future edit weakens the other.
//   - abctl's tui.sanitizeLabel is a third copy, at RENDER time, in a main module this
//     library must not import. It still filters C0 and DEL only — named here because a
//     divergence stated is a divergence someone can fix.
//
// Two copies of a five-line rule beats a dependency edge the wrong way round; two copies
// with different rules is the thing to avoid, so a change to either of the first two
// belongs in both.
//
// NOT a JSON-integrity guard — encoding/json escapes control bytes, so an unsanitised
// label could never split a line or break readDay. Every consumer downstream of the
// decode is what this protects.
//
// The one consequence worth stating: usage (the ring) does not sanitise, so a label
// carrying control bytes is now spelled differently in the two halves and group=model
// would show it as two series. That only happens for a label that is already hostile,
// the ring's copy dies with the process, and the ledger's is the one that survives —
// so the divergence is the right way round. The matching fix belongs in usage.
func sanitizeLabel(s string) string {
	if !hasControlRunes(s) {
		// The overwhelmingly common case, and no allocation for it: this runs on the
		// session-append path, under the store's write lock.
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if isControlRune(r) || (r == utf8.RuneError && size == 1) {
			b.WriteRune('\uFFFD')
			i += size
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String()
}

// hasControlRunes reports whether s carries anything sanitizeLabel would replace.
//
// A RUNE scan, and it has to be one. A byte scan is exact for C0 and DEL \u2014 no
// continuation byte of a multi-byte UTF-8 sequence is below 0x80, so a byte below 0x20
// can only be that character itself \u2014 and blind to C1, which lives entirely above 0x80.
// That blindness is what let U+009B through. The tests read this as the "did anything
// hostile reach disk" predicate, so it has to name the same set the rewrite replaces.
//
// An INVALID byte reports true (RuneError at size 1 is the decoder saying "this is not
// UTF-8"); a legitimately encoded U+FFFD does not, because it decodes at size 3 \u2014 so an
// already-sanitised label does not read as still hostile.
func hasControlRunes(s string) bool {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if isControlRune(r) || (r == utf8.RuneError && size == 1) {
			return true
		}
		i += size
	}
	return false
}

// isControlRune reports whether r is a C0 control, DEL, a C1 control, or a rune that
// rewrites or hides the text around it without being a control character at all.
//
// The one predicate the scan and the rewrite both read, so they cannot disagree about
// what a control character is. pipeline.isControlRune has the same clauses in the same
// order; see sanitizeLabel for why there are two of them, and note that BOTH copies move
// together — that file's doc says so in as many words, and this is what honouring it
// looks like. There is a THIRD copy in cmd/abctl/tui/usage_render.go which is still C0+DEL
// only; it belongs to the abctl PR and is named here so the divergence is recorded rather
// than discovered.
//
// The bidi and zero-width clause is the C1 argument applied to the runes that reasoning
// missed. C1 is here because U+009B opens a terminal escape sequence; a bidi override needs
// no escape sequence and no terminal to make one label render as another's name, and a
// zero-width rune makes two distinct keys look identical in a table a reader is comparing.
// Same class, same choke point. See pipeline.isControlRune for the full argument.
func isControlRune(r rune) bool {
	if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
		return true
	}
	switch r {
	case // Bidi overrides and isolates: reorder the glyphs around them.
		'\u202a', '\u202b', '\u202c', '\u202d', '\u202e',
		'\u2066', '\u2067', '\u2068', '\u2069',
		// Zero-width: make two distinct labels render identically.
		'\u200b', '\u200c', '\u200d', '\u2060', '\ufeff':
		return true
	}
	return false
}

// rowLabel prepares one string for a durable row: sanitised, then capped.
//
// THE ORDER MATTERS. Sanitising can triple a string's length — every replaced byte
// becomes three — so capping first would let a label of 96 control bytes reach 288 on
// disk and break the line-length arithmetic maxLabelLen exists to guarantee. Capping
// last used to be able to split a U+FFFD in half and put an invalid UTF-8 fragment in a
// durable row; truncateLabel now cuts on a rune boundary, so this order costs nothing
// and the byte bound still holds exactly.
func rowLabel(s string) string {
	return truncateLabel(sanitizeLabel(s))
}

// maxLabelsPerMinute caps how many DISTINCT rows one open minute accumulates, and
// it is usage.maxLabelsPerBucket's 64, again matched rather than re-chosen.
//
// TIGHTER THAN THE RING'S, and knowingly: usage applies 64 per axis, to independent
// marginals, while the key here is the (endpoint, model, agent, provenance) JOINT
// tuple, so 64 bounds the product rather than each factor. That is the bound this
// package needs, because the map is not the only cost — every entry is also copied
// into a batch by takeLocked on a minute roll, which happens INSIDE
// session.Store.Append's write lock, and it becomes a durable line. Unbounded, a
// caller varying the model string per request grew both: 50,000 keys held for one
// minute at 200 bytes each was measured, with an O(N) walk and allocation in front
// of the request that closed the minute.
//
// FOLDED, NOT DROPPED, past the cap — see overflowKey. A deployment busy enough to
// exceed 64 joint labels in one minute loses attribution DETAIL and no dollars,
// which is the right direction; raising this one constant is the fix if a real
// deployment ever does.
const maxLabelsPerMinute = 64

// overflowLabel collects everything past maxLabelsPerMinute. usage.overflowLabel's
// spelling, so a client that renders both sources shows one "(other)" band and not
// two.
//
// NOT A REAL LABEL, and no more spoofable-looking than the axes it replaces: a
// caller can send "(other)" as its model and land in this row, exactly as it can
// send another agent's name. The cap is a memory bound, not an authorization
// boundary.
const overflowLabel = "(other)"

// overflowKey is the one reserved accumulator slot every label past the cap folds
// into.
//
// ALL FOUR fields, not just the request-controlled two: the key is a tuple, so
// keeping any real field would let the overflow row multiply on that axis and defeat
// the bound it exists to enforce. The consequence is that a capped minute reports its
// excess as "(other)" on every axis at once, which reads as "cardinality was capped
// here" rather than as a plausible endpoint that spent money.
var overflowKey = key{overflowLabel, overflowLabel, overflowLabel, overflowLabel}

// overflow rewrites a row's identity onto overflowKey, keeping At and every counter.
// The dollars are unchanged; only the attribution is coarsened.
func overflow(r Row) Row {
	r.Endpoint, r.Model, r.Agent, r.Provenance = overflowLabel, overflowLabel, overflowLabel, overflowLabel
	return r
}
