package pi

import (
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session/timeline"
)

func TestTimeline_ImportedSessionShowsTurnsAndTools(t *testing.T) {
	s := sessionByID(t, fixtures(t), toolsID)
	events := s.Events
	for i := range events {
		events[i].Seq = uint64(i + 1)
		events[i].Source = model.ImportedSource("pi") // as importer.Run sets it
	}
	tl := timeline.Build(toolsID, events, time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC))

	turns := map[string]int{}
	for _, tu := range tl.Turns {
		turns[tu.Agent]++
	}
	if turns[timeline.MainAgent] != 2 {
		t.Fatalf("turns by agent = %v, want main 2", turns)
	}
	tools := map[string]bool{}
	for _, tool := range tl.Tools {
		tools[tool.Name] = true
	}
	for _, name := range []string{"read", "bash"} {
		if !tools[name] {
			t.Errorf("no tool %q in %+v", name, tl.Tools)
		}
	}
}
