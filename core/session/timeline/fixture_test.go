package timeline

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// The fixture is a real Claude Code session recorded through the proxy: a main agent that
// ran five background subagents, each returning by <agent-message> and <task-notification>.
// Message text is cut to what the timeline reads (markers, the progress-label prompt, a
// per-message hash), and hosts, commands and paths are replaced, so it stays small and
// carries nothing from the machine it came from.
func loadFixture(t testing.TB) []pipeline.SessionEvent {
	t.Helper()
	f, err := os.Open("testdata/claude-code-background-agents.json.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var v pipeline.SessionView
	if err := json.NewDecoder(zr).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v.Events
}

func TestRealSessionWithBackgroundAgents(t *testing.T) {
	tl := Build("s", loadFixture(t), time.Time{})

	want := map[string]string{
		"a1bf9707ee0e71012": "Survey cortex egress data + UIs",
		"a3e0dcd85821e1913": "Analyze real session egress data",
		"a1bbde404d16a554a": "Map agentop pane conventions",
		"aff38e9425137d93e": "Ground tool + subagent timing in sessions",
		"a8fc3b2ae8520e41b": "Assess Praxis impact on timeline",
	}
	if len(tl.Agents) != 1+len(want) || tl.Agents[0].ID != MainAgent {
		t.Fatalf("agents = %+v", tl.Agents)
	}
	for id, label := range want {
		a := findAgent(t, tl, id)
		if a.Label != label || a.Parent != MainAgent || a.SpawnTool == "" || a.SpawnedAt == nil || a.ReturnedAt == nil {
			t.Errorf("%s = %+v, want label %q, linked and returned", id, a, label)
		}
		if !a.ReturnedAt.After(*a.SpawnedAt) {
			t.Errorf("%s returned %v before it was spawned %v", id, a.ReturnedAt, a.SpawnedAt)
		}
	}

	// Every subagent's own tools are on its row and end in its own requests.
	total := len(tl.Tools)
	for _, x := range tl.Tools {
		if x.End != nil && x.End.Before(x.Start) {
			t.Errorf("tool %s ends before it starts", x.ID)
		}
	}
	// Only the main agent's last Bash call is still open: it was running when the fixture was
	// cut. The five hand-backs (which end a run, so no result comes back) and three subagent
	// calls whose results never arrived are lost, drawn as ticks rather than bars to the end.
	open, lost := map[string]int{}, map[string]int{}
	for _, x := range tl.Tools {
		switch {
		case x.Lost:
			lost[x.Name]++
		case x.End == nil:
			open[x.Agent+"/"+x.Name]++
		}
	}
	if len(open) != 1 || open["main/Bash"] != 1 {
		t.Errorf("open tools = %v of %d, want main's last Bash", open, total)
	}
	if lost["SubagentHandback"] != 5 || lost["Bash"] != 3 || len(lost) != 2 {
		t.Errorf("lost tools = %v, want 5 hand-backs and 3 Bash", lost)
	}

	// Five calls failed upstream (502 EOF, 499 the client gave up), and one 200 streamed for 40s
	// and ended with no finish reason, which its agent's next call re-sent unchanged. None may
	// read as clean.
	failed, resent := 0, 0
	for _, tu := range tl.Turns {
		if tu.Error {
			failed++
		}
		if tu.Resent {
			resent++
		}
	}
	if failed != 6 {
		t.Errorf("failed turns = %d, want 6", failed)
	}
	if resent == 0 {
		t.Error("no turn re-sent its predecessor's conversation, yet the cut stream was retried")
	}
	if len(tl.Returns) < len(want) {
		t.Errorf("returns = %d, want at least one per subagent", len(tl.Returns))
	}
	for _, h := range tl.Hosts {
		if h.Host == "llm-gateway.example:443" {
			t.Errorf("host :443 not folded into its row")
		}
	}
}

// BenchmarkBuild is the budget: one compute of a large session, which the pane pays on every
// refresh it asks for. Real sessions top out near 6,000 events.
func BenchmarkBuild(b *testing.B) {
	events := syntheticSession(6000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Build("s", events, time.Time{})
	}
}

// TestBuildBudget fails if one compute of a 6,000-event session takes over 50ms, the bound the
// pane's once-a-second refresh was designed around. Best of three, so a noisy CI neighbour
// does not fail it. Under -race, which CI's core job always uses, everything runs several
// times slower (135ms measured against 4.5ms), so the bound scales rather than the test
// skipping: a regression back to rescanning every message is two orders of magnitude, and
// still fails at 10x.
func TestBuildBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}
	budget := 50 * time.Millisecond
	if raceEnabled {
		budget *= 10
	}
	events := syntheticSession(6000)
	best := time.Duration(1<<63 - 1)
	for i := 0; i < 3; i++ {
		start := time.Now()
		Build("s", events, time.Time{})
		best = min(best, time.Since(start))
	}
	if best > budget {
		t.Errorf("Build took %v on 6000 events, budget %v", best, budget)
	}
}

// syntheticSession is shaped like the heaviest real sessions: a main agent and parallel
// subagents whose conversations grow every turn and re-send their history, multi-KB messages,
// tool calls closed by results, and tunnel traffic between turns.
func syntheticSession(n int) []pipeline.SessionEvent {
	var r rec
	hist := map[string][]pipeline.InferenceMessage{}
	agents := []string{"", "s1", "s2", "s3", "s4", "s5"}
	pending := map[string]string{}
	body := string(make([]byte, 4096))
	for i := 0; len(r.events) < n; i++ {
		a := agents[i%len(agents)]
		hist[a] = append(hist[a], user(fmt.Sprintf("turn %d %s", i, body)))
		var results []pipeline.InferenceToolResult
		if id, ok := pending[a]; ok {
			results = append(results, result(id, "done"))
		}
		id := fmt.Sprintf("tool-%d", i)
		pending[a] = id
		r.turn(a, float64(i), 2, slices.Clone(hist[a]), results, "tool_use",
			pipeline.InferenceToolCall{ID: id, Name: "Bash", Arguments: `{"command":"go test ./..."}`})
		if i%4 == 0 {
			rid := r.id()
			r.add(pipeline.SessionEvent{At: at(float64(i)), Phase: pipeline.SessionRequest, RequestID: rid, Host: "api.github.com:443", Tunnel: true, TunnelReason: "passthrough-host"})
			r.add(pipeline.SessionEvent{At: at(float64(i) + 0.3), Phase: pipeline.SessionResponse, RequestID: rid, Host: "api.github.com:443", Tunnel: true})
		}
	}
	return r.events
}
