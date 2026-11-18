# Central metrics stack on OpenShift — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `oc apply -k deploy/central/overlays/openshift` deploys the OTel Collector, Prometheus,
Alertmanager and Grafana in `cortex-central`, receiving authenticated OTLP pushes from Cortex and
context-guru proxies.

**Architecture:** Config files ported verbatim from context-guru `deploy/central/` become ConfigMaps
through Kustomize `configMapGenerator`; plain Deployments, Services and PVCs in `base/`; Routes,
NetworkPolicy and hosts in `overlays/openshift/`. Static checks run in containers from one script.

**Tech Stack:** Kustomize (in `oc` 4.20 / Kustomize 5.6), OpenShift Routes, otelcol-contrib 0.165.0,
Prometheus v3.2.1, Alertmanager v0.28.1, Grafana 11.6.0, kubeconform, POSIX sh.

**Spec:** `docs/superpowers/specs/2026-11-18-central-collector-openshift-design.md`

## Global Constraints

- Source of ported files: `rossoctl/context-guru` commit `cd90d55cae982e29fcf026a34a25834ef35a64b9`
  (`origin/main`, 2026-11-18). Record it in `deploy/central/README.md`.
- Images: `docker.io/otel/opentelemetry-collector-contrib:0.165.0`, `docker.io/prom/prometheus:v3.2.1`,
  `docker.io/prom/alertmanager:v0.28.1`, `docker.io/grafana/grafana:11.6.0`.
- Service names: `otel-collector`, `prometheus`, `alertmanager`, `grafana`.
- Ports: collector 4318 (Cortex), 4319 (context-guru), 8888 (own metrics), 13133 (health);
  Prometheus 9090; Alertmanager 9093; Grafana 3000.
- Prometheus flags: `--web.enable-remote-write-receiver`, `--storage.tsdb.retention.time=30d`,
  `--storage.tsdb.retention.size=20GB`.
- PVCs: Prometheus 25Gi, Alertmanager 1Gi, Grafana 1Gi; no `storageClassName`; `strategy: Recreate`.
- No `runAsUser`; `restricted-v2` SCC; `allowPrivilegeEscalation: false`, drop ALL caps,
  `runAsNonRoot: true`, `seccompProfile: RuntimeDefault`.
- Secrets created by the operator, never in the repo: `collector-tokens` (keys `cortex.txt`,
  `context-guru.txt`), `grafana-admin` (key `password`).
- Hosts: collector `cortex-central.apps.epoc001.ete14.res.ibm.com` (paths `/cortex`,
  `/context-guru`, rewrite-target `/`, edge TLS, insecure policy `None`); Grafana
  `grafana-cortex-central.apps.epoc001.ete14.res.ibm.com` (edge TLS, insecure policy `Redirect`).
- No Route for Prometheus or Alertmanager.
- Namespace `cortex-central` exists already; the overlay sets `namespace:`. The base creates no
  Namespace object (deviation from spec §5.1: the user may not have rights to create namespaces,
  and the namespace exists).
- Commits: `git commit -s`, trailer `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`.

## Review Focus

1. A push with no token or a wrong token: 401, nothing stored. → Task 5 smoke test.
2. A plain `http://` push to the collector host: refused, not redirected. → Task 5 smoke test.
3. A sender that adds its own resource attributes or a `proxy` attribute: dropped / overwritten. → Task 5
   smoke test pushes `proxy="evil"` and an extra attribute and checks the stored labels.
4. A pod restart: Prometheus series, Grafana user and Alertmanager data survive under an arbitrary UID. →
   Task 5 restart step.
5. A config edit: the pods roll (hash-suffixed ConfigMaps); a Secret edit needs `oc rollout restart`. →
   Task 3 render check asserts hash suffixes; README documents the restart.

---

### Task 1: Port the config files and the static check script

**Files:**
- Create (verbatim from the pinned commit): `deploy/central/base/otel-collector/otel-collector.yml`,
  `deploy/central/base/prometheus/prometheus.yml`, `deploy/central/base/prometheus/rules/recording.yml`,
  `deploy/central/base/prometheus/rules/alerts.yml`, `deploy/central/base/prometheus/tests/alerts_test.yml`,
  `deploy/central/base/alertmanager/alertmanager.yml`,
  `deploy/central/base/grafana/provisioning/datasources/central.yml`,
  `deploy/central/base/grafana/provisioning/dashboards/central.yml`,
  `deploy/central/base/grafana/dashboards/*.json`, `deploy/central/base/grafana/gen_dashboards.py`
- Create: `deploy/central/check.sh`

- [x] **Step 1:** Write `check.sh` (POSIX sh, `set -eu`, runs from any cwd, uses `docker` or `podman`):
  `promtool check rules`, `promtool test rules` (entrypoint `promtool` in the Prometheus image, the
  `tests/` file's `rule_files` paths must resolve inside the mount), `otelcol-contrib validate`,
  `python3 gen_dashboards.py --check`, and — once `base/kustomization.yaml` exists —
  `kubectl kustomize` (or `oc kustomize`) of the overlay piped to `kubeconform -strict -summary
  -ignore-missing-schemas`.
- [x] **Step 2:** Run `sh deploy/central/check.sh`. Expected: FAIL, files missing.
- [x] **Step 3:** Copy the files with `git show <sha>:deploy/central/<path>`. Fix only paths the move
  breaks (the test file's `rule_files`; `gen_dashboards.py`'s output directory if it is relative to
  its own location — it is, so no change).
- [x] **Step 4:** Run `check.sh`. Expected: rules OK, rule tests PASS, collector config valid, dashboards
  up to date.
- [x] **Step 5:** Commit `feat(central): Port the central stack config from context-guru`.

### Task 2: Turn on bearer-token authentication in the collector

**Files:** Modify `deploy/central/base/otel-collector/otel-collector.yml`

- [x] **Step 1:** Uncomment the two `bearertokenauth` extensions, the two receiver `auth:` lines, and add
  both names to `service.extensions`. Replace the header comment's "AUTHENTICATION IS OFF" paragraph
  with: one token per proxy kind, read from `/etc/otelcol/tokens/<kind>.txt` (Secret
  `collector-tokens`), read once at start.
- [x] **Step 2:** Run `check.sh`. Expected: `otelcol-contrib validate` passes. (The validate container
  needs the token files; `check.sh` mounts two dummy files at `/etc/otelcol/tokens/`.)
- [x] **Step 3:** Commit `feat(central): Require a bearer token on both receivers`.

### Task 3: Base manifests

**Files:** Create `deploy/central/base/kustomization.yaml` and, in each component directory,
`deployment.yaml`, `service.yaml`, and `pvc.yaml` (Prometheus, Alertmanager, Grafana).

**Interfaces:**
- Produces: ConfigMap generator names `otel-collector-config`, `prometheus-config`, `prometheus-rules`,
  `alertmanager-config`, `grafana-provisioning-datasources`, `grafana-provisioning-dashboards`,
  `grafana-dashboards`; labels `app.kubernetes.io/part-of: cortex-central`,
  `app.kubernetes.io/name: <service name>`.

- [x] **Step 1:** Write the manifests to the Global Constraints. Mounts: collector config at
  `/etc/otelcol/config.yml` (args `--config=/etc/otelcol/config.yml`), tokens Secret at
  `/etc/otelcol/tokens` (readOnly); Prometheus config at `/etc/prometheus/prometheus.yml`, rules at
  `/etc/prometheus/rules`, PVC at `/prometheus`; Alertmanager config at
  `/etc/alertmanager/alertmanager.yml`, PVC at `/alertmanager`; Grafana provisioning at
  `/etc/grafana/provisioning/{datasources,dashboards}`, dashboards at
  `/var/lib/grafana/dashboards/central`, PVC at `/var/lib/grafana`, env `GF_AUTH_ANONYMOUS_ENABLED=false`,
  `GF_USERS_ALLOW_SIGN_UP=false`, `GF_ANALYTICS_REPORTING_ENABLED=false`,
  `GF_SECURITY_ADMIN_PASSWORD` from Secret `grafana-admin`/`password`. Probes per spec §5.2. Collector
  memory limit 768Mi.
  Note: the dashboards ConfigMap must stay under 1 MiB; check its size.
- [x] **Step 2:** Run `check.sh`. Expected: kustomize renders, kubeconform reports 0 invalid, every
  generated ConfigMap name ends in a hash suffix.
- [x] **Step 3:** Commit `feat(central): Add the base manifests`.

### Task 4: OpenShift overlay

**Files:** Create `deploy/central/overlays/openshift/kustomization.yaml`, `routes.yaml`,
`networkpolicy.yaml`; `deploy/central/components/README.md` (names `events/` as the next component).

- [x] **Step 1:** Overlay: `namespace: cortex-central`, `resources: [../../base, routes.yaml,
  networkpolicy.yaml]`. Routes per Global Constraints. NetworkPolicy: Prometheus accepts ingress only
  from pods in the namespace; Alertmanager likewise; collector and Grafana accept ingress from the
  router (`policy-group.network.openshift.io/ingress: ""` namespace label) and from the namespace.
- [x] **Step 2:** Run `check.sh`. Expected: render + kubeconform OK.
- [x] **Step 3:** Run `oc apply -k deploy/central/overlays/openshift --dry-run=server`. Expected: every
  object "created (server dry run)".
- [x] **Step 4:** Commit `feat(central): Add the OpenShift overlay`.

### Task 5: Deploy and smoke test on epoc001

**Files:** Create `deploy/central/smoke.sh`

- [x] **Step 1:** Write `smoke.sh HOST TOKEN_CORTEX TOKEN_CG`. It POSTs an OTLP/JSON sum
  (`smoke_test_total`, cumulative, monotonic) with resource attributes `service.instance.id=smoke-<ts>`,
  `proxy=evil`, `extra=x` to `https://HOST/cortex/v1/metrics` and `/context-guru/v1/metrics`, and checks:
  each 2xx; no-token and wrong-token pushes give 401; `http://HOST/cortex/v1/metrics` is not 2xx. Then it
  queries Prometheus through `oc exec deploy/prometheus -- wget -qO- 'localhost:9090/api/v1/query?...'`
  until `smoke_test_total{machine="smoke-<ts>"}` appears (≤ 60 s), and asserts the label set is exactly
  `__name__, machine, proxy, job?/instance?` per the collector's kept set, with `proxy` = `cortex` and
  `context-guru` respectively (not `evil`), and no `extra`.
- [x] **Step 2:** Create the two Secrets (user runs the commands from the README), `oc apply -k`. Wait for
  4/4 pods Ready.
- [x] **Step 3:** Run `smoke.sh`. Expected: all checks PASS.
- [x] **Step 4:** Restart: `oc delete pod -l app.kubernetes.io/part-of=cortex-central`; wait Ready; re-query
  the smoke series. Expected: still present. Grafana login with the admin password still works.
- [x] **Step 5:** Real Cortex push: run a local Cortex with `metrics_push.url:
  https://cortex-central.apps.epoc001.ete14.res.ibm.com/cortex`, the token, `interval: 10s`, send one
  request; expect `llm_savings_requests_total{proxy="cortex"}` (or `llm_proxy_export_failures_total`
  staying 0) in Prometheus. If TLS fails, record it and fix per spec §5.3.
- [x] **Step 6:** Commit `test(central): Add the smoke test`.

### Task 6: Docs and CI

**Files:** Create `deploy/central/README.md`; modify `CLAUDE.md` (directory tree: add `deploy/central/`),
`docs/laptop-service.md` (`metrics_push` section: point at `deploy/central/README.md` for URL and token),
`.github/workflows/ci.yaml` (job `central-stack`: checkout, `sh deploy/central/check.sh`, timeout 10 min,
ubuntu-latest has docker and kubectl).

- [x] **Step 1:** README per spec §5.6: what it is, layout, prerequisites (Secrets commands), deploy,
  status, proxy URLs + token settings, token rotation, what is stored, failure table, Grafana users,
  alert delivery, tests (`check.sh`, `smoke.sh`), pinned source commit, events as the next component.
- [x] **Step 2:** Run `check.sh` and `pre-commit run --files <changed>`. Expected: PASS.
- [x] **Step 3:** Commit `docs(central): Document the OpenShift central stack`.
