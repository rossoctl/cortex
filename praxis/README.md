# praxis

This is the Rust workspace for praxis mode (#1714, phase A). Its crate,
`cortex-praxis-filters`, will hold the Cortex filters that run inside Praxis.
It now holds a port of the Go inference parser. It uses no Praxis API yet.

The workspace is outside `go.work`.

## Run the tests

From this directory:

```sh
cargo fmt --check
cargo clippy --all-targets --locked -- -D warnings
cargo test --locked
```

`rust-toolchain.toml` pins the toolchain (Rust 1.96.0, with `rustfmt` and
`clippy`). `rustup` installs it on the first `cargo` command.

## The parity rule

Go is the source of truth. A Go parser change regenerates `expected.json`
(`go test ./plugins/inferenceparser -run TestFixtures -update`), and this
suite then shows the Rust gap.

`tests/parity.rs` reads each case under
`core/plugins/inferenceparser/testdata/`. It feeds the body to the Rust parser
in several ways (whole, in small chunks, gzip-coded, with CRLF line endings).
Each result must equal the Go `expected.json`.

When the two differ, fix the Rust code. Do not change Go behavior or a Go
golden file to suit Rust.

A Go-only parser PR gets a report from this suite, not a block. The CI job
blocks only when the PR changes `praxis/` or its workflow.

## Go decode rules

`src/parse/inference/gojson.rs` reproduces the decode rules of Go's
`encoding/json` on purpose. Go matches keys without case, lets a later
duplicate key win, and reads `null` as "no change". `serde` does not. Do not
replace it with plain `serde` derives. `text.rs` does the same for invalid
UTF-8: Go replaces each invalid byte with U+FFFD.
