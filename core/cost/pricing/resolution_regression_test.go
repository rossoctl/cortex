package pricing

import (
	"math"
	"strings"
	"testing"
)

// The bundled table's whole justification is per-version accuracy, and these assert
// the RATE, not merely that something matched. The original bundled test fed
// provider-prefixed names and checked only provenance, so it passed while returning
// the wrong price — the failure mode this file exists to prevent.
func TestResolution_ProviderPrefixedNamesKeepTheirVersionRate(t *testing.T) {
	tab, err := NewTable(Bundled())
	if err != nil {
		t.Fatal(err)
	}
	rate := func(model string) float64 {
		r, p := tab.Resolve("api.anthropic.com", model, 0)
		if p == ProvNone {
			t.Fatalf("%s resolved unpriced", model)
		}
		v, ok := r.For(TierInput)
		if !ok {
			t.Fatalf("%s has no input rate", model)
		}
		return v * tokensPerMillion
	}

	// A gateway echoing "anthropic/claude-opus-4-1" must not be charged opus-5's
	// rate. This was 5.00 instead of 15.00 — the exact 3x error the generated table
	// replaced the family globs to fix.
	bare := rate("claude-opus-4-1")
	for _, prefixed := range []string{
		"anthropic/claude-opus-4-1",
		"aws/claude-opus-4-1",
		"bedrock/claude-opus-4-1",
	} {
		if got := rate(prefixed); got != bare {
			t.Errorf("%s = %.2f/Mtok, want %.2f (its own version's rate, not a family fallback)", prefixed, got, bare)
		}
	}

	// Version-FIRST names, which the old family glob could not match at all.
	if got := rate("bedrock/claude-3-opus-20240229"); got != rate("claude-3-opus-20240229") {
		t.Errorf("prefixed version-first name = %.2f/Mtok, want %.2f", got, rate("claude-3-opus-20240229"))
	}
}

func TestResolution_ThresholdSurvivesAProviderPrefix(t *testing.T) {
	tab, err := NewTable(Bundled())
	if err != nil {
		t.Fatal(err)
	}
	at := func(model string, prompt int) float64 {
		r, _ := tab.Resolve("api.anthropic.com", model, prompt)
		v, _ := r.For(TierInput)
		return v * tokensPerMillion
	}
	// claude-sonnet-4-5 doubles its input rate above 200k upstream. A prefixed name
	// used to fall to the family glob, which has no threshold, so a long-context
	// request was priced at the base rate.
	bare, prefixed := at("claude-sonnet-4-5", 300_000), at("anthropic/claude-sonnet-4-5", 300_000)
	if prefixed != bare {
		t.Errorf("prefixed @300k = %.2f/Mtok, want %.2f (threshold lost through the prefix)", prefixed, bare)
	}
	if base := at("claude-sonnet-4-5", 100_000); bare <= base {
		t.Fatalf("fixture is not exercising a threshold: @300k %.2f vs @100k %.2f", bare, base)
	}
}

func TestResolution_ExactHostBeatsAGlobThatCoversIt(t *testing.T) {
	mk := func(host string, perM float64) Entry {
		var r Rates
		r.Base[TierInput], r.Set[TierInput] = perM/tokensPerMillion, true
		return Entry{Host: host, Model: "*", Rates: r, Prov: ProvConfigured}
	}
	// Both orders: the bug reproduced either way, because it came from the
	// lexicographic tie-break ("*" sorts before any letter), not from slice order.
	for name, entries := range map[string][]Entry{
		"exact first": {mk("a.internal", 1.00), mk("*.internal", 99.00)},
		"glob first":  {mk("*.internal", 99.00), mk("a.internal", 1.00)},
	} {
		t.Run(name, func(t *testing.T) {
			tab, err := NewTable(entries)
			if err != nil {
				t.Fatal(err)
			}
			r, _ := tab.Resolve("a.internal", "m", 0)
			v, _ := r.For(TierInput)
			if got := v * tokensPerMillion; got != 1.00 {
				t.Errorf("rate = %.2f/Mtok, want 1.00 — an operator pinning one gateway beside a broader glob got the broad rate", got)
			}
			// And the glob still covers everything it should.
			r, _ = tab.Resolve("b.internal", "m", 0)
			v, _ = r.For(TierInput)
			if got := v * tokensPerMillion; got != 99.00 {
				t.Errorf("unpinned host rate = %.2f/Mtok, want 99.00", got)
			}
		})
	}
}

func TestResolution_CatchAllHostSpellingsRankEqually(t *testing.T) {
	// plugin-catalog.md documents "" and "*" as identical. They must therefore lose
	// to a more specific MODEL the same way, which len("*")==1 vs len("")==0 broke.
	var catchAll, exact Rates
	catchAll.Base[TierInput], catchAll.Set[TierInput] = 42.0/tokensPerMillion, true
	exact.Base[TierInput], exact.Set[TierInput] = 7.0/tokensPerMillion, true

	for _, star := range []string{"*", ""} {
		tab, err := NewTable([]Entry{
			{Host: star, Model: "*", Rates: catchAll, Prov: ProvConfigured},
			{Host: "", Model: "claude-opus-5", Rates: exact, Prov: ProvConfigured},
		})
		if err != nil {
			t.Fatal(err)
		}
		r, _ := tab.Resolve("h", "claude-opus-5", 0)
		v, _ := r.For(TierInput)
		if got := v * tokensPerMillion; got != 7.00 {
			t.Errorf("host %q: rate = %.2f/Mtok, want 7.00 — a catch-all shadowed an exact model", star, got)
		}
	}
}

// TestResolution_NativeProviderIdentifiers covers the identifier shapes a gateway
// may echo verbatim. The "*/"+key form alone was anchored too tightly: it tolerated
// a slash-delimited prefix and nothing else, so Bedrock's canonical form fell
// through to the family glob and was priced at the newest opus rate.
func TestResolution_NativeProviderIdentifiers(t *testing.T) {
	tab, err := NewTable(Bundled())
	if err != nil {
		t.Fatal(err)
	}
	rate := func(model string) (float64, Provenance) {
		r, p := tab.Resolve("api.anthropic.com", model, 0)
		v, _ := r.For(TierInput)
		return v * tokensPerMillion, p
	}
	want, _ := rate("claude-opus-4-1-20250805")
	if want == 0 {
		t.Fatal("fixture broken: the dated opus-4-1 row is missing")
	}

	for _, model := range []string{
		"anthropic.claude-opus-4-1-20250805-v1:0", // Bedrock canonical
		"anthropic.claude-opus-4-1-20250805",      // dot prefix only
		"claude-opus-4-1-20250805-v1:0",           // version tail only
		"claude-opus-4-1-20250805:thinking",       // variant selector
		"  claude-opus-4-1-20250805  ",            // stray whitespace
		"AWS/Claude-Opus-4-1-20250805",            // case plus prefix
	} {
		got, prov := rate(model)
		if got != want {
			t.Errorf("%q = %.2f/Mtok (%s), want %.2f — fell through to a family glob", model, got, prov, want)
		}
	}
}

func TestNewTable_RejectsAPortBearingHostPattern(t *testing.T) {
	// It matched NOTHING and was accepted: hostKey strips the port from the endpoint,
	// never from the pattern. Copying an endpoint out of a URL is the likeliest way
	// to write this field, and the symptom was silent — unpriced, or billed at
	// vendor list with the bundled table on.
	var r Rates
	r.Base[TierInput], r.Set[TierInput] = 1e-6, true
	_, err := NewTable([]Entry{{Host: "gw.internal:4000", Model: "*", Rates: r, Prov: ProvConfigured}})
	if err == nil {
		t.Fatal("accepted a host pattern with a port, which can never match")
	}
	for _, want := range []string{"port", "gw.internal"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestCost_RejectsHostileRates(t *testing.T) {
	// Cost validated the token count and trusted the rate. Only config validated
	// rates, so every other producer was unguarded — including phase 7's discovery
	// path, where the numbers arrive from a remote gateway.
	for name, bad := range map[string]float64{
		"negative": -1e-6,
		"+Inf":     math.Inf(1),
		"-Inf":     math.Inf(-1),
		"NaN":      math.NaN(),
	} {
		t.Run(name, func(t *testing.T) {
			var r Rates
			r.Base[TierInput], r.Set[TierInput] = bad, true
			micros, ok := Cost(r, Usage{Input: 1000})
			if ok {
				t.Errorf("priced a request at a %s rate: %d micros", name, micros)
			}
			if micros != 0 {
				t.Errorf("micros = %d, want 0", micros)
			}
		})
	}
}

func TestTable_DoesNotAliasCallerThresholds(t *testing.T) {
	// A Table is documented immutable; aliasing the caller's slice meant mutating the
	// Entry afterwards changed a live table under concurrent readers.
	var r Rates
	r.Base[TierInput], r.Set[TierInput] = 3.0/tokensPerMillion, true
	r.Thresholds = []ContextThreshold{{
		AbovePromptTokens: 200_000,
		Rate:              [numTiers]float64{TierInput: 6.0 / tokensPerMillion},
		Set:               [numTiers]bool{TierInput: true},
	}}
	entries := []Entry{{Host: "*", Model: "*", Rates: r, Prov: ProvConfigured}}
	tab, err := NewTable(entries)
	if err != nil {
		t.Fatal(err)
	}
	entries[0].Rates.Thresholds[0].Rate[TierInput] = 99.0 / tokensPerMillion

	got, _ := tab.Resolve("h", "m", 300_000)
	v, _ := got.For(TierInput)
	if want := 6.0; v*tokensPerMillion != want {
		t.Errorf("threshold rate = %.2f/Mtok after mutating the caller's slice, want %.2f", v*tokensPerMillion, want)
	}
}

// TestResolution_IntermediateFormWins pins the ordering the reviewer found: a wire
// name must land on a row matching the MOST specific normalized form, not the fully
// normalized one.
//
// Normalizing all the way and matching once skipped intermediate rows — ":0" came off
// "claude-custom-v2:0" to yield an exact row, then stripping continued and landed on
// the less specific "claude-custom".
func TestResolution_IntermediateFormWins(t *testing.T) {
	mk := func(model string, perM float64) Entry {
		var r Rates
		r.Base[TierInput], r.Set[TierInput] = perM/tokensPerMillion, true
		return Entry{Host: "*", Model: model, Rates: r, Prov: ProvConfigured}
	}
	tab, err := NewTable([]Entry{mk("claude-custom-v2", 42.00), mk("claude-custom", 7.00)})
	if err != nil {
		t.Fatal(err)
	}
	rate := func(model string) float64 {
		r, _ := tab.Resolve("h", model, 0)
		v, _ := r.For(TierInput)
		return v * tokensPerMillion
	}

	if got := rate("claude-custom-v2:0"); got != 42.00 {
		t.Errorf("claude-custom-v2:0 = %.2f/Mtok, want 42.00 — stripping ran past an exact row", got)
	}
	// Both plain forms still resolve to themselves.
	if got := rate("claude-custom-v2"); got != 42.00 {
		t.Errorf("claude-custom-v2 = %.2f/Mtok, want 42.00", got)
	}
	if got := rate("claude-custom"); got != 7.00 {
		t.Errorf("claude-custom = %.2f/Mtok, want 7.00", got)
	}
	// And the documented limit of the heuristic: an unknown version discriminator
	// falls back rather than going unpriced. This is what makes Bedrock's -v1:0 work.
	if got := rate("claude-custom-v9"); got != 7.00 {
		t.Errorf("claude-custom-v9 = %.2f/Mtok, want 7.00 (documented fallback)", got)
	}
}
