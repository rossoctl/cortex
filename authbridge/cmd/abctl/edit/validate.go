package edit

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/apiclient"
)

// ValidationError describes one problem with a proposed pipeline,
// detected by abctl before kubectl apply. The framework's own
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

// validateChain runs the Requires/RequiresAny/unknown-name checks for one direction.
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
			// abctl caches /v1/plugins for the session; a freshly-installed
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

		// Requires: every name MUST appear at strictly-lower position.
		for _, req := range entry.Requires {
			rp, present := positions[req]
			if !present {
				errs = append(errs, ValidationError{
					Direction:  direction,
					PluginName: p.Name,
					Position:   pos,
					Message: fmt.Sprintf("Requires %q, but it is not in the %s chain",
						req, direction),
				})
				continue
			}
			if rp >= pos {
				errs = append(errs, ValidationError{
					Direction:  direction,
					PluginName: p.Name,
					Position:   pos,
					Message: fmt.Sprintf("Requires %q upstream, but it is at position %d (must be < %d)",
						req, rp, pos),
				})
			}
		}

		// RequiresAny: at least one of the listed names must appear at
		// lower position. Each named one that IS present must be earlier.
		if len(entry.RequiresAny) > 0 {
			anyOK := false
			for _, opt := range entry.RequiresAny {
				rp, present := positions[opt]
				if !present {
					continue
				}
				if rp < pos {
					anyOK = true
				}
				if rp >= pos {
					errs = append(errs, ValidationError{
						Direction:  direction,
						PluginName: p.Name,
						Position:   pos,
						Message: fmt.Sprintf("RequiresAny lists %q which is at position %d (must be < %d)",
							opt, rp, pos),
					})
				}
			}
			if !anyOK {
				errs = append(errs, ValidationError{
					Direction:  direction,
					PluginName: p.Name,
					Position:   pos,
					Message: fmt.Sprintf("RequiresAny %v: none present upstream in %s chain",
						entry.RequiresAny, direction),
				})
			}
		}

	}
	return errs
}
