package litellm_budgettrack

import (
	"context"
	"net/http"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/costing"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
)

// A settled zero must be published for exactly one case and no other. The bool this
// replaced collapsed five, so publishing on "header present" claimed unpriced
// traffic as priced — counting it toward coverage and dropping it from the unpriced
// list, the inverse of the bug the branch was added to fix.
func TestSettledZero_OnlyForADeclaredFreeCall(t *testing.T) {
	// A model the table does not price, so the usage fallback cannot rescue a case.
	unpriced := func(t *testing.T) *billing {
		t.Helper()
		p := New()
		var r pricing.Rates
		r.Base[pricing.TierInput], r.Set[pricing.TierInput] = 1e-6, true
		tab, err := pricing.NewTable([]pricing.Entry{
			{Host: "*", Model: "priced-model", Rates: r, Prov: pricing.ProvConfigured},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Configure([]byte(`{"spend_file":"` + t.TempDir() + `/s.json","max_budget":100}`)); err != nil {
			t.Fatal(err)
		}
		// The rates go to the COST OWNER now, not to this plugin — see the billing
		// wrapper in plugin_test.go.
		return &billing{BudgetTrack: p, rates: pricing.NewRegistry(tab)}
	}

	for _, tc := range []struct {
		name       string
		header     string // "" means absent
		streamed   bool
		model      string
		wantEvent  bool
		wantReason string
	}{{
		name: "non-streamed literal zero is a declared free call", header: "0",
		model: "unknown-model", wantEvent: true,
		wantReason: "the gateway parsed a finite zero and this is not a stream",
	}, {
		name: "streamed zero is a placeholder, not an answer", header: "0",
		streamed: true, model: "unknown-model", wantEvent: false,
		wantReason: "LiteLLM stamps 0 on every stream by design; the fallback owns this",
	}, {
		name: "unparseable header is not a declaration", header: "not-a-number",
		model: "unknown-model", wantEvent: false,
		wantReason: "a garbage header says nothing about whether the call was free",
	}, {
		name: "negative header is not a declaration", header: "-5",
		model: "unknown-model", wantEvent: false, wantReason: "negative is unusable",
	}, {
		name: "NaN header is not a declaration", header: "NaN",
		model: "unknown-model", wantEvent: false, wantReason: "non-finite is unusable",
	}, {
		name: "Inf header is not a declaration", header: "Inf",
		model: "unknown-model", wantEvent: false, wantReason: "non-finite is unusable",
	}, {
		name: "absent header on an unpriceable model emits nothing", header: "",
		model: "unknown-model", wantEvent: false,
		wantReason: "unpriced, so the aggregator must be free to name it",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			p := unpriced(t)
			h := http.Header{}
			if tc.header != "" {
				h.Set(costing.ResponseCostHeader, tc.header)
			}
			if tc.streamed {
				h.Set("Content-Type", "text/event-stream")
			} else {
				h.Set("Content-Type", "application/json")
			}
			pctx := &pipeline.Context{Host: "gw.internal", ResponseHeaders: h}
			pctx.Extensions.Inference = &pipeline.InferenceExtension{
				Model: tc.model, InputTokens: 1000, OutputTokens: 100,
			}
			p.OnResponseFrame(context.Background(), pctx, nil, true)

			ev := getCostEvent(t, pctx)
			if tc.wantEvent && ev == nil {
				t.Fatalf("no event published; expected a settled zero because %s", tc.wantReason)
			}
			if !tc.wantEvent && ev != nil {
				t.Fatalf("published %+v; expected NO event because %s", *ev, tc.wantReason)
			}
			if ev != nil {
				if !ev.Settled || ev.CostUSD != 0 {
					t.Errorf("event = %+v, want a settled zero", *ev)
				}
				if ev.Provenance != pricing.ProvAuthoritative.String() {
					t.Errorf("provenance = %q, want authoritative", ev.Provenance)
				}
			}
			if p.ledger.TotalSpend != 0 {
				t.Errorf("ledger moved to %v on a zero-cost call", p.ledger.TotalSpend)
			}
		})
	}
}

// TestSettledZero_StreamedZeroStillPricesFromUsage: the placeholder case must still
// reach the fallback, or suppressing it would unprice all streamed traffic.
func TestSettledZero_StreamedZeroStillPricesFromUsage(t *testing.T) {
	p := configurePriced(t, 100, map[pricing.Tier]float64{
		pricing.TierInput: 1e-6, pricing.TierOutput: 5e-6,
	})
	pctx := &pipeline.Context{ResponseHeaders: http.Header{
		costing.ResponseCostHeader: {"0"},
		"Content-Type":             {"text/event-stream"},
	}}
	pricedInference(pctx, 1000, 0, 0, 100)
	p.OnResponseFrame(context.Background(), pctx, nil, true)

	ev := getCostEvent(t, pctx)
	if ev == nil {
		t.Fatal("no event; a streamed zero must fall through to usage pricing")
	}
	if ev.Settled && ev.CostUSD == 0 {
		t.Fatal("published a settled zero for a stream that could be priced from usage")
	}
	if want := 1000*1e-6 + 100*5e-6; ev.CostUSD < want-1e-12 || ev.CostUSD > want+1e-12 {
		t.Errorf("cost = %v, want %v", ev.CostUSD, want)
	}
	if ev.Source != costevent.SourceUsageFallback {
		t.Errorf("source = %q, want %q", ev.Source, costevent.SourceUsageFallback)
	}
}

func TestConfigure_RejectsTrailingGarbage(t *testing.T) {
	// Switching to a decoder for DisallowUnknownFields must not loosen what
	// json.Unmarshal rejected.
	err := New().Configure([]byte(`{"spend_file":"/tmp/s.json","max_budget":5}{"trailing":"garbage"}`))
	if err == nil {
		t.Fatal("accepted trailing content after the config object")
	}
}
