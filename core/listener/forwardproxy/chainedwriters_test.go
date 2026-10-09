package forwardproxy

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/memstore"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/storage/filestore"

	// The laptop's outbound chain, built through the registry as cmd/cortex builds it.
	_ "github.com/rossoctl/cortex/core/plugins/inferenceparser"
	_ "github.com/rossoctl/cortex/core/plugins/inferencerouter"
	_ "github.com/rossoctl/cortex/core/plugins/toolprune"
)

// bodyOrigin is a plain-http inference server that keeps the body and Authorization
// of every request, and answers each with an Anthropic message reporting usage — or,
// for a request naming refuse as its model, with LiteLLM's refusal of a model the
// team may not use.
type bodyOrigin struct {
	mu     sync.Mutex
	bodies []string
	keys   []string
	refuse string
}

func newBodyOrigin(t *testing.T, usage string) (*httptest.Server, *bodyOrigin) {
	t.Helper()
	o := &bodyOrigin{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodGet {
			http.NotFound(w, r) // the router's model-list fetch; the stored list stands
			return
		}
		o.mu.Lock()
		o.bodies = append(o.bodies, string(b))
		o.keys = append(o.keys, r.Header.Get("Authorization"))
		refuse := o.refuse != "" && strings.Contains(string(b), `"model":"`+o.refuse+`"`)
		o.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if refuse {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"type":"team_model_access_denied","param":"model","code":"403"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"glm-big",` +
			`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":` + usage + `}`))
	}))
	t.Cleanup(srv.Close)
	return srv, o
}

func (o *bodyOrigin) seen() (bodies, keys []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.bodies), slices.Clone(o.keys)
}

// inputRates is a rate table pricing each model's input tokens at its own rate, so a
// saving priced at the wrong model is off by their ratio.
func inputRates(t *testing.T, perToken map[string]float64) *pricing.Registry {
	t.Helper()
	var entries []pricing.Entry
	for model, per := range perToken {
		var r pricing.Rates
		for _, tier := range []pricing.Tier{pricing.TierInput, pricing.TierCacheWrite, pricing.TierCacheRead, pricing.TierOutput} {
			r.Base[tier], r.Set[tier] = per, true
		}
		entries = append(entries, pricing.Entry{Host: "*", Model: model, Rates: r, Prov: pricing.ProvConfigured})
	}
	tab, err := pricing.NewTable(entries)
	if err != nil {
		t.Fatalf("pricing.NewTable: %v", err)
	}
	return pricing.NewRegistry(tab)
}

// reasonsOf is plugin's records on the outbound pass, as action/reason, in order.
func reasonsOf(inv *pipeline.Invocations, plugin string) []string {
	var out []string
	if inv == nil {
		return out
	}
	for _, r := range inv.Outbound {
		if r.Plugin == plugin {
			out = append(out, string(r.Action)+"/"+r.Reason)
		}
	}
	return out
}

// The laptop chain end to end, through the forward proxy: tool-prune and the
// inference-router are two request writers in one chain, which until chained
// request-body writers the framework refused. One Claude Code request carrying a
// tool nobody needs, sent to ete for claude-opus-5-5, is pruned, then routed to
// glm and sent for glm's opus model with glm's key. The session records each
// writer's rewrite and the body-mutation record names both. And tool-prune's
// saving is recorded once, as applied, calibrated on the body actually sent and
// priced at the model the request was sent for — not at Claude Code's name, which
// glm never served.
func TestForwardProxy_ToolPruneAndTheRouterChain(t *testing.T) {
	// About four bytes a token, as English prose runs, so the calibration is a
	// realistic one.
	ete, eteSaw := newBodyOrigin(t, `{"input_tokens":1300,"output_tokens":5}`)
	glm, glmSaw := newBodyOrigin(t, `{"input_tokens":1300,"output_tokens":5}`)
	glmSaw.refuse = "claude-opus-5-5" // glm serves its own models, not Claude Code's name
	eteURL := ete.URL
	glmURL := strings.Replace(glm.URL, "127.0.0.1", "localhost", 1)

	// glm's model list, kept in the plugin store as a previous fetch would have left
	// it, so the router's substitute resolves from the first request.
	st, err := filestore.Open(filepath.Join(t.TempDir(), "plugin-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Set(context.Background(), "inference-router/models/glm", `["glm-big","glm-small"]`, 0); err != nil {
		t.Fatal(err)
	}

	const glmRate, claudeRate = 1e-6, 100e-6
	chain := []config.PluginEntry{
		{Name: "inference-parser"},
		{Name: "tool-prune", Config: json.RawMessage(`{"remove": ["Unused"]}`)},
		{Name: "inference-router", Config: json.RawMessage(`{
			"servers": {
				"ete": {"url": "` + eteURL + `", "key": "ete-key", "main": "opus", "helper": "haiku"},
				"glm": {"url": "` + glmURL + `", "key": "glm-key", "main": "glm-big", "helper": "glm-small"}
			},
			"agents": {"claude-code": "glm"}
		}`)},
	}
	p, err := plugins.BuildWithDeps(chain, plugins.Deps{
		Pricing:  inputRates(t, map[string]float64{"glm-big": glmRate, "claude-opus-5-5": claudeRate}),
		Listener: pipeline.ListenerSupport{Listener: "forward proxy", Destination: true},
		Store:    st,
	})
	if err != nil {
		t.Fatalf("BuildWithDeps: %v", err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop(context.Background())
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	shared := memstore.New()
	defer shared.Close()
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Shared:           shared,
		SessionIDHeaders: []string{session.ClaudeCodeSessionHeader},
		Client:           http.DefaultClient,
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	const keep = `{"name":"Keep","input_schema":{"type":"object"}}`
	unused := `{"name":"Unused","description":"` + strings.Repeat("never called ", 300) + `","input_schema":{"type":"object"}}`
	messages := `"messages":[{"role":"user","content":"` + strings.Repeat("please summarise this ", 230) + `"}]`
	sent := `{"model":"claude-opus-5-5","max_tokens":8,"tools":[` + keep + `,` + unused + `],` + messages + `}`
	want := `{"model":"glm-big","max_tokens":8,"tools":[` + keep + `],` + messages + `}`

	req, err := http.NewRequest(http.MethodPost, eteURL+"/v1/messages", strings.NewReader(sent))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-cli/2.1.286 (external, cli)")
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set(session.ClaudeCodeSessionHeader, "s1")
	resp, err := proxyClient(proxy, nil).Do(req)
	if err != nil {
		t.Fatalf("request through the proxy: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// The wire: glm got the pruned body twice, with its own key: for Claude Code's
	// name, which it refused, and again for its own model, which it answered.
	if bodies, _ := eteSaw.seen(); len(bodies) != 0 {
		t.Errorf("ete received %d requests, want none: the request was routed to glm", len(bodies))
	}
	bodies, keys := glmSaw.seen()
	asked := strings.Replace(want, `"model":"glm-big"`, `"model":"claude-opus-5-5"`, 1)
	if len(bodies) != 2 || bodies[0] != asked || bodies[1] != want {
		t.Fatalf("glm received %q, want %q refused and then %q", bodies, asked, want)
	}
	for _, k := range keys {
		if k != "Bearer glm-key" {
			t.Errorf("glm received Authorization %q, want Bearer glm-key", k)
		}
	}

	// The session: one request event and one response event for the request.
	v := store.View("s1")
	if v == nil {
		t.Fatal("no session s1 recorded")
	}
	// The last of each is the resent attempt's; the refused one is recorded under
	// glm, where it went, with the host the client named beside it.
	var reqEv, respEv, refused *pipeline.SessionEvent
	for i := range v.Events {
		e := &v.Events[i]
		if e.Direction != pipeline.Outbound || e.Inference == nil {
			continue
		}
		switch {
		case e.Phase == pipeline.SessionRequest:
			reqEv = e
		case e.Phase == pipeline.SessionResponse && e.StatusCode == http.StatusForbidden:
			refused = e
		case e.Phase == pipeline.SessionResponse:
			respEv = e
		}
	}
	if refused == nil || refused.Host != strings.TrimPrefix(glmURL, "http://") || refused.RequestedHost != strings.TrimPrefix(eteURL, "http://") {
		t.Errorf("refused row = %+v; want it under glm's host, requested for ete's", refused)
	}
	if reqEv == nil || respEv == nil {
		t.Fatalf("want an outbound inference request and response in s1, got %d events", len(v.Events))
	}

	if got := reasonsOf(reqEv.Invocations, "tool-prune"); !slices.Contains(got, "modify/body_rewritten") {
		t.Errorf("tool-prune recorded %v, want its modify/body_rewritten", got)
	}
	wantRouter := []string{"modify/redirected", "modify/routed", "modify/body_rewritten", "modify/model_rewritten", "modify/resent"}
	if got := reasonsOf(reqEv.Invocations, "inference-router"); !slices.Equal(got, wantRouter) {
		t.Errorf("the router recorded %v, want %v", got, wantRouter)
	}
	var mutation struct {
		Plugin       string   `json:"plugin"`
		Plugins      []string `json:"plugins"`
		LengthBefore int      `json:"length_before"`
		LengthAfter  int      `json:"length_after"`
	}
	if err := json.Unmarshal(reqEv.Plugins["body-mutation"], &mutation); err != nil {
		t.Fatalf("body-mutation record %s: %v", reqEv.Plugins["body-mutation"], err)
	}
	if !slices.Equal(mutation.Plugins, []string{"tool-prune", "inference-router"}) || mutation.Plugin != "inference-router" ||
		mutation.LengthBefore != len(sent) || mutation.LengthAfter != len(want) {
		t.Errorf("body-mutation = %+v; want both writers, from the %d bytes the client sent to the %d sent upstream",
			mutation, len(sent), len(want))
	}
	if ext := respEv.Inference; ext.Model != "glm-big" || ext.RequestedModel != "claude-opus-5-5" {
		t.Errorf("response's inference record has Model %q, RequestedModel %q; want glm-big and claude-opus-5-5",
			ext.Model, ext.RequestedModel)
	}

	// The saving: once, applied, calibrated on the bytes sent, priced at glm-big.
	var savings []event.Saving
	for i := range v.Events {
		if rec, ok := event.Record(&v.Events[i]); ok {
			savings = append(savings, rec.Avoided...)
		}
	}
	if len(savings) != 1 {
		t.Fatalf("savings across the session = %+v, want exactly one", savings)
	}
	s := savings[0]
	var pruned struct {
		BytesRemoved int  `json:"bytesRemoved"`
		Projected    bool `json:"projected"`
	}
	if err := json.Unmarshal(reqEv.Plugins["tool-prune"], &pruned); err != nil {
		t.Fatalf("tool-prune's record %s: %v", reqEv.Plugins["tool-prune"], err)
	}
	if wantRemoved := len(unused) + len(","); pruned.BytesRemoved != wantRemoved {
		t.Errorf("tool-prune removed %d bytes, want %d", pruned.BytesRemoved, wantRemoved)
	}
	wantTokens := pricing.EstimateTokensFromBytes(pruned.BytesRemoved, 1300, len(want))
	if s.Component != "tool-prune" || s.Projected || pruned.Projected || s.TokensAvoided != wantTokens {
		t.Errorf("saving = %+v; want tool-prune's, applied, %d tokens calibrated on the %d bytes sent", s, wantTokens, len(want))
	}
	if wantUSD := float64(wantTokens) * glmRate; s.Tier != pricing.TierInput.String() || math.Abs(s.USD-wantUSD) > 1e-9 {
		t.Errorf("saving priced at $%g in tier %q, want $%g in input: glm-big's rate, not claude-opus-5-5's ($%g)",
			s.USD, s.Tier, wantUSD, float64(wantTokens)*claudeRate)
	}
}
