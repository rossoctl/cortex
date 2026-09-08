# Cortex

**See what your coding agent actually sends — and pay less for it.**

Cortex sits in your agent's request path, decrypts its traffic, and shows you the model
calls, tool calls and agent-to-agent messages as they happen. It can also strip the
tool definitions your agent never calls, which is 4–20% of the prompt on every turn.

One binary, no Kubernetes. macOS or Linux, amd64 or arm64.

## Quick start

```sh
curl -fsSL https://raw.githubusercontent.com/rossoctl/cortex/main/authbridge/install.sh \
  | sh -s -- --claude-code
```

It asks before changing your Claude Code settings, then runs Cortex as a background
service that survives crashes and logins.

Then open two terminals:

```sh
abctl observe   # the viewer
claude          # as usual — no environment variables to set
```

Your agent's calls stream into `abctl`. Cortex only reads them; nothing is rewritten.

- **[Cut token cost](./authbridge/docs/laptop-token-savings.md)** — one more command
- **[Start, stop, remove](./authbridge/docs/laptop-service.md)** — `abctl service status | start | stop`
- **[Run it in Kubernetes](./authbridge/docs/kubernetes.md)** — sidecars, Keycloak, SPIFFE/SPIRE

**Any agent works**, not only Claude Code: point it at `localhost:47600` and trust
`~/.cortex/ca/ca.crt`.

The install URL is on `main`, but the script re-runs the copy from the newest
**release**, so `curl | sh` does not execute unreleased code. `--ref` overrides that.

## What else Cortex does

Traffic visibility is the part you can use in a minute. The same binary provides the
platform services agentic workloads need in production, as a sidecar or standalone:

- **Identity & access** — a verifiable identity per workload, and the right credentials
  for each downstream call, so an agent never holds a tool's secret. This layer is
  **AuthBridge**.
- **Guardrails** — block agent actions that stray from the user's intent or aren't
  grounded in the conversation.
- **Egress control** — govern which external services a workload can reach.
- **Cost controls** — trim the context a workload sends, and cap its spend.

Everything is a plugin in one pipeline; the [plugin catalog](./authbridge/docs/plugin-catalog.md)
lists what ships, and the [architecture reference](./authbridge/README.md) explains how
a request flows through it. Code lives under [`authbridge/`](./authbridge/).

## License

[Apache 2.0](./LICENSE)
