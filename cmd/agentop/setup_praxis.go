package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rossoctl/cortex/core/config"
)

// gatewayBinary is the praxis-mode data plane that cortex --supervise runs.
const gatewayBinary = "cortex-gateway"

// praxisPorts are praxis.listen and praxis.egress at their defaults. The port
// preflight of a fresh install probes them when setup runs with --praxis.
var praxisPorts = []string{"47605", "47606"}

// praxisHandFix is the fix for a praxis block that enablePraxisText cannot edit.
const praxisHandFix = "write `enabled: true` under praxis: by hand"

// enablePraxisText sets praxis.enabled to true in src with a line edit, so the
// file keeps its comments and its order. changed is false when src already sets
// it. A flow-style praxis: block is an error.
func enablePraxisText(src string) (out string, changed bool, err error) {
	lines := strings.Split(src, "\n")
	praxisAt, docIndent := topLevelKey(lines, "praxis:")
	if praxisAt < 0 {
		pad := strings.Repeat(" ", max(docIndent, 0))
		if src != "" && !strings.HasSuffix(src, "\n") {
			src += "\n"
		}
		return src + pad + "praxis:\n" + pad + "  enabled: true\n", true, nil
	}
	if rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[praxisAt]), "praxis:")); rest != "" && !strings.HasPrefix(rest, "#") {
		return "", false, fmt.Errorf("praxis: is written in flow style (%s)", rest)
	}
	// The block's own keys sit at the indentation of its first child line.
	childIndent := -1
	for i := praxisAt + 1; i < len(lines); i++ {
		l := lines[i]
		trimmed := strings.TrimSpace(l)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(l) - len(strings.TrimLeft(l, " "))
		if indent <= docIndent {
			break
		}
		if childIndent < 0 {
			childIndent = indent
		}
		if indent != childIndent || !strings.HasPrefix(trimmed, "enabled:") {
			continue
		}
		head := l[:indent] + "enabled:"
		rest := strings.TrimPrefix(trimmed, "enabled:")
		ci := strings.Index(rest, "#")
		if ci < 0 {
			ci = len(rest)
		}
		if strings.TrimSpace(rest[:ci]) == "true" {
			return src, false, nil
		}
		value := rest[:ci]
		ws := value[len(strings.TrimRight(value, " \t")):] // the space before a comment
		lines[i] = head + " true" + ws + rest[ci:]
		return strings.Join(lines, "\n"), true, nil
	}
	out, err = insertBlockKeys(src, "praxis", []pinnedListener{{key: "enabled", value: "true"}})
	return out, err == nil, err
}

// enablePraxis sets praxis.enabled to true in the config at path. It writes only
// a result that loads, turns praxis mode on and passes the checks cortex makes at
// start. Otherwise the file stays as it was.
func enablePraxis(path string) (changed bool, err error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the user's config
	if err != nil {
		return false, err
	}
	out, changed, err := enablePraxisText(string(raw))
	if err != nil || !changed {
		return false, err
	}
	if _, err := keepPreviousConfig(path, raw); err != nil {
		return false, err
	}
	err = replaceConfig(path, out, func(cfg *config.Config) error {
		if !cfg.PraxisEnabled() {
			return errors.New("the edit did not turn praxis mode on")
		}
		// As cortex does before it validates the config.
		cfg.Mode = config.ModeProxySidecar
		config.ApplyPreset(cfg)
		if err := config.Validate(cfg); err != nil {
			return fmt.Errorf("praxis mode refuses this config: %w", err)
		}
		return nil
	})
	return err == nil, err
}

// praxisGatewayProblem is the problem when setup with --praxis has no
// cortex-gateway to install or keep: the staged one under --from, or else the
// installed one. A praxis config with no gateway gives a proxy that answers 503.
func praxisGatewayProblem(env *setupEnv) *problem {
	dir := env.binDir
	if env.fromDir != "" {
		dir = env.fromDir
	}
	if fileExists(filepath.Join(dir, gatewayBinary)) {
		return nil
	}
	return &problem{reason: gatewayBinary + " is not installed",
		fix: []string{"install it with: " + installerOneLiner + " -s -- --praxis"}}
}
