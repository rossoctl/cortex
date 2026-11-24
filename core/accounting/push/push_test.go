package push

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/accounting"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// receiver is an OTLP/HTTP metrics endpoint that keeps each request's header, path and raw
// body. The body is not decoded: that needs google.golang.org/protobuf, which core/accounting may
// not import (boundary_test.go). What the provider exports is read through a ManualReader
// instead (withReader); the raw body is checked for the values that must reach the wire.
type receiver struct {
	mu    sync.Mutex
	auth  []string
	paths []string
	raw   [][]byte
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := io.Reader(r.Body)
	if r.Header.Get("Content-Encoding") == "gzip" {
		if zr, err := gzip.NewReader(r.Body); err == nil {
			body = zr
		}
	}
	raw, _ := io.ReadAll(body)
	rc.mu.Lock()
	rc.auth = append(rc.auth, r.Header.Get("Authorization"))
	rc.paths = append(rc.paths, r.URL.Path)
	rc.raw = append(rc.raw, raw)
	rc.mu.Unlock()
	// An empty ExportMetricsServiceResponse is zero bytes of protobuf.
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}

func newReceiver(t *testing.T) (*receiver, *httptest.Server) {
	rc := &receiver{}
	srv := httptest.NewServer(rc)
	t.Cleanup(srv.Close)
	return rc, srv
}

// withReader adds a ManualReader to the next provider Start builds.
func withReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	rd := sdkmetric.NewManualReader()
	extraReaders = []sdkmetric.Reader{rd}
	t.Cleanup(func() { extraReaders = nil })
	return rd
}

func collect(t *testing.T, rd *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := rd.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	return rm
}

func resourceAttrs(rm metricdata.ResourceMetrics) map[string]string {
	out := map[string]string{}
	for _, kv := range rm.Resource.Attributes() {
		out[string(kv.Key)] = kv.Value.AsString()
	}
	return out
}

// series visits every metric name and the label keys and value of each of its data points.
func series(rm metricdata.ResourceMetrics, fn func(name string, keys []string, value float64)) {
	keysOf := func(set attribute.Set) []string {
		var keys []string
		for _, kv := range set.ToSlice() {
			keys = append(keys, string(kv.Key))
		}
		return keys
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					fn(m.Name, keysOf(p.Attributes), float64(p.Value))
				}
			case metricdata.Sum[float64]:
				for _, p := range d.DataPoints {
					fn(m.Name, keysOf(p.Attributes), p.Value)
				}
			case metricdata.Gauge[int64]:
				for _, p := range d.DataPoints {
					fn(m.Name, keysOf(p.Attributes), float64(p.Value))
				}
			default:
				fn(m.Name+" (not a sum)", nil, 0)
			}
		}
	}
}

// records captures slog records at INFO and above.
type records struct {
	mu   sync.Mutex
	list []slog.Record
}

func (r *records) Enabled(context.Context, slog.Level) bool { return true }
func (r *records) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	r.list = append(r.list, rec)
	r.mu.Unlock()
	return nil
}
func (r *records) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *records) WithGroup(string) slog.Handler      { return r }

func (r *records) count(level slog.Level, contains string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, rec := range r.list {
		if rec.Level == level && strings.Contains(rec.Message, contains) {
			n++
		}
	}
	return n
}

func (r *records) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, rec := range r.list {
		b.WriteString(rec.Message)
		rec.Attrs(func(a slog.Attr) bool { b.WriteString(" " + a.String()); return true })
		b.WriteString("\n")
	}
	return b.String()
}

func captureLogs(t *testing.T) *records {
	t.Helper()
	r := &records{}
	old := slog.Default()
	slog.SetDefault(slog.New(r))
	t.Cleanup(func() { slog.SetDefault(old) })
	return r
}

func rates() pricing.Resolver {
	var r pricing.Rates
	for tier, v := range map[pricing.Tier]float64{
		pricing.TierInput: 3e-6, pricing.TierCacheWrite: 3.75e-6, pricing.TierCacheRead: 3e-7, pricing.TierOutput: 1.5e-5,
	} {
		r.Base[tier], r.Set[tier] = v, true
	}
	tab, _ := pricing.NewTable([]pricing.Entry{{Host: "*", Model: "*", Rates: r, Prov: pricing.ProvConfigured}})
	return pricing.NewRegistry(tab)
}

// everyCounter is a Result that moves every llm.savings.* counter: a saved and a caused op, a
// spend, a cache effect, an interaction, an unpriced op and a failed check.
func everyCounter() accountant.Result {
	return accountant.Result{
		Ops: []accountant.PricedOp{
			{Kind: accountant.KindRewrite, Operator: "context-guru.mask", OpID: "o1",
				Saved: accountant.TierTokens{Input: 1000, CacheRead: 2000}, Caused: accountant.TierTokens{CacheWrite5m: 40}, Estimated: true},
			{Kind: accountant.KindSpend, Operator: "context-guru.summarize", OpID: "o2",
				Call: &accountant.ModelCall{Model: "m2", Usage: accountant.CallUsage{Usage: accountant.Usage{Input: 50}, Output: 5}}},
		},
		CacheEffects: []accountant.CacheEffect{{Kind: accountant.EffectBreak, Caused: accountant.TierTokens{CacheWrite5m: 300}}},
		Interaction:  accountant.TierTokens{Input: 70},
		Unpriced:     []accountant.Unpriced{{OpID: "o9", Reason: "no_size"}},
		Checks:       []accountant.CheckFailure{{Check: "sum"}},
	}
}

func observe(s *accounting.Sink) {
	meta := accountant.RequestMeta{RequestID: "r1", Session: "s1", Model: "claude-sonnet-4-5", At: time.Now(),
		Billing: accountant.Billing{Mode: accountant.BillingAPI}}
	s.Observe(meta, accountant.Usage{Input: 3, CacheRead: 2000}, 0, everyCounter(), "api.anthropic.com", 2003, false)
}

// The series names and label keys Cortex pushes. The central collector's label allow-list is,
// after it maps the resource and sets proxy (context-guru proxyapp/metricspush/central_e2e_test.go:183,
// TestCentralLabelsAreTheSpecSet, at v0.7.4-beta.1):
//
//	__name__, machine, proxy, preset, service_version, model, provider, tier
//
// machine and service_version come from the resource (service.instance.id, service.version),
// proxy from the collector's Cortex receiver; the data-point labels are accountant/metrics' own
// set, and nothing else may be sent.
var (
	allowedNames = map[string]bool{
		"llm.savings.saved.tokens": true, "llm.savings.added.tokens": true,
		"llm.savings.saved.usd": true, "llm.savings.added.usd": true,
		"llm.savings.cache_effect.usd": true, "llm.savings.interaction.usd": true,
		"llm.savings.requests": true, "llm.savings.unpriced": true,
		"llm.savings.check_failures": true, "llm.savings.estimated": true,
		"llm.proxy.export_failures": true, "llm.proxy.info": true,
		// Tier 1 (core/accounting/metrics.go's own instruments, not accountant/metrics'):
		// llm.inference.*, llm.keepalive.*, llm.cache.*.
		"llm.inference.tokens": true, "llm.inference.cost_usd": true, "llm.inference.estimated_cost_usd": true,
		"llm.keepalive.pings": true, "llm.keepalive.cost_usd": true, "llm.keepalive.estimated_cost_usd": true,
		"llm.cache.misses": true, "llm.cache.miss_cost_usd": true,
	}
	allowedKeys = map[string]bool{"kind": true, "tier": true, "component": true, "effect": true, "cause": true,
		"direction": true, "reason": true, "check": true, "result": true, "version": true,
		// Tier 1's own instruments (core/accounting/metrics.go), not accountant/metrics': every
		// llm.inference./llm.keepalive./llm.cache. counter carries the request's model.
		"model": true}
	allowedResource = map[string]bool{"service.instance.id": true, "service.version": true, "service.name": true}
)

// One push carries the bearer token, the resource the collector maps, cumulative counters, and
// only the allowed names and label keys. The machine id is kept in its file, mode 0600, and is the
// same on the next start.
func TestStart_PushesTheSavingsCounters(t *testing.T) {
	rc, srv := newReceiver(t)
	idPath := filepath.Join(t.TempDir(), "machine-id")
	cfg := config.MetricsPushConfig{URL: srv.URL, Token: "t", Interval: time.Hour}

	var ids []string
	var first metricdata.ResourceMetrics
	for i := 0; i < 2; i++ {
		rd := withReader(t)
		mp, shutdown, err := Start(cfg, "v9.9.9", idPath)
		if err != nil || mp == nil {
			t.Fatalf("Start = %v, %v", mp, err)
		}
		observe(accounting.NewSink(mp, rates()))
		rm := collect(t, rd)
		if i == 0 {
			first = rm
		}
		ids = append(ids, resourceAttrs(rm)["service.instance.id"])
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		if err := shutdown(ctx); err != nil { // flushes: the one push of an hour interval
			t.Fatalf("shutdown: %v", err)
		}
		cancel()
	}
	if ids[0] == "" || ids[0] != ids[1] || len(ids[0]) != 32 {
		t.Errorf("service.instance.id = %q then %q, want one 32-hex id kept across starts", ids[0], ids[1])
	}
	if fi, err := os.Stat(idPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("machine-id file: %v, want mode 0600", err)
	}
	if b, _ := os.ReadFile(idPath); strings.TrimSpace(string(b)) != ids[0] {
		t.Errorf("machine-id file holds %q, want %q", b, ids[0])
	}

	// The wire: one POST per start, to /v1/metrics, with the bearer token, carrying the
	// resource and the counters.
	rc.mu.Lock()
	auth, paths, raw := rc.auth, rc.paths, rc.raw
	rc.mu.Unlock()
	if len(raw) != 2 {
		t.Fatalf("receiver got %d requests, want 2", len(raw))
	}
	if auth[0] != "Bearer t" || paths[0] != "/v1/metrics" {
		t.Errorf("Authorization %q at %q, want Bearer t at /v1/metrics", auth[0], paths[0])
	}
	for _, want := range []string{ids[0], "v9.9.9", "service.name", "cortex", "llm.savings.saved.usd", "component", "mask"} {
		if !bytes.Contains(raw[0], []byte(want)) {
			t.Errorf("the pushed body does not carry %q", want)
		}
	}

	res := resourceAttrs(first)
	if res["service.version"] != "v9.9.9" || res["service.name"] != "cortex" {
		t.Errorf("resource = %v", res)
	}
	for k := range res {
		if !allowedResource[k] {
			t.Errorf("resource attribute %q is not one the collector maps", k)
		}
	}
	names := map[string]bool{}
	series(first, func(name string, keys []string, _ float64) {
		names[name] = true
		if !allowedNames[name] {
			t.Errorf("series %q is not in the allowed set", name)
		}
		for _, k := range keys {
			if !allowedKeys[k] {
				t.Errorf("series %s has label %q, outside the allowed keys", name, k)
			}
		}
	})
	for _, want := range []string{"llm.savings.saved.tokens", "llm.savings.added.tokens", "llm.savings.saved.usd",
		"llm.savings.added.usd", "llm.savings.cache_effect.usd", "llm.savings.interaction.usd", "llm.savings.requests",
		"llm.savings.unpriced", "llm.savings.check_failures", "llm.savings.estimated"} {
		if !names[want] {
			t.Errorf("series %s was not pushed; got %v", want, names)
		}
	}
}

// A push carries llm.proxy.info = 1 with the build's version, even before the first request. The
// central dashboards list machines from it, and the MachineSilent alert fires on its absence, so an
// idle Cortex must still send it.
func TestStart_PushesProxyInfoWithNoTraffic(t *testing.T) {
	_, srv := newReceiver(t)
	rd := withReader(t)
	mp, shutdown, err := Start(config.MetricsPushConfig{URL: srv.URL, Interval: time.Hour}, "v9.9.9", "")
	if err != nil || mp == nil {
		t.Fatalf("Start = %v, %v", mp, err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	var got []string
	for _, sm := range collect(t, rd).ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "llm.proxy.info" {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("llm.proxy.info is %T, want an int64 gauge", m.Data)
			}
			for _, p := range g.DataPoints {
				v, _ := p.Attributes.Value("version")
				got = append(got, fmt.Sprintf("%d version=%s labels=%d", p.Value, v.AsString(), p.Attributes.Len()))
			}
		}
	}
	if len(got) != 1 || got[0] != "1 version=v9.9.9 labels=1" {
		t.Errorf("llm.proxy.info points = %v, want [1 version=v9.9.9 labels=1]", got)
	}
}

// The exporter asks for cumulative counters for every instrument kind, whatever the OTel
// environment prefers: the collector reads totals.
func TestStart_TemporalityIsCumulative(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE", "delta")
	_, srv := newReceiver(t)
	cfg := config.MetricsPushConfig{URL: srv.URL, Interval: time.Hour}
	exp, err := newExporter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exp.Shutdown(context.Background()) })
	for _, k := range []sdkmetric.InstrumentKind{sdkmetric.InstrumentKindCounter, sdkmetric.InstrumentKindUpDownCounter,
		sdkmetric.InstrumentKindHistogram, sdkmetric.InstrumentKindObservableCounter} {
		if got := exp.Temporality(k); got != metricdata.CumulativeTemporality {
			t.Errorf("Temporality(%v) = %v, want cumulative", k, got)
		}
	}
}

// With no URL, Start sends nothing and says so in one INFO line; disabled, the same.
func TestStart_NoURLSendsNothing(t *testing.T) {
	logs := captureLogs(t)
	mp, shutdown, err := Start(config.MetricsPushConfig{Interval: time.Second}, "dev", "")
	if err != nil || mp != nil {
		t.Fatalf("Start = %v, %v, want no provider and no error", mp, err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown = %v", err)
	}
	if n := logs.count(slog.LevelInfo, "metrics push off"); n != 1 {
		t.Errorf("%d INFO lines, want 1:\n%s", n, logs.all())
	}
	off := false
	if mp, _, err := Start(config.MetricsPushConfig{Enabled: &off, URL: "https://c.example"}, "dev", ""); mp != nil || err != nil {
		t.Errorf("disabled Start = %v, %v, want nothing", mp, err)
	}
}

// An http URL to a host that is not loopback is refused: the token would travel in the clear.
func TestStart_RefusesPlainHTTP(t *testing.T) {
	mp, _, err := Start(config.MetricsPushConfig{URL: "http://collector.example.com", Token: "s3cret"}, "dev", "")
	if err == nil || mp != nil {
		t.Fatalf("Start = %v, %v, want an error", mp, err)
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("the error quotes the token: %v", err)
	}
}

// A collector that is down costs nothing but the push: Observe returns, a WARN is logged at most
// once a minute, and llm.proxy.export_failures counts the failures. The token is never logged.
func TestStart_ExportFailureWarnsOnceAndCounts(t *testing.T) {
	logs := captureLogs(t)
	_, srv := newReceiver(t)
	srv.Close() // nothing listens at the URL now
	rd := withReader(t)
	mp, shutdown, err := Start(config.MetricsPushConfig{URL: srv.URL, Token: "s3cret", Interval: 20 * time.Millisecond}, "dev", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	sink := accounting.NewSink(mp, rates())
	failures := func() float64 {
		var n float64
		series(collect(t, rd), func(name string, _ []string, v float64) {
			if name == "llm.proxy.export_failures" {
				n += v
			}
		})
		return n
	}
	// timing: the push itself runs on OTel's own periodic reader, a real-time ticker at the
	// Interval set above; there is no seam here to inject a fake clock into the SDK's reader, so
	// waiting for it to actually fire enough times to log >= 3 failures needs the wall clock.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && failures() < 3 {
		// observe must not block, fail or panic while the collector is down: call it inline so a
		// regression that makes it block hangs the test, caught by go test's own -timeout with
		// every goroutine's stack, rather than a guard racing a short timeout here.
		observe(sink)
		// timing: paced to the real periodic reader's ticker, see above.
		time.Sleep(10 * time.Millisecond)
	}
	if n := failures(); n < 3 {
		t.Fatalf("llm.proxy.export_failures = %v, want at least 3", n)
	}
	if n := logs.count(slog.LevelWarn, "metrics push"); n != 1 {
		t.Errorf("%d WARN lines for several failures within a minute, want 1:\n%s", n, logs.all())
	}
	if strings.Contains(logs.all(), "s3cret") {
		t.Errorf("the token reached the log:\n%s", logs.all())
	}
}

// A redirect is a failed export, never followed: a collector cannot move the push, or its bearer
// token, to another destination (review finding).
func TestStart_RedirectIsNotFollowed(t *testing.T) {
	var moved, asked atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		moved.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)
	_ = withReader(t)
	mp, shutdown, err := Start(config.MetricsPushConfig{URL: redirector.URL, Token: "s3cret", Interval: 20 * time.Millisecond}, "dev", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	sink := accounting.NewSink(mp, rates())
	// timing: the push runs on OTel's own periodic reader, a real-time ticker at the Interval
	// set above; there is no seam here to inject a fake clock into the SDK's reader, so waiting
	// for it to actually fire at least twice needs the wall clock.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && asked.Load() < 2 {
		observe(sink)
		// timing: paced to the real periodic reader's ticker, see above.
		time.Sleep(20 * time.Millisecond)
	}
	if asked.Load() < 2 {
		t.Fatalf("the collector was asked %d times, want at least 2", asked.Load())
	}
	if n := moved.Load(); n != 0 {
		t.Errorf("the redirect target got %d requests, want none", n)
	}
}

// Without a path, the id is per process: random, and a fresh one each call.
func TestMachineID_NoPathIsPerProcess(t *testing.T) {
	a, b := MachineID(""), MachineID("")
	if len(a) != 32 || a == b {
		t.Errorf("MachineID(\"\") = %q, %q, want two different 32-hex ids", a, b)
	}
	// A file holding something else is replaced.
	p := filepath.Join(t.TempDir(), "sub", "machine-id")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("not-an-id\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	id := MachineID(p)
	if b, _ := os.ReadFile(p); len(id) != 32 || strings.TrimSpace(string(b)) != id {
		t.Errorf("MachineID replaced the file with %q, returned %q", b, id)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
}

// A credential in the URL's user info or query is refused rather than silently dropped
// (MetricsPushConfig.Validate): the exporter posts to SafeURL, so the collector would only answer
// 401. That holds for METRICS_PUSH_URL too, which reaches only this check: Start returns the
// error, which cmd/cortex logs as a WARN and runs without the push. The error never quotes the
// credential.
func TestStart_RefusesCredentialsInTheURL(t *testing.T) {
	logs := captureLogs(t)
	raw := "http://u:p@127.0.0.1:4318/x?token=t"
	cfg := (&config.MetricsPushConfig{}).Resolve(func(k string) string {
		if k == "METRICS_PUSH_URL" {
			return raw
		}
		return ""
	})
	mp, _, err := Start(cfg, "dev", "")
	if err == nil || mp != nil {
		t.Fatalf("Start = %v, %v, want a refusal", mp, err)
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error = %q, want it to point to metrics_push.token", err)
	}
	out := err.Error() + logs.all()
	for _, secret := range []string{"u:p", "token=t", raw} {
		if strings.Contains(out, secret) {
			t.Errorf("the error or the log quotes %q:\n%s", secret, out)
		}
	}
}
