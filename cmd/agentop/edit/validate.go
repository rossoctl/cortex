package edit

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
)

// ValidationError describes one problem with a proposed pipeline,
// detected by agentop before kubectl apply. The framework's own
// validateRelationships is the source of truth (and runs again after
// reload); this is the fast-feedback layer.
type ValidationError struct {
	// Direction is "inbound" or "outbound".
	Direction string
	// PluginName is the offending plugin's name.
	PluginName string
	// Position is the offending plugin's 1-based position in its chain.
	Position int
	// Message is operator-facing: "Requires mcp-parser, missing in
	// outbound chain" / "Unknown plugin name" / etc.
	Message string
}

// pipelineDoc mirrors the runtime YAML's pipeline subtree. Only the
// fields the validator needs.
type pipelineDoc struct {
	Inbound  pipelineChain `yaml:"inbound"`
	Outbound pipelineChain `yaml:"outbound"`
}

type pipelineChain struct {
	Plugins []pluginEntry `yaml:"plugins"`
}

type pluginEntry struct {
	Name string `yaml:"name"`
}

// pipelineRoot wraps pipelineDoc under the top-level "pipeline:" key,
// which is what callers pass in (the inner subtree).
type pipelineRoot struct {
	Pipeline pipelineDoc `yaml:"pipeline"`
}

// ValidatePipeline parses subtree YAML and checks Requires / RequiresAny
// against the catalog. Catalog comes from /v1/plugins; passing nil disables
// validation (no errors returned). Unknown plugin names produce errors so
// a typo gets caught before apply.
//
// Returns nil when all checks pass.
func ValidatePipeline(subtree []byte, catalog []apiclient.PluginCatalogEntry) []ValidationError {
	if catalog == nil {
		return nil
	}
	var root pipelineRoot
	if err := yaml.Unmarshal(subtree, &root); err != nil {
		// YAML errors are surfaced separately by the caller; this
		// validator's job is the dependency layer only.
		return nil
	}
	byName := make(map[string]apiclient.PluginCatalogEntry, len(catalog))
	for _, e := range catalog {
		byName[e.Name] = e
	}

	var errs []ValidationError
	errs = append(errs, validateChain("inbound", root.Pipeline.Inbound, byName)...)
	errs = append(errs, validateChain("outbound", root.Pipeline.Outbound, byName)...)
	return errs
}

// CheckOrder reports every dependency-ordering violation for the plugin
// named name sitting at 1-based position pos, against positions (plugin
// name → first 1-based position in the same chain). It is agentop's one
// copy of the framework's validateRelationships ordering rules — the
// pre-apply validator below and the TUI's DEPS indicator both call it, so
// the two can no longer drift apart the way they did over RequiresLater
// (#1568). Each returned string is operator-facing.
//
// Rules, mirroring core/plugins/registry.go:
//   - Requires: every named plugin at a strictly-lower position.
//   - RequiresLater: every named plugin at a strictly-higher position
//     (the response pass walks the chain in reverse).
//   - RequiresAny: at least one named plugin at a lower position, and
//     every named plugin that IS present at a lower position.
func CheckOrder(name string, pos int, positions map[string]int, requires, requiresAny, requiresLater []string) []string {
	var problems []string
	for _, req := range requires {
		rp, present := positions[req]
		switch {
		case !present:
			problems = append(problems, fmt.Sprintf("Requires %q, but it is not in this chain", req))
		case rp >= pos:
			problems = append(problems, fmt.Sprintf("Requires %q upstream, but it is at position %d (must be < %d)",
				req, rp, pos))
		}
	}
	for _, req := range requiresLater {
		rp, present := positions[req]
		switch {
		case !present:
			problems = append(problems, fmt.Sprintf("RequiresLater %q, but it is not in this chain", req))
		case rp <= pos:
			problems = append(problems, fmt.Sprintf("RequiresLater %q downstream (it reads what %q publishes while folding response frames, and the response pass runs in reverse), but it is at position %d (must be > %d)",
				req, req, rp, pos))
		}
	}
	if len(requiresAny) > 0 {
		anyUpstream := false
		for _, opt := range requiresAny {
			rp, present := positions[opt]
			if !present {
				continue
			}
			if rp < pos {
				anyUpstream = true
			} else {
				problems = append(problems, fmt.Sprintf("RequiresAny lists %q which is at position %d (must be < %d)",
					opt, rp, pos))
			}
		}
		if !anyUpstream {
			problems = append(problems, fmt.Sprintf("RequiresAny %v: none present upstream in this chain", requiresAny))
		}
	}
	return problems
}

// validateChain runs the Requires/RequiresAny/RequiresLater/unknown-name checks for one direction.
func validateChain(direction string, chain pipelineChain, byName map[string]apiclient.PluginCatalogEntry) []ValidationError {
	var errs []ValidationError
	// Track positions of each name for ordering checks. Using lowest
	// position wins on duplicates — same as the framework.
	positions := map[string]int{}
	for i, p := range chain.Plugins {
		if _, seen := positions[p.Name]; !seen {
			positions[p.Name] = i + 1
		}
	}

	for i, p := range chain.Plugins {
		pos := i + 1
		entry, known := byName[p.Name]
		if !known {
			// agentop caches /v1/plugins for the session; a freshly-installed
			// plugin in-cluster won't appear until refresh. Hint at the
			// staleness path so operators don't get stuck in confusion when
			// the framework would actually accept the edit.
			errs = append(errs, ValidationError{
				Direction:  direction,
				PluginName: p.Name,
				Position:   pos,
				Message: fmt.Sprintf("Unknown plugin %q (not in cached /v1/plugins; "+
					"catalog may be stale, press P then r to refresh)", p.Name),
			})
			continue
		}

		// Requires / RequiresAny / RequiresLater ordering — CheckOrder, the
		// one copy of the framework's rules shared with the TUI's DEPS
		// indicator. Messages name the chain direction here only.
		for _, problem := range CheckOrder(p.Name, pos, positions, entry.Requires, entry.RequiresAny, entry.RequiresLater) {
			errs = append(errs, ValidationError{
				Direction:  direction,
				PluginName: p.Name,
				Position:   pos,
				Message:    fmt.Sprintf("%s (%s chain)", problem, direction),
			})
		}

	}
	return errs
}
