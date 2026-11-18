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

### Set up a local model

This section uses [Ollama](https://ollama.com) and IBM's Granite models. A
local model is weaker than a frontier model. Expect more retries and wrong
turns on hard work. It keeps you working. It does not replace your provider.

1. [Choose a model](#choose-a-model) for your machine's memory and your agent.
2. [Install Ollama and pull the model](#install-ollama-and-pull-the-model).
3. [Set the context length](#context-length).
4. Add the server. Ollama checks no key, but `add` needs one, so
   `echo ollama` gives a placeholder. Give the full tag: a bare `granite` picks
   `granite3.3` if that is pulled too.

   ```sh
   echo ollama | agentop server add ollama http://localhost:11434 \
     --main granite4.2:3b --helper granite4.2:3b --key-stdin
   ```

#### Choose a model

Pick the model by how much memory your Mac has. The model's own size is
small. What grows is the cache that holds the context: the bigger the window,
the more memory it takes. When it does not fit in GPU memory, the Mac swaps
and everything slows down. A 36 GB Mac, for example, stalls on
`granite4.2:8b` at a 128k window. At 64k the model fits, but a smaller chip
makes the first word slow to arrive (see **Speed** below).

| Your Mac's memory | Model |
|---|---|
| 64 GB or more | `granite4.2:8b` |
| Under 64 GB | `granite4.2:3b` |

Pick the window by agent. It must hold the agent's largest prompt. Anything
bigger only costs memory and time.

| Agent | Largest prompt seen | Window |
|---|---|---|
| Bob | about 21k tokens | 32k |
| Claude Code | over 64k tokens | 64k or more |

Ollama has one window for every agent that uses it. If more than one agent
uses it, set the largest of their windows.

**Memory each model uses**, in GiB:

| Model | 32k | 64k | 128k |
|---|---|---|---|
| `granite4.2:8b` | 9.7 | 14.9 | 25.4 |
| `granite4.2:3b` | 4.4 | 7.2 | 12.1 |
| `granite4:tiny-h` | 4.4 | 4.9 | 5.8 |

**Speed** on a 20k-token prompt, the size of a Bob prompt:

| Model | Wait for the first word | Writing speed |
|---|---|---|
| `granite4.2:8b` | 48 s | 45 tokens/s |
| `granite4.2:3b` | 23 s | 82 tokens/s |
| `granite4:tiny-h` | 8.5 s | 124 tokens/s |

You wait once, at the start of a conversation. Later turns start in about
0.1 s. These numbers come from an M4 Max with Ollama 0.40.5. A smaller chip
is slower.

**What you give up:**

| Model | Good | Not so good |
|---|---|---|
| `granite4.2:8b` | Best answers | Needs the most memory |
| `granite4.2:3b` | Thinks before answering. Twice as fast as 8b | Weaker answers than 8b |
| `granite4:tiny-h` | Fastest. Memory hardly grows with the window | Older Granite 4.0. Cannot think. Not yet tried with an agent end to end |

Try `granite4:tiny-h` only if `granite4.2:3b` does not fit (see **Check what
loaded** below). Ollama refuses a thinking request to it
(`"granite4:tiny-h" does not support thinking`), and agents can send one.

Do not use `granite3.3`. It mixes its reasoning into the answer, and the
agent reads that as part of its work. Granite 4.2 keeps the two apart.

#### Install Ollama and pull the model

| | |
|---|---|
| macOS | the app from [ollama.com/download](https://ollama.com/download) |
| Linux | `curl -fsSL https://ollama.com/install.sh \| sh` |

The macOS app starts Ollama at login. On Linux the installer starts an
`ollama` service. Without systemd, run `ollama serve`.

Pull the model you chose:

```sh
ollama pull granite4.2:3b
```

Check that Ollama lists it:

```sh
curl -s localhost:11434/v1/models | jq -r '.data[].id'
```

#### Context length

Set the window yourself. Ollama does not refuse a prompt that is too long. It
cuts the prompt without an error. Then the agent behaves strangely, and nothing
says why. A fresh install also picks a small window from your GPU memory,
not the one your agent needs.

Set it where the server starts. The examples use 32k (`32768`). Use the
window you chose above.

| Where Ollama runs | How to set it |
|---|---|
| macOS app | Settings → Context length. The app restarts its server. Use the slider: once set, it overrides `OLLAMA_CONTEXT_LENGTH`. |
| Linux service | `sudo systemctl edit ollama.service`, add the lines below, then `sudo systemctl restart ollama` |
| From a shell | `OLLAMA_CONTEXT_LENGTH=32768 ollama serve` |

```ini
[Service]
Environment="OLLAMA_CONTEXT_LENGTH=32768"
```

**Optional: a smaller cache.** `OLLAMA_KV_CACHE_TYPE=q8_0`, set the same way
(the macOS app has no setting for it), cuts `granite4.2:8b` to 7.4 GiB at
32k and 15.7 GiB at 128k. It does not make anything faster.

**Check what loaded.** Send the model one prompt, then run `ollama ps`:

```
NAME             SIZE      PROCESSOR    CONTEXT
granite4.2:3b    4.7 GB    100% GPU     32768
```

- **CONTEXT** is the window you really got, whatever was configured.
- **PROCESSOR** should say `100% GPU`. Any CPU share means the model did not
  fit and will be slow. Leave room for macOS, the agent and your editor.
  If it says `100% GPU` and is still too slow, the chip is the limit: use a
  smaller model.

#### Trying another local model

Check any local model the same way before you rely on it:

| Check | How |
|---|---|
| Calls tools | `ollama show <model>` lists `tools` under capabilities. A model that cannot call tools cannot drive a coding agent. |
| Keeps thinking apart from the answer | Ask it something small. The thinking should arrive in its own field (`thinking` blocks or `reasoning`), not in the answer. |
| Window fits the agent | See [Choose a model](#choose-a-model). |
| Fits in memory | `ollama ps` shows the real size and `100% GPU`. The download size is only the weights. |

Then give it a small real task through the agent before a large one.

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
- It writes `~/.cortex/config.yaml`, and the proxy reloads it. For
  example, the Bob entry:

  ```yaml
  - name: inference-router
    config:
      servers:
        bob:
          url: https://api.us-east.bob.ibm.com/inference
          key: bob_prod_…
          main: premium-ide
          helper: fast
          api:
            chat-completions: /v1/chat/completions
          auth_header: x-api-key
          headers:
            User-Agent: bob-shell
  ```

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
