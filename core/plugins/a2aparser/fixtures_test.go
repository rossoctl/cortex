package a2aparser

import (
	"testing"

	"github.com/rossoctl/cortex/core/internal/parsefixture"
	"github.com/rossoctl/cortex/core/pipeline"
)

// TestFixtures runs the parser over the hand-built cases in testdata/<case>.
// testdata/README.md tells which spec the cases follow.
func TestFixtures(t *testing.T) {
	parsefixture.Run(t, "testdata/*", "a2a",
		func(*testing.T) parsefixture.Parser { return NewA2AParser() },
		func(_ *testing.T, x *pipeline.Extensions) any { return x.A2A })
}
