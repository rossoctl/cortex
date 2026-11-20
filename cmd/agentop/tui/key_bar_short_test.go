package tui

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/rossoctl/cortex/core/session/timeline"
)

// The Sessions bar shows only the keys that act on the pane (#2019 change 9).
func TestHelpView_SessionsShort(t *testing.T) {
	const want = "[↵] drill  [u] usage  [$] spend  [/] search  [?] keys  [q] quit"
	m := &model{pane: paneSessions}
	if got := m.helpView(); got != want {
		t.Fatalf("sessions hints\n got %q\nwant %q", got, want)
	}

	withProfile(t, termenv.Ascii)
	if got := renderKeyBar(fitHintLine(m.helpView(), 200), 200); got != want {
		t.Errorf("Ascii bar %q, want %q", got, want)
	}
	withProfile(t, termenv.TrueColor)
	bar := ansi.Strip(renderKeyBar(fitHintLine(m.helpView(), 200), 200))
	// Chips pad each key with one space, so compare the words.
	if got, w := strings.Join(strings.Fields(bar), " "), "↵ drill u usage $ spend / search ? keys q quit"; got != w {
		t.Errorf("TrueColor bar words %q, want %q", got, w)
	}
	if strings.Contains(bar, "[") {
		t.Errorf("TrueColor bar keeps brackets: %q", bar)
	}
}

func TestHelpView_ShortBarPerPane(t *testing.T) {
	agentsVia := &model{pane: paneSessions, sessionsViaAgents: true}
	pods := &model{pane: paneSessions, parentCtx: context.Background()}
	ns := &model{pane: paneNamespaces}
	for _, tc := range []struct {
		name string
		m    *model
		want string
	}{
		{"sessions via agents", agentsVia, "[↵] drill  [u] usage  [$] spend  [/] search  [esc] agents  [?] keys  [q] quit"},
		{"sessions via pods", pods, "[↵] drill  [u] usage  [$] spend  [/] search  [esc] pods  [?] keys  [q] quit"},
		{"events", &model{pane: paneEvents, eventColumns: defaultColumnSelection()},
			"[b/f] page  [↵] detail  [c] columns  [u] usage  [/] search  [esc] back  [?] keys  [q] quit"},
		{"detail", &model{pane: paneDetail},
			"[↵] fold  [tab] response  [u] usage  [/] search  [esc] back  [?] keys  [q] quit"},
		{"pipeline", &model{pane: panePipeline},
			"[↵] plugin detail  [e] edit  [esc] back  [/] search  [?] keys  [q] quit"},
		{"plugin detail", &model{pane: panePluginDetail}, "[/] search  [esc] back  [?] keys  [q] quit"},
		{"namespaces", ns,
			"[↵] open  [L] " + shortHost(ns.localEndpointOr()) + "  [r] reload  [/] search  [?] keys  [q] quit"},
		{"pods", &model{pane: panePods}, "[↵] connect  [Esc] back  [r] reload  [/] search  [?] keys  [q] quit"},
	} {
		if got := tc.m.helpView(); got != tc.want {
			t.Errorf("%s\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}

	ag := &model{pane: paneAgents}
	if got, want := ag.helpView(), "[↵] scope to this agent  [esc] back  [w] "+ag.agentsWindowLabel()+
		"  [$] spend  [/] search  [?] keys  [q] quit"; got != want {
		t.Errorf("agents\n got %q\nwant %q", got, want)
	}
}

// The Timeline bar never offers [↑↓], even when rows are hidden: the help lists it.
func TestHelpView_TimelineShort(t *testing.T) {
	m := timelineModel(t, 140, 46)
	m.timeline.windowIdx = 3
	many := sampleTimeline()
	for i := 0; i < 20; i++ {
		id := string(rune('b' + i))
		many.Agents = append(many.Agents, timeline.Agent{ID: id, Parent: timeline.MainAgent, Label: "subagent " + id})
	}
	m.timeline.tl, m.timeline.layoutKey = many, tlLayoutKey{}
	m.layout()
	if lay, ok := m.timelineLayoutNow(); !ok || m.timelineScrollMax(lay) == 0 {
		t.Fatal("fixture hides no rows")
	}
	_, _, _, win := m.timelineRange()
	want := "[w] " + timelineWindows[win].label + "  [←→] moment  [enter] events  [/] search  [esc] back  [?] keys  [q] quit"
	if got := m.helpView(); got != want {
		t.Errorf("timeline\n got %q\nwant %q", got, want)
	}
}

// Each key a bar no longer shows is still in that pane's ? help, in the section that owns it:
// the pane's own group, the ANYWHERE group, or the pane's jumps. The match is exact, so "↑↓"
// is not found as the head of "↑↓ / jk", and a key that only EVERY PANE or another group lists
// does not count.
func TestHelpOverlay_ListsRemovedKeys(t *testing.T) {
	inGroup := func(g keyGroup, k string) bool {
		return slices.ContainsFunc(g.bindings, func(b keyBinding) bool { return b.keys == k })
	}
	own := func(pane paneID, k string) bool { return inGroup(paneKeys[pane], k) }
	anywhere := func(_ paneID, k string) bool { return inGroup(anywhereKeys, k) }
	jump := func(pane paneID, k string) bool {
		return slices.ContainsFunc(jumpsFrom(pane), func(j jumpTarget) bool { return j.key == k })
	}
	type key struct {
		keys  string
		owner func(paneID, string) bool
	}
	removed := map[paneID][]key{
		paneNamespaces:   {{"↑↓ / jk", own}},
		panePods:         {{"↑↓ / jk", own}},
		paneSessions:     {{"↑↓ / jk", own}, {"p", anywhere}, {"P", jump}},
		paneEvents:       {{"↑↓ / jk", own}, {"p", anywhere}},
		paneDetail:       {{"↑↓", own}},
		panePipeline:     {{"↑↓ / jk", own}},
		panePluginDetail: {{"↑↓", own}},
		paneTimeline:     {{"↑ ↓ / k j", own}},
		paneAgents:       {{"↑↓ / jk", own}},
	}
	for pane, keys := range removed {
		for _, k := range keys {
			if !k.owner(pane, k.keys) {
				t.Errorf("%s help does not list %q in the section that owns it", paneName(pane), k.keys)
			}
		}
	}
}

// helpHasKey reports whether a help line starts its key column with k.
func helpHasKey(body, k string) bool {
	for _, line := range strings.Split(body, "\n") {
		for _, col := range strings.Split(line, "│") {
			if strings.HasPrefix(strings.TrimLeft(col, " "), k) {
				return true
			}
		}
	}
	return false
}
