package inferencerouter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/memstore"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
	"github.com/tidwall/gjson"

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
	// expired is every key Expire renewed, in order.
	expired []string
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
func (f *fakeStore) Expire(_ context.Context, k string, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.vals[k]; ok {
		f.ttls[k] = ttl
		f.expired = append(f.expired, k)
	}
	return nil
}
func (f *fakeStore) Close() error { return nil }

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

const bedrockCap = `{"error":{"message":"litellm.BadRequestError: BedrockException - {\"message\":\"The maximum tokens you requested exceeds the model limit of 128000. Try again with a maximum tokens value that is lower than 128000.\"}","type":null,"param":null,"code":"400"}}`

// withOutput gives pctx's body an output field, as OpenCode sends it.
func withOutput(t *testing.T, pctx *pipeline.Context, n int) {
	t.Helper()
	m, _ := pctx.RequestModel()
	pctx.Body = []byte(`{"model":"` + m + `","max_completion_tokens":` + strconv.Itoa(n) + `,"messages":[{"role":"user","content":"hi"}]}`)
}

func TestResend_LearnsAnOutputCapAndSendsTheRequestCapped(t *testing.T) {
	store := newStore(t)
	_, p := buildRouter(t, wordConfig(`"opencode": "glm"`))
	pctx := chat(store, "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s1", glmMain, true)
	withOutput(t, pctx, 256000)
	run(t, p, pctx)

	if !resend(p, pctx, http.StatusBadRequest, bedrockCap) {
		t.Fatal("Resend = false for a refusal that states the output cap")
	}
	if got := gjson.GetBytes(pctx.Body, "max_completion_tokens").Int(); got != 128000 {
		t.Errorf("max_completion_tokens = %d, want 128000", got)
	}
	assertRecord(t, pctx, pipeline.ActionModify, "resent",
		map[string]string{"server": "glm", "model": glmMain, "limit": "output", "value": "128000", "stated": "true"})

	// The next request of the session is capped before it is sent.
	next := chat(store, "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s1", glmMain, true)
	withOutput(t, next, 256000)
	run(t, p, next)
	if got := gjson.GetBytes(next.Body, "max_completion_tokens").Int(); got != 128000 {
		t.Errorf("next request's max_completion_tokens = %d, want 128000 before sending", got)
	}
	// applyLimits records before the router's modify/routed, so find it by reason.
	var capped *pipeline.Invocation
	for i, inv := range next.Extensions.Invocations.Outbound {
		if inv.Plugin == Name && inv.Reason == "output_capped" {
			capped = &next.Extensions.Invocations.Outbound[i]
		}
	}
	if capped == nil {
		t.Fatalf("timeline = %v, want modify/output_capped", outboundReasons(next))
	}
	for k, want := range map[string]string{"field": "max_completion_tokens", "from": "256000", "to": "128000"} {
		if got := capped.Details[k]; got != want {
			t.Errorf("output_capped %s = %q, want %q", k, got, want)
		}
	}
}

// A first request can be refused twice — the model, then a limit — and the agent
// sees neither; a third refusal is the agent's.
func TestResend_AModelRefusalThenALimitRefusalAreBothAbsorbed(t *testing.T) {
	store := newStore(t)
	_, p := buildRouter(t, wordConfig(`"opencode": "glm"`))
	pctx := chat(store, "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s1", "exo-free", true)
	withOutput(t, pctx, 256000)
	run(t, p, pctx)

	if !resend(p, pctx, http.StatusForbidden, refusal) {
		t.Fatal("the model refusal was not resent")
	}
	if !resend(p, pctx, http.StatusBadRequest, bedrockCap) {
		t.Fatal("the limit refusal after a model resend was not resent")
	}
	if got := pctx.ResendReasons(); !slices.Equal(got, []string{"model", "limit"}) {
		t.Errorf("ResendReasons = %v, want [model limit]", got)
	}
	if resend(p, pctx, http.StatusBadRequest, bedrockCap) {
		t.Error("a second limit refusal was resent; it must reach the agent")
	}
}

func TestRouter_LeavesARequestItDidNotRouteAlone(t *testing.T) {
	store := newStore(t)
	r, p := buildRouter(t, wordConfig(`"opencode": "glm"`))
	pctx := chat(store, "glm.example.com", "/v1/chat/completions", "curl/8.0", "s1", glmMain, true)
	r.saveLimits(pctx, "glm", glmMain, limits{Output: 1000, Window: 100})
	withOutput(t, pctx, 256000)
	before := string(pctx.Body)
	run(t, p, pctx)
	if string(pctx.Body) != before {
		t.Errorf("body = %s, want an unrouted request unchanged", pctx.Body)
	}
	if resend(p, pctx, http.StatusBadRequest, bedrockCap) {
		t.Error("an unrouted request was resent")
	}
}

func TestRouter_TrimsARoutedConversationNearTheWindow(t *testing.T) {
	store := newStore(t)
	r, p := buildRouter(t, wordConfig(`"opencode": "glm"`))
	pctx := chat(store, "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s1", glmMain, true)
	r.saveLimits(pctx, "glm", glmMain, limits{Window: 1000})
	long := []string{user("the task")}
	for i := 0; i < 6; i++ {
		long = append(long, asst(pad), user(pad))
	}
	pctx.Body = []byte(`{"model":"` + glmMain + `","messages":[` + strings.Join(long, ",") + `]}`)
	run(t, p, pctx)

	if n := len(gjson.GetBytes(pctx.Body, "messages").Array()); n >= len(long) {
		t.Errorf("messages = %d, want fewer than the %d sent", n, len(long))
	}
	var trimmed bool
	for _, reason := range outboundReasons(pctx) {
		trimmed = trimmed || reason == "modify/context_trimmed"
	}
	if !trimmed {
		t.Errorf("timeline = %v, want modify/context_trimmed", outboundReasons(pctx))
	}
}

const bedrockTooLong = `{"error":{"message":"litellm.BadRequestError: BedrockException - {\"message\":\"Input is too long for requested model.\"}","type":null,"param":null,"code":"400"}}`

func TestResend_ALimitRefusalStatingNoNumberLearnsAWindowBelowTheRequest(t *testing.T) {
	store := newStore(t)
	r, p := buildRouter(t, wordConfig(`"opencode": "glm"`))
	pctx := chat(store, "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s1", glmMain, true)
	var msgs []string
	for i := 0; i < 40; i++ {
		msgs = append(msgs, `{"role":"user","content":"`+pad+`"}`, `{"role":"assistant","content":"`+pad+`"}`)
	}
	msgs = append(msgs, `{"role":"user","content":"last"}`)
	pctx.Body = []byte(`{"model":"` + glmMain + `","max_completion_tokens":1000,"messages":[` + strings.Join(msgs, ",") + `]}`)
	run(t, p, pctx)

	refused := append([]byte(nil), pctx.Body...)
	if !resend(p, pctx, http.StatusBadRequest, bedrockTooLong) {
		t.Fatal("Resend = false for a limit refusal that states no number")
	}
	want := estimate(refused) * 9 / 10
	assertRecord(t, pctx, pipeline.ActionModify, "resent",
		map[string]string{"server": "glm", "model": glmMain, "limit": "window", "value": strconv.Itoa(want), "stated": "false"})
	if got := r.loadLimits(pctx, "glm", glmMain).Window; got != want {
		t.Errorf("learned window = %d, want %d", got, want)
	}
}

// Two conversations in one session — the main thread and a subagent — each keep their
// own cut, so one's recut does not move the other's.
func TestRouter_EachConversationKeepsItsOwnCut(t *testing.T) {
	store := newStore(t)
	r, p := buildRouter(t, wordConfig(`"opencode": "glm"`))
	send := func(first string, turns int, text string) *pipeline.Context {
		pctx := chat(store, "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s1", glmMain, true)
		msgs := []string{user(first)}
		for i := 0; i < turns; i++ {
			msgs = append(msgs, asst(text), user(text))
		}
		pctx.Body = []byte(`{"model":"` + glmMain + `","messages":[` + strings.Join(msgs, ",") + `]}`)
		r.saveLimits(pctx, "glm", glmMain, limits{Window: 1000})
		run(t, p, pctx)
		return pctx
	}
	dropped := func(pctx *pipeline.Context) string {
		for _, inv := range pctx.Extensions.Invocations.Outbound {
			if inv.Reason == "context_trimmed" {
				return inv.Details["dropped"]
			}
		}
		return ""
	}
	// The subagent's messages are longer, so its cut lands later than the main
	// thread's — at an index that is a valid cut in the main thread too.
	main1 := dropped(send("the main task", 9, pad))
	send("a subagent's task", 9, pad+pad)
	if main2 := dropped(send("the main task", 9, pad)); main2 != main1 {
		t.Errorf("main thread dropped %s after a subagent's request, want its own cut kept (%s)", main2, main1)
	}
}

// An agent that talks to a server serving its own model, at the host it named, was
// not switched: the router leaves its limits to the agent.
func TestRouter_DoesNotFitARequestThatWasNotSwitched(t *testing.T) {
	store := newStore(t)
	r, p := buildRouter(t, wordConfig(`"opencode": "glm"`))
	pctx := chat(store, "glm.example.com:8443", "/v1/chat/completions", opencodeUA, "s1", glmMain, true)
	r.saveLimits(pctx, "glm", glmMain, limits{Output: 1000, Window: 100})
	withOutput(t, pctx, 256000)
	before := string(pctx.Body)
	run(t, p, pctx)
	if !pctx.Redirected() {
		t.Fatal("the request was not routed; the test needs a routed request that was not switched")
	}
	if string(pctx.Body) != before {
		t.Errorf("body = %s, want a request that was not switched unchanged", pctx.Body)
	}
	if resend(p, pctx, http.StatusBadRequest, bedrockCap) {
		t.Error("a limit refusal of a request that was not switched was resent")
	}
}

// A limit is read from the server's error message, and only a number that makes sense
// for the refused request is learned.
func TestResend_LearnsOnlyAPlausibleLimitFromTheErrorMessage(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"a limit quoted from the request's own text", `{"error":{"message":"invalid request","detail":[{"input":"maximum context length is 64 tokens"}]}}`},
		{"a window below any real model's", `{"error":{"message":"This model's maximum context length is 64 tokens."}}`},
		{"an output cap below any real model's", `{"error":{"message":"The maximum tokens you requested exceeds the model limit of 5."}}`},
		{"an output cap above what was asked for", `{"error":{"message":"The maximum tokens you requested exceeds the model limit of 300000."}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			r, p := buildRouter(t, wordConfig(`"opencode": "glm"`))
			pctx := chat(store, "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s1", glmMain, true)
			withOutput(t, pctx, 256000)
			run(t, p, pctx)
			if resend(p, pctx, http.StatusBadRequest, tc.body) {
				t.Error("the request was resent on an implausible limit")
			}
			if got := r.loadLimits(pctx, "glm", glmMain); got != (limits{}) {
				t.Errorf("learned %+v, want nothing", got)
			}
		})
	}
}

// Local model servers refuse a model they do not have in their own words. Each body
// is a server's answer as recorded on 2026-10-15 (see the spec), and each refuses the
// name it carries.
func TestRefusesModel_ReadsEachLocalServersWording(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		model  string
		body   string
	}{
		{"ollama chat completions", 404, "premium-ide", `{"error":{"message":"model 'premium-ide' not found","type":"not_found_error","param":null,"code":null}}`},
		{"ollama 0.7.0 chat completions", 404, "premium-ide", `{"error":{"message":"model \"premium-ide\" not found, try pulling it first","type":"api_error","param":null,"code":null}}`},
		{"ollama messages", 404, "premium-ide", `{"type":"error","error":{"type":"not_found_error","message":"model 'premium-ide' not found"},"request_id":"req_0291eb92e74aac2c0064755e"}`},
		{"ollama responses", 404, "claude-opus-5-5", `{"error":{"message":"model 'claude-opus-5-5' not found","type":"not_found_error","param":null,"code":null}}`},
		{"ollama, a name with a tag and a namespace", 404, "ibm/granite4:latest", `{"error":{"message":"model 'ibm/granite4:latest' not found","type":"not_found_error","param":null,"code":null}}`},
		{"ollama, a name with a percent sign", 404, "100%-local", `{"error":{"message":"model '100%-local' not found","type":"not_found_error","param":null,"code":null}}`},
		{"llama.cpp with --models-dir", 400, "premium-ide", `{"error":{"code":400,"message":"model 'premium-ide' not found","type":"invalid_request_error"}}`},
		{"vllm chat completions and responses", 404, "premium-ide", "{\"error\":{\"message\":\"The model `premium-ide` does not exist.\",\"type\":\"NotFoundError\",\"param\":\"model\",\"code\":404}}"},
		{"vllm messages", 404, "premium-ide", "{\"type\":\"error\",\"error\":{\"type\":\"NotFoundError\",\"message\":\"The model `premium-ide` does not exist.\"}}"},
		{"litellm, a team-restricted key", 403, "exo-free", refusal},
		{"litellm, an unrestricted key", 400, "exo-free", `{"error":{"message":"/chat/completions: Invalid model name passed in model=exo-free."}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !refusesModel(tc.status, []byte(tc.body), tc.model) {
				t.Errorf("refusesModel(%d, %s, %q) = false, want a refusal", tc.status, tc.body, tc.model)
			}
		})
	}
}

// What is not a refusal of the model sent reaches the agent as the server answered it.
func TestRefusesModel_OtherAnswersAreNotRefusals(t *testing.T) {
	ollama := `{"error":{"message":"model 'premium-ide' not found","type":"not_found_error","param":null,"code":null}}`
	vllm := "{\"error\":{\"message\":\"The model `premium-ide` does not exist.\",\"type\":\"NotFoundError\",\"param\":\"model\",\"code\":404}}"
	for _, tc := range []struct {
		name   string
		status int
		model  string
		body   string
	}{
		{"ollama, a path it does not serve", 404, "premium-ide", `404 page not found`},
		{"vllm, a path it does not serve", 404, "premium-ide", `{"detail":"Not Found"}`},
		{"llama.cpp, a path it does not serve", 404, "premium-ide", `{"error":{"message":"File Not Found","type":"not_found_error","code":404}}`},
		{"lm studio, a path it does not serve", 200, "premium-ide", `{"error":"Unexpected endpoint or method. (POST /v1/nope)"}`},
		{"lm studio with nothing loaded", 400, "premium-ide", `{"error":{"message":"No models loaded. Please load a model in the developer page or use the 'lms load' command.","type":"invalid_request_error","param":"model","code":null}}`},
		{"another model named", 404, "granite3.3", ollama},
		{"a name the refused one starts with", 404, "qwen3", `{"error":{"message":"model 'qwen3:4b' not found","type":"not_found_error","param":null,"code":null}}`},
		{"a name the refused one starts with, ollama 0.7.0's quoting", 404, "qwen3", `{"error":{"message":"model \"qwen3:4b\" not found, try pulling it first","type":"api_error","param":null,"code":null}}`},
		{"ollama's phrase at a status not recorded", 500, "premium-ide", ollama},
		{"vllm's phrase at a status not recorded", 400, "premium-ide", vllm},
		{"no model sent", 404, "", `{"error":{"message":"model '' not found"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if refusesModel(tc.status, []byte(tc.body), tc.model) {
				t.Errorf("refusesModel(%d, %s, %q) = true, want no refusal", tc.status, tc.body, tc.model)
			}
		})
	}
}

// A local server's refusal is resent with the server's own model, as LiteLLM's is.
func TestResend_TreatsALocalServersMissingModelAsARefusal(t *testing.T) {
	_, p := buildRouter(t, wordConfig(`"opencode": "glm"`))
	pctx := chat(newStore(t), "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s1", "exo-free", true)
	run(t, p, pctx)
	if !resend(p, pctx, http.StatusNotFound, `{"error":{"message":"model 'exo-free' not found","type":"not_found_error","param":null,"code":null}}`) {
		t.Fatal("Resend = false for Ollama's refusal of the model sent")
	}
	if got := sentModel(t, pctx); got != glmMain {
		t.Errorf("model = %q, want %s", got, glmMain)
	}
	assertRecord(t, pctx, pipeline.ActionModify, "resent",
		map[string]string{"server": "glm", "model": "exo-free", "substitute": glmMain})
}

// A substitute the server refuses too ends there, whether it was resent or chosen up
// front from a remembered refusal: one resend at most, then the agent sees the answer.
func TestResend_ARefusedSubstituteEndsThere(t *testing.T) {
	missing := func(m string) string {
		return `{"error":{"message":"model '` + m + `' not found","type":"not_found_error","param":null,"code":null}}`
	}
	store := newStore(t)
	_, p := buildRouter(t, wordConfig(`"opencode": "glm"`))

	first := chat(store, "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s1", "exo-free", true)
	run(t, p, first)
	if !resend(p, first, http.StatusNotFound, missing("exo-free")) {
		t.Fatal("Resend = false for the agent's model refused")
	}
	if resend(p, first, http.StatusNotFound, missing(glmMain)) {
		t.Error("Resend = true for a refused substitute: a second resend of one request")
	}
	if got := sentModel(t, first); got != glmMain {
		t.Errorf("model = %q, want the substitute left as sent", got)
	}

	next := chat(store, "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s2", "exo-free", true)
	run(t, p, next)
	if got := sentModel(t, next); got != glmMain {
		t.Fatalf("model = %q, want the substitute up front", got)
	}
	if resend(p, next, http.StatusNotFound, missing(glmMain)) {
		t.Error("Resend = true for a substitute chosen up front and refused")
	}
}

// A session's kept substitute need not be what the server's list resolves now: the
// word changed, the list moved, or the model was deleted from the server. Refused, it
// still reaches the agent, because a request is resent for its model once at most,
// whatever a second substitute would be.
func TestResend_AKeptSubstituteRefusedIsNotResentAgain(t *testing.T) {
	const stale = "rits/zai-org/glm-4-6"
	r, p := buildRouter(t, wordConfig(`"opencode": "glm"`))
	pctx := chat(newStore(t), "opencode.ai", "/zen/v1/chat/completions", opencodeUA, "s1", "exo-free", true)
	r.keepSubstitute(pctx, "glm", "exo-free", stale)
	run(t, p, pctx)
	if got := sentModel(t, pctx); got != stale {
		t.Fatalf("model = %q, want the session's kept substitute %s", got, stale)
	}
	if resend(p, pctx, http.StatusNotFound, `{"error":{"message":"model '`+stale+`' not found","type":"not_found_error","param":null,"code":null}}`) {
		t.Error("Resend = true for a kept substitute the server refused: a second model for one request")
	}
}

// A local server's refusal is answered from that server's own list, found by the host and
// port the request was sent to.
func TestResend_ALocalServersRefusalIsAnsweredFromItsOwnList(t *testing.T) {
	r := New()
	if err := r.Configure(json.RawMessage(localServers)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	r.catalogs.set("ollama", []string{"granite3.3:8b"})
	r.catalogs.set("vllm", []string{"qwen2.5-0.5b"})
	p, err := pipeline.New([]pipeline.Plugin{r})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	pctx := claudeCode(request(newStore(t), "api.anthropic.com", claudeUA, "s1"), pipeline.AgentRoleMain, 26, 0)
	pctx.Body = []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hello"}]}`)
	run(t, p, pctx)
	assertRouted(t, pctx, "localhost:11434", "ollama-key")
	if !resend(p, pctx, http.StatusNotFound, `{"type":"error","error":{"type":"not_found_error","message":"model 'claude-opus-5-5' not found"}}`) {
		t.Fatal("Resend = false for ollama's refusal")
	}
	if got := sentModel(t, pctx); got != "granite3.3:8b" {
		t.Errorf("model = %q, want ollama's granite3.3:8b", got)
	}
}
