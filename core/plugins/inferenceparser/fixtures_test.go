package inferenceparser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rossoctl/cortex/core/internal/parsefixture"
	"github.com/rossoctl/cortex/core/pipeline"
)

// TestFixtures runs the parser over real captures in testdata/<agent>/<case>.
// testdata/README.md tells how the captures were taken and scrubbed.
func TestFixtures(t *testing.T) {
	parsefixture.Run(t, "testdata/*/*", "inference",
		func(*testing.T) parsefixture.Parser { return NewInferenceParser() },
		func(t *testing.T, x *pipeline.Extensions) any {
			if x.Inference == nil {
				t.Fatal("OnRequest left Extensions.Inference nil")
			}
			if !x.Inference.IsAction {
				t.Error("IsAction = false, want true: every parsed inference request is an agent action")
			}
			return x.Inference
		})
}

// TestFixtureCasesAreComplete fails on a capture that parsefixture.Run would skip or read with no response.
func TestFixtureCasesAreComplete(t *testing.T) {
	dirs, err := filepath.Glob("testdata/*/*")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no captures under testdata/*/*: %v", err)
	}
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, "request.headers")); err != nil {
			t.Errorf("%s: %v", dir, err)
		}
		n := 0
		for _, name := range []string{"response.sse", "response.json", "response.html"} {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				n++
			}
		}
		if n == 0 {
			t.Errorf("%s: no response.sse, response.json or response.html", dir)
		}
	}
}
