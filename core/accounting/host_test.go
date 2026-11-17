package accounting

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/tidwall/gjson"
)

// bigTool is a tool declaration large enough that removing it is a real saving.
var bigTool = `{"name":"Big","description":"` + strings.Repeat("x", 4000) + `","input_schema":{"type":"object"}}`

const smallTool = `{"name":"Read","description":"read a file","input_schema":{"type":"object"}}`

// conversation is a user turn long enough that the body's bytes and the usage below are in the
// library's plausible bytes-per-token range, with the cache mark Claude Code puts on the last
// message.
func conversation(msg string) string {
	return `[{"role":"user","content":[{"type":"text","text":"` + msg + strings.Repeat(" lorem ipsum dolor", 400) +
		`","cache_control":{"type":"ephemeral"}}]}]`
}

func clientBody(msg string) []byte {
	return []byte(`{"model":"claude-sonnet-4-5","max_tokens":10,"tools":[` + bigTool + `,` + smallTool +
		`],"messages":` + conversation(msg) + `}`)
}

func forwardedBody(msg string) []byte {
	return []byte(`{"model":"claude-sonnet-4-5","max_tokens":10,"tools":[` + smallTool +
		`],"messages":` + conversation(msg) + `}`)
}

// pruneOp is the op a tool-prune-like operator reports for removing bigTool.
func pruneOp() accountant.Operation {
	return accountant.Operation{
		Kind:       accountant.KindRewrite,
		At:         accountant.Position{Section: accountant.SectionTools, Index: 0},
		SizeBefore: accountant.Size{Bytes: len(bigTool)},
		ChangeID:   "test:big",
	}
}

// operator is a request plugin that sends fwd in place of the client body and reports ops.
type operator struct {
	fwd []byte
	ops []accountant.Operation
}

func (o *operator) Name() string { return "op" }
func (o *operator) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{WritesRequestBody: true}
}
func (o *operator) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if o.fwd != nil {
		pctx.SetBody(o.fwd)
	}
	pctx.ReportOps(o.ops...)
	return pipeline.Action{Type: pipeline.Continue}
}
func (o *operator) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

func newHost(t *testing.T) *Host {
	t.Helper()
	h, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// request runs one request through a pipeline built with h, the way a listener would, and
// fills in what the response phase knows before settle calls Compute.
func request(t *testing.T, h *Host, session, path string, client []byte, op *operator, status int, usage pipeline.InferenceExtension) *pipeline.Context {
	t.Helper()
	p, err := pipeline.New([]pipeline.Plugin{op}, pipeline.WithAccountant(h))
	if err != nil {
		t.Fatal(err)
	}
	return runOn(t, p, session, path, client, status, usage)
}

func runOn(t *testing.T, p *pipeline.Pipeline, session, path string, client []byte, status int, usage pipeline.InferenceExtension) *pipeline.Context {
	t.Helper()
	inf := usage
	if inf.Model == "" {
		inf.Model = "claude-sonnet-4-5"
	}
	pctx := &pipeline.Context{Host: "api.anthropic.com", Path: path, Method: "POST", Body: client, SessionID: session}
	pctx.Extensions.Inference = &inf
	if act := p.Run(context.Background(), pctx); act.Type != pipeline.Continue {
		t.Fatalf("action = %v", act.Type)
	}
	pctx.StatusCode = status
	return pctx
}

// coldUsage is a first request: the forwarded prompt (about 7.3 KB) written to the cache.
func coldUsage() pipeline.InferenceExtension {
	return pipeline.InferenceExtension{InputTokens: 3, CacheWriteTokens: 1800}
}

func TestHost_EndThenCompute(t *testing.T) {
	h := newHost(t)
	pctx := request(t, h, "s1", "/v1/messages", clientBody("hi"), &operator{fwd: forwardedBody("hi"), ops: []accountant.Operation{pruneOp()}}, 200, coldUsage())
	res, ok := h.Compute(pctx)
	if !ok {
		t.Fatal("Compute returned no Result")
	}
	if len(res.Ops) != 1 {
		t.Fatalf("Result.Ops = %+v, want one op", res.Ops)
	}
	op := res.Ops[0]
	if op.Operator != "op" || op.ChangeID != "test:big" {
		t.Errorf("op = %+v, want the reported op stamped with its plugin", op)
	}
	if op.Saved.IsZero() {
		t.Errorf("op.Saved = %+v, want a saving for the removed tool (checks %+v, unpriced %+v)", op.Saved, res.Checks, res.Unpriced)
	}
	// Latched: a second call answers the same, so a repeated settle does not price twice.
	again, ok := h.Compute(pctx)
	if !ok || len(again.Ops) != 1 || again.Ops[0].Saved != op.Saved {
		t.Errorf("second Compute = %+v %v, want the same Result", again, ok)
	}
}

// Review Focus 2: non-inference traffic on an accounting-on pipeline starts nothing and logs
// nothing at WARN.
func TestHost_NonInferenceIsIgnored(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := newHost(t)
	cases := []struct {
		name, path string
		body       []byte
	}{
		{"MCP POST", "/mcp", []byte(`{"jsonrpc":"2.0","method":"tools/call"}`)},
		{"GET with no body", "/v1/messages", nil},
		{"body the wire parser rejects", "/v1/messages", []byte(`not json`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pctx := request(t, h, "s1", c.path, c.body, &operator{}, 200, coldUsage())
			if _, ok := h.Compute(pctx); ok {
				t.Error("Compute gave a Result for non-inference traffic")
			}
		})
	}
	if logs.Len() != 0 {
		t.Errorf("logged at WARN or above:\n%s", logs.String())
	}
}

func TestHost_NoSessionStartsNothing(t *testing.T) {
	h := newHost(t)
	pctx := request(t, h, "", "/v1/messages", clientBody("hi"), &operator{}, 200, coldUsage())
	if pipeline.GetState[pending](pctx, pendingKey) != nil {
		t.Error("a request with no session left a pending handle")
	}
}

// Review Focus 3: a client that hangs up, or an upstream 5xx, is skipped; its handle is
// released and its goroutine ends.
func TestHost_SkipReleasesHandle(t *testing.T) {
	h := newHost(t)
	base := runtime.NumGoroutine()
	for _, status := range []int{499, 502} {
		pctx := request(t, h, "s1", "/v1/messages", clientBody("hi"), &operator{fwd: forwardedBody("hi"), ops: []accountant.Operation{pruneOp()}}, status, coldUsage())
		if _, ok := h.Compute(pctx); ok {
			t.Errorf("status %d: Compute gave a Result", status)
		}
		if pipeline.GetState[pending](pctx, pendingKey) != nil {
			t.Errorf("status %d: the pending handle was not released", status)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base {
		t.Errorf("goroutines = %d, want back to %d", n, base)
	}
}

func TestHost_ComputeTimesOut(t *testing.T) {
	h := newHost(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	h.beforeEnd = func() { <-release }
	pctx := request(t, h, "s1", "/v1/messages", clientBody("hi"), &operator{fwd: forwardedBody("hi")}, 200, coldUsage())
	start := time.Now()
	_, ok := h.Compute(pctx)
	if ok {
		t.Error("Compute gave a Result while the record was not ended")
	}
	if d := time.Since(start); d > 2100*time.Millisecond {
		t.Errorf("Compute took %v, want at most 2.1s", d)
	}
	// Latched: the second call does not wait again.
	start = time.Now()
	if _, ok := h.Compute(pctx); ok || time.Since(start) > 100*time.Millisecond {
		t.Error("a second Compute after a timeout waited again or gave a Result")
	}
}

func TestHost_PanicInEndFailsOpen(t *testing.T) {
	h := newHost(t)
	h.beforeEnd = func() { panic("hostile body") }
	pctx := request(t, h, "s1", "/v1/messages", clientBody("hi"), &operator{fwd: forwardedBody("hi")}, 200, coldUsage())
	if _, ok := h.Compute(pctx); ok {
		t.Error("Compute gave a Result after the record panicked")
	}
	// The host still works for the next request.
	h.beforeEnd = nil
	pctx = request(t, h, "s1", "/v1/messages", clientBody("hi"), &operator{fwd: forwardedBody("hi"), ops: []accountant.Operation{pruneOp()}}, 200, coldUsage())
	if _, ok := h.Compute(pctx); !ok {
		t.Error("Compute gave no Result on the request after a panic")
	}
}

func TestHost_PanicInComputeFailsOpen(t *testing.T) {
	h := newHost(t)
	pctx := request(t, h, "s1", "/v1/messages", clientBody("hi"), &operator{fwd: forwardedBody("hi")}, 200, coldUsage())
	h.computeHook = func() { panic("library bug") }
	if _, ok := h.Compute(pctx); ok {
		t.Error("Compute gave a Result after the library panicked")
	}
}

// Review Focus 4: parallel requests on many sessions, and several at once on one session (as
// subagents send them). Run under -race.
func TestHost_ConcurrentSessions(t *testing.T) {
	h := newHost(t)
	p, err := pipeline.New([]pipeline.Plugin{&operator{fwd: forwardedBody("hi"), ops: []accountant.Operation{pruneOp()}}}, pipeline.WithAccountant(h))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	fails := 0
	for s := 0; s < 32; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			var inner sync.WaitGroup
			for r := 0; r < 20; r++ {
				inner.Add(1)
				go func() {
					defer inner.Done()
					pctx := &pipeline.Context{Host: "api.anthropic.com", Path: "/v1/messages", Method: "POST",
						Body: clientBody("hi"), SessionID: fmt.Sprintf("s%d", s%8)}
					inf := coldUsage()
					inf.Model = "claude-sonnet-4-5"
					pctx.Extensions.Inference = &inf
					p.Run(context.Background(), pctx)
					pctx.StatusCode = 200
					if _, ok := h.Compute(pctx); !ok {
						mu.Lock()
						fails++
						mu.Unlock()
					}
				}()
			}
			inner.Wait()
		}(s)
	}
	wg.Wait()
	if fails != 0 {
		t.Errorf("%d of 640 requests gave no Result", fails)
	}
}

func TestHost_ModelChangeBecomesSettingOp(t *testing.T) {
	h := newHost(t)
	var seen []accountant.Operation
	h.onEnd = func(ops []accountant.Operation) { seen = ops }
	usage := coldUsage()
	usage.Model = "claude-haiku-4-5"
	usage.RequestedModel = "claude-sonnet-4-5"
	pctx := request(t, h, "s1", "/v1/messages", clientBody("hi"), &operator{fwd: forwardedBody("hi"), ops: []accountant.Operation{pruneOp()}}, 200, usage)
	res, ok := h.Compute(pctx)
	if !ok {
		t.Fatal("Compute returned no Result")
	}
	var setting *accountant.Operation
	for i := range seen {
		if seen[i].Kind == accountant.KindSetting {
			setting = &seen[i]
		}
	}
	if setting == nil {
		t.Fatalf("ops = %+v, want a setting op for the model change", seen)
	}
	ch, ok := setting.Settings["model"]
	if !ok || ch.From == nil || ch.To == nil || *ch.From != "claude-sonnet-4-5" || *ch.To != "claude-haiku-4-5" {
		t.Errorf("setting op = %+v, want model claude-sonnet-4-5 -> claude-haiku-4-5", setting.Settings)
	}
	if ch.At == nil || ch.At.Section != accountant.SectionTools || ch.At.Index != 0 {
		t.Errorf("setting At = %+v, want tools[0]: a model change starts at the first position", ch.At)
	}
	if len(res.Ops) != 2 {
		t.Errorf("Result.Ops = %d, want 2 (the rewrite and the setting)", len(res.Ops))
	}
}

func TestHost_ServerHeldHistoryIsSkipped(t *testing.T) {
	h := newHost(t)
	body := []byte(`{"model":"gpt-5","previous_response_id":"resp_1","input":[{"role":"user","content":"hi"}]}`)
	pctx := request(t, h, "s1", "/v1/responses", body, &operator{}, 200, coldUsage())
	if _, ok := h.Compute(pctx); ok {
		t.Error("Compute gave a Result for a body whose history the provider holds")
	}
}

func TestResponsesServerHeldHistory(t *testing.T) {
	for body, want := range map[string]bool{
		`{"input":"hi"}`: false,
		`{"previous_response_id":"r1","input":"hi"}`:               true,
		`{"previous_response_id":"","input":"hi"}`:                 false,
		`{"conversation":"c1","input":"hi"}`:                       true,
		`{"conversation":{"id":"c1"},"input":"hi"}`:                true,
		`{"input":[{"type":"item_reference","id":"i1"}]}`:          true,
		`{"input":[{"type":"message","role":"user"}]}`:             false,
		`{"input":[{"type":"message"},{"type":"item_reference"}]}`: true,
	} {
		if got := responsesServerHeldHistory([]byte(body)); got != want {
			t.Errorf("%s: %v, want %v", body, got, want)
		}
	}
}

// Review Focus 5: a pipeline hot reload builds a new pipeline around the same process-scoped
// Host, so the Tracker state from requests on the old build is there for the new one: the
// thread's state version counts the writes of both builds' requests.
func TestHost_SurvivesReload(t *testing.T) {
	build := func(h *Host) *pipeline.Pipeline {
		p, err := pipeline.New([]pipeline.Plugin{&operator{fwd: forwardedBody("hi"), ops: []accountant.Operation{pruneOp()}}}, pipeline.WithAccountant(h))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	hotUsage := pipeline.InferenceExtension{InputTokens: 3, CacheReadTokens: 1800}
	version := func(h *Host) int64 {
		state, err := h.trackers.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		threads := gjson.GetBytes(state, "threads").Array()
		if len(threads) != 1 {
			t.Fatalf("threads = %d, want 1: %s", len(threads), state)
		}
		return threads[0].Get("stateVersion").Int()
	}

	h := newHost(t)
	first := runOn(t, build(h), "s1", "/v1/messages", clientBody("hi"), 200, coldUsage())
	if _, ok := h.Compute(first); !ok {
		t.Fatal("first request: no Result")
	}
	afterFirst := version(h)
	second := runOn(t, build(h), "s1", "/v1/messages", clientBody("hi"), 200, hotUsage)
	if _, ok := h.Compute(second); !ok {
		t.Fatal("second request: no Result")
	}
	shared := version(h)

	// The control: the same second request alone, on a Host with no history.
	other := newHost(t)
	alone := runOn(t, build(other), "s1", "/v1/messages", clientBody("hi"), 200, hotUsage)
	if _, ok := other.Compute(alone); !ok {
		t.Fatal("control: no Result")
	}
	if fresh := version(other); shared <= afterFirst || shared <= fresh {
		t.Errorf("state version after the reload = %d (after build 1: %d, a fresh Host: %d): build 2 did not continue build 1's thread",
			shared, afterFirst, fresh)
	}
}
