# AGENTS.md

Instructions for AI coding agents that work in the `cortex` repo. Claude Code reads
[`CLAUDE.md`](CLAUDE.md) directly. If you are Claude Code, you can stop here.

## Read `CLAUDE.md` first

[`CLAUDE.md`](CLAUDE.md) is the agent guide for this repo. Its rules apply to every
agent, not only to Claude. Your tool loads this file and may not load `CLAUDE.md`, so
open `CLAUDE.md` and read all of it before you change anything.

Its content is not copied here for two reasons. It is longer than the 32 KiB of
instructions that Codex loads by default, so a copy would lose its last sections. And
two copies would drift apart.

Read the `CLAUDE.md` in your worktree (see below), not the top-level checkout's copy.
The top-level checkout can be behind `main`.

Some parts of `CLAUDE.md` describe how Claude Code behaves. The next section says what
applies to you in those places.

## What is different for you

- **Worktrees.** Do everything in
  [AI Assistant Instructions](CLAUDE.md#ai-assistant-instructions): fetch into
  `refs/base/<topic>`, create the worktree under `.claude/worktrees/<topic>`, and remove
  all three things you created when you finish. That location was chosen because of how
  Claude Code loads `CLAUDE.md`. Use it anyway: it is gitignored, and the teardown
  commands expect it. Skip the `git diff --no-index` step, because it exists only for
  Claude Code. Read the worktree's `CLAUDE.md` instead. If your tool does not keep a `cd`
  from one command to the next, start each command with
  `cd .claude/worktrees/<topic> &&`, or use `git -C`.
- **The local Cortex is shared, and your session may go through it.** If your agent was
  set up with `agentop configure <agent> enable`, its traffic goes through the one Cortex
  proxy on the machine. Then `agentop service restart` and `make dev-install` cut your
  own session, and every other agent's session. Read the last item in
  [AI Assistant Instructions](CLAUDE.md#ai-assistant-instructions) before you run either.
- **Nested guides.** Some directories have their own `CLAUDE.md`. The table is in
  [Nested CLAUDE.md files](CLAUDE.md#nested-claudemd-files). Claude Code loads these
  files automatically. Your tool may not. Before you change code in one of those
  directories, read its `CLAUDE.md`.
- **Attribution.** The [Commit Attribution Policy](CLAUDE.md#commit-attribution-policy)
  applies to you. Use an `Assisted-By:` trailer and never `Co-authored-by:`. Use the same
  format as the policy's Claude line, with your own tool and vendor. Write the trailer
  yourself. The `commit-msg` hook rewrites only trailers that name Claude, Cursor or
  Anthropic, and it writes the Claude line for all three. Put the same line at the end
  of the PR body. No hook checks PR bodies.

## Before you push

CI fails on the first four items. `CLAUDE.md` has the details for each one.

- Every commit is signed off with `git commit -s`:
  [DCO Sign-Off](CLAUDE.md#dco-sign-off-mandatory).
- The PR title is `<Prefix>: <Subject starting with uppercase>`:
  [PR titles](CLAUDE.md#pr-titles).
- In each Go module you changed, `go vet ./...` passes with `GOWORK=off`. If you removed
  a package or the last import of a dependency, `go mod tidy -diff` passes too:
  [Go](CLAUDE.md#go).
- New tests wait on events, not on the clock. The `check-test-timing` hook checks this:
  [Tests and time](CLAUDE.md#tests-and-time).
- `gofmt -l .` prints nothing in each Go module you changed. CI does not check this.
- Prose says **Cortex** for the product and **AuthBridge** only for the frozen artifact
  names: [Naming](CLAUDE.md#naming-cortex-is-the-product-authbridge-is-the-sidecar).
