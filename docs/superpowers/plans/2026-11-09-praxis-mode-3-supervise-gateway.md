# Praxis mode 3/8: Supervise cortex-gateway as a sibling of cortex — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** In praxis mode, `cortex -supervise` runs `cortex` and `cortex-gateway` as two siblings. Each child has its own restart loop. The supervisor writes the token file and `gateway.yaml` at each start. The gateway gets a clean env and exits when the supervisor goes away. In direct mode nothing changes.

**Architecture:** `cmd/cortex/gateway.go` turns a loaded `*config.Config` into the two gateway files (pure functions, golden-tested). `cmd/cortex/supervise.go` gets a list of child specs. The cortex spec is the loop of today (fallback server, port-in-use backoff). The gateway spec has no fallback. Each spec runs its own loop with its own `restartPolicy`, so a cortex exit does not touch the gateway. The gateway's stdin is a pipe from the supervisor. `cortex-gateway --exit-on-stdin-eof` exits when the pipe closes, which happens on every supervisor exit, SIGKILL included, on Linux and macOS.

**Tech Stack:** Go 1.26.10, Rust 1.96.0, `praxis-proxy =0.8.0`, POSIX sh.

**Spec:** `docs/superpowers/specs/2026-11-07-praxis-mode-design.md`, Section 4 ("Supervision", "`gateway.yaml`, in the end-state shape") and Section 6 step 3. Issue: #1720. Tracker: #1714. Previous step: #1719 (plan `docs/superpowers/plans/2026-11-09-praxis-mode-2-gateway-and-ingest.md`, merged as PR #1758).

## Decisions this plan takes

Each one closes a question that the spec or the issue leaves open.

1. **One mechanism for "no orphans": a stdin pipe, on Linux and macOS.** The supervisor sets the gateway's stdin to a pipe and keeps the write end. The kernel closes the write end when the supervisor exits for any reason, SIGKILL included. `cortex-gateway --exit-on-stdin-eof` starts a thread that reads stdin to EOF and then calls `std::process::exit(0)`. No `Pdeathsig` on Linux: it fires on the exit of the parent *thread*, not the process, which is a known trap with Go's thread pool, and one mechanism is testable on macOS. The write end is close-on-exec (Go's default), so the `cortex` child does not hold it.
2. **The flag is opt-in** (`--exit-on-stdin-eof`). Without it, a gateway that is started by hand with `</dev/null` keeps running.
3. **Stop = close the pipe, then SIGKILL after `superviseStopGrace`.** The gateway exits at once on EOF, so a stop is quick (restarts are quick, not graceful). The supervisor stops both children at the same time, so the total stop stays inside launchd's 5 s exit timeout.
4. **The gateway files go in the directory of the `--config` file**: `<dir>/gateway.token` and `<dir>/gateway.yaml`. The service always runs `cortex --supervise --config ~/.cortex/config.yaml`, so this is `~/.cortex/`. Without `--config` the supervisor has no config, so praxis mode is off.
5. **A new token at each supervisor start.** 32 random bytes, hex, written with mode 0600 by write-to-temp-then-rename in the same directory. Both children read the same file for the whole supervisor lifetime, so a restart of one child keeps the token valid for the other.
6. **The `cortex` child gets `CORTEX_GATEWAY_TOKEN_FILE=<dir>/gateway.token`** added to the supervisor's env. Only in praxis mode. In direct mode its env is the same as today.
7. **The gateway env is empty** (`cmd.Env = []string{}`, not nil). "Clean env" in the spec means no proxy variables. An empty env is the simplest form that cannot leak one. The gateway needs no variable: the config path comes from `-c`.
8. **`praxis.listen` and `praxis.egress` land in this PR**, because `gateway.yaml` names both (the listener and the interim catch-all route to the egress cluster). Defaults `127.0.0.1:47605` and `127.0.0.1:47606`. When set, each must be a literal loopback `host:port` with a non-zero port, else `Validate` refuses. The egress listener itself is #1721.
9. **The ingest address** in `gateway.yaml` is `listener.session_api_addr` after `ApplyPreset`. An empty or wildcard host becomes `127.0.0.1`.
10. **`gateway.yaml` content** (spec Section 4): `insecure_options` (`allow_unbounded_body`, `allow_private_upstreams`, `allow_private_endpoints`, all true); `body_limits` (`max_request_bytes: 33554432`, `max_response_bytes: null`); one listener `gateway` on `praxis.listen` with `forward_proxy: {enabled: true}`; one chain `main`: `ip_acl` (allow `127.0.0.1/32` and `::1/128` only, no `conditions`), `cortex_attribution`, `cortex_observe`, `access_log`, `router` (catch-all `path_prefix: "/"` to cluster `egress`), `load_balancer` (cluster `egress` on `praxis.egress`). The spec puts `access_log` last among the Cortex filters; Praxis needs `router` and `load_balancer` after it, as in `praxis/server/tests/testdata/gateway.yaml.tmpl`. The 0.8.0 names (checked in `praxis-proxy-filter-0.8.0/src/builtins/http/security/ip_acl.rs` and `praxis-proxy-core-0.8.0/src/config/listener.rs`): the filter is `- filter: ip_acl` with `allow: ["127.0.0.1/32", "::1/128"]`. Do not set `deny`: `allow` and `deny` are mutually exclusive, and the config has `deny_unknown_fields`. The listener key is `forward_proxy: {enabled: true}` on the listener entry. The `cortex_attribution` and `cortex_observe` keys (`token_file`, `ingest`) are as in `gateway.yaml.tmpl`.
11. **The admin metrics port is not in `gateway.yaml` in this PR.** The issue does not list it, and no caller reads it yet.
12. **The startup probe and `gateway_unavailable` are #1721.** The spec lists "the probe" in step 3, but the probe goes through the hop, and the hop is #1721. The issue does not list the probe.
13. **The gateway path is `filepath.Join(filepath.Dir(self), "cortex-gateway")`**, where `self` is `os.Executable()`. A missing or non-executable gateway is a start failure of that child: it logs ERROR once per run of failures and retries with the normal backoff. The supervisor never gives up (see `supervise.go`), and the `cortex` child keeps running.
14. **The stale-gateway stop runs once, at supervisor start, in praxis mode only.** It uses `holderOf(praxis.listen)` (`portconflict.go`). When the holder's exe basename is `cortex-gateway`, it sends SIGTERM, waits up to `superviseStopGrace` for the port to be free, then sends SIGKILL. Any other holder is left alone: the gateway then fails to bind and backs off, and the log names the holder.
15. **The gateway's exit status gets the normal backoff.** It does not use `exitPortInUse`, so `restartPolicy.exited(false, …)` applies.
16. **The version check of the gateway is not in this PR.** The spec ties it to `agentop update` and a released gateway. No release has a gateway before step 7 (#1723).
17. **`cortexPorts` in agentop does not change in this PR.** agentop probes ports only on a fresh install (`env.configFresh`), which has no config, so it cannot be in praxis mode. The only way to ask for praxis mode on a fresh install is `install.sh --praxis` / `agentop setup` with praxis, which step 5 (#1722) adds. The probe of 47605 and 47606 lands there, with its caller. Write this in the PR body as a deviation from the issue text.
18. **`cortex-gateway` is added to agentop's `setupBinaryNames`.** The list has three uses, all presence-based: `planRemoveBinaries` (uninstall, removes only names that `Lstat` finds), `cortexsOwn` (ownership by name), and `stagedBinaries` (installs a name only when the installer staged it, as for `cortex-session-dump`). So setup does not need the gateway, and no other code changes.
19. **`install.sh --stop`** gets `stop_gateway`: after the service and the pidfile proxy are stopped, it finds the process that listens on 47605 (`lsof -t -nP -iTCP@127.0.0.1:47605 -sTCP:LISTEN`), and sends it SIGTERM only when `pid_exe_path` is `${BIN_DIR}/cortex-gateway`. The port check after the stop also lists 47605 and 47606.

## Global Constraints

- **Direct mode does not change.** With `praxis.enabled: false` (the default): the supervisor starts only `cortex`; it does not stat, start or need `cortex-gateway`; it writes no token file and no `gateway.yaml`; the `cortex` child's env and args are the same as today. Every existing Go test passes with no change to its expectations.
- Exact names: binary `cortex-gateway`; flag `--exit-on-stdin-eof`; files `gateway.token`, `gateway.yaml`; env `CORTEX_GATEWAY_TOKEN_FILE` (`gatewayTokenEnv`, `cmd/cortex/ingest.go`); defaults `127.0.0.1:47605`, `127.0.0.1:47606`.
- The gateway command line is exactly `cortex-gateway -c <dir>/gateway.yaml --exit-on-stdin-eof`.
- Caps in `gateway.yaml`: `max_request_bytes: 33554432` (`32 << 20`, `maxRequestBodySize`), `max_response_bytes: null`.
- Log with `log/slog`, prefix `supervisor:` as today. A child's log lines say which child (`"child", "cortex"` or `"child", "cortex-gateway"`).
- Rust lint gate, from `praxis/`: `cargo fmt --check`, `cargo clippy --all-targets --locked -- -D warnings`, `cargo test --locked`. Use the rustup `cargo` (`$HOME/.cargo/bin`).
- Go: `GOWORK=off go vet ./...` and `gofmt -l <path>` in each module touched (`core`, `cmd/cortex`, `cmd/agentop`).
- Commits: `git commit -s`, message in ASD-STE100, ending with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. No `Co-Authored-By`. PR title `Feat: Supervise cortex-gateway as a sibling of cortex`, body with `Fixes #1720` and decisions 11, 12, 16 and 17 as deviations from the issue or spec.

## Review Focus

1. **A cortex crash in praxis mode.** Expected: the gateway pid does not change. Test: Task 5 `TestSupervisor_CortexRestartKeepsGateway`.
2. **A SIGKILL of the supervisor.** Expected: the gateway exits within 5 s. Test: Task 5 `TestSupervisor_KillLeavesNoGateway`; Task 2 `exits_on_stdin_eof`.
3. **Direct mode.** Expected: one child, no gateway lookup, no files. Test: Task 5 `TestSupervisorChildren_DirectModeHasOnlyCortex`.
4. **A `gateway.yaml` that Praxis 0.8.0 refuses.** Expected: the real binary loads the Go-written file and serves a request through it. Test: Task 4 `supervisor_gateway_yaml_serves`.
5. **The token file mode.** Expected: 0600, new at each start. Test: Task 3 `TestWriteGatewayFiles_TokenIs0600AndNew`.

## File Structure

| File | Task | What |
|---|---|---|
| `core/config/config.go`, `core/config/validate.go` | 1 | `praxis.listen`, `praxis.egress`, defaults, validation |
| `core/config/praxis_test.go` | 1 | tests |
| `praxis/server/src/main.rs` | 2 | `--exit-on-stdin-eof` |
| `praxis/server/tests/gateway.rs` | 2, 4 | stdin-EOF test; Go-golden test |
| `cmd/cortex/gateway.go` | 3 | `gatewayYAML`, `writeGatewayFiles` |
| `cmd/cortex/gateway_test.go`, `cmd/cortex/testdata/gateway.yaml.golden` | 3 | tests, golden (`-update`) |
| `cmd/cortex/supervise.go` | 5 | child specs, two loops, stale stop, stdin pipe |
| `cmd/cortex/supervise_test.go`, `cmd/cortex/restart_test.go` | 5 | unit and process tests |
| `cmd/cortex/portconflict.go` | 6 | `cortexBinaries` |
| `cmd/agentop/setup_step_binaries.go` (+ tests) | 6 | `setupBinaryNames` |
| `scripts/install.sh`, `scripts/install_test.sh` | 6 | `stop_gateway`, port list |
| `praxis/README.md`, `docs/laptop-service.md` | 7 | docs |

---

### Task 1: `praxis.listen` and `praxis.egress` (Go)

Depends on: none

**Files:**
- Modify: `core/config/config.go` (`PraxisConfig`; update its doc comment: `egress` is read by `gateway.yaml` now, the egress listener lands with #1721), `core/config/validate.go` (`validatePraxis`)
- Test: `core/config/praxis_test.go`

**Interfaces:**
- Produces: fields `Listen string \`yaml:"listen,omitempty" json:"listen,omitempty"\`` and `Egress string \`yaml:"egress,omitempty" json:"egress,omitempty"\`` on `PraxisConfig`; consts `DefaultPraxisListen = "127.0.0.1:47605"`, `DefaultPraxisEgress = "127.0.0.1:47606"`; methods `func (p *PraxisConfig) ListenAddr() string` and `func (p *PraxisConfig) EgressAddr() string` (the field, or the default when empty; the default on a nil receiver).

- [ ] **Step 1: Write the failing tests:** `TestPraxis_AddrDefaults` (nil block and empty fields give the defaults); `TestPraxis_AddrMustBeLoopback` (table: `0.0.0.0:47605`, `localhost:47605`, `127.0.0.1`, `127.0.0.1:0`, `10.0.0.1:1` refused, with the field name in the error; `127.0.0.1:1`, `[::1]:2` accepted). The checks run only when `praxis.enabled` is true.
- [ ] **Step 2: Run** `GOWORK=off go test ./config/ -run Praxis` from `core` → FAIL.
- [ ] **Step 3: Implement** with `netip.ParseAddrPort` and `IsLoopback()`.
- [ ] **Step 4: Run** → PASS; `GOWORK=off go vet ./...`, `gofmt -l config` clean.
- [ ] **Step 5: Commit.** "Feat: Add the praxis listen and egress addresses".

Estimated lines: 90.

### Task 2: `cortex-gateway --exit-on-stdin-eof` (Rust)

Depends on: none

**Files:**
- Modify: `praxis/server/src/main.rs`
- Test: `praxis/server/src/main.rs` (unit, flag parse), `praxis/server/tests/gateway.rs` (end to end)

**Interfaces:**
- Produces: the flag `--exit-on-stdin-eof`. With it, before `run_server_with_registry`, spawn a thread that reads `std::io::stdin()` to EOF (or error) and then calls `std::process::exit(0)`.

- [ ] **Step 1: Write the failing tests:** unit `exit_flag_is_read` (a helper `has_flag(args, "--exit-on-stdin-eof")`, also when it comes before or after `-c path`); e2e `exits_on_stdin_eof`: start the binary with the test template config, `Stdio::piped()` stdin, wait until the listener accepts, drop the stdin handle, assert the child exits within 5 s. Also `keeps_running_without_flag`: same, without the flag, assert it still runs after 1 s (then kill it).
- [ ] **Step 2: Run** `cargo test --locked -p cortex-gateway` from `praxis` → FAIL.
- [ ] **Step 3: Implement.** `config_arg` must skip the new flag (it is not a value).
- [ ] **Step 4: Run** → PASS; `cargo fmt --check`, `cargo clippy --all-targets --locked -- -D warnings` clean.
- [ ] **Step 5: Commit.** "Feat: Let cortex-gateway exit when its stdin closes".

Estimated lines: 90.

### Task 3: The gateway files (Go)

Depends on: Task 1

**Files:**
- Create: `cmd/cortex/gateway.go`, `cmd/cortex/gateway_test.go`, `cmd/cortex/testdata/gateway.yaml.golden`

**Interfaces:**
- Consumes: `config.PraxisConfig.ListenAddr`, `EgressAddr` (Task 1); `cfg.Listener.SessionAPIAddr`.
- Produces:
  - `const gatewayBinary = "cortex-gateway"`, `gatewayTokenName = "gateway.token"`, `gatewayYAMLName = "gateway.yaml"`.
  - `func gatewayYAML(cfg *config.Config, tokenPath string) ([]byte, error)`: the file of decision 10. Build it as a Go struct or `map` marshalled with `gopkg.in/yaml.v3` (the library `core/config` uses; already in the module graph; it moves from indirect to direct in `cmd/cortex/go.mod`, so run `GOWORK=off go mod tidy` there). The error is for a session API address that does not split.
  - `func ingestAddr(sessionAPIAddr string) (string, error)`: decision 9.
  - `func writeGatewayFiles(dir string, cfg *config.Config) (tokenPath, yamlPath string, err error)`: decision 5 for the token, then `gateway.yaml` (mode 0600, write-to-temp-then-rename).

- [ ] **Step 1: Write the failing tests:**
  - `TestGatewayYAML_Golden`: a config with `praxis.enabled`, default addresses, `session_api_addr: 127.0.0.1:47601`, token path `/CORTEX_DIR/gateway.token` → equals `testdata/gateway.yaml.golden`. `-update` rewrites the golden. No test in `cmd/cortex` declares an `update` flag today, so declare `var updateGolden = flag.Bool("update", false, "rewrite testdata goldens")` in `gateway_test.go`.
  - `TestGatewayYAML_Fields`: unmarshal the output and check each value of decision 10 (listener address, `forward_proxy.enabled`, filter order, `ip_acl` allows only loopback, `max_request_bytes` 33554432, `max_response_bytes` null, the three insecure options, the egress endpoint, the ingest address).
  - `TestIngestAddr`: `:47601` → `127.0.0.1:47601`; `0.0.0.0:1` → `127.0.0.1:1`; `[::]:1` → `127.0.0.1:1`; `127.0.0.1:1` unchanged; `bad` → error.
  - `TestWriteGatewayFiles_TokenIs0600AndNew`: two calls in one temp dir give two different 64-hex-char tokens; both files have mode 0600; no temp file is left in the dir.
- [ ] **Step 2: Run** `GOWORK=off go test ./ -run 'Gateway|IngestAddr'` from `cmd/cortex` → FAIL.
- [ ] **Step 3: Implement.** Run with `-update` once, then read the golden by eye against decision 10.
- [ ] **Step 4: Run** → PASS; vet and gofmt clean.
- [ ] **Step 5: Commit.** "Feat: Write the gateway token and gateway.yaml".

Estimated lines: 260 (the golden does not count).

### Task 4: The real gateway loads the Go-written `gateway.yaml` (Rust)

Depends on: Task 2, Task 3 (Task 2 edits the same `gateway.rs`)

**Files:**
- Modify: `praxis/server/tests/gateway.rs`

**Interfaces:**
- Consumes: `cmd/cortex/testdata/gateway.yaml.golden` (Task 3), read with `include_str!(concat!(env!("CARGO_MANIFEST_DIR"), "/../../cmd/cortex/testdata/gateway.yaml.golden"))`.

- [ ] **Step 1: Write the test** `supervisor_gateway_yaml_serves`: take the golden, replace `/CORTEX_DIR/gateway.token` with a temp token file, `127.0.0.1:47605` with a free listen port, `127.0.0.1:47606` with the mock origin's port, `127.0.0.1:47601` with the mock ingest port. Start the binary with it. Send one request with a valid `X-Cortex-Token` from loopback. Assert the origin gets it and the ingest mock gets a request line and a response line. Reuse the file's existing helpers.
- [ ] **Step 2: Run** `cargo test --locked -p cortex-gateway supervisor_gateway_yaml` from `praxis`. When Praxis refuses the file, fix `gatewayYAML` in Go (Task 3's file) and the golden, not the test. Go is the source of truth for the shape; Praxis is the judge of validity.
- [ ] **Step 3: Run** the full Rust gate → PASS.
- [ ] **Step 4: Commit.** "Test: Serve a request through the supervisor's gateway.yaml".

Estimated lines: 110.

### Task 5: Two siblings under `cortex -supervise` (Go)

Depends on: Task 1, Task 3

**Files:**
- Modify: `cmd/cortex/supervise.go`, `cmd/cortex/fallback.go` (load the config once; keep `supervisorForwardAddr`'s behavior and its WARN line), `cmd/cortex/restart_test.go` (`TestMain` modes)
- Test: `cmd/cortex/supervise_test.go`, `cmd/cortex/restart_test.go`

**Interfaces:**
- Consumes: `writeGatewayFiles`, `gatewayBinary` (Task 3); `holderOf` (`portconflict.go`); `gatewayTokenEnv` (`ingest.go`); `restartPolicy`, `portInUseExit`, `fallbackServer` (as today).
- Produces:
  - `type childSpec struct { name string; path string; args []string; env []string /* nil = inherit */; stdinPipe bool; fallback *fallbackServer /* cortex only */ }`.
  - `func supervisorChildren(cfg *config.Config, configPath, self string, args []string, write func(dir string, cfg *config.Config) (string, string, error)) ([]childSpec, error)`. Direct mode (`cfg == nil` or `!cfg.PraxisEnabled()`): one spec, cortex, same args and env as today; `write` is not called; nothing is stat'ed. Praxis mode: calls `write(filepath.Dir(configPath), cfg)`, adds `CORTEX_GATEWAY_TOKEN_FILE` to the cortex env (`append(os.Environ(), …)`), and adds the gateway spec (decisions 7, 13; command line in Global Constraints).
  - `func stopStaleGateway(addr string, lookup func(string) (holder, error), signal func(pid int, sig syscall.Signal) error, free func(string) bool)`: decision 14.
  - `runSupervisor` runs one goroutine per spec (`superviseChild(spec, stop <-chan struct{}) `), each with its own `restartPolicy`. The fallback open/close stays in the cortex loop only. On SIGTERM/SIGINT the supervisor closes `stop`; each loop stops its child (cortex: SIGTERM then SIGKILL after `superviseStopGrace`, as today; gateway: close the stdin pipe, then SIGKILL after `superviseStopGrace`) and the supervisor waits for both. A missing gateway binary is a start failure, not a return from `runSupervisor` (decision 13). The cortex child's `cmd.Start` failure keeps today's behavior.

- [ ] **Step 1: Write the failing unit tests** (`supervise_test.go`):
  - `TestSupervisorChildren_DirectModeHasOnlyCortex`: with `praxis.enabled: false`, and with `cfg == nil`: one spec named `cortex`, args unchanged, `env` nil; the `write` func fails the test if called; no file appears in the temp config dir. (The issue's "A test checks this".)
  - `TestSupervisorChildren_PraxisAddsGateway`: two specs; gateway path `<dir of self>/cortex-gateway`, args `-c <dir>/gateway.yaml --exit-on-stdin-eof`, env empty and non-nil, `stdinPipe` true; cortex env has `CORTEX_GATEWAY_TOKEN_FILE=<dir>/gateway.token`.
  - `TestStopStaleGateway`: table with fake funcs: holder exe `/x/cortex-gateway` → SIGTERM, port frees → no SIGKILL; port stays held → SIGKILL after the grace; holder `/x/other` → no signal; lookup error → no signal.
- [ ] **Step 2: Write the failing process tests** (`restart_test.go`). Copy `os.Args[0]` into a temp bin dir twice, as `cortex` and as `cortex-gateway` (a copy, not a link: `os.Executable` resolves links on Linux). In `TestMain`, when `filepath.Base(os.Args[0]) == "cortex-gateway"`, run a fake gateway (its env is empty, so it cannot read `CORTEX_TEST_RUN`): print `fake gateway pid <pid>` to stderr, read the `gateway` listener address from the `-c` file and listen on it, and with `--exit-on-stdin-eof` exit on stdin EOF. Add a `CORTEX_TEST_RUN=idle-child` mode: with `-supervise` run `main()`; else print `idle child pid <pid>` and block until SIGTERM. The test config has `praxis.enabled: true`, `listener.bind_loopback_only: true`, a free `praxis.listen`, and passes the existing config validation.
  - `TestSupervisor_CortexRestartKeepsGateway`: start the copied `cortex -supervise --config <cfg>`; read both pids from the output; SIGKILL the cortex child; wait for a new `idle child pid`; assert the gateway pid is the same and alive. Then SIGTERM the supervisor; assert both children exit within 5 s.
  - `TestSupervisor_KillLeavesNoGateway`: start it; read the gateway pid; SIGKILL the supervisor; assert the gateway pid is gone within 5 s (`syscall.Kill(pid, 0)` returns `ESRCH`). Kill the orphaned idle cortex child in `t.Cleanup`.
  - `TestSupervisor_StopsStaleGateway`: start a fake gateway by hand (from the copied `cortex-gateway`, without the flag) on the listen address; start the supervisor; assert the old pid exits and a new `fake gateway pid` line comes.
  - `TestSupervisor_DirectModeStartsNoGateway`: `praxis.enabled: false`, the copied `cortex-gateway` removed: the idle child starts, no `gateway` line is logged, no `gateway.token` or `gateway.yaml` is in the config dir.
- [ ] **Step 3: Run** `GOWORK=off go test -race ./ -run 'Supervisor|StopStale'` from `cmd/cortex` → FAIL.
- [ ] **Step 4: Implement.** Keep every existing comment that still holds; move a comment with its code. Keep `TestSupervisorEndsAStuckProxyInsideLaunchdsWindow` passing unchanged.
- [ ] **Step 5: Run** `GOWORK=off go test -race ./...` from `cmd/cortex` → PASS; vet, gofmt clean.
- [ ] **Step 6: Commit.** "Feat: Supervise cortex-gateway as a sibling of cortex".

Estimated lines: 650 (the supervise.go refactor and four process tests).

### Task 6: Know the gateway by name: port conflict, uninstall, `install.sh --stop`

Depends on: none

**Files:**
- Modify: `cmd/cortex/portconflict.go` (add `"cortex-gateway": true` to `cortexBinaries`), `cmd/agentop/setup_step_binaries.go` (add `"cortex-gateway"` to `setupBinaryNames`; decision 18), `scripts/install.sh` (`DEMO_GATEWAY_PORT=47605`, `DEMO_EGRESS_PORT=47606`, `stop_gateway`, the port list in `stop_cortex`, the `restage` binary list), `scripts/install_test.sh`
- Test: `cmd/cortex/portconflict_test.go`, `cmd/agentop/cmd_uninstall_test.go`, `cmd/agentop/setup_step_binaries_test.go` (create it if it does not exist), `scripts/install_test.sh`

- [ ] **Step 1: Write the failing tests:**
  - `cmd/cortex`: a `cortexBinaries` case for `cortex-gateway` in the existing table.
  - `cmd/agentop`: `planRemoveBinaries` removes a `cortex-gateway` in the bin dir; `cortexsOwn` is true for it; `othersInBinDir` does not list it. Add one `stagedBinaries` test: a staging dir without `cortex-gateway` gives no gateway entry (decision 18).
  - `install_test.sh`: `stop_gateway` with mocked `lsof`, `pid_exe_path` and `kill`: holder exe `${BIN_DIR}/cortex-gateway` → TERM; a holder from another directory → no signal; no holder → no signal. Add `stop_gateway` to every `_fn` list that extracts `stop_cortex`, and the two new port variables to every block that prints the `DEMO_*_PORT` lines.
- [ ] **Step 2: Run** the three test commands → FAIL.
- [ ] **Step 3: Implement.** `stop_gateway` follows `pre_rename_proxy_running`'s path compare (`readlink -f` fallback). It is a no-op without `lsof`.
- [ ] **Step 4: Run** → PASS; `shellcheck --severity=error scripts/install.sh`; vet and gofmt clean.
- [ ] **Step 5: Commit.** "Feat: Know cortex-gateway in the port check, uninstall and install.sh --stop".

Estimated lines: 140.

### Task 7: Docs

Depends on: Task 2, Task 5

**Files:**
- Modify: `praxis/README.md` ("Run the gateway": the supervisor now writes `gateway.yaml`; the flag `--exit-on-stdin-eof`; add the decisions of this plan that change the gateway, as a new `## Supervision decisions (#1720)` section after `## Accepted edge cases (#1719)`), `docs/laptop-service.md` (one paragraph after the "two `cortex` processes" paragraph near the top: in praxis mode the supervisor has a third child, `cortex-gateway`; it writes `~/.cortex/gateway.token` and `~/.cortex/gateway.yaml` at each start; a cortex restart keeps the gateway)

- [ ] **Step 1: Write** the text in ASD-STE100. No CLAUDE.md change.
- [ ] **Step 2: Run** `pre-commit run --files <changed files>`.
- [ ] **Step 3: Commit.** "Docs: Describe the supervised gateway".

Estimated lines: 50.

## PRs

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| pr1 | Feat: Supervise cortex-gateway as a sibling of cortex | (none) | 1, 2, 3, 4, 5, 6, 7 | 1390 |

One PR: the total is under 2,000 counted lines, and the issue is one delivery step (spec Section 6: "Each step is one PR").

## Gate

From the worktree root:

```sh
export PATH="$HOME/.cargo/bin:$PATH" && (cd praxis && cargo fmt --check && cargo clippy --all-targets --locked -- -D warnings && cargo test --locked) && (cd core && GOWORK=off go vet ./... && GOWORK=off go test ./config/... ./reloader/... ./audits/...) && (cd cmd/cortex && GOWORK=off go vet ./... && GOWORK=off go mod tidy -diff && GOWORK=off go test -race ./...) && (cd cmd/agentop && GOWORK=off go vet ./...) && scripts/dev/go-test-shards.sh -short cmd/agentop && sh scripts/install_test.sh && shellcheck --severity=error scripts/install.sh && test -z "$(gofmt -l core cmd/cortex cmd/agentop)"
```

## Self-review notes

- Issue scope → tasks: two siblings and backoff → 5; token file and `gateway.yaml` → 3 (with 1 for the addresses); clean env, exit with the supervisor, stale gateway → 2, 5; names in `cortexBinaries`, `install.sh --stop`, `agentop uninstall` → 6; `cortexPorts` → deferred (decision 17).
- Done-when → tests: cortex restart keeps the gateway and a supervisor exit stops it → `TestSupervisor_CortexRestartKeepsGateway`; no orphan after SIGKILL → `TestSupervisor_KillLeavesNoGateway`, `exits_on_stdin_eof`; direct mode → `TestSupervisorChildren_DirectModeHasOnlyCortex`, `TestSupervisor_DirectModeStartsNoGateway`.
