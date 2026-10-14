package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/rossoctl/cortex/cmd/agentop/checklist"
)

// installerOneLiner is the fix for "nothing is installed": the documented install.
const installerOneLiner = "curl -fsSL https://www.rossoctl.dev/cortex/install.sh | sh"

// setupBinaryNames are what the binaries step installs; cortex-session-dump only
// when the installer staged it (its fetch is never fatal).
var setupBinaryNames = []string{"agentop", "cortex", "cortex-session-dump"}

type binariesStep struct{}

func (binariesStep) name() string { return "installed" }

func (binariesStep) plan(env *setupEnv) (stepPlan, *problem) {
	p := stepPlan{label: "installed"}
	installed := filepath.Join(env.binDir, "agentop")
	env.freshInstall = !isExecutable(installed)
	env.readVersions()
	pair := "agentop, cortex " + env.installedVersion + " · " + env.tilde(env.binDir)
	if env.fromDir == "" {
		for _, n := range []string{"agentop", "cortex"} {
			if !isExecutable(filepath.Join(env.binDir, n)) {
				return p, &problem{reason: n + " is not installed in " + env.tilde(env.binDir),
					fix: []string{installerOneLiner}}
			}
		}
		p.done, p.doneMsg = true, pair
		return p, nil
	}
	for _, n := range []string{"agentop", "cortex"} {
		if !isExecutable(filepath.Join(env.fromDir, n)) {
			return p, &problem{reason: n + " is missing from " + env.tilde(env.fromDir)}
		}
	}
	// Only binDir is asked: a ~/.cortex/previous that cannot be written fails
	// apply, which rolls back, so it fails safe.
	if prob := checkWritable(env, env.binDir); prob != nil {
		return p, prob
	}
	env.binaryChanges = nil
	for _, n := range stagedBinaries(env) {
		// binarySHA256 is "" for a file it cannot read, so an unreadable staged
		// binary over a missing installed one reads as unchanged, and plans done.
		if binarySHA256(filepath.Join(env.fromDir, n)) != binarySHA256(filepath.Join(env.binDir, n)) {
			env.binaryChanges = append(env.binaryChanges, n)
		}
	}
	if len(env.binaryChanges) == 0 {
		p.done, p.doneMsg = true, pair
		return p, nil
	}
	p.verb, p.what, p.where = "install", strings.Join(stagedBinaries(env), ", "), env.tilde(env.binDir)
	if !env.freshInstall {
		p.verb = "replace"
		p.where = env.tilde(env.binDir) + " (previous kept until healthy)"
	}
	return p, nil
}

func (binariesStep) apply(env *setupEnv, _ *checklist.Running) (string, undo, error) {
	prevExisted := dirExists(env.previousDir())
	var restores []func() error
	u := undo{label: "binaries", manual: "copy the files in " + env.tilde(env.previousDir()) + "/ back into " + env.tilde(env.binDir) +
		" (or delete agentop and cortex there, on a fresh install)"}
	for _, name := range env.binaryChanges {
		r, err := installBinary(env, filepath.Join(env.fromDir, name), filepath.Join(env.binDir, name))
		if r != nil {
			restores = append(restores, r)
		}
		if err != nil {
			u.fn = undoMoves(restores, env.previousDir(), prevExisted)
			return "", u, err
		}
	}
	u.fn = undoMoves(restores, env.previousDir(), prevExisted)
	env.onSuccess = append(env.onSuccess, func() { _ = os.RemoveAll(env.previousDir()) })
	names := strings.Join(stagedBinaries(env), ", ")
	if env.freshInstall {
		return names + " → " + env.tilde(env.binDir), u, nil
	}
	return names + " " + env.installedVersion + " → " + env.newVersion(), u, nil
}

// stagedBinaries are the setupBinaryNames present in the staging dir.
func stagedBinaries(env *setupEnv) []string {
	var out []string
	for _, n := range setupBinaryNames {
		if fileExists(filepath.Join(env.fromDir, n)) {
			out = append(out, n)
		}
	}
	return out
}

// undoMoves is the undo of a step that moved binaries through previous/: it runs
// restores newest first, then removes previous/ when this step made it. It is nil
// when nothing was moved. A previous/ an earlier, interrupted run left behind is
// kept: only a run that applies a change clears it, on success.
func undoMoves(restores []func() error, prevDir string, prevExisted bool) func() error {
	if len(restores) == 0 {
		return nil
	}
	return func() error {
		var errs []error
		for i := len(restores) - 1; i >= 0; i-- {
			errs = append(errs, restores[i]())
		}
		if !prevExisted {
			_ = os.Remove(prevDir) // only succeeds when empty, which is the point
		}
		return errors.Join(errs...)
	}
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// installBinary writes src over dst. An existing dst is copied into previous/
// first — copied, so dst stays runnable until the rename replaces it — and the
// returned func puts it back and removes the copy. What it puts back is the
// content, as a 0755 file: a dst that was a symlink, or had another mode, is not
// recreated as such.
func installBinary(env *setupEnv, src, dst string) (func() error, error) {
	prev := filepath.Join(env.previousDir(), filepath.Base(dst))
	hadOld := fileExists(dst)
	if hadOld {
		if err := os.MkdirAll(env.previousDir(), 0o700); err != nil {
			return nil, err
		}
		if err := copyFile(dst, prev, 0o755); err != nil {
			return nil, err
		}
	}
	restore := func() error {
		if !hadOld {
			return removeIfExists(dst)
		}
		if err := copyFile(prev, dst, 0o755); err != nil {
			return err
		}
		return removeIfExists(prev)
	}
	return restore, copyFile(src, dst, 0o755)
}

// installedVersion is the last word of `path --version`'s first line, as
// install.sh's installed_version reads it; "" when there is no such binary.
func installedVersion(path string) string {
	if !isExecutable(path) {
		return ""
	}
	out, err := exec.Command(path, "--version").Output() //nolint:gosec // our own binary
	if err != nil {
		return ""
	}
	f := strings.Fields(strings.SplitN(string(out), "\n", 2)[0])
	if len(f) == 0 {
		return ""
	}
	return f[len(f)-1]
}

func isExecutable(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0
}

// checkWritable asks whether setup could write at path — the path itself if it
// exists, else its nearest existing parent — without writing anything.
func checkWritable(env *setupEnv, path string) *problem {
	p := path
	for {
		if _, err := os.Stat(p); err == nil {
			break
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	const wOK = 0x2
	if err := syscall.Access(p, wOK); err != nil {
		return &problem{reason: env.tilde(p) + " is not writable (" + err.Error() + ")"}
	}
	return nil
}
