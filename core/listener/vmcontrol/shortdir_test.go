//go:build unix

package vmcontrol

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The temporary directories these tests bind Unix sockets in. Ported from
// moca's remote-worker/internal/shortdir @ c13d556: a socket path is bounded
// by sockaddr_un.sun_path (104 bytes on darwin, 108 on Linux) and bind fails
// with EINVAL past it, so t.TempDir() — whose name embeds the test's — is
// regularly too long on a Mac (/var/folders/... is 49 bytes before the test
// name even starts). But hardcoding /tmp fails wherever /tmp is not
// writable, and go test honours TMPDIR, which a sandbox sets. So: prefer
// TMPDIR when a directory under it leaves room for the caller's socket name,
// and fall back to /tmp and the OS default otherwise.

// maxSocketPath is the smaller of the two platform limits, so a path that
// fits here fits everywhere this code runs.
const maxSocketPath = 104

// shortDir returns a directory for a test's Unix sockets, removed when the
// test ends. need is how many bytes the caller will append for its longest
// socket path, separator included.
func shortDir(t *testing.T, prefix string, need int) string {
	t.Helper()
	dir, err := makeShortDir(prefix, need)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func makeShortDir(prefix string, need int) (string, error) {
	var tried []string
	for _, base := range shortDirCandidates() {
		// The length that matters is the directory that actually exists:
		// MkdirTemp's random suffix is up to 10 digits, so neither the base
		// alone nor a fixed allowance predicts it. Create first, measure, and
		// hand the directory back if it does not fit.
		dir, err := os.MkdirTemp(base, prefix)
		if err != nil {
			tried = append(tried, fmt.Sprintf("%s (%v)", base, err))
			continue
		}
		if len(dir)+need > maxSocketPath {
			_ = os.RemoveAll(dir)
			tried = append(tried, fmt.Sprintf(
				"%s (%s is %d bytes, %d with the caller's %d-byte socket name, over the %d limit)",
				base, dir, len(dir), len(dir)+need, need, maxSocketPath,
			))
			continue
		}
		return dir, nil
	}
	return "", fmt.Errorf(
		"shortdir: no directory both writable and short enough to hold a %d-byte socket name under "+
			"it (sun_path limit %d); tried: %s",
		need, maxSocketPath, strings.Join(tried, "; "),
	)
}

// shortDirCandidates lists the parent directories to try, in order of
// preference: TMPDIR first (the knob everything else honours), then /tmp,
// then the Go default. Duplicates and empties are dropped.
func shortDirCandidates() []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range []string{os.Getenv("TMPDIR"), "/tmp", os.TempDir()} {
		if d == "" {
			continue
		}
		d = filepath.Clean(d)
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}
