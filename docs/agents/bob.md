# IBM Bob

IBM Bob is a VS Code fork, so it reads the VS Code proxy setting. `agentop
configure bob enable` writes it as a flat, top-level key in Bob's user settings,
and on macOS one more key beside it:

```json
{
  "http.proxy": "http://127.0.0.1:47600",
  "http.systemCertificates": false
}
```

Flat and dotted, not nested under an `"http"` object — that is the shape VS Code
reads, and the shape difference from `configure claude-code`, which writes a
nested `"env"` block. The address is read from `listener.forward_proxy_addr` in
`~/.cortex/config.yaml` on every run, so a moved port or an IPv6 loopback
produces the right value rather than a hardcoded 47600.

Nothing else in the file changes, and that is meant literally: the keys are spliced
into the existing bytes rather than re-serialized from a parsed document, so your
key order, your indent width, your inline arrays and your blank lines between
groups all survive untouched. A settings.json is hand-curated and often lives in
a dotfiles repo, where a diff that alphabetizes and reflows the whole file is
worse than the setting is worth. `disable` takes the lines back out the same way,
so enable-then-disable returns the file byte-for-byte.

```sh
agentop configure bob enable      # write the settings (and, on macOS, the shell block)
agentop configure bob disable     # remove what enable wrote
agentop configure bob status      # report, and act on nothing
```

`enable` and `disable` show every change and ask once before writing, keep a
`.bak` of the settings file as it was first found, and take `--yes` for unattended
use. Declined — or with no terminal to ask on — they write nothing and exit **3**,
the same convention as `configure claude-code` and `configure bobshell`.
`--settings PATH` and `--config PATH` override either file.

### Trusting Cortex's CA without the keychain (macOS)

The proxy terminates TLS with a forged leaf, so Bob has to trust Cortex's bridge
CA or Cortex cannot read its requests. Bob's model calls leave from its extension
host's `fetch`, which is Node, and Node trusts `NODE_EXTRA_CA_CERTS` — but VS
Code's proxy layer hands that `fetch` an explicit CA list (Node's bundled roots
plus the keychain) whenever `http.systemCertificates` is on, and an explicit list
*replaces* Node's default store, dropping `NODE_EXTRA_CA_CERTS` with it. So on
macOS `enable` makes two changes, and neither touches the keychain:

- **`"http.systemCertificates": false`**, which leaves `fetch` on Node's default
  store. `enable` adds it only when the key is absent, and records that it did in
  `~/.cortex/bob-state.json`; a `true`, or any value you chose, is refused rather
  than overwritten. `disable` removes it only when that record says agentop added
  it and it is still `false`, so a `false` you set yourself is never taken out.
- **A block in your shell's rc file** that exports `NODE_EXTRA_CA_CERTS` for IBM
  Bob only. Started from the Dock or Spotlight, Bob has no terminal to inherit
  from, so it runs your login shell once (`$SHELL -i -l -c`) with
  `VSCODE_RESOLVING_ENVIRONMENT=1` set and copies that shell's environment into
  its extension host. The block tests that variable *and* Bob's bundle id, so a
  terminal, or another VS Code fork resolving its own environment, never sees it:

  ```sh
  # >>> cortex agentop (bob) >>>
  # IBM Bob reads this when it starts from the Dock or Spotlight, so it trusts Cortex.
  if [ "$VSCODE_RESOLVING_ENVIRONMENT" = 1 ] && [ "$__CFBundleIdentifier" = com.ibm.software.bob ]; then
    export NODE_EXTRA_CA_CERTS=/Users/you/.cortex/ca/ca.crt
  fi
  # <<< cortex agentop (bob) <<<
  ```

  The file, and every rule for editing it, is `configure bobshell`'s: `~/.zshrc`
  for zsh, `~/.bashrc` for bash, followed through one symlink, written
  atomically, and removed by `disable` only when it is exactly what `enable`
  wrote. Any other shell, a dangling link, or a deeper chain gets the block
  printed to place yourself.

A block in a file your login shell never reads fails silently — Bob just goes back
to being passed through unread — so `enable` then starts your shell the way Bob
does and says what it found. The usual miss is bash: its login shell reads
`~/.bash_profile`, not `~/.bashrc`, and the fix is to source one from the other.
`status` reports both changes from the files alone and runs nothing.

Quit Bob and reopen it from the Dock or Spotlight afterwards. A Bob started from a
terminal (`bobide`) takes that terminal's environment instead and never runs the
block.

The cost of `http.systemCertificates: false` is that Bob's extensions stop reading
the keychain. Everything Cortex decrypts is unaffected — Cortex presents its own
certificate — but a host Cortex passes through unread whose certificate chains to
a root trusted *only* in the keychain (some company-internal CAs) fails from Bob's
extensions. Bob's own UI and webviews are Chromium, trust only the keychain, and
are not affected by any of this: they stay passed through unread.

Earlier versions of `enable` printed a `sudo security add-trusted-cert` command
for the System keychain instead. `disable` still prints its undo — two commands,
because `delete-certificate` alone leaves the admin-domain trust settings the add
wrote:

```sh
sudo security remove-trusted-cert -d ~/.cortex/ca/ca.crt
sudo security delete-certificate -c authbridge-tls-bridge-ca \
  -t /Library/Keychains/System.keychain
```

Off macOS, `enable` prints how to add `ca.crt` to the system trust store — the
usual Debian and Fedora routes, saying plainly that the step depends on the
distribution — because that needs `sudo` and agentop does not escalate on its own.
It is `ca.crt`, the single bridge CA, and deliberately **not** the `bundle.crt`
beside it, which holds every platform root as well.

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

That is not a record agentop keeps: ownership of `http.proxy` is re-decided from
the value each time. `http.systemCertificates` is the exception — see below.

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

A missing `settings.json` is normal. Bob writes the file only the first time a
setting changes, so a Bob still on its defaults has none. What says Bob has run is
its `User` directory, which it creates on first launch: with the directory there,
`enable` creates the file and `status` reports Bob as not configured. With the
directory missing too — a `--settings` typo, or the wrong machine — `enable`
refuses and names the flag, and `status` reports the Cortex status as unknown. A
file holding only `null` is refused the same way. `disable` never fails on any of
these: there is simply nothing to remove from that file.

Only macOS's settings location is known (`~/Library/Application Support/IBM
Bob/User/settings.json`). Elsewhere `--settings PATH` is required rather than
guessed — writing a proxy setting into a file nothing reads is a silent no-op,
which is worse than a refusal that names the flag.

## Subagents and subtasks

Bob can start child tasks. A subagent runs beside its parent. A subtask runs in
place of its parent until it ends. Cortex files every child in its root task's
session, so one Bob run shows as one session.

### What Bob sends

- Every chat request has `X-Task-Id`, the task's own id.
- A child's request also has `X-Parent-Task-Id`. Every child names the root
  task. A child of a subtask also names the root task.
- The `router` and `security` calls send no task id.

### What Cortex does

- A child's rows go in the root's session. The session id is the
  `X-Parent-Task-Id` value. A root task keeps its own `X-Task-Id` as the id.
- Each row of a child has `subagent` set to the child's own task id.
- Each row of a child has `subagentKind` set to one of these values:
  - `subtask`, when the request's tool list has `end_subtask`.
  - `subagent`, in all other cases. This includes a pipeline with no inference
    parser.
- A request whose `X-Parent-Task-Id` is the same as its `X-Task-Id` is a root.
- The cost of a child is part of the root's cost, as in Bob.

The two headers come from the client and Cortex does not authenticate them.
Cortex uses them only to group rows, never to identify a principal.

### What agentop shows

- Each subagent is a block. The block is collapsed at first.
- Each subtask is a section with the label `subtask <id>`, at its place in the
  conversation. The section is open at first. Push Enter to collapse it.
- The timeline has one agent row for each child. A subtask row has the label
  `subtask <id>`.

### Limits

- A child of a subtask shows as a child of the root, because the wire names
  the root.
- Archived child sessions from before this change stay as separate sessions.
- A call with no task id, such as a `router` call, stays in the root's session
  with no `subagent` id.

## The shell half: the rc-file block

`agentop configure bob enable` also makes `bob` at a prompt run through Cortex,
by appending a delimited block to your shell's rc file (the block and the
`--yes` / exit-3 conventions are in the [README's bob section](../../cmd/agentop/README.md#routing-the-ibm-bob-editor-through-cortex-agentop-configure-bob)).
What the write itself obeys:

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
