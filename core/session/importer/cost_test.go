package importer

import (
	"encoding/json"
	"testing"

	"github.com/rossoctl/cortex/core/cost/event"
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
