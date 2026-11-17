package pi

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/project"
	"github.com/rossoctl/cortex/core/session/importer"
)

const (
	plainID = "aaaaaaaa-0000-4000-8000-000000000001"
	toolsID = "aaaaaaaa-0000-4000-8000-000000000002"
	treeID  = "aaaaaaaa-0000-4000-8000-000000000003"
	demoDir = "--tmp-demo--"
)

var fileNames = map[string]string{
	plainID: "2026-11-01T10-00-00-000Z_" + plainID + ".jsonl",
	toolsID: "2026-11-01T11-00-00-000Z_" + toolsID + ".jsonl",
	treeID:  "2026-11-01T12-00-00-000Z_" + treeID + ".jsonl",
}

// fixtures copies testdata to a temporary agent directory, so that a test can change mtimes.
func fixtures(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("testdata")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func fixturePath(dir, id string) string {
	return filepath.Join(dir, "sessions", demoDir, fileNames[id])
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

func sessionByID(t *testing.T, dir, id string) importer.Session {
	t.Helper()
	for _, s := range readAll(t, New(dir), time.Time{}) {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no session %s", id)
	return importer.Session{}
}

func requests(s importer.Session) []model.SessionEvent  { return phase(s, model.SessionRequest) }
func responses(s importer.Session) []model.SessionEvent { return phase(s, model.SessionResponse) }

func phase(s importer.Session, p model.SessionPhase) []model.SessionEvent {
	var out []model.SessionEvent
	for _, e := range s.Events {
		if e.Phase == p {
			out = append(out, e)
		}
	}
	return out
}

func ids(ss []importer.Session) []string {
	var out []string
	for _, s := range ss {
		out = append(out, s.ID)
	}
	return out
}

// contents gives the Content of each request message.
func contents(e model.SessionEvent) []string {
	var out []string
	for _, m := range e.Inference.Messages {
		out = append(out, m.Content)
	}
	return out
}

// turnByText gives the request and the response of the turn whose completion is text.
func turnByText(t *testing.T, s importer.Session, text string) (req, resp model.SessionEvent) {
	t.Helper()
	reqs, resps := requests(s), responses(s)
	for i, r := range resps {
		if r.Inference.Completion == text {
			return reqs[i], r
		}
	}
	t.Fatalf("no turn with completion %q", text)
	return
}

func TestSource_AgentAndHost(t *testing.T) {
	src := New(t.TempDir())
	if src.Agent() != "pi" || src.PricingHost() != "pi.dev" {
		t.Fatalf("Agent %q, PricingHost %q", src.Agent(), src.PricingHost())
	}
}

func TestSource_NoSessionsDirYieldsNothing(t *testing.T) {
	if got := readAll(t, New(t.TempDir()), time.Time{}); len(got) != 0 {
		t.Fatalf("got %d sessions, want 0", len(got))
	}
}

func TestSource_PlainSession(t *testing.T) {
	dir := fixtures(t)
	s := sessionByID(t, dir, plainID)
	if len(s.Events) != 4 {
		t.Fatalf("got %d events, want 4", len(s.Events))
	}
	if s.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", s.Skipped)
	}
	home, _ := os.UserHomeDir()
	wantProject := project.Label(project.Root("/tmp/demo", home), home)
	wantPhases := []model.SessionPhase{model.SessionRequest, model.SessionResponse, model.SessionRequest, model.SessionResponse}
	for i, e := range s.Events {
		if e.Phase != wantPhases[i] || e.Direction != model.Outbound {
			t.Errorf("event %d: phase %v direction %v", i, e.Phase, e.Direction)
		}
		if e.Client == nil || e.Client.Name != "pi" || e.Client.Version != "" {
			t.Errorf("event %d: Client %+v", i, e.Client)
		}
		if e.Project != wantProject {
			t.Errorf("event %d: Project %q, want %q", i, e.Project, wantProject)
		}
		if e.Inference == nil || e.Inference.AgentRole != model.AgentRoleMain || e.Inference.Model != "claude-sonnet-4-5" {
			t.Fatalf("event %d: Inference %+v", i, e.Inference)
		}
	}
	for i := 0; i < 4; i += 2 {
		if s.Events[i].At.After(s.Events[i+1].At) {
			t.Errorf("request %d At %v is after its response At %v", i/2, s.Events[i].At, s.Events[i+1].At)
		}
	}
	if got, want := s.Events[0].At, time.Date(2026, 11, 1, 10, 0, 2, 0, time.UTC); !got.Equal(want) {
		t.Errorf("request At = %v, want %v", got, want)
	}
	if got, want := s.Updated, time.Date(2026, 11, 1, 10, 0, 15, 0, time.UTC); !got.Equal(want) {
		t.Errorf("Updated = %v, want %v", got, want)
	}
	if s.Events[0].RequestID != "msg_plain_1" || s.Events[1].RequestID != "msg_plain_1" {
		t.Errorf("RequestIDs %q %q", s.Events[0].RequestID, s.Events[1].RequestID)
	}
	if fi, err := os.Stat(fixturePath(dir, plainID)); err != nil || s.SizeHint != fi.Size() {
		t.Errorf("SizeHint = %d, want the file size (%v)", s.SizeHint, err)
	}
	r1 := s.Events[1].Inference
	if r1.Completion != "Hello." || r1.FinishReason != "end_turn" {
		t.Errorf("first response: Completion %q FinishReason %q", r1.Completion, r1.FinishReason)
	}
	if r1.InputTokens != 100 || r1.CompletionTokens != 20 || r1.CacheReadTokens != 50 || r1.CacheWriteTokens != 10 {
		t.Errorf("first response usage: %+v", r1)
	}
	if n := len(s.Events[2].Inference.Messages); n != 3 {
		t.Errorf("second request has %d messages, want 3", n)
	}
}

func TestSource_ToolCallsAndResults(t *testing.T) {
	s := sessionByID(t, fixtures(t), toolsID)
	reqs, resps := requests(s), responses(s)
	if len(reqs) != 2 || len(resps) != 2 {
		t.Fatalf("got %d requests, %d responses, want 2 each", len(reqs), len(resps))
	}
	r := resps[0].Inference
	if len(r.ToolCalls) != 2 || r.ToolCalls[0].Name != "read" || r.ToolCalls[1].Name != "bash" {
		t.Fatalf("first response ToolCalls = %+v", r.ToolCalls)
	}
	if r.FinishReason != "tool_use" {
		t.Errorf("FinishReason = %q, want tool_use", r.FinishReason)
	}
	if r.ResponseThinking == nil || r.ResponseThinking.Blocks != 1 {
		t.Errorf("ResponseThinking = %+v, want 1 block", r.ResponseThinking)
	}
	if n := len(reqs[0].Inference.Tools); n != 2 {
		t.Errorf("first request has %d Tools, want 2", n)
	}
	q := reqs[1].Inference
	if len(q.ToolResults) != 2 || q.ToolResults[0].ToolUseID != r.ToolCalls[0].ID || q.ToolResults[1].ToolUseID != r.ToolCalls[1].ID {
		t.Fatalf("second request ToolResults = %+v, want the ids of %+v", q.ToolResults, r.ToolCalls)
	}
	// system, user, assistant, ONE user message with both tool results.
	var users int
	for _, m := range q.Messages {
		if m.Role == "user" {
			users++
		}
	}
	if len(q.Messages) != 4 || users != 2 || q.Messages[3].ToolUseIDs != "toolu_read_1, toolu_bash_1" {
		t.Fatalf("second request Messages = %+v, want the two results in one user message", q.Messages)
	}
}

func TestSource_BranchUsesItsOwnPath(t *testing.T) {
	s := sessionByID(t, fixtures(t), treeID)
	req, _ := turnByText(t, s, "a3")
	if got, want := contents(req), []string{"u1", "a1", "u3"}; !slices.Equal(got, want) {
		t.Fatalf("a3 request = %q, want %q", got, want)
	}
}

func TestSource_CompactionKeepsFromFirstKept(t *testing.T) {
	s := sessionByID(t, fixtures(t), treeID)
	req, _ := turnByText(t, s, "a5")
	summary := compactionPrefix + "S" + compactionSuffix
	if got, want := contents(req), []string{"SYS", summary, "u3", "a3", "u4", "u5"}; !slices.Equal(got, want) {
		t.Fatalf("a5 request = %q, want %q", got, want)
	}
	// The compaction carries the system message, so the tools stay after the cut.
	if tools := req.Inference.Tools; len(tools) != 1 || tools[0].Name != "read" {
		t.Errorf("a5 request Tools = %+v, want [read]", tools)
	}
}

func TestSource_ErrorTurnHasNoUsage(t *testing.T) {
	s := sessionByID(t, fixtures(t), treeID)
	reqs, resps := requests(s), responses(s)
	// a1, a2, a3, a4e, a5 in time order: the error turn is the fourth.
	if len(resps) != 5 {
		t.Fatalf("got %d turns, want 5", len(resps))
	}
	e := resps[3]
	if u := pricing.UsageFromInference(e.Inference); u != (pricing.Usage{}) {
		t.Errorf("error turn usage = %+v, want none", u)
	}
	if want := treeID + "/30000009"; e.RequestID != want || reqs[3].RequestID != want {
		t.Errorf("error turn RequestID = %q / %q, want %q", reqs[3].RequestID, e.RequestID, want)
	}
}

func TestSource_HeaderlessFileIsUnreadable(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "sessions", demoDir)
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	const id = "bbbbbbbb-0000-4000-8000-000000000009"
	line := `{"type":"message","id":"40000001","parentId":null,"timestamp":"2026-11-01T13:00:00.000Z","message":{"role":"user","content":"hi"}}` + "\n"
	if err := os.WriteFile(filepath.Join(proj, "2026-11-01T13-00-00-000Z_"+id+".jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	got := readAll(t, New(dir), time.Time{})
	if len(got) != 1 || got[0].ID != id || !got[0].Unreadable || len(got[0].Events) != 0 {
		t.Fatalf("got %+v, want one Unreadable session %s", got, id)
	}
}

func TestSource_NewestFirstAndSince(t *testing.T) {
	dir := fixtures(t)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mid := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for id, at := range map[string]time.Time{plainID: recent, toolsID: old, treeID: old.Add(time.Hour)} {
		if err := os.Chtimes(fixturePath(dir, id), at, at); err != nil {
			t.Fatal(err)
		}
	}
	if got := ids(readAll(t, New(dir), time.Time{})); !slices.Equal(got, []string{plainID, treeID, toolsID}) {
		t.Fatalf("order = %v, want newest first", got)
	}
	if got := ids(readAll(t, New(dir), mid)); !slices.Equal(got, []string{plainID}) {
		t.Fatalf("since: %v, want only %s", got, plainID)
	}
}

// skip gets the parsed Updated before the parse, from newestAt, and a true from it stops the
// parse. The fixture mtimes are now, so only the newestAt call can match Updated.
func TestSessions_SkipBeforeTheParse(t *testing.T) {
	dir := fixtures(t)
	want := map[string]time.Time{}
	for _, s := range readAll(t, New(dir), time.Time{}) {
		want[s.ID] = s.Updated
	}
	var yielded []string
	for s, err := range New(dir).Sessions(time.Time{}, func(id string, updated time.Time) bool {
		return id == plainID && updated.Equal(want[id])
	}, func(int, int) {}) {
		if err != nil {
			t.Fatal(err)
		}
		yielded = append(yielded, s.ID)
	}
	if slices.Contains(yielded, plainID) || len(yielded) != len(want)-1 {
		t.Fatalf("yielded %v, want all but %s", yielded, plainID)
	}
}

func TestSource_HonoursPiCodingAgentDir(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", fixtures(t))
	if got := readAll(t, New(""), time.Time{}); len(got) != 3 {
		t.Fatalf("got %v, want 3 sessions", ids(got))
	}
}

// TestSource_OffSpecSystemFieldsKeepTheLine checks that a system message with a wrong-typed
// sections or tools value is still a context entry: the later
// turn keeps the earlier history, the prompt and the tools.
func TestSource_OffSpecSystemFieldsKeepTheLine(t *testing.T) {
	const lead = `{"role":"system","content":"","sections":{"preamble":"P"},"toolsAdded":[{"name":"read"},{"name":"bash"}]}`
	for _, tc := range []struct{ name, lead, delta string }{
		{"section value not a string", lead, `{"role":"system","content":"","sections":{"skills":5}}`},
		{"sections not an object", lead, `{"role":"system","content":"","sections":[]}`},
		{"toolsRemoved element not an object", lead, `{"role":"system","content":"","toolsRemoved":["bash"]}`},
		{"toolsRemoved not an array", lead, `{"role":"system","content":"","toolsRemoved":{"name":"bash"}}`},
		{"toolsAdded element not an object", lead, `{"role":"system","content":"","toolsAdded":["x"]}`},
		{"leading section value not a string", `{"role":"system","content":"","sections":{"preamble":"P","cwd":1},"toolsAdded":[{"name":"read"},{"name":"bash"}]}`,
			`{"role":"system","content":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			proj := filepath.Join(dir, "sessions", demoDir)
			if err := os.MkdirAll(proj, 0o755); err != nil {
				t.Fatal(err)
			}
			const id = "bbbbbbbb-0000-4000-8000-00000000000a"
			msg := func(n int, parent, m string) string {
				p := `null`
				if parent != "" {
					p = `"` + parent + `"`
				}
				return fmt.Sprintf(`{"type":"message","id":"5000000%d","parentId":%s,"timestamp":"2026-11-01T13:00:0%d.000Z","message":%s}`, n, p, n, m) + "\n"
			}
			body := `{"type":"session","version":3,"id":"` + id + `","timestamp":"2026-11-01T13:00:00.000Z","cwd":"/tmp/demo"}` + "\n" +
				msg(1, "", tc.lead) +
				msg(2, "50000001", `{"role":"user","content":"u1"}`) +
				msg(3, "50000002", `{"role":"assistant","content":[{"type":"text","text":"a1"}],"model":"m"}`) +
				msg(4, "50000003", tc.delta) +
				msg(5, "50000004", `{"role":"user","content":"u2"}`) +
				msg(6, "50000005", `{"role":"assistant","content":[{"type":"text","text":"a2"}],"model":"m"}`)
			if err := os.WriteFile(filepath.Join(proj, "2026-11-01T13-00-00-000Z_"+id+".jsonl"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			s := sessionByID(t, dir, id)
			if s.Skipped != 0 {
				t.Errorf("Skipped = %d, want 0", s.Skipped)
			}
			req, _ := turnByText(t, s, "a2")
			var roles, tools []string
			for _, m := range req.Inference.Messages {
				roles = append(roles, m.Role)
			}
			for _, tl := range req.Inference.Tools {
				tools = append(tools, tl.Name)
			}
			if want := []string{"system", "user", "assistant", "user"}; !slices.Equal(roles, want) {
				t.Errorf("roles = %v, want %v", roles, want)
			}
			if len(req.Inference.Messages) > 0 && req.Inference.Messages[0].Content != "P" {
				t.Errorf("system = %q, want %q", req.Inference.Messages[0].Content, "P")
			}
			if want := []string{"read", "bash"}; !slices.Equal(tools, want) {
				t.Errorf("tools = %v, want %v", tools, want)
			}
		})
	}
}
