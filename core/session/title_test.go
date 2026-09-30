package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rossoctl/cortex/core/pipeline"
)

// userEvent builds an inference event whose user messages carry the given contents.
func userEvent(contents ...string) pipeline.SessionEvent {
	msgs := make([]pipeline.InferenceMessage, 0, len(contents))
	for _, c := range contents {
		msgs = append(msgs, pipeline.InferenceMessage{Role: "user", Content: c})
	}
	return pipeline.SessionEvent{Inference: &pipeline.InferenceExtension{Messages: msgs}}
}

func renameMsg(args string) string {
	return renamePrefix + "<command-args>" + args + "</command-args>"
}

// candidateTitle is what ONE event offers as a title, sanitized — the per-event half of what a
// since-deleted per-session picker used to do inline. Most of this file's cases are about content: which tag
// wins, what a reminder does to it, where the clip falls. Those need one event and nothing else,
// so they assert on this rather than going through a store.
//
// The two-line body is deliberately the same order Append uses (titleCandidate, then sanitizeTitle
// on the winner, then "" for rankNone), so a case that passes here is describing the served
// behaviour and not a test-only variant of it. What it does NOT model is the cross-event tie-break
// — that is the fold's business, and foldTitle in store_test.go is where those cases live.
func candidateTitle(ev pipeline.SessionEvent) string {
	r, t := titleCandidate(&ev)
	if r == rankNone {
		return ""
	}
	return sanitizeTitle(t)
}

// A SESSION WITH NO NAMEABLE EVENT IS UNNAMED — "", absent on the wire.
//
// NO SEPARATE ZERO-EVENTS CASE, which is why this test asserts the has-events one instead. A
// session with no events at all is unreachable through the API: the only entry-creation site
// appends immediately, and planTrim never trims below maxEvents >= 1. So the case a real session
// can actually be in is this one — it exists, it has events, and none of them named it — and a
// distinct sentinel for the empty case would be both untestable here and indistinguishable on the
// wire from a session genuinely so titled. See TestAppend_TitleAbsentWhenNothingNamedIt.
func TestSessionTitle_NothingNamesTheSession(t *testing.T) {
	// An event with no Inference extension at all, then one whose only message is not from the user.
	events := []pipeline.SessionEvent{
		{Host: "api.example.com"},
		{Inference: &pipeline.InferenceExtension{Messages: []pipeline.InferenceMessage{
			{Role: "assistant", Content: "an answer nobody asked for"},
		}}},
	}
	if got := foldTitle(t, events...); got != "" {
		t.Errorf("got %q, want %q — something named a session that nothing in it named", got, "")
	}
}

// An event with no user text names nothing: "", absent on the wire.
func TestSessionTitle_NoMatch(t *testing.T) {
	events := []pipeline.SessionEvent{
		{Inference: &pipeline.InferenceExtension{Messages: []pipeline.InferenceMessage{
			{Role: "assistant", Content: "sure, let me look"},
			{Role: "tool", Content: "exit 0"},
		}}},
		{Host: "api.example.com"}, // Inference == nil
	}
	if got := foldTitle(t, events...); got != "" {
		t.Errorf("no user text = %q, want %q", got, "")
	}
}

// THE CORE PRECEDENCE CASE. A /rename outranks a user message that arrived after it, so the
// reverse walk must not stop at the first candidate it finds.
func TestSessionTitle_RenameWinsOverLaterInference(t *testing.T) {
	events := []pipeline.SessionEvent{
		userEvent(renameMsg("Fix the parser")),
		userEvent("and now do something else entirely"),
	}
	if got := foldTitle(t, events...); got != "Fix the parser" {
		t.Errorf("got %q, want %q — a later user message outranked a /rename", got, "Fix the parser")
	}
}

func TestSessionTitle_RenameLastWins(t *testing.T) {
	events := []pipeline.SessionEvent{
		userEvent(renameMsg("first name")),
		userEvent(renameMsg("second name")),
	}
	if got := foldTitle(t, events...); got != "second name" {
		t.Errorf("got %q, want %q", got, "second name")
	}
	// Two renames inside ONE event: the forward inner loop must also take the later.
	one := []pipeline.SessionEvent{userEvent(renameMsg("early"), renameMsg("late"))}
	if got := foldTitle(t, one...); got != "late" {
		t.Errorf("same-event renames: got %q, want %q", got, "late")
	}
}

func TestSessionTitle_UserQuery(t *testing.T) {
	events := []pipeline.SessionEvent{
		userEvent("preamble <user_query>find the bug</user_query> trailer"),
	}
	if got := foldTitle(t, events...); got != "find the bug" {
		t.Errorf("got %q, want %q", got, "find the bug")
	}
	// Outranks a plain user message that came later...
	events = append(events, userEvent("some follow-up"))
	if got := foldTitle(t, events...); got != "find the bug" {
		t.Errorf("a later plain message outranked <user_query>: got %q", got)
	}
	// ...but loses to a /rename, wherever it sits.
	events = append(events, userEvent(renameMsg("explicit")))
	if got := foldTitle(t, events...); got != "explicit" {
		t.Errorf("<user_query> outranked a /rename: got %q", got)
	}
}

func TestSessionTitle_LastUserMessage(t *testing.T) {
	// Last user message within one event.
	one := []pipeline.SessionEvent{userEvent("first ask", "second ask")}
	if got := foldTitle(t, one...); got != "second ask" {
		t.Errorf("within one event: got %q, want %q", got, "second ask")
	}
	// ACROSS events the EARLIER one holds, which is the opposite of the within-event rule above
	// and is deliberate. Within one event the reverse scan takes the last user message, because the
	// messages of a single inference request are one conversation and the newest is the ask. Across
	// events the fold is first-wins, so a session keeps the name its first prompt gave it instead of
	// re-titling on every turn. TestAppend_TitleFoldTieBreak is where that rule is pinned in full,
	// including the /rename override that is its one exception; this assertion exists here so the
	// two rules are visible side by side, since reading only one of them suggests the other.
	//
	// THIS IS ALSO THE ONE CASE the deleted per-event picker answered differently: of the 15
	// multi-event cases in this file it was the only one where its last-match rule and the fold
	// disagreed, which is why it is called out rather than quietly flipped.
	two := []pipeline.SessionEvent{userEvent("old ask"), userEvent("new ask")}
	if got := foldTitle(t, two...); got != "old ask" {
		t.Errorf("across events: got %q, want %q", got, "old ask")
	}
	// Non-user roles are never candidates, even when they are last.
	mixed := []pipeline.SessionEvent{{Inference: &pipeline.InferenceExtension{
		Messages: []pipeline.InferenceMessage{
			{Role: "user", Content: "the real ask"},
			{Role: "assistant", Content: "my answer"},
		},
	}}}
	if got := foldTitle(t, mixed...); got != "the real ask" {
		t.Errorf("an assistant message was chosen: got %q", got)
	}
}

// THE EAGER LOOP'S ROLE GUARD, which the mixed-role case in TestSessionTitle_LastUserMessage does
// not reach. That one uses plain prose, which is a rankUserMsg guess and so is filtered by the DEFERRED
// arm's guard; the eager loop only ever sees rankRename and rankUserQuery guesses. So the envelope has to be
// on the non-user message for this guard to be the thing rejecting it.
//
// WHY IT MATTERS MORE HERE THAN AT RANK 2: these are the two ranks that beat ordinary prose, and
// rankRename is sticky even against a later /rename's override. A model echoing a /rename envelope or
// quoting a <user_query> back — which is exactly what an assistant summarising a conversation
// does — would otherwise name the session, and at rankRename nothing in the session could displace it.
func TestSessionTitle_NonUserRoleCannotTitleAtAnyRank(t *testing.T) {
	for _, tc := range []struct{ name, payload string }{
		{"rename envelope", renameMsg("a name the model echoed")},
		{"user_query", "<user_query>a question the model quoted</user_query>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The non-user message is LAST and outranks the prose, so only the role check can
			// keep it from winning.
			events := []pipeline.SessionEvent{{Inference: &pipeline.InferenceExtension{
				Messages: []pipeline.InferenceMessage{
					{Role: "user", Content: "the real ask"},
					{Role: "assistant", Content: tc.payload},
				},
			}}}
			if got := foldTitle(t, events...); got != "the real ask" {
				t.Errorf("got %q, want %q — an assistant message titled the session", got, "the real ask")
			}
		})
	}
}

// The trap the literal spec walks into. A user-role message carrying a tool result or an
// image flattens to Content == "" (pipeline.InferenceMessage.ContentBytes documents it,
// TestInferenceParser_AnthropicMessages_RequestContentBytes pins it) — and it is the LAST
// message of every agentic turn after the first tool call. Taking it verbatim would leave
// the majority of real sessions unnamed.
func TestSessionTitle_EmptyUserContentSkipped(t *testing.T) {
	events := []pipeline.SessionEvent{{Inference: &pipeline.InferenceExtension{
		Messages: []pipeline.InferenceMessage{
			{Role: "user", Content: "read /etc/hosts"},
			{Role: "assistant", Content: ""},
			// A tool_result block array: billed for, but no text.
			{Role: "user", Content: "", ContentBytes: 4096},
		},
	}}}
	if got := foldTitle(t, events...); got != "read /etc/hosts" {
		t.Errorf("got %q, want %q — an empty tool-result message was chosen", got, "read /etc/hosts")
	}
}

// The harness attaches a <system-reminder> block to the user turn it belongs to, so a rankUserMsg
// candidate that takes the message verbatim titles the session with the reminder and never
// reaches the prompt. Live payload shape: the reminder leads, the real ask follows.
func TestSessionTitle_StripsReminder(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{
			"leading reminder, prompt after",
			"<system-reminder>Codebase and user instructions are shown below.</system-reminder>test connection",
			"test connection",
		},
		{
			// PROSE ON BOTH SIDES is why the block is spliced out rather than everything before it
			// being discarded. Rare — 10 of the 262 reminder-bearing messages in the 550-transcript
			// corpus — but the splice costs nothing on the other 252, so rarity is not a reason to
			// drop the head. (An earlier version of this comment claimed "67 of 68"; that figure did
			// not reproduce and the PR description withdraws it. See stripReminders for the census.)
			//
			// THIS FIXTURE CANNOT PIN THE SEPARATOR, which is why the next case exists: it has \n on
			// both sides of the block, so sanitizeTitle folds its way to a space whether or not the
			// splice inserts one.
			"prose on both sides",
			"my question\n<system-reminder>noise</system-reminder>\nand the follow-up",
			"my question and the follow-up",
		},
		{
			// NO WHITESPACE ADJACENT TO THE BLOCK, so the splice itself has to separate the words:
			// joining head to tail bare served "my questionand the follow-up". The case above passes
			// under either behaviour, which is how the fusion went unpinned.
			//
			// Not a pathological shape — it is the MAJORITY of the both-sides cases (6 of 10), and
			// every one is someone writing about the tag with it quoted mid-sentence. The harness
			// always emits a newline beside its own blocks, so nothing it injects fuses.
			"prose on both sides with no adjacent whitespace",
			"my question<system-reminder>noise</system-reminder>and the follow-up",
			"my question and the follow-up",
		},
		{
			// PROSE BETWEEN TWO BLOCKS IS LOST, and this pins the loss rather than the splice it
			// replaced ("midtail"). stripReminders keeps what precedes the FIRST open and what
			// follows the LAST close, so `mid` — which is between two blocks — goes with them.
			//
			// An accepted trade, not an oversight: excising each block separately means pairing
			// opens with closes, and doing that by depth is what cost 593ms on a 190KB nested
			// message under the store's read lock. Costs a title on a field documented as a
			// suggestion. See stripReminders.
			"repeated blocks lose the prose between them",
			"<system-reminder>a</system-reminder>mid<system-reminder>b</system-reminder>tail",
			"tail",
		},
		{
			"adjacent blocks",
			"<system-reminder>a</system-reminder><system-reminder>b</system-reminder>the ask",
			"the ask",
		},
		{
			"trailing reminder",
			"the real ask<system-reminder>appended context</system-reminder>",
			"the real ask",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := candidateTitle(userEvent(tc.in))
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// THE SPLICE MUST NOT MANUFACTURE A TAG THE CLIENT DID NOT SEND, and the separator space is the
// only thing enforcing it. stripReminders joins the head before the first block to the tail after
// the last, so a tag halved across that junction would reassemble if the two met bare — letting a
// client synthesize a title out of markup it never actually sent.
//
// THE /rename HALF IS THE ONE THAT MATTERS, because rankRename is STICKY: Append's fold lets a rename
// override an existing title, so a synthesized one holds the session until a later GENUINE rename
// displaces it. The <user_query> half is milder (rankUserQuery beats prose but loses to a rename) and is
// covered here too, since both ride the same junction.
//
// WHY THIS IS NOT REDUNDANT WITH TestSessionTitle_StripsReminder's fusion row: that row asserts a
// title reads correctly, so it fails on a cosmetic regression. This one asserts a RANK, which is
// what quickRank's one-directional bound and titleCandidate's deferral both rest on. Delete the
// separator and the fusion row fails too — but it reports a mangled string, not a client-controlled
// rankRename title, so the actual consequence would be easy to misread as cosmetic.
func TestSessionTitle_SpliceCannotManufactureATag(t *testing.T) {
	block := reminderOpen + "noise" + reminderClose
	// Each input halves a tag across the junction: the head ends mid-tag, the tail resumes it.
	for _, tc := range []struct {
		name, in string
		wantRank int
	}{
		{
			// "<user_qu" + "ery>ask</user_query>" would splice into a valid <user_query>.
			"halved user_query does not reach rankUserQuery",
			"<user_qu" + block + "ery>ask</user_query>",
			rankUserMsg,
		},
		{
			// The /rename envelope halved mid-tag. Spliced bare this titles the session "SPLICED"
			// at rankRename, and only a later genuine rename could ever displace it.
			"halved rename envelope does not reach rankRename",
			renamePrefix[:10] + block + renamePrefix[10:] + "<command-args>SPLICED</command-args>",
			rankUserMsg,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The junction must actually fall inside the tag, or the fixture proves nothing.
			if strings.Contains(stripReminders(tc.in), "<user_query>") ||
				strings.HasPrefix(stripReminders(tc.in), renamePrefix) {
				t.Fatalf("fixture no longer halves a tag: stripped to %q", stripReminders(tc.in))
			}
			if got, _ := titleFrom(tc.in); got != tc.wantRank {
				t.Errorf("titleFrom rank = %d, want %d — the splice manufactured a tag", got, tc.wantRank)
			}
			// And end-to-end: a genuine later rename must still be the thing that titles the
			// session, not the synthesized envelope.
			events := []pipeline.SessionEvent{userEvent(tc.in), userEvent(renameMsg("the real name"))}
			if got := foldTitle(t, events...); got != "the real name" {
				t.Errorf("got %q, want %q — a synthesized envelope claimed the title", got, "the real name")
			}
		})
	}
}

// A message that is nothing but a reminder names nothing. It must fall through to a real
// title behind it rather than answering "" — the same mechanism
// TestSessionTitle_EmptyUserContentSkipped pins for a tool-result message.
func TestSessionTitle_ReminderOnlyFallsThrough(t *testing.T) {
	events := []pipeline.SessionEvent{
		userEvent("the genuine ask"),
		userEvent("<system-reminder>just context, no prompt</system-reminder>"),
	}
	if got := foldTitle(t, events...); got != "the genuine ask" {
		t.Errorf("got %q, want %q — a reminder-only message won", got, "the genuine ask")
	}
}

// THE SAME FALL-THROUGH, WITHIN ONE EVENT. titleCandidate picks one message cheaply and only
// then settles its rank, so a pick it has to reject must not take the event down with it —
// an earlier message in the very same Messages slice can still title it. Every other
// fall-through case in this file spans two events, where the outer walk covers the mistake;
// these do not, and a two-pass titleCandidate answered "" for all three.
func TestSessionTitle_DemotedPickFallsBackWithinEvent(t *testing.T) {
	for _, tc := range []struct {
		name string
		msgs []string
		want string
	}{
		{
			"empty rename args after a real ask",
			[]string{"a genuine ask", renamePrefix + "<command-args></command-args>"},
			"a genuine ask",
		},
		{
			"reminder-only message after a real ask",
			[]string{"another genuine ask", "<system-reminder>ctx</system-reminder>"},
			"another genuine ask",
		},
		{
			// Two demotions deep: both trailing messages name nothing.
			"two rejects in a row",
			[]string{"the real one", "<system-reminder>a</system-reminder>", renamePrefix + "<command-args></command-args>"},
			"the real one",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := candidateTitle(userEvent(tc.msgs...))
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// AN UNTERMINATED <system-reminder> LEAVES THE MESSAGE ALONE. There is no block to excise — just a
// tag name in prose — and prose mentioning the tag is exactly the message a reader wants as a title.
//
// THIS MIRRORS TestSessionTitle_UnclosedTag, deliberately, and for one review round it did not. The
// branch returned the head, so the fixtures below titled as "why is" / "what does", and the comment
// here defended that as the price of not scanning for balance. THE TRADEOFF WAS FALSE: returning the
// message costs the same two index scans — measured on the 190KB opens-only fixture at 202µs against
// 260µs, so the fixed branch is the cheaper one — and the DoS bound is set by those scans, not by
// what they return.
//
// THE TRUNCATION WAS WORSE THAN A BLANK, which is what made it more than cosmetic. A blank result
// falls through to a real title in an earlier message
// (TestSessionTitle_ReminderOnlyFallsThrough), but "why is" is non-blank, so it won rankUserMsg — and
// under Store.Append's first-wins fold a filled rank is never revisited, so it permanently blocked
// the session's real title. TestAppend_UnterminatedReminderDoesNotBlockTheTitle pins that end of it;
// this pins the picker.
func TestSessionTitle_UnterminatedReminderIsLeftAlone(t *testing.T) {
	// The review's probe, verbatim. It used to serve "why is".
	probe := "why is <system-reminder> leaking into my session titles?"
	if got := candidateTitle(userEvent(probe)); got != probe {
		t.Errorf("got %q, want the message unchanged %q", got, probe)
	}
	in := "what does <system-reminder> mean in this code"
	if got := candidateTitle(userEvent(in)); got != in {
		t.Errorf("got %q, want the message unchanged %q", got, in)
	}
	// UNTOUCHED WHEREVER IT SITS, which is the part of this that is the strip's business: the
	// message is returned whole whether it is the only message, the first, or behind a real ask. An
	// earlier draft asserted the cross-event outcome here instead and expected the probe to win —
	// written against the deleted picker's last-match rule, and wrong about the fold, which is
	// first-wins. Which of the two gets served is TestAppend_TitleFoldTieBreak's subject; that the
	// probe survives the strip intact is this one's.
	if got := candidateTitle(userEvent("an earlier genuine ask", in)); got != in {
		t.Errorf("behind a real ask: got %q, want the message unchanged %q", got, in)
	}
	// A CLOSE IN PROSE BEFORE THE FIRST OPEN is not a block end either: splicing on it would run
	// backwards. This is why the branch compares against i rather than testing j < 0.
	back := "discussing </system-reminder> and <system-reminder> dangling"
	if got := candidateTitle(userEvent(back)); got != back {
		t.Errorf("got %q, want the message unchanged %q", got, back)
	}
}

// Why the strip runs before EVERY rank arm and not only the rankUserMsg one. Both nestings are
// wrong when it runs later: a reminder inside the query brackets rides along into the title,
// and a <user_query> inside a reminder — a reminder quoting an earlier turn is enough — gets
// mistaken for the real ask.
func TestSessionTitle_ReminderNestedWithUserQuery(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{
			"reminder outside the query",
			"<user_query>find the bug</user_query>\n<system-reminder>noise</system-reminder>",
			"find the bug",
		},
		{
			"reminder inside the query",
			"<user_query>find <system-reminder>noise</system-reminder>the bug</user_query>",
			"find the bug",
		},
		{
			"query inside the reminder",
			"<system-reminder>ctx <user_query>decoy</user_query></system-reminder>real ask",
			"real ask",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := candidateTitle(userEvent(tc.in)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// A /rename tests a PREFIX, so a reminder in front of one hides it entirely unless the strip
// has already run. The strip's placement at the top of titleFrom is what makes this work.
func TestSessionTitle_ReminderBeforeRename(t *testing.T) {
	in := "<system-reminder>noise</system-reminder>" + renameMsg("Fix the parser")
	events := []pipeline.SessionEvent{
		userEvent(in),
		userEvent("a later plain message that must not outrank it"),
	}
	if got := foldTitle(t, events...); got != "Fix the parser" {
		t.Errorf("got %q, want %q", got, "Fix the parser")
	}
}

func TestSessionTitle_Sanitizes(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"newline and tab fold", "a\nb\tc", "a b c"},
		{"leading whitespace dropped", "  lead", "lead"},
		{"trailing whitespace dropped", "trail  ", "trail"},
		{"bidi mark folds", "a‎b", "a b"},
		{"nbsp folds", "a b", "a b"},
		{"ideographic space folds", "a　b", "a b"},
		{"nul folds", "a\x00b", "a b"},
		{"a run collapses to one space", "a \n\t b", "a b"},
		{"crlf folds to one", "a\r\nb", "a b"},
		{"zero width folds", "a​b", "a b"},
		{"whitespace only", " \n\t ", ""},
		{"combining mark survives", "café", "café"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := candidateTitle(userEvent(tc.in))
			if got != tc.want {
				t.Errorf("sanitize(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.ContainsAny(got, "\n\r\t") {
				t.Errorf("result %q is not single-line", got)
			}
		})
	}
}

// A missing closing tag yields "" from between(), not the rest of the string — so the
// candidate falls through to the next rank rather than swallowing the whole message under a
// tag name it never closed.
func TestSessionTitle_UnclosedTag(t *testing.T) {
	in := "<user_query>no close"
	if got := candidateTitle(userEvent(in)); got != in {
		t.Errorf("got %q, want the raw string %q", got, in)
	}
	// A rename whose args never close yields NOTHING, not the raw envelope. The asymmetry
	// with <user_query> above is deliberate: that tag can appear in ordinary prose, so the
	// whole message is a reasonable title, whereas a /rename envelope is machinery and
	// "<command-name>/rename</command-name><command-args>no close" names nothing. This
	// assertion originally expected the raw envelope, which is what the fall-through in
	// titleFrom produced — the fall-through was the bug.
	open := renamePrefix + "<command-args>no close"
	if got := candidateTitle(userEvent(open)); got != "" {
		t.Errorf("got %q, want %q — a raw /rename envelope leaked as a title", got, "")
	}
	// And a real title behind such an envelope still wins.
	events := []pipeline.SessionEvent{userEvent("the genuine ask"), userEvent(open)}
	if got := foldTitle(t, events...); got != "the genuine ask" {
		t.Errorf("got %q, want %q", got, "the genuine ask")
	}

	// A CLOSED-BUT-EMPTY PAIR INSIDE PROSE IS STILL PROSE, and these two rows exist because the
	// guard that discards a bare <user_query></user_query> was first written as an unanchored
	// Contains — which read any message merely CONTAINING a close as a terminated envelope and
	// discarded it. That is the same overreach this test already guards for the unclosed tag, one
	// shape further along: both messages below are ordinary questions about the markup, and under
	// Append's first-wins fold a discarded one cannot title its session at all.
	//
	// The second row is order-blind rather than unanchored: its close PRECEDES its open, so no
	// envelope is present in any reading. Contains cannot tell, equality can.
	for _, in := range []string{
		"why does <user_query></user_query> render empty in my logs?",
		"closing </user_query> before opening <user_query>",
	} {
		if got := candidateTitle(userEvent(in)); got != in {
			t.Errorf("got %q, want the message unchanged %q", got, in)
		}
	}
	// The contrast that makes the anchor legible: the SAME tag pair as the whole message really is
	// machinery, and still yields nothing. Surrounding whitespace does not make it prose.
	for _, in := range []string{"<user_query></user_query>", "  <user_query></user_query>\n"} {
		if got := candidateTitle(userEvent(in)); got != "" {
			t.Errorf("got %q, want %q — a bare envelope leaked as a title", got, "")
		}
	}
}

// An argument-less /rename names nothing. If it claimed rankRename with "", it would end the
// walk and lose a real title sitting behind it.
func TestSessionTitle_EmptyCommandArgs(t *testing.T) {
	events := []pipeline.SessionEvent{
		userEvent("a genuine earlier ask"),
		userEvent(renamePrefix + "<command-args></command-args>"),
	}
	if got := foldTitle(t, events...); got != "a genuine earlier ask" {
		t.Errorf("got %q, want %q — an empty /rename won", got, "a genuine earlier ask")
	}
}

// The <user_query> envelope guard is ANCHORED AT BOTH ENDS, and this pins what that buys and what
// it costs, because the two are one decision and a future round will be tempted to re-trade it.
//
// It asserts RANKS, not served titles, and that distinction is the whole point of a separate test.
// Every blank-bodied row below is already caught downstream by foldsBlank in titleCandidate, so
// TestSessionTitle_BlankAfterSanitizeFallsThrough passes with or without this guard — what it
// cannot see is a message claiming rankUserQuery on a whitespace body, which outranks genuine prose and is
// a lie even when a net catches it. Rank is also what the deferral in titleCandidate rests on.
//
// THE LAST ROW IS AN ACCEPTED LIMIT, NOT A PASSING CASE. A trailing non-blank byte defeats the
// suffix anchor, so "<user_query></user_query>x" is still titled with its own markup — and under
// Append's first-wins fold that blocks the session's real name until a /rename. The fix asked for in
// review ("blank body whenever both tags are present and correctly ordered") does close it, and it
// discards the prose row above with it: a bug report quoting an empty envelope is a perfectly good
// title. This shape declines that trade. If a future round decides markup-as-title is the worse of
// the two, this row is the one to change, deliberately — the prose row is the cost.
func TestSessionTitle_EnvelopeAnchorLimits(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		wantRank int
	}{
		{"empty envelope", "<user_query></user_query>", rankNone},
		{"blank body", "<user_query>   </user_query>", rankNone},
		{"control body", "<user_query>\x00</user_query>", rankNone},
		{"surrounding whitespace", "  <user_query></user_query>\n", rankNone},

		// A real ask still ranks, and the guard must not swallow it.
		{"real ask", "<user_query>the real ask</user_query>", rankUserQuery},

		// Prose the anchor exists to protect. Both tags are present and correctly ordered, so the
		// remedy prescribed in review would discard exactly this.
		{"envelope quoted in prose",
			"why does <user_query></user_query> render empty in my logs?", rankUserMsg},
		// An unterminated open has no close to anchor against.
		{"unterminated open", "what does <user_query> mean in this code", rankUserMsg},
		// Mis-pairing shapes: a tag inside the anchored span declines the guard.
		{"close before open", "a</user_query>b<user_query>c", rankUserMsg},
		// Two envelopes, the second real — and it does NOT reach rankUserQuery, which is pre-existing and
		// not this guard's doing. between() pairs the FIRST open with the FIRST close, so it reads
		// the empty body, returns "", and the message falls to prose. The guard declines it (a tag
		// sits inside the anchored span), so the behavior is unchanged in both directions; the row
		// is here to say so, since the guard is the obvious suspect when someone notices.
		{"empty envelope then a real one",
			"<user_query></user_query> and <user_query>hi</user_query>", rankUserMsg},

		// THE ACCEPTED LIMIT — see the doc comment. Not an aspiration; today's behavior.
		{"trailing byte still serves markup", "<user_query></user_query>x", rankUserMsg},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := titleFrom(tc.in); got != tc.wantRank {
				t.Errorf("titleFrom(%q) rank = %d, want %d", tc.in, got, tc.wantRank)
			}
		})
	}
}

func TestSessionTitle_ClipsToMaxTitleLen(t *testing.T) {
	// A ~190KB message, the size of the largest measured event.
	t.Run("long ascii", func(t *testing.T) {
		got := candidateTitle(userEvent(strings.Repeat("x", 190_000)))
		if n := utf8.RuneCountInString(got); n != maxTitleLen {
			t.Errorf("clipped to %d runes, want %d", n, maxTitleLen)
		}
	})

	// RUNES, not bytes: 200 CJK runes is 600 bytes, and a byte cut would split one.
	t.Run("multibyte stays valid utf8", func(t *testing.T) {
		got := candidateTitle(userEvent(strings.Repeat("日", 200)))
		if n := utf8.RuneCountInString(got); n != maxTitleLen {
			t.Errorf("clipped to %d runes, want %d", n, maxTitleLen)
		}
		if !utf8.ValidString(got) {
			t.Errorf("clip produced invalid UTF-8: %q", got)
		}
	})

	t.Run("exactly at the cap is unchanged", func(t *testing.T) {
		in := strings.Repeat("y", maxTitleLen)
		if got := candidateTitle(userEvent(in)); got != in {
			t.Errorf("a title exactly at the cap was altered: %d runes", utf8.RuneCountInString(got))
		}
	})

	// ONE RUNE OVER THE CAP, which is the side the other fixtures skip: they jump from exactly
	// maxTitleLen to 190,000 and to 200 CJK runes, so a stop that fired one rune late was pinned only
	// by how many runes came out, never by WHICH. The last input rune is given a distinct identity
	// here so the assertion is that it is absent and the 80 before it survive intact — the same
	// off-by-one discipline as the combining-mark triple below, one rune from the boundary.
	t.Run("one rune over the cap drops exactly that rune", func(t *testing.T) {
		in := strings.Repeat("y", maxTitleLen) + "Z"
		got := candidateTitle(userEvent(in))
		if n := utf8.RuneCountInString(got); n != maxTitleLen {
			t.Errorf("clipped to %d runes, want %d", n, maxTitleLen)
		}
		if want := strings.Repeat("y", maxTitleLen); got != want {
			t.Errorf("got %q, want the first %d runes with the 81st dropped", got, maxTitleLen)
		}
		if strings.Contains(got, "Z") {
			t.Errorf("the 81st rune survived the cap: %q", got)
		}
	})

	// AN ACCEPTED LOSS, pinned so it is a decision rather than a surprise: the cut is a plain rune
	// slice, so it can land BETWEEN a base rune and its combining mark and silently change the
	// character. A decomposed "é" (U+0065 U+0301) whose base sits at rune index 79 keeps the bare
	// "e" and drops the acute — "café" clipped becomes "cafe", not a replacement char and not
	// invalid UTF-8.
	//
	// Not fixed, because fixing it means a grapheme-cluster boundary (golang.org/x/text/unicode/norm
	// or a segmentation table) for a display suggestion that is already truncated, and the failure
	// mode is one accent on the 80th rune of a clipped title. The sanitize table's "combining mark
	// survives" case covers the short-title path, which is the one that matters; this covers the
	// boundary the reviewer found unpinned. THE OFF-BY-ONE IS THE ASSERTION: at base 78 the pair
	// fits intact, at 79 the mark is severed, at 80 both are cut — so a clip that moved by one rune
	// in either direction fails here.
	t.Run("a combining mark at the cut is lost", func(t *testing.T) {
		// The wants are WRITTEN AS ESCAPES, not as the literal "é". A Go source literal "é" is the
		// COMPOSED U+00E9 — one rune — while this fixture builds the DECOMPOSED U+0065 U+0301, and
		// the two are different strings that no comparison here normalizes. Spelling the composed
		// form by accident is how the first draft of this test failed against correct code.
		for _, tc := range []struct {
			base int
			want string // the last runes of the clipped title
		}{
			{78, "é"}, // base at 78, mark at 79 — both inside the cap, pair intact
			{79, "ae"}, // base at 79, mark at 80 — the mark is cut, the base survives BARE
			{80, "aa"}, // base at 80 — both cut, no fragment left behind
		} {
			in := strings.Repeat("a", tc.base) + "é" + "trailing prose"
			got := candidateTitle(userEvent(in))
			if n := utf8.RuneCountInString(got); n != maxTitleLen {
				t.Errorf("base=%d: clipped to %d runes, want %d", tc.base, n, maxTitleLen)
			}
			if !utf8.ValidString(got) {
				t.Errorf("base=%d: clip produced invalid UTF-8: %q", tc.base, got)
			}
			if !strings.HasSuffix(got, tc.want) {
				t.Errorf("base=%d: title ends %q, want suffix %q", tc.base, got, tc.want)
			}
			// The severed case must not leave a DANGLING MARK either — a title opening with a
			// combining mark renders on top of whatever precedes it in a TUI cell.
			if strings.HasPrefix(got, "́") {
				t.Errorf("base=%d: title starts with a bare combining mark: %q", tc.base, got)
			}
		}
	})

	// The cut can land just after a folded space, which is why clipTitle trims again.
	t.Run("no trailing space survives the cut", func(t *testing.T) {
		in := strings.Repeat("ab ", 200) // a space lands at rune index 80
		got := candidateTitle(userEvent(in))
		if strings.HasSuffix(got, " ") {
			t.Errorf("clip left a trailing space: %q", got)
		}
		if n := utf8.RuneCountInString(got); n > maxTitleLen {
			t.Errorf("clipped to %d runes, want <= %d", n, maxTitleLen)
		}
	})
}

// The cap is what keeps an unbounded user message off every /v1/sessions response.
func TestSessionTitle_NeverExceedsCap(t *testing.T) {
	for _, in := range []string{
		strings.Repeat("x", 190_000),
		renameMsg(strings.Repeat("r", 5000)),
		"<user_query>" + strings.Repeat("q", 5000) + "</user_query>",
		strings.Repeat("日本", 5000),
	} {
		if n := utf8.RuneCountInString(candidateTitle(userEvent(in))); n > maxTitleLen {
			t.Errorf("a title reached %d runes, over the %d cap", n, maxTitleLen)
		}
	}
}

func TestListSessions_Title(t *testing.T) {
	s := New(5*time.Minute, 100, 0)
	s.Append("named", userEvent(renameMsg("Ship the title field")))
	s.Append("named", pipeline.SessionEvent{MCP: &pipeline.MCPExtension{Method: "tools/call"}})
	s.Append("unnamed", pipeline.SessionEvent{Inference: &pipeline.InferenceExtension{
		Messages: []pipeline.InferenceMessage{{Role: "assistant", Content: "no user text here"}},
	}})

	byID := map[string]string{}
	for _, sum := range s.ListSessions() {
		byID[sum.ID] = sum.Title
	}
	if byID["named"] != "Ship the title field" {
		t.Errorf("named title = %q, want %q", byID["named"], "Ship the title field")
	}
	if byID["unnamed"] != "" {
		t.Errorf("unnamed title = %q, want empty", byID["unnamed"])
	}
}

func TestSessionSummary_TitleOmittedWhenEmpty(t *testing.T) {
	b, err := json.Marshal(SessionSummary{ID: "s"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Case-insensitive for the reason TestSessionSummary_PromptContextOmittedWhenNil gives:
	// an untagged field marshals under its Go name ("Title"), so a case-sensitive check for
	// "title" would pass whether or not the tag exists and would pin nothing.
	if strings.Contains(strings.ToLower(string(b)), "title") {
		t.Errorf("an empty title serialized as %s, want the field absent", b)
	}

	b, err = json.Marshal(SessionSummary{ID: "s", Title: "a name"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back SessionSummary
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Title != "a name" {
		t.Errorf("round-tripped to %q", back.Title)
	}
}

// A candidate that SANITIZES to empty names nothing. Accepting one locks its rank and discards the
// real title behind it, and the raw-content tests inside titleFrom cannot catch that: whitespace
// and control runes are non-empty until sanitizeTitle folds them away.
//
// All three rank arms, because the two better ones fail worse: a whitespace-only /rename claims
// rankRename, which also BREAKS the walk, so nothing behind it is examined at all.
//
// EACH CASE RUNS TWICE, in two events and in one, and the two are not the same assertion. The
// separate-event form is satisfied by the fold's own blank screen in Append; the same-event form
// needs titleCandidate's, because titleCandidate collapses an event to ONE answer — a blank winner
// there does not fall through to the next message, it discards the whole event, which titles the
// session "" rather than with the real ask sitting beside it.
func TestSessionTitle_BlankAfterSanitizeFallsThrough(t *testing.T) {
	for _, tc := range []struct{ name, blank string }{
		{"spaces", "   "},
		{"tab", "\t"},
		{"newline", "\n"},
		{"nul", "\x00"},
		{"zero width", "​"},
		{"line separator", " "},
		{"rename with blank args", renameMsg("  ")},
		{"user_query with blank body", "<user_query> </user_query>"},
		// ZERO-LENGTH, not whitespace, and the two reach the envelope arm by different routes.
		// between() returns "" for an empty body and for an absent tag alike, so without the
		// anchored guard an empty envelope falls to the generic arm and the session is titled with
		// the literal markup "<user_query></user_query>" — non-blank, so foldsBlank cannot reject
		// it, and under first-wins that blocks the real ask permanently. The whitespace row above
		// is safe via foldsBlank whatever the arm decides, which is what makes it the weaker test.
		{"user_query with empty body", "<user_query></user_query>"},
		// SURROUNDING WHITESPACE, which a whole-string equality does not tolerate: a leading
		// newline is enough to defeat the anchor and serve the markup. These pass through
		// foldsBlank downstream either way, so they assert the served title, not the rank —
		// TestSessionTitle_EnvelopeAnchorLimits covers the rank, which is where the lie lives.
		{"empty envelope in whitespace", "  <user_query></user_query>  "},
		{"empty envelope after newline", "\n<user_query></user_query>"},
		{"user_query with control body", "<user_query>\x00</user_query>"},
		{"rename with control args", renameMsg("\x00")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []pipeline.SessionEvent{userEvent("the real ask"), userEvent(tc.blank)}
			if got := foldTitle(t, events...); got != "the real ask" {
				t.Errorf("got %q, want %q — a blank-after-sanitize candidate won", got, "the real ask")
			}
		})
		t.Run(tc.name+" same event", func(t *testing.T) {
			events := []pipeline.SessionEvent{userEvent("the real ask", tc.blank)}
			if got := foldTitle(t, events...); got != "the real ask" {
				t.Errorf("got %q, want %q — a blank candidate hid a title in its own event", got, "the real ask")
			}
		})
	}
}

// A session whose ONLY candidate sanitizes to blank is unnamed — "" and not the blank string,
// so omitempty drops the field rather than shipping a whitespace title.
func TestSessionTitle_OnlyBlankCandidateIsUnnamed(t *testing.T) {
	for _, in := range []string{"   ", "\t\n", renameMsg(" "), "<user_query>​</user_query>"} {
		if got := candidateTitle(userEvent(in)); got != "" {
			t.Errorf("candidateTitle(%q) = %q, want %q", in, got, "")
		}
	}
}

// quickRank is an upper bound in ONE direction only, and the code used to claim both. An
// unterminated <user_query> guesses rankUserQuery and settles at rankUserMsg, so a rank recorded from the
// guess would outrank a genuine <user_query> elsewhere in the session.
func TestSessionTitle_UnterminatedUserQueryDoesNotOutrank(t *testing.T) {
	// The prose mentioning the tag comes LAST, so an unsettled rankUserQuery guess would win.
	events := []pipeline.SessionEvent{
		userEvent("<user_query>the genuine query</user_query>"),
		userEvent("what does <user_query> mean in this code"),
	}
	if got := foldTitle(t, events...); got != "the genuine query" {
		t.Errorf("got %q, want %q — an unterminated <user_query> was recorded as rankUserQuery",
			got, "the genuine query")
	}
}

// THE SAME THING WITHIN ONE EVENT, which is where the two-pass pick lost it: titleFrom demoted
// the pick to a non-empty rankUserMsg title, and the retry loop only re-picked on EMPTY, so a
// better-ranked message sitting in front of it was discarded.
func TestSessionTitle_DemotedNonEmptyPickKeepsBetterRank(t *testing.T) {
	msgs := []string{"<user_query>the real ask</user_query>", "what does <user_query> mean here"}
	if got := candidateTitle(userEvent(msgs...)); got != "the real ask" {
		t.Errorf("got %q, want %q — a demoted pick discarded a better-ranked message", got, "the real ask")
	}
}

// A DEMOTED GUESS AND A DEFERRED ONE AT THE SAME RANK: the later message wins, per title.go's
// last-match rule. Neither test above reaches this, and the reason is worth stating because it is
// how the bug survived a review: both put a GENUINE <user_query> at index 0, which settles at
// rankUserQuery and outranks everything, so the deferred-candidate loop never runs at all. Plain
// prose at index 0 is what forces the tie — index 0 defers at rankUserMsg, index 1 guesses
// rankUserQuery and titleFrom demotes it to rankUserMsg, and the two are then equal-ranked with the
// deferred one EARLIER.
//
// titleCandidate used to return the deferred candidate unconditionally here, on a stated invariant
// that a deferred index is always the later of the two. It is not: the reverse scan meets the newest
// rankUserMsg GUESS first, but a better guess can demote to rankUserMsg from anywhere, including after it.
func TestSessionTitle_DeferredLosesToLaterDemotedAtEqualRank(t *testing.T) {
	for _, tc := range []struct{ name, early, late string }{
		// An unterminated <user_query> is the shape that reaches this: quickRank sees the opening
		// tag and guesses rankUserQuery, titleFrom finds no closing tag and settles at rankUserMsg.
		{"unterminated user_query", "early plain prose", "what does <user_query> mean here"},
		{"tag named mid-sentence", "plain earlier", "later prose mentioning <user_query> tag"},
		// NOT a case here: a /rename envelope behind prose. quickRank guesses rankUserMsg for it (the
		// test below pins that), so both messages defer and the deferred loop's own newest-first
		// walk picks the later one — it passes with or without the index comparison, which is the
		// kind of case that hid this bug in the first place.
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := candidateTitle(userEvent(tc.early, tc.late))
			if got != tc.late {
				t.Errorf("got %q, want %q — an earlier deferred candidate beat a later demoted one at equal rank", got, tc.late)
			}
		})
	}
}

// bestIdx IS A -1 SENTINEL and it participates in a comparison, so the nothing-settled path needs
// its own pin: when the eager loop accepts nothing, bestRank is rankNone and every real rank must
// still beat it rather than tripping the "equally ranked and earlier" break against index -1.
//
// THE LAST ROW PINS THE GATE'S BOUND, not the sentinel, and it is here because this is the table
// the gate is named for. The other rows all leave bestRank == rankNone, which is the ONE value that
// cannot discriminate the bound: every candidate bound is satisfied by it, so the loop is admitted
// whatever the comparison says. Telling the bounds apart needs a row where the eager loop SETTLES
// something at rankUserMsg and the deferred candidate is nonetheless the right answer — then
// tightening the bound to `>= rankNone` (which rankUserMsg fails) skips a loop that had work to do,
// and the event is titled by the EARLIER message, against last-match-within-event.
//
// Note the bound moves in the counter-intuitive direction: rankNone is the WORST rank and the
// numerically largest, so `>= rankNone` is the STRICTER gate, not the looser one.
func TestSessionTitle_DeferredOnlyWithNothingSettled(t *testing.T) {
	for _, tc := range []struct {
		name string
		msgs []string
		want string
	}{
		{"single prose", []string{"just prose"}, "just prose"},
		{"two prose, later wins", []string{"older prose", "newer prose"}, "newer prose"},
		{"blank then prose", []string{"   ", "real prose"}, "real prose"},
		{"prose then blank", []string{"real prose", "   "}, "real prose"},
		{"all blank names nothing", []string{"   ", "\t"}, ""},
		// Index 0 guesses rankUserQuery on its unterminated <user_query> and is DEMOTED to rankUserMsg by
		// titleFrom (between() finds no close), so the eager loop settles bestRank at rankUserMsg
		// with bestIdx 0. Index 1 is a plain rankUserMsg guess, deferred, and is the LATER message — so
		// the deferred loop must still run and hand it the title. A gate that rankUserMsg fails
		// skips that loop and serves index 0 instead.
		{"settled demotion does not preempt a later deferred pick",
			[]string{"what does <user_query> mean in this code", "plain prose later"},
			"plain prose later"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := candidateTitle(userEvent(tc.msgs...)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// The mirror image: a GENUINELY better-ranked demoted-loop neighbour still wins over a later
// deferred one, so the index comparison above did not turn into "later always wins".
func TestSessionTitle_LaterDeferredLosesToBetterRank(t *testing.T) {
	for _, tc := range []struct{ name, first, second, want string }{
		{"user_query then prose", "<user_query>the query</user_query>", "plain prose after it", "the query"},
		{"rename then prose", renameMsg("named"), "plain prose after it", "named"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := candidateTitle(userEvent(tc.first, tc.second))
			if got != tc.want {
				t.Errorf("got %q, want %q — a later rankUserMsg message beat a better rank", got, tc.want)
			}
		})
	}
}

// quickRank DELIBERATELY does not detect a /rename envelope sitting behind prose — only behind a
// reminder — and that blind spot is only safe while titleFrom tests the prefix too. Pinned so the
// two functions cannot silently desynchronize: if quickRank ever starts calling such a message
// rankRename while titleFrom still refuses it, or vice versa, one of these fails.
func TestSessionTitle_RenameEnvelopeBehindProseIsRank2(t *testing.T) {
	content := "please run " + renameMsg("a name") + " for me"
	if got := quickRank(content); got != rankUserMsg {
		t.Errorf("quickRank = %d, want %d (rankUserMsg)", got, rankUserMsg)
	}
	if got, _ := titleFrom(content); got != rankUserMsg {
		t.Errorf("titleFrom rank = %d, want %d (rankUserMsg) — desynchronized from quickRank", got, rankUserMsg)
	}
	// And it must therefore lose to a real /rename anywhere in the session, not win at rankRename.
	events := []pipeline.SessionEvent{userEvent(renameMsg("the real name")), userEvent(content)}
	if got := foldTitle(t, events...); got != "the real name" {
		t.Errorf("got %q, want %q — a prose-embedded envelope claimed rankRename", got, "the real name")
	}
}

// THE WALK HAS NO CEILING: a /rename is found however far back it sits, including in the very
// oldest event of a long session.
//
// THIS REPLACES A TEST THAT PINNED THE OPPOSITE. An earlier titleScanEvents capped the walk at the
// 64 newest events, because this ran under the store's read lock and an unbounded walk was a
// liability there. The cap bounded the event count without bounding the cost of any one event —
// 38.1s of lock hold at the ceiling, on a nested-reminder message — so the fix moved the title off
// the read path entirely (Store.Append folds it; see entry.Title) and the ceiling went with it.
// Pinned in this direction now so a future reader does not reintroduce a cap here believing it
// still guards a lock.
func TestSessionTitle_NoScanCeiling(t *testing.T) {
	// A /rename is rankRename, the strongest possible candidate, so placing it in the oldest event
	// proves the walk reached the end rather than stopping at some window.
	build := func(titleAt int, total int) []pipeline.SessionEvent {
		evs := make([]pipeline.SessionEvent, 0, total)
		for i := 0; i < total; i++ {
			if i == titleAt {
				evs = append(evs, userEvent(renameMsg("the name")))
				continue
			}
			evs = append(evs, userEvent("filler prose"))
		}
		return evs
	}
	const total = 200 // comfortably past the 64 the old ceiling used

	for _, titleAt := range []int{total - 1, total - 64, total - 65, 0} {
		if got := foldTitle(t, build(titleAt, total)...); got != "the name" {
			t.Errorf("/rename at event %d of %d: got %q, want %q — the walk stopped early",
				titleAt, total, got, "the name")
		}
	}
}

// A <transcript> ENVELOPE NAMES NOTHING. Observed live: a session titled `\", \"` — the fold
// reducing a wall of quoted JSONL to its punctuation. Discarded, so the walk reaches a real title
// behind it.
func TestSessionTitle_TranscriptEnvelopeDiscarded(t *testing.T) {
	// The shape from the live report, and a fuller one.
	for _, tc := range []struct{ name, in string }{
		{"reported shape", `<transcript> \", \" </transcript>`},
		{"jsonl body", `<transcript> {"type":"user","message":{"role":"user","content":"hi"}} </transcript>`},
		{"unterminated", `<transcript> {"type":"user"`},
		{"reminder in front", "<system-reminder>ctx</system-reminder>" + `<transcript> \", \" </transcript>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Alone, it names nothing at all.
			if got := candidateTitle(userEvent(tc.in)); got != "" {
				t.Errorf("alone: got %q, want %q", got, "")
			}
			// Behind a real ask, in the same event and in an earlier one.
			if got := candidateTitle(userEvent("the real ask", tc.in)); got != "the real ask" {
				t.Errorf("same event: got %q, want %q", got, "the real ask")
			}
			two := []pipeline.SessionEvent{userEvent("the real ask"), userEvent(tc.in)}
			if got := foldTitle(t, two...); got != "the real ask" {
				t.Errorf("earlier event: got %q, want %q", got, "the real ask")
			}
		})
	}
}

// THE ANCHOR IS THE RULE. Of the user messages in local transcripts that mention this tag, the only
// one is a bug report QUOTING it — which is a perfectly good title — so an unanchored match would
// discard exactly the message a reader wants. Pinned because "discard anything containing
// <transcript>" is the obvious next simplification and it is wrong.
func TestSessionTitle_ProseMentioningTranscriptSurvives(t *testing.T) {
	for _, in := range []string{
		"what does <transcript> mean here",
		"the title comes from a content that begins with <transcript>",
		"why is <transcript> being discarded",
	} {
		if got := candidateTitle(userEvent(in)); got != in {
			t.Errorf("candidateTitle(%q) = %q, want it kept verbatim", in, got)
		}
	}
}

// LAST MATCH IN EVENT ORDER WINS (title.go's stated rule), even when a reminder hides the later
// match's /rename prefix. quickRank used HasPrefix, so the reminder-prefixed rename guessed rank
// 2 and the earlier bare rename took it.
func TestSessionTitle_LaterReminderPrefixedRenameWins(t *testing.T) {
	msgs := []string{renameMsg("early"), "<system-reminder>n</system-reminder>" + renameMsg("late")}
	if got := candidateTitle(userEvent(msgs...)); got != "late" {
		t.Errorf("got %q, want %q — a later reminder-prefixed /rename lost to an earlier one", got, "late")
	}
	// Across events too, where the outer walk's reverse order should already favour the later.
	two := []pipeline.SessionEvent{
		userEvent(renameMsg("early")),
		userEvent("<system-reminder>n</system-reminder>" + renameMsg("late")),
	}
	if got := foldTitle(t, two...); got != "late" {
		t.Errorf("across events: got %q, want %q", got, "late")
	}
}

// A <user_query> ENVELOPE BEHIND PROSE IS STILL RANK 1, which is why quickRank tests Contains for
// that tag and not HasPrefix. The screen is deliberately unanchored here while the /rename test
// beside it is anchored, and the asymmetry is easy to read as an oversight — so this pins it.
//
// IT IS NOT MERELY AN OPTIMIZATION, which is the part worth having a test for. On a single message
// the anchored screen costs nothing observable: the guess drops to rankUserMsg, the message is deferred,
// and the deferred loop's titleFrom recovers the same title. But rank is also how two candidates in
// one event are ORDERED, so demoting one changes which of them wins — with a genuine rankUserQuery message
// EARLIER in the same event, HasPrefix serves the earlier one and breaks the last-match rule.
// Verified: the second assertion below returns "earlier genuine" under that mutation.
func TestSessionTitle_UserQueryBehindProseKeepsItsRank(t *testing.T) {
	const behind = "before the tag <user_query>the real question</user_query>"

	// Alone, both implementations agree — the deferred loop recovers it. Asserted anyway, because
	// it is the case a reader checks first and its agreement is what makes the next one surprising.
	if got := candidateTitle(userEvent(behind)); got != "the real question" {
		t.Errorf("alone: got %q, want %q", got, "the real question")
	}

	// THE ORDERING CASE. Both messages settle at rankUserQuery, so the LATER one wins; the envelope sitting
	// behind prose must not cost it that. Under an anchored screen this message is deferred to rankUserMsg
	// and the earlier one outranks it.
	const earlier = "<user_query>earlier genuine</user_query>"
	if got := candidateTitle(userEvent(earlier, behind)); got != "the real question" {
		t.Errorf("behind-prose last: got %q, want %q — an envelope behind prose lost its rank", got, "the real question")
	}
	// The mirror image, which holds under either implementation and is here so the assertion above
	// reads as "later wins" rather than "behind-prose wins".
	if got := candidateTitle(userEvent(behind, earlier)); got != "earlier genuine" {
		t.Errorf("behind-prose first: got %q, want %q — the later candidate should win", got, "earlier genuine")
	}
}

// NESTED REMINDER BLOCKS MUST LEAVE NO STRAY TAG IN THE TITLE, which is the property that matters
// and the one that survived the rewrite. It is now got by keeping only what precedes the FIRST open
// and follows the LAST close, rather than by pairing opens with closes by depth — the depth walk
// re-scanned the tail per level and cost 593ms on a 190KB nested message, under the store's read
// lock. Both approaches leave no tag behind; only one of them is linear.
//
// (The earlier non-nesting version paired each open with the NEXT close, which is what leaked
// "</system-reminder>" into a title and dropped the text between two opens.)
func TestSessionTitle_NestedReminders(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{
			"nested block leaves no stray tag",
			"prose <system-reminder>a<system-reminder>b</system-reminder> outer-tail</system-reminder> real ask",
			"prose real ask",
		},
		{
			"nested block spanning to the end",
			"<system-reminder>a<system-reminder>b</system-reminder>c</system-reminder>the ask",
			"the ask",
		},
		{
			// Unbalanced: two opens, one close. NO LONGER KEPT VERBATIM — everything up to the
			// last close goes, leaving "c". The old depth-tracking version returned the whole
			// string here, on the "an unclosed tag must not truncate" policy.
			//
			// THAT POLICY STILL HOLDS, and this subcase is not a counterexample to it: what it
			// protects is a message with NO close anywhere, which is the shape ordinary prose
			// mentioning the tag name has — see
			// TestSessionTitle_UnterminatedReminderIsLeftAlone, which pins that such a message
			// is returned untouched. A close IS present here, so a real block demonstrably
			// ended somewhere and splicing to it is the intended behaviour. The unbalanced form
			// gives up on WHICH open pairs with it, deliberately: pairing by depth is what cost
			// 38.1s on the nested fixture.
			"unbalanced opens do not keep the remainder",
			"<system-reminder>a<system-reminder>b</system-reminder>c",
			"c",
		},
		{
			"three deep",
			"head <system-reminder>1<system-reminder>2<system-reminder>3</system-reminder></system-reminder></system-reminder> tail",
			"head tail",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := candidateTitle(userEvent(tc.in)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	// No stray closing tag may survive in any title this function produces.
	got := candidateTitle(userEvent(
		"prose <system-reminder>a<system-reminder>b</system-reminder> t</system-reminder> ask"))
	if strings.Contains(got, reminderClose) || strings.Contains(got, reminderOpen) {
		t.Errorf("title leaked a reminder tag: %q", got)
	}
}

// A local-command block must not become the title, and must not hide the text behind it.
func TestSessionTitle_StripsLocalCommand(t *testing.T) {
	const caveat = "<local-command-caveat>Caveat: generated while running a local command</local-command-caveat>"
	for _, tc := range []struct{ name, in, want string }{
		{
			"caveat alone leaves the ask behind it",
			caveat + " the real ask",
			"the real ask",
		},
		{
			// All three envelope pairs are consumed — what proves it is that nothing between the caveat
			// and the end leaks into the title. The ARGUMENTS are what titles it, not the text behind
			// them, because they are non-blank; TestSessionTitle_ArgsBeatTheExpansionTheyPrecede is
			// where that preference lives.
			"message, name and args envelopes all go",
			caveat + " <command-message>x</command-message> <command-name>/x</command-name> <command-args>a</command-args> the real ask",
			"a",
		},
		{
			// An args-less command emits no <command-args>, so consuming each envelope to a later
			// sibling's close would run past the text this keeps.
			"command with no args",
			caveat + " <command-message>clear</command-message> <command-name>/clear</command-name> the real ask",
			"the real ask",
		},
		{
			"stdout envelope goes too",
			caveat + " <local-command-stdout>output</local-command-stdout> the real ask",
			"the real ask",
		},
		{
			"a user query behind the block still wins",
			caveat + " <command-name>/x</command-name> <user_query>the real ask</user_query>",
			"the real ask",
		},
		{
			"nothing behind the block names nothing",
			caveat + " <command-name>/clear</command-name> <command-args></command-args>",
			"",
		},
		{
			"unterminated envelope yields no markup",
			caveat + " <command-name>/clear dangling",
			"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := candidateTitle(userEvent(tc.in)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// An unterminated open tag in PROSE is left alone — the anchor decides this one frame before the
// walk, so it says nothing about a truncated block; see the test below for that.
func TestSessionTitle_UnterminatedLocalCommandIsLeftAlone(t *testing.T) {
	in := "what does <local-command-caveat> mean here"
	if got := candidateTitle(userEvent(in)); got != in {
		t.Errorf("got %q, want the message unchanged %q", got, in)
	}
	// The branch under test is only reachable when the tag leads.
	if stripsAsLocalCommand(in) {
		t.Error("prose anchored as machinery; this case no longer guards what it claims to")
	}
}

// A TRUNCATED BLOCK NAMES NOTHING. Returning it whole put the caveat boilerplate — byte-identical
// across sessions, and non-blank, so foldsBlank cannot screen it — into the title permanently.
func TestSessionTitle_TruncatedLocalCommandBlockIsDropped(t *testing.T) {
	const boilerplate = "<local-command-caveat>Caveat: The messages below were generated by the user while " +
		"running local commands. DO NOT respond"
	t.Run("alone it names nothing", func(t *testing.T) {
		if rank, got := titleFrom(boilerplate); got != "" || rank != rankNone {
			t.Errorf("titleFrom = (%d, %q), want (%d, %q)", rank, got, rankNone, "")
		}
	})
	// rankNone is what lets the walk fall through to a real title behind it.
	t.Run("prose behind it still names the session", func(t *testing.T) {
		const want = "fix the flaky login test"
		if got := foldTitle(t, titleEvent(boilerplate), titleEvent(want)); got != want {
			t.Errorf("got %q, want %q — truncated boilerplate blocked the real title", got, want)
		}
	})
}

// A /rename the user typed keeps winning; the block must not shadow the prefix that arm tests.
// A /rename IS THE ONE TITLE THE USER TYPED, so the strip must hand it to the arm that reads it
// rather than consuming it as machinery.
//
// THE CAVEAT CASES ARE THE LOAD-BEARING ONES: a bare rename returns at the anchor check and never
// reaches the walk, so asserting only that shape says nothing about the walk. Behind a caveat —
// which is how a slash command actually arrives — the walk does run, and it used to eat the rename
// envelope and the <command-args> after it, leaving the session unnamed.
func TestSessionTitle_RenameSurvivesLocalCommandStrip(t *testing.T) {
	caveat := localCommandOpen + "the user typed /rename" + localCommandClose
	for _, tc := range []struct{ name, content string }{
		{"bare", renameMsg("chosen")},
		{"behind a caveat", caveat + renameMsg("chosen")},
		{"behind a caveat and whitespace", caveat + "\n" + renameMsg("chosen")},
		{"behind a reminder and a caveat", reminderOpen + "n" + reminderClose + caveat + renameMsg("chosen")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := candidateTitle(userEvent(tc.content)); got != "chosen" {
				t.Errorf("got %q, want %q", got, "chosen")
			}
		})
	}
}

// A CAVEAT-PREFIXED /rename MUST WIN FROM ANY POSITION IN THE EVENT, which a single-message case
// cannot show: titleCandidate defers rankUserMsg guesses to a second loop and skips that loop entirely
// once a better guess has settled, so a rename only survives if quickRank rates it rankRename — the
// screen's one-directional bound. It does not hold by luck. quickRank tests a PREFIX, and a caveat
// block sits in front of the envelope exactly as a reminder can, so the caveat had to join the
// reminder in that test; without it the rename scored 2, lost the deferred ordering to any later
// message, and the session kept the wrong title under first-wins.
func TestSessionTitle_RenameBehindCaveatWinsFromAnyPosition(t *testing.T) {
	caveat := localCommandOpen + "the user typed /rename" + localCommandClose
	for _, tc := range []struct {
		name     string
		contents []string
		want     string
	}{
		// Prose after the rename outranks nothing, but it is the newest rankUserMsg message and the
		// deferred loop returns the newest one that yields a title.
		{"prose after", []string{caveat + renameMsg("chosen"), "later prose"}, "chosen"},
		// A <user_query> settles at rankUserQuery eagerly, which is what skips the deferred loop.
		{"user_query after", []string{caveat + renameMsg("chosen"), "<user_query>q</user_query>"}, "chosen"},
		// WITHIN one event the LAST rename wins, and this is the case quickRank's displacer test
		// exists for: the earlier bare rename scores 0 and the later one must too, or it loses.
		{"later rename wins", []string{renameMsg("a"), caveat + renameMsg("b")}, "b"},
		{"later rename wins, both behind caveats", []string{caveat + renameMsg("a"), caveat + renameMsg("b")}, "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := candidateTitle(userEvent(tc.contents...)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// THE SCREEN MAY NOT UNDER-PROMISE, stated as a property over the shapes this PR taught titleFrom to
// see through. quickRank is allowed to guess BETTER than titleFrom settles (it over-promises in two
// documented ways) but never worse: titleCandidate records a deferred rankUserMsg guess without settling
// it, so a message that settles at 0 or 1 while guessing 2 is silently demoted. See quickRank.
func TestQuickRank_DoesNotUnderPromiseOnLocalCommands(t *testing.T) {
	caveat := localCommandOpen + "Caveat: ..." + localCommandClose
	for _, content := range []string{
		caveat + renameMsg("x"),
		caveat + "\n" + renameMsg("x"),
		" " + caveat + renameMsg("x"),
		reminderOpen + "n" + reminderClose + caveat + renameMsg("x"),
		caveat + "<user_query>q</user_query>",
		caveat + "ordinary prose",
		// Truncated blocks settle at rankNone, the furthest a guess can over-promise against.
		localCommandOpen + "Caveat: truncated",
		localCommandOpen + "Caveat: truncated\nprose behind it",
		"<command-name>/x</command-name><command-args>a</command-args><local-command-stdout>truncated",
	} {
		settled, _ := titleFrom(content)
		if guess := quickRank(content); guess > settled {
			t.Errorf("quickRank=%d under-promises against titleFrom=%d for %q", guess, settled, content)
		}
	}
}

// A REMINDER IN FRONT OF THE BLOCK IS THE LIVE SHAPE, and it is why the anchor tolerates leading
// whitespace: stripReminders splices with a space, so the block arrives one space in. Anchoring
// without the tolerance put the raw markup back in three live sessions' titles.
func TestSessionTitle_LocalCommandBehindReminder(t *testing.T) {
	// The reminder opens the message, as it does live — so the splice leaves only its own space in
	// front of the block. Text BEFORE the reminder would leave that text there instead, and the
	// block would not be stripped; that limit is TestSessionTitle_LocalCommandTagsInProseAreLeftAlone's
	// case seen from the other side, and the anchor is what declines to widen past it.
	content := reminderOpen + "noise" + reminderClose + "\n\n" +
		localCommandOpen + "Caveat: ..." + localCommandClose +
		"<command-name>/clear</command-name><command-message>clear</command-message>" +
		"<command-args></command-args>\n\nthe real ask"
	if got := candidateTitle(userEvent(content)); got != "the real ask" {
		t.Errorf("got %q, want %q", got, "the real ask")
	}
}

// THE WHITESPACE TOLERANCE IS CONFINED TO THE CAVEAT PATH. A message with no caveat block must
// reach the arms below byte-for-byte as it arrived, because their anchors deliberately reject
// leading whitespace — trimming for everyone quietly changed how a space-led <transcript> and a
// space-led /rename rank, neither of which is this PR's business.
func TestSessionTitle_NoCaveatMeansNoTrim(t *testing.T) {
	// ONE CASE, AND IT HAS TO BE THE TRANSCRIPT TAG. The other arms cannot witness this: /rename's
	// envelope opener is itself an anchor now, so the strip fires and the space never reaches it, and
	// the <user_query> arm tolerates surrounding whitespace by construction. The transcript arm is
	// the one whose anchor both refuses whitespace and sits outside the strip's table.
	for _, tc := range []struct{ name, content, want string }{
		{"space then transcript", " " + transcriptOpen + "stuff", " " + transcriptOpen + "stuff"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := candidateTitle(userEvent(tc.content)); got != sanitizeTitle(tc.want) {
				t.Errorf("got %q, want %q", got, sanitizeTitle(tc.want))
			}
		})
	}
}

// A MESSAGE THAT IS NOTHING BUT MACHINERY offers its arguments, or nothing. The shapes below are the
// ones local transcripts actually carry; none has any prose behind the envelopes, so before this the
// whole message was the title — the literal markup, which is non-blank and therefore sticks under
// first-wins for the life of the session.
//
// These cases have nothing to compete with the arguments. Where something does, the arguments still
// win unless they are blank — TestSessionTitle_ArgsBeatTheExpansionTheyPrecede and
// TestSessionTitle_ArgsLoseToRealText are the two sides of that.
func TestSessionTitle_MachineryOffersItsArgs(t *testing.T) {
	const caveat = "<local-command-caveat>Caveat: generated while running a local command</local-command-caveat>"
	for _, tc := range []struct{ name, in, want string }{
		{
			"skill invocation titles as its args",
			"<command-message>restructure-flattened-md</command-message>\n" +
				"<command-name>/restructure-flattened-md</command-name>\n" +
				"<command-args>ONS_084</command-args>",
			"ONS_084",
		},
		{
			// Whichever order the envelopes arrive in.
			"args before the other envelopes",
			"<command-args>jons/ONS_084.md</command-args>\n<command-name>/fix-ocr</command-name>",
			"jons/ONS_084.md",
		},
		{
			// /model and friends. A blank body is not a title, so the event names nothing and the
			// walk is free to find real prose in an earlier message.
			"empty args name nothing",
			"<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args></command-args>",
			"",
		},
		{
			"no args at all names nothing",
			"<command-message>byo</command-message>\n<command-name>/byo</command-name>",
			"",
		},
		{
			// The caveat block by itself — 140 of them locally, every one previously titled with the
			// caveat's own boilerplate, which is identical across every session that has one.
			"a bare caveat names nothing",
			caveat,
			"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := candidateTitle(userEvent(tc.in)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// A BLANK ARGUMENT BODY LEAVES THE PROSE ALONE, so the strip does not shadow the very text it exists
// to uncover. This is the live shape it protects: /clear and friends are what users type ahead of
// their own words, and they carry an empty pair.
//
// The /rename case is the other way arguments lose — by rank rather than by blankness.
func TestSessionTitle_ArgsLoseToRealText(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{
			"blank args leave the prose behind them",
			"<command-name>/clear</command-name><command-args></command-args> the real ask",
			"the real ask",
		},
		{
			"whitespace args count as blank",
			"<command-name>/clear</command-name><command-args>  </command-args> the real ask",
			"the real ask",
		},
		{
			"a rename behind the envelopes still outranks",
			"<command-message>m</command-message>" + renameMsg("chosen"),
			"chosen",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := candidateTitle(userEvent(tc.in)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// ARGUMENTS BEAT THE BOILERPLATE THEY EXPAND TO, which is a within-message preference and not a rank:
// see stripLocalCommands. The expansion is byte-identical across every session invoking that command,
// while the arguments are what tell those invocations apart.
//
// EVERY CASE HERE IS ONE MESSAGE because that is the only shape live traffic has — consecutive user
// messages arrive concatenated (measured: 857 such messages, 0 with the expansion split into the next
// one). A split fixture would pin a shape the proxy never receives, and a rank high enough to satisfy
// one also retitles a session mid-conversation; see TestAppend_TitleFoldKeepsFirstPromptOverArgs.
func TestSessionTitle_ArgsBeatTheExpansionTheyPrecede(t *testing.T) {
	const invoke = "<command-message>restructure-flattened-md</command-message>\n" +
		"<command-name>/restructure-flattened-md</command-name>\n" +
		"<command-args>jons/ONS_084.md</command-args>"
	const expansion = "Base directory for this skill: /Users/x/.claude/skills/restructure-flattened-md"

	for _, tc := range []struct {
		name string
		msgs []string
		want string
	}{
		{"expansion concatenated after the invocation", []string{invoke + "\n\n" + expansion}, "jons/ONS_084.md"},
		{
			// The tag-bearing ranks are unaffected: they still outrank arguments from any position.
			"a user_query still outranks the args",
			[]string{invoke, "<user_query>the actual ask</user_query>"},
			"the actual ask",
		},
		{
			// An invocation with nothing to offer must not shadow the text behind it — /model is the
			// live example, and it carries an empty pair.
			"argument-less invocation yields to the expansion",
			[]string{"<command-name>/model</command-name>\n<command-args></command-args>\n\n" + expansion},
			expansion,
		},
		{
			// VERBATIM FROM ONE LIVE SESSION, whose title was the defect that prompted this: a caveat,
			// then /clear with an empty pair, then the real invocation, then its expansion — all one
			// message. The LAST non-blank arguments win, so the empty /clear pair ahead of them neither
			// claims the title nor blocks the ones that follow.
			"a blank invocation ahead of a real one in the same message",
			[]string{"<local-command-caveat>c</local-command-caveat>\n\n" +
				"<command-name>/clear</command-name>\n<command-args></command-args>\n\n" +
				"<local-command-stdout></local-command-stdout>\n\n" + invoke + "\n\n" + expansion},
			"jons/ONS_084.md",
		},
		{
			// A truncated tail drops, but args already captured from a well-formed envelope stand.
			"args survive an unterminated later envelope",
			[]string{invoke + "<local-command-stdout>truncated"},
			"jons/ONS_084.md",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := candidateTitle(userEvent(tc.msgs...)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// THE WIDENED ANCHOR IS STILL AN ANCHOR, and quickRank still cannot under-promise with it. The
// property is the same one TestQuickRank_DoesNotUnderPromiseOnLocalCommands asserts; these are the
// shapes the envelope-led anchor newly admits, including the one where an envelope displaces a
// /rename from the front.
func TestQuickRank_DoesNotUnderPromiseOnEnvelopeLedMachinery(t *testing.T) {
	for _, c := range []string{
		"<command-message>m</command-message>\n<command-name>/rename</command-name><command-args>x</command-args>",
		" <command-name>/rename</command-name><command-args>x</command-args>",
		reminderOpen + "r" + reminderClose + "<command-message>m</command-message>" + renameMsg("y"),
		"<command-message>restructure</command-message>\n<command-args>ONS_084</command-args>",
		"<command-name>/model</command-name><command-args></command-args>",
		"why does <command-args>x</command-args> appear in my titles?",
	} {
		settled, _ := titleFrom(c)
		if q := quickRank(c); q > settled {
			t.Errorf("quickRank(%q) = %d, settles at %d — the screen may guess better, never worse", c, q, settled)
		}
	}
}

// PROSE MENTIONING THE TAGS KEEPS ITS OWN WORDS — the reason the strip anchors. A terminated pair
// quoted mid-sentence is the case an unanchored match got wrong: it excised the pair and spliced
// the halves, titling the session with a gap where the user's example had been.
func TestSessionTitle_LocalCommandTagsInProseAreLeftAlone(t *testing.T) {
	const content = "why does " + localCommandOpen + "x" + localCommandClose + " appear in my titles?"
	if got := candidateTitle(userEvent(content)); got != content {
		t.Errorf("got %q, want %q", got, content)
	}
}
