// Package pricegen turns LiteLLM's public model_prices_and_context_window.json
// into pricing.Entry rows and renders them as Go source.
//
// It lives in internal/ because it is build tooling, not runtime code, and it is
// a library rather than living inside the generator command so the golden test can
// run the exact same transform against a committed snapshot. A generator whose
// transform only exists inside a main package cannot be tested without a network.
package pricegen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/format"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/rossoctl/cortex/core/cost/pricing"
)

// AnthropicProvider is the litellm_provider value we keep. Only the first-party
// Anthropic endpoint's rates are vendor list prices; the vertex_ai/ and bedrock/
// mirrors carry their own, and mixing them under one "*" host scope would make the
// bundled table's numbers depend on map iteration order.
const AnthropicProvider = "anthropic"

// contextThreshold is the one long-context breakpoint in scope. LiteLLM also
// publishes _above_272k and _above_512k variants for other providers, and an
// _above_1hr cache-write premium; both are deliberately out of scope (see
// pricing.Tier), so their fields are ignored rather than silently flattened.
const contextThreshold = 200_000

// tokensPerMillion renders rates in the unit providers publish.
const tokensPerMillion = 1_000_000

// modelInfo is the subset of a LiteLLM price-map entry that this package reads.
type modelInfo struct {
	Provider string `json:"litellm_provider"`

	Input      *float64 `json:"input_cost_per_token"`
	CacheWrite *float64 `json:"cache_creation_input_token_cost"`
	CacheRead  *float64 `json:"cache_read_input_token_cost"`
	Output     *float64 `json:"output_cost_per_token"`

	InputAbove      *float64 `json:"input_cost_per_token_above_200k_tokens"`
	CacheWriteAbove *float64 `json:"cache_creation_input_token_cost_above_200k_tokens"`
	CacheReadAbove  *float64 `json:"cache_read_input_token_cost_above_200k_tokens"`
	OutputAbove     *float64 `json:"output_cost_per_token_above_200k_tokens"`
}

// Filter reduces a full price map to the Anthropic-provider entries, so the
// committed snapshot is kilobytes rather than the 2.3 MB upstream file, and stamps
// the upstream commit into it under SnapshotCommitKey.
//
// Idempotent: filtering an already-filtered map is a no-op, which is what lets the
// generator and the golden test run the same Entries transform over different
// inputs.
// SnapshotCommitKey is the synthetic key the generator stamps into the snapshot so
// the golden test can prove the snapshot and the generated table came from the SAME
// upstream commit. Without it either file could be regenerated alone and the pair
// would still compare equal, since both derive from whatever was fetched last.
const SnapshotCommitKey = "_litellm_commit"

func Filter(raw []byte, commit string) ([]byte, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, fmt.Errorf("pricegen: parse price map: %w", err)
	}
	keep := map[string]json.RawMessage{}
	for name, body := range all {
		var header struct {
			Provider string `json:"litellm_provider"`
		}
		if json.Unmarshal(body, &header) != nil || header.Provider != AnthropicProvider {
			continue // sample_spec, non-model rows, and other providers
		}
		var mi modelInfo
		if err := json.Unmarshal(body, &mi); err != nil {
			// Provider-verified and still unusable: dropping it removes the row from
			// the snapshot too, and nothing downstream can then notice it is gone.
			// Upstream changing a cost field's JSON type is exactly how this arrives.
			return nil, fmt.Errorf("pricegen: %s entry %q failed to decode, which would silently drop it from the snapshot: %w", AnthropicProvider, name, err)
		}
		_ = mi // decoded to prove it is usable; the raw body is what gets stored
		keep[name] = body
	}
	if len(keep) == 0 {
		return nil, fmt.Errorf("pricegen: no %s entries in the price map", AnthropicProvider)
	}
	if commit != "" {
		stamp, err := json.Marshal(commit)
		if err != nil {
			return nil, err
		}
		keep[SnapshotCommitKey] = stamp
	}
	return json.MarshalIndent(keep, "", "  ")
}

// Entries builds the bundled rows from a price map.
//
// Two kinds of row are emitted, both at ProvBundled:
//
//   - One EXACT row per model. This is the point of generating the table: the data
//     already distinguishes claude-opus-4-1 ($15/Mtok input) from claude-opus-5
//     ($5/Mtok), which the three hand-measured family globs it replaces priced
//     identically — a 3x error on every opus-4-1 request.
//   - One family GLOB row per family, carrying the newest member's rates, so a
//     model released after this table was generated still prices instead of
//     dropping out of the dollar total. Exact beats glob within a provenance
//     level, so a known version always wins over its family's extrapolation.
//
// The family rows are an extrapolation and are the reason provenance exists: they
// are bundled, not authoritative, and an operator whose gateway disagrees pins it
// in config.
func Entries(raw []byte) ([]pricing.Entry, error) {
	// Decoded per entry rather than as map[string]modelInfo, because the snapshot
	// carries the generator's commit stamp as a bare string alongside the model
	// objects, and a whole-map decode into a struct type fails on it.
	var rawAll map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawAll); err != nil {
		return nil, fmt.Errorf("pricegen: parse price map: %w", err)
	}
	all := make(map[string]modelInfo, len(rawAll))
	var skipped []string
	for name, body := range rawAll {
		if name == SnapshotCommitKey {
			continue // the generator's provenance stamp, not a model
		}
		// The PROVIDER is decoded first, alone, so "is this a row we care about" never
		// depends on the rest of the row parsing. Keying that off the model NAME — as
		// this did — misses an Anthropic row named unconventionally, and that row is
		// then dropped silently.
		var header struct {
			Provider string `json:"litellm_provider"`
		}
		if json.Unmarshal(body, &header) != nil || header.Provider != AnthropicProvider {
			continue // sample_spec, non-model rows, and other providers
		}
		var mi modelInfo
		if err := json.Unmarshal(body, &mi); err != nil {
			// Provider-verified, so this is a row we meant to keep: record it and fail
			// below rather than dropping a real model quietly.
			skipped = append(skipped, fmt.Sprintf("%s (%v)", name, err))
			continue
		}
		all[name] = mi
	}
	sort.Strings(skipped)
	if len(skipped) > 0 {
		// Everything reaching here is already provider-verified, so any failure is a
		// row we meant to keep — no name heuristic needed to tell them apart.
		return nil, fmt.Errorf("pricegen: %d %s entr(ies) failed to decode, which would silently drop them from the table: %s",
			len(skipped), AnthropicProvider, strings.Join(skipped, "; "))
	}

	var out []pricing.Entry
	newest := map[string]string{} // family -> winning model key

	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic output regardless of map order

	for _, name := range names {
		if name == SnapshotCommitKey {
			continue // the generator's provenance stamp, not a model
		}
		mi := all[name]
		if mi.Provider != AnthropicProvider {
			continue
		}
		rates, ok := ratesOf(mi)
		if !ok {
			continue // no usable rate: a row that prices nothing is rejected by NewTable
		}
		out = append(out, pricing.Entry{
			Host:  "*",
			Model: name,
			Rates: rates,
			Prov:  pricing.ProvBundled,
		})
		if fam, ver, dated := parseName(name); fam != "" {
			cur, seen := newest[fam]
			if !seen {
				newest[fam] = name
			} else if iv, id := versionOf(cur); moreRecent(ver, dated, iv, id) {
				newest[fam] = name
			}
		}
	}

	fams := make([]string, 0, len(newest))
	for fam := range newest {
		fams = append(fams, fam)
	}
	sort.Strings(fams)
	for _, fam := range fams {
		rates, ok := ratesOf(all[newest[fam]])
		if !ok {
			continue
		}
		out = append(out, pricing.Entry{
			Host: "*",
			// "*claude-*<fam>-*", not "*claude-<fam>-*": LiteLLM uses both
			// orderings — "claude-opus-4-1" (family first) and
			// "claude-3-opus-20240229" (version first) — and the narrower glob
			// matched only the first, so version-first names outside the exact rows
			// dropped out of the total entirely.
			Model: "*claude-*" + fam + "-*",
			Rates: rates,
			Prov:  pricing.ProvBundled,
		})
	}
	return out, nil
}

// ratesOf converts one price-map entry into Rates, reporting whether any tier was
// priced at all.
func ratesOf(mi modelInfo) (pricing.Rates, bool) {
	var r pricing.Rates
	set := func(tier pricing.Tier, v *float64) {
		if v != nil && *v > 0 {
			r.Base[tier], r.Set[tier] = *v, true
		}
	}
	set(pricing.TierInput, mi.Input)
	set(pricing.TierCacheWrite, mi.CacheWrite)
	set(pricing.TierCacheRead, mi.CacheRead)
	set(pricing.TierOutput, mi.Output)

	var th pricing.ContextThreshold
	th.AbovePromptTokens = contextThreshold
	setAbove := func(tier pricing.Tier, v *float64) {
		if v != nil && *v > 0 {
			th.Rate[tier], th.Set[tier] = *v, true
		}
	}
	setAbove(pricing.TierInput, mi.InputAbove)
	setAbove(pricing.TierCacheWrite, mi.CacheWriteAbove)
	setAbove(pricing.TierCacheRead, mi.CacheReadAbove)
	setAbove(pricing.TierOutput, mi.OutputAbove)
	for _, s := range th.Set {
		if s {
			r.Thresholds = []pricing.ContextThreshold{th}
			break
		}
	}
	return r, r.Base != [4]float64{} || len(r.Thresholds) > 0
}

var (
	dateSuffix = regexp.MustCompile(`^\d{8}$`)
	numeric    = regexp.MustCompile(`^\d+$`)
)

// parseName splits a model key into its family, version tuple and whether it
// carries a date suffix.
//
// Handles both spellings LiteLLM uses: "claude-opus-4-1" (family then version) and
// "claude-3-7-sonnet-20250219" (version then family). The family is the first
// token that is neither "claude" nor a number, so neither ordering needs a special
// case.
func parseName(key string) (family string, version []int, dated bool) {
	for _, tok := range strings.Split(strings.ToLower(key), "-") {
		switch {
		case tok == "claude":
		case dateSuffix.MatchString(tok):
			dated = true
		case numeric.MatchString(tok):
			n, _ := strconv.Atoi(tok)
			version = append(version, n)
		case family == "":
			family = tok
		}
	}
	return family, version, dated
}

func versionOf(key string) (version []int, dated bool) {
	_, v, d := parseName(key)
	return v, d
}

// moreRecent reports whether (version, dated) beats the incumbent.
//
// An undated key wins over a dated one at the same version — "claude-opus-4-5" is
// the alias an operator's traffic actually names, while the dated key is the
// pinned snapshot. Otherwise the higher version tuple wins.
func moreRecent(version []int, dated bool, iv []int, id bool) bool {
	if c := compareVersion(version, iv); c != 0 {
		return c > 0
	}
	return id && !dated
}

func compareVersion(a, b []int) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] > b[i] {
				return 1
			}
			return -1
		}
	}
	switch {
	case len(a) > len(b):
		return 1
	case len(a) < len(b):
		return -1
	}
	return 0
}

// Render emits the generated Go source for a bundled table.
func Render(entries []pricing.Entry, commit string) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, `// Code generated by pricing/internal/gen. DO NOT EDIT.
//
// Regenerate with: make pricing-table COMMIT=<litellm commit sha>
//
// Source: BerriAI/litellm model_prices_and_context_window.json. These are VENDOR
// LIST prices for the first-party Anthropic endpoint. A gateway that bills below
// list — most internal LiteLLM deployments do — is OVERSTATED by this table, and
// an operator corrects it with a host-scoped %spricing:%s entry, which outranks
// anything bundled.
//
// Rates are USD per TOKEN, emitted as the shortest decimal that round-trips back to
// the identical float64. They are deliberately NOT written as a per-million value
// divided by a constant: that form read better but was lossy, so some rates could
// not be reproduced by regenerating and the golden test became unfixable.

package pricing

// BundledUpstreamCommit is the BerriAI/litellm commit this table was generated
// from. Pinned so "refresh the rates" is a scripted diff against a known base
// rather than a measurement exercise, and so the golden test can prove the
// checked-in table still matches its snapshot.
const BundledUpstreamCommit = %q

// Bundled returns the price table shipped in the binary.
//
// Deeply copied, thresholds included. The rows are package state shared by every
// Registry, so a caller that appended to the slice — or wrote through a row's
// Thresholds, which a shallow copy would still alias — would corrupt every later
// NewTable in the process.
func Bundled() []Entry {
	out := make([]Entry, len(bundledEntries))
	for i, e := range bundledEntries {
		if e.Rates.Thresholds != nil {
			e.Rates.Thresholds = append([]ContextThreshold(nil), e.Rates.Thresholds...)
		}
		out[i] = e
	}
	return out
}

var bundledEntries = []Entry{
`, "`", "`", commit)

	for _, e := range entries {
		fmt.Fprintf(&b, "\t{Host: %q, Model: %q, Prov: ProvBundled, Rates: Rates{\n", e.Host, e.Model)
		fmt.Fprintf(&b, "\t\tBase: %s,\n", renderFloats(e.Rates.Base))
		fmt.Fprintf(&b, "\t\tSet:  %s,\n", renderBools(e.Rates.Set))
		for _, th := range e.Rates.Thresholds {
			b.WriteString("\t\tThresholds: []ContextThreshold{{\n")
			fmt.Fprintf(&b, "\t\t\tAbovePromptTokens: %d,\n", th.AbovePromptTokens)
			fmt.Fprintf(&b, "\t\t\tRate:              %s,\n", renderFloats(th.Rate))
			fmt.Fprintf(&b, "\t\t\tSet:               %s,\n", renderBools(th.Set))
			b.WriteString("\t\t}},\n")
		}
		b.WriteString("\t}},\n")
	}
	b.WriteString("}\n")

	src, err := format.Source(b.Bytes())
	if err != nil {
		return nil, fmt.Errorf("pricegen: generated source does not parse: %w", err)
	}
	return src, nil
}

var tierNames = [4]string{"TierInput", "TierCacheWrite", "TierCacheRead", "TierOutput"}

func renderFloats(v [4]float64) string {
	var parts []string
	for i, f := range v {
		if f == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %s", tierNames[i], floatLiteral(f)))
	}
	return "[numTiers]float64{" + strings.Join(parts, ", ") + "}"
}

// floatLiteral renders a Go FLOAT constant that round-trips exactly.
//
// The rate is emitted as the per-TOKEN value it actually is, not as a per-million
// value divided by a constant. The divided form read better but was lossy: the
// shortest decimal for f*1e6 does not always round-trip back through the division,
// so upstream 2.1000000000000003e-08 folded to 2.1e-08 and five committed rows
// carried 0.09999999999999999 while the file header claimed exact folding. Worse,
// the golden test compares against the parsed upstream value with DeepEqual, so any
// such rate failed a comparison that regenerating reproduced identically — a
// failure fixable only by the hand-edit the same test forbids.
//
// 'g' with precision -1 is Go's shortest representation that parses back to the
// identical float64, which is exactly the property needed.
//
// The decimal point still matters: Go's untyped constant arithmetic would make an
// integer literal integer-divide, and an earlier version of this generator emitted
// "15 / 1000000" — integer division, folding to ZERO, silently unpricing every
// whole-dollar rate while the fractional rate beside it worked.
func floatLiteral(f float64) string {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

func renderBools(v [4]bool) string {
	var parts []string
	for i, s := range v {
		if s {
			parts = append(parts, tierNames[i]+": true")
		}
	}
	return "[numTiers]bool{" + strings.Join(parts, ", ") + "}"
}

// SnapshotCommit reads the upstream commit stamped into a snapshot, or "" if absent.
func SnapshotCommit(raw []byte) string {
	var all map[string]json.RawMessage
	if json.Unmarshal(raw, &all) != nil {
		return ""
	}
	stamp, ok := all[SnapshotCommitKey]
	if !ok {
		return ""
	}
	var commit string
	if json.Unmarshal(stamp, &commit) != nil {
		return ""
	}
	return commit
}
