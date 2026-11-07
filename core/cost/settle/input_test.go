package settle

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/pipeline"
)

// headers builds a response header set from name and value pairs.
func headers(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

// plainInference is the usage of the ctx fixture: 1,000 input and 500 output tokens.
func plainInference() *pipeline.InferenceExtension {
	return &pipeline.InferenceExtension{Model: "claude-opus-5", InputTokens: 1000, OutputTokens: 500}
}

// SettleInput prices a request from plain values, with no plugin context.
//
// Hand-built cases. The header names and their semantics follow LiteLLM 1.85.5, as the
// measurement on the header constants in settle.go records.
func TestSettleInput_PricesPlainValues(t *testing.T) {
	const json = "application/json"
	for _, tc := range []struct {
		name     string
		in       Input
		wantCost float64
		wantSrc  string
		priced   bool
		free     bool
		rejected string
	}{{
		name:     "the bare cost header wins",
		in:       Input{Host: "gw.internal", Inference: plainInference(), ResponseHeaders: headers("Content-Type", json, ResponseCostHeader, "0.25")},
		wantCost: 0.25, wantSrc: event.SourceGatewayHeader, priced: true,
	}, {
		name:     "the -Original header is the fallback",
		in:       Input{Host: "gw.internal", Inference: plainInference(), ResponseHeaders: headers("Content-Type", json, ResponseCostOriginalHeader, "0.25")},
		wantCost: 0.25, wantSrc: event.SourceGatewayHeader, priced: true,
	}, {
		name:     "a zero header on JSON declares the call free",
		in:       Input{Host: "gw.internal", Inference: plainInference(), ResponseHeaders: headers("Content-Type", json, ResponseCostHeader, "0")},
		wantCost: 0, wantSrc: event.SourceGatewayHeader, priced: true, free: true,
	}, {
		name:     "a zero header on SSE falls back to the table",
		in:       Input{Host: "gw.internal", Inference: plainInference(), ResponseHeaders: headers("Content-Type", "text/event-stream", ResponseCostHeader, "0")},
		wantCost: fallbackUSD(1000, 500), wantSrc: event.SourceUsageFallback, priced: true,
	}, {
		name:     "an implausible header with no Inference is refused",
		in:       Input{Host: "evil.example", Path: "/v1/embeddings", ResponseHeaders: headers("Content-Type", json, ResponseCostHeader, "2e9")},
		rejected: event.RejectedImplausible,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := SettleInput(tc.in, rates(t))
			if got.Priced != tc.priced || got.CostUSD != tc.wantCost || got.Source != tc.wantSrc ||
				got.DeclaredFree != tc.free || got.RejectedReason != tc.rejected {
				t.Errorf("got Priced=%v CostUSD=%v Source=%q DeclaredFree=%v RejectedReason=%q, want %v %v %q %v %q",
					got.Priced, got.CostUSD, got.Source, got.DeclaredFree, got.RejectedReason,
					tc.priced, tc.wantCost, tc.wantSrc, tc.free, tc.rejected)
			}
		})
	}
}

// ComparableInput reads the discount and margin headers from plain values.
//
// Hand-built cases. The header names follow LiteLLM 1.85.5.
func TestComparableInput_DiscountAndMargin(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    http.Header
		want bool
	}{
		{"the bare header is the effective figure", headers(ResponseCostHeader, "0.25", DiscountAmountHeader, "0.05"), true},
		{"a zero discount changes nothing", headers(ResponseCostOriginalHeader, "0.25", DiscountAmountHeader, "0.0"), true},
		{"a discount on the fallback", headers(ResponseCostOriginalHeader, "0.25", DiscountAmountHeader, "0.05"), false},
		{"a margin on the fallback", headers(ResponseCostOriginalHeader, "0.25", MarginAmountHeader, "0.05"), false},
		{"no headers", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ComparableInput(Input{ResponseHeaders: tc.h}); got != tc.want {
				t.Errorf("ComparableInput = %v, want %v", got, tc.want)
			}
		})
	}
}

// The pctx form is a wrapper. Over the package's settle fixtures it must return the same
// Settled as the Input form, built here field by field from the same context.
func TestSettle_PctxAndInputFormsAgree(t *testing.T) {
	const json = "application/json"
	fixtures := map[string]*pipeline.Context{
		"header":        ctx(map[string]string{"Content-Type": json, ResponseCostHeader: "0.25"}, 1000, 500),
		"original":      ctx(map[string]string{"Content-Type": json, ResponseCostOriginalHeader: "0.25"}, 1000, 500),
		"no header":     ctx(map[string]string{"Content-Type": json}, 1000, 500),
		"declared free": ctx(map[string]string{"Content-Type": json, ResponseCostHeader: "0"}, 1000, 500),
		"stream zero":   ctx(map[string]string{"Content-Type": "text/event-stream", ResponseCostHeader: "0"}, 1000, 500),
		"unusable":      ctx(map[string]string{"Content-Type": json, ResponseCostHeader: "abc"}, 1000, 500),
		"cache tiers":   cacheTierCtx(nil, blindInput, blindOutput, blindCacheWr, blindCacheRd),
		"tiers":         tierCtx(1000, 2000, 100_000, 500),
		"truncated":     truncatedCtx(),
		"implausible":   unparsedCostCtx("2e9"),
		"plausible":     unparsedCostCtx("0.25"),
	}
	for name, pctx := range fixtures {
		t.Run(name, func(t *testing.T) {
			in := Input{Host: pctx.Host, Path: pctx.Path, Inference: pctx.Extensions.Inference, ResponseHeaders: pctx.ResponseHeaders}
			want := SettleInput(in, rates(t))
			if got := Settle(pctx, rates(t)); !reflect.DeepEqual(got, want) {
				t.Errorf("Settle(pctx) = %+v\nSettleInput  = %+v", got, want)
			}
			if Comparable(pctx) != ComparableInput(in) {
				t.Error("Comparable(pctx) and ComparableInput disagree")
			}
		})
	}
}
