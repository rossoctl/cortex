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
`praxis.yaml` in the current directory. The supervisor (#1720) writes
`gateway.yaml`. Until then, write it by hand.

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
10. Tunnel joins land with the hop (#1722).
11. One replay NDJSON file is the golden for both the Go ingestion and the Rust row builder.
12. The emitter has its own thread and a current-thread Tokio runtime.
13. `praxis-proxy` builds without default features, with vendored OpenSSL.
14. The memory measurement is a script, not a CI gate.
15. The filters use the Praxis 0.8.0 API. No fork, no patch.
16. `CortexObserve` keeps its exchange in filter state as `Arc<Mutex<Exchange>>`.
17. The ingest stream ends on Cortex shutdown, so restarts stay quick.

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
