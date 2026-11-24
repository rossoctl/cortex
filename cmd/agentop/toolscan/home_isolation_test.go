package toolscan

import (
	"testing"

	"github.com/rossoctl/cortex/cmd/agentop/internal/testhome"
)

// TestMain isolates the home directory, so no test in this package can reach the real profile
// (see testhome). This guards that it still does.
func TestHomeIsIsolated(t *testing.T) { testhome.AssertIsolated(t) }
