package cpex

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/rossoctl/cortex/core/config"
)

// TestDocumentedConfigsConfigure decodes every cpex plugin entry shown
// in this package's doc comment, its README and docs/cpex-plugin.md
// through the runtime loader's PluginEntry, then Configure. Issue #856:
// an example put apl:/pipelines: under config:, which Configure's strict
// decode rejects with `unknown field "apl"`.
func TestDocumentedConfigsConfigure(t *testing.T) {
	for _, src := range docSources(t) {
		entries := 0
		for _, block := range src.blocks {
			for _, raw := range cpexEntryConfigs(t, src.name, block) {
				entries++
				p := setupFake(&FakeManager{})
				if err := p.Configure(withReadableConfigFile(t, raw)); err != nil {
					t.Errorf("%s: Configure: %v\nexample:\n%s", src.name, err, block)
				}
			}
		}
		if entries == 0 {
			t.Errorf("%s: no cpex plugin entry with a config block found", src.name)
		}
	}
}

// TestDocumentedPoliciesAreCurrent checks every CPEX policy the README and
// docs/cpex-plugin.md show against the 0.2.x keys and plugin kinds CPEX
// 0.3.0 rejects. The cpex-tagged pass loads the same policies on the real
// engine; this one runs without cgo.
func TestDocumentedPoliciesAreCurrent(t *testing.T) {
	for _, src := range docSources(t)[1:] {
		policies := 0
		for _, block := range src.blocks {
			for _, doc := range cpexPolicies(t, src.name, block) {
				policies++
				if marker := legacyPolicyMarker(doc); marker != "" {
					t.Errorf("%s: policy uses CPEX 0.2.x %s:\n%s", src.name, marker, doc)
				}
			}
		}
		if policies == 0 {
			t.Errorf("%s: no CPEX policy found", src.name)
		}
	}
}

func TestLegacyPolicyMarker(t *testing.T) {
	if m := legacyPolicyMarker(legacyPolicy); m == "" {
		t.Error("the 0.2.x policy was not recognized")
	}
	if m := legacyPolicyMarker("routes:\n  - tool: x\n    authorization:\n      pre_invocation: [allow]\n"); m != "" {
		t.Errorf("a 0.3.0 policy was flagged for %s", m)
	}
}

// legacyPolicy is a CPEX 0.2.x policy: the plugin_settings block, a
// removed plugin kind and the apl: route wrapper. CPEX 0.3.0 refuses it.
const legacyPolicy = `plugin_settings:
  routing_enabled: true
plugins:
  - name: pii-scan
    kind: validator/pii-scan
    hooks: [cmf.tool_pre_invoke]
    config:
      detect:
        - { kind: ssn }
      mode: deny
routes:
  - tool: send_email
    apl:
      authorization:
        pre_invocation:
          - "run(pii-scan)"
`

// legacyPolicyMarkers are the 0.2.x spellings most likely to survive in an
// example; CPEX's migration guide lists the rest.
var legacyPolicyMarkers = []struct{ name, re string }{
	{"apl: wrapper", `(?m)^\s*apl:`},
	{"plugin_settings", `(?m)^\s*plugin_settings:`},
	{"routing_enabled", `(?m)^\s*routing_enabled:`},
	{"identity: key", `(?m)^\s*identity:`},
	{"policy: key", `(?m)^\s*(post_)?policy:`},
	{"validator/pii-scan", `validator/pii-scan`},
	{"audit/logger", `audit/logger`},
}

func legacyPolicyMarker(doc string) string {
	for _, m := range legacyPolicyMarkers {
		if regexp.MustCompile(m.re).MatchString(doc) {
			return m.name
		}
	}
	return ""
}

type docSource struct {
	name   string
	blocks []string
}

// docSources are the files whose examples these tests check. The package
// doc comes first; it shows a plugin entry but no CPEX policy.
func docSources(t *testing.T) []docSource {
	t.Helper()
	return []docSource{
		{"plugin.go package doc", packageDocBlocks(t, "plugin.go")},
		{"README.md", markdownYAMLBlocks(t, "README.md")},
		{"docs/cpex-plugin.md", markdownYAMLBlocks(t, filepath.Join("..", "..", "..", "docs", "cpex-plugin.md"))},
	}
}

// cpexPolicyKeys are the top-level keys of a CPEX policy document. A YAML
// block with only these keys, one of them other than plugins:, is a
// policy; a cortex pipeline's plugins: list is not.
var cpexPolicyKeys = map[string]bool{
	"plugins": true, "global": true, "routes": true, "groups": true,
	"engine_settings": true, "secrets": true,
}

// cpexPolicies returns the CPEX policy documents in a YAML block: the
// block itself when it is one, and the inline config string of every
// cpex plugin entry in it.
func cpexPolicies(t *testing.T, src, block string) []string {
	t.Helper()
	var out []string
	for _, raw := range cpexEntryConfigs(t, src, block) {
		var c struct {
			Config string `json:"config"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatalf("%s: cpex config: %v", src, err)
		}
		if c.Config != "" {
			out = append(out, c.Config)
		}
	}
	if strings.Contains(block, "name: cpex") {
		return out
	}
	var top map[string]any
	if err := yaml.Unmarshal([]byte(block), &top); err != nil || len(top) == 0 {
		return out
	}
	policy := false
	for k := range top {
		if !cpexPolicyKeys[k] {
			return out
		}
		policy = policy || k != "plugins"
	}
	if policy {
		out = append(out, block)
	}
	return out
}

// packageDocBlocks returns the indented code blocks of a Go file's
// package doc comment.
func packageDocBlocks(t *testing.T, file string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ParseComments|parser.PackageClauseOnly)
	if err != nil {
		t.Fatal(err)
	}
	if f.Doc == nil {
		t.Fatalf("%s has no package doc", file)
	}
	var blocks []string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			blocks = append(blocks, strings.Join(cur, "\n"))
			cur = nil
		}
	}
	for _, line := range strings.Split(f.Doc.Text(), "\n") {
		if strings.HasPrefix(line, "\t") {
			cur = append(cur, strings.TrimPrefix(line, "\t"))
			continue
		}
		flush()
	}
	flush()
	return blocks
}

// markdownYAMLBlocks returns the bodies of a markdown file's ```yaml fences.
func markdownYAMLBlocks(t *testing.T, file string) []string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var blocks []string
	rest := string(b)
	for {
		i := strings.Index(rest, "```yaml\n")
		if i < 0 {
			return blocks
		}
		rest = rest[i+len("```yaml\n"):]
		j := strings.Index(rest, "\n```")
		if j < 0 {
			t.Fatalf("%s: unterminated yaml fence", file)
		}
		blocks = append(blocks, rest[:j])
		rest = rest[j:]
	}
}

// cpexEntryConfigs returns the raw config of every `name: cpex` plugin
// entry in a YAML document, decoded the way the runtime loader does.
func cpexEntryConfigs(t *testing.T, src, doc string) []json.RawMessage {
	t.Helper()
	if !strings.Contains(doc, "name: cpex") {
		return nil
	}
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &root); err != nil {
		t.Fatalf("%s: example is not valid YAML: %v\n%s", src, err, doc)
	}
	var out []json.RawMessage
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == "name" && n.Content[i+1].Value == "cpex" {
					var entry config.PluginEntry
					if err := n.Decode(&entry); err != nil {
						t.Fatalf("%s: plugin entry: %v\n%s", src, err, doc)
					}
					if entry.Config != nil {
						out = append(out, entry.Config)
					}
				}
			}
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(&root)
	return out
}

// withReadableConfigFile points a config_file at a file that exists, so
// Configure's read of it succeeds outside a pod.
func withReadableConfigFile(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["config_file"]; !ok {
		return raw
	}
	path := filepath.Join(t.TempDir(), "cpex.yaml")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m["config_file"] = path
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
