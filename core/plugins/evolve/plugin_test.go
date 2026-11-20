package evolve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/rossoctl/cortex/core/pipeline"
)

const claudeUA = "claude-cli/2.1.287 (external, cli)"

type fetch struct{ namespace, task, sessionID string }

type learned struct {
	namespace string
	t         Trajectory
}

type fakeEvolve struct {
	mu       sync.Mutex
	gs       []Guideline
	err      error
	calls    []fetch
	learned  []learned
	learnErr error
	// hold, when set, keeps fetches for holdSession from answering until it is
	// closed or the fetch's context ends.
	hold        chan struct{}
	holdSession string
}

func (f *fakeEvolve) Guidelines(ctx context.Context, namespace, task, sessionID string) ([]Guideline, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fetch{namespace, task, sessionID})
	hold := f.hold
	if sessionID != f.holdSession {
		hold = nil
	}
	f.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gs, f.err
}

func (f *fakeEvolve) fetches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeEvolve) Learn(_ context.Context, namespace string, t Trajectory) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.learnErr != nil {
		return f.learnErr
	}
	f.learned = append(f.learned, learned{namespace, t})
	return nil
}

func rev(n int) *int { return &n }

var twoGuidelines = []Guideline{
	{ID: "g1", Content: "Run the test suite with uv run pytest -v.", Revision: rev(1), Source: "cortex:s:9"},
	{ID: "g2", Content: "Check git status before committing.", Revision: rev(2)},
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func configured(t *testing.T, cfgJSON string, fe *fakeEvolve) (*Plugin, *clock) {
	t.Helper()
	p := New()
	p.client = fe
	c := &clock{t: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
	p.now = c.now
	if cfgJSON == "" {
		cfgJSON = `{"endpoint":"http://evolve.invalid/sse"}`
	}
	if err := p.Configure(json.RawMessage(cfgJSON)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	return p, c
}

const anthropicBody = `{"model":"claude-opus-5","system":[{"type":"text","text":"You are Claude Code."},{"type":"text","text":"env","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"fix the test"}],"tools":[{"name":"Bash"}],"max_tokens":1024}`

func reqCtx(path, body string) *pipeline.Context {
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound, Method: "POST", Path: path, Body: []byte(body),
		Headers: http.Header{"User-Agent": []string{claudeUA}},
		Session: &pipeline.SessionView{ID: "s1"},
	}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{
		AgentRole: pipeline.AgentRoleMain,
		Messages: []pipeline.InferenceMessage{
			{Role: "system", Content: "You are Claude Code."},
			{Role: "user", Content: "<system-reminder>\nrules\n</system-reminder>\nfix the test"},
		},
		Tools: []pipeline.InferenceTool{{Name: "Bash"}},
	}
	return pctx
}

func run(t *testing.T, p *Plugin, pctx *pipeline.Context, policies ...pipeline.ErrorPolicy) {
	t.Helper()
	var opts []pipeline.Option
	if len(policies) > 0 {
		opts = append(opts, pipeline.WithPolicies(policies...))
	}
	pipe, err := pipeline.New([]pipeline.Plugin{p}, opts...)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	if act := pipe.Run(context.Background(), pctx); act.Type != pipeline.Continue {
		t.Fatalf("action = %v, want Continue — evolve must never block a request", act.Type)
	}
}

func skipReason(pctx *pipeline.Context) string {
	if pctx.Extensions.Invocations == nil {
		return ""
	}
	for _, inv := range pctx.Extensions.Invocations.Outbound {
		if inv.Action == pipeline.ActionSkip {
			return inv.Reason
		}
	}
	return ""
}

func published(t *testing.T, pctx *pipeline.Context) event {
	t.Helper()
	ev, ok := pctx.Extensions.Custom[name+pipeline.PluginEventSuffix].(event)
	if !ok {
		t.Fatalf("no %s event published", name)
	}
	return ev
}

// --- Configure ---

func TestConfigure_Validation(t *testing.T) {
	for _, raw := range []string{
		`{}`,
		`{"endpoint":"evolve:8202"}`,
		`{"endpoint":"http://x","bogus":1}`,
		`{"endpoint":"http://x","top_k":99}`,
		`{"endpoint":"http://x","min_turns":-1}`,
		`{"endpoint":"http://x","placement":"tools"}`,
		`{"endpoint":"http://x","guidelines_prompt":"../default"}`,
		`{"endpoint":"http://x","guidelines_prompt":" environment-grounded"}`,
	} {
		if err := New().Configure(json.RawMessage(raw)); err == nil {
			t.Errorf("Configure(%s) succeeded, want error", raw)
		}
	}
}

func TestConfigure_OneNamespaceServesWhatIsLearned(t *testing.T) {
	// The same namespace for both skips the review: evolve's learned guidelines
	// are served to the next session.
	p := New()
	if err := p.Configure(json.RawMessage(`{"endpoint":"http://x","serve_namespace":"cortex","learn_namespace":"cortex"}`)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if p.cfg.ServeNamespace != "cortex" || p.cfg.LearnNamespace != "cortex" {
		t.Fatalf("namespaces = %q, %q", p.cfg.ServeNamespace, p.cfg.LearnNamespace)
	}
}

func TestConfigure_Defaults(t *testing.T) {
	p, _ := configured(t, "", &fakeEvolve{})
	if !*p.cfg.Inject || !*p.cfg.Learn || p.cfg.TopK != 8 || p.cfg.TimeoutMs != 3000 || p.cfg.CacheTTL != 1800 || p.cfg.IdleAfter != 900 ||
		p.cfg.ServeNamespace != "cortex" || p.cfg.LearnNamespace != "cortex-candidates" || p.cfg.Profile != "cortex" || p.cfg.Placement != "system" {
		t.Fatalf("defaults not applied: %+v", p.cfg)
	}
}

func TestConfigure_GuidelinesPrompt(t *testing.T) {
	// Unset: environment-grounded, the prompt for coding agents. Empty: evolve's own default.
	p, _ := configured(t, "", &fakeEvolve{})
	if *p.cfg.Prompt != "environment-grounded" {
		t.Fatalf("default guidelines_prompt = %q", *p.cfg.Prompt)
	}
	for raw, want := range map[string]string{
		`{"endpoint":"http://x","guidelines_prompt":""}`:           "",
		`{"endpoint":"http://x","guidelines_prompt":"default"}`:    "default",
		`{"endpoint":"http://x","guidelines_prompt":"My_rules-2"}`: "My_rules-2",
	} {
		q := New()
		if err := q.Configure(json.RawMessage(raw)); err != nil {
			t.Fatalf("Configure(%s): %v", raw, err)
		}
		if *q.cfg.Prompt != want {
			t.Errorf("Configure(%s): guidelines_prompt = %q, want %q", raw, *q.cfg.Prompt, want)
		}
	}
}

func TestCapabilities(t *testing.T) {
	caps := New().Capabilities()
	if !caps.WritesRequestBody || caps.WritesResponseBody || len(caps.RequiresAny) != 1 || caps.RequiresAny[0] != "inference-parser" {
		t.Fatalf("capabilities = %+v", caps)
	}
	if len(caps.Description) > 80 {
		t.Errorf("description is %d chars, want <= 80", len(caps.Description))
	}
}

// --- Inject ---

func TestInject_AnthropicArraySystemAppendsTrailingBlock(t *testing.T) {
	fe := &fakeEvolve{gs: twoGuidelines}
	p, _ := configured(t, "", fe)
	pctx := reqCtx("/v1/messages?beta=true", anthropicBody)
	run(t, p, pctx)

	if !pctx.BodyMutated() {
		t.Fatal("expected the body to be rewritten")
	}
	system := gjson.GetBytes(pctx.Body, "system").Array()
	if len(system) != 3 || system[1].Get("cache_control.type").String() != "ephemeral" {
		t.Fatalf("existing system blocks not preserved: %s", gjson.GetBytes(pctx.Body, "system").Raw)
	}
	text := system[2].Get("text").String()
	if !strings.HasPrefix(text, `<evolve-guidelines source="altk-evolve" ids="g1,g2">`) || !strings.Contains(text, "1. Run the test suite") {
		t.Fatalf("block = %q", text)
	}
	if got := fe.calls[0]; got != (fetch{"cortex", "fix the test", "s1"}) {
		t.Fatalf("request = %+v", got)
	}
	ev := published(t, pctx)
	if !ev.Injected || ev.Cached || strings.Join(ev.IDs, ",") != "g1,g2" || *ev.Revisions[1] != 2 || ev.Sources[0] != "cortex:s:9" {
		t.Fatalf("event = %+v", ev)
	}
}

func TestInject_Dialects(t *testing.T) {
	cases := []struct {
		name, path, body, field string
	}{
		{"anthropic string system", "/v1/messages", `{"system":"base","messages":[]}`, "system"},
		{"anthropic no system", "/v1/messages", `{"messages":[]}`, "system"},
		{"chat system message", "/v1/chat/completions", `{"messages":[{"role":"system","content":"base"},{"role":"user","content":"hi"}]}`, "messages.0.content"},
		{"chat no system message", "/v1/chat/completions", `{"messages":[{"role":"user","content":"hi"}]}`, "messages.0.content"},
		{"responses", "/v1/responses", `{"instructions":"base","input":[]}`, "instructions"},
		{"responses no instructions", "/v1/responses", `{"input":[]}`, "instructions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := configured(t, "", &fakeEvolve{gs: twoGuidelines})
			pctx := reqCtx(tc.path, tc.body)
			run(t, p, pctx)
			got := gjson.GetBytes(pctx.Body, tc.field).String()
			if !strings.Contains(got, "<evolve-guidelines") || !strings.HasSuffix(got, "</evolve-guidelines>") {
				t.Fatalf("%s = %q", tc.field, got)
			}
			if strings.Contains(tc.body, `"base"`) && !strings.HasPrefix(got, "base\n\n") {
				t.Errorf("existing instructions not kept first: %q", got)
			}
			if tc.name == "chat no system message" && gjson.GetBytes(pctx.Body, "messages.1.content").String() != "hi" {
				t.Errorf("user message not preserved: %s", pctx.Body)
			}
		})
	}
}

func TestInject_UserPlacementLeadsFirstUserMessage(t *testing.T) {
	const reminder = "<system-reminder>\n<evolve-guidelines"
	cases := []struct {
		name, path, body, field string
	}{
		{"anthropic string content", "/v1/messages", `{"system":"base","messages":[{"role":"user","content":"fix the test"},{"role":"assistant","content":"ok"},{"role":"user","content":"later"}]}`, "messages.0.content"},
		{"anthropic block content", "/v1/messages", `{"messages":[{"role":"user","content":[{"type":"text","text":"fix the test","cache_control":{"type":"ephemeral"}}]}]}`, "messages.0.content.0.text"},
		{"chat after system message", "/v1/chat/completions", `{"messages":[{"role":"system","content":"base"},{"role":"user","content":"fix the test"}]}`, "messages.1.content"},
		{"responses input", "/v1/responses", `{"instructions":"base","input":[{"role":"user","content":[{"type":"input_text","text":"fix the test"}]}]}`, "input.0.content.0.text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := configured(t, `{"endpoint":"http://x","placement":"user"}`, &fakeEvolve{gs: twoGuidelines})
			pctx := reqCtx(tc.path, tc.body)
			run(t, p, pctx)
			got := gjson.GetBytes(pctx.Body, tc.field).String()
			if !strings.HasPrefix(got, reminder) || !strings.Contains(got, "</evolve-guidelines>\n</system-reminder>") {
				t.Fatalf("%s = %q", tc.field, got)
			}
			if strings.Contains(string(pctx.Body), `"base\n\n<evolve`) || gjson.GetBytes(pctx.Body, "messages.2.content").String() == got {
				t.Fatalf("block placed outside the first user message: %s", pctx.Body)
			}
			if strings.Contains(tc.body, "cache_control") && gjson.GetBytes(pctx.Body, "messages.0.content.1.cache_control.type").String() != "ephemeral" {
				t.Fatalf("existing user blocks not preserved: %s", pctx.Body)
			}
			if !strings.Contains(string(pctx.Body), "fix the test") || !published(t, pctx).Injected {
				t.Fatalf("task text or event lost: %s", pctx.Body)
			}
		})
	}
}

func TestInject_UserPlacementWithoutUserMessageFailsOpen(t *testing.T) {
	p, _ := configured(t, `{"endpoint":"http://x","placement":"user"}`, &fakeEvolve{gs: twoGuidelines})
	body := `{"messages":[{"role":"assistant","content":"hi"}]}`
	pctx := reqCtx("/v1/messages", body)
	run(t, p, pctx)
	if pctx.BodyMutated() || skipReason(pctx) != "unsupported_request_shape" {
		t.Fatalf("mutated=%v skip=%q", pctx.BodyMutated(), skipReason(pctx))
	}
}

func TestInject_OneFetchPerSessionAndStableBytes(t *testing.T) {
	fe := &fakeEvolve{gs: twoGuidelines}
	p, _ := configured(t, "", fe)
	first := reqCtx("/v1/messages", anthropicBody)
	run(t, p, first)
	second := reqCtx("/v1/messages", anthropicBody)
	second.Extensions.Inference.Messages = append(second.Extensions.Inference.Messages, pipeline.InferenceMessage{Role: "user", Content: "now something else"})
	run(t, p, second)

	if len(fe.calls) != 1 {
		t.Fatalf("fetches = %d, want 1", len(fe.calls))
	}
	if string(first.Body) != string(second.Body) {
		t.Fatal("injected bytes differ between turns of one session")
	}
	if !published(t, second).Cached {
		t.Error("second request not marked cached")
	}
}

func TestInject_RefetchAfterTTL(t *testing.T) {
	fe := &fakeEvolve{gs: twoGuidelines}
	p, c := configured(t, `{"endpoint":"http://x","cache_ttl_seconds":60}`, fe)
	run(t, p, reqCtx("/v1/messages", anthropicBody))
	fe.gs = twoGuidelines[:1] // g2 was retired
	c.t = c.t.Add(61 * time.Second)
	pctx := reqCtx("/v1/messages", anthropicBody)
	run(t, p, pctx)
	if len(fe.calls) != 2 || strings.Join(published(t, pctx).IDs, ",") != "g1" {
		t.Fatalf("calls=%d event=%+v", len(fe.calls), published(t, pctx))
	}
}

func TestInject_FailsOpen(t *testing.T) {
	fe := &fakeEvolve{err: ErrUnavailable}
	p, c := configured(t, "", fe)
	pctx := reqCtx("/v1/messages", anthropicBody)
	run(t, p, pctx)
	if pctx.BodyMutated() || skipReason(pctx) != "evolve_unavailable" || published(t, pctx).Error != "evolve_unavailable" {
		t.Fatalf("mutated=%v skip=%q", pctx.BodyMutated(), skipReason(pctx))
	}
	run(t, p, reqCtx("/v1/messages", anthropicBody))
	if len(fe.calls) != 1 {
		t.Fatalf("retried before retry_after_seconds: %d calls", len(fe.calls))
	}
	fe.err, fe.gs = nil, twoGuidelines
	c.t = c.t.Add(61 * time.Second)
	pctx = reqCtx("/v1/messages", anthropicBody)
	run(t, p, pctx)
	if !pctx.BodyMutated() {
		t.Fatal("did not recover after retry_after_seconds")
	}
}

func TestInject_KeepsGuidelinesThroughTransientFailure(t *testing.T) {
	fe := &fakeEvolve{gs: twoGuidelines}
	p, c := configured(t, `{"endpoint":"http://x","cache_ttl_seconds":60}`, fe)
	first := reqCtx("/v1/messages", anthropicBody)
	run(t, p, first)
	fe.err = errors.New("down")
	c.t = c.t.Add(61 * time.Second)
	second := reqCtx("/v1/messages", anthropicBody)
	run(t, p, second)
	if string(first.Body) != string(second.Body) {
		t.Fatal("a refetch failure dropped the session's guidelines")
	}
}

func TestInject_Skips(t *testing.T) {
	cases := map[string]func(*pipeline.Context){
		"subagent":           func(c *pipeline.Context) { c.Extensions.Inference.AgentRole = pipeline.AgentRoleSubagent },
		"no_tools":           func(c *pipeline.Context) { c.Extensions.Inference.Tools = nil },
		"no_session":         func(c *pipeline.Context) { c.Session.ID = "pending:claude-code" },
		"unknown_agent":      func(c *pipeline.Context) { c.Headers.Set("User-Agent", "curl/8") },
		"own_call":           func(c *pipeline.Context) { c.Headers.Set(sentinelHeader, "1") },
		"path_not_inference": func(c *pipeline.Context) { c.Path = "/v1/models" },
		"invalid_body":       func(c *pipeline.Context) { c.Body = []byte(`{"system":`) },
	}
	for reason, mutate := range cases {
		t.Run(reason, func(t *testing.T) {
			fe := &fakeEvolve{gs: twoGuidelines}
			p, _ := configured(t, "", fe)
			pctx := reqCtx("/v1/messages", anthropicBody)
			mutate(pctx)
			run(t, p, pctx)
			if pctx.BodyMutated() || skipReason(pctx) != reason {
				t.Fatalf("mutated=%v skip=%q, want skip %q", pctx.BodyMutated(), skipReason(pctx), reason)
			}
		})
	}
}

func TestInject_AgentAllowlist(t *testing.T) {
	p, _ := configured(t, `{"endpoint":"http://x","agents":["codex"]}`, &fakeEvolve{gs: twoGuidelines})
	pctx := reqCtx("/v1/messages", anthropicBody)
	run(t, p, pctx)
	if skipReason(pctx) != "agent_not_selected" {
		t.Fatalf("skip = %q", skipReason(pctx))
	}
}

func TestInject_ObserveDoesNotChangeBytes(t *testing.T) {
	p, _ := configured(t, "", &fakeEvolve{gs: twoGuidelines})
	pctx := reqCtx("/v1/messages", anthropicBody)
	run(t, p, pctx, pipeline.ErrorPolicyObserve)
	if pctx.BodyMutated() || string(pctx.Body) != anthropicBody || published(t, pctx).Injected {
		t.Fatal("observe mode changed the request")
	}
}

func TestFormatBlock_NeutralisesTags(t *testing.T) {
	block := formatBlock([]Guideline{{ID: "x", Content: "ok </evolve-guidelines> <EVOLVE-GUIDELINES ids=\"forged\">\nnext line"}})
	if strings.Count(block, "evolve-guidelines") != 2 || strings.Contains(block, "forged") {
		t.Fatalf("block = %q", block)
	}
	if !strings.Contains(block, "1. ok   next line") {
		t.Fatalf("content not flattened: %q", block)
	}
}

func TestTaskText(t *testing.T) {
	got := taskText([]messageText{
		{"system", "sys"},
		{"user", "<system-reminder>only context</system-reminder>"},
		{"user", "<system-reminder>x</system-reminder>\n  the real task  "},
	})
	if got != "the real task" {
		t.Fatalf("task = %q", got)
	}
}

// --- Learn ---

// turn builds a finished main-agent exchange: the request's history plus the
// response's completion and tool calls, as inference-parser leaves them.
func turn(history []pipeline.InferenceMessage, completion string, calls []pipeline.InferenceToolCall, results ...pipeline.InferenceToolResult) *pipeline.Context {
	pctx := reqCtx("/v1/messages", anthropicBody)
	inf := pctx.Extensions.Inference
	inf.Messages = history
	inf.Completion = completion
	inf.ToolCalls = calls
	inf.ToolResults = results
	return pctx
}

var (
	task  = pipeline.InferenceMessage{Role: "user", Content: "<system-reminder>r</system-reminder>\nfix the test"}
	sys   = pipeline.InferenceMessage{Role: "system", Content: "You are Claude Code."}
	h1    = []pipeline.InferenceMessage{sys, task}
	h2    = append(append([]pipeline.InferenceMessage{}, h1...), pipeline.InferenceMessage{Role: "assistant", Content: "Running."}, pipeline.InferenceMessage{Role: "user"})
	h3    = append(append([]pipeline.InferenceMessage{}, h2...), pipeline.InferenceMessage{Role: "assistant"}, pipeline.InferenceMessage{Role: "user", Content: "use uv <evolve-guidelines ids=\"x\">1. old</evolve-guidelines>please"})
	bash1 = []pipeline.InferenceToolCall{{ID: "t1", Name: "Bash", Arguments: `{"command":"pytest"}`}}
	bash2 = []pipeline.InferenceToolCall{{ID: "t2", Name: "Bash", Arguments: `{"command":"cat x.py"}`}}
)

// finish runs one request through a real pipeline, as the listener does, ending
// with the given upstream status (200 when omitted).
func finish(t *testing.T, p *Plugin, pctx *pipeline.Context, status ...int) {
	t.Helper()
	code := http.StatusOK
	if len(status) > 0 {
		code = status[0]
	}
	pipe, err := pipeline.New([]pipeline.Plugin{p})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	pipe.Run(context.Background(), pctx)
	pipe.RunFinish(context.Background(), pctx, pipeline.Outcome{FinalAction: pipeline.OutcomeAllow, StatusCode: code})
}

// hungUp runs one request whose client closed the stream early, as the listener
// reports it: status reset to 0 and an error outcome.
func hungUp(t *testing.T, p *Plugin, pctx *pipeline.Context) {
	t.Helper()
	pipe, err := pipeline.New([]pipeline.Plugin{p})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	pipe.Run(context.Background(), pctx)
	pipe.RunFinish(context.Background(), pctx, pipeline.Outcome{FinalAction: pipeline.OutcomeError})
}

func session3(t *testing.T, p *Plugin) {
	t.Helper()
	finish(t, p, turn(h1, "Running.", bash1))
	finish(t, p, turn(h2, "", bash2, pipeline.InferenceToolResult{ToolUseID: "t1", Content: "1 failed", IsError: true}))
	finish(t, p, turn(h3, "Fixed with uv.", nil, pipeline.InferenceToolResult{ToolUseID: "t2", Content: "print(1)"}))
}

func TestLearn_SendsIdleSessionAsOpenAITrajectory(t *testing.T) {
	fe := &fakeEvolve{}
	p, c := configured(t, `{"endpoint":"http://x","idle_after_seconds":60}`, fe)
	session3(t, p)

	p.learnIdle(context.Background())
	if len(fe.learned) != 0 {
		t.Fatal("learned from a session that is not idle yet")
	}
	c.t = c.t.Add(61 * time.Second)
	p.learnIdle(context.Background())
	p.learnIdle(context.Background())
	if len(fe.learned) != 1 {
		t.Fatalf("learn calls = %d, want 1", len(fe.learned))
	}
	got := fe.learned[0]
	if got.namespace != "cortex-candidates" {
		t.Errorf("namespace = %q, want the learn namespace", got.namespace)
	}
	tr := got.t
	var roles []string
	for _, m := range tr.Messages {
		roles = append(roles, m["role"].(string))
	}
	if strings.Join(roles, ",") != "user,assistant,tool,assistant,tool,user,assistant" {
		t.Fatalf("roles = %v", roles)
	}
	if tr.Messages[0]["content"] != "fix the test" || tr.Messages[2]["content"] != "[error] 1 failed" || tr.Messages[5]["content"] != "use uv please" {
		t.Fatalf("messages = %v", tr.Messages)
	}
	if fn := tr.Messages[1]["tool_calls"].([]map[string]any)[0]["function"].(map[string]any); fn["name"] != "Bash" || fn["arguments"] != `{"command":"pytest"}` {
		t.Fatalf("tool call = %v", fn)
	}
	if tr.Batch == nil || tr.Batch.Source != "cortex" || tr.Batch.ConversationID != "s1" || !strings.HasSuffix(tr.Batch.BatchID, "-1") {
		t.Fatalf("batch = %+v", tr.Batch)
	}
	if tr.Metadata["agent_id"] != "claude-code" || tr.Metadata["creation_mode"] != "auto-cortex" || tr.Metadata["session_id"] != "s1" {
		t.Fatalf("metadata = %v", tr.Metadata)
	}
	if b, err := json.Marshal(tr); err != nil || !gjson.ValidBytes(b) {
		t.Fatalf("trajectory does not marshal: %v", err)
	}
}

func TestLearn_ResumedSessionSendsOnlyNewTurnsWithTaskContext(t *testing.T) {
	fe := &fakeEvolve{}
	p, c := configured(t, `{"endpoint":"http://x","idle_after_seconds":60,"min_turns":1}`, fe)
	finish(t, p, turn(h1, "Running.", bash1))
	c.t = c.t.Add(61 * time.Second)
	p.learnIdle(context.Background())

	finish(t, p, turn(h2, "Done.", nil, pipeline.InferenceToolResult{ToolUseID: "t1", Content: "ok"}))
	c.t = c.t.Add(61 * time.Second)
	p.learnIdle(context.Background())

	if len(fe.learned) != 2 {
		t.Fatalf("learn calls = %d", len(fe.learned))
	}
	second := fe.learned[1].t
	if second.Messages[0]["role"] != "tool" || second.Batch.BatchID == fe.learned[0].t.Batch.BatchID {
		t.Fatalf("second batch = %+v", second)
	}
	if len(second.ContextMessages) != 1 || second.ContextMessages[0]["content"] != "fix the test" {
		t.Fatalf("context = %v", second.ContextMessages)
	}
}

func TestLearn_RetriesWithSameBatchID(t *testing.T) {
	fe := &fakeEvolve{learnErr: ErrUnavailable}
	p, c := configured(t, `{"endpoint":"http://x","idle_after_seconds":60}`, fe)
	session3(t, p)
	c.t = c.t.Add(61 * time.Second)
	p.learnIdle(context.Background())
	fe.learnErr = nil
	p.learnIdle(context.Background())
	if len(fe.learned) != 1 || !strings.HasSuffix(fe.learned[0].t.Batch.BatchID, "-1") || len(fe.learned[0].t.Messages) != 7 {
		t.Fatalf("learned = %+v", fe.learned)
	}
}

func TestLearn_SkipsTooFewTurnsAndIneligibleTraffic(t *testing.T) {
	fe := &fakeEvolve{}
	p, c := configured(t, `{"endpoint":"http://x","idle_after_seconds":60}`, fe)
	finish(t, p, turn(h1, "Running.", bash1)) // one turn < min_turns
	sub := turn(h1, "x", nil)
	sub.Extensions.Inference.AgentRole = pipeline.AgentRoleSubagent
	sub.Session.ID = "s2"
	finish(t, p, sub)
	failed := turn(h1, "x", nil)
	failed.Session.ID = "s3"
	finish(t, p, failed, 529)
	c.t = c.t.Add(61 * time.Second)
	p.learnIdle(context.Background())
	if len(fe.learned) != 0 {
		t.Fatalf("learned = %+v", fe.learned)
	}
}

// Codex closes each stream as soon as it has read response.completed, so the
// listener reports most of its turns as client_canceled with status 0. A turn
// that finished before the hang-up is learned from; one cut off before the end
// is not.
func TestLearn_KeepsTurnsTheClientHungUpOnAfterTheEnd(t *testing.T) {
	fe := &fakeEvolve{}
	p, c := configured(t, `{"endpoint":"http://x","idle_after_seconds":60}`, fe)
	first := turn(h1, "Running.", bash1)
	first.Extensions.Inference.FinishReason = "completed"
	hungUp(t, p, first)
	second := turn(h2, "Done.", nil, pipeline.InferenceToolResult{ToolUseID: "t1", Content: "ok"})
	second.Extensions.Inference.FinishReason = "completed"
	hungUp(t, p, second)
	c.t = c.t.Add(61 * time.Second)
	p.learnIdle(context.Background())
	if len(fe.learned) != 1 {
		t.Fatalf("learned = %+v, want the session", fe.learned)
	}
	var roles []string
	for _, m := range fe.learned[0].t.Messages {
		roles = append(roles, m["role"].(string))
	}
	if got := strings.Join(roles, ","); got != "user,assistant,tool,assistant" {
		t.Errorf("roles = %s, want both turns and the tool result", got)
	}
}

func TestLearn_SkipsTurnsCutOffOrDenied(t *testing.T) {
	cases := map[string]func(*pipeline.Context) pipeline.Outcome{
		"no finish reason": func(*pipeline.Context) pipeline.Outcome {
			return pipeline.Outcome{FinalAction: pipeline.OutcomeError}
		},
		"failed": func(pctx *pipeline.Context) pipeline.Outcome {
			pctx.Extensions.Inference.FinishReason = "failed"
			return pipeline.Outcome{FinalAction: pipeline.OutcomeError}
		},
		"denied": func(pctx *pipeline.Context) pipeline.Outcome {
			pctx.Extensions.Inference.FinishReason = "completed"
			return pipeline.Outcome{FinalAction: pipeline.OutcomeDeny, DenyingPlugin: "ibac"}
		},
		"upstream error": func(pctx *pipeline.Context) pipeline.Outcome {
			pctx.Extensions.Inference.FinishReason = "completed"
			return pipeline.Outcome{FinalAction: pipeline.OutcomeError, StatusCode: 502}
		},
	}
	for name, outcome := range cases {
		t.Run(name, func(t *testing.T) {
			fe := &fakeEvolve{}
			p, c := configured(t, `{"endpoint":"http://x","idle_after_seconds":60}`, fe)
			for _, pctx := range []*pipeline.Context{
				turn(h1, "Running.", bash1),
				turn(h2, "Done.", nil, pipeline.InferenceToolResult{ToolUseID: "t1", Content: "ok"}),
			} {
				pipe, err := pipeline.New([]pipeline.Plugin{p})
				if err != nil {
					t.Fatal(err)
				}
				pipe.Run(context.Background(), pctx)
				pipe.RunFinish(context.Background(), pctx, outcome(pctx))
			}
			c.t = c.t.Add(61 * time.Second)
			p.learnIdle(context.Background())
			if len(fe.learned) != 0 {
				t.Fatalf("learned = %+v, want nothing", fe.learned)
			}
		})
	}
}

func TestInitShutdown(t *testing.T) {
	p, _ := configured(t, "", &fakeEvolve{})
	p.tick = time.Millisecond
	if err := p.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
	default:
		t.Fatal("notifier still running after Shutdown")
	}
}

// --- A session's fetch in progress ---

func userTask(text string) []pipeline.InferenceMessage {
	return []pipeline.InferenceMessage{{Role: "user", Content: text}}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
	}
}

func TestFetch_OutlivesTheRequestThatStartedIt(t *testing.T) {
	fe := &fakeEvolve{gs: twoGuidelines, hold: make(chan struct{}), holdSession: "s1"}
	p, _ := configured(t, `{"endpoint":"http://evolve.invalid/sse","timeout_ms":5000}`, fe)
	first, cancel := context.WithCancel(context.Background())
	started := make(chan *cached, 1)
	go func() {
		entry, _ := p.guidelinesFor(first, "s1", userTask("fix the test"))
		started <- entry
	}()
	waitFor(t, func() bool { return fe.fetches() == 1 })
	// The client that started the fetch hangs up. Before, that cancelled the fetch,
	// and the failure was cached for the whole session for retry_after_seconds.
	cancel()
	waiting := make(chan *cached, 1)
	go func() {
		entry, _ := p.guidelinesFor(context.Background(), "s1", userTask("fix the test"))
		waiting <- entry
	}()
	close(fe.hold)
	for _, entry := range []*cached{<-started, <-waiting} {
		if len(entry.guidelines) != 2 || entry.failed {
			t.Fatalf("entry = %+v, want the two guidelines", entry)
		}
	}
	if n := fe.fetches(); n != 1 {
		t.Fatalf("evolve called %d times, want one fetch shared by both requests", n)
	}
}

func TestFetch_AWaitingRequestStopsAtItsOwnCancellation(t *testing.T) {
	fe := &fakeEvolve{gs: twoGuidelines, hold: make(chan struct{}), holdSession: "s1"}
	p, _ := configured(t, `{"endpoint":"http://evolve.invalid/sse","timeout_ms":5000}`, fe)
	defer close(fe.hold)
	go p.guidelinesFor(context.Background(), "s1", userTask("fix the test"))
	waitFor(t, func() bool { return fe.fetches() == 1 })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	entry, _ := p.guidelinesFor(ctx, "s1", userTask("fix the test"))
	if took := time.Since(start); took > time.Second {
		t.Fatalf("a cancelled request waited %v for the fetch in progress", took)
	}
	if len(entry.guidelines) != 0 {
		t.Fatalf("entry = %+v, want none: the fetch hasn't answered", entry)
	}
}

func TestFetch_EvictingASessionLeavesItsFetchShared(t *testing.T) {
	fe := &fakeEvolve{gs: twoGuidelines}
	p, clk := configured(t, `{"endpoint":"http://evolve.invalid/sse","timeout_ms":5000,"max_sessions":1,"cache_ttl_seconds":60}`, fe)
	p.guidelinesFor(context.Background(), "a", userTask("task a"))
	clk.t = clk.t.Add(2 * time.Minute) // a's guidelines expire: the next request refreshes them
	fe.mu.Lock()
	fe.hold, fe.holdSession = make(chan struct{}), "a"
	fe.mu.Unlock()

	done := make(chan struct{}, 2)
	go func() { p.guidelinesFor(context.Background(), "a", userTask("task a")); done <- struct{}{} }()
	waitFor(t, func() bool { return fe.fetches() == 2 })
	// b's answer evicts a from the cache while a's refresh is still in progress.
	p.guidelinesFor(context.Background(), "b", userTask("task b"))
	go func() { p.guidelinesFor(context.Background(), "a", userTask("task a")); done <- struct{}{} }()
	time.Sleep(50 * time.Millisecond)
	close(fe.hold)
	<-done
	<-done
	// a's fetch, a's refresh and b's fetch. Before, eviction also dropped a's
	// in-progress marker, so the second request for a started another refresh.
	if n := fe.fetches(); n != 3 {
		t.Fatalf("evolve called %d times, want 3", n)
	}
}

// --- Codex's harness context ---

// codexContext is the first user item Codex 0.169 sends in a repo with an
// AGENTS.md, as the parser joins its two parts.
const codexContext = "# AGENTS.md instructions for /repo\n\n<INSTRUCTIONS>\n# Project rules\n\nRun `make test` before committing.\n\n</INSTRUCTIONS>\n" +
	"<environment_context>\n  <cwd>/repo</cwd>\n  <shell>zsh</shell>\n  <current_date>2026-11-20</current_date>\n</environment_context>"

func codexMessages(prompt string) []messageText {
	return []messageText{
		{"developer", "You are Codex."},
		{"user", codexContext},
		{"user", prompt},
	}
}

func TestTaskText_IsCodexsPromptNotItsContext(t *testing.T) {
	if got := taskText(codexMessages("Bump the version and commit the fix for the login bug")); got != "Bump the version and commit the fix for the login bug" {
		t.Fatalf("task = %q", got)
	}
	// Without an AGENTS.md, Codex sends the environment context alone.
	env := []messageText{{"user", "<environment_context>\n  <cwd>/repo</cwd>\n</environment_context>"}, {"user", "Fix the login bug"}}
	if got := taskText(env); got != "Fix the login bug" {
		t.Fatalf("task = %q", got)
	}
	// Text that merely mentions a tag mid-line is the user's.
	if got := clean("Explain what <skill> tags do in Codex"); got != "Explain what <skill> tags do in Codex" {
		t.Fatalf("clean = %q", got)
	}
}

func TestLearn_CodexsContextIsNotLearned(t *testing.T) {
	r := newRecorder("codex", 1)
	var msgs []pipeline.InferenceMessage
	for _, m := range codexMessages("Fix the login bug") {
		msgs = append(msgs, pipeline.InferenceMessage{Role: m.role, Content: m.content})
	}
	r.observe(&pipeline.InferenceExtension{Messages: msgs, Completion: "Done."})
	if r.task != "Fix the login bug" {
		t.Fatalf("task = %q", r.task)
	}
	for _, m := range r.pending {
		if c, _ := m["content"].(string); strings.Contains(c, "AGENTS.md") || strings.Contains(c, "environment_context") {
			t.Fatalf("learned Codex's context: %v", r.pending)
		}
	}
}
