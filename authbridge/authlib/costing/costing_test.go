package costing

import (
	"net/http"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
)

// rates builds a table charging 1 micro-dollar per token in every tier, so a token count
// reads directly as micros and an expectation needs no arithmetic to check.
func rates(t *testing.T) pricing.Resolver {
	t.Helper()
	var r pricing.Rates
	for _, tier := range []pricing.Tier{pricing.TierInput, pricing.TierCacheWrite, pricing.TierCacheRead, pricing.TierOutput} {
		r.Base[tier], r.Set[tier] = 1e-6, true
	}
	tab, err := pricing.NewTable([]pricing.Entry{{Host: "*", Model: "*", Rates: r, Prov: pricing.ProvConfigured}})
	if err != nil {
		t.Fatal(err)
	}
	return pricing.NewRegistry(tab)
}

func ctx(headers map[string]string, input, output int) *pipeline.Context {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &pipeline.Context{
		Host:            "gw.internal",
		ResponseHeaders: h,
		Extensions: pipeline.Extensions{Inference: &pipeline.InferenceExtension{
			Model:        "claude-opus-5",
			InputTokens:  input,
			OutputTokens: output,
		}},
	}
}

// The precedence rule, which is the reason this package exists: it used to be implemented
// twice, in two shapes, and the two could disagree about the same request.
func TestSettle_Precedence(t *testing.T) {
	const json = "application/json"
	for _, tc := range []struct {
		name     string
		headers  map[string]string
		wantCost float64
		wantSrc  string
		wantProv pricing.Provenance
		priced   bool
		free     bool
	}{{
		name:     "gateway figure wins outright",
		headers:  map[string]string{"Content-Type": json, ResponseCostHeader: "0.25"},
		wantCost: 0.25,
		wantSrc:  costevent.SourceGatewayHeader,
		wantProv: pricing.ProvAuthoritative,
		priced:   true,
	}, {
		name:     "the -original fallback is the charged cost, not a list price",
		headers:  map[string]string{"Content-Type": json, ResponseCostOriginalHeader: "0.25"},
		wantCost: 0.25,
		wantSrc:  costevent.SourceGatewayHeader,
		wantProv: pricing.ProvAuthoritative,
		priced:   true,
	}, {
		// 1000 + 500 tokens at 1e-6 each.
		name:     "no header at all falls back to the table",
		headers:  map[string]string{"Content-Type": json},
		wantCost: 0.0015,
		wantSrc:  costevent.SourceUsageFallback,
		wantProv: pricing.ProvConfigured,
		priced:   true,
	}, {
		// The zero LiteLLM stamps on every stream is a placeholder, not an answer.
		name:     "a stream's zero falls back like an absent header",
		headers:  map[string]string{"Content-Type": "text/event-stream", ResponseCostHeader: "0"},
		wantCost: 0.0015,
		wantSrc:  costevent.SourceUsageFallback,
		wantProv: pricing.ProvConfigured,
		priced:   true,
	}, {
		// A zero on a NON-streamed response is the gateway saying "free". Pricing it
		// from the usage block would bill a cache hit.
		name:     "a declared zero is an answer, and suppresses the fallback",
		headers:  map[string]string{"Content-Type": json, ResponseCostHeader: "0"},
		wantCost: 0,
		wantSrc:  costevent.SourceGatewayHeader,
		wantProv: pricing.ProvAuthoritative,
		priced:   true,
		free:     true,
	}, {
		// Garbage is not a figure and not a declaration of free either.
		name:     "an unusable header falls back to the table",
		headers:  map[string]string{"Content-Type": json, ResponseCostHeader: "abc"},
		wantCost: 0.0015,
		wantSrc:  costevent.SourceUsageFallback,
		wantProv: pricing.ProvConfigured,
		priced:   true,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := Settle(ctx(tc.headers, 1000, 500), rates(t))
			if got.Priced != tc.priced {
				t.Fatalf("Priced = %v, want %v", got.Priced, tc.priced)
			}
			if got.CostUSD != tc.wantCost {
				t.Errorf("CostUSD = %v, want %v", got.CostUSD, tc.wantCost)
			}
			if got.Source != tc.wantSrc {
				t.Errorf("Source = %q, want %q", got.Source, tc.wantSrc)
			}
			if got.Provenance != tc.wantProv {
				t.Errorf("Provenance = %v, want %v", got.Provenance, tc.wantProv)
			}
			if got.DeclaredFree != tc.free {
				t.Errorf("DeclaredFree = %v, want %v", got.DeclaredFree, tc.free)
			}
		})
	}
}

// Both figures are carried even when one wins, because the drift check compares the pair
// that was actually decided rather than recomputing a half of it.
func TestSettle_CarriesBothFigures(t *testing.T) {
	got := Settle(ctx(map[string]string{"Content-Type": "application/json", ResponseCostHeader: "0.25"}, 1000, 500), rates(t))
	if !got.HasReported || got.ReportedUSD != 0.25 {
		t.Errorf("reported = %v (has=%v), want 0.25", got.ReportedUSD, got.HasReported)
	}
	if !got.HasModelled || got.ModelledUSD != 0.0015 {
		t.Errorf("modelled = %v (has=%v), want 0.0015", got.ModelledUSD, got.HasModelled)
	}
	if got.ModelledProv != pricing.ProvConfigured {
		t.Errorf("ModelledProv = %v, want configured", got.ModelledProv)
	}
}

// A binary with no pricing wired must report unpriced, not panic. The trap is that the
// resolver is an INTERFACE: a nil one panics on call where a nil *Registry would not.
func TestSettle_NilResolverIsUnpriced(t *testing.T) {
	got := Settle(ctx(map[string]string{"Content-Type": "application/json"}, 1000, 500), nil)
	if got.Priced {
		t.Errorf("priced with no rate table: %+v", got)
	}
	if got.HasModelled {
		t.Error("claims a modelled figure with no resolver")
	}
	// A gateway figure still lands, because reading a header needs no rate table.
	withHeader := Settle(ctx(map[string]string{"Content-Type": "application/json", ResponseCostHeader: "0.25"}, 0, 0), nil)
	if !withHeader.Priced || withHeader.CostUSD != 0.25 {
		t.Errorf("gateway figure lost when no rates are configured: %+v", withHeader)
	}
}

// No inference at all is not a pricing gap: a plain proxied request has no model and no
// tokens, and inventing a figure for it would be worse than reporting none.
func TestSettle_NoInferenceIsSilent(t *testing.T) {
	pctx := &pipeline.Context{Host: "gw.internal", ResponseHeaders: http.Header{}}
	if got := Settle(pctx, rates(t)); got.Priced {
		t.Errorf("priced a request with no inference: %+v", got)
	}
	if got := Settle(nil, rates(t)); got.Priced {
		t.Error("priced a nil context")
	}
}

// Comparable is what keeps drift from blaming the rate table for a gateway's own
// discount layer: when the effective header is absent and the gateway reports an
// adjustment, the fallback figure is pre-adjustment and the two are not like for like.
func TestComparable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{"effective header present", map[string]string{ResponseCostHeader: "0.25"}, true},
		{"fallback with no adjustment", map[string]string{ResponseCostOriginalHeader: "0.25", DiscountAmountHeader: "0.0"}, true},
		{"fallback with a discount", map[string]string{ResponseCostOriginalHeader: "0.25", DiscountAmountHeader: "0.05"}, false},
		{"fallback with a margin", map[string]string{ResponseCostOriginalHeader: "0.25", MarginAmountHeader: "0.05"}, false},
		{"no headers", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Comparable(ctx(tc.headers, 0, 0)); got != tc.want {
				t.Errorf("Comparable = %v, want %v", got, tc.want)
			}
		})
	}
}

// Publish writes both keys: abctl is a separate binary that can lag the proxy, and an
// older one reads only the legacy key.
func TestPublish_WritesBothKeys(t *testing.T) {
	pctx := ctx(nil, 0, 0)
	Publish(pctx, costevent.Event{CostUSD: 0.25, Settled: true})
	for _, key := range []string{costevent.Key, costevent.PluginName} {
		if _, ok := pctx.Extensions.Custom[key+pipeline.PluginEventSuffix]; !ok {
			t.Errorf("nothing published under %q", key)
		}
	}
}

// Amend is how a budget adds the fields it owns without becoming the record's author.
func TestAmend(t *testing.T) {
	pctx := ctx(nil, 0, 0)
	if Amend(pctx, func(*costevent.Event) { t.Error("amended a record that was never published") }) {
		t.Error("Amend reported success with nothing published")
	}

	Publish(pctx, costevent.Event{CostUSD: 0.25, Settled: true})
	if !Amend(pctx, func(ev *costevent.Event) { ev.DailyTotalUSD = 9 }) {
		t.Fatal("Amend failed on a published record")
	}
	// Both keys must carry the amendment, or an older abctl shows a stale total.
	for _, key := range []string{costevent.Key, costevent.PluginName} {
		ev, ok := pctx.Extensions.Custom[key+pipeline.PluginEventSuffix].(costevent.Event)
		if !ok {
			t.Fatalf("%q missing after amend", key)
		}
		if ev.DailyTotalUSD != 9 || ev.CostUSD != 0.25 {
			t.Errorf("%q = %+v, want the amendment applied and the cost preserved", key, ev)
		}
	}
}

// Store/Load hand the full outcome to later plugins in the same request without widening
// the wire shape.
func TestStoreLoad(t *testing.T) {
	pctx := ctx(nil, 0, 0)
	if _, ok := Load(pctx); ok {
		t.Error("loaded an outcome that was never stored")
	}
	Store(pctx, Settled{CostUSD: 0.25, Priced: true, ModelledUSD: 0.3, HasModelled: true})
	got, ok := Load(pctx)
	if !ok {
		t.Fatal("stored outcome not found")
	}
	if got.CostUSD != 0.25 || got.ModelledUSD != 0.3 {
		t.Errorf("round trip lost data: %+v", got)
	}
}
