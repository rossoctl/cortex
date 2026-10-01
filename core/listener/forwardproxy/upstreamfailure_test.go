package forwardproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
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
		Client:           http.DefaultClient,
		SkipHosts:        skip,
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

// TestForwardProxy_UpstreamTimeout_RecordsResponseEvent is the issue's own
// reproduction: a hanging upstream and a client that gives up first.
func TestForwardProxy_UpstreamTimeout_RecordsResponseEvent(t *testing.T) {
	release := make(chan struct{})
	// Hang until the test ends rather than sleeping a fixed span — httptest.Close
	// waits for outstanding handlers, so a sleep would be added to the run.
	hang := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer hang.Close()
	defer close(release)

	// The timeout has to be the PROXY's, not the test client's: a client that
	// gives up first never reads the proxy's 502, and the recording happens on
	// the proxy side either way. So give the proxy's upstream client the short
	// budget and let the test client wait.
	p, err := plugintesting.BuildPipeline(nil)
	if err != nil {
		t.Fatalf("build pipeline: %v", err)
	}
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Client:           &http.Client{Timeout: 150 * time.Millisecond},
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	if got := viaProxy(t, proxy, hang.URL+"/slow", 10*time.Second); got != http.StatusBadGateway {
		t.Errorf("client status = %d, want 502", got)
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
