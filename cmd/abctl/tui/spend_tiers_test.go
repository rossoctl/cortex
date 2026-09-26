package tui

import (
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/core/pricing"
	"github.com/rossoctl/cortex/core/usage"
)

// tierCounts is one window's totals with a cache-heavy modelled mix.
//
// The mix inverts tokens against money on purpose: cache-read is the largest token tier and
// output the largest money tier. A fixture that ranked both the same way would pass against
// a renderer ordering by token count, which is the mistake this whole feature exists to fix.
func tierCounts() usage.Counts {
	return usage.Counts{
		Requests: 35, CostMicros: 4_546_200,
		InputCostMicros: 3000, CacheWriteCostMicros: 7500,
		CacheReadCostMicros: 30000, OutputCostMicros: 45000,
	}
}

// tierRowsOnly drops the reasoning child row, leaving the four rate-tier rows.
//
// The child is always rendered — the panel's height must not follow its data — but it
// is NOT a tier: it carries no rate, it is excluded from the shares that sum to 100,
// and its money is already inside output's. Every assertion below about "each tier
// row" therefore has to be made against the tiers, and a test that iterated raw
// lines would be asserting tier properties of something that is not one.
//
// MATCHED ON childTierLabel, not on "any leading space". A leading-space test says
// "indented" when the thing meant is "is the child", and the two come apart the
// moment a tier row gains an indent: the helper would silently drop real tiers and
// several assertions below would weaken without any of them failing.
func tierRowsOnly(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.HasPrefix(l, childTierLabel) {
			continue
		}
		out = append(out, l)
	}
	return out
}

// THE MONEY COLUMN IS RIGHT-ALIGNED, so the decimal points line up down the panel.
//
// It was left-flushed directly after the bar, which put "$56.51", "$25.34", "$15.42" and "$2.11"
// in three different places — measured on a live panel. That is the exact defect renderSpendBand
// argued about for itself ("left-flushed in cells of their own widths, '$4.04' and '$703.18' put
// their decimal points four columns apart") and that the tables already avoid; this panel is a
// column of four money figures read against each other and was the one surface that never got it.
//
// Asserted on the END column of each figure rather than the start: right-aligned means the last
// character shares a column, and a test on the start would pass for a left-flushed panel whose
// figures happened to be the same length.
//
// IN DISPLAY COLUMNS, and getting this wrong is how the test failed before the renderer did.
// strings.Index returns a BYTE offset while the column a reader sees is a display width, and a bar
// of eighth-blocks is three bytes per column — so the first version of this test reported the four
// figures ending at 41, 43, 55 and 63 for a panel that was already aligned at 39. The same
// confusion footer.go records the cost of, arriving in the assertion instead of the renderer.
func TestRenderTierRows_MoneyIsRightAligned(t *testing.T) {
	tiers, ok := tierCounts().ApportionTiers()
	if !ok {
		t.Fatal("the fixture apportions to nothing, so this test asserts nothing")
	}
	lines := tierRowsOnly(renderTierRows(tierCounts(), tierColumnWidth))
	type end struct {
		fig string
		at  int
	}
	var ends []end
	for i, line := range lines {
		// Every tier is in the fixture, so every row carries exactly one of the figures.
		found := false
		for _, micros := range tiers {
			f := formatUSDTotalMicros(micros)
			at := strings.Index(line, f)
			if at < 0 {
				continue
			}
			// Columns, not bytes: measure the text BEFORE the figure rather than trusting the
			// byte offset to be one.
			ends = append(ends, end{f, lipgloss.Width(line[:at]) + lipgloss.Width(f)})
			found = true
			break
		}
		if !found {
			t.Fatalf("row %d = %q carries none of the apportioned figures", i, line)
		}
	}
	if len(ends) != numTierRows {
		t.Fatalf("matched %d rows, want %d", len(ends), numTierRows)
	}
	for _, e := range ends[1:] {
		if e.at != ends[0].at {
			t.Errorf("%q ends at column %d but %q ends at %d — the money column is not "+
				"right-aligned, so the decimal points do not line up:\n%s",
				e.fig, e.at, ends[0].fig, ends[0].at, strings.Join(lines, "\n"))
		}
	}
	// Not vacuous: figures of differing widths are what alignment has to do work for.
	short, long := lipgloss.Width(ends[0].fig), lipgloss.Width(ends[0].fig)
	for _, e := range ends[1:] {
		if w := lipgloss.Width(e.fig); w < short {
			short = w
		} else if w > long {
			long = w
		}
	}
	if short == long {
		t.Logf("every figure is %d columns wide, so this fixture does not exercise padding", short)
	}
}

// EVERY TIER STATES ITS SHARE, AND THE SHARES SUM TO 100.
//
// The bar says "this one is bigger" and the figure says how much, but neither answers the question
// an operator actually brings to a cost breakdown — what FRACTION of the bill is cache? At the
// widths this panel really renders at, tierBarBudget degrades the bar to six columns, so the bar
// was carrying almost all of the proportion information in almost none of the space.
//
// SUMMING TO 100 IS THE ASSERTION, not each share individually, because four independent roundings
// do not: this fixture floors to 3 + 8 + 35 + 52 = 98. The remainder goes to the largest share for
// the same reason usage.Counts.ApportionTiers gives for its own micro — that is where it is
// proportionally smallest and cannot flip a rank — so a panel that reordered its bars to make the
// arithmetic work would be caught by TestRenderTierRows_RanksByCostNotByDeclarationOrder.
func TestRenderTierRows_SharesSumTo100(t *testing.T) {
	lines := tierRowsOnly(renderTierRows(tierCounts(), tierColumnWidth))
	total, found := 0, 0
	for _, line := range lines {
		pct, ok := sharePercent(line)
		if !ok {
			t.Errorf("row %q states no share; every tier with money in it must", line)
			continue
		}
		found++
		total += pct
	}
	if found != numTierRows {
		t.Fatalf("%d of %d rows state a share", found, numTierRows)
	}
	if total != 100 {
		t.Errorf("the shares sum to %d%%, not 100%% — four independent roundings do not add up, so "+
			"the remainder must land on the largest:\n%s", total, strings.Join(lines, "\n"))
	}
}

// sharePercent reads the "NN%" a row states, if it states one.
func sharePercent(line string) (int, bool) {
	i := strings.Index(line, "%")
	if i < 0 {
		return 0, false
	}
	j := i
	for j > 0 && line[j-1] >= '0' && line[j-1] <= '9' {
		j--
	}
	if j == i {
		return 0, false
	}
	n, err := strconv.Atoi(line[j:i])
	if err != nil {
		return 0, false
	}
	return n, true
}

// A TIER UNDER HALF A PERCENT SAYS "<1%", NEVER "0%".
//
// "0%" beside a non-zero figure is the one claim this panel refuses, arriving through the share
// column instead of the money one. renderTierRows' own doc spells out the rule for the figure —
// never $0.00, because that asserts the tier was FREE — and a share rounded down to nothing asserts
// exactly the same thing about the same tier, one column to the left. Measured before the fix:
// $0.20 of a $99.20 window rendered "input  0%  $0.20", which is self-contradicting on one row.
//
// "<1%" rather than a decimal place: a decimal would claim a precision the modelled mix does not
// have, and humanizeDurationMs already established this spelling for the same situation ("<1ms" for
// a duration too small to state at the panel's precision but too real to call zero).
//
// IT DOES NOT COUNT TOWARD THE 100, which is why this is a rendering rule and not an arithmetic one:
// tierShares still floors to 0 and gives the remainder to the largest tier, so the stated shares
// still sum to 100 — see TestRenderTierRows_SharesSumTo100. A "<1%" row is the disclosure that the
// sum is carrying it.
func TestRenderTierRows_ASubPercentTierIsNotZero(t *testing.T) {
	c := usage.Counts{
		Requests: 100, CostMicros: 99_200_000,
		InputCostMicros: 200, CacheWriteCostMicros: 25_000,
		CacheReadCostMicros: 56_000, OutputCostMicros: 18_000,
	}
	lines := renderTierRows(c, tierColumnWidth)
	var input string
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "input") {
			input = line
		}
	}
	if input == "" {
		t.Fatalf("no input row:\n%s", strings.Join(lines, "\n"))
	}
	// The premise: the tier really does hold money.
	if !strings.Contains(input, "$0.20") {
		t.Fatalf("the fixture no longer apportions $0.20 to input, so this asserts nothing: %q", input)
	}
	if strings.Contains(input, "0%") && !strings.Contains(input, "<1%") {
		t.Errorf("input renders a 0%% share beside $0.20, which says the tier was free: %q", input)
	}
	if !strings.Contains(input, "<1%") {
		t.Errorf("input = %q, want a \"<1%%\" share", input)
	}
	// And a tier that really is over 1% still states its number.
	if !strings.Contains(strings.Join(lines, "\n"), "57%") {
		t.Errorf("no row states a plain percentage, so the rule swallowed them all:\n%s",
			strings.Join(lines, "\n"))
	}
}

// A TIER THE MIX NEVER MENTIONED STATES NO SHARE, for the same reason it states no money.
//
// "0%" is a claim — this tier was free — where the absent row means "not known here". The zero case
// and the no-mix case are one case in this renderer already (see renderTierRows), and a share
// column must not be the door a "$0.00 for a figure that might be unknown" lie comes in through.
func TestRenderTierRows_AnAbsentTierStatesNoShare(t *testing.T) {
	c := tierCounts()
	c.CacheWriteCostMicros = 0 // never wrote cache
	lines := tierRowsOnly(renderTierRows(c, tierColumnWidth))

	var cacheWrite string
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "cache-write") {
			cacheWrite = line
		}
	}
	if cacheWrite == "" {
		t.Fatalf("no cache-write row at all:\n%s", strings.Join(lines, "\n"))
	}
	if _, ok := sharePercent(cacheWrite); ok {
		t.Errorf("cache-write states a share for a tier absent from the mix: %q", cacheWrite)
	}
	if !strings.Contains(cacheWrite, emptyCell) {
		t.Errorf("cache-write = %q, want the not-known cell %q", cacheWrite, emptyCell)
	}
	// The other three still state theirs, or this passes for a renderer that dropped shares whole.
	for _, line := range lines {
		if line == cacheWrite {
			continue
		}
		if _, ok := sharePercent(line); !ok {
			t.Errorf("row %q lost its share because a SIBLING tier was absent", line)
		}
	}
}

// THE BAR YIELDS BEFORE THE FIGURES, which is the rule tierBarBudget already stated and which the
// share column has to inherit: a bar is decoration over a number that is printed anyway, while the
// share IS a number. So a terminal too narrow for both keeps the share and drops the bar.
func TestRenderTierRows_TheBarYieldsBeforeTheShare(t *testing.T) {
	narrow := tierRowsOnly(renderTierRows(tierCounts(), tierLabelWidth+1+tierPctWidth+1+tierMoneyWidth))
	for _, line := range narrow {
		if strings.ContainsAny(line, "█▉▊▋▌▍▎▏") {
			t.Errorf("row %q drew a bar at a width that only fits the figures", line)
		}
		if _, ok := sharePercent(line); !ok {
			t.Errorf("row %q gave up its share to keep a bar", line)
		}
	}
}

// Ranked by MONEY, descending — not by pricing.Tier's declaration order, which starts with
// input, and not by token count.
func TestRenderTierRows_RanksByCostNotByDeclarationOrder(t *testing.T) {
	lines := tierRowsOnly(renderTierRows(tierCounts(), 60))
	if len(lines) != numTierRows {
		t.Fatalf("lines = %d, want exactly %d: %q", len(lines), numTierRows, lines)
	}
	want := []string{"output", "cache-read", "cache-write", "input"}
	for i, w := range want {
		if !strings.Contains(lines[i], w) {
			t.Errorf("row %d = %q, want %q (full: %q)", i, lines[i], w, lines)
		}
	}
	// pricing.Tier declares input first; if that order leaked through, row 0 is input.
	if strings.HasPrefix(strings.TrimSpace(lines[0]), "input") {
		t.Errorf("row 0 is the input tier, so the rows follow declaration order, not cost")
	}
}

// NO TIER figure wears the inexact marker, which is the reverse of what this test used to assert.
//
// SCOPED TO tierRowsOnly, because "no row in this panel wears the marker" is FALSE: the reasoning
// child wears one, by the rule spend_tiers.go:108 states ("ONE FIGURE WEARS inexactMarker: the
// reasoning child, and only it"). This loop passed over that only because tierCounts() reports no
// split, so the child rendered the not-known cell and never reached the marker branch — the
// fixture was doing the work, not the panel. Swapping in reasoningCounts() failed it outright.
// The complement — that the child DOES wear one — is TestRenderTierRows_OnlyTheChildWearsTheInexactMarker.
//
// Every figure here still IS modelled — the mix is the rate table's while the total may be the
// gateway's — so the disclosure was real and was given up rather than made unnecessary. It was
// dropped because this panel has no money column HEADER to hold it, unlike the sessions table and
// the band, which left a glyph on every row as the only alternative. renderTierRows records the
// argument.
//
// Inverted rather than deleted, because the marker coming BACK is a change someone should have to
// make deliberately: it would put a tilde on every row of the panel again.
func TestRenderTierRows_MarksNoFigureInexact(t *testing.T) {
	rows := 0
	for _, line := range tierRowsOnly(renderTierRows(tierCounts(), 60)) {
		if line == "" {
			continue
		}
		rows++
		if strings.Contains(line, inexactMarker) {
			t.Errorf("tier row %q carries %q; the tier rows state the caveat nowhere", line,
				inexactMarker)
		}
	}
	if rows == 0 {
		t.Fatal("no rows rendered, so the loop asserted nothing")
	}
}

// And they read in cents, like every other scanned money surface.
func TestRenderTierRows_FiguresReadInCents(t *testing.T) {
	joined := strings.Join(renderTierRows(tierCounts(), 60), "\n")
	// tierCounts' output tier apportions to $2.39; four decimals would render "$2.3927".
	if !strings.Contains(joined, "$2.39") {
		t.Errorf("no cents figure in:\n%s", joined)
	}
	if strings.Contains(joined, "$2.3927") {
		t.Errorf("a tier row kept four decimals:\n%s", joined)
	}
}

// No mix means the "not known here" cell, never $0.00 and never a guess.
func TestRenderTierRows_NoMixRendersTheUnknownCell(t *testing.T) {
	lines := tierRowsOnly(renderTierRows(usage.Counts{Requests: 35, CostMicros: 4_546_200}, 60))
	if len(lines) != numTierRows {
		t.Fatalf("lines = %d, want %d even with no mix", len(lines), numTierRows)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, emptyCell) {
		t.Errorf("no mix but no %q cell:\n%s", emptyCell, joined)
	}
	if strings.Contains(joined, "$0.00") {
		t.Errorf("rendered $0.00 for an unknown tier:\n%s", joined)
	}
}

// Reasoning is never a TIER here, which is not the same as never being shown.
//
// This test used to assert reasoning was absent entirely. It is now displayed, as an
// indented child of output, because the panel was the only cost surface that could
// not answer "what is my effort setting costing me". What has NOT changed is the
// reason the original assertion existed: reasoning is a SUBSET of output — tokenSplit
// labels it "reasoning (of output)" — so it must never be counted as a peer. There
// are five token kinds on Counts and four rate tiers, and that difference is still
// the point; it is now carried by the indent and by exclusion from tierShares rather
// than by the row's absence.
//
// The fixture reports reasoning tokens precisely so a renderer that enumerated KINDS
// as TIERS fails here.
func TestRenderTierRows_ReasoningIsNotATier(t *testing.T) {
	c := tierCounts()
	c.ReasoningTokens = 12_000
	c.OutputTokens = 42_000
	c.PresentKinds = uint8(usage.KindOutput | usage.KindReasoning)
	lines := renderTierRows(c, 60)

	// numTierRows counts RATES, and `numTierRows != pricing.NumTiers` was asserted here
	// — a tautology, since that is the const's definition. What is worth pinning is that
	// the SUMMING rows are still exactly the rate tiers, which is a property of the
	// render and can fail.
	if got := len(tierRowsOnly(lines)); got != pricing.NumTiers {
		t.Errorf("%d summing rows against %d rate tiers; reasoning became a tier",
			got, pricing.NumTiers)
	}
	// The reasoning row must be INDENTED — flush left it reads as a fifth tier.
	//
	// ANCHORED ON THE LABEL FIRST. The row is formatted from childTierLabel, so
	// `HasPrefix(row, childTierLabel)` is always true and flattening the label to plain
	// "reasoning" would keep such a check green. The indent has to be asserted on the
	// label itself, which is the thing that can change.
	if !strings.HasPrefix(childTierLabel, " ") {
		t.Errorf("childTierLabel %q is flush left, so the row reads as a fifth tier", childTierLabel)
	}
	found := false
	for _, l := range lines {
		if !strings.Contains(l, "reasoning") {
			continue
		}
		found = true
		if !strings.HasPrefix(l, " ") {
			t.Errorf("reasoning row is flush with the tiers, so it reads as a peer: %q", l)
		}
	}
	if !found {
		t.Fatal("no reasoning row rendered, so the indent check above cannot fail")
	}
	// And it must not be in the sum the tier rows own.
	total := 0
	for _, l := range tierRowsOnly(lines) {
		if pct, ok := sharePercent(l); ok {
			total += pct
		}
	}
	if total != 100 {
		t.Errorf("tier shares sum to %d%%, want 100%% — reasoning is being double-counted", total)
	}
}

// The line count is IDENTICAL across every state and width.
//
// The panel sits in a fixed reservation that layout() computes from the terminal height and
// cannot consult this function. A renderer whose height follows its data is what overflowed
// this pane by five rows and under-filled it by six.
func TestRenderTierRows_HeightIsConstant(t *testing.T) {
	for name, c := range map[string]usage.Counts{
		// A REPORTED SPLIT among the fixtures, so the populated child is rendered at every
		// width in the sweep — including the narrow ones where tierBarBudget returns 0 and
		// reasoningChildRow takes its bar-less branch, which nothing else renders.
		"reasoning": reasoningCounts(),
		"full mix":  tierCounts(),
		"no mix":    {Requests: 35, CostMicros: 4_546_200},
		"empty":     {},
		"one tier":  {CostMicros: 4_546_200, OutputCostMicros: 45000},
		"negative":  {CostMicros: -5, OutputCostMicros: 45000},
	} {
		for _, w := range []int{10, 20, 34, 46, 60, 100, 200} {
			got := renderTierRows(c, w)
			// tierPanelLines: four tiers plus the reasoning child, which renders the
			// not-known cell rather than vanishing when no split was reported. Constant
			// is the invariant; the constant itself grew by one.
			if len(got) != tierPanelLines {
				t.Errorf("%s at width %d: %d lines, want %d", name, w, len(got), tierPanelLines)
			}
			for i, line := range got {
				if n := len([]rune(line)); n > w {
					t.Errorf("%s at width %d: row %d is %d runes: %q", name, w, i, n, line)
				}
			}
		}
	}
}

// A negative total is not a total: the same refusal every other money surface here makes.
func TestRenderTierRows_NegativeTotalIsRefused(t *testing.T) {
	joined := strings.Join(renderTierRows(usage.Counts{
		CostMicros: -5, OutputCostMicros: 45000,
	}, 60), "\n")
	if !strings.Contains(joined, emptyCell) {
		t.Errorf("a negative total produced figures rather than %q:\n%s", emptyCell, joined)
	}
	if strings.Contains(joined, "$-") {
		t.Errorf("rendered a negative figure:\n%s", joined)
	}
}

// A tier absent from the modelled mix shows the unknown cell, NOT $0.0000.
//
// FOUND BY RENDERING, NOT BY A TEST, which is the lesson: tierCounts() populates all four
// tiers, so every assertion above was blind to a partial mix — and a partial mix is the
// normal case, since a window of cache-heavy traffic may report no cache WRITES at all.
// "$0.0000" in a money column asserts the tier was free, which is the lie this package
// refuses in sessionMoneyCell and in `abctl cost`'s headline.
func TestRenderTierRows_ATierAbsentFromTheMixIsUnknownNotFree(t *testing.T) {
	c := usage.Counts{
		Requests: 35, CostMicros: 4_546_200,
		InputCostMicros: 3000, OutputCostMicros: 45000, // no cache tiers in the mix
	}
	lines := tierRowsOnly(renderTierRows(c, 60))
	joined := strings.Join(lines, "\n")

	// Both spellings, because the figures read in cents now and "$0.00" is the one this panel
	// can actually produce — formatUSDTotalMicros(0) returns it, so the emptyCell guard above
	// the formatter is the only thing standing between an absent tier and a claim that it was
	// free. "$0.0000" is kept in the check to catch a revert to the four-decimal formatter.
	if strings.Contains(joined, "$0.0000") || strings.Contains(joined, "$0.00") {
		t.Errorf("a tier absent from the mix rendered as free:\n%s", joined)
	}
	// The two tiers that ARE in the mix keep their figures: this must not blank the column.
	if !strings.Contains(joined, "$") {
		t.Errorf("the tiers that are in the mix lost their figures:\n%s", joined)
	}
	// And the absent ones say so.
	var unknown int
	for _, line := range lines {
		if strings.Contains(line, emptyCell) {
			unknown++
		}
	}
	if unknown != 2 {
		t.Errorf("%d rows show %q, want 2 (cache-read and cache-write):\n%s",
			unknown, emptyCell, joined)
	}
}
