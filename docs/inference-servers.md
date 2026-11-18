# Route an agent to another inference server

Cortex can send an agent's inference to a server that you choose. You do not
change the agent's own settings. You need only one server. It takes three
steps:

1. **Set up the server.** Follow the section for your server in
   [Pick a server](#pick-a-server). It ends with one `agentop server add`
   command.
2. **Choose it for an agent.** See
   [Choose the server for an agent](#choose-the-server-for-an-agent).
3. **Check that it works.** See [Check it works](#check-it-works).

Before you start, install Cortex and route the agent through it. See
[getting started](getting-started.md). Nothing restarts when you add or choose
a server.

## Pick a server

| Server | Setup |
|---|---|
| `ete-int`, `ete-ext` or `cb`: IBM's LiteLLM gateways | [IBM's LiteLLM gateways](#ibms-litellm-gateways) |
| `bob`: IBM Bob's gateway | [IBM Bob](#ibm-bob) |
| A model on your own machine, with Ollama | [Set up a local model](#set-up-a-local-model) |
| Any other gateway or model server | [Another server](#another-server) |

Follow only the section for your server. To add more than one server, follow
each section. The names, such as `ete-int`, are only names. You can use other
names.

### IBM's LiteLLM gateways

All three need the IBM network or VPN.

| Server | URL | Where to make the key |
|---|---|---|
| `ete-int` | `https://ete-litellm.ai-models.vpc-int.res.ibm.com` | <https://ete-litellm.ai-models.vpc-int.res.ibm.com/ui/api-keys/> |
| `ete-ext` | `https://ete-litellm.ai-models.vpc.res.ibm.com` | <https://ete-litellm.ai-models.vpc.res.ibm.com/ui/api-keys/> |
| `cb` | `https://litellm.cb.ete.res.ibm.com` | <https://litellm.cb.ete.res.ibm.com/ui/api-keys/> |

Make a key, then run the line for your server:

```sh
agentop server add ete-int https://ete-litellm.ai-models.vpc-int.res.ibm.com \
  --main claude-opus-5-5 --helper claude-sonnet-5-5

agentop server add ete-ext https://ete-litellm.ai-models.vpc.res.ibm.com \
  --main claude-opus-4-6 --helper claude-sonnet-4-6

agentop server add cb https://litellm.cb.ete.res.ibm.com \
  --main glm --helper NVIDIA-Nemotron-3-Ultra-550B-A55B-NVFP4
```

**Cost:** in dollars. The `cb` models have no rate in Cortex's price list. A
`cb` request whose cost LiteLLM does not send shows as unpriced until you give
the model a rate. See [pricing](pricing.md).

### IBM Bob

Make a static API key at <https://bob.ibm.com/admin/apikeys>, then run:

```sh
agentop server add bob https://api.us-east.bob.ibm.com/inference \
  --main premium-ide --helper fast \
  --auth-header x-api-key --header User-Agent=bob-shell
```

Bob needs the last two flags: it takes its key only as `x-api-key`, and it
refuses a request whose User-Agent does not start with `bob-shell`.

Bob serves chat completions only, so Cortex translates each Anthropic request,
such as one from Claude Code, and its reply. A request with a PDF document gets
a 400 from Cortex. See
[Servers that serve another protocol](inference-router-plugin.md#servers-that-serve-another-protocol).

**Cost:** in Bobcoins, at a flat 2 for each million tokens. agentop shows
Bobcoins apart from dollars.

### Set up a local model

1. Install [Ollama](https://ollama.com/download), and pull a model that can
   call tools. With 64 GB of memory or more, use `granite4.2:8b`. With less,
   use `granite4.2:3b`. Do not use `granite3.3`: it mixes its reasoning into
   the answer.

   ```sh
   ollama pull granite4.2:3b
   ```

2. Set the [context length](#context-length).
3. Add the server. Ollama checks no key, but `add` needs one, so `echo ollama`
   gives a placeholder. Give the full model tag.

   ```sh
   echo ollama | agentop server add ollama http://localhost:11434 \
     --main granite4.2:3b --helper granite4.2:3b --key-stdin
   ```

A local model is weaker than a frontier model. Expect more retries on hard
work.

**Cost:** Cortex counts the tokens, but shows the model as unpriced until you
give it a rate. See [pricing](pricing.md).

#### Choose a model

The window must hold the agent's largest prompt: 32k for Bob, 64k or more for
Claude Code. A bigger window needs more memory. If the model does not fit in
GPU memory, it is slow. Then use the smaller model.

#### Context length

Ollama does not refuse a prompt that is too long. It cuts the prompt without
an error, and the agent then behaves strangely. So set the window yourself:

| Where Ollama runs | How to set it |
|---|---|
| macOS app | Settings → Context length |
| Linux service | `sudo systemctl edit ollama.service`, add `Environment="OLLAMA_CONTEXT_LENGTH=32768"` under `[Service]`, then `sudo systemctl restart ollama` |
| From a shell | `OLLAMA_CONTEXT_LENGTH=32768 ollama serve` |

After one prompt, `ollama ps` shows the window that you got in its `CONTEXT`
column.

### Another server

Any other LiteLLM gateway, provider API or local model server, such as LM
Studio, llama.cpp's `llama-server` or vLLM, works the same way:

```sh
agentop server add <name> <url> --main <word> --helper <word>
```

See [Local model servers](../cmd/agentop/README.md#local-model-servers), and
`--auth-header` and `--header` under
[`add`](../cmd/agentop/README.md#choosing-an-inference-server-agentop-server)
for a gateway that needs its own headers.

## What `add` does

- It asks for the key at a prompt that does not echo.
- `--main` and `--helper` name the model for the agent's main work, and the
  model for side requests such as titles. Cortex uses them only when the server
  refuses the model that the agent asked for. Each is a word: `add` picks the
  newest model on the server's list whose name contains it.
- It reads the server's model list and prints the model that each word picks.
  It refuses a word that names no model, and shows the list.
- It writes `~/.cortex/config.yaml`, and the proxy reloads it.

To change a key or a model, run the same `add` again.

### When `add` fails

| Message | Cause | What to do |
|---|---|---|
| `could not reach the server: context deadline exceeded` | For `ete-int`, `ete-ext` or `cb`: you are not on the IBM network. | Connect to the IBM network or VPN. |
| `could not reach the server: … connection refused` | For Ollama: the server is not running. | Start Ollama. |
| `the server answered 401, …` | The key is wrong. | Make a new key on the server's key page. |
| `the server answered 403, 403, 403, 403` for Bob | The two Bob flags are missing. | Add `--auth-header x-api-key --header User-Agent=bob-shell`. |
| `--main … names none of …'s models` | The server does not serve a model with that word. | Choose a word from the list that the message shows. |

## Choose the server for an agent

```sh
agentop server use ete-ext --agent claude-code   # new Claude Code sessions → ete-ext
agentop server move <session-id> bob             # one running session → bob
agentop server reset --agent claude-code         # back to Claude Code's own provider
```

- `--agent` is the agent's name as agentop shows it, such as `claude-code` or
  `bob`. In `agentop observe`, press `A` for the agents, then `S` on one to do
  the same.
- New sessions go to the server. Running sessions stay where they started
  until you move them.
- The first turn after a move sends the whole conversation again with no
  cache, so it is slow. Start a new session when you can.
- If the server stops, its sessions' requests fail. Nothing fails over on its
  own.

For every form of the command, see
[Choosing an inference server](../cmd/agentop/README.md#choosing-an-inference-server-agentop-server).

## Check it works

Run `agentop server`. It lists each server, the model that each word picks,
and the agents that each server gets. It never shows a key.

```
  bob       api.us-east.bob.ibm.com                     main premium-ide → premium-ide · helper fast → fast
  ete-ext   ete-litellm.ai-models.vpc.res.ibm.com       main claude-opus-4-6 → claude-opus-4-6 · helper claude-sonnet-4-6 → claude-sonnet-4-6   claude-code
  ete-int   ete-litellm.ai-models.vpc-int.res.ibm.com   main claude-opus-5-5 · helper claude-sonnet-5-5 (model list unavailable: could not reach the server: context deadline exceeded)
```

`model list unavailable` means that agentop cannot reach the server. Then start
a new session in the agent. With more than one server, the sessions table in
`agentop observe` has a `SERVER` column that names each session's server.
