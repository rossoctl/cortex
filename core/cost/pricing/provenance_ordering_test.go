package pricing

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ProvDiscovered has no producer in the tree: gateway rate discovery was designed,
// prototyped against LiteLLM's /model/info, and then deliberately dropped — the
// benefit was avoiding the transcription of three numbers that change monthly, and
// the cost was a virtual key to mint and mount, an outbound dependency and a refresh
// loop. See the spec's Discovery section.
//
// The level stays because the PRECEDENCE is the durable decision: a rate fetched from
// a gateway should beat the shipped table and lose to an operator's explicit override.
// Anything that later learns rates from a gateway lands there, and these tests keep
// the ordering and the guards honest in the meantime. They are cheap; the alternative
// is rediscovering the ordering argument from scratch.

// ProvDiscovered has to rank strictly between bundled and configured: a fetched rate
// should beat the shipped table but lose to an operator's explicit override.
func TestProvenance_DiscoveredRanksBetweenBundledAndConfigured(t *testing.T) {
	if !(ProvBundled < ProvDiscovered && ProvDiscovered < ProvConfigured) {
		t.Fatalf("ordering broken: bundled=%d discovered=%d configured=%d",
			ProvBundled, ProvDiscovered, ProvConfigured)
	}
	rate := func(perM float64, prov Provenance) Entry {
		var r Rates
		r.Base[TierInput], r.Set[TierInput] = perM/tokensPerMillion, true
		return Entry{Host: "gw.internal", Model: "claude-opus-5", Rates: r, Prov: prov}
	}
	for _, tc := range []struct {
		name    string
		entries []Entry
		want    float64
		prov    Provenance
	}{
		{"discovered beats bundled", []Entry{rate(5, ProvBundled), rate(3, ProvDiscovered)}, 3, ProvDiscovered},
		{"configured beats discovered", []Entry{rate(3, ProvDiscovered), rate(1, ProvConfigured)}, 1, ProvConfigured},
		{"all three", []Entry{rate(5, ProvBundled), rate(3, ProvDiscovered), rate(1, ProvConfigured)}, 1, ProvConfigured},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tab, err := NewTable(tc.entries)
			if err != nil {
				t.Fatal(err)
			}
			r, p := tab.Resolve("gw.internal", "claude-opus-5", 0)
			v, _ := r.For(TierInput)
			if v*tokensPerMillion != tc.want || p != tc.prov {
				t.Errorf("got %.2f/Mtok (%s), want %.2f (%s)", v*tokensPerMillion, p, tc.want, tc.prov)
			}
		})
	}
}

// A refreshed table must reach consumers that captured the Resolver earlier, without
// any of them re-registering. That is the whole reason Registry holds an atomic
// pointer, and it is what the config reloader relies on today.
func TestRegistry_SwapReachesConsumersCapturedEarlier(t *testing.T) {
	mk := func(perM float64, prov Provenance) *Table {
		var r Rates
		r.Base[TierInput], r.Set[TierInput] = perM/tokensPerMillion, true
		tab, err := NewTable([]Entry{{Host: "*", Model: "*", Rates: r, Prov: prov}})
		if err != nil {
			t.Fatal(err)
		}
		return tab
	}
	reg := NewRegistry(mk(5, ProvBundled))
	// Captured before the refresh, exactly as a plugin and the usage aggregator are.
	var consumer Resolver = reg

	reg.Swap(mk(3, ProvDiscovered)) // what a refresh loop would do

	r, p := consumer.Resolve("gw.internal", "claude-opus-5", 0)
	v, _ := r.For(TierInput)
	if v*tokensPerMillion != 3 || p != ProvDiscovered {
		t.Errorf("consumer saw %.2f/Mtok (%s) after a refresh, want 3.00 (discovered)", v*tokensPerMillion, p)
	}
}

// The strict decoder derives its key set by reflection rather than a hand-written
// list, so a new config field is accepted without touching strict.go — and, more to
// the point, a field RENAMED in Go starts being rejected in YAML loudly here rather
// than in an operator's config.
func TestStrict_KeySetComesFromReflection(t *testing.T) {
	keys := yamlKeys(reflect.TypeOf(Config{}))
	for _, want := range []string{"bundled", "endpoints"} {
		if _, ok := keys[want]; !ok {
			t.Errorf("yamlKeys missed the declared field %q, so reflection is not driving it", want)
		}
	}
	// An endpoint block's keys likewise come from the struct, not a hand-written list.
	epKeys := yamlKeys(reflect.TypeOf(EndpointConfig{}))
	for _, want := range []string{"hosts", "models"} {
		if _, ok := epKeys[want]; !ok {
			t.Errorf("yamlKeys missed EndpointConfig field %q", want)
		}
	}
	// And an undeclared key is still refused, which is what makes the above matter.
	var c Config
	err := yaml.Unmarshal([]byte("refresh: 5m\n"), &c)
	if err == nil {
		t.Fatal("an undeclared key was accepted; strictness is not in effect")
	}
	if !strings.Contains(err.Error(), "refresh") {
		t.Errorf("error %q does not name the undeclared key", err)
	}
}

// Cost must not trust a rate. Config validates what an operator writes, but nothing
// validates a rate reaching Cost by any other route, and the per-tier arithmetic is
// the last place to catch one.
func TestCost_GuardsRatesFromAnySource(t *testing.T) {
	var r Rates
	r.Base[TierInput], r.Set[TierInput] = -1, true
	if _, ok := Cost(r, Usage{Input: 1000}); ok {
		t.Error("Cost accepted a negative rate; a hostile /model/info response would emit negative money")
	}
}
