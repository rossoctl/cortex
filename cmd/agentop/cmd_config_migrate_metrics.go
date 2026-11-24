package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/rossoctl/cortex/core/config"
	"gopkg.in/yaml.v3"
)

// The built-in config pushes metrics, but only a config written after it started
// to do so. writeBuiltinConfig never rewrites an existing file, so this step adds
// the same block to a config that has no top-level metrics_push key. A key with
// any value, also an empty one or enabled: false, is the operator's choice and
// stays.

// metricsPushURL is the central collector that the built-in config names.
const metricsPushURL = "https://cortex-central.apps.epoc001.ete14.res.ibm.com/cortex"

// metricsPushLines is the comment and the block, unindented. KEEP IN STEP with
// builtinConfigYAML (cmd/cortex/local.go): a new install and a migrated one must
// push the same way.
var metricsPushLines = []string{
	"# Metrics push. Every 30s the accountant's counters -- tokens and dollars saved,",
	"# request counts and timings, labelled by model -- go to the central collector",
	"# over OTLP/HTTP. No prompt, path, host, session or key is sent. No token is",
	"# needed: the central collector takes a Cortex push without one. A collector",
	"# that wants one gets it from token below, or METRICS_PUSH_TOKEN.",
	"#",
	"# To turn it OFF:",
	"#   1. Set enabled: false below.",
	"#   2. Run: agentop service restart",
	"# To turn it back ON:",
	"#   1. Set enabled: true below.",
	"#   2. If METRICS_PUSH=false is set in the proxy's environment, remove it: the",
	"#      environment wins over this file.",
	"#   3. Run: agentop service restart",
	"#",
	"# Restart-only, not hot-reloaded: an edit here is REFUSED by the reloader (the",
	"# whole save with it) until the restart. See docs/laptop-service.md.",
	"metrics_push:",
	"  enabled: true",
	"  url: " + metricsPushURL,
	`  # token: ""                  # or METRICS_PUSH_TOKEN; sent as Authorization: Bearer`,
	"  # interval: 30s              # or METRICS_PUSH_INTERVAL",
}

// migrateMetricsPush adds the metrics_push block to a config that has no
// metrics_push key. It reports whether it changed the file.
func migrateMetricsPush(path string, stdout io.Writer) (changed bool, err error) {
	raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return false, err
	}
	if _, err := config.Load(path); err != nil {
		return false, fmt.Errorf("%s does not parse (%w); not touching it", path, err)
	}
	updated, err := insertMetricsPush(string(raw))
	if err != nil || updated == "" {
		return false, err
	}

	// This step's own backup, overwritten on each run that changes the file.
	bak := path + ".before-agentop-metrics"
	if werr := os.WriteFile(bak, raw, 0o600); werr != nil {
		return false, fmt.Errorf("writing %s: %w", bak, werr)
	}
	if err := replaceConfig(path, updated, metricsPushVerify); err != nil {
		return false, err
	}

	fmt.Fprintf(stdout, "Updated %s (previous kept as %s):\n", path, bak)
	fmt.Fprintf(stdout, "  + metrics_push: on, to %s\n", metricsPushURL)
	fmt.Fprintf(stdout, "  It sends the savings, inference, keep-alive and cache counters, with a random machine id as service.instance.id. It sends no prompt, path, host, session or key.\n")
	fmt.Fprintf(stdout, "  To stop it, set metrics_push.enabled: false, or METRICS_PUSH=false in the proxy's environment.\n")
	fmt.Fprintf(stdout, "  Metrics push starts only after a restart of the proxy.\n")
	return true, nil
}

// metricsPushVerify is the check migrateMetricsPush makes of its own result
// before it swaps it in: the push must be on, to the central collector.
func metricsPushVerify(c *config.Config) error {
	if m := c.MetricsPush; m == nil || !m.IsEnabled() || m.URL != metricsPushURL {
		return fmt.Errorf("the added block does not push metrics to %s", metricsPushURL)
	}
	return nil
}

// insertMetricsPush appends the block after the last non-blank line, at the
// document's own top-level indent. It returns "" when the document has a
// top-level metrics_push key, whatever its value.
func insertMetricsPush(src string) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		return "", fmt.Errorf("reading the config's structure: %w", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("the config is not a mapping; not editing it")
	}
	root := doc.Content[0]
	if k, _ := mappingEntry(root, "metrics_push"); k != nil {
		return "", nil
	}
	if root.Style&yaml.FlowStyle != 0 {
		return "", fmt.Errorf("the config is written in flow style; add metrics_push by hand (see docs/laptop-service.md)")
	}
	lines := strings.Split(src, "\n")
	at := len(lines)
	for at > 0 && strings.TrimSpace(lines[at-1]) == "" {
		at--
	}
	return insertLines(lines, at, indented(metricsPushLines, root.Column-1)), nil
}
