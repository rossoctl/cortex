package forwardproxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins/inferenceparser"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins/mcpparser"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
)

// TestForwardProxy_MCP_SSEResponse_RecordsObserve reproduces the live
// weather-tool-mcp scenario: an MCP tools/list call whose response is
// text/event-stream (Streamable HTTP). The request is parsed (observe), and
// the RESPONSE must also be parsed into a result + recorded as an mcp-parser
// observe on the response event. The live cluster showed result=N,
// invocations=[] on the response — this test pins whether the proxy+parser
// path reproduces that.
func TestForwardProxy_MCP_SSEResponse_RecordsObserve(t *testing.T) {
	// Upstream mimics weather-tool-mcp: tools/list -> SSE with one data frame
	// carrying the JSON-RPC result, exactly as FastMCP emits it.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"tools\":[{\"name\":\"get_weather\"}]}}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	// Production builds Configurable plugins through plugins.Build, which
	// wraps them via pipeline.WrapConfigured. mcp-parser is Configurable +
	// StreamingResponder, so it MUST stay a StreamingResponder after wrapping
	// — that's the bug this test guards (a bare wrapper drops OnResponseFrame
	// and RunResponseFrame skips it, leaving the SSE response unparsed).
	// inference-parser is not Configurable, so it's added raw, as in production.
	pipe, err := pipeline.New([]pipeline.Plugin{
		pipeline.WrapConfigured(mcpparser.NewMCPParser(), nil),
		inferenceparser.NewInferenceParser(),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(pipeline.NewHolder(pipe), store, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	reqBody := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	req, _ := http.NewRequest("POST", upstream.URL+"/mcp", bytes.NewReader([]byte(reqBody)))
	req.Header.Set("Content-Type", "application/json")
	proxyClient := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))}}
	resp, err := proxyClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	v := store.View(session.DefaultSessionID)
	if v == nil {
		t.Fatal("no session recorded")
	}
	var reqEv, respEv *pipeline.SessionEvent
	for i := range v.Events {
		e := &v.Events[i]
		if e.MCP == nil || e.MCP.Method != "tools/list" {
			continue
		}
		switch e.Phase {
		case pipeline.SessionRequest:
			reqEv = e
		case pipeline.SessionResponse:
			respEv = e
		}
	}
	if reqEv == nil || respEv == nil {
		t.Fatalf("missing req/resp events: req=%v resp=%v", reqEv != nil, respEv != nil)
	}

	// Request side parsed (sanity).
	if reqEv.Invocations == nil || len(reqEv.Invocations.Outbound) == 0 {
		t.Errorf("request event has no mcp-parser invocation: %+v", reqEv.Invocations)
	}

	// THE ASSERTION: the response was parsed into a result and recorded an
	// observe. Live cluster fails both.
	if respEv.MCP.Result == nil {
		t.Errorf("response MCP.Result not populated (parser never saw the SSE result)")
	}
	respObserved := respEv.Invocations != nil && len(respEv.Invocations.Outbound) > 0
	if !respObserved {
		t.Errorf("response event has NO mcp-parser invocation — reproduces the bug. invocations=%+v", respEv.Invocations)
	}
}
