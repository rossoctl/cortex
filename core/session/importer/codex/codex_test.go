package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/project"
	"github.com/rossoctl/cortex/core/session/importer"
)

const (
	plainID = "11111111-1111-4111-8111-111111111111"
	toolsID = "22222222-2222-4222-8222-222222222222"
	subID   = "33333333-3333-4333-8333-333333333333"

	plainFile = "sessions/2026/11/02/rollout-2026-11-02T10-00-00-" + plainID + ".jsonl"
	toolsFile = "sessions/2026/11/03/rollout-2026-11-03T09-00-00-" + toolsID + ".jsonl"
)

// fixtures copies testdata to a temporary Codex home. Git does not keep mtimes, so it sets
// the mtime of each file to the timestamp of its last JSON line.
func fixtures(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("testdata")); err != nil {
		t.Fatal(err)
	}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		at := lastTimestamp(t, path)
		return os.Chtimes(path, at, at)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func lastTimestamp(t *testing.T, path string) time.Time {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var last time.Time
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		var l struct {
			Timestamp time.Time `json:"timestamp"`
		}
		if json.Unmarshal(sc.Bytes(), &l) == nil && !l.Timestamp.IsZero() {
			last = l.Timestamp
		}
	}
	return last
}

func readAll(t *testing.T, src importer.Source, since time.Time) []importer.Session {
	t.Helper()
	var out []importer.Session
	done, total := -1, -1
	progress := func(d, n int) { done, total = d, n }
	for s, err := range src.Sessions(since, func(string, time.Time) bool { return false }, progress) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if done != total || total < len(out) {
		t.Fatalf("last progress = %d of %d, want all of at least %d", done, total, len(out))
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

func phase(s importer.Session, p model.SessionPhase, agent string) []model.SessionEvent {
	var out []model.SessionEvent
	for _, e := range s.Events {
		if e.Phase == p && e.Subagent == agent {
			out = append(out, e)
		}
	}
	return out
}

func label(t *testing.T, cwd string) string {
	t.Helper()
	home, _ := os.UserHomeDir()
	return project.Label(project.Root(cwd, home), home)
}

// failOpens makes openRollout fail when fail gives true for the path and its open count.
func failOpens(t *testing.T, fail func(path string, n int) bool) {
	t.Helper()
	orig := openRollout
	counts := map[string]int{}
	openRollout = func(path string) (io.ReadCloser, error) {
		counts[filepath.Base(path)]++
		if fail(path, counts[filepath.Base(path)]) {
			return nil, errors.New("injected open failure")
		}
		return orig(path)
	}
	t.Cleanup(func() { openRollout = orig })
}

func TestSource_AgentAndHost(t *testing.T) {
	src := New(t.TempDir())
	if src.Agent() != "codex" || src.PricingHost() != "api.openai.com" {
		t.Fatalf("Agent %q, PricingHost %q", src.Agent(), src.PricingHost())
	}
}

func TestSource_NoSessionsDirYieldsNothing(t *testing.T) {
	if got := readAll(t, New(t.TempDir()), time.Time{}); len(got) != 0 {
		t.Fatalf("got %d sessions, want 0", len(got))
	}
}

func TestSource_HonoursCodexHome(t *testing.T) {
	t.Setenv("CODEX_HOME", fixtures(t))
	if got := ids(readAll(t, New(""), time.Time{})); !slices.Equal(got, []string{toolsID, plainID}) {
		t.Fatalf("ids = %v", got)
	}
}

func TestSource_PlainSession(t *testing.T) {
	s := sessionByID(t, fixtures(t), plainID)
	if len(s.Events) != 2 || s.Events[0].RequestID != plainID+":1" || s.Events[1].RequestID != plainID+":1" {
		t.Fatalf("events = %+v", s.Events)
	}
	if s.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", s.Skipped)
	}
	req, resp := s.Events[0], s.Events[1]
	if req.Phase != model.SessionRequest || resp.Phase != model.SessionResponse {
		t.Fatalf("phases %v %v", req.Phase, resp.Phase)
	}
	if req.Inference.Model != "gpt-5-codex" {
		t.Errorf("Model = %q", req.Inference.Model)
	}
	var roles []string
	for _, m := range req.Inference.Messages {
		roles = append(roles, m.Role)
	}
	if !slices.Equal(roles, []string{"developer", "user"}) {
		t.Errorf("roles = %v", roles)
	}
	in := resp.Inference
	if in.Completion != "hi" || in.InputTokens != 200 || in.CacheReadTokens != 1000 ||
		in.CompletionTokens != 50 || in.ReasoningTokens != 20 || in.TotalTokens != 1250 {
		t.Errorf("response = %+v", in)
	}
	if in.ResponseThinking == nil || in.ResponseThinking.Blocks != 1 {
		t.Errorf("ResponseThinking = %+v", in.ResponseThinking)
	}
	for _, e := range s.Events {
		if e.Client == nil || e.Client.Name != "codex" || e.Client.Version != "0.169.0" {
			t.Errorf("Client = %+v", e.Client)
		}
		if e.Project != label(t, "/tmp/demo") {
			t.Errorf("Project = %q", e.Project)
		}
		if e.Inference.AgentRole != model.AgentRoleMain {
			t.Errorf("AgentRole = %q", e.Inference.AgentRole)
		}
		if e.Direction != model.Outbound {
			t.Errorf("Direction = %v", e.Direction)
		}
	}
	if !req.At.Equal(time.Date(2026, 11, 2, 10, 0, 3, 0, time.UTC)) || !resp.At.Equal(time.Date(2026, 11, 2, 10, 0, 8, 0, time.UTC)) {
		t.Errorf("At = %v, %v", req.At, resp.At)
	}
	if !s.Updated.Equal(resp.At) {
		t.Errorf("Updated = %v", s.Updated)
	}
}

func TestSource_DuplicateTokenCountIsIgnored(t *testing.T) {
	s := sessionByID(t, fixtures(t), plainID)
	if n := len(phase(s, model.SessionResponse, "")); n != 1 {
		t.Fatalf("%d responses, want 1", n)
	}
}

func TestSource_ToolCallsAndResults(t *testing.T) {
	s := sessionByID(t, fixtures(t), toolsID)
	reqs, resps := phase(s, model.SessionRequest, ""), phase(s, model.SessionResponse, "")
	if len(reqs) != 2 || len(resps) != 2 {
		t.Fatalf("%d requests, %d responses", len(reqs), len(resps))
	}
	calls := resps[0].Inference.ToolCalls
	if len(calls) != 2 || calls[0].ID != "c1" || calls[0].Name != "shell" || calls[0].Arguments != `{"command":["ls"]}` ||
		calls[1].ID != "c2" || calls[1].Name != "apply_patch" {
		t.Errorf("ToolCalls = %+v", calls)
	}
	var tools []string
	for _, tool := range reqs[0].Inference.Tools {
		tools = append(tools, tool.Name)
	}
	if !slices.Equal(tools, []string{"shell"}) {
		t.Errorf("Tools = %v", tools)
	}
	var results []string
	for _, r := range reqs[1].Inference.ToolResults {
		results = append(results, r.ToolUseID)
	}
	if !slices.Equal(results, []string{"c1", "c2"}) {
		t.Errorf("ToolResults = %v", results)
	}
	last := resps[1].Inference
	if last.Completion != "listed" || last.TotalTokens != 0 || last.InputTokens != 0 || last.OutputTokens != 0 {
		t.Errorf("second response = %+v", last)
	}
	if resps[0].RequestID != toolsID+":1" || resps[1].RequestID != toolsID+":2" {
		t.Errorf("RequestIDs %q %q", resps[0].RequestID, resps[1].RequestID)
	}
}

func TestSource_SubagentInRootSession(t *testing.T) {
	all := readAll(t, New(fixtures(t)), time.Time{})
	if slices.Contains(ids(all), subID) {
		t.Fatalf("subagent is its own session: %v", ids(all))
	}
	s := sessionByID(t, fixtures(t), toolsID)
	sub := append(phase(s, model.SessionRequest, subID), phase(s, model.SessionResponse, subID)...)
	if len(sub) != 2 {
		t.Fatalf("%d subagent events, want 2", len(sub))
	}
	for _, e := range sub {
		if e.Inference.AgentRole != model.AgentRoleSubagent || e.Project != label(t, "/tmp/tools") {
			t.Errorf("subagent event AgentRole %q, Project %q", e.Inference.AgentRole, e.Project)
		}
	}
	if !slices.IsSortedFunc(s.Events, func(a, b model.SessionEvent) int { return a.At.Compare(b.At) }) {
		t.Error("events are not in time order")
	}
}

func TestSource_NewestFirstSinceAndSkip(t *testing.T) {
	dir := fixtures(t)
	if got := ids(readAll(t, New(dir), time.Time{})); !slices.Equal(got, []string{toolsID, plainID}) {
		t.Fatalf("ids = %v", got)
	}
	since := lastTimestamp(t, filepath.Join(dir, plainFile)).Add(time.Second)
	if got := ids(readAll(t, New(dir), since)); !slices.Equal(got, []string{toolsID}) {
		t.Fatalf("since: ids = %v", got)
	}
	full := sessionByID(t, dir, toolsID)
	var got time.Time
	var out []string
	for s, err := range New(dir).Sessions(time.Time{}, func(id string, at time.Time) bool {
		if id == toolsID {
			got = at
			return true
		}
		return false
	}, func(int, int) {}) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, s.ID)
	}
	if !slices.Equal(out, []string{plainID}) {
		t.Fatalf("skip: ids = %v", out)
	}
	if got.Before(full.Updated) {
		t.Errorf("skip time %v is before Updated %v", got, full.Updated)
	}
}

func TestSource_CompressedRollout(t *testing.T) {
	dir := fixtures(t)
	want := sessionByID(t, dir, plainID)
	path := filepath.Join(dir, plainFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path + ".zst")
	if err != nil {
		t.Fatal(err)
	}
	w, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	got := sessionByID(t, dir, plainID)
	if len(got.Events) != len(want.Events) || got.Events[1].Inference.Completion != want.Events[1].Inference.Completion {
		t.Fatalf("compressed read = %+v, want %+v", got.Events, want.Events)
	}
}

func TestSource_UnreadableRootIsReported(t *testing.T) {
	dir := fixtures(t)
	// The opens are the listing, the fast scan and the parse. Only the parse fails.
	failOpens(t, func(path string, n int) bool { return strings.Contains(path, toolsID) && n == 3 })
	s := sessionByID(t, dir, toolsID)
	if !s.Unreadable {
		t.Fatalf("session = %+v, want Unreadable", s)
	}
}

func TestSource_UnlistableFileIsLeftOut(t *testing.T) {
	dir := fixtures(t)
	failOpens(t, func(path string, _ int) bool { return strings.Contains(path, plainID) })
	all := readAll(t, New(dir), time.Time{})
	if got := ids(all); !slices.Equal(got, []string{"", toolsID}) || !all[0].Unreadable {
		t.Fatalf("ids = %v, want an unreadable entry then %s", got, toolsID)
	}
	if len(all[1].Events) != 6 {
		t.Errorf("tools session has %d events, want 6", len(all[1].Events))
	}
}
