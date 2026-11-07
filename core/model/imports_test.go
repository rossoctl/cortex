package model_test

import (
	"os/exec"
	"strings"
	"testing"
)

// These packages read and store Cortex events. They must build without the plugin engine
// (#1502), so none of them may reach core/pipeline, even through another package.
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
}

const engine = "github.com/rossoctl/cortex/core/pipeline"

func TestDataPackagesDoNotImportThePipeline(t *testing.T) {
	// go list fails on a package that does not exist, so a stale entry fails here too.
	out, err := exec.Command("go", append([]string{"list", "-f", "{{.ImportPath}} {{join .Deps \" \"}}"}, dataOnly...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != len(dataOnly) {
		t.Fatalf("go list gave %d packages, want %d:\n%s", len(lines), len(dataOnly), out)
	}
	for _, line := range lines {
		pkg, deps, _ := strings.Cut(line, " ")
		for _, d := range strings.Fields(deps) {
			if d == engine {
				t.Errorf("%s imports %s; move what it needs to core/model", pkg, engine)
			}
		}
	}
}
