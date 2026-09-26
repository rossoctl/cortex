package sessionapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// meteredPlugin implements pipeline.MetricsProvider on top of fakePlugin's shape.
type meteredPlugin struct {
	name    string
	metrics []pipeline.Metric
}

func (m *meteredPlugin) Name() string { return m.name }
func (m *meteredPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (m *meteredPlugin) OnRequest(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (m *meteredPlugin) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (m *meteredPlugin) Metrics() []pipeline.Metric { return m.metrics }

func pipelineJSON(t *testing.T, outbound []pipeline.Plugin) (string, []pipelinePluginView) {
	t.Helper()
	pipe, err := pipeline.New(outbound)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	srv := New(":0", store, WithPipelines(nil, pipeline.NewHolder(pipe)))
	ts := httptest.NewServer(srv.server.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/pipeline")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	var body struct {
		Outbound []pipelinePluginView `json:"outbound"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("Unmarshal: %v — raw=%s", err, raw)
	}
	return string(raw), body.Outbound
}

// TestPipelineView_OmitsMetricsForNonProviders: a plugin that does not
// implement MetricsProvider must not emit a metrics key at all. abctl relies
// on absence-vs-empty to render "(none)" rather than an empty table, and an
// always-present null would also churn every existing golden payload.
func TestPipelineView_OmitsMetricsForNonProviders(t *testing.T) {
	raw, views := pipelineJSON(t, []pipeline.Plugin{&fakePlugin{name: "token-exchange"}})
	if len(views) != 1 {
		t.Fatalf("got %d plugins, want 1", len(views))
	}
	if views[0].Metrics != nil {
		t.Errorf("Metrics = %v, want nil for a non-provider", views[0].Metrics)
	}
	if strings.Contains(raw, "metrics") {
		t.Errorf("payload should not mention metrics at all:\n%s", raw)
	}
}

// TestPipelineView_CarriesProviderMetrics: values, units and notes survive the
// round trip, including the Note that labels an estimate as one.
func TestPipelineView_CarriesProviderMetrics(t *testing.T) {
	want := []pipeline.Metric{
		{Name: "requests seen", Value: 1284, Unit: "count"},
		{Name: "bytes removed", Value: 9389184, Unit: "bytes"},
		{Name: "tokens saved / request", Value: 1830.5, Unit: "tokens", Note: "estimate, n=1284"},
	}
	_, views := pipelineJSON(t, []pipeline.Plugin{&meteredPlugin{name: "tool-prune", metrics: want}})
	if len(views) != 1 {
		t.Fatalf("got %d plugins, want 1", len(views))
	}
	got := views[0].Metrics
	if len(got) != len(want) {
		t.Fatalf("got %d metrics, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("metric %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestPipelineView_ProviderReturningNilOmitsKey: a provider that has nothing
// to report yet behaves like a non-provider on the wire, so a freshly started
// plugin doesn't render an empty table.
func TestPipelineView_ProviderReturningNilOmitsKey(t *testing.T) {
	_, views := pipelineJSON(t, []pipeline.Plugin{&meteredPlugin{name: "tool-prune", metrics: nil}})
	if len(views) != 1 {
		t.Fatalf("got %d plugins, want 1", len(views))
	}
	if views[0].Metrics != nil {
		t.Errorf("Metrics = %v, want nil", views[0].Metrics)
	}
}

// TestPipelineView_CarriesMetricsThroughConfiguredWrapper is the regression test
// for a bug the tests above could not see. A plugin that has config is wrapped
// by pipeline.WrapConfigured, and Go does not promote optional interfaces
// through the wrapper's embedded Plugin — so MetricsProvider has to be
// forwarded explicitly, exactly as Initializer/Shutdowner/Finisher/Readier are.
//
// Every plugin an operator actually configures takes this path, so before the
// forwarding existed, metrics were invisible in every real deployment while the
// unconfigured case above passed happily. End-to-end verification caught it;
// this test keeps it caught.
func TestPipelineView_CarriesMetricsThroughConfiguredWrapper(t *testing.T) {
	want := []pipeline.Metric{
		{Name: "requests pruned", Value: 3, Unit: "count"},
		{Name: "bytes removed", Value: 825, Unit: "bytes"},
	}
	inner := &meteredPlugin{name: "tool-prune", metrics: want}
	wrapped := pipeline.WrapConfigured(inner, json.RawMessage(`{"remove":["NotebookEdit"]}`))

	_, views := pipelineJSON(t, []pipeline.Plugin{wrapped})
	if len(views) != 1 {
		t.Fatalf("got %d plugins, want 1", len(views))
	}
	got := views[0].Metrics
	if len(got) != len(want) {
		t.Fatalf("got %d metrics through the wrapper, want %d — MetricsProvider is not being forwarded", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("metric %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// The wrapper must still surface config, so the two channels coexist.
	if len(views[0].Config) == 0 {
		t.Error("wrapped plugin lost its config")
	}
}

// TestPipelineView_WrappedNonProviderStillOmitsMetrics: forwarding makes every
// wrapped plugin satisfy MetricsProvider, so confirm that does not turn into an
// empty metrics table for plugins that report nothing.
func TestPipelineView_WrappedNonProviderStillOmitsMetrics(t *testing.T) {
	wrapped := pipeline.WrapConfigured(&fakePlugin{name: "token-exchange"}, json.RawMessage(`{"a":1}`))
	raw, views := pipelineJSON(t, []pipeline.Plugin{wrapped})
	if len(views) != 1 {
		t.Fatalf("got %d plugins, want 1", len(views))
	}
	if views[0].Metrics != nil {
		t.Errorf("Metrics = %v, want nil for a wrapped non-provider", views[0].Metrics)
	}
	if strings.Contains(raw, "metrics") {
		t.Errorf("payload should omit metrics entirely:\n%s", raw)
	}
}
