package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/rossoctl/cortex/cmd/agentop/checklist"
)

// staleBinaries are the pre-rename names: abctl is agentop now, and
// authbridge-proxy is cortex.
var staleBinaries = []string{"abctl", preRenameBinary}

// cleanupStep is install.sh's remove_stale. afterService admits authbridge-proxy,
// which install.sh removes only once the service has moved to cortex.
type cleanupStep struct{ afterService bool }

func (cleanupStep) name() string { return "cleaned up" }

func (c cleanupStep) candidates(env *setupEnv) []string {
	var out []string
	for _, old := range staleBinaries {
		if old == preRenameBinary && !c.afterService {
			continue
		}
		if isOurStaleBinary(filepath.Join(env.binDir, old), old) {
			out = append(out, old)
		}
	}
	return out
}

func (c cleanupStep) plan(env *setupEnv) (stepPlan, *problem) {
	p := stepPlan{label: "cleaned up"}
	names := c.candidates(env)
	if len(names) == 0 {
		p.done, p.hidden = true, true
		return p, nil
	}
	p.verb, p.what, p.where = "remove", "pre-rename "+strings.Join(names, ", "), env.tilde(env.binDir)
	return p, nil
}

// apply returns an undo, and leaves previous/ to be cleared on success, only when
// it moved something: a run that kept every binary changed nothing.
func (c cleanupStep) apply(env *setupEnv, _ *checklist.Running) (string, undo, error) {
	prevExisted := dirExists(env.previousDir())
	pidFile := filepath.Join(env.cortexDir, proxyPIDFile)
	var restores []func() error
	u := undo{label: "pre-rename binaries", manual: "copy the files in " + env.tilde(env.previousDir()) + "/ back into " + env.tilde(env.binDir)}
	var removed, keptForUnit, keptForPID []string
	for _, old := range c.candidates(env) {
		if unitStillNames(env, old) {
			keptForUnit = append(keptForUnit, old)
			continue
		}
		if old == preRenameBinary && pidfileProcessUnnamed(pidFile) {
			keptForPID = append(keptForPID, old)
			continue
		}
		r, err := moveToPrevious(env, filepath.Join(env.binDir, old))
		if r != nil {
			restores = append(restores, r)
		}
		if err != nil {
			u.fn = undoMoves(restores, env.previousDir(), prevExisted)
			return "", u, err
		}
		removed = append(removed, old)
	}
	u.fn = undoMoves(restores, env.previousDir(), prevExisted)
	var parts []string
	if len(removed) > 0 {
		env.onSuccess = append(env.onSuccess, func() { _ = os.RemoveAll(env.previousDir()) })
		parts = append(parts, "removed "+strings.Join(removed, ", "))
	}
	if len(keptForUnit) > 0 {
		// install.sh's remove_stale, both lines of it.
		parts = append(parts, "kept "+strings.Join(keptForUnit, ", ")+": the service still runs it;"+
			" `agentop service install` moves the service over, then delete the old file")
	}
	if len(keptForPID) > 0 {
		parts = append(parts, "kept "+strings.Join(keptForPID, ", ")+": the process in "+env.tilde(pidFile)+
			" may still run it (ps cannot name it)")
	}
	return strings.Join(parts, "; "), u, nil
}

// moveToPrevious moves path into previous/ — a copy, then a remove, so it works
// when the two are on different filesystems — and returns the func that copies it
// back and removes the copy. The func is nil when nothing was moved. It copies back
// the content, as a 0755 file: a symlink or another mode is not recreated.
func moveToPrevious(env *setupEnv, path string) (func() error, error) {
	prev := filepath.Join(env.previousDir(), filepath.Base(path))
	if err := os.MkdirAll(env.previousDir(), 0o700); err != nil {
		return nil, err
	}
	if err := copyFile(path, prev, 0o755); err != nil {
		return nil, err
	}
	restore := func() error {
		if err := copyFile(prev, path, 0o755); err != nil {
			return err
		}
		return removeIfExists(prev)
	}
	return restore, removeIfExists(path)
}

// isOurStaleBinary is remove_stale's ownership rule: the file carries our module
// path. It is never executed — a stranger's abctl may do anything when run.
func isOurStaleBinary(path, old string) bool {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	b, err := os.ReadFile(path) //nolint:gosec // a file in our own bin dir
	if err != nil {
		return false
	}
	re := regexp.MustCompile(`github\.com/rossoctl/cortex/(authbridge/)?cmd/` + regexp.QuoteMeta(old))
	return re.Match(b)
}

// unitStillNames is remove_stale's unit guard: either unit file, whatever the OS,
// containing /<old>.
func unitStillNames(env *setupEnv, old string) bool {
	for _, unit := range []string{
		filepath.Join(env.home, "Library", "LaunchAgents", launchdLabel+".plist"),
		filepath.Join(env.home, ".config", "systemd", "user", systemdUnit),
	} {
		if b, err := os.ReadFile(unit); err == nil && bytes.Contains(b, []byte("/"+old)) { //nolint:gosec
			return true
		}
	}
	return false
}
