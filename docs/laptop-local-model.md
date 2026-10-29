# Run an agent on a local model

Cortex can send an agent's inference to a model server on your own machine.
Use it when your provider is down or out of quota, or when you want the
traffic to stay local.

This page uses [Ollama](https://ollama.com) and IBM's Granite models. Any
OpenAI- or Anthropic-compatible server works the same way: LM Studio,
llama.cpp's `llama-server`, vLLM. See
[Local model servers](../cmd/agentop/README.md#local-model-servers) for their
details.

Before you start:

- **A local model is weaker than a frontier model.** Expect more retries and
  wrong turns on hard work. It keeps you working; it does not replace your
  provider.
- **Nothing restarts.** You add the server and route the agent with two
  commands, and the proxy reloads them.

## Choose a model

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
0.1 s. These numbers come from an M4 Max with Ollama 0.40.5; a smaller chip
is slower.

**What you give up:**

| Model | Good | Not so good |
|---|---|---|
| `granite4.2:8b` | Best answers | Needs the most memory |
| `granite4.2:3b` | Thinks before answering; twice as fast as 8b | Weaker answers than 8b |
| `granite4:tiny-h` | Fastest; memory hardly grows with the window | Older Granite 4.0; cannot think; not yet tried with an agent end to end |

Try `granite4:tiny-h` only if `granite4.2:3b` does not fit (see **Check what
loaded** below). Ollama refuses a thinking request to it
(`"granite4:tiny-h" does not support thinking`), and agents can send one.

Do not use `granite3.3`. It mixes its reasoning into the answer, and the
agent reads that as part of its work. Granite 4.2 keeps the two apart.

## Install Ollama and pull the model

| | |
|---|---|
| macOS | the app from [ollama.com/download](https://ollama.com/download) |
| Linux | `curl -fsSL https://ollama.com/install.sh \| sh` |

The macOS app starts Ollama at login. On Linux the installer starts an
`ollama` service; without systemd, run `ollama serve`.

Pull the model you chose:

```sh
ollama pull granite4.2:3b
```

Check that Ollama lists it:

```sh
curl -s localhost:11434/v1/models | jq -r '.data[].id'
```

## Context length

Set the window yourself. Ollama does not refuse a prompt that is too long; it
cuts it without an error, and the agent then behaves strangely with nothing
saying why. A fresh install also picks a small window from your GPU memory,
not the one your agent needs.

Set it where the server starts. The examples use 32k (`32768`); use the
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

## Route the agent to it

```sh
echo ollama | agentop server add ollama http://localhost:11434 \
  --main granite4.2:3b --helper granite4.2:3b --key-stdin
agentop server use ollama --agent <agent>
```

- **The key:** Ollama checks none, but `add` needs one, so `echo ollama`
  gives a placeholder.
- **`--main` and `--helper`:** the model for the agent's main work and for
  side requests such as titles. Give the full tag: a bare `granite` picks
  `granite3.3` if that is pulled too. `add` prints what each one picked, and
  refuses one that names no model on the server, which also catches a wrong
  URL or a stopped server.
- **`--agent`:** the agent's name as agentop shows it, such as `bob` or
  `claude-code`. `agentop configure status` lists yours. In the TUI, `S` on
  the agents pane does the same.

To change the model later, run the same `add` with the new model and `--yes`,
which replaces the entry. Running sessions keep the model they started with,
so start a new session to use the new one.

## What happens next

| | |
|---|---|
| **New sessions** | Go to the local server. |
| **Running sessions** | Stay where they are. To move one, press `S` on it in the sessions pane, or run `agentop server move <session-id> ollama`. |
| **The model** | The agent still asks for its own model names, which Ollama does not have. Cortex sends the request again with your `--main` or `--helper` model before the agent sees the refusal, and remembers that for an hour. |
| **Where it shows** | The agents pane's `SERVER` column names each agent's server. With more than one server configured, the sessions table gets one too. |
| **Cost** | Tokens are counted, but the model shows as unpriced until you give it a rate ([pricing](pricing.md)). |
| **Switching back** | `agentop server use <server> --agent <agent>`, or `agentop server reset --agent <agent>` to go back to the agent's own provider. |
| **If Ollama stops** | Its sessions' requests fail. Nothing fails over on its own; move the session or route the agent elsewhere. |

**Moving a long session is slow and may not fit.** Its first turn on Ollama
sends the whole conversation, with no cache, so the wait is long. If the
conversation is bigger than the window, Ollama cuts it without an error.
Prefer starting a new session for a local model.

## Trying another model

Check any model the same way before you rely on it:

| Check | How |
|---|---|
| Calls tools | `ollama show <model>` lists `tools` under capabilities. A model that cannot call tools cannot drive a coding agent. |
| Keeps thinking apart from the answer | Ask it something small. The thinking should arrive in its own field (`thinking` blocks or `reasoning`), not in the answer. |
| Window fits the agent | See [Choose a model](#choose-a-model). |
| Fits in memory | `ollama ps` shows the real size and `100% GPU`. The download size is only the weights. |

Then give it a small real task through the agent before a large one.

## See also

- [Choosing an inference server](../cmd/agentop/README.md#choosing-an-inference-server-agentop-server) —
  every `agentop server` command
- [Local model servers](../cmd/agentop/README.md#local-model-servers) —
  vLLM, llama.cpp and LM Studio
- [The inference-router](inference-router-plugin.md) — routing, pins and
  model substitution in full
- [Claude Code](agents/claude-code.md) and [OpenCode](agents/opencode.md) —
  their switching sections
- [Pricing](pricing.md) — adding a rate
