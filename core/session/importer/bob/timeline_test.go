package bob

import (
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session/timeline"
)

func TestTimeline_ImportedTaskShowsToolsAndChild(t *testing.T) {
	s := sessionByID(t, readAll(t, New(fixtureDB(t)), time.Time{}), "t-tools")
	events := s.Events
	for i := range events {
		events[i].Seq = uint64(i + 1)
		events[i].Source = model.ImportedSource("bob") // as importer.Run sets it
	}
	tl := timeline.Build("t-tools", events, time.UnixMilli(1763000010000))

	tools := map[string]bool{}
	for _, tool := range tl.Tools {
		tools[tool.Name] = true
	}
	for _, name := range []string{"read_file", "list_files"} {
		if !tools[name] {
			t.Errorf("no tool %q in %+v", name, tl.Tools)
		}
	}
	var child bool
	for _, a := range tl.Agents {
		child = child || a.ID == "t-child"
	}
	if !child {
		t.Fatalf("no t-child row in %+v", tl.Agents)
	}
}
