package opencode

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/project"
	"github.com/rossoctl/cortex/core/session/importer"
	"github.com/rossoctl/cortex/core/session/importer/sqlitecli"
)

// fixtureDB builds <tmp>/opencode.db from testdata/opencode.sql. It skips the test when sqlite3 is not on PATH.
func fixtureDB(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath(sqlitecli.Program); err != nil {
		t.Skip("sqlite3 is not on PATH")
	}
	sql, err := os.Open(filepath.Join("testdata", "opencode.sql"))
	if err != nil {
		t.Fatal(err)
	}
	defer sql.Close()
	db := filepath.Join(t.TempDir(), "opencode.db")
	cmd := exec.Command(sqlitecli.Program, db)
	cmd.Stdin = sql
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v: %s", err, out)
	}
	return db
}

func readAll(t *testing.T, src importer.Source, since time.Time) []importer.Session {
	t.Helper()
	var out []importer.Session
	for s, err := range src.Sessions(since, func(string, time.Time) bool { return false }, func(int, int) {}) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func sessionByID(t *testing.T, sessions []importer.Session, id string) importer.Session {
	t.Helper()
	for _, s := range sessions {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no session %s", id)
	return importer.Session{}
}

// phase gives the events of phase made by subagent ("" is the main agent), in order.
func phase(s importer.Session, p model.SessionPhase, subagent string) []model.SessionEvent {
	var out []model.SessionEvent
	for _, e := range s.Events {
		if e.Phase == p && e.Subagent == subagent {
			out = append(out, e)
		}
	}
	return out
}

func ids(sessions []importer.Session) []string {
	var out []string
	for _, s := range sessions {
		out = append(out, s.ID)
	}
	return out
}

func TestSource_AgentAndHost(t *testing.T) {
	src := New("")
	if src.Agent() != "opencode" || src.PricingHost() != "opencode.ai" {
		t.Fatalf("agent %q host %q", src.Agent(), src.PricingHost())
	}
}

func TestSource_NoDatabaseYieldsNothing(t *testing.T) {
	if got := readAll(t, New(filepath.Join(t.TempDir(), "missing.db")), time.Time{}); len(got) != 0 {
		t.Fatalf("sessions %v, want none", ids(got))
	}
}

func TestSource_HonoursXDGDataHome(t *testing.T) {
	db := fixtureDB(t)
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	b, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "opencode", "opencode.db"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	sessionByID(t, readAll(t, New(""), time.Time{}), "ses_plain")
}

func TestSource_MissingSqlite3FailsWithItsName(t *testing.T) {
	src := source{dbPath: fixtureDB(t), prog: "sqlite3-not-here-1874"}
	var errs []error
	var n int
	for _, err := range src.Sessions(time.Time{}, func(string, time.Time) bool { return false }, func(int, int) {}) {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	if n != 0 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "sqlite3-not-here-1874") {
		t.Fatalf("sessions %d, errors %v; want 0 sessions and one error naming the program", n, errs)
	}
}

func TestSource_PlainSession(t *testing.T) {
	s := sessionByID(t, readAll(t, New(fixtureDB(t)), time.Time{}), "ses_plain")
	if len(s.Events) < 2 {
		t.Fatalf("events %d, want at least 2", len(s.Events))
	}
	req, resp := s.Events[0], s.Events[1]
	if req.Phase != model.SessionRequest || resp.Phase != model.SessionResponse || req.RequestID != resp.RequestID || req.RequestID == "" {
		t.Fatalf("phases %v/%v, request ids %q/%q", req.Phase, resp.Phase, req.RequestID, resp.RequestID)
	}
	if req.Inference.Model != "fledge-alpha-free" {
		t.Errorf("model %q", req.Inference.Model)
	}
	if len(req.Inference.Messages) != 1 || req.Inference.Messages[0].Role != "user" || req.Inference.Messages[0].Content != "hello opencode" {
		t.Errorf("request messages %+v", req.Inference.Messages)
	}
	in := resp.Inference
	if in.Completion != "hi" || in.InputTokens != 193 || in.CompletionTokens != 154 || in.CacheReadTokens != 7040 || in.FinishReason != "stop" {
		t.Errorf("response %+v", in)
	}
	if in.ResponseThinking == nil || in.ResponseThinking.Blocks != 1 {
		t.Errorf("thinking %+v", in.ResponseThinking)
	}
	home, _ := os.UserHomeDir()
	wantProj := project.Label(project.Root("/tmp/demo", home), home)
	for _, e := range []model.SessionEvent{req, resp} {
		if e.Client == nil || e.Client.Name != "opencode" || e.Client.Version != "2.0.26" {
			t.Errorf("client %+v", e.Client)
		}
		if e.Project != wantProj {
			t.Errorf("project %q, want %q", e.Project, wantProj)
		}
		if e.Direction != model.Outbound || e.Inference.AgentRole != model.AgentRoleMain || e.Subagent != "" {
			t.Errorf("direction %v, role %q, subagent %q", e.Direction, e.Inference.AgentRole, e.Subagent)
		}
	}
	if _, ok := event.Record(&resp); ok {
		t.Errorf("a cost record is set; importer.Run must price the turn")
	}
	if !req.At.Equal(time.UnixMilli(1763000001500)) || !resp.At.Equal(time.UnixMilli(1763000002000)) {
		t.Errorf("request At %v, response At %v", req.At, resp.At)
	}
}

func TestSource_ToolCallsAndResults(t *testing.T) {
	s := sessionByID(t, readAll(t, New(fixtureDB(t)), time.Time{}), "ses_tools")
	reqs, resps := phase(s, model.SessionRequest, ""), phase(s, model.SessionResponse, "")
	if len(reqs)+len(resps) != 4 {
		t.Fatalf("main events %d, want 4 (the error row gives none)", len(reqs)+len(resps))
	}
	a := resps[0].Inference
	var got []string
	for _, c := range a.ToolCalls {
		got = append(got, c.ID+":"+c.Name)
	}
	if !slices.Equal(got, []string{"c1:read", "c2:shell", "c3:subagent"}) {
		t.Errorf("tool calls %v", got)
	}
	if a.CompletionTokens != 8+14 {
		t.Errorf("completion tokens %d, want 22 (output + reasoning)", a.CompletionTokens)
	}
	results := map[string]model.InferenceToolResult{}
	for _, r := range reqs[1].Inference.ToolResults {
		results[r.ToolUseID] = r
	}
	if len(results) != 3 || results["c1"].Content != "file body" || results["c1"].IsError ||
		!results["c2"].IsError || results["c2"].Content != "exit 1" || !strings.Contains(results["c3"].Content, "ses_child") {
		t.Errorf("tool results %+v", reqs[1].Inference.ToolResults)
	}
	if s.Skipped != 2 {
		t.Errorf("Skipped %d, want 2 (the not-json row and the bogus row)", s.Skipped)
	}
}

func TestSource_SubagentInParentSession(t *testing.T) {
	sessions := readAll(t, New(fixtureDB(t)), time.Time{})
	if slices.Contains(ids(sessions), "ses_child") {
		t.Fatalf("ses_child is its own session: %v", ids(sessions))
	}
	s := sessionByID(t, sessions, "ses_tools")
	child := append(phase(s, model.SessionRequest, "ses_child"), phase(s, model.SessionResponse, "ses_child")...)
	if len(child) != 2 {
		t.Fatalf("child events %d, want 2", len(child))
	}
	for _, e := range child {
		if e.Inference.AgentRole != model.AgentRoleSubagent || e.SubagentKind != "" {
			t.Errorf("role %q kind %q", e.Inference.AgentRole, e.SubagentKind)
		}
	}
}

func TestSource_ForkFilesUnderSourceWithoutCopies(t *testing.T) {
	sessions := readAll(t, New(fixtureDB(t)), time.Time{})
	if slices.Contains(ids(sessions), "ses_fork") {
		t.Fatalf("ses_fork is its own session: %v", ids(sessions))
	}
	s := sessionByID(t, sessions, "ses_plain")
	if len(s.Events) != 4 {
		t.Fatalf("events %d, want 4 (its own turn and the fork's turn)", len(s.Events))
	}
	fork := s.Events[2]
	if fork.RequestID != "msg_f4" || fork.Subagent != "" || s.Events[3].Subagent != "" {
		t.Fatalf("fork request %q subagent %q", fork.RequestID, fork.Subagent)
	}
	var texts []string
	for _, m := range fork.Inference.Messages {
		texts = append(texts, m.Role+":"+m.Content)
	}
	if !slices.Equal(texts, []string{"user:hello opencode", "assistant:hi", "user:forked"}) {
		t.Errorf("fork messages %v", texts)
	}
}

func TestSource_MissingParentKeepsAffinityID(t *testing.T) {
	s := sessionByID(t, readAll(t, New(fixtureDB(t)), time.Time{}), "ses_gone")
	resps := phase(s, model.SessionResponse, "ses_orphan")
	if len(resps) != 1 || resps[0].Inference.TotalTokens != 0 {
		t.Fatalf("responses %+v", resps)
	}
}

func TestSource_NewestFirstSinceAndSkip(t *testing.T) {
	db := fixtureDB(t)
	all := readAll(t, New(db), time.Time{})
	if got := ids(all); !slices.Equal(got, []string{"ses_tools", "ses_plain", "ses_gone"}) {
		t.Fatalf("ids %v, want ses_tools ses_plain ses_gone (no ses_empty)", got)
	}
	if got := ids(readAll(t, New(db), time.UnixMilli(1763000000500))); !slices.Equal(got, []string{"ses_tools", "ses_plain"}) {
		t.Errorf("since: %v, want ses_gone dropped", got)
	}
	skipAt := map[string]time.Time{}
	var got []string
	for s, err := range New(db).Sessions(time.Time{}, func(id string, at time.Time) bool {
		skipAt[id] = at
		return id == "ses_tools"
	}, func(int, int) {}) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, s.ID)
	}
	if slices.Contains(got, "ses_tools") || len(got) != 2 {
		t.Errorf("skip kept ses_tools: %v", got)
	}
	for _, s := range all {
		if skipAt[s.ID].Before(s.Updated) {
			t.Errorf("%s: skip got %v, before Updated %v", s.ID, skipAt[s.ID], s.Updated)
		}
	}
}

func TestSource_UnreadableRoot(t *testing.T) {
	db := fixtureDB(t)
	if out, err := exec.Command(sqlitecli.Program, db, "alter table session_message rename column data to data_gone").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	sessions := readAll(t, New(db), time.Time{})
	if got := ids(sessions); !slices.Equal(got, []string{"ses_tools", "ses_plain", "ses_gone"}) {
		t.Fatalf("sessions %v", got)
	}
	for _, s := range sessions {
		if !s.Unreadable {
			t.Errorf("session %s is not Unreadable", s.ID)
		}
	}
}

// TestSource_UnreadableChildRow fails the messages query of one non-root row. The group stays
// readable and counts the row in Skipped: ses_tools with its root row, and ses_gone, which has
// no root row and keeps one readable row.
func TestSource_UnreadableChildRow(t *testing.T) {
	db := fixtureDB(t)
	if out, err := exec.Command(sqlitecli.Program, db, `insert into session_v2 values ('ses_orphan2', 'ses_gone', null, '/tmp/demo', '2.0.26', 1763000000060);
insert into session_message values ('msg_o9', 'ses_orphan2', 'user', 1, 1763000000110, 1763000000110, '{"text":"orphan two","time":{"created":1763000000110}}');`).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	base := readAll(t, New(db), time.Time{})
	// The shim fails each query that names ses_child or ses_orphan, and runs sqlite3 for the others.
	shim := filepath.Join(t.TempDir(), "sqlite3-shim")
	script := "#!/bin/sh\ncase \"$*\" in *\"'ses_child'\"*|*\"'ses_orphan'\"*) echo no such table >&2; exit 1;; esac\nexec " + sqlitecli.Program + " \"$@\"\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	got := readAll(t, source{dbPath: db, prog: shim}, time.Time{})
	for _, id := range []string{"ses_tools", "ses_gone"} {
		s, b := sessionByID(t, got, id), sessionByID(t, base, id)
		if s.Unreadable {
			t.Errorf("%s is Unreadable", id)
		}
		if s.Skipped != b.Skipped+1 {
			t.Errorf("%s: Skipped %d, want %d", id, s.Skipped, b.Skipped+1)
		}
	}
	// ses_orphan2 has only a user message, so ses_gone is readable with no events.
	tools := sessionByID(t, got, "ses_tools")
	if len(tools.Events) == 0 || len(phase(tools, model.SessionRequest, "ses_child")) != 0 {
		t.Errorf("ses_tools: events %d, want the root row's events and none from ses_child", len(tools.Events))
	}
}

// TestSource_SubagentForkGivesNoCopies: a row with both a parent and a fork source holds copies
// of the source's rows, with new ids. The copies give no turns, as for a fork with no parent.
func TestSource_SubagentForkGivesNoCopies(t *testing.T) {
	db := fixtureDB(t)
	if out, err := exec.Command(sqlitecli.Program, db, `insert into session_v2 values ('ses_child2', 'ses_tools', 'ses_child', '/tmp/tools', '2.0.26', 1763000009500);
insert into session_message values ('msg_d1', 'ses_child2', 'user', 1, 1763000008000, 1763000008000, '{"text":"go"}');
insert into session_message values ('msg_d2', 'ses_child2', 'assistant', 2, 1763000008500, 1763000009000,
  '{"model":{"id":"gpt-5"},"content":[{"type":"text","text":"child reply"}],"tokens":{"input":20,"output":2,"reasoning":0,"cache":{"read":0,"write":0}},"time":{"completed":1763000009000}}');
insert into session_message values ('msg_d3', 'ses_child2', 'user', 3, 1763000009600, 1763000009600, '{"text":"again"}');
insert into session_message values ('msg_d4', 'ses_child2', 'assistant', 4, 1763000009700, 1763000009800,
  '{"model":{"id":"gpt-5"},"content":[{"type":"text","text":"fork child reply"}],"tokens":{"input":30,"output":3,"reasoning":0,"cache":{"read":0,"write":0}},"time":{"completed":1763000009800}}');`).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	s := sessionByID(t, readAll(t, New(db), time.Time{}), "ses_tools")
	var got []string
	for _, e := range phase(s, model.SessionRequest, "ses_child2") {
		got = append(got, e.RequestID)
	}
	if !slices.Equal(got, []string{"msg_d4"}) {
		t.Fatalf("ses_child2 requests %v, want only msg_d4 (msg_d2 is a copy)", got)
	}
}

// TestSource_SkipTimeIsLastResponse: rows after the last response (a user row with no reply, an
// idle row) do not move the time that Sessions gives skip. Else a session that was imported
// would be parsed again on each run.
func TestSource_SkipTimeIsLastResponse(t *testing.T) {
	db := fixtureDB(t)
	if out, err := exec.Command(sqlitecli.Program, db, `insert into session_message values ('msg_t8', 'ses_tools', 'user', 8, 1763000010000, 1763000010000, '{"text":"no reply yet"}');
insert into session_message values ('msg_t9', 'ses_tools', 'idle', 9, 1763000010100, 1763000010100, '{}');`).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	all := readAll(t, New(db), time.Time{})
	skipAt := map[string]time.Time{}
	for range New(db).Sessions(time.Time{}, func(id string, at time.Time) bool { skipAt[id] = at; return true }, func(int, int) {}) {
	}
	for _, s := range all {
		if !skipAt[s.ID].Equal(s.Updated) {
			t.Errorf("%s: skip got %v, want Updated %v", s.ID, skipAt[s.ID], s.Updated)
		}
	}
}
