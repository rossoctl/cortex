//go:build cpex

package cpex

import (
	"context"
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestDocumentedPoliciesLoad loads every CPEX policy the README and
// docs/cpex-plugin.md show on the real engine: LoadConfig in Configure,
// Initialize in Init. Nothing here sends a request, so policies that name
// remote IdPs load without reaching them.
func TestDocumentedPoliciesLoad(t *testing.T) {
	for _, src := range docSources(t)[1:] {
		loaded := 0
		for _, block := range src.blocks {
			for _, doc := range cpexPolicies(t, src.name, block) {
				setDocSecrets(t, doc)
				if err := loadPolicy(doc); err != nil {
					t.Errorf("%s: %v\npolicy:\n%s", src.name, err, doc)
					continue
				}
				loaded++
			}
		}
		t.Logf("%s: %d policies loaded", src.name, loaded)
	}
}

// A 0.2.x policy must not load: the examples are only worth checking if
// the engine refuses what they used to say.
func TestLegacyPolicyRejected(t *testing.T) {
	if err := loadPolicy(legacyPolicy); err == nil {
		t.Fatal("CPEX 0.3.0 loaded a 0.2.x policy")
	}
}

func loadPolicy(doc string) error {
	raw, err := json.Marshal(map[string]any{
		"config": doc,
		"hooks":  map[string]any{"on_request": []string{HookToolPreInvoke}},
	})
	if err != nil {
		return err
	}
	p := NewCPEX()
	if err := p.Configure(raw); err != nil {
		return err
	}
	defer func() { _ = p.Shutdown(context.Background()) }()
	return p.Init(context.Background())
}

// setDocSecrets sets every environment variable a policy's env_var
// secret sources name, which CPEX reads at load.
func setDocSecrets(t *testing.T, doc string) {
	t.Helper()
	var root any
	if err := yaml.Unmarshal([]byte(doc), &root); err != nil {
		t.Fatal(err)
	}
	var walk func(v any)
	walk = func(v any) {
		switch n := v.(type) {
		case map[string]any:
			if n["kind"] == "env_var" {
				if name, ok := n["name"].(string); ok {
					t.Setenv(name, "doc-example")
				}
			}
			for _, c := range n {
				walk(c)
			}
		case []any:
			for _, c := range n {
				walk(c)
			}
		}
	}
	walk(root)
}
