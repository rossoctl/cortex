package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
)

// deadClient is a non-nil client pointed at a port nothing listens on. The key
// handlers under test only check that a client EXISTS before changing panes —
// the Cmd they return is never run here, so no request is ever made.
func deadClient() *apiclient.Client { return apiclient.New("http://127.0.0.1:1") }

// `P` opens the pipeline from the same panes `u` opens Usage from. Matching that
// allowlist is the point: both are top-level surfaces reached from wherever the
// operator happens to be standing in the session views, and picking a different
// set for each would make "which keys work here" unanswerable.
//
// SO THE TEST DRIVES `u` TOO, on the same panes and in the same loop. Asserting
// only `P` against a hardcoded list would leave the parity claim — in this test's
// own name — unenforced: `u`'s allowlist could move and this would stay green.
//
// Note the conditions are NOT identical, only the panes: `u` needs a selected
// session on Events/Detail (its charts are session-scoped), which is why
// selectedSess is set below. The pipeline is the same on every session and asks
// for no session at all. See the `P` handler for why that asymmetry is correct.
func TestPipelineKey_OpensFromTheSamePanesAsUsage(t *testing.T) {
	panes := []paneID{paneSessions, paneEvents, paneDetail}

	for _, from := range panes {
		newModel := func() *model {
			return &model{
				pane:               from,
				client:             deadClient(),
				selectedSess:       "s1", // `u` requires one on Events/Detail.
				previousPane:       paneNone,
				pipelineReturnPane: paneNone,
			}
		}

		m := newModel()
		m.handleKey(keyRune('P'))
		if m.pane != panePipeline {
			t.Errorf("`P` from pane %v opened pane %v, want panePipeline", from, m.pane)
		}
		if m.pipelineReturnPane != from {
			t.Errorf("`P` from pane %v recorded return pane %v, want %v",
				from, m.pipelineReturnPane, from)
		}

		// The other half of the parity claim: if `u` stops working here, the two
		// allowlists have diverged and the comment on the `P` handler is stale.
		u := newModel()
		u.handleKey(keyRune('u'))
		if u.pane != paneUsage {
			t.Errorf("`u` from pane %v opened pane %v, want paneUsage — `P` and `u` "+
				"no longer share an allowlist, so the claim on the P handler is stale",
				from, u.pane)
		}
	}

	// And the exclusions, which the handler's comment claims deliberately: the
	// panes that are themselves key-opened surfaces. `P` from any of them would
	// have to pick a pane to return to, and there is no good answer — so it does
	// nothing, and that must stay true rather than becoming an accident.
	for _, from := range []paneID{paneUsage, panePluginDetail, panePipeline} {
		m := &model{
			pane:               from,
			client:             deadClient(),
			selectedSess:       "s1",
			previousPane:       paneNone,
			pipelineReturnPane: paneNone,
		}
		m.handleKey(keyRune('P'))

		if m.pane != from {
			t.Errorf("`P` on pane %v changed the pane to %v; it is outside the "+
				"allowlist and must be a no-op", from, m.pane)
		}
		if m.pipelineReturnPane != paneNone {
			t.Errorf("`P` on pane %v recorded a return pane (%v) without opening "+
				"anything", from, m.pipelineReturnPane)
		}
	}
}

// Esc leaves the pipeline the way Esc leaves Usage: back to the pane that opened
// it. It used to tear down the port-forward and the SSE stream instead, because
// panePipeline was grouped with paneSessions in the back-out switch — so a
// keystroke that only meant "show me the config" cost the operator their
// connection. parentCtx is non-nil here precisely because that is the condition
// the old grouping keyed on.
func TestPipelineKey_EscReturnsToTheCallerAndKeepsTheConnection(t *testing.T) {
	m := newPickerModel(context.Background(), &fakeLister{}, nil)
	m.client = deadClient()
	m.pane = paneEvents
	m.selectedSess = "s1"

	m.handleKey(keyRune('P'))
	if m.pane != panePipeline {
		t.Fatalf("precondition: `P` did not open the pipeline (got %v)", m.pane)
	}
	if m.parentCtx == nil {
		t.Fatal("precondition: this test needs a picker model, whose parentCtx is non-nil")
	}

	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})

	if m.pane == panePods {
		t.Fatal("esc from the pipeline fell back to the pods picker, tearing down PF + SSE")
	}
	if m.pane != paneEvents {
		t.Errorf("esc from the pipeline landed on %v, want paneEvents (the pane it was opened from)", m.pane)
	}
	if m.pipelineReturnPane != paneNone {
		t.Errorf("the return pane was not cleared on the way out: %v", m.pipelineReturnPane)
	}
}

// The return pane is the pipeline's OWN field, not model.previousPane, which the
// plugin detail also writes. Opening a plugin from the pipeline and backing out
// twice has to land where the operator started; sharing one field is the bug
// usageState.returnPane was introduced to fix, arriving through a third door.
//
// STARTS FROM EVENTS, NOT SESSIONS, and that is the whole point of the test. A
// pipeline sharing previousPane loses the caller to the detail's write and then
// falls back — and the fallback is paneSessions, so starting from Sessions the
// broken implementation reaches the right pane by luck and the test proves
// nothing. Verified by mutation: with the shared field, the Sessions version of
// this test passes and this one fails.
func TestPipelineKey_ReturnPaneSurvivesThePluginDetail(t *testing.T) {
	m := &model{
		pane:               paneEvents,
		client:             deadClient(),
		selectedSess:       "s1",
		previousPane:       paneNone,
		pipelineReturnPane: paneNone,
		pipelineTbl:        newPipelineTable(),
		pipeline:           &apiclient.PipelineView{Outbound: []apiclient.PipelinePlugin{{Name: "mcp-parser", Direction: "outbound", Position: 1}}},
	}
	m.handleKey(keyRune('P'))                   // events -> pipeline
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter}) // pipeline -> plugin detail (writes previousPane)
	if m.pane != panePluginDetail {
		t.Fatalf("precondition: Enter did not open the plugin (got %v)", m.pane)
	}

	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc}) // detail -> pipeline
	if m.pane != panePipeline {
		t.Fatalf("esc from the plugin detail landed on %v, want panePipeline", m.pane)
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc}) // pipeline -> events
	if m.pane == paneSessions {
		t.Fatal("esc from the pipeline fell back to Sessions — the detail clobbered the return pane")
	}
	if m.pane != paneEvents {
		t.Errorf("esc from the pipeline landed on %v, want paneEvents", m.pane)
	}
}

// The plugin catalog is the pipeline pane's "available" section now, so `C` opens
// nothing, and `P` fetches /v1/plugins on every open to fill that section.
func TestCatalogKey_FoldedIntoThePipeline(t *testing.T) {
	onC := &model{pane: paneSessions, client: deadClient(), previousPane: paneNone}
	onC.handleKey(keyRune('C'))
	if onC.pane != paneSessions {
		t.Errorf("`C` opened pane %v; the catalog has no pane of its own any more", onC.pane)
	}

	onP := &model{
		pane:               paneSessions,
		client:             deadClient(),
		previousPane:       paneNone,
		pipelineReturnPane: paneNone,
	}
	if cmd := onP.handleKey(keyRune('P')); cmd == nil {
		t.Error("`P` returned no command; it must fetch the catalog for the available rows")
	}
	if onP.pane != panePipeline {
		t.Errorf("`P` opened pane %v, want panePipeline", onP.pane)
	}
}

// The whole reason the pipeline took `P` rather than `p` is that lowercase `p`
// pauses a live stream, is printed in two footers, and has no good replacement
// (`space` and `d` are bubbles' page-down and half-page-down). If this test ever
// fails, the trade was made the wrong way round.
func TestPauseKeep_StaysOnLowercaseP(t *testing.T) {
	m := &model{pane: paneSessions}
	m.handleKey(keyRune('p'))
	if !m.paused {
		t.Fatal("`p` no longer pauses the stream")
	}
	m.handleKey(keyRune('p'))
	if m.paused {
		t.Error("`p` no longer resumes the stream")
	}
}

// With the "[Sessions] Pipeline" strip gone, the sessions title must not still
// advertise a tab that does nothing — and the pipeline pane, which used to be
// named only BY that strip, has to name itself.
func TestTopLevelTitles_CarryNoTabStrip(t *testing.T) {
	sessions := &model{width: 140, height: 40, endpoint: "http://ep:9094", pane: paneSessions}
	if title := firstLine(sessions.paneView()); strings.Contains(title, "Pipeline") {
		t.Errorf("the sessions title still carries the tab strip: %q", title)
	}

	pipe := &model{width: 140, height: 40, endpoint: "http://ep:9094", pane: panePipeline}
	title := firstLine(pipe.paneView())
	if !strings.Contains(strings.ToLower(title), "pipeline") {
		t.Errorf("with the strip gone, the pipeline pane must name itself in its title: %q", title)
	}
}

// A surface reached by a key is only as good as the advertisement for that key.
// fitHintLine drops hints from the FRONT, so `[P] plugins` sits near the tail.
//
// THE ASSERTION IS AT 60 COLUMNS, NOT 80, and the difference matters: measured,
// the key survives to 34 columns at the tail and to 79 third from the left, so an
// 80-column assertion passes for BOTH placements and tests nothing. 60 is inside
// the gap — narrow enough that only the tail placement holds, and a width a tmux
// split or half-screen terminal actually reaches. 80 is asserted too, as the
// floor that must never regress.
func TestSessionsFooter_AdvertisesThePipelineKeyAtAWidthThatSurvives(t *testing.T) {
	m := &model{pane: paneSessions}

	got := m.helpView()
	if !strings.Contains(got, "[P] plugins") {
		t.Errorf("the sessions footer omits [P] plugins:\n  %s", got)
	}
	for _, w := range []int{80, 60} {
		if fit := fitHintLine(got, w); !strings.Contains(fit, "[P]") {
			t.Errorf("a %d-column fit drops the pipeline key:\n  %s", w, fit)
		}
	}

	// The drilled-in spelling is a separate string with its own [esc] pods.
	m.parentCtx = context.Background()
	if got := m.helpView(); !strings.Contains(got, "[P] plugins") {
		t.Errorf("the drilled-in sessions footer omits [P] plugins:\n  %s", got)
	}
}

// `tab` is gone, so no footer may still offer it, and the pipeline's own footer
// has to say where esc goes now that it no longer dumps the operator at the
// pods picker.
func TestPipelineFooter_DropsTabAndNamesWhereEscGoes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		parentCtx context.Context
	}{
		{"bypass mode", nil},
		{"picker mode", context.Background()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &model{pane: panePipeline, pipelineReturnPane: paneSessions, parentCtx: tc.parentCtx}
			got := m.helpView()
			if strings.Contains(got, "[tab]") {
				t.Errorf("the pipeline footer still advertises the retired [tab] key:\n  %s", got)
			}
			if !strings.Contains(got, "[esc] back") {
				t.Errorf("the pipeline footer does not say where esc goes:\n  %s", got)
			}
		})
	}

	if got := (&model{pane: paneSessions}).helpView(); strings.Contains(got, "[tab]") {
		t.Errorf("the sessions footer still advertises the retired [tab] key:\n  %s", got)
	}
}

// The [?] overlay is the surface that cannot run out of room, so it is where
// both remapped keys must be correct. A stale entry here is worse than no
// entry: it sends the reader to the wrong pane.
func TestHelpOverlay_DocumentsTheRemappedKeys(t *testing.T) {
	// `P` and `C` moved out of the flat global group into jumpTargets, which is
	// the section that renders them per pane. Their MEANING is asserted against
	// the pane each one names rather than against a description string: the row's
	// label is derived from that pane, so naming the pane is naming the key.
	target := map[string]paneID{}
	for _, jt := range jumpTargets {
		target[jt.key] = jt.pane
	}
	if got := target["P"]; got != panePipeline {
		t.Errorf("jumpTargets does not point `P` at the pipeline (got pane %v)", got)
	}
	if _, ok := target["C"]; ok {
		t.Error("jumpTargets still lists `C`; the catalog is the pipeline's available section")
	}

	desc := map[string]string{}
	for _, g := range nonPaneGroups() {
		for _, kb := range g.bindings {
			desc[kb.keys] = strings.ToLower(kb.desc)
		}
	}
	if d := desc["p"]; !strings.Contains(d, "pause") {
		t.Errorf("the global groups lost `p` as pause (got %q)", d)
	}

	for _, pane := range []paneID{paneSessions, panePipeline} {
		for _, kb := range paneKeys[pane].bindings {
			if strings.Contains(kb.keys, "tab") {
				t.Errorf("paneKeys[%v] still lists the retired `tab` binding: %q", pane, kb.desc)
			}
		}
	}
}

// Two bindings claiming one key is the defect this whole change corrects: `P`
// was the catalog's and "pipeline" had no key at all. The overlay is where such
// a collision is visible, so assert it cannot come back silently.
// Now spans every non-pane group AND the jump targets, because the single
// globalKeys list this checked has been split into three surfaces — and a key
// claimed by two of THEM is the same defect one step out. The keys:"" exemption
// is gone with the prose that needed it; see
// TestHelpBindings_NeverHaveAnEmptyKeyColumn.
func TestGlobalKeys_AdvertiseNoKeyTwice(t *testing.T) {
	seen := map[string]string{}
	claim := func(key, what string) {
		if prev, dup := seen[key]; dup {
			t.Errorf("the overlay advertises %q for two different things: %q and %q",
				key, prev, what)
		}
		seen[key] = what
	}
	for _, g := range nonPaneGroups() {
		for _, kb := range g.bindings {
			claim(kb.keys, kb.desc)
		}
	}
	for _, jt := range jumpTargets {
		claim(jt.key, jt.name())
	}
}

// The two REMAPPED keys must mean the same thing wherever they are documented. A
// pane group is free to repeat a global key (the sessions group repeats `P`,
// which is useful on the pane that wants it most), but not to redefine it — a
// `{"C", "clear filter"}` added to some pane group would send a reader of that
// group to the wrong surface, and the overlay renders both groups together.
//
// SCOPED TO `P` AND `C` ON PURPOSE. A blanket globalKeys x paneKeys uniqueness
// check is not viable: measured, it flags seven overlaps today and six are the
// legitimate modal case — `↑↓ / jk` navigates a pane but scrolls the help
// overlay while that is up, so the same key genuinely means two things. Such a
// test would ship needing a six-entry exemption list, which encodes exemptions
// rather than catching bugs. These two keys have exactly one meaning each, so
// the narrow check needs no exemptions and covers the realistic regression.
func TestRemappedKeys_MeanTheSameThingInEveryGroup(t *testing.T) {
	want := map[string]string{"P": "pipeline", "C": "catalog"}

	// jumpTargets is now the single definition, so the check that used to compare
	// copies becomes a check that there is exactly one. A pane group is no longer
	// free to repeat these keys at all — see TestHelpPaneKeys_DoNotRepeatTheJumpKeys
	// — so any occurrence here is a REDEFINITION, which is what sent readers to the
	// wrong surface.
	for _, pane := range otherPaneOrder {
		for _, kb := range paneKeys[pane].bindings {
			if substr, watched := want[kb.keys]; watched {
				t.Errorf("paneKeys[%v] binds %q to %q; %q belongs to jumpTargets "+
					"(the %s) and must not be redefined per pane",
					pane, kb.keys, kb.desc, kb.keys, substr)
			}
		}
	}

	// And the definition itself, which the jump rows render from.
	for _, jt := range jumpTargets {
		if substr, watched := want[jt.key]; watched {
			if !strings.Contains(strings.ToLower(jt.name()), substr) {
				t.Errorf("jumpTargets labels %q %q, want it to name the %s",
					jt.key, jt.name(), substr)
			}
		}
	}

	// The jump rows are the only place a reader learns which pane a key opens, so
	// the label each row shows is pinned to a LITERAL.
	//
	// It used to be compared against strings.ToLower(paneName(jt.pane)) — which is
	// the body of jt.name() itself, so the assertion could not fail for any pane
	// target and passed vacuously. Literals catch what a reader would actually
	// notice: a pane retitled without its jump row being reconsidered.
	wantLabel := map[string]string{
		"u": "usage",
		"P": "pipeline",
		"C": "plugin catalog",
		"$": "spend",
		"A": "agents",
		"T": "timeline",
	}
	for _, jt := range jumpTargets {
		if want, ok := wantLabel[jt.key]; !ok {
			t.Errorf("jumpTargets has an undocumented key %q — add it here", jt.key)
		} else if jt.name() != want {
			t.Errorf("the %q jump row reads %q, want %q", jt.key, jt.name(), want)
		}
		// label is consulted only for paneNone, so a value set on a pane target is
		// dead weight the compiler cannot see. Keep it empty so it cannot disagree
		// with the name actually rendered.
		if jt.pane != paneNone && jt.label != "" {
			t.Errorf("jumpTargets sets label %q on %q, whose pane is %v — the label is "+
				"ignored for pane targets, so it can only mislead",
				jt.label, jt.key, jt.pane)
		}
	}
}
