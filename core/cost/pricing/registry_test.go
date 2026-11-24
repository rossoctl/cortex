package pricing

import (
	"sync"
	"testing"
)

func TestRegistry_ResolvesFromItsTable(t *testing.T) {
	reg := NewRegistry(mustTable(t,
		Entry{Host: "*", Model: "*claude-opus-*", Rates: rate(3.80), Prov: ProvBundled},
	))
	r, p := reg.Resolve("gw.internal", "claude-opus-5", 0)
	if p != ProvBundled {
		t.Errorf("provenance = %s, want bundled", p)
	}
	if got := inputPerMillion(r); got != 3.80 {
		t.Errorf("input rate = %v, want 3.80", got)
	}
}

func TestRegistry_SwapIsVisibleToAnExistingHolder(t *testing.T) {
	// This is the reason Registry exists. buildPipelines is a closure the config
	// reloader re-invokes, but the usage aggregator that shares this resolver is
	// created once and outlives every rebuild. So the resolver must be swapped in
	// place — a holder from before the reload has to see the new rates.
	reg := NewRegistry(mustTable(t,
		Entry{Host: "*", Model: "*", Rates: rate(1.00), Prov: ProvConfigured},
	))
	var holder Resolver = reg // captured before the swap, as the aggregator is

	reg.Swap(mustTable(t,
		Entry{Host: "*", Model: "*", Rates: rate(2.00), Prov: ProvConfigured},
	))

	if got := inputPerMillion(first(holder.Resolve("h", "m", 0))); got != 2.00 {
		t.Errorf("holder saw %v after swap, want 2.00", got)
	}
}

func TestRegistry_NilIsUnpricedNotAPanic(t *testing.T) {
	// A binary built without pricing wiring must report traffic as unpriced, not
	// crash on the response path. Same posture as a nil Table.
	var reg *Registry
	if _, p := reg.Resolve("h", "m", 0); p != ProvNone {
		t.Errorf("nil Registry resolved as %s, want none", p)
	}
	if _, _, ok := reg.Cost("h", "m", Usage{Input: 1000}); ok {
		t.Error("nil Registry reported a priced request")
	}
	reg.Swap(nil) // must not panic
}

func TestRegistry_NilTableIsUnpriced(t *testing.T) {
	reg := NewRegistry(nil)
	if _, p := reg.Resolve("h", "m", 0); p != ProvNone {
		t.Errorf("Registry over a nil table resolved as %s, want none", p)
	}
}

func TestRegistry_Cost(t *testing.T) {
	reg := NewRegistry(mustTable(t,
		Entry{Host: "*", Model: "*claude-opus-*", Rates: rate(3.80), Prov: ProvBundled},
	))
	micros, prov, ok := reg.Cost("gw.internal", "claude-opus-5", Usage{Input: 1000})
	if !ok {
		t.Fatal("Cost reported unpriced")
	}
	if prov != ProvBundled {
		t.Errorf("provenance = %s, want bundled", prov)
	}
	if want := int64(3_800); micros != want {
		t.Errorf("Cost = %d micros, want %d", micros, want)
	}
}

// Registry.Cost includes the 1h cache-write premium. On a Claude model the 1h
// rate is 2x input: 1000 writes at 4.75 + 600 x (7.60 - 4.75) = 4750 + 1710 micros.
func TestRegistry_CostIncludesThe1hPremium(t *testing.T) {
	reg := NewRegistry(mustTable(t,
		Entry{Host: "*", Model: "*claude-opus-*", Rates: claudeOpus(), Prov: ProvBundled},
	))
	micros, _, ok := reg.Cost("gw.internal", "claude-opus-5", Usage{CacheWrite: 1000, CacheWrite1h: 600})
	if !ok {
		t.Fatal("Cost reported unpriced")
	}
	if want := int64(4750 + 1710); micros != want {
		t.Errorf("Cost = %d micros, want %d", micros, want)
	}
}

// TestRegistry_CostDoesNotDoubleAFlatGatewaysCacheWrite is huang195's review probe
// on PR #2065: a Claude model name is not proof the attached table bills at
// Anthropic's 1.25x-of-input cache-write ratio. A configured gateway that happens
// to serve claude-sonnet-4-5 at a FLAT rate (cache write == input) must not have
// its 1h writes doubled — that would price a premium the gateway never described,
// with nothing marking the figure as a guess. All-1h must cost the same as
// all-5m: this table gives CacheWrite1hRate no reason to prefer 2x input over the
// 5m rate it already has.
func TestRegistry_CostDoesNotDoubleAFlatGatewaysCacheWrite(t *testing.T) {
	flat := Rates{
		Base: [numTiers]float64{TierInput: 2.0 / perMillion, TierCacheWrite: 2.0 / perMillion},
		Set:  [numTiers]bool{TierInput: true, TierCacheWrite: true},
	}
	reg := NewRegistry(mustTable(t,
		Entry{Host: "*", Model: "claude-sonnet-4-5", Rates: flat, Prov: ProvConfigured},
	))
	fiveMOnly, _, ok := reg.Cost("gw.internal", "claude-sonnet-4-5", Usage{CacheWrite: 1_000_000})
	if !ok {
		t.Fatal("5m-only: Cost reported unpriced")
	}
	allOneHour, _, ok := reg.Cost("gw.internal", "claude-sonnet-4-5", Usage{CacheWrite: 1_000_000, CacheWrite1h: 1_000_000})
	if !ok {
		t.Fatal("all-1h: Cost reported unpriced")
	}
	if fiveMOnly != allOneHour {
		t.Errorf("5m-only = %d micros, all-1h = %d micros, want equal — a flat-rate table "+
			"gives no evidence for Anthropic's 2x-input 1h premium", fiveMOnly, allOneHour)
	}
	if want := int64(2_000_000); fiveMOnly != want {
		t.Errorf("5m-only = %d micros, want %d ($2/Mtok x 1M tokens)", fiveMOnly, want)
	}
}

func TestRegistry_CostOnUnknownModelIsUnpriced(t *testing.T) {
	reg := NewRegistry(mustTable(t,
		Entry{Host: "*", Model: "*claude-*", Rates: rate(3.80), Prov: ProvBundled},
	))
	micros, prov, ok := reg.Cost("gw.internal", "gpt-5", Usage{Input: 1000})
	if ok {
		t.Error("unknown model reported priced")
	}
	if prov != ProvNone {
		t.Errorf("provenance = %s, want none", prov)
	}
	if micros != 0 {
		t.Errorf("micros = %d, want 0", micros)
	}
}

func TestRegistry_CostReportsNoneWhenTiersAreUnpriced(t *testing.T) {
	// A matched row whose tiers do not cover the request is unpriced, and the
	// provenance must collapse to none — reporting "bundled" for a figure of zero
	// would label an absent answer as a sourced one.
	reg := NewRegistry(mustTable(t,
		Entry{Host: "*", Model: "*", Rates: Rates{
			Base: [numTiers]float64{TierCacheRead: 0.38 / perMillion},
			Set:  [numTiers]bool{TierCacheRead: true},
		}, Prov: ProvConfigured},
	))
	_, prov, ok := reg.Cost("h", "m", Usage{CacheWrite: 5000})
	if ok {
		t.Error("a tier with no rate reported priced")
	}
	if prov != ProvNone {
		t.Errorf("provenance = %s, want none", prov)
	}
}

func TestRegistry_ConcurrentResolveAndSwap(t *testing.T) {
	// Run under -race: the reloader swaps while request-path goroutines resolve.
	reg := NewRegistry(mustTable(t, Entry{Host: "*", Model: "*", Rates: rate(1.00), Prov: ProvConfigured}))
	tables := []*Table{
		mustTable(t, Entry{Host: "*", Model: "*", Rates: rate(2.00), Prov: ProvConfigured}),
		mustTable(t, Entry{Host: "*", Model: "*", Rates: rate(3.00), Prov: ProvConfigured}),
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 200; n++ {
				if _, p := reg.Resolve("gw.internal", "claude-opus-5", 250_000); p != ProvConfigured {
					t.Errorf("provenance = %s during swap, want configured", p)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for n := 0; n < 200; n++ {
			reg.Swap(tables[n%len(tables)])
		}
	}()
	wg.Wait()
}

func TestRegistryImplementsResolver(t *testing.T) {
	var _ Resolver = (*Registry)(nil)
}
