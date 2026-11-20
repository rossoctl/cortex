package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/rossoctl/cortex/cmd/agentop/cluster"
)

// The message and the key line sit in the middle of the body, and the key line is as wide with
// a chip as with brackets.
func TestEmptyState_Centered(t *testing.T) {
	msg, before, key, after := "No session belongs to project=x.", "Press ", "esc", " to go back to the agents."
	render := func(p termenv.Profile) []string {
		restore := lipgloss.ColorProfile()
		defer lipgloss.SetColorProfile(restore)
		lipgloss.SetColorProfile(p)
		return strings.Split(emptyState(msg, before, key, after, 60, 9), "\n")
	}
	ascii, color := render(termenv.Ascii), render(termenv.TrueColor)
	for name, lines := range map[string][]string{"ascii": ascii, "truecolor": color} {
		if len(lines) > 9 {
			t.Fatalf("%s: %d lines, want 9 or fewer", name, len(lines))
		}
		top := (9 - 2) / 2
		if len(lines) < top+2 {
			t.Fatalf("%s: %d lines, want the block on line %d", name, len(lines), top)
		}
		for i := 0; i < top; i++ {
			if strings.TrimSpace(ansi.Strip(lines[i])) != "" {
				t.Errorf("%s: line %d = %q, want blank above the block", name, i, lines[i])
			}
		}
		if got := strings.TrimSpace(ansi.Strip(lines[top])); got != msg {
			t.Errorf("%s: message line = %q, want %q", name, got, msg)
		}
		for _, l := range lines[top : top+2] {
			plain := ansi.Strip(l)
			trimmed := strings.TrimLeft(plain, " ")
			if pad, want := len(plain)-len(trimmed), (60-lipgloss.Width(trimmed))/2; pad != want {
				t.Errorf("%s: %q has left pad %d, want %d", name, plain, pad, want)
			}
		}
	}
	if got := strings.TrimSpace(ansi.Strip(color[(9-2)/2+1])); got != "Press  esc  to go back to the agents." {
		t.Errorf("truecolor key line = %q", got)
	}
	if got := strings.TrimSpace(ansi.Strip(ascii[(9-2)/2+1])); got != "Press [esc] to go back to the agents." {
		t.Errorf("ascii key line = %q", got)
	}
	for i := range ascii {
		if a, c := lipgloss.Width(ascii[i]), lipgloss.Width(color[i]); a != c {
			t.Errorf("line %d: %d columns under Ascii, %d under TrueColor", i, a, c)
		}
	}
}

func TestEmptyState_TooNarrowIsCut(t *testing.T) {
	for _, l := range strings.Split(emptyState("No agent traffic in this window.", "Press ", "w", " to pick another window.", 10, 5), "\n") {
		if w := lipgloss.Width(l); w > 10 {
			t.Errorf("%q is %d columns, want 10 or fewer", l, w)
		}
	}
}

// Every empty state of the plan's table, through View() at 100x30 under Ascii: the message and
// the key line are in the box, centered, and the view is as tall as the terminal.
func TestEmptyStates_EveryPane(t *testing.T) {
	const w, h = 100, 30
	timelineState := func(set func(m *model)) func(t *testing.T) *model {
		return func(t *testing.T) *model {
			m := timelineModel(t, w, h)
			set(m)
			return m
		}
	}
	for _, tc := range []struct {
		name  string
		build func(t *testing.T) *model
		msg   []string
		keys  string
	}{
		{"sessions scoped, no match", func(t *testing.T) *model {
			m := fitModel(t, paneSessions, w, h, nil)
			m.sessions = sessionsFixture()
			m.agentScope = "nobody"
			return m
		}, []string{"No session belongs to nobody."}, "Press [A] to pick another agent."},
		{"sessions scoped, via agents", func(t *testing.T) *model {
			m := fitModel(t, paneSessions, w, h, nil)
			m.sessions = sessionsFixture()
			m.agentScope = "nobody"
			m.sessionsViaAgents = true
			return m
		}, []string{"No session belongs to nobody."}, "Press [esc] to go back to the agents."},
		{"sessions scoped, via pods", func(t *testing.T) *model {
			m := fitModel(t, paneSessions, w, h, nil)
			m.sessions = sessionsFixture()
			m.agentScope = "nobody"
			m.parentCtx = context.Background()
			return m
		}, []string{"No session belongs to nobody."}, "Press [esc] to go back to the pods."},
		{"pipeline loading", func(t *testing.T) *model {
			m := fitModel(t, panePipeline, w, h, nil)
			m.pipeline = nil
			return m
		}, []string{"Loading the pipeline…"}, "Press [esc] to go back."},
		{"agents error", func(t *testing.T) *model {
			m := fitModel(t, paneAgents, w, h, nil)
			m.agents, m.agentsErr = nil, errors.New("connection refused")
			return m
		}, []string{"Agent breakdown unavailable: connection refused."}, "Press [w] to try again with the next window."},
		{"agents none", func(t *testing.T) *model {
			m := fitModel(t, paneAgents, w, h, nil)
			m.agents = nil
			return m
		}, []string{"No agent traffic in this window."}, "Press [w] to pick another window."},
		{"timeline unsupported", timelineState(func(m *model) { m.timeline.tl, m.timeline.err = nil, errTimelineUnsupported }),
			[]string{"This proxy does not serve the timeline. Upgrade Cortex to see it."}, "Press [esc] to go back."},
		{"timeline retrying", timelineState(func(m *model) {
			m.timeline.tl, m.timeline.err = nil, errors.New("deadline")
			m.timeline.retryAt = m.clock().Add(3 * time.Second)
		}), []string{"Loading… The last try failed. Retrying in 3s: deadline"}, "Press [esc] to go back."},
		{"timeline session gone", timelineState(func(m *model) { m.timeline.tl, m.sessions = nil, nil }),
			[]string{"The proxy no longer has this session, so it has no timeline."}, "Press [esc] to go back."},
		{"timeline loading", timelineState(func(m *model) { m.timeline.tl = nil }),
			[]string{"Loading…"}, "Press [esc] to go back."},
		{"timeline too short", func(t *testing.T) *model {
			return timelineModel(t, w, 9)
		}, []string{"Make the terminal taller to see the timeline."}, "Press [esc] to go back."},
		{"namespaces none", func(t *testing.T) *model {
			m := fitModel(t, paneNamespaces, w, h, nil)
			m.namespaces = []cluster.AgentNamespace{}
			return m
		}, []string{"No Cortex agents found in this cluster.", "To use another proxy, run agentop --endpoint http://...."},
			"Press [L] to connect to " + (&model{}).localEndpointOr() + " (an existing port-forward)."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.build(t)
			m.layout()
			m.rebuildSessionsTable()
			view := m.View()
			lines := strings.Split(view, "\n")
			if got := len(lines); got != m.height {
				t.Errorf("view is %d rows, want %d", got, m.height)
			}
			want := append(append([]string{}, tc.msg...), tc.keys)
			at := -1
			for i, l := range lines {
				if strings.Contains(l, want[0]) {
					at = i
					break
				}
			}
			if at < 0 {
				t.Fatalf("view has no %q:\n%s", want[0], view)
			}
			// The box body runs from the row after the top edge to the row before the bottom edge.
			top, bottom := -1, -1
			for i, l := range lines {
				if top < 0 && strings.HasPrefix(l, "╭") {
					top = i
				}
				if strings.HasPrefix(l, "╰") {
					bottom = i
				}
			}
			if top < 0 || bottom <= top {
				t.Fatalf("no box in the view:\n%s", view)
			}
			// Within a row: a picker's box also holds the band row layout() reserves by height.
			if exp := top + 1 + (bottom-top-1-len(want))/2; at < exp-1 || at > exp+1 {
				t.Errorf("block starts on row %d, want row %d (body rows %d-%d)", at, exp, top+1, bottom-1)
			}
			for i, s := range want {
				l := lines[at+i]
				if !strings.HasPrefix(l, "│") || !strings.HasSuffix(l, "│") {
					t.Errorf("row %d is not in the box: %q", at+i, l)
				}
				inner := strings.TrimSuffix(strings.TrimPrefix(l, "│"), "│")
				trimmed := strings.TrimLeft(inner, " ")
				if strings.TrimRight(trimmed, " ") != s {
					t.Errorf("row %d = %q, want %q", at+i, strings.TrimSpace(inner), s)
				}
				if pad, wantPad := len(inner)-len(trimmed), (m.innerWidth()-lipgloss.Width(s))/2; pad != wantPad {
					t.Errorf("row %d has left pad %d, want %d", at+i, pad, wantPad)
				}
			}
		})
	}
}

// A search set on the chart finds nothing once the Timeline shows an empty state: the matches
// of the chart that is gone do not stay.
func TestTimelineEmptyState_SearchFindsNothing(t *testing.T) {
	m := timelineModel(t, 100, 30)
	m.search = map[paneID]string{paneTimeline: "a"}
	m.timelineBody()
	if len((timelineSurface{m}).matchLines()) == 0 {
		t.Fatal("the chart has no match for \"a\"; the test needs one")
	}
	m.timeline.tl = nil
	m.timelineBody()
	if got := (timelineSurface{m}).matchLines(); len(got) != 0 {
		t.Errorf("empty state: %d matches, want 0", len(got))
	}
}
