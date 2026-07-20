package sessionapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
)

// newTestServer wires a Server backed by a fresh Store onto httptest so tests
// hit the real mux and real handlers. Returns a teardown closer.
func newTestServer(t *testing.T, opts ...Option) (*httptest.Server, *session.Store) {
	t.Helper()
	store := session.New(5*time.Minute, 100, 0)
	// Use a tiny heartbeat by default so SSE tests don't hang.
	opts = append([]Option{WithHeartbeatInterval(50 * time.Millisecond)}, opts...)
	srv := New(":0", store, opts...)
	ts := httptest.NewServer(srv.server.Handler)
	t.Cleanup(func() {
		ts.Close()
		store.Close()
	})
	return ts, store
}

func TestHandleList_Empty(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Sessions []session.SessionSummary `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sessions) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(body.Sessions))
	}
}

func TestHandleList_ShowsAppendedSessions(t *testing.T) {
	ts, store := newTestServer(t)
	store.Append("s1", pipeline.SessionEvent{A2A: &pipeline.A2AExtension{}})
	store.Append("s2", pipeline.SessionEvent{A2A: &pipeline.A2AExtension{}})
	store.Append("s2", pipeline.SessionEvent{A2A: &pipeline.A2AExtension{}})

	resp, err := http.Get(ts.URL + "/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Sessions []session.SessionSummary `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d (%+v)", len(body.Sessions), body.Sessions)
	}
	// Most recently updated first.
	if body.Sessions[0].ID != "s2" {
		t.Errorf("first = %q, want s2", body.Sessions[0].ID)
	}
	if body.Sessions[0].EventCount != 2 {
		t.Errorf("s2 eventCount = %d, want 2", body.Sessions[0].EventCount)
	}
	if !body.Sessions[0].Active {
		t.Errorf("s2 should be marked active")
	}
}

func TestHandleGet_NotFound(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/sessions/does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHandleGet_ReturnsEventsAndReadableEnums(t *testing.T) {
	ts, store := newTestServer(t)
	store.Append("s1", pipeline.SessionEvent{
		Direction: pipeline.Inbound,
		Phase:     pipeline.SessionResponse,
		A2A:       &pipeline.A2AExtension{Method: "message/stream"},
	})

	resp, err := http.Get(ts.URL + "/v1/sessions/s1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), `"direction":"inbound"`) {
		t.Errorf("expected string enums in payload: %s", raw)
	}
	if !strings.Contains(string(raw), `"phase":"response"`) {
		t.Errorf("expected string phase in payload: %s", raw)
	}
}

func TestHandleStream_DeliversAppendedEvent(t *testing.T) {
	ts, store := newTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/v1/events", nil)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", resp.Header.Get("Content-Type"))
	}

	// Scanner with a 4MB buffer; SSE frames can be large.
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 8192), 4<<20)

	// Wait for the initial ": ok" comment to know the handler is ready.
	waitForLine(t, sc, ":", "initial comment", 2*time.Second)

	// Now append an event and expect it in the stream.
	store.Append("s1", pipeline.SessionEvent{
		Direction: pipeline.Inbound,
		Phase:     pipeline.SessionRequest,
		A2A:       &pipeline.A2AExtension{Method: "message/stream"},
	})

	// Expect event/id/data lines, in order, within 2s.
	sawEvent := scanUntil(t, sc, "event: session-event", 2*time.Second)
	if !sawEvent {
		t.Fatal("event: session-event line never arrived")
	}
	sawID := scanUntil(t, sc, "id: 1", time.Second)
	if !sawID {
		t.Error("id: 1 line never arrived")
	}
	sawData := scanUntilPrefix(t, sc, "data: ", 5*time.Second)
	if sawData == "" {
		t.Fatal("data: line never arrived")
	}
	if !strings.Contains(sawData, `"method":"message/stream"`) {
		t.Errorf("data line missing expected field: %s", sawData)
	}
}

func TestHandleStream_Heartbeat(t *testing.T) {
	// With heartbeat=25ms, we should see at least one heartbeat comment
	// within 200ms even with no events appended.
	ts, _ := newTestServer(t, WithHeartbeatInterval(25*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/v1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	// Consume the initial ": ok", then expect ": heartbeat" within the window.
	waitForLine(t, sc, ":", "initial", 5*time.Second)
	got := scanUntilExact(t, sc, ": heartbeat", 500*time.Millisecond)
	if !got {
		t.Error("no heartbeat observed within 500ms")
	}
}

func TestHandleStream_SessionFilter(t *testing.T) {
	ts, store := newTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/v1/events?session=keep", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 8192), 4<<20)
	waitForLine(t, sc, ":", "initial", 5*time.Second)

	// Event with a different SessionID — should be filtered out.
	store.Append("drop", pipeline.SessionEvent{
		A2A: &pipeline.A2AExtension{Method: "message/stream"},
	})
	// Event with matching SessionID — should come through.
	store.Append("keep", pipeline.SessionEvent{
		A2A: &pipeline.A2AExtension{Method: "message/stream"},
	})

	data := scanUntilPrefix(t, sc, "data: ", 5*time.Second)
	if data == "" {
		t.Fatal("no data frame received")
	}
	if !strings.Contains(data, `"sessionId":"keep"`) {
		t.Errorf("expected keep session in stream, got: %s", data)
	}
	if strings.Contains(data, `"sessionId":"drop"`) {
		t.Errorf("unfiltered drop event leaked: %s", data)
	}
}

func TestHandleStream_SessionFilter_WorksOnOutboundMCP(t *testing.T) {
	// Prior to SessionEvent.SessionID being populated at Append time, this
	// test would have failed: the outbound MCP event has no A2A extension
	// and the old filter check bailed when A2A was nil, letting foreign
	// events through. Guards against regression.
	ts, store := newTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/v1/events?session=keep", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 8192), 4<<20)
	waitForLine(t, sc, ":", "initial", 5*time.Second)

	// Outbound MCP call appended to the wrong bucket — must be filtered.
	store.Append("drop", pipeline.SessionEvent{
		MCP: &pipeline.MCPExtension{Method: "tools/call"},
	})
	// Outbound MCP call in the target bucket — must pass.
	store.Append("keep", pipeline.SessionEvent{
		MCP: &pipeline.MCPExtension{Method: "tools/list"},
	})

	data := scanUntilPrefix(t, sc, "data: ", 5*time.Second)
	if data == "" {
		t.Fatal("no data frame received within deadline")
	}
	if !strings.Contains(data, `"sessionId":"keep"`) {
		t.Errorf("expected sessionId:keep, got: %s", data)
	}
	if !strings.Contains(data, `"method":"tools/list"`) {
		t.Errorf("expected MCP tools/list event, got: %s", data)
	}
}

// fakePlugin implements pipeline.Plugin for the /v1/pipeline handler tests
// without pulling in the real plugins package (credential files, etc.).
type fakePlugin struct {
	name string
	caps pipeline.PluginCapabilities
}

func (f *fakePlugin) Name() string                              { return f.name }
func (f *fakePlugin) Capabilities() pipeline.PluginCapabilities { return f.caps }
func (f *fakePlugin) OnRequest(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (f *fakePlugin) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

func TestHandlePipeline(t *testing.T) {
	inbound, err := pipeline.New([]pipeline.Plugin{
		&fakePlugin{name: "jwt-validation"},
		&fakePlugin{name: "a2a-parser", caps: pipeline.PluginCapabilities{ReadsBody: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	outbound, err := pipeline.New([]pipeline.Plugin{
		&fakePlugin{name: "token-exchange"},
		&fakePlugin{name: "mcp-parser", caps: pipeline.PluginCapabilities{ReadsBody: true}},
	})
	if err != nil {
		t.Fatal(err)
	}

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	srv := New(":0", store, WithPipelines(pipeline.NewHolder(inbound), pipeline.NewHolder(outbound)))
	ts := httptest.NewServer(srv.server.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/pipeline")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body struct {
		Inbound  []pipelinePluginView `json:"inbound"`
		Outbound []pipelinePluginView `json:"outbound"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Inbound) != 2 || len(body.Outbound) != 2 {
		t.Fatalf("inbound=%d outbound=%d, want 2/2", len(body.Inbound), len(body.Outbound))
	}
	if body.Inbound[0].Name != "jwt-validation" || body.Inbound[0].Position != 1 {
		t.Errorf("inbound[0] = %+v", body.Inbound[0])
	}
	if !body.Inbound[1].ReadsBody {
		t.Errorf("inbound[1] should have ReadsBody=true, got %+v", body.Inbound[1])
	}
	if body.Outbound[1].Direction != "outbound" {
		t.Errorf("outbound direction = %q, want outbound", body.Outbound[1].Direction)
	}
}

func TestHandlePipeline_NilPipelines(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	srv := New(":0", store) // no WithPipelines
	ts := httptest.NewServer(srv.server.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/pipeline")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var body struct {
		Inbound  []pipelinePluginView `json:"inbound"`
		Outbound []pipelinePluginView `json:"outbound"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	// Empty slices, not null — the UI expects [].
	if body.Inbound == nil || body.Outbound == nil {
		t.Errorf("want empty slices, got nil: %+v", body)
	}
	if len(body.Inbound) != 0 || len(body.Outbound) != 0 {
		t.Errorf("want empty, got inbound=%d outbound=%d", len(body.Inbound), len(body.Outbound))
	}
}

// TestHandlePipelineSurfacesConfig verifies that the Config field on
// /v1/pipeline carries each Configurable plugin's raw config bytes
// (when wrapped by the registry's WrapConfigured), and that
// non-Configurable plugins emit no Config field.
func TestHandlePipelineSurfacesConfig(t *testing.T) {
	configRaw := json.RawMessage(`{"hello":"world"}`)
	wrapped := pipeline.WrapConfigured(&fakePlugin{name: "with-config"}, configRaw)
	plain := &fakePlugin{name: "without-config"}

	inbound, err := pipeline.New([]pipeline.Plugin{wrapped, plain})
	if err != nil {
		t.Fatal(err)
	}

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	srv := New(":0", store, WithPipelines(pipeline.NewHolder(inbound), nil))
	ts := httptest.NewServer(srv.server.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/pipeline")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body struct {
		Inbound []struct {
			Name   string          `json:"name"`
			Config json.RawMessage `json:"config,omitempty"`
		} `json:"inbound"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Inbound) != 2 {
		t.Fatalf("want 2 plugins, got %d", len(body.Inbound))
	}
	for _, p := range body.Inbound {
		switch p.Name {
		case "with-config":
			if string(p.Config) != `{"hello":"world"}` {
				t.Fatalf("with-config Config: got %q want %q",
					string(p.Config), `{"hello":"world"}`)
			}
		case "without-config":
			if len(p.Config) != 0 {
				t.Fatalf("without-config should emit no Config, got %q",
					string(p.Config))
			}
		default:
			t.Fatalf("unexpected plugin name: %q", p.Name)
		}
	}
}

func TestHandleHealthz(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("ok")) {
		t.Errorf("body = %q, want to contain \"ok\"", body)
	}
}

// --- helpers -------------------------------------------------------------

// waitForLine scans until a line equal to want appears or the deadline fires.
func waitForLine(t *testing.T, sc *bufio.Scanner, want, label string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !sc.Scan() {
			t.Fatalf("scanner closed before %s line; err=%v", label, sc.Err())
		}
		if sc.Text() == want || strings.HasPrefix(sc.Text(), want) {
			return
		}
	}
	t.Fatalf("deadline waiting for %s line (%q)", label, want)
}

// scanUntil returns true if line == want appears before the deadline.
func scanUntil(t *testing.T, sc *bufio.Scanner, want string, d time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !sc.Scan() {
			return false
		}
		if sc.Text() == want {
			return true
		}
	}
	return false
}

// scanUntilExact returns true if the exact line appears before the deadline.
func scanUntilExact(t *testing.T, sc *bufio.Scanner, exact string, d time.Duration) bool {
	return scanUntil(t, sc, exact, d)
}

// scanUntilPrefix returns the line (once) if it starts with prefix, else "".
func scanUntilPrefix(t *testing.T, sc *bufio.Scanner, prefix string, d time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !sc.Scan() {
			return ""
		}
		if strings.HasPrefix(sc.Text(), prefix) {
			return sc.Text()
		}
	}
	return ""
}

// TestHandleGet_SerializesInvocations verifies the wire shape of
// SessionEvent.Invocations on /v1/sessions/{id}. A downstream consumer
// (abctl, curl pipes, scripts) must be able to decode the structured
// Inbound / Outbound slices without a side channel — this locks the
// schema, including the 5-value Action vocabulary.
func TestHandleGet_SerializesInvocations(t *testing.T) {
	ts, store := newTestServer(t)
	store.Append("s-inv", pipeline.SessionEvent{
		Direction: pipeline.Inbound,
		Phase:     pipeline.SessionDenied,
		Invocations: &pipeline.Invocations{
			Inbound: []pipeline.Invocation{{
				Plugin: "jwt-validation",
				Action: pipeline.ActionDeny,
				Reason: "jwt_failed",
				Details: map[string]string{
					"expected_issuer":   "http://issuer.example",
					"expected_audience": "agent-aud",
				},
			}},
		},
	})

	resp, err := http.Get(ts.URL + "/v1/sessions/s-inv")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)

	// Phase renders as string
	if !strings.Contains(string(body), `"phase":"denied"`) {
		t.Errorf("missing denied phase in wire: %s", body)
	}
	// Invocations sub-object present; action rendered as the
	// 5-value string, not the old "decision":"deny" shape.
	if !strings.Contains(string(body), `"invocations":`) {
		t.Errorf("missing invocations field in wire: %s", body)
	}
	if !strings.Contains(string(body), `"action":"deny"`) {
		t.Errorf("missing action=deny in wire: %s", body)
	}

	// Structural decode — consumer can unmarshal straight into the
	// canonical types. This is the contract abctl relies on.
	var view pipeline.SessionView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("SessionView unmarshal: %v", err)
	}
	if len(view.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(view.Events))
	}
	got := view.Events[0]
	if got.Phase != pipeline.SessionDenied {
		t.Errorf("Phase = %v, want SessionDenied", got.Phase)
	}
	if got.Invocations == nil || len(got.Invocations.Inbound) != 1 {
		t.Fatalf("Invocations not decoded: %+v", got.Invocations)
	}
	inv := got.Invocations.Inbound[0]
	if inv.Action != pipeline.ActionDeny {
		t.Errorf("Action = %q, want deny", inv.Action)
	}
	if inv.Reason != "jwt_failed" {
		t.Errorf("Reason = %q, want jwt_failed", inv.Reason)
	}
}

// TestHandleGet_SerializesPluginsMap verifies the escape-hatch Plugins
// field round-trips as keyed json.RawMessage — abctl consumes each
// plugin's payload by key without needing to know the plugin's schema
// at compile time.
func TestHandleGet_SerializesPluginsMap(t *testing.T) {
	ts, store := newTestServer(t)
	store.Append("s-plug", pipeline.SessionEvent{
		Direction: pipeline.Outbound,
		Phase:     pipeline.SessionResponse,
		Plugins: map[string]json.RawMessage{
			"rate-limiter": json.RawMessage(`{"allowed":true,"tokensLeft":42}`),
		},
	})

	resp, err := http.Get(ts.URL + "/v1/sessions/s-plug")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(body), `"plugins":{"rate-limiter":`) {
		t.Errorf("plugins field missing or reshaped: %s", body)
	}

	var view pipeline.SessionView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	raw, ok := view.Events[0].Plugins["rate-limiter"]
	if !ok {
		t.Fatalf("rate-limiter key missing: %+v", view.Events[0].Plugins)
	}
	// Round-trip the per-plugin payload to a caller-defined type — the
	// exact pattern abctl will use to render plugin events it knows
	// about, while leaving unknown plugins as raw JSON.
	var payload struct {
		Allowed    bool `json:"allowed"`
		TokensLeft int  `json:"tokensLeft"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("plugin payload decode: %v", err)
	}
	if !payload.Allowed || payload.TokensLeft != 42 {
		t.Errorf("payload drift: %+v", payload)
	}
}

// TestHandlePipelineSurfacesCapabilityMetadata verifies capability metadata
// (Requires/RequiresAny/Description) flows through to /v1/pipeline and is
// omitted when empty.
func TestHandlePipelineSurfacesCapabilityMetadata(t *testing.T) {
	rich := pipeline.PluginCapabilities{
		Requires:    []string{"a2a-parser"},
		RequiresAny: []string{"jwt-validation", "token-broker"},
		Description: "Test plugin description",
	}
	inbound, err := pipeline.New([]pipeline.Plugin{
		&fakePlugin{name: "rich-plugin", caps: rich},
		&fakePlugin{name: "bare-plugin"},
	})
	if err != nil {
		t.Fatal(err)
	}

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	srv := New(":0", store, WithPipelines(pipeline.NewHolder(inbound), nil))
	ts := httptest.NewServer(srv.server.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/pipeline")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Decode into raw JSON to verify omitempty for the bare plugin.
	var raw struct {
		Inbound []map[string]any `json:"inbound"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Inbound) != 2 {
		t.Fatalf("inbound = %d, want 2", len(raw.Inbound))
	}

	got := raw.Inbound[0]
	if got["description"] != "Test plugin description" {
		t.Errorf("description = %v", got["description"])
	}
	if reqs, _ := got["requires"].([]any); len(reqs) != 1 || reqs[0] != "a2a-parser" {
		t.Errorf("requires = %v", got["requires"])
	}
	if reqA, _ := got["requiresAny"].([]any); len(reqA) != 2 {
		t.Errorf("requiresAny = %v", got["requiresAny"])
	}

	bare := raw.Inbound[1]
	for _, k := range []string{"requires", "requiresAny", "description"} {
		if _, present := bare[k]; present {
			t.Errorf("bare plugin should omit %q, got %v", k, bare[k])
		}
	}
}

func TestHandlePluginCatalog_ListsRegisteredPlugins(t *testing.T) {
	stub := func() []CatalogEntry {
		return []CatalogEntry{
			{Name: "alpha", Description: "First plugin"},
			{Name: "beta", Requires: []string{"alpha"}},
		}
	}
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	srv := New(":0", store, WithCatalog(stub))
	ts := httptest.NewServer(srv.server.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/plugins")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Plugins []CatalogEntry `json:"plugins"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Plugins) != 2 {
		t.Fatalf("plugins = %d, want 2", len(body.Plugins))
	}
	if body.Plugins[0].Name != "alpha" || body.Plugins[0].Description != "First plugin" {
		t.Errorf("plugins[0] = %+v", body.Plugins[0])
	}
	if len(body.Plugins[1].Requires) != 1 || body.Plugins[1].Requires[0] != "alpha" {
		t.Errorf("plugins[1].Requires = %v", body.Plugins[1].Requires)
	}
}

// TestHandlePluginCatalog_IncludesFieldSchemas confirms field-level
// schemas attached by the catalog provider make it onto the wire
// (and that omitempty hides them when absent).
func TestHandlePluginCatalog_IncludesFieldSchemas(t *testing.T) {
	stub := func() []CatalogEntry {
		return []CatalogEntry{
			{
				Name:        "alpha",
				Description: "Has fields",
				Fields: []FieldSchemaEntry{
					{Name: "endpoint", Type: "string", Required: true, Description: "API URL."},
					{Name: "policy", Type: "string", Default: "allow", Enum: []string{"allow", "deny"}},
				},
			},
			{Name: "beta", Description: "No fields"},
		}
	}
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	srv := New(":0", store, WithCatalog(stub))
	ts := httptest.NewServer(srv.server.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/plugins")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var body struct {
		Plugins []CatalogEntry `json:"plugins"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Plugins) != 2 {
		t.Fatalf("plugins = %d, want 2", len(body.Plugins))
	}
	// alpha: fields populated, including required/enum/default.
	if got := len(body.Plugins[0].Fields); got != 2 {
		t.Fatalf("alpha.Fields = %d, want 2", got)
	}
	if !body.Plugins[0].Fields[0].Required {
		t.Errorf("alpha.endpoint should be required")
	}
	if body.Plugins[0].Fields[1].Default != "allow" {
		t.Errorf("alpha.policy.Default = %q", body.Plugins[0].Fields[1].Default)
	}
	if got := len(body.Plugins[0].Fields[1].Enum); got != 2 {
		t.Errorf("alpha.policy.Enum length = %d", got)
	}
	// beta: no fields → wire format omits the field; decoded slice is nil/empty.
	if len(body.Plugins[1].Fields) != 0 {
		t.Errorf("beta.Fields should be empty, got %+v", body.Plugins[1].Fields)
	}
}

func TestHandlePluginCatalog_NoProvider404(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	srv := New(":0", store) // no WithCatalog
	ts := httptest.NewServer(srv.server.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/plugins")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
