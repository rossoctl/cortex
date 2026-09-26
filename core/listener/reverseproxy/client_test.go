package reverseproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// uaMutatingPlugin rewrites the User-Agent on pctx.Headers, which is what any
// header-mutating plugin does. Nothing in the tree rewrites this header today, which is
// exactly why the ordering below has to be pinned by a test rather than left to hold by luck.
//
// set == "" DELETES the header instead — the same defect run backwards, and the direction
// that turns a named caller into untagged traffic.
type uaMutatingPlugin struct{ set string }

func (p *uaMutatingPlugin) Name() string { return "ua-mutate" }
func (p *uaMutatingPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (p *uaMutatingPlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if p.set == "" {
		pctx.Headers.Del("User-Agent")
	} else {
		pctx.Headers.Set("User-Agent", p.set)
	}
	// An invocation, so the request-phase event is recorded at all: the inbound request gate
	// records when A2A, Invocations or plugin-public Custom entries are present.
	pctx.Allow("ok")
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *uaMutatingPlugin) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// serveWithUAPlugin starts a reverse proxy whose inbound pipeline holds p, and returns the
// proxy URL and the session store its events land in.
func serveWithUAPlugin(t *testing.T, p pipeline.Plugin) (string, *session.Store) {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	pl, err := pipeline.New([]pipeline.Plugin{p})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	store := session.New(5*time.Minute, 100, 100)
	t.Cleanup(store.Close)

	srv, err := NewServer(pipeline.NewHolder(pl), store, backend.URL, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)
	return proxy.URL, store
}

// getWithUA sends one request through the proxy, with the given User-Agent when non-empty.
func getWithUA(t *testing.T, proxyURL, ua string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	// Set explicitly rather than left to Go's default, which would send its own
	// "Go-http-client/1.1" and make the no-User-Agent case untestable.
	req.Header.Set("User-Agent", ua)
	if ua == "" {
		req.Header["User-Agent"] = nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request through the proxy: %v", err)
	}
	_ = resp.Body.Close()
}

// inboundEvents waits for the request and response events of one turn.
//
// SUBSCRIBED, NOT POLLED. store.Subscribe delivers each event as it is appended, so this waits
// exactly as long as the listener takes and no longer — where a 100 x 20ms poll both sleeps on
// a fast machine and reports "got <nil>" on a slow one, with no way to tell a listener that was
// slow from a listener that recorded nothing. The subscription has to be taken BEFORE the
// request, which is why it is a parameter here rather than something this helper opens.
func inboundEvents(t *testing.T, sub *session.Subscription, want int) []pipeline.SessionEvent {
	t.Helper()
	var out []pipeline.SessionEvent
	deadline := time.After(5 * time.Second)
	for len(out) < want {
		select {
		case ev := <-sub.Events():
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("waited 5s for %d inbound events, got %d: %+v", want, len(out), out)
		}
	}
	return out
}

// TestReverseProxy_ClientIsResolvedBeforeThePipelineRuns is the inbound half of the ordering
// guarantee, and the listener that had none.
//
// Context.ClientInfo memoizes on first call, and every recording site in this file is
// downstream of the pipeline. pctx.Headers is a clone plugins write to, so without the pin at
// construction the attribution of an inbound request is decided by whichever recorder asks
// first, AFTER the pipeline has had its way with the header. The plugin here names an agent
// that never made the call; every event must still name the one that did.
//
// Fails if the ResolveClient call is removed, and equally if it is merely MOVED after the
// pipeline run — which is the whole difference between "resolved once" and "resolved before
// anything can change it".
func TestReverseProxy_ClientIsResolvedBeforeThePipelineRuns(t *testing.T) {
	const sent = "claude-cli/2.1.14 (external, cli)"
	proxyURL, store := serveWithUAPlugin(t, &uaMutatingPlugin{set: "impostor/9.9"})
	sub, unsubscribe := store.Subscribe()
	defer unsubscribe()

	getWithUA(t, proxyURL, sent)

	for _, ev := range inboundEvents(t, sub, 2) {
		if ev.Client == nil {
			t.Fatalf("phase %s: Client is nil", ev.Phase)
		}
		if ev.Client.Raw != sent {
			t.Errorf("phase %s: Client.Raw = %q, want the User-Agent the CALLER sent; a plugin "+
				"rewrote the header and the attribution followed it, so this request is filed "+
				"under a program that never made it", ev.Phase, ev.Client.Raw)
		}
		if ev.Client.Name != "claude-code" {
			t.Errorf("phase %s: Client.Name = %q, want claude-code", ev.Phase, ev.Client.Name)
		}
	}
}

// A plugin that DELETES the header cannot turn a named caller into untagged traffic.
//
// Pinned separately from the rewrite above because the two fail independently: a "resolve
// later" bug that happens to preserve names can still lose them, and losing one means the
// caller lands in the reserved "unknown" bucket — indistinguishable from a request that sent
// no User-Agent at all.
func TestReverseProxy_APluginCannotEraseTheClient(t *testing.T) {
	proxyURL, store := serveWithUAPlugin(t, &uaMutatingPlugin{set: ""})
	sub, unsubscribe := store.Subscribe()
	defer unsubscribe()

	getWithUA(t, proxyURL, "claude-cli/2.1.14 (external, cli)")

	for _, ev := range inboundEvents(t, sub, 2) {
		if ev.Client == nil {
			t.Fatalf("phase %s: Client is nil — a plugin deleted the header and the caller that "+
				"actually made this request became untagged traffic", ev.Phase)
		}
		if ev.Client.Name != "claude-code" {
			t.Errorf("phase %s: Client = %+v, want claude-code/2.1.14", ev.Phase, ev.Client)
		}
	}
}

// And absence survives a plugin too: a request that carried NO User-Agent must not gain one.
//
// The invented-agent direction is the one that puts a fictional line item on a cost table, so
// nil here has to mean nil. This is also what keeps "unknown" honest on this listener: it now
// means the caller sent nothing, not that the listener was never wired.
func TestReverseProxy_APluginCannotInventAClient(t *testing.T) {
	proxyURL, store := serveWithUAPlugin(t, &uaMutatingPlugin{set: "impostor/9.9"})
	sub, unsubscribe := store.Subscribe()
	defer unsubscribe()

	getWithUA(t, proxyURL, "")

	for _, ev := range inboundEvents(t, sub, 2) {
		if ev.Client != nil {
			t.Errorf("phase %s: Client = %+v, want nil: the caller sent no User-Agent and a "+
				"plugin's write became the recorded agent", ev.Phase, ev.Client)
		}
		// No Label() assertion here, deliberately: Label() is nil-safe by construction, so
		// asserting it answers "unknown" on a value the check above requires to be nil cannot
		// fail. pipeline.TestSessionEvent_OldWireFormatDecodesWithNoClient is where the
		// nil-receiver contract belongs, and it guards the deref with Fatalf.
	}
}
