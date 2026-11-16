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
	if e.Phase != model.SessionResponse || pricing.UsageFromInference(e.Inference) == (pricing.Usage{}) {
		return
	}
	st := settle.SettleInput(settle.Input{Host: host, Path: "/v1/messages", Inference: e.Inference}, rates)
	rec := settle.NewRecord(st, nil)
	rec.Source = event.SourceTranscript
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
