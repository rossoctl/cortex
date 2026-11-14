# PPE 3/5: Record PPE policy decisions in the gateway's rows (#1799)

Source: the plan in the body of issue #1799 (parent #1785). This file copies that plan
and adds the decisions that the issue leaves open. Each decision has its reason.

Base: `4e4708d9a43654ca04a730f446b346437c2c9e01` (upstream `main`, 2026-11-13, the merge
of #1858).
Branch: `feat/ppe-decisions-1799`.

## Goal

Put each PPE decision in the session row of its request. A denied request must show its
violation code and not only `gateway_refused`. agentop already renders
`invocations.outbound`, so this plan changes the gateway only.

## Scope

In scope: a Rust audit sink of kind `cortex/recorder`, its bounded store, the response
row, and the Go end-to-end tests.

Not in scope:

- Labels and taints in the row.
- A wrapper that adds `cortex/recorder` to a policy file automatically. #1800 owns the
  policy documentation.
- Any change to the Go `/v1/ingest` decoder or to agentop (see Decision 6).

## Facts checked in the code at the base commit

- The gateway registers two HTTP filters, `cortex_attribution` and `cortex_observe`
  (`praxis/server/src/main.rs`). `cortex_observe` writes the request row and one response
  row.
- `cortex_observe` reads the id from `X-Cortex-Request-Id`, or from the filter context
  when the header is absent (`REQUEST_ID_HEADER`,
  `praxis/filters/src/filters/observe.rs`). It sets the id again in the reserved
  `x-praxis-cortex-request-id` header (`JOIN_HEADER`), because the proxy strips the hop
  headers before `policy` runs. The proxy strips every `x-praxis-*` header before the
  upstream, and admission refuses a client that sends a reserved name.
- `cortex_observe::submit` is the one place that sends a `Line` to the emitter. Three
  call sites use it: `on_request_body` (request row), `finish` (response row), and
  `emit_deferred_record` (response row).
- `Event` (`praxis/filters/src/record/line.rs`) has no `invocations` field. The list
  `NOT_SET_IN_PRAXIS_MODE` names `invocations` as a field that praxis mode does not set.
- Two tests depend on that list:
  - `praxis/filters/src/record/line.rs`, test `every_session_event_field_is_set_or_listed`:
    a field must be set by `full_event()` or listed in `NOT_SET_IN_PRAXIS_MODE`, not both.
  - `praxis/filters/tests/row_parity.rs`: `skipped()` extends the skip list with
    `NOT_SET_IN_PRAXIS_MODE` and compares the Rust row with a Go replay row.
- The Go replay rows in `core/sessionapi/testdata/ingest/replay.ndjson` carry
  `invocations`, because direct mode runs Go plugins. The Rust row builder has no policy,
  so it writes none.
- The Go wire type already carries the field: `sessionEventWire.Invocations`
  (`core/model/session.go`) and its `UnmarshalJSON` copies it. `pipeline.Invocations` and
  `pipeline.Invocation` are in `core/model/extensions.go`.
- `praxis-proxy-filter` 0.8.0 exports `register_policy_plugin_factory`, `PolicyFilter`,
  `PolicyPluginFactoryFn` and `fips_crypto_blocker`. It does not re-export the `ppe`
  crate. The sink types come from `praxis-policy` 0.6.0: `prelude::Plugin`,
  `prelude::PluginFactory`, `prelude::PluginInstance`, `prelude::Extensions`, and
  `praxis_policy_core::audit::AuditHandler`.
- `AuditHandler::handle` gets `&dyn PluginPayload`, `&Extensions` and `&DecisionLog`. The
  engine filters `Extensions` for each sink by the capabilities in that sink's `plugins:`
  entry. `read_headers` keeps `ext.http.request_headers`.
- `DecisionLog::verdict()` gives `Option<&Verdict>`. `Verdict::Deny` carries
  `PluginViolation`, whose `code` is the violation code.
- The engine calls the sinks after the assertions and before it returns the result, for an
  allow, a deny and a route-resolution deny (`engine.rs`, `finish`).
- The existing e2e tests already write a policy file through `chainOpts{policy: ...}`
  (`cmd/cortex/praxis_e2e_test.go`). `newPraxisChain` writes the file and sets
  `praxis.policy.config_path`.
- agentop reads `Invocations.Outbound` (`cmd/agentop/tui/app.go`) and shows `action` and
  `reason`. No agentop change is necessary.
- Toolchain: `praxis/rust-toolchain.toml` names 1.96.0. Homebrew `cargo` ignores that
  file. Run `rustup run 1.96.0 cargo ...`.

## Decisions made for this plan

1. **One process-wide store, in `policy_recorder.rs` (T1).** The sink runs inside the
   policy filter's engine, and `cortex_observe` runs in the same process. A static
   `Mutex` is the only shared place both can reach. Reason: the issue requires a join by
   request id across two filters, and this process serves one gateway.
2. **The store lives in `praxis/filters`, keyed by request id (T1).** The observe filter
   is in the same crate, so it can call `take`. The server crate holds only the
   registration line. Reason: the crate that writes the field must also read the store,
   and that keeps `cortex-gateway` unchanged apart from `main.rs`.
3. **Dependency `praxis-policy = { version = "=0.6.0" }` with default features (T1).**
   `praxis-proxy-filter` does not re-export the sink traits, so the crate needs its own
   dependency. Keep the default feature `crypto-rustcrypto`: the filters crate's tests
   construct a policy engine without the server crate, and an engine with no crypto
   backend cannot hash. The gateway's `policy-crypto-default` selects the same backend, so
   feature unification gives one backend and no double-backend compile error. Leave the
   filters crate's `praxis-proxy-filter` dependency as it is
   (`default-features = false`): the crate never names `register_policy_plugin_factory`,
   which is behind that crate's `policy-engine` feature, and only `praxis/server` calls
   it, where `praxis-proxy`'s `policy-engine` feature enables it.
4. **The sink writes an entry on every `handle` call, allow or deny (T1).** The plan
   in the issue asks for an `allow` entry. A deny replaces an allow for the same request
   id, because the last call is the verdict the caller got.
5. **The store caps at 4096 ids and evicts the oldest insertion (T1).** `take` removes
   the id from the order queue too, so an evicted or taken id is never visited twice.
   Reason: the issue requires a bound, and a response row always takes its entry, so the
   normal case never reaches the cap.
6. **No Go production change (T3).** `sessionEventWire` already has `Invocations`, and its
   `UnmarshalJSON` copies it. The e2e test proves the round trip. Reason: requirement 6
   says "fix it if it does not" — it does.
7. **`row_parity.rs` gets a local skip for `invocations` (T2).** The replay rows come from
   direct mode, where Go plugins recorded invocations. The parity run has no policy, so
   the Rust row has none. Reason: the parity test compares the row builder without a
   policy, and requirement 5 says a row does not change without a policy.
8. **The first test is engine-level, not filter-level (T1).** `praxis-proxy-filter`'s
   request harness (`test_utils`) is `pub(crate)`, so a test in this repository cannot
   drive `PolicyFilter` with a request. The test loads the same policy document into a
   `PolicyEngine` and invokes `http.request`. Reason: this runs the real engine, the real
   factory registration and the real verdict emit, which is the risk the issue names.
   The Go e2e test (T3) proves the live call through the real gateway binary.
9. **The registration call goes just before the server starts (T1).** `main.rs` calls
   `praxis::run_server_with_registry` last. Put the call one line before it, with a
   comment. Reason: the policy filter is constructed during startup, so the registration
   must come first.
10. **One PR.** The estimated counted lines are about 720, below the 2000 limit.
11. **The join id also goes in the reserved `x-praxis-cortex-request-id` header
    (T1/T2).** The proxy removes the hop headers before the policy filter runs, so the
    recorder cannot see `x-cortex-request-id` on the live path. `cortex_observe` sets
    `JOIN_HEADER` for the policy filter, and the recorder falls back to the hop header.
    praxis-proxy-core 0.8.0 lists `x-praxis-` in `RESERVED_HEADER_PREFIXES`, strips
    these headers from the upstream request, and admission refuses a client that sends a
    reserved name. Residual risk: a praxis upgrade that stops passing filter-set headers
    to later filters, or stops stripping `x-praxis-*` before the upstream, empties
    `invocations` again. Only the T3 e2e test guards this.

## Tasks

Task ids are T1 to T4. "Depends on:" names the tasks whose code a task needs.

### T1: The recorder sink and its store

Depends on: none.

Files:

- `praxis/filters/Cargo.toml`: add
  `praxis-policy = { version = "=0.6.0" }`.
- `praxis/filters/src/lib.rs`: add `pub mod policy_recorder;`.
- `praxis/filters/src/policy_recorder.rs` (new):
  - `pub const KIND: &str = "cortex/recorder";`
  - `pub struct Decision { pub action: &'static str, pub reason: String }`.
  - A static store: `HashMap<String, Decision>` plus `VecDeque<String>` for insertion
    order, in one `Mutex`, with `std::sync::LazyLock`. The cap is 4096.
  - `pub fn record(request_id: &str, decision: Decision)`.
  - `pub fn take(request_id: &str) -> Option<Decision>`.
  - `pub struct CortexRecorder { config: PluginConfig }`. Implement
    `Plugin::config(&self) -> &PluginConfig` and
    `Plugin::as_audit_handler(self: Arc<Self>) -> Option<Arc<dyn AuditHandler>>`,
    which returns `Some(self)`. `AuditHandler::handle` is
    `async fn handle(&self, _payload: &dyn PluginPayload, ext: &Extensions, log: &DecisionLog)`:
    - read `x-praxis-cortex-request-id`, then `x-cortex-request-id`, from
      `ext.http.request_headers` with a case-insensitive key compare. The reserved copy
      is the live path; the hop header keeps the direct engine test valid. Return with
      no write when neither header is present or empty.
    - `DecisionLog::verdict()`: a `Deny(v)` gives
      `action: "deny", reason: v.code`. Any other verdict gives
      `action: "allow", reason: String::new()`.
    - call `record`.
  - `pub struct CortexRecorderFactory` with `PluginFactory::create`, which returns a
    `PluginInstance` with the plugin and no handlers. A sink declares no hooks and loads
    from a policy that names only its `kind:`.
  - `#[cfg(test)] mod tests`:
    - Record then take gives the decision back.
    - A deny replaces an allow.
    - A take empties the entry.
    - The 4097th id evicts the first.
- `praxis/filters/src/filters/observe.rs`: in `on_request_head`, push the id into
  `ctx.request_headers_to_set` as `x-praxis-cortex-request-id` (`JOIN_HEADER`). The
  policy filter runs after the proxy removes the hop headers, so the recorder needs this
  reserved copy. Keep the id also on the hop header, as it is today.
- `praxis/server/src/main.rs`: before `praxis::run_server_with_registry`, call
  `praxis_filter::register_policy_plugin_factory(cortex_praxis_filters::policy_recorder::KIND, std::sync::Arc::new(|| Box::new(cortex_praxis_filters::policy_recorder::CortexRecorderFactory)))`.
- `praxis/filters/tests/policy_recorder.rs` (new): the engine test.
  - Policy document:
    ```yaml
    plugins:
      - name: cortex-recorder
        kind: cortex/recorder
        capabilities: [read_headers]
    routes:
      - llm: model-ok
        authorization:
          pre_invocation:
            - "llm.provider == 'never': deny('unreachable', 'never')"
      - llm: "*"
        authorization:
          pre_invocation:
            - "deny('model is not permitted', 'model_not_allowed')"
    ```
  - Imports: `praxis_policy::PolicyEngine`,
    `praxis_policy::prelude::{Extensions, PluginFactory}`,
    `praxis_policy::praxis_policy_core::http_hook::{HttpHook, HttpPayload}`. The facade
    re-exports the whole `praxis_policy_core` crate, so this path is nameable with only
    the `praxis-policy` dependency.
  - Build `PolicyEngine::default()`, call
    `register_factory(KIND, Box::new(CortexRecorderFactory))`, `load_config_yaml`,
    `initialize().await`.
  - Invoke `mgr.invoke_named::<HttpHook>("http.request", HttpPayload, extensions, None)`.
    `extensions` is `Extensions` with `meta.entity_type = Some("llm".into())`,
    `meta.entity_name = Some("model-bad".into())` (or `"model-ok"`), and
    `http.request_headers` holding `("x-cortex-request-id", "r1")`. `HttpExtension` and
    `MetaExtension` hold these fields; `Extensions.http` and `Extensions.meta` are
    `Option<Arc<..>>`.
  - Assert: `model-bad` gives a deny and `take("r1")` gives
    `{action: "deny", reason: "model_not_allowed"}`. `model-ok` gives an allow and a
    second id gives `{action: "allow", reason: ""}`. A call with no request-id header
    leaves the store empty.
  - A second test sets only `x-praxis-cortex-request-id` (`JOIN_HEADER`) and proves the
    reserved copy is read. This is the live path; the hop-header test above stays.
  - Write this test before the rest of T1's code. If `handle` does not run on a deny,
    record that as a finding in the PR description and stop. Do not change the design.

Test command: `rustup run 1.96.0 cargo test --locked -p cortex-praxis-filters` from
`praxis/`.

### T2: The decision in the response row

Depends on: T1.

Files:

- `praxis/filters/src/record/line.rs`:
  - Add `Invocation { plugin: String, action: String, reason: String }` with
    `#[serde(skip_serializing_if = "String::is_empty")]` on `reason`, and derive
    `Debug, Clone, Default, Serialize` (`Event` derives `Debug, Clone`).
  - Add `Invocations { outbound: Vec<Invocation> }` with the same skip on `outbound`,
    and the same derives.
  - Add `pub invocations: Option<Invocations>` to `Event`, with
    `#[serde(skip_serializing_if = "Option::is_none")]`.
  - Remove `"invocations"` from `NOT_SET_IN_PRAXIS_MODE`. Put this comment in its place:
    `// Only a policy decision sets this (policy_recorder).`
  - Add `invocations: Some(...)` to `full_event()` so the drift test sees the field set
    once.
- `praxis/filters/src/policy_recorder.rs` (changed here, not in T1): add
  `pub(crate) fn attach(line: &mut Line)`. When `line.event.phase == "response"`, it
  calls `take(&line.request_id)` and, on `Some(decision)`, sets
  `line.event.invocations = Some(Invocations { outbound: vec![Invocation { plugin:
  "policy".into(), action: decision.action.into(), reason: decision.reason }] })`. On any
  other phase it changes nothing. This function needs the `Event.invocations` type that
  this task adds, so it stays in T2.
- `praxis/filters/src/filters/observe.rs`:
  - Change `submit(&self, line: &Line)` to `submit(&self, mut line: Line)`.
  - In `submit`, call `crate::policy_recorder::attach(&mut line)` before `line.queued()`.
  - Change the three call sites to pass the line by value.
- `praxis/filters/tests/row_parity.rs`: add `"invocations"` to the local `skipped()`
  vector with this comment: `// Direct mode's Go plugins record these; this run has no policy.`

Tests, in the `#[cfg(test)] mod tests` of `policy_recorder.rs`:

- `attach` on a `"response"` line with a recorded deny sets exactly one outbound entry
  with `plugin: "policy"`, `action: "deny"`, the code in `reason`, and the store is empty
  after it.
- `attach` on a `"response"` line with no stored decision leaves `invocations` as `None`.
- `attach` on a `"request"` line sets nothing and keeps the entry in the store.
- An allow entry serializes without `reason`, which matches Go's `omitempty`.
- `row_parity_matches_replay` passes.
- `every_session_event_field_is_set_or_listed` passes.

`observe::submit` is private, so no unit test drives it. The Go e2e test in T3 proves
that `submit` calls `attach` on the live path.

### T3: Go end-to-end tests

Depends on: T2.

File: `cmd/cortex/praxis_e2e_test.go`.

- Add a const that holds the sink declaration:
  ```go
  const e2eRecorderPlugins = `plugins:
    - name: cortex-recorder
      kind: cortex/recorder
      capabilities: [read_headers]
  `
  ```
  The test passes `e2eRecorderPlugins + e2ePolicy` (and `+ e2eToolPolicy`) as
  `chainOpts.policy`.
- Add `TestPraxisE2E_PolicyDecisionInRow`:
  - `model-ok`: status 200. Its response row has
    `invocations.outbound == [{plugin: "policy", action: "allow"}]`.
  - `model-bad`: status 403. Its response row has
    `invocations.outbound == [{plugin: "policy", action: "deny", reason: "model_not_allowed"}]`.
  - With `e2eToolPolicy`, a `tools/call` for `danger_tool` (status 200, JSON-RPC error
    -32001). Its response row has the same shape with `reason: "tool_not_allowed"`.
  - Use one chain and one request for each case, so the last response row belongs to
    that request: read it with
    `lastResponse(t, c.waitResponses(t, 1, 10*time.Second))` and inspect its
    `Invocations.Outbound`.
  - For the request-row check, read `eventsJSON(t, c)` after `c.waitResponses` and select
    the row with `phase == "request"`. Assert `invocations` is absent on it. Do not
    select rows by `inference.model` or `mcp.name`: the deferred deny row is built from
    the exchange state, and the test must not depend on which request fields survive a
    deny.
- Update the comment in `TestPraxisE2E_SameEventsAsDirect` that says the gateway runs no
  Cortex plugin. The run has no policy, and requirement 5 keeps the rows unchanged. Keep
  the assertion that `invocations` is absent.

Run: the e2e part of the Gate command in this file. It runs exactly these tests.

### T4: The praxis README

Depends on: none.

File: `praxis/README.md`, "Gateway decisions" item 6. Remove `invocations` from the list
of fields the gateway cannot know. Add one sentence: the response row carries one
`invocations.outbound` entry when the policy file declares a plugin of kind
`cortex/recorder` with capability `read_headers`, as
`{plugin: "policy", action: "allow"|"deny", reason: <violation code>}`. Without a policy,
or without that declaration, the row does not change.

## PR list

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | Feat: Record PPE policy decisions in the gateway's rows | "" | T1, T2, T3, T4 | 730 |

## Gate

Run from the worktree root. One command. It runs the Rust gates of the `Praxis` CI
workflow and the Go vet and e2e steps.

```
rustup run 1.96.0 cargo fmt --all --manifest-path praxis/Cargo.toml --check && rustup run 1.96.0 cargo clippy --manifest-path praxis/Cargo.toml --all-targets --locked -- -D warnings && rustup run 1.96.0 cargo test --manifest-path praxis/Cargo.toml --locked && rustup run 1.96.0 cargo build --locked -p cortex-gateway --manifest-path praxis/Cargo.toml && TAGS=$(GOWORK=off go -C scripts/profile-tags run . local) && CORTEX_GATEWAY_BIN=$PWD/praxis/target/debug/cortex-gateway GOWORK=off go -C cmd/cortex test -count=1 -timeout 15m -tags "praxis_e2e,$TAGS" -run TestPraxisE2E . && GOWORK=off go -C cmd/cortex vet ./...
```

The `--all` on the fmt step is required, and it was checked on this base. Without it,
`cargo fmt --manifest-path praxis/Cargo.toml --check` exits 1 with `Failed to find
targets` and formats nothing. With `--all` it checks every package of the workspace that
`cargo fmt --check`, run in `praxis/`, checks (verified: exit 0 on this base). The other
steps are the CI forms; `cargo fmt --all --check --manifest-path praxis/Cargo.toml` was
run once from the worktree root and passed, and `GOWORK=off go -C cmd/cortex vet ./...`
was run once from the worktree root and passed.

## For a human after review

None of the tasks in this plan needs a human. These steps are not tasks:

1. After P1 merges, close #1799 and tick its row in the #1785 checklist. It needs P1.
2. Run the live check on a laptop (the PPE policy path with a real agent), in the same
   way as #1799's parent checklist asks. This needs a desktop session and real
   credentials, so it is not part of this workflow. It needs P1.
