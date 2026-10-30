package tui

import (
	"fmt"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/cmd/agentop/edit"
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

// depPositions maps each plugin name to its first 1-based position in
// chain — the same earliest-occurrence-wins rule the framework's
// validateRelationships uses.
func depPositions(chain []apiclient.PipelinePlugin) map[string]int {
	positions := make(map[string]int, len(chain))
	for _, q := range chain {
		if _, seen := positions[q.Name]; !seen {
			positions[q.Name] = q.Position
		}
	}
	return positions
}

// positionStatus returns one depCheck per entry in names. Satisfied iff
// the named plugin is present strictly before (or, for RequiresLater,
// strictly after) p. One helper for all three declarations — Requires,
// RequiresAny and RequiresLater render per-row through it; the verdict
// itself is edit.CheckOrder's, not a reimplementation.
func positionStatus(p *apiclient.PipelinePlugin, chain []apiclient.PipelinePlugin, names []string, higher bool) []depCheck {
	out := make([]depCheck, 0, len(names))
	for _, name := range names {
		c := depCheck{Name: name}
		for _, q := range chain {
			if q.Name != name {
				continue
			}
			if (!higher && q.Position < p.Position) || (higher && q.Position > p.Position) {
				c.Satisfied = true
				c.UpstreamPosition = q.Position
			}
			break
		}
		out = append(out, c)
	}
	return out
}

// requiresStatus returns one depCheck per entry in p.Requires.
func requiresStatus(p *apiclient.PipelinePlugin, chain []apiclient.PipelinePlugin) []depCheck {
	return positionStatus(p, chain, p.Requires, false)
}

// requiresAnyStatus returns one depCheck per entry in p.RequiresAny.
// Satisfied means "present at lower position." Used by the detail
// pane to show which alternatives currently satisfy the OR-group.
func requiresAnyStatus(p *apiclient.PipelinePlugin, chain []apiclient.PipelinePlugin) []depCheck {
	return positionStatus(p, chain, p.RequiresAny, false)
}

// requiresLaterStatus returns one depCheck per entry in p.RequiresLater.
// Satisfied means "present at higher position" — the response pass walks
// the chain in reverse, so a RequiresLater dependency sits downstream.
func requiresLaterStatus(p *apiclient.PipelinePlugin, chain []apiclient.PipelinePlugin) []depCheck {
	return positionStatus(p, chain, p.RequiresLater, true)
}

// pluginDepsAllSatisfied returns true iff Requires, RequiresAny and
// RequiresLater are all OK for p in chain. Drives the Pipeline pane's
// per-row ✓/✗ indicator. The verdict is edit.CheckOrder's — the same
// copy of the framework's validateRelationships rules the pre-apply
// validator runs, so the indicator cannot show ✓ for a chain the proxy
// would refuse to reload (#1568: it did, for RequiresLater).
func pluginDepsAllSatisfied(p *apiclient.PipelinePlugin, chain []apiclient.PipelinePlugin) bool {
	return len(edit.CheckOrder(p.Name, p.Position, depPositions(chain),
		p.Requires, p.RequiresAny, p.RequiresLater)) == 0
}

// pluginHasAnyDeps reports whether p declares any dependency that the
// indicator can render. Plugins without any declarations get a blank
// indicator (no false-positive ✓).
func pluginHasAnyDeps(p *apiclient.PipelinePlugin) bool {
	return len(p.Requires) > 0 || len(p.RequiresAny) > 0 || len(p.RequiresLater) > 0
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
	// Not "NOT in this chain": a ✗ also covers the misorder case, where the
	// name IS present but on the wrong side of p (Requires wants it earlier,
	// RequiresLater later).
	return styleError.Render("✗ ") + c.Name +
		styleHint.Render("  — not satisfied in this chain")
}
