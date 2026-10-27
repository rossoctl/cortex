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
