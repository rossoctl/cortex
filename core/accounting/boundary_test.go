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

// TestNoContextGuruDependency holds two lines. Every package core/accounting/... imports
// DIRECTLY must be in allowedModules (or the standard library): that is the reviewed set. And
// nothing in the whole dependency closure may be context-guru.
//
// The two are checked over different sets on purpose. The host needs core/cost/pricing and
// core/session (Options.Resolver, Options.CostRecorders), and their own dependencies (gobwas/glob,
// yaml.v3) are core's, already reviewed where core takes them; checking the closure against the
// allowlist would make every dependency any core package ever adds fail here. Context-guru is the
// one dependency forbidden at any depth, so the closure is checked for it alone.
func TestNoContextGuruDependency(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		// Fatal, not Skip (review finding): this test enforces the PR's headline boundary claim
		// (core/accounting never imports context-guru), and a silent skip would let that
		// invariant vanish in a toolchain-less environment without anyone noticing.
		t.Fatalf("go toolchain not on PATH: %v", err)
	}
	direct, err := exec.Command("go", "list", "-f", `{{join .Imports "\n"}}{{"\n"}}{{join .TestImports "\n"}}`, "./...").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	std, err := exec.Command("go", "list", "std").Output()
	if err != nil {
		t.Fatalf("go list std: %v", err)
	}
	isStd := map[string]bool{}
	for _, p := range strings.Fields(string(std)) {
		isStd[p] = true
	}
	for _, line := range strings.Fields(string(direct)) {
		if isStd[line] || forbidden(line) {
			continue // forbidden ones are reported from the closure below
		}
		if !allowedModule(line) {
			t.Errorf("core/accounting imports %q: not in the allowed set %v", line, allowedModules)
		}
	}
	closure, err := exec.Command("go", "list", "-deps", "-test", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", "./...").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	for _, line := range strings.Fields(string(closure)) {
		if forbidden(line) {
			t.Errorf("core/accounting depends on %q: context-guru is the one dependency this boundary exists to forbid", line)
		}
	}
}

func forbidden(importPath string) bool {
	return importPath == forbiddenModule || strings.HasPrefix(importPath, forbiddenModule+"/")
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
