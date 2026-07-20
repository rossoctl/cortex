# Rossoctl Extensions

Kubernetes security extensions for the [Rossoctl](https://github.com/rossoctl/rossoctl) ecosystem, providing **zero-trust authentication** for workloads through transparent token exchange and dynamic Keycloak client registration using SPIFFE/SPIRE identities.

## AuthBridge

[AuthBridge](./authbridge/) provides end-to-end authentication for Kubernetes workloads with [SPIFFE/SPIRE](https://spiffe.io) integration. It consists of:

- **[Authlib](./authbridge/authlib/)** — shared Go library: JWT validation, RFC 8693 token exchange, plugin pipeline, listener implementations.
- **Mode-specific binaries** under [`authbridge/cmd/`](./authbridge/cmd/):
  - [`authbridge-proxy`](./authbridge/cmd/authbridge-proxy/) — proxy-sidecar (default): HTTP forward + reverse proxies, full plugin set.
  - [`authbridge-envoy`](./authbridge/cmd/authbridge-envoy/) — envoy-sidecar: ext_proc gRPC server hooked into Envoy, full plugin set.
  - `authbridge-lite` — **not a separate binary**: the `authbridge-lite` image is `authbridge-proxy` built with `exclude_plugin_*` tags (auth-only: jwt-validation + token-exchange), for size-constrained deployments. See [`authbridge-proxy`](./authbridge/cmd/authbridge-proxy/).
- **[proxy-init](./authbridge/proxy-init/)** — iptables init container used by envoy-sidecar mode for transparent traffic interception.
- **[Keycloak Sync](./authbridge/keycloak_sync.py)** — Declarative tool for synchronizing Keycloak configuration.

Keycloak client registration runs in the [operator](https://github.com/rossoctl/operator) (separate repo, post-#411 / operator#361 — no in-pod registration sidecar).

See the [AuthBridge README](./authbridge/README.md) for architecture details and the [demos index](./authbridge/demos/README.md) for getting started.

## Container Images

All images are published to `ghcr.io/rossoctl/cortex/`. After
cortex#411 the unified binary was split into three
mode-specific combined images, and the per-component sidecars
(`client-registration`, standalone `spiffe-helper`) were retired:

| Image | Description |
|-------|-------------|
| `authbridge` | proxy-sidecar combined (default): authbridge-proxy + bundled spiffe-helper, full plugin set |
| `authbridge-envoy` | envoy-sidecar combined: Envoy + ext_proc + bundled spiffe-helper, full plugin set |
| `authbridge-lite` | `authbridge-proxy` built with `exclude_plugin_*` tags — auth-only (jwt-validation + token-exchange, OPA + parsers dropped), for size-constrained deployments. A build variant, not a separate binary |
| `proxy-init` | Alpine + iptables init container (envoy-sidecar mode only) |

`spiffe-helper` is bundled inside each combined image and gated per
workload by the `SPIRE_ENABLED` env var. Client registration is
handled by the operator's `ClientRegistrationReconciler` and
no longer ships as a separate image. The legacy `authbridge-unified`,
`authbridge-light`, `client-registration`, and standalone `spiffe-helper`
images are no longer published; older release tags continue to
publish the previous shape.

## Development

```bash
# Install pre-commit hooks
make pre-commit

# Run formatters
make fmt

# Build the proxy-init iptables init container image
make build-proxy-init

# Run local testing (requires Kind cluster)
./local-build-and-test.sh
```

See [LOCAL_TESTING_GUIDE.md](./LOCAL_TESTING_GUIDE.md) for the full local development setup.

## Related Repositories

- [rossoctl](https://github.com/rossoctl/rossoctl) — Core Rossoctl platform
- [operator](https://github.com/rossoctl/operator) — Kubernetes operator for sidecar injection (includes the admission webhook)

## License

[Apache 2.0](./LICENSE)
