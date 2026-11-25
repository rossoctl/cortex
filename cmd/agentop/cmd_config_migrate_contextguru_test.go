package main

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/config"
)

// cgHead is the part of every fixture above the outbound plugin list.
const cgHead = `mode: proxy-sidecar
listener:
  roles: [forward]
  forward_proxy_addr: 127.0.0.1:47600
pipeline:
  outbound:
    plugins:
      - name: inference-parser
      - name: mcp-parser
      - name: a2a-parser
`

const cgTail = `pricing:
  endpoints: []
`

func cgDoc(entries string) string { return cgHead + entries + cgTail }

const tpBashGh = `      # tool-prune comment, directly above the entry.
      - name: tool-prune
        on_error: enforce
        config:
          remove: [Bash, mcp__gh__issue]
`

const cgPlain = `      - name: context-guru
        on_error: enforce
`

func TestPlanContextGuru(t *testing.T) {
	cases := []struct {
		name     string
		src      string
		changed  bool
		restart  bool
		names    []string // outbound plugin names after the change
		remove   []string // context-guru toolfilter.remove after the change
		preset   string   // context-guru engine.preset after the change
		note     string   // a substring of a note, when one is wanted
		errorHas string
	}{
		{name: "1 context-guru, no tool-prune", src: cgDoc(cgPlain)},
		{
			name: "2 context-guru with no config + tool-prune", src: cgDoc(cgPlain + tpBashGh), changed: true,
			names:  []string{"inference-parser", "mcp-parser", "a2a-parser", "context-guru"},
			remove: []string{"Bash", "mcp__gh__issue"}, preset: "xhigh",
		},
		{
			name: "3 existing remove list", changed: true,
			src: cgDoc(`      - name: context-guru
        config:
          engine:
            preset: xhigh
            components:
              toolfilter:
                remove: [Read]
      - name: tool-prune
        config:
          remove: [Read, Bash]
`),
			names:  []string{"inference-parser", "mcp-parser", "a2a-parser", "context-guru"},
			remove: []string{"Read", "Bash"}, preset: "xhigh",
		},
		{
			name: "4 no context-guru, tool-prune", src: cgDoc(tpBashGh + "      - name: lineage\n"), changed: true,
			names:  []string{"inference-parser", "mcp-parser", "a2a-parser", "context-guru", "lineage"},
			remove: []string{"Bash", "mcp__gh__issue"}, preset: "xhigh",
		},
		{
			name: "5 no context operator", src: cgDoc(""), changed: true, restart: true,
			names:  []string{"inference-parser", "mcp-parser", "a2a-parser", "context-guru"},
			remove: []string{}, preset: "xhigh",
		},
		{name: "6 tool-prune observe", src: cgDoc(strings.Replace(tpBashGh, "enforce", "observe", 1)), note: "on_error is observe"},
		{name: "7 tool-prune off", src: cgDoc(strings.Replace(tpBashGh, "enforce", "off", 1)), note: "on_error is off"},
		{
			name: "8 tool-prune paths", note: "config.paths",
			src: cgDoc(tpBashGh + "          paths: [/v1/messages]\n"),
		},
		{name: "9 mcp server name", src: cgDoc(strings.Replace(tpBashGh, "mcp__gh__issue", "mcp__gh", 1)), note: "mcp__gh"},
		{name: "10 skill name", src: cgDoc(strings.Replace(tpBashGh, "mcp__gh__issue", "skill__x", 1)), note: "skill__x"},
		{name: "11 name with a space", src: cgDoc(strings.Replace(tpBashGh, "mcp__gh__issue", `"a b"`, 1)), note: `"a b"`},
		{
			name: "12 context-guru off", note: "context-guru's on_error is off",
			src: cgDoc(strings.Replace(cgPlain, "enforce", "off", 1) + tpBashGh),
		},
		{
			name: "13 context-guru ultrahigh", note: "ultrahigh",
			src: cgDoc("      - name: context-guru\n        config:\n          engine:\n            preset: ultrahigh\n" + tpBashGh),
		},
		{
			name:     "14 no parser",
			src:      "mode: proxy-sidecar\npipeline:\n  outbound:\n    plugins:\n      - name: lineage\n",
			errorHas: "add the context-guru entry by hand",
		},
		{name: "15 body reader after the parsers", src: cgDoc("      - name: ibac\n"), errorHas: "add the context-guru entry by hand"},
		{
			name: "16 two-space document indent", src: metricsIndentDoc(cgDoc(tpBashGh + "      - name: lineage\n")), changed: true,
			names:  []string{"inference-parser", "mcp-parser", "a2a-parser", "context-guru", "lineage"},
			remove: []string{"Bash", "mcp__gh__issue"}, preset: "xhigh",
		},
		{
			name: "17 block-form remove", changed: true,
			src:    cgDoc("      - name: tool-prune\n        config:\n          remove:\n            - Bash\n            - mcp__gh__issue\n"),
			names:  []string{"inference-parser", "mcp-parser", "a2a-parser", "context-guru"},
			remove: []string{"Bash", "mcp__gh__issue"}, preset: "xhigh",
		},
		{
			name: "18 block-form existing remove", changed: true,
			src: cgDoc(`      - name: context-guru
        config:
          engine:
            preset: high
            components:
              toolfilter:
                remove:
                  - Read
` + tpBashGh),
			names:  []string{"inference-parser", "mcp-parser", "a2a-parser", "context-guru"},
			remove: []string{"Read", "Bash", "mcp__gh__issue"}, preset: "high",
		},
		{
			name: "19 empty engine", changed: true,
			src:    cgDoc("      - name: context-guru\n        config:\n          engine:\n" + tpBashGh),
			names:  []string{"inference-parser", "mcp-parser", "a2a-parser", "context-guru"},
			remove: []string{"Bash", "mcp__gh__issue"}, preset: "xhigh",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			updated, restart, notes, err := planContextGuru(tc.src)
			if tc.errorHas != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errorHas) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.errorHas)
				}
				return
			}
			if err != nil {
				t.Fatalf("planContextGuru: %v", err)
			}
			if restart != tc.restart {
				t.Errorf("restart = %v, want %v", restart, tc.restart)
			}
			if tc.note != "" && !slices.ContainsFunc(notes, func(n string) bool { return strings.Contains(n, tc.note) }) {
				t.Errorf("notes %q lack %q", notes, tc.note)
			}
			if !tc.changed {
				if updated != "" {
					t.Fatalf("changed the config:\n%s", updated)
				}
				return
			}
			if updated == "" {
				t.Fatal("reported no change")
			}
			cfg, loaded, err := loadCandidate(updated)
			if err != nil || !loaded {
				t.Fatalf("result does not load (err %v):\n%s", err, updated)
			}
			orig, _, _ := loadCandidate(tc.src)
			if err := contextGuruVerify(toolPruneNames(orig))(cfg); err != nil {
				t.Fatalf("verify: %v\n%s", err, updated)
			}
			var names []string
			for _, e := range cfg.Pipeline.Outbound.Plugins {
				names = append(names, e.Name)
			}
			if !slices.Equal(names, tc.names) {
				t.Errorf("outbound = %v, want %v\n%s", names, tc.names, updated)
			}
			preset, remove := cgEngine(t, cfg)
			if preset != tc.preset || remove == nil || !slices.Equal(remove, tc.remove) {
				t.Errorf("preset %q remove %v, want %q %v\n%s", preset, remove, tc.preset, tc.remove, updated)
			}
			if strings.Contains(updated, "tool-prune comment") {
				t.Errorf("the comment directly above tool-prune stayed:\n%s", updated)
			}
			// A second plan of the result changes nothing.
			if again, _, _, err := planContextGuru(updated); err != nil || again != "" {
				t.Errorf("second plan: err %v, changed:\n%s", err, again)
			}
		})
	}
}

// cgEngine decodes the context-guru entry's preset and toolfilter.remove.
func cgEngine(t *testing.T, cfg *config.Config) (string, []string) {
	t.Helper()
	for _, e := range cfg.Pipeline.Outbound.Plugins {
		if e.Name != "context-guru" {
			continue
		}
		var c struct {
			Engine struct {
				Preset     string `json:"preset"`
				Components struct {
					Toolfilter struct {
						Remove []string `json:"remove"`
					} `json:"toolfilter"`
				} `json:"components"`
			} `json:"engine"`
		}
		if err := json.Unmarshal(e.Config, &c); err != nil {
			t.Fatalf("context-guru config: %v", err)
		}
		return c.Engine.Preset, c.Engine.Components.Toolfilter.Remove
	}
	t.Fatal("no context-guru entry")
	return "", nil
}

func TestMigrateContextGuru(t *testing.T) {
	p := writeCfg(t, cgDoc(tpBashGh))
	original := readFile(t, p)

	var out bytes.Buffer
	changed, restart, err := migrateContextGuru(p, &out)
	if err != nil || !changed || restart {
		t.Fatalf("migrate: changed %v, restart %v, err %v", changed, restart, err)
	}
	if got, err := os.ReadFile(p + ".before-agentop-context-guru"); err != nil || string(got) != original {
		t.Errorf("backup missing or not the original (err %v)", err)
	}
	for _, want := range []string{contextGuruOnLine, "- pipeline.outbound.plugins: tool-prune",
		"Bash, mcp__gh__issue", "no restart"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	after := readFile(t, p)
	out.Reset()
	changed, _, err = migrateContextGuru(p, &out)
	if err != nil || changed {
		t.Fatalf("second run: changed %v, err %v", changed, err)
	}
	if readFile(t, p) != after || out.Len() != 0 {
		t.Errorf("second run changed the file or printed:\n%s", out.String())
	}

	p = writeCfg(t, cgDoc(""))
	out.Reset()
	if changed, restart, err := migrateContextGuru(p, &out); err != nil || !changed || !restart {
		t.Fatalf("no operator: changed %v, restart %v, err %v", changed, restart, err)
	}
	if !strings.Contains(out.String(), "needs a restart") {
		t.Errorf("output lacks the restart line:\n%s", out.String())
	}

	p = writeCfg(t, cgDoc(strings.Replace(tpBashGh, "enforce", "off", 1)))
	out.Reset()
	if changed, _, err := migrateContextGuru(p, &out); err != nil || changed {
		t.Fatalf("refused: changed %v, err %v", changed, err)
	}
	if !strings.Contains(out.String(), "tool-prune stays in "+p+": ") {
		t.Errorf("output lacks the note:\n%s", out.String())
	}
}
