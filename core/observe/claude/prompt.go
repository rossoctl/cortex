package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"strings"
)

// titleFromTranscript reads one transcript and returns the best name for it.
//
// The model-generated title when the transcript carries one, otherwise the working
// directory it ran in, otherwise "". Both are LAST-wins: a session can be titled more
// than once and can change directory, and the newest claim is the current one.
//
// TWO LINE KINDS CARRY A TITLE — {"type":"ai-title","aiTitle":…} and
// {"type":"agent-name","agentName":…} — and both feed the same last-wins value. Which one an
// install writes varies: one local config dir had agent-name in 16 transcripts and ai-title in
// only 2, with no overlap, so reading ai-title alone left most of those sessions named by their
// working directory instead of their real title.
//
// Returns "" rather than an error for an unreadable or malformed transcript. A
// missing title costs a label; refusing the whole harvest over one bad file would
// cost every other name. That is the opposite of toolscan's choice on the same files,
// and deliberately so: there, under-reporting makes it propose removing MORE tools,
// so it must fail loudly.
//
// The second return reports a read that ENDED EARLY — a line past the 16MB buffer, or an
// I/O error partway through. The title is still returned, because whatever was found
// before the stop is better than nothing, but the caller must be able to say so: this
// function's fallbacks make a truncated read indistinguishable from a session that simply
// has no title, and the difference matters when the answer is a stale title from early in
// a long session rather than the current one.
func titleFromTranscript(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied transcript dir
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only

	sc := bufio.NewScanner(f)
	// Transcript lines routinely exceed the default 64KB — the largest single line
	// measured locally was 1.36MB — and a Scanner past its limit stops silently, which
	// would drop the title of exactly the longest sessions. Same buffer as
	// toolscan.scanFile.
	sc.Buffer(make([]byte, 0, 256*1024), 16*1024*1024)

	// lastPrompt is Claude Code's own record and outranks every reconstruction below it.
	//
	// THREE GRADES OF RECONSTRUCTED PROMPT, best first. Each is last-wins within its own grade, so a later turn
	// of the same quality replaces an earlier one but never a better one.
	//
	//   human  — origin.kind == "human" AND string content. What the person typed, stated by
	//            Claude Code rather than inferred. 640 such turns across 128 measured transcripts,
	//            every one of them a string.
	//   str    — string content with no origin field, the shape older transcripts use. Still a
	//            prompt in practice, just unattributed.
	//   blocks — text extracted from a content ARRAY. Last resort: these are where the harness
	//            injects, and a "Base directory for this skill: …" title came from one.
	var title, cwd, lastPrompt, human, str, blocks string
	for sc.Scan() {
		line := sc.Bytes()
		// Hot path: most lines are conversation turns carrying neither field. A
		// substring test over the raw bytes is far cheaper than parsing them, and
		// bytes.Contains avoids the copy that strings.Contains(string(line), …) makes.
		//
		// BARE KEYS ONLY. `"role":"user"` is more selective — it matched 22% of lines against 59%
		// for `"role"` — but it embeds a key-value PAIR and so assumes compact JSON: a line written
		// as `"role": "user"` would be skipped before decoding, silently losing the prompt. Every
		// other term here is a bare key for that reason, and the role is re-checked after the
		// decode anyway, so the narrow form bought selectivity at the cost of depending on the
		// writer's spacing. Measured on a real tree it also did not pay: 1.29s against 1.19s for
		// the bare key, because the scan is dominated by a few very large lines rather than by how
		// many lines reach the decoder.
		if !bytes.Contains(line, []byte(`"aiTitle"`)) &&
			!bytes.Contains(line, []byte(`"agentName"`)) &&
			!bytes.Contains(line, []byte(`"cwd"`)) &&
			!bytes.Contains(line, []byte(`"lastPrompt"`)) &&
			!bytes.Contains(line, []byte(`"role"`)) &&
			// A UNICODE-ESCAPED member name would not match any literal above: JSON permits
			// `"last\u0050rompt"`, which decodes to lastPrompt but is skipped by a raw-byte scan.
			// Nothing observed writes keys that way — 0 of 46,124 real lines — so this is latent,
			// and it is taken only because it is nearly free: just 0.2% of lines contain a `\u`
			// escape at all, so admitting them costs a decode on one line in five hundred.
			!bytes.Contains(line, []byte(`\u`)) {
			continue
		}
		var e transcriptMeta
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		// NORMALISED BEFORE THE GUARD, like all five prompt tiers, rather than tested raw.
		//
		// A raw `!= ""` accepted a cwd that normalises away to nothing — whitespace only, an ESC
		// sequence, a bare tag — and last-wins then let it CLOBBER a good value from an earlier line,
		// so the session titled as "" instead of "/w/good". The cwd is the last fallback, so an empty
		// result here is an unnamed session, not a fall-through to something else.
		//
		// normalizeTitle, not clipTitle: a path's distinguishing end is its leaf, so this tier
		// deliberately keeps its full length and the renderer truncates from the left.
		if c := normalizeTitle(e.Cwd); c != "" {
			cwd = c
		}
		// Guarded on Type as well as on the value: "aiTitle" appearing on some other
		// line kind is not a title claim.
		if e.Type == "ai-title" && e.AiTitle != "" {
			title = e.AiTitle
		}
		// The other spelling, treated as equal rather than as a fallback. Both feed the same
		// last-wins `title`, so whichever line appears later in the transcript is the current
		// name — the same rule that already applies between two ai-title lines. Where a
		// transcript carries both kinds they agree anyway (measured: 16 of 16), so ordering
		// them against each other would be inventing a distinction the data does not have.
		if e.Type == "agent-name" && e.AgentName != "" {
			title = e.AgentName
		}
		// LAST-WINS on the user's own prompts, kept as a third tier below both title kinds. Only
		// consulted when neither fired, so a titled session is never renamed by its transcript.
		//
		// Guarded on both the line type and the message role: an assistant turn is not a prompt,
		// and `"role"` appears on every turn either way. Tool output and Claude Code's own
		// bracketed markers are filtered by the two helpers rather than here, so this stays a
		// statement about WHICH turn counts.
		// Claude Code's own record of the last prompt. Preferred over anything reconstructed from
		// user turns below: same information, stated by the agent instead of inferred, and it
		// arrives already free of the tool output and harness envelopes those turns carry. Still
		// put through the same unwrap and synthetic checks, since the value is the prompt text and
		// a slash command is recorded in its envelope form there too.
		if e.Type == "last-prompt" && e.LastPrompt != "" {
			// ONE SHAPE, shared with the human branch below via promptCandidate, so a later fix to
			// the unwrap order cannot land in only one of them. The two used to spell the same
			// intent differently.
			if p, ok := promptCandidate(e.LastPrompt); ok {
				lastPrompt = p
			}
		}
		if e.Type == "user" && e.Message != nil && e.Message.Role == "user" {
			kind := ""
			if e.Origin != nil {
				kind = e.Origin.Kind
			}
			text, wasString := promptFromMessage(e.Message.Content)
			switch {
			case text == "":
				// Nothing typed: a tool_result array, or blocks with no text.
			case kind == "human" && wasString:
				// ATTRIBUTED, so the text is taken as-is. No synthetic-prompt filter here: a
				// slash command the user really typed arrives as
				// "<command-message>review</command-message>…", which the structural filter
				// would reject — and origin already settles authorship, so guessing from the
				// text would only overrule better evidence. The envelopes a typed turn arrives in
				// are unwrapped rather than filtered, for the same reason: the user did type it,
				// so the answer is to render what they typed rather than to drop the turn.
				// ORDER MATTERS. The command envelope is read FIRST: it is a multi-tag structure
				// whose <command-args> sit past the first tag, so stripping a leading wrapper
				// beforehand threw the arguments away and left a bare "/review". Only a turn that
				// is not a command envelope reaches the wrapper strip.
				if p, ok := promptCandidate(text); ok {
					human = p
				}
			case kind != "" && kind != "human":
				// Explicitly NOT human — "task-notification" or "peer". Discarded outright;
				// this is the traffic the text heuristics existed to catch.
			default:
				// EVERY TIER GOES THROUGH promptCandidate, not just the attributed one. These two
				// used to take the raw text, so the same content was titled one way with
				// origin.kind=human and lost entirely without it: an unattributed slash command
				// stayed as its "<command-message>…" envelope, failed the synthetic check, and fell
				// through to the cwd. Unattributed turns are the MAJORITY — 9940 against 640 on the
				// measured tree — so the tier that skipped the unwrapping was the common one.
				//
				// The synthetic check moves INSIDE promptCandidate rather than standing as its own
				// case above, which is what makes this possible: it runs after the envelope and
				// wrapper are removed, so a turn that only looks machine-written because of its
				// wrapper is no longer discarded for it.
				p, ok := promptCandidate(text)
				if !ok {
					break
				}
				if wasString {
					str = p
				} else {
					blocks = p
				}
			}
		}
	}
	// Reported, not discarded. A truncated read still yields whatever was found before the
	// stop — which is why the title is returned alongside the error rather than dropped —
	// but "token too long" means every LATER title claim in the file went unseen, and
	// last-wins is the rule here, so the name returned may be an old one. The buffer above
	// makes this rare; silence made it invisible.
	err = sc.Err()
	// FOUR TIERS, in descending confidence: a title Claude Code generated, then its own record of
	// the last prompt, then the last prompt this package reconstructs from user turns, then the
	// directory the session ran in. The prompt tier is what
	// takes a tree from "mostly paths" to "mostly readable" — measured on one config dir, 110 of
	// 128 sessions had no title line of either kind and fell through to a cwd, and every one of
	// those has a usable prompt.
	//
	// Clipped at the source. A prompt is unbounded and a title is a table cell; see MaxTitleLen.
	// NORMALISED BEFORE the switch, not inside each arm. A whitespace-only candidate passes a
	// bare `!= ""` and clipTitle then empties it, so the tier below was skipped and the cell came
	// out blank — testing the clipped value is what makes each guard mean "this tier has
	// something to show".
	title, lastPrompt = clipTitle(title), clipTitle(lastPrompt)
	human, str = clipTitle(human), clipTitle(str)
	blocks = clipTitle(blocks)
	switch {
	case title != "":
		return title, err
	case lastPrompt != "":
		return lastPrompt, err
	case human != "":
		return human, err
	case str != "":
		return str, err
	case blocks != "":
		return blocks, err
	}
	// THE CWD IS NOT CLIPPED. A path's distinguishing end is its LEAF, and clipping keeps the
	// head: two sibling worktrees under a prefix of 80 runes or more clip to byte-identical
	// titles, so the column stops telling them apart — exactly what it is for. Clipping the cwd
	// was also a regression against the behaviour before this change, which never truncated here.
	//
	// LEFT-TRUNCATION IS A PROPERTY OF THE LEADING SLASH, NOT OF BEING A CWD, and this function
	// cannot convey which is which: a prompt and a cwd come back through the same string, so the
	// renderer decides by looking at the first character. A cwd therefore keeps its tail only
	// because it starts with "/" — and since this change a "/review …" prompt takes that branch
	// too, while a relative cwd would not. An earlier version of this comment claimed the renderer
	// truncates "a path" from the left, which overstated what it can know.
	//
	// So the reason to leave it long is narrower: a REAL path is bounded by the filesystem, where a
	// prompt is unbounded free text, and the cap exists for the latter.
	//
	// "BOUNDED BY THE FILESYSTEM" WAS THE FLAW. e.Cwd is a JSON string field, not a stat() result —
	// nothing validates its length, and a transcript is a file anything can write. So this tier was
	// the one unbounded string in the harvester, and it reached the renderer's per-rune width search,
	// where a 20,000-rune value cost seconds per redraw. Capped at MaxCwdLen now: still far longer
	// than MaxTitleLen so a genuine deep path keeps its whole leaf, which is what this tier is for,
	// but no longer unbounded. The truncation keeps the TAIL, matching how the renderer treats a path.
	//
	// It is still normalised — whitespace collapsed, markup and non-graphic runes dropped — so a
	// cwd cannot carry hidden characters into a cell any more than a prompt can.
	//
	// NORMALISED TWICE, and deliberately so: the accumulator above normalises each candidate to decide
	// whether it is empty, and this call is the guarantee for the RETURN. normalizeTitle is idempotent
	// (asserted, since its fixed-point loop is what makes that true), so the second pass costs one
	// walk and means this line does not depend on every assignment upstream having been normalised.
	return clipCwd(normalizeTitle(cwd)), err
}

// promptFromMessage returns the text a person typed in one user turn, and whether the content was
// a plain STRING rather than an array of blocks.
//
// Two shapes, because Claude Code writes both: a bare string, or an array of blocks of which only
// `text` is human input. A tool_result array yields "" — it is tool output, and titling a session
// with a grep dump was the thing this function exists to prevent.
//
// The shape is returned because it grades the result. Every one of the 640 attributed human turns
// on the measured tree had string content and none had an array, while the arrays are where the
// harness injects — a "Base directory for this skill: …" title came from one. So a string is
// evidence about authorship, not just an encoding detail, and the caller ranks on it.
func promptFromMessage(raw json.RawMessage) (text string, wasString bool) {
	if len(raw) == 0 {
		return "", false
	}
	var plain string
	if err := json.Unmarshal(raw, &plain); err == nil {
		return plain, true
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", false
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type != "text" || blk.Text == "" {
			continue
		}
		// FILTERED PER BLOCK, not after joining. A harness block can arrive ALONGSIDE the user's
		// real text — "my question" and "<system-reminder>…" as two text blocks of one turn — and
		// testing only the joined string let the markup through, because the guard is anchored at
		// the start and the join begins with the prose.
		// A CONTENT-BEARING WRAPPER IS UNWRAPPED FIRST, so the test below judges what the user
		// actually wrote. A "<pasted_content …>" block is synthetic-LOOKING but its body is the
		// user's, and dropping it here lost that body for an array turn while a string turn of the
		// identical content kept it — attribution and encoding deciding the outcome rather than the
		// content. A harness-output wrapper is not unwrapped, so "<system-reminder>…" still goes.
		blockText := stripWrapperTag(blk.Text)
		if isSyntheticPrompt(blockText) {
			continue
		}
		// AND WITHIN the block. isSyntheticPrompt is anchored, so a block holding prose followed by
		// markup — "my question\n<system-reminder>…" as ONE block — passed the check and reached
		// the title intact. Cut here rather than after the join, so a later block cannot be
		// mistaken for the appended markup of an earlier one.
		text := cutTrailingHarness(blockText)
		if text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(text)
	}
	return b.String(), false
}

// unwrapCommandEnvelope renders a slash-command turn as the command the user typed, or returns s
// unchanged when it is not one.
//
// The turn is genuinely the user's — origin.kind is "human" — but the text is an envelope:
// "<command-message>review</command-message><command-name>/review</command-name>
// <command-args>some/path.md</command-args>". Measured, 28 of 128 transcripts ended with one, so
// left alone they are the commonest title shape and every one reads as markup. Worse, 27 of those 28
// share the same command, so they would all carry an identical title.
//
// Yields "/review some/path.md": the line the person would recognise, and one that tells the 28
// apart by their arguments. <command-message> is dropped as a duplicate of the name.
//
// Hand-parsed rather than by regexp, and deliberately: the obvious pattern for a tag pair wants a
// BACKREFERENCE to match the closing tag, which RE2 does not support — `regexp.MustCompile` panics
// at init on `\1`, which compiles clean and dies on first use. Two literal scans need no such
// trick.
func unwrapCommandEnvelope(s string) string {
	if !strings.Contains(s, "<command-name>") {
		return s
	}
	name := between(s, "<command-name>", "</command-name>")
	if name == "" {
		return s
	}
	if args := between(s, "<command-args>", "</command-args>"); args != "" {
		return name + " " + args
	}
	return name
}

// between returns the text bracketed by open and closing, trimmed, or "".
//
// The second parameter is `closing`, not `close`: that would shadow the predeclared builtin, which
// go vet does not report and this module's lint step (go fmt + go vet) would therefore never catch.
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
	return strings.TrimSpace(rest[:j])
}

// contentBearingWrappers are the harness tags whose BODY is the user's own text.
//
// The distinction that matters for stripWrapperTag: a `<pasted_content>` block holds something the
// user pasted, so its body is a prompt and unwrapping it recovers a real title. A `<bash-stdout>`,
// `<system-reminder>` or `<task-notification>` block holds the harness's own output, so its body is
// not a prompt at any depth and the turn must fall through instead.
//
// An allowlist rather than a denylist because the failure directions are not symmetric: omitting a
// content-bearing tag costs one fallback to the previous prompt, while omitting a harness tag puts
// tool output in the title.
var contentBearingWrappers = []string{
	"pasted_content",
}

// isContentBearingWrapper reports whether tag — a full opening tag, "<name …>" — is one whose body
// is the user's own text.
func isContentBearingWrapper(tag string) bool {
	name := strings.TrimPrefix(strings.TrimSpace(tag), "<")
	if j := strings.IndexAny(name, " \t/>"); j >= 0 {
		name = name[:j]
	}
	name = strings.ToLower(name)
	for _, want := range contentBearingWrappers {
		if name == want {
			return true
		}
	}
	return false
}

// stripWrapperTag removes a leading CONTENT-BEARING wrapper from text the user really typed, keeping
// what is inside it.
//
// Content-bearing means the body belongs to the user — see contentBearingWrappers. A wrapper holding
// the harness's own output is left alone, so the caller's synthetic test rejects it and the turn
// falls through rather than titling a session with tool stdout.
//
// Pasted input arrives as `<pasted_content id="2e21">\n…the actual text…`, on a turn whose
// origin.kind is "human" — so it must not be filtered like injected traffic, but the wrapper is
// still markup and left alone it became the title. Unlike a slash command there is nothing to
// reconstruct: the content after the tag IS the prompt.
//
// Only a LEADING tag, and only one. A "<" further in is ordinary prose ("is 3 < 5 in Go?"), and
// stripping repeatedly would start eating text that merely looks like markup.
func stripWrapperTag(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "<") {
		return s
	}
	i := strings.IndexByte(t, '>')
	if i < 0 {
		return s
	}
	// AN ALLOWLIST, not the structural test. Unwrapping anything tag-shaped promoted the body of
	// whatever the harness had wrapped: `<bash-stdout>total 40</bash-stdout>` became the title
	// "total 40", and a `<system-reminder>` body became a title — the grep-dump outcome
	// isSyntheticPrompt's own doc says it exists to prevent, and a contradiction of this PR's claim
	// that markup all the way through falls through.
	//
	// Only wrappers whose CONTENT IS THE USER'S qualify. `<pasted_content>` is the one: the user
	// pasted what is inside it, so the body is a prompt. Everything else the harness emits is its
	// own output — tool stdout, a reminder, a notification — and belongs to the next tier down, not
	// in the title.
	if !isContentBearingWrapper(t[:i+1]) {
		return s
	}
	inner := strings.TrimSpace(t[i+1:])
	if inner == "" {
		return s
	}
	// A closing tag at the end is dropped too, so "<x>body</x>" yields "body".
	//
	// `j >= 0`, not `j > 0`: an EMPTY-bodied wrapper leaves the closing tag at index 0, so the
	// stricter guard skipped the trim and this returned a bare "</pasted_content>".
	//
	// REDUNDANT for the caller as it stands — the call site re-tests the result against
	// isSyntheticPrompt, which rejects a bare closing tag either way, and reverting this to `j > 0`
	// breaks no test. Kept so the helper is right on its own terms rather than only in the company
	// of that check: a second caller would otherwise inherit the bug.
	if j := strings.LastIndex(inner, "</"); j >= 0 && strings.HasSuffix(inner, ">") {
		inner = strings.TrimSpace(inner[:j])
	}
	// RE-TESTED against the same guard, which is what closes the whole family rather than one
	// shape of it. Three ways markup survived a single leading strip:
	//
	//   - an empty body, leaving a bare closing tag;
	//   - a malformed command envelope ("<command-name></command-name> real text"), where the
	//     unwrapper bails on the empty name and control falls through to here;
	//   - a nested wrapper ("<a><b>x</b></a>"), where only the outer tag is removed.
	//
	// Any of them leaves something that still opens with a tag, so asking the guard again is both
	// the narrowest fix and the one that does not need a list of shapes. Returning s unchanged on
	// a still-markup result hands the caller a value its own synthetic filter will reject, so the
	// turn falls through to the next tier instead of titling a session with markup.
	if inner == "" || isSyntheticPrompt(inner) {
		return s
	}
	return inner
}

// promptCandidate turns one raw prompt string into a usable title, reporting whether anything
// survived.
//
// ONE implementation for both callers — the recorded last-prompt line and the attributed human turn
// — because they want exactly the same thing and used to say so in two different shapes. Verified
// behaviourally identical at the time, which is precisely the state in which a later fix lands in
// only one of them.
//
// Order is load-bearing:
//
//  1. the command envelope first, because it is a multi-tag structure whose <command-args> sit past
//     the first tag — stripping a leading wrapper beforehand threw the arguments away;
//  2. otherwise a leading wrapper, keeping what it contains;
//  3. then trailing harness markup, which neither of the above sees since both are anchored;
//  4. then the synthetic test, because every helper returns its input UNCHANGED when it cannot make
//     sense of it, and assigning that verbatim is what put raw markup in titles.
func promptCandidate(text string) (string, bool) {
	candidate := unwrapCommandEnvelope(text)
	if candidate == text {
		candidate = stripWrapperTag(text)
	}
	candidate = cutTrailingHarness(candidate)
	if isSyntheticPrompt(candidate) || isClear(candidate) {
		return "", false
	}
	return candidate, true
}

// isClear reports whether s is the /clear command. /clear resets the session, so it names nothing:
// Claude Code writes it as the first user turn of the NEW transcript, before the user asks anything,
// and a session left idle after it was titled "/clear".
func isClear(s string) bool {
	f := strings.Fields(s)
	return len(f) > 0 && f[0] == "/clear"
}

// isSyntheticPrompt reports whether s READS AS markup Claude Code inserted rather than as something
// the user typed.
//
// A TEST, not a policy. What callers do with a positive answer differs, and the distinction matters
// because an earlier version of this comment claimed harness blocks are "dropped": some are, but a
// wrapper around text the user really pasted has its body kept (stripWrapperTag), and a slash command
// is re-rendered rather than discarded (unwrapCommandEnvelope). This function only answers the
// question; promptCandidate decides.
//
// Both observed families open with a bracket and are machine-written: "[Request interrupted...]"
// (46 occurrences on the measured tree) and "[Image: original 2100x200, displayed at...]" (11).
// Neither describes what a session is about, and the interrupt one is especially misleading as a
// LAST prompt, since it is exactly what a transcript ends with when someone stopped a tool call.
//
// Matched by prefix rather than by an exact set: these strings are Claude Code's, not ours, and a
// reworded variant should keep being skipped. The cost of the loose rule is that a genuine prompt
// beginning "[" is skipped too, which is rare and costs a fallback to the previous prompt.
func isSyntheticPrompt(s string) bool {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "[Request interrupted") || strings.HasPrefix(t, "[Image:") {
		return true
	}
	// A HARNESS BLOCK, which arrives on the user turn because that is the channel the tool
	// harness speaks on, but which nobody typed: <task-notification>, <bash-stdout>,
	// <system-reminder>, <command-name> and friends. Measured, 27 of 128 transcripts ended with
	// one, so without this the commonest title on a real tree is a notification envelope.
	//
	// Detected structurally — opens with an XML-ish tag — rather than by listing the tags, which
	// are the harness's vocabulary and grow. A prompt that genuinely opens with "<" is skipped
	// too; that costs a fallback to the previous prompt, which is the safe direction.
	if strings.HasPrefix(t, "<") {
		// A CLOSING tag counts too. A malformed command envelope —
		// "<command-name></command-name> real text" — bails out of the unwrapper on the empty
		// name, falls through to the wrapper strip, and leaves "</command-name> real text": a
		// dangling close that is still markup and still not a title. Skipping the slash here means
		// one guard recognises both halves of a tag pair.
		if strings.HasPrefix(t, "</") {
			t = "<" + t[2:]
		}
		if i := strings.IndexByte(t, '>'); i > 1 {
			// THE TAG NAME ONLY, cut at the first space or slash. An opening tag may carry
			// attributes — a real transcript produced a title of raw
			// `<pasted_content id="...">` markup — and the space, `=` and `"` all failed the
			// character check below, so every attributed tag went undetected. Self-closing
			// tags are cut the same way.
			tag := t[1:i]
			if j := strings.IndexAny(tag, " \t/"); j >= 0 {
				tag = tag[:j]
			}
			// Lowercased before checking: every tag observed on a real tree is lowercase, so
			// this is latent rather than a live bug, but it costs one call and the alternative
			// is a filter that a capitalised variant walks straight through.
			tag = strings.ToLower(tag)
			if tag != "" && strings.IndexFunc(tag, func(r rune) bool {
				return !(r >= 'a' && r <= 'z') && r != '-' && r != '_'
			}) == -1 {
				return true
			}
		}
	}
	return false
}

// transcriptMeta is the minimum shape needed from a transcript line. Decoding only
// these fields keeps the parse cheap on multi-megabyte lines.
type transcriptMeta struct {
	Type    string `json:"type"`
	AiTitle string `json:"aiTitle"`
	// AgentName is the title Claude Code records as {"type":"agent-name","agentName":…}.
	//
	// A SECOND SPELLING of the same idea, not a different field: some installs write ai-title
	// lines, some write agent-name, and some write both. Measured locally: of 128 transcripts
	// under one config dir, 16 carried agent-name and 2 carried ai-title with no overlap, while
	// another dir had 26 ai-title and 16 agent-name — and in all 16 of those the two values were
	// IDENTICAL. So this is a naming variant to accept, not a competing claim to arbitrate.
	AgentName string `json:"agentName"`
	// LastPrompt is Claude Code's own record of the session's last prompt, written as
	// {"type":"last-prompt","lastPrompt":…}.
	//
	// The most direct answer available: it is the agent stating what the prompt WAS, rather than
	// this package reconstructing it from user turns and then filtering harness traffic back out.
	// Measured, 129 of 130 transcripts carry one, so it is the usual case rather than a bonus.
	LastPrompt string `json:"lastPrompt"`
	Cwd        string `json:"cwd"`
	// Message is the turn body, decoded only far enough to recover a typed prompt.
	//
	// Content is json.RawMessage because Claude Code writes it two ways: a plain string for a
	// simple prompt, and an array of blocks otherwise. Decoding it as either concrete type would
	// silently drop the other — measured on one config dir, 1026 strings against 9718 arrays.
	Message *struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	// Origin says who produced the turn, and it is the only authoritative answer available.
	//
	// Measured across 128 transcripts: kind is "human" on 640 turns, absent on 9940, and
	// "task-notification" (151) or "peer" (33) on the rest. Those last two are precisely the
	// harness-injected traffic that heuristics here used to guess at from the text — so where the
	// field is present it replaces the guessing rather than supplementing it.
	Origin *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
}

// contentBlock is one element of a message's content array.
//
// Only text blocks carry anything a person typed. The overwhelming majority are tool_result —
// 9576 of 9718 arrays on the measured tree — which is tool OUTPUT: grep hits, build logs, a
// CSpell summary. Titling a session with those was the failure mode this shape exists to avoid.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
