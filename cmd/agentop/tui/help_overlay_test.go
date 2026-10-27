package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// keyRune is a helper for pressing a single-rune key.
func keyRune(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

// sizedPickerModel returns a picker model that has been given a terminal
// size, so View() renders the full frame rather than "initializing…".
func sizedPickerModel(t *testing.T) *model {
	t.Helper()
	m := newPickerModel(context.Background(), &fakeLister{namespaces: fixtureNamespaces}, nil)
	updated, _ := m.Update(m.Init()())
	mm := updated.(*model)
	updated, _ = mm.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return updated.(*model)
}

func TestHelpOverlayOpensAndCloses(t *testing.T) {
	mm := sizedPickerModel(t)
	if mm.helpVisible {
		t.Fatal("help overlay should start hidden")
	}
	updated, _ := mm.Update(keyRune('?'))
	mm = updated.(*model)
	if !mm.helpVisible {
		t.Fatal("`?` should open the help overlay")
	}
	// `?` again closes it (as does Esc / q — covered below).
	updated, _ = mm.Update(keyRune('?'))
	mm = updated.(*model)
	if mm.helpVisible {
		t.Fatal("`?` should toggle the help overlay closed")
	}
}

func TestHelpOverlayClosesOnEscAndQ(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyMsg
	}{
		{"esc", tea.KeyMsg{Type: tea.KeyEsc}},
		{"q", keyRune('q')},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mm := sizedPickerModel(t)
			updated, _ := mm.Update(keyRune('?'))
			mm = updated.(*model)
			updated, cmd := mm.Update(tc.key)
			mm = updated.(*model)
			if mm.helpVisible {
				t.Fatalf("%s should close the overlay", tc.name)
			}
			// Critically, `q` must not quit agentop while the overlay is up.
			if cmd != nil {
				t.Fatalf("%s on the overlay should not emit a Cmd (got quit?)", tc.name)
			}
		})
	}
}

// The overlay is modal: keys that would navigate the pane underneath must
// not reach it while help is up.
func TestHelpOverlayIsModal(t *testing.T) {
	mm := sizedPickerModel(t)
	updated, _ := mm.Update(keyRune('?'))
	mm = updated.(*model)
	// Enter would normally drill into the selected namespace.
	updated, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mm = updated.(*model)
	if mm.pane != paneNamespaces {
		t.Fatalf("Enter should be swallowed by the overlay; pane moved to %v", mm.pane)
	}
	if mm.selectedNamespace != "" {
		t.Fatalf("Enter should be swallowed; selectedNamespace = %q", mm.selectedNamespace)
	}
}

func TestHelpOverlayRendersAndEmphasizesCurrentPane(t *testing.T) {
	mm := sizedPickerModel(t)
	updated, _ := mm.Update(keyRune('?'))
	mm = updated.(*model)
	view := mm.View()
	// The active pane's group is rendered in full, marked as "this pane".
	if !strings.Contains(view, "NAMESPACES (this pane)") {
		t.Fatalf("overlay should emphasize the active pane's group:\n%s", view)
	}
	// The pane's PURPOSE, not just its keys. The overlay named nine panes and
	// described none of them before the pane-focused rewrite.
	if !strings.Contains(view, paneKeys[paneNamespaces].purpose) {
		t.Fatalf("overlay should say what the active pane shows:\n%s", view)
	}
	// `C` is the binding this overlay exists to make discoverable. On the picker it
	// does not work yet, so the section keeps its heading and names the catalog in
	// prose rather than advertising a dead key — see
	// TestHelpBody_PickerPanesExplainWhyThereIsNoJumpSection for both halves.
	if !strings.Contains(view, "plugin catalog") {
		t.Fatalf("overlay should name the plugin catalog:\n%s", view)
	}
	if !strings.Contains(view, jumpSectionTitle) {
		t.Fatalf("overlay should keep the %q heading on the picker:\n%s",
			jumpSectionTitle, view)
	}
	// Other panes are documented, not omitted. That they are documented IN FULL is
	// asserted on the body instead (see
	// TestHelpBody_EveryPaneSectionSpellsOutEveryDescription): the section runs
	// past the fold on any ordinary terminal, so View() legitimately shows only its
	// first rows.
	if !strings.Contains(view, everyPaneTitle) {
		t.Fatalf("overlay should list every other pane:\n%s", view)
	}
	if !strings.Contains(view, "close") {
		t.Fatalf("overlay should say how to close itself:\n%s", view)
	}
}

// The overlay must be reachable from the session views too, not just the
// picker — that is where `P` actually works.
func TestHelpOverlayAvailableInSessionPanes(t *testing.T) {
	for _, pane := range []paneID{
		paneSessions, paneEvents, paneDetail,
		panePipeline, panePluginDetail, paneCatalog,
	} {
		m := newPickerModel(context.Background(), &fakeLister{}, nil)
		m.pane = pane
		updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		mm := updated.(*model)
		updated, _ = mm.Update(keyRune('?'))
		mm = updated.(*model)
		if !mm.helpVisible {
			t.Fatalf("`?` should open the overlay from pane %v", pane)
		}
		if g, ok := paneKeys[pane]; !ok {
			t.Errorf("pane %v has no help group defined", pane)
		} else if !strings.Contains(mm.View(), g.title) {
			t.Errorf("overlay over pane %v missing its own group %q", pane, g.title)
		}
	}
}

// renderHelpOverlay must not panic or return nothing at small sizes, and
// the close hint must survive every one of them — it lives in the fixed
// footer precisely so a short terminal can't scroll it away.
func TestHelpOverlayTinyTerminal(t *testing.T) {
	for _, dim := range [][2]int{{0, 0}, {1, 1}, {20, 5}, {40, 10}} {
		m := &model{pane: paneSessions, width: dim[0], height: dim[1], helpVp: viewport.New(0, 0)}
		m.syncHelpViewport(true)
		out := renderHelpOverlay(m.helpVp, dim[0], dim[1], "", "")
		if out == "" {
			t.Errorf("renderHelpOverlay(%d,%d) returned empty", dim[0], dim[1])
		}
		// The close hint must be present whenever the terminal is tall
		// enough for the frame AND wide enough to hold the hint. Below
		// that width MaxWidth necessarily clips it — at 20 columns there
		// is no rendering that fits, so assert only that the hint line
		// started (the "[?]" prefix survives) rather than the full text.
		if dim[1] >= helpOverlayFrameH {
			want := "close"
			if dim[0] < lipgloss.Width("[?] or [esc] close")+6 {
				want = "[?]"
			}
			if !strings.Contains(out, want) {
				t.Errorf("renderHelpOverlay(%d,%d) lost the close hint (wanted %q):\n%s",
					dim[0], dim[1], want, out)
			}
		}
	}
}

// overlayCenter must preserve the base view's line count so the frame
// doesn't jump when the overlay opens.
func TestOverlayCenterPreservesHeight(t *testing.T) {
	base := strings.Repeat("x\n", 19) + "x" // 20 lines
	over := "AAA\nBBB"
	got := overlayCenter(base, over, 40, 20)
	if n := len(strings.Split(got, "\n")); n != 20 {
		t.Fatalf("overlayCenter changed line count: got %d, want 20", n)
	}
	if !strings.Contains(got, "AAA") || !strings.Contains(got, "BBB") {
		t.Fatalf("overlay content missing from composite:\n%s", got)
	}
}

// A base view shorter than the terminal should still get a centered
// overlay (padded), not one pinned to the first row.
func TestOverlayCenterPadsShortBase(t *testing.T) {
	got := overlayCenter("only one line", "PANEL", 40, 20)
	lines := strings.Split(got, "\n")
	if len(lines) != 20 {
		t.Fatalf("got %d lines, want 20", len(lines))
	}
	if strings.Contains(lines[0], "PANEL") {
		t.Fatal("overlay should be centered, not pinned to row 0")
	}
}

func TestTruncToWidth(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"hello", 5, "hello"},
		{"hello", 3, "hel"},
		{"hello", 0, ""},
		{"héllo", 3, "hél"},
	}
	for _, c := range cases {
		if got := truncToWidth(c.in, c.n); got != c.want {
			t.Errorf("truncToWidth(%q,%d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
	// Styled input keeps its escape but gets reset-terminated so the
	// overlay that follows isn't tinted by a dangling style. A literal
	// escape is used rather than styleTitle.Render because lipgloss
	// strips color when stdout isn't a TTY (as under `go test`), which
	// would make this assertion vacuous.
	styled := "\x1b[1;34mabcdef\x1b[0m"
	got := truncToWidth(styled, 3)
	if !strings.HasSuffix(got, "\x1b[0m") {
		t.Errorf("truncated styled string should end with a reset, got %q", got)
	}
	if !strings.Contains(got, "abc") {
		t.Errorf("truncated styled string should retain visible text, got %q", got)
	}
	if strings.Contains(got, "def") {
		t.Errorf("truncated styled string should drop text past the budget, got %q", got)
	}
	// Escape bytes must not consume the width budget.
	if w := lipgloss.Width(got); w != 3 {
		t.Errorf("truncToWidth budget miscounted escapes: width %d, want 3", w)
	}
}

// Every pane in otherPaneOrder must have a group, or the "OTHER PANES"
// section silently drops a pane.
func TestPaneKeysCoverAllPanes(t *testing.T) {
	for _, p := range otherPaneOrder {
		if _, ok := paneKeys[p]; !ok {
			t.Errorf("otherPaneOrder includes pane %v with no paneKeys entry", p)
		}
	}
	if len(paneKeys) != len(otherPaneOrder) {
		t.Errorf("paneKeys has %d entries, otherPaneOrder %d — keep them in sync",
			len(paneKeys), len(otherPaneOrder))
	}

	// Both lists agreeing is not enough: a new pane absent from BOTH satisfies
	// the checks above while being undocumented everywhere. paneUsage shipped
	// exactly that way — reachable with `u`, named in no footer and no overlay.
	// Enumerate against the real pane range so a new paneID has to be
	// documented or fail here.
	for p := paneNamespaces; p <= lastPaneID; p++ {
		if _, ok := paneKeys[p]; !ok {
			t.Errorf("pane %v has no paneKeys entry — it would be undiscoverable in the [?] overlay", p)
		}
	}
}

// Every pane must name its keys in the footer, and any pane reachable by a
// binding from elsewhere must have that binding advertised where it is pressed.
// A key nobody can discover is a key nobody uses.
func TestFooterHintsMentionUsageKey(t *testing.T) {
	m := &model{}
	for _, tc := range []struct {
		pane paneID
		want string
	}{
		{paneSessions, "[u] usage"},
		{paneEvents, "[u] usage"},
		{paneDetail, "[u] usage"},
		// The spend drawer's key, on the pane the README's own footer sample shows. It was
		// written down in the [?] overlay and in that sample and missing from the line both
		// describe, so `$` existed only for a reader who went looking for it.
		{paneSessions, "[$] spend"},
	} {
		m.pane = tc.pane
		if got := m.helpView(); !strings.Contains(got, tc.want) {
			t.Errorf("pane %v footer omits %q:\n  %s", tc.pane, tc.want, got)
		}
	}
	// Both spellings of the Sessions footer, since the drilled-in one is a separate string and
	// has its own [esc] pods.
	m.pane = paneSessions
	m.parentCtx = context.Background()
	if got := m.helpView(); !strings.Contains(got, "[$] spend") {
		t.Errorf("the drilled-in sessions footer omits [$] spend:\n  %s", got)
	}
	m.parentCtx = nil

	// The usage pane's own footer must not fall through to the bare default.
	m.pane = paneUsage
	got := m.helpView()
	for _, want := range []string{"[m] metric", "[w] window", "[b] breakdown"} {
		if !strings.Contains(got, want) {
			t.Errorf("usage footer omits %q:\n  %s", want, got)
		}
	}

	// Latency has no per-label breakdown, so the footer must not advertise [b]
	// there — a key shown as available but inert reads as a broken binding.
	m.usage.metric = metricLatency
	got = m.helpView()
	if strings.Contains(got, "[b]") {
		t.Errorf("latency footer advertises the inert breakdown key:\n  %s", got)
	}
	if !strings.Contains(got, "[m] metric") {
		t.Errorf("latency footer lost the metric key:\n  %s", got)
	}
}

// Every key the usage footer advertises must be one the pane actually handles,
// and every key it handles should be advertised. A binding nobody can discover
// and a hint that does nothing are the same class of bug.
func TestUsageFooterMatchesHandledKeys(t *testing.T) {
	m := &model{pane: paneUsage, selectedSess: "s1"}

	// Keys the pane handles, DRIVEN rather than copied. The list used to be
	// hand-written "per the paneUsage switch in handleKey" and never pressed, so the
	// footer-versus-handler claim in this test's name rested on the two lists having
	// been kept in sync by hand — which is the drift the test exists to catch.
	//
	// Each key is pressed on a fresh model and counted as handled if it changed the
	// pane's state. `m`/`b` cycle enums, `w` refetches with a new window, `s` toggles
	// scope; a key the switch stopped handling changes nothing and is reported.
	handled := []string{}
	for _, probe := range []struct {
		key     rune
		changed func(before, after *usageState) bool
	}{
		{'m', func(b, a *usageState) bool { return a.metric != b.metric }},
		{'w', func(b, a *usageState) bool { return a.windowIdx != b.windowIdx }},
		{'b', func(b, a *usageState) bool { return a.group != b.group }},
		{'s', func(b, a *usageState) bool { return a.session != b.session }},
	} {
		mm := &model{pane: paneUsage, selectedSess: "s1"}
		before := mm.usage
		mm.handleKey(keyRune(probe.key))
		if probe.changed(&before, &mm.usage) {
			handled = append(handled, string(probe.key))
		} else {
			t.Errorf("`%c` changed nothing on the usage pane — the footer advertises it",
				probe.key)
		}
	}

	footer := m.helpView()
	for _, k := range handled {
		if !strings.Contains(footer, "["+k+"]") {
			t.Errorf("footer omits handled key %q:\n  %s", k, footer)
		}
	}
	// And the retired ones must be gone from both surfaces.
	for _, k := range []string{"[t]", "[g]", "[r]"} {
		if strings.Contains(footer, k) {
			t.Errorf("footer still advertises retired key %s:\n  %s", k, footer)
		}
	}
	for _, kb := range paneKeys[paneUsage].bindings {
		if kb.keys == "t" || kb.keys == "g" || kb.keys == "r" {
			t.Errorf("help overlay still lists retired key %q", kb.keys)
		}
	}
}

// `g` is globally "go to top". The usage pane must not shadow it.
func TestUsagePane_DoesNotShadowGlobalG(t *testing.T) {
	if strings.Contains((&model{pane: paneUsage}).helpView(), "[g]") {
		t.Error("usage footer claims [g], which is the global go-to-top motion")
	}
}

// The overlay must stay inside the terminal, and must never push the
// composite wider than the pane underneath already was. The picker's own
// tables use fixed column widths and can exceed a narrow terminal on
// their own, so the bound is max(terminal, base) — the overlay is
// responsible only for not making things worse.
func TestHelpOverlayAddsNoOverflow(t *testing.T) {
	for _, w := range []int{60, 80, 100, 140} {
		m := newPickerModel(context.Background(), &fakeLister{namespaces: fixtureNamespaces}, nil)
		u, _ := m.Update(m.Init()())
		mm := u.(*model)
		u, _ = mm.Update(tea.WindowSizeMsg{Width: w, Height: 34})
		mm = u.(*model)

		baseMax := widestLine(mm.paneView())
		u, _ = mm.Update(keyRune('?'))
		mm = u.(*model)
		overlayMax := widestLine(mm.View())

		budget := w
		if baseMax > budget {
			budget = baseMax
		}
		if overlayMax > budget {
			t.Errorf("width %d: overlay rendered %d columns, over the %d budget (base was %d)",
				w, overlayMax, budget, baseMax)
		}
	}
}

func widestLine(s string) int {
	max := 0
	for _, ln := range strings.Split(s, "\n") {
		if w := lipgloss.Width(ln); w > max {
			max = w
		}
	}
	return max
}

// The edit overlay is itself modal and owns y/N/r/Esc. `?` must not
// layer help on top of it and swallow the apply confirmation.
func TestHelpOverlaySuppressedDuringEdit(t *testing.T) {
	mm := sizedPickerModel(t)
	mm.pane = panePipeline
	mm.editState = editState{phase: editPhaseDiff}

	updated, _ := mm.Update(keyRune('?'))
	mm = updated.(*model)
	if mm.helpVisible {
		t.Fatal("`?` should not open the help overlay during an edit")
	}
	// The edit overlay's own keys must still work.
	updated, _ = mm.Update(keyRune('n'))
	mm = updated.(*model)
	if mm.editState.phase != editPhaseDone {
		t.Fatalf("`n` should still abort the edit, phase = %v", mm.editState.phase)
	}
	// Once the edit is done, `?` works again.
	updated, _ = mm.Update(keyRune('?'))
	mm = updated.(*model)
	if !mm.helpVisible {
		t.Fatal("`?` should work again after the edit finishes")
	}
}

// --- scrolling -----------------------------------------------------------

// helpModelAt returns a model with the help overlay open at the given
// terminal size.
func helpModelAt(t *testing.T, pane paneID, w, h int) *model {
	t.Helper()
	m := newPickerModel(context.Background(), &fakeLister{namespaces: fixtureNamespaces}, nil)
	u, _ := m.Update(m.Init()())
	mm := u.(*model)
	mm.pane = pane
	u, _ = mm.Update(tea.WindowSizeMsg{Width: w, Height: h})
	mm = u.(*model)
	u, _ = mm.Update(keyRune('?'))
	return u.(*model)
}

// On a terminal too short for the whole reference, the body must be
// scrollable rather than silently truncated.
func TestHelpOverlayScrolls(t *testing.T) {
	mm := helpModelAt(t, paneSessions, 100, 14)
	if mm.helpVp.TotalLineCount() <= mm.helpVp.VisibleLineCount() {
		t.Fatalf("setup: expected content taller than the viewport (total %d, visible %d)",
			mm.helpVp.TotalLineCount(), mm.helpVp.VisibleLineCount())
	}
	if !mm.helpVp.AtTop() {
		t.Fatal("overlay should open scrolled to the top")
	}

	// The last group must be off-screen initially, and reachable by
	// scrolling — that is the whole point of this change.
	if strings.Contains(mm.View(), "PLUGIN CATALOG") {
		t.Fatal("setup: expected the tail of the reference to be off-screen")
	}
	u, _ := mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
	mm = u.(*model)
	if !mm.helpVp.AtBottom() {
		t.Fatal("`G` should jump to the bottom of the help body")
	}
	if !strings.Contains(mm.View(), "PLUGIN CATALOG") {
		t.Fatalf("after scrolling to the bottom, the tail should be visible:\n%s", mm.View())
	}

	// And back up.
	u, _ = mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	mm = u.(*model)
	if !mm.helpVp.AtTop() {
		t.Fatal("`g` should jump back to the top")
	}
}

// Arrow / vim / page keys must all move the help body.
func TestHelpOverlayScrollKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyMsg
	}{
		{"down arrow", tea.KeyMsg{Type: tea.KeyDown}},
		{"j", keyRune('j')},
		{"pgdown", tea.KeyMsg{Type: tea.KeyPgDown}},
		{"f", keyRune('f')},
		{"d", keyRune('d')},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mm := helpModelAt(t, paneSessions, 100, 14)
			before := mm.helpVp.YOffset
			u, _ := mm.Update(tc.key)
			mm = u.(*model)
			if mm.helpVp.YOffset <= before {
				t.Fatalf("%s should scroll down: offset %d → %d", tc.name, before, mm.helpVp.YOffset)
			}
			// And back up. THIS USED TO ASSERT NOTHING: the branch it had was an
			// `if … { return }` that passed on every path, so up-scrolling was
			// untested while looking covered.
			down := mm.helpVp.YOffset
			u, _ = mm.Update(tea.KeyMsg{Type: tea.KeyUp})
			mm = u.(*model)
			if mm.helpVp.YOffset >= down && !mm.helpVp.AtTop() {
				t.Errorf("up arrow should scroll back: offset %d → %d (AtTop=%v)",
					down, mm.helpVp.YOffset, mm.helpVp.AtTop())
			}
		})
	}
}

// helpNoScrollHeight is a terminal tall enough to show the whole reference at
// helpWideTerminal columns, so the no-affordance case is testable.
//
// IT IS BIG, AND THAT IS THE POINT. The body is 104 lines once every pane carries
// its purpose and its descriptions, and 107 rows is the exact floor. The previous
// version of this test asked for 60 and t.Skip()ed when the content did not fit —
// which, the moment the body grew, silently took the three short-terminal
// assertions below with it and reported PASS. A number that has to track the body's
// height is asserted, never skipped.
//
// It has earned that three times now. Adding the MOVING AROUND THIS HELP group took the
// body from 86 lines to 91, adding the AGENTS pane took it from 91 to 99, the AGENTS
// pane's S took it from 101 to 102, and search's n / N on SESSIONS and EVENTS took it to
// 104 — each time this failed naming the value to use rather than going quiet again.
//
// A PANE COSTS ITS TITLE, ITS PURPOSE, ITS BINDINGS AND ITS NOTES, so this constant
// moves whenever a pane is added or its prose changes — and it is only ever right as
// a MEASURED number. Do not raise it speculatively: the failure message reports the
// body's real height, and the floor is that plus the frame's three rows. It briefly
// read 103 here, for an AGENTS entry that advertised two bindings for an agent scope
// /v1/usage turns out to be unable to express; dropping them took the body back to
// 99, and a constant left at 103 would have gone on passing while describing a body
// that no longer existed. Too small fails loudly; too large fails silently.
const (
	helpWideTerminal   = 100
	helpNoScrollHeight = 116
)

// With everything visible there must be no scroll affordance — it would be noise
// pointing at content that is already on screen.
func TestHelpOverlayScrollHint_AbsentWhenEverythingFits(t *testing.T) {
	tall := helpModelAt(t, paneSessions, helpWideTerminal, helpNoScrollHeight)
	if got, vis := tall.helpVp.TotalLineCount(), tall.helpVp.VisibleLineCount(); got > vis {
		t.Fatalf("%d×%d shows %d of %d body lines — raise helpNoScrollHeight to at "+
			"least %d rather than skipping this case",
			helpWideTerminal, helpNoScrollHeight, vis, got, got+helpOverlayFrameH)
	}
	// Match the affordance specifically, not the word "scroll" — the
	// global key list legitimately contains "scroll this help".
	if strings.Contains(tall.View(), "[↑↓] scroll") {
		t.Errorf("no scroll affordance expected when everything fits:\n%s", tall.View())
	}
}

// When it does overflow the affordance appears and reports position, so the reader
// knows there is more below and how much of it they have seen.
func TestHelpOverlayScrollHint_ReportsPosition(t *testing.T) {
	short := helpModelAt(t, paneSessions, helpWideTerminal, 14)
	v := short.View()
	if !strings.Contains(v, "[↑↓] scroll") {
		t.Errorf("scroll affordance expected when content overflows:\n%s", v)
	}
	if !strings.Contains(v, "0%") {
		t.Errorf("scroll hint should report position at top:\n%s", v)
	}
	u, _ := short.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
	short = u.(*model)
	if !strings.Contains(short.View(), "100%") {
		t.Errorf("scroll hint should report 100%% at the bottom:\n%s", short.View())
	}
}

// A resize while the overlay is open must re-range the viewport (so a
// grown terminal reveals more) without losing the reader's place.
func TestHelpOverlayResizePreservesScroll(t *testing.T) {
	mm := helpModelAt(t, paneSessions, 100, 14)
	u, _ := mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	mm = u.(*model)
	u, _ = mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	mm = u.(*model)
	before := mm.helpVp.YOffset
	if before == 0 {
		t.Fatal("setup: expected a non-zero scroll offset")
	}

	// Grow the terminal: the visible window must grow with it.
	visBefore := mm.helpVp.VisibleLineCount()
	u, _ = mm.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	mm = u.(*model)
	if mm.helpVp.VisibleLineCount() <= visBefore {
		t.Errorf("resize should enlarge the visible window: %d → %d",
			visBefore, mm.helpVp.VisibleLineCount())
	}
	// Scroll position is preserved (clamping on a taller window may reduce
	// it, but it must not reset to the top unless clamping demands it).
	if mm.helpVp.YOffset == 0 && !mm.helpVp.AtBottom() {
		t.Error("resize should not reset scroll position to the top")
	}
}

// Reopening the overlay starts at the top rather than resuming a stale
// scroll position from a previous viewing.
func TestHelpOverlayReopenResetsScroll(t *testing.T) {
	mm := helpModelAt(t, paneSessions, 100, 14)
	u, _ := mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
	mm = u.(*model)
	if mm.helpVp.AtTop() {
		t.Fatal("setup: expected to be scrolled away from the top")
	}
	// Close, reopen.
	u, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEsc})
	mm = u.(*model)
	u, _ = mm.Update(keyRune('?'))
	mm = u.(*model)
	if !mm.helpVp.AtTop() {
		t.Fatal("reopening the overlay should start at the top")
	}
}

// Scrolling the help overlay must not disturb the detail pane's own
// viewport — the overlay can open over it.
func TestHelpOverlayScrollDoesNotDisturbDetailPane(t *testing.T) {
	mm := helpModelAt(t, paneDetail, 100, 14)
	mm.detailVp.SetContent(strings.Repeat("detail line\n", 100))
	mm.detailVp.Height = 10
	mm.detailVp.SetYOffset(7)

	u, _ := mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
	mm = u.(*model)
	if mm.detailVp.YOffset != 7 {
		t.Fatalf("help scrolling clobbered the detail viewport: offset %d, want 7",
			mm.detailVp.YOffset)
	}
	if mm.helpVp.AtTop() {
		t.Fatal("the help viewport should have scrolled instead")
	}
}

// `?` is a legitimate search character — session IDs and hosts can
// contain one — so the search prompt must receive it rather than having it
// stolen to open the help overlay.
func TestHelpOverlayDoesNotStealSearchInput(t *testing.T) {
	mm := helpModelAt(t, paneSessions, 100, 40)
	// Close the overlay opened by the helper; we want the search path.
	u, _ := mm.Update(tea.KeyMsg{Type: tea.KeyEsc})
	mm = u.(*model)

	// Open the search prompt, then type a value containing `?`.
	u, _ = mm.Update(keyRune('/'))
	mm = u.(*model)
	if !mm.searching {
		t.Fatal("setup: `/` should open the search prompt")
	}
	for _, r := range "ab?c" {
		u, _ = mm.Update(keyRune(r))
		mm = u.(*model)
	}
	if mm.helpVisible {
		t.Fatal("`?` while searching should not open the help overlay")
	}
	if got := mm.searchInput.Value(); got != "ab?c" {
		t.Fatalf("search prompt should have received the `?`, got %q", got)
	}

	// Committing the search releases the keyboard, so `?` works again.
	u, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mm = u.(*model)
	if mm.searching {
		t.Fatal("setup: Enter should commit the search")
	}
	if got := mm.search[paneSessions]; got != "ab?c" {
		t.Fatalf("committed search should be %q, got %q", "ab?c", got)
	}
	u, _ = mm.Update(keyRune('?'))
	mm = u.(*model)
	if !mm.helpVisible {
		t.Fatal("`?` should open the overlay once the search prompt is closed")
	}
}

// The AGENTS note names every surface the scope reaches.
func TestPaneKeys_TheAgentsNoteSaysWhatTheScopeReaches(t *testing.T) {
	g, ok := paneKeys[paneAgents]
	if !ok {
		t.Fatal("paneKeys has no entry for paneAgents")
	}
	joined := strings.Join(g.notes, " ")
	if joined == "" {
		t.Fatal("the AGENTS group carries no notes; the disclosure has nowhere to live")
	}
	for _, surface := range []string{"sessions", "usage pane", "spend band"} {
		if !strings.Contains(joined, surface) || strings.Contains(joined, "not the "+surface) {
			t.Errorf("the AGENTS note does not say the scope reaches the %s:\n  %s", surface, joined)
		}
	}
}
