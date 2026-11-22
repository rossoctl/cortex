package toolscan

import (
	"os"
	"testing"

	"github.com/rossoctl/cortex/cmd/agentop/internal/testhome"
)

// TestMain points the home directory at a temporary one for the whole run: the scan reads the
// agents' config under the home directory, and a test must not read the real profile's.
func TestMain(m *testing.M) {
	cleanup := testhome.Isolate()
	code := m.Run()
	cleanup()
	os.Exit(code)
}
