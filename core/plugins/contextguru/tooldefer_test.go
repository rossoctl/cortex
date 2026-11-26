package contextguru

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/accrec"
	"github.com/rossoctl/context-guru/accwire"
	"github.com/rossoctl/context-guru/tooldefer"
	"github.com/tidwall/gjson"

	"github.com/rossoctl/cortex/core/accounting"
	"github.com/rossoctl/cortex/core/pipeline"
)

// Tool deferral (rossoctl/cortex#2150). Each test seeds a Pass probe verdict, so no probe goes
// out, and uses always_defer, so the decision does not wait for 3 sessions of tool history.

const deferModel = "claude-sonnet-4-5"

var deferAlways = []string{"mcp__gh__issue", "mcp__gh__pr"}

// deferBody is an Anthropic request with two built-in tools and one MCP server whose two tools
// weigh more than tooldefer.MinTokens together.
func deferBody(t *testing.T) []byte {
	t.Helper()
	var tools []map[string]any
	for _, n := range []string{"Bash", "Read", "mcp__gh__issue", "mcp__gh__pr"} {
		tools = append(tools, map[string]any{
			"name": n, "description": "tool " + n + " " + strings.Repeat("words ", 200),
			"input_schema": map[string]any{"type": "object"},
		})
	}
	b, err := json.Marshal(map[string]any{
		"model": deferModel, "max_tokens": 1024, "tools": tools,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// deferBodyWithMissingRef is deferBody, with a transcript turn that names a tool `tools` no
// longer declares: context-guru's RepairRefs adds a deferred stub for it before Flow.Apply
// decides anything else (rossoctl/context-guru#871).
func deferBodyWithMissingRef(t *testing.T) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(deferBody(t), &doc); err != nil {
		t.Fatal(err)
	}
	doc["messages"] = append(doc["messages"].([]any), map[string]any{
		"role": "user", "content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "t1", "content": []map[string]any{
				{"type": "tool_reference", "tool_name": "mcp__gone__x"},
			}},
		},
	})
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// deferPlugin is the plugin at ultrahigh, started (Init), with a Pass verdict for host's route.
func deferPlugin(t *testing.T, host string) *ContextGuru {
	t.Helper()
	cfg, err := json.Marshal(map[string]any{"engine": map[string]any{
		"preset":     "ultrahigh",
		"components": map[string]any{"toolfilter": map[string]any{"defer": "auto", "always_defer": deferAlways}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	p := configured(t, string(cfg))
	if err := p.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	if p.defer_ == nil {
		t.Fatal("ultrahigh built no deferral state")
	}
	p.defer_.prober.Seed(tooldefer.ProbeKey{Upstream: "http://" + host, Model: deferModel, Route: "/v1/messages"},
		tooldefer.Pass, time.Now())
	return p
}

func deferredTools(body []byte) []string {
	var out []string
	for _, tl := range gjson.GetBytes(body, "tools").Array() {
		if tl.Get("defer_loading").Bool() {
			out = append(out, tl.Get("name").String())
		}
	}
	slices.Sort(out)
	return out
}

func deferPctx(t *testing.T, host string, resendable bool) *pipeline.Context {
	pctx := inferencePctx("/v1/messages", deferBody(t))
	pctx.Host = host
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: deferModel}
	if resendable {
		pctx.SetResendable()
	}
	return pctx
}

// The issue's reproduction: at ultrahigh, a request on a listener that can resend goes upstream
// with the MCP server's tools deferred, as the library defers them.
func TestToolDefer_UltrahighDefersOnAResendableListener(t *testing.T) {
	p := deferPlugin(t, "h1")
	pctx := deferPctx(t, "h1", true)
	invokeReq(p, pctx)
	if got := deferredTools(pctx.Body); !slices.Equal(got, deferAlways) {
		t.Fatalf("deferred %v, want %v", got, deferAlways)
	}
	var deferOps int
	for _, op := range pctx.ReportedOps() {
		if op.Operator == accwire.Operator(accwire.OpToolDefer) {
			deferOps++
		}
	}
	if deferOps == 0 {
		t.Fatal("no tooldefer op reported")
	}
}

// A listener that never resends would show the client deferral's own 400, so nothing is deferred.
func TestToolDefer_NothingOnAListenerThatCannotResend(t *testing.T) {
	p := deferPlugin(t, "h2")
	pctx := deferPctx(t, "h2", false)
	invokeReq(p, pctx)
	if got := deferredTools(pctx.Body); len(got) != 0 {
		t.Fatalf("deferred %v on a listener that cannot resend", got)
	}
}

// xhigh never defers, and builds no deferral state.
func TestToolDefer_XhighBuildsNoState(t *testing.T) {
	p := configured(t, `{"engine":{"preset":"xhigh"}}`)
	if err := p.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	if p.deferOn || p.defer_ != nil {
		t.Fatalf("xhigh: deferOn %v, state %v", p.deferOn, p.defer_ != nil)
	}
}

// Two instances on one file share one state, as a config reload has them, and the last release
// closes it.
func TestToolDefer_StateIsSharedAcrossInstances(t *testing.T) {
	a, b := deferPlugin(t, "h3"), deferPlugin(t, "h3")
	if a.defer_ != b.defer_ {
		t.Fatal("two instances on one file built two deferral states")
	}
	path := a.defer_.path
	// What Shutdown does with the state; Shutdown itself runs once, in the cleanup.
	a.defer_.release()
	a.defer_ = nil
	deferHostsMu.Lock()
	_, open := deferHosts[path]
	deferHostsMu.Unlock()
	if !open {
		t.Fatal("the state closed while an instance still uses it")
	}
	b.defer_.release()
	b.defer_ = nil
	deferHostsMu.Lock()
	_, open = deferHosts[path]
	deferHostsMu.Unlock()
	if open {
		t.Fatal("the state stayed open after its last instance shut down")
	}
}

func resend(p *ContextGuru, pctx *pipeline.Context, status int, body string) bool {
	pctx.SetCurrentPlugin(p.Name(), pipeline.InvocationPhaseRequest)
	defer pctx.ClearCurrentPlugin()
	return p.Resend(context.Background(), pctx, status, []byte(body))
}

// A 400 to a deferred request sends the original once. When the original goes through, the
// refusal was deferral's: the library turns deferral off for the model (Flow.Retry).
func TestToolDefer_RefusalResendsTheOriginal(t *testing.T) {
	p := deferPlugin(t, "h4")
	pctx := deferPctx(t, "h4", true)
	orig := pctx.Body
	invokeReq(p, pctx)
	if len(deferredTools(pctx.Body)) == 0 {
		t.Fatal("premise: nothing deferred")
	}
	if resend(p, pctx, http.StatusTooManyRequests, `{}`) {
		t.Fatal("a 429 is not deferral's refusal, but it was resent")
	}
	if !resend(p, pctx, http.StatusBadRequest, `{"error":{"message":"bad"}}`) {
		t.Fatal("a 400 to a deferred request was not resent")
	}
	if got := deferredTools(pctx.Body); len(got) != 0 {
		t.Fatalf("the resent body still defers %v", got)
	}
	if len(pctx.Body) != len(orig) || !slices.Equal(pctx.ResendReasons(), []string{"tool_defer_refused"}) {
		t.Fatalf("resent %d bytes (want %d), reasons %v", len(pctx.Body), len(orig), pctx.ResendReasons())
	}
	if resend(p, pctx, http.StatusBadRequest, `{"error":{"message":"bad"}}`) {
		t.Fatal("the same refusal was answered twice")
	}
	pctx.StatusCode = http.StatusOK
	p.OnFinish(context.Background(), pctx)
	if !p.defer_.gate.Broken("", deferModel) {
		t.Fatal("the original went through, but deferral stays on for the model")
	}
	next := deferPctx(t, "h4", true)
	next.Session, next.SessionID = &pipeline.SessionView{ID: "s2"}, "s2"
	invokeReq(p, next)
	if got := deferredTools(next.Body); len(got) != 0 {
		t.Fatalf("deferred %v after the model was turned off", got)
	}
}

// The original is refused too, because its transcript names a tool `tools` lacks: it goes once
// more with a deferred stub for that tool (rossoctl/context-guru#871). Deferral stays on.
func TestToolDefer_MissingToolGetsAStub(t *testing.T) {
	p := deferPlugin(t, "h5")
	pctx := deferPctx(t, "h5", true)
	invokeReq(p, pctx)
	if !resend(p, pctx, http.StatusBadRequest, `{}`) {
		t.Fatal("premise: the first refusal was not resent")
	}
	missing := `{"type":"error","error":{"type":"invalid_request_error","message":"Tool reference 'mcp__gone__x' not found in available tools"}}`
	// go.mod pins context-guru exactly, so a RepairFromError that cannot parse this error text
	// is not a future-version concern to skip past: it means the stub round this test exercises
	// does not work at the pinned version, and a Skip would hide that regression (review finding).
	if _, ok := tooldefer.RepairFromError([]byte(missing), pctx.Body); !ok {
		t.Fatal("tooldefer.RepairFromError does not parse this missing-tool error text at the pinned context-guru version")
	}
	if !resend(p, pctx, http.StatusBadRequest, missing) {
		t.Fatal("the missing-tool refusal was not resent with a stub")
	}
	if !strings.Contains(string(pctx.Body), "mcp__gone__x") {
		t.Fatal("the resent body has no stub for the missing tool")
	}
	pctx.StatusCode = http.StatusOK
	p.OnFinish(context.Background(), pctx)
	if p.defer_.gate.Broken("", deferModel) {
		t.Fatal("a refusal that was not deferral's turned deferral off")
	}
}

// noReq is the plugin without its inference-parser requirement, so a pipeline of it alone builds.
type noReq struct{ *ContextGuru }

func (n noReq) Capabilities() pipeline.PluginCapabilities {
	c := n.ContextGuru.Capabilities()
	c.RequiresAny = nil
	return c
}

// The accountant prices the body the provider bills (accountant R56), the deferral's saving goes to
// the tooldefer operator, and the library's sum check holds.
func TestToolDefer_AccountantBooksTheSaving(t *testing.T) {
	p := deferPlugin(t, "h6")
	h := accounting.New(accounting.Options{})
	t.Cleanup(func() { _ = h.Close() })
	pipe, err := pipeline.New([]pipeline.Plugin{noReq{p}}, pipeline.WithAccountant(h))
	if err != nil {
		t.Fatal(err)
	}
	pctx := deferPctx(t, "h6", true)
	if act := pipe.Run(context.Background(), pctx); act.Type != pipeline.Continue {
		t.Fatalf("action = %v", act.Type)
	}
	if len(deferredTools(pctx.Body)) == 0 {
		t.Fatal("premise: nothing deferred")
	}
	billed, unplaced := pctx.BilledBody(pctx.Body)
	if unplaced || len(billed) >= len(pctx.Body) {
		t.Fatalf("billed body: %d bytes of %d sent, unplaced %v", len(billed), len(pctx.Body), unplaced)
	}
	pctx.StatusCode = http.StatusOK
	pctx.Extensions.Inference.InputTokens = 3
	pctx.Extensions.Inference.CacheWriteTokens = len(billed) / 4
	res, ok := h.Compute(pctx)
	if !ok {
		t.Fatal("Compute returned no Result")
	}
	noSumFailure(t, res)
	var saved bool
	for _, o := range res.Ops {
		s := o.Saved
		if o.Operator == accwire.Operator(accwire.OpToolDefer) && s.Input+s.CacheWrite5m+s.CacheWrite1h+s.CacheRead > 0 {
			saved = true
		}
	}
	if !saved {
		t.Fatalf("no tooldefer saving booked: %+v", res.Ops)
	}
}

// A request sent again books no figure (upstream_rounds), whatever deferral reported.
func TestToolDefer_ResentRequestBooksNoFigure(t *testing.T) {
	p := deferPlugin(t, "h7")
	h := accounting.New(accounting.Options{})
	t.Cleanup(func() { _ = h.Close() })
	pipe, err := pipeline.New([]pipeline.Plugin{noReq{p}}, pipeline.WithAccountant(h))
	if err != nil {
		t.Fatal(err)
	}
	pctx := deferPctx(t, "h7", true)
	pipe.Run(context.Background(), pctx)
	if !resend(p, pctx, http.StatusBadRequest, `{}`) {
		t.Fatal("premise: not resent")
	}
	pctx.StatusCode = http.StatusOK
	if _, ok := h.Compute(pctx); ok {
		t.Fatal("a resent request booked a figure")
	}
}

// A tool RepairRefs stubs is billed as the minimal stub accrec.DeferredStub gives, the same as
// one toolfilter itself deferred: the accountant's pre-deferral snapshot must be the body BEFORE
// Flow.Apply ran RepairRefs, not di.Orig, which Flow.Apply captures AFTER (context-guru v0.7.9,
// tooldefer/flow.go:109-113,160). di.Orig already carries the repair stub, so accrec.OwnDeferred
// would read it as already deferred before this request and leave its StubTools-sized definition
// (name + description + input_schema) in the billed body (clawgenti review finding).
func TestToolDefer_ARepairedToolIsBilledAsTheMinimalStub(t *testing.T) {
	p := deferPlugin(t, "h8")
	h := accounting.New(accounting.Options{})
	t.Cleanup(func() { _ = h.Close() })
	pipe, err := pipeline.New([]pipeline.Plugin{noReq{p}}, pipeline.WithAccountant(h))
	if err != nil {
		t.Fatal(err)
	}
	pctx := deferPctx(t, "h8", true)
	pctx.Body = deferBodyWithMissingRef(t)
	if act := pipe.Run(context.Background(), pctx); act.Type != pipeline.Continue {
		t.Fatalf("action = %v", act.Type)
	}
	if !strings.Contains(string(pctx.Body), "mcp__gone__x") {
		t.Fatal("premise: RepairRefs added no stub for the missing tool")
	}
	billed, unplaced := pctx.BilledBody(pctx.Body)
	if unplaced {
		t.Fatal("billed body unplaced")
	}
	got := gjson.GetBytes(billed, `tools.#(name=="mcp__gone__x")`).Raw
	if want := accrec.DeferredStub("mcp__gone__x"); got != want {
		t.Fatalf("billed stub for the repaired tool = %s, want the minimal stub %s", got, want)
	}
}

// release holds the registry lock for the whole flush, so a concurrent acquireDeferHost for the
// same path cannot build a second Mem on the file while the first one's final flush is still
// writing it (CodeRabbit review finding).
func TestToolDefer_ReleaseHoldsTheRegistryLockThroughTheFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), tooldefer.FileName)
	f, err := tooldefer.OpenFile(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	gate, started := make(chan struct{}), make(chan struct{})
	h := &deferHost{path: path, file: f, hist: tooldefer.NewMem(f.DB), prober: tooldefer.NewProber(),
		gate: tooldefer.NewGate(), users: 1, stop: func() { close(started); <-gate }}
	deferHostsMu.Lock()
	deferHosts[path] = h
	deferHostsMu.Unlock()

	done := make(chan struct{})
	go func() { h.release(); close(done) }()
	<-started // release() has called h.stop(); it is now blocked on <-gate.

	// At this instant release()'s goroutine has already called deferHostsMu.Lock() (Go's channel
	// send in stop() happens-after that Lock, by release's own code order) and has not yet
	// Unlock()ed: Unlock only runs after h.stop() returns, which it cannot do until gate closes.
	// So a correct release still holds the lock here; TryLock succeeding means it does not.
	if deferHostsMu.TryLock() {
		deferHostsMu.Unlock()
		close(gate)
		<-done
		t.Fatal("release() released the registry lock before its flush finished")
	}
	close(gate)
	<-done
}
