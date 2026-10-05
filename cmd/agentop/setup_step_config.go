package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rossoctl/cortex/cmd/agentop/checklist"
	"github.com/rossoctl/cortex/core/config"
)

// configStep writes the built-in config through the installed cortex, or keeps
// the existing one, and runs both migrations itself, so service install's are
// no-ops after it.
type configStep struct{}

func (configStep) name() string { return "config" }

func (configStep) plan(env *setupEnv) (stepPlan, *problem) {
	p := stepPlan{label: "config"}
	cfg := env.configPath()
	if !fileExists(cfg) {
		env.configFresh = true
		p.verb, p.what, p.where = "write", "the cortex config", env.tilde(cfg)
		return p, nil
	}
	if _, err := config.Load(cfg); err != nil {
		return p, &problem{reason: env.tilde(cfg) + " will not load: " + err.Error(),
			fix: []string{"correct it, then " + env.rerun(),
				"or, to start over from the built-in config, move it aside first: mv " +
					env.shellPath(cfg) + " " + env.shellPath(cfg+".broken")}}
	}
	pending, err := configMigrationPending(cfg)
	if err != nil {
		return p, &problem{reason: err.Error()}
	}
	// Listeners on every interface that the pins cannot bind to loopback are what
	// apply and service install refuse, whether or not a migration is pending.
	// Refused here, the run stops before any step changes anything.
	if listenerPinsBlocked(cfg) {
		if exposed := wildcardListeners(cfg); len(exposed) > 0 {
			return p, &problem{reason: env.tilde(cfg) + " binds " + strings.Join(exposed, ", ") +
				" on every interface, and the listener pins cannot be added",
				fix: []string{pinsRemedy}}
		}
	}
	if !pending {
		p.done, p.doneMsg = true, env.tilde(cfg)+" (kept)"
		return p, nil
	}
	env.configPinsPending = listenerPinsPending(cfg)
	p.verb, p.what, p.where = "update", "the cortex config", env.tilde(cfg)+" (previous kept)"
	return p, nil
}

// apply snapshots the config and both migration backups, so the undo puts back
// exactly what was there. Its migration failures follow service install's rule.
// A failed pin migration fails the step only when the config binds a listener on
// every interface, and otherwise warns in the detail line. A Bob pricing failure
// only warns, because an unpriced Bob exposes nothing.
func (configStep) apply(env *setupEnv, _ *checklist.Running) (string, undo, error) {
	cfg := env.configPath()
	// Whether ~/.cortex was there before this step, not before this run: the undo
	// keeps one an earlier step made, as the binaries step does for previous/.
	_, statErr := os.Stat(env.cortexDir)
	dirExisted := statErr == nil
	var snaps []fileSnapshot
	for _, f := range []string{cfg, cfg + ".before-agentop-migrate", cfg + ".before-agentop-pricing"} {
		s, err := snapshotFile(f)
		if err != nil {
			return "", undo{}, err
		}
		snaps = append(snaps, s)
	}
	u := undo{label: env.tilde(cfg), fn: func() error {
		var errs []error
		for _, s := range snaps {
			errs = append(errs, s.restore())
		}
		if !dirExisted {
			_ = os.Remove(env.cortexDir) // only succeeds when empty, which is the point
		}
		return errors.Join(errs...)
	}}
	manual := func(bak string) string {
		return "restore " + env.tilde(cfg) + " from " + env.tilde(cfg) + bak + ", or delete it on a fresh install"
	}
	u.manual = manual(".before-agentop-migrate")
	detail := env.tilde(cfg)
	if env.configFresh {
		// No timeout: --write-config writes one file and exits, and a hang here is
		// a hang of setup.
		out, err := exec.Command(filepath.Join(env.binDir, "cortex"), "--local", "--write-config").CombinedOutput() //nolint:gosec
		if err != nil {
			return "", u, stepError{reason: "cortex --local --write-config failed", detail: tailLines(string(out), 5)}
		}
		if !fileExists(cfg) {
			return "", u, stepError{reason: "cortex --local --write-config wrote no " + env.tilde(cfg)}
		}
	}
	// wildcardListeners reports nothing for a config that will not load, so such a
	// config is refused here, as service install refuses it before migrating.
	if _, err := config.Load(cfg); err != nil {
		return "", u, stepError{reason: env.tilde(cfg) + " will not load", detail: []string{firstLine(err)}}
	}
	var log bytes.Buffer
	changed, pinsErr := migrateConfig(cfg, &log)
	// Not Bob's rate: a running proxy reloads pricing from the file by itself.
	env.configPinsChanged = changed
	if pinsErr != nil {
		if exposed := wildcardListeners(cfg); len(exposed) > 0 {
			return "", u, stepError{
				reason: env.tilde(cfg) + " binds " + strings.Join(exposed, ", ") +
					" on every interface, and the listener pins could not be added",
				detail: []string{firstLine(pinsErr), pinsRemedy},
			}
		}
	}
	priced, bobErr := migrateBobPricing(cfg, &log)
	if !changed && priced {
		// .before-agentop-migrate is written once, so it may be older than this
		// run; Bob's backup is rewritten by each pricing migration.
		u.manual = manual(".before-agentop-pricing")
	}
	if !env.configFresh && (changed || priced) {
		detail += " · updated"
	}
	if pinsErr != nil {
		detail += " · listener pins not added (" + firstLine(pinsErr) + ")"
	}
	if bobErr != nil {
		detail += " · IBM Bob left unpriced (" + firstLine(bobErr) + ")"
	}
	return detail, u, nil
}

// pinsRemedy is service install's remedy for listeners the pin migration cannot
// bind to loopback.
const pinsRemedy = "add `bind_loopback_only: true` under listener:, or delete the file and run: cortex --local --write-config"

// listenerPinsBlocked reports whether the file at path lacks listener pins that
// migrateConfig cannot add.
func listenerPinsBlocked(path string) bool {
	raw, err := os.ReadFile(path) //nolint:gosec // the user's config
	if err != nil {
		return false
	}
	missing, err := missingListenerPins(raw)
	return err == nil && len(missing) > 0 && !listenerPinsPending(path)
}

// listenerPinsPending is pinsMigrationPending for the file at path, with any
// error, the file's own included, read as not pending.
func listenerPinsPending(path string) bool {
	raw, err := os.ReadFile(path) //nolint:gosec // the user's config
	if err != nil {
		return false
	}
	pending, err := pinsMigrationPending(raw)
	return err == nil && pending
}

// firstLine is err's message up to its first newline.
func firstLine(err error) string { return strings.SplitN(err.Error(), "\n", 2)[0] }

// tailLines is the last n non-empty lines of s.
func tailLines(s string, n int) []string {
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}
