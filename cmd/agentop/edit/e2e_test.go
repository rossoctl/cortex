//go:build e2e

package edit

import (
	"strings"
	"testing"
)

// TestE2E_FetchExistingAgent verifies Fetch works against a real cluster.
// Requires `make demo-ibac` to have run.
//
// Run:
//
//	go test -tags=e2e ./edit/ -run TestE2E_FetchExistingAgent -v
func TestE2E_FetchExistingAgent(t *testing.T) {
	fp, err := Fetch(t.Context(), DefaultRunner, "team1", "email-agent")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	subtree := fp.InnerYAML[fp.PipelineStart:fp.PipelineEnd]
	if !strings.Contains(string(subtree), "pipeline:") {
		t.Fatalf("subtree missing header: %s", subtree)
	}
	if fp.PipelineEnd <= fp.PipelineStart {
		t.Fatalf("invalid range: [%d, %d)", fp.PipelineStart, fp.PipelineEnd)
	}
}
