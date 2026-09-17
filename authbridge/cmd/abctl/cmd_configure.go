package main

import (
	"fmt"
	"io"
)

const configureUsage = `abctl configure — point a coding agent at Cortex

Usage:
  abctl configure claude-code enable  [--yes] [--settings PATH] [--config PATH]
  abctl configure claude-code disable [--yes] [--settings PATH]
  abctl configure claude-code status  [--settings PATH]
  abctl configure bob | codex | opencode

Agents:
  claude-code    writes the proxy and CA variables into ~/.claude/settings.json, so
                 every session on the machine goes through Cortex. Run
                 "abctl configure claude-code --help" for the detail.
  bob            not yet persistent — use "abctl exec -- bob"
  codex          not yet persistent — use "abctl exec -- codex"
  opencode       not yet persistent — use "abctl exec -- opencode"

One verb for every agent, because "how do I point X at Cortex" is the same question
whatever X is, and the answer used to be spelled differently per agent: a top-level
"abctl claude-code" for the one agent with a settings file, and nothing at all for
the ones without. The agents that cannot yet be configured persistently say so and
name the command that works today, rather than being absent and leaving the reader
to conclude Cortex cannot drive them.

Only Claude Code persists because only Claude Code reads a settings file. Everything
else reads the process environment and nothing else, so its routing lasts exactly as
long as the process — which is what "abctl exec" is for.

"abctl claude-code" is the old spelling of "abctl configure claude-code". It still
works, and prints a notice pointing here.

Exit status: whatever the agent's own action returns (0 applied or already correct,
3 declined, 1 something went wrong), 0 for an agent that only prints guidance, or 2
for a usage error.
`

// comingSoon is the message for an agent Cortex can already run but cannot yet
// configure persistently.
//
// A helper rather than three near-identical literals: the value that differs is the
// name twice over — once display-cased for the sentence, once as the binary — and
// three hand-written copies is three chances for that pair to disagree. Which is not
// hypothetical; the request this implements had exactly that slip in two of its three
// messages.
//
// Built by concatenation rather than written as a raw string because the message
// quotes `abctl exec -- <agent>` in backticks, and a backtick is what would end a raw
// literal. Printed commands are quoted this way elsewhere too (cmd_exec.go:152,
// main.go's deprecation notices).
// product is the name in the closing clause, which is not always the configuration
// name: Bob configures as "Bob" but runs as "IBM Bob".
func comingSoon(display, binary, product string) string {
	return "Persistent " + display + " configuration coming soon.  Until then, use " +
		"`abctl exec -- " + binary + "` to run " + product + " under Cortex.\n"
}

// runConfigure dispatches on the agent name. Returns the process exit code.
//
// It parses no flags of its own: everything after the agent name is handed through
// untouched, so `--yes` / `--settings` / `--config` reach claude-code's own flag set
// and behave identically under both spellings.
func runConfigure(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, configureUsage)
		return 2
	}
	agent := args[0]
	// Same shape as `abctl service` (cmd_service.go) and, as of the same change,
	// `abctl claude-code`: an explicit --help is a request for the list, so it must
	// not be read as the name of an agent that does not exist.
	switch agent {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, configureUsage)
		return 0
	}

	switch agent {
	case "claude-code":
		// The whole point of this subcommand: identical behaviour to the old top-level
		// spelling, because it IS that spelling's implementation, called with the
		// action and flags untouched.
		//
		// The deprecation notice lives in main's `claude-code` dispatch arm, which is
		// the only place that knows which spelling the user typed — so it does not
		// fire here. A user who already typed the current spelling must not be told to
		// type something else.
		return runClaudeCode(args[1:], stdout, stderr)
	case "bob":
		fmt.Fprint(stdout, comingSoon("Bob", "bob", "IBM Bob"))
		return 0
	case "codex":
		fmt.Fprint(stdout, comingSoon("Codex", "codex", "Codex"))
		return 0
	case "opencode":
		fmt.Fprint(stdout, comingSoon("OpenCode", "opencode", "OpenCode"))
		return 0
	default:
		// The named list is the answer to a typo; the usage block after it is the
		// answer to "what else can this do", which is what someone who guessed an
		// agent name wrong most likely wanted. Same pairing as the no-argument case.
		fmt.Fprintf(stderr, "abctl: unknown agent %q (claude-code, bob, codex, opencode)\n", agent)
		fmt.Fprint(stderr, configureUsage)
		return 2
	}
}
