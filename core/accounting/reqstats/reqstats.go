// Package reqstats pushes the llm.inference.* transport metrics (requests by status class,
// duration, time-to-first-byte, request/response bytes) that core/accounting/metrics.go's
// per-request accountant data cannot provide, because that call site never sees an HTTP status,
// a wall-clock duration, or a byte count.
//
// Those facts already exist on every model.SessionEvent, computed once by
// core/pipeline/pipeline.go regardless of which listener (forward-proxy, extproc, reverse-proxy)
// handled the request. New is a session.Recorder built on that existing fact, registered the same
// way core/cost/usage.Aggregator and the cost ledger already are
// (cmd/cortex/main.go, sessions.AddRecorder), so it needs no listener change.
package reqstats

import (
	"context"
	"log/slog"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// MeterName is the instrumentation scope these instruments are created under.
const MeterName = "github.com/rossoctl/cortex/core/accounting/reqstats"

// recorder is the session.Recorder New returns. A zero-value recorder (every instrument nil) is
// the no-op case: Record's early checks never reach a nil instrument, mirroring how
// accounting.Sink's own nil case is reached before any instrument is used.
type recorder struct {
	requests      metric.Int64Counter
	duration      metric.Float64Histogram
	ttfb          metric.Float64Histogram
	responseBytes metric.Int64Counter
	requestBytes  metric.Int64Counter
}

// New builds the recorder on mp. It returns a no-op recorder — not nil, since session.Recorder is
// an interface Store.AddRecorder calls unconditionally — when mp is nil (no push configured) or
// the meter refuses an instrument, exactly as accounting.NewSink does for its own nil cases.
func New(mp metric.MeterProvider) session.Recorder {
	if mp == nil {
		return &recorder{}
	}
	meter := mp.Meter(MeterName)
	r := &recorder{}
	instruments := []struct {
		name string
		new  func() error
	}{
		{"llm.inference.requests", func() (err error) {
			r.requests, err = meter.Int64Counter("llm.inference.requests",
				metric.WithDescription("HTTP requests the proxy completed, by status class."))
			return err
		}},
		{"llm.inference.duration_seconds", func() (err error) {
			r.duration, err = meter.Float64Histogram("llm.inference.duration_seconds",
				metric.WithDescription("Wall-clock time from request entry to response recording."), metric.WithUnit("s"))
			return err
		}},
		{"llm.inference.ttfb_seconds", func() (err error) {
			r.ttfb, err = meter.Float64Histogram("llm.inference.ttfb_seconds",
				metric.WithDescription("Wall-clock time from request entry to the upstream's response headers."), metric.WithUnit("s"))
			return err
		}},
		{"llm.inference.response_bytes", func() (err error) {
			r.responseBytes, err = meter.Int64Counter("llm.inference.response_bytes",
				metric.WithDescription("Response body bytes observed on the wire."))
			return err
		}},
		{"llm.inference.request_bytes", func() (err error) {
			r.requestBytes, err = meter.Int64Counter("llm.inference.request_bytes",
				metric.WithDescription("Request body bytes forwarded, as rewritten by any plugin."))
			return err
		}},
	}
	for _, inst := range instruments {
		if err := inst.new(); err != nil {
			slog.Warn("reqstats: an inference transport metric could not be created; no reqstats metrics are pushed",
				"instrument", inst.name, "error", err)
			return &recorder{}
		}
	}
	return r
}

// Record implements session.Recorder. A tunnel row (e.Tunnel) is skipped: its bytes are opaque
// and it carries no HTTP status or meaningful duration for these dashboards. A nil recorder
// field (the no-op case from New) makes every branch below a no-op, since each checks its own
// instrument before using it.
func (r *recorder) Record(_ string, e *model.SessionEvent) {
	if e == nil || e.Tunnel {
		return
	}
	ctx := context.Background()
	switch e.Phase {
	case model.SessionRequest:
		if r.requestBytes != nil {
			r.requestBytes.Add(ctx, e.BytesUp)
		}
	case model.SessionResponse:
		if r.requests != nil {
			r.requests.Add(ctx, 1, metric.WithAttributes(attribute.String("status_class", statusClass(e.StatusCode))))
		}
		if r.duration != nil {
			r.duration.Record(ctx, e.Duration.Seconds())
		}
		if r.ttfb != nil && e.Ttfb > 0 {
			r.ttfb.Record(ctx, e.Ttfb.Seconds())
		}
		if r.responseBytes != nil {
			r.responseBytes.Add(ctx, e.BytesDown)
		}
	}
}

// statusClass buckets an HTTP status into "2xx"/"4xx"/"5xx", or "unknown" for 0 (not yet known —
// a denial recorded before a status, a connection reset) or anything outside 100-599.
func statusClass(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 400 && code < 500:
		return "4xx"
	case code >= 500 && code < 600:
		return "5xx"
	default:
		return "unknown"
	}
}
