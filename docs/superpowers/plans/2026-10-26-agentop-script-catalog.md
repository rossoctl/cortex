# Bundled script plugins and `agentop script` — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship Cortex's script plugins inside the `agentop` binary, and install or remove one in a single step with `agentop script`.

**Architecture:** Three changes, each a task:
- a new package `core/plugins/script/catalog` embeds `entries/<name>/` folders, each holding scripts and an `entry.yaml`;
- agentop's config editor learns to pick one of several same-named entries by a config value, and to remove a whole entry;
- `agentop script list|install|remove` uses both, through the existing `edit.WritePluginConfig`, which keeps comments, waits for the reload and rolls back.

**Tech Stack:** Go 1.26.10, `embed`, `gopkg.in/yaml.v3`, agentop's `edit` and `apiclient` packages; pytest for the moved script.

**Spec:** `docs/superpowers/specs/2026-10-26-agentop-script-catalog-design.md`

## Global Constraints

- Work only in `.worktrees/agentop-script`, branch `feat/agentop-script`. Never touch the top-level checkout.
- **Do not restart or reconfigure the shared Cortex** on `:47600`: no `make dev-install`, no `agentop service restart`, no `agentop script install` without `HOME` set to a scratch folder.
- Go commands run per module with `GOWORK=off`.
- No new Go dependencies. `gopkg.in/yaml.v3` is already in `core/go.mod`.
- The catalog package imports nothing from `core/plugins/script`, so agentop links no plugin code.
- Catalog entry names are slot `dir`s: one plain path element, not starting with `.` or `_`.
- `list --json`'s field names are a contract: `name`, `description`, `state`, `failClosed`, `scripts`, `loaded`, `notStarted`, under a top-level `scripts` array.
- States are exactly: `not installed`, `installed`, `outdated`, `modified`, `incomplete`.
- The install record is `~/.cortex/plugins/<name>/.agentop-install.json`.
- Files written by install: scripts mode 0700, other files 0600, folder 0700. `entry.yaml` is never copied.
- Exit codes: 0 success or nothing to do; 1 a failure after arguments parsed; 2 usage error or unknown name.
- Every commit: `git commit -s`, ending with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. Never `Co-Authored-By`.
- Prose: say "Cortex" for the product. Docs are short, problem first, lists over paragraphs.
- Do not push or open a PR without asking.

## Review Focus

Failure modes the spec implies but no test exercises by name. Each has a test in the task named.

1. **A pytest run leaves `__pycache__` in a catalog folder,** and `all:` embeds it, breaking the catalog for every user. The catalog must skip it. Task 1 (found in the dry run).
2. **A user's own folder already named `moca-sandbox`.** Install must refuse rather than mix its files with the user's. Task 3.
3. **A user's extra script dropped into an installed slot.** Remove must refuse and leave both the file and the slot. Task 3.
4. **A refused reload.** Install must put the config back and delete the files it wrote. Task 3.
5. **A bare `- script` entry in the chain.** Matching by `dir` must skip it, not refuse the whole chain as "written as a bare name". Task 2.

---

### Task 1: The catalog package, with moca-sandbox moved into it

**Files:**
- Move: `examples/script-plugins/moca-sandbox.py` → `core/plugins/script/catalog/entries/moca-sandbox/moca-sandbox.py` (`git mv`, keeps mode 100755)
- Create: `core/plugins/script/catalog/entries/moca-sandbox/entry.yaml`
- Create: `core/plugins/script/catalog/catalog.go`
- Test: `core/plugins/script/catalog/catalog_test.go`, `core/plugins/script/catalog_names_test.go`
- Modify: `tests/test_example_moca_sandbox.py` (two path lines only; its docs test changes in Task 4)

**Interfaces:**
- Produces, in package `catalog`:
  - `type CatalogEntry struct { Name, Description string; FailClosed bool; Timeout string; Files map[string][]byte }`
  - `func (e CatalogEntry) Scripts() []string`: the file names a slot would run, sorted.
  - `func Catalog() ([]CatalogEntry, error)`: every entry, sorted by name.

- [ ] **Step 1: Move the script and add its metadata**

```bash
mkdir -p core/plugins/script/catalog/entries/moca-sandbox
git mv examples/script-plugins/moca-sandbox.py core/plugins/script/catalog/entries/moca-sandbox/moca-sandbox.py
cat > core/plugins/script/catalog/entries/moca-sandbox/entry.yaml <<'EOF'
description: Keep Claude Code on moca's sandbox tools without a project settings file
fail_closed: true
EOF
git ls-files -s core/plugins/script/catalog/entries/moca-sandbox/moca-sandbox.py   # must start with 100755
```

In `tests/test_example_moca_sandbox.py`, change the first two references:

```python
"""Tests for the moca-sandbox catalog entry, core/plugins/script/catalog/entries/moca-sandbox/."""
```

```python
SCRIPT = REPO / "core" / "plugins" / "script" / "catalog" / "entries" / "moca-sandbox" / "moca-sandbox.py"
```

Run: `pytest tests/test_example_moca_sandbox.py -q`
Expected: 14 passed, 1 failed. `test_the_guides_install_steps_match_the_script` still reads the old docs; Task 4 fixes it.

- [ ] **Step 2: Write the failing tests**

Create `core/plugins/script/catalog/catalog_test.go`:

```go
package catalog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Every entry Cortex ships loads, and adding a script is adding a folder: this
// test covers each one without being told about it.
func TestCatalog_EveryEntryIsInstallable(t *testing.T) {
	entries, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("the catalog is empty")
	}
	for _, e := range entries {
		t.Run(e.Name, func(t *testing.T) {
			if _, ok := e.Files[entryFile]; ok {
				t.Errorf("%s is in Files; it is metadata, not part of the slot", entryFile)
			}
			for _, name := range e.Scripts() {
				if !bytes.HasPrefix(e.Files[name], []byte("#!")) {
					t.Errorf("%s does not start with #!, so it cannot run as a script", name)
				}
			}
		})
	}
}

// What Python writes beside a script is not part of the entry, so a test run in
// a checkout cannot break the catalog the binary embeds.
func TestCatalog_SkipsWhatPythonGenerates(t *testing.T) {
	for _, name := range []string{"__pycache__", "x.cpython-312.pyc"} {
		if !generated(name) {
			t.Errorf("%s is not skipped", name)
		}
	}
	for _, name := range []string{"moca-sandbox.py", "_helper.py", "entry.yaml"} {
		if generated(name) {
			t.Errorf("%s is skipped", name)
		}
	}
}

// moca-sandbox is the entry moca installs, so its name and slot settings are a
// contract.
func TestCatalog_MocaSandbox(t *testing.T) {
	entries, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name != "moca-sandbox" {
			continue
		}
		if !e.FailClosed || e.Timeout != "" {
			t.Errorf("moca-sandbox slot = fail_closed %v, timeout %q; want fail_closed and the default timeout", e.FailClosed, e.Timeout)
		}
		if got := e.Scripts(); len(got) != 1 || got[0] != "moca-sandbox.py" {
			t.Errorf("scripts = %v", got)
		}
		return
	}
	t.Fatal("no moca-sandbox entry")
}

// Each script sends a valid handshake when run with the SDK on its path. Skipped
// where there is no python3, as TestPythonScriptThroughTheSDK is.
func TestCatalog_EveryScriptHandshakes(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3 on PATH")
	}
	// The SDK, from its source in the repo: this package must not import the
	// plugin that embeds it.
	sdk, err := filepath.Abs(filepath.Join("..", "sdk"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		dir := t.TempDir()
		for name, b := range e.Files {
			if err := os.WriteFile(filepath.Join(dir, name), b, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		for _, name := range e.Scripts() {
			t.Run(e.Name+"/"+name, func(t *testing.T) {
				cmd := exec.Command(py, filepath.Join(dir, name))
				cmd.Env = append(os.Environ(), "PYTHONPATH="+sdk)
				cmd.Stdin = bytes.NewReader(nil)
				out, err := cmd.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				defer cmd.Process.Kill()
				line := make(chan []byte, 1)
				go func() {
					s := bufio.NewScanner(out)
					if s.Scan() {
						line <- append([]byte(nil), s.Bytes()...)
					}
					close(line)
				}()
				select {
				case b := <-line:
					var hs struct {
						Cortex int      `json:"cortex"`
						Hooks  []string `json:"hooks"`
					}
					if err := json.Unmarshal(b, &hs); err != nil || hs.Cortex != 1 || len(hs.Hooks) == 0 {
						t.Fatalf("handshake %q: %v", b, err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("no handshake within 10s")
				}
			})
		}
	}
}
```

Create `core/plugins/script/catalog_names_test.go`:

```go
package script

import (
	"testing"

	"github.com/rossoctl/cortex/core/plugins/script/catalog"
)

// Every catalog entry's name is a dir this plugin accepts, so an installed entry's
// slot loads. The catalog checks names by its own copy of the rule; this holds the
// copy to SlotDir.
func TestCatalog_NamesAreSlotDirs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	entries, err := catalog.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if _, err := SlotDir(e.Name); err != nil {
			t.Errorf("%s: %v", e.Name, err)
		}
	}
}
```

- [ ] **Step 3: Run them to verify they fail**

Run: `cd core && GOWORK=off go test -count=1 ./plugins/script/catalog/`
Expected: FAIL to compile: `undefined: Catalog`.

- [ ] **Step 4: Implement**

Create `core/plugins/script/catalog/catalog.go`:

```go
// Package catalog is the script plugins Cortex ships: each folder under entries/
// is one entry, which installed is one slot. It imports nothing from the script
// plugin, so agentop can read it without linking the plugin in.
package catalog

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// catalogFS holds the scripts Cortex ships. `all:` keeps the _-prefixed helper
// files an entry may hold, which embed would otherwise leave out.
//
//go:embed all:entries
var catalogFS embed.FS

// entryFile is each catalog entry's metadata file. It is not a script: a slot
// skips it because it is not executable, and installing an entry does not copy it.
const entryFile = "entry.yaml"

// CatalogEntry is one script plugin Cortex ships. Installed, it is one slot: its
// files go to ~/.cortex/plugins/<Name>/, and the slot's settings are its own.
type CatalogEntry struct {
	Name        string
	Description string
	FailClosed  bool
	// Timeout is the slot's timeout, "" for the plugin's default.
	Timeout string
	// Files is every file of the entry but entry.yaml, by name.
	Files map[string][]byte
}

// Scripts is the names of the files a slot would run, in the order it would run
// them: the ones a slot does not skip by name.
func (e CatalogEntry) Scripts() []string {
	var out []string
	for name := range e.Files {
		if !strings.HasPrefix(name, ".") && !strings.HasPrefix(name, "_") {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

type entryMeta struct {
	Description string `yaml:"description"`
	FailClosed  bool   `yaml:"fail_closed"`
	Timeout     string `yaml:"timeout"`
}

// generated reports whether name is something Python leaves beside a script it
// imported or tested, rather than part of the entry. `all:` embeds whatever is in
// the folder, and a pytest run in a checkout writes __pycache__ there.
func generated(name string) bool {
	return name == "__pycache__" || strings.HasSuffix(name, ".pyc")
}

// validName is a folder name a slot's dir accepts: one plain path element. It is
// the script plugin's SlotDir rule, restated so this package needs nothing from it;
// TestCatalog_NamesAreSlotDirs in the plugin's package holds the two together.
func validName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\`) &&
		!strings.HasPrefix(name, ".") && !strings.HasPrefix(name, "_")
}

// Catalog is every entry Cortex ships, sorted by name. An error means the binary
// was built with a broken catalog, which the catalog's own test refuses.
func Catalog() ([]CatalogEntry, error) {
	dirs, err := fs.ReadDir(catalogFS, "entries")
	if err != nil {
		return nil, err
	}
	var out []CatalogEntry
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		e, err := readEntry(d.Name())
		if err != nil {
			return nil, fmt.Errorf("script catalog: %s: %w", d.Name(), err)
		}
		out = append(out, e)
	}
	return out, nil
}

func readEntry(name string) (CatalogEntry, error) {
	e := CatalogEntry{Name: name, Files: map[string][]byte{}}
	if !validName(name) {
		return e, fmt.Errorf("%q is not a name a slot's dir accepts", name)
	}
	dir := path.Join("entries", name)
	files, err := fs.ReadDir(catalogFS, dir)
	if err != nil {
		return e, err
	}
	var meta entryMeta
	haveMeta := false
	for _, f := range files {
		if generated(f.Name()) {
			continue
		}
		if f.IsDir() {
			return e, fmt.Errorf("%s is a folder; a slot runs only the files directly in its own", f.Name())
		}
		b, err := catalogFS.ReadFile(path.Join(dir, f.Name()))
		if err != nil {
			return e, err
		}
		if f.Name() == entryFile {
			dec := yaml.NewDecoder(strings.NewReader(string(b)))
			dec.KnownFields(true)
			if err := dec.Decode(&meta); err != nil {
				return e, fmt.Errorf("%s: %w", entryFile, err)
			}
			haveMeta = true
			continue
		}
		e.Files[f.Name()] = b
	}
	switch {
	case !haveMeta:
		return e, fmt.Errorf("no %s", entryFile)
	case strings.TrimSpace(meta.Description) == "":
		return e, fmt.Errorf("%s has no description", entryFile)
	}
	if meta.Timeout != "" {
		if d, err := time.ParseDuration(meta.Timeout); err != nil || d <= 0 {
			return e, fmt.Errorf("%s: timeout %q is not a positive duration", entryFile, meta.Timeout)
		}
	}
	e.Description, e.FailClosed, e.Timeout = meta.Description, meta.FailClosed, meta.Timeout
	if len(e.Scripts()) == 0 {
		return e, fmt.Errorf("no script: every file is a helper")
	}
	return e, nil
}
```

- [ ] **Step 5: Run the tests**

Run: `cd core && GOWORK=off go vet ./plugins/script/... && GOWORK=off go test -race -count=1 ./plugins/script/...`
Expected: PASS. `TestCatalog_EveryScriptHandshakes` runs where `python3` is on PATH and skips otherwise.

Run: `cd core && GOWORK=off go list -deps ./plugins/script/catalog | grep rossoctl`
Expected: exactly one line, `github.com/rossoctl/cortex/core/plugins/script/catalog`. Nothing of the plugin is linked in.

- [ ] **Step 6: Commit**

```bash
gofmt -l core/plugins/script   # must print nothing
git add -A core/plugins/script tests/test_example_moca_sandbox.py examples
git commit -s -m "feat: Embed Cortex's script plugins as a catalog" \
  -m "moca-sandbox moves from examples/ into core/plugins/script/catalog, each entry a folder with an entry.yaml naming its slot settings. A test covers every entry." \
  -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 2: The config editor picks a slot by its `dir`, and removes a whole entry

**Files:**
- Modify: `cmd/agentop/edit/pluginconfig.go` (`ConfigChange`, `SetPluginConfig`, `findEntry`, `appendEntry`, `renderKey`; new `BoolValue`, `configHolds`, `describeMatch`, `removeEntry`)
- Test: `cmd/agentop/edit/pluginconfig_match_test.go`

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces, in package `edit`:
  - `ConfigChange.Match map[string]string`: picks the entry named `Plugin` whose `config:` holds every key with that string value. A created entry gets these keys first in its `config:`.
  - `ConfigChange.RemoveEntry bool`: removes the whole matched entry. `Path` and `Value` are unused. An absent entry changes nothing.
  - `func BoolValue(b bool) *yaml.Node`: a value written as a bare `true`/`false`.

Behaviour, for review:
- Without `Match`, everything is as before, including the "two script entries" refusal.
- With `Match`, a bare `- script` entry is skipped: it has no `config:`, so it matches nothing.
- Two entries matching is still an error, now naming the match.
- Removing a chain's only entry leaves `[]`, so the file still loads.

- [ ] **Step 1: Write the failing tests**

Create `cmd/agentop/edit/pluginconfig_match_test.go`:

```go
package edit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/config"
)

// withSlots is localConfig with three script slots: the default one, a named one,
// and another named one, each with a comment of its own.
const withSlots = `# The running proxy watches this file.
mode: proxy-sidecar
stats:
  address: 127.0.0.1:47602
pipeline:
  outbound:
    plugins:
      - name: inference-parser
      - name: tool-prune
        config:
          remove: []
      - name: script
      # the guard slot
      - name: script
        config:
          dir: guard
          fail_closed: true
      # the moca slot
      - name: script
        config:
          dir: moca-sandbox
          fail_closed: true
session:
  client_affinity: true
`

// load parses src as Cortex does, from a file.
func load(t *testing.T, src string) (*config.Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.Load(p)
}

func slot(dir string) ConfigChange {
	return ConfigChange{Chain: "outbound", Plugin: "script", Match: map[string]string{"dir": dir}}
}

// Match picks one of several entries sharing a name, by their config.
func TestSetPluginConfig_MatchPicksOneOfSeveralEntries(t *testing.T) {
	ch := slot("guard")
	ch.Path, ch.Value = []string{"timeout"}, ScalarValue("5s")
	got := apply(t, withSlots, ch)
	want := strings.Replace(withSlots, "          dir: guard\n          fail_closed: true\n",
		"          dir: guard\n          fail_closed: true\n          timeout: 5s\n", 1)
	assertYAML(t, got, want)
}

// Without Match, several entries of one name are still refused, as before.
func TestSetPluginConfig_SeveralEntriesWithoutMatchAreRefused(t *testing.T) {
	_, err := SetPluginConfig([]byte(withSlots), ConfigChange{Chain: "outbound", Plugin: "script", Path: []string{"timeout"}, Value: ScalarValue("5s")})
	if err == nil || !strings.Contains(err.Error(), "two script entries") {
		t.Fatalf("err = %v, want the two-entries refusal", err)
	}
}

func TestSetPluginConfig_TwoEntriesMatchingIsRefused(t *testing.T) {
	src := strings.Replace(withSlots, "dir: moca-sandbox", "dir: guard", 1)
	ch := slot("guard")
	ch.Path, ch.Value = []string{"timeout"}, ScalarValue("5s")
	_, err := SetPluginConfig([]byte(src), ch)
	if err == nil || !strings.Contains(err.Error(), "match dir: guard") {
		t.Fatalf("err = %v, want a two-matches refusal naming the match", err)
	}
}

// A created entry carries Match's keys, then Path, and loads as the plugin
// expects: a bool written as a bool, not as the string "true".
func TestSetPluginConfig_CreateWritesTheMatchKeys(t *testing.T) {
	ch := slot("new-one")
	ch.Path, ch.Value, ch.CreatePlugin = []string{"fail_closed"}, BoolValue(true), true
	got := apply(t, withSlots, ch)
	want := strings.Replace(withSlots, "          dir: moca-sandbox\n          fail_closed: true\n",
		"          dir: moca-sandbox\n          fail_closed: true\n      - name: script\n        config:\n          dir: new-one\n          fail_closed: true\n", 1)
	assertYAML(t, got, want)
	cfg, err := load(t, got)
	if err != nil {
		t.Fatal(err)
	}
	last := cfg.Pipeline.Outbound.Plugins[len(cfg.Pipeline.Outbound.Plugins)-1]
	if string(last.Config) != `{"dir":"new-one","fail_closed":true}` {
		t.Errorf("created entry's config = %s", last.Config)
	}
}

// RemoveEntry takes the whole entry out, with the comments inside it, and leaves
// the comment above it and every other entry alone.
func TestSetPluginConfig_RemoveEntryTakesOutOneSlot(t *testing.T) {
	ch := slot("moca-sandbox")
	ch.RemoveEntry = true
	got := apply(t, withSlots, ch)
	want := strings.Replace(withSlots, "      - name: script\n        config:\n          dir: moca-sandbox\n          fail_closed: true\n", "", 1)
	assertYAML(t, got, want)
}

func TestSetPluginConfig_RemoveEntryOfAnAbsentOneChangesNothing(t *testing.T) {
	ch := slot("not-there")
	ch.RemoveEntry = true
	if got := apply(t, withSlots, ch); got != withSlots {
		t.Errorf("removing an absent entry changed the file:\n%s", got)
	}
}

// Removing a chain's only entry leaves an empty list, so the file still loads.
func TestSetPluginConfig_RemoveEntryOfTheOnlyEntryLeavesAnEmptyList(t *testing.T) {
	src := "mode: proxy-sidecar\npipeline:\n  outbound:\n    plugins:\n      - name: script\n        config:\n          dir: x\n"
	ch := slot("x")
	ch.RemoveEntry = true
	got := apply(t, src, ch)
	if got != "mode: proxy-sidecar\npipeline:\n  outbound:\n    plugins:\n      []\n" {
		t.Errorf("got:\n%s", got)
	}
	if _, err := load(t, got); err != nil {
		t.Errorf("the result does not load: %v", err)
	}
}

// A bare `- script` has no config:, so it matches nothing and is not refused.
func TestSetPluginConfig_MatchSkipsABareName(t *testing.T) {
	src := strings.Replace(withSlots, "      - name: script\n      # the guard slot", "      - script\n      # the guard slot", 1)
	ch := slot("guard")
	ch.RemoveEntry = true
	got := apply(t, src, ch)
	if !strings.Contains(got, "      - script\n") || strings.Contains(got, "dir: guard") {
		t.Errorf("got:\n%s", got)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `cd cmd/agentop && GOWORK=off go test -count=1 -run 'Match|RemoveEntry|SeveralEntries|TwoEntries|CreateWrites' ./edit/`
Expected: FAIL to compile: `unknown field Match in struct literal`.

- [ ] **Step 3: Implement**

Apply this change to `cmd/agentop/edit/pluginconfig.go`. Save it as `/tmp/pluginconfig.diff` and run `git apply /tmp/pluginconfig.diff` from the repo root, or make the same edits by hand:

```diff
--- a/cmd/agentop/edit/pluginconfig.go
+++ b/cmd/agentop/edit/pluginconfig.go
@@ -4,6 +4,7 @@
 	"errors"
 	"fmt"
 	"reflect"
+	"sort"
 	"strings"
 
 	"gopkg.in/yaml.v3"
@@ -28,6 +29,16 @@
 	// CreatePlugin appends an entry for Plugin at the end of Chain when the chain
 	// has none. Without it a missing entry is an error. A removal never creates one.
 	CreatePlugin bool
+	// Match, when set, picks the entry among those named Plugin whose config: holds
+	// every key in it with that string value, such as {"dir": "moca-sandbox"}: the
+	// way to address one of several entries sharing a name, as every script slot
+	// does. Two matches are still an error. A created entry gets Match's keys in its
+	// config:, ahead of Path.
+	Match map[string]string
+	// RemoveEntry removes the whole entry, comments inside it included, instead of
+	// editing its config:. Path and Value are then unused. An absent entry changes
+	// nothing.
+	RemoveEntry bool
 }
 
 // ScalarValue is a string value for ConfigChange.Value. It is written as a YAML
@@ -36,6 +47,15 @@
 	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
 }
 
+// BoolValue is a boolean value for ConfigChange.Value, written as true or false.
+func BoolValue(b bool) *yaml.Node {
+	v := "false"
+	if b {
+		v = "true"
+	}
+	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: v}
+}
+
 // MapValue is a mapping of string keys to string values, in the order given:
 // MapValue("url", u, "key", k). It panics on an odd number of arguments, which is a
 // programming error rather than input.
@@ -66,7 +86,7 @@
 // plugins list, written in flow style ({...} or [...]) is refused with an error
 // saying so; a value it replaces whole may be in any style.
 func SetPluginConfig(src []byte, ch ConfigChange) ([]byte, error) {
-	if len(ch.Path) == 0 {
+	if len(ch.Path) == 0 && !ch.RemoveEntry {
 		return nil, errors.New("edit: a config change needs at least one key")
 	}
 	// Refuse a src holding U+0085 (NEL), U+2028 (LS), U+2029 (PS), or a \r not
@@ -92,13 +112,17 @@
 	if err != nil {
 		return nil, err
 	}
-	item, err := findEntry(lines, plugins, ch.Plugin)
+	item, err := findEntry(lines, plugins, ch.Plugin, ch.Match)
 	if err != nil {
 		return nil, err
 	}
 
 	var out []string
 	switch {
+	case ch.RemoveEntry && item == nil:
+		return src, nil
+	case ch.RemoveEntry:
+		out = removeEntry(lines, plugins, item)
 	case item == nil && ch.Value == nil:
 		return src, nil
 	case item == nil && !ch.CreatePlugin:
@@ -132,11 +156,15 @@
 	return plugins, nil
 }
 
-// findEntry is the plugins list item named name, or nil when there is none.
-func findEntry(lines []string, plugins *yaml.Node, name string) (*yaml.Node, error) {
+// findEntry is the plugins list item named name whose config: holds match, or nil
+// when there is none.
+func findEntry(lines []string, plugins *yaml.Node, name string, match map[string]string) (*yaml.Node, error) {
 	var found *yaml.Node
 	for _, item := range plugins.Content {
 		if item.Kind == yaml.ScalarNode && item.Value == name {
+			if len(match) > 0 {
+				continue // a bare name has no config:, so it matches nothing
+			}
 			return nil, fmt.Errorf("the %s entry is written as a bare name; write it as `- name: %s` to give it config", name, name)
 		}
 		if item.Kind != yaml.MappingNode {
@@ -145,7 +173,13 @@
 		if _, n := mappingEntry(item, "name"); n == nil || n.Value != name {
 			continue
 		}
+		if !configHolds(item, match) {
+			continue
+		}
 		if found != nil {
+			if len(match) > 0 {
+				return nil, fmt.Errorf("two %s entries in one chain match %s; edit the config by hand", name, describeMatch(match))
+			}
 			return nil, fmt.Errorf("two %s entries in one chain; edit the config by hand", name)
 		}
 		if item.Style&yaml.FlowStyle != 0 || !strings.HasPrefix(strings.TrimSpace(lines[item.Line-1]), "-") {
@@ -154,10 +188,53 @@
 		found = item
 	}
 	return found, nil
+}
+
+// configHolds reports whether item's config: has each key of match set to that
+// string value. An empty match holds for every entry.
+func configHolds(item *yaml.Node, match map[string]string) bool {
+	if len(match) == 0 {
+		return true
+	}
+	_, cfg := mappingEntry(item, "config")
+	for k, want := range match {
+		_, v := mappingEntry(cfg, k)
+		if v == nil || v.Kind != yaml.ScalarNode || v.Value != want {
+			return false
+		}
+	}
+	return true
 }
 
+// describeMatch is match as `dir: x`, keys sorted, for errors.
+func describeMatch(match map[string]string) string {
+	keys := make([]string, 0, len(match))
+	for k := range match {
+		keys = append(keys, k)
+	}
+	sort.Strings(keys)
+	parts := make([]string, len(keys))
+	for i, k := range keys {
+		parts[i] = k + ": " + match[k]
+	}
+	return strings.Join(parts, ", ")
+}
+
+// removeEntry drops item's lines from the plugins list. It refuses nothing: an
+// entry the list holds can always be cut, since findEntry only returns one written
+// as `- name:` with its keys below. A chain left with no entries keeps its
+// plugins: key, with an empty list, so the file still loads.
+func removeEntry(lines []string, plugins *yaml.Node, item *yaml.Node) []string {
+	start := item.Line - 1
+	end := blockEnd(lines, start, plugins.Column-1, false)
+	if len(plugins.Content) == 1 {
+		return splice(lines, start, end+1, []string{pad(plugins.Column-1) + "[]"})
+	}
+	return splice(lines, start, end+1, nil)
+}
+
 // appendEntry adds `- name: <Plugin>` and its config: after the chain's last entry,
-// at the indentation that entry uses.
+// at the indentation that entry uses. Match's keys come first in config:.
 func appendEntry(lines []string, plugins *yaml.Node, ch ConfigChange) ([]string, error) {
 	dash := plugins.Column - 1
 	last := plugins.Content[len(plugins.Content)-1]
@@ -169,10 +246,26 @@
 	if err != nil {
 		return nil, err
 	}
-	body, err := renderPath(ch.Path, ch.Value, keys+2)
-	if err != nil {
-		return nil, err
+	var body []string
+	mkeys := make([]string, 0, len(ch.Match))
+	for k := range ch.Match {
+		mkeys = append(mkeys, k)
 	}
+	sort.Strings(mkeys)
+	for _, k := range mkeys {
+		line, err := renderPath([]string{k}, ScalarValue(ch.Match[k]), keys+2)
+		if err != nil {
+			return nil, err
+		}
+		body = append(body, line...)
+	}
+	if len(ch.Path) > 0 {
+		rest, err := renderPath(ch.Path, ch.Value, keys+2)
+		if err != nil {
+			return nil, err
+		}
+		body = append(body, rest...)
+	}
 	add := append([]string{
 		pad(dash) + "-" + pad(keys-dash-1) + "name: " + name,
 		pad(keys) + "config:",
@@ -272,6 +365,9 @@
 func renderKey(key string, value *yaml.Node, indent int) ([]string, error) {
 	switch value.Kind {
 	case yaml.ScalarNode:
+		if value.Tag == "!!bool" && (value.Value == "true" || value.Value == "false") {
+			return []string{pad(indent) + key + ": " + value.Value}, nil
+		}
 		s, err := scalarText(value.Value)
 		if err != nil {
 			return nil, err
```

- [ ] **Step 4: Run the package's tests**

Run: `cd cmd/agentop && GOWORK=off go vet ./edit/ && GOWORK=off go test -race -count=1 ./edit/`
Expected: PASS, the existing editor tests included.

- [ ] **Step 5: Commit**

```bash
gofmt -l cmd/agentop/edit   # must print nothing
git add cmd/agentop/edit/pluginconfig.go cmd/agentop/edit/pluginconfig_match_test.go
git commit -s -m "feat: Let agentop's config editor address one of several script slots" \
  -m "Every slot is named script, so an entry is now found by a config value such as its dir, and can be removed whole." \
  -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 3: `agentop script list|install|remove`

**Files:**
- Create: `cmd/agentop/cmd_script.go`
- Modify: `cmd/agentop/main.go` (dispatch, `dispatchableSubcommands`, root usage)
- Test: `cmd/agentop/cmd_script_test.go`

**Interfaces:**
- Consumes:
  - from Task 1: `catalog.Catalog() ([]catalog.CatalogEntry, error)`, `CatalogEntry.Scripts() []string`, and its `Name`, `Description`, `FailClosed`, `Timeout`, `Files`;
  - from Task 2: `edit.ConfigChange{…, Match, RemoveEntry}`, `edit.BoolValue`;
  - existing: `serverTarget(cfgPath)` (config, path, stats URL), `serverFlags`, `writePluginConfig` (the stubbable `edit.WritePluginConfig`), `dialURL`, `homeTilde`, `version`, `apiclient.New(url).GetPipeline(ctx)`;
  - in tests: `serverEnvWithSessions`, `closedAddr`, `readConfig` from the existing server tests.
- Produces: `runScript(args []string, stdout, stderr io.Writer) int`, wired as `agentop script`.

How the pieces fit, for review:
- **State** is read from three things: whether the slot is in the config (any `on_error`), whether the folder exists, and the install record against the files on disk and this release's catalog.
- **`install`** writes the files, then adds the slot through `WritePluginConfig`. With Cortex running, it polls `GET /v1/pipeline` for up to `scriptLoadWait` (10s) until the slot reports every script loaded, or one not started.
- **On failure** it takes back only what this run added: the slot if it added it, the folder if it created it.
- **A rollback after a refused reload** needs no slot removal: `WritePluginConfig` has already put the file back.

- [ ] **Step 1: Write the failing tests**

Create `cmd/agentop/cmd_script_test.go`:

```go
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCortex answers /reload/status as a proxy that reloads every write, and
// /v1/pipeline with a script slot for dir "moca-sandbox" whose counts are what the
// test set, or no such slot when present is false.
type fakeCortex struct {
	srv                *httptest.Server
	present            atomic.Bool
	loaded, notStarted atomic.Int32
	refuse             atomic.Bool
	polls              atomic.Int32
}

func newFakeCortex(t *testing.T) *fakeCortex {
	t.Helper()
	f := &fakeCortex{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/reload/status":
			// A refusal, as the poll recognises one: reloads_failed rising after the
			// poll's first answer, with last_success left behind the write. Poll 1 is
			// serverTarget's probe and poll 2 the poll's baseline, so it rises at 3.
			n := f.polls.Add(1)
			st := map[string]any{"last_success": time.Now(), "reloads_ok": 1, "reloads_failed": 0}
			if f.refuse.Load() {
				st["last_success"] = time.Now().Add(-time.Hour)
				if n >= 3 {
					st["reloads_failed"], st["last_error"] = 1, "refused by the test"
				}
			}
			_ = json.NewEncoder(w).Encode(st)
		case "/v1/pipeline":
			out := []map[string]any{{"name": "inference-parser"}}
			if f.present.Load() {
				out = append(out, map[string]any{"name": "script", "config": map[string]any{"dir": "moca-sandbox", "fail_closed": true},
					"metrics": []map[string]any{{"name": "scripts loaded", "value": f.loaded.Load()}, {"name": "scripts not started", "value": f.notStarted.Load()}}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"inbound": []any{}, "outbound": out})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCortex) addr() string { return strings.TrimPrefix(f.srv.URL, "http://") }

// scriptEnv is a scratch HOME with a config whose stats and session API are
// statsAddr and apiAddr, and returns the config path and ~/.cortex/plugins.
func scriptEnv(t *testing.T, statsAddr, apiAddr string) (cfg, plugins string) {
	t.Helper()
	path := serverEnvWithSessions(t, statsAddr, apiAddr, "")
	home := filepath.Dir(path)
	return path, filepath.Join(home, ".cortex", "plugins")
}

func runScriptCmd(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = runScript(args, &out, &errb)
	return code, out.String(), errb.String()
}

func listJSON(t *testing.T, cfg string) map[string]scriptStatus {
	t.Helper()
	code, out, errOut := runScriptCmd(t, "list", "--json", "--config", cfg)
	if code != 0 {
		t.Fatalf("list: exit %d: %s", code, errOut)
	}
	var v struct {
		Scripts []scriptStatus `json:"scripts"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("list --json: %v\n%s", err, out)
	}
	m := map[string]scriptStatus{}
	for _, s := range v.Scripts {
		m[s.Name] = s
	}
	return m
}

func TestScript_ListShowsTheCatalogNotInstalled(t *testing.T) {
	cfg, _ := scriptEnv(t, closedAddr(t), closedAddr(t))
	st, ok := listJSON(t, cfg)["moca-sandbox"]
	if !ok || st.State != stateNotInstalled || !st.FailClosed || st.Scripts != 1 || st.Loaded != nil {
		t.Fatalf("moca-sandbox = %+v", st)
	}
}

// list --json is read by other tools (moca), so its field names are a contract.
func TestScript_ListJSONFieldNames(t *testing.T) {
	f := newFakeCortex(t)
	f.present.Store(true)
	f.loaded.Store(1)
	cfg, _ := scriptEnv(t, f.addr(), f.addr())
	_, out, _ := runScriptCmd(t, "list", "--json", "--config", cfg)
	var v map[string][]map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatal(err)
	}
	row := v["scripts"][0]
	for _, k := range []string{"name", "description", "state", "failClosed", "scripts", "loaded", "notStarted"} {
		if _, ok := row[k]; !ok {
			t.Errorf("list --json has no %q: %v", k, row)
		}
	}
}

func TestScript_InstallWithNoProxyWritesFilesAndSlot(t *testing.T) {
	cfg, plugins := scriptEnv(t, closedAddr(t), closedAddr(t))
	code, out, errOut := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg)
	if code != 0 {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	if !strings.Contains(out, "Cortex is not running") {
		t.Errorf("out = %q", out)
	}
	info, err := os.Stat(filepath.Join(plugins, "moca-sandbox", "moca-sandbox.py"))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("script: %v %v", err, info)
	}
	if _, err := os.Stat(filepath.Join(plugins, "moca-sandbox", "entry.yaml")); err == nil {
		t.Error("entry.yaml was copied into the slot")
	}
	text := readConfig(t, cfg)
	if !strings.Contains(text, "      - name: script\n        config:\n          dir: moca-sandbox\n          fail_closed: true\n") {
		t.Errorf("config:\n%s", text)
	}
	if st := listJSON(t, cfg)["moca-sandbox"]; st.State != stateInstalled {
		t.Errorf("state after install = %s", st.State)
	}
	// Again: nothing to do.
	code, out, _ = runScriptCmd(t, "install", "moca-sandbox", "--config", cfg)
	if code != 0 || !strings.Contains(out, "already installed") {
		t.Errorf("second install: exit %d, %q", code, out)
	}
	if got := readConfig(t, cfg); got != text {
		t.Errorf("second install changed the config")
	}
}

func TestScript_InstallChecksTheSlotLoaded(t *testing.T) {
	f := newFakeCortex(t)
	f.present.Store(true)
	f.loaded.Store(1)
	cfg, _ := scriptEnv(t, f.addr(), f.addr())
	code, out, errOut := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg)
	if code != 0 || !strings.Contains(out, "Installed moca-sandbox") {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
}

// A script that does not start: install takes back the slot and the files.
func TestScript_InstallRollsBackAScriptThatDidNotStart(t *testing.T) {
	f := newFakeCortex(t)
	f.present.Store(true)
	f.notStarted.Store(1)
	cfg, plugins := scriptEnv(t, f.addr(), f.addr())
	before := readConfig(t, cfg)
	code, _, errOut := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg)
	if code != 1 || !strings.Contains(errOut, "0 of 1 scripts started") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if got := readConfig(t, cfg); got != before {
		t.Errorf("the slot was left in the config:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(plugins, "moca-sandbox")); !os.IsNotExist(err) {
		t.Errorf("the folder was left: %v", err)
	}
}

func TestScript_InstallRollsBackARefusedReload(t *testing.T) {
	f := newFakeCortex(t)
	f.refuse.Store(true)
	cfg, plugins := scriptEnv(t, f.addr(), f.addr())
	before := readConfig(t, cfg)
	code, _, errOut := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg)
	if code != 1 || !strings.Contains(errOut, "refused by the test") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if got := readConfig(t, cfg); got != before {
		t.Errorf("config not put back")
	}
	if _, err := os.Stat(filepath.Join(plugins, "moca-sandbox")); !os.IsNotExist(err) {
		t.Errorf("the folder was left: %v", err)
	}
}

func TestScript_StatesFromTheFiles(t *testing.T) {
	cfg, plugins := scriptEnv(t, closedAddr(t), closedAddr(t))
	if code, _, e := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg); code != 0 {
		t.Fatal(e)
	}
	script := filepath.Join(plugins, "moca-sandbox", "moca-sandbox.py")
	rec := filepath.Join(plugins, "moca-sandbox", scriptRecord)
	orig, _ := os.ReadFile(script)

	// An earlier release's copy: the file matches its record, the record not this release.
	old := []byte("#!/usr/bin/env python3\n# an earlier release\n")
	os.WriteFile(script, old, 0o700)
	b, _ := json.Marshal(installRecord{Version: "v0.0.1", Files: map[string]string{"moca-sandbox.py": sum(old)}})
	os.WriteFile(rec, b, 0o600)
	if st := listJSON(t, cfg)["moca-sandbox"]; st.State != stateOutdated {
		t.Fatalf("state = %s, want outdated", st.State)
	}
	if code, out, e := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg); code != 0 || !strings.Contains(out, "Updated") {
		t.Fatalf("update: %d %s%s", code, out, e)
	}
	if got, _ := os.ReadFile(script); !bytes.Equal(got, orig) {
		t.Error("update did not write this release's script")
	}

	// Edited by hand: modified, and install refuses it.
	os.WriteFile(script, append(orig, []byte("# my change\n")...), 0o700)
	if st := listJSON(t, cfg)["moca-sandbox"]; st.State != stateModified {
		t.Fatalf("state = %s, want modified", st.State)
	}
	if code, _, e := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg); code != 1 || !strings.Contains(e, "remove moca-sandbox") {
		t.Fatalf("install over an edit: %d %s", code, e)
	}

	// The slot taken out by hand: incomplete.
	os.WriteFile(script, orig, 0o700)
	text := readConfig(t, cfg)
	os.WriteFile(cfg, []byte(strings.Replace(text, "      - name: script\n        config:\n          dir: moca-sandbox\n          fail_closed: true\n", "", 1)), 0o600)
	if st := listJSON(t, cfg)["moca-sandbox"]; st.State != stateIncomplete {
		t.Fatalf("state = %s, want incomplete", st.State)
	}
	if code, _, e := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg); code != 0 {
		t.Fatalf("repair: %s", e)
	}
	if st := listJSON(t, cfg)["moca-sandbox"]; st.State != stateInstalled {
		t.Fatalf("state after repair = %s", st.State)
	}
}

func TestScript_RemoveTakesBackWhatInstallAdded(t *testing.T) {
	cfg, plugins := scriptEnv(t, closedAddr(t), closedAddr(t))
	before := readConfig(t, cfg)
	runScriptCmd(t, "install", "moca-sandbox", "--config", cfg)
	code, out, errOut := runScriptCmd(t, "remove", "moca-sandbox", "--config", cfg)
	if code != 0 || !strings.Contains(out, "Removed moca-sandbox") {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	if got := readConfig(t, cfg); got != before {
		t.Errorf("config after remove differs from before install:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(plugins, "moca-sandbox")); !os.IsNotExist(err) {
		t.Errorf("folder left: %v", err)
	}
	if code, out, _ := runScriptCmd(t, "remove", "moca-sandbox", "--config", cfg); code != 0 || !strings.Contains(out, "not installed") {
		t.Errorf("second remove: %d %q", code, out)
	}
}

func TestScript_RemoveRefusesAFileInstallDidNotWrite(t *testing.T) {
	cfg, plugins := scriptEnv(t, closedAddr(t), closedAddr(t))
	runScriptCmd(t, "install", "moca-sandbox", "--config", cfg)
	mine := filepath.Join(plugins, "moca-sandbox", "20-mine.py")
	os.WriteFile(mine, []byte("#!/bin/sh\n"), 0o700)
	code, _, errOut := runScriptCmd(t, "remove", "moca-sandbox", "--config", cfg)
	if code != 1 || !strings.Contains(errOut, "20-mine.py") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Errorf("the user's file is gone: %v", err)
	}
	if !strings.Contains(readConfig(t, cfg), "dir: moca-sandbox") {
		t.Error("the slot was removed although remove refused")
	}
}

// A folder of the same name that install did not write is not taken over.
func TestScript_InstallRefusesAFolderItDidNotWrite(t *testing.T) {
	cfg, plugins := scriptEnv(t, closedAddr(t), closedAddr(t))
	os.MkdirAll(filepath.Join(plugins, "moca-sandbox"), 0o700)
	os.WriteFile(filepath.Join(plugins, "moca-sandbox", "mine.py"), []byte("#!/bin/sh\n"), 0o700)
	code, _, errOut := runScriptCmd(t, "install", "moca-sandbox", "--config", cfg)
	if code != 1 || !strings.Contains(errOut, "mine.py") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
}

func TestScript_UnknownNameListsTheCatalog(t *testing.T) {
	cfg, _ := scriptEnv(t, closedAddr(t), closedAddr(t))
	code, _, errOut := runScriptCmd(t, "install", "nope", "--config", cfg)
	if code != 2 || !strings.Contains(errOut, "moca-sandbox") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `cd cmd/agentop && GOWORK=off go test -count=1 -run 'TestScript_' .`
Expected: FAIL to compile: `undefined: runScript`.

- [ ] **Step 3: Implement the command**

Create `cmd/agentop/cmd_script.go`:

```go
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/cmd/agentop/edit"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/plugins/script/catalog"
)

const scriptUsage = `agentop script — the script plugins Cortex ships
Usage:
  agentop script list [--json] [--config PATH]
  agentop script install <name> [--config PATH]
  agentop script remove <name> [--config PATH]

Each script plugin in this release is a folder of scripts with its own settings.
Installing one copies the folder to ~/.cortex/plugins/<name>/ and adds a script
slot for it to the config, so it runs on every request without a restart.

"list" shows every one and its state: not installed, installed, outdated (an
earlier release's copy), modified (its files were edited), or incomplete (only
the slot or only the folder is there). With Cortex running it also shows how many
of the slot's scripts loaded.

"install" waits for Cortex to load the slot and checks that every script in it
started. If one did not, it takes back what it added and says why. Run again, it
updates an outdated copy and leaves a current one alone. A modified copy is
refused: remove it first.

"remove" takes out the slot, then the files. It refuses a folder holding a file
install did not write.
`

// scriptRecord is the file, inside an installed entry's folder, that says what
// install wrote there. Its leading "." keeps the slot from running it.
const scriptRecord = ".agentop-install.json"

// scriptLoadWait bounds how long install waits, after the reload, for the slot to
// report its scripts. A var so a test can shorten it.
var scriptLoadWait = 10 * time.Second

type installRecord struct {
	Version string            `json:"version"`
	Files   map[string]string `json:"files"` // name -> sha256 hex
}

// scriptState is one of the five states list reports.
type scriptState string

const (
	stateNotInstalled scriptState = "not installed"
	stateInstalled    scriptState = "installed"
	stateOutdated     scriptState = "outdated"
	stateModified     scriptState = "modified"
	stateIncomplete   scriptState = "incomplete"
)

// scriptStatus is one entry as list shows it. Its JSON is what other tools read,
// so the field names are pinned by a test.
type scriptStatus struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	State       scriptState `json:"state"`
	FailClosed  bool        `json:"failClosed"`
	// Loaded and NotStarted are the slot's counts from the running proxy, absent
	// when no proxy answered or it runs no slot for this entry.
	Loaded     *int `json:"loaded,omitempty"`
	NotStarted *int `json:"notStarted,omitempty"`
	// Scripts is how many scripts the entry has: what Loaded should reach.
	Scripts int `json:"scripts"`
}

// scriptTarget is what every action works on.
type scriptTarget struct {
	cfg      *config.Config
	path     string // the config file
	statsURL string // "" when no proxy answers
	apiURL   string // the session API, "" when no proxy answers
	root     string // ~/.cortex/plugins
}

func runScript(args []string, stdout, stderr io.Writer) int {
	action := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action, args = args[0], args[1:]
	}
	switch action {
	case "", "help":
		fmt.Fprint(stdout, scriptUsage)
		if action == "" {
			return 2
		}
		return 0
	case "list":
		return scriptList(args, stdout, stderr)
	case "install":
		return scriptInstall(args, stdout, stderr)
	case "remove":
		return scriptRemove(args, stdout, stderr)
	}
	fmt.Fprintf(stderr, "agentop script: unknown action %q\n\n", action)
	fmt.Fprint(stderr, scriptUsage)
	return 2
}

// scriptFlags parses an action's flags around at most one positional name.
func scriptFlags(name string, args []string, stdout, stderr io.Writer, wantName bool) (cfgPath, entry string, asJSON bool, code int, ok bool) {
	flags := flag.NewFlagSet("script "+name, flag.ContinueOnError)
	p := flags.String("config", "", "Cortex config file (default ~/.cortex/config.yaml)")
	j := new(bool)
	if name == "list" {
		j = flags.Bool("json", false, "print JSON for another program to read")
	}
	pos, code, ok := serverFlags(flags, args, stdout, stderr)
	if !ok {
		return "", "", false, code, false
	}
	if wantName != (len(pos) == 1) || len(pos) > 1 {
		if wantName {
			fmt.Fprintf(stderr, "agentop script %s: want one name: agentop script %s <name>\n", name, name)
		} else {
			fmt.Fprintf(stderr, "agentop script %s: takes no arguments\n", name)
		}
		return "", "", false, 2, false
	}
	if wantName {
		entry = pos[0]
	}
	return *p, entry, *j, 0, true
}

func loadScriptTarget(cfgPath string) (scriptTarget, error) {
	cfg, path, statsURL, err := serverTarget(cfgPath)
	if err != nil {
		return scriptTarget{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return scriptTarget{}, err
	}
	t := scriptTarget{cfg: cfg, path: path, statsURL: statsURL, root: filepath.Join(home, ".cortex", "plugins")}
	if statsURL != "" {
		t.apiURL = dialURL(cfg.Listener.SessionAPIAddr)
	}
	return t, nil
}

// findCatalogEntry is the catalog entry named name, or an error listing the names.
func findCatalogEntry(name string) (catalog.CatalogEntry, error) {
	entries, err := catalog.Catalog()
	if err != nil {
		return catalog.CatalogEntry{}, err
	}
	var names []string
	for _, e := range entries {
		if e.Name == name {
			return e, nil
		}
		names = append(names, e.Name)
	}
	return catalog.CatalogEntry{}, fmt.Errorf("no script plugin named %q; this release has: %s", name, strings.Join(names, ", "))
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// hasSlot reports whether the outbound chain has a script entry for dir, under
// any on_error.
func hasSlot(cfg *config.Config, dir string) bool {
	return slices.ContainsFunc(cfg.Pipeline.Outbound.Plugins, func(e config.PluginEntry) bool {
		return e.Name == scriptPlugin && entryDir(e) == dir
	})
}

const scriptPlugin = "script"

func entryDir(e config.PluginEntry) string {
	var c struct {
		Dir string `json:"dir"`
	}
	if len(e.Config) > 0 {
		_ = json.Unmarshal(e.Config, &c)
	}
	return c.Dir
}

func readRecord(dir string) (installRecord, bool) {
	var r installRecord
	b, err := os.ReadFile(filepath.Join(dir, scriptRecord))
	if err != nil || json.Unmarshal(b, &r) != nil {
		return installRecord{}, false
	}
	return r, true
}

// stateOf is e's state on this machine. The record says what install wrote: files
// matching it but not this release are outdated, files differing from it modified.
func stateOf(t scriptTarget, e catalog.CatalogEntry) scriptState {
	dir := filepath.Join(t.root, e.Name)
	_, statErr := os.Stat(dir)
	folder := statErr == nil
	slot := hasSlot(t.cfg, e.Name)
	switch {
	case !folder && !slot:
		return stateNotInstalled
	case !folder || !slot:
		return stateIncomplete
	}
	rec, ok := readRecord(dir)
	if !ok {
		return stateModified
	}
	current := len(rec.Files) == len(e.Files)
	for name, want := range rec.Files {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || sum(b) != want {
			return stateModified
		}
		if src, ok := e.Files[name]; !ok || sum(src) != want {
			current = false
		}
	}
	if current {
		return stateInstalled
	}
	return stateOutdated
}

// slotCounts is the running proxy's scripts loaded / scripts not started for the
// slot whose dir is name. ok is false when no proxy answered or it runs no such slot.
func slotCounts(ctx context.Context, apiURL, name string) (loaded, notStarted int, ok bool) {
	if apiURL == "" {
		return 0, 0, false
	}
	view, err := apiclient.New(apiURL).GetPipeline(ctx)
	if err != nil {
		return 0, 0, false
	}
	for _, p := range view.Outbound {
		if p.Name != scriptPlugin || len(p.Config) == 0 {
			continue
		}
		var c struct {
			Dir string `json:"dir"`
		}
		if json.Unmarshal(p.Config, &c) != nil || c.Dir != name {
			continue
		}
		for _, m := range p.Metrics {
			switch m.Name {
			case "scripts loaded":
				loaded = int(m.Value)
			case "scripts not started":
				notStarted = int(m.Value)
			}
		}
		return loaded, notStarted, true
	}
	return 0, 0, false
}

func scriptList(args []string, stdout, stderr io.Writer) int {
	cfgPath, _, asJSON, code, ok := scriptFlags("list", args, stdout, stderr, false)
	if !ok {
		return code
	}
	t, err := loadScriptTarget(cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop script list: %v\n", err)
		return 1
	}
	entries, err := catalog.Catalog()
	if err != nil {
		fmt.Fprintf(stderr, "agentop script list: %v\n", err)
		return 1
	}
	rows := make([]scriptStatus, 0, len(entries))
	for _, e := range entries {
		r := scriptStatus{Name: e.Name, Description: e.Description, State: stateOf(t, e), FailClosed: e.FailClosed, Scripts: len(e.Scripts())}
		if l, n, ok := slotCounts(context.Background(), t.apiURL, e.Name); ok {
			r.Loaded, r.NotStarted = &l, &n
		}
		rows = append(rows, r)
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"scripts": rows})
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATE\tLOADED\tDESCRIPTION")
	for _, r := range rows {
		loaded := ""
		if r.Loaded != nil {
			loaded = fmt.Sprintf("%d/%d", *r.Loaded, r.Scripts)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Name, r.State, loaded, r.Description)
	}
	_ = tw.Flush()
	return 0
}

// slotChange adds or removes e's slot.
func slotChange(e catalog.CatalogEntry, remove bool) edit.ConfigChange {
	ch := edit.ConfigChange{Chain: "outbound", Plugin: scriptPlugin, Match: map[string]string{"dir": e.Name}}
	if remove {
		ch.RemoveEntry = true
		return ch
	}
	ch.CreatePlugin = true
	ch.Path, ch.Value = []string{"fail_closed"}, edit.BoolValue(e.FailClosed)
	return ch
}

func slotChanges(e catalog.CatalogEntry) []edit.ConfigChange {
	out := []edit.ConfigChange{slotChange(e, false)}
	if e.Timeout != "" {
		out = append(out, edit.ConfigChange{Chain: "outbound", Plugin: scriptPlugin, Match: map[string]string{"dir": e.Name},
			Path: []string{"timeout"}, Value: edit.ScalarValue(e.Timeout)})
	}
	return out
}

// writeFiles puts e's files in dir, each through a temp file and a rename so the
// reloader never sees half a script, then the record.
func writeFiles(dir string, e catalog.CatalogEntry) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	rec := installRecord{Version: version, Files: map[string]string{}}
	names := make([]string, 0, len(e.Files))
	for n := range e.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	scripts := e.Scripts()
	for _, n := range names {
		mode := os.FileMode(0o600)
		if slices.Contains(scripts, n) {
			mode = 0o700
		}
		if err := writeAtomic(filepath.Join(dir, n), e.Files[n], mode); err != nil {
			return err
		}
		rec.Files[n] = sum(e.Files[n])
	}
	b, _ := json.MarshalIndent(rec, "", "  ")
	return writeAtomic(filepath.Join(dir, scriptRecord), append(b, '\n'), 0o600)
}

// writeAtomic writes b to path through a temp file whose name a slot ignores.
func writeAtomic(path string, b []byte, mode os.FileMode) error {
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// removeFiles deletes what rec lists, the record, then dir when it is empty.
func removeFiles(dir string, rec installRecord) error {
	for n := range rec.Files {
		if err := os.Remove(filepath.Join(dir, n)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(filepath.Join(dir, scriptRecord)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// unrecorded is the files in dir that rec does not list: the user's.
func unrecorded(dir string, rec installRecord) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, d := range ents {
		n := d.Name()
		if n == scriptRecord {
			continue
		}
		if _, ok := rec.Files[n]; !ok {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func scriptInstall(args []string, stdout, stderr io.Writer) int {
	cfgPath, name, _, code, ok := scriptFlags("install", args, stdout, stderr, true)
	if !ok {
		return code
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "agentop script install: "+format+"\n", a...)
		return 1
	}
	e, err := findCatalogEntry(name)
	if err != nil {
		fmt.Fprintf(stderr, "agentop script install: %v\n", err)
		return 2
	}
	t, err := loadScriptTarget(cfgPath)
	if err != nil {
		return fail("%v", err)
	}
	dir := filepath.Join(t.root, e.Name)
	state := stateOf(t, e)
	switch state {
	case stateInstalled:
		fmt.Fprintf(stdout, "%s is already installed.\n", e.Name)
		return 0
	case stateModified:
		return fail("%s holds files that were edited since it was installed; run `agentop script remove %s` first", homeTilde(dir), e.Name)
	}
	_, statErr := os.Stat(dir)
	hadFolder := statErr == nil
	if hadFolder {
		if rec, ok := readRecord(dir); ok {
			if extra := unrecorded(dir, rec); len(extra) > 0 {
				return fail("%s holds files install did not write (%s); move them out first", homeTilde(dir), strings.Join(extra, ", "))
			}
		} else if extra := unrecorded(dir, installRecord{}); len(extra) > 0 {
			return fail("%s already exists and was not written by install (%s); move it out first", homeTilde(dir), strings.Join(extra, ", "))
		}
	}
	hadSlot := hasSlot(t.cfg, e.Name)

	if err := writeFiles(dir, e); err != nil {
		return fail("writing %s: %v", homeTilde(dir), err)
	}
	undoFiles := func() {
		if !hadFolder {
			rec, _ := readRecord(dir)
			_ = removeFiles(dir, rec)
		}
	}

	addedSlot := false
	if !hadSlot {
		res, err := writePluginConfig(context.Background(), edit.ConfigWrite{Path: t.path, StatsURL: t.statsURL, Changes: slotChanges(e)})
		if err != nil {
			undoFiles()
			return fail("%v", err)
		}
		switch res.Outcome {
		case edit.WriteReloadFailed, edit.WriteStatusUnreachable:
			undoFiles()
			return fail("Cortex did not take the slot, so %s was put back as it was:\n  %s", homeTilde(t.path), res.ReloadError)
		case edit.WriteReloadTimedOut:
			fmt.Fprintf(stdout, "Added the slot, but Cortex has not reported the reload yet; check with `agentop script list`.\n")
			return 0
		}
		addedSlot = res.Outcome != edit.WriteUnchanged
		if res.Outcome == edit.WriteNotRunning {
			fmt.Fprintf(stdout, "Installed %s. Cortex is not running; the slot loads when it starts.\n", e.Name)
			return 0
		}
	}

	if t.apiURL != "" {
		want := len(e.Scripts())
		deadline := time.Now().Add(scriptLoadWait)
		var loaded, notStarted int
		var seen bool
		for {
			loaded, notStarted, seen = slotCounts(context.Background(), t.apiURL, e.Name)
			if seen && (loaded == want || notStarted > 0) {
				break
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if !seen || loaded != want || notStarted > 0 {
			why := fmt.Sprintf("%d of %d scripts started", loaded, want)
			if !seen {
				why = "Cortex runs no slot for it"
			}
			if addedSlot {
				if _, err := writePluginConfig(context.Background(), edit.ConfigWrite{Path: t.path, StatsURL: t.statsURL,
					Changes: []edit.ConfigChange{slotChange(e, true)}}); err != nil {
					fmt.Fprintf(stderr, "agentop script install: could not take the slot back out: %v\n", err)
				}
			}
			undoFiles()
			return fail("%s did not load: %s. The Cortex log says why; for a Python script, check that python3 runs.", e.Name, why)
		}
	}
	verb := "Installed"
	if state == stateOutdated {
		verb = "Updated"
	}
	fmt.Fprintf(stdout, "%s %s: %s.\n", verb, e.Name, e.Description)
	return 0
}

func scriptRemove(args []string, stdout, stderr io.Writer) int {
	cfgPath, name, _, code, ok := scriptFlags("remove", args, stdout, stderr, true)
	if !ok {
		return code
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "agentop script remove: "+format+"\n", a...)
		return 1
	}
	e, err := findCatalogEntry(name)
	if err != nil {
		fmt.Fprintf(stderr, "agentop script remove: %v\n", err)
		return 2
	}
	t, err := loadScriptTarget(cfgPath)
	if err != nil {
		return fail("%v", err)
	}
	dir := filepath.Join(t.root, e.Name)
	if stateOf(t, e) == stateNotInstalled {
		fmt.Fprintf(stdout, "%s is not installed.\n", e.Name)
		return 0
	}
	rec, _ := readRecord(dir)
	if extra := unrecorded(dir, rec); len(extra) > 0 {
		return fail("%s holds files install did not write (%s); move them out first", homeTilde(dir), strings.Join(extra, ", "))
	}
	if hasSlot(t.cfg, e.Name) {
		res, err := writePluginConfig(context.Background(), edit.ConfigWrite{Path: t.path, StatsURL: t.statsURL,
			Changes: []edit.ConfigChange{slotChange(e, true)}})
		if err != nil {
			return fail("%v", err)
		}
		if res.Outcome == edit.WriteReloadFailed || res.Outcome == edit.WriteStatusUnreachable {
			return fail("Cortex did not take the change, so %s was put back as it was and nothing was removed:\n  %s", homeTilde(t.path), res.ReloadError)
		}
	}
	if err := removeFiles(dir, rec); err != nil {
		return fail("removing %s: %v", homeTilde(dir), err)
	}
	fmt.Fprintf(stdout, "Removed %s.\n", e.Name)
	return 0
}
```

- [ ] **Step 4: Wire it into `main.go`**

Apply with `git apply`, or by hand. It adds the `script` case, the name in `dispatchableSubcommands`, and two usage lines:

```diff
--- a/cmd/agentop/main.go
+++ b/cmd/agentop/main.go
@@ -69,7 +69,7 @@
 // The deprecated "claude-code" alias still dispatches (its case stays in main)
 // but is intentionally omitted here: it is no longer advertised in the usage
 // block or the unknown-subcommand hint. Use "configure claude-code" instead.
-var dispatchableSubcommands = []string{"observe", "setup", "doctor", "uninstall", "service", "configure", "exec", "server", "tools", "pipeline", "pricing", "cost", "experimental"}
+var dispatchableSubcommands = []string{"observe", "setup", "doctor", "uninstall", "service", "configure", "exec", "server", "script", "tools", "pipeline", "pricing", "cost", "experimental"}
 
 // unknownSubcommandMessage is the error for an unrecognised first argument.
 func unknownSubcommandMessage(name string) string {
@@ -97,6 +97,8 @@
                                environment, for tools with no settings file
   agentop server [<action>]    the inference server each agent's new sessions
                                use: add, remove, use, reset; alone, list them
+  agentop script <action>      the script plugins Cortex ships: list, install,
+                               remove
   agentop tools <action>       tool-definition costs: scan
   agentop pipeline <action>    the plugin pipeline in effect: get
   agentop pricing              show the model rates in effect (--host <gateway>)
@@ -153,6 +155,8 @@
 			os.Exit(runExec(os.Args[2:], os.Stdout, os.Stderr))
 		case "server":
 			os.Exit(runServer(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
+		case "script":
+			os.Exit(runScript(os.Args[2:], os.Stdout, os.Stderr))
 		case "service":
 			os.Exit(runService(os.Args[2:], os.Stdout, os.Stderr))
 		case "observe":
```

- [ ] **Step 5: Run the tests**

Run: `cd cmd/agentop && GOWORK=off go vet . && GOWORK=off go test -race -count=1 -run 'TestScript_' .`
Expected: PASS, 11 tests, in about 2s. `TestScript_InstallRollsBackARefusedReload` takes about 1s, the poll's interval.

Run: `cd cmd/agentop && GOWORK=off go test -race -count=1 ./...`
Expected: PASS. The usage tests in `cmd_observe_test.go` check that every entry in `dispatchableSubcommands` appears in the usage block, so they cover the new line.

- [ ] **Step 6: Commit**

```bash
gofmt -l cmd/agentop   # must print nothing
git add cmd/agentop/cmd_script.go cmd/agentop/cmd_script_test.go cmd/agentop/main.go
git commit -s -m "feat: Add agentop script to install the script plugins Cortex ships" \
  -m "list shows each bundled plugin's state; install copies its folder, adds its slot, and checks every script started, taking it all back if not; remove undoes it." \
  -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 4: Docs

**Files:**
- Modify: `docs/script-plugin.md` ("Examples" becomes "Bundled scripts", plus "Adding a bundled script")
- Modify: `tests/test_example_moca_sandbox.py` (`test_the_guides_install_steps_match_the_script`)
- Modify: `cmd/agentop/README.md` (a new section)
- Modify: `CLAUDE.md` (the directory tree)

**Interfaces:**
- Consumes: Task 3's command names and states; Task 1's `entry.yaml` fields.
- Produces: the docs section `### moca-sandbox:`, which the pytest parses.

- [ ] **Step 1: Make the docs test describe the new install**

In `tests/test_example_moca_sandbox.py`, replace the comment and body of `test_the_guides_install_steps_match_the_script` with:

```python
# The guide's moca-sandbox section must match the entry: the slot it shows is
# the one install writes, and the MCP server it registers is the one the script
# keys on.
def test_the_guides_install_steps_match_the_script():
    section = GUIDE.read_text().split("### moca-sandbox:", 1)[1].split("\n### ", 1)[0]
    blocks = re.findall(r"```(\w+)\n(.*?)```", section, re.S)
    sh = [textwrap.dedent(b) for lang, b in blocks if lang == "sh"]
    (slot,) = yaml.safe_load(textwrap.dedent(next(b for lang, b in blocks if lang == "yaml")))
    entry = yaml.safe_load((SCRIPT.parent / "entry.yaml").read_text())
    assert slot == {"name": "script", "config": {"dir": SCRIPT.parent.name, "fail_closed": entry["fail_closed"]}}
    assert sh[0].strip() == "agentop script install " + SCRIPT.parent.name
    server = re.search(r"claude mcp add -s local (\S+) --", sh[1]).group(1)
    assert "mcp__%s__" % server in SCRIPT.read_text()
```

Run: `pytest tests/test_example_moca_sandbox.py -q`
Expected: 14 passed, 1 failed, with `IndexError`: the docs have no `### moca-sandbox:` section yet.

- [ ] **Step 2: Rewrite the docs section**

Apply to `docs/script-plugin.md` with `git apply`, or by hand. It replaces everything from `## Examples` up to, but not including, `## Without the SDK`. The "What it costs" list is kept unchanged:

```diff
--- a/docs/script-plugin.md
+++ b/docs/script-plugin.md
@@ -202,14 +202,33 @@
 | `timeout` | `2s` | How long one script may take to answer one request |
 | `fail_closed` | `false` | Block a request when an enforcing script fails on it, and every request while a script in the slot did not start |
 
-## Examples
+## Bundled scripts
 
-Scripts written for a particular setup live in
-[`examples/script-plugins/`](../examples/script-plugins/). Cortex does not install
-them; copy the ones you want.
+Cortex ships some script plugins. agentop installs one in a single step:
 
-### moca-sandbox.py: keep Claude Code on moca's sandbox tools
+```sh
+agentop script list
+agentop script install moca-sandbox
+agentop script remove moca-sandbox
+```
 
+Each bundled script plugin is a folder with its own settings. Installing it:
+
+- copies the folder to `~/.cortex/plugins/<name>/`;
+- adds a `script` slot for it to `~/.cortex/config.yaml`, with the settings the
+  plugin needs;
+- waits for Cortex to load it, and checks every script in it started. If one did
+  not, it takes back what it added and says why.
+
+No restart is needed. Run `install` again after upgrading Cortex to update an
+outdated copy; one you edited is left alone until you `remove` it.
+
+`list` shows each one's state: `not installed`, `installed`, `outdated`,
+`modified` or `incomplete`, and, with Cortex running, how many of its scripts
+loaded. `list --json` gives the same for another program to read.
+
+### moca-sandbox: keep Claude Code on moca's sandbox tools
+
 [moca](https://github.com/rossoctl/moca) runs an agent's tools in a remote
 sandbox. To keep Claude Code off its local tools, `mocactl sbx up` denies them in
 the repo's `.claude/settings.json`. That deny also stops Claude Code loading
@@ -223,19 +242,15 @@
   nothing;
 - it replaces the result of a local call that already ran with an error.
 
-Other sessions are untouched.
+Other sessions are untouched. Its slot fails closed.
 
-Install it in a slot of its own, failing closed:
+1. Install it:
 
-1. Copy the script, from a checkout of this repo:
-
    ```sh
-   mkdir -p ~/.cortex/plugins/moca-sandbox
-   cp examples/script-plugins/moca-sandbox.py ~/.cortex/plugins/moca-sandbox/
-   chmod +x ~/.cortex/plugins/moca-sandbox/moca-sandbox.py
+   agentop script install moca-sandbox
    ```
 
-2. Add the slot as the last outbound entry of `~/.cortex/config.yaml`:
+   It adds this slot as the last outbound entry:
 
    ```yaml
          - name: script
@@ -244,9 +259,7 @@
              fail_closed: true
    ```
 
-   Cortex reloads it; no restart.
-
-3. In the repo, create the sandbox without the settings file, and register the
+2. In the repo, create the sandbox without the settings file, and register the
    sandbox server for this repo only:
 
    ```sh
@@ -254,17 +267,6 @@
    claude mcp add -s local sandbox -- mocactl sbx mcp
    ```
 
-Check that it loaded:
-
-```sh
-curl --noproxy '*' -s http://127.0.0.1:47601/v1/pipeline \
-  | jq '.outbound[] | select(.config.dir == "moca-sandbox") | .metrics[:2]'
-```
-
-It should show `scripts loaded` 1 and `scripts not started` 0. agentop's
-plugin pane cannot show it yet: it opens the first `script` slot whichever row
-you pick.
-
 What it costs, and what it does not cover:
 
 - **Streams are held back for every session.** The script rewrites responses, so
@@ -286,6 +288,21 @@
   prompt writes `.claude/settings.local.json` in the repo. Headless runs need
   `--allowedTools 'mcp__sandbox__*'`.
 
+
+### Adding a bundled script
+
+A bundled script plugin is a folder under `core/plugins/script/catalog/entries/`:
+the scripts, any `_`-prefixed helpers, and an `entry.yaml`:
+
+```yaml
+description: One line, shown by agentop script list
+fail_closed: false      # optional; the slot's setting
+timeout: 2s             # optional; the slot's setting
+```
+
+The folder's name is the slot's `dir`. A test in that package checks every
+entry, so adding one is adding the folder.
+
 ## Without the SDK
 
 The SDK is a convenience; the protocol is one JSON object per line on stdin and
```

Run: `pytest tests/test_example_moca_sandbox.py -q`
Expected: 15 passed.

- [ ] **Step 3: agentop's README**

In `cmd/agentop/README.md`, insert before `## Choosing an inference server (\`agentop server\`)`:

```markdown
## Installing a bundled script plugin (`agentop script`)

Cortex ships some [script plugins](../../docs/script-plugin.md#bundled-scripts)
inside agentop. `agentop script` installs one in a single step:

    agentop script list                  every bundled plugin and its state
    agentop script install <name>        copy it and add its slot
    agentop script remove <name>         take both back out

- **`install`** copies the plugin's folder to `~/.cortex/plugins/<name>/` and
  adds a `script` slot for it to `~/.cortex/config.yaml`. It then waits for
  Cortex to load the slot and checks every script in it started. If one did not,
  it takes back what it added and says why.
- **Run again,** it updates a copy from an earlier release and leaves a current
  one alone. A copy you edited is refused: `remove` it first.
- **`remove`** refuses a folder holding a file `install` did not write.
- **`list --json`** is for other programs, such as moca. Its fields are `name`,
  `description`, `state`, `failClosed`, `scripts` and, with Cortex running,
  `loaded` and `notStarted`.

`--config PATH` names another Cortex config, as for `agentop server`.

```

- [ ] **Step 4: CLAUDE.md's directory tree**

In `CLAUDE.md`, remove this line from the tree:

```
├── examples/script-plugins/          # Example script plugins; copied by hand, not installed
```

and in the `core/` part of the tree, insert two lines between the `jwtvalidation/` and `tokenexchange/` lines,
so the block reads:

```
│   ├── plugins/                      #   Every plugin; each owns its own config
│   │   ├── jwtvalidation/            #     JWKS-backed JWT verifier (validation/)
│   │   ├── script/catalog/           #     Script plugins shipped in agentop;
│   │   │                             #     `agentop script install <name>`
│   │   └── tokenexchange/            #     RFC 8693 exchange client + token cache
```

- [ ] **Step 5: Check nothing still names the old path**

Run: `grep -rn "examples/script-plugins" --include=*.md --include=*.go --include=*.py . | grep -v docs/superpowers`
Expected: no output.

- [ ] **Step 6: Commit**

```bash
git add docs/script-plugin.md tests/test_example_moca_sandbox.py cmd/agentop/README.md CLAUDE.md
git commit -s -m "docs: Install bundled script plugins with agentop script" \
  -m "The moca-sandbox steps become one command, and a section says how to add a bundled script." \
  -m "Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 5: Whole-branch checks and a live run

**Files:** none changed unless a check fails. A failure goes back to the task that owns the code.

The live run uses a **scratch** Cortex under its own `HOME`, on ports 49700-49704. It never touches the shared proxy on `:47600`, nor your real `~/.cortex`.

- [ ] **Step 1: Every check CI runs on these modules**

```bash
R=$(git rev-parse --show-toplevel)
(cd "$R/core" && GOWORK=off go vet ./... && GOWORK=off go test -race -count=1 ./plugins/script/...)
(cd "$R/cmd/agentop" && GOWORK=off go vet ./... && GOWORK=off go test -race -count=1 ./...)
(cd "$R/cmd/cortex" && for p in local full lite; do T=$(go -C ../../scripts/profile-tags run . $p); \
  GOWORK=off go vet -tags "$T" . && GOWORK=off go test -count=1 -tags "$T" . || exit 1; done)
gofmt -l "$R/core/plugins/script" "$R/cmd/agentop"
for m in core cmd/agentop cmd/cortex; do (cd "$R/$m" && GOWORK=off go mod tidy -diff >/dev/null) || echo "tidy: $m"; done
(cd "$R" && pytest tests/ -q -x --ignore=tests/e2e)
find "$R/core/plugins/script/catalog" -name __pycache__
```

Expected: every command passes, `gofmt -l` prints nothing, there is no `tidy:` line, and `find` prints nothing. Delete any `__pycache__` it finds before committing; the catalog skips it, but it must not be committed.

- [ ] **Step 2: Build the scratch Cortex and agentop**

```bash
R=$(git rev-parse --show-toplevel); S=/tmp/agentop-script-live; rm -rf $S; mkdir -p $S/home
(cd "$R/cmd/cortex" && GOWORK=off go build -tags "$(go -C ../../scripts/profile-tags run . full)" -o $S/cortex .)
(cd "$R/cmd/agentop" && GOWORK=off go build -o $S/agentop .)
HOME=$S/home $S/cortex --local --write-config
C=$S/home/.cortex/config.yaml
sed -i 's/127\.0\.0\.1:4760\([0-4]\)/127.0.0.1:4970\1/' $C
grep -c 4760 $C                       # 0
HOME=$S/home nohup $S/cortex --config $C > $S/cortex.log 2>&1 &
sleep 2; cp $C $S/before.yaml
A="env HOME=$S/home $S/agentop"
```

Expected: `grep -c` prints `0`.

- [ ] **Step 3: Install, check, use**

```bash
$A script list
$A script install moca-sandbox; echo "exit=$?"
$A script list
tail -4 $C
(python3 -I /tmp/moca-spike/mock_toolcall.py 49710 Bash '{"command":"x"}' $S/llm.jsonl >/dev/null 2>&1 &); sleep 0.5
curl -s -x http://127.0.0.1:49700 -H 'content-type: application/json' -H 'x-api-key: d' -H 'X-Claude-Code-Session-Id: s1' \
  -d '{"model":"m","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"mcp__sandbox__bash","input_schema":{"type":"object"}},{"name":"Bash","input_schema":{"type":"object"}}]}' \
  http://127.0.0.1:49710/v1/messages | grep -o '"name":"[^"]*"'
```

Expected:
- the first `list` shows `moca-sandbox  not installed`;
- `install` prints `Installed moca-sandbox: …` and `exit=0`;
- the second `list` shows `installed  1/1`;
- the config ends with the slot (`dir: moca-sandbox`, `fail_closed: true`);
- `curl` prints `"name":"blocked__Bash"`.

If `/tmp/moca-spike/mock_toolcall.py` is gone, skip the `curl` and note it in the report.

- [ ] **Step 4: Edit, remove, and a script that cannot start**

```bash
echo "# mine" >> $S/home/.cortex/plugins/moca-sandbox/moca-sandbox.py
$A script list | sed -n 2p
$A script remove moca-sandbox; echo "exit=$?"
cmp $C $S/before.yaml && echo "config back to before"; ls $S/home/.cortex/plugins/
pkill -f "$S/cortex --config"; sleep 1
env -i HOME=$S/home PATH=/nonexistent $S/cortex --config $C > $S/cortex2.log 2>&1 &
sleep 2
$A script install moca-sandbox; echo "exit=$?"
cmp $C $S/before.yaml && echo "config back to before"; ls $S/home/.cortex/plugins/
```

Expected:
- `list` shows `modified`;
- `remove` prints `Removed moca-sandbox.`, `exit=0`, `config back to before`, and the plugins folder holds only `_sdk`;
- with no `python3` on Cortex's PATH, `install` prints `moca-sandbox did not load: 0 of 1 scripts started…`, `exit=1`, `config back to before`, and again only `_sdk`.

- [ ] **Step 5: Tear down**

```bash
pkill -f "mock_toolcall.py 49710"; pkill -f "$S/cortex --config"; sleep 1; rm -rf $S
lsof -nP -iTCP:49700-49710 -sTCP:LISTEN || echo "ports free"
```

Expected: `ports free`.

- [ ] **Step 6: Report**

Give Hai Steps 1-4's results as a short list, and ask before pushing or opening the PR. The PR title is `Feat: Bundle script plugins in agentop and install them with agentop script`. The body is passed with `--body-file`, ends with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`, and names the follow-ups: v0.9.3, then moca's `sbx up` change.
