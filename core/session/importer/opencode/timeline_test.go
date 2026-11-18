package opencode

import (
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session/timeline"
)

func TestTimeline_ImportedSessionShowsToolsAndSubagent(t *testing.T) {
	s := sessionByID(t, readAll(t, New(fixtureDB(t)), time.Time{}), "ses_tools")
	events := s.Events
	for i := range events {
		events[i].Seq = uint64(i + 1)
		events[i].Source = model.ImportedSource("opencode") // as importer.Run sets it
	}
	tl := timeline.Build("ses_tools", events, time.UnixMilli(1763000010000))

	tools := map[string]bool{}
	for _, tool := range tl.Tools {
		tools[tool.Name] = true
	}
	for _, name := range []string{"read", "subagent"} {
		if !tools[name] {
			t.Errorf("no tool %q in %+v", name, tl.Tools)
		}
	}
	found := false
	for _, a := range tl.Agents {
		found = found || a.ID == "ses_child"
	}
	if !found {
		t.Errorf("no ses_child row in %+v", tl.Agents)
	}
}
