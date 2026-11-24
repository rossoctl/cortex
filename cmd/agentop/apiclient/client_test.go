package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

func TestListSessions(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions" {
			t.Errorf("wrong path: %q", r.URL.Path)
		}
		json.NewEncoder(w).Encode(struct {
			Sessions []session.SessionSummary `json:"sessions"`
		}{
			Sessions: []session.SessionSummary{{ID: "abc", EventCount: 3}},
		})
	}))
	defer ts.Close()

	c := New(ts.URL)
	got, err := c.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "abc" || got[0].EventCount != 3 {
		t.Errorf("got %+v", got)
	}
}

func TestGetSession(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions/ctx-abc" {
			t.Errorf("wrong path: %q", r.URL.Path)
		}
		json.NewEncoder(w).Encode(pipeline.SessionView{
			ID: "ctx-abc",
			Events: []pipeline.SessionEvent{
				{Direction: pipeline.Inbound, Phase: pipeline.SessionRequest},
			},
		})
	}))
	defer ts.Close()

	c := New(ts.URL)
	got, err := c.GetSession(context.Background(), "ctx-abc")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "ctx-abc" || len(got.Events) != 1 {
		t.Errorf("got %+v", got)
	}
}

func TestGetSession_404(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer ts.Close()
	c := New(ts.URL)
	_, err := c.GetSession(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestEndpointTrimSlash(t *testing.T) {
	c := New("http://localhost:9094/")
	if c.Endpoint() != "http://localhost:9094" {
		t.Errorf("trailing slash not trimmed: %q", c.Endpoint())
	}
}

func TestGetPipeline(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/pipeline" {
			t.Errorf("wrong path: %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"inbound":  [{"name":"jwt-validation","direction":"inbound","position":1,"readsBody":false},
			             {"name":"a2a-parser","direction":"inbound","position":2,"readsBody":true}],
			"outbound": [{"name":"token-exchange","direction":"outbound","position":1}]
		}`))
	}))
	defer ts.Close()

	c := New(ts.URL)
	got, err := c.GetPipeline(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Inbound) != 2 || len(got.Outbound) != 1 {
		t.Fatalf("got %d inbound / %d outbound, want 2/1", len(got.Inbound), len(got.Outbound))
	}
	if got.Inbound[1].Name != "a2a-parser" || !got.Inbound[1].ReadsBody {
		t.Errorf("inbound[1] = %+v", got.Inbound[1])
	}
}

// TestPipelinePluginDecodesConfig verifies the new Config field on
// /v1/pipeline survives JSON round-trip through PipelinePlugin.
func TestPipelinePluginDecodesConfig(t *testing.T) {
	body := `{"inbound":[
	  {"name":"with-config","direction":"inbound","position":1,"readsBody":false,
	   "config":{"hello":"world"}},
	  {"name":"without-config","direction":"inbound","position":2,"readsBody":false}
	],"outbound":[]}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/pipeline" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := New(srv.URL)
	view, err := c.GetPipeline(context.Background())
	if err != nil {
		t.Fatalf("GetPipeline: %v", err)
	}
	if len(view.Inbound) != 2 {
		t.Fatalf("want 2 inbound, got %d", len(view.Inbound))
	}
	// First plugin: Config decoded.
	if string(view.Inbound[0].Config) != `{"hello":"world"}` {
		t.Fatalf("with-config Config: got %q want %q",
			string(view.Inbound[0].Config), `{"hello":"world"}`)
	}
	// Second plugin: Config absent → empty/nil.
	if len(view.Inbound[1].Config) != 0 {
		t.Fatalf("without-config Config should be empty, got %q",
			string(view.Inbound[1].Config))
	}
}

func TestGetPluginCatalog(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/plugins" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"plugins": [
				{"name": "alpha", "description": "A plugin", "writes": ["x"]},
				{"name": "beta", "requires": ["alpha"]}
			]
		}`))
	}))
	defer ts.Close()

	c := New(ts.URL)
	cat, err := c.GetPluginCatalog(context.Background())
	if err != nil {
		t.Fatalf("GetPluginCatalog: %v", err)
	}
	if len(cat.Plugins) != 2 {
		t.Fatalf("plugins = %d, want 2", len(cat.Plugins))
	}
	if cat.Plugins[0].Description != "A plugin" {
		t.Errorf("plugins[0].Description = %q", cat.Plugins[0].Description)
	}
	if len(cat.Plugins[1].Requires) != 1 || cat.Plugins[1].Requires[0] != "alpha" {
		t.Errorf("plugins[1].Requires = %v", cat.Plugins[1].Requires)
	}
}

// TestGetPluginCatalog_DecodesFieldSchemas guards against tag drift
// between server-side FieldSchemaEntry (core/sessionapi/server.go)
// and client-side PluginFieldEntry (here in apiclient). Every JSON
// key the server emits must decode into the matching Go field, or the
// agentop edit templates renderer silently loses metadata. Covers
// nested fields too — token-exchange's identity sub-schema is the
// real-world use case.
func TestGetPluginCatalog_DecodesFieldSchemas(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/plugins" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"plugins": [
				{
					"name": "te",
					"description": "Token exchange.",
					"fields": [
						{"name": "token_url", "type": "string", "description": "Endpoint."},
						{
							"name": "identity",
							"type": "object",
							"description": "Client credentials.",
							"fields": [
								{"name": "type", "type": "string", "required": true,
								 "description": "Scheme.", "enum": ["spiffe", "client-secret"]},
								{"name": "timeout_ms", "type": "int", "default": "5000"}
							]
						}
					]
				}
			]
		}`))
	}))
	defer ts.Close()

	c := New(ts.URL)
	cat, err := c.GetPluginCatalog(context.Background())
	if err != nil {
		t.Fatalf("GetPluginCatalog: %v", err)
	}
	if len(cat.Plugins) != 1 || len(cat.Plugins[0].Fields) != 2 {
		t.Fatalf("unexpected catalog shape: %+v", cat)
	}
	te := cat.Plugins[0]

	tokenURL := te.Fields[0]
	if tokenURL.Name != "token_url" || tokenURL.Type != "string" || tokenURL.Description != "Endpoint." {
		t.Errorf("token_url decode mismatch: %+v", tokenURL)
	}

	id := te.Fields[1]
	if id.Name != "identity" || id.Type != "object" || id.Description != "Client credentials." {
		t.Errorf("identity decode mismatch: %+v", id)
	}
	if len(id.Fields) != 2 {
		t.Fatalf("identity.Fields = %d, want 2", len(id.Fields))
	}

	idType := id.Fields[0]
	if idType.Name != "type" || !idType.Required || idType.Description != "Scheme." {
		t.Errorf("identity.type decode mismatch: %+v", idType)
	}
	if len(idType.Enum) != 2 || idType.Enum[0] != "spiffe" || idType.Enum[1] != "client-secret" {
		t.Errorf("identity.type.enum = %v", idType.Enum)
	}

	timeout := id.Fields[1]
	if timeout.Name != "timeout_ms" || timeout.Type != "int" || timeout.Default != "5000" {
		t.Errorf("identity.timeout_ms decode mismatch: %+v", timeout)
	}
}

// TestPipelinePluginDecodesCapabilityMetadata verifies the capability
// metadata fields decode correctly through the apiclient.
func TestPipelinePluginDecodesCapabilityMetadata(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/pipeline" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"inbound": [
				{
					"name": "rich",
					"direction": "inbound",
					"position": 1,
					"requires": ["a"],
					"requiresAny": ["b","c"],
					"description": "Rich plugin"
				}
			],
			"outbound": []
		}`))
	}))
	defer ts.Close()

	c := New(ts.URL)
	view, err := c.GetPipeline(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := view.Inbound[0]
	if len(p.Requires) != 1 || p.Requires[0] != "a" {
		t.Errorf("Requires = %v", p.Requires)
	}
	if len(p.RequiresAny) != 2 {
		t.Errorf("RequiresAny = %v", p.RequiresAny)
	}
	if p.Description != "Rich plugin" {
		t.Errorf("Description = %q", p.Description)
	}
}

func shortenRESTDefault(t *testing.T, d time.Duration) {
	t.Helper()
	prev := restDefaultTimeout
	restDefaultTimeout = d
	t.Cleanup(func() { restDefaultTimeout = prev })
}

// shortenHeaderTimeout must be called BEFORE New, which reads the value into the
// transport it clones. A test that sets it afterwards asserts nothing.
func shortenHeaderTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := responseHeaderTimeout
	responseHeaderTimeout = d
	t.Cleanup(func() { responseHeaderTimeout = prev })
}

// TestGetJSON_CallerDeadlineIsNotPreEmpted is the regression this file exists for
// twice over: a bound the CLIENT imposes must never cut a call short of the budget
// its CALLER set.
//
// The shape is `agentop cost` scaled down. That command allows 15s, because a symbolic
// window is answered by reading day files off disk, and the client carried a fixed 10s
// http.Client.Timeout, so every call died at 10s and the 15s could not be reached.
// Both are hard stops and the shorter always wins, which made the comment stating the
// 15s describe behaviour that could not happen.
//
// Written against the default rather than the old constant, so it catches a
// context.WithTimeout applied in getJSON UNCONDITIONALLY — WithTimeout shortens and never
// lengthens, so an unconditional one is the same defect under a different spelling. The
// default is a nanosecond, already over by the time the request is sent, so any call it
// governs fails; the caller's hour is out of play.
//
// It does NOT catch the other spelling. A 10s Timeout restored on the http.Client passes
// this test. That mutation is caught structurally by
// TestNew_NeitherClientCarriesAWholeRequestTimeout, and the pair is the guard — this one
// alone would have let it back in.
func TestGetJSON_CallerDeadlineIsNotPreEmpted(t *testing.T) {
	shortenRESTDefault(t, time.Nanosecond)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(struct {
			Sessions []session.SessionSummary `json:"sessions"`
		}{Sessions: []session.SessionSummary{{ID: "abc"}}})
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
	defer cancel()
	got, err := New(ts.URL).ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v — the caller budgeted an hour, so this error is a bound the "+
			"client imposed on top of the caller's own; a client-side timeout that pre-empts "+
			"the caller's makes the caller's stated budget unreachable", err)
	}
	if len(got) != 1 || got[0].ID != "abc" {
		t.Errorf("got %+v, want the one session the server sent", got)
	}
}

// TestGetJSON_DeadlinelessCallerIsStillBounded is the other half, and the reason the
// default was not simply deleted. The TUI hands its ROOT context to GetPipeline,
// GetPluginCatalog, ListSessions and GetSession, so for those four the client's
// default is the only thing between a dead endpoint and a fetch that never returns.
//
// Asserted through a server that answers at once and a default of zero: the context
// getJSON derives is done as it is made, and the transport refuses a done context
// before it sends anything, so the answer can never win. Without the default the call
// SUCCEEDS, so this fails on the missing error rather than by hanging.
func TestGetJSON_DeadlinelessCallerIsStillBounded(t *testing.T) {
	shortenRESTDefault(t, 0)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(struct {
			Sessions []session.SessionSummary `json:"sessions"`
		}{Sessions: []session.SessionSummary{{ID: "abc"}}})
	}))
	defer ts.Close()

	// No deadline, exactly as the TUI's root context has none.
	_, err := New(ts.URL).ListSessions(context.Background())
	if err == nil {
		t.Fatal("ListSessions returned nil error for a caller with no deadline against a server " +
			"that answers at once; the zero default was not applied, so a dead endpoint " +
			"would hang the TUI's pipeline and session fetches forever")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want context.DeadlineExceeded: the bound must be the default deadline, "+
			"not some other failure that happens to look like one", err)
	}
}

// TestGetBody_StreamingCallerIsBoundedBeforeTheHeaders covers the path getJSON's default
// CANNOT reach. GetSessionPage streams: getBody returns an unread body, so there is
// nothing for a buffered-call deadline to wrap, and removing http.Client.Timeout left
// that path with no bound at all until the transport got a header timeout.
//
// A server that accepts the connection and then never answers is the realistic failure —
// a wedged proxy, not a refused dial — and before the header timeout it hung the TUI
// until the operator quit.
//
// THE CALLER'S CONTEXT CARRIES AN HOUR, so neither it nor streamDefaultTimeout (which a
// deadline-less caller would get) can end the call: the header timeout, at a nanosecond, is
// the only bound in play. Without it the call waits on a server that never answers.
//
// NOT DISTINGUISHED BY errors.Is: net/http's "timeout awaiting response headers" ALSO
// satisfies errors.Is(err, context.DeadlineExceeded). The message names the mechanism.
func TestGetBody_StreamingCallerIsBoundedBeforeTheHeaders(t *testing.T) {
	shortenHeaderTimeout(t, time.Nanosecond)
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // wedged: connected, no headers
	}))
	defer func() { close(release); ts.Close() }()

	ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
	defer cancel()
	_, err := New(ts.URL).GetSessionPage(ctx, "s1", 0, 10)
	if err == nil {
		t.Fatal("GetSessionPage returned nil error against a server that never sent headers")
	}
	if !strings.Contains(err.Error(), "awaiting response headers") {
		t.Errorf("failed with %v, want the header timeout: the streaming path has no bound of "+
			"its own, and a caller passing a root context (the TUI does) hangs on a wedged "+
			"proxy for as long as streamDefaultTimeout", err)
	}
}

// TestGetBody_ASlowBodyIsNotCutOff is why the bound above is a HEADER timeout and not a
// timeout on the whole request.
//
// The deleted http.Client.Timeout covered the body read, which is exactly how a large
// snapshot — 5000 events, 17 seconds of transfer — failed at 10s and left the timeline
// empty. SnapshotEventLimit's own doc records that failure. A header timeout cannot
// reproduce it however long the body takes, and this asserts that difference rather than
// trusting the mechanism's name.
//
// Headers first, then a stall LONGER than the header timeout, then the real body: under a
// whole-request timeout this fails, under a header timeout it succeeds.
//
// The stall ends when a second request, sent after the first one's headers, has itself hit
// the header timeout: by then the first request's timer has run out too. The bound is a real
// net/http timer, so the first request's headers still have to arrive inside it.
func TestGetBody_ASlowBodyIsNotCutOff(t *testing.T) {
	shortenHeaderTimeout(t, 100*time.Millisecond)
	headersSent, release := make(chan struct{}), make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/wedged" {
			<-r.Context().Done() // connected, no headers, until the client gives up
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(headersSent)
		<-release // mid-body
		_ = json.NewEncoder(w).Encode(pipeline.SessionView{ID: "s1"})
	}))
	defer ts.Close()

	c := New(ts.URL)
	go func() {
		<-headersSent
		if _, err := c.getBody(t.Context(), "/wedged"); err == nil {
			t.Error("a request to a server that never sent headers succeeded")
		}
		close(release)
	}()
	view, err := c.GetSessionPage(context.Background(), "s1", 0, 10)
	if err != nil {
		t.Fatalf("GetSessionPage: %v — the headers arrived inside the header timeout and only "+
			"the BODY was slow, so this is a bound on the transfer; that bound is what made a "+
			"large session snapshot come up empty", err)
	}
	if view.ID != "s1" {
		t.Errorf("ID = %q, want s1", view.ID)
	}
}

// A 400 must be distinguishable from a dial failure, and the server's own explanation must
// survive to the caller: "that proxy does not know that window" is actionable where
// "unexpected status 400" sends an operator to check whether the proxy is running.
//
// Asserted on GetUsageWindow because the unsupported-window 400 is the reachable case:
// the server refuses a symbolic window it cannot serve.
func TestGetUsageWindow_BadRequestCarriesTheServersReason(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unsupported window"})
	}))
	defer ts.Close()

	_, err := New(ts.URL).GetUsageWindow(context.Background(), "yesterday", 0, "", "")
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("error = %v, want ErrBadRequest: a refusal the caller can act on must not "+
			"look like a transport failure", err)
	}
	if !strings.Contains(err.Error(), "unsupported window") {
		t.Errorf("error = %v, want the server's own message carried through", err)
	}
}

// A 400 body that is not the documented shape must produce a bare ErrBadRequest, never a
// line of someone else's markup in a terminal. Reachable: the endpoint is unauthenticated,
// so the thing answering may not be our proxy at all.
func TestGetUsageWindow_BadRequestWithAnUnreadableBodyStaysBare(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("<html><body>Bad Request</body></html>"))
	}))
	defer ts.Close()

	_, err := New(ts.URL).GetUsageWindow(context.Background(), "today", 0, "", "")
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("error = %v, want ErrBadRequest", err)
	}
	if strings.Contains(err.Error(), "html") {
		t.Errorf("error = %v: markup from an unrecognised responder reached the message", err)
	}
}

// ErrNoDetail SEPARATES "the refusal would not say why" from "it refused something else",
// and adds nothing to the message a user reads.
//
// A caller that wants to add a hint when the server's words did not arrive has no other
// way to tell the two apart: matching the server's wording makes a refusal about a
// DIFFERENT parameter look like a missing detail, which is the bug this fixes in
// `agentop export`. ErrBadRequest must keep matching either way — callers test for it.
func TestGetBody_ErrNoDetailMarksARefusalThatExplainedNothing(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantDetail bool
	}{
		{"the documented shape, so the words arrived", `{"error":"since must be a duration"}`, true},
		{"not the documented shape", "<html>Bad Request</html>", false},
		// Longer than the 512 bytes getBody reads: the JSON arrives cut mid-string, so
		// badRequestDetail cannot parse it. The export refusal's real failure mode.
		{"cut by the 512-byte read", `{"error":"` + strings.Repeat("x", 600) + `"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer ts.Close()

			_, err := New(ts.URL).Export(context.Background(), []string{"nope"}, "", false)
			if !errors.Is(err, ErrBadRequest) {
				t.Fatalf("error = %v, want ErrBadRequest in the chain either way", err)
			}
			if got := !errors.Is(err, ErrNoDetail); got != tc.wantDetail {
				t.Fatalf("error = %v: detail arrived = %v, want %v", err, got, tc.wantDetail)
			}
			// The sentinel is for matching, not for reading: it must not reach the message.
			if strings.Contains(err.Error(), "no detail") {
				t.Errorf("error = %v: the sentinel's own words reached a user-facing message", err)
			}
		})
	}
}

// GetUsageWindow sends the window VERBATIM and omits resolution when zero, which is the
// one-bucket path a symbolic window is served on. Asserted on the wire, because a
// stringified duration here ("0s", or "24h0m0s" for "today") is refused by the server and
// the failure would look like an unsupported window.
func TestGetUsageWindow_SendsTheWindowVerbatimAndOmitsAZeroResolution(t *testing.T) {
	var got string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"window": "today"})
	}))
	defer ts.Close()

	if _, err := New(ts.URL).GetUsageWindow(context.Background(), "today", 0, "", ""); err != nil {
		t.Fatalf("GetUsageWindow: %v", err)
	}
	if got != "window=today" {
		t.Errorf("query = %q, want exactly %q: a resolution sent alongside a symbolic window is "+
			"refused, and the server serves it as one bucket", got, "window=today")
	}
}

// TestNew_NeitherClientCarriesAWholeRequestTimeout pins the two structural facts the
// behavioural tests above cannot reach cheaply: no whole-request timeout on either
// http.Client, and a header timeout on the transport they share.
//
// STRUCTURAL ON PURPOSE. Catching a restored http.Client.Timeout behaviourally needs a test
// that sleeps past it, and the value is 10s — the suite is not the place for that. Reading
// the field costs nothing and fails on the exact edit.
//
// The two are asserted together because each alone permits a broken client: no timeout and
// no header timeout is an unbounded hang on the streaming path, and a whole-request timeout
// with a header timeout is the transfer cap that emptied the timeline. Only the pair is
// correct — see New.
func TestNew_NeitherClientCarriesAWholeRequestTimeout(t *testing.T) {
	c := New("http://127.0.0.1:1")
	if c.http.Timeout != 0 {
		t.Errorf("http.Timeout = %v, want 0: a Timeout here applies to every call this Client "+
			"will ever make, so it pre-empts any caller that budgeted more and it bounds the "+
			"body read as well as the wait", c.http.Timeout)
	}
	if c.httpStream.Timeout != 0 {
		t.Errorf("httpStream.Timeout = %v, want 0: this one carries SSE, which is unbounded by "+
			"definition", c.httpStream.Timeout)
	}
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport is %T, want *http.Transport", c.http.Transport)
	}
	if tr.ResponseHeaderTimeout != responseHeaderTimeout {
		t.Errorf("ResponseHeaderTimeout = %v, want %v: it is the only bound the streaming path "+
			"has, and dropping it lets a wedged proxy hang the TUI until the operator quits",
			tr.ResponseHeaderTimeout, responseHeaderTimeout)
	}
	// Both clients must share the transport, or the header timeout covers only one of them
	// and the idle-connection pool is duplicated.
	if c.httpStream.Transport != c.http.Transport {
		t.Error("the two clients do not share a Transport: the header timeout and the connection " +
			"pool would both apply to only one of them")
	}
}

// A caller's budget must survive the server THINKING for longer than the deadline-less
// default, which is the shape `agentop cost` is built around: /v1/usage computes its snapshot
// before writing any header, so a symbolic window's day-file walk happens entirely inside the
// wait for headers.
//
// The bound that gets this wrong is the transport's, not the client's, and it got it wrong
// once already: a 10s header bound capped a 15s budget exactly as the deleted 10s
// http.Client.Timeout had. This one leaves HeaderTimeout at its production value and shortens
// only the DEFAULT, to a nanosecond, so any think time the server spends is longer than the
// default.
//
// TestCallerBudgets_FitUnderTheHeaderBackstop in package main is the other half: this proves
// the mechanism works at production scale, that proves the two constants cannot invert.
func TestGetJSON_ServerThinkTimeInsideTheCallersBudgetSucceeds(t *testing.T) {
	shortenRESTDefault(t, time.Nanosecond)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"window": "today"})
	}))
	defer ts.Close()

	// A budget well above the think time, as costFetchTimeout is above a real scan.
	ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
	defer cancel()
	snap, err := New(ts.URL).GetUsageWindow(ctx, "today", 0, "", "")
	if err != nil {
		t.Fatalf("GetUsageWindow: %v — the caller budgeted an hour, so this is a bound the "+
			"CLIENT imposed on the server's think time. That is what a header timeout at a "+
			"caller-budget scale does, and it is why HeaderTimeout is %v",
			err, HeaderTimeout)
	}
	if snap.Window != "today" {
		t.Errorf("Window = %q, want today", snap.Window)
	}
}

// A peer that sends headers and then STALLS MID-BODY must not hang a caller that set no
// deadline of its own. The TUI is exactly that caller: tui/paging.go fetches each older page
// with the app's root context.
//
// THE HEADER BOUND CANNOT COVER THIS, by definition — it ends at the first response header —
// and removing http.Client.Timeout removed the only thing that did, so this path went from
// bounded at 10s to unbounded. Every other test in this file watches the pre-header window,
// which is why the regression had no coverage: the two stalls are different failures and only
// one of them was ever asserted.
//
// The body is deliberately left OPEN by the server and the client's read blocks; a bound has to
// come from somewhere other than the reader.
//
// The default runs out MID-BODY: the test ends it as soon as the headers arrive, and the stall
// lasts until the client gives up. The error has to come from the body read: a default that
// ends at the headers fails a call only before they arrive.
func TestGetBody_ADeadlinelessCallerIsBoundedMidBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(stallMidBody(`{"id":"s1","events":[`)))
	defer ts.Close()

	c := New(ts.URL)
	given := expireStreamDeadlineOnHeaders(t, c)
	// No deadline, exactly as the TUI's root context has none.
	_, err := c.GetSessionPage(context.Background(), "s1", 0, 10)
	if err == nil {
		t.Fatal("GetSessionPage returned no error against a server that stalled mid-body")
	}
	if !strings.Contains(err.Error(), ": decode: ") {
		t.Errorf("failed with %v, want a failed body read: the call ended before the body", err)
	}
	if *given != streamDefaultTimeout {
		t.Errorf("the call was bounded by %v, want streamDefaultTimeout (%v)", *given, streamDefaultTimeout)
	}
}

// stallMidBody sends the headers and the start of a body, then nothing until the client gives up.
func stallMidBody(start string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Headers are out, so the header bound is satisfied and no longer applies.
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte(start))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}
}

// expireStreamDeadlineOnHeaders makes streamDeadline end c's call as soon as its response
// headers arrive, until the test ends. The returned duration is the bound the call was given.
func expireStreamDeadlineOnHeaders(t *testing.T, c *Client) *time.Duration {
	t.Helper()
	var given time.Duration
	var expire context.CancelFunc
	streamDeadline = func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		given = d
		ctx, cancel := context.WithCancel(parent)
		expire = cancel
		return ctx, cancel
	}
	t.Cleanup(func() { streamDeadline = context.WithTimeout })
	c.http.Transport = expireAfterHeaders{c.http.Transport, func() {
		if expire != nil {
			expire()
		}
	}}
	return &given
}

// expireAfterHeaders runs expire once a response's headers have arrived.
type expireAfterHeaders struct {
	http.RoundTripper
	expire func()
}

func (e expireAfterHeaders) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := e.RoundTripper.RoundTrip(r)
	if err == nil {
		e.expire()
	}
	return resp, err
}

// And the other direction: the default must not cut off a caller that DID set a budget, nor a
// body that is merely slow within it. streamDefaultTimeout is a floor for the deadline-less,
// never a ceiling on anyone.
func TestGetBody_ASlowBodyInsideTheCallersBudgetSurvives(t *testing.T) {
	prev := streamDefaultTimeout
	streamDefaultTimeout = time.Nanosecond // over before the request is sent, and must be ignored
	t.Cleanup(func() { streamDefaultTimeout = prev })

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		_ = json.NewEncoder(w).Encode(pipeline.SessionView{ID: "s1"})
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
	defer cancel()
	view, err := New(ts.URL).GetSessionPage(ctx, "s1", 0, 10)
	if err != nil {
		t.Fatalf("GetSessionPage: %v — the caller budgeted an hour, so the client's own "+
			"default pre-empted a budget it is supposed to defer to", err)
	}
	if view.ID != "s1" {
		t.Errorf("ID = %q, want s1", view.ID)
	}
}

// A 400 MESSAGE IS PRINTED TO A TERMINAL, so it cannot carry control bytes.
//
// The endpoint is unauthenticated and this client cannot verify what answered, which is why the
// read is already capped at 512 bytes — that bound stops a flood, and this one stops a payload
// that fits inside it. An escape sequence in the message can reposition the cursor, recolour the
// rest of the session or hide what follows it, and `agentop cost` prints this straight to stderr
// while the TUI puts it in a flash line.
//
// pipeline.IsControlRune is the predicate the ledger and the aggregate already sanitise labels
// with, so the same byte is refused on every surface.
func TestGetUsageWindow_BadRequestDetailCarriesNoControlBytes(t *testing.T) {
	// A plausible server message with an escape sequence, a carriage return and a NUL spliced in.
	hostile := "unsupported window\x1b[2J\x1b[H\rwiped\x00"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": hostile})
	}))
	defer ts.Close()

	_, err := New(ts.URL).GetUsageWindow(context.Background(), "yesterday", 0, "", "")
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("error = %v, want ErrBadRequest", err)
	}
	msg := err.Error()
	for _, bad := range []string{"\x1b", "\r", "\x00"} {
		if strings.Contains(msg, bad) {
			t.Errorf("message %q carries %q — it is printed to a terminal", msg, bad)
		}
	}
	// The READABLE part survives, or sanitising would have cost the diagnostic it exists to carry.
	for _, want := range []string{"unsupported window", "wiped"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lost %q: stripping control bytes must not drop the text", msg, want)
		}
	}
}

// History is the archived list, asked for explicitly, with the archive's usage beside it.
func TestListSessionsArchived_SendsTheQueryAndDecodesUsage(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions" || r.URL.Query().Get("archived") != "true" {
			t.Errorf("request %s, want /v1/sessions?archived=true", r.URL)
		}
		w.Write([]byte(`{"sessions":[{"id":"old","resident":false}],"archive":{"bytes":42,"maxBytes":100,"retentionDays":30}}`))
	}))
	defer ts.Close()
	list, err := New(ts.URL).ListSessionsArchived(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].Resident == nil || *list.Sessions[0].Resident ||
		list.Archive == nil || list.Archive.Bytes != 42 {
		t.Fatalf("list = %+v", list)
	}
}

// A proxy without an archive, or one that predates it, ignores the query and answers the plain
// list: no archive object, and no error.
func TestListSessionsArchived_AnOldProxyIgnoresTheQuery(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"sessions":[{"id":"live"}]}`))
	}))
	defer ts.Close()
	list, err := New(ts.URL).ListSessionsArchived(context.Background())
	if err != nil || list.Archive != nil || len(list.Sessions) != 1 {
		t.Fatalf("list = %+v, err %v", list, err)
	}
}

// onError decodes when the proxy serves it — only for a plugin not under the default policy — and
// is "" when it does not, which is how agentop tells an observing plugin from one that acts.
func TestPipelinePluginDecodesOnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"inbound":[],"outbound":[
		  {"name":"inference-parser","direction":"outbound","position":1,"readsBody":true},
		  {"name":"inference-router","direction":"outbound","position":2,"readsBody":true,"onError":"observe"}]}`))
	}))
	defer srv.Close()
	view, err := New(srv.URL).GetPipeline(context.Background())
	if err != nil {
		t.Fatalf("GetPipeline: %v", err)
	}
	if got := view.Outbound[0].OnError; got != "" {
		t.Errorf("inference-parser OnError = %q, want empty", got)
	}
	if got := view.Outbound[1].OnError; got != pipeline.ErrorPolicyObserve {
		t.Errorf("inference-router OnError = %q, want observe", got)
	}
}

// GetUsageWindowScoped puts project= and agent= on the query.
func TestGetUsageWindowScoped_SendsProject(t *testing.T) {
	var got url.Values
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_, _ = w.Write([]byte(`{"window":"today","totals":{}}`))
	}))
	defer ts.Close()
	if _, err := New(ts.URL).GetUsageWindowScoped(context.Background(), "today", 0, "", "claude-code", "/repo/a", usage.GroupModel); err != nil {
		t.Fatal(err)
	}
	if got.Get("project") != "/repo/a" || got.Get("agent") != "claude-code" || got.Get("group") != "model" {
		t.Errorf("query = %v, want project=/repo/a, agent=claude-code and group=model", got)
	}
}
