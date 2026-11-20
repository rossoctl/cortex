package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

const barEssential = "[?] keys  [q] quit"

// barLine is the footer of m as Ascii text, which keeps the keys' brackets. Under TrueColor
// and under Ascii it must be one line exactly m.width columns wide.
func barLine(t *testing.T, m *model, label string) string {
	t.Helper()
	orig := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(orig)
	var text string
	for _, p := range []termenv.Profile{termenv.TrueColor, termenv.Ascii} {
		lipgloss.SetColorProfile(p)
		f := m.footerView()
		if strings.Contains(f, "\n") {
			t.Fatalf("%s: footer is %d lines, want 1:\n%s", label, lipgloss.Height(f), f)
		}
		if w := lipgloss.Width(f); w != m.width {
			t.Fatalf("%s profile %v: bar is %d columns, want %d:\n%q", label, p, w, m.width, ansi.Strip(f))
		}
		text = f
	}
	return text
}

func TestBottomBar_StatusOnTheRight(t *testing.T) {
	m := &model{width: 140, pane: paneSessions, connState: connStateInfo{phase: connOpen}}
	line := barLine(t, m, "sessions 140")
	if first := strings.Split(m.helpView(), "  ")[0]; !strings.HasPrefix(line, first) {
		t.Errorf("bar %q does not start with the first hint %q", line, first)
	}
	if !strings.HasSuffix(line, "● connected  0.0 events/sec") {
		t.Errorf("bar %q does not end with the status", line)
	}
	if strings.Contains(line, feedbackURL) {
		t.Error("bar still has the feedback URL")
	}
}

// Review the priority: [?] keys [q] quit first, then the connection state, then the notes,
// then the other hints, which drop from the left.
func TestBottomBar_Priority(t *testing.T) {
	for p := paneID(0); p <= lastPaneID; p++ {
		for _, w := range []int{80, 100, 140} {
			m := &model{
				pane: p, width: w, eventColumns: defaultColumnSelection(),
				connState: connStateInfo{phase: connOpen}, paused: true,
				search:  map[paneID]string{p: "github-tool"},
				sortCol: colCost, sortDesc: true,
			}
			if m.surface(p) == nil {
				continue
			}
			line := barLine(t, m, paneName(p))
			q := strings.Index(line, barEssential)
			if q < 0 {
				t.Fatalf("pane %v at %d: bar %q lost %q", p, w, line, barEssential)
			}
			statusRoom := w - lipgloss.Width("… "+barEssential) - 2
			c := strings.Index(line, "● connected")
			if statusRoom >= lipgloss.Width("● connected") && c < 0 {
				t.Errorf("pane %v at %d: bar %q lost the connection state", p, w, line)
			}
			if c >= 0 && c < q {
				t.Errorf("pane %v at %d: the status is left of the keys: %q", p, w, line)
			}
			if !strings.Contains(line, m.helpView()) && !strings.HasPrefix(line, "… ") {
				t.Errorf("pane %v at %d: hints were cut but the bar has no … mark: %q", p, w, line)
			}
		}
	}
}

func TestBottomBar_LongStatusIsCutNotQuit(t *testing.T) {
	m := &model{
		pane: paneEvents, width: 80, eventColumns: defaultColumnSelection(),
		connState: connStateInfo{phase: connOpen},
		search:    map[paneID]string{paneEvents: strings.Repeat("x", 300)},
	}
	line := barLine(t, m, "long note")
	for _, want := range []string{barEssential, "● connected"} {
		if !strings.Contains(line, want) {
			t.Errorf("bar %q lost %q", line, want)
		}
	}
}

// Review Focus 3: a flash longer than the bar with a reconnecting state.
func TestBottomBar_StickyFlash(t *testing.T) {
	m := &model{
		pane: paneEvents, width: 80, eventColumns: defaultColumnSelection(),
		connState: connStateInfo{phase: connReconnecting, attempt: 2, nextRetry: time.Now().Add(5 * time.Second)},
		paused:    true,
		search:    map[paneID]string{paneEvents: "github-tool"},
		flash:     "yanked → " + strings.Repeat("d/", 95) + "TAIL.json", flashSticky: true,
	}
	line := barLine(t, m, "sticky flash")
	if !strings.HasPrefix(line, barEssential) {
		t.Errorf("bar %q: want only %q on the left", line, barEssential)
	}
	for _, want := range []string{"◐ reconnecting", "TAIL.json"} {
		if !strings.Contains(line, want) {
			t.Errorf("bar %q lost %q", line, want)
		}
	}
	for _, gone := range []string{"events/sec", "[paused]", "[/github-tool", "yanked"} {
		if strings.Contains(line, gone) {
			t.Errorf("bar %q still has %q", line, gone)
		}
	}
}

func TestBottomBar_NonStickyFlash(t *testing.T) {
	m := &model{
		pane: paneSessions, width: 140, connState: connStateInfo{phase: connOpen},
		paused: true, flash: "copied", flashUntil: time.Now().Add(time.Hour),
	}
	line := barLine(t, m, "non-sticky flash")
	f, n := strings.Index(line, "copied"), strings.Index(line, "[paused]")
	if f < 0 || n < 0 || f < n {
		t.Errorf("bar %q: want the flash after the notes", line)
	}
}

func TestBottomBar_Pickers(t *testing.T) {
	lister := &fakeLister{namespaces: fixtureNamespaces}
	pm := newPickerModel(context.Background(), lister, nil)
	updated, _ := pm.Update(pm.Init()())
	m := updated.(*model)
	m.width, m.height = 140, 30
	m.layout()
	for _, p := range []paneID{paneNamespaces, panePods} {
		m.pane = p
		m.pickerErr = "boom"
		m.search = map[paneID]string{p: "zz"}
		st := m.searchStatus(p)
		if st == "" {
			t.Fatalf("pane %v: no search status to check", p)
		}
		var line string
		for _, prof := range []termenv.Profile{termenv.TrueColor, termenv.Ascii} {
			withProfile(t, prof)
			lines := strings.Split(m.View(), "\n")
			line = lines[len(lines)-1]
			if w := lipgloss.Width(line); w != 140 {
				t.Fatalf("pane %v profile %v: bar is %d columns, want 140", p, prof, w)
			}
		}
		q := strings.Index(line, barEssential)
		if q < 0 {
			t.Fatalf("pane %v: bar %q lost the keys", p, line)
		}
		for _, want := range []string{"error: boom", st} {
			if i := strings.Index(line, want); i < q {
				t.Errorf("pane %v: bar %q: want %q right of the keys", p, line, want)
			}
		}
		for _, gone := range []string{"connected", "connecting", "events/sec"} {
			if strings.Contains(line, gone) {
				t.Errorf("pane %v: bar %q has a connection state", p, line)
			}
		}
	}
}

func TestHelp_HasFeedbackURL(t *testing.T) {
	if !strings.Contains(ansi.Strip(helpBodyLines(paneSessions, 120)), feedbackURL) {
		t.Error("the ? help does not name the feedback URL")
	}
	for _, p := range []paneID{paneSessions, paneEvents, panePipeline, paneDetail, paneUsage, paneAgents, paneTimeline} {
		m := fitModel(t, p, 200, 50, cursorRowsFixture(10))
		if strings.Contains(ansi.Strip(m.View()), feedbackURL) {
			t.Errorf("pane %v: the view still shows the feedback URL", p)
		}
	}
}
