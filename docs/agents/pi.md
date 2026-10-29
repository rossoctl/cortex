# Pi

[Pi](https://pi.dev) ([earendil-works/pi](https://github.com/earendil-works/pi)) is a
supported agent. Cortex recognises its User-Agent, gives each Pi session its own row, and
parses and prices its inference on the API formats Cortex reads. `agentop configure pi`
routes it through Cortex and takes that out again.

Two facts shape it:

- **The CA has to be in Pi's environment when it starts.** Pi is a Node program, and Node
  takes an extra CA only from `NODE_EXTRA_CA_CERTS`, read once at startup. Pi has no CA
  setting, and its `httpProxy` setting sets the proxy alone. So `enable` defines a `pi`
  shell function that starts Pi under `agentop exec`, the same approach as the `bob`
  shell function.
- **Pi sends no session id by default.** Without one, every Pi conversation lands in one
  bucket. Pi's extension API can add a header to every request, on every provider, so
  `enable` installs a small extension that sends Pi's session id as `x-session-id`.

## Enable and revert

```sh
agentop configure pi enable    # route Pi through Cortex
agentop configure pi status    # what is in place
agentop configure pi disable   # take it out again
```

Then open a new terminal and run `pi` as usual. It needs Cortex running.

**What `enable` writes**, shown together under one confirmation (`--yes` skips it):

| Piece | Where | What it does |
|---|---|---|
| Shell function | `~/.zshrc` or `~/.bashrc`, picked from `$SHELL` | `pi() { agentop exec -- pi "$@"; }` and `export CORTEX_PI=1`, between marker lines |
| Extension | `~/.pi/agent/extensions/cortex-session.ts`, or under `$PI_CODING_AGENT_DIR` | Sends Pi's session id as `x-session-id` |

The extension, as written:

```ts
export default function (pi: any) {
  pi.on("before_provider_headers", (event: any, ctx: any) => {
    const id = ctx.sessionManager?.getSessionId?.();
    if (id) event.headers["x-session-id"] = id;
  });
}
```

**Ownership.** Both pieces are fixed text, so agentop recognises them exactly:

- `enable` never overwrites. A file at the extension's path that agentop did not write is
  left alone and reported, and `enable` exits 1.
- `disable` removes only what `enable` wrote. An edited block, or one there twice, gets
  "remove it by hand between the markers" instead.
- The rc file follows the same rules as the `bob` shell function: `$SHELL` picks
  `~/.zshrc` or `~/.bashrc`, one symlink is followed, and anything else gets the block
  printed for you to place. See [bob.md's shell half](bob.md#which-file-it-writes),
  including the bash-on-macOS login-shell case.

`agentop uninstall` removes both pieces by the same rules.

**`status`** says `enabled` when both pieces are in place, and `partly enabled` with the
missing piece named when only one is. Unlike the bob function's status, it reads both
files, so it does not need a new terminal to say `enabled`. It also says whether
`CORTEX_PI` is set in the current shell. If it is not, that terminal predates `enable`,
so open a new one. As with bob, the variable is only evidence: `type pi` in a shell says
`pi is a shell function` when the function is live there.

### Without the shell function

`agentop exec -- pi` does the same as the function for one run. Put the extension in place
by hand, or run `enable` and delete the block, to keep the session ids.

### Undoing it by hand

Without `agentop`: delete the lines between `# >>> cortex agentop (pi) >>>` and
`# <<< cortex agentop (pi) <<<` in your rc file, and delete the extension.

```sh
$EDITOR ~/.zshrc                                   # or ~/.bashrc
rm ~/.pi/agent/extensions/cortex-session.ts       # under $PI_CODING_AGENT_DIR if set
```

## CA trust

Cortex's TLS bridge decrypts Pi's HTTPS with certificates forged from its own CA, so Pi
has to trust that CA. `agentop exec` sets `NODE_EXTRA_CA_CERTS` to `ca.crt`, which Node
**adds** to its own roots, so hosts the bridge does not terminate still verify. Node reads
it once at startup and Pi has no setting for it, which is why the routing is a shell
function rather than a Pi setting. Pi's `httpProxy` setting alone would send traffic to
the proxy with no CA, and the bridge would fall back to opaque tunnels.

## What Cortex shows for it

- **Agent `pi`**, with the version from `pi/1.4.0 (darwin; node/v24.3.0; arm64)`. Pi has
  its own row in the Agents pane and its own spend, and `agentop server use <name> --agent
  pi` pins its new sessions to a server.
- **One session per Pi session**, titled from its first prompt. Its id is Pi's own session
  id, the one Pi's `/session` shows and its `--session-id` flag takes.
- **Inference** on Anthropic Messages, OpenAI Chat Completions and OpenAI Responses: model,
  messages, tool calls, token counts and cost.

## Verified depth

Tested live with Pi 1.4.0 on macOS 26 (arm64), against a LiteLLM gateway on
`openai-completions`, under a scratch `HOME`:

- `configure pi enable`, then a new interactive zsh: `type pi` named the function and
  `CORTEX_PI` was set. `pi -p` with a `read` tool call ran through Cortex.
- Cortex recorded it as its own session, `pilive-0001`, titled from the prompt, with the
  model, tool call, tokens and cost. Parsing those means the bridge terminated, so the CA
  was trusted.
- `disable` returned `~/.zshrc` to its original bytes and removed the extension.
- Before this change: the extension alone gave Pi its own session even when Pi was
  started from inside a Claude Code session.

Not tested live:

- **The `pi` agent name.** The live run used a Cortex release from before this change, so
  the row showed Pi's raw User-Agent. Recognition, its version and the fold that router
  pins and the Agents pane key on are covered by unit tests (`TestParseUserAgent_Pi`).
- **Linux and bash.** Covered by unit tests only.
- **Pi's Anthropic, OpenAI and OpenRouter providers,** and Pi's interactive mode. Only
  `pi -p` on a LiteLLM gateway was run.

## Known issues

- **On an Anthropic subscription, Pi's traffic is filed under Claude Code.** Signed in to
  Anthropic with `/login` (Pro or Max), Pi sends `user-agent: claude-cli/<version>`.
  Cortex then counts that traffic and spend under Claude Code. Read from Pi's source
  (`pi-ai`'s `anthropic-messages.js`); not yet seen live. The session id still keeps each
  Pi session in its own row.
- **Some of Pi's APIs are proxied but not parsed:** `google-generative-ai`, `google-vertex`,
  `bedrock-converse-stream`, `mistral-conversations` and `pi-messages`. Requests go
  through, but no model, tokens or cost are recorded.
- **Tool results are not recorded on Chat Completions.** Tool calls are. This applies to
  every agent using that API.
- **No Timeline turns and no harvested titles.** Those come from Claude Code's own
  signals and transcripts, so Pi has host rows on the Timeline and a title from its
  traffic, the same as Bob, OpenCode and Codex.
- **A custom User-Agent is not recognised.** Pi's identity setting can replace its
  User-Agent. Its default and `short` forms are recognised; a custom string is not.
