package apiclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGetPipeline_DecodesPluginMetrics guards against tag drift between
// server-side pipelinePluginView.Metrics (core/sessionapi/server.go) and
// client-side PluginMetric here. The payload below is the exact shape the
// server emits; if a key stops decoding, the abctl metrics pane silently
// renders zeros, which is worse than rendering nothing.
func TestGetPipeline_DecodesPluginMetrics(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/pipeline" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"inbound": [],
			"outbound": [
				{
					"name": "tool-prune",
					"direction": "outbound",
					"position": 4,
					"readsBody": true,
					"metrics": [
						{"name": "requests seen", "value": 1284, "unit": "count"},
						{"name": "bytes removed", "value": 9389184, "unit": "bytes"},
						{"name": "tokens saved / request", "value": 1830.5,
						 "unit": "tokens", "note": "estimate, n=1284"}
					]
				},
				{"name": "mcp-parser", "direction": "outbound", "position": 2}
			]
		}`))
	}))
	defer ts.Close()

	c := New(ts.URL)
	view, err := c.GetPipeline(context.Background())
	if err != nil {
		t.Fatalf("GetPipeline: %v", err)
	}
	if len(view.Outbound) != 2 {
		t.Fatalf("got %d outbound plugins, want 2", len(view.Outbound))
	}

	got := view.Outbound[0].Metrics
	if len(got) != 3 {
		t.Fatalf("got %d metrics, want 3: %+v", len(got), got)
	}
	if got[0].Name != "requests seen" || got[0].Value != 1284 || got[0].Unit != "count" {
		t.Errorf("metrics[0] = %+v", got[0])
	}
	if got[2].Value != 1830.5 {
		t.Errorf("metrics[2].Value = %v, want 1830.5 (fractional values must survive)", got[2].Value)
	}
	if got[2].Note != "estimate, n=1284" {
		t.Errorf("metrics[2].Note = %q — the estimate caveat must decode", got[2].Note)
	}
	// A plugin with no metrics key decodes to nil, which the pane renders
	// as "(none)" rather than an empty table.
	if view.Outbound[1].Metrics != nil {
		t.Errorf("mcp-parser Metrics = %+v, want nil", view.Outbound[1].Metrics)
	}
}
