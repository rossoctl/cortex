package accounting

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/session"
)

// A ping's cost record carries the basis of the thread's billing mode (design §5.7a), so the
// ledger and /v1/usage book its dollars as money only on an API key, exactly where the metrics
// sink moves its USD counters. A subscription ping adds nothing to KeepAliveMicros and its
// usage-limit units, output included, to KeepAliveUnits; the sink books no USD for it. An
// unknown-billing ping is shown apart and moves no money either. Every ping counts in
// KeepAlivePings.
func TestPing_BasisFollowsTheThreadsBilling_LedgerAndSinkAgree(t *testing.T) {
	w := config.DefaultSubscriptionWeights()
	p := testPing()
	// Input 1, CacheRead 1800, Output 1 at the default weights: 1 + 180 + 5 units.
	wantUnits := event.MilliUnits(Units(w, accountant.TierTokens{Input: p.Usage.Input, CacheRead: p.Usage.CacheRead}, p.Usage.Output))
	if wantUnits != 186_000 {
		t.Fatalf("test arithmetic: units = %d, want 186000", wantUnits)
	}
	for _, tc := range []struct {
		mode  accountant.BillingMode
		basis string
	}{
		{accountant.BillingAPI, event.BasisUSD},
		{accountant.BillingSubscription, event.BasisSubscriptionEquivalent},
		{accountant.BillingUnknown, event.BasisUnknown},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			mp, rd := manual(t)
			rates := pingRates(t)
			ring := usage.New(usage.WithPricing(rates))
			rec := &fakeRecorder{}
			h, err := New(Options{Resolver: rates, Sink: NewSink(mp, rates), CostRecorders: []session.Recorder{ring, rec}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = h.Close() })
			h.setThreadBilling("s1", "main", tc.mode)
			// At now, so the ring's window holds it.
			ping := testPing()
			ping.At = time.Now()
			ping.Window = ping.At.Add(-4 * time.Minute)
			if _, ok := h.Ping("s1", nil, "api.anthropic.com", ping, okInfo()); !ok {
				t.Fatal("ping refused")
			}
			h.observing.Wait()

			_, evs := rec.got()
			if len(evs) != 1 {
				t.Fatalf("%d events, want 1", len(evs))
			}
			var r event.Event
			if err := json.Unmarshal(evs[0].Plugins[event.Key], &r); err != nil {
				t.Fatal(err)
			}
			if r.Basis != tc.basis || r.Estimated != (tc.basis != event.BasisUSD) {
				t.Errorf("record basis %q estimated %v, want %q %v", r.Basis, r.Estimated, tc.basis, tc.basis != event.BasisUSD)
			}

			tot := ring.Snapshot(10*usage.BucketWidth, usage.BucketWidth, "", usage.GroupNone).Totals
			sinkUSD := sum(t, rd, "llm.savings.added.usd", nil)
			if tot.KeepAlivePings != 1 {
				t.Errorf("KeepAlivePings = %d, want 1", tot.KeepAlivePings)
			}
			if got := int64(math.Round(sinkUSD * 1e6)); got != tot.KeepAliveMicros {
				t.Errorf("sink added.usd = %d micros, ledger KeepAliveMicros = %d; they must agree", got, tot.KeepAliveMicros)
			}
			switch tc.mode {
			case accountant.BillingAPI:
				if tot.KeepAliveMicros <= 0 || tot.KeepAliveUnits != 0 || tot.KeepAliveUnknownMicros != 0 {
					t.Errorf("api ping: %+v, want only KeepAliveMicros", tot)
				}
			case accountant.BillingSubscription:
				if tot.KeepAliveMicros != 0 || tot.KeepAliveUnits != wantUnits || tot.KeepAliveAPIEquivalentMicros <= 0 {
					t.Errorf("subscription ping: micros %d units %d API equivalent %d, want 0, %d, >0",
						tot.KeepAliveMicros, tot.KeepAliveUnits, tot.KeepAliveAPIEquivalentMicros, wantUnits)
				}
			case accountant.BillingUnknown:
				if tot.KeepAliveMicros != 0 || tot.KeepAliveUnits != 0 || tot.KeepAliveUnknownMicros <= 0 {
					t.Errorf("unknown ping: %+v, want only KeepAliveUnknownMicros", tot)
				}
			}
		})
	}
}
