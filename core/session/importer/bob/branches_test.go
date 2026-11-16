package bob

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session/importer/sqlitecli"
)

func TestTrees_CycleGivesOneRoot(t *testing.T) {
	last := int64(1)
	// b and a name each other; c hangs off the cycle.
	got := trees([]task{{ID: "b", ParentID: "a", Last: &last}, {ID: "a", ParentID: "b", Last: &last}, {ID: "c", ParentID: "b"}})
	if len(got) != 1 || got[0].root.ID != "a" || len(got[0].tasks) != 3 {
		var roots []string
		for _, tr := range got {
			roots = append(roots, tr.root.ID)
		}
		t.Fatalf("roots %v, want one tree with root a and 3 tasks", roots)
	}
}

func TestTask_Branches(t *testing.T) {
	p := parser{projects: map[string]string{}}
	rows := []row{
		{ID: "m1", Role: "user", Data: `{"content":"go"}`, CreatedAt: 10},
		{ID: "m2", Role: "assistant", Data: `{"content":"ok","reasoning":"abc"}`, CreatedAt: 30},
		{ID: "m3", Role: "developer", Data: `{"content":"?"}`, CreatedAt: 31},
		{ID: "m4", Role: "user", Data: `{"content":"again"}`, CreatedAt: 20}, // out of order
		{ID: "m5", Role: "assistant", Data: `{"content":"done"}`, CreatedAt: 40},
	}
	events, skipped := p.task(&task{ID: "t", Model: "premium-ide"}, true, rows)
	if skipped != 1 {
		t.Errorf("skipped %d, want 1 (the unknown role)", skipped)
	}
	if len(events) != 4 {
		t.Fatalf("events %d, want 4", len(events))
	}
	// Decision 9: no thinkingBlocks and a reasoning text count one thinking block.
	if th := events[1].Inference.ResponseThinking; th == nil || th.Blocks != 1 || th.TextBytes != 3 {
		t.Errorf("thinking %+v, want 1 block of 3 bytes", th)
	}
	// Decision 7: a request is never before the previous response.
	if !events[2].At.Equal(time.UnixMilli(30)) {
		t.Errorf("second request At %v, want the first response's %v", events[2].At, time.UnixMilli(30))
	}
}

func TestSource_UnreadableChildKeepsRoot(t *testing.T) {
	db := fixtureDB(t)
	if runtime.GOOS == "windows" {
		t.Skip("the wrapper is a shell script")
	}
	real, err := exec.LookPath(sqlitecli.Program)
	if err != nil {
		t.Fatal(err)
	}
	prog := filepath.Join(t.TempDir(), "sqlite3-fail-child")
	script := "#!/bin/sh\ncase \"$*\" in *\"'t-child'\"*) echo boom >&2; exit 1;; esac\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(prog, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s := sessionByID(t, readAll(t, source{dbPath: db, prog: prog}, time.Time{}), "t-tools")
	if s.Unreadable || len(phase(s, model.SessionResponse, "")) != 2 || len(phase(s, model.SessionResponse, "t-child")) != 0 {
		t.Fatalf("Unreadable %v, events %d; want the root's 4 events and no child events", s.Unreadable, len(s.Events))
	}
	if s.Skipped != 2 {
		t.Errorf("Skipped %d, want 2 (the bad row and the child task)", s.Skipped)
	}
}
