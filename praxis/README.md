# praxis

This is the Rust workspace for praxis mode (#1714). It has two crates:

- `cortex-praxis-filters` (`filters/`): the Cortex filters that run inside
  Praxis, a port of the Go inference parser (`parse`), and the row builder and
  emitter that send rows to Cortex (`record`).
- `cortex-gateway` (`server/`): the Praxis server with the Cortex filters
  registered (#1719).

The workspace is outside `go.work`.

## Run the tests

From this directory:

```sh
rustup toolchain install
cargo fmt --check
cargo clippy --all-targets --locked -- -D warnings
cargo test --locked
```

`rust-toolchain.toml` pins the toolchain (Rust 1.96.0, with `rustfmt` and
`clippy`). `rustup toolchain install`, run in this directory, installs it, as CI does.

## Run the gateway

```sh
cargo run --locked -p cortex-gateway -- -c gateway.yaml
```

The config path comes from `-c <path>`, then `PRAXIS_CONFIG`, then
`praxis.yaml` in the current directory. In praxis mode, `cortex -supervise`
writes `gateway.yaml` and starts the gateway (#1720). To run the gateway by
hand, write the file yourself.

`--exit-on-stdin-eof` makes the gateway exit when its stdin closes. The
supervisor uses it. Without the flag, a gateway that you start with
`</dev/null` keeps running.

The binary registers two filters. Put them first in the chain, in this order,
and add `access_log` after them:

- `cortex_attribution` (`token_file`): refuses a request without the token in
  `X-Cortex-Token` with 403, and removes the hop headers before the upstream.
- `cortex_observe` (`token_file`, `ingest` as `host:port`): records the request
  and response rows and streams them to `POST /v1/ingest`.

Without `access_log` in the chain, Praxis does not call the hook that records
an aborted or refused request, so that request has no response row.
`server/tests/testdata/gateway.yaml.tmpl` is a full example.

`server/tests/gateway.rs` starts the real binary between a mock origin and a
mock ingest endpoint. Run it with `cargo test --locked -p cortex-gateway`.

## Gateway decisions (#1719)

The plan `docs/superpowers/plans/2026-11-09-praxis-mode-2-gateway-and-ingest.md`
gives the full text.

1. The filters stay thin. All logic is in `record` and `parse`, which take plain values. End-to-end tests start the real binary.
2. Cortex reads the token file path from `CORTEX_GATEWAY_TOKEN_FILE`. The gateway reads it from the `token_file` filter config key. Cortex never creates the file.
3. The Go `praxis` config block has only `enabled` for now.
4. Ingest counters are at `GET /v1/ingest/stats` on the session API, in praxis mode only.
5. `X-Cortex-Attribution` is base64url JSON without padding. A missing or bad header gives the default attribution.
6. Fields that the gateway cannot know in v1 stay unset (`identity`, `tls`, `invocations`, and others).
7. `client`, `subagent` and `parentSubagent` are ported to Rust, pinned by a Go golden table.
8. `client_canceled` and `upstream_error` messages are Rust text. Only the message differs from Go.
9. Ingestion keeps the request id of a kept request capture for up to 1 hour, until its response line arrives.
10. Tunnel joins land with the hop (#1721).
11. One replay NDJSON file is the golden for both the Go ingestion and the Rust row builder.
12. The emitter has its own thread and a current-thread Tokio runtime.
13. `praxis-proxy` builds without default features, with vendored OpenSSL.
14. The memory measurement is a script, not a CI gate.
15. The filters use the Praxis 0.8.0 API. No fork, no patch.
16. `CortexObserve` keeps its exchange in filter state as `Arc<Mutex<Exchange>>`.
17. The ingest stream ends on Cortex shutdown, so restarts stay quick.
18. A stream cut after the response head is `client_canceled`, also when the upstream cut it. Praxis 0.8.0 gives no error source after the head.
19. A response row leaves out the request-only inference fields (`messages`, `tools`, `toolResults`, `historyToolCalls`, `historyThinking`). Cortex puts them back from the request row (`docs/session-api.md`, line format v1).
20. The gateway removes `X-Cortex-Dest` and `X-Cortex-Token` before the upstream, against the issue text. No egress listener exists yet, so they would send the token to the origin. #1721 keeps them again for its egress listener.
21. A buffered response body that goes over the 10 MiB cap after the response head gives a row with the upstream status, not 502. The client has that status and a cut body. Direct mode reads the body before it sends the head, so its client and its row get 502. A `Content-Length` over the cap gives 502 in both modes.

## Accepted edge cases (#1719)

These come from the PR #1717 review. This PR accepts them:

1. **A restart loses the erase marks.** They are in memory. A line that the
   gateway queued before an erase, and sends after a Cortex restart, can bring
   the erased session back.
2. **The erase check and the append are two steps.** An erase between them
   lets that one line create the session again. A full fix checks under the
   store's lock.
3. **A stream longer than 10 minutes can record after an erase.** Its
   response line arrives after the erase mark expired. Direct mode has the
   same window for a response in flight at an erase.
4. **`at` is set when Cortex appends the row.** So a request that names the
   session, and arrives before an earlier request that is still pending, can
   change which rows are linked to the session.

## Supervision decisions (#1720)

The plan `docs/superpowers/plans/2026-11-09-praxis-mode-3-supervise-gateway.md`
gives the full text.

1. `cortex -supervise` runs `cortex` and `cortex-gateway` as two siblings. Each child has its own restart loop, so a `cortex` restart does not stop the gateway.
2. The command line is `cortex-gateway -c <dir>/gateway.yaml --exit-on-stdin-eof`. `<dir>` is the directory of the `--config` file, `~/.cortex/` for the service. The binary is in the same directory as `cortex`.
3. The gateway's stdin is a pipe from the supervisor. When the supervisor exits for any reason, SIGKILL included, the pipe closes and the gateway exits. This is the only orphan guard, on Linux and macOS. No `Pdeathsig`.
4. To stop the gateway, the supervisor closes the pipe. After `superviseStopGrace` it sends SIGKILL.
5. At each start, the supervisor writes a new `gateway.token` (32 random bytes, hex, mode 0600) and `gateway.yaml`. It writes each file to a temp file and then renames it. The `cortex` child gets `CORTEX_GATEWAY_TOKEN_FILE`.
6. The gateway gets an empty env, so no proxy variable can get to it.
7. `gateway.yaml` listens on `praxis.listen` (default `127.0.0.1:47605`). A catch-all route sends all traffic to `praxis.egress` (default `127.0.0.1:47606`). The egress listener is #1721. `ip_acl` allows only `127.0.0.1/32` and `::1/128`.
8. A missing gateway binary, or a gateway that exits, gets the normal backoff. The supervisor does not stop, and `cortex` keeps running.
9. The supervisor does not stop a gateway from an earlier run. The stdin pipe ends that gateway when its supervisor exits, so a new gateway that cannot bind `praxis.listen` binds it on the next try. A config that `config.Validate` refuses starts no gateway.
10. Not in this step: the admin metrics port, the startup probe and `gateway_unavailable` (#1721), and the version check of the gateway (#1723).
11. Direct mode does not change. The supervisor starts only `cortex` and writes no gateway file.

## The parity rule

Go is the source of truth. A Go parser change regenerates `expected.json`,
and this suite then shows the Rust gap. To regenerate, from this directory:

```sh
GOWORK=off go -C ../core test ./plugins/inferenceparser -run TestFixtures -update
```

`filters/tests/parity.rs` reads each case under
`../core/plugins/inferenceparser/testdata/`. It feeds the body to the Rust parser
in several ways (whole, in small chunks, gzip-coded, with CRLF line endings).
Each result must equal the Go `expected.json`.

When the two differ, fix the Rust code. Do not change Go behavior or a Go
golden file to suit Rust.

A Go-only parser PR gets a report from this suite, not a block. The CI job
blocks only when the PR changes `praxis/` or its workflow.

## Go decode rules

`filters/src/parse/inference/gojson.rs` reproduces the decode rules of Go's
`encoding/json` on purpose. Go matches keys without case, lets a later
duplicate key win, and reads `null` as "no change". `serde` does not. Do not
replace it with plain `serde` derives.
`filters/src/parse/inference/text.rs` does the same for invalid UTF-8: Go
replaces each invalid byte with U+FFFD.
