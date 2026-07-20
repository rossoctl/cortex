# Echo demo — credential placeholder swap

A minimal, UI-testable demo of AuthBridge's **credential placeholder swap**:
the user's real token never reaches the agent, yet the agent's outbound call
still arrives at the upstream carrying a correctly **exchanged** token.

## What it proves

When you chat `echo` with the agent in the rossoctl UI, the reply shows two
lines:

```
inbound:  abph_3f0c…           <- what the AGENT received
outbound: eyJhbGciOi…          <- what ECHO-UPSTREAM received (aud=echo-upstream)
```

- **inbound** is an opaque `abph_…` placeholder. The injected AuthBridge
  sidecar's inbound `jwt-validation` runs in `placeholder_mode`: it validates
  the user's real token, stashes it in the shared store, and forwards only the
  placeholder to the agent. The agent never sees the user's bearer.
- **outbound** is a real JWT. On the agent's outbound call to `echo-upstream`,
  the sidecar's `token-exchange` plugin resolves the placeholder back to the
  real token (`resolve_placeholders: true`) and exchanges it for one with
  `aud=echo-upstream`. `echo-upstream` simply returns the `Authorization`
  header it received, giving you ground truth.

`echo-upstream` carries **no** `rossoctl.io/*` labels, so the operator's webhook
leaves it un-injected — what it logs is exactly what left the agent's sidecar.

## Prerequisites

- A running rossoctl **kind** cluster (`rossoctl-system` + `team1` namespaces).
  See the [install guide](https://github.com/rossoctl/rossoctl/blob/main/docs/install.md).
- `kubectl`, `kind`, `podman` or `docker`.
- `python3` + PyYAML (config merge) and `python-keycloak` (Keycloak setup):
  `pip install pyyaml python-keycloak`.

## Run it

```bash
cd authbridge/demos/echo
make demo-echo
```

`demo-echo` runs, in order:

```
preflight → build-sidecar → load-sidecar → override-sidecar-image
          → build-images → load-images → deploy → wait-pods
          → setup-keycloak → patch-config
```

The placeholder-swap capability is new, so the demo builds the AuthBridge
sidecar from the repo root (`build-sidecar`), loads it into kind, and points
the rossoctl platform config's `images.authbridge` at the local tag
(`override-sidecar-image`) before deploying the agent.

## Use it from the UI

1. Open <http://rossoctl-ui.localtest.me:8080>.
2. Log in as **alice / alice123**. If you were already signed in, **log out
   and back in** — the demo adds a new audience scope to the UI client and
   tokens only pick it up on a fresh login.
3. Pick **echo-agent** from the Agents list.
4. Chat: `echo`.
5. Confirm the reply's `inbound:` line is an `abph_…` placeholder and the
   `outbound:` line is a JWT.

## Ground-truth check

```bash
kubectl -n team1 logs deploy/echo-upstream
# or:
make show-result
```

`echo-upstream` logs the `Authorization` header it received. Decode the JWT
(e.g. paste into jwt.io) and confirm `aud` is `echo-upstream` — proof the
exchange happened on the outbound leg, not the inbound one.

## Testing in envoy-sidecar mode

The demo defaults to **proxy-sidecar** mode. The placeholder feature is
mode-agnostic — it also works under **envoy-sidecar** mode (Envoy data plane +
ext_proc + proxy-init iptables). To exercise it:

1. Build + load the ext_proc image and point the platform at it (mirrors
   `build-sidecar`/`override-sidecar-image`, but for `images.envoyProxy`):
   ```bash
   podman build -f ../../cmd/authbridge-envoy/Dockerfile -t authbridge-envoy:placeholder-dev ../..
   kind load docker-image authbridge-envoy:placeholder-dev --name rossoctl
   podman exec rossoctl-control-plane ctr -n k8s.io images tag \
     localhost/authbridge-envoy:placeholder-dev docker.io/library/authbridge-envoy:placeholder-dev
   # set images.envoyProxy in rossoctl-system/rossoctl-platform-config to
   # authbridge-envoy:placeholder-dev, then restart the operator:
   kubectl -n rossoctl-system rollout restart deploy/rossoctl-controller-manager
   ```
2. Put echo-agent in envoy mode and restart so the operator re-injects Envoy +
   ext_proc + proxy-init:
   ```bash
   kubectl -n team1 patch agentruntime echo-agent --type=merge \
     -p '{"spec":{"authBridgeMode":"envoy-sidecar"}}'
   kubectl -n team1 rollout restart deploy/echo-agent
   ```
3. Re-run `make patch-config`, then chat from the UI as above. Both the inbound
   `abph_…` placeholder and the outbound exchanged JWT behave identically to
   proxy-sidecar mode.

**Why the upstream listens on `:8888`, not `:8080`.** In envoy mode the
operator's proxy-init **excludes the agent's own Service port (8080) from
outbound iptables interception**. An upstream on 8080 would bypass
Envoy/ext_proc and the placeholder would reach it unresolved. echo-upstream
therefore uses `:8888`, which is intercepted. (Proxy-sidecar mode routes egress
via `HTTP_PROXY` and works on any port — so this choice is harmless there.)

## Known live-cluster gotchas

1. **Operator pull policy.** `override-sidecar-image` only helps if the
   injected sidecar uses `imagePullPolicy: IfNotPresent` (so kind's locally
   loaded image is used instead of pulling `:latest` from the registry). If
   the agent pod shows `ErrImagePull` / `ImagePullBackOff` for the sidecar,
   the platform is configured to always pull — load the tag and confirm the
   pull policy, or push the sidecar image somewhere reachable.
2. **Re-run `setup-keycloak` after the agent registers.** The agent's own
   Keycloak client is created dynamically when the agent pod starts. If
   `setup-keycloak` ran before the client existed, the `echo-upstream-aud`
   optional scope won't be attached to the agent client and the outbound
   exchange fails. Re-run `make setup-keycloak` once the agent is up.
3. **The route must match.** The outbound exchange only fires for the host in
   the `echo-patch.yaml` route (`echo-upstream`). The agent's `UPSTREAM_URL`
   must resolve to that host. If the upstream sees the placeholder instead of
   an exchanged token, the host didn't match the route — check
   `k8s/echo-patch.yaml` against the agent's `UPSTREAM_URL`.

## Clean up

```bash
make undeploy
```

This removes the agent, upstream, and AgentRuntime from `team1`. The
operator-created `authbridge-config-echo-agent` ConfigMap may linger; remove it
manually if needed:

```bash
kubectl -n team1 delete configmap authbridge-config-echo-agent
```

## Design

See the design spec:
[`docs/superpowers/specs/2026-06-02-credential-placeholder-swap-design.md`](../../docs/superpowers/specs/2026-06-02-credential-placeholder-swap-design.md).
