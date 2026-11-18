# Agent guides: "Use it" first (#1940) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rewrite `docs/agents/claude-code.md`, `docs/agents/codex.md` and `docs/agents/any-agent.md` to the agent-guide template, so a new user reads "Use it" first.

**Architecture:** Docs only. Each guide gets the template H2 sections in order. Existing detail moves down, unchanged, into "How it works (for developers)" as H3 sections. No CLI change.

**Tech Stack:** Markdown. Gate: `scripts/dev/check-doc-links.py` and pre-commit.

**Spec:** issue #1940 (`gh issue view 1940 --repo rossoctl/cortex`). Parent: #1937. #1938 merged as PR #1945, so the link checker and `docs/troubleshooting.md` are on the base.

## Global Constraints

- Write in ASD-STE100 prose mode: one idea per sentence, at most 25 words, active voice, no semicolons, no phrasal verbs, no marketing adjectives.
- Say **Cortex** for the product. Say **AuthBridge** only for a frozen artifact name (root `CLAUDE.md`, "Naming").
- Do not state counts that drift ("six keys", "nine variables"). Show the table or the list instead.
- The first paragraph and "Use it" do not mention pruning, cost reduction, token savings, `agentop tools scan`, `--write` or tool-prune. Showing spend is fine. Move such text to "How it works".
- Do not delete detail. Move it to "How it works (for developers)" as H3 sections. Keep the old heading text for each moved H2 or H3, so that its anchor stays valid. (Exception: the template H2s below replace "Enable and revert" and "What Cortex shows for it". Fix every link to them, as each task says.)
- Remove changelog wording ("as of the same change", "previously", "earlier versions", "used to", "now", "an earlier version of this page"). Say what is true now. Remove a fact only when it is no longer true, and list each such line in the task's commit message, so the PR body can list it.
- Do not edit `docs/laptop-service.md` (sub-issue #1942 owns it).
- Do not touch opencode.md, bob.md, pi.md (sub-issue #1941).

### The template (verbatim from the issue)

```markdown
# <Agent name>

<One paragraph, at most 4 sentences: what Cortex does for this agent, and how well
it is supported.>

## Use it
1. Enable: `agentop configure <agent> enable` (or `agentop exec -- <cmd>`).
2. Start the agent: the exact command, and whether a running agent or a background
   daemon must restart. Say how to restart it.
3. Check: run `agentop observe`, send one prompt, and see a row for <agent>.
   `agentop configure <agent> status` shows what is set.
4. Undo: `agentop configure <agent> disable`, then the same restart rule.

## What you see
<What agentop shows for this agent: grouping, subagents, cost, what is missing.>

## Switching inference servers   (only if the agent supports it)
## Import history                 (only if `agentop import` supports it)

## Known issues
<Symptom → fix. Link docs/troubleshooting.md for problems shared by all agents.>

## How it works (for developers)
<Everything else, moved down without changes.>
```

"Use it" is at most 25 lines, from its heading to the next H2, blank lines included.

### Shared decisions (all tasks use these exact names)

- **CA explanation anchor.** T3 puts the one copy of the "extends or replaces" CA explanation in `any-agent.md` under `### CA variables: extend or replace`. Its anchor is `any-agent.md#ca-variables-extend-or-replace`. T1 links to it. Reason: the issue keeps it once, in `any-agent.md` → "How it works". A fixed name lets T1 and T3 run at the same time.
- **"What you see" anchor.** The H2 "What Cortex shows for it" becomes `## What you see` in claude-code.md and codex.md. Its anchor becomes `#what-you-see`. Reason: the template fixes the H2 name. The one inbound link is `docs/framework-architecture.md:989` (`agents/codex.md#what-cortex-shows-for-it`). T2 changes it to `agents/codex.md#what-you-see`.
- **"Known issues" anchors stay.** `docs/troubleshooting.md:405-406` link `agents/claude-code.md#known-issues` and `agents/codex.md#known-issues`. Keep the H2 text `Known issues`.
- **Pruning link.** `docs/using-agentop.md` does not exist on the base. Keep the link to `../../cmd/agentop/README.md#panes` in claude-code.md (line 123 today).
- **"6 of 7" in claude-code.md "Verified depth".** Keep it. It is a dated test record from before #1729, and it was true for that run.

## Review Focus

- A reader who follows only "Use it" must finish. Check that each step names a real command. The commands are in each task.
- A moved fact can be lost. Run the word diff in each task's verify step, and account for every removed line.
- An internal anchor (`#enable-and-revert`, `#codexs-background-app-server`, `#ca-trust`) can break when its section moves. The link checker catches this. Run it.
- Rule 11 text can stay in the first paragraph. The Claude Code intro says "can prune the tools it never calls" today. Move it.
- Changelog wording can survive inside moved text. Grep each file: `grep -nE 'as of the same|previously|earlier version|used to|now points|an earlier' <file>`. Each hit must be rewritten or be a dated record (Verified depth, "Before #1729").

---

## Files

| File | Task |
|---|---|
| `docs/agents/claude-code.md` | T1 |
| `docs/agents/codex.md` | T2 |
| `docs/framework-architecture.md` (line 989 only) | T2 |
| `docs/agents/any-agent.md` | T3 |

### Task T1: claude-code.md to the template

Depends on: none.

**Files:** Modify `docs/agents/claude-code.md`.

- [ ] **Step 1: Save the old text.** `git show HEAD:docs/agents/claude-code.md > /tmp/cc-old.md`.
- [ ] **Step 2: Write the intro paragraph.** At most 4 sentences. Keep: supported agent, the one Cortex is built and tested against first, User-Agent recognition, grouping by Claude Code's own session id, inference parsing. Move "can prune the tools it never calls" to "How it works".
- [ ] **Step 3: Write `## Use it`** (at most 25 lines):
  1. `agentop configure claude-code enable`. The installer's `--claude-code` flag runs the same step.
  2. Start `claude` the usual way, with no flags and no environment variables. Restart each `claude` that already runs: Claude Code reads its `env` block only at start. `claude --resume` picks the conversation back up. While enabled, Claude Code needs Cortex running. `agentop service status` says whether the proxy is up.
  3. Check: `agentop observe`, send one prompt, see a `claude-code` row. `agentop configure claude-code status` shows what is set.
  4. Undo: `agentop configure claude-code disable`, then restart `claude` again.
- [ ] **Step 4: Order the H2s:** `Use it`, `What you see`, `Switching inference servers`, `Import history`, `Known issues`, `How it works (for developers)`.
  - `What you see` is the old "What Cortex shows for it" body, with one change. Move the whole `**Subagents.**` paragraph (from `**Subagents.**` to the line before `**Typed inference.**`) to `### Subagent headers`. In its old place, leave one line: "agentop groups each subagent's rows under a collapsible header. See [Subagent headers](#subagent-headers)." Keep all other paragraphs, `**Tool pruning is supported.**` included (rule 11 covers only the intro and "Use it").
  - `Switching inference servers` keeps these paragraphs: the first one (`With more than one inference server …`), `Claude Code keeps naming the model it asked for …`, `A session is pinned on its conversation's opening turn …`, `A running session can also be **moved** …`, `Nothing fails over …`, and `**`/model` may list a server's own names.**`. Move these paragraphs, in their order, to `### Router behavior`: `Claude Code needs no setting of its own …`, `Claude Code's own model names go first …`, `The key is the server's …`, `A conversation carried to a new session id …`, and `If the server's model has a smaller output limit …`. At the end of `Switching inference servers`, add one line: "How the router handles names, keys and limits: [Router behavior](#router-behavior)." 
- [ ] **Step 5: Known issues.** Add the CA trust-failure symptom from "CA trust" ("The symptom when trust fails" paragraph, with its two troubleshooting links) as the first item. Keep the three existing items.
- [ ] **Step 6: How it works.** H3 sections, in this order, with these exact heading texts: `### What enable writes`, `### The record`, `### Undoing it by hand` (keep its link to `../laptop-service.md#if-agentop-is-already-gone`), `### CA trust`, `### Subagent headers`, `### Router behavior` (both from step 4), then `### Verified depth`. In `### CA trust`, replace the "each of those variables *replaces* its tool's trust store …" explanation with one sentence and a link to `any-agent.md#ca-variables-extend-or-replace`. Keep the macOS Go tools sentence and its link.
- [ ] **Step 7: Verify.**
  - `python3 scripts/dev/check-doc-links.py` exits 0. (It fails on the any-agent anchor until T3 lands. In that case check that the only error is that anchor.)
  - `git diff --no-index --word-diff=plain /tmp/cc-old.md docs/agents/claude-code.md | grep -E '\[-'` — every removed word is moved, rewritten as STE, or listed as no longer true.
  - `awk '/^## Use it/{f=1;next} /^## /{f=0} f' docs/agents/claude-code.md | wc -l` prints at most 24.
- [ ] **Step 8: Commit.** `git commit -s -m "docs: Put Use it first in the Claude Code guide"` with the no-longer-true lines (or "none") in the body and the `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>` trailer.

### Task T2: codex.md to the template

Depends on: none.

**Files:** Modify `docs/agents/codex.md`, `docs/framework-architecture.md:989`.

Facts from `cmd/agentop/codex_daemon.go` and `cmd/agentop/cmd_codex.go` on the base:
- The daemon is `codex app-server --managed-daemon`. The manual restart command is `codex app-server daemon restart` (`codexDaemonRestartCmd`).
- `enable` and `disable` restart a running daemon once, after they write the file. They stop the updater (`codex app-server daemon pid-update-loop`) first. With no daemon running, the next `codex` starts one that reads the new file.
- If the restart fails, the file is still changed, the command exits 0, and it prints `codex app-server daemon restart`.
- The dotenv file is `$CODEX_HOME/.env`, or `~/.codex/.env` when `CODEX_HOME` is not set.

- [ ] **Step 1: Save the old text.** `git show HEAD:docs/agents/codex.md > /tmp/codex-old.md`.
- [ ] **Step 2: Intro paragraph.** At most 4 sentences: supported agent, User-Agent recognition, inference parsing, `agentop configure codex` writes the routing into Codex's dotenv file.
- [ ] **Step 3: `## Use it`** (at most 25 lines):
  1. `agentop configure codex enable`. It asks first. `--yes` skips the question.
  2. Start Codex with `codex`. `enable` restarts a running background daemon for you. If it says the restart failed, run `codex app-server daemon restart`. A restart interrupts running turns and Codex resumes them. Link `#codexs-background-app-server` for detail.
  If `enable` prints a `kill <pid> && codex app-server daemon restart` line for the auto-updater, run that line (`reportRestartedUpdater` in `cmd/agentop/codex_daemon.go`).
  3. Check: `agentop observe`, send one prompt, see a `codex` row with tokens. A `pending:codex@…` bucket with no tokens means the daemon did not restart. `agentop configure codex status` shows what is set and whether the daemon uses Cortex.
  4. Undo: `agentop configure codex disable`. It restarts the daemon the same way.
- [ ] **Step 4: Order the H2s:** `Use it`, `What you see` (old "What Cortex shows for it"), `Import history`, `Known issues`, `How it works (for developers)`. Codex has no "Switching inference servers" H2 today. Do not add one.
- [ ] **Step 5: How it works.** H3 sections with these exact heading texts, in this order: `### What enable writes` (the variables table, and the dotenv paragraph from the intro), `### The CA goes in SSL_CERT_FILE` (the `SSL_CERT_FILE` vs `CODEX_CA_CERTIFICATE` reasoning, the "symptom is success" paragraph), `### How enable reads the file` (the dotenvy paragraph), `### The record` , `### What disable does`, `### What status says`, `### Codex's background app-server` (keep this exact text: `#codexs-background-app-server` is linked), `### Undoing it by hand`, `### CA trust` (the old H2 body, unchanged), `### Verified depth`. Replace "which an earlier version of this page recommended" with a statement of what is true now. Fix each `#enable-and-revert` link to point at the new H3 that holds the fact. On the base there is one, at codex.md:186 in "CA trust" (`see [Enable and revert](#enable-and-revert) above`, about `SSL_CERT_FILE`). Change it to `[The CA goes in SSL_CERT_FILE](#the-ca-goes-in-ssl_cert_file)` and drop "above". Also change the internal link at codex.md:534 in "Known issues", `[What Cortex shows for it](#what-cortex-shows-for-it)`, to `[What you see](#what-you-see)`.
- [ ] **Step 6: Fix the inbound link.** In `docs/framework-architecture.md:989`, change `agents/codex.md#what-cortex-shows-for-it` to `agents/codex.md#what-you-see`.
- [ ] **Step 7: Verify.**
  - `python3 scripts/dev/check-doc-links.py` exits 0.
  - `git diff --no-index --word-diff=plain /tmp/codex-old.md docs/agents/codex.md | grep -E '\[-'` — account for every removed word.
  - `awk '/^## Use it/{f=1;next} /^## /{f=0} f' docs/agents/codex.md | wc -l` prints at most 24.
  - `grep -n 'proxy.log' docs/agents/codex.md` has no hit above `## How it works`.
- [ ] **Step 8: Commit.** `git commit -s -m "docs: Put Use it first in the Codex guide"`, no-longer-true lines in the body, `Assisted-By` trailer.

### Task T3: any-agent.md to the template

Depends on: none.

**Files:** Modify `docs/agents/any-agent.md`.

- [ ] **Step 1: Save the old text.** `git show HEAD:docs/agents/any-agent.md > /tmp/any-old.md`.
- [ ] **Step 2: Intro paragraph.** At most 4 sentences: for agents with no settings file of their own (`curl`, `python`, `node`, test suites), `agentop exec` sends one child process through Cortex. Keep the link to `../../README.md#supported-agents`.
- [ ] **Step 3: `## Use it`** (at most 25 lines):
  1. `agentop exec -- <command>`, with the three existing examples (`curl`, `claude --dangerously-skip-permissions`, `bob`). Everything after `--` passes through as typed.
  2. Nothing to restart: the environment applies to that child only. Requires Cortex running and the TLS bridge enabled.
  3. Check: `agentop observe`, run the command, see its row.
  4. Undo: nothing to undo. No file changes and nothing goes to the shell.
  5. If the agent ignores `HTTPS_PROXY`: `agentop exec --print` shows the variables, and `eval "$(agentop exec --print)"` applies them to the current shell. Take this text from the existing guide (the `--print` block, old lines 77-84). The old guide has no other text on agents that ignore the proxy. Do not add any.
- [ ] **Step 4: H2s:** `Use it`, `Known issues` (the `ca.crt`-missing-before-first-start case, and the `tls_bridge.mode: disabled` refusal, written as symptom → fix), `How it works (for developers)`.
- [ ] **Step 5: How it works.** Keep `### Running one command through Cortex (\`agentop exec\`)` with the same text as the old H2 heading, so its anchor stays. Under it, H3s: `### The variables` (the variables table and the four-spellings reason), `### CA variables: extend or replace` (the one shared copy, exact heading text, see Shared decisions), `### The proxy URL is http://`, `### Where the values come from`, `### Exit status and signals`, `### --print`, `### Requirements`. Remove the changelog paragraph "`agentop configure claude-code enable` shares this requirement as of the same change …". Rewrite it as present facts: `configure claude-code enable` also refuses `tls_bridge.mode: disabled` with a `ca_dir` set, and it points its four replacing variables at `bundle.crt`. Remove "nine" from "these nine variables" (old line 28) and from the `# nine shell-quoted export lines` comment (old line 78), rule 7. Put the rewritten `configure claude-code enable` facts in `### Requirements`, after the requirement text.
- [ ] **Step 6: Verify.**
  - `python3 scripts/dev/check-doc-links.py` exits 0.
  - `git diff --no-index --word-diff=plain /tmp/any-old.md docs/agents/any-agent.md | grep -E '\[-'` — account for every removed word.
  - `awk '/^## Use it/{f=1;next} /^## /{f=0} f' docs/agents/any-agent.md | wc -l` prints at most 24.
- [ ] **Step 7: Commit.** `git commit -s -m "docs: Put Use it first in the any-agent guide"`, no-longer-true lines in the body, `Assisted-By` trailer.

## PRs

Estimated counted lines (moved text counts twice): T1 about 450, T2 about 800, T3 about 200. Total about 1450, under 2000, so one PR.

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `docs: Put "Use it" first in the Claude Code, Codex and any-agent guides` | none | T1, T2, T3 | 1450 |

The PR body lists each line that is no longer true (from the three commit bodies), and ends with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`.

## Gate

From the worktree root:

```sh
python3 scripts/dev/check-doc-links.py && pre-commit run --files docs/agents/claude-code.md docs/agents/codex.md docs/agents/any-agent.md docs/framework-architecture.md
```

The gate has no format or vet step (no Go, Rust or Python code changes). `python3 scripts/dev/check-doc-links.py` exits 0 on the unchanged base (checked 2026-11-17). pre-commit was not run here.

The CI spellcheck job (pyspelling) is not installed here. If CI rejects a new word, add it to `.github/spellcheck/.wordlist.txt`.

## For a human after review

None.
