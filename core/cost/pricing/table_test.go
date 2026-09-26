package pricing

import (
	"strings"
	"testing"
)

// rate builds a Rates priced only on input, at the given USD per million. Enough
// to tell resolved rows apart by their value.
func rate(perM float64) Rates {
	return Rates{
		Base: [numTiers]float64{TierInput: perM / perMillion},
		Set:  [numTiers]bool{TierInput: true},
	}
}

// inputPerMillion is the inverse of rate, for readable assertions.
func inputPerMillion(r Rates) float64 { return r.Base[TierInput] * perMillion }

func mustTable(t *testing.T, entries ...Entry) *Table {
	t.Helper()
	tab, err := NewTable(entries)
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	return tab
}

func TestProvenanceString(t *testing.T) {
	for p, want := range map[Provenance]string{
		ProvNone:          "none",
		ProvBundled:       "bundled",
		ProvDiscovered:    "discovered",
		ProvConfigured:    "configured",
		ProvAuthoritative: "authoritative",
	} {
		if got := p.String(); got != want {
			t.Errorf("Provenance(%d).String() = %q, want %q", p, got, want)
		}
	}
}

func TestResolve_ProvenanceOutranksSpecificity(t *testing.T) {
	// A configured catch-all beats a bundled exact match. An override is an
	// override: an operator who priced their gateway must not be silently
	// overruled by the shipped table just because it names the model precisely.
	tab := mustTable(t,
		Entry{Host: "*", Model: "*", Rates: rate(1.00), Prov: ProvConfigured},
		Entry{Host: "*", Model: "claude-opus-5", Rates: rate(5.00), Prov: ProvBundled},
	)
	r, p := tab.Resolve("gw.internal", "claude-opus-5", 0)
	if p != ProvConfigured {
		t.Errorf("provenance = %s, want configured", p)
	}
	if got := inputPerMillion(r); got != 1.00 {
		t.Errorf("input rate = %v, want 1.00", got)
	}
}

func TestResolve_ConfiguredBeatsDiscoveredBeatsBundled(t *testing.T) {
	all := []Entry{
		{Host: "*", Model: "claude-opus-5", Rates: rate(3.00), Prov: ProvBundled},
		{Host: "*", Model: "claude-opus-5", Rates: rate(2.00), Prov: ProvDiscovered},
		{Host: "*", Model: "claude-opus-5", Rates: rate(1.00), Prov: ProvConfigured},
	}
	for _, tc := range []struct {
		name    string
		entries []Entry
		want    Provenance
		perM    float64
	}{
		{"all three", all, ProvConfigured, 1.00},
		{"discovered and bundled", all[:2], ProvDiscovered, 2.00},
		{"bundled only", all[:1], ProvBundled, 3.00},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, p := mustTable(t, tc.entries...).Resolve("gw.internal", "claude-opus-5", 0)
			if p != tc.want {
				t.Errorf("provenance = %s, want %s", p, tc.want)
			}
			if got := inputPerMillion(r); got != tc.perM {
				t.Errorf("input rate = %v, want %v", got, tc.perM)
			}
		})
	}
}

func TestResolve_ExactModelBeatsGlobWithinLevel(t *testing.T) {
	// This is the whole point of the bundled slice: it distinguishes
	// claude-opus-4-1 from claude-opus-5, which a family glob cannot.
	tab := mustTable(t,
		Entry{Host: "*", Model: "*claude-opus-*", Rates: rate(5.00), Prov: ProvBundled},
		Entry{Host: "*", Model: "claude-opus-4-1", Rates: rate(15.00), Prov: ProvBundled},
	)
	if got := inputPerMillion(first(tab.Resolve("api.anthropic.com", "claude-opus-4-1", 0))); got != 15.00 {
		t.Errorf("exact model rate = %v, want 15.00", got)
	}
	// And a version the exact rows do not name still lands on the family glob.
	if got := inputPerMillion(first(tab.Resolve("api.anthropic.com", "claude-opus-6", 0))); got != 5.00 {
		t.Errorf("glob fallback rate = %v, want 5.00", got)
	}
}

func TestResolve_LongerGlobWins(t *testing.T) {
	tab := mustTable(t,
		Entry{Host: "*", Model: "*claude-*", Rates: rate(1.00), Prov: ProvBundled},
		Entry{Host: "*", Model: "*claude-opus-*", Rates: rate(5.00), Prov: ProvBundled},
	)
	if got := inputPerMillion(first(tab.Resolve("h", "claude-opus-5", 0))); got != 5.00 {
		t.Errorf("rate = %v, want 5.00 (longer glob)", got)
	}
	if got := inputPerMillion(first(tab.Resolve("h", "claude-haiku-4-5", 0))); got != 1.00 {
		t.Errorf("rate = %v, want 1.00 (shorter glob)", got)
	}
}

func TestResolve_NamedHostBeatsCatchAllHost(t *testing.T) {
	// The endpoint axis outranks the model axis: a gateway priced by the operator
	// must not be overruled by a broader model glob on "*".
	tab := mustTable(t,
		Entry{Host: "*", Model: "*claude-opus-*", Rates: rate(15.00), Prov: ProvConfigured},
		Entry{Host: "gw.internal", Model: "*", Rates: rate(3.80), Prov: ProvConfigured},
	)
	if got := inputPerMillion(first(tab.Resolve("gw.internal", "claude-opus-5", 0))); got != 3.80 {
		t.Errorf("rate on named host = %v, want 3.80", got)
	}
	// Traffic to any other endpoint still gets the catch-all row.
	if got := inputPerMillion(first(tab.Resolve("api.anthropic.com", "claude-opus-5", 0))); got != 15.00 {
		t.Errorf("rate on other host = %v, want 15.00", got)
	}
}

func TestResolve_HostScopingExcludes(t *testing.T) {
	// The requirement this dimension exists for: one laptop, several endpoints,
	// each priced differently. A row for one gateway must not price another.
	tab := mustTable(t,
		Entry{Host: "gw-a.internal", Model: "*", Rates: rate(3.80), Prov: ProvConfigured},
		Entry{Host: "gw-b.internal", Model: "*", Rates: rate(7.60), Prov: ProvConfigured},
	)
	if got := inputPerMillion(first(tab.Resolve("gw-a.internal", "m", 0))); got != 3.80 {
		t.Errorf("gw-a rate = %v, want 3.80", got)
	}
	if got := inputPerMillion(first(tab.Resolve("gw-b.internal", "m", 0))); got != 7.60 {
		t.Errorf("gw-b rate = %v, want 7.60", got)
	}
	if _, p := tab.Resolve("api.anthropic.com", "m", 0); p != ProvNone {
		t.Errorf("unlisted endpoint resolved as %s, want none", p)
	}
}

func TestResolve_HostIsCaseInsensitive(t *testing.T) {
	tab := mustTable(t, Entry{Host: "GW.Internal", Model: "*", Rates: rate(3.80), Prov: ProvConfigured})
	if _, p := tab.Resolve("gw.internal", "m", 0); p != ProvConfigured {
		t.Errorf("host match was case-sensitive: got %s", p)
	}
}

func TestResolve_ModelIsCaseInsensitive(t *testing.T) {
	// Gateways vary in how they echo model names, and a case mismatch would
	// silently unprice the traffic rather than fail visibly.
	tab := mustTable(t, Entry{Host: "*", Model: "Claude-Opus-5", Rates: rate(5.00), Prov: ProvBundled})
	if _, p := tab.Resolve("h", "claude-opus-5", 0); p != ProvBundled {
		t.Errorf("model match was case-sensitive: got %s", p)
	}
}

func TestResolve_ModelGlobSpansDashAndSlash(t *testing.T) {
	// Provider prefixes and dated suffixes must not defeat a family glob, so the
	// model matcher is compiled with no separator (toolprune/pricing.go:65-67).
	tab := mustTable(t, Entry{Host: "*", Model: "*claude-opus-*", Rates: rate(5.00), Prov: ProvBundled})
	for _, model := range []string{
		"claude-opus-5",
		"aws/claude-opus-4-1-20250805",
		"anthropic/claude-opus-4-8",
	} {
		if _, p := tab.Resolve("h", model, 0); p != ProvBundled {
			t.Errorf("model %q did not match the family glob", model)
		}
	}
}

func TestResolve_NoMatchIsProvNone(t *testing.T) {
	tab := mustTable(t, Entry{Host: "*", Model: "*claude-*", Rates: rate(5.00), Prov: ProvBundled})
	r, p := tab.Resolve("h", "gpt-5", 0)
	if p != ProvNone {
		t.Errorf("provenance = %s, want none", p)
	}
	if r.any() {
		t.Error("unmatched model returned rates")
	}
}

func TestResolve_FlattensThresholdForPromptTotal(t *testing.T) {
	r := rate(3.80)
	r.Thresholds = []ContextThreshold{{
		AbovePromptTokens: 200_000,
		Rate:              [numTiers]float64{TierInput: 7.60 / perMillion},
		Set:               [numTiers]bool{TierInput: true},
	}}
	tab := mustTable(t, Entry{Host: "*", Model: "*", Rates: r, Prov: ProvConfigured})

	// Resolve hands back already-flattened rates, so a caller cannot forget to
	// apply the premium.
	got, _ := tab.Resolve("h", "m", 300_000)
	if inputPerMillion(got) != 7.60 {
		t.Errorf("resolved input rate above threshold = %v, want 7.60", inputPerMillion(got))
	}
	if len(got.Thresholds) != 0 {
		t.Error("Resolve returned rates that still carry thresholds")
	}
	got, _ = tab.Resolve("h", "m", 100_000)
	if inputPerMillion(got) != 3.80 {
		t.Errorf("resolved input rate below threshold = %v, want 3.80", inputPerMillion(got))
	}
}

func TestNilTableResolvesNone(t *testing.T) {
	// A binary built without pricing wiring must report traffic as unpriced, not
	// panic on the response path.
	var tab *Table
	if _, p := tab.Resolve("h", "m", 0); p != ProvNone {
		t.Errorf("nil table resolved as %s, want none", p)
	}
}

func TestNewTable_Rejects(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry Entry
		want  string
	}{{
		// Authoritative is a settled per-request figure, not a rate. A table row
		// claiming it would outrank every real rate and never be corrected.
		name:  "authoritative provenance",
		entry: Entry{Host: "*", Model: "*", Rates: rate(1), Prov: ProvAuthoritative},
		want:  "authoritative",
	}, {
		name:  "no provenance",
		entry: Entry{Host: "*", Model: "*", Rates: rate(1)},
		want:  "provenance",
	}, {
		// A row that prices nothing matches traffic and then resolves it as
		// unpriced — indistinguishable from having no row, and it hides the typo.
		name:  "prices nothing",
		entry: Entry{Host: "*", Model: "*", Prov: ProvConfigured},
		want:  "no rate",
	}, {
		name:  "bad model glob",
		entry: Entry{Host: "*", Model: "claude-[", Rates: rate(1), Prov: ProvConfigured},
		want:  "model pattern",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewTable([]Entry{tc.entry})
			if err == nil {
				t.Fatal("NewTable accepted an invalid entry")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestNewTable_EmptyIsValid(t *testing.T) {
	// An operator with no pricing config and the bundled slice disabled is a
	// legitimate deployment; it prices nothing and says so.
	tab, err := NewTable(nil)
	if err != nil {
		t.Fatalf("NewTable(nil): %v", err)
	}
	if _, p := tab.Resolve("h", "m", 0); p != ProvNone {
		t.Errorf("empty table resolved as %s, want none", p)
	}
}

func TestTableImplementsResolver(t *testing.T) {
	var _ Resolver = (*Table)(nil)
}

// first drops the provenance from a Resolve result, for assertions that only
// look at the rates.
func first(r Rates, _ Provenance) Rates { return r }
