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
// which caps a HARVESTED title and is exported for cmd/agentop's contract with the harvester.
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

// A tag is three independent facts: how to recognize it, what to take from it, and what rank that
// earns. titleRules states each as a row; titleFrom is a loop over it. Add a tag by adding a row.
//
// stripReminders and stripLocalCommands stay out of the table. They rewrite the message before any row
// is tested, and neither is a plain "strip this tag": the reminder splice separator is a security
// boundary, and the local-command walk prefers a <command-args> body over what survives it.

// tagMatch is how a rule recognizes its tag.
type tagMatch int

const (
	// matchPrefix: the tag opens the message. The anchor is what keeps a message that merely MENTIONS
	// the tag as prose — a bug report quoting it — from being treated as machinery.
	matchPrefix tagMatch = iota
	// matchContains: the tag may sit anywhere. Only <user_query>, which the harness nests.
	matchContains
)

// tagTake is what a rule extracts once its tag matched.
type tagTake int

const (
	// takeDiscard: the message is machinery and names nothing.
	takeDiscard tagTake = iota
	// takeFirstLine: the body between open and close, reduced to one line by firstLine.
	takeFirstLine
	// takeArgs: the <command-args> body, wherever it sits. The /rename body follows its tag rather than
	// sitting inside it.
	takeArgs
	// takeQueryBody: the body, preferring a whole-message envelope over a bracketed one.
	takeQueryBody
)

// titleRule is one tag's row: recognize `open`, extract per `take`, rank the result `rank`.
type titleRule struct {
	open  string
	close string // the matching close tag; unused by takeDiscard and takeArgs
	match tagMatch
	take  tagTake
	rank  int
}

// takeOutcome says what a blank extraction means. The two are not interchangeable.
//
// It is a property of which extraction path hit, not of the row: a <user_query> claims as a
// whole-message envelope and declines as a loose tag in prose.
type takeOutcome int

const (
	// takeClaimed: the row is the machinery here. titleFrom answers rankNone, so the walk looks behind
	// it. This is what stops the literal markup becoming the title — markup is non-blank, so foldsBlank
	// cannot reject it, and under first-wins it would block the real title permanently.
	takeClaimed takeOutcome = iota
	// takeDeclined: the tag was probably not acting as a tag. The message keeps its own words at the
	// generic prose arm. "what does <user_query> mean in this code" is prose ABOUT the tag.
	takeDeclined
)

// titleRules is the ordered tag table titleFrom walks. First match wins.
//
// ORDER IS PRECEDENCE AND IS LOAD-BEARING. Two orderings are deliberate:
//
//   - <transcript> leads because it discards. Its body is quoted JSONL that can contain any other tag
//     here, so a row above it would mine a title out of someone else's quoted turn.
//   - /rename sits above <user_query>, matching the rank constants. Both can appear in one message.
//
// The three leading envelopes are mutually exclusive in practice; their relative order says nothing.
var titleRules = []titleRule{
	// Machinery, not an ask. Observed live: a message opening with this tag titled a session `\", \"`,
	// the fold having reduced a wall of quoted JSONL to its punctuation.
	{open: transcriptOpen, match: matchPrefix, take: takeDiscard, rank: rankNone},

	// A leading envelope's body is the title; what follows the close is machinery. The close need not
	// end the message — captured output follows <bash-input>, instructions follow <session>.
	//
	// <conversation> is a handoff brief whose body is a markdown document, which is what
	// takeFirstLine's heading rule is for. It is matched on its open alone like the other two even
	// though live messages do close on it: a both-ends match would let one trailing byte drop the
	// message to the generic arm and put its own markup in the title.
	{open: "<bash-input>", close: "</bash-input>", match: matchPrefix, take: takeFirstLine, rank: rankUserMsg},
	{open: "<session>", close: "</session>", match: matchPrefix, take: takeFirstLine, rank: rankUserMsg},
	{open: "<conversation>", close: "</conversation>", match: matchPrefix, take: takeFirstLine, rank: rankUserMsg},

	// The one title the user typed, so it outranks everything.
	{open: renamePrefix, match: matchPrefix, take: takeArgs, rank: rankRename},

	// The only unanchored row, and so the only one that can decline a message rather than claim it: an
	// unanchored match can land on prose discussing the tag. See takeQueryBody.
	{open: "<user_query>", close: "</user_query>", match: matchContains, take: takeQueryBody, rank: rankUserQuery},
}

// screenedTags returns the rows quickRank must test: those ranking above rankUserMsg. A row at
// rankUserMsg or rankNone needs no test, because quickRank's fall-through already guesses rankUserMsg
// and its bound only has to hold downward.
//
// quickRank's tests are written out by hand rather than driven from this, because it runs on every
// message of every event and a slice loop there is measurably slower. This exists so the duplication
// cannot drift: TestScreenedRules_CoversEveryHighRankingTag checks quickRank against these rows. A tag
// the screen misses is guessed rankUserMsg, deferred, and then recorded without titleFrom ever
// settling it.
func screenedTags() []screenRow {
	var out []screenRow
	for _, r := range titleRules {
		if r.rank < rankUserMsg {
			out = append(out, screenRow{open: r.open, prefix: r.match == matchPrefix, rank: r.rank})
		}
	}
	return out
}

// screenRow is one tag that must be screened: the tag, whether it is anchored, and the rank a hit
// guesses.
type screenRow struct {
	open   string
	prefix bool
	rank   int
}

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
	// A request capped at one output token is a probe (e.g. Claude Code's "quota" check), not a
	// turn, so it names nothing — not even through a /rename.
	if mt := e.Inference.MaxTokens; mt != nil && *mt <= 1 {
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
// IT SCREENS ONLY THE TAGS THAT RANK ABOVE rankUserMsg, which is what keeps it cheap. A tag ranking at
// rankUserMsg (the leading envelopes) or at rankNone (<transcript>) needs no test here: falling through
// to the rankUserMsg guess below is already sound for it, the bound being allowed to be too good but
// never too bad. screenedTags selects that set from titleRules for the test below.
func quickRank(content string) int {
	if content == "" {
		return rankNone
	}
	// Hand-written, one comparison per high-ranking tag, and kept in step with titleRules by
	// TestScreenedRules_CoversEveryHighRankingTag rather than by sharing its loop. Each test's anchor
	// matches its row's match column: /rename is a prefix, <user_query> is not.
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
	// Before the table, whose anchored rows this block would otherwise hide.
	content, fromArgs := stripLocalCommands(content)
	// Returned WITHOUT consulting the table: this is an extracted body, so testing it would rank a tag
	// quoted inside someone's arguments as though the message carried one.
	if fromArgs {
		return rankUserMsg, content
	}
	// One pass over the tag table; first match wins. See titleRules for why its order matters.
	for _, r := range titleRules {
		if !tagPresent(content, r.open, r.match) {
			continue
		}
		// BLANK, NOT EMPTY. The two shapes arrive by different routes: an empty body yields "" from
		// between(), while a whitespace body would otherwise reach rankUserQuery and outrank genuine
		// prose. foldsBlank covers both and is sanitizeTitle's own predicate, so it still ranks any body
		// that would survive the fold.
		t, outcome := r.extract(content)
		if !foldsBlank(t) {
			return r.rank, t
		}
		// Named nothing. What that means is the extraction's call, not the row's. See takeOutcome.
		if outcome == takeDeclined {
			break // keep the message's own words: on to the generic prose arm
		}
		return rankNone, ""
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

// tagPresent reports whether tag is present in content the way m requires.
//
// A matchPrefix row anchors with no leading-whitespace tolerance. The user messages that mention these
// tags mention them as prose — a bug report quoting one — so an unanchored match would discard exactly
// the message a reader wants.
//
// Takes two fields rather than a titleRule receiver: this runs once per row per message of every event,
// and the narrower signature keeps it inlinable.
func tagPresent(content, tag string, m tagMatch) bool {
	if m == matchContains {
		return strings.Contains(content, tag)
	}
	return strings.HasPrefix(content, tag)
}

// extract pulls this rule's title text out of a message its tag already matched, and says what a blank
// result means. See takeOutcome.
func (r titleRule) extract(content string) (string, takeOutcome) {
	switch r.take {
	case takeDiscard:
		return "", takeClaimed
	case takeFirstLine:
		// Anchored on the open, so a match is the machinery. A blank body claims the message.
		return firstLine(between(content, r.open, r.close)), takeClaimed
	case takeArgs:
		// Anchored likewise. A message opening on the /rename envelope is a /rename whether or not it
		// carried arguments.
		return between(content, argsOpen, argsClose), takeClaimed
	case takeQueryBody:
		// A whole-message envelope claims the message: nothing else is in it, so a blank body names
		// nothing rather than letting the markup through.
		//
		// ONE ACCEPTED LIMIT follows from anchoring both ends. A trailing non-blank byte defeats the
		// suffix, so "<user_query></user_query>x" is still titled with its own markup. Widening to "both
		// tags present anywhere" fixes that and regresses the prose case.
		// TestSessionTitle_EnvelopeAnchorLimits pins both halves.
		if t, ok := wholeMessageEnvelope(content); ok {
			return t, takeClaimed
		}
		// Anything else declines. This is the unanchored row, so a body that will not extract is the
		// signal that the tag is loose in prose.
		return between(content, r.open, r.close), takeDeclined
	}
	return "", takeClaimed
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

// firstLine reduces an envelope body to the one line that names it: its first markdown ATX heading
// with the `#` markers stripped, else its first non-blank line.
//
// A body need not be one line. sanitizeTitle folds a multi-line body rather than choosing within it, so
// a markdown document arrives as its subject followed by the next heading down and whatever else fit.
//
// The heading is searched for, not only tested at the top, because a brief may open on a lead
// paragraph. The first heading wins regardless of level: ranking levels would mean reading the whole
// body to find a `#` that may not exist.
//
// Only a bounded prefix of the body is scanned, because this runs under the store's write lock and a
// body is unbounded. A heading deep in a long document is therefore not found.
//
// Returns "" for an entirely blank body. titleFrom's foldsBlank check turns that into rankNone.
func firstLine(body string) string {
	// Hand-scanned rather than strings.Split, which would allocate a slice proportional to a body this
	// deliberately refuses to read all of.
	//
	// The budget is in BYTES CONSUMED, not lines examined. A line budget bounds neither of the two
	// unbounded shapes: one enormous line, and many short ones. It is generous against maxTitleLen so a
	// heading can still be found a few lines down behind a lead paragraph.
	const budget = maxTitleLen * 16
	rest := body
	if len(rest) > budget {
		rest = rest[:budget]
	}
	firstProse := ""
	for rest != "" {
		line := rest
		if i := strings.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i+1:]
		} else {
			rest = ""
		}
		line = strings.Trim(line, " \t\r")
		if line == "" {
			continue
		}
		if t, ok := atxHeading(line); ok {
			if t != "" {
				return t
			}
			// A bare rule of '#' names nothing. Keep scanning; a real heading may follow.
			continue
		}
		if firstProse == "" {
			firstProse = line
			// A first line that already fills the title ends the search. A heading behind it could not be
			// a better name, and looking for one is the unbounded scan the budget exists to prevent.
			// Compared in bytes against a rune cap, so this is conservative.
			if len(firstProse) >= maxTitleLen {
				break
			}
		}
	}
	return firstProse
}

// atxHeading reports whether line is a markdown ATX heading and returns its text.
//
// The whitespace after the markers is the whole test, as in CommonMark. It is what separates
// `# Heading` from `#hashtag` and `#!/bin/sh`, which are ordinary prose lines.
//
// A line of nothing but markers is a heading with empty text: ok is true and text is "". firstLine
// skips those. The trailing markers of a closed ATX heading (`## x ##`) come off with the text's trim.
func atxHeading(line string) (string, bool) {
	h := strings.TrimLeft(line, "#")
	if h == line {
		return "", false // no markers at all
	}
	if h == "" {
		return "", true // nothing but markers
	}
	if h[0] != ' ' && h[0] != '\t' {
		return "", false // #hashtag, #!/bin/sh — markers glued to the text
	}
	return strings.Trim(h, " \t#"), true
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
