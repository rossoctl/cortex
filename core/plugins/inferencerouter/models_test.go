package inferencerouter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/memstore"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"

	// The laptop's outbound chain, which the router joins last.
	_ "github.com/rossoctl/cortex/core/plugins/a2aparser"
	_ "github.com/rossoctl/cortex/core/plugins/inferenceparser"
	_ "github.com/rossoctl/cortex/core/plugins/mcpparser"
	_ "github.com/rossoctl/cortex/core/plugins/toolprune"
)

const (
	bobUA = "ai-sdk/openai-compatible/3.0.36 ai-sdk/provider-utils/5.0.29 runtime/node.js/24 bob-shell/2.0.5"

	// glmMain and glmHelper are what glm's words resolve to in glmModels.
	glmMain   = "rits/zai-org/glm-5-3"
	glmHelper = "rits/nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B-NVFP4"
)

// glmModels is glm's model list: an older GLM beside the newest, so a word resolving
// to the newest is seen to.
var glmModels = []string{"rits/zai-org/glm-4-6", glmMain, glmHelper}

// refusal is LiteLLM's answer to a model the key's team may not use.
const refusal = `{"error":{"message":"team not allowed to access model","type":"team_model_access_denied","param":"model","code":"403"}}`

// wordConfig is routerConfig with glm's own main and helper words.
func wordConfig(agents string) string {
	return `{
		"servers": {
			"ete": {"url": "https://ete.example.com", "key": "ete-key", "main": "opus", "helper": "haiku"},
			"glm": {"url": "https://glm.example.com:8443", "key": "glm-key", "main": "glm", "helper": "nemotron"}
		},
		"agents": {` + agents + `}
	}`
}

// buildRouter is build, returning the router too, with glm's model list in place.
func buildRouter(t *testing.T, config string, opts ...pipeline.Option) (*Router, *pipeline.Pipeline) {
	t.Helper()
	r := New()
	if err := r.Configure(json.RawMessage(config)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	r.catalogs.set("glm", glmModels)
	p, err := pipeline.New([]pipeline.Plugin{r}, opts...)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	return r, p
}

// chat is a request for model to host at path, in session, from the agent with
// User-Agent ua, with tools when tools is true, as inference-parser would have read it.
func chat(store *memstore.Store, host, path, ua, session, model string, tools bool) *pipeline.Context {
	var shared pipeline.SharedStore // a nil *memstore.Store is not a nil store
	if store != nil {
		shared = store
	}
	pctx := request(shared, host, ua, session)
	pctx.Path = path
	pctx.Body = []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`)
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: model}
	if tools {
		pctx.Extensions.Inference.Tools = []pipeline.InferenceTool{{Name: "Write"}}
	}
	return pctx
}

func sentModel(t *testing.T, pctx *pipeline.Context) string {
	t.Helper()
	m, ok := pctx.RequestModel()
	if !ok {
		t.Fatal("the body names no model")
	}
	return m
}

func outboundReasons(pctx *pipeline.Context) []string {
	var out []string
	if pctx.Extensions.Invocations == nil {
		return out
	}
	for _, inv := range pctx.Extensions.Invocations.Outbound {
		out = append(out, string(inv.Action)+"/"+inv.Reason)
	}
	return out
}

// A routed agent's inference request is sent to its server wherever the agent
// addressed it, at the server's path for its API format, with the server's key.
func TestRouter_CapturesARoutedAgentsInferenceWhereverItWasAddressed(t *testing.T) {
	for _, tc := range []struct{ name, ua, host, path, want string }{
		{"OpenCode Zen chat completions", opencodeUA, "opencode.ai", "/zen/v1/chat/completions", "/v1/chat/completions"},
		{"OpenCode Zen Claude", opencodeUA, "opencode.ai", "/zen/v1/messages", "/v1/messages"},
		{"OpenCode Zen GPT", opencodeUA, "opencode.ai", "/zen/v1/responses", "/v1/responses"},
		{"Bob Shell", bobUA, "api.us-east.bob.ibm.com", "/inference/v1/chat/completions", "/v1/chat/completions"},
		{"Claude Code on Anthropic", claudeUA, "api.anthropic.com", "/v1/messages", "/v1/messages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			_, p := buildRouter(t, wordConfig(`"claude-code": "glm", "opencode": "glm", "bob-shell": "glm"`))
			pctx := chat(store, tc.host, tc.path, tc.ua, "s1", "exo-free", true)
			if a := run(t, p, pctx); a.Type != pipeline.Continue {
				t.Fatalf("action = %+v", a)
			}
			assertRouted(t, pctx, glmHost, "glm-key")
			if path, ok := pctx.RedirectPath(); !ok || path != tc.want || pctx.Path != tc.want {
				t.Errorf("RedirectPath() = %q, %v and Path %q; want %s", path, ok, pctx.Path, tc.want)
			}
			if got := pctx.RequestedHost(); got != tc.host {
				t.Errorf("RequestedHost() = %q, want the host the agent named", got)
			}
			assertRecord(t, pctx, pipeline.ActionModify, "routed", map[string]string{"server": "glm", "pin": pinNew, "captured": tc.want})
			if sentModel(t, pctx) != "exo-free" {
				t.Errorf("model = %q, want the agent's own on a first try", sentModel(t, pctx))
			}
		})
	}
}

// Only a request inference-parser read as inference is captured. An agent's own tools
// can POST to an API whose path ends the same way, and its body and the server's key
// must not go to the inference server.
func TestRouter_CapturesOnlyAnInferenceRequest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ua     string
		mutate func(*pipeline.Context)
	}{
		{"no inference parse", opencodeUA, func(c *pipeline.Context) { c.Extensions.Inference = nil }},
		{"not a POST", opencodeUA, func(c *pipeline.Context) { c.Method = http.MethodGet }},
		{"another path", opencodeUA, func(c *pipeline.Context) { c.Path = "/api/v1/send" }},
		{"an agent not routed", "curl/8.7.1", func(*pipeline.Context) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			_, p := buildRouter(t, wordConfig(`"opencode": "glm"`))
			pctx := chat(store, "graph.example.com", "/v1.0/me/messages", tc.ua, "s1", "m", true)
			tc.mutate(pctx)
			run(t, p, pctx)
			assertUntouched(t, pctx, "graph.example.com")
			assertRecord(t, pctx, pipeline.ActionSkip, "not_an_inference_server", nil)
			if _, ok := pinOf(t, store, "s1"); ok {
				t.Error("a request the router did not capture pinned its session")
			}
		})
	}
}

// The server's own answer decides whether it serves a name, not its model list,
// which leaves out aliases: the agent's own model always goes first.
func TestRouter_SendsTheAgentsOwnModelFirst(t *testing.T) {
	_, p := buildRouter(t, wordConfig(`"claude-code": "glm"`))
	pctx := chat(newStore(t), eteHost, "/v1/messages", claudeUA, "s1", "claude-haiku-4-5", false)
	run(t, p, pctx)
	if got := sentModel(t, pctx); got != "claude-haiku-4-5" {
		t.Errorf("model = %q, want the agent's own", got)
	}
	if slices.Contains(outboundReasons(pctx), "modify/model_rewritten") {
		t.Errorf("timeline = %v, want no rewrite", outboundReasons(pctx))
	}
}

// resend runs the router's Resend as the forward proxy does after an answer.
func resend(p *pipeline.Pipeline, pctx *pipeline.Context, status int, body string) bool {
	return p.Resend(context.Background(), pctx, status, []byte(body))
}

func TestResend_SendsARefusedModelAsTheServersMainOrHelper(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tools bool
		want  string
	}{
		{"a request with tools gets main", true, glmMain},
		{"a request without tools gets helper", false, glmHelper},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			_, p := buildRouter(t, wordConfig(`"opencode": "glm"`))
			pctx := chat(store, "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s1", "exo-free", tc.tools)
			run(t, p, pctx)

			if !resend(p, pctx, http.StatusForbidden, refusal) {
				t.Fatal("Resend = false for a refused model with a substitute to send")
			}
			if got := sentModel(t, pctx); got != tc.want {
				t.Errorf("model = %q, want %s", got, tc.want)
			}
			assertRecord(t, pctx, pipeline.ActionModify, "resent",
				map[string]string{"server": "glm", "model": "exo-free", "substitute": tc.want})
			if got := pctx.Headers.Get("Authorization"); got != "Bearer glm-key" {
				t.Errorf("Authorization = %q, want glm's key still", got)
			}
		})
	}
}

// Only the server's refusal of the model sends a request again: anything else is the
// agent's to see, and a request already sent with a substitute is never sent twice.
func TestResend_OnlyForAModelTheServerRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"a bad key", http.StatusUnauthorized, `{"error":{"type":"token_not_found_in_db"}}`},
		{"another 403", http.StatusForbidden, `{"error":{"type":"budget_exceeded"}}`},
		{"a server error", http.StatusInternalServerError, refusal},
		{"no JSON", http.StatusForbidden, `forbidden`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, p := buildRouter(t, wordConfig(`"opencode": "glm"`))
			pctx := chat(newStore(t), "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s1", "exo-free", true)
			run(t, p, pctx)
			if resend(p, pctx, tc.status, tc.body) {
				t.Errorf("Resend = true for %d %s", tc.status, tc.body)
			}
			if got := sentModel(t, pctx); got != "exo-free" {
				t.Errorf("model = %q, want it unchanged", got)
			}
		})
	}
	t.Run("a substitute", func(t *testing.T) {
		_, p := buildRouter(t, wordConfig(`"opencode": "glm"`))
		pctx := chat(newStore(t), "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s1", "exo-free", true)
		run(t, p, pctx)
		resend(p, pctx, http.StatusForbidden, refusal)
		if resend(p, pctx, http.StatusForbidden, refusal) {
			t.Error("Resend = true for a request already sent with a substitute")
		}
	})
}

// With no list yet, or a word naming nothing on it, there is nothing to send instead:
// the refusal reaches the agent, and the router says why.
func TestResend_WithNoSubstituteTheRefusalGoesToTheAgent(t *testing.T) {
	r, p := buildRouter(t, wordConfig(`"opencode": "glm"`))
	r.catalogs.set("glm", nil)
	pctx := chat(newStore(t), "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s1", "exo-free", true)
	run(t, p, pctx)
	if resend(p, pctx, http.StatusForbidden, refusal) {
		t.Fatal("Resend = true with no model list")
	}
	assertRecord(t, pctx, pipeline.ActionSkip, "no_substitute", map[string]string{"server": "glm", "model": "exo-free"})
}

// Once a server has refused a name, the next request for it is sent with the
// substitute from the start, with no refused round trip, by any session.
func TestRouter_ARefusedModelIsSubstitutedUpFront(t *testing.T) {
	store := newStore(t)
	_, p := buildRouter(t, wordConfig(`"opencode": "glm"`))
	first := chat(store, "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s1", "exo-free", true)
	run(t, p, first)
	resend(p, first, http.StatusForbidden, refusal)

	next := chat(store, "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s2", "exo-free", true)
	run(t, p, next)
	if got := sentModel(t, next); got != glmMain {
		t.Errorf("model = %q, want the substitute up front", got)
	}
	if ext := next.Extensions.Inference; ext.RequestedModel != "exo-free" {
		t.Errorf("RequestedModel = %q, want the agent's name kept for the row", ext.RequestedModel)
	}
}

// A session keeps the substitute it started with, so a newer model on the server's
// list moves only the sessions that have not started yet.
func TestRouter_ASessionKeepsItsSubstitute(t *testing.T) {
	store := newStore(t)
	r, p := buildRouter(t, wordConfig(`"opencode": "glm"`))
	first := chat(store, "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s1", "exo-free", true)
	run(t, p, first)
	resend(p, first, http.StatusForbidden, refusal)

	r.catalogs.set("glm", append([]string{"rits/zai-org/glm-6"}, glmModels...))
	same := chat(store, "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s1", "exo-free", true)
	run(t, p, same)
	if got := sentModel(t, same); got != glmMain {
		t.Errorf("the running session sends %q, want the substitute it started with", got)
	}
	other := chat(store, "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s2", "exo-free", true)
	run(t, p, other)
	if got := sentModel(t, other); got != "rits/zai-org/glm-6" {
		t.Errorf("a new session sends %q, want the newest glm", got)
	}
}

// Under observe nothing moves, so no model is chosen either.
func TestRouter_UnderObserveChoosesNoModel(t *testing.T) {
	store := newStore(t)
	_, p := buildRouter(t, wordConfig(`"opencode": "glm"`), pipeline.WithPolicies(pipeline.ErrorPolicyObserve))
	pctx := chat(store, glmHost, "/v1/chat/completions", opencodeUA, "s1", "exo-free", true)
	run(t, p, pctx)
	if got := sentModel(t, pctx); got != "exo-free" || pctx.BodyMutated() {
		t.Errorf("model = %q, BodyMutated = %v; want nothing changed under observe", got, pctx.BodyMutated())
	}
	if p.HasResenders() {
		t.Error("a router under observe is asked to resend")
	}
}

// fakeStore is a durable store that records each write's TTL.
type fakeStore struct {
	mu   sync.Mutex
	vals map[string]string
	ttls map[string]time.Duration
}

func (f *fakeStore) Get(_ context.Context, k string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.vals[k], nil
}
func (f *fakeStore) Set(_ context.Context, k, v string, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.vals == nil {
		f.vals, f.ttls = map[string]string{}, map[string]time.Duration{}
	}
	f.vals[k], f.ttls[k] = v, ttl
	return nil
}
func (f *fakeStore) Incr(context.Context, string, int64) (int64, error)             { return 0, nil }
func (f *fakeStore) HashIncr(context.Context, string, string, int64) (int64, error) { return 0, nil }
func (f *fakeStore) HashGet(context.Context, string) (map[string]string, error)     { return nil, nil }
func (f *fakeStore) HashSetNX(context.Context, string, string, string) (bool, error) {
	return false, nil
}
func (f *fakeStore) Expire(context.Context, string, time.Duration) error { return nil }
func (f *fakeStore) Close() error                                        { return nil }

// A refusal is believed for an hour and no longer, and kept where pins are, so a
// restart keeps it; a session's substitutes are kept as long as its pin.
func TestRouter_KeepsRefusalsAndSubstitutesInTheDurableStore(t *testing.T) {
	st := &fakeStore{}
	r := New()
	r.SetStore(st)
	if err := r.Configure(json.RawMessage(wordConfig(`"opencode": "glm"`))); err != nil {
		t.Fatal(err)
	}
	r.catalogs.set("glm", glmModels)
	p, _ := pipeline.New([]pipeline.Plugin{r})
	pctx := chat(nil, "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s1", "exo-free", true)
	run(t, p, pctx)
	resend(p, pctx, http.StatusForbidden, refusal)

	if ttl := st.ttls[refusedPrefix+"glm/exo-free"]; ttl != refusedFor || refusedFor != time.Hour {
		t.Errorf("refusal TTL = %v, want an hour", ttl)
	}
	key := substitutePrefix + "s1/opencode/glm"
	if st.ttls[key] != pinTTL || !strings.Contains(st.vals[key], glmMain) {
		t.Errorf("substitute %q kept for %v, want %s for the pin's TTL", st.vals[key], st.ttls[key], glmMain)
	}
}

// The real outbound chain parses an OpenCode Zen request and the router captures it.
func TestRouter_TheParserAndTheRouterCaptureAZenRequest(t *testing.T) {
	p, err := plugins.BuildWithDeps(entries(wordConfig(`"opencode": "glm"`)),
		plugins.Deps{Listener: pipeline.ListenerSupport{Listener: "forward proxy", Destination: true}})
	if err != nil {
		t.Fatalf("BuildWithDeps: %v", err)
	}
	pctx := request(newStore(t), "opencode.ai", opencodeUA, "s1")
	pctx.Extensions.Inference = nil
	pctx.Path = "/zen/v1/chat/completions"
	pctx.Body = []byte(`{"model":"nemotron-3.5-lightning-free","messages":[{"role":"user","content":"hi"}],"tools":[]}`)
	run(t, p, pctx)
	assertRouted(t, pctx, glmHost, "glm-key")
	if pctx.Path != "/v1/chat/completions" {
		t.Errorf("Path = %q, want glm's", pctx.Path)
	}
}

// Init reads each server's list back from the store, so a substitute resolves before
// the first fetch answers; the fetch then replaces it and keeps it.
func TestInit_FetchesEachServersModelsAndKeepsThem(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer glm-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"rits/zai-org/glm-6"},{"id":"rits/nvidia/nemotron-4"}]}`))
	}))
	defer srv.Close()

	st := &fakeStore{}
	_ = st.Set(context.Background(), catalogPrefix+"glm", `["rits/zai-org/glm-5-3"]`, 0)
	r := New()
	r.SetStore(st)
	r.client = srv.Client()
	cfg := `{"servers": {"glm": {"url": "` + srv.URL + `", "key": "glm-key", "main": "glm", "helper": "nemotron"}}}`
	if err := r.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatal(err)
	}
	if err := r.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Shutdown(context.Background()) }()

	deadline := time.Now().Add(5 * time.Second)
	for routerconfig.Resolve("glm", r.catalogs.get("glm")) != "rits/zai-org/glm-6" {
		if time.Now().After(deadline) {
			t.Fatalf("list = %v, want the fetched one", r.catalogs.get("glm"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if v, _ := st.Get(context.Background(), catalogPrefix+"glm"); !strings.Contains(v, "glm-6") {
		t.Errorf("stored list = %s, want the fetched one kept", v)
	}
}

// On a captured request the provider's credential headers below are dropped, the
// server's key goes in the header the agent sent its own in, and the other headers
// below go as they came.
func TestRouter_ACapturedRequestCarriesNoCredentialOfTheHostItAddressed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		apiKey   bool // the agent authenticated with X-Api-Key rather than a bearer token
		wantAuth string
		wantKey  string
	}{
		{"a bearer token", false, "Bearer glm-key", ""},
		{"an X-Api-Key", true, "", "glm-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, p := buildRouter(t, wordConfig(`"opencode": "glm"`))
			pctx := chat(newStore(t), "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s1", "exo-free", true)
			if tc.apiKey {
				pctx.Headers.Del("Authorization")
				pctx.Headers.Set("X-Api-Key", "provider-key")
			}
			provider := map[string]string{
				"Api-Key":                   "azure-key",
				"X-Goog-Api-Key":            "google-key",
				"Cookie":                    "session=provider",
				"X-Auth-Token":              "gateway-token",
				"Ocp-Apim-Subscription-Key": "apim-key",
				"X-Request-Signature":       "hmac",
				"X-Client-Secret":           "secret",
			}
			for k, v := range provider {
				pctx.Headers.Set(k, v)
			}
			benign := map[string]string{"Content-Type": "application/json", "Anthropic-Version": "2023-06-01", "X-Team-Id": "t1"}
			for k, v := range benign {
				pctx.Headers.Set(k, v)
			}
			run(t, p, pctx)

			for k := range provider {
				if v := pctx.Headers.Get(k); v != "" {
					t.Errorf("%s = %q reaches the server; it was the provider's", k, v)
				}
			}
			for k, v := range benign {
				if got := pctx.Headers.Get(k); got != v {
					t.Errorf("%s = %q, want %q kept", k, got, v)
				}
			}
			if got := pctx.Headers.Get("Authorization"); got != tc.wantAuth {
				t.Errorf("Authorization = %q, want %q", got, tc.wantAuth)
			}
			if got := pctx.Headers.Get("X-Api-Key"); got != tc.wantKey {
				t.Errorf("X-Api-Key = %q, want %q", got, tc.wantKey)
			}
		})
	}
}

// A request the agent addressed to a server's own host carries what the agent meant
// for that server, and only the key is replaced.
func TestRouter_ARequestAddressedToAServerKeepsItsOtherHeaders(t *testing.T) {
	_, p := buildRouter(t, wordConfig(`"opencode": "glm"`))
	pctx := chat(newStore(t), glmHost, "/v1/chat/completions", opencodeUA, "s1", "exo-free", true)
	pctx.Headers.Set("Cookie", "meant=for-glm")
	run(t, p, pctx)
	if got := pctx.Headers.Get("Cookie"); got != "meant=for-glm" {
		t.Errorf("Cookie = %q, want it kept: the agent addressed glm", got)
	}
	assertRouted(t, pctx, glmHost, "glm-key")
}

// A request addressed to one server and routed to another goes to a host it did not
// address, as a captured one does: the first server's credential headers stay behind.
func TestRouter_ARequestRoutedToAnotherServerLeavesTheFirstServersCredentials(t *testing.T) {
	_, p := buildRouter(t, wordConfig(`"opencode": "glm"`))
	pctx := chat(newStore(t), eteHost, "/v1/chat/completions", opencodeUA, "s1", "exo-free", true)
	pctx.Headers.Set("Cookie", "meant=for-ete")
	pctx.Headers.Set("X-Litellm-Api-Key", "ete-custom")
	pctx.Headers.Set("Anthropic-Version", "2023-06-01")
	run(t, p, pctx)
	assertRouted(t, pctx, glmHost, "glm-key")
	for _, k := range []string{"Cookie", "X-Litellm-Api-Key"} {
		if v := pctx.Headers.Get(k); v != "" {
			t.Errorf("%s = %q reaches glm; it was ete's", k, v)
		}
	}
	if got := pctx.Headers.Get("Anthropic-Version"); got != "2023-06-01" {
		t.Errorf("Anthropic-Version = %q, want it kept", got)
	}
}

// A session pinned to a server stays on it when its agent is unrouted, captured or
// not, as one addressed to a server's host does. A session of an unrouted agent that
// holds no pin is left where the agent sent it, and is not pinned.
func TestRouter_UnroutingAnAgentDoesNotMoveItsCapturedSessions(t *testing.T) {
	store := newStore(t)
	_, routed := buildRouter(t, wordConfig(`"opencode": "glm"`))
	first := chat(store, "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s1", "exo-free", true)
	run(t, routed, first)
	assertRouted(t, first, glmHost, "glm-key")

	_, unrouted := buildRouter(t, wordConfig(``)) // agentop server reset --agent opencode
	next := chat(store, "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s1", "exo-free", true)
	run(t, unrouted, next)
	assertRouted(t, next, glmHost, "glm-key")
	assertRecord(t, next, pipeline.ActionModify, "routed", map[string]string{"server": "glm", "pin": pinExisting})

	other := chat(store, "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s2", "exo-free", true)
	other.Headers.Set("Authorization", "Bearer client-key")
	run(t, unrouted, other)
	assertUntouched(t, other, "opencode.ai")
	if _, ok := pinOf(t, store, "s2"); ok {
		t.Error("an unrouted agent's session was pinned")
	}
}

// Each request's kind decides its substitute, main with tools and helper without, and
// a session keeps one for each: whichever kind is refused first, the other still gets
// its own.
func TestRouter_ASessionKeepsASubstituteForEachKindOfRequest(t *testing.T) {
	for _, order := range [][]bool{{false, true}, {true, false}} {
		store := newStore(t)
		_, p := buildRouter(t, wordConfig(`"opencode": "glm"`))
		want := map[bool]string{true: glmMain, false: glmHelper}
		for _, tools := range order {
			pctx := chat(store, "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s1", "exo-free", tools)
			run(t, p, pctx)
			if sentModel(t, pctx) == "exo-free" {
				resend(p, pctx, http.StatusForbidden, refusal)
			}
			if got := sentModel(t, pctx); got != want[tools] {
				t.Errorf("order %v: a request with tools=%v is sent as %q, want %q", order, tools, got, want[tools])
			}
		}
	}
}

// A Claude Code side request captured from its provider in a session the router has
// not pinned goes to the agent's server and decides nothing: left where Claude Code
// sent it, a title or a subagent's request would reach the provider the agent was
// routed away from, with the user's own credential. A captured continuation, a
// conversation already running on the provider, still stays there.
func TestRouter_ACapturedSideRequestGoesToTheAgentsServer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		role  pipeline.AgentRole
		tools int
	}{
		{"a title or classifier call", pipeline.AgentRoleMain, 0},
		{"a subagent's request", pipeline.AgentRoleSubagent, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			_, p := buildRouter(t, wordConfig(`"claude-code": "glm"`))
			pctx := claudeCode(request(store, "api.anthropic.com", claudeUA, "s1"), tc.role, tc.tools, 0)
			pctx.Body = []byte(`{"model":"claude-haiku-4-5","messages":[]}`)
			run(t, p, pctx)
			assertRouted(t, pctx, glmHost, "glm-key")
			if _, ok := pinOf(t, store, "s1"); ok {
				t.Error("a side request pinned its session")
			}
		})
	}
	t.Run("a continuation stays on the provider", func(t *testing.T) {
		store := newStore(t)
		_, p := buildRouter(t, wordConfig(`"claude-code": "glm"`))
		pctx := claudeCode(request(store, "api.anthropic.com", claudeUA, "s1"), pipeline.AgentRoleMain, 3, 2)
		run(t, p, pctx)
		assertUntouched(t, pctx, "api.anthropic.com")
		if pin, ok := pinOf(t, store, "s1"); !ok || pin != "" {
			t.Errorf("pin = %q, %v; want the session pinned as not routed", pin, ok)
		}
	})
}

// An unrestricted LiteLLM key's answer to a model the server lacks is a 400 naming it
// invalid, as a team-restricted key's is a 403: either is a refusal of the model.
func TestResend_TreatsAnUnrestrictedKeysInvalidModelAsARefusal(t *testing.T) {
	for _, body := range []string{
		"{\"error\":\"/v1/messages: Invalid model name passed in model=exo-free. Call `/v1/models` to view available models for your key.\"}",
		"{\"error\":{\"message\":\"/chat/completions: Invalid model name passed in model=exo-free. Call `/v1/models` to view available models for your key.\"}}",
	} {
		_, p := buildRouter(t, wordConfig(`"opencode": "glm"`))
		pctx := chat(newStore(t), "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s1", "exo-free", true)
		run(t, p, pctx)
		if !resend(p, pctx, http.StatusBadRequest, body) {
			t.Errorf("Resend = false for %s", body)
			continue
		}
		if got := sentModel(t, pctx); got != glmMain {
			t.Errorf("model = %q, want %s", got, glmMain)
		}
	}
	_, p := buildRouter(t, wordConfig(`"opencode": "glm"`))
	pctx := chat(newStore(t), "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s1", "exo-free", true)
	run(t, p, pctx)
	if resend(p, pctx, http.StatusBadRequest, `{"error":{"message":"max_tokens is too large"}}`) {
		t.Error("Resend = true for a 400 of another kind")
	}
}
