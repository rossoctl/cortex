package ledger

import (
	"context"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/cost/usage"
)

// flatResolver prices every tier at a flat per-token rate, with no context threshold.
//
// Mirrors tierResolver in core/cost/usage's own tier tests rather than inventing a second
// fixture shape: the four rates differ from each other so a test can tell which tier a
// figure came from, and none is a round multiple of another so a transposed pair shows up as
// a wrong number instead of a coincidentally right one.
func flatResolver(t *testing.T, model string) pricing.Resolver {
	t.Helper()
	var r pricing.Rates
	for tier, perToken := range map[pricing.Tier]float64{
		pricing.TierInput:      7.0 / 1e6,
		pricing.TierCacheWrite: 11.0 / 1e6,
		pricing.TierCacheRead:  3.0 / 1e6,
		pricing.TierOutput:     23.0 / 1e6,
	} {
		r.Base[tier], r.Set[tier] = perToken, true
	}
	return newResolver(t, model, r)
}

// newResolver wraps one entry in a table and a registry.
func newResolver(t *testing.T, model string, r pricing.Rates) pricing.Resolver {
	t.Helper()
	tab, err := pricing.NewTable([]pricing.Entry{
		{Host: "*", Model: model, Rates: r, Prov: pricing.ProvConfigured},
	})
	if err != nil {
		t.Fatal(err)
	}
	return pricing.NewRegistry(tab)
}

// tokenRow is a ledger row as it exists on disk today: tokens and a total, no split.
func tokenRow(requests, in, cacheWrite, cacheRead, out, micros int64) Row {
	return Row{
		At: at.Truncate(time.Minute), Endpoint: "gw", Model: "m", Provenance: "bundled",
		Counts: usage.Counts{
			Requests: requests, PricedRequests: requests, PriceableRequests: requests,
			InputTokens: in, CacheWriteTokens: cacheWrite, CacheReadTokens: cacheRead,
			OutputTokens: out, Tokens: in + cacheWrite + cacheRead + out,
			CostMicros: micros,
		},
	}
}

// TestRepriceTiers_FillsTheSplitFromPersistedTokens is what recovers history.
//
// Every row written before the split was persisted has the four token counters and no
// tier costs, so the mix can be rebuilt from what is already on disk. Without this, a day
// recorded by an older binary can never show a breakdown however long the fix has been
// running — and with a seven-day retention that is a week of blank panels for spend the
// ledger can otherwise account for exactly.
func TestRepriceTiers_FillsTheSplitFromPersistedTokens(t *testing.T) {
	rows := []Row{tokenRow(1, 1_000, 2_000, 4_000, 500, 41_500)}

	repriceTiers(rows, flatResolver(t, "m"))

	r := rows[0]
	for _, c := range []struct {
		name string
		got  int64
		want int64
	}{
		// perToken x tokens, in micros: 7x1000, 11x2000, 3x4000, 23x500.
		{"InputCostMicros", r.InputCostMicros, 7_000},
		{"CacheWriteCostMicros", r.CacheWriteCostMicros, 22_000},
		{"CacheReadCostMicros", r.CacheReadCostMicros, 12_000},
		{"OutputCostMicros", r.OutputCostMicros, 11_500},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	// THE CONSUMER, not just the fields: the display asks ApportionTiers, and a mix it
	// refuses is indistinguishable from no mix at all.
	tiers, ok := r.ApportionTiers()
	if !ok {
		t.Fatal("ApportionTiers refused the repriced split; the display stays blank")
	}
	// Apportioned against the row's OWN authoritative total, which is the contract: the mix
	// is the rate table's and the magnitude is the gateway's, so these sum to CostMicros and
	// not to the modelled figures above.
	var sum int64
	for _, v := range tiers {
		sum += v
	}
	if sum != r.CostMicros {
		t.Errorf("apportioned tiers sum to %d, want the row's own total %d", sum, r.CostMicros)
	}
}

// TestRepriceTiers_LeavesAPersistedSplitAlone protects the better figure.
//
// A split that Writer.Record persisted was computed by the producer at the request's OWN
// prompt total, under the rates in force at the time. This function's figure is neither: it
// resolves at a per-minute mean and against today's table. So a row that already has one
// must be left exactly as it is, or the fix for old rows would quietly degrade new ones.
func TestRepriceTiers_LeavesAPersistedSplitAlone(t *testing.T) {
	row := tokenRow(1, 1_000, 2_000, 4_000, 500, 41_500)
	// Deliberately inconsistent with the tokens and with the rates: only an untouched copy
	// can still read this way afterwards.
	row.InputCostMicros, row.CacheWriteCostMicros = 111, 222
	row.CacheReadCostMicros, row.OutputCostMicros = 333, 444
	rows := []Row{row}

	repriceTiers(rows, flatResolver(t, "m"))

	if rows[0].Counts != row.Counts {
		t.Errorf("repriced a row that already had a split:\n got %+v\nwant %+v",
			rows[0].Counts, row.Counts)
	}
}

// TestRepriceTiers_APartialSplitCountsAsPersisted is the boundary of the test above.
//
// A request that used exactly one tier publishes a split with three zeros in it, which is a
// REAL split rather than an absent one — see the mix's own doc on absent-from-the-mix meaning
// absent-from-the-answer. Keying the skip on "any of the four is set" rather than on all four
// keeps that row alone; keying it on all four would overwrite a true one-tier mix with a
// modelled four-tier one.
func TestRepriceTiers_APartialSplitCountsAsPersisted(t *testing.T) {
	row := tokenRow(1, 1_000, 0, 0, 500, 41_500)
	row.InputCostMicros = 7_000
	rows := []Row{row}

	repriceTiers(rows, flatResolver(t, "m"))

	if rows[0].OutputCostMicros != 0 {
		t.Errorf("OutputCostMicros = %d, want 0: a one-tier split is a split, not an absence",
			rows[0].OutputCostMicros)
	}
	if rows[0].InputCostMicros != 7_000 {
		t.Errorf("InputCostMicros = %d, want the persisted 7000", rows[0].InputCostMicros)
	}
}

// TestRepriceTiers_ResolvesAtThePerRequestPromptTotal is the trap this function is most
// likely to be written into.
//
// A Row is ONE MINUTE'S TOTALS for one key, so its token counters are already summed over
// Requests requests. A long-context premium is a property of a SINGLE request's prompt, so
// handing the summed figure to Resolve applies the premium to traffic where no individual
// request came close to the threshold — and it gets worse the busier the minute, which is
// exactly when the numbers are being looked at.
//
// Four requests of 50k against a threshold at 100k: correct pricing never leaves the base
// rate, while resolving at the sum (200k) applies a 10x premium. The two answers differ by a
// factor of ten, so this cannot pass by luck.
func TestRepriceTiers_ResolvesAtThePerRequestPromptTotal(t *testing.T) {
	var r pricing.Rates
	r.Base[pricing.TierInput], r.Set[pricing.TierInput] = 1.0/1e6, true
	r.Base[pricing.TierOutput], r.Set[pricing.TierOutput] = 1.0/1e6, true
	r.Thresholds = []pricing.ContextThreshold{{
		AbovePromptTokens: 100_000,
		Rate:              [pricing.NumTiers]float64{pricing.TierInput: 10.0 / 1e6},
		Set:               [pricing.NumTiers]bool{pricing.TierInput: true},
	}}
	rows := []Row{tokenRow(4, 200_000, 0, 0, 0, 200_000)}

	repriceTiers(rows, newResolver(t, "m", r))

	// 4 x (50_000 tokens at $1/Mtok) = 200_000 micros. At the premium it would be 2_000_000.
	if got := rows[0].InputCostMicros; got != 200_000 {
		t.Errorf("InputCostMicros = %d, want 200000 (2000000 in particular means the "+
			"long-context premium was applied to a summed prompt no single request reached)", got)
	}
}

// TestRepriceTiers_ABusyMinuteStaysPlausible is the second half of the same trap.
//
// pricing.CostByTier refuses a usage past MaxPlausibleTokens, which is 10M PER FIELD and a
// bound on ONE request. Twelve 1M-context calls in a minute sum to 12M input tokens, so a
// summed usage is refused outright — and the failure is silent: ok=false leaves the split
// empty and the panel blank, which is indistinguishable from the bug this whole change fixes.
// Pricing the per-request mean keeps the guard measuring what it was written to measure.
func TestRepriceTiers_ABusyMinuteStaysPlausible(t *testing.T) {
	rows := []Row{tokenRow(12, 12_000_000, 0, 0, 12_000, 84_000_000)}

	repriceTiers(rows, flatResolver(t, "m"))

	if rows[0].InputCostMicros == 0 {
		t.Fatal("a minute summing past MaxPlausibleTokens was refused; " +
			"the per-request mean is what the guard bounds")
	}
	// 12 x (1_000_000 tokens at $7/Mtok) = 84_000_000 micros.
	if got := rows[0].InputCostMicros; got != 84_000_000 {
		t.Errorf("InputCostMicros = %d, want 84000000", got)
	}
}

// TestRepriceTiers_ScalesWithRequests checks the mean is put back.
//
// The split has to be the minute's total spend per tier, not one request's, because Fold
// sums these columns across rows and windows. Pricing the mean and forgetting to multiply
// would under-report a busy minute by a factor of Requests — and since the consumer uses the
// four as a RATIO, the error would be invisible on a single-key window and wrong only where
// two keys have different request counts, which is the hardest kind of wrong to notice.
func TestRepriceTiers_ScalesWithRequests(t *testing.T) {
	one := []Row{tokenRow(1, 1_000, 0, 0, 0, 7_000)}
	ten := []Row{tokenRow(10, 10_000, 0, 0, 0, 70_000)}

	res := flatResolver(t, "m")
	repriceTiers(one, res)
	repriceTiers(ten, res)

	if got, want := ten[0].InputCostMicros, one[0].InputCostMicros*10; got != want {
		t.Errorf("InputCostMicros = %d for ten requests, want %d (10x the single-request row)",
			got, want)
	}
}

// TestRepriceTiers_SkipsWhatItCannotPrice enumerates the refusals.
//
// Each of these leaves the split empty rather than guessing, and empty is a state the
// consumer already renders as "not known here". The zero-Requests case is the one with teeth:
// the mean is a division by Requests, so without the guard this panics on a row that a
// hand-edited or truncated day file can perfectly well contain.
func TestRepriceTiers_SkipsWhatItCannotPrice(t *testing.T) {
	noModel := tokenRow(1, 1_000, 0, 0, 500, 41_500)
	noModel.Model = ""
	noRequests := tokenRow(0, 1_000, 0, 0, 500, 41_500)
	noTokens := tokenRow(1, 0, 0, 0, 0, 41_500)
	unknownModel := tokenRow(1, 1_000, 0, 0, 500, 41_500)
	unknownModel.Model = "not-in-the-table"

	for _, c := range []struct {
		name string
		row  Row
	}{
		{"no model to resolve against", noModel},
		{"no requests to take a mean over", noRequests},
		{"no tokens to apportion", noTokens},
		{"no rate for this model", unknownModel},
	} {
		rows := []Row{c.row}
		repriceTiers(rows, flatResolver(t, "m"))
		r := rows[0]
		if r.InputCostMicros|r.CacheWriteCostMicros|r.CacheReadCostMicros|r.OutputCostMicros != 0 {
			t.Errorf("%s: split = %d/%d/%d/%d, want all zero", c.name,
				r.InputCostMicros, r.CacheWriteCostMicros, r.CacheReadCostMicros, r.OutputCostMicros)
		}
		// The row's own spend is untouched either way: a mix we cannot model is not a reason
		// to lose dollars the ledger recorded.
		if r.CostMicros != c.row.CostMicros {
			t.Errorf("%s: CostMicros = %d, want %d unchanged", c.name, r.CostMicros, c.row.CostMicros)
		}
	}
}

// TestRepriceTiers_NoResolverIsANoOp keeps the ledger usable without a rate table.
//
// Writer.New takes the resolver as an option, so a deployment that wires none — and every
// existing test in this package — must behave exactly as before rather than panic on a nil
// interface.
func TestRepriceTiers_NoResolverIsANoOp(t *testing.T) {
	rows := []Row{tokenRow(1, 1_000, 2_000, 4_000, 500, 41_500)}
	repriceTiers(rows, nil)
	if rows[0].InputCostMicros != 0 {
		t.Errorf("InputCostMicros = %d, want 0 with no resolver", rows[0].InputCostMicros)
	}
}

// TestQuery_ServesARepricedSplit is the wiring, end to end over a real day file.
//
// The unit tests above prove the arithmetic; this proves it is reached. Query is the single
// point every row read from disk passes through — Window delegates to it and then appends
// only the unflushed minute, which Record has already split — so a call site anywhere else
// would leave the surfaces that actually matter blank.
func TestQuery_ServesARepricedSplit(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	// line() writes tokens and a total and no split: a row from before the fix, byte for byte.
	writeDay(t, dir, base, line(base, "gw", "m", 2, 2_000, 1_000, 37_000))

	w, err := New(dir, WithClock(func() time.Time { return base }),
		WithPricing(flatResolver(t, "m")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	rows, _, err := w.Query(context.Background(), base, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	// 2 x (1000 input at $7/Mtok) and 2 x (500 output at $23/Mtok).
	if got := rows[0].InputCostMicros; got != 14_000 {
		t.Errorf("InputCostMicros = %d, want 14000", got)
	}
	if got := rows[0].OutputCostMicros; got != 23_000 {
		t.Errorf("OutputCostMicros = %d, want 23000", got)
	}
	if _, ok := rows[0].ApportionTiers(); !ok {
		t.Error("ApportionTiers refused a row served from Query; the drawer stays blank")
	}
}

// TestQuery_LeavesTheSplitAloneWithNoResolver is the same path with nothing wired.
func TestQuery_LeavesTheSplitAloneWithNoResolver(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	writeDay(t, dir, base, line(base, "gw", "m", 2, 2_000, 1_000, 37_000))
	w := newTestWriter(t, dir, func() time.Time { return base })

	rows, _, err := w.Query(context.Background(), base, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].InputCostMicros != 0 {
		t.Errorf("InputCostMicros = %d, want 0 with no resolver wired", rows[0].InputCostMicros)
	}
	// The total still arrives, which is the property that keeps a ledger useful in a
	// deployment that prices nothing locally.
	if rows[0].CostMicros != 37_000 {
		t.Errorf("CostMicros = %d, want 37000", rows[0].CostMicros)
	}
}
