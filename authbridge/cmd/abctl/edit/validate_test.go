package edit

import (
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/apiclient"
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
