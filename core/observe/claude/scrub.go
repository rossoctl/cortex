package claude

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxCwdLen caps the cwd fallback, in RUNES.
//
// Deliberately much larger than MaxTitleLen: this tier exists to show a directory, whose leaf is the
// identifying part, so clipping it to a title's budget would defeat it. It is not a display budget —
// the renderer truncates to the column anyway — it is a BOUND, so that one field in a transcript
// cannot hand the renderer an arbitrarily long string. Linux caps a path at 4096 bytes; this is
// comfortably above any real one and still finite.
const MaxCwdLen = 1024

// clipCwd caps s at MaxCwdLen runes, keeping the TAIL.
//
// The tail, not the head, because a path's distinguishing end is its leaf and that is the end the
// renderer keeps too. A plain rune slice is safe for the same reason it is in clipTitle: normalizeTitle
// has already removed every rune that binds to its neighbour.
func clipCwd(s string) string {
	r := []rune(s)
	if len(r) <= MaxCwdLen {
		return s
	}
	return strings.TrimSpace(string(r[len(r)-MaxCwdLen:]))
}

// MaxTitleLen caps a harvested title, in RUNES.
//
// EXPORTED so the renderer's tests can hold the cross-module contract: the cap is only safe because
// every renderer re-truncates by display width, and while it was package-private neither side could
// name the other's half. cmd/agentop/tui asserts the relationship against this constant.
//
// A prompt is unbounded — the longest on the measured tree ran to several KB — and a title is a
// table cell. Clipping at the source keeps the metadata file small and stops every consumer having
// to defend itself; the viewer truncates again to whatever the column allows, which is narrower
// still. Runes, not bytes, so a multi-byte prompt is not cut mid-character.
//
// NOT A DISPLAY-COLUMN BUDGET. 80 runes of CJK occupy 160 columns, so nothing may treat this as a
// width. It is safe only because every renderer re-truncates by display width — the sessions pane
// measures with lipgloss.Width.
//
// THAT IS NOT THE ONLY RULER THE CELL MEETS, and saying only "the pane measures with lipgloss.Width"
// was incomplete: bubbles v1.0.0 applies runewidth.Truncate to every cell before styling it, and
// runewidth is NOT ANSI-aware. Today that is harmless, because a title cell carries no escape bytes —
// this package guarantees it, and the pane asserts it. But the two facts are load-bearing together: if
// a title were ever styled, the escape bytes would be charged against the column budget and an
// 11-column cell would collapse to a lone ellipsis. Anyone adding colour to a title needs to know that
// before they do it, which is why it is recorded here rather than left to be rediscovered.
//
// THAT RELATIONSHIP IS GUARDED, from the renderer's side:
// TestTitleCap_IsSafeOnlyBecauseTheRendererRemeasures in cmd/agentop/tui takes a title at exactly this
// cap in the worst case for the mismatch — MaxTitleLen runes of CJK, twice that in columns — and
// requires the rendered cell to fit anyway. Removing the renderer's truncation fails it, along with
// ten other tests in that package.
//
// An earlier version of this comment said the opposite: that no single test held the relationship
// and that "nothing fails if someone removes the renderer's truncation". That was true when written
// and false once the guard landed, which is the worse direction for a comment to be wrong in — it
// invites a maintainer to delete guarded code. This package still cannot assert the width half
// itself (it has no width library, and the exported constant is what lets the other side name it),
// so the guard lives there and this comment points at it by name.
const MaxTitleLen = 80

// maxNormalizePasses bounds normalizeTitle's fixed-point loop.
//
// Each pass only ever deletes, so the string strictly shrinks and the loop converges long before
// this — two passes is the most any observed input needs. The cap exists so a future pass that
// somehow grows the string cannot spin, not because convergence is in doubt.
const maxNormalizePasses = 8

// clipTitle trims s and caps it at MaxTitleLen runes.
//
// No ellipsis: this is not the display truncation — the TITLE column applies its own, measured in
// display columns — and a marker added here would be re-truncated downstream, leaving a cell with
// two of them.
// normalizeTitle makes s plain: no markup, no characters that fail to print as themselves, one line.
//
// SEPARATED FROM THE LENGTH CAP so the cwd fallback can have the guarantee without the clipping.
// Before this it returned through a bare strings.Fields, which collapsed whitespace and nothing
// else — so a directory name carrying an ESC sequence, a bidi override or a tag reached the file
// unfiltered, while every other tier was clean. A guarantee that holds for three tiers out of four
// is not one.
func normalizeTitle(s string) string {
	// STRIP ALL MARKUP FIRST, wherever it sits. Every earlier attempt filtered markup by SHAPE —
	// anchored at the start, or line-leading, or per block — and each round of review found another
	// shape that slipped past: markup inside <command-args>, markup mid-line on a later line, a
	// block indented with a vertical tab. Enumerating shapes cannot converge, because the shapes are
	// the harness's to choose.
	//
	// So the rule here is positional-independent and applies to every tier: a title carries no tags
	// at all. Callers still unwrap envelopes and drop harness-output wrappers, because those
	// decisions are about WHICH TEXT to use; this is about what may appear in the result.
	//
	// ORDER, AND THEN A FIXED POINT.
	//
	// Every pass here REWRITES the string, so any of them can expose work for another, and the
	// exposure runs in every direction — which is why they are applied until nothing changes rather
	// than once each in a hand-picked order.
	//
	// stripANSI PRECEDES scrubRunes, and scrubRunes PRECEDES the markup passes. Both orderings are
	// forced, in opposite directions, which is why this sequence is not arbitrary.
	//
	// stripANSI must see the ESC byte intact, because it consumes a whole sequence as a unit and
	// scrubRunes would otherwise delete just the ESC and leave the payload as text: "\x1b[31mred"
	// became "[31mred", and inside a tag name "<system-\x1b[0mreminder>" became
	// "<system-[0mreminder>" — still not the tag, so its body still leaked.
	//
	// scrubRunes is INSIDE the loop, and that placement is the whole point. It used to run after the
	// loop had converged, and deleting a rune is exactly the kind of rewrite that exposes work:
	//
	//	"<\u200bsystem-reminder>BODY</\u200bsystem-reminder>"  ->  the tag name is split, so no
	//	pass recognised it; dropping the zero-width space then RE-JOINED "<" with its tag name and
	//	the output was intact markup, "<system-reminder>BODY</system-reminder>".
	//
	// Any invisible rune did it — zero-width space, bidi override, combining mark, variation
	// selector — and the file is what consumers other than the TUI read. The mid-NAME variant was
	// worse than the mid-bracket one: "hello <sys\u200btem-reminder>SECRET</...>" came out as
	// "hello SECRET world", which reads as ordinary prose while a harness block's body has been
	// promoted into the title with nothing visible to flag it.
	//
	// stripANSI precedes the markup passes for the same reason one order down: an escape sequence
	// inside a tag name hides that tag from the only pass that removes a block together with its
	// body, and stripTags would then unwrap it to its bare body.
	//
	// Bounded because each pass only ever deletes, so the length strictly decreases until it
	// stabilises. Looping past convergence is cheap; the passes are linear and titles are short.
	for i := 0; i < maxNormalizePasses; i++ {
		before := s
		s = stripANSI(s)
		s = scrubRunes(s)
		s = stripHarnessSpans(s)
		s = stripTags(s)
		if s == before {
			break
		}
	}
	return strings.TrimSpace(s)
}

// scrubRunes reduces s to one line of characters that print as themselves.
//
// Split out of normalizeTitle so it can run INSIDE that loop rather than after it; see the ordering
// argument there for what running it last allowed through.
//
// The old version collapsed whitespace and stopped, leaving ESC sequences, C1 controls, bidi
// overrides and zero-width characters in the written file. That was safe only because the viewer
// runs sanitizeLabel over every cell — so the guarantee lived in one consumer, and anything else
// reading ~/.cortex/session-metadata.json got the raw bytes. A title is meant to be plain text with
// nothing hidden in it, which has to be true of the FILE, not of one reader.
//
// An ALLOWLIST, not a denylist of known-bad ranges: unicode.IsGraphic is false for every control,
// format, surrogate and unassigned code point, so a category nobody enumerated cannot leak through.
//
// EVERY space becomes U+0020. unicode.IsSpace covers the exotic ones — U+00A0, U+3000, the U+2000
// block — and they are normalised rather than preserved, so a title holds no whitespace a reader
// cannot see for what it is. That also removes them as a way to smuggle shape past a consumer that
// splits on ASCII space; see looksLikePath, which had that bug.
// isSeparatorSpace reports whether r is whitespace that should become a single space rather than be
// dropped outright.
//
// The printing spaces, plus the three ASCII separators that structure prose: newline, carriage return
// and tab. Deliberately NOT unicode.IsSpace, which also claims U+000B, U+000C and U+0085 — see the
// non-graphic arm in scrubRunes for why those three are dropped instead.
func isSeparatorSpace(r rune) bool {
	switch r {
	case '\n', '\r', '\t':
		return true
	}
	return unicode.IsSpace(r) && unicode.IsGraphic(r)
}

func scrubRunes(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := true // leading whitespace is dropped
	for _, r := range s {
		switch {
		case isSeparatorSpace(r):
			// FOLDS TO U+0020. Two disjoint groups reach here and both are real gaps a reader sees or
			// a writer typed: the printing spaces (U+0020, U+00A0, U+3000, the U+2000 block), and the
			// LINE AND TAB separators, which do not print themselves but separate words in prose that
			// was never one line to begin with. Dropping those joined words — "line one\nline two"
			// became "line oneline two" — so they fold rather than vanish.
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
		case !unicode.IsGraphic(r):
			// EVERY OTHER non-printing rune is DROPPED, including the ones unicode.IsSpace also
			// claims: U+000B, U+000C and U+0085 (NEL). That overlap is the subtle part. Folding them
			// to a space turned "<\u0085system-reminder>BODY</...>" into
			// "< system-reminder>BODY</ system-reminder>" — and a space is not a name character, so
			// tagSpanLen correctly declined to call that a tag, the block was never removed with its
			// body, and the body became the title. They are vanishingly rare in prose and a tag-name
			// splitter in the adversarial case, so they go.
			// Controls (C0/C1/DEL), format characters — bidi overrides and isolates, ZWJ, ZWSP,
			// variation selectors — surrogates and unassigned code points. None of these print as
			// themselves, and the bidi ones actively reorder what surrounds them.
			//
			// Dropping a line break rather than folding it to a space is deliberate: a title is one
			// line, and the passes above have already removed the markup whose body a break might
			// have separated from surrounding prose.
		case unicode.Is(unicode.Mn, r), unicode.Is(unicode.Me, r), unicode.Is(unicode.Sk, r):
			// COMBINING AND MODIFYING characters: non-spacing marks, enclosing marks, and modifier
			// symbols (skin tones). Dropped rather than kept, which is what lets the length cut
			// in clipTitle be a plain rune slice.
			//
			// They only ever attach to the character before them, so a cut that lands between the
			// two leaves a dangling mark on whatever now precedes it — or a base character shorn of
			// its accent. Keeping them would mean measuring in grapheme clusters, which needs a
			// segmentation library this module does not have. Dropping them costs an accent
			// ("café" titles as "cafe") and keeps this function simple, which is the trade the
			// rest of it already makes.
		case r >= 0x1F1E6 && r <= 0x1F1FF:
			// Dropped, which can empty a title entirely — a prompt of nothing but flags. The tier
			// switch treats an empty result as "this tier has nothing", so the session falls through
			// to the next one rather than rendering blank; see the normalisation before that switch.
			// REGIONAL INDICATORS, which only carry meaning in pairs: a cut between them turns a
			// flag into a lone letter glyph. Two runes, never independently meaningful, so they go
			// together or not at all.
		default:
			b.WriteRune(r)
			prevSpace = false
		}
	}
	return b.String()
}

// clipTitle normalises s and caps it at MaxTitleLen runes.
//
// What every tier but the cwd fallback goes through; that one uses normalizeTitle alone, because a
// path's distinguishing end is its leaf and clipping keeps the head.
//
// A PLAIN RUNE CUT IS SAFE HERE, because normalizeTitle removed every character that binds to its
// neighbour: combining marks, modifiers, joiners and regional indicators are all gone, so one rune
// is one grapheme and the slice cannot land inside a cluster. Without that a cut could leave a
// dangling accent or half a flag.
func clipTitle(s string) string {
	s = normalizeTitle(s)
	r := []rune(s)
	if len(r) <= MaxTitleLen {
		return s
	}
	return strings.TrimSpace(string(r[:MaxTitleLen]))
}

// stripANSI removes a CSI/OSC escape sequence together with its parameters.
//
// Dropping the ESC byte alone — which the non-graphic filter below does — leaves the payload behind:
// "\x1b[31mred\x1b[0m" became "[31mred[0m", which is not dangerous but is not a title either. The
// whole sequence goes, so the result reads as what the user wrote.
//
// Handles the two forms that carry parameters: CSI (ESC [ … final byte in @-~) and OSC
// (ESC ] … terminated by BEL or ST). Any other escape is left to the non-graphic filter, which drops
// the ESC and leaves at most one stray character.
func stripANSI(s string) string {
	if !strings.ContainsRune(s, 0x1b) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		if rs[i] != 0x1b {
			b.WriteRune(rs[i])
			continue
		}
		// A LONE TRAILING ESC IS DROPPED, not written through. The old code fell to the default
		// branch and copied it, contradicting this function's own doc — harmless inside
		// normalizeTitle, where the allowlist catches it, but stripANSI has direct callers in the
		// renderer's width assertions.
		if i+1 >= len(rs) {
			break
		}
		switch rs[i+1] {
		case '[': // CSI: parameters, then a final byte in @ to ~
			j := i + 2
			for j < len(rs) && (rs[j] < '@' || rs[j] > '~') {
				j++
			}
			i = j
		case ']', 'P', 'X', '^', '_':
			// STRING-ARGUMENT SEQUENCES, all terminated by BEL or ST: OSC (]), DCS (P), SOS (X),
			// PM (^) and APC (_). Only OSC was handled, so a DCS payload — "\x1bPq …\x1b\\" —
			// lost its ESC and leaked the rest as literal text, which is a garbage title rather
			// than a dangerous one. They share a terminator, so they share a branch.
			//
			// The property test cannot tell these apart from the two-byte fallback, since both leave
			// a plain title — only the CONTENT differs, and "garbage but plain" satisfies the
			// guarantee. TestStripANSI_HandlesEveryEscapeClass is what pins the payload actually
			// going, rather than the ESC alone.
			j := i + 2
			for j < len(rs) && rs[j] != 0x07 {
				if rs[j] == 0x1b && j+1 < len(rs) && rs[j+1] == '\\' {
					j++
					break
				}
				j++
			}
			i = j
		case '(', ')', '*', '+', '-', '.', '/', '%', '#', ' ':
			// CHARSET SELECTION and other two-byte intermediates: ESC ( B, ESC # 8 and friends take
			// exactly one more byte. Skipping only the ESC left "(B" in the title.
			i += 2
		default:
			// A two-character escape (ESC 7, ESC c, ESC =). The ESC and its single final byte go.
			i++
		}
	}
	return b.String()
}

// stripHarnessSpans removes a known harness block AND ITS BODY, wherever it sits.
//
// stripTags alone deletes the angle-bracket spans and leaves what was between them, so
// "my question\n<system-reminder>LEAK</system-reminder>" became "my question LEAK" — the tags gone
// and the injected text promoted into the title. For a harness block the BODY is the payload, so the
// whole span has to go.
//
// Only the named set is removed with its body: those tags are the harness's, so their content is
// never the user's. An unknown tag keeps its body, because there the text between the brackets may
// well be the prompt — stripTags then removes the brackets alone.
//
// Position-independent, and applied before stripTags, so a block anywhere in the string is handled:
// at the start, appended on an indented line, mid-line on a later line, or nested inside a
// <command-args> value. That last one is the leak review found this round; the rest are the leaks it
// found in the rounds before.
func stripHarnessSpans(s string) string {
	if !strings.ContainsRune(s, '<') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	// A CURSOR, and one output buffer. This used to re-slice s on every excision and then restart
	// strings.Index from offset 0, which is quadratic in the number of blocks: measured end to end
	// through titleFromTranscript, 155KB took 21ms, 620KB 242ms and 2.5MB 2.49s — four times the input
	// for eleven times the work — and bufio admits lines up to 16MB, so a single chatty transcript line
	// could stall the harvest for seconds. cutTrailingHarness already advanced a cursor; this matches
	// it.
	//
	// SPANS ARE FOUND STRUCTURALLY, by tagSpanLen, and then judged by CANONICAL NAME. This used to
	// scan for each literal tag string from the list, which made the match as narrow as a byte
	// compare — and every way a real name can differ from its literal form downgraded "remove with
	// body" to "neuter the brackets", promoting the body into the title:
	//
	//	"<SYSTEM-REMINDER>INJECTED</SYSTEM-REMINDER>"    case
	//	"<system\u02c6-reminder>INJECTED</...>"           a rune wedged into the name
	//	"</system-reminder>"                             an orphan close, byte 1 is "/"
	//
	// One parser, one lookup: tagSpanLen decides what a span IS and isHarnessSpan decides what it
	// MEANS, so the two cannot disagree the way a second literal scanner did.
	for pos := 0; pos < len(s); {
		i := strings.IndexByte(s[pos:], '<')
		if i < 0 {
			b.WriteString(s[pos:])
			break
		}
		i += pos
		n := tagSpanLen(s[i:])
		if n == 0 || !isHarnessSpan(s[i:i+n]) {
			// Not a harness span. Emit through this "<" and keep looking; stripTags handles whatever
			// neutering the rest needs.
			b.WriteString(s[pos : i+1])
			pos = i + 1
			continue
		}
		b.WriteString(s[pos:i])
		b.WriteByte(' ')

		// The span runs to the matching close when there is one, and TO THE END OF THE STRING
		// otherwise.
		//
		// Not to the end of the opening tag, which is what it did: an unclosed block then left its
		// body behind as bare prose — "prose <system-reminder>INJECTED payload" kept "INJECTED
		// payload" — with no escape sequence needed. A harness block that is not closed is still a
		// harness block, and everything after it is its content as far as anyone can tell.
		//
		// THE MATCHING CLOSE, counting depth — not the first one. With the same tag NESTED,
		// "<system-reminder>a<system-reminder>b</system-reminder>LEAKED</system-reminder>" ended its
		// span at the INNER close, so the outer close and everything before it survived as "LEAKED".
		// The next iteration then found no opening tag, because this one had consumed it, so the
		// fixed-point loop could not recover it either.
		//
		// Depth is counted over spans of THE SAME canonical name, so a different harness tag nested
		// inside does not close this one; it is removed with this span's body regardless.
		//
		// The depth scan walks each byte of the span at most once, and the cursor never revisits it,
		// so the whole pass is linear in len(s).
		want := canonicalTagName(neuteredSpan(s[i : i+n]))
		end := len(s)
		depth := 0
		for k := i; k < len(s); {
			if s[k] != '<' {
				k++
				continue
			}
			m := tagSpanLen(s[k:])
			if m == 0 || canonicalTagName(neuteredSpan(s[k:k+m])) != want {
				k++
				continue
			}
			if strings.HasPrefix(s[k:], "</") {
				depth--
				if depth == 0 {
					end = k + m
					break
				}
			} else {
				depth++
			}
			k += m
		}
		pos = end
	}
	return b.String()
}

// stripTags NEUTERS every tag-name-shaped span in s: the angle brackets go, the text between them
// stays.
//
// POSITION-INDEPENDENT, which is the point: the shape-by-shape filters above it each guard one
// placement, and review found a new placement each round. This one cannot be flanked.
//
// IT USED TO DELETE THE WHOLE SPAN, and that cost more than the trade it was justified by. The
// argument was that a plain title cannot also faithfully quote markup, so a prompt quoting a tag
// loses it — one token, for an unflankable guarantee. Measured, two shapes cost far more than a
// token:
//
//	"compare a<b and c>d"       ->  "compare ad"
//	"why does List<String> fail"  ->  "why does List fail"
//
// Neither is markup. The first is two comparisons, and everything between them matched the
// name-then-anything rule, so six words of prose went with it. Swallowing a line is not a plain-text
// guarantee, it is data loss.
//
// NOT FIXED HERE, and deliberately: "the <system-reminder> tag is what I mean" still titles as "the".
// That span carries a KNOWN name, so stripHarnessSpans has already consumed it and everything after
// it before this pass runs — an unclosed harness tag is treated as running to end-of-string, because
// "prose <system-reminder>INJECTED payload" has the identical shape and truncating at the ">" would
// leak the payload. Telling a legitimate mention from an injection means guessing which trailing text
// is prose, and guessing wrong on the second is the failure this whole file exists to prevent. A
// truncated title for a prompt about the harness is the cheaper error.
//
// So the brackets are what goes. Prose keeps its words, and nothing tag-shaped survives — which is
// the property that matters downstream and the one the plainness test asserts generically.
//
// WHY THIS IS STILL SAFE for the case the deletion existed for: a harness block never reaches here.
// stripHarnessSpans runs first and removes every name in harnessTagNames TOGETHER WITH ITS BODY, so
// an injected instruction is already gone. What is left for this pass is markup with a name nobody
// enumerated, and for that, inert-but-noisy beats swallowing the line:
//
//	"<unknown-tag>X</unknown-tag>"  ->  "unknown-tag X /unknown-tag"
//
// A lone "<" is still left alone, so "is 3 < 5 in Go?" and "List<String>" both survive whole — the
// second now intact rather than sacrificed.
func stripTags(s string) string {
	if !strings.ContainsRune(s, '<') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for {
		i := strings.IndexByte(s, '<')
		if i < 0 {
			b.WriteString(s)
			break
		}
		// A TAG-NAME-SHAPED SPAN, not just anything between angle brackets. Two defects came from
		// treating every "<" as an opener and counting depth:
		//
		//   - two balanced comparison operators cancelled out, so the depth-0 fallback never fired
		//     and everything between them was eaten: "is 3 < 5 and 6 > 2 in Go?" became
		//     "is 3 2 in Go?";
		//   - one unbalanced "<" disabled stripping for the WHOLE string, including balanced tags
		//     before it, so normalizeTitle's no-markup contract was simply false for such inputs.
		//
		// Deciding per span fixes both: a "<" that does not begin a plausible tag is ordinary text
		// and is kept, and each span is judged on its own.
		if n := tagSpanLen(s[i:]); n > 0 {
			b.WriteString(s[:i])
			// THE INTERIOR, UNLESS IT IS ATTRIBUTE SYNTAX. Both extremes lose real text, and the
			// middle is measurable rather than a guess.
			//
			// Keeping everything surfaced machine syntax: `<a href="x>y">link</a>` neutered to
			// `a href="x>y" link /a`, putting quotes and a ">" back into a title meant to be plain.
			// Keeping only the NAME was worse — it read `b and c` in "compare a<b and c>d" as a name
			// plus attributes and dropped "and c", which is the data loss this change exists to
			// undo.
			//
			// Measured on 857 string user turns from two real config dirs: 6615 bare "<name>" spans,
			// 126 attribute-bearing, and 45 whose interior is prose. EVERY one of the 126 carries an
			// "=" or a quote (`from="a505…"`, `id="2e21"`, `pasted_content id=…`) and none of the 45
			// does. So that is the test: an interior holding "=", a double or a single quote is
			// machine syntax and only the name survives; anything else is text the user may have
			// typed and is kept whole.
			//
			// FENCED WITH SPACES, because the brackets were the only thing separating the span from
			// its neighbours and dropping them silently joined words: "List<String>" became
			// "ListString" and "compare a<b and c>d" became "compare ab and cd". Two tokens a reader
			// can see beats one word that never existed. scrubRunes collapses the runs and trims the
			// ends, so a fence at the start or between adjacent spans costs nothing.
			b.WriteByte(' ')
			b.WriteString(neuteredSpan(s[i : i+n]))
			b.WriteByte(' ')
			s = s[i+n:]
			continue
		}
		b.WriteString(s[:i+1])
		s = s[i+1:]
	}
	return b.String()
}

// tagSpanLen returns the length of the tag starting at s[0], or 0 if s does not start with one.
//
// A tag is "<", an optional "/", a name of letters, digits, "-" or "_", then attributes up to the
// closing ">". That is deliberately narrow: "< 5" and "<" at end-of-string are not tags, so a
// comparison operator in a real prompt survives, while "<div>", "</system-reminder>" and
// "<a href=\"x\">" do not.
//
// A ">" INSIDE A QUOTED ATTRIBUTE VALUE does not close the tag, which HTML permits and the first
// version of this got wrong by scanning for the first ">" byte. `<a href="x>y">link</a>` closed the
// span at the ">" in the URL, and the rest — `y">link` — survived into the title as residual
// markup. Both quote characters are tracked, since either may contain the other unescaped.
// neuteredSpan returns what survives of the tag span s once its brackets are gone.
//
// s must be exactly one span as measured by tagSpanLen. The interior, minus the brackets and any
// leading slash — except that an interior carrying attribute syntax ("=", a double or single quote)
// keeps only the tag name, since such an interior is machine-generated rather than typed. See
// stripTags for the measurement behind that rule.
//
//	"<div>"              -> "div"
//	"</div>"             -> "div"
//	"<b and c>"          -> "b and c"     (prose between two comparisons)
//	`<a href="x>y">`     -> "a"           (attribute syntax: name only)
func neuteredSpan(s string) string {
	in := strings.TrimPrefix(strings.TrimSuffix(strings.TrimPrefix(s, "<"), ">"), "/")
	if !strings.ContainsAny(in, `="'`) {
		return in
	}
	if i := strings.IndexFunc(in, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') &&
			!(r >= '0' && r <= '9') && r != '-' && r != '_'
	}); i >= 0 {
		return in[:i]
	}
	return in
}

func tagSpanLen(s string) int {
	if len(s) < 2 || s[0] != '<' {
		return 0
	}
	i := 1
	if s[i] == '/' {
		i++
	}
	nameStart := i
	for i < len(s) {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			i++
			continue
		}
		break
	}
	if i == nameStart {
		return 0 // no name: "< 5", "<>", "<="
	}
	var quote byte // 0 outside a quoted value, else the quote character awaiting its match
	for ; i < len(s); i++ {
		switch c := s[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			return i + 1
		}
	}
	// An opening tag with no ">" anywhere. Not a span, so the "<" is kept as text — the alternative
	// is swallowing the rest of the string on a stray bracket.
	return 0
}

// harnessTagNames are the wrapper tags observed on real transcripts, for the trailing-markup cut.
//
// A NAMED SET here, unlike isSyntheticPrompt's structural test, and deliberately: that one asks
// "does this text BEGIN as markup", where a false positive costs one fallback. This one cuts text
// off mid-prompt, so it may only fire on tags known to be the harness's. Matching any "<word>"
// would truncate a prompt that quotes HTML or generics.
// canonicalTagName reduces a tag name to the form the harness-tag lookup compares.
//
// Lowercased, and stripped of everything that is not a letter or digit. Both halves close a body-leak
// class rather than being tidiness:
//
//   - CASE. tagSpanLen accepts A-Z in a name, but the tag list is all lowercase and was compared byte
//     for byte, so "<SYSTEM-REMINDER>INJECTED</SYSTEM-REMINDER>" matched nothing, fell through to the
//     neutering pass, and promoted its body into the title. isSyntheticPrompt already lowercased and
//     documented why; the function that removes the BODY did not.
//   - NON-ALPHANUMERICS. Dropping them handles any rune wedged into a name, not just the hyphens and
//     underscores the list happens to contain. U+02C6 is category Lm, so scrubRunes does not drop it
//     (it drops Mn/Me/Sk), and "<system\u02c6-reminder>" split the name past any literal compare with
//     nothing in the pipeline to remove the rune — so the fixed-point loop could not rescue it either.
//     Canonicalising the NAME rather than enumerating runes to delete means the next such category
//     needs no change here.
func canonicalTagName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		}
	}
	return b.String()
}

// harnessTagSet is harnessTagNames keyed by canonical name, for the body-removing lookup.
var harnessTagSet = func() map[string]bool {
	m := make(map[string]bool, len(harnessTagNames))
	for _, tag := range harnessTagNames {
		m[canonicalTagName(tag)] = true
	}
	return m
}()

// isHarnessSpan reports whether the tag span s names a harness block, whatever its case, its
// punctuation, or whether it is an opening or a closing tag.
//
// s must be exactly one span as measured by tagSpanLen. neuteredSpan already drops the brackets and a
// leading slash, so a CLOSING tag canonicalises to the same name as its opener — which is what makes
// an orphan "</system-reminder>" recognisable. It was not: the scan looked for "<system-reminder" and
// byte 1 of an orphan close is "/", so it missed, and the neutering pass turned the orphan into the
// bare word "system-reminder".
func isHarnessSpan(s string) bool {
	return harnessTagSet[canonicalTagName(neuteredSpan(s))]
}

var harnessTagNames = []string{
	"<system-reminder",
	"<task-notification",
	"<pasted_content",
	"<bash-stdout",
	"<bash-stderr",
	"<local-command-caveat",
	"<command-message",
	"<command-name",
	"<command-args",
	"<agent-message",
	"<user-prompt-submit-hook",
}

// cutTrailingHarness drops a harness block appended AFTER the user's prose.
//
// Measured, 7 of 130 transcripts had a string turn shaped "…real question…<system-reminder>…", which
// the anchored guard cannot see because the line starts with prose. Left alone the tag reached the
// title and was clipped mid-tag at 80 runes.
//
// Cuts at the first known tag THAT STARTS A LINE, and keeps what precedes it — the prompt is the part
// the user typed, and everything the harness appends comes after, on its own line. The positional
// constraint is what stops prose being truncated for merely mentioning a tag: the named set alone was
// not enough, and "how do I use <command-args> in a skill?" came out as "how do I use".
//
// Returns s unchanged when nothing precedes the tag, so a turn that is markup all the way through
// still falls to isSyntheticPrompt rather than becoming "".
func cutTrailingHarness(s string) string {
	cut := -1
	for _, tag := range harnessTagNames {
		for from := 0; ; {
			i := strings.Index(s[from:], tag)
			if i < 0 {
				break
			}
			i += from
			from = i + len(tag)
			// POSITIONAL. The harness appends its block on a line of its own, so a known tag only
			// counts when it STARTS A LINE. Without this, prose that merely mentions a tag was
			// truncated mid-sentence — "how do I use <command-args> in a skill?" became "how do I
			// use" — which is the very outcome the named set was chosen to avoid, and the comment
			// above claimed it did.
			//
			// LEADING WHITESPACE STILL COUNTS AS LINE-LEADING. Requiring s[i-1] to be exactly a
			// newline let an INDENTED block through: "my question\n   <system-reminder>…" kept the
			// tag and was clipped mid-tag at 80 runes, which is the failure this function's own doc
			// describes. Only whitespace is skipped, so the constraint still holds against anything
			// with real text before it on the line.
			//
			// A known tag mid-line on a LATER line — "line one\nline two <system-reminder>x" — is
			// deliberately left alone: it is genuinely ambiguous between prose and appended markup,
			// and cutting it would risk the mid-sentence truncation the positional rule exists to
			// prevent.
			//
			// Every occurrence is examined, not just the first: a prompt may mention a tag inline
			// and still have a real appended block after it.
			if i > 0 {
				// ANY whitespace counts as indentation, not just space and tab — and it is decoded as
				// a RUNE, which is the part the previous version got wrong. `unicode.IsSpace(rune(s[j]))`
				// converts one BYTE, so a multi-byte space (NBSP, U+3000, U+2028, ogham) never matched:
				// its continuation bytes are not IsSpace, so the scan stopped on them and the block was
				// treated as mid-line. The comment claimed those very shapes were covered.
				//
				// Not a leak end to end — normalizeTitle removes the tag either way — but a defence that
				// only appears to cover a case is worse than one that admits it does not.
				j := i
				for j > 0 {
					r, size := utf8.DecodeLastRuneInString(s[:j])
					if r == '\n' || r == '\r' || !unicode.IsSpace(r) {
						break
					}
					j -= size
				}
				if j > 0 {
					if r, _ := utf8.DecodeLastRuneInString(s[:j]); r != '\n' && r != '\r' {
						continue
					}
				}
			}
			if cut < 0 || i < cut {
				cut = i
			}
			break
		}
	}
	if cut <= 0 {
		return s
	}
	if head := strings.TrimSpace(s[:cut]); head != "" {
		return head
	}
	return s
}
