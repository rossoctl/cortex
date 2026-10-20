package contextguru

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	cgcomponents "github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/cost/spend"
	"github.com/rossoctl/cortex/core/pipeline"
)

// recordedCosts is a spend.CostRecorder that keeps every charge.
type recordedCosts struct {
	mu  sync.Mutex
	got []spend.Charge
}

func (r *recordedCosts) RecordCost(c spend.Charge) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, c)
}

func (r *recordedCosts) charges() []spend.Charge {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]spend.Charge(nil), r.got...)
}

// usageServer answers every chat-completions call with 1200 prompt tokens (1000 of them cached)
// and 80 completion tokens.
func usageServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"short"}}],`+
			`"usage":{"prompt_tokens":1200,"completion_tokens":80,"prompt_tokens_details":{"cached_tokens":1000}}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func costedPlugin(t *testing.T, costs spend.CostRecorder, cfgJSON string) *ContextGuru {
	t.Helper()
	p := New()
	if costs != nil {
		p.SetCostRecorder(costs)
	}
	if err := p.Configure(json.RawMessage(cfgJSON)); err != nil {
		t.Fatalf("configure: %v", err)
	}
	return p
}

func agentPctx(host string) *pipeline.Context {
	pctx := inferencePctx("/v1/chat/completions", chatBody("x"))
	pctx.Host = host
	pctx.Headers.Set("User-Agent", "claude-cli/2.1.14 (external, cli)")
	return pctx
}

var wantCallUsage = pricing.Usage{Input: 200, CacheRead: 1000, Output: 80}

// A call on the operator's configured model is money the session's compaction cost. It is
// charged to the session that caused it, at the endpoint that billed it.
func TestStaticModelCallIsChargedToTheSession(t *testing.T) {
	srv := usageServer(t)
	costs := &recordedCosts{}
	p := costedPlugin(t, costs, `{"model":{"base_url":"`+srv.URL+`","model":"cheap-1"}}`)
	pctx := agentPctx("api.example.com")

	spec := p.modelsFor(pctx, bschemas.OpenAI, "", "s1")
	if spec.Static == nil {
		t.Fatal("no static model, though one is configured")
	}
	if _, err := spec.Static.Complete(context.Background(), "summarize this"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got := costs.charges()
	if len(got) != 1 {
		t.Fatalf("charges = %d, want 1", len(got))
	}
	c := got[0]
	u, _ := url.Parse(srv.URL)
	if c.Plugin != "context-guru" || c.Purpose != spend.PurposeCompaction || c.Session != "s1" ||
		c.Host != u.Host || c.Model != "cheap-1" || c.Usage != wantCallUsage || c.Status != http.StatusOK {
		t.Fatalf("charge = %+v", c)
	}
	if c.Client == nil || !strings.Contains(c.Client.Raw, "claude-cli") {
		t.Errorf("client = %+v, want the agent that sent the request", c.Client)
	}
	if c.At.IsZero() || c.Duration <= 0 {
		t.Errorf("at/duration = %v %v, want both measured", c.At, c.Duration)
	}
}

// A call on the agent's own model goes to the agent's own endpoint, so that is the host billed.
func TestIncomingModelCallIsChargedToTheRequestsEndpoint(t *testing.T) {
	srv := usageServer(t)
	u, _ := url.Parse(srv.URL)
	costs := &recordedCosts{}
	p := costedPlugin(t, costs, `{}`)
	pctx := agentPctx(u.Host)

	spec := p.modelsFor(pctx, bschemas.OpenAI, "", "s1")
	if spec.Incoming == nil {
		t.Fatal("no incoming model for an OpenAI chat-completions request")
	}
	if _, err := spec.Incoming.Complete(context.Background(), "summarize this"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got := costs.charges()
	if len(got) != 1 || got[0].Host != u.Host || got[0].Model != "gpt-4o-mini" || got[0].Usage != wantCallUsage {
		t.Fatalf("charges = %+v", got)
	}
}

// With no recorder wired, model calls work as before and nothing is recorded. An unset static
// model stays a nil interface: a typed nil would make the engine call through it and panic.
func TestModelCallsWithoutARecorder(t *testing.T) {
	srv := usageServer(t)
	u, _ := url.Parse(srv.URL)
	p := costedPlugin(t, nil, `{}`)
	spec := p.modelsFor(agentPctx(u.Host), bschemas.OpenAI, "", "s1")
	if spec.Static != nil {
		t.Fatalf("Static = %#v, want nil with no model block", spec.Static)
	}
	if _, err := spec.Incoming.Complete(context.Background(), "x"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if spec := p.modelsFor(agentPctx(u.Host), bschemas.Anthropic, "", "s1"); spec.Incoming != nil {
		t.Fatalf("Incoming = %#v, want nil for a dialect llmclient does not speak", spec.Incoming)
	}
}

// A ping is charged to the session it kept warm, at the endpoint of the request that
// established the entry, with the ping's own tiers.
func TestPingIsChargedToTheSession(t *testing.T) {
	costs := &recordedCosts{}
	p := costedPlugin(t, costs, `{}`)
	pctx := finishedPctx(200, pipeline.AgentRoleMain)
	pctx.Headers.Set("User-Agent", "claude-cli/2.1.14 (external, cli)")
	em := p.pingEmitter(pctx, "s1")

	r, ok := recordFor(pctx, "s1", keepAliveConfig{Enabled: true}.policy(), func() dispatch.Ref { return "" }, em)
	if !ok || r.Emitter != em {
		t.Fatalf("record emitter = %#v, want the ping emitter", r.Emitter)
	}
	ka, ok := r.Emitter.(cgcomponents.KeepAliveEmitter)
	if !ok {
		t.Fatal("the ping emitter is not a KeepAliveEmitter, so the keeper would never report to it")
	}
	ka.KeepAlivePing(cgcomponents.KeepAliveReport{Session: "s1", Model: "claude-sonnet-5", Pings: 1,
		FreshInput: 3, CacheRead: 48_000, Output: 1, Status: 200, DurationMs: 420, TS: 1_700_000_000_000})

	got := costs.charges()
	if len(got) != 1 {
		t.Fatalf("charges = %d, want 1", len(got))
	}
	c := got[0]
	want := pricing.Usage{Input: 3, CacheRead: 48_000, Output: 1}
	if c.Purpose != spend.PurposeKeepAlive || c.Session != "s1" || c.Host != "ollama" ||
		c.Model != "claude-sonnet-5" || c.Usage != want || c.Status != 200 {
		t.Fatalf("charge = %+v", c)
	}
	if c.Duration != 420*time.Millisecond || !c.At.Equal(time.UnixMilli(1_700_000_000_000)) {
		t.Errorf("at/duration = %v %v", c.At, c.Duration)
	}
	if c.Client == nil || !strings.Contains(c.Client.Raw, "claude-cli") {
		t.Errorf("client = %+v", c.Client)
	}
}

// A ping that reported no tokens spent nothing anyone can price, so nothing is recorded.
func TestPingWithNoUsageIsNotCharged(t *testing.T) {
	costs := &recordedCosts{}
	p := costedPlugin(t, costs, `{}`)
	em := p.pingEmitter(finishedPctx(200, pipeline.AgentRoleMain), "s1")
	em.(cgcomponents.KeepAliveEmitter).KeepAlivePing(cgcomponents.KeepAliveReport{Session: "s1", Status: 502})
	if got := costs.charges(); len(got) != 0 {
		t.Fatalf("charges = %+v, want none", got)
	}
}
