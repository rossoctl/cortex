# The local proxy's contract

`agentop` and `scripts/install.sh` drive the local Cortex proxy (`cortex`) through
ports, HTTP endpoints, a config file and a command line. This page lists each part
they rely on. A replacement for the proxy must keep every item here, or change its
callers in the same PR.

Each item names the code that implements it, so you can check it. The session API
on `:47601` has its own reference: [`session-api.md`](session-api.md).

## Ports

The built-in config pins every listener to `127.0.0.1` (`builtinConfigYAML` in
`cmd/cortex/local.go`). `install.sh` probes the same ports (`DEMO_*_PORT`), and so
does agentop setup (`cortexPorts` in `cmd/agentop/setup_probe.go`).

| Port | Config key | Serves |
|---|---|---|
| 47600 | `listener.forward_proxy_addr` | The forward proxy, the `HTTPS_PROXY` target. While the child is down, the `--supervise` parent answers `502 Cortex proxy is not serving` here (`cmd/cortex/fallback.go`) |
| 47601 | `listener.session_api_addr` | The session API ([`session-api.md`](session-api.md)). agentop reads the address from the config (`localSessionEndpoint` in `cmd/agentop/local_endpoint.go`) |
| 47602 | `stats.address` | The stats server (see below). It also serves `/debug/pprof/*` because the config sets `bind_loopback_only` (`WithPprof` in `core/observe/statserver.go`) |
| 47603 | `listener.transparent_proxy_addr` | Nothing on a local install. The built-in config pins the port, but `config.ListenerPlan` skips the transparent listener when the config is under `~/.cortex` |
| 47604 | `listener.health_addr` | `/healthz` and `/readyz` (see below) |

A port that another process holds stops the proxy before it opens any file. It exits
with status 3 (`exitPortInUse` in `cmd/cortex/portconflict.go`), and the supervisor
does not restart it at speed.

## Stats endpoints (`:47602`)

`core/observe/statserver.go` (`NewStatServer`) serves these. agentop finds the server
at `http://localhost:47602/` (`defaultCortexStatsURL` in
`cmd/agentop/local_endpoint.go`). `agentop exec` takes `--cortex-stats-url` to change
it, and `agentop pricing` takes `--stats-url`. The pipeline editor and `doctor` read
`stats.address` from the config instead.

| Endpoint | What agentop reads | Reader |
|---|---|---|
| `GET /reload/status` | `last_success`, `reloads_ok`, `reloads_failed`, `last_error` (shape: `Status` in `core/reloader/status.go`) | `edit.PollUntilReloaded` in `cmd/agentop/edit/status.go`. `localStatsUp` in `cmd/agentop/local_endpoint.go` requires a 2xx with a `reloads_ok` field |
| `GET /config` | `listener.forward_proxy_addr`, `tls_bridge.mode`, `tls_bridge.ca_dir`. It ignores all other fields | `runningConfig` and `runningConfigFields` in `cmd/agentop/local_endpoint.go`, for `agentop exec` |
| `GET /pricing/table` | The table (`describeBody`). With `?host=<gateway>`, the rates for that host (`effective`) | `cmd/agentop/cmd_pricing.go`. A 404 means a proxy older than the endpoint. Served by `Registry.Handler` in `core/cost/pricing/handler.go` |
| `GET /tls-bridge/unread` | `tlsbridge.UnreadReport` (`programs`, `hosts`, `osTrustsCA`) | `fetchUnread` in `cmd/agentop/doctor_unread.go`. A non-200 or no route makes the check silent |

`/config` reports the config in effect, with secret values masked. `GET /stats` and
the `/` index also exist. agentop does not read them.

## Health endpoints (`:47604`)

`core/bootstrap/bootstrap.go` (`ServeHealthServer`) serves these.

| Endpoint | Contract | Reader |
|---|---|---|
| `GET /healthz` | 200 while the process is up | `agentop service` and `doctor`. The URL comes from `listener.health_addr` after `config.ApplyPreset` (`resolveHealthURL` in `cmd/agentop/cmd_service.go`). When the config does not load, `agentop service` uses `builtinHealthURL` (`servicePaths.probeURL`) |
| `GET /readyz` | 200 when every plugin is ready. Otherwise 503 with the body `inbound plugin not ready: <name>` or `outbound plugin not ready: <name>` | `checkServing` in `cmd/agentop/cmd_doctor_checks.go`. `doctor` shows the first line of the body (`probe`, which reads up to 512 bytes) |

## The Host and Origin guard

When `listener.bind_loopback_only` is true (the built-in config sets it), the session
API and the stats server answer 403 to:

- a request whose `Host` is not `localhost`, `127.0.0.1` or `::1`. This stops DNS
  rebinding.
- a request with an `Origin` header. A browser always sends one.

`httpx.LoopbackOnly` in `core/listener/httpx/loopback.go` implements the guard.
`sessionapi.WithLoopbackOnly` and `observe.WithLoopbackOnly` apply it, from `main` in
`cmd/cortex/main.go`. The health server and the forward proxy do not have this guard.

## Hot-reload

agentop changes the running pipeline by writing the config file. It does not call an
API. The contract has these parts:

1. **The file.** The proxy watches the file it started with. On a local install that
   is `~/.cortex/config.yaml` (`localCortexConfig` in
   `cmd/agentop/local_endpoint.go`). The reloader watches the directory, not the file
   (`Reloader.Start` in `core/reloader/reloader.go`). So a rename into place is a
   change.
2. **The write.** agentop writes a temp file beside the config, syncs it, and renames
   it over the config (`Apply` in `cmd/agentop/edit/local.go`). It reads its own clock
   just before the rename. That time is the apply time.
3. **The debounce.** The reloader waits for 250 ms of quiet, then reloads once
   (`watchLoop`). A file with the same SHA-256 as the active config changes nothing,
   and `/reload/status` does not change (`reloadOnce`).
4. **Success.** The reloader sets `last_success` to the reload's start time, from the
   proxy's clock (`recordSuccess`). agentop reports success when `last_success` is after its
   apply time. The two clocks must agree, so the proxy and agentop must run on the
   same machine.
5. **Refusal.** A failed reload increments `reloads_failed` and sets `last_error`
   (`recordFailure`). The active pipeline continues to serve. agentop takes the
   counter's value at its first poll as the baseline, and reports a failure when the
   counter goes above it.
6. **Timeout.** agentop waits up to 30 s for a local file (`LocalPollDeadline` in
   `cmd/agentop/edit/edit.go`). Five transport errors in a row end the wait early as
   "unreachable".

`validateReloadable` in `core/reloader/reloader.go` refuses a reload that changes any
of these sections. They need a restart:

- `mode`
- `listener.*`
- `session.*` (this includes `session.archive`)
- `cost_ledger.*`
- `mtls.*`
- `tls_bridge.*`
- `spiffe.*`
- `memory.*`

A change to `pipeline:` or `pricing:` reloads. The proxy swaps the pricing table after it
accepts the reload (`applyPricing`, passed as `reloader.WithOnCommit` in
`cmd/cortex/main.go`). A change to `stats.address` is not refused, but the stats
listener binds once at startup (`config.ListenerPlan`). So the new address takes
effect at the next restart.

## Command line

`cmd/cortex/main.go` (`main`) parses these flags. agentop and `install.sh` use these
forms:

| Form | Used by | Contract |
|---|---|---|
| `cortex --version` | `agentop doctor`, `install.sh` | Prints one line: `cortex <version>`, or `cortex (cpex) <version>` for a cpex build (`versionLine` in `cmd/cortex/main.go`). agentop reads the last word of the first line as the version (`installedVersion` in `cmd/agentop/setup_step_binaries.go`). `doctor` reports a problem when it is not agentop's version (`checkBinaries`). agentop and `install.sh` look for ` (cpex) ` to find a cpex build (`installedIsCpex`) |
| `cortex --local --write-config` | agentop setup (`cmd/agentop/setup_step_config.go`) | Writes `~/.cortex/config.yaml` and its directory if the file is absent, then exits 0. It does not change a file that exists (`writeBuiltinConfig` in `cmd/cortex/local.go`) |
| `cortex --local --supervise` | agentop setup without a service (`cmd/agentop/setup_unsupervised.go`) | Runs the built-in config under the supervisor. agentop writes the pid to `~/.cortex/proxy.pid` and the output to `~/.cortex/proxy.log` |
| `cortex --supervise --config <path>` | the launchd plist (`cmd/agentop/cmd_service_platform.go`) | The parent starts a child with the same arguments, without `--supervise`, and restarts it when it exits (`runSupervisor` in `cmd/cortex/supervise.go`) |
| `cortex --config <path>` | the systemd unit (`cmd/agentop/cmd_service_platform.go`) | Runs the proxy with no supervisor. systemd restarts it (`Restart=on-failure`) |

Other rules:

- **The name.** The binary is `cortex` in `~/.local/bin`. `install.sh` (`proxy_running`)
  and agentop identify the process by that name. The constants are in
  `cmd/agentop/proxy_names.go`.
- **A local install.** A config under `~/.cortex` makes the process a local install,
  whichever flag started it (`startedFromLocalInstall` in `cmd/cortex/main.go`).
- **Stop.** The proxy must exit on SIGTERM. `install.sh --stop` sends SIGTERM, waits
  18 s, then sends SIGKILL (`stop_pidfile_proxy`). The systemd unit sets
  `TimeoutStopSec=5`.

## `install.sh` flags

`scripts/install.sh` parses these (the `for arg` loop and `usage`). `agentop update`
(PR #1676) will use them. Some flags go to `agentop setup` unchanged (`hand_off`).
The `hand_off` comment says that set is frozen: an older agentop must accept every
flag it sends.

| Flag | Effect | Passed to `agentop setup` |
|---|---|---|
| (none), `--local` | Download, check the checksums, stage, then run `agentop setup --from <stage>` | no (`--local` is setup's default) |
| `--install-only` | Install the binaries and PATH, then stop | `--install-only` |
| `--claude-code` | Also route Claude Code through Cortex | `--claude-code` |
| `--no-service` | Run the proxy as a background process, not under launchd or systemd | `--no-service` |
| `--no-modify-path` | Edit no shell profile | `--no-modify-path` |
| `--yes`, `-y` | Do not ask. Apply the changes setup lists | `--yes` |
| `--cpex` | Download `cortex-cpex_<ver>_<os>_<arch>.tar.gz`. macOS arm64 and Linux only (`cpex_platform_check`, `require_cpex_build`) | no |
| `--ref=REF` | Run the copy of this script at the git ref `REF`, such as `main` or a tag, and install that ref's binaries | no (removed before the re-run) |
| `--stop` | Stop the service and the pidfile proxy, check the ports, exit. Downloads nothing (`stop_cortex`) | no |
| `-h`, `--help` | Print the usage text | no |

An unknown flag stops the script with an error. With a download, `install.sh` also
sends `--from <stage>`, `--handoff-bytes=<n>` and `--handoff-seconds=<n>` to setup.
