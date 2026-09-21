package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// TestUsageScopeMax_CoversTheWidestHeader recomputes usageScopeMax's allowance from the sources
// renderUsage draws on, rather than from a list written out by hand.
//
// THE HAND-WRITTEN LIST IS WHY THIS TEST EXISTED AND FAILED TO WORK. Its first version
// enumerated grouping strings the renderer never emits ("no breakdown", "by model" without the
// prefix it actually adds) while omitting the longest one it does — "no breakdown for latency",
// 24 columns by itself — and hardcoded one window, so its computed widest was 51 against a real
// 65. It then asserted that 51 fitted the constant, which it did, and passed.
//
// So the inputs come from usageWindows, the metric enum and the grouping branches in
// renderUsage. A grouping added there is the one thing this still cannot see; the switch is
// three lines long and sits beside the format string it feeds.
func TestUsageScopeMax_CoversTheWidestHeader(t *testing.T) {
	// The strings renderUsage's grouping switch can produce. "no breakdown for latency" is the
	// literal it emits for the latency metric; the rest are "by " + the group name.
	groupings := []string{"ungrouped", "no breakdown for latency"}
	for _, g := range []usage.Group{usage.GroupStatus, usage.GroupModel, usage.GroupPlugin, usage.GroupHost} {
		groupings = append(groupings, "by "+string(g))
	}

	widest, worst := 0, ""
	for m := usageMetric(0); m < usageMetricCount; m++ {
		for _, w := range usageWindows {
			for _, g := range groupings {
				// The same format renderUsage writes, with an empty scope so what remains is
				// the fixed cost the allowance has to cover.
				line := fmt.Sprintf("  USAGE — %s — %s @ %s — %s — %s", "", w.window, w.resolution, m, g)
				if n := len([]rune(line)); n > widest {
					widest, worst = n, fmt.Sprintf("%s / %s / %s@%s", m, g, w.window, w.resolution)
				}
			}
		}
	}

	// The allowance must cover the widest remainder. Asserted by asking usageScopeMax what it
	// grants on a terminal wide enough that its floor cannot be what answers — the floor is
	// what made the earlier version's "case that matters" assertion unreachable at width 80.
	const wide = 1000
	if granted := usageScopeMax(wide); granted != wide-widest {
		t.Errorf("usageScopeMax reserves %d columns for the rest of the line; the widest "+
			"reachable remainder is %d (%s)", wide-granted, widest, worst)
	}
}

// TestUsageHeader_LatencyOverflowsEightyAndFitsAt88 states the header's real width behaviour,
// because nothing did: the pane-fit tests render the model's zero value — tokens metric, the
// shortest window — and never reach the combination that motivated raising the allowance.
//
// THROUGH renderUsage, not a hand-built format string. An earlier revision of this test rebuilt
// the header itself; the two agreed exactly, so there was no live defect, but a change to the
// real format string would have moved the header and left this test green against its own copy.
//
// ASSERTED AS AN OVERFLOW, not as a fit. The header still runs past an 80-column terminal on the
// latency metric at the longest window, because usageScopeMax's 24-column floor wins there and
// returns more room than the line has. That floor is deliberate — a scope nobody can read is
// worse than a wrapped line — so this records the cost rather than asserting a fit the code does
// not deliver. If a later change removes the overflow, this fails and says so, which is the
// right way round for a known shortfall.
func TestUsageHeader_LatencyOverflowsEightyAndFitsAt88(t *testing.T) {
	const id = "0e61b82d-8578-4d16-a18e-1234567890ab"
	const title = "/Users/someone/src/cortex/.worktrees/x"
	header := func(w int) string {
		m := newTitleModel(t, map[string]SessionMetadata{id: {Title: title}})
		// The widest reachable header: latency's grouping literal, and the longest window.
		m.usage = usageState{session: id, metric: metricLatency, windowIdx: len(usageWindows) - 1}
		m.width = w
		return strings.Split(m.renderUsage(w, 20), "\n")[0]
	}
	if got := header(80); len([]rune(got)) <= 80 {
		t.Errorf("the latency header now fits 80 columns (%d): %q — the overflow this records is "+
			"gone, and usageScopeMax's doc should stop disclosing it", len([]rune(got)), got)
	}
	if got := header(88); len([]rune(got)) > 88 {
		t.Errorf("the latency header is %d columns at 88: %q", len([]rune(got)), got)
	}
}
