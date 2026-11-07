package mcpparser

import (
	"testing"

	"github.com/rossoctl/cortex/core/internal/parsefixture"
	"github.com/rossoctl/cortex/core/pipeline"
)

// TestFixtures runs the parser over the hand-built cases in testdata/<case>.
// testdata/README.md tells which spec the cases follow.
// The parser has the default config, so /mcp is the MCP path.
func TestFixtures(t *testing.T) {
	parsefixture.Run(t, "testdata/*", "mcp", func(t *testing.T) parsefixture.Parser {
		p := NewMCPParser()
		if err := p.Configure(nil); err != nil {
			t.Fatal(err)
		}
		return p
	}, func(_ *testing.T, x *pipeline.Extensions) any { return x.MCP })
}
