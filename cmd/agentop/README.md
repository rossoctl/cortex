# agentop

Interactive terminal UI for inspecting AuthBridge's in-memory session store.
`agentop` connects to the session API exposed by an AuthBridge sidecar
(default `http://localhost:9094`, typically reached via `kubectl port-forward`)
and lets you browse active sessions, follow a session's event stream live,
and read individual events as pretty-printed JSON.

## Install

Download a prebuilt `agentop` for your platform (linux/macOS, amd64/arm64) from the
[Releases page](https://github.com/rossoctl/cortex/releases) — see
[Download prebuilt binaries](../../docs/architecture.md#download-prebuilt-binaries) for the download,
checksum-verify, and macOS quarantine steps — and drop it on your PATH.

Or build from source:

```sh
cd cmd/agentop
go build .
```

Either way you get a single binary (~10 MB; the linux build is fully static).

## Run

`agentop observe` discovers AuthBridge agents in your current `kubectl`
context and lets you pick one:

```sh
./agentop observe
```

Bare `./agentop` does the same thing, but is **deprecated** and will stop
opening the viewer in a future release. It printed no hint that the
other subcommands existed, so anyone who never ran `--help` reasonably
concluded the TUI was all agentop did — `agentop service` least visible of
all, and that is what you need when Cortex is not running. Run
`agentop --help` for the full list.

You'll see a Namespaces pane listing each namespace that contains an
AuthBridge agent. Enter drills into the Pods pane for that namespace;
Enter on a pod starts a `kubectl port-forward` automatically and drops
you into the session-events view. Esc backs out. `q` (or Ctrl+C) quits
and tears the port-forward down.

The picker shells out to `kubectl` — whatever context you're in is the
context agentop uses. There's no separate auth.

### Connecting to an existing port-forward

Press `l` on the Namespaces pane to skip the cluster entirely and connect to a
session API on this host. Where that is depends on whether you have a Cortex
installed: it goes to the address in `~/.cortex/config.yaml` when one answered
there (47601 by default), and otherwise to `http://localhost:9094`, the
in-cluster default port. The second case is the useful one when you already have
your own `kubectl port-forward` running, when agentop runs inside the mesh, or
when your kubeconfig can't list pods but a tunnel is up.

agentop probes `/v1/sessions` before switching panes, so an endpoint with
nothing listening surfaces as a footer error and leaves you in the
picker rather than dropping you into a silently empty session view.
`Esc` from a session entered this way returns to the Namespaces pane
(there's no pod to go back to).

Pipeline editing (`e`) follows the same split. Connected to this machine's
Cortex, `e` edits its config file — see [Editing the pipeline](#editing-the-pipeline).
Connected to `:9094` through somebody else's port-forward, there is neither a pod
identity to resolve a ConfigMap from nor a local config file to write, so `e`
flashes a hint instead.

### Power-user / scripting bypass

Pass `--endpoint` to skip the picker entirely:

```sh
kubectl port-forward -n team1 pod/weather-agent-xxxx 9094:9094 &
./agentop observe --endpoint http://localhost:9094
```

This preserves the pre-picker behavior for scripts, CI, or remote
session APIs that aren't in your kube context.

### Choosing between a cluster and a local Cortex (`--kubernetes`)

With no `--endpoint`, agentop decides between the cluster picker and the Cortex
running on this machine (read from `~/.cortex/config.yaml`, and probed first —
a stale config from an install that is no longer running is ignored).

`--kubernetes` controls that choice and **defaults to false**, so a local Cortex
that is answering wins and the picker appears only when none is:

```sh
./agentop observe                # the local Cortex, when one is running
./agentop observe --kubernetes   # the picker, even with a local Cortex running
```

The default favours the local one because that is the quickstart, and it should
need no flag: install Cortex on your laptop, run `agentop observe`, watch traffic.
`--kubernetes` is for the machine that has both — a local install AND cluster
work — where the probe would otherwise win every time and `--endpoint` could
only substitute for the picker by naming a namespace, a pod and a port-forward
by hand.

A machine with no local install needs no flag either: with nothing answering,
`agentop observe` opens the picker on its own.

The flag is ignored when `--endpoint` is given — an explicit address always
wins. Resolution in full:

| `--endpoint` | Local Cortex answering | `--kubernetes` | Result |
|---|---|---|---|
| given | — | — | that endpoint |
| — | yes | absent (default) | the local Cortex |
| — | yes | passed | Namespaces picker |
| — | no | either | Namespaces picker |

### Naming sessions from Claude Code (`--skip-claude-metadata`)

Cortex buckets traffic by session id, and a session id is a UUID. Claude Code
knows more about the same session: it writes a transcript per session carrying a
model-generated title and the directory the session ran in. `agentop observe`
reads those transcripts and writes what it finds to
`~/.cortex/session-metadata.json`, so the sessions table can show a `TITLE`
column instead of a bare id.

This is **one of two** routes to a name, and the one this section is about. The
proxy also derives a title from a session's own events and serves it on
`/v1/sessions`; `TITLE` prefers the harvested name and falls back to that one,
so the column can be populated for a session with no transcript here at all.
Everything below concerns the harvest only — including `--skip-claude-metadata`,
which suppresses this route and not the served fallback.

The scan runs in the **background**, while the viewer is already up: `agentop observe` paints
immediately and the titles appear when the scan finishes — usually before you have
picked a pod. The viewer opens with whatever titles the last run recorded, so a scan
only ever adds names.

This happens by default; `--skip-claude-metadata` turns it off:

```sh
./agentop observe                            # open the viewer; titles arrive as they scan
./agentop observe --skip-claude-metadata     # skip the scan; previously-recorded titles still show
```

The scan is also **incremental**: a transcript whose mtime has not moved since it was
last read is skipped, because a file that has not changed cannot have grown a new
title. On a tree of 124 transcripts totalling 207 MB, a launch that follows a recent
one re-reads 2 of them. A rewrite that *preserves* mtime — `rsync -t`, a restore from
backup — therefore keeps whatever title the entry already had; re-run the explicit
command below to force a re-read.

Because the scan outlives nothing, quitting the viewer before it finishes simply means
it did not save, and the next launch scans again. The window is the ~0.7s of a first
full scan, and milliseconds once the file exists.

Pass `--skip-claude-metadata` when the scan is unwanted, or when `~/.claude` should
simply not be touched. It suppresses only the *scan*: the viewer still reads
`~/.cortex/session-metadata.json`, so titles recorded by earlier runs keep rendering and
only sessions new or renamed since the last scan go unharvested — and those still show the
title the proxy serves, if it derived one, rather than a bare id. There is no flag that
hides titles already on disk — delete the file for that, and note that it does not suppress
the served title either, which arrives over the API and not from any file.

A harvest that cannot run is never fatal, and it costs less than it used to: the viewer
still opens, and a session the proxy has named still shows that name, because the served
title arrives over the API and not from this file. What a missing or unreadable file
costs is therefore the `TITLE` column only for sessions the proxy has not named — which,
on a machine whose agents all route through the proxy, may be none of them. A file that
does not parse is rebuilt from the transcripts rather than costing anything; the entries
a rebuild cannot recover are sessions whose transcripts Claude Code has already pruned.
The failures that need a human, such as an unreadable metadata file, print one line to
stderr with the repair before the viewer starts; success says nothing.

The config directory is `CLAUDE_CONFIG_DIR` when set, and `~/.claude`
otherwise. To read a different directory, or to force a full re-read of every
transcript — the way to repair entries that are wrong — run the harvest
explicitly:

```sh
./agentop experimental read-claude-sessions              # full scan, reports counts
./agentop experimental read-claude-sessions --dir PATH   # a different config dir
./agentop experimental read-claude-sessions --merge=false  # rebuild, dropping stale entries
```

Both the background scan and the default subcommand run **upsert**, so an entry stays once
written: a session whose transcript Claude Code has pruned keeps its title indefinitely,
and the file grows with sessions-ever-seen rather than sessions-that-exist. That is what
`--merge=false` is for — but it drops every entry the run did not see, including entries
harvested from a different `--dir`, so pass it with the same `--dir` that built the file.

Only the explicit subcommand reports a transcript it could not read to the end. Such a
session still gets whatever title was found before the stop, which may be an older one, and
the background scan has nowhere to say so once the viewer owns the screen — so re-run the
subcommand if a title looks wrong.

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

## Typing `bob` instead of `agentop exec -- bob` (`agentop configure bobshell`)

`agentop exec -- bob` routes one invocation. `agentop configure bobshell enable`
makes that the meaning of `bob` in every new shell, by appending a delimited
block to your rc file:

```sh
# >>> cortex agentop (bobshell) >>>
bob() {
  agentop exec -- bob "$@"
}
export CORTEX_BOBSHELL=1
# <<< cortex agentop (bobshell) <<<
```

```sh
agentop configure bobshell enable    # append the block
agentop configure bobshell disable   # remove exactly that block
agentop configure bobshell status    # is bob routed in THIS shell?
```

Both `enable` and `disable` ask before writing, and `--yes` skips the question:

```sh
agentop configure bobshell enable --yes    # no prompt
```

With no terminal to ask on — CI, a container, a Dockerfile `RUN` — they decline
rather than prompt, print `Re-run with --yes to apply`, write nothing and exit
**3**. Not 0: a decline is not a failure, but it is also not an applied change,
and 3 is what lets an unattended caller tell the two apart — the same code
`configure claude-code` and `service` return when they are declined. An
unattended caller that omits `--yes` is still a no-op; it is just no longer a
silent one. Scripted callers should pass `--yes`.

`bob` and `bobshell` are two different agents, not two spellings of one. `bob`
configures the IBM Bob **editor** — a VS Code fork, so the lever is `http.proxy`
in its `settings.json` (below). `bobshell` configures the **shell integration** —
a `bob` function in your rc file, so typing `bob` at a prompt runs through
Cortex. Configuring one does not configure the other, and neither name is an
alias of the other: `agentop configure bob --settings X` is a usage error under
`bobshell`, and vice versa.

The editor agent briefly did not exist. It was removed on the reasoning that
what needed configuring was the shell integration and "IBM Bob itself needs no
configuring", which was right about the binary and wrong about the editor. It is
back, and the two now stand side by side.

Let `enable` write it rather than pasting the block above. What `enable` appends
begins with a blank line, which the fence cannot show you: it is invisible when
rendered, and formatters strip a leading blank line inside a fence anyway. That
newline is part of the block rather than something added to your content, so the
block starts on a line of its own even after a file whose last line has no
newline of its own, and `disable` takes the separator away with the rest of it.
Paste the fence's text verbatim at the top of a file and `disable` will report a
block "edited since it was written" and decline — it matches the whole constant,
leading newline included. It declines rather than guessing, so nothing is lost,
but the block is then yours to remove by hand.

Letting `enable` write it is also why nothing else in the file is touched and the
file comes back byte-for-byte — a round-trip test asserts exactly that,
including on a file with no trailing newline, so the two halves cannot drift
apart.
Run `enable` twice and the second run is a no-op. If the block is there but
hand-edited, or there twice over, both verbs decline and say so rather than
guess which copy you meant.

A shell function, not an alias: bash does not expand aliases in
non-interactive shells unless `expand_aliases` is set, and `"$@"` forwards
arguments explicitly, so `bob "two words"` stays one argument.

It cannot recurse into itself, which is why there is no machinery to resolve
the real binary past the function. `agentop exec` runs the `bob` binary as a child
process, and that process never reads your rc file, so the `bob` inside the body
is always the one on `PATH`.

### Which file it writes

The basename of `$SHELL` picks it, and only these two:

| `$SHELL` | File |
|---|---|
| …`/zsh` | `~/.zshrc` |
| …`/bash` | `~/.bashrc` |
| anything else, or unset | nothing is written — `enable` prints the block for you to place, `disable` tells you which block to delete |

That is the whole rule. `agentop` does not work out whether your shell will be a
login or a non-login shell, or which of zsh's four startup files you meant,
because being wrong about it writes a block into a file nothing reads and
leaves you with no reason to look there.

**bash on macOS is the case to know about.** Terminal.app starts bash as a
*login* shell, which reads `~/.bash_profile` and not `~/.bashrc`. If the
function does not appear in a new window, either source `~/.bashrc` from
`~/.bash_profile` — the usual arrangement — or move the block there yourself.
`enable` always prints the path it wrote, so you can see where it went.

### Symlinks

A `~/.zshrc` symlinked into a dotfiles repo is followed, and the block lands in
the real file: writing the link itself would replace it with a regular file and
silently detach it from the repo, leaving the tracked copy stale with nothing
in `git status` to show it. Two or more links deep, or a dangling link, and
both verbs decline to write — a chain that long is somebody's deliberate
arrangement, and a write through it is more likely to surprise than to help.

What they print instead follows the verb, as it does for an unrecognised
`$SHELL`: `enable` gives you the block to paste, and `disable` names the markers
to delete between, because you already have the block — it is in your file — and
printing it at someone removing the integration reads as an instruction to put
it back.

The write is temp-file-then-rename, so a failure part way through leaves your
rc file as it was rather than half-written. An existing file keeps its own
permissions; a new one is created `0644`.

### What `status` does and does not know

It reports whether `CORTEX_BOBSHELL` is set in the environment, and it reads no
files. So it says `not enabled` in the very shell that just ran `enable`, until
you open a new terminal or source the file — recognising the block inside a
startup script would mean parsing shell, which is the thing this command
deliberately does not do.

What the variable proves is narrower than "`bob` is routed", which is why the
report separates the two. The variable is exported, so every child process inherits
it — including a **non-interactive** subshell or a script, which does not read
your startup file and therefore has no `bob` function at all. There, `bob` is the
plain binary and Cortex is not in the path of the call, while the variable still
says `1`. `status` cannot tell the two apart: the shell's function table lives in
that shell's memory and is never exported, so `agentop`, as a child process, cannot
see it. To settle it in a particular shell, ask that shell: `type bob` says
`bob is a function` when the function is live, and names a file when it is not.

Use `type`, not `which`. In bash, `which` is `/usr/bin/which` — a separate
process, which cannot see its parent shell's functions, so with the function live
it reports the `bob` binary's path and looks like a definitive "not routed". zsh's
`which` is a builtin and does report the function, so the wrong advice works in
one of the two shells this writes a file for. `type` is a shell builtin in sh,
bash, zsh and dash alike. (`mise doctor` splits `activated:` from
`shims_on_path:` for the same reason.)

Both answers exit 0: "not enabled" is a report, not a failure.

## Routing the IBM Bob editor through Cortex (`agentop configure bob`)

IBM Bob is a VS Code fork, so it reads the VS Code proxy setting. `agentop
configure bob enable` writes exactly one flat, top-level key into Bob's user
settings:

```json
{
  "http.proxy": "http://127.0.0.1:47600"
}
```

Flat and dotted, not nested under an `"http"` object — that is the shape VS Code
reads, and the shape difference from `configure claude-code`, which writes a
nested `"env"` block. The address is read from `listener.forward_proxy_addr` in
`~/.cortex/config.yaml` on every run, so a moved port or an IPv6 loopback
produces the right value rather than a hardcoded 47600.

Nothing else in the file changes, and that is meant literally: the key is spliced
into the existing bytes rather than re-serialized from a parsed document, so your
key order, your indent width, your inline arrays and your blank lines between
groups all survive untouched. A settings.json is hand-curated and often lives in
a dotfiles repo, where a diff that alphabetizes and reflows the whole file is
worse than the setting is worth. `disable` takes the line back out the same way,
so enable-then-disable returns the file byte-for-byte.

```sh
agentop configure bob enable      # write the key, print the CA trust command
agentop configure bob disable     # remove the key, print the optional undo
agentop configure bob status      # report, and act on nothing
```

`enable` and `disable` show the one-line change and ask before writing, keep a
`.bak` of the file as it was first found, and take `--yes` for unattended use.
Declined — or with no terminal to ask on — they write nothing and exit **3**, the
same convention as `configure claude-code` and `configure bobshell`. `--settings
PATH` and `--config PATH` override either file. Restart Bob afterwards: whether
it re-reads a proxy change live is unverified, so the message says restart rather
than guess.

### Certificate trust is printed, never performed

The proxy terminates TLS with a forged leaf, so Bob has to trust Cortex's bridge
CA or every HTTPS request fails. Installing a root CA is a machine-wide change
needing `sudo`, and a tool that silently escalates to do it is not what anyone
wants — so `enable` prints the exact command and stops:

```sh
sudo security add-trusted-cert -d -r trustRoot \
  -k /Library/Keychains/System.keychain ~/.cortex/ca/ca.crt
```

`disable` prints the undo, which is **two** commands rather than one, and says
it is safe to leave the certificate in place:

```sh
sudo security remove-trusted-cert -d ~/.cortex/ca/ca.crt
sudo security delete-certificate -c authbridge-tls-bridge-ca \
  -t /Library/Keychains/System.keychain
```

`delete-certificate` alone does not undo the `add-trusted-cert` above it. The add
writes trust settings to the **admin** domain (that is what its `-d` selects);
`delete-certificate -t` removes the certificate and, per its own usage text,
*user* trust settings — a different domain, so the admin-domain trust survives it.
`remove-trusted-cert -d` is the documented inverse of the add, and its `-d` has to
be repeated for the same reason. The keychain is named on the delete because an
add to the System keychain is not undone by a delete that defaults to the login
one.

On macOS, `status` suggests `security verify-cert` without running it. Off
macOS it suggests nothing: there is no portable check to name, and `enable` and
`disable` already carry the trust-store guidance for those platforms — naming
the usual Debian and Fedora routes and saying plainly that the exact step
depends on the distribution.

It is `ca.crt` — the single bridge CA — and deliberately **not** the
`bundle.crt` in the same directory, which holds the bridge CA *plus* every
platform root and exists for tools whose CA setting *replaces* the trust store
(`SSL_CERT_FILE` and friends, as `agentop exec` sets). The keychain is additive,
so `-r trustRoot` on the bundle would install explicit machine-wide root trust
for every public CA in it, and the undo above would not take that back.

### What it knows, and what it does not

"Enabled" means the value's **host and port both match** `listener.forward_proxy_addr`
from `~/.cortex/config.yaml` — the address this machine's Cortex actually listens
on, not a port range. Compared whole via `url.Parse`, so neither a path
(`http://corp.example.com/?next=127.0.0.1:47600`) nor a suffixed host
(`localhost:47600.evil.com`) can pass as ours; only `http` counts, since it is the
only scheme `enable` writes. The loopback spellings are folded together
(`localhost` / `127.0.0.1` / `::1`) because `forward_proxy_addr` may bind `0.0.0.0`
while the settings file names `127.0.0.1`, and a hand-typed address must not be
called someone else's proxy.

That is still not a record agentop keeps — there is no state file, so ownership is
re-decided from the value each time.

Ownership has three answers, not two. A value that matches is **ours**; a
corporate proxy, or a non-string value, is **not ours** and is left alone by both
verbs. The third is **cannot tell**: when there is no address to compare against
— the config is missing or unreadable — any loopback `http` proxy could be this
one, and a bool would have to guess. Guessing "not ours" toward a `delete` is the
dangerous direction, so it is not a bool. `status` reports "cannot tell" in those
words rather than ruling on it. `disable` asks before removing such a value and
refuses under `--yes`, since `--yes` means "do not ask me", not "decide for me".
`enable` refuses rather than overwriting anything it does not own. Either verb
prints exactly what it will do to the file, and what it will leave beside it,
before it does it.

**Whether anything is listening is a separate question**, reported on its own
line. A stopped Cortex is the normal state of a laptop and is not a verdict on the
setting: the setting is right either way, and the answer to "nothing is listening"
is `agentop service start`, not an edit here.

`http.proxy` governs VS Code's core networking and its extension host. An
extension that bundles its own HTTP client can still go around it; this is the
documented lever, not a guarantee of coverage.

A settings file with comments in it is refused, not rewritten. VS Code permits
them; the strict JSON reader here does not, and silently stripping a user's
comments to add one key is the wrong trade.

`enable` and `disable` need a settings document to already exist: a path that is
missing, empty, or holds only `null` means IBM Bob has not saved settings there,
and both refuse rather than creating a file at a path nothing reads. The refusal
names the path, and `--settings` if it is the wrong one. `status` reports the
Cortex status as unknown there, saying which of the three it found.

Only macOS's settings location is known (`~/Library/Application Support/IBM
Bob/User/settings.json`). Elsewhere `--settings PATH` is required rather than
guessed — writing a proxy setting into a file nothing reads is a silent no-op,
which is worse than a refusal that names the flag.

## Routing OpenCode through Cortex (`agentop configure opencode`)

OpenCode does not send its traffic from the process you run. One background
service per user, `opencode serve --service`, sends every session's requests: the
first client starts it, later clients reuse it, and it outlives them with the
environment it started with. So `agentop exec -- opencode` changes nothing for a
service that is already running, and warns when it finds one running without
Cortex. The service also keeps an environment of its own, set with `opencode
service set env`, which it takes over the one it inherited. That is where
`enable` puts the routing:

```sh
agentop configure opencode enable    # set the nine variables in the service's environment
agentop configure opencode disable   # remove them again
agentop configure opencode status    # the nine, and the running service
```

`enable` and `disable` take `[--yes] [--config PATH] [--opencode BIN]`; `status` takes
`[--config PATH] [--opencode BIN]` and no `--yes`, since it changes nothing.

The nine are the ones `agentop exec` sets (the table above), derived from
`~/.cortex/config.yaml` as `configure claude-code enable` derives them. Every
change goes through the `opencode` CLI: `enable` makes one `set env` per variable
that differs, CA variables first so the proxy is never set without its CA, and
`disable` one `unset env` (or a `set env` putting back a recorded value) per
variable it changes. Nothing else in the service's environment is touched.
`enable` refuses to overwrite a value someone else set, such as a corporate
proxy, and names the `opencode service unset env` that removes it. So the only values it replaces are
Cortex's own, such as a proxy at an older Cortex address. The first run records
the nine in `~/.cortex/opencode-state.json`, counting Cortex's values as absent,
and `disable` removes them rather than putting an old Cortex address back.
`disable` changes only Cortex's values, whatever the record says: one set some
other way, even after `enable`, is left alone and named, with the `opencode
service unset env` that removes it. Declined, or with no terminal to ask on, both
write nothing and exit **3**. `--yes` skips the question, `--config PATH` reads
another Cortex config, and `--opencode BIN` names the CLI when it is neither on
`PATH` nor in `~/.opencode/bin`.

Changing the service's environment through OpenCode's CLI stops a running
service, and an open OpenCode can start it again straight away, before the change
lands, with the environment it had. So when the service was running, `enable` and
`disable` restart it once, after their last change, with `opencode service
restart`, which starts it with its service environment. That interrupts every
OpenCode session using it, and an open OpenCode reconnects to it. When there is
something to change they say first that the service is running and that the
change restarts it, with its pid, and ask; when whether it runs cannot be told,
they say only that the change stops it if it is. `--yes` skips the question, not
the warning. After the restart they say whether the service's proxy matches the
change, judged by the environment its new process started with; when it does
not, they name `opencode service restart`. A restart that fails is reported: the
service may be stopped, or running with its old environment, and `opencode
service restart` applies the change. The command still exits 0, because the
change itself succeeded. A change that fails part way is not followed by a
restart, and they say the service may be stopped. A service that was not running
starts with the new environment the next time you run OpenCode.

`status` changes nothing. It judges the running service by the proxy a restart
would give it, the one its service environment names, and prints one of: that it
is using Cortex; that it is not; that it is running with its old environment,
with the restart command, when a restart would put it on Cortex; or that it is
using Cortex although its service environment does not route it there, so its
next start will not use Cortex, which is how a service started under `agentop
exec` looks, with `enable` named to keep it on Cortex. Otherwise `status` says the
service is not running, or, without a readable Cortex config, gives its pid and
proxy. When the service's process or environment cannot be read, or `opencode
service status` fails, the line says it could not check rather than guessing.

[OpenCode's page](../../docs/agents/opencode.md) covers the rest: how to undo this by
hand, what OpenCode needs to trust Cortex's CA, what Cortex records for it, what was
verified, and its known issues.

## Panes

The UI has these panes. `Enter` drills in; `Esc` backs out.

**Sessions is the only pane you land on.** Pipeline (`P`), Usage (`u`) and the
plugin catalog (`C`) are each opened by a key from anywhere in the session views
and return to the pane you pressed it on. There is no tab strip: Sessions is what
agentop is for, and the other three are surfaces you visit and leave.

- **Sessions** (default): table of active sessions in the store, most
  recently updated first. Columns: session (truncated), title, updated
  (relative), event count, tokens, cost, saved, context. `TITLE` comes from
  Claude Code's transcripts — see
  [`--skip-claude-metadata`](#naming-sessions-from-claude-code---skip-claude-metadata)
  — and **falls back to the title the proxy serves** on `/v1/sessions`, which it
  derives from the session's own events. So a session with no transcript on this
  machine can still be named, and the cell is empty when neither source names it
  — or when the proxy has stopped listing the session, since a row kept alive by
  its cached events alone has no summary to carry a served title. A session named
  only by the proxy therefore loses its name at that point while its events
  remain, which is the one case where a title visibly disappears. The harvested
  title wins when both exist — a fixed precedence, not a claim that it is always
  the better string; the two sides rank candidates differently and may not agree
  on a given session. The column does not say which source it used. Numerics are
  right-aligned so the digits line up between rows.

  `CTX(1M)` is a gauge, not a figure: how full the **conversation's**
  context was on its latest turn, against a fixed one-million-token window. The
  brackets are the scale, drawn on every row, so a nearly-empty session reads
  as empty-out-of-something rather than as a blank cell — and an em dash, which
  means *no context known*, stays distinguishable from the sliver a barely-used
  session gets.

  **A session is three kinds of caller, not one conversation.** Claude Code
  sends your conversation, the subagents it spawns, and its own one-shot
  completions — title generation, the permission security monitor, the auto-mode
  classifier — under one session id. Two facts sort them out, and the gauge
  needs both:

  | | tool manifest | `agentRole` |
  |---|---|---|
  | your conversation | present | `main` |
  | a subagent | present | `subagent` |
  | a one-shot | **absent** | `main` |

  The manifest excludes the one-shots. Measured across 115 responses in three
  live sessions they carry *no tools* and 2–3 messages, while every conversation
  turn carried 27–31 tools and 63–1572 messages. They are not small, which is
  why size is not the filter: one measured 295k and another 421,220 against a
  true context of 998,334. And they declare themselves `main`, because the CLI
  issues them rather than a subagent.

  The role excludes the subagents, which Claude Code states on every request.
  The message count only approximated it — subagent turns reached 177 and 186
  messages against main threads at 188 and 288 — and it cannot see a compaction,
  which restarts your conversation at a low count while leaving the role alone.

  Among what is left, the **latest** turn wins. There is deliberately no recency
  window: your conversation goes silent while a subagent runs, and that silence
  is structural, so a last-N-requests window can fill with the subagent's
  traffic and hand the gauge to it. Both filters are exact, so none of that
  traffic is a candidate in the first place.

  Three things worth knowing. The denominator is fixed at 1M, the largest
  window on any path the proxy sees, so a model with a smaller window reads
  lower than it really is.

  The figure comes from agentop's own event cache, filled by the live stream or
  by drilling into a session. The timeline fetch asks for `view=summary`, and
  that projection drops the slices this rule used to read, so it records their
  **lengths** before dropping them (`messageCount` / `toolCount`) and the gauge
  reads either shape. Without that a delivered row could not be read at all, and
  an idle session showed a dash however long you looked at it. `agentRole` needs
  none of that: it is a scalar the projection copies as-is.

  Against a proxy older than those fields, an idle row still shows the dash
  until traffic arrives or you open one of its events — the one request that
  returns an event in full. Once a figure is established it is kept, so a
  projection that says nothing cannot erase it.

  **Against a proxy that publishes no `agentRole`**, the gauge falls back to the
  old rule — tool-carrying requests, the one with the most messages — and pays
  what that costs. After a compaction it can stay on the pre-compaction context
  for the rest of the session, because the older, longer request still holds the
  most messages: measured at 999,623 against a true 400,249, a bar at 98.6% for
  a session with 600k of headroom. A stale figure is still preferred to one that
  flips to a subagent's. With the role published there is nothing to wait for —
  the column follows the compaction on the next turn.

  Since the figure outlives the events it was read from, the way to clear one you
  do not believe is `Esc` back to the Pods pane and re-enter: a different pod is
  the one thing that discards it, and re-attaching starts the column from
  whatever streams next. That reset only exists in picker mode — under
  `--endpoint` there is no Pods pane to back out to, so restarting agentop is the
  equivalent.

  It replaced an `ACTIVE` column whose `●` nobody acted on — `UPDATED` already
  answers "is this live", in seconds rather than as a dot. The `cached` marker
  that column also carried, for sessions the server has forgotten but agentop
  still holds events for, moved into `UPDATED`.

  **Every figure in this table is a per-session total**, summed over that
  session's whole history rather than over a clock window — which is why its
  TOKENS column does not match the token counts on the `$` breakdown or the
  Usage pane, each of which covers the window it names. Both are right; neither
  is a check on the other.

  The table is also **not** a longer span than the band, which is the reading
  worth heading off. The session store is in memory, so a session's figures only
  reach back as far as the current proxy process — on a freshly restarted proxy
  the whole COST column can sum to less than `TODAY`, because `TODAY` comes from
  the durable cost ledger and survives restarts. `[?]` states both facts; the
  title deliberately does not, since no single span is true of every row.

  Rendered at 100 columns, where every column has its declared width; 93 is the
  narrowest terminal that carries them all at once:

  ```
  agentop · http://localhost:9094
  LAST 1H    TODAY   7 DAYS    MONTH
    $4.04   $18.80  $216.44  $703.18
  ────────────────────────────────────────────────────────────────────────────────────────────────────
   SESSION         TITLE        UPDATED           EVENTS      TOKENS        COST      SAVED~  CTX(1M)
   ctx-abc-123-4…  …pend-spans  3s ago                42       48.2k       $0.12       $0.01  ▕████ ▏
   ctx-def-567-4…  weather-ag…  18m ago               15        1.2k      <$0.01           —  ▕▏    ▏
   ctx-ghi-901-4…               42m ago                7        2.9k           —           —  ▕██   ▏
   default                      1h ago                 8           —           —           —        —

  ● connected  2.1 events/sec   feedback: https://github.com/rossoctl/cortex/issues/new/choose
  [↑↓] nav  [↵] drill  [u] usage  [$] spend  [/] filter  [p] pause  [P] pipeline  [?] keys  [q] quit
  ```

  The two money columns are dropped entirely on a terminal too narrow to show a
  sub-cent charge honestly — below 93 columns — rather than rounded to `$0.00`
  or blanked. A charge under a cent reads `<$0.01`. `TITLE` is fitted first and
  the money columns yield to it, so from 69 to 92 the table carries `TITLE` and no
  money, and at 93 the whole set holds every minimum at once. Any column added or
  resized moves these numbers; `sessionsShowMoney` states the arithmetic.

  `SAVED~` carries the tilde in its **heading** rather than on every row: a
  saving is always an estimate, so the caveat belongs to the column rather than
  to any one figure in it. Neither money column in this table marks a *value* —
  the only glyph a cell here can wear is `+`, meaning the session's total hit the
  int64 ceiling and the figure is a floor. The conditional `~`, earned by a figure
  whose requests could not all be settled exactly, appears on the band's `TODAY`
  and `LAST 1H` and on the `$` drawer's per-model cost.

  The screen above shows all of this; the markers are absent there because that fixture
  has nothing to disclose, which is itself the rule — a clean figure wears none.

  Labels sit above their values rather than beside them: `$30.94 today`
  reads as a list, `TODAY` over `$30.94` reads as a figure.

  **One cell per span a budget is read against**, ascending left to right, and
  every cell's label names the period its figure covers. That last part is the
  rule the band is built on: an earlier version carried `CACHE HIT` and `TOKENS`
  read off the rolling hour while sitting in a row that opened with `TODAY`, so
  an hour's token count read as a day's with nothing on screen to say otherwise.
  Volume readings live where there is room to scope them — the `$` breakdown, the
  sessions table, and the Usage pane.

  All four figures share one column width and are right-aligned, so their decimal
  points line up and the four periods can be compared by eye.

  As the terminal narrows, whole cells drop — never a clipped figure — and the
  middle yields first: `7 DAYS`, then `LAST 1H`, leaving `TODAY` and `MONTH` as
  the last two readings. Four fit in 34 columns, three in 25, two in 16, one in 7.

  A span this deployment cannot answer reads `—`, not a number. Without a cost
  ledger (Kubernetes by default) the proxy answers `today`, `7d` and `month` from
  its six-hour in-memory ring and reports the window it actually served; a
  six-hour figure under a `MONTH` label would understate the month by about 120x
  while looking perfectly well-formed.

  <a id="spans-and-the-cost-ledger"></a>
  **Spans and the cost ledger.** Three of the four spans are ledger-backed, so what
  agentop can really distinguish depends on whether the proxy keeps one:

  | | `LAST 1H` | `TODAY` | `7 DAYS` | `MONTH` |
  |---|---|---|---|---|
  | local install (ledger on) | ring | ledger | ledger | ledger |
  | Kubernetes (no ledger) | ring | `—` | `—` | `—` |

  The **band** detects this and draws `—`, so it never labels six hours of spend as a
  month. The **`$` breakdown does not**: `w` still offers all four spans there, and
  without a ledger three of them return the same clamped six-hour fold under three
  different captions. That is a known limitation rather than a design: the target is
  a local install, where the ledger is on by default and all four spans are real.
  `w` used to offer 15m/1h/6h, which is what a ledger-less deployment could actually
  tell apart, and those remain reachable through `agentop cost --window` and the Usage
  pane.

  A poll chain that stops answering is dated on its own label — `TODAY 7m` — so a
  wedged chain cannot pass for a current reading. Each span polls on its own
  cadence (20s for the ring-served hour, a minute for today, five minutes for the
  two that walk many day files), so the age is per cell rather than per band.

  The `~` on `SAVED` sits in the **heading**, not on every value. A saving is
  estimated in every row, so a per-row marker distinguished nothing while
  diluting the same glyph where it *is* conditional — on a cost figure whose
  pricing was incomplete. `+` (the real figure is larger) and `!` (spend is
  missing from the sum) stay on the values, because those are per-row claims.

  **How precise a money figure is depends on whether you scan it.** Anything read
  down a column or compared against its neighbours reads in **cents**: all four
  band cells (`LAST 1H`, `TODAY`, `7 DAYS`, `MONTH`), the sessions table's `COST`
  and `SAVED`, the Usage pane's `COST`, and both columns of the `$` drawer. Two
  digits of extra precision are noise on a surface whose job is comparing rows to
  each other.

  **One request** is the exception, and keeps four decimals: the events table's
  `COST`. A single cache-read request is $0.000038, so cents there would render
  a whole column identically — the one place the precision carries all of the
  information rather than a little of it.

  Neither form ever prints a positive figure as `$0.00`: cents falls back to
  `<$0.01` and four decimals to `<$0.0001`. "Free" is a claim about the traffic,
  never a rounding artefact.

  **Money is shown in the unit it was billed in.** An endpoint configured with a
  `unit:` (see [Billing units](../../docs/pricing.md#billing-units)) reads in that
  unit everywhere — `0.03 Bobcoins`, never `$0.03` — with the same precision rules
  as dollars. Figures in different units are never added: a band span holding
  both prints them side by side (`$6.20 + 0.03 Bobcoins`), and the drawer, the
  agents pane and the sessions table label each row with its own unit. A row that
  itself spans units reads `(mixed)`, the drawer's tier column is withheld for a
  mixed window, and so is the Usage pane's cost chart — scope to one agent (`A`,
  then `u`) to chart it. Where a column is too narrow for the name it is shortened
  (`0.03 Bobc…`), down to `¤`. A deployment with no `unit:` configured sees
  exactly the dollar figures it always did.

  `$` expands the band into two columns — where the money went, by rate tier,
  and who spent it, by model, endpoint or agent:

  ```
    WHERE IT WENT                               BY MODEL
  output        54% ████████████      $5.85   claude-opus-5         $11.12      17 req
   └ reasoning  31% ███████▏         ~$3.48   claude-sonnet-5       <$0.01       2 req
  cache-read    35% ███████▉          $3.90   claude-haiku-4-5      <$0.01     120 req
  cache-write    8% █▉                $0.98   (other)              <$0.01+       9 req
  input          3% ▊                 $0.39
   [a] [model] · endpoint · agent   [w] 1h   esc closes
  ```

  The tier figures are **modelled, not measured**: the split comes from the rate
  table even when the total beside it is a gateway's own authoritative figure,
  because a gateway reports one number per call and never breaks it down. They
  are apportioned so the column sums to the window total exactly, and a tier the
  rate table says nothing about shows `—` rather than `$0.00`, which would claim
  the tier was free. Below 86 columns the tier column drops and the panel
  degrades to the by-model breakdown alone.

  `└ reasoning` wears a `~` that no other row does, because it is **modelled twice
  over**: its share of output comes from the token counts, not from a cost the provider
  reported, so it is only the share of output
  *spend* where every model in the window bills output at one rate. A mixed window can
  be off by the spread between those rates. Nothing reports a reasoning cost, so this
  is the best available figure rather than a measured one.

  `└ reasoning` is a **child of output, not a fifth tier**. Reasoning has no rate
  of its own — it is the share of the generated tokens the model spent thinking,
  billed at the output rate — so its figure is already inside output's, and only
  the four unindented rows sum to the window total. Its share is denominated in
  that same total, which is what makes `31% ⊂ 54%` read as containment.

  The row is always present, in one of three states. A reported split shows its figure
  with a `~`. A split reported as **zero** shows an exact `$0.00` and no `~` — the model
  was asked to think and spent nothing on it, which is a measurement and the reading that
  says an effort setting is not reaching the model. `—` means no figure: the endpoint
  reports no split, or the apportioned share fell below a micro. Anthropic reports the
  count as `output_tokens_details.thinking_tokens` and OpenAI-format endpoints as
  `completion_tokens_details.reasoning_tokens`; both are read.

  The four **tier** rows carry **no** `~`, unlike the sessions table's `SAVED~`
  (the `└ reasoning` child does, for the reason given above). The caveat is
  real but this panel has no money heading to hang it on — `WHERE IT WENT` names
  the column, not the figures — so the choice was a tilde on every row or the
  sentence above, and the sentence won.

  The selected row is reverse-video rather than marked with a glyph, so it is
  the one thing these listings cannot show.

  **What the markers on a figure mean.** Each rides on the figure it qualifies,
  and a clean figure carries none — a marker present on every row would
  distinguish nothing:

  | | |
  |---|---|
  | `~` | estimated, so the figure is a lower bound rather than an exact total |
  | `+` | a floor: some traffic in the span is unpriced, or the span reaches back past what the ledger retains |
  | `!` | short by an amount nothing can state — a damaged ledger read, or a counter that hit its ceiling |

  This paragraph used to describe a single `SPEND` line whose figures were grouped
  by span — `SPEND  $30.93 today   1h: $2.91   cache 81% …` — with the day's cost
  and saving before `1h:` and the rolling window's readings after it, and a `~` on
  the saving on every row. That line is gone: it mixed spans on one row, which is
  the defect the band replaced, and the saving's unconditional `~` was noise on
  100% of rows. Avoided spend now lives per session in the sessions table and per
  series in the `$` breakdown; volume readings live in the Usage pane.
- **Events**: per-session event table. `c` opens a column picker — a popup with
  a checkbox and a one-line description per column, since twelve abbreviated
  headers are not self-describing.

  The picker is also where sorting lives: `s` orders the table by the column
  under the cursor, descending first, so "which calls took longest" and "which
  cost the most" are one keystroke from the column that answers them. Press it
  again for ascending, and a third time to return to arrival order. The sorted
  column is marked in its header (`DURATION▼`) and named in the footer
  (`[sort: DURATION▼]`) — the footer matters on a narrow terminal, where the
  sorted column may be one the table had to drop.

  Numeric columns sort numerically, not by the text in the cell: DURATION orders
  90ms before 1.20s, and TOKENS orders 900 before 1,048,576. Blank cells — a
  request whose response has not landed, or a figure the proxy could not model —
  sort to the bottom of a descending view rather than crowding the end you sorted
  toward. Sorting never changes the `#` exchange pairing or the per-row token and
  cost figures; it reorders the finished rows only.

  The twelve default columns together need ~168 terminal columns, so the table
  drops what does not fit and the footer says how many (`→ N more columns`). Columns carry a
  keep rank rather than being equally expendable: DIR, DURATION, TOKENS and COST
  give way first, while `#` and HOST survive longest. That is what makes HOST
  usable at 80 columns despite being last in display order — it is the column
  most people open this pane for.

  Twelve of the thirteen are on by default: `#` (exchange number, shared by a
  request and its response), TIME, DIR, PHASE, ACTION, PLUGIN, METHOD, STATUS,
  DURATION, TOKENS, COST, HOST. The thirteenth, BYTES, is opt-in through the
  picker (`c`), because only an opaque tunnel's close row has a figure for it.
  On a narrow terminal the low-ranked ones are hidden rather than turned off,
  so widening the window brings them back without touching the picker.

  Live-updates while in view — if the cursor is on the last row, it
  auto-follows new events.

  All twelve columns, on a terminal wide enough for them. Each `#` appears
  twice — once for the request, once for its response — which is how a row
  with no STATUS is read as "still in flight" rather than "failed":

  ```
  agentop · ctx-abc-1234…

   #     TIME          DIR   PHASE    ACTION    PLUGIN              METHOD              STATUS   DURATION    TOKENS             COST                 HOST
   1     14:23:07.41   in    req      allow     jwt-validation                                                                                       weather-agent
   1     14:23:07.52   in    resp     —         —                                       200      118ms                                               weather-agent
   2     14:23:07.71   out   req      observe   inference-parser    claude-sonnet-5                                            681,300(−9.9k)   $0.2546(−$0.0037)   api.anthropic.com
   2     14:23:08.91   out   resp     —         —                   claude-sonnet-5     200      1.20s       412                                     api.anthropic.com
   3     14:23:09.01   out   req      modify    token-exchange      tools/call                                                                       github-tool-mcp
   3     14:23:09.10   out   resp     —         —                   tools/call          503      96ms                                                github-tool-mcp

  ● connected   2.1 events/sec   [sort: DURATION▼]   [filter: anthropic]
  [↑↓] nav  [b/f] page  [↵] detail  [c] columns  [u] usage  [s] hide passthru/skip  [p] pause  [/] filter  [esc] back  ·  → 4 more columns ([c] to choose)  [?] keys  [q] quit
  ```

  `—` in ACTION and PLUGIN means no plugin acted on that message; a `tunnel`
  there is an opaque CONNECT, where METHOD is blank because opaque bytes carry
  no request line. The tunnel's STATUS arrives on its `resp` row when it closes,
  with DURATION for how long it stayed open and, in BYTES, what it carried each
  way. That STATUS is the
  CONNECT's own (200, or 502 when the destination could not be reached), never
  the destination's, which travels inside the client's TLS. The TOKENS and COST figures on a request
  row carry what `tool-prune` saved in parentheses — `−` for a counted saving,
  `~` for a projected one.
- **Detail**: pretty-printed JSON of a single event. Scroll with arrow
  keys; `y` yanks to `~/.cortex/agentop-events/<timestamp>-<rand>.json` and
  shows the path in the footer until you press another key. The directory is
  private to you (0700, inside `~/.cortex`) and the files are 0600 — yanked
  events carry identity subjects, raw LLM completions and tool arguments, so
  they are deliberately not written to a shared location such as `/tmp`.
  Nothing prunes them: they accumulate until you delete them, and unlike
  `$TMPDIR` this location is never cleared by the OS. Given what they hold,
  `rm` the ones you are done with. The footer shows the path expanded rather
  than abbreviated with `~`, and gives it the whole line while the notice is up;
  on a terminal too narrow for it the path is truncated from the left, so the
  filename stays readable.
- **Pipeline**: the active plugin chain in inbound + outbound order, opened by
  `P` from Sessions, Events or Detail; `Esc` returns to whichever of those it was
  opened from. Columns: position, direction, plugin name, DEPS (✓/✗ — see "Plugin
  dependencies" below), writes, body access, event count. `e` opens
  the editor. Outside the viewer, `agentop pipeline get` prints the same
  composition — plus each plugin's description and config — and `--json` emits
  `/v1/pipeline`'s own shape for a script. It has no DEPS or event count:
  one is derived from the chain rather than reported by the proxy, the other
  counts invocations in cached session events, which a one-shot command has
  none of.
- **Plugin detail**: drill-into-row for Pipeline or Catalog. Shows
  description, position, reads/writes, body access, plugin config, and
  per-dependency satisfaction status against the active chain.
- **Usage**: time-bucketed charts of volume, errors, latency and cost,
  opened by `u` from Sessions (all sessions) or Events/Detail (the
  selected session). Sourced from `/v1/usage`, which the proxy
  aggregates server-side — so every operator watching a pod sees the
  same history, including traffic from before they attached. Refetches
  every 20s while in view.

  `m` cycles the metric. Counts (tokens/requests/errors) and cost render
  as bars; latency renders as mean-with-whiskers (`┼` mean, `┬`/`┴` ±1σ),
  because a bar encodes magnitude from a zero baseline and mean latency
  has no meaningful zero. On a terminal with room to spare the bar chart
  captions its y-axis with the metric's unit (`tok`, `req`, `err`, `USD`, or a
  cost chart's billing unit, such as `Bobc…`);
  latency has no caption, because its own labels carry the unit per
  magnitude (`820ms`, `4.1s`). `b` cycles the breakdown, which stacks each
  bar by status, model, plugin or host — each series marked with a letter
  derived from its name (`s` for claude-sonnet-5) on a coloured ground, so
  the chart reads without colour too. Statuses ≥400 render red. `b` is not
  offered for latency: the aggregator holds no per-label latency, so there
  is no per-status mean to plot.

  The host breakdown answers "where is my traffic going" — one band per
  upstream, so two agents sharing one Cortex are told apart by the hosts
  they call. Ports are folded in, so a bridged `api.anthropic.com:443`
  and the request inside that tunnel are one band. A request the listener
  recorded no host for joins the `(unlabelled)` remainder. Hosts are more
  numerous than models or statuses, so expect `(other)` here sooner.

  An idle bucket shows `0` rather than an empty column, so a gap in
  traffic is distinguishable from traffic too small to plot. A bucket
  carrying traffic no label claims shows an `(unlabelled)` band, and
  series past the palette fold into `(other)` — every band drawn has a
  legend entry.

  ```
  agentop · http://localhost:9094 · usage · all

    USAGE — all sessions — 10m0s @ 1m0s — tokens — by model

     12k                            ▄▄▄▄
                              ████  ssss
    9.6k        ▃▃▃▃          ssss  ssss
                ssss    ▅▅▅▅  ssss  ssss
    7.2k  ▂▂▂▂  ssss    ssss  ssss  ssss
          ssss  ssss    hhhh  hhhh  ssss
    4.8k  ssss  ssss    hhhh  hhhh  hhhh
          hhhh  hhhh    hhhh  hhhh  hhhh
    2.4k  hhhh  hhhh    ····  ····  hhhh
          ····  ····    ····  ····  ····
       0 ┼────┴────┴────┴────┴────┴────
         14:23   :25   :27   :29   :31
        6.1k  8.8k     0  11k   9.9k  12k

    s claude-sonnet-5 (34.2k)   h claude-haiku-4-5 (9.4k)   · (unlabelled) (2.1k)

    REQUESTS 412    ERRORS 3 (0.7%)    TOKENS 48.2k    LATENCY 1.31s    COST $0.94

    updated 7s ago (every 20s)

  [m] metric  [w] window  [b] breakdown  [s] this session  [esc] back  [?] keys  [q] quit
  ```

  Under `latency` the bars give way to mean-with-whiskers and the breakdown
  hint disappears, since the aggregator holds no per-label latency:

  ```
    USAGE — all sessions — 10m0s @ 1m0s — latency — no breakdown for latency

   2.4s              ┬
                     │       ┬
   1.8s        ┬     │       │
               │     ┼       │
   1.2s  ┬     ┼     │       ┼
         ┼     │     ┴       │
   600ms ┴     ┴           ┴
       0 ┼────┴────┴────┴────┴────
         14:23   :25   :27   :29
        1.2s  1.8s     0  2.1s  1.4s

    ┼ mean   ┬ +1σ   ┴ −1σ   (0 = no measured responses)
  ```

- **Catalog**: registered-plugin browser, opened by `P` from any
  session-view pane. Lists every plugin the running binary knows how to
  construct, including ones not in the active pipeline. Useful for
  discovering what's available before adding to the pipeline. Sourced
  from `/v1/plugins`.

  REQUIRES lists a plugin's hard dependencies comma-separated; an either-or
  group appears as one entry joined by `|`. Both are checked against the
  ACTIVE pipeline in the Pipeline pane's DEPS column, not here — this pane
  lists what the binary can build, not what is wired up.

  ```
  agentop · http://localhost:9094 · catalog

   NAME                    REQUIRES                      DESCRIPTION
   jwt-validation                                        Validate inbound JWTs against JWKS
   token-exchange                                        RFC 8693 exchange for a target audience
   mcp-parser                                            Parse MCP JSON-RPC requests and results
   inference-parser                                      Parse LLM chat/completions traffic
   ibac                    a2a-parser                    Intent-based access control
   tool-prune              inference-parser              Drop unused tool definitions

  [↑↓] nav  [↵] plugin detail  [r] refresh  [esc] back  [?] keys  [q] quit
  ```

- **Kubernetes Namespaces** (optional): the way in when agentop has no endpoint
  to connect to — one row per namespace holding an AuthBridge agent, then a
  Pods pane, then an automatic `kubectl port-forward` into the session view.
  Shown when `--endpoint` was not given and either no local Cortex is answering
  or `--kubernetes` was passed; `[l]` leaves it for the Cortex on this machine.

  ```
  agentop · pick namespace

   NAMESPACE                       PODS
   team1                           2
   team2                           1
   default                         1

  [↑↓/jk] nav  [↵] open  [l] localhost:47601  [r] reload  [?] keys  [q] quit
  ```

  Without `--kubernetes`, a running local Cortex is connected to directly and
  this pane never appears. With `--endpoint` the flag is moot: an explicit
  address always wins.

Layered on top of all of them:

- **Key help**: a modal overlay opened by `?` from anywhere (picker
  included). It is a map of the panes as much as a key list, ordered by
  what a lost reader asks first:

  1. the current pane, highlighted — what it shows, then its own keys;
  2. **GO TO ANOTHER PANE** — the keys that leave, each naming the pane
     it opens and what is on it. Rendered per pane and only where the
     key actually works: all four (`u`, `P`, `C`, `$`) from the session
     views, `C` alone on Usage, and on the two pickers the heading kept
     with a line saying they open once you're connected, rather than four
     dead keys;
  3. **THE DRILL PATH** — `namespaces → pods → sessions → events → event
     detail` on one line, since that spine is also what `Esc` walks back.
     If you're on one of its five panes it's in brackets; the four
     key-opened panes are not steps on it, so they get a line naming that
     and naming where `Esc` returns them;
  4. **ANYWHERE** — `?`, `p`, `q`, the three keys with one meaning
     everywhere — then **MOVING AROUND THIS HELP** (`↑↓`/`jk`, `b`/`f`,
     `g`/`G`: these move the overlay on every pane, and a note names the
     one pane where their closed-overlay meaning differs), then **INSIDE
     THE SPEND DRAWER** (`a`/`w` are live only while it is open, so they
     are not mixed in with the keys that always work — and the section is
     omitted on the panes where `$` is refused);
  5. **EVERY PANE** — the other nine in full, purpose and every
     binding's description. Not compacted to bare keys: `USAGE  m w b s
     esc` said the pane has five keys and nothing about what any of them
     do.

  While it's up it owns the keyboard — `?`, `Esc`, or `q` closes it
  (`q` closes the overlay rather than quitting agentop). This is the
  discoverable home for keys the single-line footer has no room for,
  `P` among them. Two exceptions: while a pipeline edit is in flight
  that overlay is already modal and owns `y`/`N`, and while the filter
  input is focused `?` is a character you're typing (session IDs and
  hosts can contain one). In both cases `?` is inert until the keyboard
  is released.

  The body scrolls, so the full reference is reachable on a short
  terminal: `↑↓`/`jk` by line, `b`/`f` or PgUp/PgDn by page, `u`/`d` by
  half page, `g`/`G` to the ends. A `[↑↓] scroll  <n>%` affordance
  appears in the overlay's footer only when the content overflows; the
  close hint stays pinned there at every scroll position. Resizing the
  terminal re-ranges AND re-wraps the body without losing your place —
  prose wraps to the panel width rather than being clipped at the right
  edge, so the descriptions and the scope note survive a narrow
  terminal.

  Spelling out all ten panes costs roughly four screens at 24 rows,
  which `g`/`G` and the pinned close hint are what make affordable. The
  overlay is the one surface with no width or height budget to defend,
  so it is where completeness belongs.

## Keybindings

| Key | Context | Action |
|---|---|---|
| `?` | any (not while filtering or mid-edit) | open the key-help overlay (`?`/`Esc`/`q` closes) |
| `↑ ↓` / `k j`, `b`/`f`, `u`/`d`, `g`/`G` | key help | scroll the overlay |
| `↑ ↓` / `k j` | picker, list | navigate rows |
| `Enter` | namespaces | open the namespace |
| `l` | namespaces | connect directly to this machine's Cortex, or to `localhost:9094` when none is installed |
| `Enter` | pods | port-forward + connect |
| `Esc` | pods | back to namespaces |
| `r` | namespaces, pods | reload agent list from cluster |
| `Enter` / `→` / `l` | sessions, events | drill into selection |
| `Esc` / `←` / `h` | detail, events | back out |
| `Esc` | sessions | (picker mode) tear down port-forward and back to pods. The only pane that does this — every key-opened surface returns to its caller instead |
| `/` | sessions, events | filter (substring match; Enter commits and saves, Esc cancels the edit and saves nothing; clear the box and press Enter to remove a saved filter) |
| `s` | events | toggle skip-row visibility (default: hidden; the events footer shows the hidden count) |
| `c` | events | open the column picker (`↑↓`/`jk` move, `space`/`x` toggle, `s` sort, `r` reset, `Esc`/`Enter`/`c` close); the selection and sort are saved on close |
| `s` | column picker | sort by the column under the cursor: descending → ascending → chronological. Pressing it on a different column starts that column descending. `#` is not sortable — its order already *is* chronological |
| `p` | any | pause/resume stream |
| `y` | detail | yank event JSON to `~/.cortex/agentop-events` (path stays until the next keypress) |
| `g` / `G` | lists | jump to top / bottom. In the events timeline this also sets where the *next* session opens — see [Where a session opens](#where-a-session-opens) |
| `u` | sessions, events, detail | open the usage charts (sessions: all sessions; events/detail: the selected session) |
| `$` | every pane except the two pickers, usage and agents | expand the band into a breakdown — where the money went by rate tier, and who spent it by model, endpoint or agent — in place, so the table stays on screen. Needs 27 rows; refuses on the two pickers (nothing is connected yet), on the usage pane, which is already a breakdown with its own cycles, and on the agents pane, which is itself the per-agent breakdown |
| `a` | while the breakdown is open | cycle the axis: model / endpoint / agent. Not `g`, which is the global "jump to top" |
| `w` | while the breakdown is open | cycle the span: the band's four — last 1h / today / 7 days / month. Without a cost ledger only the hour is distinct; see [Spans and the cost ledger](#spans-and-the-cost-ledger) |
| `m` | usage | cycle metric: tokens / requests / errors / latency / cost |
| `w` | usage | cycle window: 10m / 1h / 6h |
| `b` | usage | cycle breakdown: none / status / method / plugin (not offered for latency — there is no per-label latency) |
| `s` | usage | toggle between this session and all sessions |
| `Esc` | usage | back to the pane it was opened from |
| `P` | sessions, events, detail | open the pipeline. Capital `P` because lowercase `p` pauses the stream and has no replacement worth the trade — `space` and `d` are the table's page-down and half-page-down |
| `Esc` | pipeline | back to the pane `P` was pressed on |
| `C` | any session-view pane (not the picker) | open the registered-plugin catalog. Was `P` until the pipeline took that letter |
| `r` | catalog | refresh the catalog from `/v1/plugins` |
| `A` | any session-view pane (not the picker) | open the per-agent cost breakdown — what each coding agent has spent today, plus a row of dashes for any agent that owns a listed session but has spent nothing today, since the sessions list is not limited to today and every session in it needs a row to scope to. Capital `A` because lowercase `a` cycles the spend drawer's axis. Refetches on every press, then **refuses below two agents**, counting both kinds of row, and says which one it found: a one-row breakdown restates a total already on screen. Not in the footer for that reason; the `?` overlay names it |
| `↑↓` / `jk` | agents | move the cursor |
| `↵` | agents | scope to the agent under the cursor and list its sessions, whichever pane `A` was pressed on — including the agent already scoped, which stays scoped. **The first row, All agents, clears the scope.** It reaches the sessions list, the usage pane, and the spend band and its drawer; `agentop cost --agent` is a separate process |
| `Esc` | agents | back to the pane `A` was pressed on, leaving the scope as it is |
| `e` | pipeline | edit pipeline subtree in `$EDITOR` |
| `y` | edit/diff | apply the edit |
| `N` | edit/diff | abort the edit |
| `r` | edit/error | retry: re-open the editor (post-edit failure) or refetch (fetch failure) |
| `Esc` | edit/{fetching,editing,applying} | abort the edit, return to Pipeline pane |
| `Esc` | edit/{waiting,rollback} | background the watch; result lands as a footer flash |
| `q` / `Ctrl+C` | any | quit (closes the key-help overlay first, if open) |

**A trackpad or wheel scroll is `↑ ↓`**, in whichever pane has focus. agentop puts the
terminal in application cursor-key mode, as vi does, which is what makes Terminal.app
translate a scroll over a full-screen program into arrow keys rather than scrolling its
own scrollback. agentop does not ask for mouse reporting, so click-and-drag selection
stays the terminal's and copying text out of a pane works as it does at a shell prompt.
The cost is that a scroll moves the focused pane, not the one under the pointer.

**The picker also opens itself at startup**, once per connection, when two or more
agents have spent in the window or own a listed session — the same two-agent rule `A`
applies, so a one-agent proxy goes straight to the sessions pane and says nothing. The
`default` bucket and unrecognised clients' sessions never count toward the two. When the
spending reply beats the first session list, that list gets one more look, and only
while the sessions pane is still showing. The cursor
starts on `All agents`, so `↵` and `Esc` both keep every agent in view. Nothing is
remembered between runs: a second agent appearing is exactly when the picker
becomes worth showing, so a remembered dismissal would go stale then.

**Agents the proxy does not recognise share one row, `Other`**, listed last. The
proxy names an agent only from a User-Agent it recognises (`claude-code`,
`bob-shell`, `ibm-bob`, `opencode`); anything else is reported under its raw
User-Agent, and one program can send several — the IBM Bob IDE was three rows before
it was recognised, and none of them could reach its session ([#1210]). Picking `Other`
lists every session that names no recognised agent, including the `default` and
`pending:` buckets and sessions only agentop's cache still holds, so every listed
session belongs to exactly one row. An `Other` row that only sessions put there —
no unrecognised traffic in the window — is shown when the picker opens, but never
opens it by itself. Its figures are the pooled series, narrowed by agentop rather
than asked of the server, so the spend drawer reads `BY NONE` on every axis under
it. The raw User-Agents are still in the unscoped drawer's agent axis and in
`agentop cost --by agent`.

[#1210]: https://github.com/rossoctl/cortex/issues/1210

While a scope is active, two things on the usage pane change and both say so:
`[b]` disappears from the footer, because the scope needs `group=agent` on the
wire and there is no second axis left to break down by; and the latency metric
reports that it is unavailable per agent rather than plotting zeroes. Response
times are recorded per bucket across every agent that shared it, so
`/v1/usage` carries nothing that could attribute them to one.

Elsewhere, the sessions pane lists only that agent's sessions — the `default` and
`pending:` buckets belong to no one agent and are listed under `Other` — and every pane's title
says `agent=`. Against a server that names no session's agent, the list stays whole
and its footer says `list not scoped`. The spend band and its drawer show that
agent's figures, asked of the server with `/v1/usage?agent=`; against a server too
old to answer that, agentop narrows the `group=agent` series itself, as the usage pane
does. The drawer drops its agent axis and breaks the agent's spend down by model or
endpoint in every window. It reads `BY NONE` under `Other`, and against an older proxy
that cannot break the last hour down for one agent. Unscoped, the sessions pane gains an `AGENT` column when
two agents are listed, and the agents pane a `SESSIONS` count once sessions name
their agent.

## Settings

agentop remembers the events-table column selection, the sort order, and the active
filter in `~/.cortex/agentop-config.yaml`. Columns and the sort are saved when the
column picker closes with `Esc`/`Enter`/`c` (`q` quits without saving); the filter is
saved when you commit it with `Enter`. There is no explicit save step.

A restored filter is shown in the footer as `[filter: …]` while it is in effect but
not being edited — otherwise a shortened list would have no explanation on screen.
Pressing `/` puts the cursor in the restored value so you extend it rather than
replace it, and `Esc` abandons the edit and puts the previous filter back without
writing anything. `Enter` is the only key that saves a filter, so clearing one means
emptying the box and pressing `Enter`. In picker mode the restored filter applies to
the first session view you open and is then dropped when you go back to the pod list:
a filter surviving a pod switch reads as data loss, so the active one is cleared while
the saved one stays on disk for the next start.

`--prefs PATH` reads and writes somewhere else. This is *not* the Cortex proxy
config — that is `~/.cortex/config.yaml`, and `--config` on `agentop service` and
`agentop configure claude-code`.

```yaml
# agentop user settings. Written by agentop; safe to hand-edit or delete.
# Columns not listed under events.columns keep their default — only deviations are recorded.
events:
  columns:
    - name: COST
      visible: false
    - name: TOKENS
      visible: false
  # Omit sortColumn (or name a column this build does not have) for arrival order.
  sortColumn: DURATION
  sortDesc: true
filter: github-tool
```

### Schema

| Key | Type | Default | Meaning |
|---|---|---|---|
| `events.columns[].name` | string | required | column id, from the table below |
| `events.columns[].visible` | bool | required | show that column — an entry without it reads as `false` |
| `events.sortColumn` | string | unset | sort by this column; unset means arrival order. `#` is not sortable, being arrival order already |
| `events.sortDesc` | bool | `false` | sort descending; ignored unless `sortColumn` names a sortable column |
| `filter` | string | empty | the active filter |
| `usage.metric` | string | `tokens` | usage-pane metric: `tokens`, `requests`, `errors`, `latency` or `cost` |
| `usage.window` | string | `10m0s` | usage-pane window: `10m0s`, `1h0m0s` or `6h0m0s` |
| `usage.group` | string | `none` | usage-pane breakdown. `[b]` cycles `none`, `status`, `method`, `plugin`, `host`; a hand-edited file may also use `model`, `endpoint`, `session` or `agent` |

List a column only to change it — the twelve are all visible until you hide one, and
an unrecognised name or sort column is ignored. Inside an entry, always write
`visible:` explicitly: it is optional to the parser but reads as `false`, so
`- name: COST` on its own hides COST rather than showing it.

The three `usage.*` keys are the view `[m]`, `[w]` and `[b]` choose, saved as you
change them, so reopening the pane after a restart lands on the view you left. They
are stored by name rather than by position, and a name this build does not have —
a typo, or a value from a newer agentop — falls back to that field's default, so it
costs you the setting and never a broken pane. The four extra groupings above are
recognised, not defaulted: `[b]` does not cycle to them, but the pane renders them
and they survive a restart.

Column ids, in display order — the same headers the picker shows:

| Id | Shows |
|---|---|
| `#` | exchange number; a request and its response share one — quote it as `"#"` in YAML, or it reads as a comment |
| `TIME` | wall-clock time the message was recorded |
| `DIR` | `in` = toward your agent, `out` = toward an upstream |
| `PHASE` | `req`, `resp`, or `denied` |
| `ACTION` | what took effect: `deny`, `modify`, `observe`, `allow`, or `tunnel` |
| `PLUGIN` | which plugin acted; blank when none did |
| `METHOD` | protocol operation: model name, MCP or A2A method |
| `STATUS` | HTTP status of the response |
| `DURATION` | how long the exchange took |
| `BYTES` | bytes an opaque tunnel carried: ↑ sent, ↓ received — off by default |
| `TOKENS` | tokens used, and what `tool-prune` saved |
| `COST` | estimated cost, and what `tool-prune` saved |
| `HOST` | host the message was sent to |

A narrow terminal also hides columns to fit, with a `→ N more columns` note in the
footer; that is not saved, and widening the window brings them back.

Each setting comes from **this file, or the built-in default** when the file does not
set it. A missing file is normal and silent. An unreadable or malformed one is
reported on stderr and ignored in full — never partially applied, and never fatal.

Nothing else feeds a setting: there is no environment variable (no `AGENTOP_*`, and
`XDG_CONFIG_HOME` is not consulted) and no flag for an individual setting. `--prefs`
chooses *which* file, never what is in it — so to try a layout without disturbing
your own, point it at a throwaway file. Otherwise change the setting in the TUI,
which saves it, or edit the YAML.

## Where a session opens

Opening a session puts the cursor on the end you last jumped to with `g` or `G`:
the newest event by default, or the oldest if `g` was the last end you asked for.
Those two keys already mean "take me to an end", so they double as the preference
and it persists with your other view settings. An arrow key that happens to reach
row 0 does not change it — scrolling up to read is navigation, not a preference.

Only the *opening* chooses an end. Every later rebuild — the two-second poll, a
filter, a column toggle — leaves the cursor where you put it, and the timeline
follows new events only while you are on the newest row. Under a column sort the
preference does not apply at all: there "an end" is the largest or smallest value
rather than the oldest or newest event, so the cursor stays pinned to the event you
were reading instead.

## How a timeline is fetched

The events table renders no message body, so it does not ask for one. Each fetch
sends `?view=summary`, which drops the conversation payloads and keeps everything
the table shows **or its filter searches** — measured at ~163x smaller, and the
difference between a session that opens in milliseconds and one that takes seconds.

The filter is the part worth spelling out, because it is easy to assume otherwise:
`/some text` still matches completion and A2A message text, and `plugin:<name>`
still works, because those fields are searched and so are kept. Dropping them would
have been ~299x instead of ~163x — both about a megabyte for a 1000-event session,
so the filter is worth far more than the difference.

The consequence is worth knowing: the first `↵` on a row fetches that one event in
full from `/v1/sessions/{id}/events/{seq}`. The detail pane renders the summary
immediately and the bodies appear when they arrive, so there is no loading screen —
but on a slow link the message text lands a moment after the rest. Re-opening the
same row is free; the bodies are kept. If the fetch fails, the pane keeps what it
has and the error goes to the footer.

`y` yanks whatever the pane is showing, so while the bodies are still in flight — or
after a failed fetch — it says so in the flash rather than handing you a body-less
event that looks complete.

Because a projected event is ~1KB rather than ~200KB, agentop asks for the server's
full 2000-event ceiling instead of the old 500. A normal session therefore arrives
whole, and the `N older ([o] to load)` note appears only for genuinely long ones.

Against a proxy that predates `?view=summary` this still works — that server
ignores the parameter and returns full events, so the timeline is correct and as
slow as it used to be. agentop can tell the difference from the response and does not
waste a per-row fetch on bytes it already holds.

## Editing the pipeline

Press `e` on the Pipeline pane to edit the runtime `pipeline:` subtree in
`$EDITOR` (or `vi` if unset). On save, agentop shows a diff and asks
`apply this change? (y/N)`. Confirming writes the change back, then polls the
framework's `/reload/status` until the reload completes (success or failure).

Where it writes depends on what you are connected to, not on how agentop was
started:

| Connected to | `e` writes | Reload confirmed via |
|---|---|---|
| a pod, via the picker | `kubectl apply --server-side` against the per-agent ConfigMap, `--field-manager=agentop --force-conflicts=true` (taking ownership of `data.config.yaml` from the operator's webhook on first edit) | the port-forward's `:9093/reload/status` |
| the Cortex on this machine | `~/.cortex/config.yaml` directly — the file that proxy was started with and already watches | that proxy's own stats address, `stats.address` in the same file |

The local path needs neither kubectl nor a ConfigMap: the proxy watches its
config file with fsnotify, so writing the file *is* the apply. agentop writes a
temporary sibling and renames it over the target, so the watcher only ever sees
a complete file — a half-written one would book a reload failure against an
edit you never made.

Local editing is offered whenever the endpoint on screen is this machine's
Cortex: a bare `agentop` that auto-connected to it, `[l]` from the picker, or an
explicit `--endpoint` aimed at its session API. Loopback spellings are
interchangeable, so `--endpoint http://localhost:47601` and
`http://127.0.0.1:47601` both match a config bound to either. A local Cortex
merely *running* is not enough — while you are looking at a pod, `e` edits the
pod.

If neither target applies, `e` says so and names the remedy instead of starting
an edit it cannot finish.

A **symlinked** `~/.cortex/config.yaml` — pointing at a dotfiles repo, say — is
written *through*, not over. `rename(2)` replaces the link itself, so without
resolving it the live config would become a regular file while the tracked copy
silently kept the old pipeline, with nothing in `git status` to show it.
Resolving also keeps the temporary file a sibling of the real one, which the
atomicity guarantee needs: a rename across filesystems fails `EXDEV`.

That relies on the proxy watching the resolved file's directory, which the
reloader does — but the reloader ships in `cortex`, and that installs
separately from agentop. **A proxy started before that change still has the old
single watch**, so on Linux a symlinked config will not observe the write: the
poll times out and rolls the edit back. Run `agentop service restart` after
upgrading the proxy. agentop cannot detect the mismatch — nothing the proxy
exposes describes its watcher — so this is a note rather than a check.

**A concurrent write aborts the apply.** This file has other writers — `agentop
tools scan --write`, the config migration `agentop service install` runs, a second
agentop session — and `$EDITOR` can be open for minutes. The apply re-reads the file
first and refuses
if it moved, rather than renaming a whole file built from stale bytes over
somebody else's change. The refusal names the likely culprits; re-open the edit
to work from the current file. This is a compare, not a lock, so a writer
landing in the last moment before the rename still wins — it closes the
realistic window, not every window. The cluster path has no equivalent check on
purpose: it applies with `--force-conflicts=true` and takes field-manager
ownership, which is what makes editing an operator-owned ConfigMap possible.

A config with no top-level `pipeline:` key is reported as such; inventing the
block is not the editor's job.

The single edit flow covers four operations:
- **Edit a value** — change a config field of an existing plugin
- **Reorder** — move a plugin's lines up or down
- **Remove** — delete a plugin's entry from `inbound:` or `outbound:`
- **Add** — append a new plugin entry

All four work because they're all just lines you change inside the
pipeline subtree.

`e` needs a target it can write. That means either a pod chosen through the
picker, or a connection to the Cortex on this machine — including via
`--endpoint`, as described above. Pointed at anything else (a hand-run
`kubectl port-forward`, a remote session API), neither is available: there is no
pod identity to resolve a ConfigMap from and no local config file to write, so
pressing `e` flashes a hint naming the remedy instead of opening an edit it
cannot finish.

### Pre-apply validation

After save, agentop runs the same Requires/RequiresAny/After/Claims
checks the framework runs at reload-time, against the cached
`/v1/plugins` catalog. Issues land as a red banner above the diff in ~50ms
instead of at hot-reload — which means after the kubelet sync (~60s) in a
cluster, or about a second later locally:

```text
⚠ 1 validation issue — framework reload will reject:
  • [outbound] ibac pos 1: Requires "mcp-parser", but it is not in the outbound chain
```

The y/N prompt becomes "apply anyway? (y/N)" — agentop's check is
non-blocking. The framework's own validateRelationships is the
source of truth and will fire again at reload regardless.

Validation is silently skipped when the catalog isn't loaded
(operator hasn't pressed `P` yet). Visit the catalog pane once to
populate it for the rest of the session.

### Agent-name resolution

Cluster path only — a local edit has no agent and skips all of this.

The per-agent ConfigMap is named `authbridge-config-<agent>`. agentop
resolves `<agent>` from the selected pod's `app.kubernetes.io/name`
label (operator sets this). If the label is absent, agentop
falls back to stripping the last two dash-separated segments of the
pod name (the ReplicaSet hash + pod suffix).

### Auto-rollback on reload failure

If the write succeeds but the reload fails (unknown plugin name, malformed
config, validation error), the framework keeps the previous in-memory pipeline
serving requests — but the stored config now holds the bad YAML. agentop detects
this via `/reload/status` and re-applies the content captured at Fetch time,
reconciling the stored state back to what is actually running.

The error overlay names the target it reconciled:
`reload failed: <reason>; rolled back to previous ConfigMap` in the cluster, and
`…rolled back to previous config file` locally. If the rollback itself also
fails, the message says where to look — `check kubectl` for a pod, the config's
own path for a local edit.

The rollback is best-effort in both, for the same reason and by different
mechanisms. In the cluster, `--force-conflicts=true` means a third party
(controller, `kubectl edit`, kustomize) who modified the ConfigMap between Fetch
and the failed reload has their change overwritten. Locally the forward apply is
guarded by the staleness check described above, but the rollback deliberately is
not — it runs *because* the file changed, so checking would refuse every
rollback. The running pipeline is unaffected either way.

### Backgrounding the watch

Pressing `Esc` while waiting for hot-reload (or during rollback)
moves the watch to the background instead of aborting it. The
overlay closes, the footer flashes
`hot-reload watch moved to background; you'll be notified`, and you
can resume navigating the TUI. When the watch terminates, the
result lands as a one-line flash:

- `hot-reload succeeded`
- `hot-reload failed: <reason>; rolled back to previous ConfigMap` — or
  `previous config file` for a local edit
- `hot-reload failed: <reason>; rollback failed: <err>` (rare)

Flashes auto-dismiss after a few seconds; if you miss one, query
`/reload/status` directly — through the port-forward for a pod, or at the
`stats.address` in `~/.cortex/config.yaml` for a local Cortex.

### Permissions

agentop shells out to `kubectl`; kubectl uses your kubeconfig. Editing
requires `update` on `configmaps` in the agent's namespace (in
addition to `get pods` which the picker already needs). RBAC denial
surfaces verbatim in the overlay.

### Tempfile lifecycle

agentop writes the editable pipeline subtree to `$TMPDIR/agentop-pipeline-*.yaml`
on every edit. The tempfile is **left in place on every exit path**
(success, error, abort) so an interrupted edit is recoverable. On
agentop launch, files older than 24h in this glob are swept
automatically — no manual cleanup needed.

### Hot-reload window

The framework reloads via a config-file watcher, so how long the wait is depends
on how the edit reaches that file. In a cluster, kubelet syncs ConfigMap edits
into the pod's mount within ~60s and the framework then debounces and reloads —
typically under 90s wall-clock from apply. Locally there is nothing to sync: the
proxy is already watching the file agentop wrote, so a reload normally lands in
about a second. The overlay says which of the two you are waiting on. agentop shows
a spinner either way.

The poller terminates with one of:

- **Success** — `/reload/status.last_success` advances past the apply
  time.
- **Failure** — `reloads_failed` increments past its baseline; the
  framework's `last_error` is shown.
- **Unreachable** — 5 consecutive transport errors against `/reload/status`
  surface as `reload status endpoint unreachable` after a few seconds rather than
  waiting the full deadline. The endpoint is the port-forward's `:9093` for a pod
  and the local proxy's `stats.address` otherwise, so the message asks the
  question that fits: a dropped port-forward or crashed framework in the cluster,
  or simply whether the local proxy is still running.
- **Timeout** — none of the above within the target's deadline: 120s for a
  ConfigMap, sized for the kubelet sync, and 30s for a local file, which lands
  in about a second. Triggers an auto-rollback so the stored config doesn't
  drift from the running pipeline. A ceiling, not a wait — the poll returns the
  moment `last_success` moves, so this only bounds how long a *failed* reload
  takes to be declared.

## Plugin dependencies

Plugins declare dependencies in their `Capabilities()`:

- **Requires**: hard dependency. The named plugin MUST be in the same
  chain at a strictly-lower position; otherwise framework reload fails.
- **RequiresAny**: soft OR. At least one of the listed plugins must
  appear upstream; each one that IS present must be earlier.
- **After**: ordering hint. If the named plugin IS present, it must
  appear earlier; absent is OK.
- **Claims**: exclusive ownership. Within one chain, two plugins
  cannot both declare the same claim string.

agentop surfaces these in three places:

- **Pipeline pane DEPS column**: ✓ when all declared deps satisfied,
  ✗ when any fail, blank when no deps declared. The footer hint
  reports the count of plugins with unmet deps.
- **Plugin detail pane**: per-dependency rows with ✓/✗ and the
  satisfying upstream's position when applicable.
- **Pre-apply validation in the editor**: catches missing/misordered
  Requires before the write goes out (~50ms, against a framework roundtrip of
  ~60s in a cluster or ~1s locally). See the "Pre-apply validation" subsection
  above.

The framework's own validateRelationships is the source of truth and
runs at every reload. agentop's checks are the fast-feedback layer.

## Trust model

`agentop` does no authentication — same as the server. Use only against
sidecars reachable via in-cluster networking or a local port-forward.
Session events contain raw user messages, LLM completions, and tool
results; treat the output accordingly.

## Architecture

- `apiclient/` — HTTP + SSE client. Sole owner of the `:9094` wire format.
  Auto-reconnects with exponential backoff (1s → 30s, capped, indefinite).
- `tui/` — Bubble Tea model/update/view. All state mutation runs on the
  Tea event loop; the SSE goroutine produces messages the loop consumes.
- `main.go` — flag parsing, signal handling, wires `tui.Run`.

## Deferred to later PRs

- Native clipboard (currently writes a file under `~/.cortex/agentop-events`).
- More persisted settings (#954): pane sizes, theme. Each needs the setting itself
  before there is anything to persist — pane sizes are recomputed per frame, and
  there is no theme to choose. (Sort order is done: see the column picker's `s`.)
- Fuzzy search beyond substring match.
- Per-user filtering (`Identity.Subject == X`).
- Krew plugin packaging.
