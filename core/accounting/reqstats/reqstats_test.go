package reqstats

import (
	"context"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func manual(t *testing.T) (*sdkmetric.MeterProvider, *sdkmetric.ManualReader) {
	t.Helper()
	rd := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rd))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	return mp, rd
}

func sum(t *testing.T, rd *sdkmetric.ManualReader, name string) float64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	_ = rd.Collect(context.Background(), &rm)
	var total float64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					total += float64(p.Value)
				}
			case metricdata.Sum[float64]:
				for _, p := range d.DataPoints {
					total += p.Value
				}
			case metricdata.Histogram[float64]:
				for _, p := range d.DataPoints {
					total += float64(p.Count)
				}
			}
		}
	}
	return total
}

func TestRecord_ResponsePhaseCountsRequestAndDuration(t *testing.T) {
	mp, rd := manual(t)
	r := New(mp)
	r.Record("s1", &model.SessionEvent{Phase: model.SessionResponse, StatusCode: 200, Duration: 2 * time.Second, Ttfb: 500 * time.Millisecond, BytesDown: 1024})

	if got := sum(t, rd, "llm.inference.requests"); got != 1 {
		t.Errorf("llm.inference.requests = %v, want 1", got)
	}
	if got := sum(t, rd, "llm.inference.duration_seconds"); got != 1 {
		t.Errorf("llm.inference.duration_seconds count = %v, want 1", got)
	}
	if got := sum(t, rd, "llm.inference.response_bytes"); got != 1024 {
		t.Errorf("llm.inference.response_bytes = %v, want 1024", got)
	}
}

func TestRecord_RequestPhaseCountsOnlyBytesUp(t *testing.T) {
	mp, rd := manual(t)
	r := New(mp)
	r.Record("s1", &model.SessionEvent{Phase: model.SessionRequest, BytesUp: 512})

	if got := sum(t, rd, "llm.inference.request_bytes"); got != 512 {
		t.Errorf("llm.inference.request_bytes = %v, want 512", got)
	}
	if got := sum(t, rd, "llm.inference.requests"); got != 0 {
		t.Errorf("llm.inference.requests = %v, want 0 on a request-phase row", got)
	}
}

func TestRecord_TunnelRowIsSkipped(t *testing.T) {
	mp, rd := manual(t)
	r := New(mp)
	r.Record("s1", &model.SessionEvent{Phase: model.SessionResponse, Tunnel: true, StatusCode: 200, BytesDown: 999})

	if got := sum(t, rd, "llm.inference.requests"); got != 0 {
		t.Errorf("tunnel row counted: llm.inference.requests = %v, want 0", got)
	}
	if got := sum(t, rd, "llm.inference.response_bytes"); got != 0 {
		t.Errorf("tunnel row counted: llm.inference.response_bytes = %v, want 0", got)
	}
}

func TestRecord_ZeroStatusCodeIsUnknownNotDropped(t *testing.T) {
	mp, rd := manual(t)
	r := New(mp)
	r.Record("s1", &model.SessionEvent{Phase: model.SessionResponse, StatusCode: 0, Duration: time.Second})

	if got := sum(t, rd, "llm.inference.requests"); got != 1 {
		t.Errorf("a StatusCode-0 row must still be counted: llm.inference.requests = %v, want 1", got)
	}
}

func TestRecord_ZeroTtfbIsNotRecorded(t *testing.T) {
	mp, rd := manual(t)
	r := New(mp)
	r.Record("s1", &model.SessionEvent{Phase: model.SessionResponse, StatusCode: 200, Duration: time.Second, Ttfb: 0})

	if got := sum(t, rd, "llm.inference.ttfb_seconds"); got != 0 {
		t.Errorf("llm.inference.ttfb_seconds count = %v, want 0 (Ttfb==0 means unmeasured)", got)
	}
}

func TestNew_NilProviderIsSafe(t *testing.T) {
	r := New(nil)
	r.Record("s1", &model.SessionEvent{Phase: model.SessionResponse, StatusCode: 200}) // must not panic
}

func TestStatusClass(t *testing.T) {
	cases := map[int]string{200: "2xx", 201: "2xx", 404: "4xx", 500: "5xx", 0: "unknown", 999: "unknown"}
	for code, want := range cases {
		if got := statusClass(code); got != want {
			t.Errorf("statusClass(%d) = %q, want %q", code, got, want)
		}
	}
}
