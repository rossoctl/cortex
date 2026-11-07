package model_test

import (
	"os/exec"
	"slices"
	"strings"
	"testing"

	// A change in a guarded package must rebuild this test, so the test cache cannot hide a violation.
	_ "github.com/rossoctl/cortex/core/attribution"
	_ "github.com/rossoctl/cortex/core/cost/event"
	_ "github.com/rossoctl/cortex/core/cost/ledger"
	_ "github.com/rossoctl/cortex/core/cost/pricing"
	_ "github.com/rossoctl/cortex/core/cost/usage"
	_ "github.com/rossoctl/cortex/core/parse/a2a"
	_ "github.com/rossoctl/cortex/core/parse/inference"
	_ "github.com/rossoctl/cortex/core/parse/mcp"
	_ "github.com/rossoctl/cortex/core/session"
	_ "github.com/rossoctl/cortex/core/session/archive"
	_ "github.com/rossoctl/cortex/core/session/timeline"
)

// These packages parse requests, or read and store Cortex events. They must build without
// the plugin engine (#1502), so none of them may reach core/pipeline, even through another package.
var dataOnly = []string{
	"github.com/rossoctl/cortex/core/model",
	"github.com/rossoctl/cortex/core/cost/pricing",
	"github.com/rossoctl/cortex/core/cost/event",
	"github.com/rossoctl/cortex/core/cost/usage",
	"github.com/rossoctl/cortex/core/cost/ledger",
	"github.com/rossoctl/cortex/core/session",
	"github.com/rossoctl/cortex/core/session/archive",
	"github.com/rossoctl/cortex/core/session/timeline",
	"github.com/rossoctl/cortex/core/attribution",
	"github.com/rossoctl/cortex/core/parse/a2a",
	"github.com/rossoctl/cortex/core/parse/inference",
	"github.com/rossoctl/cortex/core/parse/mcp",
}

// The parser libraries compute no cost (#1503). The plugin that wraps a parser may.
var parsers = []string{
	"github.com/rossoctl/cortex/core/parse/a2a",
	"github.com/rossoctl/cortex/core/parse/inference",
	"github.com/rossoctl/cortex/core/parse/mcp",
}

var pricingPackages = []string{
	"github.com/rossoctl/cortex/core/cost/pricing",
	"github.com/rossoctl/cortex/core/cost/settle",
}

const engine = "github.com/rossoctl/cortex/core/pipeline"

func TestDataPackagesDoNotImportThePipeline(t *testing.T) {
	checkDeps(t, dataOnly, []string{engine}, "move what it needs to core/model")
}

func TestParsersDoNotImportPricing(t *testing.T) {
	checkDeps(t, parsers, pricingPackages, "price in the plugin that wraps the parser")
}

// checkDeps fails t when a package of pkgs reaches a package of banned, even through another package.
func checkDeps(t *testing.T, pkgs, banned []string, fix string) {
	t.Helper()
	// go list fails on a package that does not exist, so a stale entry fails here too.
	out, err := exec.Command("go", append([]string{"list", "-f", "{{.ImportPath}} {{join .Deps \" \"}}"}, pkgs...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != len(pkgs) {
		t.Fatalf("go list gave %d packages, want %d:\n%s", len(lines), len(pkgs), out)
	}
	for _, line := range lines {
		pkg, deps, _ := strings.Cut(line, " ")
		for _, d := range strings.Fields(deps) {
			if slices.Contains(banned, d) {
				t.Errorf("%s imports %s; %s", pkg, d, fix)
			}
		}
	}
}
