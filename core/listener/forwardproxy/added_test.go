package forwardproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// slowRequestPlugin stands in for the proxy's own request-side work: it takes d on every
// request before the listener sends anything upstream.
type slowRequestPlugin struct{ d time.Duration }

func (slowRequestPlugin) Name() string { return "slow-request" }
func (slowRequestPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (p slowRequestPlugin) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	// timing: this stands in for real request-side work; the test measures the resulting
	// elapsed Added duration, so the wait must be real.
	time.Sleep(p.d)
	return pipeline.Action{Type: pipeline.Continue}
}
func (slowRequestPlugin) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// TestForwardProxy_ResponseEventCarriesAdded pins what llm.inference.added_seconds reports:
// the proxy's own request-side time, StartedAt to the send, and none of the origin's. The
// plugin takes work before the send and the origin waits origin before its headers, so Added
// must cover the first and exclude the second. A send stamp taken after client.Do returned
// would put the origin's wait into Added; one taken before the pipeline ran would report 0.
func TestForwardProxy_ResponseEventCarriesAdded(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	const work, origin = 30 * time.Millisecond, 120 * time.Millisecond
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// timing: stands in for the origin's real wait before its headers; the test measures
		// the resulting elapsed Added, so the wait must be real.
		time.Sleep(origin)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	p, err := pipeline.New([]pipeline.Plugin{slowRequestPlugin{d: work}})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Sessions: store, Client: http.DefaultClient}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	req, _ := http.NewRequest("GET", backend.URL+"/v1/messages", nil)
	proxyClient := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))}}
	resp, err := proxyClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	waitForResponseEvent(t, store)

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 2 {
		t.Fatalf("expected 2 events (request + response), got %+v", v)
	}
	ev := v.Events[1]
	if ev.Added < work {
		t.Errorf("added = %v, want ≥ %v — the plugin's work ran before the send", ev.Added, work)
	}
	if ev.Added >= origin {
		t.Errorf("added = %v, want < %v — the origin's wait before its headers is not the proxy's", ev.Added, origin)
	}
	if ev.Ttfb-ev.Added < origin/2 {
		t.Errorf("ttfb−added = %v, want ≥ %v — the origin waited %v after the send", ev.Ttfb-ev.Added, origin/2, origin)
	}
}
