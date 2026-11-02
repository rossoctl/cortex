# Cortex

**`top` for your coding agent.** Watch every model call, tool call and dollar your
agent spends, live — then cut the waste.

Claude Code · OpenCode · Codex · IBM Bob · Pi · any agent that can use an HTTPS proxy.
One binary on macOS or Linux. No Kubernetes, no account.

<img src="./docs/assets/cortex-demo.svg" width="100%"
     alt="A terminal installs Cortex with one command, and a second command points Claude Code at it. Three Claude Code sessions run in separate directories, and agentop then lists all three with their token counts, cost and remaining context. Pressing $ breaks the spend down by tier, where cache reads dominate. Drilling into the busiest session shows the whole conversation and the fifteen-tool manifest it re-sends on every turn.">

## Quick start

<!-- This install command is duplicated in the website's laptop quickstart:
     rossoctl/rossoctl → docs/get-started/laptop.md ("Step 1: install the program").
     Change both, or they drift — the --ref wording already did once. -->

```sh
curl -fsSL https://www.rossoctl.dev/cortex/install.sh | sh
```

Then, in a new terminal:

```sh
agentop configure claude-code enable    # once — or opencode, codex, bob, pi
claude                                  # start (or restart) your agent as you always do
agentop observe                         # in another terminal: watch it live
```

Setup asks once before changing anything. If something looks wrong, `agentop doctor`
names the fix. Can't reach rossoctl.dev? The same script is
[in this repo](https://raw.githubusercontent.com/rossoctl/cortex/main/scripts/install.sh).

## What you can do with it

**See where your tokens go.** Every session's tokens, cost and context fill, live.
Open one to read the whole conversation, and the tool list it re-sends every turn.

```sh
agentop observe
agentop cost --window month --by agent
```

**Trim 4–20% off Claude Code's prompts** (median 6% in our measurement). Cortex
strips the tool definitions you never call.

```sh
agentop tools scan --write ~/.cortex/config.yaml
```

**Switch model servers without touching your agent.** Provider down, or want the
traffic to stay local? Send new sessions to a LiteLLM gateway or Ollama. For Ollama,
pull the model and [raise its context length](./docs/laptop-local-model.md#context-length) first.

```sh
echo ollama | agentop server add ollama http://localhost:11434 \
  --main granite4.2:8b --helper granite4.2:8b --key-stdin   # Ollama takes any key
agentop server use ollama --agent claude-code
```

Guides: [token savings](./docs/laptop-token-savings.md) ·
[local models](./docs/laptop-local-model.md) · [pricing](./docs/pricing.md)

## How it works

<!-- Two copies of one drawing, so it follows the reader's GitHub theme. They differ
     only in their <style> block: change the shapes or the text in both. -->
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="./docs/assets/cortex-architecture-dark.svg">
  <img src="./docs/assets/cortex-architecture.svg" width="100%"
       alt="Your coding agent sends its traffic through Cortex, a local process. Cortex's TLS bridge decrypts it with your own CA, the plugin pipeline parses the model, tool and agent calls, and the request goes on, re-encrypted, to the model or tool server it was meant for. The session store records each session, its tokens and its cost to ~/.cortex, and agentop observe reads it over the session API.">
</picture>

Your agent uses Cortex as its HTTPS proxy. Cortex decrypts the traffic with a CA made
on your machine, runs it through a pipeline of plugins, and sends it on re-encrypted.
[Running Cortex](./docs/laptop-service.md) covers each piece.

## Supported agents

| Agent | Connect it | |
|---|---|---|
| **Claude Code** | `agentop configure claude-code enable` | [Guide](./docs/agents/claude-code.md) |
| **OpenCode** | `agentop configure opencode enable` | [Guide](./docs/agents/opencode.md) |
| **Codex** | `agentop configure codex enable` | [Guide](./docs/agents/codex.md) |
| **IBM Bob** | `agentop configure bob enable` — the editor and the `bob` shell command both | [Guide](./docs/agents/bob.md) |
| **Pi** | `agentop configure pi enable` | [Guide](./docs/agents/pi.md) |
| **Any other agent** | `agentop exec -- <command>` | [Guide](./docs/agents/any-agent.md) |

`status` and `disable` check and undo each one.

## Your data stays on your machine

- **Loopback only.** Cortex contacts the servers your agent asks for, or the one you
  route it to. Its own calls: LiteLLM's public [price list](./docs/pricing.md), hourly
  (`pricing.bundled: false` turns it off), and the model list of any server you add.
- **Your own CA,** created locally in `~/.cortex/ca`. Never added to a keychain by Cortex.
- **Bad certificates aren't intercepted.** When a server's certificate fails, Cortex
  leaves that connection encrypted end to end.
- **Read-only** until you turn on pruning, server routing or an enforcing script.
- **History on disk** in [`~/.cortex/sessions`](./docs/laptop-service.md#session-history-is-kept-in-cortexsessions)
  and `~/.cortex/cost`. `agentop uninstall` removes Cortex;
  `--purge` also deletes `~/.cortex`.

## Beyond the laptop

The same binary runs as a Kubernetes sidecar: a verifiable identity per workload, the
right credentials for each downstream call (**AuthBridge**), guardrails, egress control
and spend caps. Start with [Running Cortex in Kubernetes](./docs/kubernetes.md).

The same binary also runs on a remote machine — a VM, a home server, a pod — with
`agentop` connecting over an encrypted tunnel. See
[Running Cortex on a remote machine](./docs/remote.md).

## Learn more

[All docs](./docs/README.md) · [agentop reference](./cmd/agentop/README.md) ·
[plugin catalog](./docs/plugin-catalog.md) · [script plugins](./docs/script-plugin.md) ·
[cpex policy build](./docs/laptop-service.md#the-cpex-build) ·
[full install guide](https://www.rossoctl.dev/docs/dev/get-started/laptop) ·
[install stats](https://rossoctl.github.io/cortex/install-stats/) (from GitHub's download counts, no telemetry)

Something broke? Tell us with the **Laptop feedback** form under
[new issue](https://github.com/rossoctl/cortex/issues/new/choose), or on
[Slack](https://ibm.biz/rossoctl-slack). A half-finished install with the error pasted
in helps more than a polished report never sent. To contribute, see
[CONTRIBUTING.md](./CONTRIBUTING.md); to report a vulnerability, [SECURITY.md](./SECURITY.md).

Licensed under [Apache 2.0](./LICENSE).
