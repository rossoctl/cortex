package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// metricsBase is a config from before the built-in config pushed metrics: no
// metrics_push key.
const metricsBase = `mode: proxy-sidecar
listener:
  roles: [forward]
  forward_proxy_addr: 127.0.0.1:47600
pipeline:
  outbound:
    plugins:
      - name: inference-parser

`

func TestInsertMetricsPush(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		changed bool
	}{
		{"no block", metricsBase, true},
		{"turned off", metricsBase + "metrics_push: {enabled: false}\n", false},
		{"another URL", metricsBase + "metrics_push:\n  enabled: true\n  url: https://other.example/x\n", false},
		{"empty key", metricsBase + "metrics_push:\n", false},
		{"two-space document indent", metricsIndentDoc(metricsBase), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			updated, err := insertMetricsPush(tc.src)
			if err != nil {
				t.Fatalf("insertMetricsPush: %v", err)
			}
			if !tc.changed {
				if updated != "" {
					t.Fatalf("changed a config that has a metrics_push key:\n%s", updated)
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
			if err := metricsPushVerify(cfg); err != nil {
				t.Fatalf("verify: %v\n%s", err, updated)
			}
			if cfg.Pipeline.Outbound.Plugins[0].Name != "inference-parser" {
				t.Errorf("the plugin list changed:\n%s", updated)
			}
			col := metricsDocIndent(tc.src)
			if !strings.Contains(updated, "\n"+pad(col)+"metrics_push:\n") {
				t.Errorf("metrics_push: is not at column %d:\n%s", col+1, updated)
			}
		})
	}
}

func TestMigrateMetricsPush(t *testing.T) {
	p := writeCfg(t, metricsBase)
	original := readFile(t, p)

	var out bytes.Buffer
	changed, err := migrateMetricsPush(p, &out)
	if err != nil || !changed {
		t.Fatalf("migrate: changed %v, err %v", changed, err)
	}
	if got, err := os.ReadFile(p + ".before-agentop-metrics"); err != nil || string(got) != original {
		t.Errorf("backup missing or not the original (err %v)", err)
	}
	for _, want := range []string{"+ metrics_push: on, to " + metricsPushURL, "METRICS_PUSH=false",
		"Metrics push starts only after a restart of the proxy."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	after := readFile(t, p)
	out.Reset()
	changed, err = migrateMetricsPush(p, &out)
	if err != nil || changed {
		t.Fatalf("second run: changed %v, err %v", changed, err)
	}
	if readFile(t, p) != after || out.Len() != 0 {
		t.Errorf("second run changed the file or printed:\n%s", out.String())
	}
}

// metricsIndentDoc shifts every non-blank line two columns right.
func metricsIndentDoc(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = "  " + l
		}
	}
	return strings.Join(lines, "\n")
}

func metricsDocIndent(s string) int { return len(s) - len(strings.TrimLeft(s, " ")) }
