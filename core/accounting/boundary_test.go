package accounting

import (
	"os/exec"
	"strings"
	"testing"
)

// forbiddenModule is the module core/accounting must never depend on (global-constraints.md,
// design §5.2): accounting is a Cortex feature, not a context-guru feature, and context-guru
// depends on core/accounting, never the other way around.
const forbiddenModule = "github.com/rossoctl/context-guru"

// TestNoContextGuruDependency runs go list -deps over this package tree and fails on any
// dependency rooted at forbiddenModule. This is accountant's own boundary_test.go pattern
// (TestImportsStandardLibraryOnly), aimed at the other side of the boundary: accountant pins "we
// import only the standard library"; Cortex's accounting host pins "we never import
// context-guru".
func TestNoContextGuruDependency(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	out, err := exec.Command("go", "list", "-deps", "./...").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == forbiddenModule || strings.HasPrefix(line, forbiddenModule+"/") {
			t.Errorf("core/accounting depends on %q", line)
		}
	}
}
