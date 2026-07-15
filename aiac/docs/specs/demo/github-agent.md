# Demo Spec: `github-agent` (A2A) — source + issue operations over `github-tool`

> **Status:** spec (design source of truth). Implementation is decomposed into
> `docs/issues/demo/GA-*.md` and entry-pointed by
> `docs/handoffs/handoff-github-agent-implementation.md`.
>
> **This is a demo/reference agent**, not part of the AIAC service tree (`src/aiac/`). It is a
> self-contained, deployable A2A agent that realises the canonical `github-agent` used by the AIAC
> policy-pipeline integration test.

---

## 1. Purpose & scenario mapping

The AIAC policy-pipeline integration test
([`../integration-test/policy-pipeline.md`](../integration-test/policy-pipeline.md))
and its fixture ([`../../../test/integration/scenario.py`](../../../test/integration/scenario.py)) are written
around a canonical **`github-agent`**: an autonomous A2A agent that acts on a user's behalf against
**source repositories** and an **issue tracker**, calling the **`github-tool`** MCP server. Until now
that agent has existed only as test data; this spec defines a **real, deployable** agent that matches it.

The existing runnable reference, `agent-examples/a2a/git_issue_agent`, is **issue-only and single-skill**.
This agent generalises it to the scenario's **two capability areas**.

### Mapping to the policy-pipeline scenario

| Scenario element | This agent |
|---|---|
| Agent client `github-agent` (type **Agent**) | This A2A agent |
| Agent role `source-operator` → agent scope `source-access` | **Skill 1** — Source repository operations |
| Agent role `issues-operator` → agent scope `issues-access` | **Skill 2** — Issue & PR tracker operations |
| Tool `github-tool` scopes `source-read`/`source-write` | Source tool allow-list (see §5) |
| Tool `github-tool` scopes `issues-read`/`issues-write` | Issue/PR tool allow-list (see §5) |

The agent does **not** know or enforce these scopes itself — AIAC/OPA + AuthBridge do. The agent simply
exposes the two capability areas; the AuthBridge sidecar performs inbound JWT validation and outbound
RFC-8693 token exchange, and the `github-tool` MitM swaps the exchanged token for a GitHub PAT by scope.

### Related artefacts
- Scenario spec: [`../integration-test/policy-pipeline.md`](../integration-test/policy-pipeline.md)
- Scenario fixture: [`../../../test/integration/scenario.py`](../../../test/integration/scenario.py)
- Existing (issue-only) agent card: [`../../analysis/github-agent-card.json`](../../analysis/github-agent-card.json)
- `github-tool` MCP tool catalog (44 tools): [`../../analysis/github-mcp-tools-summary.json`](../../analysis/github-mcp-tools-summary.json)
- Reference agent: `agent-examples/a2a/git_issue_agent/`
- Reference deployment: `kagenti-extensions/authbridge/demos/github-issue/k8s/`
- **Sibling tool spec (UC-1 onboarding fixture):** [`github-tool.md`](github-tool.md) — a simplified
  4-tool stub (`source-read`, `source-write`, `issues-read`, `issues-write`) deployed as Service
  `github-tool`. **This is not the tool this agent connects to.** The agent connects to the production
  44-tool server at `github-tool-mcp:9090/mcp`. Both coexist in namespace `team1` under different
  Service names.

---

## 2. Architecture

Reuse the `git_issue_agent` stack verbatim — do not introduce a new framework:

- **A2A SDK 1.x** server (route factories, `AgentInterface`, snake_case card fields). Binds `0.0.0.0`
  on `PORT` (default **8000**). `create_jsonrpc_routes(..., enable_v0_3_compat=True)` — **required**
  because Kagenti uses A2A 0.3 client libraries — plus the agent card at both
  `/.well-known/agent-card.json` and legacy `/.well-known/agent.json`.
- **CrewAI** orchestration (`Agent`/`Crew`/`Task`, `Process.sequential`), LLM via **litellm** (`crewai.LLM`).
- **`crewai-tools[mcp]` `MCPServerAdapter`** — per-request connection to the MCP tool over
  `transport="streamable-http"` at `MCP_URL` (default `http://github-tool-mcp:9090/mcp`), inside a
  `with` block, torn down after each request.
- **Auth (three tiers, verbatim from the reference `GithubExecutor.execute`):**
  1. `GITHUB_TOKEN` env set → `Authorization: Bearer <token>` to MCP;
  2. else pass through the inbound request's `Authorization` header
     (`context.call_context.state["headers"]["authorization"]`) — the AuthBridge/Envoy path;
  3. else unauthenticated + warning.

```
 A2A client ──(JSON-RPC /)──► github-agent (:8000)
                                  │  CrewAI: prereq extract → researcher
                                  └──(streamable-http, MCP_URL)──► github-tool-mcp:9090/mcp ──► GitHub
        (AuthBridge sidecar: inbound JWT validation; outbound RFC-8693 token exchange for MCP_URL host)
```

---

## 3. AgentCard (two skills — comprehensive)

Built in `a2a_agent.py::get_agent_card()`. Fields:

- **name:** `Github agent`
- **version:** `1.0.0`
- **description:** *"Autonomous Agent acting on a user's behalf against source repositories and an
  issue tracker. It inspects and changes repository source contents and reads, creates, and updates
  issues and their threads."* (verbatim from the scenario's `github-agent` description so the card and
  the policy-pipeline fixture agree).
- **supported_interfaces:** `[AgentInterface(url=<AGENT_ENDPOINT or http://host:port/>, protocol_binding="JSONRPC")]`
- **capabilities:** `AgentCapabilities(streaming=True)`
- **default_input_modes / default_output_modes:** `["text"]`
- **security_schemes:** single `Bearer` → `HTTPAuthSecurityScheme(scheme="bearer", bearer_format="JWT", description="OAuth 2.0 JWT token")`
- **skills:** two `AgentSkill`s:

| id | name | description | tags | examples |
|---|---|---|---|---|
| `source_operations` | Source repository operations | Browse and search code; read, create, and modify repository file contents, branches, and commits. | `git, github, source, repositories, files, branches, commits` | "Show the README of kagenti/kagenti", "List the branches of owner/repo", "Create a branch and commit a fix to owner/repo" |
| `issue_operations` | Issue & PR tracker operations | Read, search, create, and update issues, comments, sub-issues, and pull requests. | `git, github, issues, pull-requests` | "List open issues in kubernetes/kubernetes", "Open an issue in owner/repo titled …", "Summarise PR #42 in owner/repo" |

> The existing `git_issue_agent` card ([`../../analysis/github-agent-card.json`](../../analysis/github-agent-card.json))
> has a single `github_issue_agent` skill — this card supersedes it with the two-skill shape.

---

## 4. Agent internals

Generalise the reference two-phase CrewAI flow from issue-only to source + issues:

- **Phase A — prerequisite extraction.** A no-tools CrewAI agent parses the query into a generalised
  `GithubQueryInfo`. Reuse the reference's robustness measures: **avoid `output_pydantic`** (fails on
  small Ollama models) and parse raw JSON from the LLM text via a flat-object regex
  (`re.search(r"\{[^{}]*\}", raw)`); coerce string arrays to lists.

  ```python
  class GithubQueryInfo(BaseModel):
      owner: str | None = None
      repo: str | None = None
      ref: str | None = None          # branch / tag / sha, if named
      path: str | None = None         # file path, if named
      numbers: list[int] | None = None  # issue or PR numbers
  ```

  **Validation gates** (return a helpful message, no tool call, when unmet):
  - `numbers` present → `owner` **and** `repo` required.
  - `repo` present → `owner` required.

- **Phase B — researcher.** One CrewAI "GitHub operations" agent wired with the **curated tool set**
  (§5), `inject_date=True`, bounded `max_iter`/`max_retry_limit`, `respect_context_window=True`. A
  generalised ReAct `TOOL_CALL_PROMPT` with a tool-selection decision tree spanning **source**
  (files / branches / commits / code search) and **issues / PRs**, and a generalised `INFO_PARSER_PROMPT`
  for Phase A. Identifiers (owner/repo/numbers) are copied verbatim; the final answer is grounded only
  in tool output.

`event.py` (streaming `Event` ABC) and `llm.py` (`CrewLLM`, incl. the Ollama `num_ctx=8192` bump) are
copied verbatim from the reference.

---

## 5. Tool wiring (scenario source + issue)

`github-tool` federates 44 GitHub tools at startup. This agent wires only the **source + issue** subset
that matches the scenario scopes, via an **explicit name allow-list** in `github_agent/tools.py` (robust
vs the reference's substring matching). The executor keeps only MCP tools whose `.name` is in the set
and raises `RuntimeError` if none resolve. An `ENABLED_TOOLS` env var (comma-separated) overrides the
default set.

**Source (`source-access` → tool `source-read`/`source-write`):**
- read: `get_file_contents`, `list_branches`, `get_commit`, `list_commits`, `search_code`
- write: `create_or_update_file`, `delete_file`, `push_files`, `create_branch`

**Issue & PR (`issues-access` → tool `issues-read`/`issues-write`):**
- read: `issue_read`, `list_issues`, `search_issues`, `list_issue_types`, `pull_request_read`, `list_pull_requests`
- write: `issue_write`, `add_issue_comment`, `sub_issue_write`, `create_pull_request`, `update_pull_request`

**Excluded by default** (out of the policy-pipeline scenario; one edit / `ENABLED_TOOLS` to add back):
Teams & Users (`get_me`, `get_team_members`, `get_teams`), Security (`run_secret_scanning`), and
repo-lifecycle / release tools (`create_repository`, `fork_repository`, `list_tags`, `get_tag`,
`get_label`, releases). These are named in the module so they are trivially re-enabled.

> The allow-list is a single editable constant grouped by skill/scope so the source/issue split stays
> legible and auditable against the scenario.

---

## 6. Configuration & LLM presets

Config via `github_agent/config.py` (`pydantic-settings`), adapted from the reference:

| Variable | Description | Default |
|---|---|---|
| `TASK_MODEL_ID` | litellm model id | `ollama/ibm/granite4:latest` |
| `LLM_API_BASE` | OpenAI-compatible base URL | `http://host.docker.internal:11434` |
| `LLM_API_KEY` | LLM API key | `my_api_key` |
| `MODEL_TEMPERATURE` | Sampling temperature | `0` |
| `EXTRA_HEADERS` | Extra LLM headers (JSON) | `{}` |
| `MCP_URL` | MCP tool endpoint | `http://github-tool-mcp:9090/mcp` |
| `MCP_TIMEOUT` | MCP connect timeout (s) | `600` |
| `ENABLED_TOOLS` | Override the curated tool allow-list (comma-separated) | (unset → §5 default) |
| `PORT` | A2A listen port | `8000` |
| `LOG_LEVEL` | Log level | `INFO` |
| `GITHUB_TOKEN` | Static Bearer to MCP (else inbound passthrough) | (unset) |
| `ISSUER` | Expected `iss` of inbound JWTs (informational) | (unset) |
| `AGENT_ENDPOINT` | Override the URL advertised in the card | (unset) |

**Env presets shipped:**
- `.env.ollama` **(default)** — `ollama/ibm/granite4:latest`, `LLM_API_BASE=http://host.docker.internal:11434`.
- `.env.openai` — `gpt-4o-mini`, key from k8s secret `openai-secret`.
- `.env.claude` — litellm Anthropic (`TASK_MODEL_ID=anthropic/claude-sonnet-4-6`, `ANTHROPIC_API_KEY`
  from k8s secret `claude-secret`); native Anthropic endpoint (no `LLM_API_BASE`).
- `.env.template` — documented placeholders + `MCP_URL=http://github-tool-mcp:9090/mcp`.

---

## 7. Deployment (aiac level)

Manifests live under `aiac/demo/agents/github_agent/k8s/`, adapted from the github-issue demo. Namespace
`team1` (installer-provided ConfigMaps/secrets assumed present).

- **`github-agent-deployment.yaml`** — `ServiceAccount` + `Deployment` + `Service` + `AgentRuntime`:
  - **Deployment labels** (on `metadata.labels` **and** pod template): `protocol.kagenti.io/a2a: ""`
    in addition to `app.kubernetes.io/name: github-agent`. The `protocol.kagenti.io/a2a` label is
    required by the `AgentCardSyncReconciler` (`shouldSyncWorkload` gate): the operator stamps
    `kagenti.io/type=agent` automatically via the `AgentRuntime`, but the protocol label must be set
    in the manifest. Without it, no `AgentCard` CR is auto-created and `analyze_agent` falls back to
    the default minimal scope.
  - Pod labels `kagenti.io/inject: enabled`, `kagenti.io/spire: enabled`, `protocol.kagenti.io/a2a: ""`.
  - Container port `8000`; env `MCP_URL=http://github-tool-mcp:9090/mcp`,
    `JWKS_URI=http://keycloak-service.keycloak.svc:8080/realms/kagenti/protocol/openid-connect/certs`,
    LLM vars, `PORT`, `LOG_LEVEL`; `/shared` `emptyDir` for operator-mounted client creds.
  - `Service` (ClusterIP) with **two ports** — port order matters because
    `AgentCardReconciler.getServicePort()` always takes `Ports[0]`:
    1. `agent: 8001 → 8001` (first) — direct path to the agent after authbridge port-stealing
       (`8000 → 8001`); used by the operator's `AgentCardReconciler` to fetch `/.well-known/agent.json`
       without going through the authbridge reverse proxy (which blocks unauthenticated requests).
    2. `proxy: 8080 → 8000` (second) — the public/authenticated path through the authbridge reverse
       proxy; used by A2A clients that carry a valid JWT.
  - `AgentRuntime{ type: agent, targetRef: this Deployment }` — enrolls the workload (operator applies
    `kagenti.io/type=agent`, registers a Keycloak client, injects the AuthBridge sidecar).
  - Image `github-agent:latest`, `imagePullPolicy: IfNotPresent` (kind-load; name is a documented knob).
- **`configmaps.yaml`** — `authbridge-config` (Keycloak URL/realm/issuer) + `authproxy-routes` with the
  outbound token-exchange route:
  ```yaml
  - host: "github-tool-mcp"
    target_audience: "github-tool"
    token_scopes: "openid github-tool-aud github-full-access"
  ```
- **Prerequisite (reused, not created here):** the existing **production** `github-tool` Deployment/Service
  (`authbridge/demos/github-issue/k8s/github-tool-deployment.yaml`, Service name `github-tool-mcp`) +
  `github-tool-secrets`, a running Kagenti cluster (Keycloak realm `kagenti`, namespace `team1`).
  The sibling UC-1 stub at `demo/tools/github_tool/` (Service `github-tool`) is a separate deployment
  for AIAC onboarding discovery and is **not** a runtime dependency of this agent.

**Wiring invariant:** agent `MCP_URL` host (`github-tool-mcp`) == `authproxy-routes` host == tool
Service name; exchanged audience (`github-tool`) == tool `AUDIENCE`.

---

## 8. Verification

**Local (no cluster — primary gate):**
1. `cd aiac/demo/agents/github_agent && uv lock && uv sync`.
2. `podman build -t github-agent:latest .`.
3. Startup + card: run `uv run --no-sync server` (or `test_startup.exp`), then
   `curl -s localhost:8000/.well-known/agent-card.json | jq '.name, .skills[].id'` →
   `"Github agent"` + `source_operations`, `issue_operations`.
4. (Optional; needs a GitHub PAT + reachable LLM) `GITHUB_TOKEN=… MCP_URL=https://api.githubcopilot.com/mcp/`,
   send an A2A `message/send` read query and confirm a grounded, tool-cited answer.

**Cluster (HITL — live Kagenti + Keycloak + LLM + tool PAT):**
5. `kind load docker-image github-agent:latest --name kagenti`.
6. Ensure `github-tool` + `github-tool-secrets` exist in `team1`.
7. `kubectl apply -f k8s/configmaps.yaml -f k8s/github-agent-deployment.yaml`.
8. Confirm AuthBridge injection + `kagenti.io/type=agent`; confirm the `AgentCard` CR was
   auto-created (`kubectl get agentcard -n team1` → `github-agent-deployment-card`, `SYNCED=True`).
   `kubectl port-forward svc/github-agent 8080:8080 -n team1` (proxy port);
   send an authenticated A2A message; verify token exchange reaches `github-tool` and an answer returns.

---

## 9. Out of scope
- Any changes to `github_tool` (reused as-is).
- Any changes to the AIAC pipeline, `policy-pipeline.md`, or the integration test.
- Wiring the agent into `agent-examples` CI (`build.yaml`) — aiac/demo images build independently.
- Onboarding this agent through the AIAC UC1 pipeline (separate activity; the card here is its input).
