// External test package: the golden test imports internal/pricegen, which imports
// pricing, so an in-package test would be an import cycle.
package pricing_test

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/cost/pricing/internal/pricegen"
)

const snapshotPath = "testdata/model_prices.snapshot.json"

// TestBundled_MatchesSnapshot is the golden test.
//
// It re-runs the generator's transform over the committed snapshot and compares
// the result to the committed table. That makes it hermetic — no network — while
// still catching every way the pair can drift: a hand-edit to bundled.go, a
// hand-edit to the snapshot, or a change to pricegen that was not followed by a
// regeneration.
//
// It also catches a class of bug that is otherwise invisible: rates rendered as Go
// INTEGER constant expressions. "15 / 1000000" is integer division in Go and
// compiles to zero, so a whole-dollar rate would silently unprice its model while
// the fractional rate beside it worked. The regenerated entries carry real float64
// values from JSON, so any such row compares unequal here.
func TestBundled_MatchesSnapshot(t *testing.T) {
	raw, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	want, err := pricegen.Entries(raw)
	if err != nil {
		t.Fatalf("pricegen.Entries: %v", err)
	}
	got := pricing.Bundled()

	if len(got) != len(want) {
		t.Fatalf("bundled table has %d entries, snapshot yields %d — regenerate with `make pricing-table COMMIT=%s`",
			len(got), len(want), pricing.BundledUpstreamCommit)
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("entry %d (%s) drifted:\n  table:    %+v\n  snapshot: %+v",
				i, want[i].Model, got[i], want[i])
		}
	}
}

// TestBundled_SnapshotMatchesTheRecordedCommit ties the two committed files together.
//
// Without it either could be regenerated alone and the golden comparison would still
// pass, since both derive from whatever was fetched last — so the pin could drift from
// the data it claims to describe.
func TestBundled_SnapshotMatchesTheRecordedCommit(t *testing.T) {
	raw, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	stamped := pricegen.SnapshotCommit(raw)
	if stamped == "" {
		t.Fatal("the snapshot carries no commit stamp; regenerate with `make pricing-table COMMIT=<sha>`")
	}
	if stamped != pricing.BundledUpstreamCommit {
		t.Errorf("snapshot was generated from %s but the table records %s — one of the two files was regenerated alone",
			stamped, pricing.BundledUpstreamCommit)
	}
}

// TestBundled_EveryAnthropicSnapshotKeyHasAnExactRow breaks the circularity in the
// golden test above.
//
// That test compares Entries(snapshot) against a table built from the same Entries,
// so a model silently dropped during decoding vanishes from BOTH sides and the length
// check still passes. This walks the snapshot JSON directly — an independent decode
// that shares no code with Entries — and asserts a row exists for every
// anthropic-provider key it finds.
func TestBundled_EveryAnthropicSnapshotKeyHasAnExactRow(t *testing.T) {
	raw, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var rawAll map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawAll); err != nil {
		t.Fatalf("parse snapshot: %v", err)
	}

	// Independent minimal shape: only the provider field, decoded here rather than
	// through pricegen's own type.
	var want []string
	for name, body := range rawAll {
		if name == pricegen.SnapshotCommitKey {
			continue
		}
		var probe struct {
			Provider string `json:"litellm_provider"`
		}
		if json.Unmarshal(body, &probe) != nil || probe.Provider != pricegen.AnthropicProvider {
			continue
		}
		want = append(want, name)
	}
	sort.Strings(want)
	if len(want) == 0 {
		t.Fatal("snapshot contains no anthropic entries; the fixture is broken")
	}

	have := map[string]bool{}
	for _, e := range pricing.Bundled() {
		have[e.Model] = true
	}
	var missing []string
	for _, name := range want {
		if !have[strings.ToLower(name)] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d of %d anthropic models in the snapshot have no exact row, so they would price via a family glob or not at all: %s",
			len(missing), len(want), strings.Join(missing, ", "))
	}
}

func TestBundled_UpstreamCommitIsPinned(t *testing.T) {
	// A table that cannot say where it came from turns "refresh the rates" back
	// into a measurement exercise, which is how the stale 4x comment survived a
	// gateway repricing.
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(pricing.BundledUpstreamCommit) {
		t.Errorf("BundledUpstreamCommit = %q, want a full 40-character sha", pricing.BundledUpstreamCommit)
	}
}

func TestBundled_BuildsIntoATable(t *testing.T) {
	// NewTable rejects rows with no provenance, no rates, or a bad glob, so this
	// asserts every generated row is well-formed.
	if _, err := pricing.NewTable(pricing.Bundled()); err != nil {
		t.Fatalf("bundled table does not build: %v", err)
	}
}

func TestBundled_ReturnsADeepCopy(t *testing.T) {
	// The rows are package state shared by every Registry, so a caller that wrote
	// through one would corrupt every later NewTable. Thresholds are the part a
	// shallow copy still aliases, which is why they are checked explicitly.
	a := pricing.Bundled()
	if len(a) == 0 {
		t.Fatal("bundled table is empty")
	}
	a[0].Model = "mutated"
	if pricing.Bundled()[0].Model == "mutated" {
		t.Error("Bundled() aliases its rows")
	}

	var i, ti = -1, -1
	for n, e := range a {
		if len(e.Rates.Thresholds) > 0 {
			i, ti = n, 0
			break
		}
	}
	if i < 0 {
		t.Skip("no bundled entry carries a threshold to test aliasing against")
	}
	before := pricing.Bundled()[i].Rates.Thresholds[ti].Rate
	a[i].Rates.Thresholds[ti].Rate[pricing.TierInput] = 99
	if after := pricing.Bundled()[i].Rates.Thresholds[ti].Rate; after != before {
		t.Errorf("Bundled() aliases threshold state: %v became %v", before, after)
	}
}

// TestBundled_DistinguishesOpusVersions is success criterion 2 from the spec:
// claude-opus-4-1 and claude-opus-5 must price ~3x apart from bundled data with no
// operator configuration. The three hand-measured family globs this table replaces
// priced them identically, so every opus-4-1 request was understated 3x.
func TestBundled_DistinguishesOpusVersions(t *testing.T) {
	tab, err := pricing.NewTable(pricing.Bundled())
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}

	rate := func(model string) float64 {
		r, p := tab.Resolve("api.anthropic.com", model, 0)
		if p != pricing.ProvBundled {
			t.Fatalf("%s resolved as %s, want bundled", model, p)
		}
		v, ok := r.For(pricing.TierInput)
		if !ok {
			t.Fatalf("%s has no input rate — check for integer constant division in bundled.go", model)
		}
		return v
	}

	old, current := rate("claude-opus-4-1"), rate("claude-opus-5")
	if old <= 0 || current <= 0 {
		t.Fatalf("rates must be positive: opus-4-1 %v, opus-5 %v", old, current)
	}
	if ratio := old / current; ratio < 2.5 || ratio > 3.5 {
		t.Errorf("opus-4-1 / opus-5 input ratio = %v, want ~3x (4-1 %v, 5 %v)", ratio, old, current)
	}
}

func TestBundled_FamilyGlobCoversAnUnreleasedVersion(t *testing.T) {
	// A model released after this table was generated must still price, or it would
	// drop out of the dollar total silently. Exact beats glob, so a known version
	// is unaffected by the extrapolation.
	tab, err := pricing.NewTable(pricing.Bundled())
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	for _, model := range []string{
		"claude-opus-9",                // not yet released
		"aws/claude-sonnet-5-20990101", // provider prefix and a dated suffix
		"anthropic/claude-haiku-7",
	} {
		if _, p := tab.Resolve("api.anthropic.com", model, 0); p != pricing.ProvBundled {
			t.Errorf("%s resolved as %s, want bundled via a family glob", model, p)
		}
	}
}

func TestBundled_LongContextThresholdIsCarried(t *testing.T) {
	// claude-sonnet-4-5 doubles its input rate above 200k prompt tokens upstream.
	// Losing that in generation would price every long session at the base rate.
	tab, err := pricing.NewTable(pricing.Bundled())
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	below, _ := tab.Resolve("api.anthropic.com", "claude-sonnet-4-5", 100_000)
	above, _ := tab.Resolve("api.anthropic.com", "claude-sonnet-4-5", 300_000)

	lo, ok := below.For(pricing.TierInput)
	if !ok {
		t.Fatal("no input rate below the threshold")
	}
	hi, ok := above.For(pricing.TierInput)
	if !ok {
		t.Fatal("no input rate above the threshold")
	}
	if hi <= lo {
		t.Errorf("long-context input rate %v is not above the base rate %v", hi, lo)
	}
}
