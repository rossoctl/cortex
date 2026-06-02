package tui

import (
	"fmt"

	"github.com/kagenti/kagenti-extensions/authbridge/cmd/abctl/apiclient"
)

// depCheck describes whether one declared dependency is satisfied by
// the rest of a same-direction chain. Used by both the Pipeline pane's
// per-row indicator and the Plugin-detail pane's per-Requires section.
type depCheck struct {
	Name      string // the dependency-target plugin name
	Satisfied bool
	// UpstreamPosition is the 1-based position of the satisfying
	// upstream plugin when Satisfied is true, otherwise 0.
	UpstreamPosition int
}

// sameDirectionChain returns the slice of plugins in p's chain. Caller
// supplies the full PipelineView so we can look up the right side
// without re-fetching.
func sameDirectionChain(p *apiclient.PipelinePlugin, view *apiclient.PipelineView) []apiclient.PipelinePlugin {
	if view == nil {
		return nil
	}
	if p.Direction == "inbound" {
		return view.Inbound
	}
	if p.Direction == "outbound" {
		return view.Outbound
	}
	return nil
}

// requiresStatus returns one depCheck per entry in p.Requires.
// Satisfied iff the named plugin is present at a strictly-lower
// position. (RequiresAny semantics differ; see requiresAnyStatus.)
func requiresStatus(p *apiclient.PipelinePlugin, chain []apiclient.PipelinePlugin) []depCheck {
	out := make([]depCheck, 0, len(p.Requires))
	for _, name := range p.Requires {
		c := depCheck{Name: name}
		for _, q := range chain {
			if q.Name == name && q.Position < p.Position {
				c.Satisfied = true
				c.UpstreamPosition = q.Position
				break
			}
		}
		out = append(out, c)
	}
	return out
}

// requiresAnyOK matches the framework's validateRelationships rule
// exactly: at least one named plugin must be present upstream, AND
// every name that IS present must be earlier (a downstream presence
// is a misorder). Earlier this returned true on at-least-one-upstream
// without checking the misorder condition, which let the Pipeline
// pane's DEPS column show ✓ for chains the framework would reject.
func requiresAnyOK(p *apiclient.PipelinePlugin, chain []apiclient.PipelinePlugin) bool {
	if len(p.RequiresAny) == 0 {
		return true
	}
	anyUpstream := false
	for _, name := range p.RequiresAny {
		for _, q := range chain {
			if q.Name != name {
				continue
			}
			if q.Position < p.Position {
				anyUpstream = true
			} else if q.Position > p.Position {
				// Present-but-downstream violates the ordering rule.
				return false
			}
			break
		}
	}
	return anyUpstream
}

// requiresAnyStatus returns one depCheck per entry in p.RequiresAny.
// Satisfied means "present at lower position." Used by the detail
// pane to show which alternatives currently satisfy the OR-group.
func requiresAnyStatus(p *apiclient.PipelinePlugin, chain []apiclient.PipelinePlugin) []depCheck {
	out := make([]depCheck, 0, len(p.RequiresAny))
	for _, name := range p.RequiresAny {
		c := depCheck{Name: name}
		for _, q := range chain {
			if q.Name == name && q.Position < p.Position {
				c.Satisfied = true
				c.UpstreamPosition = q.Position
				break
			}
		}
		out = append(out, c)
	}
	return out
}

// pluginDepsAllSatisfied returns true iff Requires and RequiresAny are
// all OK for p in chain. Drives the Pipeline pane's per-row ✓/✗ indicator.
func pluginDepsAllSatisfied(p *apiclient.PipelinePlugin, chain []apiclient.PipelinePlugin) bool {
	for _, c := range requiresStatus(p, chain) {
		if !c.Satisfied {
			return false
		}
	}
	return requiresAnyOK(p, chain)
}

// pluginHasAnyDeps reports whether p declares any dependency that the
// indicator can render. Plugins without any declarations get a blank
// indicator (no false-positive ✓).
func pluginHasAnyDeps(p *apiclient.PipelinePlugin) bool {
	return len(p.Requires) > 0 || len(p.RequiresAny) > 0
}

// formatDepCheck returns a one-line description of a dependency check.
// Catalog-mode (no chain to check against) skips the ✓/✗ prefix and
// just shows the name — operators see the declared dependency without
// a misleading "satisfied" claim.
func formatDepCheck(c depCheck, withStatus bool) string {
	if !withStatus {
		return c.Name
	}
	if c.Satisfied {
		if c.UpstreamPosition > 0 {
			return styleOK.Render("✓ ") + c.Name +
				styleHint.Render(fmt.Sprintf("  — at position %d", c.UpstreamPosition))
		}
		return styleOK.Render("✓ ") + c.Name +
			styleHint.Render("  — absent (soft)")
	}
	return styleError.Render("✗ ") + c.Name +
		styleHint.Render("  — NOT in this chain")
}
