package inferenceparser

import (
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
