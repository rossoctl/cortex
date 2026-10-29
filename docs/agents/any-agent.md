# Any other agent

For agents with no settings file of their own — `curl`, `python`, `node`, test suites —
`agentop exec` scopes Cortex routing to a single child process. See also the
[supported agents table](../../README.md#supported-agents).

## Running one command through Cortex (`agentop exec`)

`agentop configure claude-code enable` works because Claude Code has a settings
file: the variables can be written once and reach every session on the machine,
background agents included. Nothing else has that. `curl`, `python`,
`node`, `gh` and your test suite read the process environment and nothing
else, and the usual workaround — exporting `HTTPS_PROXY` in your shell —
leaks into every unrelated command in that terminal until you remember to
unset it.

`agentop exec` scopes the routing to a single child process:

```sh
agentop exec -- curl -sv https://api.anthropic.com/v1/messages
agentop exec -- claude --dangerously-skip-permissions
agentop exec -- bob
```

Everything after `--` is passed through exactly as typed. agentop never
parses it, so the command's own flags need no escaping — even ones agentop
also has, like `--print`. The child inherits your whole environment plus
these nine variables:

| Variable | Value |
|---|---|
| `HTTP_PROXY` `HTTPS_PROXY` `http_proxy` `https_proxy` | the forward proxy URL |
| `NODE_EXTRA_CA_CERTS` | `ca.crt`, *added* to the runtime's own roots |
| `CURL_CA_BUNDLE` `REQUESTS_CA_BUNDLE` `SSL_CERT_FILE` `GIT_SSL_CAINFO` | `bundle.crt` — the bridge CA plus the platform roots |

Four proxy spellings because there is no agreed one: Go and most Unix
tools read the lowercase pair, Node the uppercase, libcurl either. A tool
that reads only the spelling we left out would silently bypass the proxy —
invisible, because it keeps working.

The CA names split two ways, and the difference matters. `NODE_EXTRA_CA_CERTS`
*extends* Node's trust store, so it takes `ca.crt` directly. The other four
*replace* it: whatever file they name becomes the complete set of roots, so
pointing them at `ca.crt` would leave the child trusting the bridge and nothing
else — breaking every host the bridge does not terminate. They get `bundle.crt`
instead, which Cortex writes beside `ca.crt` on startup (bridge CA + platform
roots). These are the same values `agentop configure claude-code enable` writes
into `settings.json`; `exec` reuses that derivation rather than repeating it, so
the two commands cannot disagree.

Both proxy variables get the **`http://`** URL, deliberately. The scheme
in a `*_PROXY` variable says how to reach the *proxy*, not what the
proxied request is; Cortex's forward proxy speaks plain HTTP and
CONNECT-tunnels TLS, so `https://` there would make clients attempt TLS
to the proxy itself and fail the handshake.

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

agentop exits with the child's status (127 if the command was not found,
128+signum if it was killed), so it is safe in a pipeline or a Makefile.
Keyboard signals (Ctrl-C) reach the child directly through the shared
process group; a signal aimed at agentop itself — `timeout 30 agentop exec
-- …`, a CI runner, systemd — is relayed to the child, so it is not left
orphaned with the injected environment.

To see the variables without running anything:

```sh
agentop exec --print                 # nine shell-quoted export lines
eval "$(agentop exec --print)"       # or apply them to the current shell
```

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

`agentop configure claude-code enable` shares this requirement as of the same
change: it too refuses `tls_bridge.mode: disabled` with a `ca_dir` set, a
combination it used to
accept and write into `settings.json`, where the CA bought nothing because the
bridge terminated no TLS. `enable` also now points its four replacing variables at
`bundle.crt` rather than the bare `ca.crt`.

Requires an enabled TLS bridge — both `tls_bridge.mode: enabled` and
`tls_bridge.ca_dir`. `mode: disabled` with a `ca_dir` set is a valid config,
but the bridge then terminates nothing, so a CA would buy the child nothing
while breaking its https; `agentop exec` refuses rather than inject either half
of a setup that cannot work.

Before Cortex's first start, `ca.crt` does not exist yet. `exec` still runs the
command and says so, but leaves the four replacing variables unset — the child
keeps its own public roots and only bridged hosts fail, rather than losing all
trust to a bundle with no bridge CA in it.
