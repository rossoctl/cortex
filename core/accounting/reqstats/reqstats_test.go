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

// A policy denial (rate-limit, guardrail, egress-allow) is recorded as model.SessionDenied, with
// a real status and duration but never as model.SessionResponse. It must still be counted, not
// silently dropped. (review finding 5, Important)
func TestRecord_DeniedPhaseIsCounted(t *testing.T) {
	mp, rd := manual(t)
	r := New(mp)
	r.Record("s1", &model.SessionEvent{Phase: model.SessionDenied, StatusCode: 429, Duration: 5 * time.Millisecond})

	if got := sum(t, rd, "llm.inference.requests"); got != 1 {
		t.Errorf("a denied request must be counted: llm.inference.requests = %v, want 1", got)
	}
	if got := sum(t, rd, "llm.inference.duration_seconds"); got != 1 {
		t.Errorf("llm.inference.duration_seconds count = %v, want 1", got)
	}
}

// requests/duration/ttfb/bytes carry a model label when the row is an inference request, so the
// dashboards' per-model panels have something to group by. (review finding 8, Important)
func TestRecord_InferenceRowCarriesModelLabel(t *testing.T) {
	mp, rd := manual(t)
	r := New(mp)
	r.Record("s1", &model.SessionEvent{Phase: model.SessionResponse, StatusCode: 200, Duration: time.Second,
		Inference: &model.InferenceExtension{Model: "claude-sonnet-4-5"}})

	var rm metricdata.ResourceMetrics
	_ = rd.Collect(context.Background(), &rm)
	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "llm.inference.requests" {
				continue
			}
			if d, ok := m.Data.(metricdata.Sum[int64]); ok {
				for _, p := range d.DataPoints {
					if v, ok := p.Attributes.Value("model"); ok && v.AsString() == "claude-sonnet-4-5" {
						found = true
					}
				}
			}
		}
	}
	if !found {
		t.Error("llm.inference.requests has no data point with model=claude-sonnet-4-5")
	}
}

// requestsByStream sums llm.inference.requests by its "stream" label value.
func requestsByStream(t *testing.T, rd *sdkmetric.ManualReader) map[bool]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	_ = rd.Collect(context.Background(), &rm)
	got := map[bool]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "llm.inference.requests" {
				continue
			}
			for _, p := range m.Data.(metricdata.Sum[int64]).DataPoints {
				v, ok := p.Attributes.Value("stream")
				if !ok {
					t.Fatalf("llm.inference.requests data point has no stream label: %v", p.Attributes.ToSlice())
				}
				got[v.AsBool()] += p.Value
			}
		}
	}
	return got
}

// The "stream" label says whether the ANSWER streamed (StreamedResponse), not what the request
// asked for (Stream): a gateway can answer a streaming request with a buffered body. A
// non-inference row streamed nothing and is labelled false, not dropped.
func TestRecord_StreamLabelIsTheResponseShape(t *testing.T) {
	mp, rd := manual(t)
	r := New(mp)
	r.Record("s1", &model.SessionEvent{Phase: model.SessionResponse, StatusCode: 200,
		Inference: &model.InferenceExtension{Model: "m", Stream: true, StreamedResponse: true}})
	r.Record("s1", &model.SessionEvent{Phase: model.SessionResponse, StatusCode: 200,
		Inference: &model.InferenceExtension{Model: "m", Stream: true}}) // asked, answered buffered
	r.Record("s1", &model.SessionEvent{Phase: model.SessionResponse, StatusCode: 200})

	got := requestsByStream(t, rd)
	if got[true] != 1 || got[false] != 2 {
		t.Errorf("requests by stream = %v, want map[false:2 true:1]", got)
	}
}

// A row with no Inference extension (a non-inference proxied request) still counts, under an
// empty model label rather than being dropped or panicking.
func TestRecord_NonInferenceRowHasEmptyModelLabel(t *testing.T) {
	mp, rd := manual(t)
	r := New(mp)
	r.Record("s1", &model.SessionEvent{Phase: model.SessionResponse, StatusCode: 200, Duration: time.Second})

	if got := sum(t, rd, "llm.inference.requests"); got != 1 {
		t.Errorf("llm.inference.requests = %v, want 1", got)
	}
}

// The duration and ttfb histograms use explicit boundaries sized for seconds, not the OTel SDK's
// millisecond-sized defaults (0, 5, 10, 25, ... up to 10000) — with those defaults almost every
// request's duration falls in the first bucket and p50/p95/p99 are meaningless.
// (review finding 6, Important)
func TestNew_HistogramsUseSecondSizedBuckets(t *testing.T) {
	mp, rd := manual(t)
	r := New(mp)
	r.Record("s1", &model.SessionEvent{Phase: model.SessionResponse, StatusCode: 200, Duration: 2 * time.Second, Ttfb: time.Second})

	var rm metricdata.ResourceMetrics
	_ = rd.Collect(context.Background(), &rm)
	defaultBounds := []float64{0, 5, 10, 25, 50, 75, 100, 250, 500, 750, 1000, 2500, 5000, 7500, 10000}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "llm.inference.duration_seconds" && m.Name != "llm.inference.ttfb_seconds" {
				continue
			}
			d, ok := m.Data.(metricdata.Histogram[float64])
			if !ok || len(d.DataPoints) == 0 {
				t.Fatalf("%s: no histogram data points", m.Name)
			}
			bounds := d.DataPoints[0].Bounds
			if len(bounds) == len(defaultBounds) {
				same := true
				for i, b := range bounds {
					if b != defaultBounds[i] {
						same = false
						break
					}
				}
				if same {
					t.Errorf("%s: uses the SDK's millisecond-sized default bucket boundaries %v", m.Name, bounds)
				}
			}
		}
	}
}

func TestStatusClass(t *testing.T) {
	cases := map[int]string{200: "2xx", 201: "2xx", 404: "4xx", 500: "5xx", 0: "unknown", 999: "unknown"}
	for code, want := range cases {
		if got := statusClass(code); got != want {
			t.Errorf("statusClass(%d) = %q, want %q", code, got, want)
		}
	}
}

// llm.inference.added_seconds records a response row's Added: not a denial (it never reached
// the upstream, so none of its time is "on top of" an upstream's) and not a zero (the send was
// never stamped). Its buckets start at a millisecond: the proxy's own work sits far below the
// 50ms floor of the duration buckets, where every observation would land in the first one.
func TestRecord_AddedSecondsIsResponseOnly(t *testing.T) {
	mp, rd := manual(t)
	r := New(mp)
	r.Record("s1", &model.SessionEvent{Phase: model.SessionResponse, StatusCode: 200, Added: 3 * time.Millisecond,
		Inference: &model.InferenceExtension{Model: "m"}})
	r.Record("s1", &model.SessionEvent{Phase: model.SessionDenied, StatusCode: 429, Added: time.Millisecond})
	r.Record("s1", &model.SessionEvent{Phase: model.SessionResponse, StatusCode: 502})

	if got := sum(t, rd, "llm.inference.added_seconds"); got != 1 {
		t.Errorf("llm.inference.added_seconds count = %v, want 1 (the response row only)", got)
	}
	var rm metricdata.ResourceMetrics
	_ = rd.Collect(context.Background(), &rm)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "llm.inference.added_seconds" {
				continue
			}
			dp := m.Data.(metricdata.Histogram[float64]).DataPoints[0]
			if dp.Bounds[0] != 0.001 {
				t.Errorf("added_seconds first bound = %v, want 0.001", dp.Bounds[0])
			}
			if m.Unit != "s" {
				t.Errorf("added_seconds unit = %q, want \"s\" (the dashboard reads llm_inference_added_seconds_bucket)", m.Unit)
			}
		}
	}
}
