package main

import (
	"bytes"
	"strings"
	"testing"
)

// claude-code used to take `status --yes`, a stray argument, and `--help` after the action,
// and checked the action only after the home directory. It now parses as the other agents do.
func TestRunClaudeCode_ArgumentsParseLikeTheOtherAgents(t *testing.T) {
	t.Setenv("HOME", "") // a late action check would exit 1 here, not 2
	for _, tc := range []struct {
		name     string
		args     []string
		code     int
		onStdout bool
	}{
		{name: "no args", args: nil, code: 2},
		{name: "--help", args: []string{"--help"}, code: 0, onStdout: true},
		{name: "help after the action", args: []string{"status", "--help"}, code: 0, onStdout: true},
		{name: "unknown action", args: []string{"enabel"}, code: 2},
		{name: "status --yes", args: []string{"status", "--yes"}, code: 2},
		{name: "a stray argument", args: []string{"enable", "now"}, code: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := runClaudeCode(tc.args, &out, &errb)
			if code != tc.code {
				t.Errorf("exit %d, want %d; stderr %q", code, tc.code, errb.String())
			}
			if got := strings.Contains(out.String(), claudeCodeUsage); got != tc.onStdout {
				t.Errorf("usage on stdout = %v, want %v:\n%s", got, tc.onStdout, out.String())
			}
		})
	}
}
