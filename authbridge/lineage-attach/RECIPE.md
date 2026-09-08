# Recipe — attach lineage to a running Deployment

A step-by-step for an operator or a coding agent. Every step is one command,
the one line of output that means it worked, and what to do when it did not.
Run from this directory. The explanations are in [README.md](README.md); the
reasons behind them in [DESIGN.md](DESIGN.md).

**Inputs.** `NS` — the namespace · `DEPLOY` — the Deployment · `CONTAINER` —
the name of its app container (`kubectl -n $NS get deploy/$DEPLOY -o jsonpath='{.spec.template.spec.containers[*].name}'`)
· `IMAGE` — that container's image, present locally under the name the engine knows it by (a podman build of
`my-agent:latest` is `localhost/my-agent:latest`; a bare name is asked of the engine first, then taken as `docker.io/library/<name>`).

## 0. Preconditions (read-only)

| check | command | pass |
|---|---|---|
| the Deployment exists and is not platform-enrolled | `kubectl -n $NS get deploy $DEPLOY -o jsonpath='{.spec.template.spec.initContainers[*].name} {.spec.template.spec.containers[*].name}'` | no `proxy-init`, no `envoy-proxy` (the target's own app/init containers are fine — the sidecar attaches as a native initContainer ahead of them) |
| no volume-name collision (volumes merge by name too) | `kubectl -n $NS get deploy $DEPLOY -o jsonpath='{.spec.template.spec.volumes[*].name}'` | no `envoy-config`, no `authbridge-runtime` |
| the platform rendered the sidecar's config here | `kubectl -n $NS get cm envoy-config` | found |
| the app's image is local (for the bake) | `podman image exists $IMAGE` (docker: `docker image inspect $IMAGE >/dev/null`) | exit 0 |
| an OTLP/gRPC collector is reachable in-cluster | `kubectl -n rossoctl-system get deploy otel-collector` | found (else set `OTEL_ENDPOINT` in step 3) |

Enrolled workloads (an `AgentRuntime` CR) already carry a sidecar: this recipe
refuses them by design; see README "Enrolled workloads".

## 1. A sidecar image that carries the plugin (once per cluster, until a release does)

The plugin is cortex #761: build from a tree that carries `authbridge/authlib/plugins/lineage/`
(`main` once #761 has merged; the #761 branch until then).

```sh
( cd .. && podman build -f cmd/authbridge-envoy/Dockerfile -t docker.io/library/authbridge-envoy:latest . \
            && podman build -f proxy-init/Dockerfile.init -t docker.io/library/proxy-init:latest proxy-init/ )
for ref in authbridge-envoy proxy-init; do podman save docker.io/library/$ref:latest -o /tmp/$ref.tar \
  && KIND_EXPERIMENTAL_PROVIDER=podman kind load image-archive /tmp/$ref.tar --name rossoctl; rm -f /tmp/$ref.tar; done
export SIDECAR_IMAGE=docker.io/library/authbridge-envoy:latest PROXY_INIT_IMAGE=docker.io/library/proxy-init:latest
```

Pass: `podman exec <kind-node> crictl images | grep -E 'library/(authbridge-envoy|proxy-init)'` lists both.
Docker hosts: `docker build` with the same `-f`/`-t`, then `kind load docker-image <ref> --name rossoctl`.
Skip this step once the published `ghcr.io/rossoctl/cortex/authbridge-envoy` carries `lineage-telemetry`;
the symptom of skipping it too early is the sidecar crash-looping with `unknown plugin "lineage-telemetry"`.

## 2. Bake the propagation shim onto the app image (once per image)

```sh
KIND_CLUSTER_NAME=rossoctl ./build-otel-shim.sh $IMAGE
# positional: ./build-otel-shim.sh <base> [wrapper-tag] [venv-python] [app-uid[:gid]]
# NO_KIND_LOAD=1 builds and attests only (skips the cluster load)
```

Pass (exit 0): `>> loaded docker.io/library/<name>-otel:latest into kind cluster rossoctl`
(a `>> NOTE:` block follows it, so this is not the literal last line; the
attestation runs before the load and prints nothing when it passes).
Fail `REFUSING to bake … already instruments …` (exit 3): the app instruments itself — go to step 3 **without** `APP_CONTAINER`/`APP_IMAGE` (capture only).
Fail `REFUSING to bake … no runnable Python found` (exit 3): outside the shim's envelope (DESIGN "The envelope") — same, capture only; or pass the interpreter as arg 3 if you know it.
Fail `REFUSING to bake … is not present locally` (exit 3): wrong `IMAGE` — see Inputs; nothing was built.
Fail `ATTESTATION FAILED` (exit 4): the bake itself is broken (the image was not loaded) — read the assertion it prints; not an app property.

## 3. Attach (once per Deployment)

```sh
NAMESPACE=$NS DEPLOY=$DEPLOY APP_CONTAINER=$CONTAINER APP_IMAGE=docker.io/library/<name>-otel:latest ./sidecar-patch.sh
```

Add `OUTBOUND_PORTS_EXCLUDE=5432,1025` (comma-separated) if the app speaks a plaintext **non-HTTP**
protocol to a store — Postgres, SMTP, Redis. Never exclude LLM, tool, peer or S3 ports.
Set `OTEL_ENDPOINT=host:port` for a collector other than the platform's.

Pass — the output ends with:
```
>> back out: kubectl -n <ns> patch deploy/<deploy> --type strategic -p '<the reverse patch>' && kubectl -n <ns> delete cm authbridge-lineage-config-<deploy>
>>   (the patch restores image <pre-attach ref> — drop its "image" field if the app is re-imaged after this attach)
deployment "<deploy>" successfully rolled out
>> lineage sidecar attached to deploy/<deploy> (self_id=<deploy>, ns=<ns>)
```
(preceded by `configmap/authbridge-lineage-config-<deploy> created` and `deployment.apps/<deploy> patched`;
the back-out line is printed before the rollout wait so it is there even when the wait fails).
Add `CAPTURE_IO=true` to attach the parsed content — prompts, tool arguments, messages — to the spans (off by default; PII).
Fail `already has a container named` / `already declares containerPort` / `already has a volume named` → enrolled or colliding workload (README "How to attach").
Fail `has no container named` → wrong `APP_CONTAINER`; nothing was applied.
Rollout stuck → the Deployment is left patched on purpose (Kubernetes keeps the old pod serving);
run the back-out line printed above, then read the sidecar log (step 4).

## 4. Verify

```sh
kubectl -n $NS logs deploy/$DEPLOY -c envoy-proxy | grep 'lineage-telemetry: initialized'
```
Pass: `… endpoint=<otel_endpoint> self_id=<deploy>`.

Then one request **from inside the cluster** (a port-forward bypasses the sidecar) with a trace id you choose:
```sh
T=$(python3 -c 'import secrets;print(secrets.token_hex(16))')
kubectl -n $NS run drive --rm -i --restart=Never --image=curlimages/curl:8.11.1 -- \
  curl -s -H "traceparent: 00-$T-0000000000000001-01" http://<service>:<port>/<path>    # any request the app answers
kubectl -n rossoctl-system logs deploy/otel-collector | grep -c "$T"
```
Pass: a count ≥ 2 (one request span + one response span per exchange the app took part in).
Attribution check, when the app calls out: every hop after the entry must show
`lineage.parent.source: tracestate`; a `none` (or `wire`) on a non-entry hop is an un-propagated call
(DESIGN "Why the shim is needed").

## 5. Back out

Run the line step 3 printed: a strategic-merge **reverse patch** that deletes, by name, exactly
what the attach added — and restores the app image it replaced — then deletes the ConfigMap:

```sh
kubectl -n $NS patch deploy/$DEPLOY --type strategic -p '<the printed reverse patch>' \
  && kubectl -n $NS delete cm authbridge-lineage-config-$DEPLOY
```

It is right at any later time: whatever the owner rolled since the attach stays in place. (A
`rollout undo` is not a back-out — it restores a whole earlier pod template, taking the owner's
later changes with it.) A few caveats. The patch restores the pre-attach image ref — drop its
`"image"` field if the app was re-imaged after the attach. The `$patch: delete` on
`LINEAGE_PROPAGATE` *removes* that env var — if the app owner set it themselves before the attach
(unusual — it is the shim's activation), edit the line to restore their value instead. And unlike
the attach, the printed line is raw `kubectl patch` with no precondition check, so if the app
container was **renamed** after the attach, the `containers` entry would add a stub by the old
name — regenerate with the current name. If the printed line is gone,
regenerate the patch with the attach's own knobs:
`EMIT=undo NAME=$DEPLOY NAMESPACE=$NS APP_CONTAINER=$CONTAINER RESTORE_IMAGE=<pre-attach ref> ./attach-lineage.sh`
(leave `APP_CONTAINER`/`RESTORE_IMAGE` off for a capture-only attach; `APP_IMAGE` is the ref to
INSTALL and `EMIT=undo` refuses it — reusing the attach line verbatim would "restore" the -otel image). Delete the ConfigMap after the
patch, not before: pods of a revision that still mounts it cannot start without it.

## A fleet

Bake once per image, attach once per Deployment; two loops. Then check the **shape** of one trace
(one root, unstamped only at the entry), not just that spans arrived.

```sh
for img in agent-a agent-b tool-x; do KIND_CLUSTER_NAME=rossoctl ./build-otel-shim.sh docker.io/library/$img:latest; done
for d in agent-a agent-b tool-x; do
  NAMESPACE=$NS DEPLOY=$d APP_CONTAINER=app APP_IMAGE=docker.io/library/$d-otel:latest ./sidecar-patch.sh
done
```
