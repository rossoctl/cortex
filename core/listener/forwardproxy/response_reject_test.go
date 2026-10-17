package forwardproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// responseDenyPlugin rewrites responses, so an SSE response is buffered,
// and denies every one after recording why.
type responseDenyPlugin struct{}

func (responseDenyPlugin) Name() string { return "response-guard" }
func (responseDenyPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{ReadsBody: true, WritesResponseBody: true}
}
func (responseDenyPlugin) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (responseDenyPlugin) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	return pctx.DenyAndRecord("blocked for test", "test.blocked", "blocked for test")
}

// A response-phase deny of a buffered SSE response reaches the client as
// the rejection, with none of the stream, and is recorded in the request's
// session with the response pass's invocation.
func TestForwardProxy_ResponseRejectOfStreamIsRecorded(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\"}\n\n")
	}))
	defer backend.Close()

	p, err := pipeline.New([]pipeline.Plugin{responseDenyPlugin{}})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Client:           http.DefaultClient,
		SessionIDHeaders: []string{session.ClaudeCodeSessionHeader},
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	const sid = "6a1f0c8e-2d7b-4f3e-9a51-0b8c4e2d7f10"
	req, _ := http.NewRequest("POST", backend.URL+"/v1/messages", strings.NewReader(`{}`))
	req.Header.Set(session.ClaudeCodeSessionHeader, sid)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode < 400 || strings.Contains(string(body), "message_start") ||
		resp.Header.Get("Content-Type") == "text/event-stream" {
		t.Fatalf("client got %d %q %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}

	v := store.View(sid)
	if v == nil {
		t.Fatalf("no session %s", sid)
	}
	var denied *pipeline.SessionEvent
	for i := range v.Events {
		if v.Events[i].Phase == pipeline.SessionDenied {
			denied = &v.Events[i]
		}
	}
	if denied == nil || denied.StatusCode != resp.StatusCode || denied.Invocations == nil ||
		len(denied.Invocations.Outbound) != 1 || denied.Invocations.Outbound[0].Plugin != "response-guard" {
		t.Fatalf("denied row = %+v", denied)
	}
}
