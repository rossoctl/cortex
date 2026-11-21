package toolpair

import (
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
)

// The builders below are a deliberate, minimal copy of core/session/timeline's test helpers.
//
// WHY A COPY RATHER THAN A SHARED PACKAGE. The timeline's helpers are unexported and written
// against the core/pipeline aliases, so sharing them means either exporting them from a package
// under core/session that imports core/pipeline — a trap, since the data packages must not reach
// the plugin engine (core/model's imports_test) — or rewriting ~70 test functions in the one
// change whose whole value is proving the timeline's behaviour did not move. 30 lines of
// scaffolding is the cheaper duplicate. Promote both to a shared helper if a third consumer
// appears (#505's OTLP emitter).

var t0 = time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)

func at(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }

// tools is the manifest every agent request carries; a request without one is a side call.
var manifest = []model.InferenceTool{{Name: "Bash"}}

type rec struct {
	events []model.SessionEvent
	n      int
}

func (r *rec) add(e model.SessionEvent) {
	e.Seq = uint64(len(r.events) + 1)
	e.Direction = model.Outbound
	r.events = append(r.events, e)
}

func (r *rec) id() string { r.n++; return "req" + string(rune('a'+r.n-1)) }

// turn records one model call by agent (empty = main), sending msgs and answering with calls.
func (r *rec) turn(agent string, start, dur float64, msgs []model.InferenceMessage, results []model.InferenceToolResult, finish string, calls ...model.InferenceToolCall) {
	role := model.AgentRoleMain
	if agent != "" {
		role = model.AgentRoleSubagent
	}
	id := r.id()
	r.add(model.SessionEvent{At: at(start), Phase: model.SessionRequest, RequestID: id, Subagent: agent,
		Inference: &model.InferenceExtension{AgentRole: role, Tools: manifest, Messages: msgs, ToolResults: results, Stream: true}})
	r.add(model.SessionEvent{At: at(start + dur), Phase: model.SessionResponse, RequestID: id, Subagent: agent,
		StatusCode: 200, Duration: time.Duration(dur * float64(time.Second)),
		Inference: &model.InferenceExtension{FinishReason: finish, ToolCalls: calls}})
}

func user(s string) model.InferenceMessage {
	return model.InferenceMessage{Role: "user", Content: s}
}

func result(id, content string) model.InferenceToolResult {
	return model.InferenceToolResult{ToolUseID: id, Content: content}
}

func callOf(t *testing.T, cs []Call, toolUseID string) Call {
	t.Helper()
	for _, c := range cs {
		if c.ToolUseID == toolUseID {
			return c
		}
	}
	t.Fatalf("no call %q among %d", toolUseID, len(cs))
	return Call{}
}

// TestCallRunsFromItsResponseEndToItsResult is the ordinary case, and the one every other test
// here is a deviation from.
func TestCallRunsFromItsResponseEndToItsResult(t *testing.T) {
	var r rec
	r.turn("", 0, 2, []model.InferenceMessage{user("go")}, nil, "tool_use",
		model.InferenceToolCall{ID: "t1", Name: "Bash", Arguments: `{"command":"ls"}`})
	r.turn("", 5, 1, []model.InferenceMessage{user("go")}, []model.InferenceToolResult{result("t1", "out")}, "end_turn")

	got := Calls(r.events, Options{})
	if len(got.Calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(got.Calls))
	}
	c := got.Calls[0]
	if !c.Start.Equal(at(2)) {
		t.Errorf("Start = %v, want %v (the asking response's end)", c.Start, at(2))
	}
	if c.End == nil || !c.End.Equal(at(5)) {
		t.Errorf("End = %v, want %v (the request carrying its result)", c.End, at(5))
	}
	if c.EndReason != ReasonResult {
		t.Errorf("EndReason = %q, want %q", c.EndReason, ReasonResult)
	}
	if c.CallSeq != 2 || c.ResultSeq != 3 {
		t.Errorf("CallSeq/ResultSeq = %d/%d, want 2/3", c.CallSeq, c.ResultSeq)
	}
	if c.ResultBytes != 3 {
		t.Errorf("ResultBytes = %d, want 3", c.ResultBytes)
	}
	if c.ModelDur != 2*time.Second {
		t.Errorf("ModelDur = %v, want 2s", c.ModelDur)
	}
}

// TestResultBeforeItsResponseIsClampedToStart: Claude Code runs a tool while the response that
// asked for it still streams, so the carrying request can be recorded first. A zero-length call
// is the honest answer; a negative one is not.
func TestResultBeforeItsResponseIsClampedToStart(t *testing.T) {
	var r rec
	r.turn("", 0, 10, []model.InferenceMessage{user("go")}, nil, "tool_use",
		model.InferenceToolCall{ID: "t1", Name: "Bash"})
	r.turn("", 4, 1, []model.InferenceMessage{user("go")}, []model.InferenceToolResult{result("t1", "x")}, "end_turn")

	c := callOf(t, Calls(r.events, Options{}).Calls, "t1")
	if c.End == nil || !c.End.Equal(c.Start) {
		t.Errorf("End = %v, want it clamped to Start %v", c.End, c.Start)
	}
}

// TestResultInAnotherAgentDoesNotCloseTheCall pins the per-agent keying: two agents can use the
// same tool-use id, and a result only closes its own agent's call.
func TestResultInAnotherAgentDoesNotCloseTheCall(t *testing.T) {
	var r rec
	r.turn("sub1", 0, 1, []model.InferenceMessage{user("go")}, nil, "tool_use",
		model.InferenceToolCall{ID: "t1", Name: "Bash"})
	r.turn("sub2", 2, 1, []model.InferenceMessage{user("go")}, []model.InferenceToolResult{result("t1", "x")}, "end_turn")

	c := callOf(t, Calls(r.events, Options{}).Calls, "t1")
	if c.End != nil {
		t.Errorf("End = %v, want nil: another agent's result must not close it", c.End)
	}
}

// TestBatchSharesAStartAndIsOrdered: a response's calls share a start by construction, so
// CallIndex is the only thing that can put them back in the order the model asked.
func TestBatchSharesAStartAndIsOrdered(t *testing.T) {
	var r rec
	r.turn("", 0, 2, []model.InferenceMessage{user("go")}, nil, "tool_use",
		model.InferenceToolCall{ID: "t1", Name: "Read"},
		model.InferenceToolCall{ID: "t2", Name: "Bash"},
		model.InferenceToolCall{ID: "t3", Name: "Edit"})

	got := Calls(r.events, Options{}).Calls
	if len(got) != 3 {
		t.Fatalf("calls = %d, want 3", len(got))
	}
	for i, want := range []string{"Read", "Bash", "Edit"} {
		if got[i].Name != want || got[i].CallIndex != i {
			t.Errorf("call %d = %q index %d, want %q index %d", i, got[i].Name, got[i].CallIndex, want, i)
		}
		if got[i].SiblingN != 3 {
			t.Errorf("call %d SiblingN = %d, want 3", i, got[i].SiblingN)
		}
		if !got[i].Start.Equal(at(2)) {
			t.Errorf("call %d Start = %v, want %v", i, got[i].Start, at(2))
		}
	}
}

// TestBackgroundAcknowledgementIsItsOwnReason is T32: the call ends at the acknowledgement, so
// its duration is real but is not the command's. Marked, not folded into "result", so a
// consumer can count how much tool time is missing.
func TestBackgroundAcknowledgementIsItsOwnReason(t *testing.T) {
	var r rec
	r.turn("", 0, 1, []model.InferenceMessage{user("go")}, nil, "tool_use",
		model.InferenceToolCall{ID: "t1", Name: "Bash"})
	r.turn("", 3, 1, []model.InferenceMessage{user("go")},
		[]model.InferenceToolResult{result("t1", "Command running in background with ID: job7")}, "end_turn")

	got := Calls(r.events, Options{})
	c := callOf(t, got.Calls, "t1")
	if c.EndReason != ReasonBackground {
		t.Errorf("EndReason = %q, want %q", c.EndReason, ReasonBackground)
	}
	if c.End == nil {
		t.Fatal("End = nil, want the acknowledgement's time: a background call does have a duration")
	}
	if len(got.Jobs) != 1 || got.Jobs[0].ID != "job7" {
		t.Errorf("Jobs = %+v, want one job7", got.Jobs)
	}
}

// TestEndedRunCallsGetNoResultAndSayWhy: a StructuredOutput ends the run, so neither it nor any
// sibling of the same response is ever answered.
func TestEndedRunCallsGetNoResultAndSayWhy(t *testing.T) {
	var r rec
	r.turn("", 0, 1, []model.InferenceMessage{user("go")}, nil, "tool_use",
		model.InferenceToolCall{ID: "t1", Name: "StructuredOutput"},
		model.InferenceToolCall{ID: "t2", Name: "Bash"})

	got := Calls(r.events, Options{}).Calls
	for _, id := range []string{"t1", "t2"} {
		c := callOf(t, got, id)
		if c.EndReason != ReasonEndedRun {
			t.Errorf("%s EndReason = %q, want %q", id, c.EndReason, ReasonEndedRun)
		}
		if c.End != nil {
			t.Errorf("%s End = %v, want nil", id, c.End)
		}
	}
}

// TestAbandonedCallIsNamedAsSuch: the agent asked again without ever carrying the result.
func TestAbandonedCallIsNamedAsSuch(t *testing.T) {
	var r rec
	r.turn("", 0, 1, []model.InferenceMessage{user("go")}, nil, "tool_use",
		model.InferenceToolCall{ID: "t1", Name: "Bash"})
	r.turn("", 5, 1, []model.InferenceMessage{user("again")}, nil, "end_turn")

	c := callOf(t, Calls(r.events, Options{}).Calls, "t1")
	if c.EndReason != ReasonAbandoned {
		t.Errorf("EndReason = %q, want %q", c.EndReason, ReasonAbandoned)
	}
	if c.End != nil {
		t.Errorf("End = %v, want nil", c.End)
	}
}

// TestOpenCallHasNoReason is the distinction the zero value exists for: a call still being
// waited on is not a lost one, and must not read as a 0 ms call.
func TestOpenCallHasNoReason(t *testing.T) {
	var r rec
	r.turn("sub1", 0, 1, []model.InferenceMessage{user("go")}, nil, "tool_use",
		model.InferenceToolCall{ID: "t1", Name: "Bash"})

	c := callOf(t, Calls(r.events, Options{}).Calls, "t1")
	if c.EndReason != "" {
		t.Errorf("EndReason = %q, want empty (still open)", c.EndReason)
	}
	if c.End != nil {
		t.Errorf("End = %v, want nil", c.End)
	}
	if c.EndReason.HasEnd() {
		t.Error("HasEnd() = true for an open call")
	}
}

// TestStoppedWinsOverAbandoned pins the one precedence the timeline left unspecified: a call can
// satisfy both, and a stopped run is the more specific fact.
func TestStoppedWinsOverAbandoned(t *testing.T) {
	var r rec
	// A subagent opens a call, then the main agent carries the notification that ended that
	// run — and the subagent also asked again later, which alone would read as abandoned.
	r.turn("sub1", 0, 1, []model.InferenceMessage{user("go")}, nil, "tool_use",
		model.InferenceToolCall{ID: "t1", Name: "Bash"})
	r.turn("", 5, 1, []model.InferenceMessage{user("<task-notification>\n<task-id>sub1</task-id>")}, nil, "end_turn")
	r.turn("sub1", 9, 1, []model.InferenceMessage{user("more")}, nil, "end_turn")

	c := callOf(t, Calls(r.events, Options{}).Calls, "t1")
	if c.EndReason != ReasonStopped {
		t.Errorf("EndReason = %q, want %q", c.EndReason, ReasonStopped)
	}
}

// TestArgumentsAreAbsentUnlessAsked is the privacy property core/sessionexport relies on: its
// Call values hold no argument bytes at all, rather than holding them beside a field it does
// not read.
func TestArgumentsAreAbsentUnlessAsked(t *testing.T) {
	const secret = "SECRET-COMMAND-DO-NOT-EXPORT"
	var r rec
	r.turn("", 0, 1, []model.InferenceMessage{user("go")}, nil, "tool_use",
		model.InferenceToolCall{ID: "t1", Name: "Bash", Arguments: `{"command":"` + secret + `"}`})

	if got := callOf(t, Calls(r.events, Options{}).Calls, "t1").Arguments; got != "" {
		t.Errorf("Arguments = %q, want empty without Options.Arguments", got)
	}
	if got := callOf(t, Calls(r.events, Options{Arguments: true}).Calls, "t1").Arguments; got == "" {
		t.Error("Arguments empty with Options.Arguments: the timeline needs them for its preview")
	}
}

// TestResultContentNeverReachesACall: a Call carries the size and the error flag, never the text.
func TestResultContentNeverReachesACall(t *testing.T) {
	const secret = "SECRET-RESULT-DO-NOT-EXPORT"
	var r rec
	r.turn("", 0, 1, []model.InferenceMessage{user("go")}, nil, "tool_use",
		model.InferenceToolCall{ID: "t1", Name: "Bash"})
	r.turn("", 3, 1, []model.InferenceMessage{user("go")},
		[]model.InferenceToolResult{{ToolUseID: "t1", Content: secret, IsError: true}}, "end_turn")

	c := callOf(t, Calls(r.events, Options{}).Calls, "t1")
	if c.ResultBytes != len(secret) {
		t.Errorf("ResultBytes = %d, want %d", c.ResultBytes, len(secret))
	}
	if !c.IsError {
		t.Error("IsError = false, want true")
	}
	for _, s := range []string{c.Name, c.Arguments, c.Agent, c.ToolUseID, string(c.EndReason)} {
		if s == secret {
			t.Errorf("result content reached a Call field: %q", s)
		}
	}
}

// TestReturnsAreOptional: the scan runs either way — two closing cases need it — but a consumer
// that does not want Returns does not pay for the slice.
func TestReturnsAreOptional(t *testing.T) {
	var r rec
	r.turn("", 0, 1, []model.InferenceMessage{user(`<agent-message from="sub1" />`)}, nil, "end_turn")

	if got := Calls(r.events, Options{}).Returns; len(got) != 0 {
		t.Errorf("Returns = %+v, want none without Options.Returns", got)
	}
	if got := Calls(r.events, Options{Returns: true}).Returns; len(got) != 1 {
		t.Errorf("Returns = %+v, want one with Options.Returns", got)
	}
}

// TestOnlyNewMessagesAreScanned guards the incremental skip. Without it the scan is quadratic in
// turns, which core/session/timeline's TestBuildBudget measures as a 10-100x cliff.
func TestOnlyNewMessagesAreScanned(t *testing.T) {
	var r rec
	history := []model.InferenceMessage{user("aaaa"), user("bbbb")}
	r.turn("", 0, 1, history, nil, "end_turn")
	r.turn("", 2, 1, append(append([]model.InferenceMessage{}, history...), user("cc")), nil, "end_turn")

	got := Calls(r.events, Options{}).ScannedBytes
	if want := 4 + 4 + 2; got != want {
		t.Errorf("ScannedBytes = %d, want %d: the second turn must rescan nothing", got, want)
	}
}

// TestNoEventsIsNoCalls — a session with nothing paired must not panic or allocate.
func TestNoEventsIsNoCalls(t *testing.T) {
	if got := Calls(nil, Options{}); len(got.Calls) != 0 || len(got.Jobs) != 0 {
		t.Errorf("Calls(nil) = %+v, want empty", got)
	}
}

// TestCommonPrefix moved here with the function it tests.
func TestCommonPrefix(t *testing.T) {
	a, b, c := user("a"), user("b"), user("c")
	for _, tc := range []struct {
		prev, cur []model.InferenceMessage
		want      int
	}{
		{nil, []model.InferenceMessage{a}, 0},
		{[]model.InferenceMessage{a, b}, []model.InferenceMessage{a, b, c}, 2},
		{[]model.InferenceMessage{a, b}, []model.InferenceMessage{a, c}, 1},    // history rewritten
		{[]model.InferenceMessage{a, b, c}, []model.InferenceMessage{a, b}, 2}, // compacted shorter
		{[]model.InferenceMessage{{Role: "system", Content: "a"}}, []model.InferenceMessage{a}, 0},
	} {
		if got := CommonPrefix(tc.prev, tc.cur); got != tc.want {
			t.Errorf("CommonPrefix(%v, %v) = %d, want %d", tc.prev, tc.cur, got, tc.want)
		}
	}
}
