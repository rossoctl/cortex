package reverseproxy

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
// request before the listener hands it to the backend.
type slowRequestPlugin struct{ d time.Duration }

func (slowRequestPlugin) Name() string { return "slow-request" }
func (slowRequestPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (p slowRequestPlugin) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	time.Sleep(p.d)
	return pipeline.Action{Type: pipeline.Continue}
}
func (slowRequestPlugin) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// TestReverseProxy_ResponseEventCarriesAdded is the forward proxy's
// TestForwardProxy_ResponseEventCarriesAdded on this listener: Added covers the pipeline's
// work before ServeHTTP and none of the backend's wait before its headers, so
// llm.inference.added_seconds means one thing on every listener.
func TestReverseProxy_ResponseEventCarriesAdded(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	const work, origin = 30 * time.Millisecond, 120 * time.Millisecond
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(origin)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	p, err := pipeline.New([]pipeline.Plugin{slowRequestPlugin{d: work}})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(pipeline.NewHolder(p), store, backend.URL, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	resp, err := http.DefaultClient.Get(proxy.URL + "/api")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	waitForResponseEvent(t, store, session.DefaultSessionID)

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 2 {
		t.Fatalf("expected 2 events (request + response), got %+v", v)
	}
	ev := v.Events[1]
	if ev.Added < work {
		t.Errorf("added = %v, want ≥ %v — the plugin's work ran before the send", ev.Added, work)
	}
	if ev.Added >= origin {
		t.Errorf("added = %v, want < %v — the backend's wait before its headers is not the proxy's", ev.Added, origin)
	}
}
