# Reorder laptop-service.md for daily use (#1942) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Put the daily sections of `docs/laptop-service.md` first and the internals last, with no lost text and no broken anchor.

**Architecture:** One docs task. Move whole sections. Change heading levels only where this plan says. Replace three duplicate passages with links.

**Tech Stack:** Markdown. `scripts/dev/check-doc-links.py` checks links and anchors. One Go test reads the doc.

**Spec:** issue #1942 (`gh issue view 1942 --repo rossoctl/cortex`). Parent #1937.

**Base:** `61fe8b1df8cdb720585be1223bca6ea79bd7d0f8` (main). Sub-issue #1938 is merged on this base: `docs/troubleshooting.md`, `docs/getting-started.md` and `scripts/dev/check-doc-links.py` exist.

## Global Constraints

- Write new or changed sentences in ASD-STE100 prose mode: one idea per sentence, at most 25 words, active voice, no semicolons, no phrasal verbs, no marketing adjectives.
- Cortex is the product and the binaries. AuthBridge only for frozen artifact names (root `CLAUDE.md`, "Naming").
- Keep the text of every heading, so its anchor stays the same, except where this plan says.
- Keep the title "Re-running the installer is safe". `cmd/agentop/cmd_service.go:455` names it.
- `cmd/agentop/cmd_claudecode_test.go` `TestManualUninstallDocListsEveryManagedKey` requires that `laptop-service.md` names every key in `managedKeys` (`HTTPS_PROXY`, `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`, `GIT_SSL_CAINFO`, `REQUESTS_CA_BUNDLE`, `CURL_CA_BUNDLE`, and the rest of the list in `cmd/agentop/cmd_claudecode.go`). It also forbids the strings "the three Cortex keys" and "three keys". So the key list in "If `agentop` is already gone" stays.
- `core/redact/headers_test.go` pins the examples `Idempotency-Key`, `Sec-WebSocket-Key` and `accessToken` that the doc gives. Keep that paragraph as is.
- Do not state counts that drift.
- PR title: `docs: Reorder laptop-service.md for daily use`. Sign off each commit (`git commit -s`). End the commit message and PR body with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`.

## Decisions made for the issue's open points

1. **"Three ways to turn it off": keep the title.** The issue allows either a rename or a one-sentence pointer. Keep the title so no inbound link changes. Add one sentence as the last part of the section: "The third way is to remove Cortex, as [Remove it](#remove-it) says."
2. **The two pointer stubs that #1938 left** ("Why traffic disappears from `agentop`" and "Install and TLS bridge problems") are not in the issue's order. The acceptance says every section stays once. Keep both as H2, unchanged, at the end of the file, after "How it works". Reason: they are pointers only, and the new line under the H1 already sends readers with problems to Troubleshooting.
3. **The praxis text in the intro.** The issue moves "the praxis three-process text" to "The supervisor". Move these intro paragraphs there too, in their current order: the praxis process paragraph, the Linux systemd `--supervise` paragraph. Move the rest of the praxis intro text (from "To turn praxis mode on, run the installer with `--praxis`" to the end of the source-build code block) to a new H3 **"Praxis mode"** under "Configuration", after "The `tls_bridge:` configuration". Reason: turning a mode on is configuration, not internals. No link targets this text today, so the new heading breaks nothing.
4. **The CA "extends or replaces" text.** Delete the paragraph in "Unwire Claude Code" that starts "There are several CA variables". Put in its place one sentence: "[Running one command through Cortex](agents/any-agent.md#running-one-command-through-cortex-agentop-exec) tells why `NODE_EXTRA_CA_CERTS` gets `ca.crt` and the other CA variables get `bundle.crt`." The one place that keeps it in this file is the safety warning in "If `agentop` is already gone" ("unlike `NODE_EXTRA_CA_CERTS` each of them *replaces*..."). Reason: that warning tells the user why the order of the steps matters.
5. **The Claude Code undo-by-hand duplicates.** In "Unwire Claude Code", delete the paragraph that starts "It also leaves alone any one of those keys whose value you have **changed since enable set it**". Put in its place one sentence: "[The Claude Code guide](agents/claude-code.md) tells which keys `disable` leaves, and why." Keep the key list and procedure in "If `agentop` is already gone": `docs/agents/claude-code.md` "Undoing it by hand" links to it as "the full procedure", and the Go test pins the keys there. Link the file with no anchor: `**The record.**` in claude-code.md is bold text, not a heading, so `#the-record` does not resolve in `scripts/dev/check-doc-links.py`.
6. **Stale position words.** After the move, fix each "above", "below" and "see below" that points the wrong way. Known cases:
   - "How traffic is grouped into sessions": "`session.max_sessions` of them (see below)". The target is now above. Change to a link to `#session-history-is-kept-in-cortexsessions`.
   - "Remove it", "Check nothing is left": "if you followed the Go-tools step above". That step moved in #1938. Change to "if you followed [Go tools on macOS](troubleshooting.md#go-tools-on-macos-are-passed-through-unread)".
   - The intro line "Everything below is `agentop service`" stays true.
   - Check each other match of `grep -n 'above\|below' docs/laptop-service.md` against its new position.
7. **The `kill` warning.** The intro keeps the bold "**Never use `kill` or `pkill` to stop it.**", the first reason sentence ("The proxy is supervised, so killing it gets it restarted within a couple of seconds, which looks like a process refusing to die.") and "`agentop service stop` is the stop that works." The text from "That restart is the point, though" moves to "The supervisor".

## Review Focus

1. An anchor that another doc links goes missing. The link checker must pass with zero errors.
2. A sentence is lost in the move. Compare a word diff (Task 1, Step 5).
3. A position word ("above", "below") points the wrong way after the move.
4. A heading level is wrong: a H4 left under the promoted "Remove it", or a demoted section that keeps a H3 child as H3.
5. A code fence breaks during the move. Lines that start with `#` inside fences are shell comments, not headings.

---

### Task T1: Reorder `docs/laptop-service.md`

**Depends on:** none.

**Files:**
- Modify: `docs/laptop-service.md`
- Test: `scripts/dev/check-doc-links.py`, `cmd/agentop/cmd_claudecode_test.go` (`TestManualUninstallDocListsEveryManagedKey`), `core/redact/headers_test.go` (`TestCredentialName_DocumentedExamples`). Do not change these files.

**Estimated counted lines:** about 800 (moved lines count twice).

**Target outline** (H1, then H2 in this order; `-` is H3, `--` is H4):

```
# Running Cortex: start, stop, and remove it
  pointer line (Step 1), intro command block, doctor/uninstall paragraph, kill warning (Decision 7)
## Is it working?
## Start and stop
## Three ways to turn it off
- Pause it
- Unwire Claude Code            (Decisions 4 and 5; then the Decision 1 sentence ends the section)
## Remove it                     (was H3)
- Check nothing is left          (was H4)
- If you reinstall afterwards, restart your agents   (was H4)
- If `agentop` is already gone   (was H4)
## Re-running the installer is safe
## Automatic updates
## Linux: staying up after you log out
## Where your data lives         (new)
- Cost history is written to `~/.cortex/cost`
- Session history is kept in `~/.cortex/sessions`
-- Importing history from before Cortex
-- Clearing it
- Plugin state is kept in `~/.cortex/plugin-state.json`
## Configuration                 (new)
- The `memory:` configuration
- The `tls_bridge:` configuration
- Praxis mode                    (new, Decision 3)
## How it works                  (new)
- The supervisor                 (new, Decision 3 and 7)
- How traffic is grouped into sessions
- The cpex build
## Why traffic disappears from `agentop`   (stub, unchanged)
## Install and TLS bridge problems         (stub, unchanged)
```

Each new H2 ("Where your data lives", "Configuration", "How it works") gets one short STE sentence under it that says what its sections hold. Any H3 inside a section that becomes H3 becomes H4.

- [ ] **Step 1: Add the pointer line.** Under the H1, as its own paragraph before the intro text: `New to Cortex? Start with [Getting started](getting-started.md). For problems, see [Troubleshooting](troubleshooting.md).`

- [ ] **Step 2: Move the sections** to the target outline. Move whole blocks. Apply Decisions 1, 3, 4, 5 and 7. Change no other sentence.

- [ ] **Step 3: Fix the position words** (Decision 6).

- [ ] **Step 4: Check the heading order.**

Run: `awk '/^(```|~~~)/{f=!f} !f && /^#{1,4} /' docs/laptop-service.md`
Expected: the target outline, with the levels shown.

- [ ] **Step 5: Check that no text is lost.**

Run: `git diff --word-diff=porcelain 61fe8b1df8cdb720585be1223bca6ea79bd7d0f8 -- docs/laptop-service.md | grep '^-' | grep -v '^---' | sort > /tmp/1942-del.txt; git diff --word-diff=porcelain 61fe8b1df8cdb720585be1223bca6ea79bd7d0f8 -- docs/laptop-service.md | grep '^+' | grep -v '^+++' | sed 's/^+/-/' | sort > /tmp/1942-add.txt; comm -23 /tmp/1942-del.txt /tmp/1942-add.txt`
Expected: only the words of the deleted duplicates (Decisions 4 and 5), the old "Go-tools step above" and "see below" words, the moved `kill` text, and heading markers. Write each changed sentence, and why, into the commit message so the PR body can list it.

- [ ] **Step 6: Run the gates.**

Run: `python3 scripts/dev/check-doc-links.py && pre-commit run --files docs/laptop-service.md docs/superpowers/plans/2026-11-17-laptop-service-order-1942.md && (cd cmd/agentop && GOWORK=off go test -run 'TestManualUninstallDocListsEveryManagedKey' .) && (cd core && GOWORK=off go test -run 'TestCredentialName_DocumentedExamples' ./redact/)`
Expected: the link checker reports no errors, every hook passes, both tests PASS.

- [ ] **Step 7: Commit.**

```bash
git add docs/laptop-service.md
git commit -s -F - <<'EOF'
docs: Reorder laptop-service.md for daily use

<list each changed sentence and its reason>

Fixes #1942

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
EOF
```

## PRs

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `docs: Reorder laptop-service.md for daily use` | none | T1 | 800 |

One PR: the total is under 2,000 counted lines.

## Gate check (plan review)

The gate has no format or vet step. It has a link check, pre-commit and two Go tests, so the plan review ran none of them. The plan review confirmed with the link checker's `slugs()` logic that `#running-one-command-through-cortex-agentop-exec` resolves in `docs/agents/any-agent.md` and that `#the-record` does not resolve in `docs/agents/claude-code.md` (Decision 5).

## For a human after review

None. The issue says so too.
