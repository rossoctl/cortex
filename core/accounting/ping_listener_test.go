package accounting

import (
	"testing"

	"github.com/rossoctl/accountant"
)

// A listener gets every accounted ping: its session, thread, tokens and its spend by basis. A ping
// on an API key is money (KeepAliveMicros): 1 input, 1,800 cache read and 1 output at pingRates is
// 3 + 540 + 15 = 558 micros.
func TestPingListener_GetsTheAccountedPing(t *testing.T) {
	h := New(Options{Resolver: pingRates(t)})
	t.Cleanup(func() { _ = h.Close() })
	h.setThreadBilling("s1", "main", accountant.BillingAPI)
	var got []PingSpend
	h.AddPingListener(func(s PingSpend) { got = append(got, s) })

	if _, ok := h.Ping("s1", nil, "api.anthropic.com", testPing(), okInfo()); !ok {
		t.Fatal("ping refused")
	}
	if len(got) != 1 {
		t.Fatalf("listener called %d times, want 1", len(got))
	}
	s := got[0]
	if s.Session != "s1" || s.Thread != "main" {
		t.Errorf("session, thread = %q, %q; want s1, main", s.Session, s.Thread)
	}
	if s.Usage.Input != 1 || s.Usage.CacheRead != 1800 || s.Usage.Output != 1 {
		t.Errorf("usage = %+v, want the ping's", s.Usage)
	}
	if s.Figures.KeepAliveMicros != 558 {
		t.Errorf("KeepAliveMicros = %d, want 558", s.Figures.KeepAliveMicros)
	}
	if s.Figures.AvoidedMicros != 0 || s.Figures.KeepAliveUnknownMicros != 0 {
		t.Errorf("figures = %+v, want the usd keep-alive spend alone", s.Figures)
	}
}

// With no recorder the ping still reaches the listeners: a process without a ledger still counts
// it against the session and its budget.
func TestPingListener_CalledWithNoRecorder(t *testing.T) {
	h := New(Options{Resolver: pingRates(t)})
	t.Cleanup(func() { _ = h.Close() })
	n := 0
	h.AddPingListener(func(PingSpend) { n++ })
	if _, ok := h.Ping("s1", nil, "api.anthropic.com", testPing(), okInfo()); !ok {
		t.Fatal("ping refused")
	}
	if n != 1 {
		t.Errorf("listener called %d times, want 1", n)
	}
}

// A removed listener is not called, and a refused ping calls no listener.
func TestPingListener_RemovedAndRefused(t *testing.T) {
	h := newHost(t)
	n := 0
	remove := h.AddPingListener(func(PingSpend) { n++ })
	bad := okInfo()
	bad.Status = 500
	if _, ok := h.Ping("s1", nil, "h", testPing(), bad); ok {
		t.Fatal("a 500 ping was accounted")
	}
	if n != 0 {
		t.Errorf("a refused ping called the listener %d times", n)
	}
	remove()
	remove() // idempotent
	if _, ok := h.Ping("s1", nil, "h", testPing(), okInfo()); !ok {
		t.Fatal("ping refused")
	}
	if n != 0 {
		t.Errorf("a removed listener was called %d times", n)
	}
}

// A panicking listener costs only itself: the others run, and Ping still reports ok.
func TestPingListener_PanicIsRecovered(t *testing.T) {
	h := newHost(t)
	h.AddPingListener(func(PingSpend) { panic("boom") })
	n := 0
	h.AddPingListener(func(PingSpend) { n++ })
	if _, ok := h.Ping("s1", nil, "h", testPing(), okInfo()); !ok {
		t.Fatal("ping refused")
	}
	if n != 1 {
		t.Errorf("the second listener was called %d times, want 1", n)
	}
}

func TestPingListener_NilHostAndNilFunc(t *testing.T) {
	var h *Host
	h.AddPingListener(func(PingSpend) {})()
	newHost(t).AddPingListener(nil)()
}
