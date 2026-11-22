package contextguru

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	cgcomponents "github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/cortex/core/accounting"
	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// costRecorder is a session.Recorder that keeps the events it is given.
type costRecorder struct {
	mu  sync.Mutex
	evs []*model.SessionEvent
}

func (r *costRecorder) Record(_ string, e *model.SessionEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evs = append(r.evs, e)
}

func (r *costRecorder) events() []*model.SessionEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*model.SessionEvent(nil), r.evs...)
}

func acctHost(t *testing.T, recs ...session.Recorder) *accounting.Host {
	t.Helper()
	h := accounting.New(accounting.Options{CostRecorders: recs})
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// The field table of wiring W4.6: what a KeepAliveReport becomes as an accountant.Ping.
func TestPingFromReport(t *testing.T) {
	window := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	ts := window.Add(4*time.Minute + 2*time.Second)
	r := cgcomponents.KeepAliveReport{
		Session: "s1", Thread: "", Model: "claude-sonnet-4-5", Route: "/v1/messages",
		FreshInput: 3, CacheRead: 48_000, CacheWrite: 700, CacheWrite1h: 200, Output: 1,
		CostUSD: 9.99, Status: 200, DurationMs: 2000, TS: ts.UnixMilli(), Window: window,
	}
	p := pingFromReport(r, "api.anthropic.com", "")
	want := accountant.Ping{
		Session: "s1", ThreadID: "", Model: "claude-sonnet-4-5", Host: "api.anthropic.com",
		Usage: accountant.CallUsage{Usage: accountant.Usage{Input: 3, CacheRead: 48_000, CacheWrite: 700,
			CacheWrite1h: 200}, Output: 1},
		At: window.Add(4 * time.Minute), Window: window,
	}
	if !p.At.Equal(want.At) {
		t.Errorf("At = %v, want the ping's start %v (TS minus its duration)", p.At, want.At)
	}
	p.At, want.At = time.Time{}, time.Time{}
	if p != want {
		t.Errorf("ping = %+v\nwant  %+v", p, want)
	}
	if p.Usage.CostUSD != 0 {
		t.Errorf("CostUSD = %v: the report's own figure must not be carried; Cortex prices with its rates", p.Usage.CostUSD)
	}
}

func TestPingEmitter_AccountsThroughTheHost(t *testing.T) {
	rec := &costRecorder{}
	p := New()
	p.SetAccounting(acctHost(t, rec))
	pctx := finishedPctx(200, pipeline.AgentRoleMain)
	pctx.Host = "api.anthropic.com"
	em, ok := p.pingEmitter(pctx, "s1").(cgcomponents.KeepAliveEmitter)
	if !ok {
		t.Fatal("pingEmitter is not a KeepAliveEmitter")
	}
	window := time.Unix(1_700_000_000, 0)
	em.KeepAlivePing(cgcomponents.KeepAliveReport{Session: "s1", Model: "claude-sonnet-5",
		CacheRead: 48_000, Output: 1, Status: 200, TS: window.Add(5 * time.Minute).UnixMilli(), Window: window})
	evs := rec.events()
	if len(evs) != 1 {
		t.Fatalf("recorder got %d events, want 1", len(evs))
	}
	if evs[0].Host != "api.anthropic.com" || evs[0].SessionID != "s1" {
		t.Errorf("event host/session = %q/%q", evs[0].Host, evs[0].SessionID)
	}
	if r, ok := event.Record(evs[0]); !ok || r.Purpose != accounting.PingPurpose {
		t.Errorf("cost record = %+v (present %v), want Purpose %q", r, ok, accounting.PingPurpose)
	}

	// A refused ping (no window) records nothing.
	em.KeepAlivePing(cgcomponents.KeepAliveReport{Session: "s1", CacheRead: 1, Status: 200})
	if n := len(rec.events()); n != 1 {
		t.Errorf("a ping with no window was recorded (%d events)", n)
	}
}

// A request with no session id is tracked under the caller's identity (sessionID's fallback),
// but Host.RequestEnd keys thread billing and ping facts on pctx.SessionID only. Accounting the
// ping under the identity would give it BillingUnknown and facts no request ever reads, so it
// is only logged; with a session id it is accounted under that id, the key RequestEnd uses.
func TestPingEmitter_AccountsOnlyUnderTheRequestsSessionID(t *testing.T) {
	rec := &costRecorder{}
	p := New()
	p.SetAccounting(acctHost(t, rec))
	report := cgcomponents.KeepAliveReport{Session: "subject-x", Model: "claude-sonnet-5",
		CacheRead: 48_000, Output: 1, Status: 200, TS: time.Now().UnixMilli(), Window: time.Now().Add(-5 * time.Minute)}

	anon := finishedPctx(200, pipeline.AgentRoleMain)
	anon.Session, anon.SessionID = nil, ""
	p.pingEmitter(anon, "subject-x").(cgcomponents.KeepAliveEmitter).KeepAlivePing(report)
	if n := len(rec.events()); n != 0 {
		t.Fatalf("a ping for a request with no session id was accounted (%d events)", n)
	}

	named := finishedPctx(200, pipeline.AgentRoleMain) // SessionID "s1"
	report.Session = "s1"
	p.pingEmitter(named, sessionID(named)).(cgcomponents.KeepAliveEmitter).KeepAlivePing(report)
	evs := rec.events()
	if len(evs) != 1 || evs[0].SessionID != named.SessionID {
		t.Fatalf("events = %d (session %q), want 1 under %q", len(evs), sessionOf(evs), named.SessionID)
	}
}

func sessionOf(evs []*model.SessionEvent) string {
	if len(evs) == 0 {
		return ""
	}
	return evs[0].SessionID
}

func TestPingEmitter_NoHostLogsOnly(t *testing.T) {
	em := New().pingEmitter(finishedPctx(200, pipeline.AgentRoleMain), "s1").(cgcomponents.KeepAliveEmitter)
	em.KeepAlivePing(cgcomponents.KeepAliveReport{Session: "s1", CacheRead: 1, Status: 200, Window: time.Now()})
}

// A real ping, sent by the keeper through cortexDispatcher to a fake upstream, reaches the
// accountant host's cost recorders as a keep-alive record.
func TestKeeperPingReachesTheCostRecorder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":1,"cache_read_input_tokens":48000,"output_tokens":1}}`))
	}))
	defer srv.Close()
	rec := &costRecorder{}
	p := New()
	p.SetAccounting(acctHost(t, rec))

	d := newCortexDispatcher(newPingClient(true))
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	k := keepalive.New(keepalive.Options{Dispatcher: d, Now: func() time.Time { return now },
		Go: func(f func()) { f() }})
	t.Cleanup(k.Shutdown)
	pol := keepalive.Policy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2,
		MaxUSDPerPing: 0.25, MinPrefixTokens: 20000}
	host := stripScheme(srv.URL)
	for i := 0; i < 2; i++ { // a session's first request is never held
		pctx := finishedPctx(200, pipeline.AgentRoleMain)
		pctx.Scheme, pctx.Host, pctx.StartedAt = "http", host, now
		hdr := pctx.Headers
		r, ok := recordFor(pctx, "s1", pol, func() dispatch.Ref { return d.hold("http", host, hdr) },
			p.pingEmitter(pctx, "s1"))
		if !ok {
			t.Fatal("setup: recordFor refused a main-thread request")
		}
		k.Record(r)
	}
	now = now.Add(281 * time.Second)
	if n := k.Sweep(now); n != 1 {
		t.Fatalf("sweep sent %d pings, want 1", n)
	}
	// Go runs the ping inline (set above), so Sweep returning means it already finished.
	evs := rec.events()
	if len(evs) != 1 {
		t.Fatalf("cost recorder got %d events, want 1", len(evs))
	}
	r, ok := event.Record(evs[0])
	if !ok || r.Purpose != accounting.PingPurpose {
		t.Fatalf("cost record = %+v (present %v), want Purpose %q", r, ok, accounting.PingPurpose)
	}
	if evs[0].Host != host || evs[0].Inference == nil || evs[0].Inference.CacheReadTokens != 48_000 {
		t.Errorf("event host %q inference %+v", evs[0].Host, evs[0].Inference)
	}
}

// A ping that timed out reaches the Host with the keeper's projection: counted as a timeout and
// booked at r.CostUSD, the only cost such a ping has (it has no usage to price).
func TestPingEmitter_PassesATimeoutWithItsProjectedCost(t *testing.T) {
	rd := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rd))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	h := accounting.New(accounting.Options{Sink: accounting.NewSink(mp, nil)})
	p := New()
	p.SetAccounting(h)
	pctx := finishedPctx(200, pipeline.AgentRoleMain)
	pctx.Host = "api.anthropic.com"
	em := p.pingEmitter(pctx, "s1").(cgcomponents.KeepAliveEmitter)
	em.KeepAlivePing(cgcomponents.KeepAliveReport{Session: "s1", Model: "claude-sonnet-5",
		TimedOut: true, CostUSD: 0.02, Window: time.Unix(1_700_000_000, 0)})
	_ = h.Close() // waits for the count

	var rm metricdata.ResourceMetrics
	if err := rd.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					if o, _ := dp.Attributes.Value("outcome"); o.AsString() != "" {
						got[m.Name+"{"+o.AsString()+"}"] += float64(dp.Value)
					}
				}
			case metricdata.Sum[float64]:
				for _, dp := range d.DataPoints {
					got[m.Name] += dp.Value
				}
			}
		}
	}
	want := map[string]float64{"llm.keepalive.pings{timeout}": 1, "llm.keepalive.cost_usd": 0.02, "llm.keepalive.estimated_cost_usd": 0.02}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v (all: %v)", k, got[k], v, got)
		}
	}
}
