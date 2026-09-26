package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// cmdDir and repoRoot are resolved relative to the process working directory,
// matching the `go -C <this-module>` convention every call site uses.
const (
	cmdDir   = "../../cmd"
	repoRoot = "../.."
)

// legacyTagEscape exempts a file from the retired-tag-form guard. Prose that
// legitimately discusses the old convention — a release note, an ADR, a design
// doc explaining what changed — should not have to choose between being accurate
// and passing CI, because the easy way out of that is to weaken the guard.
const legacyTagEscape = "allow-legacy-plugin-tag"

// blankPluginImport matches a blank import of a plugin package. A regexp rather
// than an AST walk: it matches inside a grouped import block as well as a single
// one, since it does not anchor on the `import` keyword. If the registration
// shape ever loosens beyond `_ "<path>"`, switch to go/ast — see
// core/plugins/injection_coverage_test.go for the pattern.
var blankPluginImport = regexp.MustCompile(`_\s+"github\.com/rossoctl/cortex/core/plugins/(\w+)"`)

// profileInvocations match the shapes a call site uses to name a profile. All
// three deliberately require the name to start with a letter, so a shell
// indirection (`"${profile}"`, `"$1"`) does not match — those values live in
// loops this cannot evaluate, and the loop's literal list is caught by the third
// pattern instead.
var profileInvocations = []*regexp.Regexp{
	// Direct: go -C .../profile-tags run . full
	regexp.MustCompile(`profile-tags run \. "?([a-z][a-z0-9_]*)`),
	// Via the shell helper both workflows define: profile_tags full
	regexp.MustCompile(`profile_tags ([a-z][a-z0-9_]*)`),
	// The loop list in ci.yaml: profiles="local full lite"
	regexp.MustCompile(`profiles="([a-z0-9_ ]+)"`),
}

// skipDir reports directories no guard should descend into. .worktrees matters
// most: sibling worktrees hold other branches, and scanning them would fail this
// module's tests based on code that is not in this tree.
func skipDir(name string) bool {
	switch name {
	case ".git", "vendor", "node_modules", ".worktrees", ".venv":
		return true
	}
	return false
}

// buildFile reports whether a file can plausibly carry a build tag or name a
// profile: the workflow, shell, make and container surfaces that drive builds.
func buildFile(name string) bool {
	switch filepath.Ext(name) {
	case ".yaml", ".yml", ".sh":
		return true
	}
	return name == "Makefile" || strings.HasPrefix(name, "Dockerfile")
}

// scannedFile reports whether the retired-tag guard should read a file. It covers
// buildFile's surfaces plus Go sources and prose: a stale tag in a Dockerfile is
// a silent no-op, and a stale tag in documentation is a wrong instruction.
func scannedFile(name string) bool {
	switch filepath.Ext(name) {
	case ".go", ".md":
		return true
	}
	return buildFile(name)
}

// TestNoExcludePluginTagsRemain guards the convention itself. Under all-opt-in
// nothing links by default, so `exclude_plugin_*` has no meaning — but Go does
// NOT error on an unsatisfied build tag, so a leftover `-tags exclude_plugin_x`
// is a silent no-op rather than a build failure. Anything still naming the old
// form is either dead or actively lying about what it excludes.
func TestNoExcludePluginTagsRemain(t *testing.T) {
	// Assembled at runtime so this test file does not match itself.
	needle := "exclude" + "_plugin_"
	var hits []string
	err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !scannedFile(d.Name()) {
			return nil
		}
		if strings.HasSuffix(path, "guards_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		text := string(data)
		if strings.Contains(text, legacyTagEscape) {
			return nil
		}
		if strings.Contains(text, needle) {
			hits = append(hits, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(hits) > 0 {
		t.Errorf("%d file(s) still reference the removed tag form %q:\n  %s\n\n"+
			"If a file legitimately discusses the old convention (release note, ADR, "+
			"design history), add the marker %q to it rather than relaxing this guard.",
			len(hits), needle, strings.Join(hits, "\n  "), legacyTagEscape)
	}
}

// TestEveryPluginFileIsTagged closes the gap that would reintroduce the very bug
// this convention removes. A file named plugins_foo.go with a blank import and NO
// build directive is skipped by the unconditional-import guard (it matches the
// plugins_ prefix) and invisible to discoverPlugins (no directive to find), so it
// links its plugin into every artifact unconditionally and nothing reports it.
func TestEveryPluginFileIsTagged(t *testing.T) {
	files, err := pluginFiles(cmdDir)
	if err != nil {
		t.Fatalf("pluginFiles: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no plugins_*.go files found — the registration convention changed " +
			"and this guard went blind")
	}
	for _, path := range files {
		name, err := extractIncludeSuffix(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if name == "" {
			t.Errorf("%s has no `//go:build include_plugin_<name>` directive, so its "+
				"plugin links unconditionally into every profile", path)
		}
	}
}

// TestNoUnconditionalPluginImports guards the hole that shipped in
// authbridge-envoy and authbridge-cpex: both blank-imported plugin packages
// straight from main.go, so those plugins could not be excluded by any tag and
// no error said so. Every plugin must enter through a tagged plugins_*.go file.
func TestNoUnconditionalPluginImports(t *testing.T) {
	var hits []string
	entries, err := os.ReadDir(cmdDir)
	if err != nil {
		t.Fatalf("read %s: %v", cmdDir, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		goFiles, err := filepath.Glob(filepath.Join(cmdDir, e.Name(), "*.go"))
		if err != nil {
			t.Fatalf("glob: %v", err)
		}
		for _, f := range goFiles {
			base := filepath.Base(f)
			// plugins_*.go files are the sanctioned entry point; that they actually
			// carry a directive is TestEveryPluginFileIsTagged's job.
			if strings.HasPrefix(base, "plugins_") || strings.HasSuffix(base, "_test.go") {
				continue
			}
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			for _, m := range blankPluginImport.FindAllStringSubmatch(string(data), -1) {
				hits = append(hits, f+" imports "+m[1])
			}
		}
	}
	if len(hits) > 0 {
		t.Errorf("%d unconditional plugin import(s) outside a tagged plugins_*.go:\n  %s",
			len(hits), strings.Join(hits, "\n  "))
	}
}

// reservedFileSuffixes are the GOOS and GOARCH tokens Go treats as an implicit
// build constraint when they appear as a filename suffix.
var reservedFileSuffixes = map[string]bool{
	"aix": true, "android": true, "darwin": true, "dragonfly": true,
	"freebsd": true, "hurd": true, "illumos": true, "ios": true, "js": true,
	"linux": true, "netbsd": true, "openbsd": true, "plan9": true,
	"solaris": true, "wasip1": true, "windows": true, "zos": true,
	"386": true, "amd64": true, "arm": true, "arm64": true, "loong64": true,
	"mips": true, "mips64": true, "mips64le": true, "mipsle": true,
	"ppc64": true, "ppc64le": true, "riscv": true, "riscv64": true,
	"s390x": true, "sparc": true, "sparc64": true, "wasm": true,
}

// TestNoPluginFileShadowedByGOOSGOARCH guards a silent drop that Go itself
// causes. A file named plugins_sparc.go carries an implicit GOARCH=sparc
// constraint, so it compiles on no normal machine — the plugin vanishes with no
// build error, and a tag-scanning guard still "finds" it because the directive is
// right there in the source. cmd/authbridge-proxy already worked around this by
// naming its file plugins_sparcplugin.go; nothing enforced it until now.
func TestNoPluginFileShadowedByGOOSGOARCH(t *testing.T) {
	files, err := pluginFiles(cmdDir)
	if err != nil {
		t.Fatalf("pluginFiles: %v", err)
	}
	for _, path := range files {
		base := strings.TrimSuffix(filepath.Base(path), ".go")
		suffix := base[strings.LastIndex(base, "_")+1:]
		if reservedFileSuffixes[suffix] {
			t.Errorf("%s: filename suffix %q is a GOOS/GOARCH token, so Go excludes "+
				"this file on every other platform and the plugin is silently dropped; "+
				"rename it (e.g. plugins_%splugin.go)", path, suffix, suffix)
		}
	}
}

// TestEveryPluginIsAccountedFor is the guard against a plugin silently vanishing
// from production. Under opt-out, a new plugin reached every artifact for free;
// under opt-in a missing membership entry means it reaches none, and nothing
// fails. Every plugin discovered in the tree must have an entry — one listing
// profiles, or an empty one marking it deliberately optional.
func TestEveryPluginIsAccountedFor(t *testing.T) {
	found, err := discoverPlugins(cmdDir)
	if err != nil {
		t.Fatalf("discoverPlugins: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("no plugins discovered — the build-tag convention changed and this guard went blind")
	}
	for _, n := range found {
		if _, ok := membership[n]; !ok {
			t.Errorf("plugin %q has no membership entry: name the profiles that carry "+
				"it, or give it an empty entry to mark it deliberately optional", n)
		}
	}
}

// TestOptionalSetIsExplicit pins which plugins deliberately ship in no artifact.
// TestEveryPluginIsAccountedFor only requires an entry to exist, so a plugin
// whose last profile is removed still passes there while quietly disappearing
// from every image. Shipping nothing has to be a decision recorded here, not a
// side effect of editing one profile.
func TestOptionalSetIsExplicit(t *testing.T) {
	want := map[PluginName]bool{
		"contextguru":   true, // ~16 MiB: bifrost/core, tiktoken-go, tree-sitter, starlark
		"sessionbudget": true, // ~6 MiB: go-redis
	}
	for plugin := range membership {
		switch {
		case isOptional(plugin) && !want[plugin]:
			t.Errorf("plugin %q is carried by no profile, so it ships in no artifact. "+
				"If that is intended, add it to this test's want set; otherwise name "+
				"the profiles that carry it", plugin)
		case !isOptional(plugin) && want[plugin]:
			t.Errorf("plugin %q is expected to be optional but now names profiles %v; "+
				"remove it from this test's want set if that is intended",
				plugin, membership[plugin])
		}
	}
}

// TestNoStaleMembershipEntries is the mirror: an entry naming a plugin that no
// longer exists yields `-tags include_plugin_gone`, which Go accepts silently, so
// the artifact quietly ships without it.
func TestNoStaleMembershipEntries(t *testing.T) {
	found, err := discoverPlugins(cmdDir)
	if err != nil {
		t.Fatalf("discoverPlugins: %v", err)
	}
	exists := map[PluginName]bool{}
	for _, n := range found {
		exists[n] = true
	}
	for plugin := range membership {
		if !exists[plugin] {
			t.Errorf("membership names %q, which has no plugins_%s.go anywhere under %s",
				plugin, plugin, cmdDir)
		}
	}
}

// TestMembershipNamesKnownProfiles catches a typo in a membership entry. Because
// Tags filters rather than looks up, an unknown profile name there would not
// error — it would simply carry the plugin nowhere.
func TestMembershipNamesKnownProfiles(t *testing.T) {
	for plugin, carriedBy := range membership {
		for _, p := range carriedBy {
			if !isKnownProfile(p) {
				t.Errorf("plugin %q names profile %q, which is not defined (known: %v)",
					plugin, p, known())
			}
		}
	}
}

// TestEveryProfileCarriesSomething — a profile no plugin names resolves to an
// empty tag list. Tags fails closed on that, but only when something asks for it;
// this reports it at test time instead of mid-build.
func TestEveryProfileCarriesSomething(t *testing.T) {
	for _, p := range allProfiles {
		if _, err := Tags(p); err != nil {
			t.Errorf("profile %q: %v", p, err)
		}
	}
}

// TestCallSitesUseKnownProfiles guards the failure this refactor actually hit:
// the tag-form guard passed while .github/workflows/ci.yaml still invoked the
// deleted generator, because a broken call site contains no build tag to find. A
// profile name is a string in YAML and shell — a typo or a rename produces a
// non-zero exit deep in a build, or worse an empty tag list.
func TestCallSitesUseKnownProfiles(t *testing.T) {
	checked := 0
	// Walk the whole repository, not just its top level. A stale call site is
	// exactly what this guard exists for, and one can live in a nested Makefile,
	// scripts/*.sh or Dockerfile as easily as in .github/workflows.
	err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !buildFile(d.Name()) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, re := range profileInvocations {
			for _, m := range re.FindAllStringSubmatch(string(data), -1) {
				for _, name := range strings.Fields(m[1]) {
					checked++
					if !isKnownProfile(ProfileName(name)) {
						t.Errorf("%s names profile %q, which is not defined (known: %v)",
							path, name, known())
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if checked == 0 {
		t.Error("no profile-tags call sites found — either the invocation shape changed " +
			"or CI stopped resolving profiles, and this guard went blind")
	}
}
