// Package push sends Cortex's accountant counters (llm.savings.*) to the central collector over
// OTLP/HTTP (design §5.10). It owns what accountant/metrics leaves to the host: the OTel SDK, the
// exporter, the URL and the token.
//
// It matches what the central collector reads from a proxy: resource attributes
// service.instance.id (a random machine id), service.version and service.name "cortex";
// cumulative temporality; the token as Authorization: Bearer. The collector's Cortex receiver
// sets proxy="cortex", so Cortex does not send it.
//
// A failed export never reaches a request: the counters are read on the periodic reader's own
// goroutine, the failure increments llm.proxy.export_failures, and a WARN is logged at most once
// a minute.
package push

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rossoctl/cortex/core/config"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
)

// ServiceName is the resource's service.name.
const ServiceName = "cortex"

// exportTimeout bounds one export. A collector that does not answer costs one push, never more:
// retries are off, because the counters are cumulative and the next push carries the same totals.
const exportTimeout = 10 * time.Second

// warnEvery is the most often a failed export is logged.
const warnEvery = time.Minute

// extraReaders are added to every provider Start builds. Tests only: they read the provider's
// data with a ManualReader, because decoding the OTLP wire format needs a module core/accounting
// may not import.
var extraReaders []sdkmetric.Reader

// Start builds the MeterProvider that pushes to cfg.URL + "/v1/metrics" every cfg.Interval, and
// returns it with the function that flushes and stops it.
//
// cfg is the resolved block (config.MetricsPushConfig.Resolve). With no URL, or disabled, it logs
// one INFO line and returns a nil provider: nothing is sent. A URL that is not https (http is
// allowed to a loopback address only) is an error, and so is an exporter that cannot be built;
// the caller runs without a push. The token is never logged.
//
// machineIDPath is where the machine id is kept (MachineID); empty gives a per-process id.
func Start(cfg config.MetricsPushConfig, version, machineIDPath string) (metric.MeterProvider, func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }
	if !cfg.IsEnabled() {
		slog.Info("metrics push off — metrics_push.enabled is false; no metrics leave this process")
		return nil, noop, nil
	}
	if cfg.URL == "" {
		slog.Info("metrics push off — no metrics_push.url (METRICS_PUSH_URL) is set; no metrics leave this process")
		return nil, noop, nil
	}
	// The env-resolved block, checked again: config.Validate saw only the YAML. A refusal here
	// is the caller's WARN, and the proxy runs without the push (see MetricsPushConfig.Validate).
	if err := cfg.Validate(); err != nil {
		return nil, noop, err
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = config.DefaultMetricsPushInterval
	}
	inner, err := newExporter(cfg)
	if err != nil {
		return nil, noop, err
	}
	exp := &exporter{Exporter: inner, url: config.SafeURL(cfg.URL)}
	res := resource.NewSchemaless(
		attribute.String("service.instance.id", MachineID(machineIDPath)),
		attribute.String("service.version", version),
		attribute.String("service.name", ServiceName),
	)
	readerTimeout := exportTimeout
	if interval < readerTimeout {
		readerTimeout = interval
	}
	mpOpts := []sdkmetric.Option{
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp,
			sdkmetric.WithInterval(interval), sdkmetric.WithTimeout(readerTimeout))),
	}
	for _, r := range extraReaders {
		mpOpts = append(mpOpts, sdkmetric.WithReader(r))
	}
	mp := sdkmetric.NewMeterProvider(mpOpts...)
	meter := mp.Meter("github.com/rossoctl/cortex/core/accounting/push")
	failures, err := meter.Int64Counter(
		"llm.proxy.export_failures", metric.WithDescription("Metric pushes the collector did not accept."))
	if err == nil {
		err = registerProxyInfo(meter, version)
	}
	if err != nil {
		_ = mp.Shutdown(context.Background())
		return nil, noop, fmt.Errorf("metrics push: %w", err)
	}
	// Stored after the reader has started: an export can run first, so the field is atomic.
	// One that fails before the store is logged but not counted.
	exp.failures.Store(&failures)
	slog.Info("metrics push on", "url", config.SafeURL(cfg.URL), "interval", interval, "token", cfg.Token != "")
	return mp, mp.Shutdown, nil
}

// registerProxyInfo adds llm.proxy.info: a gauge that is always 1, with the build's version as its
// one label. context-guru sends the same gauge (proxyapp/metricspush.go registerProxyInfo), with a
// preset label that Cortex does not have. The central dashboards list proxies and machines from it,
// and the MachineSilent alert fires when it stops, so it is observed on every push, also before
// the first request.
func registerProxyInfo(m metric.Meter, version string) error {
	g, err := m.Int64ObservableGauge("llm.proxy.info", metric.WithDescription("Always 1. Shows the build."))
	if err != nil {
		return err
	}
	attrs := metric.WithAttributes(attribute.String("version", version))
	_, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(g, 1, attrs)
		return nil
	}, g)
	return err
}

// newExporter is the OTLP/HTTP exporter for cfg: cumulative, no retries, bounded by
// exportTimeout, with the bearer token when there is one.
func newExporter(cfg config.MetricsPushConfig) (sdkmetric.Exporter, error) {
	opts := []otlpmetrichttp.Option{
		// The safe form: the exporter's own errors quote its endpoint, and a credential in the
		// URL's user info or query must not reach them. The token travels in the header.
		otlpmetrichttp.WithEndpointURL(endpointURL(config.SafeURL(cfg.URL))),
		otlpmetrichttp.WithTimeout(exportTimeout),
		otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{Enabled: false}),
		// Cumulative, whatever OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE says: the
		// collector reads totals.
		otlpmetrichttp.WithTemporalitySelector(sdkmetric.DefaultTemporalitySelector),
		// Treat any redirect as a failed export rather than following it (review finding): the
		// default OTel HTTP client has no CheckRedirect, so a compromised or misbehaving
		// collector could move the push to an arbitrary destination, and Go's net/http keeps the
		// Authorization header on a same-host redirect — including an https-to-http downgrade —
		// sending the token in cleartext. Validate() only checks the CONFIGURED URL; this covers
		// the EFFECTIVE one.
		otlpmetrichttp.WithHTTPClient(&http.Client{
			Timeout: exportTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}),
	}
	if cfg.Token != "" {
		opts = append(opts, otlpmetrichttp.WithHeaders(map[string]string{"Authorization": "Bearer " + cfg.Token}))
	}
	exp, err := otlpmetrichttp.New(context.Background(), opts...)
	if err != nil {
		// The exporter's error can quote its options; say only what failed.
		return nil, errors.New("metrics push: the OTLP exporter could not be built")
	}
	return exp, nil
}

// endpointURL is the OTLP metrics path under base: base + "/v1/metrics", unless base already ends
// with it.
func endpointURL(base string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/v1/metrics") {
		return base
	}
	return base + "/v1/metrics"
}

// exporter is the OTLP exporter with Cortex's failure handling: a failed export is counted and
// logged here, at most once a minute, and not returned, so the SDK does not report it again
// through the global OTel error handler on every interval.
type exporter struct {
	sdkmetric.Exporter
	url      string
	failures atomic.Pointer[metric.Int64Counter]

	mu       sync.Mutex
	lastWarn time.Time
}

func (e *exporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	err := e.Exporter.Export(ctx, rm)
	if err == nil {
		return nil
	}
	if c := e.failures.Load(); c != nil {
		(*c).Add(context.Background(), 1)
	}
	e.mu.Lock()
	warn := e.lastWarn.IsZero() || time.Since(e.lastWarn) >= warnEvery
	if warn {
		e.lastWarn = time.Now()
	}
	e.mu.Unlock()
	if warn {
		// The error names the URL and the status; it carries no header, so no token.
		slog.Warn("metrics push failed; the counters are kept and sent with the next push (logged at most once a minute)",
			"url", e.url, "error", err)
	}
	return nil
}

// MachineID returns the id kept at path: 16 random bytes in hex. It creates the file, mode 0600,
// when it is missing or does not hold an id, and keeps the id across restarts. With an empty path
// — a cluster pod, which has no home to keep it in — it returns a new id each call, so the id is
// per process. A file that cannot be written gives an id for this process, with a WARN.
func MachineID(path string) string {
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			if id := strings.TrimSpace(string(b)); isID(id) {
				_ = os.Chmod(path, 0o600)
				return id
			}
		}
	}
	id := newID()
	if path == "" {
		return id
	}
	if err := writeID(path, id); err != nil {
		slog.Warn("metrics push: the machine id could not be kept; this process uses its own", "path", path, "error", err)
	}
	return id
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func isID(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// writeID writes id to path through a synced temporary file, so a crash never leaves half an id.
func writeID(path, id string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".machine-id-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.WriteString(id + "\n"); err != nil {
		f.Close()
		return err
	}
	// Synced before the rename, or a power loss can leave the renamed file empty (#1547).
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
