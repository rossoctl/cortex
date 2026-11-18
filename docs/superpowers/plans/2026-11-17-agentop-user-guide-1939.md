# agentop user guide and reference reorder (#1939) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `docs/using-agentop.md`, a short user guide for a laptop user, and reorder `cmd/agentop/README.md` into a reference.

**Architecture:** Docs only. Three tasks in two stacked PRs. T3 (PR P1) is a pure section move in `cmd/agentop/README.md`, done by a script. T1 (PR P2) finishes the reorder and the edits in `cmd/agentop/README.md`. T2 (PR P2) creates `docs/using-agentop.md` and changes three link lines. T1 and T2 share no file, so they run at the same time. This plan pins every heading and anchor that T1 and T2 share.

**Tech Stack:** Markdown. `scripts/dev/check-doc-links.py` (from #1938, merged as PR #1945). pre-commit.

**Spec:** GitHub issue rossoctl/cortex#1939 (`gh issue view 1939 --repo rossoctl/cortex`). Parent: #1937. Read the issue body. Its "Rules for every PR in this overhaul" apply to each task.

## Global Constraints

- Language: ASD-STE100 prose mode. One idea per sentence, at most 25 words. Active voice. No semicolons. No phrasal verbs. No marketing adjectives. Invoke the `asd-ste100` skill before you write.
- Naming: **Cortex** for the product and its binaries. **AuthBridge** only for a frozen artifact (image, `authbridge-proxy` container, ConfigMap, header, env var, mode field).
- Move, do not copy. When text moves, delete it from the old place. Leave at most one pointer line.
- Facts come from the code on the base commit. Do not state counts that drift.
- No cost-reduction pitch in `docs/using-agentop.md`, `README.md` or the "New here" part of `docs/README.md`. Do not name `agentop tools scan`, `--write` or tool-prune there. Showing spend is fine.
- Keep these paths valid: `docs/pricing.md`, `docs/praxis-policy.md#move-from-the-cpex-plugin`, `docs/session-export.md`, `docs/laptop-local-model.md`, `docs/laptop-service.md`. The link checker checks them.
- No change to agentop behavior. No Go change is planned (the usage texts of `cost` and `pipeline` are correct on the base).
- Size: each PR is at most 2,000 counted diff lines. The count is `git diff --numstat <parent>...HEAD` with git's default diff algorithm, minus this plan file. Moved text counts twice. On this file the default algorithm counts a big move badly: the full reorder in one diff counts about 3,430 lines (measured), not the 1,380 of the moved text. That is why the reorder is in two PRs. Budget: T3 about 1,036 (measured). T1 at most 1,000 against T3's commit. T2 at most 280.
- Line numbers: "base line N" is line N of `cmd/agentop/README.md` on the base commit. Read it with `git show 784ce29e5e118ed463efd3b97e45b8b0b817b7e1:cmd/agentop/README.md`. After T3, the same text is at a different line. Find it by its text.
- Commits: `git commit -s`. End each message with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. No `Co-Authored-By`.

## Decisions made for this plan

1. **Issue key `p` is two keys.** The code binds `p` to pause and `P` to the Pipeline pane (`cmd/agentop/tui/keys.go`: `case "p"` toggles `m.paused`, `case "P"` opens the pipeline). The key table lists both. Reason: rule 6, facts come from the code.
2. **Catalog key is `C`.** `keys.go` `case "C"` opens the catalog. `P` was its old key.
3. **T2 does not depend on T1 or T3.** T2 links anchors that T1 creates. This plan pins the exact headings, so T1 and T2 run at the same time. Reason: T2 shares no file with T1 or T3. T2 is in P2, so T3 is below it.
4. **The mock-up moves by T1 deleting and T2 adding.** T2 copies the block from the base file. T1 deletes it. Together the PR is a move.
5. **README section moves are whole-section moves.** Do not re-wrap or re-word moved text, except the four fact fixes. Reason: moved text counts twice, and the budget is tight.
6. **Two stacked PRs.** Measured with the default `git diff --numstat` on a simulation of the final file: the reorder in one diff counts about 3,430 lines. With T3's intermediate order, P1 counts 1,036 (518 + 518) and the rest of the reorder plus the edits counts about 960. P1 = T3 (about 1,036). P2 = T1 + T2 (about 960 + 270 = 1,230), parent P1.
7. **T3's intermediate order is pinned.** The order below is the one measured. Do not change it: another order can make git count a much larger diff. P1 passes the gate alone: a pure move keeps every heading, so every anchor still resolves, and T3 adds no link to `docs/using-agentop.md`.

## Shared headings and anchors (pinned)

T1 creates these headings in `cmd/agentop/README.md`. T2 links them. Anchors are the output of `slug()` in `scripts/dev/check-doc-links.py`.

| Heading (exact text) | Level | Anchor |
|---|---|---|
| `Commands` | H2 | `#commands` |
| ``What your agents have spent (`agentop cost`)`` | H3 | `#what-your-agents-have-spent-agentop-cost` |
| ``The pipeline that runs (`agentop pipeline get`)`` | H3 | `#the-pipeline-that-runs-agentop-pipeline-get` |
| `Per-agent configure` | H2 | `#per-agent-configure` |
| `Internals` | H2 | `#internals` |
| `How CTX is derived` | H3 | `#how-ctx-is-derived` |

These headings exist on the base and keep their text (H2 becomes H3 for command sections. The anchor does not change):

| Anchor | Section |
|---|---|
| `#updating-agentop-update` | Updating |
| `#choosing-between-a-cluster-and-a-local-cortex---kubernetes` | cluster choice |
| `#panes` | Panes |
| `#keybindings` | Keybindings |
| `#which-harnesses-are-wired-into-cortex-agentop-configure-status` | configure status |
| `#running-one-command-through-cortex-agentop-exec` | exec |
| `#choosing-an-inference-server-agentop-server` | server |
| `#local-model-servers` | server, H3 (becomes H4) |
| `#importing-agent-history-agentop-import` | import |
| `#installing-a-bundled-script-plugin-agentop-script` | script |
| `#editing-the-pipeline` | Editing the pipeline |
| `#naming-sessions-from-claude-code---skip-claude-metadata` | Run subsection |
| `#routing-the-ibm-bob-editor-through-cortex-agentop-configure-bob` | Bob |

## Review Focus

1. A laptop reader of `docs/using-agentop.md` meets a cluster-only step (Namespaces, Pods, port-forward). Expect: each one is marked "cluster only". T2 step 4 checks it.
2. An inbound link from another doc to an old `cmd/agentop/README.md` anchor breaks after the reorder. Expect: the link checker passes. T3 step 2 and T1 step 7 check it.
3. A key in the guide does not match `cmd/agentop/tui/keys.go`. Expect: every key in the T2 table is in the code. T2 step 4 checks it.
4. The Sessions mock-up exists twice, or zero times, after the PR. Expect: exactly once, in `docs/using-agentop.md`. The gate cannot see this. T1 step 6 and T2 step 4 each check their half with `grep -c`.
5. A section heading text changes by accident in the move. Expect: every anchor in the second table above still resolves. The link checker checks it.

---

### Task T3: Move the README sections into the intermediate order (PR P1)

**Depends on:** none.

**Files:**
- Modify: `cmd/agentop/README.md`
- Test: `python3 scripts/dev/check-doc-links.py`

This is a pure move. It changes no line. It only changes the order of whole sections. Every range below starts at an H2 heading and ends at a blank line (checked on the base).

- [ ] **Step 1: Run the move script from the worktree root.**

```sh
git show 784ce29e5e118ed463efd3b97e45b8b0b817b7e1:cmd/agentop/README.md > /tmp/agentop-readme-base-1939.md
python3 - /tmp/agentop-readme-base-1939.md cmd/agentop/README.md <<'EOF'
import sys
L = open(sys.argv[1]).read().split('\n')
def r(a, b): return L[a-1:b-1]
new = (r(1, 226) + r(421, 623) + r(856, 1894) + r(226, 275) + r(645, 856) + r(623, 645)
       + r(1953, 2180) + r(275, 421) + r(2180, 2187) + r(1894, 1953) + r(2187, len(L) + 1))
open(sys.argv[2], 'w').write('\n'.join(new))
EOF
```

  The H2 order after the script: Install, Updating, Run, Bob editor ("Routing the IBM Bob editor…"), OpenCode, Pi, Panes, Keybindings, Settings, Where a session opens, configure status, exec, server, import, script, Editing the pipeline, Plugin dependencies, "Typing `bob`…", Trust model, How a timeline is fetched, Architecture.

- [ ] **Step 2: Check.**
  Run: `git diff --numstat -- cmd/agentop/README.md`. Expected: `518	518`.
  Run: `wc -l < cmd/agentop/README.md`. Expected: `2193`.
  Run: `python3 scripts/dev/check-doc-links.py`. Expected: exit 0.

- [ ] **Step 3: Commit.** `git commit -s -m "docs: Move the agentop README sections into reference order"` with the Assisted-By line.

### Task T1: Reorder `cmd/agentop/README.md` into a reference

**Depends on:** T3.

**Files:**
- Modify: `cmd/agentop/README.md` (2,193 lines on the base)
- Test: `python3 scripts/dev/check-doc-links.py`

**Interfaces:**
- Produces: the pinned headings in the first table above.

Base line map (H2 sections, base commit, before T3; see "Line numbers" in Global Constraints):
Install 10, Updating 26, Run 41 (subsections 76, 98, 110, 147), configure status 226, exec 259, "Typing `bob`…" 275, Bob 421, OpenCode 589, Pi 608, script 623, server 645 (Local model servers 765), import 793, Panes 856, Keybindings 1571, Settings 1793, Where a session opens 1879, How a timeline is fetched 1894, Editing the pipeline 1953, Plugin dependencies 2153, Trust model 2180, Architecture 2187.

- [ ] **Step 1: New intro (replace lines 3-8).** At most 8 lines. Say: "Reference for `agentop`. For a first tour, read [Using agentop](../../docs/using-agentop.md)." Then: it works with the local Cortex on a laptop, and with an AuthBridge sidecar in a cluster. Link `../../docs/session-api.md`.

- [ ] **Step 2: Install and Run.**
  - Install: put the curl one-liner first. Copy its exact text from `docs/getting-started.md` (the install step). Then the release download paragraph, then `go build`.
  - Run: laptop first. `agentop observe` connects to the local Cortex. Keep the deprecation and `AGENTOP_PPROF_ADDR` paragraphs. Move the Namespaces/Pods/port-forward/kubectl paragraphs (base lines 65-74) into "Choosing between a cluster and a local Cortex (`--kubernetes`)". Keep that heading text.

- [ ] **Step 3: New H2 order.** T3 already moved most sections. In this step, move only one block: the three sections "Routing the IBM Bob editor through Cortex…", "Routing OpenCode…" and "Routing Pi…" (now between Run and Panes), as one block, to directly after the "Typing `bob`…" section. Do not move any other section. Then add the three new H2s (`Commands`, `Per-agent configure`, `Internals`) and change heading levels. The target order:
  1. Install, 2. Updating, 3. Run (with its subsections), 4. Panes, 5. Keybindings, 6. Settings, 7. Where a session opens,
  8. `## Commands` with these H3s in this order: configure status, exec, server (its "Local model servers" becomes H4), import, script, `What your agents have spent (`agentop cost`)` (new), `The pipeline that runs (`agentop pipeline get`)` (new), Editing the pipeline (its H3s become H4), Plugin dependencies.
  9. `## Per-agent configure`: "Typing `bob` instead of…", Bob (with its H3s), OpenCode, Pi. Demote each one level. Change no other text, except step 5's "above".
  10. Trust model.
  11. `## Internals`: How a timeline is fetched, Architecture, `How CTX is derived` (all H3).
  Keep every heading text. Only the level changes.

- [ ] **Step 4: Write the two new sections.** Each one is at most 40 lines. Facts, from `cmd/agentop/cmd_cost.go` and `cmd/agentop/cmd_pipeline.go`:
  - `agentop cost` flags, one line each:
    - `--window`: `today` (default, from local midnight), `month` (from the 1st), `7d` (rolling seven days), or a duration such as `1h` or `6h` (from the in-memory ring).
    - `--by <axis>`: one row per label, costliest first. Axes: agent, project, model, endpoint, currency, session, status, plugin, host. The ledger windows (`today`, `month`, `7d`) serve only agent, project, model, endpoint and currency. The others need a duration window.
    - `--agent <name>`: only this agent, as the Agents pane spells it (for example `bob`).
    - `--json`: the totals as JSON.
    - `--endpoint <URL>`: ask this proxy. Default: the Cortex on this machine.
    - Note: `today`, `month` and `7d` come from the cost ledger. It is on for a local install and off in Kubernetes. When it is off, the command prints the window the proxy holds, not the one you asked for.
    - Examples: `agentop cost --window month --by agent`, `agentop cost --by model`, `agentop cost --json`.
    - Link `../../docs/pricing.md` for how cost is calculated.
  - `agentop pipeline get`: prints the active plugin pipeline as a table, inbound chain then outbound, with the application between them. Same composition as the Pipeline pane (`P`). DESCRIPTION is the last column. It has no EVENTS and DEPS columns. `--json` prints `/v1/pipeline`'s shape, with `requires`/`requiresAny`. `--endpoint <URL>` as for cost. Use it to see the pipeline that runs: the config file alone does not show the defaults. Link `#editing-the-pipeline`.

- [ ] **Step 5: Fix the wrong facts.**
  - Catalog entry (base line 1471): "opened by `P`" becomes "opened by `C`".
  - Pane intro (base lines 860-863): add Agents (`A`) to the list of surfaces, and remove "the other four" (do not state a count).
  - Base line 279: "configures the editor above" becomes "configures the editor below" (the Bob editor section stays below it).
  - CTX reset (base lines 967-972, now in "How CTX is derived"): say the `Esc`-to-Pods reset is cluster only. Say that on a laptop and under `--endpoint`, you restart agentop.
  - Move the CTX derivation text (base lines 904-978, from "`CTX(1M)` is a gauge" to the end of the reset paragraph) to `### How CTX is derived` under Internals. In Sessions, leave one line: what CTX shows and a link to `#how-ctx-is-derived`.

- [ ] **Step 6: Delete the mock-up.** Delete base lines 1002-1019 (from "Rendered at 100 columns" to the closing fence). Leave one line: "The screen is shown in [Using agentop](../../docs/using-agentop.md#the-screen)." Then run:
  `grep -c 'LAST 1H    TODAY   7 DAYS    MONTH' cmd/agentop/README.md`. Expected: `0`.

- [ ] **Step 7: Check.**
  Run: `python3 scripts/dev/check-doc-links.py`. Expected: exit 0, or failures only for `docs/using-agentop.md` and its `#the-screen` anchor when T2 is not yet in the tree.
  Run: `pre-commit run --files cmd/agentop/README.md`. Expected: all hooks pass.
  Run: `git diff --numstat "$(git log -1 --format=%H --grep='Move the agentop README sections')" -- cmd/agentop/README.md`. Expected: added + removed at most 1,000.

- [ ] **Step 8: Commit.** `git commit -s -m "docs: Reorder the agentop README into a reference"` with the Assisted-By line.

### Task T2: Add `docs/using-agentop.md` and link it

**Depends on:** none (see Decision 3).

**Files:**
- Create: `docs/using-agentop.md` (at most 260 lines)
- Modify: `README.md:122`, `docs/README.md:12`, `docs/getting-started.md` (section "Read the screen", line 67)
- Test: `python3 scripts/dev/check-doc-links.py`

**Interfaces:**
- Consumes: the pinned anchors in both tables above.
- Produces: `docs/using-agentop.md` with an H2 `The screen` (anchor `#the-screen`). T1 links it.

- [ ] **Step 1: Write `docs/using-agentop.md` with these H2s, in this order.**
  1. `Open it`: `agentop observe`. On a laptop it connects to the local Cortex. One line links `../cmd/agentop/README.md#choosing-between-a-cluster-and-a-local-cortex---kubernetes` for a cluster.
  2. `The screen`: the Sessions mock-up, copied exactly from base `cmd/agentop/README.md` lines 1005-1019 (the fenced block: `git show 784ce29e5e118ed463efd3b97e45b8b0b817b7e1:cmd/agentop/README.md | sed -n 1005,1019p`), unindented (remove the two leading spaces). Then:
     - One line per column: SESSION, TITLE, UPDATED, EVENTS, TOKENS, COST, SAVED~, CTX(1M), and SERVER (shown only when the inference router routes to more than one server). Give the unit (relative time, count, tokens, US dollars, bar against a 1M-token window). Each figure is a per-session total. SAVED~: one neutral line, "an estimate of what Cortex plugins saved this session". No pitch.
     - The `(*)` import marker: read its meaning in the base README (`grep -n '(\*)' cmd/agentop/README.md`) and give it in one line.
     - Band: one line each for LAST 1H, TODAY, 7 DAYS, MONTH. A `—` means this deployment cannot answer that period.
     - Link `../cmd/agentop/README.md#panes` and `#how-ctx-is-derived` for detail.
  3. `Panes`: one line each, with the key: Sessions (start pane), Events (`Enter` on a session), Detail (`Enter` on an event), Timeline (`T`), Usage (`u`), Agents (`A`), Pipeline (`P`), Plugin detail (`Enter` on a Pipeline or Catalog row), Catalog (`C`), Key help (`?`), Namespaces ("cluster only"). Check each key in `cmd/agentop/tui/keys.go`.
  4. `Keys`: a table of `↑↓`, `Enter`, `Esc`, `/` (search), `?` (all keys), `$` (spend drawer), `u`, `T`, `A`, `S` (choose the server for an agent's new sessions on Agents, or move a session on Sessions), `p` (pause), `P` (pipeline), `s` (hide skip rows), `q` / `Ctrl+C` (quit). Then "Press `?` for the full list." Link `../cmd/agentop/README.md#keybindings`.
  5. `Everyday commands`: a table with columns command, what it does, example, more. One row each, in this order, for: observe, setup, doctor, update, uninstall, service, configure, exec, server, script, pipeline, pricing, cost, export, import. Take each one-line summary from the usage text in `cmd/agentop/main.go` (lines 80-120). Links: setup, doctor, service, uninstall → `laptop-service.md`. pricing → `pricing.md`. export → `session-export.md`. observe → `../cmd/agentop/README.md#panes`. update → `#updating-agentop-update`. configure → `#which-harnesses-are-wired-into-cortex-agentop-configure-status`. exec → `#running-one-command-through-cortex-agentop-exec`. server → `#choosing-an-inference-server-agentop-server`. script → `#installing-a-bundled-script-plugin-agentop-script`. pipeline → `#the-pipeline-that-runs-agentop-pipeline-get`. cost → `#what-your-agents-have-spent-agentop-cost`. import → `#importing-agent-history-agentop-import`. Do not list `tools`, `experimental` or `claude-code`.

- [ ] **Step 2: Link it.**
  - `README.md:122`: `[Using agentop](./cmd/agentop/README.md#panes)` becomes `[Using agentop](./docs/using-agentop.md)`.
  - `docs/README.md:12`: `../cmd/agentop/README.md#panes` becomes `using-agentop.md`.
  - `docs/getting-started.md`, end of "Read the screen": add `More: [Using agentop](using-agentop.md).`

- [ ] **Step 3: Rule 11 check.** Run `grep -niE 'sav(e|ing)s? (money|tokens)|cut the waste|tools scan|--write|tool-prune' docs/using-agentop.md`. Expected: no output.

- [ ] **Step 4: Fact checks.**
  - For each key in the Keys table and Panes list, find its `case` in `cmd/agentop/tui/keys.go`.
  - `grep -c 'LAST 1H    TODAY   7 DAYS    MONTH' docs/using-agentop.md`. Expected: `1`.
  - Each cluster-only item says "cluster only".
  Run: `python3 scripts/dev/check-doc-links.py`. Expected: exit 0, or failures only for the T1-created anchors (`#commands`, `#what-your-agents-have-spent-agentop-cost`, `#the-pipeline-that-runs-agentop-pipeline-get`, `#how-ctx-is-derived`) when T1 is not yet in the tree.
  Run: `pre-commit run --files docs/using-agentop.md README.md docs/README.md docs/getting-started.md`. Expected: pass.

- [ ] **Step 5: Spellcheck words.** Add to `.github/spellcheck/.wordlist.txt` any new word outside code spans that aspell rejects: `aspell list --lang=en < docs/using-agentop.md | sort -u`, then compare with the word list. Add only real terms.

- [ ] **Step 6: Commit.** `git commit -s -m "docs: Add the agentop user guide"` with the Assisted-By line.

## PRs

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `docs: Move the agentop README sections into reference order` | none | T3 | 1,036 |
| P2 | `docs: Add an agentop user guide and reorder the agentop reference` | P1 | T1, T2 | 1,230 |

## Gate

From the worktree root:

```sh
python3 scripts/dev/check-doc-links.py && pre-commit run --from-ref 784ce29e5e118ed463efd3b97e45b8b0b817b7e1 --to-ref HEAD
```

The gate has no format or vet step. `python3 scripts/dev/check-doc-links.py` ran on the base from the worktree root and exited 0. pre-commit was not run in plan review.

## For a human after review

None.
