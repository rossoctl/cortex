package script

import (
	"bytes"
	_ "embed"
	"os"
	"path/filepath"
)

//go:embed sdk/cortex.py
var sdkSource []byte

// installSDK writes the Python SDK to ~/.cortex/plugins/_sdk/cortex.py when it is
// missing or differs from the one this binary carries, and returns its directory
// for the workers' PYTHONPATH, so `from cortex import on` works with nothing to
// install. The folder starts with "_", so no slot runs it as a script, and an
// upgrade of Cortex upgrades the SDK with it.
func installSDK() (string, error) {
	root, err := SlotDir("")
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "_sdk")
	path := filepath.Join(dir, "cortex.py")
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, sdkSource) {
		return dir, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, sdkSource, 0o600); err != nil {
		return "", err
	}
	return dir, os.Rename(tmp, path)
}
