package plugins

import (
	"github.com/rossoctl/rossocortex/authbridge/authlib/auth"
	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
)

// StatsSource is an optional interface a plugin implements when it
// maintains observability counters worth exposing on the /stats
// endpoint. Lives in the plugins package (rather than pipeline)
// because auth.Stats is the shared counter type and the pipeline
// package has no reason to depend on auth.
//
// Plugins that don't report stats don't implement this interface;
// the host skips them during aggregation.
type StatsSource interface {
	Stats() *auth.Stats
}

// CollectStats walks a pipeline and returns every *auth.Stats
// exposed by a StatsSource-implementing plugin. Order matches plugin
// declaration order in the pipeline, which keeps the aggregated
// output deterministic.
//
// Returns nil (not an empty slice) when no plugins implement
// StatsSource, so callers can pass the result straight to
// auth.MergeStats without guarding against zero-length.
func CollectStats(p *pipeline.Pipeline) []*auth.Stats {
	if p == nil {
		return nil
	}
	var out []*auth.Stats
	for _, plugin := range p.Plugins() {
		src, ok := plugin.(StatsSource)
		if !ok {
			continue
		}
		if s := src.Stats(); s != nil {
			out = append(out, s)
		}
	}
	return out
}
