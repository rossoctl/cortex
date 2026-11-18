# Kubernetes journey and contributor docs cleanup (#1944) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give a Kubernetes user one page from prerequisites to a verified sidecar, fix stale names in the reference docs, link the orphan files, start `CONTRIBUTING.md` with the dev loop, and merge the two export docs into one.

**Architecture:** Docs only. Five tasks change five disjoint sets of files, so all five run at the same time. No Go code changes.

**Tech Stack:** Markdown. Gates: `scripts/dev/check-doc-links.py`, pre-commit, one Go doc test (`core/sessionexport/doc_example_test.go`).

**Spec:** GitHub issue rossoctl/cortex#1944 (parent #1937). Read it with `gh issue view 1944 --repo rossoctl/cortex`.

## Global Constraints

- Language: ASD-STE100 prose mode. One idea per sentence, at most 25 words. Active voice. No semicolons. No phrasal verbs. No marketing adjectives.
- Naming: **Cortex** for the product and its binaries (`cortex`, `cortex-envoy`). **AuthBridge** only for a frozen artifact: an image (`authbridge`, `authbridge-envoy`, `authbridge-lite`, `authbridge-cpex`), the `authbridge-proxy` container, a ConfigMap (`authbridge-config-<agent>`, `authbridge-runtime-config`), a header, an `AUTHBRIDGE_*` env var, the mode field. Table: root `CLAUDE.md`, section "Naming".
- Move, do not copy. Delete the old text, update every inbound link and anchor (`grep -rn '<file>#<anchor>' --include='*.md' --include='*.go' --include='*.sh' .`), leave at most one pointer line.
- Keep these paths valid: `docs/pricing.md`, `docs/praxis-policy.md` with anchor `#move-from-the-cpex-plugin`, `docs/session-export.md`, `docs/laptop-local-model.md`, `docs/laptop-service.md` (its section titles are named in comments in `cmd/agentop/cmd_service.go` and `cmd/agentop/cmd_claudecode.go`).
- Facts come from the code on `main`. Do not state counts that drift ("all fourteen", "seven keys"). Link the list instead.
- Do not touch `docs/plugin-tutorial.md` or any `docs/*-plugin.md` except `docs/lineage-telemetry-plugin.md` (#1943 owns them).
- Commits: `git commit -s`. End each message with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. PR title: `docs: Add the Kubernetes journey and clean up the contributor docs`.

## Facts checked for this plan

Checked on 2026-11-17. Use these values. Do not look them up again.

**Operator** (`rossoctl/operator` at `e9a2c22599bc41cf20aabf3b39dd9d6102a97145`):

- `operator/internal/webhook/injector/pod_mutator.go`, function `InjectAuthBridge`:
  - It injects only into a workload with label `rossoctl.io/type` = `agent`, or `tool` when the feature gate `injectTools` is on.
  - `rossoctl.io/inject: disabled` opts a workload out. Any other value, or no label, does not block injection.
  - Feature gate `globalEnabled: false` turns off all injection.
- Feature gates: `operator/internal/webhook/config/feature_gates.go`. ConfigMap `rossoctl-feature-gates` in the operator's release namespace, key `feature-gates.yaml`. Defaults (`charts/operator/values.yaml`): `globalEnabled: true`, `injectTools: false`.
- Webhook `inject.rossoctl.io` (`charts/operator/templates/webhook/webhooks.yaml`): acts on pod CREATE only. Its `namespaceSelector` needs the namespace label `rossoctl-enabled: "true"`, and excludes `kube-system`, `kube-public`, `kube-node-lease` and the operator's own namespace. Its `objectSelector` repeats the two labels above. So an existing pod gets no sidecar until it is re-created.
- Mode resolution in `InjectAuthBridge` (first non-empty wins): 1. `mode:` in the namespace `authbridge-runtime-config` ConfigMap, 2. the deprecated annotation `rossoctl.io/authbridge-mode`, 3. `proxy-sidecar`. **The injector does not read `AgentRuntime.Spec.AuthBridgeMode`** at this commit. The CRD field exists (`operator/api/v1alpha1/agentruntime_types.go`) and `operator/internal/webhook/v1alpha1/agentruntime_webhook.go` validates it. Decision: the doc states the chain the injector runs, and says the CRD field is validated but the injector does not use it yet. Reason: rule 6, the code wins over `docs/architecture.md`.
- Mode values (`operator/internal/webhook/injector/constants.go`): `proxy-sidecar` (default), `envoy-sidecar`, `lite` (the proxy-sidecar shape with the `authbridge-lite` image, the Cortex config `mode:` stays `proxy-sidecar`), `waypoint` (a standalone deployment, not injected). Decision: the issue says "remove lite from the mode list". The operator accepts `lite` as a mode value. So `kubernetes.md` lists the two shapes `proxy-sidecar` and `envoy-sidecar`, and then says in one line that the operator also accepts `lite` (an image choice for the proxy-sidecar shape) and `waypoint` (not injected).
- Container names: `AuthBridgeProxyContainerName = "authbridge-proxy"` (`constants.go`) in proxy-sidecar and lite. `EnvoyProxyContainerName = "envoy-proxy"` and `ProxyInitContainerName = "proxy-init"` (`container_builder.go`).

**External pages** (`curl -sI` returned 200):

- Quickstart: `https://www.rossoctl.dev/docs/get-started/kubernetes/`
- Install reference: `https://github.com/rossoctl/rossoctl/blob/main/docs/operate/install-kubernetes.md`
- Troubleshooting: `https://www.rossoctl.dev/docs/operate/troubleshooting/`

**This repo** (base `6b73c75dc3632841cd8fd5974e3eddb44cc61a97`):

- Anchors that exist: `docs/session-api.md#quick-examples` (the port-forward recipe), `docs/framework-architecture.md#9-config-hot-reload` (has `kubectl port-forward ... 9093:9093` and `curl http://localhost:9093/reload/status`), `docs/framework-architecture.md#example` (the abbreviated config shape), `docs/framework-architecture.md#operator-configuration-session` (the `session:` table), `deploy/lineage-attach/README.md#troubleshooting`.
- A refused reload: the active pipeline keeps serving, `reloads_failed` increments, `/reload/status` names the error. `mode`, `listener.*`, `session.*`, `cost_ledger`, `mtls`, `tls_bridge`, `spiffe`, `memory`, `stats` need a pod restart.
- The per-agent config: ConfigMap `authbridge-config-<agent>`, mounted at `/etc/authbridge/config.yaml`. Edit with `kubectl edit configmap authbridge-config-<agent> -n <ns>`. Kubelet syncs the mount in about 60 s.
- Ports: health `:9091`, stats `:9093`, session API `:9094`.
- `docs/plugin-catalog.md` has no "Where" column yet (#1943 adds it, open). Link the catalog. Do not name the column.
- Makefile targets: `make agentop`, `make cortex` (`PROFILE=full|lite|local`, default `full`), `make dev-install` (runs `./bin/agentop setup --from ./bin --yes --no-modify-path --restart`), `make lint` (= `pre-commit run --all-files`), `make test` (`-short` by default, `TEST_FLAGS=` for the full suite), `make pre-commit` (= `pre-commit install --hook-type pre-commit --hook-type commit-msg`), `make build-proxy-init` (= `cd deploy/proxy-init && make docker-build-init`).
- Go modules (7): `core`, `core/storage/redis`, `cmd/cortex`, `cmd/cortex-envoy`, `cmd/agentop`, `scripts/profile-tags`, `scripts/readme-demo`.
- Praxis, from `praxis/` (same as `.github/workflows/praxis.yaml`): `rustup toolchain install`, `cargo fmt --check`, `cargo clippy --all-targets --locked -- -D warnings`, `cargo test --locked`.
- `.pre-commit-config.yaml` hooks: `trailing-whitespace`, `end-of-file-fixer`, `check-added-large-files`, `check-yaml`, `check-json`, `check-merge-conflict`, `mixed-line-ending`, `ai-assisted-by-trailer` (commit-msg stage), `check-doc-links`, `ruff`, `ruff-format`. No Go hook and no Helm hook.
- Unlinked `.md` files on base (outside `docs/superpowers/` and the allowed exceptions `core/pipeline/README.md`, `core/plugins/README.md`, `docs/proposals/`): `praxis/README.md`, `deploy/lineage-attach/test/propagation/README.md`, `scripts/readme-demo/README.md`. `deploy/lineage-attach/README.md` is linked only from the root `CLAUDE.md`.
- `core/sessionexport/doc_example_test.go` reads the **first** ` ```json ` block in `docs/session-export.md` and strict-decodes it into `Record`. It needs non-zero `inputTokens`, `cacheReadTokens` and `outputTokens`.

## Review Focus

- A reader who labels a pod in a namespace without `rossoctl-enabled: "true"` gets no sidecar. T1's troubleshooting row must name this cause.
- A reader with a running Deployment adds the label and sees no sidecar, because the webhook acts on pod CREATE. T1 must say to restart the rollout (`kubectl rollout restart deploy/<name> -n <ns>`).
- The merged export doc's first ` ```json ` block must stay the full record example, or `TestDocExampleRecordMatchesWire` fails (T5).
- A reader in envoy-sidecar mode looks for `authbridge-proxy` and finds `envoy-proxy`. T1's verify step names both.
- Old links to `session-export-guide.md` and its anchors (in-repo) must all move to `session-export.md` anchors (T5).

---

### Task T1: Make `docs/kubernetes.md` the journey

**Depends on:** none

**Files:**
- Modify: `docs/kubernetes.md`

- [ ] **Step 1:** Add `## The journey` after "What changes". A table with columns `Step | What to do | Where`. One row for each step, in this order:
  1. Prerequisites and install the platform: SPIRE, Keycloak and the operator. Link the quickstart and the install reference (URLs above).
  2. Deploy an agent: link the Weather Agent walkthrough (`https://github.com/rossoctl/cortex-demos/blob/main/weather-agent/demo-ui.md`).
  3. Opt a workload in: link `#opt-a-workload-in`.
  4. Verify: link `#verify-the-sidecar`.
  5. Configure plugins: name the ConfigMap `authbridge-config-<agent>` (mounted at `/etc/authbridge/config.yaml`, edit with `kubectl edit configmap authbridge-config-<agent> -n <ns>`, kubelet syncs it in about 60 s). Link `framework-architecture.md#example`, `framework-architecture.md#9-config-hot-reload` and `plugin-catalog.md`.
  6. Troubleshoot: link `#troubleshooting`.
  Merge the old "Getting a cluster" and "Start here" text into this table and the topic table. Do not keep both.
- [ ] **Step 2:** Add `## Opt a workload in` with the operator facts above: namespace label, workload label, opt-out label, `injectTools`, `globalEnabled`, pod CREATE only, mode chain, mode values. Put this HTML comment at the top of the section: `<!-- Checked against rossoctl/operator e9a2c225: operator/internal/webhook/injector/pod_mutator.go (InjectAuthBridge), constants.go, charts/operator/templates/webhook/webhooks.yaml. -->`. Give this minimal example:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: team1
  labels:
    rossoctl-enabled: "true"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-agent
  namespace: team1
spec:
  selector:
    matchLabels: {app: my-agent}
  template:
    metadata:
      labels:
        app: my-agent
        rossoctl.io/type: agent      # opt in
        # rossoctl.io/inject: disabled   # opt out
    spec:
      containers:
        - name: agent
          image: <your-agent-image>
```

- [ ] **Step 3:** Add `## Verify the sidecar`: `kubectl get pod <pod> -n <ns> -o jsonpath='{.spec.containers[*].name}'` shows `authbridge-proxy` (proxy-sidecar or lite) or `envoy-proxy` (envoy-sidecar). `kubectl logs <pod> -n <ns> -c authbridge-proxy`. Session API: link `session-api.md#quick-examples`. Reload status: link `framework-architecture.md#9-config-hot-reload`. Point `agentop` at the pod (keep the existing sentence about its Namespaces → Pods picker).
- [ ] **Step 4:** Add `## Troubleshooting`, a `Symptom | Cause | Fix` table: no sidecar (namespace label missing, workload label missing or `rossoctl.io/inject: disabled`, `type: tool` with `injectTools` off, `globalEnabled` off, pod created before the label: restart the rollout). A refused reload (an unreloadable field changed: `/reload/status` names it, `reloads_failed` increments, restart the pod). Lineage: link `../deploy/lineage-attach/README.md#troubleshooting`. Platform problems: link `https://www.rossoctl.dev/docs/operate/troubleshooting/`.
- [ ] **Step 5:** In the topic table, change the "Deployment modes" row to `proxy-sidecar, envoy-sidecar` and link `../cmd/README.md`. Remove `lite` from it. Keep "A note on trust boundaries".
- [ ] **Step 6:** Run `python3 scripts/dev/check-doc-links.py`. Expected: exit 0.
- [ ] **Step 7:** Commit `docs: Add the Kubernetes journey to kubernetes.md`.

Estimated counted lines: 150.

### Task T2: Architecture and reference cleanup

**Depends on:** none

**Files:**
- Modify: `docs/architecture.md`, `cmd/README.md`, `core/README.md`, `docs/framework-architecture.md`, `core/llmclient/README.md`, `deploy/proxy-init/README.md`, `deploy/sparc-service/README.md`, `docs/idp-plugin-contract.md`, `SECURITY.md`

- [ ] **Step 1:** `docs/architecture.md`:
  - H1 becomes `# Cortex in Kubernetes: sidecar architecture`. Add after it: one sentence that says the page is for operators and contributors who run Cortex as the AuthBridge sidecar. Change "token management for Kubernetes workloads" to describe the sidecar, not the product.
  - Replace both tables, the "only these two modes" paragraph and the mode-chain sentence in "Deployment Modes" with two lines: images and modes are in `../cmd/README.md`, and how the operator picks a mode is in `kubernetes.md`. Link `kubernetes.md` with no anchor (T1 adds the anchors at the same time).
  - Replace "Prerequisites" and "Quick Setup" with one line that links `kubernetes.md`.
  - In the profile table, replace `all fourteen` with a link to `../scripts/profile-tags/profiles.go`.
  - Keep the headings `## Deployment Modes`, `### \`spiffe:\` configuration` and `## Build-tag plugin selection` with their text unchanged: `CONTRIBUTING.md`, `core/plugins/README.md` and `docs/configuration.md` link the anchors `#build-tag-plugin-selection` and `#spiffe-configuration`.
  - Replace "Component Documentation" with one line that links `../cmd/README.md` and `../core/README.md`. Keep the ClientRegistrationReconciler sentence only once in the file.
- [ ] **Step 2:** `cmd/README.md`: title `# Cortex binaries`. First sentence: "Two sidecar binaries, `cortex` and `cortex-envoy`, plus the `agentop` TUI." Replace the `../CLAUDE.md#config-hot-reload-9093reloadstatus` link with `../docs/framework-architecture.md#9-config-hot-reload`.
- [ ] **Step 3:** `core/README.md`: line 62's `CLAUDE.md` link becomes `../docs/framework-architecture.md#operator-configuration-session`. Keep line 25's link (no user doc holds the frozen-name table).
- [ ] **Step 4:** `docs/framework-architecture.md`: "AuthBridge's plugin pipeline" (line 3) → "Cortex's plugin pipeline". "AuthBridge intercepts" (line 25) → "Cortex intercepts". "(AuthBridge is pre-1.0)" (line 1060) → "(Cortex is pre-1.0)". Keep the `CLAUDE.md#gotchas-and-known-issues` link at line 915: no user doc describes the intent pin. Leave ConfigMap, image and path names.
- [ ] **Step 5:** `core/llmclient/README.md`: "authbridge plugins" → "Cortex plugins" (line 3). "the same authbridge listener" → "the same Cortex listener" (line 59).
- [ ] **Step 6:** `deploy/proxy-init/README.md` lines 18 and 22: "AuthBridge's outbound listener (port 15123)" → "Envoy's outbound listener (port 15123)", and the inbound one (port 15124) → "Envoy's inbound listener". These are redirect-mode lines.
- [ ] **Step 7:** `deploy/sparc-service/README.md` lines 8 and 19: "AuthBridge's Go `sparc` plugin" → "Cortex's Go `sparc` plugin", "the AuthBridge plugin" → "the Cortex plugin".
- [ ] **Step 8:** `docs/idp-plugin-contract.md` line 3: `> **Status:** Implemented — cortex#481`.
- [ ] **Step 9:** `SECURITY.md`: the "Pre-commit Hooks" item lists the hooks from "Facts checked". Remove "go fmt/vet" and "Helm lint".
- [ ] **Step 10:** Run `python3 scripts/dev/check-doc-links.py`. Expected: exit 0. Then `grep -rn 'CLAUDE.md' cmd/README.md docs/architecture.md`. Expected: no output.
- [ ] **Step 11:** Commit `docs: Fix names and duplicate tables in the reference docs`.

Estimated counted lines: 100.

### Task T3: Link the orphans

**Depends on:** none

**Files:**
- Modify: `docs/lineage-telemetry-plugin.md`, `docs/praxis-policy.md`, `deploy/lineage-attach/README.md`

- [ ] **Step 1:** `docs/lineage-telemetry-plugin.md`: add a last line that links `../deploy/lineage-attach/README.md` as the kit that attaches the plugin to a live Deployment.
- [ ] **Step 2:** `docs/praxis-policy.md`: add one line that links `../praxis/README.md` for the Rust dev loop (build and test `cortex-gateway`). Do not change any heading: `#move-from-the-cpex-plugin` must stay.
- [ ] **Step 3:** `deploy/lineage-attach/README.md`, "Files" table: make `test/propagation/` a link to `test/propagation/README.md`.
- [ ] **Step 4:** Run `python3 scripts/dev/check-doc-links.py`. Expected: exit 0.
- [ ] **Step 5:** Commit `docs: Link the lineage kit and the praxis dev loop`.

Estimated counted lines: 10.

### Task T4: `CONTRIBUTING.md` starts with the dev loop

**Depends on:** none

**Files:**
- Modify: `CONTRIBUTING.md`

- [ ] **Step 1:** Add `## Build, test, lint, run` as the first section after the intro. One code block for each group, with a one-line label:
  - Build: `make agentop`, `make cortex` (`PROFILE=full|lite|local`).
  - Install and run: `make dev-install`. Add a warning block: it restarts the one shared local proxy and cuts every attached session.
  - Test: `scripts/dev/go-test-shards.sh -short cmd/agentop` while you change code. Per module, from the module directory: `GOWORK=off go test -race ./...`. Name the 7 modules. Also `make test`.
  - Lint: `go vet ./...` and `gofmt -l .` per module ("Always give `gofmt` a path"), `make lint`.
  - Hooks: `pre-commit install --hook-type pre-commit --hook-type commit-msg` (or `make pre-commit`).
  - Praxis, from `praxis/`: the four commands from "Facts checked". Link `praxis/README.md`.
  - README demo: link `scripts/readme-demo/README.md` (regenerates `docs/assets/cortex-demo.svg`).
- [ ] **Step 2:** Move "Contributing Code" (the rossoctl contribution-guide link) below the new section.
- [ ] **Step 3:** "Development Setup": remove the proxy-init build and the dev-loop prose that the new section now holds. Change `pre-commit install` to the full hook command. Keep clone and prerequisites.
- [ ] **Step 4:** "Testing against a local cluster": add at the top, from the existing text only: what works today (install the platform, build and load `:local` images, run `verify-spire-keycloak.sh`, deploy a demo) and what does not (the platform does not run the `:local` images, because the image-override overlay was removed from `rossoctl`). Move the proxy-init build here as `make build-proxy-init`. Remove the old "This step is inert" note, so the fact is stated once. Do not invent a fix.
- [ ] **Step 5:** "Code Style → Go Code": remove each command the new section now states, and keep the reasons (CI gates, `go mod tidy -diff`, license header).
- [ ] **Step 6:** Run `python3 scripts/dev/check-doc-links.py` and `make -n agentop cortex dev-install lint test pre-commit build-proxy-init`. Expected: both exit 0.
- [ ] **Step 7:** Commit `docs: Start CONTRIBUTING.md with build, test and lint commands`.

Estimated counted lines: 110.

### Task T5: Merge the export docs

**Depends on:** none

**Files:**
- Modify: `docs/session-export.md`, `docs/README.md`
- Delete: `docs/session-export-guide.md`
- Test: `core/sessionexport/doc_example_test.go` (no change, it must still pass)

- [ ] **Step 1:** Rewrite `docs/session-export.md` (keep the path and the H1 `# Export sessions for analysis`). Order:
  1. The intro (what the export is, a month is about 12 MB) and the guide's "Terms used on this page".
  2. The guide's Step 1 to Step 4. Step 1's `jq '.archive'` reply block: change its fence from ` ```json ` to a bare ` ``` `. Step 3's record example: use the reference's full record example (the ` ```json ` block now under "What a record is"). It must be the first ` ```json ` block in the file. Reword the "SECRET-PROMPT-DO-NOT-EXPORT" sentence so that it does not refer to the old example's run.
  3. "Answer a question with the export" and "Three traps".
  4. "Export from a cluster sidecar", "Use the export in a script", "Read the refusals".
  5. The field reference: "What a record is" (prose only, no second example), "Reading rules", "Selection", "`agentop export`" flags, "What it does not carry, and why", "Truncation, not corruption".
  Each command and each flag appears once. Remove the intro's command block where Step 3 and Step 4 show the same commands. Remove the guide's "See also" item for `session-export.md`. Merge the rest of "See also" in at the end.
- [ ] **Step 2:** Make the table of contents link the new headings. Where the reading rules and the traps say the same rule, keep the trap's example and make the rule one line that links the trap.
- [ ] **Step 3:** `git rm docs/session-export-guide.md`.
- [ ] **Step 4:** `docs/README.md`: in "Daily use", the export row is `[`session-export.md`](session-export.md)` only. In "Reference → Kubernetes", add a row: "Keep whole sessions from a cluster as files | [`session-dump.md`](session-dump.md)".
- [ ] **Step 5:** Run `grep -rn 'session-export-guide' --include='*.md' --include='*.go' --include='*.sh' --exclude-dir=superpowers --exclude-dir=.claude .`. Expected: no output. Run `python3 scripts/dev/check-doc-links.py`. Expected: exit 0. Run `(cd core && GOWORK=off go test -run TestDocExampleRecordMatchesWire ./sessionexport/)`. Expected: `ok`.
- [ ] **Step 6:** Commit `docs: Merge the export walkthrough into session-export.md`.

Estimated counted lines: 1360 (596 deleted with the guide, about 160 removed and 600 added in the rewrite of `session-export.md`).

## PRs

Total estimated counted lines: about 1730. That is at most 2000, so there is one PR.

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `docs: Add the Kubernetes journey and clean up the contributor docs` | none | T1, T2, T3, T4, T5 | 1730 |

## Gate

Run from the worktree root:

```sh
python3 scripts/dev/check-doc-links.py && pre-commit run --all-files && (cd core && GOWORK=off go test ./sessionexport/) && test ! -e docs/session-export-guide.md && ! grep -rn 'session-export-guide' --include='*.md' --include='*.go' --include='*.sh' --exclude-dir=superpowers --exclude-dir=.claude . && grep -q 'rossoctl.io/inject: disabled' docs/kubernetes.md && grep -q 'rossoctl-enabled' docs/kubernetes.md && python3 -c "import re,pathlib as P;R=P.Path('.').resolve();S={'.git','.claude','node_modules','testdata'};M=[p for p in R.rglob('*.md') if not S&set(p.relative_to(R).parts) and not str(p.relative_to(R)).startswith('docs/superpowers')];L={(p.parent/t.split('#')[0]).resolve() for p in M for t in re.findall(r'\]\(([^)\s#]+)',p.read_text(errors='ignore')) if '://' not in t};L|={q/'README.md' for q in L if q.is_dir()};O=[str(p.relative_to(R)) for p in M if p.resolve() not in L and not re.match(r'core/(pipeline|plugins)/README.md|docs/proposals/',str(p.relative_to(R)))];print(*O,sep='\n');raise SystemExit(bool(O))"
```

The gate has no format or vet step: no task changes Go, Rust or Python code. On the unchanged base (checked 2026-11-17), `check-doc-links.py` exits 0. The orphan check exits 1 and lists the three base orphans named in "Facts checked". T3 and T4 link them. This is a fault in the base docs, not in how the step is called.

The orphan check lists each `.md` file outside `docs/superpowers/` that no other `.md` file links, except `core/pipeline/README.md`, `core/plugins/README.md` and `docs/proposals/`. It exits 1 when it finds one. The spellcheck job (pyspelling) runs only in CI. Add any word it rejects to `.github/spellcheck/.wordlist.txt`.

## For a human after review

- Needs: P1 reviewed. The external rossoctl Kubernetes quickstart is labeled v0.8 but runs `git checkout v0.7.0`. Report it to the rossoctl site owners. Steps: open `https://www.rossoctl.dev/docs/get-started/kubernetes/`, confirm the mismatch, and open an issue in `rossoctl/rossoctl` with the page URL and the two version strings.
- Not a doc task: `AgentRuntime.Spec.AuthBridgeMode` is validated but the injector does not read it (operator `e9a2c225`). Decide whether to report it to `rossoctl/operator`.
