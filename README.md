# Cortex

**See what your coding agent actually sends — and pay less for it.**

<img src="./docs/assets/cortex-demo.svg" width="100%"
     alt="A terminal installs Cortex with one command and points Claude Code at it. Three Claude Code sessions run in separate directories, and abctl then lists all three with their token counts, cost and remaining context. Pressing $ breaks the spend down by tier, where cache reads dominate. Drilling into the busiest session shows the whole conversation and the fifteen-tool manifest it re-sends on every turn.">

Cortex sits in your agent's request path, decrypts its traffic, and shows you the model
calls, tool calls and agent-to-agent messages as they happen. It can also strip the
tool definitions your agent never calls, which is 4–20% of the prompt on every turn.

**Think `top`, for your coding agent.** Where `top` shows which processes are eating
your CPU, `abctl observe` shows which agent sessions are eating your tokens, your
context window and your money — live, as they run.

One binary, no Kubernetes. macOS or Linux, amd64 or arm64.

## Quick start

<!-- This install command is duplicated in the website's laptop quickstart:
     rossoctl/rossoctl → docs/get-started/laptop.md ("Step 1: install the program").
     Change both, or they drift — the --ref wording already did once. -->

```sh
curl -fsSL https://raw.githubusercontent.com/rossoctl/cortex/main/install.sh \
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

- **[Cut token cost](./docs/laptop-token-savings.md)** — one more command
- **[Start, stop, remove](./docs/laptop-service.md)** — `abctl service status | start | stop`
- **[Run it in Kubernetes](./docs/kubernetes.md)** — sidecars, Keycloak, SPIFFE/SPIRE

**Any agent works**, not only Claude Code: point it at `localhost:47600` and trust
`~/.cortex/ca/ca.crt`.

`curl | sh` never executes unreleased code — the script re-runs the copy from the newest
release. Pin or override with `--ref`
([CONTRIBUTING.md](./CONTRIBUTING.md#installing-an-unreleased-build)).

**Full install guide:** [Cortex on your laptop](https://www.rossoctl.dev/docs/dev/get-started/laptop)
— prerequisites, step-by-step walkthrough, service management and troubleshooting.

## Feedback

> [!NOTE]
> **Cortex on a laptop is new, and we want to hear when it breaks.**
>
> If the install failed, the numbers looked wrong, or anything was unclear:
>
> - Open the **Laptop feedback** form → [new issue](https://github.com/rossoctl/cortex/issues/new/choose)
> - Or say so in [Slack](https://ibm.biz/rossoctl-slack)
>
> A half-finished install with the error pasted in is more useful to us than a polished
> bug report you never sent.

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

Everything is a plugin in one pipeline; the [plugin catalog](./docs/plugin-catalog.md)
lists what ships, and the [architecture reference](./docs/architecture.md) explains how
a request flows through it. The shared library is [`core/`](./core/); the
binaries live under [`cmd/`](./cmd/).

## License

[Apache 2.0](./LICENSE)
