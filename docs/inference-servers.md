# Route an agent to another inference server

Cortex can send an agent's inference to a server that you choose. You do not
change the agent's own settings. Use it in these cases:

- Your provider is down or out of quota.
- You want to use your team's gateway.
- You want the traffic to stay on your machine.

This guide covers these servers:

| Server | URL | Network | Protocol |
|---|---|---|---|
| `ete-int` | `https://ete-litellm.ai-models.vpc-int.res.ibm.com` | IBM network or VPN only | LiteLLM: Anthropic and chat completions |
| `ete-ext` | `https://ete-litellm.ai-models.vpc.res.ibm.com` | IBM network or VPN only | LiteLLM: Anthropic and chat completions |
| `cb` | `https://litellm.cb.ete.res.ibm.com` | IBM network or VPN only | LiteLLM: Anthropic and chat completions |
| `bob` | `https://api.us-east.bob.ibm.com/inference` | Internet | Chat completions only |
| `ollama` | `http://localhost:11434` | Your machine | Anthropic and chat completions |

The server names in the first column are only names. You can use other names.
Any other LiteLLM gateway, or a local server such as LM Studio, llama.cpp's
`llama-server` or vLLM, works the same way. See
[Local model servers](../cmd/agentop/README.md#local-model-servers) for their
details.

## The steps

The steps are the same for each server:

1. **Prepare the server.** For a gateway, get a key. See
   [Get a key for an IBM server](#get-a-key-for-an-ibm-server). For a local
   model, install it and set its context length. See
   [Set up a local model](#set-up-a-local-model).
2. **Add the server** with `agentop server add`. See
   [Add the server](#add-the-server).
3. **Choose it for an agent** with `agentop server use`. See
   [Choose a server for an agent](#choose-a-server-for-an-agent).
4. **Check that it works.** See [Check it works](#check-it-works).

Before you start:

- Install Cortex and route the agent through it. See
  [getting started](getting-started.md) and the guide for your agent, such as
  [Claude Code](agents/claude-code.md).
- **Nothing restarts.** `agentop server` writes `~/.cortex/config.yaml`, and
  the proxy reloads it.

## Get a key for an IBM server

Make a key on each server's own key page.

| Server | Where to make the key |
|---|---|
| `ete-int` | <https://ete-litellm.ai-models.vpc-int.res.ibm.com/ui/api-keys/> |
| `ete-ext` | <https://ete-litellm.ai-models.vpc.res.ibm.com/ui/api-keys/> |
| `cb` | <https://litellm.cb.ete.res.ibm.com/ui/api-keys/> |
| `bob` | <https://bob.ibm.com/admin/apikeys> |

- **ete-int and ete-ext:** before you can make a key, ETE must give you access. Request
  [Research managed non open source models](https://data-forms.apps.res.ibm.com/user/form?type=ROAI)
  and name the Claude models that you need. ETE sends an email when your team
  has access.
- **ete-int, ete-ext and cb:** connect to the IBM network or VPN, before you
  make the key and while you use the server.
- **Bob:** make a static API key. Do not use Bob's login token. That token is a
  JWT, and it expires after 12 hours.

Keep the key in a password manager. `agentop server add` asks for it at a
prompt that does not echo. The key never goes into shell history.

## Set up a local model

This section uses [Ollama](https://ollama.com) and IBM's Granite models.

- **A local model is weaker than a frontier model.** Expect more retries and
  wrong turns on hard work. It keeps you working. It does not replace your
  provider.

### Choose a model

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

### Install Ollama and pull the model

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

### Context length

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

## Add the server

Run one `add` for each server that you want. Each `add` asks for that
server's key.

```sh
agentop server add ete-int https://ete-litellm.ai-models.vpc-int.res.ibm.com \
  --main claude-opus-5-5 --helper claude-sonnet-5-5

agentop server add ete-ext https://ete-litellm.ai-models.vpc.res.ibm.com \
  --main claude-opus-4-6 --helper claude-sonnet-4-6

agentop server add cb https://litellm.cb.ete.res.ibm.com \
  --main glm --helper NVIDIA-Nemotron-3-Ultra-550B-A55B-NVFP4

agentop server add bob https://api.us-east.bob.ibm.com/inference \
  --main premium-ide --helper fast \
  --auth-header x-api-key --header User-Agent=bob-shell

echo ollama | agentop server add ollama http://localhost:11434 \
  --main granite4.2:3b --helper granite4.2:3b --key-stdin
```

- **`--main` and `--helper`:** the model for the agent's main work, and the
  model for side requests such as titles. Cortex uses them only when the server
  refuses the model that the agent asked for. Each value is a word: `add` picks
  the newest model on the server's list whose name contains the word. A full
  name, as above, usually picks that model. A short word, such as `opus`,
  picks the newest Opus, also one that the server adds later. For Ollama, give
  the full tag: a bare `granite` picks `granite3.3` if that is pulled too.
- **The models above:** on 2026-11-18, the `ete-ext`, `cb` and `bob` words
  picked these models. On `cb`, the names carry a `rits/…/` prefix, and
  `glm` picked `rits/zai-org/glm-5-3`. Nobody checked the `ete-int` list on that day. `add` reads the
  server's list and prints the model that each word picks. It refuses a word
  that names no model, and it shows the list. Use that list to choose again.
- **Bob's two flags:** Bob takes its key only as `x-api-key`. Bob's edge refuses
  each request whose User-Agent does not start with `bob-shell`.
  `agentop server add` does not set these for you. See
  [IBM Bob](../cmd/agentop/README.md#choosing-an-inference-server-agentop-server)
  under `add`.
- **Bob's protocol:** Bob serves chat completions only. `add` sends a small
  test request to find the path, and writes it to the config. Cortex then
  translates each Anthropic request, such as one from Claude Code, to chat
  completions, and it translates the reply back. See
  [Servers that serve another protocol](inference-router-plugin.md#servers-that-serve-another-protocol).
- **Ollama's key:** Ollama checks no key, but `add` needs one, so
  `echo ollama` gives a placeholder.

`add` writes `~/.cortex/config.yaml`, and the proxy reloads it. The `ete-int`
and `bob` entries look like this:

```yaml
- name: inference-router
  config:
    servers:
      ete-int:
        url: https://ete-litellm.ai-models.vpc-int.res.ibm.com
        key: sk-…
        main: claude-opus-5-5
        helper: claude-sonnet-5-5
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

To change a key or a model later, run the same `add` again. `add` asks
before it replaces the entry. Add `--yes` to skip the question. Running
sessions keep the model that they started with, so start a new session to use
the new model.

### When `add` fails

| Message | Cause | What to do |
|---|---|---|
| `could not reach the server: context deadline exceeded` | For `ete-int`, `ete-ext` or `cb`: you are not on the IBM network. | Connect to the IBM network or VPN. Then run `add` again. |
| `could not reach the server: … connection refused` | For Ollama: the server is not running. | Start Ollama. Then run `add` again. |
| `the server answered 401, …` | The key is wrong, or it is a key for another server. | Make a key on the server's own key page. |
| `the server answered 403, 403, 403, 403` for Bob | The two Bob flags are missing. | Run `add` again with `--auth-header x-api-key --header User-Agent=bob-shell`. |
| `--main … names none of …'s models` | The server does not serve a model with that word. | Choose a word from the list that the message shows. For Ollama, pull the model first. |

## Choose a server for an agent

```sh
agentop server use ete-ext --agent claude-code   # new Claude Code sessions → ete-ext
agentop server move <session-id> bob             # one running session → bob
agentop server reset --agent claude-code         # back to Claude Code's own provider
```

`--agent` is the agent's name as agentop shows it, such as `bob` or
`claude-code`. `agentop configure status` lists yours. In the TUI, press `S`
on the agents pane to do what `use` and `reset` do.

| | |
|---|---|
| **New sessions** | Go to the server that you chose. |
| **Running sessions** | Stay on the server where they started. To move one, press `S` on it in the sessions pane, or run `agentop server move <session-id> <server>`. |
| **The model** | The agent still asks for its own model names. When the server refuses a name, Before the agent sees the refusal, Cortex sends the request again with your `--main` or `--helper` model. It remembers the refusal for an hour. |
| **The agent's own settings** | Do not change. If Claude Code already has `ANTHROPIC_BASE_URL` set to an ete server, routing still works. Cortex puts the chosen server's key on each request. |
| **Where it shows** | The agents pane's `SERVER` column names each agent's server. With more than one server configured, the sessions table gets one too. |
| **Switching back** | `agentop server use <server> --agent <agent>`, or `agentop server reset --agent <agent>` to go back to the agent's own provider. |
| **If the server stops** | Its sessions' requests fail. Nothing fails over on its own. Move the session, or route the agent to another server. |

**Moving a long session is slow.** The first turn on the new server sends the
whole conversation again, with no cache, so the wait is long and it costs
more. On a local model, a conversation bigger than the window is cut without
an error. Start a new session when you can.

For every form of the command, see
[Choosing an inference server](../cmd/agentop/README.md#choosing-an-inference-server-agentop-server).

## Check it works

Run `agentop server`. It reads each server's model list with its key. It
never shows a key.

```
  bob       api.us-east.bob.ibm.com                     main premium-ide → premium-ide · helper fast → fast
      api: chat-completions=/v1/chat/completions
      auth header: x-api-key
      headers: User-Agent
  cb        litellm.cb.ete.res.ibm.com                  main glm → rits/zai-org/glm-5-3 · helper NVIDIA-Nemotron-3-Ultra-550B-A55B-NVFP4 → rits/nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B-NVFP4
  ete-ext   ete-litellm.ai-models.vpc.res.ibm.com       main claude-opus-4-6 → claude-opus-4-6 · helper claude-sonnet-4-6 → claude-sonnet-4-6   claude-code
  ete-int   ete-litellm.ai-models.vpc-int.res.ibm.com   main claude-opus-5-5 · helper claude-sonnet-5-5 (model list unavailable: could not reach the server: context deadline exceeded)
  ollama    http://localhost:11434                      main granite4.2:8b → granite4.2:8b · helper granite4.2:8b → granite4.2:8b
```

- The last column names the agents that each server gets.
- `model list unavailable` means that agentop cannot reach the server. On
  `ete-int`, `ete-ext` or `cb`, you are not on the IBM network. Requests routed there fail until
  the server answers again.

Then start a new session in the agent. In `agentop`, the sessions table's
`SERVER` column names the server of each session.

## Cost

| Server | How Cortex prices it |
|---|---|
| `ete-int`, `ete-ext` | In dollars. LiteLLM sends the cost of a request that does not stream. For other requests, Cortex uses its price list. See [pricing](pricing.md). |
| `cb` | In dollars when LiteLLM sends the cost of the request. Cortex's price list has no rate for the `cb` models, so the other requests show as unpriced until you give them a rate. See [pricing](pricing.md). |
| `bob` | In Bobcoins, at a flat 2 Bobcoins for each million tokens. Input, output and cache reads cost the same. A local install has this rate in `~/.cortex/config.yaml`. agentop shows Bobcoins apart from dollars, and never adds the two. |
| `ollama` | Cortex counts the tokens, but shows the model as unpriced until you give it a rate. See [pricing](pricing.md). |

Bob's budget is per team, and Bob refuses requests with 402 when the team
spends it all. A coding agent can send many millions of tokens each day. Try
a small task on Bob before you route a whole day of work to it. To see the
spend, run `agentop cost --by agent`.

## Limits on Bob

Cortex translates each Anthropic request to chat completions for Bob. Some
requests cannot go across:

- A request with a PDF document gets a 400 from Cortex, and nothing goes to
  Bob.
- Cortex removes the fields that chat completions has no place for, such as
  `metadata` and `top_k`. The request row in agentop names them.

See [Servers that serve another protocol](inference-router-plugin.md#servers-that-serve-another-protocol)
for the full rules.

## Trying another local model

Check any local model the same way before you rely on it:

| Check | How |
|---|---|
| Calls tools | `ollama show <model>` lists `tools` under capabilities. A model that cannot call tools cannot drive a coding agent. |
| Keeps thinking apart from the answer | Ask it something small. The thinking should arrive in its own field (`thinking` blocks or `reasoning`), not in the answer. |
| Window fits the agent | See [Choose a model](#choose-a-model). |
| Fits in memory | `ollama ps` shows the real size and `100% GPU`. The download size is only the weights. |

Then give it a small real task through the agent before a large one.

## See also

- [Choosing an inference server](../cmd/agentop/README.md#choosing-an-inference-server-agentop-server):
  every `agentop server` command
- [Local model servers](../cmd/agentop/README.md#local-model-servers):
  vLLM, llama.cpp and LM Studio
- [The inference-router](inference-router-plugin.md): routing, pins and
  translation
- [Claude Code](agents/claude-code.md) and [OpenCode](agents/opencode.md):
  their switching sections
