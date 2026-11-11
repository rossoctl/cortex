package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session/timeline"
)

// tlTool is a tool of agent "main" that response seq asked for at start. end < 0 means no end.
func tlTool(seq uint64, id, name string, start, end float64) timeline.Tool {
	t := timeline.Tool{Agent: "main", ID: id, Name: name, Start: tlAt(start), Seq: seq, Preview: "p-" + id}
	if end >= 0 {
		t.End = tlPtr(end)
	}
	return t
}

// onlyRow indexes tl and returns the one tool row with the given id.
func onlyRow(t *testing.T, tl *timeline.Timeline, id string) toolRow {
	t.Helper()
	for _, rows := range indexTools(tl) {
		for _, r := range rows {
			if r.ID == id {
				return r
			}
		}
	}
	t.Fatalf("no tool row %q", id)
	return toolRow{}
}

func TestIndexTools_Nil(t *testing.T) {
	if idx := indexTools(nil); idx != nil {
		t.Fatalf("indexTools(nil) = %v, want nil", idx)
	}
}

func TestIndexTools_GroupsBySeqInOrder(t *testing.T) {
	tl := &timeline.Timeline{Tools: []timeline.Tool{
		tlTool(5, "a", "Bash", 1, 2),
		tlTool(5, "b", "Read", 1, 3),
		tlTool(7, "c", "Grep", 4, 5),
	}}
	idx := indexTools(tl)
	if len(idx[5]) != 2 || idx[5][0].Name != "Bash" || idx[5][1].Name != "Read" {
		t.Fatalf("idx[5] = %+v, want Bash then Read", idx[5])
	}
	if len(idx[7]) != 1 || idx[7][0].Name != "Grep" {
		t.Fatalf("idx[7] = %+v, want Grep", idx[7])
	}
}

func TestIndexTools_Parallel(t *testing.T) {
	tl := &timeline.Timeline{Tools: []timeline.Tool{
		tlTool(5, "a", "Bash", 1, 9),
		tlTool(5, "b", "Read", 1, 9),
		tlTool(5, "c", "Grep", 1, 4),
		tlTool(6, "d", "Bash", 10, 12),
		tlTool(7, "e", "Bash", 13, -1),
		tlTool(7, "f", "Read", 13, -1),
	}}
	want := map[string]bool{"a": true, "b": true, "c": false, "d": false, "e": false, "f": false}
	for id, par := range want {
		if got := onlyRow(t, tl, id).Parallel; got != par {
			t.Errorf("tool %s Parallel = %v, want %v", id, got, par)
		}
	}
}

func TestToolRow_DurationCell(t *testing.T) {
	now := tlAt(100)
	lost := tlTool(1, "l", "Bash", 1, 2)
	lost.Lost = true
	eor := tlTool(1, "s", "StructuredOutput", 1, 2)
	eor.Lost = true
	cases := []struct {
		name string
		row  toolRow
		want string
	}{
		{"parallel", toolRow{Tool: tlTool(1, "a", "Bash", 1, 14.21), Parallel: true}, "∥13.21s"},
		{"short", toolRow{Tool: tlTool(1, "a", "Bash", 1, 1.3)}, "300ms"},
		{"running", toolRow{Tool: tlTool(1, "a", "Bash", 62, -1)}, "⋯ 38.00s"},
		{"lost", toolRow{Tool: lost}, ""},
		{"end of run", toolRow{Tool: eor, EndsRun: true}, ""},
	}
	for _, c := range cases {
		if got := c.row.durationCell(now); got != c.want {
			t.Errorf("%s: durationCell = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestToolRow_Status(t *testing.T) {
	errTool := tlTool(1, "e", "Bash", 1, 2)
	errTool.Error = true
	lost := tlTool(1, "l", "Bash", 1, 2)
	lost.Lost = true
	eor := tlTool(1, "s", "StructuredOutput", 1, 2)
	eor.Lost = true
	tl := &timeline.Timeline{Tools: []timeline.Tool{tlTool(1, "r", "Bash", 1, -1), errTool, lost, eor, tlTool(1, "o", "Bash", 1, 3)}}
	want := map[string]string{"r": "running", "e": "error", "l": "lost", "s": "", "o": "ok"}
	for id, st := range want {
		if got := onlyRow(t, tl, id).status(); got != st {
			t.Errorf("tool %s status = %q, want %q", id, got, st)
		}
	}
}

func TestIndexTools_AgentRun(t *testing.T) {
	tl := &timeline.Timeline{
		Tools:  []timeline.Tool{tlTool(3, "t1", "Agent", 8, 9.7)},
		Agents: []timeline.Agent{{ID: "main"}, {ID: "sub", Parent: "main", SpawnTool: "t1", SpawnedAt: tlPtr(10), ReturnedAt: tlPtr(358)}},
	}
	if got, want := onlyRow(t, tl, "t1").trailing(tlAt(400)), "run 5m48s · p-t1"; got != want {
		t.Errorf("ended run: trailing = %q, want %q", got, want)
	}
	tl.Agents[1].ReturnedAt = nil
	if got, want := onlyRow(t, tl, "t1").trailing(tlAt(40)), "run ⋯ 30.00s · p-t1"; got != want {
		t.Errorf("running run: trailing = %q, want %q", got, want)
	}
}

func TestIndexTools_BgJob(t *testing.T) {
	tl := &timeline.Timeline{
		Tools: []timeline.Tool{tlTool(4, "b", "Bash", 19, 20)},
		Jobs:  []timeline.Job{{Agent: "main", ID: "j1", Start: tlAt(20), End: tlPtr(262)}},
	}
	if got, want := onlyRow(t, tl, "b").trailing(tlAt(300)), "bg 4m02s · p-b"; got != want {
		t.Errorf("ended job: trailing = %q, want %q", got, want)
	}
	tl.Jobs[0].End = nil
	if got := onlyRow(t, tl, "b").trailing(tlAt(50)); !strings.HasPrefix(got, "bg ⋯ ") {
		t.Errorf("running job: trailing = %q, want prefix %q", got, "bg ⋯ ")
	}

	// Two parallel tools end where the job starts: only the Bash one owns it.
	tl = &timeline.Timeline{
		Tools: []timeline.Tool{tlTool(4, "b", "Bash", 19, 20), tlTool(4, "r", "Read", 19, 20)},
		Jobs:  []timeline.Job{{Agent: "main", ID: "j1", Start: tlAt(20), End: tlPtr(262)}},
	}
	if got := onlyRow(t, tl, "b").trailing(tlAt(300)); !strings.HasPrefix(got, "bg ") {
		t.Errorf("Bash tool: trailing = %q, want a bg part", got)
	}
	if got, want := onlyRow(t, tl, "r").trailing(tlAt(300)), "p-r"; got != want {
		t.Errorf("Read tool: trailing = %q, want %q", got, want)
	}

	// Two Bash calls of one response: their results share a request, so they end at one time.
	// Only the call that started the job owns it, and each job goes to its own call.
	tl = &timeline.Timeline{
		Tools: []timeline.Tool{tlTool(4, "b1", "Bash", 19, 20), tlTool(4, "b2", "Bash", 19, 20)},
		Jobs:  []timeline.Job{{Agent: "main", ID: "j1", ToolUseID: "b2", Start: tlAt(20), End: tlPtr(262)}},
	}
	if got, want := onlyRow(t, tl, "b1").trailing(tlAt(300)), "p-b1"; got != want {
		t.Errorf("Bash call with no job: trailing = %q, want %q", got, want)
	}
	if got := onlyRow(t, tl, "b2").trailing(tlAt(300)); !strings.HasPrefix(got, "bg 4m02s") {
		t.Errorf("Bash call with the job: trailing = %q, want a bg part", got)
	}
	tl.Jobs = append(tl.Jobs, timeline.Job{Agent: "main", ID: "j2", ToolUseID: "b1", Start: tlAt(20), End: tlPtr(80)})
	if got := onlyRow(t, tl, "b1").trailing(tlAt(300)); !strings.HasPrefix(got, "bg 1m00s") {
		t.Errorf("second job: trailing = %q, want bg 1m00s", got)
	}

	// A reused ID (call_0): the job goes to the call that started it, not to a later call
	// with the same ID.
	tl = &timeline.Timeline{
		Tools: []timeline.Tool{tlTool(4, "call_0", "Bash", 19, 20), tlTool(9, "call_0", "Bash", 40, 50)},
		Jobs:  []timeline.Job{{Agent: "main", ID: "j1", ToolUseID: "call_0", Start: tlAt(20), End: tlPtr(262)}},
	}
	idx := indexTools(tl)
	if got := idx[4][0].trailing(tlAt(300)); !strings.HasPrefix(got, "bg 4m02s") {
		t.Errorf("call that started the job: trailing = %q, want a bg part", got)
	}
	if got, want := idx[9][0].trailing(tlAt(300)), "p-call_0"; got != want {
		t.Errorf("later call with the same ID: trailing = %q, want %q", got, want)
	}

	// Build clamped the tool's End up to its Start: the result request (seq 3, 4.9s) came
	// before the asking response (seq 4) ended at 5s. The job still goes to the call.
	tl = &timeline.Timeline{
		Tools: []timeline.Tool{tlTool(4, "t1", "Bash", 5, 5)},
		Jobs:  []timeline.Job{{Agent: "main", ID: "bk1", ToolUseID: "t1", Start: tlAt(4.9), End: tlPtr(10), Seq: 3}},
	}
	if got := onlyRow(t, tl, "t1").trailing(tlAt(300)); !strings.HasPrefix(got, "bg ") {
		t.Errorf("clamped End: trailing = %q, want a bg part", got)
	}

	// A reused ID where only the later call starts a job: the earlier call gets none.
	tl = &timeline.Timeline{
		Tools: []timeline.Tool{tlTool(4, "call_0", "Bash", 19, 20), tlTool(9, "call_0", "Bash", 40, 50)},
		Jobs:  []timeline.Job{{Agent: "main", ID: "j1", ToolUseID: "call_0", Start: tlAt(50), End: tlPtr(60), Seq: 10}},
	}
	idx = indexTools(tl)
	if got, want := idx[4][0].trailing(tlAt(300)), "p-call_0"; got != want {
		t.Errorf("earlier call, no job: trailing = %q, want %q", got, want)
	}
	if got := idx[9][0].trailing(tlAt(300)); !strings.HasPrefix(got, "bg 10.00s") {
		t.Errorf("later call with the job: trailing = %q, want bg 10.00s", got)
	}

	// A job of another agent matches nothing.
	tl = &timeline.Timeline{
		Tools: []timeline.Tool{tlTool(4, "b1", "Bash", 19, 20), tlTool(4, "b2", "Bash", 19, 20)},
		Jobs:  []timeline.Job{{Agent: "main", ID: "j1", ToolUseID: "b2", Start: tlAt(20), End: tlPtr(262)}},
	}
	tl.Jobs[0].Agent = "sub"
	if got, want := onlyRow(t, tl, "b2").trailing(tlAt(300)), "p-b2"; got != want {
		t.Errorf("job of another agent: trailing = %q, want %q", got, want)
	}
}

func TestToolRow_EndsRunTrailing(t *testing.T) {
	h := tlTool(2, "h", "SubagentHandback", 1, 1)
	h.Lost, h.Preview = true, "x"
	tl := &timeline.Timeline{Tools: []timeline.Tool{h}}
	r := onlyRow(t, tl, "h")
	if !r.EndsRun {
		t.Fatal("lost SubagentHandback: EndsRun = false")
	}
	if got, want := r.trailing(tlAt(5)), "ends run · x"; got != want {
		t.Errorf("trailing = %q, want %q", got, want)
	}
	r.Preview = ""
	if got, want := r.trailing(tlAt(5)), "ends run"; got != want {
		t.Errorf("empty preview: trailing = %q, want %q", got, want)
	}
	// Not lost: an ordinary tool, not the end of the run.
	h.Lost = false
	if onlyRow(t, &timeline.Timeline{Tools: []timeline.Tool{h}}, "h").EndsRun {
		t.Error("SubagentHandback with a result: EndsRun = true")
	}
}

func TestToolRow_DurationSortValue(t *testing.T) {
	now := tlAt(100)
	lost := tlTool(1, "l", "Bash", 1, 2)
	lost.Lost = true
	cases := []struct {
		name string
		row  toolRow
		want time.Duration
	}{
		{"running", toolRow{Tool: tlTool(1, "a", "Bash", 60, -1)}, 40 * time.Second},
		{"lost", toolRow{Tool: lost}, 0},
		{"end of run", toolRow{Tool: lost, EndsRun: true}, 0},
		{"ended", toolRow{Tool: tlTool(1, "a", "Bash", 1, 3.5)}, 2500 * time.Millisecond},
	}
	for _, c := range cases {
		if got := c.row.duration(now); got != c.want {
			t.Errorf("%s: duration = %v, want %v", c.name, got, c.want)
		}
	}
}

// Some local models send one tool ID (call_0) on every turn: the pin finds the row of its own
// response, not the first row with that ID.
func TestPinKey_ToolRowWithAReusedID(t *testing.T) {
	a, b := tlTool(4, "call_0", "Bash", 1, 2), tlTool(9, "call_0", "Bash", 5, 6)
	rows := []eventRow{{tool: &toolRow{Tool: a}}, {tool: &toolRow{Tool: b}}}
	if got := findByKey(rows, pinKeyOf(rows[1])); got != 1 {
		t.Errorf("findByKey = %d, want 1", got)
	}
}

// The clamp case in append order: the result request (at 8) is appended before the
// response that asked for the call (finished at 10), so it has the lower Seq.
func TestIndexTools_BgJobOfAClampedCallFromBuild(t *testing.T) {
	tools := []pipeline.InferenceTool{{Name: "Bash"}}
	h := []pipeline.InferenceMessage{{Role: "user", Content: "go"}}
	ev := []pipeline.SessionEvent{
		{Seq: 1, At: tlAt(0), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, RequestID: "r1", Host: "llm",
			Inference: &pipeline.InferenceExtension{AgentRole: pipeline.AgentRoleMain, Tools: tools, Messages: h}},
		{Seq: 2, At: tlAt(8), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, RequestID: "r2", Host: "llm",
			Inference: &pipeline.InferenceExtension{AgentRole: pipeline.AgentRoleMain, Tools: tools, Messages: h,
				ToolResults: []pipeline.InferenceToolResult{{ToolUseID: "t1", Content: "Command running in background with ID: bk1"}}}},
		{Seq: 3, At: tlAt(9), Direction: pipeline.Outbound, Phase: pipeline.SessionResponse, RequestID: "r2", Host: "llm", StatusCode: 200, Duration: time.Second,
			Inference: &pipeline.InferenceExtension{FinishReason: "end_turn"}},
		{Seq: 4, At: tlAt(10), Direction: pipeline.Outbound, Phase: pipeline.SessionResponse, RequestID: "r1", Host: "llm", StatusCode: 200, Duration: 10 * time.Second,
			Inference: &pipeline.InferenceExtension{FinishReason: "tool_use", ToolCalls: []pipeline.InferenceToolCall{{ID: "t1", Name: "Bash"}}}},
	}
	tl := timeline.Build("s", ev, time.Time{})
	if got := onlyRow(t, tl, "t1").trailing(tlAt(300)); !strings.HasPrefix(got, "bg ") {
		t.Errorf("clamped End from Build: trailing = %q, want a bg part", got)
	}
}

// A reused ID (call_0) gives the subagent's run span only to the first Agent call of its
// parent, not to every tool with that ID.
func TestIndexTools_RunOnlyOnTheSpawningCall(t *testing.T) {
	sub := tlTool(5, "call_0", "Read", 11, 12)
	sub.Agent = "sub"
	tl := &timeline.Timeline{
		Tools: []timeline.Tool{
			tlTool(2, "call_0", "Bash", 1, 2),
			tlTool(3, "call_0", "Agent", 8, 9.7),
			sub,
			tlTool(7, "call_0", "Agent", 20, 21),
		},
		Agents: []timeline.Agent{{ID: "main"}, {ID: "sub", Parent: "main", SpawnTool: "call_0", SpawnedAt: tlPtr(10), ReturnedAt: tlPtr(14)}},
	}
	idx := indexTools(tl)
	for seq, want := range map[uint64]bool{2: false, 3: true, 5: false, 7: false} {
		if got := idx[seq][0].RunFrom != nil; got != want {
			t.Errorf("seq %d (%s): has run = %v, want %v", seq, idx[seq][0].Name, got, want)
		}
	}
}

// Two calls with no ID in one response: the pin finds the second, not the first.
func TestPinKey_TwoToolsWithNoIDInOneResponse(t *testing.T) {
	idx := indexTools(&timeline.Timeline{Tools: []timeline.Tool{tlTool(4, "", "Bash", 1, 2), tlTool(4, "", "Bash", 1, 3)}})
	rows := []eventRow{{tool: &idx[4][0]}, {tool: &idx[4][1]}}
	if got := findByKey(rows, pinKeyOf(rows[1])); got != 1 {
		t.Errorf("findByKey = %d, want 1", got)
	}
}
