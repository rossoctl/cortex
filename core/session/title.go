package session

import (
	"strings"
	"unicode"

	"github.com/rossoctl/cortex/core/capabilities"
	"github.com/rossoctl/cortex/core/pipeline"
)

// maxTitleLen caps a title, in RUNES — a user message is unbounded (the largest measured event
// carries ~190KB) and this is served on every /v1/sessions response. Runes, not bytes, so a
// multi-byte title is not cut mid-character, but NOT a display-column budget: 80 runes of CJK
// occupy 160 columns, so a client still truncates by width.
//
// Deliberately the same 80 as core/observe/claude.MaxTitleLen and deliberately NOT that constant,
// which caps a HARVESTED title and is exported for cmd/abctl's contract with the harvester.
// Importing it would make the session store depend on the transcript harvester for a number and
// couple two caps that answer to different consumers. If they ever need to differ, they can.
const maxTitleLen = 80

// Title sources, best first. Lower wins; rankNone means "named nothing".
//
// WITHIN ONE RANK THE TIE-BREAK DEPENDS ON THE SCOPE, and the two differ: titleCandidate takes the
// LAST match among one event's messages, while Store.Append's fold across events keeps the FIRST
// unless the tie is at rankRename. See entry.Title for why that pairing is what it wants.
//
// A COMMAND'S ARGUMENTS RANK AS ORDINARY PROSE, deliberately. They beat the boilerplate they expand
// to by being SELECTED over it inside one message (see stripLocalCommands), not by outranking it — a
// rank is session-global, so a rank above prose would also displace an earlier turn's real prompt.
const (
	rankRename    = 0
	rankUserQuery = 1
	rankUserMsg   = 2
	rankNone      = 3
)

const renamePrefix = "<command-name>/rename</command-name>"

// titleCandidate returns the best-ranked title this event's user messages offer.
//
// ONE REVERSE SCAN, NO RETRIES — reverse so that the first acceptance at any rank is the LAST such
// message in event order, which is this scope's tie-break rule. Retrying the scan whenever
// titleFrom refused a pick was quadratic in messages-per-event, 31.9µs at n=50 rising to 7.20ms at
// n=800; BenchmarkSessionTitle_ReminderFanout is what holds that down.
//
// TWO CLASSES OF MESSAGE, because quickRank is a bound and a bound is not a rank:
//
//   - a guess BETTER than rankUserMsg (rare, since the tags and the leading machinery are) is
//     SETTLED EAGERLY by calling titleFrom, because such a guess can be demoted and must not be
//     recorded before it is settled.
//   - a rankUserMsg guess (the majority) is DEFERRED to the second loop below: calling titleFrom on
//     every message cost +70%, so the scan remembers the index instead. This is the one direction
//     of the bound that holds — a message with no <user_query>, no leading machinery and no
//     reminder-hidden /rename prefix has nothing for the strip to uncover. See quickRank.
func titleCandidate(e *pipeline.SessionEvent) (int, string) {
	if e.Inference == nil {
		return rankNone, ""
	}
	msgs := e.Inference.Messages
	bestRank, bestTitle, bestIdx := rankNone, "", -1
	// deferredHead is the index one past the newest rankUserMsg guess not yet settled. The deferred
	// candidates are scanned lazily below, latest first, because such a guess can still settle at
	// rankNone — a message that is nothing but reminders guesses rankUserMsg and names nothing — and
	// the next one back must then get its turn. Storing a single index lost exactly that case.
	deferredHead := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != capabilities.RoleUser {
			continue
		}
		switch quickRank(msgs[i].Content) {
		case rankNone:
			continue
		case rankUserMsg:
			if deferredHead < 0 {
				deferredHead = i + 1
			}
			continue
		}
		// A guess better than rankUserMsg. Settle it now; titleFrom may demote or refuse it.
		r, t := titleFrom(msgs[i].Content)
		if t == "" || r >= bestRank {
			continue
		}
		// REJECT A BLANK CANDIDATE BEFORE IT CLAIMS THE RANK, because no later screen can: a
		// caller sees ONE answer per event, so a blank winner here discards the whole event
		// rather than falling through to a real title inside it.
		//
		// foldsBlank rather than sanitizeTitle, which is why foldsBlank exists: this runs per
		// candidate message and folding here measured 326µs→635µs on
		// BenchmarkListSessions_Title/user-text. Only the winner is folded, by Store.Append.
		if foldsBlank(t) {
			continue
		}
		bestRank, bestTitle, bestIdx = r, t, i
		// PURE OPTIMIZATION — UNOBSERVABLE. `r >= bestRank` above already rejects every later
		// candidate once bestRank is 0, and the deferred loop's gate fails at 0 too, so deleting
		// this changes no result. Kept because the remaining scan is provably wasted work.
		if bestRank == rankRename {
			return bestRank, bestTitle // nothing in this event can outrank it
		}
	}
	// Settle the deferred rankUserMsg candidates only if nothing better was found — and, at equal
	// rank, only if the deferred one is LATER in event order, per the last-match rule.
	//
	// THE INDEX COMPARISON IS NOT REDUNDANT. The scan runs newest-first, so the first deferred guess it
	// meets is the newest — but an eager guess that titleFrom DEMOTES to rankUserMsg can sit anywhere,
	// including after deferredHead, and is then the later of the two.
	//
	// THE GATE MUST NOT TIGHTEN TO rankNone, which (being the numerically LARGEST rank) is the
	// stricter test: it skips the loop whenever the eager pass settled anything, and a settled
	// demotion is exactly when the loop still has work.
	// TestSessionTitle_DeferredOnlyWithNothingSettled pins both of these.
	//
	// Both loops visit the prefix below deferredHead. Skipping the already-settled indices is
	// possible and measured slower than the redundancy (8.8µs against 12.3µs), because the break
	// below stops this loop within an index or two of deferredHead.
	if deferredHead >= 0 && bestRank >= rankUserMsg {
		for i := deferredHead - 1; i >= 0; i-- {
			// No quickRank here: re-classifying costs a second scan of every message (+45% on
			// prose), and titleFrom is the authority anyway. A message that guessed better than
			// rankUserMsg and reaches this point was already settled and rejected by the loop above.
			if msgs[i].Role != capabilities.RoleUser {
				continue
			}
			r, t := titleFrom(msgs[i].Content)
			// foldsBlank, NOT sanitizeTitle — see the eager arm above for the 2x this avoids.
			if foldsBlank(t) {
				// Named nothing. Keep walking back: an earlier message in this same event may still
				// title it — TestSessionTitle_DemotedPickFallsBackWithinEvent pins that fall-through.
				continue
			}
			// `r > bestRank` is unreachable today, rankNone being the only worse rank and always
			// carrying t == "". Written as the full comparison so the rule — better rank wins, then
			// later index — lives in the code rather than only in a comment.
			if r > bestRank || (r == bestRank && i < bestIdx) {
				break // the already-settled pick is better ranked, or equally ranked and later
			}
			return r, t
		}
	}
	return bestRank, bestTitle
}

// quickRank guesses a message's rank without stripping reminders, cheaply enough to run on
// every message of every event.
//
// A SCREEN, NOT A RANK. titleFrom is the only authority on what a message settles at; this exists
// solely to keep titleFrom off the messages where the answer is almost always rankUserMsg.
//
// THE BOUND HOLDS IN ONE DIRECTION ONLY — the guess can be too GOOD but never too bad, which is the
// asymmetry titleCandidate's two loops are shaped around: a guess better than rankUserMsg MUST be
// settled by titleFrom before its rank is recorded, a rankUserMsg guess can be DEFERRED. It
// over-promises in two ways, both tested (rankUserQuery for an unterminated <user_query>; a
// tag-bearing rank for a payload that is empty once extracted), and the deferred arm still calls titleFrom
// rather than recording the guess, because a rankUserMsg guess can settle at rankNone — a message
// that is nothing but reminders.
//
// IT CANNOT UNDER-PROMISE, AND THAT RESTS ON stripReminders' SPLICE SEPARATOR: a splice that could
// MANUFACTURE a tag would let a deferred rankUserMsg guess settle at rankUserQuery or a sticky
// rankRename, making the deferral unsound rather than merely lazy. See stripReminders, and do not
// weaken it.
//
// Contains for BOTH tags, not HasPrefix for the /rename envelope: HasPrefix systematically
// under-rates a reminder-prefixed /rename to rankUserMsg, which is how a later genuine /rename
// loses to an earlier one inside the same event. The prefix test belongs in titleFrom, where the
// reminders are already stripped and a prefix is the right question.
func quickRank(content string) int {
	if content == "" {
		return rankNone
	}
	if strings.HasPrefix(content, renamePrefix) {
		return rankRename
	}
	if strings.Contains(content, "<user_query>") {
		return rankUserQuery
	}
	// MACHINERY AT THE FRONT is the only thing that can displace a /rename envelope from the front,
	// which is why the HasPrefix above misses one and this exists. Everything titleFrom strips before
	// testing its own prefix has to be tolerated here, or the guess lands under where titleFrom
	// settles — and a deferred rankUserMsg guess is only sound when the guess cannot be too low. THE
	// ANCHORS ARE A COST DECISION: an unanchored Contains for the 37-byte envelope, scanned over the
	// whole message to answer "no" for almost all of them, cost +45% on 500-byte prose
	// (325µs→472µs), and gating it on an unanchored leading test cost the same. `<` discriminates
	// nothing — prose with a code snippet has one. Machinery behind PROSE is deliberately not caught:
	// titleFrom's strip anchors too, so it leaves such a message whole and settles it at rankUserMsg,
	// which is exactly what falling past this block guesses.
	//
	// The second test trims first, matching the tolerance titleFrom's strip has, so the two agree on
	// every shape rather than only the ones a splice happens to produce, and it asks the same
	// question stripLocalCommands anchors on rather than a copy of it. The trim runs on the tail of a
	// HasPrefix that has already said no for almost every message.
	if strings.HasPrefix(content, reminderOpen) || stripsAsLocalCommand(content) {
		if strings.Contains(content, renamePrefix) {
			return rankRename
		}
		// Over-promises freely: such a message settles at rankUserMsg on surviving prose or arguments,
		// or at rankNone on nothing. titleFrom decides which.
		return rankUserMsg
	}
	return rankUserMsg
}

// titleFrom ranks one user message and returns the title it offers.
//
// <system-reminder> blocks are excised first, before every rank arm rather than only the
// rankUserMsg one. All three need it and two are wrong without it: a reminder nested INSIDE a
// <user_query> rides along into the title, and a <user_query> nested inside a REMINDER (one quoting
// an earlier turn is enough) is mistaken for the real ask. The /rename arm tests a PREFIX, so a
// reminder in front of an envelope hides it entirely.
//
// THE STRIP IS NOT FREE, which is why titleCandidate does not call this on every message: excising a
// tag that can sit anywhere means scanning the whole message and building a new string, and per
// message that cost +70% on BenchmarkListSessions_Title/user-text (~45k messages of ~500 bytes).
// quickRank screens instead; this settles only the guesses that are not reliable.
func titleFrom(content string) (int, string) {
	content = stripReminders(content)
	// Before the arms below, which test prefixes this block would hide.
	content, fromArgs := stripLocalCommands(content)
	// Returned before the arms below, not through them: this is an extracted body, so re-testing it
	// would rank a tag quoted inside someone's arguments as though the message carried one.
	if fromArgs {
		return rankUserMsg, content
	}
	// A TRANSCRIPT ENVELOPE IS MACHINERY, NOT AN ASK. Observed live: a message opening with this tag
	// titled a session `\", \"` — the fold reduced a wall of quoted JSONL to its punctuation.
	// Discarded rather than ranked, so the walk reaches a real title behind it.
	//
	// ANCHORED, WITH NO LEADING-WHITESPACE TOLERANCE, and every arm below anchors for the same reason:
	// of the local-transcript user messages mentioning this tag, the only one does so as PROSE — a bug
	// report quoting it — so an unanchored match discards exactly the message a reader wants.
	if strings.HasPrefix(content, transcriptOpen) {
		return rankNone, ""
	}
	// AN ENVELOPE THAT IS THE WHOLE MESSAGE YIELDS ITS BODY OR NOTHING, and must never fall through
	// to the generic rankUserMsg arm: falling through takes the LITERAL MARKUP as the title, which is
	// non-blank, so foldsBlank cannot reject it and under first-wins it blocks the session's real
	// title for the rest of its life. rankNone lets the walk reach a real title behind it instead.
	//
	// Each arm anchors "the whole message" differently — HasPrefix here, since a /rename body follows
	// its tag; both ends below, since an envelope body sits between them.
	if strings.HasPrefix(content, renamePrefix) {
		if t := between(content, "<command-args>", "</command-args>"); t != "" {
			return rankRename, t
		}
		return rankNone, ""
	}
	if strings.Contains(content, "<user_query>") {
		// BLANK, NOT EMPTY. The two blank shapes arrive by different routes: an empty body yields ""
		// from between() and falls to the generic arm (markup as title, per above), while a whitespace
		// body reaches rankUserQuery — the worse lie, since it outranks genuine prose. foldsBlank covers
		// both, and being sanitizeTitle's own predicate it still ranks any body that would survive
		// the fold.
		//
		// ONE ACCEPTED LIMIT follows from the anchor: a trailing non-blank byte defeats the suffix, so
		// "<user_query></user_query>x" is still titled with its own markup. Widening to "both tags
		// present anywhere" fixes it and regresses the prose case — the trade this shape declines.
		// TestSessionTitle_EnvelopeAnchorLimits pins both halves, the fixed and the accepted.
		if t, ok := wholeMessageEnvelope(content); ok {
			if foldsBlank(t) {
				return rankNone, ""
			}
			return rankUserQuery, t
		}
		if t := between(content, "<user_query>", "</user_query>"); t != "" {
			return rankUserQuery, t
		}
	}
	// A user-role message whose payload was a tool result or an image flattens to "" (see
	// pipeline.InferenceMessage.ContentBytes) — it is the last message of every agentic turn
	// and must not be chosen as the title. A message that was nothing BUT reminders reaches
	// here as "" too, and falls through by the same route: rankNone, so the walk finds a real
	// title behind it.
	if content != "" {
		return rankUserMsg, content
	}
	return rankNone, ""
}

const (
	reminderOpen  = "<system-reminder>"
	reminderClose = "</system-reminder>"
	// transcriptOpen is matched as a PREFIX only, and with no attribute tolerance: the opening
	// tag observed in the harness payload is exactly this, and every widening of the match moves
	// it toward the prose case that must keep working. See titleFrom.
	transcriptOpen = "<transcript>"

	localCommandOpen  = "<local-command-caveat>"
	localCommandClose = "</local-command-caveat>"
)

// argsOpen / argsClose wrap the arguments a user typed after a command name. Named because
// stripLocalCommands keeps that one body rather than only discarding it; the table below refers to
// these so the two cannot drift apart.
const (
	argsOpen  = "<command-args>"
	argsClose = "</command-args>"
)

// localCommandEnvelopes are tag pairs that wrap local-command machinery rather than a user's own
// words. Extend the table to cover more of them.
var localCommandEnvelopes = [][2]string{
	{"<command-message>", "</command-message>"},
	{"<command-name>", "</command-name>"},
	{argsOpen, argsClose},
	{"<local-command-stdout>", "</local-command-stdout>"},
}

// stripLocalCommands drops a leading local-command block and any envelopes following it, preferring
// the arguments those envelopes carried and falling back to whatever text remains.
//
// fromArgs tells the caller the text came from arguments, which is what lets it skip re-testing an
// extracted body for envelopes. Both outcomes rank the same; see the rank constants.
//
// A BLANK ARGUMENT BODY IS THE WHOLE TEST for preferring arguments over surviving text — nothing
// inspects that text, so this needs no notion of what an expansion looks like. Consecutive user
// messages arrive concatenated, so text after the envelopes is either what the user typed next or the
// boilerplate the command expanded to, and the two divide on blankness: an invocation WITH arguments
// is followed by its expansion, while commands typed ahead of the user's own prose (/clear and
// friends) carry an empty pair.
//
// Do not survey the on-disk transcripts for this shape — they record invocation and expansion as
// separate entries and show it zero times. Only the concatenated form the proxy receives has it.
//
// ANCHORED, like every other tag test here: a message merely mentioning the tag as prose keeps its
// own words. Unanchored, it lost them.
//
// THE ANCHOR IS THE CAVEAT **OR** ANY ENVELOPE OPENER, because the caveat is not always what comes
// first — a message can open directly on an envelope, and requiring the caveat left those showing
// their own markup as the title.
//
// LEADING WHITESPACE IS TOLERATED ON THE MACHINERY PATH ONLY, because this runs after stripReminders
// and that splices with a space — so a block the reminder used to precede arrives one space in. The
// original is returned otherwise: trimming unconditionally would hand the arms below a string they
// did not receive before, and their anchors deliberately do not tolerate whitespace.
func stripLocalCommands(orig string) (text string, fromArgs bool) {
	if !stripsAsLocalCommand(orig) {
		return orig, false
	}
	// Past the caveat block if there is one; a message can also open straight into the envelopes.
	rest := strings.TrimLeft(orig, " \t\r\n")
	if strings.HasPrefix(rest, localCommandOpen) {
		j := strings.Index(rest, localCommandClose)
		if j < 0 {
			// Truncated block, not prose — the anchor already ruled prose out. Dropped, because the
			// boilerplate is byte-identical across sessions and non-blank titles stick under first-wins.
			return "", false
		}
		rest = rest[j+len(localCommandClose):]
	}
	// args holds the last <command-args> body seen — the last, because a message can carry several
	// invocations and the one a title should name is the one the user ended on.
	args := ""
	// Each iteration must consume something or stop, so a malformed tail cannot loop.
	for {
		trimmed := strings.TrimLeft(rest, " \t\r\n")
		// A /rename is a title the user typed, not machinery: hand the arms below the envelope
		// they test for rather than consuming it.
		if strings.HasPrefix(trimmed, renamePrefix) {
			return trimmed, false
		}
		open, next := "", ""
		for _, env := range localCommandEnvelopes {
			if strings.HasPrefix(trimmed, env[0]) {
				open, next = env[0], env[1]
				break
			}
		}
		if next == "" {
			// Non-empty arguments beat whatever survived them; see the doc comment.
			if args != "" {
				return args, true
			}
			return trimmed, false
		}
		k := strings.Index(trimmed, next)
		if k < 0 {
			// Unterminated envelope: dropped rather than kept, so markup cannot become a title. Args
			// captured from a well-formed earlier envelope still stand.
			if args != "" {
				return args, true
			}
			return "", false
		}
		if open == argsOpen {
			args = strings.TrimSpace(trimmed[len(open):k])
		}
		rest = trimmed[k+len(next):]
	}
}

// stripsAsLocalCommand reports whether stripLocalCommands would treat s as machinery rather than
// prose. ONE DEFINITION OF THE ANCHOR, shared with quickRank: a second copy of this test is how the
// screen comes to disagree with where titleFrom settles, which is the one direction the screen may
// not be wrong in.
func stripsAsLocalCommand(s string) bool {
	s = strings.TrimLeft(s, " \t\r\n")
	return strings.HasPrefix(s, localCommandOpen) || opensEnvelope(s)
}

// opensEnvelope reports whether s begins with one of the envelope openers.
func opensEnvelope(s string) bool {
	for _, env := range localCommandEnvelopes {
		if strings.HasPrefix(s, env[0]) {
			return true
		}
	}
	return false
}

// stripReminders returns what surrounds the reminder blocks: everything before the FIRST
// <system-reminder> joined to everything after the LAST </system-reminder>. The harness injects
// these blocks into the user turn it is attached to, so without this the block itself becomes the
// title and the real prompt — which sits after it — is never reached.
//
// DELIBERATELY UNBALANCED, and that is a DoS fix. Pairing each open with its own close by depth
// re-scanned the tail once per nesting level: a 190KB message of 11,444 nested open tags took
// 593ms, and this runs under the store's write lock on the proxy request path. Two index scans
// cannot go quadratic regardless of what the content nests — 250µs on that same fixture with no
// close tag anywhere (the worst case: LastIndex scans the whole payload for a close that is not
// there), 31ns once a close is present.
//
// ONE ACCEPTED LOSS: PROSE BETWEEN TWO BLOCKS IS DROPPED. "<sr>a</sr>mid<sr>b</sr>tail" yields
// "tail". The trade is one mangled title against another, not against a clean read: per-block
// excision would yield "midtail", since `mid` and `tail` abut two different blocks rather than one
// separator. Measured across 550 local transcripts (2804 user text messages, 253 reminder-bearing):
// 8 of that shape, against 242 reminder-only and 3 with an unclosed open.
//
// Called once at the top of titleFrom, so every rank arm sees content with the blocks already gone.
// Matched literally in lowercase, the only form the harness emits into an inference payload, and on
// RAW content — folding U+0085 to a space first would turn "<\u0085system-reminder>" into a non-tag
// and hide the block from this scan.
func stripReminders(s string) string {
	// The common case is no reminder at all, and this runs on every message of every event: a
	// session whose best rank is rankUserMsg can never terminate the reverse walk early, since it is
	// always beatable. One Index over the message beats building a string for each.
	i := strings.Index(s, reminderOpen)
	if i < 0 {
		return s
	}
	// LastIndex, not Index: pairing the first open with the FIRST close is what leaks a stray
	// "</system-reminder>" into the title when the blocks nest, since the inner close arrives
	// first. Searching the whole string from the right is one scan either way.
	j := strings.LastIndex(s, reminderClose)
	if j < i {
		// NO CLOSE AFTER THE FIRST OPEN, so there is no block to excise — just a tag name sitting in
		// prose. Return the message UNCHANGED rather than its head, matching titleFrom's policy for
		// an unclosed <user_query>: returning s[:i] serves "why is" for "why is <system-reminder>
		// leaking into my session titles?", which is non-blank, so under first-wins it claims rankUserMsg
		// and blocks the session's real title permanently.
		//
		// Compared against i, not 0: a close sitting in prose BEFORE the first open is not the end
		// of a block either, and splicing on it would run backwards.
		return s
	}
	// A SPACE, AND IT IS A SECURITY BOUNDARY — see quickRank, whose one-directional bound rests on
	// it. Head and tail were never adjacent, so joining them bare both fuses words across the gap
	// ("my question<sr>noise</sr>and the follow-up" → "my questionand the follow-up") and lets a
	// halved tag REASSEMBLE, which is the serious half: no tag in the vocabulary contains a space, so
	// the separator is the only thing stopping a client synthesizing a rankUserQuery or sticky rankRename title
	// out of markup it never sent. TestSessionTitle_SpliceCannotManufactureATag is what catches that
	// — the fusion case is not — so do not "simplify" the separator away.
	//
	// NOT WHEN THE HEAD IS EMPTY, and that guard is load-bearing too. Splicing unconditionally looks
	// safe because sanitizeTitle drops leading whitespace, but titleFrom reads this string BEFORE
	// anything trims it, and its rankRename and transcript arms are HasPrefix tests that a leading space
	// defeats — every reminder-prefixed /rename would demote from rankRename to rankUserMsg. It is also the
	// common case: 242 of the 253 reminder-bearing messages in the corpus are reminder-only, landing
	// here with an empty head that must stay exactly "" for foldsBlank to screen it.
	//
	// The mirror case needs no guard: a trailing block leaves a trailing space, and no anchored test
	// looks at the end of the string — every arm either extracts up to a close tag or is TrimRighted
	// by sanitizeTitle, including at the clip boundary.
	head := s[:i]
	if head == "" {
		return s[j+len(reminderClose):]
	}
	return head + " " + s[j+len(reminderClose):]
}

// wholeMessageEnvelope returns the body of a <user_query> envelope that is the ENTIRE message,
// modulo surrounding whitespace, and reports whether the message has that shape at all. The
// body may be empty or blank — deciding what that means is titleFrom's job, not this one's.
//
// Both ends anchor, and a tag inside the span declines: that is what separates a whole-message
// envelope from a tag pair quoted in prose, from an unterminated open, and from markup that
// mis-pairs across two envelopes. See titleFrom's user_query arm for why each of those matters.
func wholeMessageEnvelope(s string) (string, bool) {
	const open, closing = "<user_query>", "</user_query>"
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, open) || !strings.HasSuffix(s, closing) || len(s) < len(open)+len(closing) {
		return "", false
	}
	body := s[len(open) : len(s)-len(closing)]
	if strings.Contains(body, open) || strings.Contains(body, closing) {
		return "", false
	}
	return body, true
}

// between returns the text bracketed by open and closing, or "" if either is absent.
//
// Hand-parsed rather than a regexp: matching a tag pair wants a backreference, which RE2
// lacks. A local copy of core/observe/claude's unexported helper — importing a transcript
// harvester into the session store for a string helper is the wrong dependency direction.
// Unlike the original this one does not TrimSpace; sanitizeTitle trims at the end.
func between(s, open, closing string) string {
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, closing)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// sanitizeTitle folds a title to a single line: every whitespace or control rune becomes at
// most one U+0020, and the result is clipped to maxTitleLen runes.
//
// Applied ONCE, to the winner — not per candidate and not per event. Both were measured: either
// one roughly doubles BenchmarkListSessions_Title/user-text (326µs→635-651µs), because rankUserMsg never
// terminates the walk so every message would pay a fold. foldsBlank supplies the one thing that
// does have to happen earlier — a candidate folding to blank must not claim a rank — as a predicate
// that builds no string.
//
// Rank never depends on whitespace, so comparing raw strings is safe. And applied to the
// EXTRACTED title, never the haystack: folding U+0085 to a space turns "<\u0085command-name>"
// into "< command-name>", which is no longer a tag.
//
// O(maxTitleLen), NOT O(len(s)), and that is what lets it stay under the store's write lock. It
// stops as soon as maxTitleLen runes are emitted, so a 190KB candidate costs the same as an 80-rune
// one: 916µs and 958KB of allocation against 366ns and 320B, where 320 is `maxTitleLen * 4` (the
// b.Grow below) rather than a figure to re-measure.
//
// NO LOOKAHEAD IS NEEDED PAST THE STOP, which is why the early exit is correct and not merely fast:
// the fold never revisits an emitted rune, and the only thing a later rune could affect is the
// TrimRight, which a folded space at rune 80 gets whether or not more input follows. Counting
// EMITTED runes rather than input consumed is load-bearing too: a whitespace run emits at most one.
//
// A PLAIN RUNE BOUNDARY, which the early stop does not change: it can sever a combining mark from
// its base, leaving a dangling accent. core/observe/claude's clip is exempt because its scrubRunes
// drops every combining mark first; this one keeps them, so "café" survives and the cut is the
// price. Grapheme clusters need a segmentation library core/ does not have.
func sanitizeTitle(s string) string {
	var b strings.Builder
	// Sized to the OUTPUT, not the input. 4 bytes per rune is the max UTF-8 encoding, so this is
	// one allocation for any input, where b.Grow(len(s)) was proportional to a hostile message.
	b.Grow(maxTitleLen * 4)
	emitted := 0
	prevSpace := true // drops leading whitespace
	for _, r := range s {
		if emitted == maxTitleLen {
			break
		}
		// TWO PREDICATES, and each catches runes the other misses. pipeline.IsControlRune covers
		// C0/C1/DEL plus the BIDI and zero-width runes that reorder or hide their surroundings.
		// unicode.IsSpace covers \n\r\t, the exotic spaces (U+00A0, U+3000, the U+2000 block) —
		// and, most importantly for a single-line guarantee, U+2028 LINE SEPARATOR and U+2029
		// PARAGRAPH SEPARATOR, which are the runes most likely to break a title across lines and
		// are NOT control runes. Both FOLD rather than drop, so "one\ntwo" does not become
		// "onetwo".
		if unicode.IsSpace(r) || pipeline.IsControlRune(r) {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
				emitted++
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
		emitted++
	}
	// TrimRight, not clipTitle: the loop cannot emit more than maxTitleLen runes, so there is
	// nothing left to clip. The trim is still needed because the stop can land just after a
	// folded space, exactly as the old cut could.
	return strings.TrimRight(b.String(), " ")
}

// foldsBlank reports whether sanitizeTitle(s) would be "" — that is, whether s holds no rune the
// fold keeps. Exactly sanitizeTitle's own predicate, negated: change one and change both.
//
// Separate from sanitizeTitle because titleCandidate must REJECT a blank candidate without folding
// it. Folding there instead is a 2x on prose (326µs→635µs on BenchmarkListSessions_Title/user-text):
// sanitizeTitle is 2.8µs on a 500-byte message and titleCandidate examines every message of every
// appended event, where Store.Append folds only a candidate that already won its rank. This
// allocates nothing and returns at the first kept rune, so a real message pays one rune's worth of
// work.
func foldsBlank(s string) bool {
	for _, r := range s {
		if !unicode.IsSpace(r) && !pipeline.IsControlRune(r) {
			return false
		}
	}
	return true
}
