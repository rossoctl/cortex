# Route an agent to IBM's inference servers

Cortex can send an agent's inference to one of three IBM servers. You do not
change the agent's own settings. `agentop server` adds each server once, and
then chooses which server an agent's new sessions use.

| Server | URL | Network | Protocol |
|---|---|---|---|
| `ete-int` | `https://ete-litellm.ai-models.vpc-int.res.ibm.com` | IBM network or VPN only | LiteLLM: Anthropic and chat completions |
| `ete-ext` | `https://ete-litellm.ai-models.vpc.res.ibm.com` | Internet | LiteLLM: Anthropic and chat completions |
| `bob` | `https://api.us-east.bob.ibm.com/inference` | Internet | Chat completions only |

The server names in the first column are only names. You can use other names.

## Before you start

- Install Cortex and route the agent through it. See
  [getting started](getting-started.md) and the guide for your agent, such as
  [Claude Code](agents/claude-code.md).
- For `ete-int`, connect to the IBM network or VPN. Its name resolves to an
  address in `9.0.0.0/8`, which is not on the internet.
- For `ete-int` and `ete-ext`, get access to the models first. See
  [Get a key](#get-a-key).

## Get a key

Make a key on each server's own key page.

| Server | Where to make the key |
|---|---|
| `ete-int` | <https://ete-litellm.ai-models.vpc-int.res.ibm.com/ui/api-keys/> |
| `ete-ext` | <https://ete-litellm.ai-models.vpc.res.ibm.com/ui/api-keys/> |
| `bob` | <https://bob.ibm.com/admin/apikeys> |

- **ete:** before you can make a key, ETE must give you access. Request
  [Research managed non open source models](https://data-forms.apps.res.ibm.com/user/form?type=ROAI)
  and name the Claude models that you need. ETE sends an email when your team
  has access.
- **Bob:** make a static API key. Do not use Bob's login token. That token is a
  JWT, and it expires after 12 hours.

Keep the key in a password manager. `agentop server add` asks for it at a
prompt that does not echo. The key never goes into shell history.

## Add the servers

Run one `add` for each server that you want. Each `add` asks for that
server's key.

```sh
agentop server add ete-int https://ete-litellm.ai-models.vpc-int.res.ibm.com \
  --main claude-opus-5-5 --helper claude-sonnet-5-5

agentop server add ete-ext https://ete-litellm.ai-models.vpc.res.ibm.com \
  --main claude-opus-4-6 --helper claude-sonnet-4-6

agentop server add bob https://api.us-east.bob.ibm.com/inference \
  --main premium-ide --helper fast \
  --auth-header x-api-key --header User-Agent=bob-shell
```

- **`--main` and `--helper`:** the model for the agent's main work, and the
  model for side requests such as titles. Cortex uses them only when the server
  refuses the model that the agent asked for. Each value is a word: `add` picks
  the newest model on the server's list whose name contains the word. A full
  name, as above, usually picks that model. A short word, such as `opus`,
  picks the newest Opus, also one that the server adds later.
- **The models above:** on 2026-11-18, the `ete-ext` and `bob` words picked
  these models. Nobody checked the `ete-int` list on that day. `add` reads the server's list and prints the model that each word picks. It
  refuses a word that names no model, and it shows the list. Use that list to
  choose again.
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

`add` writes `~/.cortex/config.yaml`, and the proxy reloads it. Nothing
restarts. The `ete-int` and `bob` entries look like this:

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

### When `add` fails

| Message | Cause | What to do |
|---|---|---|
| `could not reach the server: context deadline exceeded` | For `ete-int`: you are not on the IBM network. | Connect to the IBM network or VPN. Then run `add` again. |
| `the server answered 401, …` | The key is wrong, or it is a key for another server. | Make a key on the server's own key page. |
| `the server answered 403, 403, 403, 403` for Bob | The two Bob flags are missing. | Run `add` again with `--auth-header x-api-key --header User-Agent=bob-shell`. |
| `--main … names none of …'s models` | The server does not serve a model with that word. | Choose a word from the list that the message shows. |

To change a key or a model later, run the same `add` again. `add` asks
before it replaces the entry. Add `--yes` to skip the question.

## Choose a server for an agent

```sh
agentop server use ete-ext --agent claude-code   # new Claude Code sessions → ete-ext
agentop server move <session-id> bob             # one running session → bob
agentop server reset --agent claude-code         # back to Claude Code's own provider
```

- **New sessions** go to the server that you chose.
- **Running sessions** stay on the server where they started. To move one,
  use `agentop server move`, or press `S` on the session in the sessions pane.
  The first turn after a move sends the whole conversation again with no
  cache, so it is slow and it costs more.
- **The agent's own settings** do not change. If Claude Code already has
  `ANTHROPIC_BASE_URL` set to an ete server, routing still works. Cortex puts
  the chosen server's key on each request.
- **In the TUI**, press `S` on the agents pane to do what `use` and `reset` do.

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
  ete-ext   ete-litellm.ai-models.vpc.res.ibm.com       main claude-opus-4-6 → claude-opus-4-6 · helper claude-sonnet-4-6 → claude-sonnet-4-6   claude-code
  ete-int   ete-litellm.ai-models.vpc-int.res.ibm.com   main claude-opus-5-5 · helper claude-sonnet-5-5 (model list unavailable: could not reach the server: context deadline exceeded)
```

- The last column names the agents that each server gets.
- `model list unavailable` on `ete-int` means that you are not on the IBM
  network. Requests routed to `ete-int` fail until you connect.

Then start a new session in the agent. In `agentop`, the sessions table's
`SERVER` column names the server of each session.

## Cost

| Server | How Cortex prices it |
|---|---|
| `ete-int`, `ete-ext` | In dollars. LiteLLM sends the cost of a request that does not stream. For other requests, Cortex uses its price list. See [pricing](pricing.md). |
| `bob` | In Bobcoins, at a flat 2 Bobcoins for each million tokens. Input, output and cache reads cost the same. A local install has this rate in `~/.cortex/config.yaml`. agentop shows Bobcoins apart from dollars, and never adds the two. |

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

See [A translated request](inference-router-plugin.md#servers-that-serve-another-protocol)
for the full rules.

## See also

- [Run an agent on a local model](laptop-local-model.md): Ollama and other
  servers on your Mac
- [Choosing an inference server](../cmd/agentop/README.md#choosing-an-inference-server-agentop-server):
  every `agentop server` command
- [The inference-router](inference-router-plugin.md): routing, pins and
  translation
