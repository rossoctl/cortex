// profile-tags emits the CSV of `include_plugin_*` and `include_driver_*` build
// tags for a named profile. It replaces the retired lite-tags generator, which derived a
// negative tag list for one variant back when plugins were compiled in by default.
//
// Every plugin and driver is opt-in: nothing is linked unless a tag names it, so
// an artifact's contents are exactly its entries in profiles.go. A build with
// no tags registers no plugins and rejects any config naming one.
//
// Usage: every call site uses `go -C <path>/scripts/profile-tags run . <profile>`,
// which chdirs to this module before exec so the relative plugin paths the guard
// tests use resolve correctly.
//
//	go -C scripts/profile-tags run . local
//	include_plugin_a2aparser,include_plugin_inferenceparser,...
package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	out, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(out)
}

// run resolves a profile name to its build-tag CSV. Separated from main so the
// argument contract is testable without spawning a process.
func run(args []string) (string, error) {
	if len(args) != 1 {
		return "", fmt.Errorf("usage: profile-tags <profile> (known: %v)", known())
	}
	tags, err := Tags(ProfileName(args[0]))
	if err != nil {
		return "", err
	}
	return strings.Join(tags, ","), nil
}
