package tui

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/rossoctl/cortex/core/observe/claude"
)

// sessionTitle names a session from the harvested metadata, or "" when nothing names it.
//
// Nil-safe by construction rather than by a guard: a read on a nil map yields the zero
// SessionMetadata, so a session nobody harvested, an id not in the file, and the file being
// absent altogether all render the same empty cell. That is the right answer for all three —
// none of them is a fact about the session, only about whether anyone has run the harvester.
//
// HARVEST-ONLY, AND THAT IS LOAD-BEARING — do not fold the server's /v1/sessions title in here.
// sessionHasTitle reads this, and through it every backoff predicate in session_metadata.go, so a
// server title arriving would make the row read as named and STOP the re-harvest for good. The
// fallback lives in sessionTitleFor instead, which only display paths call. See its doc.
func (m *model) sessionTitle(id string) string {
	// SANITISED AT THE ACCESSOR, so every consumer is covered by one line. The title is
	// LLM-generated transcript text — sessions_metadata.go says so — and it reaches a table
	// cell, the title bar, and two headers. The cell path stripped newlines but not escape
	// sequences or carriage returns, and the title-bar path constrained neither: a newline
	// splits the frame and an ESC recolours the pane from a string nobody here wrote.
	//
	// sanitizeLabel is the package's existing answer for exactly this, introduced with a
	// CWE-150 citation. Severity is bounded — the file lives under the operator's own home
	// directory — which is why this is a one-line routing rather than a redesign.
	//
	// AND CAPPED HERE TOO, for the reason sessionTitleFor caps the served title: the harvester
	// emits at most claude.MaxTitleLen runes, but NOTHING RE-CHECKS THAT ON LOAD.
	// LoadSessionMetadata parses the JSON and applies no cap, so a hand-edited or rewritten
	// ~/.cortex/session-metadata.json reaches the quadratic path in truncLeft/truncRight exactly
	// as an uncapped served title would. Measured before this line existed: a 10003-rune
	// path-shaped title with combining marks made ONE rebuildSessionsTable take 1.11s.
	//
	// Pre-existing rather than introduced by the fallback — but capping only the served side left
	// the two sources asymmetric for no reason, and the fix is the same constant. Re-capping an
	// already-capped harvested title costs a length check.
	//
	// TRIMMED BEFORE THE CUT, and that is a CORRECTNESS requirement rather than tidiness — but note
	// the history, because this comment asserted the OPPOSITE order for the same reason and was
	// wrong twice over.
	//
	// The cap was first written claiming "truncation cannot turn a non-blank title blank". False. It
	// was then rewritten to trim AFTER the cut, with a comment claiming that trimming cannot
	// introduce the failure it prevents "because it only ever removes whitespace". Also false, and
	// the measurements are the refutation: a title whose first MaxTitleLen runes are whitespace with
	// real text after them loses the text to the CUT, and is then emptied by the TRIM. Measured at
	// MaxTitleLen = 80: 40 leading spaces keeps 50 runes, 79 keeps just "m", and 80 or more returns
	// "" — for U+0020, U+00A0 and U+3000 alike. sessionHasTitle reads sessionTitle, so that flips a
	// named row to unnamed and re-harvests ~/.claude every ~3 minutes for the life of the process:
	// the permanent-rescan cost this file documents as the price of an UNNAMABLE session, charged
	// instead to a session with a perfectly good name.
	//
	// Trimming first dissolves the problem rather than guarding against it. Leading whitespace is not
	// part of the name, so it should never have consumed the rune budget; once it does not, no amount
	// of it can push real text past the cut. capTitleRunes trims both ends of its input before
	// measuring, and trims again after cutting to drop whitespace the cut newly exposed.
	//
	// clipTitle (core/observe/claude/scrub.go) trims after its own cut and is safe doing so for a
	// reason that does not transfer: normalizeTitle has already collapsed every whitespace run ahead
	// of it, so it never sees a leading run long enough to matter. Neither string here has been
	// through that — which is the same precondition the cluster walk below exists because this side
	// lacks.
	return capTitleRunes(sanitizeLabel(m.sessionsData[id].Title))
}

// capTitleRunes cuts s to at most claude.MaxTitleLen runes, on a grapheme-cluster boundary, and
// trims the result.
//
// ONE HELPER BECAUSE THERE ARE TWO SOURCES. sessionTitle caps the harvested title and
// sessionTitleFor caps the served one; both need the same bound, and the two open-coded copies that
// preceded this differed only in which string they read. The cost of the cap is documented at both
// call sites — briefly, truncLeft/truncRight bound their OUTPUT but search quadratically over their
// INPUT whenever a zero-width rune disables the fast path, so an uncapped title is seconds per row
// per rebuild on the UI goroutine.
//
// utf8.RuneCountInString RATHER THAN len([]rune(s)), because the common case is a title already
// under the cap and that case must allocate nothing. The rune slice was 160 B/op and 232ns for a
// 40-rune title against 0 B/op and 155ns here, on a path reached ~3-4x per row per 2s tick. The
// slice is still built when a cut is actually needed, where its cost is the point rather than
// overhead. Note the trade: counting first walks the string twice, so on the over-long path this is
// ~45% slower than slicing immediately (780µs vs 1.13ms at 200k runes). That is the right way round
// — the short path is the one that runs constantly, and the long path is a cut this is preventing
// the expensive consequences of, not an operation to optimise.
//
// THE CUT LANDS ON A CLUSTER BOUNDARY, and that is not a nicety. clipTitle upstream says a plain
// rune cut is safe for it ONLY BECAUSE normalizeTitle has already removed every character that
// binds to its neighbour — combining marks, modifiers, joiners, regional indicators. NEITHER string
// here has been through that: the harvested title is re-read from a file that may have been
// rewritten, and the served title comes from core/session's sanitizeTitle, whose own doc says it
// KEEPS combining marks "so café survives". So a blind cut at MaxTitleLen can sever a cluster,
// leaving a dangling accent bound to whatever precedes it, half an emoji ZWJ sequence, or one
// regional indicator of a flag — a title that renders as something nobody wrote. Walking back off
// the binders costs a few rune tests on the only path that ever cuts.
//
// normalizeTitle is unexported and staying that way, so this is a boundary walk rather than a reuse.
// It is deliberately narrower than normalising: the goal is only that the cut not land mid-cluster,
// not that clusters be removed.
func capTitleRunes(s string) string {
	// TRIMMED FIRST, so surrounding whitespace never consumes the rune budget. See sessionTitle's
	// note: trimming after the cut let 80 leading spaces empty a genuinely-named title, because the
	// cut kept only the spaces and the trim then removed them. Whitespace is not part of a name, so
	// the fix is for it not to count rather than to detect the damage afterwards.
	//
	// This also makes the early return below exact. Trimming after it would mean a string of 80
	// real runes plus one trailing space took the cut path to produce a result the early return
	// could have returned untouched.
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= claude.MaxTitleLen {
		return s
	}
	r := []rune(s)
	cut := claude.MaxTitleLen
	// Walk back off a cut that would orphan r[cut] from what precedes it.
	//
	// ONE CLUSTER IS THE WHOLE BUDGET, and saying so is the entire reason this is a loop with a
	// floor rather than a while-it-binds walk. Two earlier versions each walked until the rune at
	// the cut stopped binding, and each emptied a non-blank title on a long enough run:
	//
	//   - Regional indicators pair into flags, so in a run only every second one binds. Treating
	//     all of them as binders walked to index 0: 41 consecutive flags capped to "".
	//   - Combining marks STACK — "a mark cannot follow a mark" is false, and this comment used to
	//     assert it as the loop's bound. "a" + 100 U+0301 capped to "", and "a"*60 + 40 marks lost
	//     21 runes against a documented bound of one step.
	//
	// Both are the same bug reached by different routes, and the shared root cause is that the
	// binding predicate answers "is this rune part of a cluster?" while the loop needed "where does
	// this cluster START?". An unbounded walk answers the first question repeatedly and can consume
	// the whole title; a degenerate cluster is not a reason to return nothing.
	//
	// Emptying the title is the failure that matters, not the shortening: sessionHasTitle reads
	// sessionTitle, so a named row that caps to "" reads as unnamed and restarts the permanent
	// ~3-minute re-harvest. That is the same failure f5a0a615 fixed for whitespace, which is why
	// this walk now has a floor that cannot reach 0 while any non-binder precedes the cut.
	//
	// So: find the start of the cluster the cut lands inside, then take all of it or none of it.
	// clusterStart stops at the first non-binder, so it reads at most the ONE cluster straddling the
	// cut — and the ONLY way to lose the whole title is a string that is a single degenerate cluster
	// from index 0, a title with no base character at all, which no longer costs anything because
	// the fallback below keeps MaxTitleLen runes of it rather than returning "".
	//
	// "One cluster" is the right bound but NOT a small number, and it is worth being exact because a
	// reviewer read the earlier wording as promising one step. A degenerate cluster can be as long as
	// the cut, so the scan is min(cluster length, MaxTitleLen) — 80 steps for "a" + 100 marks, and
	// still 80 for "a" + 2000, which is the part that matters: it does not grow with the title. On
	// every shape where ordinary text precedes the cut it is 1 step, because r[cut] is a base
	// character and the loop returns immediately.
	// A LEADING DEGENERATE CLUSTER IS THE ONE SHAPE WITH NO BOUNDARY TO CUT ON, and it is decided
	// STRUCTURALLY — by clusterStart reporting 0 — rather than by noticing afterwards that the
	// result came out blank. That distinction is the whole reason this reads the way it does. A
	// blanket "if the cap emptied a non-blank title, cut bluntly instead" guard was written here
	// first and rejected: it rescues the output of ANY walk, including the unbounded one this
	// replaces, so every mutation test of the walk passed under it. A fallback that makes the
	// broken and the fixed implementation indistinguishable is not a safety net, it is a mask.
	//
	// clusterStart == 0 means r[0] itself binds to what precedes it, i.e. the title opens with
	// marks or an odd flag half and there is no earlier boundary in the string. Cutting bluntly at
	// MaxTitleLen splits that cluster, which is the lesser evil by a wide margin: a dangling accent
	// renders as one odd glyph, whereas "" flips sessionHasTitle to unnamed and restarts the
	// permanent ~3-minute re-harvest.
	if start := clusterStart(r, cut); start > 0 {
		cut = start
	}
	return strings.TrimSpace(string(r[:cut]))
}

// clusterStart returns the index of the first rune of the grapheme cluster that r[cut] belongs to,
// or cut itself when r[cut] starts its own cluster and the cut is already on a boundary.
//
// BOUNDED BY THE ONE CLUSTER STRADDLING THE CUT, not by the title. The scan stops at the first rune
// that does not bind to what precedes it. That bound is the fix for two defects that shipped in this
// file: capTitleRunes' doc above has the measurements.
//
// NOT bounded by a constant, and the difference has been misread: a degenerate cluster can run the
// whole way back, so the worst case is min(cluster length, cut) steps — 80 for "a" + 100 combining
// marks, and still 80 for "a" + 2000, which is the property that matters. Where ordinary text
// precedes the cut it returns on the first iteration.
//
// cut MAY EQUAL len(r), meaning "the cut is past the last rune". r[cut] does not exist there, so
// there is nothing to orphan and the answer is cut itself. Handled explicitly rather than left to
// the caller: capTitleRunes only ever passes a cut strictly inside r (its early return guarantees
// len(r) > MaxTitleLen), so this arm is unreachable from the one live caller today — but the
// alternative was an index-out-of-range panic on a plausible direct call, load-bearing on a coupling
// two functions apart that nothing stated and no test pinned. bindsToPrevious' doc invites other
// callers in this package; this makes the invitation safe. A cut ABOVE len(r) is a caller bug and
// still panics, deliberately: clamping it would invent an answer for a question the caller got wrong.
//
// Regional indicators are resolved by parity rather than by the per-rune predicate, because their
// binding depends on POSITION — only the second of a pair binds. riBindsAtCut counts the preceding
// run, so an even run means r[i] opens a fresh pair and i is already a boundary.
func clusterStart(r []rune, cut int) int {
	if cut >= len(r) {
		return cut
	}
	for i := cut; i > 0; i-- {
		if r[i] >= 0x1F1E6 && r[i] <= 0x1F1FF {
			if !riBindsAtCut(r, i) {
				return i
			}
		} else if !bindsToPrevious(r[i]) {
			return i
		}
	}
	return 0
}

// riBindsAtCut reports whether the regional indicator at r[cut] is the SECOND half of a flag, so
// cutting before it would leave a bare letter where a flag was.
//
// Regional indicators are the one binder whose binding depends on POSITION rather than on the rune:
// they pair left to right, so in "🇺🇸🇬🇧" the first and third bind to nothing while the second and
// fourth complete a flag. Counting the unbroken run of them that precedes the cut gives the parity —
// an even-length run means r[cut] starts a fresh pair and the cut is already on a boundary.
//
// The scan is bounded by the run, not by the title: it stops at the first non-RI rune. A title that
// is nothing but flags is the worst case, and it is exactly the case a global walk got wrong.
func riBindsAtCut(r []rune, cut int) bool {
	run := 0
	for i := cut - 1; i >= 0 && r[i] >= 0x1F1E6 && r[i] <= 0x1F1FF; i-- {
		run++
	}
	return run%2 == 1
}

// bindsToPrevious reports that r renders as part of the cluster started by the rune before it, so a
// cut immediately before r would split that cluster.
//
// Mn/Me/Mc ARE THE MARK CATEGORIES, and together they approximate Unicode's Grapheme_Extend: Mn
// non-spacing (a combining accent, a variation selector), Me enclosing, Mc spacing-combining (a
// Devanagari vowel sign, which occupies a column but still belongs to the letter before it). Two
// more bind without any category saying so: U+200D ZWJ is what joins the codepoints of a
// multi-part emoji, and Regional_Indicator runes pair up into flags, so a cut between two leaves a
// bare letter where a flag was.
//
// REGIONAL INDICATORS ANSWER TRUE HERE BUT ARE NOT DECIDED HERE. Their binding depends on position,
// not on the rune — only the second of a pair binds — and this function sees one rune with no
// context. capTitleRunes routes them through riBindsAtCut instead; this arm remains so the
// predicate's answer to "can this rune ever bind?" stays honest for any other caller.
//
// THE ZWJ ARM IS UNREACHABLE FROM BOTH PRODUCTION CALLERS TODAY, and deliberately kept. Since
// sanitizeLabel began delegating to pipeline.IsControlRune, a ZWJ is replaced by U+FFFD before either
// cap site sees it, and U+FFFD binds to nothing. So the hazard the paragraph above describes cannot
// currently occur on either path. It is kept because the unreachability is incidental — it depends on
// a sanitiser in another file continuing to treat ZWJ as a control rune, which is a rule about
// terminal safety and not about grapheme clusters. A future caller that caps an unsanitised string,
// or a narrowing of IsControlRune, restores the hazard silently. A dead rune test is cheaper than
// that coupling.
//
// DELIBERATELY NOT Lm. Modifier LETTERS (U+02B0 ʰ and the like) read as though they belong here and
// Grapheme_Extend excludes them — and the category also holds runes that legitimately START a
// cluster, U+02BB ʻokina being a letter in Hawaiian orthography. Including Lm would walk the cut
// back off an ordinary word character. Sk (modifier SYMBOLS, U+02C7 ˇ) is out for the same reason.
// A test fixture built on U+02B0 is what surfaced this; it was the fixture that was wrong.
//
// A SMALL EXPLICIT SET rather than a grapheme-segmentation library: agentop has no such dependency,
// this is one cut on one display path, and being slightly conservative only moves the cut earlier by
// a rune or two. The failure it prevents is a severed cluster; the cost of over-walking is a shorter
// title.
func bindsToPrevious(r rune) bool {
	if unicode.In(r, unicode.Mn, unicode.Me, unicode.Mc) {
		return true
	}
	return r == '‍' || (r >= 0x1F1E6 && r <= 0x1F1FF)
}

// noServedTitle is the served-title argument for a row that cannot have one. Only the cached-only
// rows qualify, and only because the server has stopped listing those sessions entirely.
//
// It exists because sessionTitleFor takes the served title as a parameter, so passing "" silently
// disables the fallback and compiles. The parameter stays — the live row loop already holds the
// summary, and resolving it inside would put a scan of m.sessions in the per-row render path — so
// the one legitimate empty argument says so by name instead.
//
// "CANNOT HAVE ONE" IS ABOUT TODAY'S STRUCTURE, not a claim that nothing better is possible. A
// session named ONLY by the proxy keeps its name while listed and loses it the moment it becomes
// cached-only, so an operator watches a title vanish from a row whose events are deliberately
// preserved (the #870 scenario). Remembering the last-seen served title would fix that, and nothing
// here does: neither this constant nor cachedOnlySessionIDs retains anything from the summary that
// went away. Deferred rather than overlooked — it means holding title state across list refreshes,
// which is a store question and not a rendering one.
const noServedTitle = ""

// sessionTitleFor names a session for DISPLAY, falling back to the title the proxy served.
//
// Two independent sources, and each covers what the other cannot. The harvest reads Claude Code's
// transcripts on this machine; /v1/sessions carries a title the proxy derived from the session's
// own events. So a session the harvester has no transcript for can still be named, and that is not
// a rare shape: on a laptop where every blank harvested entry was checked, all of them belonged to
// an agent with no Claude Code transcript tree — one that does route through the proxy, so a served
// title existed for exactly those. A blank cell was never "this session has no name", only "no name
// where agentop was looking".
//
// HARVEST WINS when both exist. Deliberately a fixed precedence and not a judgement about which
// string is better: both sides pick a title through their own ranking, both may change, and this
// says as little as possible about either mechanism so that it does not go stale when they do.
// What it costs is worth knowing — the two rankings do not agree on every session, so a row can
// show a harvested title while the proxy held one an operator would have preferred (an explicit
// /rename is the clearest case). That is accepted for now, pending what operators report; the
// precedence is one line to invert if it turns out wrong.
//
// THE FALLBACK IS ONLY HERE, not in sessionTitle. Every backoff predicate in session_metadata.go
// judges "unnamed" through sessionTitle, so this deliberately leaves a server-titled row reading as
// unnamed to them: the harvest keeps looking for the harvested title, at the cost of a periodic
// transcript scan. That cost is the trade, not a leak — but it is a PERMANENT steady state on
// exactly the rows this fallback serves, not a transient one. An agent with no Claude Code
// transcript tree is the case that motivated the feature, and for it the harvest can never
// succeed, so the backoff pins at untitledBackoffCap and re-walks ~/.claude every 3 minutes for
// the life of the process (~0.7s for a first full scan, per the README). Accepted because the
// alternative is the re-harvest stopping on a served title and never picking up a transcript that
// appears later; worth revisiting if that walk ever becomes expensive enough to matter.
//
// THROUGH titleIsBlank rather than == "", because a harvested " " renders as nothing and filling
// nothing is the whole point. titleIsBlank's own doc anticipates this seam.
//
// served IS SANITISED TOO, and not on the assumption that it arrives clean. The proxy does trim and
// cap it, but /v1/sessions is unauthenticated and the title is folded from caller-supplied event
// content — sessionTitle's CWE-150 reasoning applies to this string at least as much as to the
// harvested one.
//
// THAT INDEPENDENCE IS NOW ACTUALLY TRUE, and it was not when the fallback first landed.
// sanitizeLabel then replaced the BIDI overrides and isolates but not the BIDI MARKS (U+200E/200F,
// U+061C) or the zero-widths (U+200B/200C/200D/2060/FEFF), all of which pipeline.IsControlRune names
// and core/session's sanitizeTitle does strip. So the only thing keeping a mark out of the cell was
// the producer this comment claimed not to rely on — a claim the code contradicted, for exactly the
// class of rune whose whole purpose is to make the rendered order differ from the byte order.
// sanitizeLabel delegates to IsControlRune now; see its doc.
//
// AN EMPTY served DOES NOT ALWAYS MEAN "the proxy derived none". A session that arrives on the
// event stream before it appears in a list refresh gets a stub SessionSummary with a zero Title
// (app.go's streamed-event path), so its row shows no served title until the next poll fills the
// summary in — under two seconds, and it self-corrects with no help from here. Worth knowing only
// because it makes a blank cell briefly ambiguous; nothing downstream needs to tell the two apart.
// served IS CAPPED HERE, and this is the only thing that bounds it. The harvested title arrives
// already capped at claude.MaxTitleLen runes, and the existing cross-module cap test asserts that
// against a harvested fixture — a path the served title never takes. The proxy does cap at its own
// maxTitleLen, but that is 80 in ANOTHER MODULE, unexported on purpose (its doc: "deliberately NOT
// that constant"), so nothing here can assert it and no client should assume it. /v1/sessions is
// unauthenticated and operator-pointed, so a title of any length is a thing agentop can be handed.
//
// What that costs without a cap is not a wide cell — truncLeft/truncRight bound the OUTPUT — it is
// the SEARCH inside them. Their fast path is disabled by any zero-width rune, and a served title
// keeps its combining marks, so a long one runs a quadratic scan: measured, 20003 runes takes 4.50s
// on ONE call, and 200003 did not finish in two minutes. That is the UI goroutine, once per row per
// rebuild. Capping the input is what REMOVES THE QUADRATIC TERM — not what makes the whole path
// flat, which an earlier version of this comment claimed and the code does not do. Two passes still
// run over the UNTRUNCATED string before the cap can apply: sanitizeLabel builds a new string with
// b.Grow(len(s)), and the cap's own rune count walks it. Both are linear, so a 200k-rune title still
// costs ~1.6MB of transient allocation and a couple of walks per row per rebuild.
//
// TWO IS THE FLOOR, AND IT WAS THREE. Review caught sessionTitleFor calling titleIsBlank(served) and
// then sanitizeLabel(served), which sanitised the untruncated string TWICE — ~3.2MB, not ~1.6MB, for
// the same answer, because sanitizeLabel is idempotent and the second copy was pure waste. It now
// sanitises into a local and blank-checks that (see blankSanitized). Anything that reintroduces a
// second sanitise, or a second []rune conversion, doubles the number in this paragraph; there is no
// benchmark that would notice, so read the call sites. Linear is the
// difference between a laggy column and an unusable one — the measured 4.50s at 20003 runes was the
// quadratic search, not these — but "flat" was wrong, and the honest bound is what a future reader
// needs when deciding whether to cap EARLIER, at the decode in apiclient, where it would be.
//
// AT THE HARVESTER'S CAP, reusing claude.MaxTitleLen rather than a new number: it is what the other
// source is already capped to, so the two titles get the same budget and the column keeps one rule.
// Runes, not columns, matching what that constant counts — the width re-measure downstream is what
// turns either into a fitted cell.
//
// THE TWO ARGUMENTS ARE UNCHECKED AGAINST EACH OTHER, and nothing here can detect a mismatch. served
// is meant to be THIS id's Title, but it is passed in rather than looked up, so pairing one session's
// id with another's title compiles and renders a confident wrong name — the worst failure shape this
// column has, because a title is what an operator uses to pick a row before acting on it. The two
// live callers are safe by construction (the row loop reads both from one SessionSummary; sessionLabel
// resolves served from the same id it passes), and that is the invariant a third caller must preserve:
// RESOLVE served FROM id, never from an index, a neighbouring row, or a previous frame's summary.
// servedTitle(id) exists for exactly that and is the right thing to reach for. The parameter stays
// because resolving inside would put a scan of m.sessions in the per-row render path — see
// noServedTitle — so this is a documented contract rather than an enforced one.
func (m *model) sessionTitleFor(id, served string) string {
	// PRECEDENCE IS DECIDED ON THE RAW HARVESTED TITLE, not on what the cap left of it, and that
	// separation is the fix for a real inversion. This read m.sessionTitle(id) — sanitised AND capped
	// — and asked whether THAT was blank. So any route by which the cap could blank a non-blank
	// harvested title also silently handed the row to the served one: sessionTitleFor(85 spaces +
	// "real", "served-name") returned "served-name", showing the proxy's name to an operator who has
	// been told in two docs and a core/session comment that HARVEST WINS. Worse than the missing
	// title it replaced, because a wrong name is acted on and a blank one is not.
	//
	// The whitespace defect behind that specific case is fixed in capTitleRunes, so the two
	// formulations now agree on every input known to differ. This one is still the right question to
	// ask: "did the harvest name this session?" is about the harvest, and routing it through a
	// length cap makes a display bound into a precedence rule. Whatever the cap does to a long
	// title, it cannot move the row to the other source.
	//
	// sanitizeLabel is still applied — blankness must be judged on what RENDERS, which is
	// titleIsBlank's whole reason for sanitising before trimming ("\t" paints a visible glyph, so it
	// is not blank). Only the cap is out of the decision.
	if raw := m.sessionsData[id].Title; !titleIsBlank(sanitizeLabel(raw)) {
		return m.sessionTitle(id)
	}
	// SANITISE ONCE, INTO A LOCAL, then use it for both the blank check and the cap.
	//
	// SANITISE BEFORE CAPPING, matching sessionTitle, so the cluster walk inside the cap sees the
	// string that will actually render. sanitizeLabel is rune-for-rune, so the two orders agree on
	// WHERE the cut lands — but only one of them agrees on WHAT is at the cut: sanitising afterwards
	// would walk back off a combining mark that sanitizeLabel then replaces with U+FFFD, a standalone
	// glyph that never needed the walk. Ordering it this way keeps one rule for both sources.
	//
	// ONE PASS, NOT TWO: this used to call titleIsBlank(served) and then sanitizeLabel(served) on the
	// next line. Both sanitise, and sanitizeLabel is O(len) with a b.Grow(len(s)) on the UNTRUNCATED
	// input, so the pair built two full copies of a served title to reach one answer — doubling the
	// transient allocation this function's own cost note bounds, per row per rebuild. Idempotent made
	// it harmless but not free.
	clean := sanitizeLabel(served)
	// BLANK-CHECKED ON THIS SIDE TOO, because titleIsBlank was applied to the harvested title and
	// not to this one — so a whitespace-only served title painted spaces into the cell while
	// sessionLabel, which blank-checks what this returns, showed the bare id. The same session named
	// two different ways by two callers of one accessor. Returning "" makes the cell agree with the
	// header, and "" is what both already do when nothing names a session.
	//
	// blankSanitized rather than titleIsBlank because clean has already been through sanitizeLabel —
	// and the ORDER that check documents is preserved, not dropped: sanitising happened above, and the
	// trim happens here, which is the same sanitise-then-trim titleIsBlank performs internally.
	if blankSanitized(clean) {
		// A pending bucket nothing names still says what it is. Last, so a title either source
		// gives it wins; and here, not in sessionTitle, which the re-harvest backoff reads as
		// "named". The pid is the client's to shape, so it is sanitised like any title.
		if _, pid, ok := pendingBucket(id); ok {
			return capTitleRunes(sanitizeLabel(pendingTitle(pid)))
		}
		return ""
	}
	return capTitleRunes(clean)
}

// sessionTitleCell is sessionTitleFor fitted to the TITLE column, truncated from the LEFT.
//
// Truncating from the left because the titles are mostly paths. bubbles truncates every cell
// from the right, which on "/Users/person/src/cortex/.worktrees/claudesessions" keeps
// "/Users/person/src/cor…" — the half every session on the machine shares, and none of the
// half that says which one this is. Keeping the tail instead gives "…es/claudesessions".
//
// Pre-truncated here rather than left to bubbles because bubbles offers no choice of side, so
// the cell has to arrive already short enough. That means reading the LIVE column width, not
// the declared one: layout() fits the columns and this reads back what it decided, which is
// why layout() must also rebuild these rows — see its call to rebuildSessionsTable.
//
// Left alone when it is not a path: a title is prose, and prose reads from the left.
func (m *model) sessionTitleCell(id, served string, titleW int) string {
	title := m.sessionTitleFor(id, served)
	if title == "" {
		return title
	}
	// EVERY CELL IS BOUNDED, whichever branch it takes. Prose was returned untouched on the
	// reasoning that it reads from the left and so loses nothing to a right-side cut — true, but
	// it left the cell UNBUDGETED, and bubbles then truncated it in the fixed-width box anyway.
	// A Windows-style path took this branch too and came out 63 columns wide against a budget of
	// 11: that was previously dismissed as unreachable on this platform, which confused where the
	// string comes FROM with where it is rendered. Titles are harvested text; the renderer does
	// not get to assume their shape.
	if !looksLikePath(title) {
		if titleW <= 0 {
			// FAIL SAFE, not wide. A non-positive budget means the caller could not tell us how
			// much room there is, and returning the full title then hands an unbounded cell to a
			// table that has to cut it somewhere — the one thing every other branch here exists
			// to prevent. Traces as unreachable today (the column is admitted with a floor, and
			// layout only shrinks to it), which is precisely why it should not be the branch that
			// quietly reintroduces an unbudgeted cell if that ever changes.
			return ""
		}
		// From the RIGHT for prose, which reads left-to-right — the opposite of a path, whose
		// tail is the distinguishing end.
		return truncRight(title, titleW)
	}
	// THE WIDTH IS PASSED IN, not read back off the live table. The rows are built before
	// SetColumns installs the new header, so reading the table gave the PREVIOUS width: on a
	// narrowing resize the cell came out too wide, reached the table's fixed-width box, and was
	// re-truncated from the RIGHT — destroying the tail this function exists to keep, which is
	// the inversion it was written to prevent. Bounded in practice, since the next poll rebuilt
	// it correctly, so it showed only while polling was stalled.
	//
	// Passing it in also removes the header lookup, which had to go through headerTitle and
	// silently returned the untruncated title if it ever found nothing.
	if titleW <= 0 {
		// Same as the prose branch above: no budget means no cell, not an unbounded one.
		return ""
	}
	return truncLeft(title, titleW)
}

// looksLikePath reports whether a title should be truncated from the LEFT, keeping its tail.
//
// A LEADING SLASH IS NOT ENOUGH. It was the whole test until session titles started coming from the
// user's own prompts, and a typed slash command begins with one too: "/review <url> carefully" was
// left-truncated to "…pull/1101 carefully", discarding the command name — the one part of it a reader
// needs. That inverts this file's own rule that prose reads left-to-right.
//
// The distinction that matters is whether the string's LEAF is the identifying part. A filesystem path
// has more than one segment and no spaces in its first one; a slash command is one word followed by
// arguments. So a title qualifies only if it has a second "/" before any space — which "/review x"
// does not, and "/Users/somebody/src" does.
//
// Deliberately simple, and wrong in one direction on purpose: "/tmp foo" reads as prose and would be
// right-truncated. A single-segment path is not something Claude Code records as a cwd, and the cost
// is a cell cut at the other end rather than anything unbounded.
//
// THAT MISS IS PINNED, by TestLooksLikePath_SingleSegmentPathWithASpaceReadsAsProse, as
// characterization rather than endorsement — it also measures the cost, so widening this rule is a
// visible diff. The same test covers the opposite direction, which is NOT a miss: "/review a/b" is a
// slash command whose argument holds a slash, and prose is the right answer. One clause produces both,
// so neither behaviour can change alone.
//
// ANY UNICODE SPACE SEPARATES, not just " " and "\t". An earlier IndexAny(rest, " \t") saw no
// separator in "/review\u00a0docs/plan.md", found the second "/", and left-truncated the slash
// command — the exact regression above, reachable through a non-breaking space, an ideographic space,
// or any of the U+2000 block. Only from a stale or hand-edited metadata file today, since the
// harvester now folds every unicode space to U+0020 before writing; this does not rely on that,
// because a renderer should not assume its input came from the current harvester.
func looksLikePath(title string) bool {
	if !strings.HasPrefix(title, "/") {
		return false
	}
	rest := title[1:]
	if i := strings.IndexFunc(rest, unicode.IsSpace); i >= 0 {
		rest = rest[:i]
	}
	return strings.Contains(rest, "/")
}

// zeroWidthFree reports whether every rune in s occupies at least one display column.
//
// What licenses the prefix skip in truncLeft and truncRight: with no zero-width rune, a run of more
// than n runes cannot fit n columns, so n runes from the relevant end is an exact lower bound and
// every index beyond it is provably too wide to measure. One zero-width rune breaks that, and a
// differential test against the pre-skip implementation caught exactly that case.
//
// Checked structurally rather than by measuring: the classes runewidth gives zero columns are
// non-spacing and enclosing marks, format characters and controls, plus the modifier symbols this
// project already drops upstream. Cheap — one pass, no allocation — and the answer is yes for every
// title, so the skip is not hypothetical.
func zeroWidthFree(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) ||
			unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Sk, r) {
			return false
		}
	}
	return true
}

// truncRight clips s to n DISPLAY COLUMNS keeping the LEFT end, marking the cut with a
// trailing ellipsis.
//
// The mirror of truncLeft, for prose: a title reads left-to-right, so the opening words are
// the distinguishing end and the tail is what goes.
//
// Its own function rather than a fix to trunc, which this replaced at one call site: trunc
// counts RUNES and has four other callers (session ids, an events line, a test helper) that
// budget in characters and would change behaviour under a column-aware rewrite. Measured, the
// rune count is wrong by about 2x for the input this path actually gets: an 11-column budget
// returned 21 columns of CJK and 14 of emoji. Narrowing trunc's own gap is a separate change
// to those callers; this one bounds the cell that harvested titles flow into.
//
// The symptom it fixes is over-truncation, not a broken frame: the table's fixed-width box
// re-cuts an over-wide cell, so the line stayed at terminal width — a CJK title just lost
// characters it had budget for, and lost them at a point the renderer chose. Titles are
// model-generated text, so CJK and emoji are expected rather than exotic.
func truncRight(s string, n int) string {
	if lipgloss.Width(s) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	// Runes are dropped from the end until the remainder fits the budget. One at a time
	// rather than by arithmetic, for truncLeft's reason: a rune is 1 or 2 columns wide, so
	// there is no index that can be computed from the total.
	//
	// The ELLIPSIS PLUS THE HEAD is measured, not the head plus one, so a trailing combining
	// mark that fuses onto the ellipsis cannot push the result over budget.
	//
	// AND IT SKIPS AHEAD, for truncLeft's reason, under the same guard: a head longer than n runes
	// cannot fit n columns PROVIDED no rune is zero-width, so the first n runes are then an exact
	// lower bound. This had the identical quadratic shape — 2500 runes 39ms, 20000 2.30s on one call
	// — and is reachable the same way, since looksLikePath needs a leading "/" so a RELATIVE cwd
	// routes down this branch while just as uncapped.
	//
	// AND THE GUARD'S SLOW PATH RUNS HERE TOO, for the reason truncLeft now spells out: a title
	// served by /v1/sessions keeps its combining marks, so an accent or an emoji turns the skip off.
	// Prose is the COMMON shape for a served title — it is folded from a user's own message — so this
	// branch is the likelier of the two to meet one. Correctness is unaffected; the cost is bounded
	// by sessionTitleFor's cap, not by anything here.
	r := []rune(s)
	if len(r) > n && zeroWidthFree(s) {
		r = r[:n]
	}
	for i := len(r); i > 0; i-- {
		if out := string(r[:i]) + "…"; lipgloss.Width(out) <= n {
			return out
		}
	}
	return "…"
}

// truncLeft clips s to n DISPLAY COLUMNS keeping the RIGHT end, marking the cut with a leading
// ellipsis.
//
// The mirror of trunc, for values whose distinguishing end is the last one: a path, where every
// sibling shares the prefix. n < 1 yields "" and n == 1 yields just the ellipsis, so the result
// never exceeds the cell it was measured for.
func truncLeft(s string, n int) string {
	// DISPLAY COLUMNS, not runes. Every caller budgets in columns — a table cell's fitted
	// width, a header's remaining space — and a rune count is a different number the moment
	// the title is not ASCII. Titles are model-generated text or filesystem paths, which this
	// package documents as content nobody here controls, so CJK and emoji are expected rather
	// than exotic: measured, a 14-column budget returned 27 columns of CJK.
	//
	// The two failures differ and both are bad. In a header the over-wide string wraps the terminal,
	// costing a body row.
	//
	// In the table the cell is cut a SECOND time, and the result is worse than either end alone.
	// bubbles renders every cell through runewidth.Truncate(value, width, "…"), which keeps the HEAD
	// and appends its own ellipsis — so an over-wide left-truncation is cut again from the right,
	// with this function's leading ellipsis already in place. Measured on a 14-column budget:
	//
	//	truncLeft, correct     "…日日日日日日"      13 columns, tail kept, one ellipsis
	//	measured in runes      "…日日日日日日日日日日日日日"  27 columns
	//	  ...after the table   "…日日日日日日…"     14 columns, TWO ellipses, tail gone
	//
	// So the failure is not that left-truncation inverts into right-truncation — it is that the cell
	// ends up cut at BOTH ends and keeps the middle, which is the one part of a path that identifies
	// nothing. At a narrow budget it degenerates completely: an 11-column cell renders "…日日日日…" and
	// a 2-column one renders "……".
	//
	// lipgloss.Width, mirroring padLeft, which measures this way for the same reason.
	//
	// AND IT IS NOT THE LAST MEASUREMENT THE CELL MEETS. bubbles v1.0.0 runs runewidth.Truncate over
	// every cell before styling, and runewidth does not skip ANSI. Measuring here in display columns
	// is therefore necessary but not sufficient: it holds only while the cell is PLAIN, which for a
	// title is guaranteed upstream (core/observe/claude normalises a harvested one; sessionTitleFor
	// runs sanitizeLabel over a served one) and asserted below.
	// Styling a title would put escape bytes inside that second budget and collapse a narrow cell to
	// a lone ellipsis.
	if lipgloss.Width(s) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	// Runes are dropped from the front until the remainder fits the budget less the ellipsis.
	// One at a time rather than by arithmetic: a rune's width is 1 or 2, so there is no index
	// that can be computed from the total.
	//
	// BUT THE SEARCH SKIPS AHEAD FIRST, because measuring from i == 0 made this quadratic: each
	// iteration rebuilt the whole remaining tail and handed it to lipgloss.Width, so the cost grew
	// with the square of the input. Measured on one call, dropping runes one at a time from the
	// front: 2500 runes 36ms, 5000 142ms, 10000 572ms, 20000 2.33s — four times the input for
	// sixteen times the work. The same shape stripHarnessSpans was flagged for; these two were left.
	//
	// THE SKIP IS GUARDED, because the obvious bound is not universally true. "Every rune is at
	// least one column, so the last n runes are an exact lower bound" holds only while no rune is
	// ZERO columns — and combining marks, joiners and variation selectors all are. A differential
	// test against the old implementation caught it at n == 1: on a string of combining marks the
	// bound skipped past marks the one-at-a-time search would have kept, and the two returned
	// different bytes.
	//
	// So skipping is conditional on the premise: zeroWidthFree reports whether s contains any
	// zero-width rune, and only then is the prefix provably untestable.
	//
	// THE SLOW PATH IS REACHABLE, and the comment here used to deny it. It read that a title never
	// contains a zero-width rune because core/observe/claude drops every Mn/Me/Cf/Cc/Sk — true of a
	// HARVESTED title, and no longer the only kind. A title served by /v1/sessions goes through
	// core/session.sanitizeTitle instead, which deliberately KEEPS combining marks (its own doc: "so
	// café survives"), and pipeline.IsControlRune covers C0/C1/DEL/BIDI/Cf but not Mn/Me/Sk. So an
	// accented word or any ordinary emoji — U+FE0F VARIATION SELECTOR-16 is Mn — makes zeroWidthFree
	// false and runs the quadratic search this comment claimed never executes. Measured on one call
	// with a combining mark every other rune: 2503 runes 72ms, 5003 287ms, 10003 1.12s, 20003 4.50s,
	// which is worse than the ASCII figures above because each measurement now folds a mark too.
	//
	// The guard is STRUCTURAL, so the answer stays correct either way — this is about the premise,
	// not a bug. It is called out because the premise is what someone would delete the guard on, and
	// because the cost is only bounded by sessionTitleFor capping its input; see the cap there.
	r := []rune(s)
	start := 0
	if len(r) > n && zeroWidthFree(s) {
		start = len(r) - n
	}
	for i := start; i < len(r); i++ {
		// The ELLIPSIS PLUS THE TAIL is measured, not the tail plus one. A tail that begins
		// with a combining mark or a variation selector fuses onto the ellipsis, so the pair
		// is narrower than the sum of its parts — and assuming the ellipsis always adds
		// exactly one column let the result exceed the budget. Measuring what is actually
		// returned cannot be wrong about it.
		if out := "…" + string(r[i:]); lipgloss.Width(out) <= n {
			return out
		}
	}
	return "…"
}
