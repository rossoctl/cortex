package bob

import (
	"math"
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

// fixtureDB builds <tmp>/bob.db from testdata/bob.sql. It skips the test when sqlite3 is not on PATH.
func fixtureDB(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath(sqlitecli.Program); err != nil {
		t.Skip("sqlite3 is not on PATH")
	}
	sql, err := os.Open(filepath.Join("testdata", "bob.sql"))
	if err != nil {
		t.Fatal(err)
	}
	defer sql.Close()
	db := filepath.Join(t.TempDir(), "bob.db")
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
	for s, err := range src.Sessions(since, func(string, time.Time) bool { return false }) {
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

// phase gives the events of phase made by subagent ("" is the root task), in order.
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
	if src.Agent() != "bob" || src.PricingHost() != "api.us-east.bob.ibm.com" {
		t.Fatalf("Agent %q, PricingHost %q", src.Agent(), src.PricingHost())
	}
}

func TestSource_NoDatabaseYieldsNothing(t *testing.T) {
	if got := readAll(t, New(filepath.Join(t.TempDir(), "missing.db")), time.Time{}); len(got) != 0 {
		t.Fatalf("got %d sessions, want 0", len(got))
	}
}

func TestSource_MissingSqlite3FailsWithItsName(t *testing.T) {
	src := source{dbPath: fixtureDB(t), prog: "sqlite3-not-here-1872"}
	var errs []error
	var n int
	for s, err := range src.Sessions(time.Time{}, func(string, time.Time) bool { return false }) {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		n++
		_ = s
	}
	if n != 0 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "sqlite3-not-here-1872") {
		t.Fatalf("sessions %d, errors %v; want 0 sessions and one error naming the program", n, errs)
	}
}

func TestSource_PlainTask(t *testing.T) {
	s := sessionByID(t, readAll(t, New(fixtureDB(t)), time.Time{}), "t-plain")
	if len(s.Events) != 2 {
		t.Fatalf("events %d, want 2", len(s.Events))
	}
	req, resp := s.Events[0], s.Events[1]
	if req.Phase != model.SessionRequest || resp.Phase != model.SessionResponse || req.RequestID != resp.RequestID || req.RequestID == "" {
		t.Fatalf("phases %v/%v, request ids %q/%q", req.Phase, resp.Phase, req.RequestID, resp.RequestID)
	}
	if req.Inference.Model != "premium-ide" {
		t.Errorf("model %q", req.Inference.Model)
	}
	if len(req.Inference.Messages) != 2 || req.Inference.Messages[0].Role != "system" ||
		req.Inference.Messages[1].Role != "user" || req.Inference.Messages[1].Content != "hello bob" {
		t.Errorf("request messages %+v", req.Inference.Messages)
	}
	if resp.Inference.Completion != "hi" || resp.Inference.TotalTokens != 9 {
		t.Errorf("completion %q, totalTokens %d", resp.Inference.Completion, resp.Inference.TotalTokens)
	}
	home, _ := os.UserHomeDir()
	wantProj := project.Label(project.Root("/tmp/demo", home), home)
	for _, e := range []model.SessionEvent{req, resp} {
		if e.Client == nil || e.Client.Name != "bob" || e.Client.Version != "2.1.0" {
			t.Errorf("client %+v", e.Client)
		}
		if e.Project != wantProj {
			t.Errorf("project %q, want %q", e.Project, wantProj)
		}
		if e.Direction != model.Outbound {
			t.Errorf("direction %v", e.Direction)
		}
	}
	rec, ok := event.Record(&resp)
	if !ok || rec.CostUSD != 0.000018 || rec.Source != event.SourceTranscript || rec.Provenance != "authoritative" || !rec.Settled {
		t.Errorf("record %+v ok %v", rec, ok)
	}
	if resp.Inference.AgentRole != model.AgentRoleMain {
		t.Errorf("agent role %q", resp.Inference.AgentRole)
	}
	if !resp.At.Equal(time.UnixMilli(1763000002000)) || !req.At.Equal(time.UnixMilli(1763000001000)) {
		t.Errorf("request At %v, response At %v", req.At, resp.At)
	}
	if !s.Updated.Equal(resp.At) {
		t.Errorf("Updated %v, want %v", s.Updated, resp.At)
	}
}

func TestSource_ToolCallsAndResults(t *testing.T) {
	s := sessionByID(t, readAll(t, New(fixtureDB(t)), time.Time{}), "t-tools")
	reqs, resps := phase(s, model.SessionRequest, ""), phase(s, model.SessionResponse, "")
	if len(reqs) != 2 || len(resps) != 2 {
		t.Fatalf("requests %d, responses %d; want 2 and 2", len(reqs), len(resps))
	}
	calls := resps[0].Inference.ToolCalls
	if len(calls) != 2 || calls[0].ID != "c1" || calls[0].Name != "read_file" || calls[0].Arguments != `{"path":"a.go"}` ||
		calls[1].ID != "c2" || calls[1].Name != "spawn_subagent" || calls[1].Arguments != `{"message":"list the files"}` {
		t.Errorf("tool calls %+v", calls)
	}
	if th := resps[0].Inference.ResponseThinking; th == nil || th.Blocks != 1 || th.TextBytes != len("look first") || th.SignatureBytes != len("sig") {
		t.Errorf("thinking %+v", th)
	}
	var results []string
	for _, r := range reqs[1].Inference.ToolResults {
		results = append(results, r.ToolUseID)
	}
	if !slices.Equal(results, []string{"c1", "c2"}) {
		t.Errorf("tool results %v, want c1 c2", results)
	}
	var tools []string
	for _, tl := range reqs[1].Inference.Tools {
		tools = append(tools, tl.Name)
	}
	if !slices.Equal(tools, []string{"read_file", "spawn_subagent"}) {
		t.Errorf("tools %v", tools)
	}
	if !slices.ContainsFunc(reqs[1].Inference.Messages, func(m model.InferenceMessage) bool {
		return m.Role == "tool" && m.Content == "tool-extra" && m.ToolUseID == ""
	}) {
		t.Errorf("no tool-extra message in %+v", reqs[1].Inference.Messages)
	}
	rec, ok := event.Record(&resps[1])
	if !ok || rec.CostUSD != 0 || !rec.Settled || resps[1].Inference.TotalTokens != 120 {
		t.Errorf("second record %+v ok %v, totalTokens %d", rec, ok, resps[1].Inference.TotalTokens)
	}
	if resps[1].Inference.FinishReason != "stop" {
		t.Errorf("finish reason %q", resps[1].Inference.FinishReason)
	}
	// The second request comes after the tool rows, not after the skipped row.
	if !reqs[1].At.Equal(time.UnixMilli(1763000007000)) {
		t.Errorf("second request At %v", reqs[1].At)
	}
	if s.Skipped != 1 {
		t.Errorf("Skipped %d, want 1", s.Skipped)
	}
}

func TestSource_ChildTaskInRootSession(t *testing.T) {
	sessions := readAll(t, New(fixtureDB(t)), time.Time{})
	if slices.Contains(ids(sessions), "t-child") {
		t.Fatalf("t-child is its own session: %v", ids(sessions))
	}
	s := sessionByID(t, sessions, "t-tools")
	child := append(phase(s, model.SessionRequest, "t-child"), phase(s, model.SessionResponse, "t-child")...)
	if len(child) != 4 {
		t.Fatalf("child events %d, want 4", len(child))
	}
	rootProj := phase(s, model.SessionRequest, "")[0].Project
	if rootProj == "" {
		t.Fatal("root has no project")
	}
	for _, e := range child {
		if e.SubagentKind != "subtask" || e.Inference.AgentRole != model.AgentRoleSubagent || e.Project != rootProj {
			t.Errorf("child event kind %q role %q project %q (root %q)", e.SubagentKind, e.Inference.AgentRole, e.Project, rootProj)
		}
	}
	if !s.Updated.Equal(time.UnixMilli(1763000009000)) {
		t.Errorf("Updated %v, want the child's last response", s.Updated)
	}
	if !slices.IsSortedFunc(s.Events, func(a, b model.SessionEvent) int { return a.At.Compare(b.At) }) {
		t.Error("events not sorted by At")
	}
}

func TestSource_SubagentTurnsFromSpawnRows(t *testing.T) {
	s := sessionByID(t, readAll(t, New(fixtureDB(t)), time.Time{}), "t-tools")
	rootProj := phase(s, model.SessionRequest, "")[0].Project
	for _, c := range []struct {
		id    string
		turns int
		cost  float64 // the spawn row's _meta.spend.cost
		first int64   // the first request's time, in ms
	}{
		{"t-sub", 2, 0.0001, 1763000006000},      // a task row with no messages
		{"s-inner", 1, 0.00001, 1763000006200},   // spawned by t-sub, which Bob does not allow; no task row
		{"s-copy", 1, 0.00005, 1763000006600},    // spawned by subtask t-child; also in the start_subtask copy
		{"s-resumed", 1, 0.00002, 1763000006740}, // resumes s-copy: its spawn row repeats j1 and j2
	} {
		reqs, resps := phase(s, model.SessionRequest, c.id), phase(s, model.SessionResponse, c.id)
		if len(reqs) != c.turns || len(resps) != c.turns {
			t.Errorf("%s: requests %d, responses %d; want %d", c.id, len(reqs), len(resps), c.turns)
			continue
		}
		var cost float64
		for _, e := range append(reqs, resps...) {
			if e.SubagentKind != model.SubagentKindSubagent || e.Inference.AgentRole != model.AgentRoleSubagent || e.Project != rootProj {
				t.Errorf("%s: kind %q role %q project %q", c.id, e.SubagentKind, e.Inference.AgentRole, e.Project)
			}
			if rec, ok := event.Record(&e); ok {
				cost += rec.CostUSD
			}
		}
		if math.Abs(cost-c.cost) > 1e-12 {
			t.Errorf("%s: cost %v, want %v", c.id, cost, c.cost)
		}
		if !reqs[0].At.Equal(time.UnixMilli(c.first)) {
			t.Errorf("%s: first request At %v, want %v", c.id, reqs[0].At, time.UnixMilli(c.first))
		}
	}
	// A subagent task with its own rows: the spawn row's copy adds nothing.
	orphan := sessionByID(t, readAll(t, New(fixtureDB(t)), time.Time{}), "t-orphan")
	if n := len(phase(orphan, model.SessionResponse, "t-own")); n != 1 {
		t.Errorf("t-own responses %d, want 1", n)
	}
	if reqs := phase(s, model.SessionRequest, "t-sub"); len(reqs) == 0 || len(reqs[0].Inference.Messages) != 2 || reqs[0].Inference.Messages[0].Role != "system" {
		t.Errorf("t-sub requests %+v, want the first with its system and user rows", reqs)
	}
}

func TestSource_OrphanChildIsItsOwnRoot(t *testing.T) {
	s := sessionByID(t, readAll(t, New(fixtureDB(t)), time.Time{}), "t-orphan")
	resps := phase(s, model.SessionResponse, "")
	if len(resps) != 1 {
		t.Fatalf("responses %d, want 1", len(resps))
	}
	if _, ok := event.Record(&resps[0]); ok || resps[0].Inference.TotalTokens != 0 {
		t.Errorf("orphan response has a record (%v) or tokens %d", ok, resps[0].Inference.TotalTokens)
	}
}

func TestSource_NewestFirstSinceAndSkip(t *testing.T) {
	db := fixtureDB(t)
	all := readAll(t, New(db), time.Time{})
	if got := ids(all); !slices.Equal(got, []string{"t-tools", "t-orphan", "t-plain"}) {
		t.Fatalf("ids %v, want t-tools t-orphan t-plain (no t-empty)", got)
	}
	if got := ids(readAll(t, New(db), time.UnixMilli(1763000002500))); slices.Contains(got, "t-plain") || len(got) != 2 {
		t.Errorf("since dropped the wrong trees: %v", got)
	}
	updated := sessionByID(t, all, "t-tools").Updated
	var skipAt time.Time
	var got []string
	for s, err := range New(db).Sessions(time.Time{}, func(id string, at time.Time) bool {
		if id == "t-tools" {
			skipAt = at
			return true
		}
		return false
	}) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, s.ID)
	}
	if slices.Contains(got, "t-tools") || len(got) != 2 {
		t.Errorf("skip kept t-tools: %v", got)
	}
	if skipAt.Before(updated) {
		t.Errorf("skip got %v, before Updated %v", skipAt, updated)
	}
}

func TestSource_UnreadableRoot(t *testing.T) {
	db := fixtureDB(t)
	if out, err := exec.Command(sqlitecli.Program, db, "alter table messages rename column role to role_gone").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	sessions := readAll(t, New(db), time.Time{})
	if len(sessions) != 3 {
		t.Fatalf("sessions %v, want 3", ids(sessions))
	}
	for _, s := range sessions {
		if !s.Unreadable {
			t.Errorf("session %s is not Unreadable", s.ID)
		}
	}
}

func TestSource_BadTaskOrMessageValueKeepsTheImport(t *testing.T) {
	clean := sessionByID(t, readAll(t, New(fixtureDB(t)), time.Time{}), "t-tools")
	for name, sql := range map[string]string{
		"env empty":             `insert into tasks values ('t-bad', null, 'bad', '', null, '', 'normal')`,
		"env truncated":         `insert into tasks values ('t-bad', null, 'bad', '', null, '{"model":', 'normal')`,
		"model id a number":     `insert into tasks values ('t-bad', null, 'bad', '', null, '{"model":{"id":7}}', 'normal')`,
		"created_at as text":    `insert into tasks values ('t-bad', null, 'bad', '', null, '{}', 'normal'); insert into messages values ('m-bad', 't-bad', 'user', '{"content":"x"}', '2026-11-16')`,
		"child created_at text": `insert into messages values ('m-bad', 't-child', 'user', '{"content":"x"}', '2026-11-16')`,
	} {
		t.Run(name, func(t *testing.T) {
			db := fixtureDB(t)
			if out, err := exec.Command(sqlitecli.Program, db, sql).CombinedOutput(); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			sessions := readAll(t, New(db), time.Time{})
			for _, id := range []string{"t-plain", "t-tools", "t-orphan"} {
				if s := sessionByID(t, sessions, id); s.Unreadable || len(s.Events) == 0 {
					t.Errorf("%s: Unreadable %v, events %d; want it imported", id, s.Unreadable, len(s.Events))
				}
			}
			if s := sessionByID(t, sessions, "t-tools"); name == "child created_at text" && (s.Skipped != clean.Skipped+1 || len(s.Events) != len(clean.Events)) {
				t.Errorf("t-tools Skipped %d, events %d; want %d and %d (the bad row skipped and counted)", s.Skipped, len(s.Events), clean.Skipped+1, len(clean.Events))
			}
		})
	}
}
