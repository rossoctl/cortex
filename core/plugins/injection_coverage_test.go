package plugins

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryBinaryInjectsPricing guards the gap that shipped in this PR's first round:
// only cmd/authbridge-proxy was wired, so authbridge-envoy and authbridge-cpex
// injected no resolver at all.
//
// That was a zero-config REGRESSION rather than a missing feature. tool-prune is
// linked into those binaries by default and used to carry its own rate table, so
// `$ saved` worked with no configuration everywhere. Worse, config.Validate builds
// the pricing table and discards it, so an operator's `pricing:` block validated
// cleanly in envoy mode and was then never applied — reporting as "nothing is
// priced", indistinguishable from having configured nothing.
//
// Parsed rather than substring-matched, for three reasons the first version of this
// test got wrong: a substring scan over concatenated source also matched comments
// and _test.go files; it hardcoded the variable name `pricingRegistry`, so a correct
// binary naming it otherwise would fail; and checking merely that Swap appears
// somewhere was satisfied by a boot-time call, so a binary that swapped once and
// never on reload passed with permanently frozen rates.
func TestEveryBinaryInjectsPricing(t *testing.T) {
	root := filepath.Join("..", "..", "cmd")
	entries, err := os.ReadDir(root)
	if err != nil {
		// NOT a skip. A skip here passes vacuously in any job that tests core
		// alone, which is exactly the job most likely to run.
		t.Fatalf("cannot reach cmd/ from this module, so this guard cannot run: %v", err)
	}
	var checked int
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "abctl" { // abctl is a client, no pipelines
			continue
		}
		files, err := filepath.Glob(filepath.Join(root, e.Name(), "*.go"))
		if err != nil || len(files) == 0 {
			continue
		}
		facts := scanBinary(t, files)
		if !facts.buildsPipelines {
			continue
		}
		checked++
		t.Run(e.Name(), func(t *testing.T) {
			if facts.usesBuildWithSPIFFE {
				t.Errorf("calls plugins.BuildWithSPIFFE, which injects no pricing resolver; " +
					"use BuildWithDeps with Deps{SPIFFE: ..., Pricing: ...}")
			}
			if !facts.constructsRegistry {
				t.Errorf("builds pipelines but never calls pricing.NewRegistry, so every request " +
					"it serves is unpriced and any `pricing:` config is silently ignored")
			}
			if !facts.passesPricingDep {
				t.Errorf("never sets the Pricing field of plugins.Deps, so the resolver never " +
					"reaches a plugin")
			}
			// The reload half. A Swap at top level of main() is the boot-time call; the
			// one that matters is inside the closure the reloader re-invokes.
			if !facts.swapsInsideAClosure {
				t.Errorf("only swaps the pricing table at top level, never inside the " +
					"pipeline-building closure, so rates would be frozen at boot and a " +
					"config reload would silently keep the old prices")
			}
		})
	}
	if checked == 0 {
		t.Fatal("found no pipeline-building binaries; the detector is broken, not the tree")
	}
}

type binaryFacts struct {
	buildsPipelines     bool
	usesBuildWithSPIFFE bool
	constructsRegistry  bool
	passesPricingDep    bool
	swapsInsideAClosure bool
}

// scanBinary reads the facts off the AST, so comments and string literals cannot
// satisfy any of them.
func scanBinary(t *testing.T, files []string) binaryFacts {
	t.Helper()
	var f binaryFacts
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			// A file behind a build tag this platform does not select still parses;
			// a genuine syntax error is worth failing on.
			t.Fatalf("parse %s: %v", path, err)
		}
		// closureDepth tracks whether we are inside a FuncLit, which is what the
		// reloader re-invokes.
		var closureDepth int
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncLit:
				closureDepth++
				ast.Inspect(node.Body, func(inner ast.Node) bool {
					if isSelectorCall(inner, "Swap") {
						f.swapsInsideAClosure = true
					}
					return true
				})
				closureDepth--
				return true
			case *ast.CallExpr:
				if isQualifiedCall(node, "plugins", "BuildWithDeps") ||
					isQualifiedCall(node, "plugins", "Build") {
					f.buildsPipelines = true
				}
				if isQualifiedCall(node, "plugins", "BuildWithSPIFFE") {
					f.buildsPipelines = true
					f.usesBuildWithSPIFFE = true
				}
				if isQualifiedCall(node, "pricing", "NewRegistry") {
					f.constructsRegistry = true
				}
			case *ast.KeyValueExpr:
				if id, ok := node.Key.(*ast.Ident); ok && id.Name == "Pricing" {
					f.passesPricingDep = true
				}
			}
			return true
		})
	}
	return f
}

func isQualifiedCall(call *ast.CallExpr, pkg, name string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}

// isSelectorCall matches any receiver, so the registry variable's name is not part
// of the contract.
func isSelectorCall(n ast.Node, name string) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == name
}
