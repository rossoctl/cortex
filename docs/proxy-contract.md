# The local proxy's contract

`agentop` and `scripts/install.sh` drive the local Cortex proxy (`cortex`) through
ports, HTTP endpoints, a config file and a command line. This page lists each part
they rely on. A replacement for the proxy must keep every item here, or change its
callers in the same PR.

Each item names the code that implements it, so you can check it. The session API
on `:47601` has its own reference: [`session-api.md`](session-api.md).

## Ports

The built-in config pins every listener to `127.0.0.1` (`builtinConfigYAML` in
`cmd/cortex/local.go`). `install.sh` probes 47600, 47601, 47602 and 47604 (`DEMO_*_PORT`), and so
does agentop setup (`cortexPorts` in `cmd/agentop/setup_probe.go`). Neither one
probes 47603.

| Port | Config key | Serves |
|---|---|---|
| 47600 | `listener.forward_proxy_addr` | The forward proxy, the `HTTPS_PROXY` target. While the child is down, the `--supervise` parent answers `502 Cortex proxy is not serving` here (`cmd/cortex/fallback.go`) |
| 47601 | `listener.session_api_addr` | The session API ([`session-api.md`](session-api.md)). agentop reads the address from the config (`localSessionEndpoint` in `cmd/agentop/local_endpoint.go`) |
| 47602 | `stats.address` | The stats server (see below). It also serves `/debug/pprof/*` because the config sets `bind_loopback_only` (`WithPprof` in `core/observe/statserver.go`) |
| 47603 | `listener.transparent_proxy_addr` | Nothing on a local install. The built-in config pins the port, but `config.ListenerPlan` skips the transparent listener when the config is under `~/.cortex` |
| 47604 | `listener.health_addr` | `/healthz` and `/readyz` (see below) |

A port that another process holds stops the proxy before the cost ledger and the
session archive open (`reserveListeners`, called from `main` in `cmd/cortex/main.go`).
The `--supervise` parent then retries, less often over time, and the proxy starts when
the port is free (`restartPolicy` in `cmd/cortex/supervise.go`). The child tells the
parent about a held port with exit status 3 (`exitPortInUse` in
`cmd/cortex/portconflict.go`). That status is internal to `cmd/cortex`. Do not depend
on it.

## Stats endpoints (`:47602`)

`core/observe/statserver.go` (`NewStatServer`) serves these. agentop finds the server
at `http://localhost:47602/` (`defaultCortexStatsURL` in
`cmd/agentop/local_endpoint.go`). `agentop exec` takes `--cortex-stats-url` to change
it, and `agentop pricing` takes `--stats-url`. The pipeline editor and `doctor` read
`stats.address` from the config instead.

| Endpoint | What agentop reads | Reader |
|---|---|---|
| `GET /reload/status` | `last_success`, `reloads_ok`, `reloads_failed`, `last_error` (shape: `Status` in `core/reloader/status.go`) | `edit.PollUntilReloaded` in `cmd/agentop/edit/status.go`. `localStatsUp` in `cmd/agentop/local_endpoint.go` requires a 2xx with a `reloads_ok` field |
| `GET /config` | `listener.forward_proxy_addr`, `tls_bridge.mode`, `tls_bridge.ca_dir`, `praxis.enabled`. It ignores all other fields | `runningConfig` and `runningConfigFields` in `cmd/agentop/local_endpoint.go`, for `agentop exec` and `praxisRefusal` |
| `GET /pricing/table` | The table (`describeBody`). With `?host=<gateway>`, the rates for that host (`effective`) | `cmd/agentop/cmd_pricing.go`. A 404 means a proxy older than the endpoint. `Registry.Handler` in `core/cost/pricing/handler.go` serves it |
| `GET /tls-bridge/unread` | `tlsbridge.UnreadReport` (`programs`, `hosts`, `osTrustsCA`) | `fetchUnread` in `cmd/agentop/doctor_unread.go`. A non-200 or no route makes the check silent |

**Fixtures.** Each endpoint above has a JSON fixture beside its handler. The
fixture holds the keys that agentop reads:

- `core/reloader/testdata/reload-status.json`
- `core/observe/testdata/config.json`
- `core/cost/pricing/testdata/pricing-table.json` and `pricing-effective.json`
- `core/tlsbridge/testdata/unread.json`

A test beside each handler checks that its real output has every fixture key, with
the same JSON type. `cmd/agentop/wire_fixture_test.go` decodes the same files into
agentop's types and refuses unknown keys. It then marshals each value again and
requires the fixture back. Thus a renamed JSON tag on either side fails a test.

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
- a request with an `Origin` header.

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
   (`watchLoop`). The reloader hashes the file together with the fingerprint of the
   script plugin folders (`Reloader.hash`, set by `reloader.WithExtraWatch` in
   `cmd/cortex/main.go`). When the hash equals the active one, nothing changes, and
   `/reload/status` does not change (`reloadOnce`).
4. **Success.** The reloader sets `last_success` to the reload's start time, from the
   proxy's clock (`recordSuccess`). agentop reports success when `last_success` is after its
   apply time. The two clocks must agree, so the proxy and agentop must run on the
   same machine.
5. **Refusal.** A failed reload increments `reloads_failed` and sets `last_error`
   (`recordFailure`). The active pipeline continues to serve. agentop reads the
   counter before it writes, and uses that value as the baseline (`ReloadBaseline` in
   `cmd/agentop/edit/status.go`). When `/reload/status` does not answer before the
   write, agentop takes the value at its first poll. When a poll reads a value below
   the baseline, the proxy restarted, and that value becomes the baseline. agentop
   reports a failure when the counter is more than the baseline.
6. **Timeout.** agentop waits up to 30 s for a local file (`LocalPollDeadline` in
   `cmd/agentop/edit/edit.go`). Five failed polls in a row (a transport error or a non-200 answer) end the wait early as
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
- `stats.*`

A change to `pipeline:` or `pricing:` reloads. The proxy swaps the pricing table after it
accepts the reload (`applyPricing`, which `main` in `cmd/cortex/main.go` gives to
`reloader.WithOnCommit`).

**A `stats.address` edit.** The stats listener binds once at startup
(`config.ListenerPlan`), so the reloader refuses a reload that changes `stats.*`.
`/reload/status` reports the error, and `/config` keeps the old address. The listener
stays on the old address until a restart. Until then, the file differs from the active
config in `stats.*`, so the reloader also refuses every later reload of the file. And
agentop reads `stats.address` from the file, where nothing answers:

- The TUI pipeline editor finds no local target and does not edit
  (`localEditTargets` in `cmd/agentop/local_endpoint.go`).
- `agentop server` and `agentop script` write the file, but do not wait for a reload.
  They say that no Cortex answered and that the change applies at the next start
  (`serverTarget` in `cmd/agentop/cmd_server.go`, `WriteNotRunning` in
  `cmd/agentop/edit/pluginwrite.go`). That is correct: the running proxy refuses the
  reload, because the file still has the changed `stats.*`.
- `doctor` does not show the unread-programs check (`checkUnread` in
  `cmd/agentop/doctor_unread.go`).

## Command line

`cmd/cortex/main.go` (`main`) parses these flags. agentop and `install.sh` use these
forms:

| Form | Used by | Contract |
|---|---|---|
| `cortex --version` | `agentop doctor`, `agentop update`, `install.sh` | Prints one line: `cortex <version>`, or `cortex (cpex) <version>` for a cpex build (`versionLine` in `cmd/cortex/main.go`). agentop reads the last word of the first line as the version (`installedVersion` in `cmd/agentop/setup_step_binaries.go`). `doctor` reports a problem when it is not agentop's version (`checkBinaries`). `update` downloads nothing when it is agentop's version and the release asked for (`currentRelease` in `cmd/agentop/cmd_update.go`). A cpex build has ` (cpex) ` in this line. agentop (`installedIsCpex`) and `install.sh` find a cpex build by that text |
| `cortex --local --write-config` | agentop setup (`cmd/agentop/setup_step_config.go`) | Writes `~/.cortex/config.yaml` and its directory if the file is absent, then exits 0. It does not change a file that exists (`writeBuiltinConfig` in `cmd/cortex/local.go`) |
| `cortex --local --supervise` | agentop setup without a service (`cmd/agentop/setup_unsupervised.go`) | Runs `~/.cortex/config.yaml` under the supervisor. It writes the built-in config there first only when the file is absent. agentop writes the pid to `~/.cortex/proxy.pid` and the output to `~/.cortex/proxy.log` |
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

`scripts/install.sh` parses these (the `for arg` loop and `usage`). For `--ref=main`
(any ref that does not start with `v`), `agentop update` runs the website `install.sh`
with some of them (`updateFlags` in
[`cmd/agentop/cmd_update.go`](../cmd/agentop/cmd_update.go)). Some flags go to
`agentop setup` unchanged (`hand_off`).
For a release tag (the newest release, or `--ref=vX`), `agentop update` does not run
`install.sh`. It downloads and checks the tarballs itself, and checks the
build-provenance attestation of each one (see *Re-running the installer is safe* in
[`laptop-service.md`](laptop-service.md)). It gets each bundle from
`https://api.github.com/repos/rossoctl/cortex/attestations/sha256:<digest>` with no
login. It sends `GH_TOKEN` or `GITHUB_TOKEN` when one is set, for the rate limit. Then
it runs `<stage>/agentop setup --from <stage>` with the `hand_off` set: `--claude-code`,
`--yes`, `--no-service`, `--install-only`, `--no-modify-path`, `--praxis`, plus
`--handoff-bytes` and `--handoff-seconds`. `--cpex` selects the `cortex-cpex_` tarball,
and `--praxis` adds the `cortex-gateway_` tarball.
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
| `--praxis` | Also download `cortex-gateway_<ver>_<os>_<arch>.tar.gz`, check it against `checksums.txt` and stage `cortex-gateway`. Setup then turns on praxis mode. macOS arm64 and Linux only (`praxis_platform_check`, `require_praxis_build`). Not frozen: every release that `require_praxis_build` accepts has an agentop that takes `--praxis` | `--praxis` |
| `--ref=REF` | Install the binaries of `REF`: `main` (the developer channel) or a release tag. For any other ref, such as a branch or a SHA, the script warns and installs the newest release's binaries (`resolve_version`). A piped run also runs the copy of this script at `REF`. A run from a local file keeps that file | no (removed before the re-run) |
| `--stop` | Stop the service and the pidfile proxy, check the ports, exit. Downloads nothing (`stop_cortex`) | no |
| `-h`, `--help` | Print the usage text | no |

An unknown flag stops the script with an error. With a download, `install.sh` also
sends `--from <stage>`, `--handoff-bytes=<n>` and `--handoff-seconds=<n>` to setup.
