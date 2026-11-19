package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/observe/claude"
)

// The launch-time opt-out (#1923): `agentop exec --no-record`, or CORTEX_RECORD=off in
// agentop's own environment, asks Cortex not to record the child's session. Claude Code only
// for now: the child gets model.RecordHeader through ANTHROPIC_CUSTOM_HEADERS, which Claude
// Code adds to every request it sends. The proxy honours the header for a request that names
// its own session, which Claude Code's requests do (X-Claude-Code-Session-Id).

// recordEnv is the variable that asks for the opt-out, like --no-record. Only model.RecordOff
// counts, without case or padding, as for the header itself.
const recordEnv = "CORTEX_RECORD"

// customHeadersEnv is Claude Code's list of extra request headers: "Name: value" lines.
const customHeadersEnv = "ANTHROPIC_CUSTOM_HEADERS"

// noRecordWanted reports whether this launch asks not to be recorded.
func noRecordWanted(flagSet bool, getenv func(string) string) bool {
	return flagSet || strings.EqualFold(strings.TrimSpace(getenv(recordEnv)), model.RecordOff)
}

// withRecordOff returns headers, a Claude Code ANTHROPIC_CUSTOM_HEADERS value, with
// model.RecordHeader set to off. The headers already there are kept, in order; an
// X-Cortex-Record line already there is replaced, so the child sends exactly one.
func withRecordOff(headers string) string {
	var keep []string
	for _, line := range strings.Split(headers, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		name, _, _ := strings.Cut(line, ":")
		if strings.EqualFold(strings.TrimSpace(name), model.RecordHeader) {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(append(keep, model.RecordHeader+": "+model.RecordOff), "\n")
}

// customHeadersOverride returns the first Claude Code settings file whose "env" block sets
// ANTHROPIC_CUSTOM_HEADERS: the user's (under home, and under configDir, Claude Code's
// CLAUDE_CONFIG_DIR, when set) and the project's, in dir. Claude Code applies a settings env
// block over the process environment, so such a file would replace the value given to the
// child and the session would be recorded, silently. A file that cannot be read as settings is
// an error: whether it sets the variable is unknown. Not checked: managed settings, a file
// given with `claude --settings`, and project settings above dir.
func customHeadersOverride(home, configDir, dir string) (string, error) {
	var paths []string
	if home != "" {
		paths = append(paths, filepath.Join(home, settingsRel))
	}
	if configDir != "" {
		paths = append(paths, filepath.Join(configDir, "settings.json"))
	}
	if dir != "" {
		paths = append(paths, filepath.Join(dir, ".claude", "settings.json"), filepath.Join(dir, ".claude", "settings.local.json"))
	}
	for _, p := range paths {
		doc, err := readSettings(p)
		if err != nil {
			return "", err
		}
		if env, ok := doc["env"].(map[string]any); ok {
			if _, set := env[customHeadersEnv]; set {
				return p, nil
			}
		}
	}
	return "", nil
}

// addRecordOff adds the opt-out to inject for a child that runs cmd ("" for --print), or
// refuses: for a child that is not Claude Code, which would ignore the header and be recorded,
// and when a settings file would override it.
func addRecordOff(inject map[string]string, cmd, home, dir string, getenv func(string) string) error {
	if cmd != "" && !isClaudeCode(cmd) {
		return fmt.Errorf("--no-record supports Claude Code only for now; %s would be recorded", filepath.Base(cmd))
	}
	p, err := customHeadersOverride(home, getenv(claude.ConfigDirEnv), dir)
	if err != nil {
		return fmt.Errorf("--no-record: could not check the Claude Code settings for %s: %w", customHeadersEnv, err)
	}
	if p != "" {
		return fmt.Errorf("--no-record: %s sets %s in its \"env\" block, which Claude Code applies over the\n"+
			"  environment, so the child would be recorded anyway. Add %q to that value\n"+
			"  instead, or remove it from the file and run this again", p, customHeadersEnv, model.RecordHeader+": "+model.RecordOff)
	}
	inject[customHeadersEnv] = withRecordOff(getenv(customHeadersEnv))
	return nil
}

// isClaudeCode reports whether cmd, a child's argv[0], runs the Claude Code CLI, by its base
// name as warnOpenCodeService recognizes OpenCode.
func isClaudeCode(cmd string) bool {
	base := filepath.Base(cmd)
	return base == "claude" || strings.EqualFold(base, "claude.exe")
}

// launchDirs is where customHeadersOverride looks: the user's home and the current directory.
// Either is "" when it cannot be found, and its files are then not checked.
func launchDirs() (home, dir string) {
	home, _ = os.UserHomeDir()
	dir, _ = os.Getwd()
	return home, dir
}
