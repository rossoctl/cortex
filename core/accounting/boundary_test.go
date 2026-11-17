package accounting

import (
	"os/exec"
	"strings"
	"testing"
)

// allowedModules are the only non-standard-library module prefixes core/accounting/... may ever
// depend on (global-constraints.md): accountant itself (and its test/cases helper package),
// OTel, Cortex's own core/... packages, and tidwall's gjson/sjson — wire.Parse ports the proxy's
// accParse, which uses both, and core/go.mod already requires them directly. Review round 1
// changed this test from a context-guru denylist to this allowlist, so a new, unreviewed
// dependency of any kind fails loudly instead of only the one named dependency this boundary was
// first written to catch.
var allowedModules = []string{
	"github.com/rossoctl/accountant",
	"go.opentelemetry.io/",
	"github.com/rossoctl/cortex/core/",
	"github.com/tidwall/",
}

// forbiddenModule gets its own explicit message on top of the allowlist check below, because it
// is the one dependency this boundary exists to forbid (design §5.2: accounting is a Cortex
// feature, not a context-guru feature, and context-guru depends on core/accounting, never the
// other way around) — a reviewer scanning test output for this specific name should still find
// it named, not just "not allowed".
const forbiddenModule = "github.com/rossoctl/context-guru"

// TestNoContextGuruDependency runs go list -deps over this package tree (accountant's own
// boundary_test.go, TestImportsStandardLibraryOnly, uses the same -f trick to isolate
// non-standard-library imports) and fails on anything outside allowedModules.
func TestNoContextGuruDependency(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	out, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", "./...").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		if line == forbiddenModule || strings.HasPrefix(line, forbiddenModule+"/") {
			t.Errorf("core/accounting depends on %q: context-guru is the one dependency this boundary exists to forbid", line)
			continue
		}
		if !allowedModule(line) {
			t.Errorf("core/accounting depends on %q: not in the allowed set %v", line, allowedModules)
		}
	}
}

// allowedModule reports whether importPath is, or is under, one of allowedModules.
func allowedModule(importPath string) bool {
	for _, p := range allowedModules {
		p = strings.TrimSuffix(p, "/")
		if importPath == p || strings.HasPrefix(importPath, p+"/") {
			return true
		}
	}
	return false
}
