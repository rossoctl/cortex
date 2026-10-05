package forwardproxy

import (
	"bufio"
	"bytes"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/plugintesting"
	"github.com/rossoctl/cortex/core/session"
)

// A request whose body cannot be buffered never reaches the pipeline, and used to
// record nothing: a 33 MiB POST got its 413 and left no row, and on a bridged
// tunnel the only rows were the tunnel's open and a close saying 200, in the
// default bucket. These tests pin the failure row each such request now records.

// assertRequestThenFailure checks events is exactly a request row and its paired
// failure row, and returns the failure.
func assertRequestThenFailure(t *testing.T, events []pipeline.SessionEvent, status int, kind string) pipeline.SessionEvent {
	t.Helper()
	if len(events) != 2 {
		t.Fatalf("recorded %d event(s), want the request and its failure: %+v", len(events), events)
	}
	req, resp := events[0], events[1]
	if req.Phase != pipeline.SessionRequest || resp.Phase != pipeline.SessionResponse {
		t.Fatalf("phases %q, %q; want request then response", req.Phase, resp.Phase)
	}
	if req.RequestID == "" || req.RequestID != resp.RequestID {
		t.Errorf("request ids %q and %q; the failure must pair with its request", req.RequestID, resp.RequestID)
	}
	if req.HTTPMethod != http.MethodPost || req.HTTPPath != "/v1/messages" {
		t.Errorf("request row %s %q, want POST /v1/messages", req.HTTPMethod, req.HTTPPath)
	}
	if resp.StatusCode != status {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, status)
	}
	if resp.Error == nil {
		t.Fatal("Error = nil, want why the body could not be buffered")
	}
	if resp.Error.Kind != kind {
		t.Errorf("Error.Kind = %q, want %q", resp.Error.Kind, kind)
	}
	if resp.Error.Code != "" {
		t.Errorf("Error.Code = %q, want empty: no upstream was asked", resp.Error.Code)
	}
	return resp
}

func TestForwardProxy_OversizeRequest_RecordsProxyError(t *testing.T) {
	proxy, store := failureProxy(t, nil, &bodyRecorderPlugin{})

	// closedAddr: had the proxy forwarded this, the client would see a 502.
	body := bytes.Repeat([]byte("x"), maxRequestBodySize+1)
	req, _ := http.NewRequest(http.MethodPost, "http://"+closedAddr(t)+"/v1/messages", bytes.NewReader(body))
	c := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))}}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("client status = %d, want 413", resp.StatusCode)
	}

	fail := assertRequestThenFailure(t, allEvents(t, store), http.StatusRequestEntityTooLarge, "proxy_error")
	for _, want := range []string{"33554433 bytes", "33554432-byte"} {
		if !strings.Contains(fail.Error.Message, want) {
			t.Errorf("Error.Message = %q, want it to name %q: the announced size and the limit", fail.Error.Message, want)
		}
	}
}

// The bridged case is how Claude Code reaches api.anthropic.com. The tunnel's open
// joins the request's session as it does for any recorded request, and no close is
// recorded, because the request's own rows answer the tunnel.
func TestForwardProxy_BridgedOversizeRequest_RecordsProxyError(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	proxyAddr, target, ca, done := bridgingProxyWith(t, store, func(s *Server) {
		s.SessionIDHeaders = []string{session.ClaudeCodeSessionHeader}
		p, err := plugintesting.BuildPipeline([]pipeline.Plugin{&bodyRecorderPlugin{}})
		if err != nil {
			t.Fatalf("BuildPipeline: %v", err)
		}
		s.OutboundPipeline = pipeline.NewHolder(p)
	})

	raw, br, _ := sendConnect(t, proxyAddr, target)
	tc := bridgedTLS(t, raw, br, target, ca)
	body := bytes.Repeat([]byte("x"), maxRequestBodySize+1)
	req, _ := http.NewRequest(http.MethodPost, "https://"+hostOnly(target)+"/v1/messages", bytes.NewReader(body))
	req.Header.Set(session.ClaudeCodeSessionHeader, "sess-1")
	if resp := bridgedRoundTrip(t, tc, req); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("bridged request status %d, want 413", resp.StatusCode)
	}
	_ = tc.Close()
	waitDone(t, done)

	openThen(t, store, "sess-1")
	var rows []pipeline.SessionEvent
	for _, e := range store.View("sess-1").Events {
		if !e.Tunnel {
			rows = append(rows, e)
		}
	}
	assertRequestThenFailure(t, rows, http.StatusRequestEntityTooLarge, "proxy_error")
	if _, closes := tunnelRows(store, "sess-1"); len(closes) != 0 {
		t.Errorf("recorded %d tunnel close(s); the request's rows answer the tunnel", len(closes))
	}
	if v := store.View(session.DefaultSessionID); v != nil {
		t.Errorf("default holds %d event(s); every row belongs to sess-1", len(v.Events))
	}
}

// A body that is not over the limit can still fail to arrive. Neither case is the
// proxy's limit, so neither says proxy_error.
func TestForwardProxy_UnreadableRequestBody_RecordsFailure(t *testing.T) {
	tests := []struct {
		name       string
		headers    string
		body       string
		closeWrite bool
		status     int
		kind       string
	}{
		{
			// The client hangs up mid-upload. net/http cancels the request context
			// on the read error, so nobody gets the 400, as for an abandoned exchange.
			name:       "client hangs up mid-upload",
			headers:    "Content-Length: 100\r\n",
			body:       "only ten b",
			closeWrite: true,
			status:     pipeline.StatusClientClosedRequest,
			kind:       "client_canceled",
		},
		{
			// The client is still there and gets the 400.
			name:    "malformed chunked body",
			headers: "Transfer-Encoding: chunked\r\n",
			body:    "zz\r\n",
			status:  http.StatusBadRequest,
			kind:    "request_unreadable",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			proxy, store := failureProxy(t, nil, &bodyRecorderPlugin{})
			conn, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL, "http://"))
			if err != nil {
				t.Fatalf("dial proxy: %v", err)
			}
			defer conn.Close()
			target := closedAddr(t)
			if _, err := conn.Write([]byte("POST http://" + target + "/v1/messages HTTP/1.1\r\nHost: " + target + "\r\n" +
				tc.headers + "\r\n" + tc.body)); err != nil {
				t.Fatalf("write: %v", err)
			}
			if tc.closeWrite {
				_ = conn.(*net.TCPConn).CloseWrite()
			} else {
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
				if err != nil {
					t.Fatalf("read response: %v", err)
				}
				resp.Body.Close()
				if resp.StatusCode != tc.status {
					t.Errorf("client status = %d, want %d", resp.StatusCode, tc.status)
				}
			}

			waitForResponseEvent(t, store)
			assertRequestThenFailure(t, allEvents(t, store), tc.status, tc.kind)
		})
	}
}
