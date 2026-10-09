package main

import (
	"fmt"
	"io"
)

const configureUsage = `agentop configure — point a coding agent at Cortex

Usage:
  agentop configure claude-code enable  [--yes] [--settings PATH] [--config PATH]
  agentop configure claude-code disable [--yes] [--settings PATH]
  agentop configure claude-code status  [--settings PATH]
  agentop configure bob enable  [--yes] [--settings PATH] [--config PATH]
  agentop configure bob disable [--yes] [--settings PATH] [--config PATH]
  agentop configure bob status  [--settings PATH] [--config PATH]
  agentop configure bobshell enable | disable | status
  agentop configure opencode enable | disable [--yes] [--config PATH] [--opencode BIN]
  agentop configure opencode status [--config PATH] [--opencode BIN]
  agentop configure codex enable  [--yes] [--env PATH] [--config PATH]
  agentop configure codex disable [--yes] [--env PATH]
  agentop configure codex status  [--env PATH] [--config PATH]

Agents:
  claude-code    writes the proxy and CA variables into ~/.claude/settings.json, so
                 every session on the machine goes through Cortex. Run
                 "agentop configure claude-code --help" for the detail.
  bob            writes "http.proxy" into IBM Bob's settings.json, so the Bob
                 editor itself goes through Cortex. Run
                 "agentop configure bob --help" for the detail.
  bobshell       defines a "bob" shell function in your shell's rc file, so typing
                 "bob" runs it through Cortex. A different thing from "bob" above,
                 and the two are independent. Run
                 "agentop configure bobshell --help" for the detail.
  opencode       sets the proxy and CA variables in OpenCode's background-service
                 environment, which sends all of OpenCode's traffic. Run
                 "agentop configure opencode --help" for the detail.
  codex          writes the proxy and CA variables into Codex's own ~/.codex/.env,
                 which Codex loads itself at startup. Run
                 "agentop configure codex --help" for the detail.

One verb for every agent, because "how do I point X at Cortex" is the same question
whatever X is, and the answer used to be spelled differently per agent: a top-level
"agentop claude-code" for the one agent with a settings file, and nothing at all for
the ones without.

Five agents persist, by four different mechanisms. Claude Code and Bob read settings
files, so their configuration goes there — the key differs (Claude Code keeps an "env"
block, Bob is a VS Code fork and reads "http.proxy"), and Bob additionally needs the
bridge CA trusted by the OS, which "configure bob enable" prints rather than performs.
Bob Shell gets a shell function written into the rc file instead, so the routing is
applied when you type the command. OpenCode's background service keeps an environment
of its own, so its configuration goes there, through the opencode CLI. Codex has no
settings file and no background service, but loads a dotenv file, ~/.codex/.env, on
its own at startup — confirmed against the real Codex CLI — so its configuration goes
there, scanning the whole file for an existing definition of each key rather than only
a block agentop wrote itself.

"agentop claude-code" is the old spelling of "agentop configure claude-code". It still
works, and prints a notice pointing here.

Exit status: whatever the agent's own action returns (0 applied or already correct,
3 declined, 1 something went wrong), 0 for an agent that only prints guidance, or 2
for a usage error.
`

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
	// Same shape as `agentop service` (cmd_service.go) and, as of the same change,
	// `agentop claude-code`: an explicit --help is a request for the list, so it must
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
		// Two distinct agents, both spelled with "bob", because there are two
		// separate things to configure and they persist differently:
		//
		//	bob       IBM Bob the editor. A VS Code fork, so it has a settings.json
		//	          and reads "http.proxy" from it.
		//	bobshell  the shell integration — a "bob" function in the rc file, so
		//	          typing "bob" at a prompt runs through Cortex.
		//
		// This arm reverses part of #1133, which removed "configure bob" reasoning
		// that "IBM Bob itself needs no configuring". That was right about the
		// binary and wrong about the editor: Bob has a settings file, and without
		// this a Bob user had to find "http.proxy" and the CA trust step by hand.
		// Configuring one does not configure the other; keep both.
		return runBob(args[1:], stdout, stderr)
	case "bobshell":
		return runBobShell(args[1:], stdout, stderr)
	case "codex":
		// Codex has no settings file and no background service, but loads a
		// dotenv file of its own at startup: see cmd_codex.go.
		return runCodex(args[1:], stdout, stderr)
	case "opencode":
		// OpenCode's background service keeps an environment of its own, which is what
		// made it configurable at all: see cmd_opencode.go.
		return runOpenCode(args[1:], stdout, stderr)
	default:
		// The named list is the answer to a typo; the usage block after it is the
		// answer to "what else can this do", which is what someone who guessed an
		// agent name wrong most likely wanted. Same pairing as the no-argument case.
		fmt.Fprintf(stderr, "agentop: unknown agent %q (claude-code, bob, bobshell, codex, opencode)\n", agent)
		fmt.Fprint(stderr, configureUsage)
		return 2
	}
}
