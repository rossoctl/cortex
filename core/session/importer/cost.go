package importer

import (
	"encoding/json"
	"log/slog"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/cost/settle"
	"github.com/rossoctl/cortex/core/model"
)

// price adds the cost record of a response event in place, as sessionapi's ingest does. A
// response with no token counts gets no record. host is for the rate lookup only: the event
// keeps no host, because the host is a field that only the proxy sets.
func price(e *model.SessionEvent, host string, rates pricing.Resolver) {
	if e.Phase != model.SessionResponse {
		return
	}
	// A source that has the agent's own figure (Bob) sets the record first. The figure is in
	// the endpoint's unit (Bobcoins for Bob), which only the rate table knows.
	if rec, ok := event.Record(e); ok {
		var modelID string
		if e.Inference != nil {
			modelID = e.Inference.Model
		}
		if rec.Currency == "" {
			if rec.Currency = pricing.UnitOf(rates, host, modelID); rec.Currency != "" {
				setRecord(e, rec)
			}
		}
		return
	}
	if pricing.UsageFromInference(e.Inference) == (pricing.Usage{}) {
		return
	}
	st := settle.SettleInput(settle.Input{Host: host, Path: "/v1/messages", Inference: e.Inference}, rates)
	rec := settle.NewRecord(st, nil)
	rec.Source = event.SourceTranscript
	setRecord(e, rec)
}

// setRecord puts rec on e under both keys that event.Record reads.
func setRecord(e *model.SessionEvent, rec event.Event) {
	b, err := json.Marshal(rec)
	if err != nil {
		slog.Warn("importer: cost record", "error", err)
		return
	}
	if e.Plugins == nil {
		e.Plugins = map[string]json.RawMessage{}
	}
	e.Plugins[event.Key], e.Plugins[event.PluginName] = b, b
}
