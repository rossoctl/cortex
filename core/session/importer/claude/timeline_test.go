package claude

import (
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session/timeline"
)

func TestTimeline_ImportedSessionShowsTurnsToolsSubagents(t *testing.T) {
	s := sessionByID(t, fixtures(t), toolsID)
	events := s.Events
	for i := range events {
		events[i].Seq = uint64(i + 1)
		events[i].Source = model.ImportedSource("claude-code") // as importer.Run sets it
	}
	tl := timeline.Build(toolsID, events, time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC))

	turns := map[string]int{}
	for _, tu := range tl.Turns {
		turns[tu.Agent]++
	}
	if turns[timeline.MainAgent] != 4 || turns["a1b2c3"] != 2 {
		t.Fatalf("turns by agent = %v, want main 4 and a1b2c3 2", turns)
	}
	tools := map[string]bool{}
	for _, tool := range tl.Tools {
		tools[tool.Name] = true
	}
	for _, name := range []string{"Read", "Agent", "Glob"} {
		if !tools[name] {
			t.Errorf("no tool %q in %+v", name, tl.Tools)
		}
	}
	var sub bool
	for _, a := range tl.Agents {
		sub = sub || a.ID == "a1b2c3"
	}
	if !sub {
		t.Fatalf("no subagent row in %+v", tl.Agents)
	}
}
