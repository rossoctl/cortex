package ledger

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/cost/usage"
)

// A row written before Currency existed decodes as USD.
//
// THIS IS THE ONE TEST THAT PROTECTS THE WHOLE RETENTION WINDOW rather than the current process.
// The package's schema rule is "additive changes keep old files readable because an absent field
// decodes to zero; a RENAME would read every historical row as zero for that column. Add, never
// rename." Currency is additive, so every row already on disk — for as long as
// cost_ledger.retention_days keeps it — arrives with the field absent, and the only reading that
// does not silently relabel history is USD.
//
// ASSERTED ON BYTES THAT CARRY NO currency KEY, not on a Row with the field set to "". The round
// trip through the decoder IS the property; a hand-built struct skips the step that could break
// it, which is exactly the shape of a test that reads as coverage and cannot fail.
func TestRow_AHistoricalRowWithNoCurrencyDecodesAsUSD(t *testing.T) {
	// Byte-for-byte what a pre-Currency writer emitted: no currency key at all.
	const historical = `{"at":"2026-09-01T10:00:00Z","endpoint":"api.anthropic.com",` +
		`"model":"claude-opus-5","agent":"claude-code/2.1.270","provenance":"authoritative",` +
		`"requests":3,"tokens":1000,"costMicros":4170000}`

	var r Row
	if err := json.Unmarshal([]byte(historical), &r); err != nil {
		t.Fatalf("a historical row must stay readable: %v", err)
	}
	if got := r.currencyOrUSD(); got != pricing.CurrencyUSD {
		t.Errorf("historical row reads as %q, want %s — this relabels every retained day",
			got, pricing.CurrencyUSD)
	}
	// And the raw field stays empty rather than being rewritten on read: the file is the record,
	// and "" is the lossless spelling of "written before units existed". Same argument Agent's own
	// comment makes for storing absence as "" rather than as the display string.
	if r.Currency != "" {
		t.Errorf("Currency = %q, want empty; the decoder must not invent a value", r.Currency)
	}
}

// Currency is part of the accumulation key, so two units never fold into one row.
//
// THE WHOLE POINT. The ledger already buckets by (endpoint, model, agent, provenance); adding
// currency to that tuple is what makes "never sum credits into dollars" fall out of folding that
// already exists, rather than needing arithmetic that checks. Two rows identical but for the unit
// must stay two rows — if they merge, the merged CostMicros is a number with no meaning.
func TestRow_CurrencyIsPartOfTheKey(t *testing.T) {
	usd := Row{Endpoint: "gw", Model: "m", Agent: "a", Provenance: "configured", Currency: "USD"}
	credits := Row{Endpoint: "gw", Model: "m", Agent: "a", Provenance: "configured", Currency: "credits"}

	if usd.key() == credits.key() {
		t.Fatal("two currencies share one key; their costs would be summed into a figure that " +
			"is neither dollars nor credits")
	}
}

// A row with no currency and a row that says USD land in the SAME bucket.
//
// Otherwise the day a unit is first configured, every endpoint's history splits in two: rows
// written yesterday with the field absent, rows written today saying USD, identical in every
// other respect and reported as separate series. The key has to normalise, which is why it reads
// currencyOrUSD rather than the raw field.
func TestRow_LegacyAndExplicitUSDShareABucket(t *testing.T) {
	legacy := Row{Endpoint: "gw", Model: "m", Agent: "a", Provenance: "configured"}
	explicit := Row{Endpoint: "gw", Model: "m", Agent: "a", Provenance: "configured", Currency: "USD"}

	if legacy.key() != explicit.key() {
		t.Errorf("an absent currency keys apart from an explicit %s, so history splits the day a "+
			"unit is first configured", pricing.CurrencyUSD)
	}
}

// Currency is compared case-insensitively, so "credits" and "Credits" are one bucket.
//
// The config preserves an operator's spelling deliberately — they see back what they typed — which
// means two endpoints can legitimately arrive spelled differently for the same unit. Keying on the
// raw string would report those as two currencies and refuse to combine figures that belong
// together, which is the mirror image of the defect this field prevents.
func TestRow_CurrencyKeyIsCaseInsensitive(t *testing.T) {
	lower := Row{Endpoint: "gw", Model: "m", Currency: "credits"}
	upper := Row{Endpoint: "gw", Model: "m", Currency: "Credits"}

	if lower.key() != upper.key() {
		t.Error("two spellings of one unit key apart, so figures in the same currency would be " +
			"reported as two and never combined")
	}
}

// The overflow row coarsens every label and keeps the unit.
//
// Its key names the unit too, so two units past the cap are two overflow rows. One row for both
// would add credits to dollars, which Row.Currency exists to prevent.
func TestOverflow_KeepsTheUnit(t *testing.T) {
	r := overflow(Row{
		Endpoint: "gw", Model: "m", Agent: "a", Provenance: "configured", Currency: "credits",
	})
	if r.Currency != "credits" {
		t.Errorf("Currency = %q after overflow, want credits", r.Currency)
	}
	if r.key() != overflowKeyFor("", "credits") {
		t.Errorf("overflow row key = %+v, want %+v", r.key(), overflowKeyFor("", "credits"))
	}
	if overflowKeyFor("", "credits") == overflowKeyFor("", "usd") {
		t.Error("two units share one overflow key, so their figures would be summed into one row")
	}
}

// CurrenciesIn reports every distinct unit a window's rows carry, sorted.
//
// IT IS WHAT MAKES THE REFUSAL POSSIBLE. A total is only meaningful when the rows behind it share
// a unit, and nothing else on a snapshot can say whether they do: Counts is a flat sum by design,
// and the spec that introduced units is explicit that it must stay that way — "a unit belongs to
// the grouping key, not to the numbers". So the fact travels beside the numbers instead.
//
// NORMALISED AND DEDUPLICATED on the same rule key() uses, or the answer would disagree with the
// bucketing: a row with no currency and one saying USD are ONE unit, and "credits" and "Credits"
// are one unit spelled twice.
func TestCurrenciesIn_ReportsDistinctUnitsSorted(t *testing.T) {
	rows := []Row{
		{Endpoint: "bob", Currency: "credits", Counts: usage.Counts{PricedRequests: 1}},
		{Endpoint: "anthropic", Counts: usage.Counts{PricedRequests: 1}},                 // absent: USD
		{Endpoint: "litellm", Currency: "USD", Counts: usage.Counts{PricedRequests: 1}},  // explicit: the same USD
		{Endpoint: "bob2", Currency: "Credits", Counts: usage.Counts{PricedRequests: 1}}, // a second spelling of one unit
	}

	got := CurrenciesIn(rows)

	want := []string{"USD", "credits"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (sorted, so a caller's message is stable between reads)", got, want)
		}
	}
}

// One currency, or none at all, is reported as such — the single-unit answer must stay singular.
//
// This is the case every deployment is in today, and it is what keeps the refusal downstream from
// firing on traffic that is perfectly summable. An empty window reports nothing rather than
// inventing USD: there is no figure to label.
func TestCurrenciesIn_SingleUnitAndEmpty(t *testing.T) {
	if got := CurrenciesIn(nil); len(got) != 0 {
		t.Errorf("an empty window reported %v, want nothing", got)
	}
	rows := []Row{{Endpoint: "a", Counts: usage.Counts{PricedRequests: 1}}, {Endpoint: "b", Currency: "USD", Counts: usage.Counts{PricedRequests: 1}}}
	if got := CurrenciesIn(rows); len(got) != 1 || got[0] != "USD" {
		t.Errorf("a USD-only window reported %v, want [USD]", got)
	}
}

// group=currency is served by the ledger, and folds every row including the legacy ones.
//
// It is the axis the cross-unit refusal POINTS AT, so it has to work — `agentop cost` tells a reader
// to use --by currency when it withholds a combined total, and a key that does nothing is the
// failure this repo has been bitten by more than once.
//
// The legacy row is the case worth the fixture: written before Currency existed, it has to land in
// the USD bucket rather than a nameless one, or a per-currency table would not reconcile against
// the total printed beside it.
func TestFold_GroupCurrencySeparatesUnitsAndKeepsLegacyRowsInUSD(t *testing.T) {
	if !Groupable(usage.GroupCurrency) {
		t.Fatal("the ledger does not serve group=currency, which the cost command points readers at")
	}
	rows := []Row{
		{Endpoint: "bob", Currency: "credits", Counts: usage.Counts{Requests: 4, CostMicros: 77_400}},
		{Endpoint: "anthropic", Counts: usage.Counts{Requests: 6, CostMicros: 2_000_000}},
		{Endpoint: "litellm", Currency: "USD", Counts: usage.Counts{Requests: 1, CostMicros: 500_000}},
	}

	_, series, _, _ := Fold(rows, usage.GroupCurrency)

	if len(series) != 2 {
		t.Fatalf("got %d series, want 2 (USD and credits): %v", len(series), series)
	}
	// The legacy row and the explicit one share the USD bucket.
	if got := series[pricing.CurrencyUSD]; got.Requests != 7 || got.CostMicros != 2_500_000 {
		t.Errorf("USD = %+v, want Requests 7 and CostMicros 2500000 — the legacy row must fold in here",
			got)
	}
	if got := series["credits"]; got.Requests != 4 || got.CostMicros != 77_400 {
		t.Errorf("credits = %+v, want Requests 4 and CostMicros 77400", got)
	}
}

// A capped minute does not make a single-currency deployment refuse its own total.
//
// Asserted as the consequence: a capped USD row must not read as a second unit, or every
// deployment that has only billed in dollars withholds its headline the moment one minute
// overflows.
func TestCurrenciesIn_TheOverflowLabelIsNotABillingUnit(t *testing.T) {
	// The shape a capped minute actually produces: real rows, plus the one folded accumulator.
	rows := []Row{
		{Endpoint: "anthropic", Currency: "USD", Counts: usage.Counts{Requests: 1, PricedRequests: 1}},
		overflow(Row{Endpoint: "gw", Model: "m", Currency: "USD", Counts: usage.Counts{Requests: 9, PricedRequests: 9}}),
	}

	got := CurrenciesIn(rows)

	if len(got) != 1 || got[0] != pricing.CurrencyUSD {
		t.Fatalf("CurrenciesIn = %v, want [%s]: a capped minute must not read as a second unit, or "+
			"every deployment today withholds its total the moment one minute overflows",
			got, pricing.CurrencyUSD)
	}
	for _, c := range got {
		if c == overflowLabel {
			t.Errorf("%q is reported as a billing unit; normaliseUnit could never have accepted it",
				overflowLabel)
		}
	}
}

// A unit that appears only past the cap is still reported, and is not added to dollars.
//
// The case a capped minute's unit was lost in: 63 dollar rows fill the accumulator, and the one
// credits request after them lands in overflow. Exercised through Record rather than on hand-built
// rows, because it is foldLocked's key choice that decides whether the two units share a row.
func TestRecord_AUnitSeenOnlyPastTheCapIsReportedAndNotAddedToDollars(t *testing.T) {
	tbl, err := pricing.Build(&pricing.Config{Endpoints: []pricing.EndpointConfig{{
		Hosts: []string{"gw.bob"}, Unit: "credits",
		Models: map[string]pricing.ModelConfig{
			"premium-ide": {TierRates: pricing.TierRates{InputCostPerMillion: 2}},
		},
	}}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	now := at
	w, err := New(t.TempDir(), WithClock(func() time.Time { return now }), WithPricing(tbl))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	// maxLabelsPerMinute distinct dollar rows: the last one is already past the cap.
	for i := 0; i < maxLabelsPerMinute; i++ {
		w.Record("s1", costedEvent(t, "gw", fmt.Sprintf("model-%d", i), 0.25, 100, 50))
	}
	w.Record("s1", costedEvent(t, "gw.bob", "premium-ide", 0.10, 100, 50))

	held, _, _ := w.pending()
	// The bound foldLocked states: maxLabelsPerMinute, plus one for the second unit's overflow row.
	if len(held) != maxLabelsPerMinute+1 {
		t.Errorf("held %d rows, want %d: one overflow row per unit, beside maxLabelsPerMinute-1 real ones",
			len(held), maxLabelsPerMinute+1)
	}
	if got := CurrenciesIn(held); len(got) != 2 || got[0] != pricing.CurrencyUSD || got[1] != "credits" {
		t.Fatalf("CurrenciesIn = %v, want [USD credits]: the only credits traffic is in overflow, and a "+
			"window that cannot see it presents a dollar total with credits folded in", got)
	}
	var usd, credits int64
	for _, r := range held {
		switch r.currencyOrUSD() {
		case "credits":
			credits += r.CostMicros
			if r.Endpoint != overflowLabel {
				t.Errorf("the credits row is %q, want the overflow row: this fixture no longer reaches the cap", r.Endpoint)
			}
		default:
			usd += r.CostMicros
		}
	}
	if credits != 100_000 || usd != int64(maxLabelsPerMinute)*250_000 {
		t.Errorf("credits = %d, USD = %d micros; want 100000 and %d, each unit summed alone",
			credits, usd, int64(maxLabelsPerMinute)*250_000)
	}
}

// Two spellings of one unit are one group=currency series, under the spelling CurrenciesIn names.
//
// Otherwise the per-currency table shows "credits" and "Credits" as two rows while the headline
// counts one unit, and the table no longer reconciles with the units reported beside it.
func TestFold_GroupCurrencyFoldsSpellingsLikeCurrenciesIn(t *testing.T) {
	rows := []Row{
		{Endpoint: "bob", Currency: "credits", Counts: usage.Counts{Requests: 1, PricedRequests: 1, CostMicros: 100}},
		{Endpoint: "bob2", Currency: "Credits", Counts: usage.Counts{Requests: 2, PricedRequests: 2, CostMicros: 200}},
		{Endpoint: "anthropic", Counts: usage.Counts{Requests: 4, PricedRequests: 4, CostMicros: 400}},
	}

	_, series, _, _ := Fold(rows, usage.GroupCurrency)

	if len(series) != 2 {
		t.Fatalf("got %d series, want 2 (USD and credits): %v", len(series), series)
	}
	units := CurrenciesIn(rows)
	if len(units) != 2 {
		t.Fatalf("CurrenciesIn = %v, want 2 units; the loop below checks nothing otherwise", units)
	}
	for _, u := range units {
		if _, ok := series[u]; !ok {
			t.Errorf("CurrenciesIn names %q but no series is keyed by it: %v", u, series)
		}
	}
	if got := series["credits"]; got.Requests != 3 || got.CostMicros != 300 {
		t.Errorf("credits = %+v, want both spellings in one series (Requests 3, CostMicros 300)", got)
	}
}

// A charge for a model the gateway's block does not name is written in the gateway's unit.
//
// The bundled table prices this model on every host in dollars. Through a credits gateway it must
// not: a figure the gateway reports is in credits whichever row the model would have matched, and
// written as USD it is summed into the dollar total.
func TestRecord_AModelTheGatewaysBlockDoesNotNameIsWrittenInItsUnit(t *testing.T) {
	tbl, err := pricing.Build(&pricing.Config{Endpoints: []pricing.EndpointConfig{{
		Hosts: []string{"gw.bob"}, Unit: "credits",
		Models: map[string]pricing.ModelConfig{
			"premium-ide": {TierRates: pricing.TierRates{InputCostPerMillion: 2}},
		},
	}}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	const bundledModel = "claude-4-opus-20250514"
	if _, prov := tbl.Resolve("api.anthropic.com", bundledModel, 0); prov != pricing.ProvBundled {
		t.Fatalf("%s is not a bundled model any more (prov %v), so this pins nothing", bundledModel, prov)
	}
	now := at
	dir := t.TempDir()
	w, err := New(dir, WithClock(func() time.Time { return now }), WithPricing(tbl))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	w.Record("s1", costedEvent(t, "gw.bob", bundledModel, 0.25, 100, 50))
	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	rows := readAllRows(t, dir)
	if len(rows) != 1 || rows[0].Currency != "credits" {
		t.Fatalf("rows = %+v, want one row in credits", rows)
	}
}

// The configured unit reaches the row on disk, and the default writes no field at all.
//
// THE PRODUCER SIDE OF THE WHOLE FEATURE, end to end through the seam the PR body says needed no
// threading: config -> Build -> CurrencyFor -> Record -> the bytes -> decode. Nothing covered it,
// so deleting the writer's entire unit-write block left the suite green, and so did dropping its
// not-the-default condition.
//
// ASSERTED ON THE BYTES, not only on the decoded Row, because both halves of the claim live there:
// a credits deployment must find "credits" in the file, and a dollars deployment must produce a
// file with no currency key in it — which is what "a single-currency deployment produces
// byte-identical files" means and the only form of that claim a test can check.
func TestRecord_ConfiguredUnitReachesTheRowOnDisk(t *testing.T) {
	for _, tc := range []struct {
		name string
		unit string // as written in pricing.endpoints[].unit
		// wantOnDisk is the currency value the decoded row carries, "" when the field is absent.
		wantOnDisk     string
		wantKeyInBytes bool
	}{
		{"absent means USD and writes nothing", "", "", false},
		{"explicit USD writes nothing either", "USD", "", false},
		// The spelling four consumers used to read as a non-default unit. normaliseUnit
		// canonicalises it, so it must be as silent on disk as "USD" is.
		{"a lowercase usd is still the default", "usd", "", false},
		{"a gateway billing in credits", "credits", "credits", true},
		{"an operator's own spelling is preserved", "Bobcoins", "Bobcoins", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tbl, err := pricing.Build(&pricing.Config{Endpoints: []pricing.EndpointConfig{{
				Hosts: []string{"gw"},
				Unit:  tc.unit,
				Models: map[string]pricing.ModelConfig{
					"m": {TierRates: pricing.TierRates{InputCostPerMillion: 2}},
				},
			}}})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}

			dir := t.TempDir()
			now := at
			w, err := New(dir, WithClock(func() time.Time { return now }), WithPricing(tbl))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = w.Close() })

			w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
			now = at.Add(time.Minute)
			if err := w.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}

			if got := string(readAllBytes(t, dir)); strings.Contains(got, `"currency"`) != tc.wantKeyInBytes {
				t.Errorf("currency key present on disk = %v, want %v; line was:\n%s",
					!tc.wantKeyInBytes, tc.wantKeyInBytes, got)
			}
			rows := readAllRows(t, dir)
			if len(rows) != 1 {
				t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
			}
			if rows[0].Currency != tc.wantOnDisk {
				t.Errorf("Row.Currency on disk = %q, want %q", rows[0].Currency, tc.wantOnDisk)
			}
			// And the row reads back as the unit a reader will label its figure with, which is the
			// answer the refusal is computed from — the default spelled out, never empty.
			wantRead := tc.wantOnDisk
			if wantRead == "" {
				wantRead = pricing.CurrencyUSD
			}
			if got := rows[0].currencyOrUSD(); got != wantRead {
				t.Errorf("currencyOrUSD() = %q, want %q", got, wantRead)
			}
		})
	}
}

// A gateway-reported charge whose model could not be read lands on disk in the gateway's unit.
//
// THE CONSEQUENCE, at the layer where it is a wrong figure rather than a wrong lookup. bestRow needs
// the host AND the model to match, so a model-less response found no row and CurrencyFor answered
// USD; the writer omits the field for USD, so a credits charge reached the file with no unit and the
// read side folded it into the dollar total. That is a cross-unit sum — the one thing Row.Currency
// exists to prevent — and it arrived through the only path that can carry a figure without a rate:
// a cost the gateway reported, which settle publishes without consulting the table.
//
// ASSERTED ON THE BYTES AND ON CurrenciesIn, not on CurrencyFor. The pricing-layer lookup is pinned
// in that package; what this adds is that the fix survives the writer's omit-the-default rule, which
// is the step that turned a wrong lookup into a wrong file.
func TestRecord_AModellessChargeOnACreditsGatewayIsNotWrittenAsUSD(t *testing.T) {
	tbl, err := pricing.Build(&pricing.Config{Endpoints: []pricing.EndpointConfig{{
		Hosts: []string{"gw.bob"},
		Unit:  "credits",
		// A concrete pattern, so nothing here matches an empty model.
		Models: map[string]pricing.ModelConfig{
			"premium-ide": {TierRates: pricing.TierRates{InputCostPerMillion: 2}},
		},
	}}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	dir := t.TempDir()
	now := at
	w, err := New(dir, WithClock(func() time.Time { return now }), WithPricing(tbl))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	// Model "" is the response the parser could not read — an embeddings call, say.
	w.Record("s1", costedEvent(t, "gw.bob", "", 0.25, 100, 50))
	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	if rows[0].Currency != "credits" {
		t.Errorf("Row.Currency = %q, want credits; a credits charge with no unit on disk reads as "+
			"USD and is summed into the dollar total", rows[0].Currency)
	}
	// And the refusal downstream can see it, which is what the field is for.
	if got := CurrenciesIn(rows); len(got) != 1 || got[0] != "credits" {
		t.Errorf("CurrenciesIn = %v, want [credits]", got)
	}
}

// group=agent names each agent's own units, spelled as CurrenciesIn spells them, so a scoped figure
// can be labelled without the window's mixture. Every other grouping gets nothing.
func TestSeriesCurrenciesIn_NamesEachAgentsUnits(t *testing.T) {
	rows := []Row{
		{Endpoint: "bob", Agent: "bob-shell/2.0.5", Currency: "credits", Counts: usage.Counts{Requests: 4, PricedRequests: 4}},
		{Endpoint: "bob", Agent: "bob-shell/2.0.5", Currency: "Credits", Counts: usage.Counts{Requests: 2, PricedRequests: 2}},
		{Endpoint: "anthropic", Agent: "claude-code/2.1.284", Counts: usage.Counts{Requests: 6, PricedRequests: 6}},
		{Endpoint: "litellm", Agent: "claude-code/2.1.284", Currency: "usd", Counts: usage.Counts{Requests: 1, PricedRequests: 1}},
	}
	got := SeriesCurrenciesIn(rows, usage.GroupAgent, foldSeries(rows, usage.GroupAgent))
	// Keyed by the folded name: labelFor reads these rows through pipeline.AgentName,
	// so the pre-convergence "bob-shell/2.0.5" labels above fold to "bob" rather than
	// forming a row of their own beside it.
	if want := []string{"credits"}; !slices.Equal(got["bob"], want) {
		t.Errorf("bob = %v, want %v", got["bob"], want)
	}
	if want := []string{pricing.CurrencyUSD}; !slices.Equal(got["claude-code"], want) {
		t.Errorf("claude-code = %v, want %v; a legacy row and an explicit usd one are one unit",
			got["claude-code"], want)
	}
	if got := SeriesCurrenciesIn(rows, usage.GroupEndpoint, foldSeries(rows, usage.GroupEndpoint))["bob"]; !slices.Equal(got, []string{"credits"}) {
		t.Errorf("endpoint bob = %v, want [credits]; the drawer's endpoint axis labels by this", got)
	}
	if other := SeriesCurrenciesIn(rows, usage.GroupStatus, foldSeries(rows, usage.GroupStatus)); other != nil {
		t.Errorf("group=status got %v, want nothing: it is defined for the drawer's axes only", other)
	}
}

// Model names come from requests, so SeriesCurrencies must be bounded by the same cap as the series
// it labels: a key per series on the wire, with a capped-away label's units on the overflow band.
func TestSeriesCurrenciesIn_IsBoundedLikeTheSeries(t *testing.T) {
	var rows []Row
	for i := range usage.MaxSeriesInResponse + 4 {
		rows = append(rows, Row{Endpoint: "gw", Model: fmt.Sprintf("m%02d", i),
			Counts: usage.Counts{Requests: 1, PricedRequests: 1, CostMicros: int64(1000 - i)}})
	}
	rows = append(rows, Row{Endpoint: "bob", Model: "tiny", Currency: "credits", Counts: usage.Counts{Requests: 1, PricedRequests: 1, CostMicros: 1}},
		// A client can spell the overflow label itself; it must not hide the band's real contents.
		Row{Endpoint: "gw", Model: overflowLabel, Counts: usage.Counts{Requests: 1, PricedRequests: 1, CostMicros: 5000}})
	_, series, _, _ := Fold(rows, usage.GroupModel)
	got := SeriesCurrenciesIn(rows, usage.GroupModel, series)
	for label := range got {
		if _, ok := series[label]; !ok {
			t.Errorf("SeriesCurrencies names %q, which is not a series on the wire", label)
		}
	}
	if !slices.Contains(got[overflowLabel], "credits") {
		t.Errorf("overflow units = %v, want credits among them: the capped-away row is in it", got[overflowLabel])
	}
}

func foldSeries(rows []Row, g usage.Group) map[string]usage.Counts {
	_, series, _, _ := Fold(rows, g)
	return series
}

// A unit is named only where a row shows a figure: priced, or carrying a saving. An unpriced
// row beside another unit's spend is no mixture — there is nothing of it to add.
func TestCurrenciesIn_NamesOnlyUnitsWithAFigure(t *testing.T) {
	rows := []Row{
		{Endpoint: "bob", Currency: "credits", Counts: usage.Counts{Requests: 2, PricedRequests: 2, CostMicros: 30}},
		{Endpoint: "ollama", Counts: usage.Counts{Requests: 5}},
	}
	if got := CurrenciesIn(rows); !slices.Equal(got, []string{"credits"}) {
		t.Errorf("CurrenciesIn = %v, want [credits]: the USD row priced nothing", got)
	}
	rows[1].AvoidedMicros = 400
	if got := CurrenciesIn(rows); !slices.Equal(got, []string{pricing.CurrencyUSD, "credits"}) {
		t.Errorf("CurrenciesIn = %v, want [USD credits]: a saving is a figure", got)
	}
}
