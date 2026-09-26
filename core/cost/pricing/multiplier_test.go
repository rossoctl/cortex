package pricing

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The IBM LiteLLM gateways bill a uniform fraction of vendor list — measured at
// exactly 0.7600 across three models and all four tiers. That is a scalar, not a rate
// card, so it is expressed as a multiplier: one number instead of twelve, and it tracks
// upstream repricing automatically because the gateway's price is DERIVED from list.
func TestMultiplier_BundledDefaultCoversTheIBMGateways(t *testing.T) {
	tab, err := Build(nil) // no operator config whatsoever
	if err != nil {
		t.Fatal(err)
	}
	inputPerM := func(host, model string) (float64, Provenance) {
		r, p := tab.Resolve(host, model, 0)
		v, _ := r.For(TierInput)
		return v * tokensPerMillion, p
	}

	// The real gateway, with and without a port.
	for _, host := range []string{
		"ete-litellm.ai-models.vpc-int.res.ibm.com",
		"ete-litellm.ai-models.vpc-int.res.ibm.com:443",
	} {
		got, prov := inputPerM(host, "claude-opus-5")
		if want := 5.00 * 0.76; got < want-1e-9 || got > want+1e-9 {
			t.Errorf("%s opus-5 input = %.4f/Mtok, want %.4f (list x 0.76)", host, got, want)
		}
		if prov != ProvBundled {
			t.Errorf("%s provenance = %s, want bundled (rate and multiplier both shipped)", host, prov)
		}
	}

	// Direct to the vendor must stay at LIST. A global default would understate this
	// by 24% — silently, which is the failure mode the whole package exists to avoid.
	if got, _ := inputPerM("api.anthropic.com", "claude-opus-5"); got != 5.00 {
		t.Errorf("api.anthropic.com opus-5 input = %.4f/Mtok, want 5.00 (list, unscaled)", got)
	}
	// And someone else's gateway is not covered by the IBM rule.
	if got, _ := inputPerM("litellm.example.com", "claude-opus-5"); got != 5.00 {
		t.Errorf("third-party gateway = %.4f/Mtok, want 5.00 (unscaled)", got)
	}
}

// Every tier scales, not just input — the discount was measured as uniform.
func TestMultiplier_AppliesToEveryTier(t *testing.T) {
	tab, err := Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	const host = "ete-litellm.ai-models.vpc-int.res.ibm.com"
	// Measured on the gateway: opus-5 3.80 / 4.75 / 0.38 / 19.00.
	for _, tc := range []struct {
		tier Tier
		want float64
	}{
		{TierInput, 3.80}, {TierCacheWrite, 4.75}, {TierCacheRead, 0.38}, {TierOutput, 19.00},
	} {
		r, _ := tab.Resolve(host, "claude-opus-5", 0)
		v, ok := r.For(tc.tier)
		if !ok {
			t.Errorf("tier %d unpriced", tc.tier)
			continue
		}
		if got := v * tokensPerMillion; got < tc.want-1e-9 || got > tc.want+1e-9 {
			t.Errorf("tier %d = %.4f/Mtok, want %.4f (measured on the gateway)", tc.tier, got, tc.want)
		}
	}
	// Haiku included, cache tiers too, per the decision to apply it uniformly.
	r, _ := tab.Resolve(host, "claude-haiku-4-5", 0)
	if v, ok := r.For(TierCacheRead); !ok || v*tokensPerMillion < 0.076-1e-9 || v*tokensPerMillion > 0.076+1e-9 {
		t.Errorf("haiku cache-read = %.5f/Mtok (ok=%v), want 0.076", v*tokensPerMillion, ok)
	}
}

// Long-context thresholds must scale as well, or a discounted gateway would be
// correct below 200k and wrong above it.
func TestMultiplier_ScalesContextThresholds(t *testing.T) {
	var r Rates
	r.Base[TierInput], r.Set[TierInput] = 3.00/tokensPerMillion, true
	r.Thresholds = []ContextThreshold{{
		AbovePromptTokens: 200_000,
		Rate:              [numTiers]float64{TierInput: 6.00 / tokensPerMillion},
		Set:               [numTiers]bool{TierInput: true},
	}}
	half := 0.5
	tab, err := NewTable([]Entry{{Host: "gw", Model: "*", Rates: r, Prov: ProvConfigured}},
		MultiplierRule{Host: "gw", Factor: half, Prov: ProvConfigured})
	if err != nil {
		t.Fatal(err)
	}
	below, _ := tab.Resolve("gw", "m", 100_000)
	above, _ := tab.Resolve("gw", "m", 300_000)
	if v, _ := below.For(TierInput); v*tokensPerMillion != 1.50 {
		t.Errorf("below threshold = %.2f/Mtok, want 1.50", v*tokensPerMillion)
	}
	if v, _ := above.For(TierInput); v*tokensPerMillion != 3.00 {
		t.Errorf("above threshold = %.2f/Mtok, want 3.00 (the premium, halved)", v*tokensPerMillion)
	}
}

func TestMultiplier_ConfiguredOverridesBundled(t *testing.T) {
	var c Config
	src := `
endpoints:
  - hosts: ["ete-litellm.ai-models.vpc-int.res.ibm.com"]
    multiplier: 0.50
`
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatal(err)
	}
	tab, err := Build(&c)
	if err != nil {
		t.Fatal(err)
	}
	// An exact host beats the bundled *.res.ibm.com glob, and a configured multiplier
	// outranks a bundled one — an operator whose deal changed says so once.
	r, p := tab.Resolve("ete-litellm.ai-models.vpc-int.res.ibm.com", "claude-opus-5", 0)
	v, _ := r.For(TierInput)
	if got := v * tokensPerMillion; got != 2.50 {
		t.Errorf("input = %.2f/Mtok, want 2.50 (list x 0.50)", got)
	}
	if p != ProvConfigured {
		t.Errorf("provenance = %s, want configured — the operator supplied the multiplier", p)
	}
}

func TestMultiplier_Validation(t *testing.T) {
	for name, src := range map[string]string{
		// 76 instead of 0.76 inflates every figure 100x and looks plausible in YAML.
		"typo: whole number":         "endpoints:\n  - hosts: [gw]\n    multiplier: 76\n",
		"zero would free everything": "endpoints:\n  - hosts: [gw]\n    multiplier: 0\n",
		"negative":                   "endpoints:\n  - hosts: [gw]\n    multiplier: -1\n",
	} {
		t.Run(name, func(t *testing.T) {
			var c Config
			if err := yaml.Unmarshal([]byte(src), &c); err != nil {
				t.Fatal(err)
			}
			_, err := Build(&c)
			if err == nil {
				t.Fatalf("accepted %s", name)
			}
			if !strings.Contains(err.Error(), "multiplier") {
				t.Errorf("error %q does not name the field", err)
			}
		})
	}
	// 1.0 is legal and means "no discount", which is worth being able to state.
	var c Config
	if err := yaml.Unmarshal([]byte("endpoints:\n  - hosts: [gw]\n    multiplier: 1.0\n"), &c); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(&c); err != nil {
		t.Errorf("rejected multiplier 1.0: %v", err)
	}
}

// A multiplier-only endpoint needs no models block: it scales what already resolves.
func TestMultiplier_EndpointNeedsNoModels(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("endpoints:\n  - hosts: [gw]\n    multiplier: 0.5\n"), &c); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(&c); err != nil {
		t.Fatalf("rejected a multiplier-only endpoint: %v", err)
	}
}

// Two behaviours the docs now promise, both surprising enough that they must be pinned:
// a configured catch-all outranks a shipped host-specific rule (provenance decides before
// specificity), and an explicit 1.0 is how an operator drops a shipped discount.
func TestMultiplier_ConfiguredCatchAllOutranksShippedSpecific(t *testing.T) {
	one := 1.0
	nine := 0.9
	shipped := "ete-litellm.ai-models.vpc-int.res.ibm.com"

	// Control: shipped rule alone gives 0.76.
	base := mustBuild(t, nil)
	if f, prov := base.multiplierFor(shipped); f != 0.76 || prov != ProvBundled {
		t.Fatalf("shipped rule = %v/%v, want 0.76/bundled", f, prov)
	}

	t.Run("catch-all replaces it", func(t *testing.T) {
		tab := mustBuild(t, &Config{Endpoints: []EndpointConfig{{Hosts: []string{"*"}, Multiplier: &nine}}})
		f, prov := tab.multiplierFor(shipped)
		if f != 0.9 || prov != ProvConfigured {
			t.Errorf("multiplierFor(%s) = %v/%v, want 0.9/configured — a configured catch-all must outrank the shipped specific rule", shipped, f, prov)
		}
	})

	t.Run("explicit 1.0 drops the shipped discount", func(t *testing.T) {
		tab := mustBuild(t, &Config{Endpoints: []EndpointConfig{{Hosts: []string{shipped}, Multiplier: &one}}})
		if f, _ := tab.multiplierFor(shipped); f != 1 {
			t.Errorf("multiplierFor = %v, want 1 — an explicit 1.0 must cancel the shipped factor", f)
		}
		// And the rates must come back at list, not scaled.
		rates, prov := tab.Resolve(shipped, "claude-opus-5", 0)
		if got := perM(rates, TierInput); got != 5 {
			t.Errorf("input = %v/Mtok, want the unscaled 5 (prov %v)", got, prov)
		}
		// Reported CONFIGURED, not bundled. Writing `multiplier: 1.0` is an operator
		// telling us their endpoint bills at list — the same act as writing 0.76 — so
		// reporting bundled would send them a WarnIfUnpinned line about the endpoint
		// they just pinned.
		if prov != ProvConfigured {
			t.Errorf("provenance = %v, want configured for a deliberate 1.0 pin", prov)
		}
	})
}

// scale is unreachable from Resolve with thresholds present, because Resolve flattens
// first. Tested directly anyway: "no caller reaches it today" is how the previous version
// came to pass thresholds through unscaled without anything noticing.
//
// 0.5 is used deliberately — halving is exact in binary, so these are equalities rather
// than tolerances.
func TestRates_ScaleScalesThresholds(t *testing.T) {
	var r Rates
	r.Base[TierInput] = 10e-6
	r.Set[TierInput] = true
	r.Thresholds = []ContextThreshold{{AbovePromptTokens: 200_000}}
	r.Thresholds[0].Rate[TierInput] = 15e-6
	r.Thresholds[0].Set[TierInput] = true

	got := r.scale(0.5)
	if got.Base[TierInput] != 5e-6 {
		t.Errorf("base = %v, want 5e-6", got.Base[TierInput])
	}
	if len(got.Thresholds) != 1 {
		t.Fatalf("thresholds dropped: %+v", got.Thresholds)
	}
	if got.Thresholds[0].Rate[TierInput] != 7.5e-6 {
		t.Errorf("threshold rate = %v, want 7.5e-6 — a discounted gateway discounts its long-context tier too",
			got.Thresholds[0].Rate[TierInput])
	}
	if r.Thresholds[0].Rate[TierInput] != 15e-6 {
		t.Errorf("scale mutated the receiver's thresholds: %v", r.Thresholds[0].Rate[TierInput])
	}

	// The property that makes the ordering safe: scaling and flattening COMMUTE. Resolve
	// flattens then scales; any other caller may scale then flatten. Both must charge the
	// same, which is exactly what passing thresholds through unscaled broke.
	a := r.At(500_000).scale(0.5)
	b := r.scale(0.5).At(500_000)
	if a.Base != b.Base || a.Set != b.Set {
		t.Errorf("scale and At do not commute:\n flatten-then-scale %v\n scale-then-flatten %v", a.Base, b.Base)
	}
	if b.Base[TierInput] != 7.5e-6 {
		t.Errorf("above the threshold = %v, want the scaled premium 7.5e-6", b.Base[TierInput])
	}
}

// An operator who measures their gateway and pins the real per-model rates must not have
// those rates scaled again by a shipped multiplier. The pinned figures are already
// post-discount; scaling them understates spend by the factor, and understating is the
// direction that hides cost.
func TestMultiplier_ShippedFactorDoesNotScaleConfiguredRates(t *testing.T) {
	shipped := "ete-litellm.ai-models.vpc-int.res.ibm.com"
	// The operator's own measurement: 3.80/Mtok input, i.e. list x 0.76 already.
	var pinned Rates
	pinned.Base[TierInput] = 3.8 / tokensPerMillion
	pinned.Set[TierInput] = true

	tab, err := NewTable(
		[]Entry{{Host: shipped, Model: "claude-opus-5", Rates: pinned, Prov: ProvConfigured}},
		bundledMultipliers()...,
	)
	if err != nil {
		t.Fatal(err)
	}
	rates, prov := tab.Resolve(shipped, "claude-opus-5", 0)
	if got := perM(rates, TierInput); got != 3.8 {
		t.Errorf("input = %v/Mtok, want the pinned 3.8 unscaled; 2.888 means the shipped 0.76 was applied on top", got)
	}
	if prov != ProvConfigured {
		t.Errorf("provenance = %v, want configured", prov)
	}
	// A CONFIGURED multiplier still applies to configured rates — the operator asked
	// for both, and the host view shows the factor.
	half := 0.5
	tab2, err := Build(&Config{Endpoints: []EndpointConfig{{
		Hosts:      []string{shipped},
		Multiplier: &half,
		Models: map[string]ModelConfig{"claude-opus-5": {
			TierRates: TierRates{InputCostPerMillion: 3.8},
		}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := perM(mustResolve(t, tab2, shipped, "claude-opus-5"), TierInput); got != 1.9 {
		t.Errorf("configured x configured = %v/Mtok, want 1.9", got)
	}
}

func mustResolve(t *testing.T, tab *Table, host, model string) Rates {
	t.Helper()
	r, prov := tab.Resolve(host, model, 0)
	if prov == ProvNone {
		t.Fatalf("%s/%s resolved unpriced", host, model)
	}
	return r
}

// Both checks the rate rows apply, applied to multiplier rules too. Each failure is
// silent and in the same direction: the gateway stays at list, overstating a discount.
func TestMultiplier_RejectsPortAndDuplicateRules(t *testing.T) {
	t.Run("port in the pattern", func(t *testing.T) {
		_, err := NewTable(nil, MultiplierRule{Host: "gw.internal:4000", Factor: 0.9, Prov: ProvConfigured})
		if err == nil {
			t.Fatal("accepted a host pattern with a port, which can never match")
		}
		if !strings.Contains(err.Error(), `use "gw.internal"`) {
			t.Errorf("error does not name the fix: %v", err)
		}
	})
	t.Run("duplicate host and provenance", func(t *testing.T) {
		_, err := NewTable(nil,
			MultiplierRule{Host: "gw.internal", Factor: 0.9, Prov: ProvConfigured},
			MultiplierRule{Host: "gw.internal", Factor: 0.5, Prov: ProvConfigured},
		)
		if err == nil {
			t.Fatal("accepted two factors for one host; one would be silently ignored")
		}
	})
	t.Run("same host at different provenance is fine", func(t *testing.T) {
		if _, err := NewTable(nil,
			MultiplierRule{Host: "gw.internal", Factor: 0.9, Prov: ProvBundled},
			MultiplierRule{Host: "gw.internal", Factor: 0.5, Prov: ProvConfigured},
		); err != nil {
			t.Fatalf("rejected a legitimate operator override of a shipped rule: %v", err)
		}
	})
}
