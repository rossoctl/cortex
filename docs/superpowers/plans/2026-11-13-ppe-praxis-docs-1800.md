# PPE 4/5: Document PPE in praxis mode and deprecate the laptop CPEX build (#1800)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `docs/praxis-policy.md` with a migration section, add a Rust test that
loads every policy example in that page, and print one deprecation line for the laptop
CPEX build.

**Architecture:** Three tasks. T1 writes the page and its three links. T2 adds the test
that pins the page's five policy examples to the real PPE 0.6.0 engine. T3 adds the
deprecation copy to the installer, the binary, and three existing documents. T2 needs
T1. T3 needs nothing. All three tasks go in one PR.

**Tech Stack:** Markdown, Rust 1.96 (workspace `praxis/`, Praxis 0.8.0 crates, PPE
0.6.0), Go 1.26 (`cmd/cortex`), POSIX sh (`scripts/install.sh`).

**Spec:** The body of issue #1800 (parent #1785). This plan copies that body's facts
and decisions, and it adds the decisions that the issue leaves open.

**Coverage.** Every requirement of #1800 is in this plan; nothing is left out.

| Issue requirement | Task |
|---|---|
| 1. `docs/praxis-policy.md` with every section, linked from three places | T1 |
| 2. The migration section, with the table of options with no equivalent | T1 |
| 3. The deprecation banner, the installer line, the binary Warn line, and the notes in `docs/laptop-service.md`, `cmd/README.md` and `README.md` | T3 |
| 4. A Rust test that loads every example in the page | T2 |

**Base:** `afaa76546c469f24cec693e55eae23fcfc970c9a` (upstream `main`, 2026-11-13,
the merge of #1863). Branch: `feat/ppe-docs-1800`.

## Global Constraints

- Kubernetes use of the cpex plugin is **not** deprecated. The `authbridge-cpex`
  image, the Go `cpex` plugin and the `go-ci-cpex` job stay as they are.
- Do not remove anything. #1801 owns removal.
- Do not build Cedar, OPA, OAuth or CIBA into the gateway.
- Add no dependency to any Go module or to the `praxis/` workspace. The Rust test uses
  crates that `praxis/server` already depends on.
- The five policy examples are load-tested. Each one is a complete policy file and uses
  the fence ```` ```yaml policy ````. Every other YAML block in the page uses a plain
  ```` ```yaml ```` fence.
- The deprecation sentence is one fixed string. Use it in the installer line and in the
  binary's Warn line, which issue #1800 requires to carry "the same text". The document
  banners are prose and can differ. The sentence must carry the `--praxis` flag and the
  page path, as the issue requires:
  `the laptop cpex build is deprecated; use praxis mode with praxis.policy instead (enable it with --praxis; see docs/praxis-policy.md); Kubernetes cpex use is not affected`
- The `praxis.policy.config_path` value must be an absolute path that exists
  (`core/config/validate.go`, `validatePraxisPolicy`). The page says so.
- Run `pre-commit run --all-files` before you push.

## Review Focus

1. **A reader turns policy on but forgets the restart.** The supervisor reads
   `praxis.*` only when it starts (`docs/laptop-service.md`), so a saved config path
   alone changes nothing. The page must put `agentop service restart` in the same
   numbered list. T1's text pins this.
2. **A reader copies an example that the engine refuses.** Every tagged example must
   load in the gateway's build of PPE 0.6.0. T2 fails on the first one that does not.
3. **A reader copies an example that needs a file or the network.** The identity
   example uses an inline PEM key, so the test needs no file and no network. T2's
   `init_timeout_secs` stays at its default, and the test must finish in seconds.
4. **A sidecar logs the deprecation line.** Only a local install logs it. A cluster
   sidecar sets `localInstall` false. T3's Go test pins both cases.
5. **An installed CPEX build stops working after the change.** The installer must
   install as before and only add the line. T3's install test pins the exit status, the
   download and the handoff.

---

## File Structure

| File | Task | Responsibility |
|---|---|---|
| `docs/praxis-policy.md` (new) | T1 | The policy page, the five examples and the migration section |
| `docs/README.md` | T1 | One index row that points to the page |
| `docs/laptop-service.md` | T1, T3 | The praxis-mode link, and the cpex deprecation note |
| `praxis/README.md` | T1 | One paragraph that points to the page |
| `praxis/server/tests/doc_policy_examples.rs` (new) | T2 | Loads every tagged example with `PolicyFilter::from_config` |
| `cmd/cortex/praxis.go` | T3 | The deprecation Warn line |
| `cmd/cortex/praxis_test.go` | T3 | The Warn line is on for a local install, off for a sidecar |
| `scripts/install.sh` | T3 | The `warn` line for `--cpex` |
| `scripts/install_test.sh` | T3 | The `--cpex` stderr check |
| `docs/cpex-plugin.md` | T3 | The deprecation banner |
| `cmd/README.md` | T3 | The laptop cpex build is marked deprecated |
| `README.md` | T3 | The learn-more link says the laptop cpex build is deprecated |

---

### Task 1: The policy page, its links and the migration section

**Depends on:** none.

**Files:**
- Create: `docs/praxis-policy.md`
- Modify: `docs/README.md` (the "Getting started (laptop)" table)
- Modify: `docs/laptop-service.md` (the praxis paragraph, after the `--praxis` install block)
- Modify: `praxis/README.md` (the "Run the gateway" section)

**Interfaces:**
- Produces: `docs/praxis-policy.md` with exactly five fenced blocks that start with
  ```` ```yaml policy ````. T2 reads that file and those fences.

**Decisions and reasons:**
- **The five examples are complete policy files, not fragments.** PPE refuses a
  fragment, and the test loads each block as a file. A fragment would make the test
  weak.
- **The identity example uses `decoding_key: { kind: pem, pem: ... }`.** A `jwks_url`
  makes the engine fetch the URL while it builds the filter, so the test would need the
  network and could take 30 s to fail. An inline PEM needs neither. The page says that
  production uses `jwks_url` or `pem_file` and that a JWKS URL also needs
  `capabilities: [perform_http]`.
- **The tool example starts with an `llm:` route.** A policy with only `tool:` routes
  returns 500 for a request that has no MCP method (`missing_protocol_metadata`
  rejection). The `llm:` route admits those requests. The e2e test
  `TestPraxisE2E_PolicyDeniesTool` in `cmd/cortex/praxis_e2e_test.go` has this shape.
- **The model example uses the never-true guard `llm.provider == 'never'`.** That shape
  loads and passes today (`TestPraxisE2E_PolicyDeniesModel`). Do not invent a new step
  form here.
- **The page links from three places.** The issue names `docs/README.md`,
  `docs/laptop-service.md` and `praxis/README.md`. One row and two short paragraphs
  are enough.

- [ ] **Step 1: Create `docs/praxis-policy.md`**

The page has these sections, in this order. Use this text and these values.

**Title and one paragraph.** Title: `# Policy in praxis mode`. The paragraph says that
in praxis mode the policy filter runs inside `cortex-gateway`, that it uses PPE, and
that `praxis.policy` is the replacement for the laptop CPEX build.

**`## Turn policy on`** A numbered list:

1. Install praxis mode:
   `curl -fsSL https://www.rossoctl.dev/cortex/install.sh | sh -s -- --praxis`. It
   installs `cortex-gateway` and sets `praxis: {enabled: true}`. `--praxis` needs a
   release that carries `cortex-gateway`. If the installer stops with `release … has no
   cortex-gateway build for <os>/<arch>`, add `--ref=main` to install the channel
   build, which has the gateway, or build it from source
   ([`laptop-service.md`](laptop-service.md)). On a machine that already has
   `cortex-gateway`, `agentop setup --praxis` sets the flag without a download. Without
   the binary, setup stops and gives the installer one-liner as the fix.
2. Write the policy file, for example `~/.cortex/policy.yaml`. Copy one of the examples
   below.
3. Set the path in `~/.cortex/config.yaml`. It must be absolute:

```yaml
praxis:
  enabled: true
  policy:
    config_path: /Users/you/.cortex/policy.yaml
```

4. Run `agentop service restart`. The supervisor reads `praxis.*` only when it starts,
   so a save alone changes nothing.
5. Edit the policy file later without a restart. The gateway reloads it. A file that
   does not parse keeps the last good policy.

**`## What the gateway runs`** Two lists.

Built:
- `identity/jwt`, with its JWKS, PEM and JWK key sources, and its claim presets.
- `cel:` steps. They need `global: { pdp: [{ kind: cel }] }`.
- Everything in PPE that does not need a plugin from the list below.

Not built:
- Cedar, OPA, OAuth delegation, CIBA, Vault and Valkey.
- A2A policy. The gateway has no A2A route.

**`## Where policy runs`** One paragraph. The supervisor writes this chain into
`~/.cortex/gateway.yaml` from `praxis.policy.config_path`. The chain is `ip_acl`,
`cortex_attribution`, `cortex_observe`, `policy`, `access_log`, `router`,
`load_balancer` (`cmd/cortex/gateway.go`). So `policy` runs after the row is recorded
and a denied request still gets its row.

**`## Record the decision in the session row`** One paragraph and a YAML block with a
plain fence. Add this plugin to the top of the policy file. The response row then
carries `{plugin: "policy", action: "allow"|"deny", reason: <violation code>}`:

```yaml
plugins:
  - name: cortex-recorder
    kind: cortex/recorder
    capabilities: [read_headers]
```

**`## Examples`** One short paragraph before each example. Every block below uses the
fence ```` ```yaml policy ````. These are the five blocks, exactly:

Model allow list:

```yaml policy
routes:
  - llm: claude-sonnet-4
    authorization:
      pre_invocation:
        - "llm.provider == 'never': deny('unreachable', 'never')"
  - llm: "*"
    authorization:
      pre_invocation:
        - "deny('model is not permitted', 'model_not_allowed')"
```

MCP tool deny:

```yaml policy
routes:
  - llm: "*"
    authorization:
      pre_invocation:
        - "llm.provider == 'never': deny('unreachable', 'never')"
  - tool: read_file
    authorization:
      pre_invocation:
        - "http.method == 'NEVER': deny('unreachable', 'never')"
  - tool: "*"
    authorization:
      pre_invocation:
        - "deny('tool is not permitted', 'tool_not_allowed')"
```

User JWT in `X-Agent-Token`:

```yaml policy
plugins:
  - name: user-jwt
    kind: identity/jwt
    hooks: [identity.resolve]
    config:
      credential: { kind: header, name: X-Agent-Token }
      trusted_issuers:
        - issuer: https://idp.example.com
          audiences: [cortex]
          algorithms: [ES256]
          decoding_key:
            kind: pem
            pem: |
              -----BEGIN PUBLIC KEY-----
              MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEaSRwuPUoZ6cKqrpd88zDaoK8Ph2N
              bkonAXfrgNqiB9ccLFGc/vkCNYh2Gndh7L1Cc07oMseoSM4MoJWjYhnp/w==
              -----END PUBLIC KEY-----
routes:
  - llm: "*"
    authentication: [user-jwt]
```

The paragraph for this example says: on a laptop, `Authorization` carries the agent's
provider key, so a user JWT goes in another header. PPE does not remove
`X-Agent-Token`, so the origin receives it. A loopback JWKS needs
`allow_private_idp: true`, which `praxis.policy` does not set. A JWKS URL also needs
`capabilities: [perform_http]`.

Conditional taint:

```yaml policy
routes:
  - llm: "*"
    authorization:
      pre_invocation:
        - when: "llm.input.max_tokens > 8000"
          do: ["taint(oversized, session)"]
```

The paragraph says: the step needs a resolved subject and an `X-Session-Id` header. It
does not block. `on_allow` on a `cel:` step is not a replacement, because a false
`cel:` step denies. Taints live in memory, so a gateway restart or a policy reload
loses them.

Deny a streamed inference response:

```yaml policy
routes:
  - llm: "*"
    authorization:
      pre_invocation:
        - "custom.llm.stream == true: deny('a streamed response is not allowed', 'stream_not_allowed')"
```

The paragraph says: the gateway runs no `llm:` response-phase rule (see Known gaps), so
a check on the completion cannot be enforced; to refuse streamed responses outright,
deny the stream in the request phase.

**`## Deny responses`** One table:

| Case | Response |
|---|---|
| Identity or transport failure | 401, `WWW-Authenticate: Bearer`, `X-Policy-Violation: <code>` |
| Inference request deny | 403 by default, from the policy's `denyWith`, with an OpenAI-compatible error envelope, `X-Policy-Violation: <code>` |
| Generic HTTP deny | 403 by default, from the policy's `denyWith`, `X-Policy-Violation: <code>` |
| MCP deny | HTTP 200 with a JSON-RPC error, `code: -32001`, `X-Policy-Violation: <code>` |
| MCP request with no `mcp.method` metadata | 500 |

**`## Known gaps`** This list, verbatim:
- No A2A policy.
- A `praxis.policy` block does nothing while `praxis.enabled` is false, and startup
  prints no warning.
- The gateway sets no `body_access`, so the policy runs in the filter's default
  `read_only` mode. That mode does not buffer the response, so no `llm:`, `tool:`,
  `prompt:` or `resource:` response-phase rule runs, streamed or not. Every
  `post_invocation` step on those routes loads and has no effect, and so does a
  `result.<field>` rule on a `tool:` route. A `result.<field>` rule on a `prompt:` or
  `resource:` route stops the policy from loading. A request-side rewrite such as
  `args.ssn: redact(...)` also has no effect. A `post_invocation` step on an `http:`
  route or under `global:` runs at the response header phase, which carries no
  completion. Put a check on a completion in a `pre_invocation` step.
- A body with no readable `model` takes no `llm:` route. The gateway sets
  `llm.require_model: false` (`cmd/cortex/gateway.go`), so PPE runs the policy's HTTP
  admission on it: a matching `http:` route, or else the `global` rules. A policy with
  no HTTP rules and no request assertions admits it on identity. A gzip or zstd body,
  and a body with no string `model`, take this path. A `global:` rule that reads
  `llm.request` denies the request, because it carries no inference request. Keep model
  rules in `llm:` routes or `global.defaults.llm`.
- Taints are held in memory and are lost at each gateway restart and each policy
  reload.
- WebSocket upgrades do not go through the gateway (`cmd/cortex/praxis.go`), so no
  policy runs on them.
- These Cortex-only CPEX options have no equivalent: `observe_codes`,
  `principal.token_file`, `workload`, `bypass_hosts`, `bypass_paths`, `strict_hosts`,
  `synthetic_egress`, `delegated_credential_hosts`, `session_source`,
  `fallback_identification`, `hooks`, `fail_open`.

**`## Move from the CPEX plugin`** A numbered list:
1. Move the CPEX policy YAML out of the `cpex` plugin entry and into its own file.
2. Delete every construct the gateway does not build: `elicitation/ciba` plugins,
   `delegator/oauth` plugins, and the `cedar-direct` PDP. `hr-cpex` carries all three.
   Also delete each step that calls one. A `require_approval(...)` or `confirm(...)`
   step with no plugin, a `delegate(...)` step with no plugin, and a `cedar:` step with
   no PDP each deny the request.
3. Apply the 0.5 to 0.6 changes. `ConversationContext.history` is now `Vec<Message>`,
   in CMF shape. `&` and `|` behave differently when an operand cannot be ordered.
   There are new `llm.input.*`, `llm.model.*` and `llm.output.*` facts, and the old
   `llm.model_id` keys stay as aliases.
4. Remove the `cpex` entry from `pipeline.outbound`.
5. Set `praxis.policy.config_path` and restart, as "Turn policy on" says.
6. Check the options in the table below. Move each one you use another way, or accept
   that it is gone.

After the list, add this table of the CPEX plugin options with no equivalent (issue
#1800, requirement 2). Every option comes from the "Known gaps" list, and every
description comes from the option table in `docs/cpex-plugin.md`:

| CPEX option | In praxis mode |
|---|---|
| `observe_codes` | No equivalent. Write the step as a `when:`/`do:` taint step. |
| `principal.token_file` | No equivalent. `identity/jwt` reads a header, cookie or query parameter, not a file. |
| `workload` (`workload.audience`, `workload.header`) | No equivalent. The gateway asserts no workload token. |
| `bypass_hosts` | No equivalent. Every request that reaches the gateway is judged. |
| `bypass_paths` | No equivalent. |
| `strict_hosts` | No equivalent. |
| `synthetic_egress` | No equivalent. |
| `delegated_credential_hosts` | No equivalent. The gateway builds no OAuth delegation. |
| `session_source` | No equivalent. A taint step reads the `X-Session-Id` header. |
| `fallback_identification` | No equivalent. Write the rule in the policy. |
| `hooks` | The `hooks:` key of each plugin in the policy file. |
| `fail_open` | No equivalent. |

The paragraph before the list says: the constructs in
`core/plugins/cpex/testdata/hr-cpex/cpex-policy.yaml` and in the examples in
`docs/cpex-plugin.md` exist in PPE 0.6.0. The gateway builds less than a full PPE, so
neither file loads there as it stands: `hr-cpex` fails on `delegator/oauth`, which it
names.

- [ ] **Step 2: Add the three links**

- `docs/README.md`, "Getting started (laptop)" table, after the token-savings row:
  `| Enforce policy on agent traffic in praxis mode | [` + "`praxis-policy.md`](praxis-policy.md)" + ` |`
- `docs/laptop-service.md`, at the end of the praxis-mode paragraph that starts "To
  turn praxis mode on": one sentence that links `[policy in praxis
  mode](praxis-policy.md)`.
- `praxis/README.md`, in "Run the gateway", after the sentence that describes
  `cortex -supervise`: one sentence that says a policy needs
  `praxis.policy.config_path` in `config.yaml` and that the supervisor writes the
  `policy` filter into `gateway.yaml`, with a link to `../docs/praxis-policy.md`.

- [ ] **Step 3: Check every link you added**

Run from the worktree root:

```sh
ls docs/praxis-policy.md && ls docs/README.md docs/laptop-service.md praxis/README.md
```

Then read each added line and confirm its relative target exists from that file's
directory. `docs/README.md` and `docs/laptop-service.md` use `praxis-policy.md`.
`praxis/README.md` uses `../docs/praxis-policy.md`.

- [ ] **Step 4: Run the docs gates**

Run: `pre-commit run --files docs/praxis-policy.md docs/README.md docs/laptop-service.md praxis/README.md`
Expected: every hook passes.

- [ ] **Step 5: Commit**

```bash
git add docs/praxis-policy.md docs/README.md docs/laptop-service.md praxis/README.md
git commit -s -m "docs: Add the praxis policy page and its migration section (#1800)"
```

---

### Task 2: The test that loads the documented policies

**Depends on:** T1 (it reads the fences that T1 writes).

**Files:**
- Create: `praxis/server/tests/doc_policy_examples.rs`

**Interfaces:**
- Consumes: `docs/praxis-policy.md`, five blocks that start with ```` ```yaml policy ````
  (T1). `praxis_filter::PolicyFilter::from_config(&serde_yaml::Value) ->
  Result<Box<dyn HttpFilter>, FilterError>`. The crate is
  `praxis-proxy-filter` in `Cargo.toml` but `praxis_filter` in Rust code, because its
  `[lib] name` is `praxis_filter`; `praxis/server/src/main.rs` already imports it that
  way. `praxis_filter::register_policy_plugin_factory(kind: impl Into<String>,
  make: PolicyPluginFactoryFn)`. `cortex_praxis_filters::policy_recorder::{KIND,
  CortexRecorderFactory}`.
- Produces: no code that another task uses.

**Decisions and reasons:**
- **The test runs `PolicyFilter::from_config`, not a bare `PolicyEngine`.** That
  function is the gateway's own build path. It installs the builtin plugin factories
  and reads the file, so the test loads each example with exactly the features
  `praxis/server/Cargo.toml` enables (`policy-jwt`, `policy-cel`,
  `policy-crypto-default`). Its doc comment says it builds the filter without a
  sub-request connector, which a load test does not need.
- **No `Cargo.toml` change is needed.** `praxis/server` sets
  `default-features = false` on `praxis-proxy-filter` with no features of its own, but
  the same file enables `policy-engine`, `policy-jwt`, `policy-cel` and
  `policy-crypto-default` on `praxis-proxy`, and those turn on the matching
  `praxis-filter/*` features. `praxis/server/src/main.rs` already uses
  `praxis_filter::register_policy_plugin_factory` under that setup. `PolicyFilter::from_config`
  is `pub` and `PolicyFilter::new` is `pub(crate)`, so call `from_config`.
- **The test lives in `praxis/server/tests/`.** That package holds the feature list
  that defines "the gateway's features". A test in `filters/` would need its own copy
  of that list, and the two would drift.
- **The test asserts the count is exactly five.** A page that loses an example must
  fail, not pass with fewer checks.
- **Register `cortex/recorder` before the first example.** No tagged example names that
  sink kind today, because its `plugins:` block is a plain fence. Register it anyway,
  as `praxis/server/src/main.rs` does before the server starts: a later edit that tags
  the recorder block as a policy example then loads instead of failing on an unknown
  `kind:`, and the test keeps the same build path as the gateway.
- **A failing example is fixed in the document, not in the test.** The test is the
  contract. If `from_config` cannot build an example, change the example.

- [ ] **Step 1: Write the test**

Create `praxis/server/tests/doc_policy_examples.rs` with this content. Use the imports
as written.

```rust
//! Every policy example in `docs/praxis-policy.md` loads in the gateway's build.
//!
//! `PolicyFilter::from_config` is the gateway's own construction path, so this test
//! installs the same builtin plugin factories and reads the same kind of file.

use std::path::PathBuf;
use std::sync::Arc;

use cortex_praxis_filters::policy_recorder::{CortexRecorderFactory, KIND};

/// The fence that marks a load-tested policy example. Every other YAML block in the
/// page uses a plain ```yaml fence and is not loaded.
const FENCE: &str = "```yaml policy\n";

/// The number of load-tested examples the page must carry.
const EXAMPLES: usize = 5;

/// The bodies of every ```yaml policy fence in `doc`.
fn policy_examples(doc: &str) -> Vec<String> {
    let mut out = Vec::new();
    let mut rest = doc;
    while let Some(i) = rest.find(FENCE) {
        rest = &rest[i + FENCE.len()..];
        let Some(j) = rest.find("\n```") else {
            panic!("docs/praxis-policy.md: unterminated ```yaml policy fence");
        };
        out.push(rest[..j].to_owned());
        rest = &rest[j..];
    }
    out
}

#[test]
fn every_documented_policy_loads() {
    let path = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../docs/praxis-policy.md");
    let doc = std::fs::read_to_string(&path).expect("read docs/praxis-policy.md");
    let blocks = policy_examples(&doc);
    assert_eq!(
        blocks.len(),
        EXAMPLES,
        "docs/praxis-policy.md must carry {EXAMPLES} ```yaml policy examples"
    );

    // As main.rs does before the server starts: the factory must be registered first.
    praxis_filter::register_policy_plugin_factory(
        KIND,
        Arc::new(|| Box::new(CortexRecorderFactory)),
    );
    let dir = std::env::temp_dir().join(format!("cortex-doc-policy-{}", std::process::id()));
    std::fs::create_dir_all(&dir).expect("temp dir");
    for (i, block) in blocks.iter().enumerate() {
        let file = dir.join(format!("example-{i}.yaml"));
        std::fs::write(&file, block).expect("write example");
        let config: serde_yaml::Value = serde_yaml::from_str(&format!(
            "config_path: {:?}\n",
            file.to_str().expect("utf-8 path")
        ))
        .expect("filter config");
        let _filter = praxis_filter::PolicyFilter::from_config(&config)
            .unwrap_or_else(|e| panic!("example {i} does not load: {e}\n{block}"));
    }
}
```

- [ ] **Step 2: Run the test**

Run: `cargo test --locked --manifest-path praxis/Cargo.toml -p cortex-gateway --test doc_policy_examples`
Expected: PASS. If it fails, the message names the example and the text. Fix that
example in `docs/praxis-policy.md` and run again. Do not edit `Cargo.toml` and do not
edit the test: the examples are the contract. Both names come from the crate root
(`praxis-proxy-filter-0.8.0/src/lib.rs`), which re-exports them from
`builtins::http::security::policy`.

- [ ] **Step 3: Run the Rust gates**

Run, from the worktree root:

```sh
cargo fmt --check --all --manifest-path praxis/Cargo.toml
cargo clippy --all-targets --locked --manifest-path praxis/Cargo.toml -- -D warnings
cargo test --locked --manifest-path praxis/Cargo.toml -p cortex-gateway
```

Expected: all three pass. If `clippy` names a lint in the new test, fix the test.
`--all` is required: without it, `cargo fmt` prints `Failed to find targets` for this
workspace's virtual manifest. CI runs plain `cargo fmt --check` inside `praxis/`, which
formats the same two member packages.

- [ ] **Step 4: Commit**

```bash
git add praxis/server/tests/doc_policy_examples.rs
git commit -s -m "test: Load every documented praxis policy example (#1800)"
```

---

### Task 3: The deprecation lines

**Depends on:** none. The three links it adds point at `docs/praxis-policy.md`, which
T1 creates in the same PR; a link is not a dependency, and the page exists before the
PR is ready.

**Files:**
- Modify: `cmd/cortex/praxis.go` (the package-level constants and `outboundEntries`)
- Modify: `cmd/cortex/praxis_test.go`
- Modify: `scripts/install.sh` (the `--cpex` block in `main`, near line 1181)
- Modify: `scripts/install_test.sh` (the `--cpex` success case, near line 1969)
- Modify: `docs/cpex-plugin.md` (the top of the file)
- Modify: `docs/laptop-service.md` (the "The cpex build" section)
- Modify: `cmd/README.md` (the "cpex builds" section)
- Modify: `README.md` (the "Learn more" line)

**Interfaces:**
- Produces: `deprecatedCpexWarning` (an unexported `const string` in package `main`,
  `cmd/cortex`) and `warnDeprecatedCpex(c *config.Config, log *slog.Logger)`. No other
  task uses them.

**Decisions and reasons:**
- **One fixed sentence in the installer and the binary.** The issue asks the binary for
  "the same text" as the installer. The sentence is the one in Global Constraints, and
  both use it byte for byte.
- **The Go line is logged from `outboundEntries`.** That function is the only point
  that both knows the outbound list and runs on the start-up build. It is also the
  existing home of the praxis-mode skip Warn, and that Warn already repeats on a
  config reload. A reload repeats this line for the same reason: the reload rebuilds
  the outbound chain.
- **The Go code reads the package variable `localInstall`.** That is the check the
  issue points at: `core/config/session_archive.go`'s `ArchiveRunsOnLocalInstall`
  cannot decide it, because its own doc comment says "That half is the caller's to
  decide, since only the caller knows where the config lives". The caller's decision is
  `startedFromLocalInstall(*configPath)` (`cmd/cortex/main.go:266`), and `main` caches
  it in `localInstall` at `cmd/cortex/main.go:460`, before the first `buildPipelines`
  call that reaches `outboundEntries` at `cmd/cortex/main.go:583`. The repository
  already uses that variable this way. Do not add a parameter: no caller has a better
  value.
- **No line in praxis mode.** In praxis mode the outbound plugins are off, and the
  existing skip Warn already names `cpex`.
- **`install.sh` warns and then installs as before.** The issue says "Install as
  before". Use the existing `warn` helper, which writes to stderr with the
  `warning: ` prefix.
- **The banner in `docs/cpex-plugin.md` goes above the existing blockquote.** It is the
  first thing a reader sees.

- [ ] **Step 1: Write the failing Go test**

Add to `cmd/cortex/praxis_test.go`:

```go
func TestOutboundEntries_WarnsDeprecatedCpexOnLocalInstall(t *testing.T) {
	old := localInstall
	t.Cleanup(func() { localInstall = old })

	cfg := praxisCfg(false)
	cfg.Pipeline.Outbound.Plugins = []config.PluginEntry{{Name: "cpex"}}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	localInstall = true
	if got := outboundEntries(cfg, log); len(got) != 1 {
		t.Fatalf("local install built %v, want the cpex entry", got)
	}
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 {
		t.Fatalf("local install logged %d Warn lines, want 1: %q", n, buf.String())
	}
	if !strings.Contains(buf.String(), "the laptop cpex build is deprecated") {
		t.Errorf("log %q does not name the deprecation", buf.String())
	}

	buf.Reset()
	localInstall = false
	outboundEntries(cfg, log)
	if buf.Len() != 0 {
		t.Errorf("a sidecar logged %q", buf.String())
	}

	buf.Reset()
	localInstall = true
	if got := outboundEntries(praxisCfg(true), log); len(got) != 0 {
		t.Fatalf("praxis mode built %v, want none", got)
	}
	if strings.Contains(buf.String(), "deprecated") {
		t.Errorf("praxis mode logged the deprecation: %q", buf.String())
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `GOWORK=off go -C cmd/cortex test -run TestOutboundEntries_WarnsDeprecatedCpex ./`
Expected: FAIL, because the Warn line is missing.

- [ ] **Step 3: Add the line in `cmd/cortex/praxis.go`**

Above `outboundEntries`, add:

```go
// deprecatedCpexWarning is the one deprecation sentence for the laptop cpex build.
// The installer prints the same words.
const deprecatedCpexWarning = "the laptop cpex build is deprecated; use praxis mode with praxis.policy instead (enable it with --praxis; see docs/praxis-policy.md); Kubernetes cpex use is not affected"

// warnDeprecatedCpex logs the deprecation once when a local install builds the cpex
// plugin. A sidecar does not log it, and praxis mode does not build the plugin.
func warnDeprecatedCpex(c *config.Config, log *slog.Logger) {
	if !localInstall || c.PraxisEnabled() {
		return
	}
	for _, e := range c.Pipeline.Outbound.Plugins {
		if e.Name == "cpex" {
			log.Warn(deprecatedCpexWarning)
			return
		}
	}
}
```

Call `warnDeprecatedCpex(c, log)` as the first statement of `outboundEntries`. Do not
change the rest of the function.

- [ ] **Step 4: Run the test to verify it passes**

Run: `GOWORK=off go -C cmd/cortex test -run TestOutboundEntries -v ./`
Expected: PASS.

- [ ] **Step 5: Add the installer line and its test**

In `scripts/install.sh`, inside the `if [ -n "${CPEX}" ]; then` block at the start of
`main`, after `cpex_platform_check`, add:

```sh
warn "the laptop cpex build is deprecated; use praxis mode with praxis.policy instead (enable it with --praxis; see docs/praxis-policy.md); Kubernetes cpex use is not affected"
```

`warn` (`scripts/install.sh:92`) writes to stderr with the `warning: ` prefix. Put the
line after `cpex_platform_check` and before the `AUTHBRIDGE_SKIP_DOWNLOAD` check, so a
platform that has no cpex build still dies first and prints no warning.

In `scripts/install_test.sh`, in the linux/amd64 `--cpex` success case (after the
`staging the cpex build as cortex` check), add:

```sh
check "  warning that the laptop cpex build is deprecated" "yes" \
	"$(t_in 'warning: the laptop cpex build is deprecated' err)"
```

- [ ] **Step 6: Run the installer test**

Run: `sh scripts/install_test.sh`
Expected: all checks pass, including the new one.

- [ ] **Step 7: Write the four documents**

- `docs/cpex-plugin.md`, above the existing top blockquote:

  ```markdown
  > **Deprecated on a laptop.** `install.sh --cpex` and the laptop cpex build are
  > deprecated. Use praxis mode with
  > [`praxis.policy`](praxis-policy.md) instead. The
  > `authbridge-cpex` image and Kubernetes deployments are **not** deprecated.
  ```

- `docs/laptop-service.md`, "The cpex build" section, a paragraph right after the
  heading: the laptop build is deprecated, with a link to `praxis-policy.md`. The
  paragraph also says that the section stays for existing installs, and that
  Kubernetes is not affected.

- `cmd/README.md`, "cpex builds" section, on the **Laptop** bullet: add the word
  `Deprecated on a laptop.` at the start, and a link to
  `../docs/praxis-policy.md`. Leave the **`authbridge-cpex` image** bullet unchanged.

- `README.md`, the "Learn more" line: replace
  `[cpex policy build](./docs/laptop-service.md#the-cpex-build)` with
  `[cpex policy build (deprecated on a laptop)](./docs/laptop-service.md#the-cpex-build)`
  and add ` · [policy in praxis mode](./docs/praxis-policy.md)` after it.

- [ ] **Step 8: Run the checks that the documents and the script touch**

Run: `pre-commit run --all-files && sh scripts/install_test.sh`
Expected: every hook and every check passes.

- [ ] **Step 9: Commit**

```bash
git add cmd/cortex/praxis.go cmd/cortex/praxis_test.go scripts/install.sh scripts/install_test.sh docs/cpex-plugin.md docs/laptop-service.md cmd/README.md README.md
git commit -s -m "feat: Deprecate the laptop cpex build and name praxis policy instead (#1800)"
```

---

## PRs

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | docs: Document praxis policy and deprecate the laptop cpex build | "" | T1, T2, T3 | 700 |

The tasks add about 350 counted lines: one new page, one new Rust test, one Go test, and
edits to five existing documents and two scripts. 700 is the estimate with headroom.
That is under the 2,000-line limit, so this is one PR. The plan document is not counted.

T1 and T3 change no shared code, so they run at the same time. T2 needs T1.

## Gate

Run this one command from the worktree root. Every step must pass.

```sh
pre-commit run --all-files && sh scripts/install_test.sh && GOWORK=off go -C cmd/cortex vet ./... && GOWORK=off go -C cmd/cortex test -race ./... && cargo fmt --check --all --manifest-path praxis/Cargo.toml && cargo clippy --all-targets --locked --manifest-path praxis/Cargo.toml -- -D warnings && cargo test --locked --manifest-path praxis/Cargo.toml -p cortex-gateway
```

**Checked on the base.** On `afaa7654`, from this worktree, `GOWORK=off go -C cmd/cortex
vet ./...` passes. `cargo fmt --check --manifest-path praxis/Cargo.toml` fails with
`Failed to find targets` because the workspace manifest is virtual; adding `--all`
makes it pass (`cargo fmt --check --all --manifest-path praxis/Cargo.toml`, exit 0), and
it formats the same two member packages that CI's `cargo fmt --check` inside `praxis/`
does. That is the change: `--all` was added to the `cargo fmt` step here and in T2
Step 3. No step was removed or narrowed.

## Done when

- The requirements in issue #1800 hold.
- `every_documented_policy_loads` passes with five examples.
- `TestOutboundEntries_WarnsDeprecatedCpexOnLocalInstall` passes.
- `scripts/install_test.sh` passes, including the new check.
- The gate above passes.
- Every link that T1 and T3 add resolves.

## Not in scope

- Removing anything (#1801).
- Building Cedar, OPA, OAuth or CIBA into the gateway.
- A change to the gateway, the policy filter or the session row code. #1797, #1798 and
  #1799 already merged.

## For a human after review

These steps are outside the repository. No task does them.

1. **Tell the CPEX contributor about the deprecation.** It needs T1 and T3.
   - Contact Frederico Araujo (`araujof`). He wrote `observe_codes`, the principal
     `token_file` and synthetic egress in #1429.
   - Send the deprecation sentence from Global Constraints, and the link to
     `docs/praxis-policy.md`.
2. **Send the external message only after the PR merges.** It needs P1.
   - Do not announce a deprecation that the main branch does not carry yet.
3. **Add the release-notes line to the release that ships this change.** It needs P1.
   - Open the release record for that version.
   - Add one line under the deprecations heading. Use the deprecation sentence from
     Global Constraints, and say that Kubernetes is not affected.
4. **Tick #1800 in the #1785 checklist and close #1800.** It needs P1 merged.
   - Comment on #1785 with the PR link.
   - Close #1800 when the release that carries the change is out, or when the
     maintainers agree the merge is enough.
