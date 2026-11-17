package importer

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/ledger"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/model"
)

func TestPrice_TokensGetTranscriptRecord(t *testing.T) {
	e := fakeSession("s1", start, 1).Events[1]
	price(&e, "api.anthropic.com", testRates(t))
	rec, ok := event.Record(&e)
	if !ok {
		t.Fatal("no cost record")
	}
	if rec.Source != event.SourceTranscript || !rec.Priced() {
		t.Fatalf("record = %+v, want a priced transcript record", rec)
	}
}

func TestPrice_NoUsageNoRecord(t *testing.T) {
	e := model.SessionEvent{Phase: model.SessionResponse, Inference: &model.InferenceExtension{Model: "claude-sonnet-4-5"}}
	price(&e, "api.anthropic.com", testRates(t))
	if _, ok := event.Record(&e); ok {
		t.Fatal("a response with no usage got a cost record")
	}
}

func TestPrice_KeepsASourcesRecord(t *testing.T) {
	e := fakeSession("s1", start, 1).Events[1]
	b, err := json.Marshal(event.Event{CostUSD: 0.5, Source: event.SourceTranscript, Provenance: "authoritative", Settled: true})
	if err != nil {
		t.Fatal(err)
	}
	e.Plugins = map[string]json.RawMessage{event.Key: b}
	price(&e, "api.anthropic.com", testRates(t))
	rec, ok := event.Record(&e)
	if !ok {
		t.Fatal("no cost record")
	}
	if rec.CostUSD != 0.5 || rec.Provenance != "authoritative" {
		t.Fatalf("record = %+v, want the source's own record", rec)
	}
}

// bobcoinSource is fakeSource with the host of an endpoint that bills in Bobcoins.
type bobcoinSource struct{ fakeSource }

func (*bobcoinSource) PricingHost() string { return "gw.bob" }

// A source's own figure gets the unit of the source's endpoint, on the archived record, the
// session summary and the ledger row. An imported event has no Host, so the ledger cannot ask
// the rate table for it.
func TestRun_SourceRecordGetsTheEndpointUnit(t *testing.T) {
	rates, err := pricing.Build(&pricing.Config{Endpoints: []pricing.EndpointConfig{{
		Hosts: []string{"gw.bob"}, Unit: "Bobcoins",
		Models: map[string]pricing.ModelConfig{
			"premium-ide": {TierRates: pricing.TierRates{InputCostPerMillion: 2}},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	s := fakeSession("s1", start, 1)
	b, err := json.Marshal(event.Event{CostUSD: 0.5, Source: event.SourceTranscript, Provenance: "authoritative", Settled: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := range s.Events {
		s.Events[i].Inference.Model = "premium-ide"
	}
	s.Events[1].Plugins = map[string]json.RawMessage{event.Key: b, event.PluginName: b}
	a, w := openArchive(t), openLedger(t, ledger.WithPricing(rates))

	r := Run(context.Background(), &bobcoinSource{fakeSource{sessions: []Session{s}}},
		Options{Archive: a, Ledger: w, Rates: rates})
	if r.Imported != 1 {
		t.Fatalf("report = %+v, want 1 imported", r)
	}
	for _, e := range archivedEvents(t, a, "s1") {
		if rec, ok := event.Record(e); ok && (rec.Currency != "Bobcoins" || rec.CostUSD != 0.5) {
			t.Errorf("archived record = %+v, want the source's 0.5 in Bobcoins", rec)
		}
	}
	if sum, _ := a.Lookup("s1"); !slices.Equal(sum.Currencies, []string{"Bobcoins"}) {
		t.Errorf("summary currencies = %v, want [Bobcoins]", sum.Currencies)
	}
	rows := ledgerRows(t, w)
	if len(rows) != 1 || rows[0].Currency != "Bobcoins" {
		t.Fatalf("ledger rows = %+v, want one Bobcoins row", rows)
	}
}
