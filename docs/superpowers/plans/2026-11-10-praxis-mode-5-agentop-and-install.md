# Praxis mode 5/8: agentop and the installer — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a user turn on praxis mode with `install.sh --praxis`, keep it across `agentop update`, and make agentop refuse the commands that have no effect in praxis mode (#1722).

**Architecture:** agentop learns `praxis.enabled` from `GET /config` and from the config file. One helper (`praxisRefusal`) refuses `agentop server …`, `agentop script …` and `agentop tools scan --write`. `install.sh --praxis` downloads the `cortex-gateway` archive into the stage and passes `--praxis` to `agentop setup`. Setup then adds `praxis: {enabled: true}` to the config with a targeted line edit. The systemd unit uses `--supervise` in praxis mode. The supervisor refuses a gateway whose `--version` differs from its own.

**Tech Stack:** Go 1.26.10 (`cmd/agentop`, `cmd/cortex`, `core/observe`), POSIX sh (`scripts/install.sh`), Rust (`praxis/server`, Praxis 0.8.0).

**Spec:** `docs/superpowers/specs/2026-11-07-praxis-mode-design.md` (Section 4: "Config", "Supervision", "agentop", "Release and install"; Section 6 step 5). Issue: #1722. Earlier plans: `2026-11-09-praxis-mode-3-supervise-gateway.md` (decisions 17 and 20 defer work to this step), `2026-11-09-praxis-mode-4-hop-and-egress.md`.

## Global Constraints

- Direct mode does not change. With `praxis.enabled` false or absent, every existing test passes with no change to its expectations.
- `make dev-install` and `agentop setup` work without `cortex-gateway` while `praxis.enabled` is false.
- Setup with `--praxis` adds the `praxis` block, new config or existing, and changes nothing else in the file. Plugins and `agentop server` entries stay. Setup without `--praxis` leaves an existing `praxis` block as it is.
- Turning the mode off is `praxis.enabled: false` and a restart. No code path removes the block.
- `install.sh --praxis` refuses darwin/amd64. Supported: darwin/arm64, linux/amd64, linux/arm64.
- Archive name, verbatim: `cortex-gateway_<version>_<os>_<arch>.tar.gz`. It holds one file, `cortex-gateway`.
- Gateway version line, verbatim: `cortex-gateway <version>`. `<version>` is the build-time env `CORTEX_VERSION`, or `dev` when it is not set.
- Ports in praxis mode: `47605` (praxis.listen) and `47606` (praxis.egress).
- Run per-module Go commands with `GOWORK=off`. Log with `log/slog`.
- Comments and commit messages in ASD-STE100 style. Commits use `git commit -s` and end with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. No `Co-Authored-By`.
- Prose says Cortex for the product (CLAUDE.md naming rule).

## Decisions (made for this plan, with reasons)

1. **Setup learns praxis mode from a new `--praxis` flag.** `install.sh` passes it only when the user gave `--praxis`. Reason: the `hand_off` flags are frozen because `--ref=main` can run a newer script against an older agentop. `install.sh --praxis` refuses every release that has no gateway archive, and the first release with one also has an agentop with `--praxis`. So an older agentop never receives the flag. An env var would be silently ignored by an old agentop, which is worse than a usage error.
2. **`praxisRefusal` refuses when the config file sets `praxis.enabled`, OR the running Cortex reports it in `GET /config`.** Reason: the file is what the next start runs, and `/config` is what runs now. In both cases a plugin edit has no effect until the mode is off and Cortex restarts. The message names both facts.
3. **The check sits in `serverTarget`.** Reason: every `agentop server` action and every `agentop script` action loads its target there, so one call covers all of them. Help output (`agentop server help`, `agentop script help`) does not call `serverTarget` and still works. `agentop tools scan` without `--write` only reads transcripts, so it still works.
4. **Refusal message, verbatim:** `agentop: praxis mode is on (<where>), so Cortex runs no plugins and this command has no effect. To turn it off, set praxis.enabled: false in <config path> and run: agentop service restart`. `<where>` is `in <config path>`, `in the running Cortex`, or `in <config path> and in the running Cortex`. Exit status 1.
5. **Setup adds the block with a line edit, not a YAML round trip.** Three cases: no top-level `praxis:` key: append `praxis:\n  enabled: true\n` at the end, with the indentation of the file's first top-level key. `praxis:` block without `enabled:`: insert `enabled: true` into the block. `enabled:` with any other value: replace the value with `true` on that line. A flow-style `praxis: {…}` is a problem with the fix "write `enabled: true` under praxis: by hand". Reason: the file keeps its comments and order, as `insertListenerKeys` does. The result goes through `replaceConfig` with a verify that requires `cfg.PraxisEnabled()` and `config.Validate(cfg) == nil`, so a config that praxis mode refuses (no `bind_loopback_only`, `mtls` set) never replaces the original.
6. **`insertListenerKeys` becomes `insertBlockKeys(src, block string, pins []pinnedListener)`.** `insertListenerKeys(src, pins)` stays as a one-line wrapper. Reason: the praxis insert needs the same indentation logic. No second copy.
7. **Setup with `--praxis` needs `cortex-gateway`:** staged in `--from`, or else installed in the bin dir when there is no `--from`. Otherwise the config step plans a problem: `cortex-gateway is not installed; install it with: curl -fsSL https://www.rossoctl.dev/cortex/install.sh | sh -s -- --praxis`. Reason: a praxis config without the gateway gives a proxy that answers 503 to every request.
8. **The systemd unit adds `--supervise` when the config file sets `praxis.enabled`.** `servicePaths` gets `supervise bool`. `runServiceInstall` sets it from the config file right before it writes the unit. `serviceIsCurrent` returns false on Linux when the unit's `--supervise` does not match the config. Reason: plan 3 decision 20. Direct mode keeps today's unit (Restart=on-failure and StartLimit). Setup's config step runs before the service step, so the unit sees the new block.
9. **The supervisor checks the gateway version before each gateway start.** `childSpec` gets `wantVersion string`, set to `version` for the gateway. Before each start, `superviseChild` runs `<path> --version` with a 2 s timeout. When the last word differs from `wantVersion`, it logs `supervisor: cortex-gateway version differs from cortex, so it does not start` with both versions, and treats it as a failed start (normal backoff). Reason: a mismatch must not start, and a backoff retry lets a later `agentop update` fix it with no restart of the supervisor. A missing binary already fails each start in the same way. The check is a package var `gatewayVersionOf func(path string) string` so tests replace it.
10. **`cortex-gateway --version` prints `cortex-gateway <version>` and exits 0 before any config load.** `<version>` is `option_env!("CORTEX_VERSION").unwrap_or("dev")`. Reason: the supervisor needs a version, and a dev build of both binaries reads `dev`, so `make dev-install` plus a local `cargo build` still matches. The release job (step 7) sets `CORTEX_VERSION`.
11. **`install.sh` downloads the gateway archive after the two main archives, with a plain `curl`, not through the progress bar.** It is checked against `checksums.txt` with the same anchored match, and the expected count of checksum lines becomes 3. Reason: the bar code tracks exactly two pids. The gateway archive is small next to `cortex`. Add the comment `shortcut: the bar does not count the gateway archive, add it to the bar if users report a stall`.
12. **`require_praxis_build <version>` mirrors `require_cpex_build`** and runs in both places that `require_cpex_build` runs (before the re-exec for a `v*` ref, and after `resolve_version`). Message: `release <version> has no cortex-gateway build for <os>/<arch>`. `praxis_platform_check` mirrors `cpex_platform_check`. `--praxis` with `AUTHBRIDGE_SKIP_DOWNLOAD=1` dies as `--cpex` does. Reason: no release has the archive before step 7, so every current release is refused with a clear message (spec, Section 6 step 7).
13. **`agentop update` passes `--praxis` when `~/.cortex/config.yaml` loads and sets `praxis.enabled`.** A config that does not load passes nothing. Reason: the spec says update detects the mode from the config, as it detects `--cpex` from the binary.
14. **The port preflight of a fresh install also probes `47605` and `47606` when `opts.praxis` is set.** Reason: plan 3 decision 17. A fresh install has no config, so the flag is the only signal.
15. **No new Makefile target for the gateway.** A developer who wants praxis mode from a tree runs `cargo build --release -p cortex-gateway --manifest-path praxis/Cargo.toml`, copies `praxis/target/release/cortex-gateway` into `bin/`, and runs `bin/agentop setup --from bin --yes --no-modify-path --restart --praxis`. `docs/laptop-service.md` says so. Reason: YAGNI. `make dev-install` must work with no Rust toolchain.
16. **The plugin pane needs no change.** Reason: in praxis mode Cortex builds no outbound pipeline (plan 4), so `GET /v1/pipeline` already reports no plugins.
17. **`install.sh --stop` and `agentop uninstall` need no change.** Reason: plan 3 already added `cortex-gateway` to both.
18. **One PR.** The estimate is about 1,250 counted lines, under the 2,000 limit.

## Review Focus

1. A user runs `install.sh --praxis` over an existing config with plugins and `agentop server` entries: only the `praxis` block may change. Pinned in Task 4 (`TestConfigStep_PraxisKeepsEverythingElse`).
2. A config whose `praxis:` block says `enabled: false` with a comment after the value: the edit must set `true` and keep the comment. Pinned in Task 4 (`TestEnablePraxis_ReplacesFalseKeepsComment`).
3. Setup with `--praxis` on a config without `listener.bind_loopback_only` that the pin migration cannot fix: setup must refuse and leave the file. Pinned in Task 4 (`TestEnablePraxis_InvalidPraxisConfigLeavesFile`).
4. A user turns praxis off in the file, but Cortex still runs in praxis mode: `agentop server add` must refuse and name the running Cortex. Pinned in Task 2 (`TestPraxisRefusal_RunningOnly`).
5. `agentop update` on a praxis install whose config does not load: update must not crash and must not pass `--praxis`. Pinned in Task 6 (`TestUpdateFlags_PraxisConfigBrokenPassesNothing`).

---

## File map

| File | Task | Change |
|---|---|---|
| `cmd/agentop/local_endpoint.go`, `core/observe/testdata/config.json`, `core/observe/wire_fixture_test.go`, `cmd/agentop/local_endpoint_test.go`, `docs/proxy-contract.md` (`GET /config` row) | 1 | decode `praxis.enabled` |
| `cmd/agentop/praxis_mode.go` (new), `cmd/agentop/praxis_mode_test.go` (new), `cmd/agentop/cmd_server.go`, `cmd/agentop/cmd_tools.go` | 2 | the refusal helper and its callers |
| `praxis/server/src/main.rs`, `cmd/cortex/supervise.go`, `cmd/cortex/supervise_test.go` | 3 | gateway `--version`, supervisor version check |
| `cmd/agentop/cmd_setup.go`, `cmd/agentop/setup_runner.go`, `cmd/agentop/setup_step_config.go`, `cmd/agentop/cmd_config_migrate.go`, `cmd/agentop/setup_step_service.go`, `cmd/agentop/setup_praxis.go` (new), `cmd/agentop/setup_praxis_test.go` (new) | 4 | `setup --praxis` |
| `cmd/agentop/cmd_service.go`, `cmd/agentop/cmd_service_platform.go`, `cmd/agentop/cmd_service_test.go`, `docs/laptop-service.md` | 5 | systemd `--supervise` in praxis mode, docs |
| `cmd/agentop/cmd_update.go`, `cmd/agentop/cmd_update_test.go` | 6 | update keeps `--praxis` |
| `scripts/install.sh`, `scripts/install_test.sh`, `docs/proxy-contract.md` (`install.sh` flags table) | 7 | `install.sh --praxis` |

## PRs

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `feat: Support praxis mode in agentop and the installer` | "" | T1, T2, T3, T4, T5, T6, T7 | 1250 |

---

### Task 1: Decode `praxis.enabled` from `GET /config`

Depends on: none. Estimate: 70 lines.

**Files:**
- Modify: `cmd/agentop/local_endpoint.go` (`runningConfigFields`, `runningConfig`)
- Modify: `core/observe/testdata/config.json`, `core/observe/wire_fixture_test.go`
- Test: `cmd/agentop/local_endpoint_test.go`
- Modify: `docs/proxy-contract.md` (the `GET /config` row: four fields, and `praxisRefusal` as a second reader)

**Interfaces:**
- Produces: `runningConfig(base string) (*config.Config, error)` now sets `cfg.Praxis = &config.PraxisConfig{Enabled: true}` when `/config` has `"praxis": {"enabled": true}`. `cfg.Praxis` stays nil otherwise.

- [ ] **Step 1: Write the failing tests.** `TestRunningConfig_ReadsPraxisEnabled`: a test server answers `{"listener":{"forward_proxy_addr":"127.0.0.1:47600"},"praxis":{"enabled":true}}`. Assert `cfg.PraxisEnabled()` is true. `TestRunningConfig_NoPraxisBlock`: the same body without `praxis`. Assert `cfg.Praxis == nil`.
- [ ] **Step 2: Run** `GOWORK=off go -C cmd/agentop test -run 'TestRunningConfig_' .` Expected: FAIL.
- [ ] **Step 3: Implement.** Add `Praxis *struct{ Enabled bool \`json:"enabled"\` } \`json:"praxis"\`` to `runningConfigFields` and copy it into `cfg`. Add `"praxis": {"enabled": true}` to the fixture. In `TestConfigEndpoint_CoversTheWireFixture`, set `cfg.Praxis = &config.PraxisConfig{Enabled: true}`. Update the doc row.
- [ ] **Step 4: Run** `GOWORK=off go -C cmd/agentop test -run 'TestRunningConfig_|TestStatsDecoders' . && GOWORK=off go -C core test ./observe/...` Expected: PASS.
- [ ] **Step 5: Commit** `feat: Read praxis.enabled from the running Cortex config`.

### Task 2: One helper refuses the commands that praxis mode makes useless

Depends on: T1. Estimate: 170 lines.

**Files:**
- Create: `cmd/agentop/praxis_mode.go`, `cmd/agentop/praxis_mode_test.go`
- Modify: `cmd/agentop/cmd_server.go` (`serverTarget`), `cmd/agentop/cmd_tools.go` (before `toolscan.PatchConfig`)

**Interfaces:**
- Consumes: `runningConfig` (T1).
- Produces: `func praxisRefusal(cfg *config.Config, path, statsURL string) error`. It returns nil when neither source sets the mode. `statsURL == ""` skips the `/config` read. A failed `/config` read counts as "not set". The error text is decision 4, without the `agentop: ` prefix (callers print it with their usual prefix).

- [ ] **Step 1: Write the failing tests** in `praxis_mode_test.go`:
  - `TestPraxisRefusal_DirectMode`: file without praxis, server without praxis. Assert nil.
  - `TestPraxisRefusal_FileOnly`: `cfg.Praxis = &config.PraxisConfig{Enabled: true}`, `statsURL ""`. Assert the error contains `praxis mode is on (in <path>)` and `agentop service restart`.
  - `TestPraxisRefusal_RunningOnly`: file direct mode, test server reports `praxis.enabled: true`. Assert the error contains `in the running Cortex`.
  - `TestServerAdd_RefusedInPraxisMode`: a temp config with `praxis: {enabled: true}` and `listener.bind_loopback_only: true`. Run `runServer([]string{"add", …, "--config", path}, …)`. Assert exit 1, the refusal on stderr, and the file bytes unchanged.
  - `TestScriptList_RefusedInPraxisMode`: the same config, `runScript([]string{"list", "--config", path}, …)`. Assert exit 1.
  - `TestToolsScanWrite_RefusedInPraxisMode`: `runTools` with `scan --dir <temp transcripts> --write <praxis config>`. Assert exit 1 and file unchanged. `TestToolsScan_NoWriteWorksInPraxisMode`: no `--write`. Assert exit 0.
- [ ] **Step 2: Run** `GOWORK=off go -C cmd/agentop test -run 'Praxis' .` Expected: FAIL.
- [ ] **Step 3: Implement** `praxisRefusal`. Call it at the end of `serverTarget` (return its error). In `runTools`, when `*write != ""`, load the file with `config.Load`, find the stats URL as `serverTarget` does (`dialURL(cfg.Stats.StatsAddress)` plus `localStatsUp`), and call `praxisRefusal` before the scan. A file that does not load keeps today's path (PatchConfig reports the error).
- [ ] **Step 4: Run** `scripts/dev/go-test-shards.sh -short cmd/agentop` Expected: PASS.
- [ ] **Step 5: Commit** `feat: Refuse plugin edits in praxis mode`.

### Task 3: The supervisor refuses a gateway of another version

Depends on: none. Estimate: 140 lines.

**Files:**
- Modify: `praxis/server/src/main.rs`
- Modify: `cmd/cortex/supervise.go` (`childSpec`, `supervisorChildren`, `superviseChild`)
- Test: `cmd/cortex/supervise_test.go`, unit test in `praxis/server/src/main.rs`

**Interfaces:**
- Produces: `cortex-gateway --version` prints `cortex-gateway <version>\n`, exit 0. Go: `var gatewayVersionOf = func(path string) string` (last word of the first line of `path --version`, "" on any failure, 2 s timeout, stdin nil). `childSpec.wantVersion string`.

- [ ] **Step 1: Write the failing tests.**
  - Rust: `fn version_line_names_the_binary()` asserts `version_line()` starts with `"cortex-gateway "` and its last word is `option_env!("CORTEX_VERSION").unwrap_or("dev")`.
  - Go: `TestSupervisorChildren_GatewayWantsCortexVersion`: in `TestSupervisorChildren_PraxisAddsGateway`'s setup, assert `specs[1].wantVersion == version` and `specs[0].wantVersion == ""`.
  - Go: `TestSuperviseChild_GatewayVersionMismatchDoesNotStart`: set `gatewayVersionOf` to return `"v0.0.1"`, `wantVersion "v9.9.9"`, path a script that writes a marker file. Close `stop` after 200 ms. Assert the marker file does not exist.
  - Go: `TestSuperviseChild_GatewayVersionMatchStarts`: `gatewayVersionOf` returns `wantVersion`. Assert the marker file exists.
- [ ] **Step 2: Run** `cargo test --manifest-path praxis/Cargo.toml -p cortex-gateway` and `GOWORK=off go -C cmd/cortex test -tags "$(GOWORK=off go -C scripts/profile-tags run . local)" -run 'TestSupervis' .` Expected: FAIL.
- [ ] **Step 3: Implement.** Rust: `fn version_line() -> String`. In `main`, before `install_crypto_provider`, when any arg is `--version`, print `version_line()` and return. Go: set `wantVersion: version` on the gateway spec. In `superviseChild`, before `exec.Command`, when `spec.wantVersion != ""` and `gatewayVersionOf(spec.path) != spec.wantVersion`, log per decision 9 (`"want", spec.wantVersion, "got", got`) and take the same path as a start error.
- [ ] **Step 4: Run** the Step 2 commands. Expected: PASS. Also `cargo clippy --manifest-path praxis/Cargo.toml --all-targets --locked -- -D warnings`.
- [ ] **Step 5: Commit** `feat: Refuse a cortex-gateway whose version differs from cortex`.

### Task 4: `agentop setup --praxis`

Depends on: none. Estimate: 420 lines.

**Files:**
- Modify: `cmd/agentop/setup_runner.go` (`setupOptions.praxis bool`), `cmd/agentop/cmd_setup.go` (`fs.BoolVar(&o.praxis, "praxis", false, "")`, `setupUsage` line `--praxis  turn on praxis mode (needs cortex-gateway)`)
- Modify: `cmd/agentop/cmd_config_migrate.go` (decision 6)
- Create: `cmd/agentop/setup_praxis.go`, `cmd/agentop/setup_praxis_test.go`
- Modify: `cmd/agentop/setup_step_config.go` (plan and apply), `cmd/agentop/setup_step_service.go` (`preflightPorts`)

**Interfaces:**
- Produces:
  - `func insertBlockKeys(src, block string, pins []pinnedListener) (string, error)`.
  - `func enablePraxisText(src string) (out string, changed bool, err error)` (decision 5, pure text).
  - `func enablePraxis(path string) (changed bool, err error)`: reads, calls `enablePraxisText`, and writes through `replaceConfig` with the decision-5 verify. `changed` false when the file already sets `enabled: true`.
  - `func praxisGatewayProblem(env *setupEnv) *problem` (decision 7).
  - `var praxisPorts = []string{"47605", "47606"}`.

- [ ] **Step 1: Write the failing tests** in `setup_praxis_test.go`:
  - `TestEnablePraxisText_AppendsBlock`: input without `praxis:`. Assert the output is the input plus `praxis:\n  enabled: true\n`, and loads with `PraxisEnabled()`.
  - `TestEnablePraxisText_IndentedDocument`: every top-level key indented 2 spaces. Assert the appended `praxis:` has 2 spaces and `enabled:` has 4.
  - `TestEnablePraxisText_InsertsEnabled`: `praxis:\n  listen: 127.0.0.1:47605\n`. Assert `enabled: true` lands inside the block and `listen` stays.
  - `TestEnablePraxis_ReplacesFalseKeepsComment`: `praxis:\n  enabled: false # off for now\n`. Assert the line becomes `  enabled: true # off for now`.
  - `TestEnablePraxisText_AlreadyOn`: assert `changed == false` and the output equals the input.
  - `TestEnablePraxisText_FlowStyleRefused`: `praxis: {enabled: false}`. Assert an error.
  - `TestEnablePraxis_InvalidPraxisConfigLeavesFile`: a config without `bind_loopback_only`. Assert an error and file bytes unchanged.
  - `TestConfigStep_PraxisKeepsEverythingElse`: an existing config with two outbound plugins and a router `servers:` entry. Run the config step with `opts.praxis`. Assert the only diff lines are the added `praxis` lines.
  - `TestConfigStep_NoPraxisFlagLeavesBlock`: an existing config with `praxis: {enabled: true}` written as a block, no flag. Assert the file is unchanged.
  - `TestConfigStep_PraxisNeedsGateway`: `opts.praxis`, a `--from` dir without `cortex-gateway`. Assert the plan returns the decision-7 problem.
  - `TestSetup_NoGatewayDirectModeWorks`: the existing setup scene with no `cortex-gateway` anywhere and no flag. Assert success (pins the Global Constraint).
  - `TestPreflightPorts_PraxisProbesGatewayPorts`: fresh install, `opts.praxis`, `portInUse = func(p string) bool { return p == "47605" }` (the existing seam in `setup_probe.go`). Assert the problem names 47605. Without `opts.praxis`, assert no problem.
- [ ] **Step 2: Run** `GOWORK=off go -C cmd/agentop test -run 'Praxis|NoGateway' .` Expected: FAIL.
- [ ] **Step 3: Implement.** In `configStep.plan`: when `opts.praxis`, return `praxisGatewayProblem(env)` if it is not nil. For an existing config, a praxis edit that is pending makes the plan `update` (not `done`). In `configStep.apply`: after the pin migration and the Bob pricing, call `enablePraxis(cfg)` when `opts.praxis`. A failure is a `stepError` (the step's undo restores the snapshot). Append ` · praxis mode on` to the detail when it changed. In `preflightPorts`: probe `append(cortexPorts, praxisPorts...)` when `opts.praxis`.
- [ ] **Step 4: Run** `scripts/dev/go-test-shards.sh -short cmd/agentop`, then `scripts/dev/go-test-shards.sh -race cmd/agentop` (setup scenes). Expected: PASS.
- [ ] **Step 5: Commit** `feat: Add the praxis block from agentop setup --praxis`.

### Task 5: The systemd unit supervises in praxis mode

Depends on: none. Estimate: 130 lines.

**Files:**
- Modify: `cmd/agentop/cmd_service.go` (`servicePaths.supervise`, `runServiceInstall`, `serviceIsCurrent`)
- Modify: `cmd/agentop/cmd_service_platform.go` (`renderUnitFor` Linux branch, and its comment)
- Test: `cmd/agentop/cmd_service_test.go`
- Modify: `docs/laptop-service.md` (remove the "On Linux, do not set `praxis.enabled`" paragraph; add how to turn praxis mode on with `install.sh --praxis` and off with `praxis.enabled: false` plus `agentop service restart`; add decision 15's developer steps)

**Interfaces:**
- Produces: `func configPraxisEnabled(path string) bool` in `cmd_service.go` (false when the file does not load). T6 uses it.

- [ ] **Step 1: Write the failing tests.** `TestRenderUnit_LinuxPraxisSupervises`: `p.supervise = true`. Assert `ExecStart=` contains `--supervise`. `TestRenderUnit_LinuxDirectUnchanged`: `p.supervise = false`. Assert the unit equals today's text. `TestConfigPraxisEnabled`: three files (praxis on, praxis off, broken YAML). Assert true, false, false.
- [ ] **Step 2: Run** `GOWORK=off go -C cmd/agentop test -run 'TestRenderUnit_Linux|TestConfigPraxisEnabled' .` Expected: FAIL.
- [ ] **Step 3: Implement.** Linux `ExecStart` adds ` --supervise` after the binary when `p.supervise`. `runServiceInstall` sets `p.supervise = configPraxisEnabled(p.configFile)` before `renderUnit`. `serviceIsCurrent` on Linux returns false when `strings.Contains(body, "--supervise") != configPraxisEnabled(p.configFile)`. Update the comment above `renderUnitFor` that says the unit never uses `--supervise`.
- [ ] **Step 4: Run** `scripts/dev/go-test-shards.sh -race cmd/agentop`. Expected: PASS.
- [ ] **Step 5: Commit** `feat: Supervise the gateway under systemd in praxis mode`.

### Task 6: `agentop update` keeps praxis mode

Depends on: T5. Estimate: 50 lines.

**Files:**
- Modify: `cmd/agentop/cmd_update.go` (`updateFlags`, and the help text line that lists what update keeps)
- Test: `cmd/agentop/cmd_update_test.go`

**Interfaces:**
- Consumes: `configPraxisEnabled(path string) bool` (T5).

- [ ] **Step 1: Write the failing tests.** `TestUpdateFlags_PraxisConfigPassesPraxis`: `env.configPath()` sets praxis on. Assert `--praxis` in the flags. `TestUpdateFlags_DirectConfigNoPraxis`: assert no `--praxis`. `TestUpdateFlags_PraxisConfigBrokenPassesNothing`: unparseable YAML. Assert no `--praxis` and no panic.
- [ ] **Step 2: Run** `GOWORK=off go -C cmd/agentop test -run 'TestUpdateFlags_' .` Expected: FAIL.
- [ ] **Step 3: Implement.** After the `--cpex` line: `if configPraxisEnabled(env.configPath()) { flags = append(flags, "--praxis") }`. Add "praxis mode" to the help text list beside "the cpex build".
- [ ] **Step 4: Run** the Step 2 command. Expected: PASS.
- [ ] **Step 5: Commit** `feat: Keep praxis mode across agentop update`.

### Task 7: `install.sh --praxis`

Depends on: none. Estimate: 270 lines.

**Files:**
- Modify: `scripts/install.sh` (arg loop, `usage`, unknown-option message, `praxis_platform_check`, `require_praxis_build`, the re-exec check, the skip-download check, the download, checksum and extract steps, `hand_off`)
- Test: `scripts/install_test.sh`
- Modify: `docs/proxy-contract.md` (`install.sh` flags table: a `--praxis` row, passed to setup as `--praxis`)

**Interfaces:**
- Consumes: setup's `--praxis` flag (T4) by name only. The test uses a fake agentop, so no code dependency.
- Produces: the stage holds `cortex-gateway` (mode 0755, quarantine cleared on darwin) when `--praxis` is set. `hand_off` appends `--praxis`.

- [ ] **Step 1: Write the failing tests** in `install_test.sh`, in the style of the `--cpex` block (`make_release`, `run_install`, `check`):
  - `--praxis` on darwin/amd64 dies with `--praxis is not available for darwin/amd64 (supported: darwin/arm64, linux/amd64, linux/arm64)`.
  - `--praxis` from a release without a `cortex-gateway_…` line dies with `release v9.9.9 has no cortex-gateway build for linux/amd64`.
  - `--praxis` from a release with the archive: the fake agentop receives `--praxis`, and the stage has an executable `cortex-gateway`.
  - `--praxis` with a gateway archive whose checksum does not match: dies with `checksum verification failed`.
  - No `--praxis`: the fake agentop does not receive `--praxis`, and the gateway archive is not fetched.
  - `--praxis` with `AUTHBRIDGE_SKIP_DOWNLOAD=1` dies.
  - Extend `make_release` with a `praxis` and a `nopraxis` mode, as `cpex`/`nocpex`.
- [ ] **Step 2: Run** `sh scripts/install_test.sh`. Expected: the new checks FAIL.
- [ ] **Step 3: Implement** decisions 11 and 12. Usage line: `--praxis  also install cortex-gateway and turn on praxis mode (macOS arm64, Linux amd64/arm64). To turn it off, set praxis.enabled: false in ~/.cortex/config.yaml and run agentop service restart`. Put `--praxis` in the unknown-option message list. Add the gateway archive to the checksum loop only when `PRAXIS` is set, and make the line count `2` or `3` to match. Add its bytes to `bytes`. Extract it, require `cortex-gateway` in the stage, `chmod +x`, and clear quarantine.
- [ ] **Step 4: Run** `sh scripts/install_test.sh` and `shellcheck scripts/install.sh` when `shellcheck` is installed. Expected: PASS.
- [ ] **Step 5: Commit** `feat: Add install.sh --praxis`.

---

## Self-review

- Spec coverage (Section 4, "agentop" and "Release and install"): `/config` field → T1. Refusals with one helper → T2. `install.sh --praxis`, darwin/amd64 refusal, gateway build missing → T7. Setup adds the block, keeps the rest, needs the gateway → T4. Turn off = `enabled: false` plus restart → T2 message, T5 docs, no code removes the block. `agentop update` keeps it → T6. Supervisor version refusal → T3. `cortexPorts` 47605/47606 (plan 3 decision 17) → T4. systemd `--supervise` (plan 3 decision 20) → T5. Plugin pane empty, `--stop`, uninstall → decisions 16 and 17, no task.
- Out of scope, by delivery step: MCP and A2A parsers (step 6). The release job that publishes `cortex-gateway_*` archives and sets `CORTEX_VERSION` (step 7). Until step 7, `install.sh --praxis` refuses every release. That is intended (spec Section 6, step 7).
- Issue "Done when", first item ("a fresh `install.sh --praxis` gives a working praxis mode"): only testable by hand after step 7 publishes an archive. Before that, the PR body describes a manual run from a tree with decision 15's steps.
