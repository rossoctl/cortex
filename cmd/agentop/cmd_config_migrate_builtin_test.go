package main

import (
	"os"
	"strings"
	"testing"
)

// The context-guru and metrics push migrations write copies of blocks in
// builtinConfigYAML. agentop cannot import cmd/cortex, so this test reads its
// source: a new install and a migrated one must get the same blocks.
func TestMigrationBlocksMatchTheBuiltinConfig(t *testing.T) {
	src, err := os.ReadFile("../cortex/local.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		lines  []string
		indent int
	}{
		{"context-guru entry", contextGuruEntryLines, 6},
		{"metrics_push block", metricsPushLines, 0},
	} {
		if block := strings.Join(indented(tc.lines, tc.indent), "\n"); !strings.Contains(string(src), block) {
			t.Errorf("cmd/cortex/local.go does not contain the %s that the migration writes:\n%s", tc.name, block)
		}
	}
}
