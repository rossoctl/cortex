package contextguru

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain keeps every test's session-state database in one temp directory, never in the
// developer's own ~/.cortex or cache directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "contextguru-test-")
	if err != nil {
		panic(err)
	}
	stateDBPathFor = func(bool) (string, error) { return filepath.Join(dir, stateDBFileName), nil }
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
