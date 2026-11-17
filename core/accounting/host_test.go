package accounting

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
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

// headerRewriter is a request plugin that replaces a header after RequestStart has already run
// — token-exchange's shape, and the reason BillingFrom must be read in RequestStart rather than
// at RequestEnd (design §5.7).
type headerRewriter struct {
	name, value string
}

func (r *headerRewriter) Name() string { return "header-rewriter" }
func (r *headerRewriter) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (r *headerRewriter) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	pctx.Headers.Set(r.name, r.value)
	return pipeline.Action{Type: pipeline.Continue}
}
func (r *headerRewriter) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
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

// TestHost_ComputeFillsCacheWrite1h: Compute must fill accountant.Usage.CacheWrite1h
// from the extension's CacheWrite1hTokens — a subset of CacheWrite, never an
// addition to it. Task 7 (2026-11-17 accountant-integration).
func TestHost_ComputeFillsCacheWrite1h(t *testing.T) {
	h := newHost(t)
	var got accountant.Usage
	h.usageHook = func(u accountant.Usage) { got = u }

	usage := coldUsage()
	usage.CacheWrite1hTokens = 600 // a subset of CacheWriteTokens (1800) above
	pctx := request(t, h, "s1", "/v1/messages", clientBody("hi"), &operator{fwd: forwardedBody("hi")}, 200, usage)
	if _, ok := h.Compute(pctx); !ok {
		t.Fatal("Compute returned no Result")
	}
	if got.CacheWrite1h != 600 {
		t.Errorf("accountant.Usage.CacheWrite1h = %d, want 600", got.CacheWrite1h)
	}
	if got.CacheWrite != 1800 {
		t.Errorf("accountant.Usage.CacheWrite = %d, want 1800 (the total, unchanged by CacheWrite1h)", got.CacheWrite)
	}
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

// TestHost_BillingFromClientHeaderBeforeRewrite checks that RequestMeta.Billing reflects the
// CLIENT's credential, read in RequestStart before any plugin runs — not whatever credential
// the request carries by the time the pipeline finishes. token-exchange replaces Authorization
// on outbound requests, so a request that arrives on a subscription token but is rewritten to an
// API key before it leaves must still bill as subscription (design §5.7).
func TestHost_BillingFromClientHeaderBeforeRewrite(t *testing.T) {
	h := newHost(t)
	var meta accountant.RequestMeta
	h.metaHook = func(m accountant.RequestMeta) { meta = m }
	p, err := pipeline.New([]pipeline.Plugin{&headerRewriter{"Authorization", "Bearer sk-ant-api03-y"}}, pipeline.WithAccountant(h))
	if err != nil {
		t.Fatal(err)
	}
	pctx := &pipeline.Context{
		Host: "api.anthropic.com", Path: "/v1/messages", Method: "POST",
		Body: clientBody("hi"), SessionID: "s1",
		Headers: http.Header{"Authorization": []string{"Bearer sk-ant-oat01-x"}},
	}
	inf := coldUsage()
	inf.Model = "claude-sonnet-4-5"
	pctx.Extensions.Inference = &inf
	if act := p.Run(context.Background(), pctx); act.Type != pipeline.Continue {
		t.Fatalf("action = %v", act.Type)
	}
	if meta.Billing.Mode != accountant.BillingSubscription {
		t.Errorf("RequestMeta.Billing.Mode = %q, want %q (the client's original credential, read before the plugin's rewrite)",
			meta.Billing.Mode, accountant.BillingSubscription)
	}
	// The rewrite did happen, so this is really testing read-order and not a plugin that did
	// nothing.
	if got := pctx.Headers.Get("Authorization"); got != "Bearer sk-ant-api03-y" {
		t.Fatalf("Authorization after the pipeline = %q, want the plugin's rewrite", got)
	}
}

// TestHost_ThreadBillingIsLastRequests checks that ThreadBilling answers with the most recent
// request's mode on that thread, and accountant.BillingUnknown for a thread that has made none.
func TestHost_ThreadBillingIsLastRequests(t *testing.T) {
	h := newHost(t)
	if got := h.ThreadBilling("s1", "main"); got.Mode != accountant.BillingUnknown {
		t.Errorf("ThreadBilling before any request = %q, want %q", got.Mode, accountant.BillingUnknown)
	}

	runWithAuth := func(session, auth string) {
		t.Helper()
		p, err := pipeline.New([]pipeline.Plugin{&operator{}}, pipeline.WithAccountant(h))
		if err != nil {
			t.Fatal(err)
		}
		pctx := &pipeline.Context{
			Host: "api.anthropic.com", Path: "/v1/messages", Method: "POST",
			Body: clientBody("hi"), SessionID: session,
			Headers: http.Header{"Authorization": []string{auth}},
		}
		inf := coldUsage()
		inf.Model = "claude-sonnet-4-5"
		pctx.Extensions.Inference = &inf
		if act := p.Run(context.Background(), pctx); act.Type != pipeline.Continue {
			t.Fatalf("action = %v", act.Type)
		}
	}

	runWithAuth("s1", "Bearer sk-ant-oat01-x")
	if got := h.ThreadBilling("s1", "main"); got.Mode != accountant.BillingSubscription {
		t.Errorf("ThreadBilling after the first request = %q, want %q", got.Mode, accountant.BillingSubscription)
	}
	// A second request on the same thread, with a different credential, replaces the thread's
	// remembered mode.
	runWithAuth("s1", "Bearer sk-ant-api03-y")
	if got := h.ThreadBilling("s1", "main"); got.Mode != accountant.BillingAPI {
		t.Errorf("ThreadBilling after the second request = %q, want %q", got.Mode, accountant.BillingAPI)
	}
	// A different session's "main" thread is tracked separately.
	if got := h.ThreadBilling("s2", "main"); got.Mode != accountant.BillingUnknown {
		t.Errorf("ThreadBilling for an unrelated session = %q, want %q", got.Mode, accountant.BillingUnknown)
	}
}

// TestHost_ThreadBillingIsBounded checks that threadBilling does not grow for the life of the
// process: once it holds more than threadBillingCap (session, thread) pairs, the oldest are
// dropped (mirroring accountant.Trackers' own 10,000-thread bound).
func TestHost_ThreadBillingIsBounded(t *testing.T) {
	var tick int64
	h, err := New(Options{Now: func() time.Time {
		tick++
		return time.Unix(tick, 0)
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })

	const n = threadBillingCap + 1 // one past the cap: forces exactly one trim
	for i := 0; i < n; i++ {
		h.setThreadBilling("s", fmt.Sprintf("t%d", i), accountant.BillingAPI)
	}

	h.mu.Lock()
	size := len(h.threadBilling)
	h.mu.Unlock()
	if size > threadBillingCap {
		t.Errorf("threadBilling size = %d, want <= %d", size, threadBillingCap)
	}
	if size != threadBillingTrimTo {
		t.Errorf("threadBilling size = %d, want exactly %d after the one trim", size, threadBillingTrimTo)
	}

	// The most recently seen pair is kept.
	if got := h.ThreadBilling("s", fmt.Sprintf("t%d", n-1)); got.Mode != accountant.BillingAPI {
		t.Errorf("ThreadBilling for the most recent pair = %q, want %q (it should not have been trimmed)",
			got.Mode, accountant.BillingAPI)
	}
	// The oldest pair was dropped.
	if got := h.ThreadBilling("s", "t0"); got.Mode != accountant.BillingUnknown {
		t.Errorf("ThreadBilling for the oldest pair = %q, want %q (it should have been trimmed)",
			got.Mode, accountant.BillingUnknown)
	}
}
