package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// yankDirRel is the yank output directory, relative to the user's home. It
// follows the same convention as agentop's other durable state (cmd_claudecode.go's
// cortexCfgRel / stateRel), so there is one ~/.cortex tree rather than a new one.
const yankDirRel = ".cortex/agentop-events"

// yankDir returns the absolute directory yank writes into.
//
// Not os.TempDir(): on macOS that is /var/folders/<opaque>/T, so a yanked file
// landed on a 92-character path nobody could find or retype, which is #868.
//
// Not a fixed path under /tmp either, which is where this started. /tmp is
// world-writable, and os.MkdirAll returns nil for a path that already exists
// whatever its owner or mode — so a fixed /tmp/<name> cannot enforce 0700, can
// be pre-created as a symlink that redirects where events land, and on a shared
// host is squatted by whoever yanks first, permanently breaking everyone else.
// Session events carry identity subjects, raw LLM completions and tool
// arguments, so none of that is acceptable for a directory holding them.
//
// ~/.cortex is the user's own tree and is normally 0700, which makes those cases
// unreachable — but agentop never creates it, so its mode is whatever an installer
// or the user left. With a 0755 ~/.cortex, MkdirAll neither tightens the mode nor
// refuses to follow an agentop-events symlink someone planted, and the event lands
// in their directory. Verified. So the guarantee is enforced here rather than
// assumed: yankEventToFile Lstats the directory and refuses to write unless it is
// a real directory, owned by this user, with no group or world access.
//
// Still far shorter than where this started, and a location users already know.
func yankDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine your home directory: %w", err)
	}
	if home == "" {
		// Separate branch, not folded into the one above: %w on a nil error
		// renders as "%!w(<nil>)", which would make this defensive path
		// unreadable to whoever ever hits it.
		return "", errors.New("cannot determine your home directory: it is empty")
	}
	return filepath.Join(home, yankDirRel), nil
}

// checkYankDir refuses to write into a directory that does not actually protect
// its contents. MkdirAll returns nil for a path that already exists whatever its
// owner or mode, and it happily follows a symlink — so an agentop-events symlink
// planted by another local user would silently redirect events carrying identity
// subjects and raw LLM completions into their directory.
//
// Checks every component from the home directory down, not just the leaf: a
// symlinked ~/.cortex redirects the whole subtree just as effectively, and the
// leaf-only version of this function missed it (verified — the event landed in the
// attacker's tree). Lstat, not Stat: Stat resolves the link and would report the
// target's mode.
//
// The mode requirement applies only to the directories agentop owns (~/.cortex and
// agentop-events), not to the home directory itself: a real home is commonly 0750
// or 0755 — this machine's is 0750 — and demanding 0700 there would refuse to
// yank on an ordinary account. Every component is still checked for a symlink,
// which is the redirection risk.
//
// A loose mode on a directory agentop owns is tightened rather than refused, which
// is what the rest of the tree already does for this exact problem:
// writeBuiltinConfig in cmd/cortex/local.go chmods ~/.cortex to 0700
// after MkdirAll, and again one level down for the CA directory. Self-healing
// beats handing the user a chmod to run by hand, and if the chmod fails — someone
// else owns it — the refusal below still stands. Symlinks and non-directories stay
// hard refusals; there is no chmod out of those.
//
// Ownership is deliberately not checked via syscall.Stat_t: that type does not
// exist on Windows and would break this package's cross-compile.
func checkYankDir(dir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(home, dir)
	if err != nil {
		return err
	}

	// home first (symlink check only), then each component agentop owns.
	fi, err := os.Lstat(home)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to write session events "+
			"through it", home)
	}

	path := home
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		path = filepath.Join(path, part)
		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink; refusing to write session events "+
				"through it", path)
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", path)
		}
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			// Tighten in place; only report if that does not work.
			if err := os.Chmod(path, 0o700); err != nil {
				return fmt.Errorf("%s has mode %v and could not be tightened "+
					"(%v); session events need no group or world access "+
					"(chmod 700 %s)", path, perm, err, path)
			}
		}
	}
	return nil
}

// yankEventToFile writes the currently-focused event to a fresh file in yankDir
// as pretty JSON and returns the path. Uses os.CreateTemp so the file is
// created with 0600 perms (session events carry identity subjects, raw
// LLM completions, and tool arguments — the operator-only default keeps
// them off shared / CI hosts). CreateTemp also implies O_EXCL, so there is no
// create-then-chmod window.
func yankEventToFile(e *pipeline.SessionEvent) (string, error) {
	dir, err := yankDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := checkYankDir(dir); err != nil {
		return "", err
	}
	ts := time.Now().UTC().Format("20060102-150405")
	// No "agentop-event-" name prefix: inside a directory already called
	// agentop-events it says nothing. The random tail stays — it is what keeps
	// two yanks in the same second from clobbering each other.
	f, err := os.CreateTemp(dir, ts+"-*.json")
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
