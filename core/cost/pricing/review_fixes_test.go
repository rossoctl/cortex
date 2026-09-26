package pricing

import (
	"math"
	"strings"
	"testing"
)

// Cost validated each RATE but converted the accumulated total unchecked. A finite rate
// times a large token count overflows int64, and the conversion is then undefined —
// returning ok=true with a garbage ledger figure, which is worse than unpriced.
func TestCost_RejectsAnOverflowingTotal(t *testing.T) {
	var r Rates
	// A high but entirely finite rate: $0.01 per token, ~130x the priciest Claude tier.
	r.Base[TierInput], r.Set[TierInput] = 0.01, true

	if _, ok := Cost(r, Usage{Input: math.MaxInt32}); ok {
		// 2.1e9 tokens at $0.01 is $21M — large but representable, so this should price.
		t.Log("MaxInt32 tokens still prices, which is fine; the boundary is higher")
	}
	// Far past what int64 micros can hold.
	if micros, ok := Cost(r, Usage{Input: math.MaxInt64}); ok {
		t.Errorf("priced an overflowing total: %d micros", micros)
	}
	// And a total that would land negative after wrapping.
	if micros, ok := Cost(r, Usage{Input: math.MaxInt64, Output: math.MaxInt64}); ok {
		t.Errorf("priced a doubly-overflowing total: %d micros", micros)
	}
}

// matchHost compares a bracketed IPv6 pattern LITERALLY, because path.Match would read
// "[" as a character class. Ranking it as a glob made matching and ranking disagree: an
// overlapping glob of the same length could outrank the very address it names.
func TestResolution_IPv6LiteralOutranksAGlob(t *testing.T) {
	mk := func(host string, perM float64) Entry {
		var r Rates
		r.Base[TierInput], r.Set[TierInput] = perM/tokensPerMillion, true
		return Entry{Host: host, Model: "*", Rates: r, Prov: ProvConfigured}
	}
	for name, entries := range map[string][]Entry{
		"literal first": {mk("[fd00::1]", 1.00), mk("[fd00::?]", 99.00)},
		"glob first":    {mk("[fd00::?]", 99.00), mk("[fd00::1]", 1.00)},
	} {
		t.Run(name, func(t *testing.T) {
			tab, err := NewTable(entries)
			if err != nil {
				t.Fatal(err)
			}
			r, _ := tab.Resolve("[fd00::1]:4000", "m", 0)
			v, ok := r.For(TierInput)
			if !ok {
				t.Fatal("the IPv6 address resolved unpriced")
			}
			if got := v * tokensPerMillion; got != 1.00 {
				t.Errorf("rate = %.2f/Mtok, want 1.00 — a glob outranked the exact address", got)
			}
		})
	}
}

// The provenance switch was a denylist, so Provenance(99) passed — then outranked every
// valid row, because precedence IS the numeric ordering, while String() called it "none".
func TestNewTable_RejectsUndefinedProvenance(t *testing.T) {
	var r Rates
	r.Base[TierInput], r.Set[TierInput] = 1e-6, true
	for _, prov := range []Provenance{Provenance(99), Provenance(-1), Provenance(5)} {
		_, err := NewTable([]Entry{{Host: "*", Model: "*", Rates: r, Prov: prov}})
		if err == nil {
			t.Errorf("accepted provenance %d, which would outrank every valid row", int(prov))
			continue
		}
		if !strings.Contains(err.Error(), "not a table level") {
			t.Errorf("error for %d = %q, want it to name the problem", int(prov), err)
		}
	}
	// The three valid levels still build.
	for _, prov := range []Provenance{ProvBundled, ProvDiscovered, ProvConfigured} {
		if _, err := NewTable([]Entry{{Host: "*", Model: "*", Rates: r, Prov: prov}}); err != nil {
			t.Errorf("rejected valid provenance %s: %v", prov, err)
		}
	}
}
