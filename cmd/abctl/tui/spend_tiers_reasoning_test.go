package tui

import (
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/core/usage"
)

// reasoningCounts is tierCounts plus a reported reasoning split: 948 of 1,593
// generated tokens were reasoning, captured from a live claude-opus-5 turn at
// effort "max".
func reasoningCounts() usage.Counts {
	c := tierCounts()
	c.OutputTokens = 1593
	c.ReasoningTokens = 948
	c.PresentKinds = uint8(usage.KindOutput | usage.KindReasoning)
	return c
}

// childRows returns the reasoning child rows.
//
// Matched on childTierLabel for the reason tierRowsOnly is: "indented and mentions
// reasoning" is a description of today's output, while the label is the thing
// actually meant. Renamed from indentedRows to stop the predicate drifting back.
func childRows(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.HasPrefix(l, childTierLabel) {
			out = append(out, l)
		}
	}
	return out
}

// The child row exists, names itself, and sits DIRECTLY under output wherever
// output ranked — the adjacency is what carries "subset" to a reader who does not
// know the indent convention.
func TestRenderTierRows_ReasoningIsAChildOfOutput(t *testing.T) {
	lines := renderTierRows(reasoningCounts(), tierColumnWidth)

	outputAt, reasoningAt := -1, -1
	for i, l := range lines {
		switch {
		case strings.HasPrefix(strings.TrimSpace(l), "output"):
			outputAt = i
		case strings.HasPrefix(l, childTierLabel):
			reasoningAt = i
		}
	}
	if outputAt < 0 {
		t.Fatal("no output row")
	}
	if reasoningAt < 0 {
		t.Fatal("no reasoning row; a reported reasoning split must be shown")
	}
	if reasoningAt != outputAt+1 {
		t.Errorf("reasoning is at %d and output at %d; the child must directly follow its parent",
			reasoningAt, outputAt)
	}
	// The label itself carries the indent, so matching it above is what proves the
	// row is a child rather than a peer; this pins the indent has not been flattened
	// out of childTierLabel while the tests kept passing.
	if !strings.HasPrefix(childTierLabel, " ") {
		t.Errorf("childTierLabel %q lost its indent; flush with the tiers it reads as a peer",
			childTierLabel)
	}
}

// THE INVARIANT THIS PANEL EXISTS ON: the rows that sum to the bill still sum to
// the bill. The child is excluded from that sum because its money is already inside
// output's — counting both double-counts every reasoning token at the output rate,
// which is the error usage.Counts warns about in the field's own doc comment.
func TestRenderTierRows_ChildIsExcludedFromTheHundredPercent(t *testing.T) {
	lines := renderTierRows(reasoningCounts(), tierColumnWidth)

	total, counted := 0, 0
	for _, l := range lines {
		if strings.HasPrefix(l, childTierLabel) { // the child, not a tier
			continue
		}
		pct, ok := sharePercent(l)
		if !ok {
			continue
		}
		counted++
		total += pct
	}
	if counted != numTierRows {
		t.Errorf("counted %d tier rows, want %d", counted, numTierRows)
	}
	if total != 100 {
		t.Errorf("the four tier shares sum to %d%%, want 100%% — the child must not be in the sum", total)
	}
}

// Containment, checked as arithmetic rather than left to the label: a child that
// renders a bigger figure than its parent is the one way this layout can lie, and
// it would look authoritative doing it.
//
// THE MALFORMED FIXTURE IS THE POINT. A well-formed one (948 of 1,593) cannot
// violate containment whatever the renderer does, so asserting it proves only that
// the arithmetic is not wildly broken — the clamps that actually protect the
// invariant never execute. Reasoning exceeding output should be impossible on the
// wire, which is exactly why a gateway that reports it that way would go unnoticed
// until the panel drew a child longer than its parent.
func TestRenderTierRows_ReasoningNeverExceedsOutput(t *testing.T) {
	sane := reasoningCounts()

	// Reasoning reported ABOVE output: the shape the clamps exist for.
	inverted := reasoningCounts()
	inverted.ReasoningTokens = 4_000 // > OutputTokens (1,593)

	// Reasoning equal to output: the boundary. Every comparison in the loop below is
	// `>`, so "the clamp must not overshoot and make the child SMALLER" needs its own
	// check — asserted at the end of the subtest rather than named here and left
	// untested.
	equal := reasoningCounts()
	equal.ReasoningTokens = equal.OutputTokens

	for _, tc := range []struct {
		name string
		c    usage.Counts
		// wantEqual says the child must render its parent's figure EXACTLY, which is the
		// only way to catch a clamp that subtracts. A field rather than a string match on
		// tc.name, so renaming the case cannot silently disable the assertion.
		wantEqual bool
	}{
		{name: "well-formed", c: sane},
		{name: "reasoning reported above output", c: inverted},
		{name: "reasoning equal to output", c: equal, wantEqual: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var outputPct, reasoningPct int
			var outputRow, reasoningRow string
			for _, l := range renderTierRows(tc.c, tierColumnWidth) {
				pct, ok := sharePercent(l)
				if !ok {
					continue
				}
				switch {
				case strings.HasPrefix(l, childTierLabel):
					reasoningPct, reasoningRow = pct, l
				case strings.HasPrefix(strings.TrimSpace(l), "output"):
					outputPct, outputRow = pct, l
				}
			}
			if reasoningRow == "" {
				t.Fatal("no reasoning row rendered")
			}
			// THE OPERAND IS ASSERTED, NOT FILTERED — the rule this subtest writes out for the
			// bars below and skipped here. sharePercent's ok is spent as a `continue` above, so
			// an output row whose share does not parse never lands in outputRow and leaves
			// outputPct at 0. `reasoningPct > outputPct` then reads 0 > 0 and passes on all three
			// cases, including the fixture built to report reasoning ABOVE output.
			//
			// ASSERTED ON THE ROW, NOT ON A ZERO SHARE, and the difference was measured:
			// tierShareCell floors any tier holding money to "<1%", which sharePercent reads back
			// as 1, so `outputPct == 0` is unreachable while the row exists. Guarding the value
			// would have been exactly the dead check this comment exists to avoid.
			if outputRow == "" {
				t.Fatal("no output row rendered; the share comparison has nothing to bound against")
			}
			if reasoningPct > outputPct {
				t.Errorf("reasoning is %d%% of the bill but output is only %d%%; a subset cannot "+
					"exceed its set\n  %s\n  %s", reasoningPct, outputPct, outputRow, reasoningRow)
			}
			// THE MONEY CELL NEEDS ITS OWN ASSERTION, because the share cannot stand in
			// for it. pct is DERIVED from micros, so a share that looks sane does not
			// witness a sane figure: floor(micros*100/total) collapses a range of micros
			// onto the same percentage, and on this fixture an unclamped child rendered
			// ~2.5x output while the share check stayed green.
			//
			// rOK/oOK are asserted rather than used as a filter: a `&&` over them would
			// let this assertion skip itself the moment the child renders not-known,
			// which is exactly how a guard goes quiet without failing.
			rMoney, rOK := rowMoney(reasoningRow)
			oMoney, oOK := rowMoney(outputRow)
			if !rOK || !oOK {
				t.Fatalf("no figure to compare (child ok=%v, parent ok=%v); this assertion "+
					"cannot fail:\n  %s\n  %s", rOK, oOK, outputRow, reasoningRow)
			}
			if rMoney > oMoney {
				t.Errorf("the child's figure $%.4f exceeds its parent's $%.4f:\n  %s\n  %s",
					rMoney, oMoney, outputRow, reasoningRow)
			}
			// BOTH OPERANDS MUST COUNT, asserted before either is trusted. A `>`
			// comparison is blind on both sides: a counter returning 0 for every row makes
			// it 0 > 0, and a CHILD that drew no bar makes it 0 > 12 — both pass..
			childBar, parentBar := drawnBarGlyphs(reasoningRow), drawnBarGlyphs(outputRow)
			if parentBar == 0 || childBar == 0 {
				t.Fatalf("bar glyphs: child %d, parent %d — a zero on either side makes the "+
					"comparison below unfailable:\n  %s\n  %s",
					childBar, parentBar, outputRow, reasoningRow)
			}
			if childBar > parentBar {
				t.Errorf("the child's bar is %d glyphs against its parent's %d:\n  %s\n  %s",
					childBar, parentBar, outputRow, reasoningRow)
			}
			// THE OTHER DIRECTION, which only the equal case witnesses: all of the output
			// was reasoning, so the child must render its parent's figure and its parent's
			// bar, not clamped-down ones. Without this a clamp that SUBTRACTS passes every
			// `>` above.
			if tc.wantEqual {
				if rMoney != oMoney {
					t.Errorf("all output was reasoning, so the child should equal its parent, "+
						"got $%.4f against $%.4f:\n  %s\n  %s", rMoney, oMoney, outputRow, reasoningRow)
				}
				if childBar != parentBar {
					t.Errorf("all output was reasoning, so the bars should match, got %d "+
						"against %d:\n  %s\n  %s", childBar, parentBar, outputRow, reasoningRow)
				}
			}
		})
	}
}

// barGlyphs are the eight block glyphs tierBar draws with, U+2588 through U+258F.
//
// A SET, NOT A RANGE, and the reason is worth keeping: the glyphs run BACKWARDS
// against visual width. '█' (full) is U+2588, the LOWEST code point, and '▏' (one
// eighth) is U+258F, the highest. Written as `r >= '▏' && r <= '█'` — which reads
// correctly as "from thinnest to fullest" — it compiles, vets clean, and is
// unsatisfiable: the counter returned 0 for every row, so the assertion using it
// could not fail. A set cannot be ordered wrongly.
const barGlyphs = "█▉▊▋▌▍▎▏"

// drawnBarGlyphs counts the block glyphs in a rendered row, which is the bar's drawn
// length. Counted rather than measured off an index because the bar sits between
// two variable-width cells.
func drawnBarGlyphs(row string) int {
	n := 0
	for _, r := range row {
		if strings.ContainsRune(barGlyphs, r) {
			n++
		}
	}
	return n
}

// rowMoney reads the dollar figure a rendered row ends with.
//
// Returns false for the not-known cell, which carries no figure — a row without one
// is not a row whose figure is zero, which is the distinction this panel is built on.
func rowMoney(row string) (float64, bool) {
	i := strings.LastIndex(row, "$")
	if i < 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(row[i+1:]), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// The panel's height is CONSTANT whether or not a split was reported. This is the
// invariant the drawer's fixed reservation depends on.
func TestRenderTierRows_HeightConstantAcrossReasoningStates(t *testing.T) {
	withSplit := renderTierRows(reasoningCounts(), tierColumnWidth)
	without := renderTierRows(tierCounts(), tierColumnWidth)
	if len(withSplit) != len(without) {
		t.Errorf("panel is %d lines with a split and %d without; height must not follow the data",
			len(withSplit), len(without))
	}
	if len(withSplit) != tierPanelLines {
		t.Errorf("panel is %d lines, want tierPanelLines = %d", len(withSplit), tierPanelLines)
	}
}

// The drawer reserves its height from a constant, so the extra line has to be in
// it — otherwise the child row pushes the footer off the terminal, the defect
// keys.go records for spendDrawerLines.
func TestSpendDrawerLines_AccountsForTheChildRow(t *testing.T) {
	// EQUALITY, NOT AN INEQUALITY, in both directions. `tierPanelLines < want` passed
	// when the panel returned FEWER rows than the constant — which is the case the
	// drawer's bare tiers[i] read used to panic on — and `got > spendDrawerLines` passed
	// on under-emission, which is the floating-footer bug this file documents. Each
	// inequality guarded one side of a two-sided invariant.
	if want := len(renderTierRows(reasoningCounts(), tierColumnWidth)); want != tierPanelLines {
		t.Errorf("the panel renders %d lines but tierPanelLines is %d", want, tierPanelLines)
	}
	// A LITERAL, not spendDrawerLinesFor(w): the renderer pads to exactly what that
	// function returns, so comparing the two compares the renderer with its own padding
	// rule and cannot fail. 7 is the two-column height — 100 clears
	// spendDrawerTwoColumnMin — and it is an independent witness.
	const w, wantLines = 100, 7
	if got := len(renderSpendDrawer(reasoningSnap(), nil, usage.GroupModel, "1h", w)); got != wantLines {
		t.Errorf("the drawer emitted %d lines at width %d, want %d; either way the footer moves",
			got, w, wantLines)
	}
}

// THE CHILD'S MONEY COLUMN ALIGNS WITH THE TIERS', which
// TestRenderTierRows_MoneyIsRightAligned cannot say: it wraps its input in
// tierRowsOnly and then locks the exclusion in with `len(ends) != numTierRows`, so
// the one row this feature adds is outside the alignment it has to obey.
//
// The child is the row most likely to break it — its label is exactly
// tierLabelWidth and was what forced that constant from 11 to 12.
func TestRenderTierRows_ChildMoneyColumnAlignsWithTheTiers(t *testing.T) {
	// EVERY CHILD STATE THAT CARRIES A FIGURE, because the fixture decides which branch
	// is measured. With only the apportioned case, a reported zero formatted on its own
	// no-bar path put its figure at column 28 against the tiers' 41 and nothing failed.
	reportedZero := reasoningCounts()
	reportedZero.ReasoningTokens = 0
	for _, tc := range []struct {
		name string
		c    usage.Counts
	}{
		{"apportioned figure", reasoningCounts()},
		{"reported zero", reportedZero},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertChildFigureAligns(t, tc.c)
		})
	}
}

func assertChildFigureAligns(t *testing.T, counts usage.Counts) {
	t.Helper()
	lines := renderTierRows(counts, tierColumnWidth)

	endOf := func(row string) int {
		i := strings.LastIndex(row, "$")
		if i < 0 {
			return -1
		}
		return lipgloss.Width(row[:i]) + lipgloss.Width(strings.TrimSpace(row[i:]))
	}
	var tierEnd, childEnd int
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, childTierLabel):
			childEnd = endOf(l)
		case tierEnd == 0:
			tierEnd = endOf(l)
		}
	}
	if tierEnd <= 0 || childEnd <= 0 {
		t.Fatalf("no figure to measure (tier %d, child %d):\n%s",
			tierEnd, childEnd, strings.Join(lines, "\n"))
	}
	if childEnd != tierEnd {
		t.Errorf("the child's figure ends at column %d and a tier's at %d, so the decimal "+
			"points do not line up:\n%s", childEnd, tierEnd, strings.Join(lines, "\n"))
	}
}

// childTierLabel must not EXCEED tierLabelWidth, which spend_tiers.go asserted in a
// comment and nothing checked.
//
// A CEILING, NOT AN EQUALITY, because only one direction is a hazard: fmt's %-*s pads a
// short label and never truncates a long one, so a shorter label still aligns and only a
// longer one pushes the share, bar and figure right.
func TestChildTierLabel_FitsTheLabelWidth(t *testing.T) {
	if n := len([]rune(childTierLabel)); n > tierLabelWidth {
		t.Errorf("childTierLabel %q is %d runes, above tierLabelWidth %d — fmt will not "+
			"truncate it, so it pushes the share, bar and figure right",
			childTierLabel, n, tierLabelWidth)
	}
	// And the alignment it exists to protect, measured rather than inferred from the width.
	//
	// GUARDED, NOT FILTERED. This had two silent escapes — `continue` when no row carried
	// the prefix, then `i >= 0 &&` when no "%" was found — so a child that stopped
	// rendering skipped the body entirely and the test went green. That is the shape this
	// file rejects by name a few tests down: asserted rather than used as a filter.
	found := false
	for _, l := range renderTierRows(reasoningCounts(), tierColumnWidth) {
		if !strings.HasPrefix(l, childTierLabel) {
			continue
		}
		found = true
		i := strings.Index(l, "%")
		if i < 0 {
			t.Fatalf("the child row states no share, so its column cannot be measured:\n  %q", l)
		}
		if got, want := lipgloss.Width(l[:i]), tierLabelWidth+tierPctWidth; got != want {
			t.Errorf("the child's share cell ends at column %d, not %d:\n  %q", got, want, l)
		}
	}
	if !found {
		t.Fatal("no child row rendered, so the alignment check above asserted nothing")
	}
}

// THE CHILD'S BAR YIELDS BEFORE ITS FIGURES, the rule every tier row obeys — and the
// child was exempted from the test that enforces it.
//
// TestRenderTierRows_TheBarYieldsBeforeTheShare iterates tierRowsOnly, which filters on
// childTierLabel, so replacing its leading-space predicate with the label silently
// removed the one row this feature added from that assertion. Deleting reasoningChildRow's
// `budget > 0` arm, so the child always formats with a bar, left the whole package green.
//
// ASSERTED ON THE FIGURE, NOT ON THE ABSENCE OF GLYPHS. At a budget of 0 the bar formats
// to nothing, so both branches produce a bar-less row and a glyph count cannot tell them
// apart — the difference is one space, which pushes the row a column over and clipRow
// truncates the money cell to "$1.4". A first version of this test asserted the glyph
// count and the width and passed under exactly that mutation.
func TestRenderTierRows_ChildBarYieldsBeforeItsFigures(t *testing.T) {
	narrow := tierLabelWidth + 1 + tierPctWidth + 1 + tierMoneyWidth
	if tierBarBudget(narrow) > 0 {
		t.Fatalf("width %d still affords a bar (budget %d), so this case asserts nothing",
			narrow, tierBarBudget(narrow))
	}
	atNarrow := childRows(renderTierRows(reasoningCounts(), narrow))
	atWide := childRows(renderTierRows(reasoningCounts(), tierColumnWidth))
	if len(atNarrow) != 1 || len(atWide) != 1 {
		t.Fatalf("want one child row at each width, got %d and %d", len(atNarrow), len(atWide))
	}

	// THE FIGURE IS INTACT, which is what yielding the bar buys. Compared against the
	// same data rendered wide: a row that kept its bar overflows by a column and clipRow
	// eats the last digit, which still parses as a number.
	narrowMoney, ok := rowMoney(atNarrow[0])
	if !ok {
		t.Fatalf("child row %q carries no figure", atNarrow[0])
	}
	wideMoney, ok := rowMoney(atWide[0])
	if !ok {
		t.Fatalf("child row %q carries no figure at full width", atWide[0])
	}
	if narrowMoney != wideMoney {
		t.Errorf("the child's figure is $%.4f narrow against $%.4f wide — the bar did not "+
			"yield and clipRow truncated it:\n  %q", narrowMoney, wideMoney, atNarrow[0])
	}
	// And the share survives too.
	if _, ok := sharePercent(atNarrow[0]); !ok {
		t.Errorf("child row = %q lost its share when the bar yielded", atNarrow[0])
	}
	// The child ends where the tiers end, or its column is not the same column.
	for _, l := range renderTierRows(reasoningCounts(), narrow) {
		if strings.HasPrefix(l, childTierLabel) {
			continue
		}
		if lipgloss.Width(l) != lipgloss.Width(atNarrow[0]) {
			t.Errorf("child row is %d columns and a tier row %d:\n  %q\n  %q",
				lipgloss.Width(atNarrow[0]), lipgloss.Width(l), atNarrow[0], l)
		}
		break
	}
}

// THE CHILD FOLLOWS OUTPUT'S RANK, not a fixed line.
//
// insertAfterOutput searches tierOrder for TierOutput, and that search was unasserted:
// replacing it with `at := 1` left the whole package green, because every adjacency
// fixture happened to rank output FIRST. The committed demo SVG does exercise the
// non-zero case — cache-read outranks output there and the child lands at index 2 — so
// the behaviour was live with only a picture to prove it.
//
// The fixture below ranks output LAST (cache-read, then input, then output), which is
// also the shape a cache-heavy agent turn actually produces.
func TestRenderTierRows_ChildFollowsOutputWhereverItRanks(t *testing.T) {
	c := usage.Counts{
		Requests: 3, CostMicros: 4_000,
		InputCostMicros: 900, CacheReadCostMicros: 2_800, OutputCostMicros: 300,
		OutputTokens: 900, ReasoningTokens: 400,
		PresentKinds: uint8(usage.KindInput | usage.KindCacheRead | usage.KindOutput | usage.KindReasoning),
	}
	lines := renderTierRows(c, tierColumnWidth)

	outputAt, childAt := -1, -1
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, childTierLabel):
			childAt = i
		case strings.HasPrefix(strings.TrimSpace(l), "output"):
			outputAt = i
		}
	}
	if outputAt < 0 || childAt < 0 {
		t.Fatalf("output at %d, child at %d:\n%s", outputAt, childAt, strings.Join(lines, "\n"))
	}
	// The fixture has to actually put output off the top, or this asserts what the other
	// adjacency tests already do.
	if outputAt == 0 {
		t.Fatalf("output ranked first, so this fixture cannot distinguish a rank search "+
			"from a fixed index:\n%s", strings.Join(lines, "\n"))
	}
	if childAt != outputAt+1 {
		t.Errorf("output is at %d and the child at %d; the child must follow its parent's "+
			"RANK, not a fixed line:\n%s", outputAt, childAt, strings.Join(lines, "\n"))
	}
}

// THE CHILD WEARS inexactMarker AND THE TIERS DO NOT.
//
// The panel dropped the marker from every row because a glyph on all of them
// distinguished nothing. The child re-earns one: it is modelled twice — ApportionTiers'
// mix, then a token ratio applied to a cost figure — so it is genuinely less certain
// than its siblings, and without the glyph it renders identically to rows modelled once
// while the extra approximation lives only in prose a TUI reader never sees.
func TestRenderTierRows_OnlyTheChildWearsTheInexactMarker(t *testing.T) {
	lines := renderTierRows(reasoningCounts(), tierColumnWidth)

	child := childRows(lines)
	if len(child) != 1 {
		t.Fatalf("want one child row, got %d", len(child))
	}
	if !strings.Contains(child[0], inexactMarker) {
		t.Errorf("child row = %q carries no %q; its second approximation is invisible",
			child[0], inexactMarker)
	}
	// Prefixed, per spend_strip.go's convention for this glyph.
	if !strings.Contains(child[0], inexactMarker+"$") {
		t.Errorf("child row = %q does not prefix the figure with %q", child[0], inexactMarker)
	}

	// And no tier row wears one, or the glyph distinguishes nothing again.
	marked := 0
	for _, l := range tierRowsOnly(lines) {
		if strings.Contains(l, inexactMarker) {
			marked++
			t.Errorf("tier row %q wears %q; then it cannot single out the child", l, inexactMarker)
		}
	}
	if marked == 0 && len(tierRowsOnly(lines)) == 0 {
		t.Fatal("no tier rows to compare against")
	}

	// The decimal points still line up, which is why the marker is prefixed and the
	// column is one wider rather than the glyph trailing.
	endOf := func(row string) int {
		i := strings.LastIndex(row, "$")
		if i < 0 {
			return -1
		}
		return lipgloss.Width(row[:i]) + lipgloss.Width(strings.TrimSpace(row[i:]))
	}
	if got, want := endOf(child[0]), endOf(tierRowsOnly(lines)[0]); got != want {
		t.Errorf("the child's figure ends at column %d and a tier's at %d; the marker pushed "+
			"the column out of line:\n%s", got, want, strings.Join(lines, "\n"))
	}
}

// THE CHILD CELL'S TRUTH TABLE — six states of ONE decision, which is why it is one table
// and not six functions. Split across functions, each state asserted only the dimension
// the round that found it cared about: the negative case never checked the marker, the
// tiny-share case never checked the figure. Every row now asserts all three (cell, figure,
// marker), so a state cannot be half-covered.
//
// The distinction the present bit exists to carry runs down the wantMoney column: nil is
// "no defensible figure, show the not-known cell", and a pointer to 0 is "the provider
// MEASURED the split and it was nothing". "-" for a value we have would discard it, and
// "$0.00" for a value we lack asserts the model reasoned for free.
func TestRenderTierRows_ChildCellTruthTable(t *testing.T) {
	zero := func(f float64) *float64 { return &f }

	reportedZero := reasoningCounts()
	reportedZero.ReasoningTokens = 0 // measured, and measured as nothing: the bit stays set

	zeroBitClear := reasoningCounts()
	zeroBitClear.ReasoningTokens = 0
	zeroBitClear.PresentKinds = uint8(usage.KindOutput) // nothing reported at all

	noDenominator := reasoningCounts()
	noDenominator.OutputTokens = 0

	negative := reasoningCounts()
	negative.ReasoningTokens = -948

	for _, tc := range []struct {
		name string
		c    usage.Counts
		// nil = the not-known cell; non-nil = that exact figure, in dollars.
		wantMoney  *float64
		wantMarker bool
		why        string
	}{
		{
			name: "unreported split",
			c:    tierCounts(),
			why: "no bit, no value: the row still renders (height is constant) and says it " +
				"does not know. A $0.00 here would assert the model did no reasoning.",
		},
		{
			name: "reported zero", c: reportedZero, wantMoney: zero(0), wantMarker: false,
			why: "the provider measured the split and it was nothing. `abctl cost`'s token " +
				"line prints 0 for the same Counts, so the not-known cell would split the " +
				"two surfaces. No marker: a zero costs zero at any rate, so nothing here is " +
				"modelled for a marker to qualify.",
		},
		{
			name: "reported zero with the bit clear", c: zeroBitClear,
			why: "the half of the present bit's job that must not move — same value as the " +
				"row above, opposite cell, because nothing was reported.",
		},
		{
			// 1 reasoning token of 900 output against 300 apportioned output micros:
			// 300 * 1/900 = 0.333, which truncates to zero.
			name: "share apportions to nothing",
			c: usage.Counts{
				Requests: 3, CostMicros: 4_000,
				InputCostMicros: 900, CacheReadCostMicros: 2_800, OutputCostMicros: 300,
				OutputTokens: 900, ReasoningTokens: 1,
				PresentKinds: uint8(usage.KindInput | usage.KindCacheRead | usage.KindOutput |
					usage.KindReasoning),
			},
			why: "the apportionment multiply truncates, so a REAL count whose share falls " +
				"below one micro yields micros == 0 — reachable around a hundred output " +
				"tokens at opus-5 rates. Not \"<$0.01\" either: that means \"too small to " +
				"state\", and what is true here is that the apportionment resolved nothing.",
		},
		{
			name: "no output to apportion by", c: noDenominator,
			why: "reasoning reported but nothing generated: no denominator, no figure.",
		},
		{
			name: "negative count", c: negative,
			why: "unreachable through the live parser, which screens negatives at ingest — " +
				"but renderTierRows takes a usage.Counts from the ledger and from any other " +
				"producer, and a negative would draw a bar from a negative length.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			child := childRows(renderTierRows(tc.c, tierColumnWidth))
			if len(child) != 1 {
				t.Fatalf("want exactly one child row in every state, got %d -- %s",
					len(child), tc.why)
			}
			row := child[0]
			gotMoney, hasMoney := rowMoney(row)
			hasNotKnown := strings.Contains(row, emptyCell)

			if tc.wantMoney == nil {
				if !hasNotKnown {
					t.Errorf("child row = %q, want the not-known cell -- %s", row, tc.why)
				}
				if hasMoney {
					t.Errorf("child row = %q carries the figure $%.4f where there is none to "+
						"state -- %s", row, gotMoney, tc.why)
				}
			} else {
				if hasNotKnown {
					t.Errorf("child row = %q shows the not-known cell for a MEASURED value "+
						"-- %s", row, tc.why)
				}
				if !hasMoney {
					t.Errorf("child row = %q carries no figure, want $%.2f -- %s",
						row, *tc.wantMoney, tc.why)
				} else if gotMoney != *tc.wantMoney {
					t.Errorf("child row = %q parsed $%.4f, want $%.2f -- %s",
						row, gotMoney, *tc.wantMoney, tc.why)
				}
			}

			if got := strings.Contains(row, inexactMarker); got != tc.wantMarker {
				t.Errorf("child row = %q wears %q = %v, want %v -- %s",
					row, inexactMarker, got, tc.wantMarker, tc.why)
			}

			// A negative must not reach the row in any form, figure or bar.
			if tc.name == "negative count" && strings.Contains(row, "-") {
				t.Errorf("child row = %q carries a negative figure", row)
			}
		})
	}
}
