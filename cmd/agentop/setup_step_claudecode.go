package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rossoctl/cortex/cmd/agentop/checklist"
	"github.com/rossoctl/cortex/core/config"
)

// claudeCodeStep routes Claude Code through Cortex by writing the managed keys
// into ~/.claude/settings.json, as `agentop configure claude-code enable` does.
type claudeCodeStep struct{}

func (claudeCodeStep) name() string { return "routed" }

func (claudeCodeStep) paths(env *setupEnv) (settings, state string) {
	return filepath.Join(env.home, settingsRel), filepath.Join(env.home, stateRel)
}

func (s claudeCodeStep) plan(env *setupEnv) (stepPlan, *problem) {
	p := stepPlan{label: "routed"}
	settings, _ := s.paths(env)
	if env.configFresh {
		// No config yet, so no planClaudeCodeEnable: the built-in config the config
		// step will write always yields Cortex-shaped values, which makes this the
		// same refusal planClaudeCodeEnable would give.
		doc, err := readSettings(settings)
		if err != nil {
			return p, &problem{reason: env.tildeText(err.Error())}
		}
		vals := envStrings(doc)
		for _, k := range managedKeys {
			if cur, ok := vals[k]; ok && !isCortexValue(k, cur) {
				return p, &problem{
					reason: fmt.Sprintf("%s is already set to %q in %s", k, cur, env.tilde(settings)),
					fix:    []string{"remove it, or edit the file by hand, then " + env.rerun()},
				}
			}
		}
	} else if _, err := config.Load(env.configPath()); err == nil {
		// A config that will not load is the config step's problem to report; the
		// run stops on it, so this plan is never applied.
		pl, err := planClaudeCodeEnable(settings, env.configPath())
		if err != nil {
			lines := strings.Split(env.tildeText(err.Error()), "\n")
			return p, &problem{reason: lines[0], fix: trimAll(lines[1:])}
		}
		if len(pl.changes) == 0 {
			p.done, p.doneMsg = true, "Claude Code already routed"
			return p, nil
		}
	}
	p.verb, p.what, p.where = "route", "Claude Code via Cortex", env.tilde(settings)
	if backupWillBeWritten(settings) {
		p.where += " (.bak kept)"
	}
	return p, nil
}

// already tops up the ownership record on the path where the plan found nothing
// to change, which is the only path most installs ever take again: `install.sh
// --claude-code` re-runs on every upgrade and hands off to `agentop setup
// --claude-code`, so with settings already holding Cortex's values this step is
// "Claude Code already routed" and apply never runs. A record written before
// Written existed would then never gain it, and disable would go on treating a
// later edit of the user's as Cortex's own value and remove it. topUpWritten says
// what it will and will not write; this is the hook that reaches it, since the
// `agentop configure claude-code enable` path that also calls it is not what an
// upgrade runs.
//
// Called from applySteps only. Not from plan, which must change nothing on disk
// and which `agentop doctor` calls for every step.
func (s claudeCodeStep) already(env *setupEnv) string {
	if env.configFresh {
		// No config to read the wanted values from, and nothing routed yet either:
		// a fresh config means this step has never run here.
		return ""
	}
	settings, state := s.paths(env)
	pl, err := planClaudeCodeEnable(settings, env.configPath())
	if err != nil || len(pl.changes) > 0 {
		// Nothing a done row can act on: a refusal is plan's to report, and changes
		// appearing after the plan mean the file moved under us, which the next run
		// picks up. Neither is worth a line here.
		return ""
	}
	var errb bytes.Buffer
	topUpWritten(pl, state, &errb)
	return stderrNotes(env, errb.String())
}

// backupWillBeWritten is writeSettings' rule for its .bak: a copy of the file,
// written once, so only when there is a file and no .bak yet.
func backupWillBeWritten(settings string) bool {
	if _, err := os.Stat(settings); err != nil {
		return false
	}
	_, err := os.Stat(settings + ".bak")
	return os.IsNotExist(err)
}

// apply snapshots settings.json, its .bak and the state file, so the undo puts
// back those bytes rather than running disable, which re-marshals the file; and
// it removes ~/.claude again if the write made it and nothing else has used it.
// A re-plan that finds nothing left to change returns no undo at all: it tops the
// record up, as already does on the path where apply never runs, and that needs no
// reversing — see the return itself.
func (s claudeCodeStep) apply(env *setupEnv, _ *checklist.Running) (string, undo, error) {
	settings, state := s.paths(env)
	dir := filepath.Dir(settings)
	_, statErr := os.Stat(dir)
	dirExisted := statErr == nil
	var snaps []fileSnapshot
	for _, f := range []string{settings, settings + ".bak", state} {
		snap, err := snapshotFile(f)
		if err != nil {
			return "", undo{}, err
		}
		snaps = append(snaps, snap)
	}
	u := undo{label: env.tilde(settings), fn: func() error {
		var errs []error
		for _, snap := range snaps {
			errs = append(errs, snap.restore())
		}
		if entries, err := os.ReadDir(dir); !dirExisted && err == nil && len(entries) == 0 {
			errs = append(errs, os.Remove(dir))
		}
		return errors.Join(errs...)
	}, manual: "agentop configure claude-code disable"}
	// Planned again now: the plan before consent may predate the config, and the
	// file may have changed while the steps before this one ran.
	pl, err := planClaudeCodeEnable(settings, env.configPath())
	if err != nil {
		lines := strings.Split(env.tildeText(err.Error()), "\n")
		return "", undo{}, stepError{reason: lines[0], detail: trimAll(lines[1:])}
	}
	if len(pl.changes) == 0 {
		// The plan before consent found changes and they are gone now: the file moved
		// while the steps before this one ran. Top up the record all the same, as the
		// done path does — the reason it needs topping up is the same.
		//
		// And no undo for it, as the done path has none either: the top-up only writes
		// what settings.json already holds, and this return leaves settings.json
		// alone, so a rollback has nothing to put back. Returning u instead would
		// restore the file from the snapshot above, over any edit made since.
		var errb bytes.Buffer
		topUpWritten(pl, state, &errb)
		return "Claude Code already routed" + stderrNotes(env, errb.String()), undo{}, nil
	}
	var errb bytes.Buffer
	if err := applyClaudeCodeEnable(pl, state, &errb); err != nil {
		return "", u, err
	}
	// Its warning, a prior-settings record it could not write, as rows below.
	return "Claude Code → Cortex" + stderrNotes(env, errb.String()), u, nil
}

// trimAll is lines with each trimmed and the empty ones dropped.
func trimAll(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
