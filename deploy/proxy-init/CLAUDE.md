# deploy/proxy-init — agent notes

Loaded by Claude Code when you read a file in this directory. The repo-wide rules are in the root [`CLAUDE.md`](../../CLAUDE.md). Port numbers are in its Important Port Mapping table.

## init-iptables.sh

Extensively documented shell script that sets up iptables for transparent traffic interception. Key features:

- **Outbound**: `PROXY_OUTPUT` chain in `nat OUTPUT`, redirects to Envoy port 15123
- **Inbound**: `PROXY_INBOUND` chain in `nat PREROUTING`, redirects to Envoy port 15124
- **Istio ambient mesh coexistence**: Handles ztunnel fwmark (0x539), HBONE port (15008), DNAT to POD_IP for inbound interception
- **Exclusions**: SSH (22), loopback, configurable `OUTBOUND_PORTS_EXCLUDE` and `INBOUND_PORTS_EXCLUDE`
- **Envoy UID 1337**: Excluded from outbound redirect to prevent loops
- **Mangle rule**: Sets fwmark on Envoy's local delivery to prevent ISTIO_OUTPUT redirect loop
- Uses `-I 1` (insert first) for chain ordering stability with Istio CNI

**Environment variables:**
| Variable | Default | Description |
|----------|---------|-------------|
| `PROXY_PORT` | 15123 | Envoy outbound listener |
| `INBOUND_PROXY_PORT` | 15124 | Envoy inbound listener |
| `PROXY_UID` | 1337 | Envoy process UID (excluded from redirect) |
| `OUTBOUND_PORTS_EXCLUDE` | (empty) | Comma-separated ports to exclude |
| `INBOUND_PORTS_EXCLUDE` | (empty) | Comma-separated ports to exclude |
| `POD_IP` | (required) | Pod IP via Downward API; used as DNAT target for ambient mesh inbound interception |

## Adding New iptables Rules
- Edit `deploy/proxy-init/init-iptables.sh`
- Follow the existing pattern: document the rule's purpose, Istio interaction, and chain ordering
- Test with and without Istio ambient mesh if possible
- Rebuild from `deploy/proxy-init/`: `make docker-build-init && make load-image`
  (the target is `load-image`, singular — `load-images` does not exist)

- **Keycloak port exclusion**: When using iptables interception, Keycloak's port (8080) must be excluded from redirect via `OUTBOUND_PORTS_EXCLUDE=8080`. Otherwise, token exchange requests from authbridge get redirected back to Envoy, creating a loop.

- **iptables backend auto-detection**: `init-iptables.sh` auto-detects `iptables-legacy` vs `iptables-nft`. Override with `IPTABLES_CMD` env var if needed. Always verify with proxy-init logs after deployment.
