package forwardproxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/rossoctl/cortex/core/listener/skiphost"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/plugintesting"
	"github.com/rossoctl/cortex/core/session"
)

// Issue #1045: a request whose upstream call fails at the transport level got a
// 502 on the wire and nothing in the session store. The request event was
// already appended, so the timeline showed a lone row with no status and no
// duration — indistinguishable from a request still in flight, which is exactly
// the wrong reading. /v1/usage saw neither a request nor an error, because it
// counts on the response half.
//
// These tests assert the paired response event now exists, carries the 502 and a
// classified error, and that the Finisher outcome did NOT change with it — that
// last one is the regression the fix is shaped around.

// failureProxy builds a forward proxy over an empty pipeline (no plugin needs to
// be involved for a transport failure to be recorded) and returns it with its
// store. skip may be nil.
func failureProxy(t *testing.T, skip *skiphost.Matcher, plugins ...pipeline.Plugin) (*httptest.Server, *session.Store) {
	t.Helper()
	p, err := plugintesting.BuildPipeline(plugins)
	if err != nil {
		t.Fatalf("build pipeline: %v", err)
	}
	store := session.New(5*time.Minute, 100, 0)
	t.Cleanup(store.Close)
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		// Not http.DefaultClient: its transport honours HTTP_PROXY / HTTPS_PROXY,
		// which this repo's own laptop setup exports.
		Client:    &http.Client{Transport: &http.Transport{Proxy: nil}},
		SkipHosts: skip,
	}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)
	return proxy, store
}

// viaProxy sends a GET through the proxy and returns the status the client saw.
func viaProxy(t *testing.T, proxy *httptest.Server, target string, timeout time.Duration) int {
	t.Helper()
	c := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(proxy.URL)),
		},
	}
	resp, err := c.Get(target)
	if err != nil {
		t.Fatalf("request through proxy failed outright: %v (wanted a 502 from the proxy)", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestForwardProxy_UpstreamRefused_RecordsResponseEvent is the core case: a
// destination that refuses the connection. Asserts the full shape of the pair,
// since this is the event every consumer downstream reads.
func TestForwardProxy_UpstreamRefused_RecordsResponseEvent(t *testing.T) {
	proxy, store := failureProxy(t, nil)

	if got := viaProxy(t, proxy, "http://"+closedAddr(t)+"/v1/thing", 5*time.Second); got != http.StatusBadGateway {
		t.Errorf("client status = %d, want 502", got)
	}

	events := allEvents(t, store)
	if len(events) != 2 {
		t.Fatalf("recorded %d event(s), want 2 (the request and its paired response); got %+v", len(events), events)
	}
	req, resp := events[0], events[1]
	if req.Phase != pipeline.SessionRequest {
		t.Fatalf("first event phase = %q, want %q", req.Phase, pipeline.SessionRequest)
	}
	if resp.Phase != pipeline.SessionResponse {
		t.Fatalf("second event phase = %q, want %q", resp.Phase, pipeline.SessionResponse)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("response StatusCode = %d, want 502 (a blank status reads as 'still in flight')", resp.StatusCode)
	}
	if resp.Error == nil {
		t.Fatal("response Error = nil, want the classified transport failure")
	}
	if resp.Error.Kind != "upstream_refused" {
		t.Errorf("Error.Kind = %q, want upstream_refused", resp.Error.Kind)
	}
	if resp.Error.Message == "" {
		t.Error("Error.Message is empty; it is the operator's only route to the address and cause")
	}
	// Pairing is by RequestID — agentop folds the two rows into one exchange on
	// it, and /v1/usage claims the request half with it.
	if resp.RequestID == "" || resp.RequestID != req.RequestID {
		t.Errorf("response RequestID = %q, want the request's (%q)", resp.RequestID, req.RequestID)
	}
	// A zero duration is read as "not measured" by /v1/usage's latency view, so
	// it would drop the request out of a third pane.
	if resp.Duration <= 0 {
		t.Errorf("response Duration = %v, want > 0", resp.Duration)
	}
	// Host/method keep the row identifiable for traffic no parser recognized.
	if resp.Host == "" || resp.HTTPMethod != http.MethodGet {
		t.Errorf("response Host/Method = %q/%q, want a host and GET", resp.Host, resp.HTTPMethod)
	}
}

// TestForwardProxy_ClientGivesUp_RecordsClientCanceled is the issue's own
// reproduction — a hanging upstream and a client that gives up first — driven
// through NewServer, because the production client is what decides the outcome.
// It has no Client.Timeout and no ResponseHeaderTimeout (NewServer says why), so
// nothing on the proxy side ever times a hung upstream out: the client's hangup
// cancels the call, and that cancellation is the error the proxy holds.
//
// Recorded as a 502 upstream failure, every Esc before headers would raise the
// error rate and the 502 series for a response nobody received, and log a WARN
// besides. It is a 499 client_canceled instead, and stays quiet.
func TestForwardProxy_ClientGivesUp_RecordsClientCanceled(t *testing.T) {
	logs := captureWarnings(t)
	release := make(chan struct{})
	// Hang until the test ends rather than sleeping a fixed span — httptest.Close
	// waits for outstanding handlers, so a sleep would be added to the run.
	hang := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer hang.Close()
	defer close(release)

	fin := &outcomeRecorder{}
	p, err := plugintesting.BuildPipeline([]pipeline.Plugin{fin})
	if err != nil {
		t.Fatalf("build pipeline: %v", err)
	}
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	srv, err := NewServer(pipeline.NewHolder(p), store, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	c := &http.Client{
		Timeout:   300 * time.Millisecond,
		Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))},
	}
	if _, err := c.Get(hang.URL + "/slow"); err == nil {
		t.Fatal("request succeeded; wanted the client to give up on a hung upstream")
	}

	// The proxy records once the cancellation reaches it, after the client has
	// already returned.
	resp := waitForResponseEvent(t, store)
	if resp.StatusCode != pipeline.StatusClientClosedRequest {
		t.Errorf("StatusCode = %d, want %d — a 502 here is a response nobody received", resp.StatusCode, pipeline.StatusClientClosedRequest)
	}
	if resp.Error == nil || resp.Error.Kind != "client_canceled" {
		t.Errorf("Error = %+v, want kind client_canceled", resp.Error)
	}
	if got := fin.seen(); got == nil || got.FinalAction != pipeline.OutcomeError {
		t.Errorf("Outcome = %+v, want OutcomeError", got)
	}
	if strings.Contains(logs.String(), "upstream request failed") {
		t.Errorf("logged a WARN for a client hangup: %s", logs.String())
	}
}

// TestForwardProxy_UpstreamTimeout_RecordsResponseEvent is the timeout the
// production transport DOES enforce — the TLS handshake — so upstream_timeout is
// shown reachable on the client NewServer builds, not on one only a test builds.
// The budget is shortened from NewServer's 10s; nothing else is changed.
func TestForwardProxy_UpstreamTimeout_RecordsResponseEvent(t *testing.T) {
	p, err := plugintesting.BuildPipeline(nil)
	if err != nil {
		t.Fatalf("build pipeline: %v", err)
	}
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	srv, err := NewServer(pipeline.NewHolder(p), store, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.Client.Transport.(*http.Transport).TLSHandshakeTimeout = 150 * time.Millisecond
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	// An absolute https:// request line, as a client speaking to an HTTP proxy
	// without CONNECT sends it, to an upstream that accepts and never answers.
	target := silentAddr(t)
	conn, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL, "http://"))
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(conn, "GET https://%s/slow HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		t.Fatalf("write request: %v", err)
	}
	got, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read proxy response: %v", err)
	}
	got.Body.Close()
	if got.StatusCode != http.StatusBadGateway {
		t.Errorf("client status = %d, want 502", got.StatusCode)
	}

	events := allEvents(t, store)
	if len(events) != 2 {
		t.Fatalf("recorded %d event(s), want 2; got %+v", len(events), events)
	}
	resp := events[1]
	if resp.Phase != pipeline.SessionResponse || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("second event = {phase %q, status %d}, want {response, 502}", resp.Phase, resp.StatusCode)
	}
	if resp.Error == nil || resp.Error.Kind != "upstream_timeout" {
		t.Errorf("Error = %+v, want kind upstream_timeout", resp.Error)
	}
}

// TestForwardProxy_UpstreamFailure_KeepsQueryOffTheTimeline guards the leak a
// *url.Error carries. client.Do quotes the request URL in its error, and net/http
// strips the password from it but not the query — where clients put API keys.
// The session API is unauthenticated, and HTTPPath is query-stripped precisely to
// keep those off it; the error and the WARN must not put them back.
func TestForwardProxy_UpstreamFailure_KeepsQueryOffTheTimeline(t *testing.T) {
	logs := captureWarnings(t)
	proxy, store := failureProxy(t, nil)

	viaProxy(t, proxy, "http://"+closedAddr(t)+"/v1/thing?api_key=SECRET1045", 5*time.Second)

	events := allEvents(t, store)
	if len(events) != 2 || events[1].Error == nil {
		t.Fatalf("want a recorded failure, got %+v", events)
	}
	if msg := events[1].Error.Message; strings.Contains(msg, "SECRET1045") || msg == "" {
		t.Errorf("Error.Message = %q, want the cause without the request's query string", msg)
	}
	if strings.Contains(logs.String(), "SECRET1045") {
		t.Errorf("the WARN carries the query string: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "upstream request failed") {
		t.Errorf("no WARN logged for a refused upstream; logs = %q", logs.String())
	}
}

// TestForwardProxy_UpstreamFailure_SkipHostsRecordsNothing guards the gate. A
// skip_hosts destination bypasses the pipeline AND recording by design — bypass
// means bypass, and a failure is not an exception to it. Without the !skipped
// check the fix would start recording events for exactly the infrastructure
// traffic skip_hosts exists to keep out of the store.
func TestForwardProxy_UpstreamFailure_SkipHostsRecordsNothing(t *testing.T) {
	// 127.0.0.1 matches without a glob; skiphost strips the port first.
	skip, err := skiphost.New([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("skiphost.New: %v", err)
	}
	proxy, store := failureProxy(t, skip)

	if got := viaProxy(t, proxy, "http://"+closedAddr(t)+"/skip-me", 5*time.Second); got != http.StatusBadGateway {
		t.Errorf("client status = %d, want 502 (skip still forwards, so it still fails)", got)
	}

	if sessions := store.ListSessions(); len(sessions) != 0 {
		t.Errorf("%d session(s) recorded, want 0 — a skip_hosts failure must stay unrecorded", len(sessions))
	}
}

// TestForwardProxy_UpstreamFailure_OutcomeStaysError is the regression guard for
// the trap in this fix. The 502 belongs on the EVENT only: OutcomeFromContext
// reads a zero pctx.StatusCode as OutcomeError and any non-zero one as
// OutcomeAllow, so assigning 502 there would tell every Finisher that a request
// which never got a response succeeded — lineage would label the span "ok".
func TestForwardProxy_UpstreamFailure_OutcomeStaysError(t *testing.T) {
	fin := &outcomeRecorder{}
	proxy, store := failureProxy(t, nil, fin)

	if got := viaProxy(t, proxy, "http://"+closedAddr(t)+"/v1/thing", 5*time.Second); got != http.StatusBadGateway {
		t.Errorf("client status = %d, want 502", got)
	}

	// The event still says 502 — the two are meant to disagree.
	events := allEvents(t, store)
	if len(events) != 2 || events[1].StatusCode != http.StatusBadGateway {
		t.Fatalf("want a recorded 502 response event, got %+v", events)
	}

	got := fin.seen()
	if got == nil {
		t.Fatal("OnFinish never ran, so the outcome is unasserted")
	}
	if got.FinalAction != pipeline.OutcomeError {
		t.Errorf("Outcome.FinalAction = %v, want OutcomeError — a request that never got a response is not an allow", got.FinalAction)
	}
	if got.StatusCode != 0 {
		t.Errorf("Outcome.StatusCode = %d, want 0 — the synthetic 502 must not reach the Outcome", got.StatusCode)
	}
}

// outcomeRecorder is a Finisher that captures the Outcome it was dispatched
// with, so a test can assert on what every real Finisher (lineage, in
// particular) would have seen.
type outcomeRecorder struct {
	mu  sync.Mutex
	got *pipeline.Outcome
}

func (*outcomeRecorder) Name() string { return "outcome-recorder" }
func (*outcomeRecorder) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}

func (*outcomeRecorder) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

func (*outcomeRecorder) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

func (r *outcomeRecorder) OnFinish(_ context.Context, pctx *pipeline.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if o := pctx.Outcome(); o != nil {
		c := *o
		r.got = &c
	}
}

// seen returns the captured Outcome, waiting briefly for it. RunFinish is
// dispatched from a defer in the proxy's handler, which can land after the
// client's response has been read — so a bare read races the dispatch.
func (r *outcomeRecorder) seen() *pipeline.Outcome {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		got := r.got
		r.mu.Unlock()
		if got != nil {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

// allEvents returns every event in the store, oldest first, across all sessions.
// A transport failure lands in the default bucket (nothing named the session),
// but asserting on that id specifically would make the test fail for the wrong
// reason if bucket resolution changed.
func allEvents(t *testing.T, store *session.Store) []pipeline.SessionEvent {
	t.Helper()
	var out []pipeline.SessionEvent
	for _, s := range store.ListSessions() {
		v := store.View(s.ID)
		if v == nil {
			continue
		}
		out = append(out, v.Events...)
	}
	return out
}

// bodyReadingPlugin forces the buffered response path, where the two
// response-buffering failures live.
type bodyReadingPlugin struct{}

func (bodyReadingPlugin) Name() string { return "body-reader" }
func (bodyReadingPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{ReadsBody: true}
}

func (bodyReadingPlugin) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

func (bodyReadingPlugin) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// TestForwardProxy_OversizeResponse_RecordsProxyError is the outbound twin of
// the reverseproxy case. A response the upstream delivered fine but that exceeds
// our own buffer ceiling is the proxy's failure, so it says proxy_error. The
// row's status is the 502 the client got — every consumer keys on it, and the
// upstream's 200 there would count this as a healthy exchange — and the 200
// survives as error.code.
//
// These returns also recorded NOTHING before, which is the #1045 bug on a second
// pair of paths.
func TestForwardProxy_OversizeResponse_RecordsProxyError(t *testing.T) {
	huge := bytes.Repeat([]byte("x"), maxBodySize+1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(huge)
	}))
	defer backend.Close()

	fin := &outcomeRecorder{}
	proxy, store := failureProxy(t, nil, bodyReadingPlugin{}, fin)

	if got := viaProxy(t, proxy, backend.URL+"/big", 30*time.Second); got != http.StatusBadGateway {
		t.Errorf("client status = %d, want 502 (we cannot relay what we cannot buffer)", got)
	}

	resp := onlyResponseEvent(t, store)
	assertBufferingFailure(t, resp, http.StatusBadGateway, "proxy_error", "200")
	assertOutcomeError(t, fin)
}

// TestForwardProxy_TruncatedResponse_RecordsUpstreamFailure is the other
// buffering failure, and the reason only the size limit says proxy_error. A body
// that breaks off mid-read is almost always the upstream resetting or truncating
// its own response, so it is classified like a failed call.
func TestForwardProxy_TruncatedResponse_RecordsUpstreamFailure(t *testing.T) {
	backend := truncatingBackend(t)
	fin := &outcomeRecorder{}
	proxy, store := failureProxy(t, nil, bodyReadingPlugin{}, fin)

	if got := viaProxy(t, proxy, backend+"/cut", 5*time.Second); got != http.StatusBadGateway {
		t.Errorf("client status = %d, want 502", got)
	}

	resp := onlyResponseEvent(t, store)
	assertBufferingFailure(t, resp, http.StatusBadGateway, "upstream_error", "200")
	assertOutcomeError(t, fin)
}

// TestForwardProxy_FallbackBuffered_BufferingFailures covers the same failures on
// streamFallbackBuffered, the SSE path's buffered sibling, which records through
// the same helper and had no test of either. Driven directly, as
// TestForwardProxy_BufferedFallbackFinalizesAfterAHangup is, because the
// conditions that reach it — a ResponseWriter with no Flush, a cancelled request
// context — are properties of the caller.
func TestForwardProxy_FallbackBuffered_BufferingFailures(t *testing.T) {
	tests := []struct {
		name   string
		body   io.Reader
		hungUp bool
		status int
		kind   string
	}{
		{
			name:   "read error",
			body:   io.MultiReader(strings.NewReader("data: {\"type\""), iotest.ErrReader(io.ErrUnexpectedEOF)),
			status: http.StatusBadGateway, kind: "upstream_error",
		},
		{
			// The read failed because the client went away while we read: the
			// body's error is then the cancellation, and nobody got the 502.
			name:   "read error after a hangup",
			body:   io.MultiReader(strings.NewReader("data: {\"type\""), iotest.ErrReader(context.Canceled)),
			hungUp: true,
			status: pipeline.StatusClientClosedRequest, kind: "client_canceled",
		},
		{
			name:   "over the buffer limit",
			body:   strings.NewReader(strings.Repeat("x", maxBodySize+1)),
			status: http.StatusBadGateway, kind: "proxy_error",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pipe, err := pipeline.New([]pipeline.Plugin{&frameProbe{}})
			if err != nil {
				t.Fatalf("New pipeline: %v", err)
			}
			store := session.New(5*time.Minute, 100, 0)
			defer store.Close()
			s := &Server{OutboundPipeline: pipeline.NewHolder(pipe), Sessions: store}

			ctx := context.Background()
			if tc.hungUp {
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			}
			r := httptest.NewRequest(http.MethodPost, "http://gw.internal/v1/messages", nil).WithContext(ctx)
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(tc.body),
			}
			pctx := &pipeline.Context{
				Direction:  pipeline.Outbound,
				Host:       "gw.internal",
				Method:     http.MethodPost,
				Path:       "/v1/messages",
				StatusCode: resp.StatusCode, // as serveOutbound sets it before this runs
			}

			rec := httptest.NewRecorder()
			s.streamFallbackBuffered(noFlushWriter{rec}, r, resp, pctx)

			if rec.Code != http.StatusBadGateway {
				t.Errorf("client status = %d, want 502", rec.Code)
			}
			assertBufferingFailure(t, onlyResponseEvent(t, store), tc.status, tc.kind, "200")
			// What OutcomeFromContext reads: zero is an error, the upstream's 200
			// would be an allow for an exchange the client saw fail.
			if pctx.StatusCode != 0 {
				t.Errorf("pctx.StatusCode = %d, want 0 so the Outcome is an error, not an allow", pctx.StatusCode)
			}
		})
	}
}

// assertBufferingFailure checks the shape every buffering failure records: the
// status the client got (or 499), why, and the upstream's own status as Code.
func assertBufferingFailure(t *testing.T, resp pipeline.SessionEvent, status int, kind, code string) {
	t.Helper()
	if resp.StatusCode != status {
		t.Errorf("StatusCode = %d, want %d — the status the client got, not the upstream's", resp.StatusCode, status)
	}
	if resp.Error == nil {
		t.Fatal("Error = nil, want the buffering failure")
	}
	if resp.Error.Kind != kind {
		t.Errorf("Error.Kind = %q, want %q", resp.Error.Kind, kind)
	}
	if resp.Error.Code != code {
		t.Errorf("Error.Code = %q, want %q (what the upstream actually sent)", resp.Error.Code, code)
	}
}

// assertOutcomeError checks the Finisher saw a failure. The upstream did send a
// status, and left in pctx.StatusCode it would read as an allow — lineage
// labelling "ok" an exchange the client saw fail.
func assertOutcomeError(t *testing.T, fin *outcomeRecorder) {
	t.Helper()
	got := fin.seen()
	if got == nil {
		t.Fatal("OnFinish never ran, so the outcome is unasserted")
	}
	if got.FinalAction != pipeline.OutcomeError {
		t.Errorf("Outcome.FinalAction = %v, want OutcomeError", got.FinalAction)
	}
}

// onlyResponseEvent returns the single response event in the store, failing the
// test on none or several — one per request is what /v1/usage pairs on.
func onlyResponseEvent(t *testing.T, store *session.Store) pipeline.SessionEvent {
	t.Helper()
	var out []pipeline.SessionEvent
	for _, ev := range allEvents(t, store) {
		if ev.Phase == pipeline.SessionResponse {
			out = append(out, ev)
		}
	}
	if len(out) != 1 {
		t.Fatalf("recorded %d response event(s), want exactly 1: %+v", len(out), out)
	}
	return out[0]
}

// waitForResponseEvent polls for the response event a failure records after the
// client has already given up and returned.
func waitForResponseEvent(t *testing.T, store *session.Store) pipeline.SessionEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range allEvents(t, store) {
			if ev.Phase == pipeline.SessionResponse {
				return ev
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no response event recorded; events = %+v", allEvents(t, store))
	return pipeline.SessionEvent{}
}

// truncatingBackend returns the URL of an upstream that promises a 1000-byte
// body, sends 11 bytes of it, and closes — a real mid-body failure, read through
// the real transport.
func truncatingBackend(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 1000\r\nContent-Type: application/json\r\n\r\nhello world")
		_ = buf.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// silentAddr returns an address that accepts TCP connections and never writes a
// byte, so a TLS handshake against it can only run out of time.
func silentAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var conns []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns = append(conns, c)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return ln.Addr().String()
}

// captureWarnings routes slog's default logger at WARN and above into a buffer
// for the rest of the test. Safe because this package's tests do not run in
// parallel; see TestForwardProxy_SSE_ReadsBodyPluginWarnsAndStreams.
func captureWarnings(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}
