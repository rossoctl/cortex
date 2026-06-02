package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/session"
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
// between server-side FieldSchemaEntry (authlib/sessionapi/server.go)
// and client-side PluginFieldEntry (here in apiclient). Every JSON
// key the server emits must decode into the matching Go field, or the
// abctl edit templates renderer silently loses metadata. Covers
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
