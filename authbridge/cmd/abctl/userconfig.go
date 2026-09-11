package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/tui"
)

// userCfgRel is abctl's own settings file, relative to the user's home.
//
// A sibling of ~/.cortex/config.yaml (the PROXY's config), in the same tree but
// under a distinct name, because the two have nothing to do with each other: one
// is operator configuration the proxy reads at startup, this one is UI state abctl
// writes back as the user works. Follows cortexCfgRel / stateRel and tui's
// yankDirRel so there is one ~/.cortex tree rather than a new dotfile per tool.
const userCfgRel = ".cortex/abctl-config.yaml"

// userConfigPath returns ~/.cortex/abctl-config.yaml.
//
// Errors rather than falling back to a relative path: a settings file written into
// whatever directory abctl happened to start in would be found again only by
// accident, and the caller's fallback (no persistence) is honest about what
// happened. UserHomeDir only fails when $HOME is unset — a bare `env -i`, some
// systemd units — so this costs nothing anyone hits by accident.
func userConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine your home directory: %w", err)
	}
	if home == "" {
		// A separate branch, not folded into the one above: %w on a nil error renders
		// as "%!w(<nil>)", which would make this path unreadable to whoever hits it.
		return "", errors.New("cannot determine your home directory: it is empty")
	}
	return filepath.Join(home, userCfgRel), nil
}

// loadUserConfig reads the settings file, degrading to defaults rather than
// failing. It never returns an error, because there is no error worth stopping for:
// settings are a convenience, and a corrupt one must not keep the viewer from
// opening — the viewer being the tool you reach for when everything else is broken.
//
// A missing file is silent: nobody has one until they change something, and
// scolding a first run for that would be noise on every fresh machine. Anything
// else — unreadable, unparseable, a directory where a file should be — warns once
// on warn and continues, because silently ignoring a file the user hand-edited
// would look like abctl ignoring their edit.
//
// warn is an io.Writer rather than a hardcoded os.Stderr so tests assert on the
// message. Callers must pass a writer that is safe to use before bubbletea takes
// the alt screen; after that, writing to the terminal corrupts the frame.
func loadUserConfig(path string, warn io.Writer) tui.UserSettings {
	if path == "" {
		return tui.UserSettings{}
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(warn, "abctl: ignoring %s: %v; using default settings\n", path, err)
		}
		return tui.UserSettings{}
	}
	var s tui.UserSettings
	if err := yaml.Unmarshal(body, &s); err != nil {
		fmt.Fprintf(warn, "abctl: ignoring %s: %v; using default settings\n", path, err)
		// Discard the struct entirely rather than keeping what parsed: yaml.v3 can
		// populate some fields before erroring, and half a config applied silently is
		// harder to diagnose than none.
		return tui.UserSettings{}
	}
	return s
}

// saveUserConfig writes s to path, creating ~/.cortex if it is not there.
//
// abctl does not otherwise create that directory — the proxy's writeBuiltinConfig
// does, and tui's yank path is the one exception — so on a machine that has only
// ever run the viewer, this is the first thing to make it. 0700 matches what
// writeBuiltinConfig sets.
//
// SECURITY, considered rather than skipped: this deliberately does NOT use the
// Lstat-every-component sweep that tui's checkYankDir applies to the same tree.
// That sweep exists because yanked events carry identity subjects, raw LLM
// completions and tool arguments, so a redirected write leaks secrets. This file
// holds a filter string and a list of column names — a redirect leaks nothing worth
// having, and the sweep's other half (chmod 0700 in place) would mean that merely
// opening the viewer retightens a directory the user or an installer deliberately
// set. The residual symlink risk is closed more cheaply instead: O_EXCL refuses a
// pre-planted tempfile, and os.Rename replaces a symlink at the destination rather
// than following it.
//
// What would change that calculus: any future setting holding a secret, a
// filesystem path, or a command line. At that point this needs checkYankDir's
// posture, not this comment.
func saveUserConfig(path string, s tui.UserSettings) error {
	if path == "" {
		return errors.New("no settings path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	// Atomic: a crash between truncate and write would otherwise leave a half-file
	// that the next start reports as malformed. Same shape cmd_config_migrate.go uses.
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(fileHeader); err == nil {
		_, err = f.Write(body)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// fileHeader explains the file to whoever opens it, since it is meant to be
// hand-editable. The absent-means-visible rule is the one thing that is not
// guessable from the contents.
var fileHeader = []byte("# abctl user settings. Written by abctl; safe to hand-edit or delete.\n" +
	"# Columns not listed under events.columns are visible — only deviations are recorded.\n")
