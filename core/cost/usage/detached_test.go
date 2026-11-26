package usage

import (
	"encoding/json"
	"testing"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/pipeline"
)

// A detached model call's record (cortex#2140) is not the agent's request: its spend is
// PluginSpendMicros, as the ledger books it, and it is in no request, token or cost figure, and
// in no session ring.
func TestAggregator_ADetachedSpendIsNotARequest(t *testing.T) {
	a := New(WithPricing(nil))
	raw, err := json.Marshal(event.Event{Purpose: event.PurposeDetachedSpend,
		Spends: []event.SpendRecord{{Operator: "context-guru", Basis: event.BasisUSD, USD: 0.0195}}})
	if err != nil {
		t.Fatal(err)
	}
	a.Record("s1", &pipeline.SessionEvent{
		Phase: pipeline.SessionResponse, Host: "api.anthropic.com", RequestID: "detached:r1:1", StatusCode: 200,
		Inference: &pipeline.InferenceExtension{Model: "claude-haiku-4-5"},
		Plugins:   map[string]json.RawMessage{event.Key: raw},
	})
	tot := a.Snapshot(10*BucketWidth, BucketWidth, "", GroupNone).Totals
	if tot.PluginSpendMicros != 19_500 {
		t.Errorf("PluginSpendMicros = %d, want 19500", tot.PluginSpendMicros)
	}
	if tot.Requests != 0 || tot.CostMicros != 0 || tot.Tokens != 0 || tot.PriceableRequests != 0 || tot.KeepAlivePings != 0 {
		t.Errorf("a detached spend reached the request figures: %+v", tot)
	}
	a.mu.Lock()
	_, ring := a.sessions["s1"]
	a.mu.Unlock()
	if ring {
		t.Error("a detached spend made a session ring")
	}
}
