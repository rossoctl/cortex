package evolve

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// fakeEvolveServer serves the evolve MCP tools the plugin calls, with canned
// answers shaped like the stock server's, over a real SSE transport.
type fakeEvolveServer struct {
	mu        sync.Mutex
	calls     []mcp.CallToolRequest
	headers   []http.Header
	failLearn bool
	listErr   bool
	// old is an evolve that predates include_details: it rejects the argument
	// the way pydantic does.
	old bool
	// noDetailsField accepts include_details but answers without guidelines.
	noDetailsField bool
	// rejectPrompt is an evolve that predates guidelines_prompt (altk-evolve #479):
	// its processor config rejects the key the way pydantic does.
	rejectPrompt bool
	// holdLearn, when set, keeps process_trajectory from answering until it is
	// closed.
	holdLearn chan struct{}
	// calling, when set, receives each tool's name as its call arrives. Tests give
	// it room for every call they make.
	calling chan string
	// paged serves list_entities a page at a time: 7 and 8, then 9.
	paged bool
	// cursor, when set, makes list_entities serve 7 alone, on and on: the same
	// cursor every time ("same"), or a new one every time ("advancing").
	cursor string
	pages  int
}

func (f *fakeEvolveServer) record(req mcp.CallToolRequest) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.headers = append(f.headers, req.Header)
	f.mu.Unlock()
	if f.calling != nil {
		f.calling <- req.Params.Name
	}
}

func (f *fakeEvolveServer) start(t *testing.T) string {
	t.Helper()
	ts := server.NewTestServer(f.mcpServer())
	t.Cleanup(ts.Close)
	return ts.URL + "/sse"
}

func (f *fakeEvolveServer) mcpServer() *server.MCPServer {
	s := server.NewMCPServer("entities", "test")
	text := func(v any) *mcp.CallToolResult {
		b, _ := json.Marshal(v)
		return mcp.NewToolResultText(string(b))
	}
	s.AddTool(mcp.NewTool("get_guidelines_with_attribution"), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		f.record(req)
		_, details := req.GetArguments()["include_details"]
		if details && f.old {
			return mcp.NewToolResultError("1 validation error for call[get_guidelines_with_attribution]\ninclude_details\n" +
				"  Unexpected keyword argument [type=unexpected_keyword_argument, input_value=True, input_type=bool]"), nil
		}
		out := map[string]any{"text": "prose", "entity_ids": []string{"7", "9"}, "entity_revisions": map[string]any{"7": 2, "9": nil}, "namespace_id": "cortex"}
		if details && !f.noDetailsField {
			// 9 was consolidated from two sessions, and its content is structured.
			out["guidelines"] = []any{
				map[string]any{"id": "7", "content": "guideline 7", "trigger": "when testing", "source_task_ids": []string{"cortex:s:7"}},
				map[string]any{"id": "9", "content": map[string]any{"rule": "guideline 9"}, "trigger": nil, "source_task_ids": []string{"cortex:a:1", "cortex:b:2"}},
			}
		}
		return text(out), nil
	})
	s.AddTool(mcp.NewTool("ensure_processing_profile"), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		f.record(req)
		if f.rejectPrompt && profilePrompt(req) != "" {
			return mcp.NewToolResultError("Invalid config for guidelines (evolve.guidelines): 1 validation error for GuidelineConfig\n" +
				"guidelines_prompt\n  Extra inputs are not permitted [type=extra_forbidden, input_value='environment-grounded', input_type=str]"), nil
		}
		return text(map[string]any{"id": "cortex", "revision": 1}), nil
	})
	s.AddTool(mcp.NewTool("list_entities"), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		f.record(req)
		if f.listErr {
			return text(map[string]any{"error": "invalid cursor"}), nil
		}
		if f.cursor != "" {
			f.mu.Lock()
			f.pages++
			next := "same"
			if f.cursor == "advancing" {
				next = fmt.Sprintf("page-%d", f.pages)
			}
			f.mu.Unlock()
			return text(map[string]any{"items": []any{
				map[string]any{"id": "7", "content": "guideline 7", "metadata": map[string]any{}},
			}, "next_cursor": next}), nil
		}
		if f.paged {
			if req.GetArguments()["cursor"] != "page-2" {
				return text(map[string]any{"items": []any{
					map[string]any{"id": "7", "content": "guideline 7", "metadata": map[string]any{"source_task_id": "cortex:s:7"}},
					map[string]any{"id": "8", "content": "guideline 8", "metadata": map[string]any{}},
				}, "next_cursor": "page-2"}), nil
			}
			return text(map[string]any{"items": []any{
				map[string]any{"id": "9", "content": "guideline 9", "metadata": map[string]any{}},
			}, "next_cursor": nil}), nil
		}
		// 7 is unowned, 9 was learned from a Claude Code session; 8 is not attributed.
		return text(map[string]any{"items": []any{
			map[string]any{"id": "7", "content": "guideline 7", "metadata": map[string]any{"source_task_id": "cortex:s:7"}},
			map[string]any{"id": "8", "content": "guideline 8", "metadata": map[string]any{}},
			map[string]any{"id": "9", "content": "guideline 9", "metadata": map[string]any{"agent_id": "claude-code"}},
		}}), nil
	})
	s.AddTool(mcp.NewTool("process_trajectory"), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		f.record(req)
		if f.holdLearn != nil {
			select {
			case <-f.holdLearn:
			case <-ctx.Done():
			}
		}
		if f.failLearn {
			return mcp.NewToolResultError("Namespace not found"), nil
		}
		return text(map[string]any{"operation_id": "op", "updates": []any{}}), nil
	})
	return s
}

func (f *fakeEvolveServer) tools() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for _, c := range f.calls {
		names = append(names, c.Params.Name)
	}
	return names
}

func TestMCPEvolve_GuidelinesComeWithTheirAttribution(t *testing.T) {
	f := &fakeEvolveServer{}
	e := newMCPEvolve(f.start(t), "cortex", "environment-grounded", time.Second)
	defer e.Close()

	gs, err := e.Guidelines(context.Background(), "cortex", "fix the test", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != 2 || gs[0].ID != "7" || gs[0].Content != "guideline 7" || *gs[0].Revision != 2 || gs[0].Source != "cortex:s:7" ||
		gs[1].ID != "9" || gs[1].Content != `{"rule":"guideline 9"}` || gs[1].Revision != nil || gs[1].Source != "cortex:a:1,cortex:b:2" {
		t.Fatalf("guidelines = %+v", gs)
	}
	args := f.calls[0].GetArguments()
	if args["task"] != "fix the test" || args["namespace_id"] != "cortex" || args["session_id"] != "s1" || args["include_details"] != true {
		t.Fatalf("args = %v", args)
	}
	// One call: the guidelines come with their attribution, with no namespace read
	// and so no cap on how large the served namespace can be.
	if got := f.tools(); len(got) != 1 {
		t.Fatalf("tools called = %v, want only get_guidelines_with_attribution", got)
	}
}

func TestMCPEvolve_OlderEvolveFallsBackToReadingTheNamespace(t *testing.T) {
	f := &fakeEvolveServer{old: true}
	e := newMCPEvolve(f.start(t), "cortex", "environment-grounded", time.Second)
	defer e.Close()

	for range 2 {
		gs, err := e.Guidelines(context.Background(), "cortex", "fix the test", "s1")
		if err != nil {
			t.Fatal(err)
		}
		// 8 is in the namespace but not attributed, so it is left out.
		if len(gs) != 2 || gs[0].ID != "7" || gs[0].Content != "guideline 7" || *gs[0].Revision != 2 || gs[0].Source != "cortex:s:7" ||
			gs[1].ID != "9" || gs[1].Content != "guideline 9" || gs[1].Revision != nil {
			t.Fatalf("guidelines = %+v", gs)
		}
	}
	// The first fetch learns that this evolve rejects include_details; the second
	// doesn't send it again.
	got := f.tools()
	want := []string{"get_guidelines_with_attribution", "get_guidelines_with_attribution", "list_entities", "get_guidelines_with_attribution", "list_entities"}
	if len(got) != len(want) {
		t.Fatalf("tools called = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tools called = %v, want %v", got, want)
		}
	}
	if _, sent := f.calls[1].GetArguments()["include_details"]; sent {
		t.Fatal("retry still sent include_details")
	}
	if _, sent := f.calls[3].GetArguments()["include_details"]; sent {
		t.Fatal("include_details sent again to an evolve that rejected it")
	}
	if a := f.calls[2].GetArguments(); a["namespace_id"] != "cortex" || a["include_content"] != true {
		t.Fatalf("list_entities args = %v, want the serve namespace with content", a)
	}
}

func TestMCPEvolve_AttributionWithoutDetailsReadsTheNamespace(t *testing.T) {
	f := &fakeEvolveServer{noDetailsField: true}
	e := newMCPEvolve(f.start(t), "cortex", "environment-grounded", time.Second)
	defer e.Close()

	gs, err := e.Guidelines(context.Background(), "cortex", "t", "s")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.tools(); len(got) != 2 || got[1] != "list_entities" || len(gs) != 2 || gs[0].Source != "cortex:s:7" {
		t.Fatalf("tools called = %v, guidelines = %+v", got, gs)
	}
}

func TestMCPEvolve_LearnPreparesServerOnce(t *testing.T) {
	f := &fakeEvolveServer{}
	e := newMCPEvolve(f.start(t), "cortex", "environment-grounded", time.Second)
	defer e.Close()
	tr := Trajectory{
		Messages: []map[string]any{{"role": "user", "content": "x"}},
		Batch:    &Batch{Source: "cortex", ConversationID: "s1", BatchID: "1-1", Scope: "cortex"},
	}
	for range 2 {
		if err := e.Learn(context.Background(), "cortex-candidates", tr); err != nil {
			t.Fatal(err)
		}
	}
	got := f.tools()
	if len(got) != 4 || got[0] != "ensure_processing_profile" || got[1] != "list_entities" || got[2] != "process_trajectory" || got[3] != "process_trajectory" {
		t.Fatalf("tools called = %v", got)
	}
	if f.calls[1].GetArguments()["namespace_id"] != "cortex-candidates" {
		t.Fatal("learn namespace not created before processing")
	}
	args := f.calls[2].GetArguments()
	if args["namespace_id"] != "cortex-candidates" || args["processing_profile"] != "cortex" {
		t.Fatalf("args = %v", args)
	}
	batch := args["trajectory"].(map[string]any)["batch"].(map[string]any)
	if batch["conversation_id"] != "s1" || batch["batch_id"] != "1-1" {
		t.Fatalf("batch = %v", batch)
	}
}

func TestMCPEvolve_ToolErrorsAreUnavailable(t *testing.T) {
	f := &fakeEvolveServer{failLearn: true}
	e := newMCPEvolve(f.start(t), "cortex", "environment-grounded", time.Second)
	defer e.Close()
	err := e.Learn(context.Background(), "cortex-candidates", Trajectory{Messages: []map[string]any{}})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	f.listErr, f.noDetailsField = true, true
	if _, err := e.Guidelines(context.Background(), "cortex", "t", "s"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("list error = %v", err)
	}
}

func TestMCPEvolve_UnreachableIsUnavailable(t *testing.T) {
	e := newMCPEvolve("http://127.0.0.1:1/sse", "cortex", "environment-grounded", time.Second)
	e.connectBudget = time.Hour
	defer e.Close()
	for range 2 {
		// A refused connection fails at once, the second time too: nothing is left
		// waiting for a stream that never opened. The call has no deadline and the
		// connect an hour, so a call that waited for either would not return.
		if _, err := e.Guidelines(context.Background(), "cortex", "t", "s"); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("err = %v", err)
		}
	}
}

func TestMCPEvolve_IgnoresProxyEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	f := &fakeEvolveServer{}
	e := newMCPEvolve(f.start(t), "cortex", "environment-grounded", time.Second)
	defer e.Close()
	if _, err := e.Guidelines(context.Background(), "cortex", "t", "s"); err != nil {
		t.Fatalf("request went through the proxy: %v", err)
	}
	// The call above can't tell: Go never proxies a loopback address, which the
	// fake server is on. So check the transport itself, as an evolve on another
	// host would be reached.
	tr, ok := e.http.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Fatal("evolve's transport would use the environment's proxy")
	}
}

// profilePrompt is the guidelines_prompt an ensure_processing_profile call asked for, or "".
func profilePrompt(req mcp.CallToolRequest) string {
	def, _ := req.GetArguments()["definition"].(map[string]any)
	processors, _ := def["processors"].([]any)
	if len(processors) == 0 {
		return ""
	}
	config, _ := processors[0].(map[string]any)["config"].(map[string]any)
	prompt, _ := config["guidelines_prompt"].(string)
	return prompt
}

func TestMCPEvolve_ProfileLearnsWithTheConfiguredPrompt(t *testing.T) {
	f := &fakeEvolveServer{}
	e := newMCPEvolve(f.start(t), "cortex", "environment-grounded", time.Second)
	defer e.Close()
	if err := e.Learn(context.Background(), "cortex", Trajectory{Messages: []map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if got := profilePrompt(f.calls[0]); got != "environment-grounded" {
		t.Fatalf("profile prompt = %q, want environment-grounded", got)
	}
}

func TestMCPEvolve_EmptyPromptLeavesItToTheServer(t *testing.T) {
	f := &fakeEvolveServer{}
	e := newMCPEvolve(f.start(t), "cortex", "", time.Second)
	defer e.Close()
	if err := e.Learn(context.Background(), "cortex", Trajectory{Messages: []map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	config := f.calls[0].GetArguments()["definition"].(map[string]any)["processors"].([]any)[0].(map[string]any)["config"].(map[string]any)
	if _, sent := config["guidelines_prompt"]; sent {
		t.Fatalf("config = %v, want no guidelines_prompt", config)
	}
}

func TestMCPEvolve_AnEvolveWithoutThePromptStillLearns(t *testing.T) {
	f := &fakeEvolveServer{rejectPrompt: true}
	e := newMCPEvolve(f.start(t), "cortex", "environment-grounded", time.Second)
	defer e.Close()
	tr := Trajectory{Messages: []map[string]any{{"role": "user", "content": "x"}}}
	for range 2 {
		if err := e.Learn(context.Background(), "cortex", tr); err != nil {
			t.Fatalf("learn: %v", err)
		}
	}
	// Rejected with the prompt, then created without it; the second Learn reuses that.
	got := f.tools()
	want := []string{"ensure_processing_profile", "ensure_processing_profile", "list_entities", "process_trajectory", "process_trajectory"}
	if len(got) != len(want) {
		t.Fatalf("tools called = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tools called = %v, want %v", got, want)
		}
	}
	if profilePrompt(f.calls[0]) != "environment-grounded" || profilePrompt(f.calls[1]) != "" {
		t.Fatalf("prompts sent = %q, %q; want environment-grounded, then none", profilePrompt(f.calls[0]), profilePrompt(f.calls[1]))
	}
}

// hungStream answers the SSE request with headers and then nothing: no endpoint
// event, so the session never comes up. arrived is closed once the headers are
// sent.
func hungStream(t *testing.T) (url string, arrived <-chan struct{}) {
	t.Helper()
	stop := make(chan struct{})
	came := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		once.Do(func() { close(came) })
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	t.Cleanup(func() { close(stop); srv.Close() })
	return srv.URL + "/sse", came
}

// silentListener accepts connections and never answers them. arrived is closed
// once the first is accepted.
func silentListener(t *testing.T) (url string, arrived <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	accepting := make(chan struct{})
	came := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(accepting)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			once.Do(func() { close(came) })
		}
	}()
	t.Cleanup(func() {
		// Wait for the accept loop to stop before closing what it accepted, so a
		// connection accepted just before cleanup is closed too.
		_ = ln.Close()
		<-accepting
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return "http://" + ln.Addr().String() + "/sse", came
}

func TestMCPEvolve_AHungEvolveCostsACallOnlyItsOwnTimeout(t *testing.T) {
	for name, hung := range map[string]func(*testing.T) (string, <-chan struct{}){
		"no endpoint event": hungStream,
		"silent listener":   silentListener,
	} {
		t.Run(name, func(t *testing.T) {
			url, arrived := hung(t)
			e := newMCPEvolve(url, "cortex", "environment-grounded", time.Second)
			// The connect's own bounds are an hour, so only the calls' contexts end
			// the calls.
			e.connectBudget = time.Hour
			e.http.Transport.(*http.Transport).ResponseHeaderTimeout = time.Hour
			// Two sessions' first requests at once: neither waits on the other, and
			// each returns when its own context ends, with the connect still hung.
			// Before, the first waited 30 s (no endpoint event) or forever (silent
			// listener), and the second queued behind it.
			ctx, cancel := context.WithCancel(context.Background())
			var wg sync.WaitGroup
			for range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, err := e.Guidelines(ctx, "cortex", "t", "s"); !errors.Is(err, ErrUnavailable) {
						t.Errorf("err = %v, want ErrUnavailable", err)
					}
				}()
			}
			<-arrived
			cancel()
			wg.Wait()
			// Close returns with the connect still hung.
			e.Close()
		})
	}
}

func TestMCPEvolve_ConnectsAgainAfterClose(t *testing.T) {
	f := &fakeEvolveServer{}
	e := newMCPEvolve(f.start(t), "cortex", "environment-grounded", time.Second)
	defer e.Close()
	for range 2 {
		if _, err := e.Guidelines(context.Background(), "cortex", "t", "s"); err != nil {
			t.Fatal(err)
		}
		e.Close()
	}
}

func (f *fakeEvolveServer) called(tool string) bool {
	for _, name := range f.tools() {
		if name == tool {
			return true
		}
	}
	return false
}

func TestMCPEvolve_OneCallTimingOutLeavesTheOthersRunning(t *testing.T) {
	f := &fakeEvolveServer{holdLearn: make(chan struct{}), calling: make(chan string, 4)}
	e := newMCPEvolve(f.start(t), "cortex", "environment-grounded", time.Second)
	defer e.Close()

	learned := make(chan error, 1)
	go func() {
		learned <- e.Learn(context.Background(), "cortex", Trajectory{Messages: []map[string]any{{"role": "user", "content": "x"}}})
	}()
	// Wait for process_trajectory to start, or for Learn to fail before it does.
	for tool := ""; tool != "process_trajectory"; {
		select {
		case tool = <-f.calling:
		case err := <-learned:
			t.Fatalf("Learn ended before process_trajectory: %v", err)
		}
	}
	// A session's fetch times out while evolve is still learning. Before, the
	// timeout closed the shared session and failed the learning call with it, so
	// evolve's minutes of LLM work were sent again.
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	_, err := e.Guidelines(ctx, "cortex", "t", "s")
	cancel()
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("fetch err = %v, want ErrUnavailable", err)
	}
	close(f.holdLearn)
	if err := <-learned; err != nil {
		t.Fatalf("learning failed with the fetch: %v", err)
	}
}

// swappable serves whichever handler is current at one URL, so a test can stand
// in a fresh evolve for the one a session was opened with.
type swappable struct {
	mu sync.Mutex
	h  http.Handler
}

func (s *swappable) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	h := s.h
	s.mu.Unlock()
	h.ServeHTTP(w, r)
}

func (s *swappable) set(h http.Handler) {
	s.mu.Lock()
	s.h = h
	s.mu.Unlock()
}

func TestMCPEvolve_AnEvolveThatForgotTheSessionIsReconnected(t *testing.T) {
	sw := &swappable{}
	ts := httptest.NewServer(sw)
	t.Cleanup(ts.Close)
	first, second := &fakeEvolveServer{}, &fakeEvolveServer{}
	sw.set(server.NewSSEServer(first.mcpServer(), server.WithBaseURL(ts.URL)))
	e := newMCPEvolve(ts.URL+"/sse", "cortex", "environment-grounded", time.Second)
	defer e.Close()
	if _, err := e.Guidelines(context.Background(), "cortex", "t", "s"); err != nil {
		t.Fatal(err)
	}

	// Evolve restarts: the new process doesn't know the session, and rejects the
	// call. That call fails; the next one connects again.
	sw.set(server.NewSSEServer(second.mcpServer(), server.WithBaseURL(ts.URL)))
	if _, err := e.Guidelines(context.Background(), "cortex", "t", "s"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want the stale session refused", err)
	}
	if _, err := e.Guidelines(context.Background(), "cortex", "t", "s"); err != nil {
		t.Fatalf("not reconnected: %v", err)
	}
	if !second.called("get_guidelines_with_attribution") {
		t.Fatal("the new evolve was never called")
	}
}

func TestMCPEvolve_OlderEvolveFindsGuidelinesBeyondTheFirstPage(t *testing.T) {
	f := &fakeEvolveServer{old: true, paged: true}
	e := newMCPEvolve(f.start(t), "cortex", "environment-grounded", time.Second)
	defer e.Close()
	gs, err := e.Guidelines(context.Background(), "cortex", "t", "s")
	if err != nil {
		t.Fatal(err)
	}
	// 9 is on the second page. Before, only the first page was read, and 9 was
	// silently left out.
	if len(gs) != 2 || gs[0].ID != "7" || gs[1].ID != "9" || gs[1].Content != "guideline 9" {
		t.Fatalf("guidelines = %+v, want 7 and 9", gs)
	}
	var pages int
	for _, name := range f.tools() {
		if name == "list_entities" {
			pages++
		}
	}
	if pages != 2 {
		t.Fatalf("list_entities called %d times, want 2 pages", pages)
	}
}

// cutStream serves f behind a proxy that ends the first SSE stream after the
// initialize reply, keeping its own connection to evolve open: the session stays
// alive on the server, which goes on accepting the session's requests, but no
// reply reaches the client. Later streams are relayed whole. Evolve advertises
// the proxy's URL, since mcp-go ignores an endpoint on another host.
func cutStream(t *testing.T, f *fakeEvolveServer) string {
	t.Helper()
	var backend *httptest.Server
	var mu sync.Mutex
	cut := false
	var held []*http.Response
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, _ := http.NewRequestWithContext(context.WithoutCancel(r.Context()), r.Method, backend.URL+r.URL.RequestURI(), r.Body)
		req.Header = r.Header.Clone()
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		if r.Method != http.MethodGet {
			_, _ = io.Copy(w, resp.Body)
			_ = resp.Body.Close()
			return
		}
		mu.Lock()
		first := !cut
		cut = true
		held = append(held, resp)
		mu.Unlock()
		br := bufio.NewReader(resp.Body)
		messages := 0
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			_, _ = w.Write([]byte(line))
			w.(http.Flusher).Flush()
			if strings.HasPrefix(line, "event: message") {
				messages++
			}
			// The blank line ending the initialize reply: end this stream here.
			if first && messages == 1 && strings.TrimRight(line, "\r\n") == "" {
				return
			}
		}
	}))
	backend = httptest.NewServer(server.NewSSEServer(f.mcpServer(), server.WithBaseURL(proxy.URL)))
	t.Cleanup(func() {
		mu.Lock()
		for _, resp := range held {
			_ = resp.Body.Close()
		}
		mu.Unlock()
		proxy.Close()
		backend.Close()
	})
	return proxy.URL + "/sse"
}

func TestMCPEvolve_ASessionWhoseStreamEndedIsReplaced(t *testing.T) {
	f := &fakeEvolveServer{}
	e := newMCPEvolve(cutStream(t, f), "cortex", "environment-grounded", time.Second)
	defer e.Close()
	// The first call's reply can't arrive: the stream ended after initialize. The
	// call fails when the plugin sees the stream end; it has no deadline.
	if _, err := e.Guidelines(context.Background(), "cortex", "t", "s"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	// Before, the session was kept: evolve accepted every later request on it, and
	// each call waited out its deadline for a reply.
	if _, err := e.Guidelines(context.Background(), "cortex", "t", "s"); err != nil {
		t.Fatalf("still on the dead session: %v", err)
	}
}

// failBodyOnce answers the first tool call's POST with a body that can't be
// read, after evolve has received it, so evolve's reply is on its way over the
// stream while mcp-go's SendRequest fails with its reply channel still
// registered.
type failBodyOnce struct {
	base http.RoundTripper
	mu   sync.Mutex
	done bool
}

type unreadable struct{}

func (unreadable) Read([]byte) (int, error) { return 0, errors.New("connection reset") }
func (unreadable) Close() error             { return nil }

func (f *failBodyOnce) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(strings.NewReader(string(body)))
	}
	resp, err := f.base.RoundTrip(req)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil && !f.done && req.Method == http.MethodPost && strings.Contains(string(body), `"tools/call"`) {
		f.done = true
		_ = resp.Body.Close()
		resp.Body = unreadable{}
	}
	return resp, err
}

func TestMCPEvolve_ASessionIsClosedOnlyAfterItsReaderStopped(t *testing.T) {
	f := &fakeEvolveServer{}
	e := newMCPEvolve(f.start(t), "cortex", "environment-grounded", time.Second)
	e.http.Transport = &failBodyOnce{base: e.http.Transport}
	// Each close reports whether the session's reader had stopped.
	closed := make(chan bool, 4)
	e.closeHook = func(cn *conn) {
		select {
		case <-cn.readerDone:
			closed <- true
		default:
			closed <- false
		}
	}

	// The unreadable body is a transport error, so the session is retired and
	// closed while evolve's reply is in flight on its stream.
	if _, err := e.Guidelines(context.Background(), "cortex", "t", "s"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want the unreadable body", err)
	}
	if _, err := e.Guidelines(context.Background(), "cortex", "t", "s"); err != nil {
		t.Fatalf("not reconnected: %v", err)
	}
	e.Close()
	for i := range 2 {
		if stopped := <-closed; !stopped {
			t.Fatalf("session %d was closed while its reader could still deliver a reply", i+1)
		}
	}
}

// lateEndpoint sends the endpoint event once release is closed and then never
// answers initialize: evolve accepts the request and sends no reply. arrived is
// closed once the stream's headers are sent.
func lateEndpoint(t *testing.T, release <-chan struct{}) (url string, arrived <-chan struct{}) {
	t.Helper()
	stop := make(chan struct{})
	came := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(came)
		select {
		case <-release:
		case <-stop:
			return
		}
		_, _ = w.Write([]byte("event: endpoint\ndata: /message?sessionId=s\n\n"))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	t.Cleanup(func() { close(stop); srv.Close() })
	return srv.URL + "/sse", came
}

func TestMCPEvolve_OneDeadlineCoversStartingAndInitializing(t *testing.T) {
	release := make(chan struct{})
	url, arrived := lateEndpoint(t, release)
	e := newMCPEvolve(url, "cortex", "environment-grounded", time.Second)
	e.connectBudget = time.Hour
	var mu sync.Mutex
	start := time.Now()
	now := start
	e.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	deadlines := make(chan time.Time, 1)
	e.initializing = func(ctx context.Context) {
		d, _ := ctx.Deadline()
		deadlines <- d
	}
	defer e.Close()
	ctx, cancel := context.WithCancel(context.Background())
	failed := make(chan error, 1)
	go func() {
		_, err := e.Guidelines(ctx, "cortex", "t", "s")
		failed <- err
	}()
	// The endpoint comes half the budget late.
	<-arrived
	mu.Lock()
	now = now.Add(30 * time.Minute)
	mu.Unlock()
	close(release)
	// Initialize ends where the connect does, an hour from its start. Before,
	// initialize got an hour of its own from the endpoint's arrival.
	if got, want := <-deadlines, start.Add(time.Hour); !got.Equal(want) {
		t.Fatalf("initialize's deadline is %v after the connect began, want %v", got.Sub(start), want.Sub(start))
	}
	// Initialize never answers; the call ends with its context.
	cancel()
	if err := <-failed; !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want the connect to fail", err)
	}
}

func TestMCPEvolve_LearningSetsUpTheSessionItSendsOn(t *testing.T) {
	sw := &swappable{}
	ts := httptest.NewServer(sw)
	t.Cleanup(ts.Close)
	first, second := &fakeEvolveServer{}, &fakeEvolveServer{}
	sw.set(server.NewSSEServer(first.mcpServer(), server.WithBaseURL(ts.URL)))
	e := newMCPEvolve(ts.URL+"/sse", "cortex", "environment-grounded", time.Second)
	defer e.Close()
	swapped := false
	e.afterSetup = func() {
		if swapped {
			return
		}
		swapped = true
		// Evolve restarts just after setup, and a concurrent call retires the session.
		sw.set(server.NewSSEServer(second.mcpServer(), server.WithBaseURL(ts.URL)))
		e.mu.Lock()
		if e.client != nil {
			e.retire()
		}
		e.mu.Unlock()
	}
	tr := Trajectory{Messages: []map[string]any{{"role": "user", "content": "x"}}}
	_ = e.Learn(context.Background(), "cortex", tr) // sent on the session that was set up, which evolve no longer knows
	if err := e.Learn(context.Background(), "cortex", tr); err != nil {
		t.Fatal(err)
	}
	// The new evolve is set up before it is sent anything to learn. Before, the
	// setup was recorded for the plugin as a whole, so the batch went to the new
	// evolve unprepared; one without the profile would fail it every time.
	got := second.tools()
	if len(got) < 3 || got[0] != "ensure_processing_profile" || got[1] != "list_entities" || got[2] != "process_trajectory" {
		t.Fatalf("new evolve got %v, want setup before process_trajectory", got)
	}
}

func TestMCPEvolve_AListingThatDoesNotFinishIsAnError(t *testing.T) {
	for _, cursor := range []string{"same", "advancing"} {
		t.Run(cursor, func(t *testing.T) {
			f := &fakeEvolveServer{old: true, cursor: cursor}
			e := newMCPEvolve(f.start(t), "cortex", "environment-grounded", 5*time.Second)
			defer e.Close()
			// 9 is never listed. Before, the fallback returned 7 alone as a
			// successful fetch, and the session cached it.
			gs, err := e.Guidelines(context.Background(), "cortex", "t", "s")
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("guidelines = %+v, err = %v; want ErrUnavailable", gs, err)
			}
			if want := map[string]int{"same": 2, "advancing": maxListPages}[cursor]; f.pages != want {
				t.Fatalf("read %d pages, want %d", f.pages, want)
			}
		})
	}
}

func TestPreview_StopsAtARuneBoundary(t *testing.T) {
	if got := preview(strings.Repeat("é", 10), 7); got != "ééé…" || !utf8.ValidString(got) {
		t.Fatalf("preview = %q", got)
	}
}
