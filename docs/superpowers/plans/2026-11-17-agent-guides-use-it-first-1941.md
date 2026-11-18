# Agent guides: "Use it" first for OpenCode, Bob and Pi (#1941) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rewrite `docs/agents/bob.md`, `docs/agents/opencode.md` and `docs/agents/pi.md` to the agent-guide template. Move the per-agent configure sections out of `cmd/agentop/README.md` into those guides.

**Architecture:** Docs, plus one Go test edit and two Go comment edits. Each guide gets the template H2 sections in order. Existing detail moves down, unchanged, into "How it works (for developers)". No CLI change.

**Tech Stack:** Markdown. Gate: `scripts/dev/check-doc-links.py`, pre-commit, and `go vet` / `gofmt` / tests in `cmd/agentop`.

**Spec:** issue #1941 (`gh issue view 1941 --repo rossoctl/cortex`). Parent: #1937. Base: `main` at `61fe8b1df8cdb720585be1223bca6ea79bd7d0f8`.

## Decisions made for this plan

1. **Base on `main`, not on PR #1949.** The issue says "start after #1939 merges". #1939 is two PRs. #1948 (the README reorder) merged. #1949 is open and approved. It moves the Bob, OpenCode and Pi sections under a new `## Per-agent configure` H2 after "Plugin dependencies". Reason: the root `CLAUDE.md` says to branch from `main`. A stack on #1949 would put its diff into this PR. The conflict is small and mechanical (see decision 2 and "For a human after review").
2. **Put `## Per-agent configure` where #1949 puts it.** That is the place of today's H2 "Typing `bob` instead of `agentop exec -- bob` ...", after "## Plugin dependencies". Reason: then both PRs change the same region the same way, and a rebase keeps one heading.
3. **`laptop-service.md` needs no change.** Task 5 of the issue asks to check that `agentop uninstall` removes the Pi pieces. It does: `planRemovePiShell` and `planRemovePiExtension` in `cmd/agentop/cmd_uninstall.go` (line 362 runs them). `TestUninstall` at `cmd/agentop/cmd_uninstall_test.go:1635` pins it. `docs/laptop-service.md` → "Remove it" already says "the `bob` and `pi` shell functions and Pi's session extension". Reason: no edit, and no conflict with open PR #1951, which reorders that file.
4. **Do not edit `.github/spellcheck/.wordlist.txt`.** The moved text is already in the repository, so its words already pass. Reason: three parallel tasks that append to one file make merge conflicts. If CI spellcheck fails, the PR fix round adds the word.
5. **One PR.** The estimate is about 1,660 counted lines, under 2,000. Moved text counts twice (issue rule 8), and the task estimates include that.

## Global Constraints

- Write in ASD-STE100 prose mode: one idea per sentence, at most 25 words, active voice, no semicolons, no phrasal verbs, no marketing adjectives. Moved text is moved unchanged, except for the changelog rule below.
- Say **Cortex** for the product. Say **AuthBridge** only for a frozen artifact name (root `CLAUDE.md`, "Naming").
- Do not state counts that drift ("nine variables", "six keys") in new text. Show the list, or link to it. Moved text keeps its words.
- The first paragraph and "Use it" do not mention pruning, cost reduction, token savings, `agentop tools scan`, `--write` or tool-prune. Showing spend is fine.
- Do not delete detail. Move it to "How it works (for developers)" as H3 sections (H4 under them). Keep each moved heading's text, so its anchor stays valid.
- Remove changelog wording ("as of the same change", "previously", "earlier versions", "used to", "until 2026-10-15", "before this change", "now"). Say what is true now. Remove a fact only when it is no longer true. A dated test record in "Verified depth" may keep its date. List each removed line in the task's commit message, so the PR body can list it.
- "Use it" is at most 25 lines, from its heading to the next H2, blank lines included.
- Each task changes only its own files (table below). Do not edit `docs/troubleshooting.md`, `docs/laptop-service.md`, `claude-code.md`, `codex.md` or `any-agent.md`.

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

None of the three guides has a "Switching inference servers" section today. Do not add one.

### Anchors that other files link to (keep these heading texts)

| Anchor | Linked from |
|---|---|
| `opencode.md#ca-trust` | `docs/troubleshooting.md:196`, `opencode.md:319` |
| `opencode.md#known-issues` | `docs/troubleshooting.md:405`, `opencode.md:272` |
| `opencode.md#undoing-it-by-hand` | `docs/laptop-service.md:933` |
| `opencode.md#enable-and-revert` | `opencode.md:382` |
| `pi.md#known-issues` | `docs/troubleshooting.md:406` |
| `bob.md#what-it-knows-and-what-it-does-not` | `docs/troubleshooting.md:407` |
| `bob.md#which-file-it-writes` | `pi.md:55` |

Rule for "Enable and revert": the template H2 "Use it" replaces it at the top. The detail under it moves into "How it works" as `### Enable and revert`, so the anchor stays valid.

Rule for "What Cortex shows for it" (opencode.md, pi.md): rename the H2 to `## What you see`. No file links to `#what-cortex-shows-for-it` in these two guides.

## Review Focus

- A reader who follows only "Use it" must finish. Each step names a real command. The facts are in each task, checked in the code.
- A moved fact can be lost. Run the word diff in each task's verify step, and account for every removed line.
- The facts in the README sections that bob.md does not state yet must land in bob.md. Most of the Bob shell section (the rc block, `--yes`, exit 3, "one agent with two faces") is in the README only.
- `TestOpenCodeDocs_AFailedRestartNamesTheCommandAgentopPrints` reads `opencode.md`. Each sentence that matches `restart (that )?fails` must name `` `opencode service restart` ``, and at least one such sentence must stay.
- Changelog wording can survive inside moved text: `grep -nE 'as of the same|previously|earlier version|used to|until 20|before this change|briefly' <file>`.

---

## Files

| File | Task |
|---|---|
| `docs/agents/bob.md` | T1 |
| `docs/agents/opencode.md` | T2 |
| `docs/agents/pi.md` | T3 |
| `cmd/agentop/README.md` | T4 |
| `cmd/agentop/cmd_opencode_test.go` | T4 |
| `cmd/agentop/cmd_bobshell_test.go` (comments only) | T4 |

The README text that T1, T2 and T3 merge is on the base. Read it with `git show 61fe8b1df8cdb720585be1223bca6ea79bd7d0f8:cmd/agentop/README.md`. On the base:

- lines 226-393: `## Routing the IBM Bob editor through Cortex (`agentop configure bob`)`, with H3s "Trusting Cortex's CA without the keychain (macOS)" and "What it knows, and what it does not"
- lines 394-412: `## Routing OpenCode through Cortex (`agentop configure opencode`)`
- lines 413-427: `## Routing Pi through Cortex (`agentop configure pi`)`
- lines 1975-2120: `## Typing `bob` instead of `agentop exec -- bob` (the shell half of `agentop configure bob`)`, with H3s "Which file it writes", "Symlinks" and "What `status` does and does not know"

---

### Task T1: bob.md to the template, with the README's Bob text merged

Depends on: none.

Files: `docs/agents/bob.md`. Estimate: 750 counted lines (297 base lines rewritten, plus about 150 README lines merged in).

- [ ] **Step 1: Write the first paragraph.** At most 4 sentences. Facts: IBM Bob is a VS Code fork. `agentop configure bob` routes both the Bob editor and the `bob` shell command through Cortex. Cortex files every child task in its root task's session, so one Bob run shows as one session. `agentop import --agent bob` adds Bob tasks from before Cortex.

- [ ] **Step 2: Write "## Use it".** At most 25 lines. Facts, checked in `cmd/agentop/cmd_bob.go`:
  1. `agentop configure bob enable`. One command does both halves: the editor's `settings.json` and a `bob` function in `~/.zshrc` or `~/.bashrc`. It shows every change and asks once. `--yes` skips the question.
  2. macOS: quit IBM Bob and reopen it from the Dock or Spotlight. A Bob started from a terminal takes that terminal's environment and does not read the CA block (`cmd_bob.go:1096`). Linux and other platforms: Bob's settings path is known only on macOS. Pass `--settings PATH` with the path of Bob's `settings.json`, or `enable` skips the editor half and exits 1 (`cmd_bob.go` usage text, "On a platform where IBM Bob's settings path is not known"). Then restart Bob. `enable` prints how to add the CA to the system trust store. That needs `sudo`, so you run it yourself.
  3. Open a new terminal and run `bob`. A terminal that was open before `enable` does not have the function.
  4. Check: run `agentop observe`, send one prompt, and see a row for Bob. `agentop configure bob status` shows what is set. It says `not enabled` in a terminal opened before `enable`.
  5. Undo: `agentop configure bob disable`. Quit Bob and reopen it. Open a new terminal: `bob` keeps working in shells that are already open (`cmd_bob.go:1449-1455`).

- [ ] **Step 3: Write "## What you see".** Move today's `## Subagents and subtasks` and its H3s here unchanged, as `### Subagents and subtasks` with H4s. Add no new claims.

- [ ] **Step 4: Keep "## Import history"** as it is today (lines 165-178).

- [ ] **Step 5: Write "## Known issues".** One bullet per symptom, each with a link to the H3 or H4 under "How it works" that explains it. Take the symptoms from "What it knows, and what it does not" and "What `status` does and does not know". Keep the full text in those sections. Do not copy it.

- [ ] **Step 6: Write "## How it works (for developers)".** It holds, in this order:
  - `### Routing the IBM Bob editor through Cortex (`agentop configure bob`)`: today's lines 3-38 of bob.md (JSON shape, byte splicing, the commands block, `--yes`, exit 3, `--settings` and `--config`). Add from the README: "Restart Bob afterwards: whether it re-reads a proxy change live is unverified, so the message says restart rather than guess." Under it, as H4: `#### Trusting Cortex's CA without the keychain (macOS)` and `#### What it knows, and what it does not`, from bob.md today.
  - `### Typing `bob` instead of `agentop exec -- bob` (the shell half of `agentop configure bob`)`: the README section text (base lines 1975-2120). It replaces bob.md's `## The shell half: the rc-file block`. Merge, do not copy: where bob.md and the README say the same thing, keep one copy. Remove the changelog paragraph ("The two were separate commands ... until 2026-10-15 ..."). Keep the fact that is true now: `bobshell` is not a command. Under it, as H4: `#### Which file it writes`, `#### Symlinks`, `#### What `status` does and does not know`.
  - Remove "editor above" and "editor below" cross-references. Name the section with a link.
  - Change the link at bob.md line 71 from `#the-shell-half-the-rc-file-block` to `#typing-bob-instead-of-agentop-exec----bob-the-shell-half-of-agentop-configure-bob`. Run the link checker to confirm the anchor text. It generates GitHub's slug.
  - Remove the link to `../../cmd/agentop/README.md#routing-the-ibm-bob-editor-through-cortex-agentop-configure-bob` (bob.md line 228). Point to the section in this file instead.
  - Remove "The full guide ... is in bob.md" sentences that the README carried.

- [ ] **Step 7: Verify.**

```sh
python3 scripts/dev/check-doc-links.py
git show 61fe8b1df8cdb720585be1223bca6ea79bd7d0f8:docs/agents/bob.md > /tmp/bob-old.md
git show 61fe8b1df8cdb720585be1223bca6ea79bd7d0f8:cmd/agentop/README.md | sed -n '226,393p;1975,2120p' >> /tmp/bob-old.md
git diff --no-index --word-diff=plain /tmp/bob-old.md docs/agents/bob.md | grep -E '\[-' 
awk '/^## Use it/{f=1;n=0} f&&/^## /&&!/^## Use it/{exit} f{n++} END{print n}' docs/agents/bob.md   # must print <= 25
grep -nE 'as of the same|previously|earlier version|used to|until 20|briefly' docs/agents/bob.md
pre-commit run --files docs/agents/bob.md
```

  Every removed word in the word diff is a duplicate or changelog wording. Record each removed duplicate in the commit message.

- [ ] **Step 8: Commit** with `git commit -s`. Subject: `docs: Put "Use it" first in the Bob guide`.

### Task T2: opencode.md to the template

Depends on: none.

Files: `docs/agents/opencode.md`. Estimate: 400 counted lines.

- [ ] **Step 1: Write the first paragraph.** At most 4 sentences. Keep today's facts (lines 3-12): OpenCode is a supported agent. Cortex recognises its User-Agent, groups its traffic by OpenCode's session id and parses its inference. Its traffic leaves from one per-user background service, `opencode serve --service`, and not from the `opencode` you run.

- [ ] **Step 2: Write "## Use it".** At most 25 lines. Facts, from today's opencode.md lines 14-135:
  1. `agentop configure opencode enable`. It writes the proxy and CA variables into the service's own environment (`service.json`) through the OpenCode CLI. It asks first. `--yes` skips the question.
  2. Start `opencode` as usual. If the service runs, `enable` restarts it once with `opencode service restart`. That interrupts every OpenCode session that uses it, so `enable` says so before it asks. If the service is stopped, the next `opencode` starts it with the new environment. If the restart fails, run `opencode service restart`.
  3. Check: run `agentop observe`, send one prompt, and see a row for OpenCode. `agentop configure opencode status` shows what is set and what the running service uses.
  4. Undo: `agentop configure opencode disable`. The same restart rule applies.
  - Keep one sentence that matches `restart (that )?fails` and names `` `opencode service restart` `` (the test in "Review Focus").

- [ ] **Step 3: Rename "## What Cortex shows for it" to "## What you see".** Content unchanged.

- [ ] **Step 4: Rename "## Importing history" to "## Import history".** No file links to `#importing-history`. Content unchanged.

- [ ] **Step 5: Keep "## Known issues".**

- [ ] **Step 6: Write "## How it works (for developers)".** Move down, unchanged: today's second paragraph of the intro (the service model), `### Enable and revert` (the detail from today's "Enable and revert": what `enable` writes, the restart rules, `--yes`, exit 3, `--config PATH`, `--opencode BIN`, `agentop exec -- opencode instead`), `### Undoing it by hand`, `### CA trust` and `### Verified depth`. Keep the heading texts `Undoing it by hand`, `CA trust` and `Enable and revert`.

- [ ] **Step 7: Merge the README section** (base lines 394-412). Each fact there is in opencode.md already, except the flag summary: `enable` and `disable` take `[--yes] [--config PATH] [--opencode BIN]`, and `status` takes `[--config PATH] [--opencode BIN]`. Add that one sentence to `### Enable and revert` if the text does not already say that `status` takes `--config` and `--opencode`. List the other README sentences as removed duplicates in the commit message.

- [ ] **Step 8: Verify.**

```sh
python3 scripts/dev/check-doc-links.py
git show 61fe8b1df8cdb720585be1223bca6ea79bd7d0f8:docs/agents/opencode.md > /tmp/oc-old.md
git diff --no-index --word-diff=plain /tmp/oc-old.md docs/agents/opencode.md | grep -E '\[-'
awk '/^## Use it/{f=1;n=0} f&&/^## /&&!/^## Use it/{exit} f{n++} END{print n}' docs/agents/opencode.md   # must print <= 25
(cd cmd/agentop && GOWORK=off go test -run TestOpenCodeDocs_AFailedRestartNamesTheCommandAgentopPrints .)
pre-commit run --files docs/agents/opencode.md
```

  The Go test still reads `README.md` until T4 lands. It passes on the base README, so it passes here.

- [ ] **Step 9: Commit** with `git commit -s`. Subject: `docs: Put "Use it" first in the OpenCode guide`.

### Task T3: pi.md to the template

Depends on: none.

Files: `docs/agents/pi.md`. Estimate: 150 counted lines.

- [ ] **Step 1: Write the first paragraph.** At most 4 sentences. Facts from today's lines 3-17: Pi is a supported agent. Cortex recognises its User-Agent, gives each Pi session its own row, and parses and prices its inference on the API formats Cortex reads. `agentop configure pi` routes it through Cortex. Move "Two facts shape it" to "How it works".

- [ ] **Step 2: Write "## Use it".** At most 25 lines. Facts, checked in `cmd/agentop/cmd_pi.go` (lines 120, 226, 250, 252, 320):
  1. `agentop configure pi enable`. It adds a `pi` shell function and Pi's session extension, under one confirmation. `--yes` skips it.
  2. Open a new terminal, then run `pi` as usual. It needs Cortex running. A Pi that runs already does not go through Cortex: quit it and start it in the new terminal.
  3. Check: run `agentop observe`, send one prompt, and see a row for Pi. `agentop configure pi status` shows what is in place.
  4. Undo: `agentop configure pi disable`, then open a new terminal.

- [ ] **Step 3: Rename "## What Cortex shows for it" to "## What you see".** Content unchanged.

- [ ] **Step 4: Keep "## Import history" and "## Known issues".**

- [ ] **Step 5: Write "## How it works (for developers)".** Move down, unchanged: "Two facts shape it", `### Enable and revert` (the detail: what `enable` writes, the extension text, ownership, `agentop uninstall`, `status`), `#### Without the shell function`, `#### Undoing it by hand`, `### CA trust`, `### Verified depth`. The "Verified depth" lines "Before this change" and "from before this change" are changelog wording. Rewrite them to say what was true for the run, for example "With the extension alone, Pi got its own session ..." and "The live run used a Cortex release without the `pi` agent name".

- [ ] **Step 6: Merge the README section** (base lines 413-427). Each fact there is in pi.md already (`[--yes]`, fixed text, the extension path, `NODE_EXTRA_CA_CERTS`). Move only a fact that is not. List the duplicates in the commit message.

- [ ] **Step 7: Verify.**

```sh
python3 scripts/dev/check-doc-links.py
git show 61fe8b1df8cdb720585be1223bca6ea79bd7d0f8:docs/agents/pi.md > /tmp/pi-old.md
git diff --no-index --word-diff=plain /tmp/pi-old.md docs/agents/pi.md | grep -E '\[-'
awk '/^## Use it/{f=1;n=0} f&&/^## /&&!/^## Use it/{exit} f{n++} END{print n}' docs/agents/pi.md   # must print <= 25
pre-commit run --files docs/agents/pi.md
```

- [ ] **Step 8: Commit** with `git commit -s`. Subject: `docs: Put "Use it" first in the Pi guide`.

### Task T4: Replace the README's per-agent sections with one-line links

Depends on: none. (It removes text. T1-T3 read that text from the base commit, not from the worktree.)

Files: `cmd/agentop/README.md`, `cmd/agentop/cmd_opencode_test.go`, `cmd/agentop/cmd_bobshell_test.go`. Estimate: 360 counted lines (about 350 README lines deleted, 6 added, small Go edits).

- [ ] **Step 1: Delete** the four README sections listed under "Files" (base lines 226-427 and 1975-2120), with all their H3s.

- [ ] **Step 2: Add the list** in the place of the Bob shell section, after "## Plugin dependencies" (decision 2):

```markdown
## Per-agent configure

- `agentop configure bob`: [docs/agents/bob.md](../../docs/agents/bob.md)
- `agentop configure opencode`: [docs/agents/opencode.md](../../docs/agents/opencode.md)
- `agentop configure pi`: [docs/agents/pi.md](../../docs/agents/pi.md)
```

- [ ] **Step 3: Fix references** to the removed sections in the README. Search: `grep -nE 'bob|opencode|OpenCode|\bpi\b' cmd/agentop/README.md`. A sentence such as "the editor above" or a `#routing-...` / `#typing-bob...` link must point to the guide instead. No other file links to these anchors on the base (checked: only `docs/agents/bob.md:228`, which T1 fixes).

- [ ] **Step 4: Change the test** `TestOpenCodeDocs_AFailedRestartNamesTheCommandAgentopPrints` in `cmd/agentop/cmd_opencode_test.go` (line 1123). The README no longer says what a failed restart does. Change the list to `[]string{filepath.Join("..", "..", "docs", "agents", "opencode.md")}`. Change the comment above it (line 1100) so that it says the test reads `docs/agents/opencode.md`. Keep the history sentence about the README only if it is still useful. Remove a now-unused import, if any.

- [ ] **Step 5: Change two comments** in `cmd/agentop/cmd_bobshell_test.go`: line 229 "README.md commits to 0644 by name" and line 236 "README.md promises this". Change `README.md` to `docs/agents/bob.md`. Leave line 1519: it records history.

- [ ] **Step 6: Verify.**

```sh
python3 scripts/dev/check-doc-links.py
grep -nE 'Routing the IBM Bob|Routing OpenCode|Routing Pi|Typing `bob`' cmd/agentop/README.md   # prints nothing
(cd cmd/agentop && GOWORK=off go vet ./... && test -z "$(gofmt -l .)" && GOWORK=off go test -run 'TestOpenCodeDocs|TestREADME' ./...)
pre-commit run --files cmd/agentop/README.md cmd/agentop/cmd_opencode_test.go cmd/agentop/cmd_bobshell_test.go
```

- [ ] **Step 7: Commit** with `git commit -s`. Subject: `docs: Replace the per-agent configure sections with links`.

---

## PRs

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `docs: Put "Use it" first in the OpenCode, Bob and Pi guides` | none | T1, T2, T3, T4 | 1660 |

The PR body lists the duplicates that the tasks removed (from the commit messages), and ends with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. It says that the PR conflicts with #1949 in `cmd/agentop/README.md`, and how to resolve it (below).

## Gate

Run from the worktree root:

```sh
python3 scripts/dev/check-doc-links.py && pre-commit run --files docs/agents/bob.md docs/agents/opencode.md docs/agents/pi.md cmd/agentop/README.md cmd/agentop/cmd_opencode_test.go cmd/agentop/cmd_bobshell_test.go && (cd cmd/agentop && GOWORK=off go vet ./... && test -z "$(gofmt -l .)") && scripts/dev/go-test-shards.sh -short cmd/agentop
```

`python3 scripts/dev/check-doc-links.py` exits 0 on the base (checked 2026-11-17). CI also runs pyspelling. It is not installed here, so the gate does not run it (decision 4).

## For a human after review

- **Merge order with #1949.** If #1949 merges first, rebase this branch on `main`. In `cmd/agentop/README.md`, keep one `## Per-agent configure` H2 at #1949's position, and replace everything under it with the three list lines from T4 step 2. Delete the moved Bob, OpenCode and Pi H3s and H4s that #1949 put there. Then run the gate. Needs: T4.

## Gate check (plan review, 2026-11-17)

On the unchanged base, `(cd cmd/agentop && GOWORK=off go vet ./... && test -z "$(gofmt -l .)")` passes. The gate command is unchanged.
