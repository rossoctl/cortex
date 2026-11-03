package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// agentArgs is what parseAgentArgs read from `agentop configure <agent> <action> [flags]`.
type agentArgs struct {
	action string
	yes    bool
	home   string
}

// parseAgentArgs is the front half every `configure <agent>` command shares: help, the
// verb check, the flags, and the home directory, in that order, so nothing touches the
// machine before the arguments are known good. flags adds the agent's own flags to the
// set; everything after the home directory stays per-agent.
//
// done reports that the command is finished and code is its exit code: explicit help
// goes to stdout with 0, a missing or bad action or flag to stderr with 2, and no home
// directory with 1. --yes exists only for enable and disable, because status changes
// nothing and a flag that parses and does nothing would mislead.
func parseAgentArgs(agent, usage string, args []string, stdout, stderr io.Writer, flags func(*flag.FlagSet)) (a agentArgs, code int, done bool) {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return a, 2, true
	}
	a.action = args[0]
	switch a.action {
	case "-h", "--help", "help":
		_, _ = fmt.Fprint(stdout, usage)
		return a, 0, true
	case "enable", "disable", "status":
	default:
		_, _ = fmt.Fprintf(stderr, "agentop: unknown %s action %q (enable, disable, status)\n", agent, a.action)
		return a, 2, true
	}

	fs := flag.NewFlagSet("configure "+agent+" "+a.action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {}
	if flags != nil {
		flags(fs)
	}
	var yes *bool
	if a.action != "status" {
		yes = fs.Bool("yes", false, "do not prompt for confirmation")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprint(stdout, usage)
			return a, 0, true
		}
		return a, 2, true
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "agentop: %s %s takes no arguments (got %q)\n", agent, a.action, fs.Arg(0))
		return a, 2, true
	}
	a.yes = yes != nil && *yes

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		_, _ = fmt.Fprintf(stderr, "agentop: cannot determine your home directory: %v\n", err)
		return a, 1, true
	}
	a.home = home
	return a, 0, false
}

// askYesNo prints prompt to w and reads one answer from r. y or yes (any case) is yes, and
// an empty line is enterYes. EOF before any answer is never consent.
func askYesNo(r io.Reader, w io.Writer, prompt string, enterYes bool) bool {
	_, _ = fmt.Fprint(w, prompt)
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	case "":
		return enterYes
	}
	return false
}
