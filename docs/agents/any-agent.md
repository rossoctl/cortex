# Any other agent

Some agents and tools have no settings file of their own, for example `curl`,
`python`, `node` and test suites. For these, `agentop exec` sends one child process
through Cortex. agentop then shows the requests of that process. See also the
[supported agents table](../../README.md#supported-agents).

## Use it

1. Run the command through `agentop exec`. Everything after `--` passes through as you type it:
   ```sh
   agentop exec -- curl -sv https://api.anthropic.com/v1/messages
   agentop exec -- claude --dangerously-skip-permissions
   agentop exec -- bob
   ```
2. You do not restart anything. The environment applies only to that child process.
   Cortex must run, and its TLS bridge must be enabled.
3. Check: run `agentop observe`, run the command, and see its row.
4. Undo: there is nothing to undo. `agentop exec` changes no file and exports nothing
   to your shell.
5. To see the variables without running a command, use `--print`:
   ```sh
   agentop exec --print                 # shell-quoted export lines
   eval "$(agentop exec --print)"       # or apply them to the current shell
   ```

## Known issues

- **agentop prints `note: … does not exist yet — Cortex writes it on first start`.**
  Cortex has not started yet, so `ca.crt` does not exist. `exec` still runs the
  command and says so, but leaves the four replacing variables unset — the child
  keeps its own public roots and only bridged hosts fail, rather than losing all
  trust to a bundle with no bridge CA in it. Fix: start Cortex once
  (`agentop service start`), then run the command again.
- **agentop refuses with `has no enabled TLS bridge (tls_bridge.mode must be
  "enabled")` or `has no tls_bridge.ca_dir`.** The TLS bridge is not enabled. Fix:
  set both `tls_bridge.mode: enabled` and `tls_bridge.ca_dir`, then restart Cortex.
  See [Requirements](#requirements) for the reason.

For problems that all agents share, see [Troubleshooting](../troubleshooting.md).

## How it works (for developers)

### Running one command through Cortex (`agentop exec`)

`agentop configure claude-code enable` works because Claude Code has a settings
file: the variables can be written once and reach every session on the machine,
background agents included. Nothing else has that. `curl`, `python`,
`node`, `gh` and your test suite read the process environment and nothing
else, and the usual workaround — exporting `HTTPS_PROXY` in your shell —
leaks into every unrelated command in that terminal until you remember to
unset it.

`agentop exec` scopes the routing to a single child process. The examples are in
[Use it](#use-it).

Everything after `--` is passed through exactly as typed. agentop never
parses it, so the command's own flags need no escaping — even ones agentop
also has, like `--print`.

### The variables

The child inherits your whole environment plus these variables:

| Variable | Value |
|---|---|
| `HTTP_PROXY` `HTTPS_PROXY` `http_proxy` `https_proxy` | the forward proxy URL |
| `NODE_EXTRA_CA_CERTS` | `ca.crt`, *added* to the runtime's own roots |
| `CURL_CA_BUNDLE` `REQUESTS_CA_BUNDLE` `SSL_CERT_FILE` `GIT_SSL_CAINFO` | `bundle.crt` — the bridge CA plus the platform roots |

Four proxy spellings because there is no agreed one: Go and most Unix
tools read the lowercase pair, Node the uppercase, libcurl either. A tool
that reads only the spelling we left out would silently bypass the proxy —
invisible, because it keeps working.

### CA variables: extend or replace

The CA names split two ways, and the difference matters. `NODE_EXTRA_CA_CERTS`
*extends* Node's trust store, so it takes `ca.crt` directly. The other four
*replace* it: whatever file they name becomes the complete set of roots, so
pointing them at `ca.crt` would leave the child trusting the bridge and nothing
else — breaking every host the bridge does not terminate. They get `bundle.crt`
instead, which Cortex writes beside `ca.crt` on startup (bridge CA + platform
roots). These are the same values `agentop configure claude-code enable` writes
into `settings.json`; `exec` reuses that derivation rather than repeating it, so
the two commands cannot disagree.

### The proxy URL is http://

Both proxy variables get the **`http://`** URL, deliberately. The scheme
in a `*_PROXY` variable says how to reach the *proxy*, not what the
proxied request is; Cortex's forward proxy speaks plain HTTP and
CONNECT-tunnels TLS, so `https://` there would make clients attempt TLS
to the proxy itself and fail the handshake.

### Where the values come from

The values come from the **running proxy**, fetched from its stats endpoint
(`http://localhost:47602/config` by default, `--cortex-stats-url` to point
elsewhere). There is no `--config` flag: `exec` works against a proxy that has to
be up for the child to reach anything, and that process already has a config —
reading a file instead would let the two disagree, since listener addresses are
not hot-reloaded and a file says nothing about whether anything is listening. A
Cortex that is down is reported as down rather than yielding an environment that
points at nothing. The derivation from config to variables is still the one
`configure claude-code enable` uses, so the two produce identical values for the
same Cortex. Nothing is exported to your shell and no file is modified.

### Exit status and signals

agentop exits with the child's status (127 if the command was not found,
128+signum if it was killed), so it is safe in a pipeline or a Makefile.
Keyboard signals (Ctrl-C) reach the child directly through the shared
process group; a signal aimed at agentop itself — `timeout 30 agentop exec
-- …`, a CI runner, systemd — is relayed to the child, so it is not left
orphaned with the injected environment.

### --print

`agentop exec --print` shows the variables without running anything. The commands
are in [Use it](#use-it).

`--print` emits paths only — it writes nothing. `bundle.crt` and `ca.crt` are
created by Cortex itself on first start, so the exported paths keep resolving
long after agentop exits, which is what makes the `eval` form usable.

`--print` takes no command, and no `--`: it is a complete request on its own.
Both `agentop exec --print -- curl …` and a bare `agentop exec --print --` are usage
errors — the first asks for two different things at once, the second promises a
command and supplies none. The paths `--print` hands out are meant to be kept,
and are the same ones `agentop configure claude-code enable` writes into
`settings.json`; running a command is the opposite, applying them to one process
for its lifetime. Asking for both in one invocation is a contradiction about
which you want, so agentop says so rather than picking one.

### Requirements

Requires an enabled TLS bridge — both `tls_bridge.mode: enabled` and
`tls_bridge.ca_dir`. `mode: disabled` with a `ca_dir` set is a valid config,
but the bridge then terminates nothing, so a CA would buy the child nothing
while breaking its https; `agentop exec` refuses rather than inject either half
of a setup that cannot work.

`agentop configure claude-code enable` has the same requirement. It also refuses
`tls_bridge.mode: disabled` with a `ca_dir` set, because the bridge then terminates
no TLS and the CA does nothing. It points its four replacing variables at
`bundle.crt`, not at the bare `ca.crt`.
