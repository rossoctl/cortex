package edit

import (
	"strings"
	"testing"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
)

func validateFixtureCatalog() []apiclient.PluginCatalogEntry {
	return []apiclient.PluginCatalogEntry{
		{Name: "jwt-validation", Description: "Inbound JWT"},
		{Name: "a2a-parser", Description: "Parser"},
		{Name: "mcp-parser", Description: "MCP parser"},
		{Name: "ibac", Description: "IBAC", Requires: []string{"mcp-parser"}},
	}
}

func TestValidatePipeline_Empty(t *testing.T) {
	errs := ValidatePipeline([]byte("pipeline:\n  inbound:\n    plugins: []\n"), validateFixtureCatalog())
	if len(errs) != 0 {
		t.Fatalf("empty chains: got %v", errs)
	}
}

func TestValidatePipeline_HappyPath(t *testing.T) {
	yaml := `pipeline:
  inbound:
    plugins:
      - name: jwt-validation
      - name: a2a-parser
  outbound:
    plugins:
      - name: mcp-parser
      - name: ibac
`
	errs := ValidatePipeline([]byte(yaml), validateFixtureCatalog())
	if len(errs) != 0 {
		t.Fatalf("happy path produced errors: %+v", errs)
	}
}

func TestValidatePipeline_MissingRequires(t *testing.T) {
	yaml := `pipeline:
  outbound:
    plugins:
      - name: ibac
`
	errs := ValidatePipeline([]byte(yaml), validateFixtureCatalog())
	if len(errs) == 0 {
		t.Fatal("expected error for missing mcp-parser")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "Requires \"mcp-parser\"") {
			found = true
		}
	}
	if !found {
		t.Fatalf("error should mention mcp-parser; got %+v", errs)
	}
}

func TestValidatePipeline_MisorderedRequires(t *testing.T) {
	yaml := `pipeline:
  outbound:
    plugins:
      - name: ibac
      - name: mcp-parser
`
	errs := ValidatePipeline([]byte(yaml), validateFixtureCatalog())
	if len(errs) == 0 {
		t.Fatal("expected misorder error")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "must be <") {
			found = true
		}
	}
	if !found {
		t.Fatalf("error should call out misorder; got %+v", errs)
	}
}

func TestValidatePipeline_UnknownPlugin(t *testing.T) {
	yaml := `pipeline:
  inbound:
    plugins:
      - name: definitely-not-a-real-plugin
`
	errs := ValidatePipeline([]byte(yaml), validateFixtureCatalog())
	if len(errs) != 1 || !strings.Contains(errs[0].Message, "Unknown plugin") {
		t.Fatalf("expected Unknown plugin error; got %+v", errs)
	}
}

func TestValidatePipeline_NilCatalogSkips(t *testing.T) {
	yaml := `pipeline:
  outbound:
    plugins:
      - name: bogus
`
	errs := ValidatePipeline([]byte(yaml), nil)
	if errs != nil {
		t.Fatalf("nil catalog should disable validation, got %+v", errs)
	}
}

// The #1568 case: litellm-budget-track declares RequiresLater inference-parser,
// and neither this validator nor the TUI's DEPS indicator knew the rule — an
// edit the proxy would refuse passed agentop's pre-apply check as clean.
func validateRequiresLaterCatalog() []apiclient.PluginCatalogEntry {
	return []apiclient.PluginCatalogEntry{
		{Name: "inference-parser", Description: "Parser"},
		{Name: "litellm-budget-track", Description: "Budget",
			RequiresLater: []string{"inference-parser"}},
	}
}

func TestValidatePipeline_RequiresLaterSatisfied(t *testing.T) {
	yaml := `pipeline:
  outbound:
    plugins:
      - name: litellm-budget-track
      - name: inference-parser
`
	errs := ValidatePipeline([]byte(yaml), validateRequiresLaterCatalog())
	if len(errs) != 0 {
		t.Fatalf("satisfied RequiresLater produced errors: %+v", errs)
	}
}

func TestValidatePipeline_RequiresLaterMissing(t *testing.T) {
	yaml := `pipeline:
  outbound:
    plugins:
      - name: litellm-budget-track
`
	errs := ValidatePipeline([]byte(yaml), validateRequiresLaterCatalog())
	if len(errs) != 1 {
		t.Fatalf("expected exactly the RequiresLater error, got %+v", errs)
	}
	if !strings.Contains(errs[0].Message, "RequiresLater \"inference-parser\"") {
		t.Fatalf("error should name the missing dependency; got %+v", errs)
	}
}

func TestValidatePipeline_RequiresLaterMisordered(t *testing.T) {
	// The exact operator mistake the issue describes: inference-parser
	// moved ABOVE budget-track, so the parser it reads while folding
	// response frames now runs before it. The framework refuses this
	// reload; agentop must refuse it at edit time too.
	yaml := `pipeline:
  outbound:
    plugins:
      - name: inference-parser
      - name: litellm-budget-track
`
	errs := ValidatePipeline([]byte(yaml), validateRequiresLaterCatalog())
	if len(errs) != 1 {
		t.Fatalf("expected exactly the RequiresLater misorder error, got %+v", errs)
	}
	if !strings.Contains(errs[0].Message, "must be > 2") {
		t.Fatalf("error should say the dependency must sit later; got %+v", errs)
	}
}
