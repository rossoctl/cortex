package codex

import (
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session/timeline"
)

func TestTimeline_ImportedSessionShowsToolsAndSubagent(t *testing.T) {
	s := sessionByID(t, fixtures(t), toolsID)
	events := s.Events
	for i := range events {
		events[i].Seq = uint64(i + 1)
		events[i].Source = model.ImportedSource("codex") // as importer.Run sets it
	}
	tl := timeline.Build(toolsID, events, time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC))

	tools := map[string]bool{}
	for _, tool := range tl.Tools {
		tools[tool.Name] = true
	}
	for _, name := range []string{"shell", "apply_patch"} {
		if !tools[name] {
			t.Errorf("no tool %q in %+v", name, tl.Tools)
		}
	}
	var sub bool
	for _, a := range tl.Agents {
		sub = sub || a.ID == subID
	}
	if !sub {
		t.Fatalf("no subagent row in %+v", tl.Agents)
	}
}
