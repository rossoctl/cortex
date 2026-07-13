# Integration Test: PDP Policy Writer (OPA) — `generate_rego.py`

> **One spec among several.** This document specifies a **single** integration test.
> Integration-test specs live **one spec per test** under `docs/specs/integration-test/`
> (a sibling of `components/`), and the master PRD's *Integration test specifications* section
> ([../PRD.md](../PRD.md)) is the index of them. This is the **PDP Policy Writer (OPA)** integration
> test — not the definition of integration testing in general, and not the only integration-test PRD.

## Location
`aiac/test/pdp/policy/generate_rego.py`

## Description

A standalone launcher script that exercises the PDP Policy Writer (OPA) **filesystem stub**
end-to-end and leaves the generated Rego on disk for a human to eyeball. It is **not** a pytest
test, **not** part of CI, and **not** marked `@pytest.mark.integration` — it is run by hand when an
operator wants to see the actual `.rego` output for a known scenario.

The script drives the service through its real HTTP surface using the real client library, so it
covers the whole path: `PolicyModel` → HTTP → ASGI app → Rego generator → files on disk. Nothing is
mocked; the only shortcut is that the OPA target is the filesystem stub
([../components/pdp-policy-writer-opa.md](../components/pdp-policy-writer-opa.md) §1.14) rather than
the Kubernetes-CR implementation, so the output is `.rego` files instead of a patched
`AuthorizationPolicy` CR.

### What it does

1. Choose a local `REGO_OUTPUT_DIR` (a known directory the operator can inspect afterward).
2. Launch `aiac.pdp.service.policy.opa.main:app` as a **`uvicorn` subprocess** (no Docker), passing
   `REGO_OUTPUT_DIR` and `PORT` in its environment.
3. Poll `GET /health` until it returns `200 {"status": "ok"}` (the stub reports healthy once
   `REGO_OUTPUT_DIR` exists and is writable), with a bounded timeout.
4. Build the fixed `PolicyModel` scenario (below) and apply it via
   `aiac.pdp.policy.library.api.apply_policy` — a real `POST /policy` over HTTP.
5. Terminate the `uvicorn` subprocess.
6. Print the `REGO_OUTPUT_DIR` path so the operator knows where to look.

**Write-only.** The script performs no read-back and makes **no assertions**. Verification is
manual: the operator opens the generated `.rego` files and confirms they match the package shapes in
[../components/pdp-policy-writer-opa.md](../components/pdp-policy-writer-opa.md)
(§ *Rego package structure*). There is no pass/fail exit contract beyond the script running to
completion.

## Scenario

A single agent, fixed so the generated Rego is reproducible and reviewable by inspection. The values
below are the canonical worked example referenced by
[../components/pdp-policy-writer-opa.md](../components/pdp-policy-writer-opa.md).

| Element | Value |
|---------|-------|
| Agent | `github-agent` |
| Agent roles | `source-helper`, `issues-helper` |
| Agent scopes | `source-access`, `issues-access` |
| Subject (user) roles | `developer`, `tester` |
| Tool | `github-tool` |
| Tool scopes | `source-read`, `source-write`, `issues-read`, `issues-write` |
| Calling service (source) | none |

Role → access, as encoded by the model's inbound and outbound rules:

- `developer` — source read/write, issues read.
- `tester` — issues read/write.

This user→tool access is encoded in the model's `outbound_subject_rules` (`(user_role, tool_scope)`
pairs), which the outbound package renders as `outbound_subject_role_scopes`. The model's
`inbound_rules` (user→agent-scope) and `outbound_rules` (agent-role→tool-scope) are unchanged.

Applying this `PolicyModel` produces exactly two files in `REGO_OUTPUT_DIR`:

- `github_agent.inbound.rego` — package `authz.github_agent.inbound`
- `github_agent.outbound.rego` — package `authz.github_agent.outbound`

Both must match the **ID-only** package shapes in
[../components/pdp-policy-writer-opa.md](../components/pdp-policy-writer-opa.md): input is IDs only
(`{subject, source}` inbound, `{subject, target}` outbound); all role/scope maps are embedded in the
package; the inbound gate is subject-mandatory + source-optional; the outbound gate requires both
subject and agent to pass, but its **subject** gate is now user→**tool** — it reads
`outbound_subject_role_scopes` (grouped from `outbound_subject_rules`) and matches against
`target_scopes[input.target]`, distinct from the inbound user→agent gate — while `target_ok`
(agent→tool, from `agent_roles` × `agent_role_scopes`) is unchanged; and `target_scopes` is emitted
verbatim (target id → scopes, no inversion). Because the input carries no per-request scope, the
decision is coarse — a principal passes on having access to **at least one** relevant scope.

The `PolicyModel` / `AgentPolicyModel` / `PolicyRule` objects come from `aiac.policy.model.models`
([../components/policy-model.md](../components/policy-model.md)); the script constructs them in
Python rather than reading them from Keycloak.

## Configuration (env)

| Variable | Purpose | Default |
|----------|---------|---------|
| `AIAC_PDP_POLICY_URL` | Base URL the library client posts to; must point at the launched stub | `http://127.0.0.1:7072` |
| `REGO_OUTPUT_DIR` | Directory the stub writes `.rego` files to; passed to the subprocess and printed at the end | operator-chosen local dir |
| `PORT` | Port the `uvicorn` subprocess binds; must agree with the host/port in `AIAC_PDP_POLICY_URL` | `7072` |

`PORT` and `AIAC_PDP_POLICY_URL` must be kept consistent — the client posts to the URL, the
subprocess listens on the port.

## Runbook

```bash
.venv/bin/python test/pdp/policy/generate_rego.py
# then inspect the printed REGO_OUTPUT_DIR, e.g.:
#   github_agent.inbound.rego
#   github_agent.outbound.rego
```

To pin the output location and port explicitly:

```bash
REGO_OUTPUT_DIR=/tmp/aiac-rego PORT=7072 \
AIAC_PDP_POLICY_URL=http://127.0.0.1:7072 \
  .venv/bin/python test/pdp/policy/generate_rego.py
```

## Testing Decisions

- **Highest seam available.** The test drives the service through its real HTTP boundary
  (`AIAC_PDP_POLICY_URL`) using the real client library
  ([../components/library-pdp-policy.md](../components/library-pdp-policy.md), `aiac.pdp.policy.library.api`),
  and observes the real filesystem output. It asserts on **external behavior** (files produced on
  disk), never on internal generator functions.
- **Launch as a `uvicorn` subprocess.** The script spawns
  `uvicorn aiac.pdp.service.policy.opa.main:app` as a child process (no Docker), polls `GET /health`
  before applying the model, and terminates the subprocess at the end. This exercises the full
  HTTP + ASGI stack the way a caller would, and keeps the service lifecycle self-contained.
- **Write-only, human-verified.** The value of this test is the concrete `.rego` output for a known
  scenario — so a reviewer can confirm the ID-only redesign renders correctly. It intentionally
  makes no automated assertions; the generator's assertable behavior is covered by the OPA
  service/`rego.py` unit tests.
- **Prior art.** The stub itself and its unit tests (`test/pdp/service/policy/opa/`,
  covering `main.py` and `rego.py`) verify endpoint and rendering behavior automatically; this
  launcher complements them with an eyeball-the-output workflow. The live-Keycloak pytest
  integration tests (issue `testing/5.1-integration-tests.md`) are the marker-gated counterpart for
  the read-side services.

## Relationship to other integration tests

This is **one** integration-test spec among several indexed by the master PRD
([../PRD.md](../PRD.md), § *Integration test specifications*). It is distinct from the
**live-Keycloak pytest integration tests**, which are a different flavor — `@pytest.mark.integration`,
run in/near CI against a live Keycloak/NATS, asserting on typed responses — tracked by issue
`testing/5.1-integration-tests.md`. This launcher, by contrast, is standalone, write-only, and
manually inspected.

For the full identity→policy pipeline (Keycloak → PRB → PCE → OPA) — which drives the same
`github-agent` scenario end to end through the real Policy Computation Engine rather than a
hand-built `PolicyModel` — see [policy-pipeline.md](policy-pipeline.md).

Tracking issue for this test: `testing/5.2-pdp-writer-integration-test.md`.

## Out of Scope

- **The Rego generator implementation** — package rendering, `_slugify`, and the ID-only gate logic
  are specified by [../components/pdp-policy-writer-opa.md](../components/pdp-policy-writer-opa.md)
  and covered by the OPA service unit tests, not here.
- **The canonical policy model** — `PolicyModel` / `AgentPolicyModel` / `PolicyRule` shapes and
  semantics belong to [../components/policy-model.md](../components/policy-model.md).
- **The Kubernetes-CR Policy Writer (1.13)** — this test targets the filesystem **stub** (1.14). The
  CR-backed implementation and its `AuthorizationPolicy` schema are out of scope.
- **Live-Keycloak integration** — the marker-gated pytest integration tests (issue
  `testing/5.1-integration-tests.md`).
- **Automated pass/fail** — no assertions, no CI wiring, no `@pytest.mark.integration`.

## Further Notes

- Depends on the launcher script itself (created separately) and the OPA filesystem stub (issue
  `pdp-policy-writer/1.14-pdp-policy-writer-opa-stub.md`, status: done).
- The scenario is deliberately fixed. If the canonical worked example in
  [../components/pdp-policy-writer-opa.md](../components/pdp-policy-writer-opa.md) changes, update the
  scenario table here to match so the generated Rego stays reviewable against a single source of
  truth.
