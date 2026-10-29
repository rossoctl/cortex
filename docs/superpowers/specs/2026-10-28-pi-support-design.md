# Pi support (#1458)

Pi ([pi.dev](https://pi.dev), `earendil-works/pi`) gets what Claude Code, OpenCode,
Codex and Bob have: one `agentop configure pi` command, its own row in the Agents
pane, its own sessions, tokens and cost, router pins, and a guide. One PR, capped at
2k lines.

## Evidence

Measured on 2026-10-28 with Pi 1.4.0 on macOS, using LiteLLM providers on
`openai-completions`, run through `agentop exec -- pi`.

- **User-Agent:** `pi/1.4.0 (darwin; node/v24.3.0; arm64)`. #1458 says Pi's UA has no
  `/version`; that was true of earlier releases. Pi 1.4.0's `formatUserAgent` sends
  `pi/<ver> (…)` by default, `pi` in its `short` mode, and a custom string in its
  custom mode.
- **Not recognised.** The UA is not in `knownClients`, so the label is the raw UA. That
  puts Pi in the Agents pane's "Other" row, splits spend per Node version, and means
  `agentop server use <s> --agent pi` writes a pin that never matches. Seen live: a
  request stayed on its own host with `pin: new`, `not_routed`.
- **No session id by default.** Requests land in `default`, or in a Claude Code
  session when Pi is started from that session's shell (ancestry affinity).
- **A Pi extension sets one on every request:**

  ```ts
  pi.on("before_provider_headers", (e, ctx) => {
    e.headers["X-Session-Id"] = ctx.sessionManager.getSessionId();
  });
  ```

  In `pi-ai`'s `applyAuth`, `transformHeaders` runs after Pi's own header merge, on
  every API module. Live, with no `compat` block in `models.json`, Pi's requests got
  their own session (`pigap-ext1`), titled, with tokens and cost, even when Pi was
  started inside a Claude Code session. `X-Session-Id` is already in the laptop
  default `id_headers` (`SessionIDHeadersFor`), so Cortex needs no change for it.
- **The CA can only come from the environment.** Pi runs on the user's Node (≥22.19;
  24.3 here). That Node has no `tls.setDefaultCACertificates`, and Pi's undici
  `EnvHttpProxyAgent` is built without a `ca`. So `NODE_EXTRA_CA_CERTS` must be set
  before the process starts. Pi's `httpProxy` setting sets the proxy only, and only
  when `HTTPS_PROXY` is not already set.
- **Parsing:** Chat Completions requests, responses, tool calls, token counts and
  bundled cost all parse (Claude models). GLM on LiteLLM is unpriced, as it is for
  every agent.

## Design

### 1. Recognise Pi (`core/pipeline/client.go`)

Add `"pi": "pi"` to `knownClients`, with a comment giving the captured UA and its
source. The first-token rule then gives `Name: pi, Version: 1.4.0`. Short mode's bare
`pi` gives `Name: pi` with no version. A custom UA stays unrecognised, the same as
any agent.

Recognition alone fixes the Agents pane row, per-agent spend, router pins (`AgentName`
folds `pi/1.4.0` to `pi`) and header-less affinity (`AffinityName`). Tests:
`ParseUserAgent` on the captured UA and on bare `pi`, plus `IsKnownAgent("pi")`.

**Collision check:** `pi` is a two-letter product token. Matching is exact on the
first token, so `pip/…`, `pino/…` and `pi-…` do not match. `trailingKnownClient` (used when the
first token is unknown) also compares whole tokens, so a UA with a later token exactly
`pi` or `pi/<v>` would be filed as Pi. A token inside a comment, like `(pi`, would not. No such client is known; the test pins `pip/24.0` and
`pino/9.0` as unrecognised.

### 2. `agentop configure pi enable | disable | status`

New file `cmd/agentop/cmd_pi.go`. Enable makes two changes, previewed together under
one confirmation (`--yes` skips the confirmation), the same flow as `configure bob`:

| Piece | Where | Content |
|---|---|---|
| Shell function | The rc file `bobShellRCPath` picks for `$SHELL` (`~/.zshrc`, `~/.bashrc`) | A marker-delimited block: `pi() { agentop exec -- pi "$@"; }` and `export CORTEX_PI=1` |
| Extension | `$PI_CODING_AGENT_DIR/extensions/cortex-session.ts`, else `~/.pi/agent/extensions/` | The fixed 5-line handler above, under a header comment naming agentop as its writer |

Both pieces are **fixed strings**, so `disable` compares bytes instead of parsing. That
is the same rule as `bobShellBlock`'s comment, for the same reason.

**Reuse, not copy.** The Bob rc functions (`bobShellEnablePlan`, `bobShellDisablePlan`,
`apply`, `bobShellAdviseManual`, the symlink-hop rule in `bobShellResolve`/`rcTarget`)
take their block and markers from constants. Lift those into an `rcBlock{start, end,
body}` value and pass it in; Bob passes its constants and keeps its tests green with
no test changes. This is what keeps the PR smaller than Codex's (#1337).

**Ownership rules** (same as the other configure commands):

- Enable never overwrites. An extension file with other bytes at our path is a
  finding: it is left alone and reported, and enable still writes the rc block.
- Disable removes only what it wrote. An rc block that isn't byte-equal, or appears
  twice, gets Bob's "remove by hand between the markers" advice. An extension file
  with other bytes is left alone.
- A missing Pi agent directory is normal (Pi not run yet): enable creates
  `extensions/` with mode 0755 and the file with 0644.

**Status**, in both `configure pi status` and the `configure status` table:

| State | When |
|---|---|
| enabled | both pieces present and byte-equal |
| partly enabled | exactly one present; the detail names the missing one. An extension file with other bytes counts as missing, and the detail says so |
| not enabled | neither |
| uncertain | an rc file agentop won't read through (symlink chain, unreadable) |

Like Bob's row, the detail also says whether `CORTEX_PI` is set in this shell, which
tells a user whether they need to open a new terminal.

**Wiring:** a `case "pi"` in `cmd_configure.go` and Pi added to its usage text and
unknown-agent list. A row in `configureStatusFuncs`. In `cmd_uninstall.go`, a
`planRemovePi` beside `planRemoveBobShell` that removes both pieces under the same
rules.

### 3. Docs

- `docs/agents/pi.md`: setup with `configure pi`, what gets recorded, and
  `agentop exec -- pi` as the no-rc alternative. Known limitations:
  - **Anthropic subscription (OAuth):** Pi sends `user-agent: claude-cli/<ver>`, so its
    traffic is filed under Claude Code. This was read from Pi's source (`pi-ai`
    `anthropic-messages.js`); it is not yet seen live.
  - Pi's `google-*`, `bedrock-converse-stream`, `mistral-conversations` and
    `pi-messages` APIs are proxied but not parsed.
  - Chat Completions tool results are not recorded (true for every agent using that
    API).
  - No Timeline turns and no harvested titles (the same as Bob, OpenCode and Codex).
  - Verified on Pi 1.4.0 only.
- `README.md`: a supported-agents row, and Pi in the tagline and quick-start comment.
  `cmd/agentop/README.md`: a `configure pi` section.
- Fix the five "Pi (Inflection AI)" mentions (`core/session/idheader.go`,
  `core/config/config.go`, `cmd/cortex/local.go`, `docs/agents/opencode.md`,
  `docs/laptop-service.md`). They name a different product, and this Pi sends
  `X-Session-Id` only through the extension or OpenRouter's affinity format.

## Not in scope

- **OAuth attribution** (deferred 2026-10-28): needs a marker header read before the
  UA, which is a core change and can't be tested live without a subscription login.
- **Tool results for `/v1/chat/completions`**: a separate PR, because it affects every
  agent using that API.
- **Timeline turns and Pi title harvest**: no agent but Claude Code has these.

## Testing

- Unit tests modelled on `cmd_codex_test.go` and `cmd_bobshell_test.go`:
  - enable/disable round trip on rc and extension
  - enable refuses to overwrite an extension it didn't write
  - disable leaves an edited block and a foreign extension alone
  - `PI_CODING_AGENT_DIR` override
  - each status state
  - uninstall plan
- Bob's existing tests pass unchanged after the `rcBlock` lift.
- A live check under a scratch HOME (`HOME=$(mktemp -d)`, per
  feedback-scratch-home-for-configure): `configure pi enable`, a new shell, `pi -p`,
  then confirm the session row has `agent: pi` and the Pi session id. Then
  `configure pi disable` and confirm both pieces are gone.

## Size

| Part | Lines (est.) |
|---|---|
| `client.go` + test | 60 |
| `rcBlock` lift in `cmd_bobshell.go` / `cmd_bob.go` / `cmd_uninstall.go` | 80 |
| `cmd_pi.go` | 450 |
| `cmd_pi_test.go` | 600 |
| configure / status / uninstall wiring + tests | 120 |
| `docs/agents/pi.md` | 150 |
| README, agentop README, Inflection fixes | 90 |
| **Total** | **~1,550** across ~15 files |

If the implementation passes 2k lines, stop and split it: recognition and docs in
the first PR, `configure pi` in the second.
