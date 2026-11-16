package forwardproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// responseWriter appends suffix to the response, or refuses it when reject is
// set — the shape of an output guardrail such as cpex or sparc.
type responseWriter struct {
	name, suffix string
	reject       bool
}

func (p *responseWriter) Name() string { return p.name }
func (p *responseWriter) Capabilities() pipeline.PluginCapabilities {
	// ReadsResponseBody: its test paths are ones no parser claims.
	return pipeline.PluginCapabilities{ReadsBody: true, ReadsResponseBody: true, WritesResponseBody: true}
}
func (p *responseWriter) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *responseWriter) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if p.reject {
		return pipeline.DenyStatus(http.StatusForbidden, "test.response_refused", p.name+" refused the response")
	}
	pctx.SetResponseBody(append(append([]byte{}, pctx.ResponseBody...), p.suffix...))
	return pipeline.Action{Type: pipeline.Continue}
}

// getThroughProxy serves body with contentType from an upstream, GETs it through
// a forward proxy running plugins, and returns the client's response and body.
func getThroughProxy(t *testing.T, contentType, body string, plugins ...pipeline.Plugin) (*http.Response, string) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(upstream.Close)
	pipe, err := pipeline.New(plugins)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	srv, err := NewServer(pipeline.NewHolder(pipe), nil, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))}}
	resp, err := client.Get(upstream.URL + "/data")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, string(got)
}

// Two response writers chain on a real listener: the one placed last runs first,
// the client receives what the other left with a Content-Length that matches,
// and a per-frame reader placed ahead of both — where the parsers sit — sees
// exactly those bytes.
func TestForwardProxy_ResponseWritersChain(t *testing.T) {
	parser := newStreamingProbe(false)
	resp, body := getThroughProxy(t, "application/json", `{"id":1}`,
		parser, &responseWriter{name: "first", suffix: "+a"}, &responseWriter{name: "second", suffix: "+b"})

	if want := `{"id":1}+b+a`; body != want {
		t.Errorf("client got %q, want %q", body, want)
	}
	if resp.ContentLength != int64(len(body)) {
		t.Errorf("ContentLength = %d, want %d", resp.ContentLength, len(body))
	}
	frames, _ := parser.snapshot()
	if len(frames) != 1 || string(frames[0]) != body {
		t.Errorf("parser saw %q, want one frame carrying what the client received, %q", frames, body)
	}
}

// An SSE response with a response writer in the chain is buffered, and with two
// writers the client receives the stream as both left it.
func TestForwardProxy_ResponseWritersChainOnABufferedStream(t *testing.T) {
	_, body := getThroughProxy(t, "text/event-stream", "data: {\"id\":1}\n\n",
		&responseWriter{name: "first", suffix: "+a"}, &responseWriter{name: "second", suffix: "+b"})

	if want := "data: {\"id\":1}\n\n+b+a"; body != want {
		t.Errorf("client got %q, want %q", body, want)
	}
}

// A writer that refuses the response after an earlier-running writer rewrote it
// sends the client the refusal and none of the rewritten bytes.
func TestForwardProxy_ResponseWriterRefusalAfterARewrite(t *testing.T) {
	resp, body := getThroughProxy(t, "application/json", `{"id":1}`,
		&responseWriter{name: "first", reject: true}, &responseWriter{name: "second", suffix: "+b"})

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 from first's refusal", resp.StatusCode)
	}
	if strings.Contains(body, "+b") || strings.Contains(body, `"id"`) {
		t.Errorf("client got %q; want the refusal, not the rewritten response", body)
	}
}
