package main

import (
	"fmt"
	"io"
)

const configureUsage = `agentop configure — point a coding agent at Cortex

Usage:
  agentop configure status
  agentop configure claude-code enable  [--yes] [--settings PATH] [--config PATH]
  agentop configure claude-code disable [--yes] [--settings PATH]
  agentop configure claude-code status  [--settings PATH]
  agentop configure bob enable  [--yes] [--settings PATH] [--config PATH]
  agentop configure bob disable [--yes] [--settings PATH] [--config PATH]
  agentop configure bob status  [--settings PATH] [--config PATH]
  agentop configure opencode enable | disable [--yes] [--config PATH] [--opencode BIN]
  agentop configure opencode status [--config PATH] [--opencode BIN]
  agentop configure codex enable  [--yes] [--env PATH] [--config PATH]
  agentop configure codex disable [--yes] [--env PATH]
  agentop configure codex status  [--env PATH] [--config PATH]
  agentop configure pi enable | disable [--yes]
  agentop configure pi status

"agentop configure status" — with no agent name — answers the question the five
per-agent "status" verbs answer one at a time: which harnesses on this machine are
wired into Cortex. One row per agent, one verdict each, with the detail that verdict
rests on. There are four verdicts rather than a yes and a no, because a boolean would
be a lie for several of these: a settings file can hold some of the keys and not
others ("partly enabled"), and a shell's bob function cannot be seen from outside the
shell at all. "uncertain" is the word for everything agentop could not establish —
a file it cannot read or parse, a key defined twice, or no way to reach the agent's
configuration in the first place, which is what bob with no settings document and
opencode with no binary report. An agent whose file is simply absent is "not enabled",
since there is a place to look and nothing in it. For the paragraph behind one row,
run that agent's own "status".

Agents:
  claude-code    writes the proxy and CA variables into ~/.claude/settings.json, so
                 every session on the machine goes through Cortex. Run
                 "agentop configure claude-code --help" for the detail.
  bob            configures both faces of one agent: writes "http.proxy" into
                 IBM Bob's settings.json so the editor goes through Cortex, and
                 defines a "bob" shell function in your shell's rc file so typing
                 "bob" runs it through Cortex. Run "agentop configure bob --help"
                 for the detail.
  opencode       sets the proxy and CA variables in OpenCode's background-service
                 environment, which sends all of OpenCode's traffic. Run
                 "agentop configure opencode --help" for the detail.
  codex          writes the proxy and CA variables into Codex's own ~/.codex/.env,
                 which Codex loads itself at startup. Run
                 "agentop configure codex --help" for the detail.
  pi             defines a "pi" shell function in your shell's rc file that runs Pi
                 through Cortex, and installs a Pi extension that sends its session
                 id. Run "agentop configure pi --help" for the detail.

One verb for every agent, because "how do I point X at Cortex" is the same question
whatever X is, and the answer used to be spelled differently per agent: a top-level
"agentop claude-code" for the one agent with a settings file, and nothing at all for
the ones without.

Five agents persist, by four different mechanisms. Claude Code reads a settings
file, so its configuration goes there — it keeps an "env" block. Bob is two faces of
one agent and persists twice: the editor is a VS Code fork and reads "http.proxy"
from its settings.json, and on macOS enable also arranges the bridge-CA trust itself
(a settings key plus a launch-environment block in the rc file; off macOS it prints
the OS trust step rather than performing it), while the shell function is written
into the rc file, so the routing is applied when you type the command.
OpenCode's background service keeps an environment of its own, so its configuration
goes there, through the opencode CLI. Codex has no
settings file and no background service, but loads a dotenv file, ~/.codex/.env, on
its own at startup — confirmed against the real Codex CLI — so its configuration goes
there, scanning the whole file for an existing definition of each key rather than only
a block agentop wrote itself. Pi gets a shell function like Bob's, plus a
fixed extension file in its agent directory.

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
	// Handled with the agent names and not as a flag, because that is how someone
	// reaches for it: the five agents all take a `status` action, so `configure status`
	// with the agent left out is the natural spelling of "all of them" (#1164). It
	// occupies a name no agent can have — nothing is configured by being called
	// "status" — so this arm costs the agent namespace nothing.
	case "status":
		return runConfigureStatus(args[1:], stdout, stderr)
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
		// One agent, two faces, one command: runBob configures the IBM Bob editor
		// (a VS Code fork with a settings.json that reads "http.proxy") and the
		// "bob" shell function (an rc-file block) both.
		//
		// This arm reverses part of #1133, which removed "configure bob" reasoning
		// that "IBM Bob itself needs no configuring". That was right about the
		// binary and wrong about the editor: Bob has a settings file, and without
		// this a Bob user had to find "http.proxy" and the CA trust step by hand.
		// Until 2026-10-15 the shell function was a second subcommand, "bobshell";
		// it folded into this one when the agent converged on a single name, the
		// way its traffic had already converged in agentop's rows.
		return runBob(args[1:], stdout, stderr)
	case "codex":
		// Codex has no settings file and no background service, but loads a
		// dotenv file of its own at startup: see cmd_codex.go.
		return runCodex(args[1:], stdout, stderr)
	case "pi":
		// Pi reads its CA only from the environment it starts with, so it gets a
		// shell function like bob's, plus an extension: see cmd_pi.go.
		return runPi(args[1:], stdout, stderr)
	case "opencode":
		// OpenCode's background service keeps an environment of its own, which is what
		// made it configurable at all: see cmd_opencode.go.
		return runOpenCode(args[1:], stdout, stderr)
	default:
		// The named list is the answer to a typo; the usage block after it is the
		// answer to "what else can this do", which is what someone who guessed an
		// agent name wrong most likely wanted. Same pairing as the no-argument case.
		fmt.Fprintf(stderr, "agentop: unknown agent %q (claude-code, bob, codex, opencode, pi)\n", agent)
		fmt.Fprint(stderr, configureUsage)
		return 2
	}
}
