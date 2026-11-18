# Central metrics collector on OpenShift — design

Issue: #898. Stacked on PR #1956 (`feat/accountant-integration`), which adds the `metrics_push`
block and pushes the `accountant/metrics` counters (`llm.savings.*`, `llm.proxy.export_failures`)
over OTLP/HTTP to `<url>/v1/metrics`.

## 1. Intent

PR #1956 makes each Cortex proxy push its savings counters to a central collector. Today the only
central collector is the context-guru VM stack (`rossoctl/context-guru` `deploy/central/`: podman
containers on a RHEL 9 host, nginx in front). This PR defines a new central stack in the cortex
repo and runs it on OpenShift. **It replaces the context-guru VM**, so it serves Cortex and
context-guru proxies alike.

The VM stack's design said "Kubernetes: not in scope". This PR is that missing piece, owned by
cortex.

Later, the same stack also receives structural **events** (accountant PR #86, `docs/events/events.md`):
an OTLP logs pipeline in the same collector, written to ClickHouse. That is a later PR, but this
PR's layout must leave room for it (section 5.4).

## 2. Goals, non-goals, anti-goals

**Goals**

1. G1. One `oc apply -k` deploys the full metrics stack on OpenShift: OTel Collector (contrib),
   Prometheus, Alertmanager, Grafana.
2. G2. Same architecture and behaviour as the VM stack: one receiver per proxy (4318 Cortex, 4319
   context-guru), `proxy` set by the collector from the receiver, the same four kept labels, remote
   write to Prometheus, the same recording rules, alerts and dashboards.
3. G3. Proxies keep the VM's URL scheme: `https://<host>/cortex` and `https://<host>/context-guru`.
   Moving a proxy from the VM to OpenShift changes only the host name.
4. G4. Data survives a pod restart (PVCs for Prometheus, Alertmanager, Grafana).
5. G5. Runs under OpenShift's default `restricted-v2` SCC. No cluster-admin, no CRDs, no operators.
6. G6. The layout takes the events component later as an addition, not a rewrite.

**Non-goals**

- Events, ClickHouse and the logs pipeline (later PR, after accountant #86 merges).
- Per-install tokens, token binding to a machine, mutual TLS (context-guru#1130). This PR turns on
  one shared bearer token per proxy kind only (section 5.5).
- HA or replicas. One replica of each component.
- Backups. A PVC snapshot policy is the cluster owner's.
- Retiring the context-guru VM. That is an operations step after this stack is live.
- Any Go change. PR #1956 already pushes in the shape this collector reads.

**Anti-goals** (failure even if it works)

- The stack depends on files in the context-guru repo at deploy time or at build time.
- A secret (Grafana admin password, later DB passwords) is committed to the repo.
- A sender can choose its own `proxy` label, or add labels beyond the kept set.
- The collector accepts a push with no valid token.
- Prometheus or Alertmanager get a Route.

## 3. Constraints

- **HTTPS only.** Cortex (`MetricsPushConfig.Validate`) and context-guru refuse plain `http` to a
  non-loopback host. The Route must terminate TLS with a certificate the proxies trust.
- **Restricted SCC.** OpenShift assigns an arbitrary UID and an `fsGroup`. Images must not need a
  fixed UID or root. Do not set `runAsUser`.
- **Kustomize only.** Built into `oc`/`kubectl` (`-k`). No Helm, no templating.
- **Self-owned config.** Every config file is copied into cortex. After the copy, cortex is the
  source of truth.
- **Cumulative temporality.** The remote-write exporter needs it. Do not set delta anywhere.

## 4. Approaches

| Approach | Trade-off | Decision |
|---|---|---|
| **A. Kustomize manifests** (Deployments, Services, Routes, PVCs; config files as ConfigMaps through `configMapGenerator`) | Plain YAML, maps one-to-one to the VM layout, nothing to install. Per-environment values change through patches, not a values file. | **Chosen.** |
| B. Helm chart | `values.yaml` for host, storage, images. More templating to maintain; worth it only if many teams install their own copy. | Rejected. |
| C. OpenShift operators (user-workload monitoring, Grafana Operator, OTel Operator) | Most native, but changes the architecture (`PrometheusRule` CRDs; UWM Prometheus does not accept remote write by default) and needs cluster-admin. | Rejected: not "same architecture as the VM". |
| Reuse context-guru's `deploy/central/` files | No copy to maintain. | Rejected by the user: cortex owns its own stack. |

The future events component (ClickHouse, schema migrations, more secrets, an optional part) was
weighed against this choice. It fits A as a Kustomize `component`; ClickHouse runs as a single
`StatefulSet`, not through the Altinity operator, for the same reason C was rejected.

## 5. Design

### 5.1 Layout

```
deploy/central/
  README.md                    what it is, deploy steps, proxy URLs, what is stored, auth-off warning
  base/
    kustomization.yaml         resources + configMapGenerator for every config file below
    namespace.yaml             namespace `cortex-central` (overlay may change it)
    otel-collector/            deployment.yaml, service.yaml, otel-collector.yml
    prometheus/                deployment.yaml, service.yaml, pvc.yaml, prometheus.yml,
                               rules/recording.yml, rules/alerts.yml, tests/alerts_test.yml
    alertmanager/              deployment.yaml, service.yaml, pvc.yaml, alertmanager.yml
    grafana/                   deployment.yaml, service.yaml, pvc.yaml,
                               provisioning/{datasources,dashboards}/central.yml,
                               dashboards/*.json, gen_dashboards.py
  overlays/openshift/
    kustomization.yaml         includes base; patches below
    routes.yaml                collector Route(s) and Grafana Route
    patches/                   route host, storage class, sizes, image overrides
  components/                  empty in this PR except a README naming `events/` as the next one
```

Files ported from context-guru `deploy/central/` at a pinned commit (record the commit SHA in
`deploy/central/README.md`): `otel-collector.yml`, `prometheus/prometheus.yml`,
`prometheus/rules/{recording,alerts}.yml`, `prometheus/tests/alerts_test.yml`,
`alertmanager/alertmanager.yml`, `grafana/provisioning/**`, `grafana/dashboards/*.json`,
`grafana/gen_dashboards.py`. Not ported: `install.sh`, `deploy.sh`, `nginx.example.conf`,
`central-proxy.inc`, `compose.yaml` (their jobs are done by the manifests and Routes).

### 5.2 Components

| Component | Image (same pins as the VM) | Service ports | Storage | Route |
|---|---|---|---|---|
| OTel Collector | `otel/opentelemetry-collector-contrib:0.165.0` | 4318 (Cortex), 4319 (context-guru), 8888 (own metrics), 13133 (health) | none | yes (5.3) |
| Prometheus | `prom/prometheus:v3.2.1`, flags `--web.enable-remote-write-receiver`, `--storage.tsdb.retention.time=30d`, `--storage.tsdb.retention.size=20GB` | 9090 | PVC 25Gi | **no** |
| Alertmanager | `prom/alertmanager:v0.28.1` | 9093 | PVC 1Gi | **no** |
| Grafana | `grafana/grafana:11.6.0`; sign-up off, anonymous off, analytics off | 3000 | PVC 1Gi (users, sessions) | yes, edge TLS |

- Service names match the VM's container names (`otel-collector`, `prometheus`, `alertmanager`,
  `grafana`), so the ported configs work unchanged (`http://prometheus:9090/api/v1/write`,
  `alertmanager:9093`, `otel-collector:8888`).
- Readiness/liveness: collector `:13133/`, Prometheus `/-/ready` and `/-/healthy`, Alertmanager
  `/-/ready`, Grafana `/api/health`.
- Resource requests/limits on every container. The collector's limit must sit above the
  `memory_limiter` `limit_mib: 512` (for example 768Mi).
- Deployments with `strategy: Recreate` for the three PVC-backed components (a RWO volume cannot be
  mounted by two pods during a rolling update).
- A config change rolls the pods: `configMapGenerator` adds a content hash to each ConfigMap name.

### 5.3 Ingress (Routes)

Proxies push to `<url>/v1/metrics`. To keep the VM's URL scheme, the collector gets two Routes on
one host, by path, with the router rewriting the path away:

| Route | Host | Path | Target | Proxy URL setting |
|---|---|---|---|---|
| `collector-cortex` | `<central host>` | `/cortex` | `otel-collector:4318` | `metrics_push.url: https://<host>/cortex` |
| `collector-context-guru` | `<central host>` | `/context-guru` | `otel-collector:4319` | `--metrics-push-url https://<host>/context-guru` |
| `grafana` | `<grafana host>` (or same host, `/grafana`) | `/` | `grafana:3000` | — |

- `tls.termination: edge`, `insecureEdgeTerminationPolicy: Redirect` (Grafana) / `None`
  (collector: a plain-http push must fail, not be redirected).
- Annotation `haproxy.router.openshift.io/rewrite-target: /` on the collector Routes.
- Certificate: the cluster's default ingress certificate. On the target cluster it is a Let's
  Encrypt wildcard for `*.apps.epoc001.ete14.res.ibm.com`, which the proxies' system trust stores
  accept (checked from a laptop on 2026-11-18). If a proxy fails TLS, the fix is an overlay patch
  with a supplied certificate; no proxy change is needed or possible (`MetricsPushConfig` has no CA
  option).
- Exposure: the proxies run on laptops and VMs on the IBM network. The router's address
  (`9.2.130.136`) is IBM-internal, so the Routes are not on the open internet, but the IBM network
  is large. Therefore the receivers require a bearer token (section 5.5). No IP allowlist.

### 5.4 Room for events (later PR)

The events PR adds `components/events/`: a ClickHouse `StatefulSet` + PVC, a schema `Job`
(typed tables, materialized views, curated views from events.md §9.1), Secrets referenced by name,
and a strategic-merge patch on the collector that adds a `logs` pipeline (`otlp` receiver,
E3 allowlist `transform`/`filter`, `clickhouse` exporter). To make that a patch and not a rewrite,
this PR:

1. Keeps the collector config as one file in one ConfigMap generator entry, so a component can
   replace it with `behavior: merge` or add a second file.
2. Leaves the collector Deployment's args as `--config=/etc/otelcol/config.yml`, so a component
   can append `--config=/etc/otelcol/events.yml` (the collector merges multiple configs).

### 5.5 Secrets

- Grafana admin password: a Secret `grafana-admin` (key `password`) that the operator creates
  before deploy (`oc create secret generic grafana-admin --from-literal=password=...`). The
  Deployment reads it through `GF_SECURITY_ADMIN_PASSWORD` `valueFrom.secretKeyRef`. Nothing in
  the repo holds a value. The README gives the command.
- **Push tokens (authentication on).** The ported `otel-collector.yml` keeps the
  `bearertokenauth/cortex` and `bearertokenauth/contextguru` blocks commented out. This PR
  uncomments them, the two `auth:` lines in the receivers, and the two names in
  `service.extensions`. Each extension reads its token from a file
  (`/etc/otelcol/tokens/cortex.txt`, `/etc/otelcol/tokens/context-guru.txt`) mounted from one
  Secret `collector-tokens` (keys `cortex.txt`, `context-guru.txt`) that the operator creates. The
  README gives the commands, for example
  `oc create secret generic collector-tokens --from-literal=cortex.txt=$(openssl rand -hex 32) ...`.
- One token per proxy kind, shared by every install of that kind. A sender with the Cortex token
  can still claim any `machine` (VM design §8.8 finding 3); that is accepted for now.
- Token rotation: update the Secret and restart the collector (`oc rollout restart`). The collector
  reads token files at start. A push during the restart fails, and the next push repairs it,
  because the totals are cumulative.
- Proxy side: Cortex `metrics_push.token` (or `METRICS_PUSH_TOKEN`); context-guru
  `--metrics-push-token`.

### 5.6 Docs

- `deploy/central/README.md`: ported from the VM README, rewritten for OpenShift (deploy, status
  with `oc get pods`, proxy URLs, what is stored, failure table, auth-off warning, Grafana users,
  alert delivery, tests, the pinned context-guru commit the files came from).
- `CLAUDE.md`: add `deploy/central/` to the directory tree.
- Cortex's `metrics_push` docs (from #1956): point at this README for the collector URL.

## 6. Verification

1. `oc kustomize deploy/central/overlays/openshift` (or `kubectl kustomize`) renders with no error.
2. `kubeconform` (or `oc apply --dry-run=server` on a cluster) accepts every object, including the
   Route.
3. `promtool check rules` and `promtool test rules prometheus/tests/alerts_test.yml` pass.
4. `otelcol-contrib validate --config=otel-collector.yml` passes.
5. On a real OpenShift cluster: all four pods ready under `restricted-v2`; a Cortex proxy with
   `metrics_push.url: https://<host>/cortex` (and a short interval) pushes; Prometheus shows
   `llm_savings_*` with `proxy="cortex"` and only the kept labels; a context-guru push on
   `/context-guru` shows `proxy="context-guru"`; a plain-http push to the Route fails.
6. Delete each PVC-backed pod; its data (a Prometheus series, a Grafana user, an Alertmanager
   silence) is still there after it restarts.

7. A push with no token, or a wrong token, gets 401 and stores nothing.

Steps 1–4 can run in CI. Steps 5–7 run on the target cluster (section 8).

## 7. Left to the builder

- Exact resource requests/limits and PVC sizes (start from the VM defaults).
- Whether to add a `NetworkPolicy` that limits Prometheus/Alertmanager to in-namespace clients
  (recommended).
- Whether to run `promtool`/`otelcol validate`/`kustomize` checks in a new CI job or a Makefile
  target.
- Image registry: keep `docker.io` or mirror to `quay.io`/an internal registry if the cluster
  blocks or rate-limits docker.io.

## 8. Target cluster and decisions

| Item | Value |
|---|---|
| Cluster | `epoc001.ete14.res.ibm.com` (console `https://console-openshift-console.apps.epoc001.ete14.res.ibm.com`) |
| Namespace | `cortex-central` (exists) |
| Collector host | `cortex-central.apps.epoc001.ete14.res.ibm.com` (paths `/cortex`, `/context-guru`) |
| Grafana host | `grafana-cortex-central.apps.epoc001.ete14.res.ibm.com` |
| Storage class | `ocs-storagecluster-ceph-rbd`, the cluster default (Ceph RBD, RWO, expandable). The PVCs set no `storageClassName` |
| Versions | OpenShift client 4.20, Kubernetes 1.33.12, Kustomize 5.6.0 (in `oc`) |
| Rights | the user can create Deployments, Routes, PVCs, Secrets, ConfigMaps and NetworkPolicies in `cortex-central` (checked 2026-11-18). The namespace is empty, with no quota or LimitRange |
| SCC | namespace UID range `1000870000/10000`; pods run as an arbitrary UID from it |

Decisions taken in the design conversation:

1. Proxies run outside the cluster, on the IBM network (user). Authentication may be
   reintroduced if needed (user); the design turns it on (section 5.5, designer's call: the
   network is internal but large).
2. Certificate: try the cluster's default ingress certificate and fix if a proxy fails (user). It
   is publicly trusted (checked).
3. Test on the cluster above (user). The builder needs an `oc login` session with edit rights in
   `cortex-central`.
