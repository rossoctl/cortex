package reverseproxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// Issue #1045, inbound half. When the backend call fails at the transport level
// ReverseProxy hands the error to errorHandler, which answered 502 and returned
// without recording anything — so the failure existed only on the wire, and the
// request event left behind read as a request still in flight.

// TestReverseProxy_UnreachableBackend_RecordsResponseEvent points a server at an
// address nothing is listening on and asserts the paired 502 response event.
func TestReverseProxy_UnreachableBackend_RecordsResponseEvent(t *testing.T) {
	// Both rows are recorded whatever the pipeline holds; allowOnlyPlugin is here
	// only so the pipeline is not empty.
	p, err := pipeline.New([]pipeline.Plugin{allowOnlyPlugin{}})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	store := session.New(5*time.Minute, 100, 100)
	defer store.Close()

	srv, err := NewServer(pipeline.NewHolder(p), store, "http://"+closedAddr(t), nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	resp, err := hermeticGet(proxy.URL + "/work?api_key=SECRET1045")
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("client status = %d, want 502", resp.StatusCode)
	}

	v := store.View(session.DefaultSessionID)
	if v == nil {
		t.Fatal("no session recorded")
	}
	var respEvent *pipeline.SessionEvent
	for i := range v.Events {
		if v.Events[i].Phase == pipeline.SessionResponse {
			respEvent = &v.Events[i]
		}
	}
	if respEvent == nil {
		t.Fatalf("no response event recorded for a failed backend call; events = %+v", v.Events)
	}
	if respEvent.Direction != pipeline.Inbound {
		t.Errorf("Direction = %q, want inbound", respEvent.Direction)
	}
	if respEvent.StatusCode != http.StatusBadGateway {
		t.Errorf("StatusCode = %d, want 502", respEvent.StatusCode)
	}
	if respEvent.Error == nil {
		t.Fatal("Error = nil, want the classified transport failure")
	}
	if respEvent.Error.Kind != "upstream_refused" {
		t.Errorf("Error.Kind = %q, want upstream_refused", respEvent.Error.Kind)
	}
	if msg := respEvent.Error.Message; msg == "" || strings.Contains(msg, "SECRET1045") {
		t.Errorf("Error.Message = %q, want the address and the cause, without the request's query string", msg)
	}
	if respEvent.Duration <= 0 {
		t.Errorf("Duration = %v, want > 0 (a zero reads as 'not measured')", respEvent.Duration)
	}
}

// TestReverseProxy_ResponseRejection_RecordsNoResponseEvent pins the branch the
// fix deliberately left alone. A plugin that rejects on the response phase also
// reaches errorHandler, but as a *responseRejectedError — a policy verdict, not a
// transport failure. It must not pick up a synthetic 502 "upstream failed" row,
// which would misattribute the proxy's own decision to the backend.
//
// That this records no response event at all is a real sibling gap (modifyResponse
// returns before its append), but it has a different right answer — a denial row
// carrying the Action — so it is out of scope here. This test documents the
// current behavior so the distinction is deliberate rather than incidental.
func TestReverseProxy_ResponseRejection_RecordsNoResponseEvent(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	p, err := pipeline.New([]pipeline.Plugin{responseRejecter{}})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	store := session.New(5*time.Minute, 100, 100)
	defer store.Close()

	srv, err := NewServer(pipeline.NewHolder(p), store, backend.URL, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/work")
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusBadGateway {
		t.Errorf("status = 502; a plugin rejection must not be rendered as a bad gateway")
	}

	v := store.View(session.DefaultSessionID)
	if v == nil {
		return // nothing recorded at all is also "no response event"
	}
	for _, ev := range v.Events {
		if ev.Phase == pipeline.SessionResponse {
			t.Errorf("recorded a response event for a plugin rejection: %+v", ev)
		}
	}
}

// responseRejecter allows the request and rejects on the response phase, which
// is what makes modifyResponse return a *responseRejectedError.
type responseRejecter struct{}

func (responseRejecter) Name() string { return "response-rejecter" }
func (responseRejecter) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}

func (responseRejecter) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	pctx.Allow("ok")
	return pipeline.Action{Type: pipeline.Continue}
}

func (responseRejecter) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	return pipeline.Action{
		Type: pipeline.Reject,
		Violation: &pipeline.Violation{
			Code:   "policy_denied",
			Reason: "test rejects every response",
			Status: http.StatusForbidden,
		},
	}
}

// closedAddr returns an address nothing is listening on: bind an ephemeral port,
// note it, release it. A hardcoded port could collide with a real local service
// and turn the expected refusal into a confusing success.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}

// bodyReader forces the listener onto the buffered response path, which is where
// the two buffering failures live.
type bodyReader struct{}

func (bodyReader) Name() string { return "body-reader" }
func (bodyReader) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{ReadsBody: true}
}

func (bodyReader) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	pctx.Allow("ok")
	return pipeline.Action{Type: pipeline.Continue}
}

func (bodyReader) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	pctx.Observe("resp-ok")
	return pipeline.Action{Type: pipeline.Continue}
}

// TestReverseProxy_OversizeResponse_RecordsProxyError guards the distinction a
// reviewer caught. A response the backend sent fine but that we could not buffer
// (over maxBodySize) reaches errorHandler too, and must NOT be recorded as a
// transport failure: the buffer limit is ours. The row's status is the 502 the
// client got — every consumer keys on it, and the backend's 200 there would read
// as healthy — and the 200 survives as error.code.
func TestReverseProxy_OversizeResponse_RecordsProxyError(t *testing.T) {
	// One byte over the cap is enough, and keeps the test cheap.
	huge := bytes.Repeat([]byte("x"), maxBodySize+1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(huge)
	}))
	defer backend.Close()

	f := &finisherStub{name: "f-oversize"}
	ev, clientStatus := failedExchange(t, backend.URL+"/big", 5*time.Second, bodyReader{}, f)
	if clientStatus != http.StatusBadGateway {
		t.Errorf("client status = %d, want 502", clientStatus)
	}
	assertBufferingFailure(t, ev, http.StatusBadGateway, "proxy_error", "200")
	assertOutcomeError(t, f)
}

// TestReverseProxy_TruncatedResponse_RecordsUpstreamFailure is the buffering
// failure that is NOT ours. A body that breaks off mid-read is the backend
// resetting or truncating its own response, so it is classified like a failed
// call rather than as proxy_error, keeping the backend's status as error.code.
func TestReverseProxy_TruncatedResponse_RecordsUpstreamFailure(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		// Promises 1000 bytes, sends 11, and closes.
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 1000\r\nContent-Type: application/json\r\n\r\nhello world")
		_ = buf.Flush()
	}))
	defer backend.Close()

	f := &finisherStub{name: "f-truncated"}
	ev, clientStatus := failedExchange(t, backend.URL+"/cut", 5*time.Second, bodyReader{}, f)
	if clientStatus != http.StatusBadGateway {
		t.Errorf("client status = %d, want 502", clientStatus)
	}
	assertBufferingFailure(t, ev, http.StatusBadGateway, "upstream_error", "200")
	assertOutcomeError(t, f)
}

// TestReverseProxy_ClientGivesUp_RecordsClientCanceled covers a caller that
// stops waiting — common for a synchronous A2A message/send against a slow agent.
// The backend call is cancelled under it, and RoundTrip returns that
// cancellation; recorded as a 502 upstream failure it would count a response
// nobody received. It is a 499 client_canceled instead.
func TestReverseProxy_ClientGivesUp_RecordsClientCanceled(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer backend.Close()
	defer close(release)

	f := &finisherStub{name: "f-cancel"}
	store, proxy := failureServer(t, backend.URL, allowOnlyPlugin{}, f)

	c := &http.Client{Timeout: 300 * time.Millisecond, Transport: &http.Transport{Proxy: nil}}
	if _, err := c.Get(proxy.URL + "/slow"); err == nil {
		t.Fatal("request succeeded; wanted the client to give up on a hung backend")
	}

	ev := waitForResponseEvent(t, store)
	if ev.StatusCode != pipeline.StatusClientClosedRequest {
		t.Errorf("StatusCode = %d, want %d — a 502 here is a response nobody received", ev.StatusCode, pipeline.StatusClientClosedRequest)
	}
	if ev.Error == nil || ev.Error.Kind != "client_canceled" {
		t.Errorf("Error = %+v, want kind client_canceled", ev.Error)
	}
	assertOutcomeError(t, f)
}

// TestReverseProxy_FailedUpgrade_RecordsOneResponse pins the ErrorHandler call
// that comes AFTER a response row. For a 101, ReverseProxy runs ModifyResponse —
// which records the 101 — and only then handleUpgradeResponse, which reports a
// protocol mismatch or a failed hijack through ErrorHandler. A second, 502 row
// there would break the one-response-per-request pairing /v1/usage counts on.
func TestReverseProxy_FailedUpgrade_RecordsOneResponse(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		// Switches to a protocol the client did not ask for.
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: h2c\r\n\r\n")
		_ = buf.Flush()
	}))
	defer backend.Close()

	store, proxy := failureServer(t, backend.URL, allowOnlyPlugin{})

	conn, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL, "http://"))
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	host := strings.TrimPrefix(proxy.URL, "http://")
	if _, err := fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n", host); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read proxy response: %v", err)
	}
	resp.Body.Close()

	ev := waitForResponseEvent(t, store)
	// Give a second row the time to land, if it were going to.
	time.Sleep(100 * time.Millisecond)
	v := store.View(session.DefaultSessionID)
	var responses []pipeline.SessionEvent
	for _, e := range v.Events {
		if e.Phase == pipeline.SessionResponse {
			responses = append(responses, e)
		}
	}
	if len(responses) != 1 {
		t.Fatalf("recorded %d response rows, want 1 (the 101): %+v", len(responses), responses)
	}
	if ev.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("StatusCode = %d, want 101", ev.StatusCode)
	}
}

// failureServer builds a NewServer reverse proxy in front of backendURL over the
// given plugins, and returns its store and test server.
func failureServer(t *testing.T, backendURL string, plugins ...pipeline.Plugin) (*session.Store, *httptest.Server) {
	t.Helper()
	store := session.New(5*time.Minute, 100, 100)
	t.Cleanup(store.Close)
	srv, err := NewServer(pipelineWith(t, plugins...), store, backendURL, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)
	return store, proxy
}

// failedExchange sends one GET through a reverse proxy in front of target and
// returns its only response event and the status the client got.
func failedExchange(t *testing.T, target string, timeout time.Duration, plugins ...pipeline.Plugin) (pipeline.SessionEvent, int) {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatalf("parse %q: %v", target, err)
	}
	store, proxy := failureServer(t, u.Scheme+"://"+u.Host, plugins...)
	c := &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: nil}}
	resp, err := c.Get(proxy.URL + u.Path)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	resp.Body.Close()
	return waitForResponseEvent(t, store), resp.StatusCode
}

// assertBufferingFailure checks the shape every buffering failure records: the
// status the client got, why, and the backend's own status as Code.
func assertBufferingFailure(t *testing.T, ev pipeline.SessionEvent, status int, kind, code string) {
	t.Helper()
	if ev.StatusCode != status {
		t.Errorf("StatusCode = %d, want %d — the status the client got, not the backend's", ev.StatusCode, status)
	}
	if ev.Error == nil {
		t.Fatal("Error = nil, want the buffering failure")
	}
	if ev.Error.Kind != kind {
		t.Errorf("Error.Kind = %q, want %q", ev.Error.Kind, kind)
	}
	if ev.Error.Code != code {
		t.Errorf("Error.Code = %q, want %q (what the backend actually sent)", ev.Error.Code, code)
	}
}

// assertOutcomeError checks the Finisher saw a failure. On the buffering paths
// the backend did send a status, and left in pctx.StatusCode it would read as an
// allow — lineage labelling "ok" an exchange the client saw fail.
func assertOutcomeError(t *testing.T, f *finisherStub) {
	t.Helper()
	if !waitSeen(&f.seen) {
		t.Fatal("OnFinish never ran, so the outcome is unasserted")
	}
	if o := f.outcome.Load(); o == nil || o.FinalAction != pipeline.OutcomeError {
		t.Errorf("Outcome = %+v, want OutcomeError", o)
	}
}

// waitForResponseEvent polls the default bucket for a response event. A failure
// is recorded after the client may already have returned — immediately, on a
// hangup — so a bare read races the recording.
func waitForResponseEvent(t *testing.T, store *session.Store) pipeline.SessionEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if v := store.View(session.DefaultSessionID); v != nil {
			for _, ev := range v.Events {
				if ev.Phase == pipeline.SessionResponse {
					return ev
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no response event recorded")
	return pipeline.SessionEvent{}
}

// hermeticGet is http.Get without the environment's proxy. The default transport
// honours HTTP_PROXY / HTTPS_PROXY, which this repo's own laptop setup exports.
func hermeticGet(target string) (*http.Response, error) {
	return (&http.Client{Transport: &http.Transport{Proxy: nil}}).Get(target)
}
