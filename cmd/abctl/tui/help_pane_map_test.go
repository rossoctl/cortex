package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/core/session"
)

// helpWide is a wrap budget wider than any line the overlay builds, so tests
// that assert on content are not reading a truncated body.
const helpWide = 200

// sectionFrom returns body from title onwards, stopping at the blank line that ends
// the section when only is true.
//
// t.Fatalf ON A MISSING TITLE, which is the reason this is a helper: the callers
// used body[strings.Index(body, title):] after a non-fatal t.Errorf, so a body that
// lost a heading indexed at -1 and panicked — aborting the whole package instead of
// failing the one case with a readable message.
func sectionFrom(t *testing.T, body, title string, only bool) string {
	t.Helper()
	i := strings.Index(body, title)
	if i < 0 {
		t.Fatalf("body has no %q section:\n%s", title, body)
	}
	section := body[i:]
	if only {
		if end := strings.Index(section, "\n\n"); end > 0 {
			section = section[:end]
		}
	}
	return section
}

// allHelpGroups is every group the overlay can render anywhere: the non-pane
// groups unioned over every pane (they are pane-conditional, so asking one pane
// would miss any group that applies only elsewhere) plus each pane's own.
func allHelpGroups() []keyGroup {
	var groups []keyGroup
	seen := map[string]bool{}
	for p := paneNamespaces; p <= lastPaneID; p++ {
		for _, g := range helpGlobalGroups(p) {
			if !seen[g.title] {
				seen[g.title] = true
				groups = append(groups, g)
			}
		}
		groups = append(groups, paneKeys[p])
	}
	return groups
}

// nonPaneGroups is allHelpGroups without the panes — the surfaces that used to be
// the single `globalKeys` list.
func nonPaneGroups() []keyGroup {
	var groups []keyGroup
	seen := map[string]bool{}
	for p := paneNamespaces; p <= lastPaneID; p++ {
		for _, g := range helpGlobalGroups(p) {
			if !seen[g.title] {
				seen[g.title] = true
				groups = append(groups, g)
			}
		}
	}
	return groups
}

// Every pane must say what it IS, not just which keys it takes. The overlay
// used to name nine panes and describe none of them, which made "what can I
// look at" unanswerable from the one surface that exists to answer it.
func TestHelpPurpose_EveryPaneHasOne(t *testing.T) {
	for p := paneNamespaces; p <= lastPaneID; p++ {
		g, ok := paneKeys[p]
		if !ok {
			t.Errorf("pane %v has no paneKeys entry", p)
			continue
		}
		if strings.TrimSpace(g.purpose) == "" {
			t.Errorf("pane %v (%q) has no purpose line — the overlay would name it "+
				"without saying what it shows", p, g.title)
		}
		// thisPaneSuffix's whole claim is that paneName can strip it so the two forms
		// cannot disagree — and nothing enforced it: every title hardcodes the literal,
		// and paneName's TrimSuffix is a no-op on a title that omits it. A pane added
		// without the suffix would lose the active-pane marker while paneName kept
		// working, so the omission would be invisible.
		if !strings.HasSuffix(g.title, thisPaneSuffix) {
			t.Errorf("pane %v is titled %q, which does not end in %q — it would render "+
				"unmarked when the overlay is opened over it",
				p, g.title, thisPaneSuffix)
		}
	}
}

// THE GLYPH WALL IS THE BUG THIS LOCKS OUT. The old "OTHER PANES" section
// compacted each pane to its bare keys — `USAGE  m  w  b  s  esc` — so the
// overlay told a reader that usage has four keys and nothing about what any of
// them do. Every binding's description must survive into the body.
func TestHelpBody_EveryPaneSectionSpellsOutEveryDescription(t *testing.T) {
	body := helpBodyLines(paneSessions, helpWide)

	if !strings.Contains(body, "EVERY PANE") {
		t.Fatalf("body has no EVERY PANE section:\n%s", body)
	}
	for p := paneNamespaces; p <= lastPaneID; p++ {
		g := paneKeys[p]
		name := paneName(p)
		if !strings.Contains(body, name) {
			t.Errorf("pane %v (%q) is not named anywhere in the body", p, name)
		}
		if !strings.Contains(body, g.purpose) {
			t.Errorf("pane %v purpose %q missing from the body", p, g.purpose)
		}
		for _, kb := range g.bindings {
			if !strings.Contains(body, kb.desc) {
				t.Errorf("pane %v binds %q to %q, and the description is not in the "+
					"body — that is the glyph wall coming back", p, kb.keys, kb.desc)
			}
		}
	}
}

// The jump section must list exactly the keys that actually change panes from
// where the reader is standing. Driven against the real handlers rather than a
// second copy of their allowlists: `u`, `P` and `C` do NOT share one (P is
// sessions/events/detail, C is anything past the pickers), so a hardcoded list
// here would encode today's accident and go stale silently.
func TestHelpBody_JumpSectionMatchesTheKeysThatActuallyWork(t *testing.T) {
	for p := paneNamespaces; p <= lastPaneID; p++ {
		listed := map[string]bool{}
		for _, jt := range jumpsFrom(p) {
			listed[jt.key] = true
		}

		for _, jt := range jumpTargets {
			var works bool
			switch jt.pane {
			case paneNone:
				// `$` opens a drawer, not a pane, so "works" is the drawer actually
				// opening. PRESSED, NOT ASKED: this used to call spendDrawerHost(),
				// which is the very function jumpsFrom consults, so the expected value
				// and the implementation came from one source and the check compared
				// it with itself. A terminal tall enough for the drawer, so a height
				// refusal cannot read as a pane refusal.
				m := &model{pane: p, width: 120, height: 48, client: deadClient()}
				m.layout()
				m.handleKey(keyRune('$'))
				works = m.spend.expanded
			case p:
				// Never advertise a jump to the pane the reader is already on.
				works = false
			default:
				m := &model{
					pane:               p,
					client:             deadClient(),
					selectedSess:       "s1", // `u` needs one on events/detail.
					previousPane:       paneNone,
					pipelineReturnPane: paneNone,
				}
				m.handleKey(keyRune(rune(jt.key[0])))
				works = m.pane == jt.pane
			}

			if works && !listed[jt.key] {
				t.Errorf("%q works from pane %v but the jump section does not list it",
					jt.key, p)
			}
			if !works && listed[jt.key] {
				t.Errorf("the jump section offers %q from pane %v, where it does nothing",
					jt.key, p)
			}
		}
	}
}

// The pickers run before a connection exists, so none of the jump keys work
// there. Saying nothing would read as "this pane has no way out"; the overlay
// keeps the section and explains instead.
//
// THE TITLE MUST STAY AND THE KEYS MUST GO — asserted as two separate things,
// because the section went through both failure modes. Omitting the title left the
// explanation reading as a trailing row of the pane block above it; listing the
// keys advertised four that do nothing.
func TestHelpBody_PickerPanesExplainWhyThereIsNoJumpSection(t *testing.T) {
	for _, p := range []paneID{paneNamespaces, panePods} {
		body := helpBodyLines(p, helpWide)
		if !strings.Contains(body, jumpSectionTitle) {
			t.Errorf("pane %v drops the %q heading, so its explanation has no owner",
				p, jumpSectionTitle)
		}
		if !strings.Contains(body, "connected") {
			t.Errorf("pane %v neither offers the jump keys nor explains why:\n%s", p, body)
		}
		// No key rows under it: the section names the panes in prose only.
		head := sectionFrom(t, body, jumpSectionTitle, false)
		for _, jt := range jumpTargets {
			row := jt.key + "  " + jt.name()
			if strings.Contains(head, row) {
				t.Errorf("pane %v lists the jump row %q, where the key does nothing", p, row)
			}
		}
	}
}

// The spine, in order, on one line. This is the structure the old overlay never
// showed: a reader could not tell that events sits under sessions, or that esc
// walks back up rather than quitting.
func TestHelpBody_DrillPathIsInOrder(t *testing.T) {
	body := helpBodyLines(paneSessions, helpWide)
	if !strings.Contains(body, "THE DRILL PATH") {
		t.Fatalf("body has no drill path section:\n%s", body)
	}

	// The spine is the line under the section title, so read that line rather than
	// the whole body — an ordering assertion over the body would also be satisfied
	// by the EVERY PANE section further down. Found by the title and not by the
	// arrow: `↵ / → / l` is a binding on most panes and comes first.
	var line string
	lines := strings.Split(body, "\n")
	for i, ln := range lines {
		if strings.Contains(ln, drillSectionTitle) && i+1 < len(lines) {
			line = lines[i+1]
			break
		}
	}
	if line == "" {
		t.Fatalf("no drill path line under %q:\n%s", drillSectionTitle, body)
	}

	at := -1
	for _, p := range drillPath {
		name := strings.ToLower(paneName(p))
		i := strings.Index(line, name)
		if i < 0 {
			t.Fatalf("drill path does not name %q: %q", name, line)
		}
		if i <= at {
			t.Errorf("drill path lists %q out of order: %q", name, line)
		}
		at = i
	}

	// "You are here" is what makes the line a map rather than a list.
	if want := "[" + strings.ToLower(paneName(paneSessions)) + "]"; !strings.Contains(line, want) {
		t.Errorf("drill path does not mark the active pane with %q: %q", want, line)
	}
}

// EVERY pane must be located, not just the five on the spine. Asserting the marker
// on paneSessions alone left four panes — usage, pipeline, plugin detail and the
// catalog — rendering a bare list with nothing saying the reader was off the path
// or where esc would return them.
func TestHelpBody_DrillPathLocatesEveryPane(t *testing.T) {
	spine := map[paneID]bool{}
	for _, p := range drillPath {
		spine[p] = true
	}

	for p := paneNamespaces; p <= lastPaneID; p++ {
		body := helpBodyLines(p, helpWide)
		section := sectionFrom(t, body, drillSectionTitle, true)

		if spine[p] {
			if want := "[" + strings.ToLower(paneName(p)) + "]"; !strings.Contains(section, want) {
				t.Errorf("pane %v is on the spine but unmarked (want %q):\n%s", p, want, section)
			}
			continue
		}
		// Off the spine: no position to bracket, so it must be named in prose
		// along with where esc goes.
		if !strings.Contains(section, strings.ToLower(paneName(p))) {
			t.Errorf("pane %v is off the spine and the drill path never names it:\n%s",
				p, section)
		}
		if !strings.Contains(section, "esc") {
			t.Errorf("pane %v is off the spine and nothing says where esc returns it:\n%s",
				p, section)
		}
	}
}

// `P`, `C`, `u` and `$` are advertised once, in the jump section. They used to
// appear in globalKeys AND be repeated inside the pane groups, so the overlay
// carried two copies of each that could disagree.
func TestHelpPaneKeys_DoNotRepeatTheJumpKeys(t *testing.T) {
	jump := map[string]bool{}
	for _, jt := range jumpTargets {
		jump[jt.key] = true
	}
	for p := paneNamespaces; p <= lastPaneID; p++ {
		for _, kb := range paneKeys[p].bindings {
			if jump[kb.keys] {
				t.Errorf("paneKeys[%v] repeats the jump key %q (%q); the jump "+
					"section is where it belongs", p, kb.keys, kb.desc)
			}
		}
	}
}

// splitKeys breaks a key column into the individual keys it advertises: "b / f"
// into b and f, "q · ctrl+c" into q and ctrl+c. Without this a comparison of key
// COLUMNS misses every real collision, which is how "b / f  page up / down" sat in
// ANYWHERE on the one pane where `b` cycles the breakdown — the strings "b" and
// "b / f" are not equal, so nothing noticed.
func splitKeys(col string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(col, func(r rune) bool {
		return r == '/' || r == '·'
	}) {
		if k := strings.TrimSpace(part); k != "" {
			out = append(out, k)
		}
	}
	return out
}

// ANYWHERE may not claim a key the pane underneath rebinds to something else.
//
// This is the check whose absence let `b` ship wrong: anywhereKeys' own doc says it
// holds only the keys that work regardless of what is on screen, and `a`/`w` were
// moved out for exactly that reason while `b` was not.
//
// NO EXEMPTION LIST, which is the point of having moved the motion keys into
// helpNavKeys. While they sat here the check needed `?` and `↑↓ / jk` excused as
// "these describe the overlay, not the pane" — and an exemption list is where the
// next `b` would have hidden. ANYWHERE now holds only keys with one meaning, so any
// overlap at all is a defect.
func TestHelpBody_AnywhereKeysAreNotReboundByTheActivePane(t *testing.T) {
	for p := paneNamespaces; p <= lastPaneID; p++ {
		owned := map[string]string{}
		for _, kb := range paneKeys[p].bindings {
			for _, k := range splitKeys(kb.keys) {
				owned[k] = kb.desc
			}
		}
		for _, kb := range anywhereKeys.bindings {
			for _, k := range splitKeys(kb.keys) {
				if desc, clash := owned[k]; clash {
					t.Errorf("pane %v: ANYWHERE offers %q as %q while the pane binds %q to %q",
						p, k, kb.desc, k, desc)
				}
			}
		}
	}
}

// The behavioural half, for the claim helpNavKeys actually makes: its three rows
// move THIS OVERLAY on every pane, and its note says the closed-overlay meaning
// differs on usage alone.
//
// Both halves are pressed rather than asserted against the switches they describe.
// The overlay half is what the previous gating fix got backwards — it removed the
// paging row from the one pane whose 86-line body most needs it, on the strength of
// a pane behaviour that says nothing about what the key does while the overlay is up.
func TestHelpNavKeys_MoveTheOverlayOnEveryPane(t *testing.T) {
	for p := paneNamespaces; p <= lastPaneID; p++ {
		// A short terminal so the body always overflows and has somewhere to scroll.
		m := helpModelAt(t, p, 100, 14)
		if m.helpVp.TotalLineCount() <= m.helpVp.VisibleLineCount() {
			t.Fatalf("pane %v: body fits at 100x14, so scrolling is untestable", p)
		}

		u, _ := m.Update(keyRune('f'))
		m = u.(*model)
		if m.helpVp.YOffset == 0 {
			t.Errorf("pane %v: `f` did not page the overlay, but the nav group offers it", p)
		}
		u, _ = m.Update(keyRune('b'))
		m = u.(*model)
		if m.helpVp.YOffset != 0 {
			t.Errorf("pane %v: `b` did not page the overlay back to the top (offset %d)",
				p, m.helpVp.YOffset)
		}
		u, _ = m.Update(keyRune('G'))
		m = u.(*model)
		if !m.helpVp.AtBottom() {
			t.Errorf("pane %v: `G` did not jump the overlay to the bottom", p)
		}
	}
}

// And the exception the note names: with the overlay closed, usage is the one pane
// where `b` is not paging and `g` moves nothing.
func TestHelpNavKeys_NoteNamesTheOneExceptionCorrectly(t *testing.T) {
	m := &model{pane: paneUsage, selectedSess: "s1"}
	before := m.usage.group
	m.handleKey(keyRune('b'))
	if m.usage.group == before {
		t.Errorf("`b` no longer cycles the usage breakdown — the nav note says it does")
	}

	// The note also claims g/G do nothing on usage. goTop has no case for it; assert
	// via the footer contract the existing TestUsagePane_DoesNotShadowGlobalG relies
	// on, so both stay true together.
	if strings.Contains((&model{pane: paneUsage}).helpView(), "[g]") {
		t.Error("the usage footer claims [g] while the nav note says g does nothing there")
	}

	// A pane on the other side of the exception, so the note cannot be made true by
	// paging breaking everywhere.
	s := &model{pane: paneSessions, height: 40, width: 120}
	s.sessionsTbl = newSessionsTable()
	s.sessionsTbl.SetHeight(10)
	for i := 0; i < 40; i++ {
		s.sessions = append(s.sessions, session.SessionSummary{ID: fmt.Sprintf("s%02d", i)})
	}
	s.rebuildSessionsTable()
	s.handleKey(keyRune('f'))
	if s.sessionsTbl.Cursor() == 0 {
		t.Error("`f` did not page the sessions table, so the nav note's " +
			"\"moves the pane's own list\" is wrong outside usage too")
	}
}

// `a` and `w` are live only while the drawer is open, so listing them beside
// the keys that work everywhere taught a binding that mostly does nothing.
func TestHelpBody_SpendDrawerKeysAreTheirOwnSection(t *testing.T) {
	body := helpBodyLines(paneSessions, helpWide)
	if !strings.Contains(body, "INSIDE THE SPEND DRAWER") {
		t.Fatalf("drawer-only keys have no section of their own:\n%s", body)
	}
	for _, k := range []string{"a", "w"} {
		for _, kb := range anywhereKeys.bindings {
			if kb.keys == k {
				t.Errorf("anywhereKeys claims %q (%q), which only works while the "+
					"spend drawer is open", k, kb.desc)
			}
		}
	}
}

// Prose in the key column was how the old overlay smuggled a caveat into a key
// table: a binding with keys:"" and a sentence for a description. Notes now
// belong to a group, so no binding needs an empty key.
func TestHelpBindings_NeverHaveAnEmptyKeyColumn(t *testing.T) {
	for _, g := range allHelpGroups() {
		for _, kb := range g.bindings {
			if strings.TrimSpace(kb.keys) == "" {
				t.Errorf("group %q carries a binding with no key: %q", g.title, kb.desc)
			}
		}
	}
}

// Long prose must wrap to the terminal, not run off it. syncHelpViewport's own
// doc comment claimed the body "re-wraps" on resize while helpBodyLines took no
// width at all, so every purpose line was simply clipped at narrow widths.
// EVERY PANE AT EVERY INTERESTING WIDTH, because one pane at one width is the
// combination where this happened to hold: the first version checked only
// paneSessions at 56 and passed while the usage pane's enum-list bindings overran
// by up to 15 columns on anything below 61. The widths bracket the wrap floor
// (helpMinProseWidth), the point where the longest enum stops fitting, and a
// roomy terminal.
func TestHelpBody_WrapsProseToTheGivenWidth(t *testing.T) {
	for _, width := range []int{40, 41, 48, 50, 56, 61, 80, 120} {
		for p := paneNamespaces; p <= lastPaneID; p++ {
			for _, ln := range strings.Split(helpBodyLines(p, width), "\n") {
				if w := lipgloss.Width(ln); w > width {
					t.Errorf("pane %v at width %d: line is %d columns, over budget: %q",
						p, width, w, ln)
				}
			}
		}
	}
}
