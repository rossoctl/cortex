package main

import (
	"fmt"
	"io"
)

// unsupportedOnWindows are the subcommands that install, run or replace Cortex as a service.
// On Windows they need a service backend, a way to stop the proxy without killing it, and a
// way to replace a running .exe, none of which exists yet (#1520). Everything else in agentop
// talks to a running proxy over HTTP, or edits a settings file, and works there.
var unsupportedOnWindows = map[string]bool{
	"service":   true,
	"setup":     true,
	"uninstall": true,
	"update":    true,
}

// refuseUnsupported reports whether subcommand cmd cannot run on goos, and says so on stderr.
// goos is a parameter so the rule is testable on every OS.
func refuseUnsupported(goos, cmd string, stderr io.Writer) bool {
	if goos != "windows" || !unsupportedOnWindows[cmd] {
		return false
	}
	fmt.Fprintf(stderr, "agentop: `agentop %s` is not supported on Windows yet (#1520).\n"+
		"  Run the proxy in a terminal instead: cortex --local\n", cmd)
	return true
}
