package pipeline

import (
	"fmt"
	"testing"
	"time"
)

// THE CASE THIS COLUMN WAS REPORTED FOR. Real interleaving from one live session: a conversation
// at ~1500 messages and 830-851k, with one-shots at 3 messages carrying 282k and 6k landing
// between its turns. Before this rule the gauge followed whichever spoke last, swinging 83% to
// 0.7% between adjacent turns.
func TestSessionContext_IgnoresOneShotsBetweenTurns(t *testing.T) {
	base := time.Now()
	at := func(n int) time.Time { return base.Add(time.Duration(n) * time.Minute) }
	var evs []SessionEvent
	for _, e := range [][]SessionEvent{
		conversation("c1", at(1), 1491, 830_000),
		oneShot("o1", at(2), 282_145),
		oneShot("o2", at(3), 282_493),
		conversation("c2", at(4), 1494, 835_000),
		oneShot("o3", at(5), 6_538),
		conversation("c3", at(6), 1509, 851_000),
		oneShot("o4", at(7), 7_000), // the most recent event of all
	} {
		evs = append(evs, e...)
	}

	if got, want := PromptContextOf(evs), 851_000; got != want {
		t.Errorf("PromptContextOf = %d, want %d — the conversation's latest turn, not the "+
			"one-shot that spoke after it", got, want)
	}
}

// A ONE-SHOT RUN OF ANY LENGTH MUST NOT WIN, which is why there is no window: the conversation
// goes silent while a subagent works, and that silence is structural rather than evidence it has
// gone. Thirty one-shots after the last conversation turn is past any last-N window.
func TestSessionContext_SurvivesALongSilence(t *testing.T) {
	base := time.Now()
	evs := conversation("c1", base, 700, 445_000)
	for i := 0; i < 30; i++ {
		evs = append(evs, oneShot(fmt.Sprintf("o%d", i),
			base.Add(time.Duration(i+1)*time.Minute), 186_870)...)
	}

	if got, want := PromptContextOf(evs), 445_000; got != want {
		t.Errorf("PromptContextOf = %d, want %d — a silent conversation must not age out",
			got, want)
	}
}

// WITH NO ROLE STATED the most messages wins among conversation turns, and equal counts keep the
// most recent. This is the fallback rule, kept for a proxy that publishes no agentRole: the
// message count is then the only thing that separates the main thread from a subagent carrying
// its own tools, and it separates them only while the conversation is the longer of the two.
func TestSessionContext_UnstatedRoleMostMessagesWins(t *testing.T) {
	base := time.Now()
	var evs []SessionEvent
	for _, e := range [][]SessionEvent{
		conversation("main", base, 700, 445_000),
		conversation("sub", base.Add(time.Minute), 12, 40_000),       // a tool-carrying subagent
		conversation("main2", base.Add(2*time.Minute), 700, 448_000), // ties on messages
	} {
		evs = append(evs, e...)
	}

	if got, want := PromptContextOf(evs), 448_000; got != want {
		t.Errorf("PromptContextOf = %d, want %d", got, want)
	}
}

// THE RESPONSE'S OWN MANIFEST IS THE FILTER, and the request's is deliberately not consulted.
//
// An earlier version required the paired request to carry tools too, justified as insurance.
// It was dead code: SnapshotInference is `c := *ext`, a shallow copy, and Tools is only appended
// while parsing the REQUEST — so both snapshots carry the same slice header off the same
// extension and cannot disagree. The pairing needed a map keyed by request id, which is what made
// this function allocate on every call, for a branch that could not be reached.
func TestSessionContext_JudgesTheResponsesOwnManifest(t *testing.T) {
	evs := []SessionEvent{{
		At: time.Now(), Phase: SessionResponse, Direction: Outbound,
		Inference: &InferenceExtension{
			Model: "claude-opus-5", Messages: make([]InferenceMessage, 50),
			Tools: toolsOf(27), InputTokens: 1_000, CacheReadTokens: 99_000,
		},
	}}

	if got, want := PromptContextOf(evs), 100_000; got != want {
		t.Errorf("PromptContextOf = %d, want %d — a response with tools and tokens is a "+
			"candidate on its own account", got, want)
	}
}

// ONLY RESPONSES. A request snapshot carries no token counts, so it would be dropped anyway — but
// by accident of when SnapshotInference copies, not because the loop said so. This pins the
// phase check that makes the rule explicit: a request bearing tokens must still not count.
func TestSessionContext_IgnoresRequestEventsEvenWithCounts(t *testing.T) {
	inf := &InferenceExtension{
		Model: "claude-opus-5", Messages: make([]InferenceMessage, 900),
		Tools: toolsOf(27), InputTokens: 1_000, CacheReadTokens: 499_000,
	}
	evs := []SessionEvent{
		{At: time.Now(), Phase: SessionRequest, Direction: Outbound,
			Inference: inf},
	}

	if got := PromptContextOf(evs); got != 0 {
		t.Errorf("PromptContextOf = %d, want 0 — the prompt side is read off the response", got)
	}
}

// THE COMPACTION TRADEOFF THE FALLBACK STILL PAYS, pinned so it cannot be "fixed" by
// reintroducing the window that was ruled out.
//
// With no role stated, a compaction restarts the conversation at a low message count while the
// pre-compaction turn stays retained with 1500 of them, so the older, longer turn keeps winning
// and the gauge holds the old figure. A stale figure beats one that flips to a subagent's, and
// nothing in an unstated stream can tell the two apart. If this test starts failing because a
// recency rule was added to the FALLBACK, the silence problem is back with it — the main thread
// goes quiet while a subagent runs, and a last-N window fills with its traffic.
//
// TestSessionContext_AfterACompactionFollowsTheMainAgent is the same session with the role
// stated, and does not pay this.
func TestSessionContext_UnstatedRoleHoldsThePreCompactionFigure(t *testing.T) {
	base := time.Now()
	evs := conversation("before", base, 1500, 851_000)
	evs = append(evs, conversation("after", base.Add(time.Hour), 40, 62_000)...)

	if got, want := PromptContextOf(evs), 851_000; got != want {
		t.Errorf("PromptContextOf = %d, want %d — the stale-after-compaction tradeoff changed; "+
			"see the doc comment before accepting a new expectation here", got, want)
	}
}

// WITH NO MESSAGE COUNTS AT ALL, THE UNSTATED ARM IS LATEST-WINS — and `at` has to be the
// comparator that says so, ahead of tokens.
//
// This is the degenerate input the fallback actually meets on the wire, not a constructed one. A
// view=summary timeline that projects without setting MessageCount leaves messageCount() returning
// 0 for EVERY candidate, so all of them tie on msgs and whatever comes second decides the entire
// answer. The two candidates here are a compaction: the pre-compaction turn is large and early, the
// post-compaction turn is later and much smaller.
//
// Order tokens ahead of at and the larger figure wins, which pins 999,623 against a conversation
// that restarted at 400,249 — for the rest of the session, since nothing will ever outgrow it. That
// is the stale-figure failure this column exists to fix, reached through the one input where
// message count cannot rank anything. So tokens is a FINAL tie-break for pairs that agree on both
// msgs and at, never a substitute for at.
//
// Note this does not contradict TestSessionContext_UnstatedRoleHoldsThePreCompactionFigure above:
// there the counts are present and rank the turns, and holding the old figure is the accepted cost
// of having no role to read. Here there is nothing to rank by, and recency is all that is left.
func TestSessionContext_UnstatedWithNoCountsFollowsTheLatestTurn(t *testing.T) {
	base := time.Now()
	// msgs 0 on both: len(Messages) == 0 and MessageCount unset, which is exactly what
	// summarizeEvent produces on a proxy built before the counts landed.
	evs := conversation("pre-compaction", base, 0, 999_623)
	evs = append(evs, conversation("post-compaction", base.Add(time.Hour), 0, 400_249)...)

	if got, want := PromptContextOf(evs), 400_249; got != want {
		t.Errorf("PromptContextOf = %d, want %d — with no counts to rank by the later turn wins; "+
			"a larger-context tie-break ahead of the timestamp pins the pre-compaction figure",
			got, want)
	}
}

// THE CASE THIS RULE WAS CHANGED FOR, with the reported session's own figures.
//
// c39dae31 compacted at 21:32:20 after a turn at 2,468 messages and 999,623 prompt tokens. Ten
// hours and 313 candidate turns later its conversation was at 952 messages and 400,249 tokens,
// and the gauge still drew the pre-compaction figure — 999,623 against a one-million window, a
// bar at 98.6% for a session with 600k of headroom. The message count could not recover for the
// rest of the session: it had 1,516 to climb back.
//
// With the role stated the rule is just "the main agent's latest turn", so the column follows the
// compaction on the very next turn.
func TestSessionContext_AfterACompactionFollowsTheMainAgent(t *testing.T) {
	base := time.Now()
	evs := mainAgent("pre-compaction", base, 2468, 999_623)
	evs = append(evs, mainAgent("post-compaction", base.Add(10*time.Hour), 952, 400_249)...)

	if got, want := PromptContextOf(evs), 400_249; got != want {
		t.Errorf("PromptContextOf = %d, want %d — the gauge is holding a pre-compaction figure "+
			"for a conversation that restarted", got, want)
	}
}

// A SUBAGENT SPEAKING LAST MUST NOT TAKE THE COLUMN, and the message count is not what stops it.
//
// Figures from d6cfc02e, where the two populations overlap: the subagent reached 186 messages and
// 198,899 tokens while the main thread was at 288 and 217,121. Under latest-wins the subagent
// speaks last, so only the declared role keeps the gauge on the conversation. This is the test
// that fails if the role filter is dropped in favour of recency alone.
func TestSessionContext_IgnoresASubagentThatSpokeLast(t *testing.T) {
	base := time.Now()
	evs := mainAgent("main", base, 288, 217_121)
	evs = append(evs, subagent("sub", base.Add(time.Minute), 186, 198_899)...)

	if got, want := PromptContextOf(evs), 217_121; got != want {
		t.Errorf("PromptContextOf = %d, want %d — a subagent took the column", got, want)
	}
}

// BOTH FILTERS ARE NEEDED, AND THE ONE-SHOT IS WHY.
//
// A one-shot is issued by the same CLI as the conversation, so it declares itself MAIN — the role
// does not exclude it, and the manifest has to. And it is not small: the security monitor carries
// a rendered transcript, measured at 421,220 prompt tokens against a true context of 998,334, so
// a size threshold would not separate them either.
func TestSessionContext_AStatedOneShotIsStillAOneShot(t *testing.T) {
	base := time.Now()
	evs := mainAgent("main", base, 952, 400_249)
	evs = append(evs, roled(oneShot("monitor", base.Add(time.Minute), 421_220),
		AgentRoleMain)...)

	if got, want := PromptContextOf(evs), 400_249; got != want {
		t.Errorf("PromptContextOf = %d, want %d — a one-shot that declares itself main took the "+
			"column; the tool manifest is what excludes it", got, want)
	}
}

// A STATED TURN DISPLACES AN UNSTATED FIGURE OUTRIGHT, however much longer the unstated one was.
//
// This is a proxy upgrade mid-session: what came before was chosen by a rule that cannot tell a
// subagent from a conversation, so it is not evidence about either. Taking the first stated turn
// on the spot is what makes the fix arrive on the next event rather than on the next 500.
func TestSessionContext_AStatedTurnDisplacesAnUnstatedFigure(t *testing.T) {
	base := time.Now()
	evs := conversation("unstated", base, 2468, 999_623)
	evs = append(evs, mainAgent("stated", base.Add(time.Hour), 952, 400_249)...)

	if got, want := PromptContextOf(evs), 400_249; got != want {
		t.Errorf("PromptContextOf = %d, want %d — a stated turn must displace a figure chosen by "+
			"the fallback", got, want)
	}
}

// ONCE A SESSION STATES ROLES, AN UNSTATED ROW IS NOT A CANDIDATE — it cannot be checked against
// the filter that matters, and a session whose proxy states roles has no reason to produce one.
// Trusting it would let a single unstated row hand the column to whatever sent it.
func TestSessionContext_AStatedSessionIgnoresUnstatedRows(t *testing.T) {
	base := time.Now()
	evs := mainAgent("stated", base, 952, 400_249)
	evs = append(evs, conversation("unstated", base.Add(time.Hour), 2468, 999_623)...)

	if got, want := PromptContextOf(evs), 400_249; got != want {
		t.Errorf("PromptContextOf = %d, want %d — an unstated row took the column from a stated "+
			"session", got, want)
	}
}

// WITH NO ARRIVAL TIME THE STATED ARM IS LARGEST-WINS, which is the cost of the invariant better()
// states rather than guards: at leads that arm, so unset timestamps leave only its determinism
// filler to decide, and the filler is tokens.
//
// PINNED SO IT IS KNOWN, NOT BECAUSE IT IS WANTED. The figure it produces is the stale
// pre-compaction one this whole column exists to remove, and no producer reaches it — all four
// listeners stamp At at every construction. What the test buys is that a future producer which
// forgets can see the price, and that anyone who decides a guard is worth having has to come here
// and change an expectation rather than discover this by measuring a live gauge.
//
// THE SECOND HALF IS THE CONTROL, and it is what makes this a test of the invariant rather than of
// the degenerate input: the same two turns WITH their timestamps give the answer the rule promises.
func TestSessionContext_WithNoArrivalTimeTheStatedArmIsLargestWins(t *testing.T) {
	base := time.Now()

	timeless := append(atUnset(mainAgent("pre-compaction", base, 1491, 830_000)),
		atUnset(mainAgent("post-compaction", base.Add(time.Hour), 12, 12_000))...)
	if got, want := PromptContextOf(timeless), 830_000; got != want {
		t.Errorf("PromptContextOf = %d, want %d — with At unset the stated arm has only tokens left "+
			"to rank by; if a guard was added, read better()'s invariant paragraph before changing "+
			"this expectation", got, want)
	}

	stamped := append(mainAgent("pre-compaction", base, 1491, 830_000),
		mainAgent("post-compaction", base.Add(time.Hour), 12, 12_000)...)
	if got, want := PromptContextOf(stamped), 12_000; got != want {
		t.Errorf("PromptContextOf = %d, want %d — with At set the rule is the main agent's LATEST "+
			"turn, which is the whole difference the invariant makes", got, want)
	}
}

// EXACT-TIMESTAMP TIES NOW RESOLVE DETERMINISTICALLY, by the larger context.
//
// The sequential form fell through to arrival order here: two unstated turns with equal message
// counts and the same timestamp gave 100k or 200k depending purely on which was folded first. A
// restore that folded a session's events in any other order would have inherited the
// non-determinism.
//
// NOT THE ONLY INPUT THAT CHANGED, which this comment used to claim. The predecessor compared with
// `!Before` in the STATED arm too, so an exact-timestamp tie there went to the later arrival as well.
// The monoid fixture carries that pair: its m2 (400,249) and m3 (300,000) are two mainAgent turns at
// one instant, and folded in that order the predecessor kept 300,000 where the total order keeps
// 400,249 on tokens. Pinned there rather than here; see better()'s own note on the disagreements.
func TestPromptContextFold_ExactTimestampTiesAreDeterministic(t *testing.T) {
	at := time.Now()
	small := conversation("s", at, 600, 100_000)
	large := conversation("l", at, 600, 200_000)

	var a, b PromptContextFold
	a.AddAll(append(append([]SessionEvent{}, small...), large...))
	b.AddAll(append(append([]SessionEvent{}, large...), small...))

	if a.Tokens() != b.Tokens() {
		t.Errorf("fold order changed the answer: %d vs %d", a.Tokens(), b.Tokens())
	}
	if got := a.Tokens(); got != 200_000 {
		t.Errorf("tie resolved to %d, want 200000 — the larger context wins", got)
	}
}

// permsOf enumerates every permutation of [0,n), so a commutativity claim is made over ALL orders
// rather than over the handful an author happened to type out. n is 5 or 6 here.
func permsOf(n int) [][]int {
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	var out [][]int
	var rec func(k int)
	rec = func(k int) {
		if k == n {
			out = append(out, append([]int(nil), idx...))
			return
		}
		for i := k; i < n; i++ {
			idx[k], idx[i] = idx[i], idx[k]
			rec(k + 1)
			idx[k], idx[i] = idx[i], idx[k]
		}
	}
	rec(0)
	return out
}

// THE LAWS THE RESTORE PATH WILL RELY ON. Stated as tests because the spec claims them: if the
// fold is not commutative and associative, replay order changes a persisted figure.
//
// TWO FIXTURES, BECAUSE ONE CANNOT REACH BOTH ARMS. The first version of this test carried a
// single stated candidate, so stated dominance decided every permutation on its own and neither
// arm's internal ordering was ever the comparator that settled the answer — the sequential latch
// this reformulation replaced would have passed it unchanged, which is no test of the
// reformulation at all. So: one fixture whose answer is settled INSIDE the stated arm by
// (at, tokens), and one with no stated candidate anywhere, whose answer is settled inside the
// unstated arm by (msgs, at, tokens).
//
// AND ASSOCIATIVITY, NOT ONLY COMMUTATIVITY. Permuting turns one at a time cannot distinguish the
// two laws: a restore combines whole FOLDS, so the grouped form — ((A,B),C) against (A,(B,C)) —
// is the shape the persisted figure actually depends on.
func TestPromptContextFold_IsACommutativeMonoid(t *testing.T) {
	base := time.Now()

	for _, tc := range []struct {
		name  string
		want  int
		turns [][]SessionEvent
	}{
		{
			// THE STATED ARM DECIDES HERE. Dominance over the unstated turn settles only which
			// CLASS wins; (at, tokens) has to settle the rest — m2 beats m1 on at, and beats m3
			// on tokens at an equal at.
			name: "stated candidates ranked among themselves",
			want: 400_249,
			turns: [][]SessionEvent{
				conversation("c1", base, 1491, 830_000),                // unstated, and much larger
				oneShot("o1", base.Add(time.Minute), 282_000),          // no manifest: makes no claim
				mainAgent("m1", base.Add(2*time.Minute), 108, 217_121), // stated, earlier
				subagent("s1", base.Add(3*time.Minute), 186, 198_899),  // states a role, filtered out
				mainAgent("m2", base.Add(5*time.Minute), 40, 400_249),  // stated and LATEST: wins
				mainAgent("m3", base.Add(5*time.Minute), 30, 300_000),  // ties m2 on at, smaller
			},
		},
		{
			// THE UNSTATED ARM DECIDES HERE, because nothing states a role at all — the case the
			// fixture above cannot reach, since one stated candidate is enough to end every
			// comparison on dominance. All three of its comparators are load-bearing: u3 loses on
			// msgs though it is both later and far larger, u4 loses on at at an equal count, and
			// u2 takes it from u1 on tokens at an equal (msgs, at).
			name: "no stated candidate at all",
			want: 500_000,
			turns: [][]SessionEvent{
				conversation("u1", base.Add(8*time.Minute), 1509, 400_000),
				conversation("u2", base.Add(8*time.Minute), 1509, 500_000),
				conversation("u3", base.Add(20*time.Minute), 1491, 999_000),
				conversation("u4", base.Add(time.Minute), 1509, 900_000),
				oneShot("o1", base.Add(30*time.Minute), 282_000),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turns := tc.turns

			// foldOf folds whole turns, in the order given. n counts arrivals rather than
			// content, so it is reset out of every comparison.
			foldOf := func(order []int) PromptContextFold {
				var f PromptContextFold
				for _, i := range order {
					f.AddAll(turns[i])
				}
				f.ResetFolded()
				return f
			}

			// combine is the fold's binary operation applied to two FOLDS rather than to a fold
			// and an event: the same max over the same production comparator Add uses, with the
			// same zero-fold identity. It is not a second copy of the rule — better() is the
			// rule, and the assertion that a grouped combine equals the event-by-event fold is
			// what pins this helper to Add. A restore-then-continue needs exactly this operation.
			combine := func(a, b PromptContextFold) PromptContextFold {
				switch {
				case a.tokens == 0:
					return b
				case b.tokens == 0:
					return a
				case better(b.current(), a.current()):
					return b
				default:
					return a
				}
			}

			ident := make([]int, len(turns))
			for i := range ident {
				ident[i] = i
			}
			want := foldOf(ident)

			var zero PromptContextFold
			if want == zero {
				t.Fatal("fixture folds to the zero value; this test proves nothing")
			}
			if got := want.Tokens(); got != tc.want {
				t.Fatalf("fixture folds to %d, want %d — the fixture no longer exercises the arm "+
					"this case exists for; read the comment before changing the expectation",
					got, tc.want)
			}

			// Commutativity, over every permutation of the turns.
			for _, order := range permsOf(len(turns)) {
				if got := foldOf(order); got != want {
					t.Fatalf("order %v gave %+v, want %+v — the fold is not commutative",
						order, got, want)
				}
			}

			// Associativity, over every 3-way contiguous grouping of every permutation. The
			// second assertion is the one that keeps combine honest: a grouped combine must
			// agree with folding the same turns event by event.
			//
			// WHAT THIS BLOCK EARNS ITS PLACE FOR IS THE SECOND ASSERTION, not the first, and
			// the limitation is worth stating rather than leaving a reader to assume otherwise.
			// No mutation of better() can fail the first assertion while leaving the
			// commutativity sweep above green: associativity of a max fails only for a
			// NON-TRANSITIVE comparator, and a sweep over all permutations of three or more
			// candidates detects non-transitivity too — so the loop above reaches its Fatalf
			// first. The independent claim here is that combining two FOLDS is a valid
			// implementation of the operation, which is precisely what a restore-then-continue
			// needs and what no amount of event-at-a-time permuting can check.
			for _, order := range permsOf(len(turns)) {
				for i := 1; i < len(order)-1; i++ {
					for j := i + 1; j < len(order); j++ {
						a, b, c := foldOf(order[:i]), foldOf(order[i:j]), foldOf(order[j:])
						left, right := combine(combine(a, b), c), combine(a, combine(b, c))
						if left != right {
							t.Fatalf("order %v grouped at %d,%d: ((A,B),C) = %+v but "+
								"(A,(B,C)) = %+v — the fold is not associative",
								order, i, j, left, right)
						}
						if left != want {
							t.Fatalf("order %v grouped at %d,%d folded to %+v, want %+v — "+
								"combining group folds disagrees with folding the events",
								order, i, j, left, want)
						}
					}
				}
			}

			// Identity, both ways it gets used: folding no events, and combining a zero fold.
			withNothing := foldOf(ident)
			withNothing.AddAll(nil)
			withNothing.ResetFolded()
			if withNothing != want {
				t.Error("folding nothing changed the answer; zero is not the identity")
			}
			if combine(want, zero) != want || combine(zero, want) != want {
				t.Error("combining a zero fold changed the answer; zero is not the identity")
			}
		})
	}
}

// NIL IS "NOTHING KNOWN", and it must survive the round trip as an ABSENT field rather than as a
// zero object. contextGauge renders 0 as an em dash and a real figure as a track; a
// {"tokens":0} on the wire would assert a figure the server does not have.
func TestPromptContextFold_PublishIsNilWhenNothingIsKnown(t *testing.T) {
	var f PromptContextFold
	if got := f.Publish(); got != nil {
		t.Errorf("a zero fold published %+v, want nil", got)
	}
	f.AddAll(oneShot("o1", time.Now(), 282_000)) // no manifest: makes no claim
	if got := f.Publish(); got != nil {
		t.Errorf("a one-shot-only session published %+v, want nil", got)
	}
	f.AddAll(conversation("c1", time.Now(), 600, 500_000))
	got := f.Publish()
	if got == nil || got.Tokens != 500_000 {
		t.Fatalf("published %+v, want tokens=500000", got)
	}
	// Msgs travels too: it is the unstated arm's leading comparator, so a merge on the far end
	// cannot rank this figure without it.
	if got.Msgs != 600 {
		t.Errorf("published msgs=%d, want 600", got.Msgs)
	}
}

// THE HOLE A BARE max LEFT OPEN, and the reason PromptContext carries Stated at all.
//
// abctl attaches to a proxy predating agentRole, folds unstated turns, and lands on the
// documented stale-fallback figure — 700k held from before a compaction. The proxy is then
// upgraded; abctl keeps running, because contextRun outlives everything but a pod switch. The
// new proxy publishes a STATED 200k, the correct latest main-agent turn. max(700k, 200k) pins
// the unsound figure permanently.
func TestMergePromptContext_StatedBeatsUnstatedHoweverLarge(t *testing.T) {
	stated := &PromptContext{Tokens: 200_000, Stated: true, At: time.Now()}
	unstated := &PromptContext{Tokens: 700_000, Stated: false, At: time.Now()}

	for _, tc := range []struct {
		name string
		a, b *PromptContext
	}{
		{"stated first", stated, unstated},
		{"unstated first", unstated, stated},
	} {
		if got := MergePromptContext(tc.a, tc.b); got.Tokens != 200_000 {
			t.Errorf("%s: merged to %d, want 200000 — a figure from a rule that cannot see "+
				"subagents is not evidence about the conversation", tc.name, got.Tokens)
		}
	}
}

// NIL IS THE IDENTITY, which is what lets the client merge without version detection: an old
// proxy sends no field, and that is a valid operand rather than a case to branch on.
func TestMergePromptContext_NilIsTheIdentity(t *testing.T) {
	x := &PromptContext{Tokens: 500_000, Stated: true, At: time.Now()}
	if got := MergePromptContext(nil, x); got != x {
		t.Errorf("MergePromptContext(nil, x) = %+v, want x", got)
	}
	if got := MergePromptContext(x, nil); got != x {
		t.Errorf("MergePromptContext(x, nil) = %+v, want x", got)
	}
	if got := MergePromptContext(nil, nil); got != nil {
		t.Errorf("MergePromptContext(nil, nil) = %+v, want nil", got)
	}
}

// BOTH STATED: (At, Tokens, Msgs), in that order. At leads, because for a session that states its
// roles the rule is just "the main agent's latest turn"; Tokens and then Msgs are determinism filler
// for the pairs At cannot separate, which is the same shape better() has.
func TestMergePromptContext_BothStatedFollowsAtThenTokensThenMsgs(t *testing.T) {
	at := time.Now()

	early := &PromptContext{Tokens: 900_000, Stated: true, At: at}
	late := &PromptContext{Tokens: 200_000, Stated: true, At: at.Add(time.Minute)}
	if got := MergePromptContext(early, late); got.Tokens != 200_000 {
		t.Errorf("merged to %d, want 200000 — latest-wins, not largest", got.Tokens)
	}

	// At tied: the larger figure settles it.
	small := &PromptContext{Tokens: 200_000, Stated: true, At: at}
	big := &PromptContext{Tokens: 500_000, Stated: true, At: at}
	if got := MergePromptContext(small, big); got.Tokens != 500_000 {
		t.Errorf("on an At tie merged to %d, want 500000", got.Tokens)
	}

	// At and Tokens both tied: Msgs is the only thing left, and it must be read — which is only
	// possible because it is now published. This pair was indistinguishable on the wire before.
	short := &PromptContext{Tokens: 500_000, Msgs: 40, Stated: true, At: at}
	long := &PromptContext{Tokens: 500_000, Msgs: 952, Stated: true, At: at}
	for _, tc := range []struct {
		name string
		a, b *PromptContext
	}{
		{"short first", short, long},
		{"long first", long, short},
	} {
		if got := MergePromptContext(tc.a, tc.b); got.Msgs != 952 {
			t.Errorf("%s: merged to msgs=%d, want 952 — Msgs is the stated arm's final "+
				"comparator and is now on the wire", tc.name, got.Msgs)
		}
	}
}

// NEITHER STATED: (Msgs, At, Tokens), and Msgs LEADS. That precedence is the point of publishing
// it, so it is pinned here rather than assumed.
//
// The published order used to be a coarsening — no Msgs on the wire, so one arm served both classes
// and an unstated pair resolved on At. That was defensible only while the field was believed
// transitional, and it is not: inferenceparser.agentRole returns "" for every client that is not
// Claude Code, so the unstated arm is a live path on a CURRENT proxy and the merge has to rank it
// the way the fold does.
func TestMergePromptContext_NeitherStatedRanksMsgsAheadOfAt(t *testing.T) {
	at := time.Now()

	// MSGS AND At DISAGREE, which is the case that separates the full order from the old coarse
	// one: under the coarse rule the later turn won, and it must now lose. Figures from c39dae31's
	// compaction, the session this column was reported for.
	longerButEarlier := &PromptContext{Tokens: 999_623, Msgs: 2468, At: at.Add(-time.Hour)}
	shorterButLater := &PromptContext{Tokens: 400_249, Msgs: 952, At: at}
	for _, tc := range []struct {
		name string
		a, b *PromptContext
	}{
		{"longer first", longerButEarlier, shorterButLater},
		{"later first", shorterButLater, longerButEarlier},
	} {
		if got := MergePromptContext(tc.a, tc.b); got.Msgs != 2468 {
			t.Errorf("%s: merged to msgs=%d tokens=%d, want msgs=2468 — with no role stated the "+
				"most messages wins and At only breaks a msgs tie, which is the fold's own order; "+
				"resolving on At here would be the old coarse rule",
				tc.name, got.Msgs, got.Tokens)
		}
	}

	// MSGS TIED AT ZERO, which is not a contrived input: a view=summary timeline that projects
	// without MessageCount leaves every candidate at 0, so At decides the whole answer there. It
	// must take the LATER turn, not the larger figure — taking the larger would hold a
	// pre-compaction context for the rest of the session.
	later := &PromptContext{Tokens: 100_000, At: at}
	earlierButBigger := &PromptContext{Tokens: 700_000, At: at.Add(-time.Hour)}
	for _, tc := range []struct {
		name string
		a, b *PromptContext
	}{
		{"later first", later, earlierButBigger},
		{"bigger first", earlierButBigger, later},
	} {
		if got := MergePromptContext(tc.a, tc.b); got.Tokens != 100_000 {
			t.Errorf("%s: merged to %d, want 100000 — on a msgs tie the later turn wins; taking "+
				"the larger figure would hold a pre-compaction context forever",
				tc.name, got.Tokens)
		}
	}

	// Msgs AND At both tied: only then does the larger figure settle it.
	tied := &PromptContext{Tokens: 500_000, At: at}
	if got := MergePromptContext(later, tied); got.Tokens != 500_000 {
		t.Errorf("on a msgs and At tie merged to %d, want 500000", got.Tokens)
	}
}

// THE ZERO FOLD LOSES TO EVERY CANDIDATE AND BEATS NONE, which is the property Add's and
// TokensMergedWith's identity disjuncts exist to preserve.
//
// PINNING THE PROPERTY, NOT THE LINE, and the difference is worth stating because a review asked for
// exactly this. Those disjuncts are unreachable — every candidate that gets as far as them already
// beats an empty fold on some comparator — so no test can distinguish them from their own absence,
// and they survive mutation by construction. Their stated purpose is that a future reordering of
// better()'s comparators must not be able to make an empty operand legitimate quietly, and THAT is
// testable: this asserts the ordering property directly, so such a reordering fails here rather than
// merely widening what those lines silently cover.
//
// CANDIDATES BY FIELD, spanning the shapes the arms take: stated and unstated, a count of zero (which
// falls through to at), an unset At (which falls through to tokens), and the smallest figure a
// candidate can carry.
func TestPromptContextFold_TheZeroFoldLosesToEveryCandidate(t *testing.T) {
	at := time.Now().Round(0)
	var zero PromptContextFold
	for _, c := range []candidate{
		{tokens: 500_000, msgs: 600, at: at, stated: true},
		{tokens: 500_000, msgs: 600, at: at},
		{tokens: 500_000, at: at},    // no count: the arm falls through to at
		{tokens: 500_000},            // no count and no time: only tokens separates it
		{tokens: 1, stated: true},    // the smallest stated figure
		{tokens: 1},                  // and the smallest unstated one
		{tokens: 1, at: time.Time{}}, // an explicit zero At, which is what the fold holds
	} {
		if !better(c, zero.current()) {
			t.Errorf("better(%+v, zero fold) = false — an empty fold is not a legitimate operand, "+
				"so every candidate must outrank it", c)
		}
		if better(zero.current(), c) {
			t.Errorf("better(zero fold, %+v) = true — an empty fold outranked a real candidate", c)
		}
	}
}

// A NON-POSITIVE FIGURE IS NOT A CANDIDATE, asserted on candidateOf itself rather than through
// PromptContextOf.
//
// WHY NOT THROUGH THE FOLD: Add refuses a non-positive candidate too, so a whole-slice assertion
// reports 0 whichever of the two guards is doing the work and cannot tell `n <= 0` here from `n == 0`.
// Asking candidateOf directly is what pins the comparison this function actually makes.
//
// THE NEGATIVE IS REACHABLE, unlike this file's identity disjuncts: PromptTokensOf falls back to the
// aggregate FIELD when the split is zero, and that field arrives by JSON decode. Add takes any
// candidate onto an empty fold, so `== 0` would let one such event make a session's PUBLISHED figure
// negative. Mutation-checked: `n <= 0` -> `n == 0` fails the last case here.
func TestCandidateOf_RefusesANonPositiveFigure(t *testing.T) {
	agentic := func(promptTokens int) *SessionEvent {
		return &SessionEvent{
			At: time.Now(), Phase: SessionResponse, Direction: Outbound,
			Inference: &InferenceExtension{
				Model: "claude-opus-5", Messages: make([]InferenceMessage, 40),
				Tools: toolsOf(27), AgentRole: AgentRoleMain,
				// The aggregate field rather than the split, which is the only route to a
				// negative: PromptTokensOf reads the split first.
				PromptTokens: promptTokens,
			},
		}
	}

	// The control: an otherwise identical event with a real figure IS a candidate, or the two
	// assertions below would pass on the manifest or the phase instead.
	if got := candidateOf(agentic(500_000)); got.tokens != 500_000 {
		t.Fatalf("candidateOf on a 500k agentic response = %+v, want tokens=500000 — the fixture is "+
			"not producing a candidate at all", got)
	}
	for _, n := range []int{0, -1} {
		if got := candidateOf(agentic(n)); got != (candidate{}) {
			t.Errorf("candidateOf on a response reporting %d prompt tokens = %+v, want the zero "+
				"candidate — a non-positive figure says nothing", n, got)
		}
	}
}

// EVERY CANDIDATE'S at IS WALL-CLOCK ONLY, which is what makes better()'s equality check transitive
// and therefore the max associative.
//
// WHAT IT GUARDS: time.Time.Equal compares monotonic readings when both operands carry one and wall
// clocks otherwise. Mix the two — a streamed event stamped by time.Now() against a figure decoded
// from JSON or restored from disk, which is the restore-then-continue path the monoid claim is for —
// and "equal" stops being transitive, which is the one property a max needs of its comparator. Both
// constructors of candidate call Round(0) so the mix cannot arise.
//
// PINNING THE NORMALISATION AND NOT THE CYCLE, deliberately: the intransitive triple needs two
// timestamps that agree on the wall clock and disagree on the monotonic reading, and Go offers no way
// to build that pair on purpose — time.Now() is the only monotonic source, Add shifts both readings
// together, and Round(0) removes the reading rather than changing it. Whether two time.Now() calls
// can land wall-equal depends on the platform's clock resolution, so a test that waited for it would
// be a machine-dependent skip. This asserts the invariant instead, which is what the fix installs.
//
// Mutation-checked: dropping Round(0) from either constructor fails this.
func TestPromptContextFold_CandidateTimestampsCarryNoMonotonicReading(t *testing.T) {
	// STRUCT EQUALITY, NOT Equal(), and deliberately: t.Equal(t.Round(0)) is TRUE by definition —
	// Equal falls back to the wall clock as soon as one operand has no monotonic reading — so it
	// answers nothing about whether the reading is there. Comparing the struct is the documented way
	// to see it, and naming the helper keeps a linter's "probably want Equal" suggestion from reading
	// like an unnoticed mistake.
	carriesMonotonic := func(ts time.Time) bool { return ts != ts.Round(0) }

	// time.Now() is the only way to GET a monotonic reading, so the fixture has to start from one or
	// the assertions below hold vacuously. A FIXTURE GUARD rather than a skip, which is this file's
	// idiom: a test that cannot exercise its property should say so loudly rather than report a pass.
	now := time.Now()
	if !carriesMonotonic(now) {
		t.Fatal("time.Now() carries no monotonic reading here, so there is nothing to strip and " +
			"this test proves nothing")
	}

	var f PromptContextFold
	f.AddAll(conversation("c1", now, 600, 500_000))
	if got := f.current().at; carriesMonotonic(got) {
		t.Errorf("a folded candidate's at is %v, which still carries a monotonic reading — "+
			"candidateOf must strip it", got)
	}

	p := &PromptContext{Tokens: 500_000, Msgs: 600, Stated: true, At: now}
	if got := p.candidate().at; carriesMonotonic(got) {
		t.Errorf("a published figure's candidate at is %v, which still carries a monotonic reading — "+
			"PromptContext.candidate must strip it", got)
	}
}

// THE ORDER ITSELF, WRITTEN DOWN — which is the one thing the cross-product test below cannot state,
// because there the expectation is a call to better() and so it agrees with any better() at all.
//
// A SINGLE DESCENDING SEQUENCE, then every pair of it. better() is claimed to be a TOTAL order, so
// the whole rule can be written out as one ranking and asserted pairwise: for i before j, better must
// be true one way and false the other. That form is what makes a literal expectation possible — there
// is nothing here computed from the function under test — and it is stronger than sorting the list,
// which a comparator returning false for everything would pass by leaving an already-ordered fixture
// alone.
//
// THE NEIGHBOURS ARE THE ASSERTION. Each entry names why it sits below the one before it, and no two
// differ in more than the comparator being pinned, so a comparator that is inverted, promoted or
// demoted changes which pair fails and says which one it was.
//
// WALL-CLOCK-ONLY TIMESTAMPS, as candidateOf and PromptContext.candidate both produce: Round(0)
// strips the monotonic reading, so these literals compare the way a candidate off the wire does.
//
// MUTATION-CHECKED against the three the second review named — `return a.stated` -> `!a.stated`, the
// stated arm reordered to (tokens, at, msgs), and `a.msgs > b.msgs` -> `<` in the unstated arm. Each
// one fails here.
func TestBetter_RanksTheDocumentedOrderOnLiterals(t *testing.T) {
	at := time.Now().Round(0)
	// Descending rank: the most preferred candidate first. Hand-written, and deliberately NOT
	// derived from better() in any way.
	ranked := []struct {
		name string
		c    candidate
	}{
		// STATED FIRST, ALL OF THEM, however small — dominance, and the unstated entries below
		// include one that is later and 5x larger than anything here.
		//
		// Within the stated arm: (at, tokens, msgs).
		{"stated, latest", candidate{stated: true, at: at, tokens: 200_000}},
		{"stated, a minute earlier though 4.5x larger: at leads",
			candidate{stated: true, at: at.Add(-time.Minute), tokens: 900_000}},
		{"stated, earlier still",
			candidate{stated: true, at: at.Add(-2 * time.Minute), tokens: 500_000, msgs: 952}},
		{"stated, tied on at and tokens, fewer messages: msgs is the last word",
			candidate{stated: true, at: at.Add(-2 * time.Minute), tokens: 500_000, msgs: 40}},
		{"stated, tied on at, smaller: tokens ranks ahead of msgs, which is 9,999 here",
			candidate{stated: true, at: at.Add(-2 * time.Minute), tokens: 300_000, msgs: 9_999}},
		// UNSTATED, and every one of them below every stated one above.
		//
		// Within the unstated arm: (msgs, at, tokens) — a different order, which is the whole
		// reason Msgs is on the wire.
		{"unstated, the most messages", candidate{msgs: 2468, at: at.Add(-time.Hour), tokens: 100_000}},
		{"unstated, fewer messages though latest AND largest: msgs leads",
			candidate{msgs: 952, at: at, tokens: 999_999}},
		{"unstated, tied on msgs, an hour earlier: at is second",
			candidate{msgs: 952, at: at.Add(-time.Hour), tokens: 700_000}},
		{"unstated, tied on msgs and at, smaller: tokens is the last word",
			candidate{msgs: 952, at: at.Add(-time.Hour), tokens: 1}},
	}

	for i := range ranked {
		for j := i + 1; j < len(ranked); j++ {
			hi, lo := ranked[i], ranked[j]
			if !better(hi.c, lo.c) {
				t.Errorf("better(%s, %s) = false, want true — %+v must outrank %+v",
					hi.name, lo.name, hi.c, lo.c)
			}
			// Antisymmetry, which is not a separate law here but the same claim read backwards:
			// two candidates that compare equal both ways are interchangeable, and no pair in
			// this ranking is.
			if better(lo.c, hi.c) {
				t.Errorf("better(%s, %s) = true, want false — the ranking is not antisymmetric "+
					"for %+v against %+v", lo.name, hi.name, lo.c, hi.c)
			}
		}
	}
}

// THE PROJECTION IS ORDER-EQUIVALENT TO THE FOLD, which is the invariant publishing Msgs bought and
// the one a future hand-written second copy of the order would break.
//
// Two claims, both over an exhaustive cross-product: Publish loses nothing better() reads, and
// MergePromptContext picks whatever better() DOES pick — the delegation, not the order.
//
// WHAT THIS CANNOT DETECT, stated plainly because an earlier failure message here claimed otherwise
// ("the wire order has drifted from the fold's"): the expectation below IS MergePromptContext's body,
// so the cross-product is tautological with respect to the ORDERING. It passes unchanged under
// flipping stated dominance, under swapping the stated arm to tokens-before-At, and under inverting
// the unstated msgs comparator — every mutation whose drift that message named. The order itself is
// pinned on LITERALS by TestBetter_RanksTheDocumentedOrderOnLiterals, and by the Merge tests above;
// what is pinned HERE is that this function keeps deferring to better() rather than growing a second
// copy of it, which is a claim about the code's shape and is worth having for exactly that.
//
// AND THIS IS WHERE THE FULLY-TIED RULE IS PINNED. The last two members are distinct pointers equal
// on every compared field, which makes the cross-product assert that such a pair keeps a — better()
// is false both ways, so the incumbent stands, exactly as the fold keeps its own. That pair belongs
// HERE and not in the monoid fixture: the commutativity check there compares pointers, so a tied
// pair would fail it for a reason that has nothing to do with the law.
func TestMergePromptContext_IsTheSameOrderAsTheFold(t *testing.T) {
	// Round(0) MODELS A REAL FOLD, and the losslessness loop below needs it: a fold's at comes from
	// candidateOf, which strips the monotonic reading, so a production fold never carries one. These
	// folds are hand-built from the wire type, and a fixture that kept the reading would compare a
	// stripped p.candidate() against an unstripped f.current() and fail on the provenance of the
	// timestamp rather than on a dropped field.
	at := time.Now().Round(0)
	vals := []*PromptContext{
		{Tokens: 100_000, Msgs: 952, At: at},
		{Tokens: 700_000, Msgs: 2468, At: at.Add(-time.Hour)},
		{Tokens: 400_000, Msgs: 2468, At: at},
		{Tokens: 400_000, Msgs: 0, At: at},
		{Tokens: 200_000, Stated: true, At: at},
		{Tokens: 900_000, Stated: true, At: at.Add(-time.Minute)},
		{Tokens: 200_000, Msgs: 40, Stated: true, At: at},
		{Tokens: 500_000, Msgs: 600, Stated: true, At: at}, // fully tied with the next, and distinct
		{Tokens: 500_000, Msgs: 600, Stated: true, At: at},
	}

	// State the tie rule outright as well, so it survives a reshuffle of vals.
	tiedA, tiedB := vals[len(vals)-2], vals[len(vals)-1]
	if *tiedA != *tiedB || tiedA == tiedB {
		t.Fatalf("the last two members must be equal in value and distinct in identity: %p %p",
			tiedA, tiedB)
	}
	if got := MergePromptContext(tiedA, tiedB); got != tiedA {
		t.Errorf("a fully tied pair merged to %p, want the left operand %p — ties keep the "+
			"incumbent, as the fold does", got, tiedA)
	}
	if got := MergePromptContext(tiedB, tiedA); got != tiedB {
		t.Errorf("reversed, a fully tied pair merged to %p, want the left operand %p", got, tiedB)
	}

	// Publish is lossless: a fold round-trips through the wire type without dropping a comparator.
	for _, v := range vals {
		f := PromptContextFold{tokens: v.Tokens, msgs: v.Msgs, at: v.At, stated: v.Stated}
		p := f.Publish()
		if p == nil {
			t.Fatalf("%+v published nil", v)
		}
		if got := p.candidate(); got != f.current() {
			t.Errorf("round trip lost a field: published %+v, folded %+v", got, f.current())
		}
	}

	for _, a := range vals {
		for _, b := range vals {
			want := a
			if better(b.candidate(), a.candidate()) {
				want = b
			}
			if got := MergePromptContext(a, b); got != want {
				t.Errorf("MergePromptContext(%+v, %+v) = %+v, but better() ranks %+v first — this "+
					"function has stopped DEFERRING to better(); the ordering itself is pinned on "+
					"literals by TestBetter_RanksTheDocumentedOrderOnLiterals, not here", a, b, got, want)
			}
		}
	}
}

// THE ALLOCATION-FREE PATH MUST GIVE THE SAME ANSWER, which is the only thing standing between a
// second entry point and the drift this file rejects everywhere else.
//
// TokensMergedWith exists because publishing the local fold to merge it allocated per row per
// rebuild in abctl's sessions loop (see its doc for the numbers). It defers to better() exactly as
// MergePromptContext does, so it cannot disagree about the ORDER — but it restates the identity
// handling, and that is the part a reader has to take on trust. This takes it on evidence instead:
// for every fold and every published figure, including the absent one, the two must agree on the
// figure.
//
// THE FOLDS ARE BUILT BY FIELD rather than by folding events, because the point is to span the
// comparison space — stated against unstated, a zero fold, and a pair that ties on every compared
// field — not to re-test candidateOf.
//
// AND THAT IS ALSO THIS TEST'S ONE BLIND SPOT, named here rather than left to be discovered. The
// folds are built ONE-TO-ONE from PromptContext values, so every fold in the sweep is by construction
// publishable without loss — which is exactly the condition the two functions' agreement rests on.
// TokensMergedWith keeps the FOLD on a tie where MergePromptContext keeps the server figure, and that
// is invisible only because better() ties solely on equality of all four published fields, tokens
// included. A comparator added to the fold and NOT to PromptContext would make a tie with UNEQUAL
// tokens reachable and the two would diverge — and this fixture cannot express such a fold, so it
// would stay green. Anything adding a field to better() has to look at TokensMergedWith directly.
func TestPromptContextFold_TokensMergedWithAgreesWithMergePromptContext(t *testing.T) {
	at := time.Now()
	vals := []*PromptContext{
		nil,
		{Tokens: 100_000, Msgs: 952, At: at},
		{Tokens: 700_000, Msgs: 2468, At: at.Add(-time.Hour)}, // unstated, most messages
		{Tokens: 400_000, Msgs: 2468, At: at},                 // ties on msgs, later
		{Tokens: 200_000, Stated: true, At: at},
		{Tokens: 900_000, Stated: true, At: at.Add(-time.Minute)},
		{Tokens: 500_000, Msgs: 600, Stated: true, At: at},
	}
	// The zero fold first: it is the identity, and it is the one operand whose Publish() is nil, so
	// it is where a restated identity check would diverge.
	folds := []PromptContextFold{{}}
	for _, v := range vals[1:] {
		folds = append(folds, PromptContextFold{
			tokens: v.Tokens, msgs: v.Msgs, at: v.At, stated: v.Stated})
	}

	statedSeen, unstatedSeen, publishedWon, foldWon := false, false, false, false
	for _, f := range folds {
		for _, p := range vals {
			want := 0
			if merged := MergePromptContext(p, f.Publish()); merged != nil {
				want = merged.Tokens
			}
			got := f.TokensMergedWith(p)
			if got != want {
				t.Errorf("fold %+v merged with %+v = %d, want %d — the allocation-free path has "+
					"drifted from MergePromptContext", f.current(), p, got, want)
			}
			if p != nil && p.Stated {
				statedSeen = true
			}
			if p != nil && !p.Stated {
				unstatedSeen = true
			}
			if p != nil && want == p.Tokens && want != f.tokens {
				publishedWon = true
			}
			if want == f.tokens && f.tokens != 0 && (p == nil || want != p.Tokens) {
				foldWon = true
			}
		}
	}
	// Or the sweep could agree by never exercising a disagreement.
	if !statedSeen || !unstatedSeen || !publishedWon || !foldWon {
		t.Fatalf("the fixture does not span the space: stated=%v unstated=%v publishedWon=%v "+
			"foldWon=%v", statedSeen, unstatedSeen, publishedWon, foldWon)
	}
}

// THE SECOND MONOID, which the spec claims separately from the fold's. MergePromptContext is what
// the client and any future restore call, so its laws are load-bearing independently.
//
// THE VALUE SET SPANS BOTH CLASSES on purpose — three stated members and three unstated ones, plus
// nil — so the laws are exercised across the dominance arm AND inside each class's own ordering,
// rather than only where dominance settles it. Every member differs from every other on at least
// one compared field, which matters because the commutativity check below compares POINTERS: two
// distinct but fully tied operands would each be kept as the left one and fail it.
//
// AND Msgs IS LOAD-BEARING HERE, not carried along. The overall maximum is the last member, which
// wins only on the stated arm's Msgs comparator — it ties the fourth member on both At and Tokens.
// Before Msgs was published those two were indistinguishable on the wire.
func TestMergePromptContext_IsACommutativeMonoid(t *testing.T) {
	at := time.Now()
	vals := []*PromptContext{
		nil,
		{Tokens: 100_000, Msgs: 952, At: at},
		{Tokens: 700_000, Msgs: 2468, At: at.Add(-time.Hour)}, // unstated, most messages
		{Tokens: 400_000, Msgs: 2468, At: at},                 // ties on msgs, later
		{Tokens: 200_000, Stated: true, At: at},
		{Tokens: 900_000, Stated: true, At: at.Add(-time.Minute)},
		{Tokens: 200_000, Msgs: 40, Stated: true, At: at}, // ties At+Tokens above, wins on Msgs
	}

	// Check that claim rather than assert it, so the fixture cannot quietly stop exercising Msgs.
	var top *PromptContext
	for _, v := range vals {
		top = MergePromptContext(top, v)
	}
	if top != vals[6] {
		t.Fatalf("overall max is %+v, want %+v — the fixture no longer turns on the stated arm's "+
			"Msgs comparator", top, vals[6])
	}

	for _, x := range vals {
		for _, y := range vals {
			if MergePromptContext(x, y) != MergePromptContext(y, x) {
				t.Errorf("not commutative for %+v, %+v", x, y)
			}
			for _, z := range vals {
				if MergePromptContext(MergePromptContext(x, y), z) != MergePromptContext(x, MergePromptContext(y, z)) {
					t.Errorf("not associative for %+v, %+v, %+v", x, y, z)
				}
			}
		}
	}
}

// Nothing to say is zero, which the gauge renders as a dash rather than an empty track.
func TestSessionContext_ZeroWhenNothingCanBeSaid(t *testing.T) {
	base := time.Now()
	for _, tc := range []struct {
		name   string
		events []SessionEvent
	}{
		{"no events at all", nil},
		{"no inference on any event", []SessionEvent{
			{Phase: SessionRequest}, {Phase: SessionResponse},
		}},
		// Every request a one-shot: there is no conversation to report on, and reporting a
		// one-shot's own context is the defect this rule exists to fix.
		{"one-shots only", append(oneShot("o1", base, 60_000), oneShot("o2", base, 61_000)...)},
		// A conversation whose response reported no token counts at all — through exchange
		// directly, because conversation() takes a context and cannot express zero.
		{"a conversation with no prompt counts", exchange("c1", base, 40, 27, 0, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PromptContextOf(tc.events); got != 0 {
				t.Errorf("PromptContextOf = %d, want 0", got)
			}
		})
	}
}

// A ZERO-TOKEN FIGURE IS NOTHING KNOWN, in either operand position and in both combine paths.
//
// THE BAD CASE IS `&PromptContext{Stated: true}`, which better() ranks first on the dominance check:
// before nothingKnown it beat a real unstated local figure of 700,000 and both combines returned 0 —
// the gauge blanking for a session near its limit, from an operand carrying no information at all.
//
// LATENT, NOT LIVE: Publish() is the only producer today and returns nil rather than a zero-token
// struct. But PromptContext is exported with exported fields, so a JSON decode, a test or a future
// producer can construct one, and the identity is pinned here for the same reason Add keeps its
// provably-dead identity disjunct — it is a property of the monoid rather than an accident of which
// field better() happens to compare last.
func TestMergePromptContext_AZeroTokenFigureIsTheIdentity(t *testing.T) {
	at := time.Now()
	for _, empty := range []*PromptContext{
		{Stated: true}, // the dangerous one: wins the dominance check against any unstated figure
		{Stated: true, At: at.Add(time.Hour), Msgs: 9_000}, // and would win it later and longer
		{Msgs: 10_000},             // unstated, but ahead on the arm's leading comparator
		{At: at.Add(time.Hour)},    // unstated, ahead on the arm's second
		{Tokens: -1, Msgs: 10_000}, // <= 0 rather than == 0: nothing refuses a negative
		{},                         // and the plain zero value
	} {
		// The local figure an old proxy's fold arrives at: unstated, 700k held from before a
		// compaction, which is the documented stale-fallback state.
		local := &PromptContext{Tokens: 700_000, At: at, Msgs: 2468}
		if got := MergePromptContext(local, empty); got != local {
			t.Errorf("MergePromptContext(local, %+v) = %+v, want the local figure — an empty "+
				"operand is the identity, not a figure of zero", empty, got)
		}
		if got := MergePromptContext(empty, local); got != local {
			t.Errorf("MergePromptContext(%+v, local) = %+v, want the local figure", empty, got)
		}

		// And the allocation-free path agrees, which it has to: it restates the identity handling
		// and nothing else, so this is where a divergence would live.
		var f PromptContextFold
		f.AddAll(conversation("c1", at, 2468, 700_000))
		if got, want := f.TokensMergedWith(empty), 700_000; got != want {
			t.Errorf("TokensMergedWith(%+v) = %d, want %d", empty, got, want)
		}
	}

	// CLOSED OVER "NOTHING KNOWN", IN BOTH DIRECTIONS, which the loop above cannot reach: every pair
	// it builds has one real figure in it, so it never asks what two empty operands merge to. The
	// answer has to be nil — a result that is itself a valid operand — rather than whichever of them
	// was passed, or the identity is true of the inputs and false of the output, and the next merge
	// or the next marshal carries a "nothing known" value shaped exactly like a real figure.
	for _, pair := range [][2]*PromptContext{
		{nil, {Stated: true}},           // the direction that returned the figure
		{{Stated: true}, nil},           // and the one that already returned nil
		{{Stated: true}, {Msgs: 9_000}}, // neither nil, neither saying anything
		{nil, nil},
	} {
		if got := MergePromptContext(pair[0], pair[1]); got != nil {
			t.Errorf("MergePromptContext(%+v, %+v) = %+v, want nil — two operands that say nothing "+
				"must merge to nothing", pair[0], pair[1], got)
		}
	}
}

// THE CURSOR CONTRACT, pinned in the package that owns it: AddAll advances n, Add does not.
//
// n is a cursor into a CALLER's slice — abctl reads it through Folded() to decide whether a slice
// grew, shrank or was replaced wholesale — and a per-event caller has no slice, so the session store
// folds thousands of events and leaves n at zero forever. Until this test, adding `f.n++` to Add left
// `go test ./core/pipeline/` entirely green: the only thing guarding the contract stated at AddAll
// was cmd/abctl/tui, a suite in another module, and the fold has a second consumer now.
//
// COUNTED IN EVENTS, NOT IN CANDIDATES. AddAll advances by len(events) including the one-shots the
// rule rejects, because the length check it serves compares against len(slice) — a cursor that
// counted only winners would re-fold the whole tail on every call.
func TestPromptContextFold_AddAllAdvancesTheCursorAndAddDoesNot(t *testing.T) {
	base := time.Now()
	evs := conversation("c1", base, 600, 500_000)
	evs = append(evs, oneShot("o1", base.Add(time.Minute), 282_000)...) // rejected, still counted

	var byAll PromptContextFold
	byAll.AddAll(evs)
	if got, want := byAll.Folded(), len(evs); got != want {
		t.Errorf("AddAll left Folded() = %d, want %d — every event, one-shots included", got, want)
	}

	var byOne PromptContextFold
	for i := range evs {
		byOne.Add(&evs[i])
	}
	if got := byOne.Folded(); got != 0 {
		t.Errorf("Add advanced Folded() to %d, want 0 — the store's caller has no slice to index "+
			"and never resets this", got)
	}
	// Same figure either way, so the two entry points differ in the cursor and nothing else.
	if got, want := byOne.Tokens(), byAll.Tokens(); got != want {
		t.Errorf("per-event fold = %d, whole-slice fold = %d", got, want)
	}
	if got, want := byAll.Tokens(), 500_000; got != want {
		t.Errorf("folded %d, want %d — the fixture is not exercising the rule", got, want)
	}

	// ResetFolded zeroes the cursor and KEEPS the figure, which is the whole of what a rebase needs
	// from it: dropping the figure there blanks a live session's gauge.
	byAll.ResetFolded()
	if got := byAll.Folded(); got != 0 {
		t.Errorf("ResetFolded left Folded() = %d, want 0", got)
	}
	if got, want := byAll.Tokens(), 500_000; got != want {
		t.Errorf("ResetFolded dropped the figure: %d, want %d", got, want)
	}
}

// THE PROJECTED SHAPE, pinned in the package that owns the function that reads it.
//
// sessionapi.summarizeEvent nils Messages and Tools — 99.5% of an event — after recording their
// lengths in ToolCount and MessageCount, and toolCount/messageCount exist to answer on either shape.
// Until this test no fixture in this package set the counts with the slices nil, so the branch that
// exists FOR that shape was reached only from cmd/abctl/tui, across a module boundary, while
// statement coverage here looked complete.
//
// A CLIENT CONCERN, NOT A SERVER ONE, and worth stating because the reverse is easy to assume: the
// store folds whole events with their slices populated, so Append always takes the len() branch. The
// function lives here regardless, so its own suite pins both branches rather than resting on another
// package's.
func TestSessionContext_ReadsAProjectedEventThroughTheCounts(t *testing.T) {
	base := time.Now()

	// ToolCount is all that says this was an agentic turn rather than a one-shot.
	if got, want := PromptContextOf(projected(conversation("c1", base, 600, 500_000))), 500_000; got != want {
		t.Errorf("PromptContextOf = %d, want %d — a projected turn is admitted through ToolCount",
			got, want)
	}
	// And a projected one-shot is still excluded, by a count of zero rather than a missing manifest.
	if got := PromptContextOf(projected(oneShot("o1", base, 282_000))); got != 0 {
		t.Errorf("PromptContextOf = %d, want 0 — ToolCount 0 is a one-shot", got)
	}

	// MessageCount has to RANK, not merely admit. Two unstated projected turns, the earlier one
	// longer: messageCount leads the unstated arm, so the 1500-message turn wins. Read the slices
	// only and both tie at msgs == 0, at decides, and the later smaller turn takes it — which is how
	// this branch going missing would show, and why the assertion is a ranking rather than a read.
	evs := projected(conversation("before", base, 1500, 851_000))
	evs = append(evs, projected(conversation("after", base.Add(time.Hour), 40, 62_000))...)
	if got, want := PromptContextOf(evs), 851_000; got != want {
		t.Errorf("PromptContextOf = %d, want %d — MessageCount has to rank the projected turns, "+
			"not just let them through", got, want)
	}
}
