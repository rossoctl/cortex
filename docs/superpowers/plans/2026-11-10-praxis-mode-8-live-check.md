# Praxis mode 8/8: Live check and the default decision — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Issue:** #1725 (part of tracker #1714, phase A, step 8). Steps #1718 to #1724 are closed.

**Goal:** Write the criteria for the default, run Claude Code and Codex through praxis mode on a scratch Cortex, measure it against direct mode, and record the result and the decision on #1725 and #1714.

**Architecture:** One new dev script, `scripts/dev/praxis-live-check.sh`, starts two scratch Cortex instances side by side (one direct, one praxis) under scratch HOMEs, on ports that do not touch the shared proxy. It measures interleaved latency and samples the CPU and heap of each process. The live check and the decision are GitHub comments, not code.

**Tech Stack:** bash 3.2 (macOS), curl, ps, the Go `cortex` binary, the Rust `cortex-gateway` binary.

**Spec:** `docs/superpowers/specs/2026-11-07-praxis-mode-design.md`, Section 5 "Live check" and Section 4 "Release and install" (`praxis.enabled` stays `false` until the live check is done).

## Global Constraints

- **Never touch the shared proxy.** Do not run `agentop setup`, `agentop service *`, `make dev-install` or `install.sh`. Do not use ports 47600-47606. Do not read or write the real `~/.cortex/` except to read field names and the router's server URLs. Every scratch process runs with `HOME=<scratch>/home`.
- Scratch ports. Direct instance: proxy `127.0.0.1:49700`, session API `:49701`, stats `:49702`, transparent `:49703`, health `:49704`. Praxis instance: `:49710`-`:49714` in the same order, `praxis.listen: 127.0.0.1:49715`, `praxis.egress: 127.0.0.1:49716`.
- `cortex -supervise` finds `cortex-gateway` in the directory of its own binary (`cmd/cortex/supervise.go:239`). So both binaries go in one scratch `bin/` directory.
- Credentials: read an API key into an environment variable only. Never write a key into the repository, a commit, a plan, an issue comment or a file outside the scratch directory. Delete the scratch directory at the end (`rm -rf`).
- Bash 3.2: no `mapfile`, no associative arrays, no `${var,,}`. The script must pass `shellcheck`.
- Posts go to rossoctl/cortex issues #1725 and #1714 only.
- Text in comments and the PR body: ASD-STE100 style, short paragraphs, lists over prose.

## Decisions taken in this plan

- **D-A: The criteria live in this plan and in a comment on #1725 and #1714.** Reason: the issue says "write the criteria on this issue (#1714, Gates)". This plan is the reviewed copy. Task 2 posts them before Task 3 starts.
- **D-B: The default flip is NOT in this plan.** Reason: praxis mode turns off every outbound plugin by design (`cmd/cortex/praxis.go:33`) and agentop refuses `server`, `script` and `tools` in praxis mode (`praxisRefusal`). A default laptop install uses `tool-prune` and `inference-router`. So criterion C1 fails by design today, and the decision is "direct mode stays the default". If, against this, every criterion passes, the executor records "all criteria pass" and leaves the flip to Hai: a change of the default for every user needs a human decision.
- **D-C: Two scratch instances at the same time, not one instance restarted between modes.** Reason: the issue asks for interleaved latency. Alternate requests between the two proxies in one loop, so gateway noise hits both modes equally (see the 2026-10-02 latency probe method).
- **D-D: The latency probe sends a ~1 MiB Claude-Code-shaped `/v1/messages` body with an invalid `x-api-key`.** Reason: the upstream refuses it with 401 before a model runs, so it costs no tokens, and the parsers still read the full body.
- **D-E: The script is a dev tool, not CI.** Reason: it needs real credentials and a real upstream. It stays in `scripts/dev/` so the rollback check and phase B can run it again.
- **D-F: The PR holds the script and this plan only.** The results go on the issues, not into a repository document. Reason: the issue says "record on this issue"; a second copy drifts.

## Criteria for the default (Task 2 posts these verbatim)

Praxis mode can become the default only when **every** criterion passes:

- **C1 Feature parity.** Every feature that a default laptop install has works in praxis mode: `tool-prune`, `inference-router` (`agentop server`), script plugins (`agentop script`), `agentop tools`. Status today: fails by design (D-B).
- **C2 Facts.** For the same live session, agentop shows in praxis mode: one session per agent run, the title, every timeline row, the tool call names, the subagent blocks, input/output/cache tokens equal to the `usage` in the raw capture, and cost equal to direct mode's price for those tokens. Zero rows with a `gateway_*` error kind, apart from the fail-closed step.
- **C3 Latency.** 200 interleaved pairs, ~1 MiB body. Praxis p50 minus direct p50 is at most 5 ms. Praxis p95 minus direct p95 is at most 20 ms.
- **C4 Resources.** On a long session (at least 40 inference turns and at least 2 subagents): the peak RSS of cortex plus cortex-gateway is at most 1.5 times the peak RSS of direct-mode cortex. Their CPU seconds summed are at most 2 times direct-mode cortex CPU seconds.
- **C5 Streaming.** Claude Code SSE and Codex (`chatgpt.com` `/backend-api/codex/responses`) stream frame by frame in praxis mode: the time to the first frame is within 100 ms of direct mode.
- **C6 Fail closed.** Kill `cortex-gateway`: the next agent request gets 503 with a `gateway_unavailable` row. The supervisor restarts the gateway, and requests succeed again within 10 s.

**Condition to go back to direct mode** (after a future flip): set `praxis.enabled: false` and restart when any one of these occurs:

- an agent request with no row in the session store;
- a `gateway_unavailable` row outside a Cortex or gateway restart;
- a measured p50 added latency over 20 ms;
- a cortex plus cortex-gateway RSS over 2 times direct mode;
- a parser parity test red on `main` for more than 7 days.

## Review Focus

- A scratch instance that binds a 476xx port, or a process that inherits the real `HOME`. Expect: `up` refuses to start when any 476xx port appears in the generated config, and it exports `HOME` before it starts `cortex`.
- `down` leaves `cortex-gateway` running. Expect: `down` stops the supervisor, then waits until no process from the scratch `bin/` runs (`pgrep -f <scratch>/bin/`), and kills the rest after 5 s.
- A probe pair where one request fails (connection refused). Expect: the pair is dropped and counted, and the summary prints the dropped count; it does not compute a percentile on mixed data.
- The sampler that finds no gateway pid in direct mode. Expect: the gateway column is empty for direct mode, not a failure.
- Percentiles on an even count or a count of 1. Expect: nearest-rank percentile (`ceil(p/100*n)`), pinned by `selftest`.

---

### Task 1: The live-check script

**Depends on:** none.

**Files:**
- Create: `scripts/dev/praxis-live-check.sh` (mode 0755)

**Interfaces:**
- Produces, for Task 3 (subcommands, each prints one result per line):
  - `praxis-live-check.sh build <scratch>`: copy the build form of `scripts/dev/praxis-measure.sh`. `tags=$(go -C "$root/scripts/profile-tags" run . full)`, then `(cd "$root/cmd/cortex" && GOWORK=off go build -tags "$tags" -o <scratch>/bin/cortex .)`. Then `(cd "$root/praxis" && cargo build --release --locked -p cortex-gateway)` and `cp "$root/praxis/target/release/cortex-gateway" <scratch>/bin/`. `$root` is `$(cd "$(dirname "$0")/../.." && pwd)`.
  - `praxis-live-check.sh up direct|praxis <scratch>`: writes `<scratch>/<mode>/home/.cortex/config.yaml` (the config shape of `scripts/dev/praxis-measure.sh` plus the TLS bridge: `mode: proxy-sidecar`; `listener: {roles: [forward], bind_loopback_only: true, forward_proxy_addr, session_api_addr, transparent_proxy_addr, health_addr}` with the scratch ports; `stats: {address: 127.0.0.1:<stats>}`; no `session.id_headers` (keep the default); `tls_bridge: {mode: enabled, generate_ca: true, ca_dir: <scratch>/<mode>/home/.cortex/ca}`, outbound plugins `inference-parser`, `mcp-parser`, `a2a-parser`; for `praxis` also `praxis: {enabled: true, listen: …, egress: …}`). Starts `HOME=<scratch>/<mode>/home <scratch>/bin/cortex -supervise --config <that file>` in the background, writes `<scratch>/<mode>/supervisor.pid`, logs to `<scratch>/<mode>/cortex.log`, and waits up to 30 s for `GET http://127.0.0.1:<health>/healthz` 200. Exit 1 on timeout with the last 20 log lines.
  - `praxis-live-check.sh env direct|praxis <scratch>`: prints `export` lines for an agent shell: `HOME`, `HTTPS_PROXY`, `HTTP_PROXY`, `https_proxy`, `http_proxy` (all `http://127.0.0.1:<proxy>`), `NO_PROXY=127.0.0.1,localhost`, `NODE_EXTRA_CA_CERTS=<ca_dir>/ca.crt`, and `SSL_CERT_FILE`, `CURL_CA_BUNDLE`, `REQUESTS_CA_BUNDLE`, `GIT_SSL_CAINFO` all `=<ca_dir>/bundle.crt`. Cortex writes `ca.crt` and `bundle.crt` (bridge CA plus platform roots) into `ca_dir` itself (see `agentop exec --help`); `env` exits 1 when either file is missing.
  - `praxis-live-check.sh probe <scratch> <n> <url>`: `n` pairs; in each pair the order of direct and praxis alternates. Each request: `curl -s -o /dev/null -w '%{http_code} %{time_total}' --proxy http://127.0.0.1:<proxy> --cacert <ca_dir>/bundle.crt -H 'x-api-key: invalid' -H 'anthropic-version: 2023-06-01' --data-binary @<scratch>/body.json <url>/v1/messages`. The body file is generated once: a `messages` array of user text to ~1 MiB, `model: claude-opus-4-6`, `max_tokens: 16`, `stream: true`. Writes `<scratch>/probe.tsv` (`pair mode code ms`) and prints `direct p50 p95`, `praxis p50 p95`, `delta p50 p95`, `dropped <k>`. A pair where either code is `000` is dropped.
  - `praxis-live-check.sh sample <scratch> <seconds>`: every 5 s until `<seconds>`, appends to `<scratch>/sample.tsv` the line `epoch mode proc pid rss_kb cpu_s heap_inuse_bytes` for `cortex` in each mode and `cortex-gateway` in praxis mode (`ps -o rss=,time= -p <pid>`; heap from `curl -s --noproxy '*' "http://127.0.0.1:<stats>/debug/pprof/heap?gc=1&debug=1"`, the `# HeapInuse = ` line as in `praxis-measure.sh`, cortex only; `-` when absent; cortex-gateway serves no pprof, so its heap is not measured and RSS stands for it). Find the pids with `pgrep -f "<scratch>/bin/cortex --config <scratch>/<mode>/"` (the supervised child, not the supervisor) and `pgrep -f "<scratch>/bin/cortex-gateway"`. At the end, or on SIGTERM or SIGINT (a `trap`), prints and writes to `<scratch>/sample-summary.txt`, per mode and process: peak RSS (KiB), peak heap in use (bytes), and CPU seconds used (last `cpu_s` minus first). Exit 0 after a signal.
  - `praxis-live-check.sh ttff <scratch> <n> <url>`: for C5. `n` interleaved pairs like `probe`, each a small streaming `/v1/messages` request (`max_tokens: 16`, `stream: true`) with `-H "Authorization: Bearer $ANTHROPIC_AUTH_TOKEN"`; exit 2 when that variable is empty. Time to the first line that starts with `data:` in the response, measured with `perl -MTime::HiRes=time` (macOS `date` has no milliseconds). Writes `<scratch>/ttff.tsv` (`pair mode ms`) and prints `direct p50`, `praxis p50`, `delta p50`, `dropped <k>`. A pair where either side has no `data:` line is dropped.
  - `praxis-live-check.sh down direct|praxis|all <scratch>`: see Review Focus.
  - `praxis-live-check.sh selftest`: runs the pure functions on fixed input and exits 0 only when all pass.
- Pure functions, defined before the dispatch so `selftest` can call them: `pct <p>` (reads numbers on stdin, prints the nearest-rank percentile), `cputime_s <ps-time>` (`MM:SS.ss` or `HH:MM:SS` or `D-HH:MM:SS` to seconds), `ports_ok <config-file>` (exit 1 when the file names any `:476[0-9][0-9]`).

- [ ] **Step 1: Write `selftest` first.** Cases:
  - `printf '1\n2\n3\n4\n' | pct 50` prints `2`; `pct 95` prints `4`; `printf '7\n' | pct 50` prints `7`.
  - `cputime_s 0:01.50` prints `1.50`; `cputime_s 01:02:03` prints `3723`; `cputime_s 1-00:00:01` prints `86401`.
  - `ports_ok` fails on a file with `forward_proxy_addr: 127.0.0.1:47600` and passes on one with `127.0.0.1:49700`.
- [ ] **Step 2: Run it and see it fail.** Run: `scripts/dev/praxis-live-check.sh selftest`. Expected: non-zero exit (functions not defined).
- [ ] **Step 3: Write the functions and the subcommands** as the Interfaces block says. `set -euo pipefail`. `up` calls `ports_ok` on the generated file and refuses on failure.
- [ ] **Step 4: Run the gate.** Run: `bash -n scripts/dev/praxis-live-check.sh && shellcheck scripts/dev/praxis-live-check.sh && scripts/dev/praxis-live-check.sh selftest`. Expected: exit 0, `selftest: ok`.
- [ ] **Step 5: Smoke test with no credentials.** `S=$(mktemp -d)`; `build $S`; `up direct $S`; `up praxis $S`; `probe $S 5 https://api.anthropic.com`; `sample $S 15`; `down all $S`. Expected: both `up` succeed, the probe prints 5 pairs with code 401 in both modes, `sample.tsv` has `cortex-gateway` rows only for praxis, and `pgrep -f $S/bin/` prints nothing after `down`. Then `rm -rf $S`.
- [ ] **Step 6: Commit.** `git add scripts/dev/praxis-live-check.sh && git commit -s -m "feat: Add a dev script for the praxis-mode live check"` with the attribution trailer the repository CLAUDE.md names.

### Task 2: Post the criteria before the check

**Depends on:** none.

**Files:** none (GitHub comments).

- [ ] **Step 1:** Post the section "Criteria for the default" (C1-C6 and the condition to go back) as one comment on #1725, with `gh issue comment 1725 --repo rossoctl/cortex --body-file <file>`. Start it with "Criteria for the default, written before the live check." Write the body file in the scratch directory, not in the repository.
- [ ] **Step 2:** Post a short comment on #1714 that links that comment and says it fills the two gaps under "Gates": criteria for the default, and rollback criteria.
- [ ] **Step 3:** Record both comment URLs in the Task 4 report.

### Task 3: Run the live check

**Depends on:** Task 1, Task 2.

**Files:** none in the repository. All output goes to `<scratch>`.

- [ ] **Step 1: Set up.** `S=$(mktemp -d)`, `chmod 700 $S`. `build`, `up direct`, `up praxis`. In praxis `cortex.log`, find the Warn line "praxis mode: outbound plugins are off".
- [ ] **Step 2: Credentials.** Praxis mode has no `inference-router`, so the agent must call the upstream directly. Take the first server URL under `inference-router` in the real `~/.cortex/config.yaml` and its key, into `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN` in the agent shell only. For Codex, copy `~/.codex/config.toml` and `~/.codex/auth.json` into each scratch HOME's `.codex/`. When a credential is missing, do the steps that do not need it, and mark the dependent criteria "not run" with the reason.
- [ ] **Step 3: Latency (C3).** `probe $S 200 "$ANTHROPIC_BASE_URL"`. Keep the printed summary.
- [ ] **Step 4: Long session with subagents (C2, C4, C5).** Start `praxis-live-check.sh sample $S 3600 &` and keep its pid in `SAMPLER`. In each mode, in the shell from `eval "$(praxis-live-check.sh env <mode> $S)"`, run the same Claude Code task with `claude -p --max-turns 60` that makes at least 2 subagents (prompt: "Use two Task subagents in parallel: one lists every Go module in this repository with its go.mod path, one counts the test files under core/. Then summarize both results."), run from a fresh `git clone --depth 1` of the repository inside `$S`. Then one `codex exec` task ("List the files under scripts/dev and say what each does.") in each mode. When one Claude Code run has fewer than 40 inference turns, run the same prompt again in the same session (`claude -p --continue`) in both modes until it has 40. Then, for C5, `praxis-live-check.sh ttff $S 20 "$ANTHROPIC_BASE_URL"`. For Codex, C5 uses the median `ttfbMs` of its inference rows in each mode from the session API (Codex authenticates with an OAuth token, so `ttff` cannot call its upstream). Say in the result that `ttfbMs` measures the response headers, not the first frame.
- [ ] **Step 5: Check in agentop.** For each mode: `HOME=<scratch>/<mode>/home agentop --endpoint http://127.0.0.1:<session-api>` for a visual check, and read the numbers with `curl -s --noproxy '*' http://127.0.0.1:<session-api>/v1/sessions` and `/v1/sessions/{id}` (endpoint reference: `docs/session-api.md`). Compare the two modes for C2: session count, titles, timeline rows, tool call names, subagents, tokens against the raw capture `usage` (`/v1/sessions/{id}/events/{seq}?raw=1`), cost. Record each difference. A difference that Section 5 lists as known (captured request headers, timing within 50 ms) is not a failure.
- [ ] **Step 6: Fail closed (C6).** `kill -9 "$(pgrep -f "$S/bin/cortex-gateway")"`, and at once send one request with the `probe` curl line (praxis proxy `127.0.0.1:49710`, invalid key). Expect code 503 and, in `/v1/sessions`, a row with error kind `gateway_unavailable`. Then send the same request once each 0.5 s; record the seconds from the kill to the first code other than 503 and 000. Pass when that is at most 10 s.
- [ ] **Step 7: Resources (C4).** `kill -TERM "$SAMPLER"; wait "$SAMPLER"`. Keep `$S/sample-summary.txt`. Copy it out of `$S` (into the Task 4 comment body file in a second `mktemp -d`) before Step 8.
- [ ] **Step 8: Tear down.** `down all $S`, check `pgrep -f $S/bin/` is empty, `rm -rf $S`.

### Task 4: Record the result and the decision

**Depends on:** Task 3.

**Files:** none (GitHub comments).

- [ ] **Step 1:** Post one comment on #1725: a table with one row per criterion C1-C6 (measured value, threshold, pass / fail / not run), the probe and sample summaries, the agent and Claude Code / Codex versions, the date, and every C2 difference.
- [ ] **Step 2: Decide.** Rule: the default changes only when C1-C6 all pass. Expected result: C1 fails by design, so the decision is "direct mode stays the default; praxis mode stays opt-in (`agentop setup --praxis`)". Write the decision and the criteria that block it.
- [ ] **Step 3:** Post a short comment on #1714 with the decision and a link to the result comment. Update the tracker's phase A row when the body has one for step 8.
- [ ] **Step 4:** Do not close #1725 by hand. The PR from Task 1 says `Closes #1725` in its body, so the merge closes it after the result is recorded.

## PRs

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `feat: Add a dev script for the praxis-mode live check` | "" (none) | T1, T2, T3, T4 | 340 |

Estimate: T1 is about 340 counted lines (the script with `selftest`); T2-T4 are 0. The plan document does not count.

T2-T4 change no files. They run in the P1 session after T1, and the PR body links the result comment.

## Gate

From the worktree root:

```sh
bash -n scripts/dev/praxis-live-check.sh && shellcheck scripts/dev/praxis-live-check.sh && scripts/dev/praxis-live-check.sh selftest
```

Gate check on base 412ed825 (plan review, 2026-11-10): `bash` and `shellcheck` 0.11.0 are installed and run from the worktree root. Both steps fail on the base only because Task 1 creates the file (`No such file or directory`). The gate is stricter than CI, which runs `shellcheck --severity=error` (`.github/workflows/security-scans.yaml`); keep the strict form. The existing `scripts/dev/praxis-measure.sh` passes plain `shellcheck`.

## Scope left out

- The default flip (D-B).
- The heap of `cortex-gateway`: it serves no pprof, so C4 uses its RSS and CPU only. The issue asks for "the CPU and heap of both processes"; the result comment must say this.
