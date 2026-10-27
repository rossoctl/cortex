# moca without a project settings.json: a Cortex example script

Status: Design approved 2026-10-23. The moca PR is deferred (see its section).

## Problem

1. moca's `mocactl sbx up` writes a project `.claude/settings.json` that denies 17 Claude Code built-in tools. The agent can then only use moca's sandbox MCP tools, which run in a remote microVM.
2. The `Read` deny also stops Claude Code from loading `CLAUDE.md` (rossoctl/moca#568). A project's own instructions never reach the model.
3. Measured with Claude Code 2.1.295: denying only `Read` in settings drops `CLAUDE.md`; denying everything else keeps it. A path-scoped `Read(//**)` deny drops it too.

## Goal

With Cortex in the path, a moca repo needs **no `.claude/settings.json` and no `.mcp.json`**. `CLAUDE.md` loads, and the model still cannot run local tools.

Out of scope:

- OpenCode (`opencode.json` is unchanged; untested);
- other agents;
- streaming each response through unchanged (see "Known costs").

## How it works

Claude Code reads `CLAUDE.md` from disk itself and puts it in the first message of every request. No tool call is involved, so with no settings deny it loads.

A Cortex script, `moca-sandbox.py`, then keeps the model away from local tools. It acts only in sessions whose tool list carries an `mcp__sandbox__*` tool.

- **Request:** remove the 17 local tools from the list the model sees. Replace the result of any local call that already ran with an error.
- **Response:** rename a local `tool_use` the model emits to `blocked__<name>`. Claude Code then answers "No such tool available" and runs nothing.

Both halves are needed:

- Claude Code runs a tool the model calls even when Cortex removed it from the list. A hidden `Read` returned the file.
- A real model did call local `Bash` once with no deny in place, mid-task.

## Evidence

Real model and real moca sandbox (Lima VM, Claude Code 2.1.309, claude-opus-5-5; a repo with no `.claude/` and no `.mcp.json`):

| Test | Result |
|---|---|
| moca's demo task, headless and interactive | Only sandbox tools used; `CLAUDE.md` rule followed |
| Six turns in one resumed session | Only sandbox tools; a request for local `Bash` refused |
| Prompt asks for local `Bash`, a subagent, local `Read` | Nothing ran |
| `ENABLE_TOOL_SEARCH=true` | Sandbox tools still listed; worked |

Failure cases (a fake LLM forcing `Bash: touch PWNED`):

| Case | Result |
|---|---|
| Healthy | `blocked__Bash`; not run |
| Cortex down | Claude Code reports "Connection refused"; nothing run |
| Script crashes in a hook, `fail_closed` slot | 502; nothing run |
| **Script fails to start** | **Slot skipped; `Bash` ran.** Fixed by Cortex item 1 |
| Old history holding a local result | Result replaced with an error |

Approval of the sandbox tools (interactive, default permission mode):

| Where the approval is | Prompts | Project file |
|---|---|---|
| Nowhere | one per tool, first use | "don't ask again" writes `.claude/settings.local.json` |
| `~/.claude.json` project `allowedTools` | ignored | none |
| `CLAUDE.md` text | ignored | none |
| Auto mode (`defaultMode: auto`) | none | none |

Auto mode is not Claude Code's default: a fresh config reports `permissionMode=default`.

## Cortex PR

One PR, about 800-1,000 added lines across about 12 files, two-thirds of it tests.

### 1. A `fail_closed` slot whose script does not load refuses requests

- Today a script that fails to start (no handshake, bad interpreter, syntax error) is skipped with a WARN, and the slot passes requests through.
- New: a slot with `fail_closed: true` remembers each script that did not load. While any is missing, `OnRequest` rejects with 502, code `plugin.script-failed`, reason `script <name> did not start`.
- The refusal covers all traffic through that chain. Nothing can tell a moca session apart while the script that does so is down.
- It clears when the script loads: on the next reload, which an edit to the script triggers, or when the file is deleted. The slot does not retry a never-started script on its own.
- `fail_closed: false` keeps today's behaviour.
- Files: `core/plugins/script/plugin.go`, `plugin_test.go`.

### 2. Slot folders added after startup are watched

- Today `cmd/cortex/main.go` passes `scriptFolders(cfg)` from the startup config to `reloader.WithExtraWatch` once. A slot added by a later reload loads its scripts, but edits in its folder are not seen until a restart.
- New: `WithExtraWatch` takes a function from config to folders. After every accepted reload the reloader adds a watch for each new folder, creating it as `Start` does.
- Folders removed from the config may stay watched until restart; a stray event only triggers a no-op reload.
- Files: `core/reloader/reloader.go`, `reloader_test.go`, `cmd/cortex/main.go`.

### 3. The default slot stays when another slot is named

- Today `addScriptSlot` appends the default `~/.cortex/plugins` slot only when the chain names no `script` entry at all. Adding a moca slot would turn the user's own scripts off.
- New: append the default slot unless the chain already has a `script` entry with an empty `dir`.
- Where it goes: **before the first `script` entry the config names**, not at the end. The pipeline requires response readers to come before response rewriters, and the moca slot rewrites responses. With the default slot after it, a user script that reads responses made the whole config fail to load (tested). With it before, it loads and local `Bash` is still blocked (tested).
- An entry with `on_error: off` still counts as naming the slot, as today.
- Files: `cmd/cortex/scripts.go`, `scripts_test.go`.

### 4. `examples/script-plugins/moca-sandbox.py`

- A new top-level `examples/script-plugins/` folder. Nothing in Cortex embeds, installs or references it at run time.
- The script is the tested spike, with these changes:
  - `SANDBOX_PREFIX = "mcp__sandbox__"`, moca's server name;
  - `LOCAL` is moca's `CLAUDE_DENY` list, with a comment naming its source;
  - the refusal text names the `mcp__sandbox__` tools;
  - sessions are remembered by id, so a request that carries no tool list is still covered.
- Tests: `tests/test_example_moca_sandbox.py`, run by CI's existing `pytest tests/`. It loads the script with the SDK on the path, as `tests/test_script_sdk.py` loads the SDK, and covers:
  - local tools are hidden only in a session carrying `mcp__sandbox__*`;
  - a local `tool_result` is replaced and marked `is_error`;
  - a local `tool_use` is renamed in SSE and in JSON, and every other byte is unchanged;
  - `mcp__sandbox__*` calls and sessions without the sandbox tools are untouched.
- Docs: an "Examples" section in `docs/script-plugin.md`, with the install steps:
  1. copy the script to `~/.cortex/plugins/moca-sandbox/`;
  2. add `{name: script, config: {dir: moca-sandbox, fail_closed: true}}` as the last outbound entry.

## moca PR (deferred)

**Deferred, 2026-10-23.** Cortex is not installed by moca by default, so `sbx up` must not require it. Undecided: how much moca knows about Cortex. Options:

- an opt-in flag that only skips the deny list and registers the MCP server, with no Cortex knowledge;
- the same flag plus one check of Cortex's pipeline API;
- the flag also installs the script and slot (the sketch below).

Today's `sbx doctor` fails its agent-config check on a repo with no deny list; any option needs doctor to accept that.

The sketch below is the third option, kept for reference. After the Cortex PR merges. It changes `sbx up`'s agent-config step and `sbx rm`'s take-back; push, binding and pull are unchanged.

1. **Check Cortex first.** Before creating anything, `sbx up` checks:
   - Cortex answers on its session API, `127.0.0.1:47601/v1/pipeline`;
   - that pipeline lists `script`;
   - `python3` runs.

   If any check fails, it stops and names the fix. `--no-agent-config` skips this, as it skips the rest of the agent config today.
2. **Install the script.**
   - Write a copy pinned to a Cortex release to `~/.cortex/plugins/moca-sandbox/moca-sandbox.py`. CI compares the copy with that release's `examples/script-plugins/moca-sandbox.py`.
   - Add the slot as the last outbound entry of `~/.cortex/config.yaml`, using mocactl's `yaml` dependency. Cortex hot-reloads it.
3. **Register the MCP server:** `claude mcp add -s local sandbox -- mocactl sbx mcp`, stored in `~/.claude.json`.
4. **Stop writing** `.mcp.json` and `.claude/settings.json`. `opencode.json` and the `CLAUDE.md`/`AGENTS.md` block are unchanged.
5. **`sbx rm`** takes back what `up` added, from the record moca already keeps in `.sbx/agent-config.json`:
   - the MCP registration;
   - the slot and the script, when no other bound repo still uses them.
6. **`sbx doctor`** replaces the deny-list check with:
   - Cortex is healthy;
   - the `moca-sandbox` slot reports `scripts loaded` of 1 or more;
   - Claude Code's `HTTPS_PROXY` points at Cortex.
7. **Approval:** no allow rule is written. `sbx up` prints that interactive users approve each sandbox tool once (or use auto mode), and that headless runs need `--allowedTools 'mcp__sandbox__*'`.
8. **Tests:** update `sbx-agent-config.test.ts`, `sbx-up.test.ts` and `sbx-doctor.test.ts`.

## Known costs and open items

- **Streams are held back for every session.** While a script declares `rewrite_response`, the forward proxy buffers every streamed response, not only moca's. Measured: first text at 15.7s against 12.9s without it, arriving all at once. A later Go plugin could rename the tool call while it streams.
- **No Cortex, no protection.** If Claude Code is not routed through Cortex, nothing hides or blocks local tools. `sbx doctor` checks the proxy setting.
- **A down script blocks all traffic.** With item 1, a `fail_closed` slot whose script does not load refuses every request, not only moca's.
- **Claude Code's own tools stay visible.** `Skill`, `Cron*`, `SendMessage` and `ScheduleWakeup` are not on moca's list. In one run `Skill` loaded a local skill.
- **Untested:**
  - OpenCode;
  - sessions over 20 turns or with compaction;
  - a destructive-looking sandbox command in auto mode;
  - subdirectory `CLAUDE.md` files, which Claude Code loads when it reads a file there locally.

## Test plan

- **Cortex PR:** unit tests per item above; `go vet`, `gofmt -l` and `GOWORK=off go test` in `core` and `cmd/cortex`; `pytest tests/`.
- **End to end, before the moca PR merges,** repeating the spike's harness:
  - the failure matrix on a fake LLM;
  - moca's demo task with a real model, headless and interactive;
  - a user script that reads responses next to the moca slot.
