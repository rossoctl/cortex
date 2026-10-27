package plugins

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

type capsPlugin struct {
	name string
	caps pipeline.PluginCapabilities
}

func (p capsPlugin) Name() string                              { return p.name }
func (p capsPlugin) Capabilities() pipeline.PluginCapabilities { return p.caps }
func (p capsPlugin) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p capsPlugin) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// subsets returns every subset of names, the empty one included, each in names' order.
func subsets(names []string) [][]string {
	var out [][]string
	for mask := 0; mask < 1<<len(names); mask++ {
		var s []string
		for i, n := range names {
			if mask&(1<<i) != 0 {
				s = append(s, n)
			}
		}
		out = append(out, s)
	}
	return out
}

// chains returns every ordering of every subset of others, with the plugin under test
// "x" inserted at every position.
func chains(others []string) [][]string {
	var out [][]string
	var permute func(prefix, rest []string)
	permute = func(prefix, rest []string) {
		for i := 0; i <= len(prefix); i++ {
			out = append(out, slices.Concat(prefix[:i], []string{"x"}, prefix[i:]))
		}
		for i, n := range rest {
			permute(append(slices.Clone(prefix), n), slices.Concat(rest[:i], rest[i+1:]))
		}
	}
	permute(nil, others)
	return out
}

// validChain reports whether BuildWithDeps's two chain checks accept chain, with "x"
// declaring caps and every other plugin what others gives it.
func validChain(chain []string, others map[string]pipeline.PluginCapabilities, caps pipeline.PluginCapabilities) bool {
	ps := make([]pipeline.Plugin, len(chain))
	for i, n := range chain {
		ps[i] = capsPlugin{name: n, caps: others[n]}
		if n == "x" {
			ps[i] = capsPlugin{name: n, caps: caps}
		}
	}
	if validateRelationships(ps) != nil {
		return false
	}
	_, err := pipeline.New(ps)
	return err == nil
}

// assertWithinCeilingIsSafe fails for any pair WithinCeiling admits where some chain
// accepts the ceiling and refuses the configured set.
func assertWithinCeilingIsSafe(t *testing.T, label string, others map[string]pipeline.PluginCapabilities, caps []pipeline.PluginCapabilities) {
	t.Helper()
	all := chains(slices.Sorted(maps.Keys(others)))
	for _, ceiling := range caps {
		for _, configured := range caps {
			if configured.WithinCeiling(ceiling) != "" {
				continue
			}
			for _, chain := range all {
				if validChain(chain, others, ceiling) && !validChain(chain, others, configured) {
					t.Errorf("%s: configured %+v is within ceiling %+v, but chain %v accepts the ceiling and refuses it",
						label, configured, ceiling, chain)
					break
				}
			}
		}
	}
}

// A configured instance WithinCeiling admits must be accepted by every chain its
// ceiling is accepted by, or the catalog — and agentop's edit check, which reads it —
// passes a chain the build then refuses.
func TestWithinCeiling_AdmitsNoRelationshipTheCeilingDoesNot(t *testing.T) {
	names := []string{"a", "b", "c"}
	others := map[string]pipeline.PluginCapabilities{"a": {}, "b": {}, "c": {}}
	fields := map[string]func([]string) pipeline.PluginCapabilities{
		"Requires":      func(n []string) pipeline.PluginCapabilities { return pipeline.PluginCapabilities{Requires: n} },
		"RequiresAny":   func(n []string) pipeline.PluginCapabilities { return pipeline.PluginCapabilities{RequiresAny: n} },
		"RequiresLater": func(n []string) pipeline.PluginCapabilities { return pipeline.PluginCapabilities{RequiresLater: n} },
	}
	for field, mk := range fields {
		var caps []pipeline.PluginCapabilities
		for _, n := range subsets(names) {
			caps = append(caps, mk(n))
		}
		assertWithinCeilingIsSafe(t, field, others, caps)
	}
}

// The same holds for the flags pipeline.New orders by: a body reader must precede every
// mutator, and a chain has at most one destination writer.
func TestWithinCeiling_AdmitsNoFlagTheCeilingDoesNot(t *testing.T) {
	others := map[string]pipeline.PluginCapabilities{
		"reader":   {ReadsBody: true},
		"req-mut":  {WritesRequestBody: true},
		"resp-mut": {WritesResponseBody: true},
		"dest":     {WritesDestination: true},
	}
	var caps []pipeline.PluginCapabilities
	for mask := 0; mask < 16; mask++ {
		caps = append(caps, pipeline.PluginCapabilities{
			ReadsBody:          mask&1 != 0,
			WritesRequestBody:  mask&2 != 0,
			WritesResponseBody: mask&4 != 0,
			WritesDestination:  mask&8 != 0,
		})
	}
	assertWithinCeilingIsSafe(t, "flags", others, caps)
}
