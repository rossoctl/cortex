# `inference-router`

Sends a coding agent's new sessions to the inference server chosen for it — a
LiteLLM gateway or a local model server such as Ollama, with its own URL and key —
and keeps every session on the
server it started on until you move it (see **Moving a running session**), wherever the agent addressed its requests: its provider,
Anthropic, OpenCode Zen or Bob's gateway, so the agent needs no setting of its own
beyond going through the proxy. Routing is opt-in per agent: an agent not listed
under `agents` is not routed, and its requests go where it sent them, keys untouched. Manage it
with [`agentop server`](../cmd/agentop/README.md#choosing-an-inference-server-agentop-server);
the config stays hand-editable. Built for the laptop: it needs a listener that
honors a redirect, so `cortex-envoy` and a config with an `mtls:` block refuse it
at startup, and in-cluster use has not been examined.

- `servers` (map, required) — inference servers by name: lowercase letters, digits, `.`, `_` and `-`. Each has:
  - `url` (string, required) — `scheme://host[:port]`, `http` or `https`, no path, query or fragment. A port must be 1–65535; the scheme's default is dropped, so `https://x:443` is `x`. No two servers may share a host, compared without case and, except on a loopback host such as `localhost`, without port, since one machine serves many things there: a session's server is named from the host its requests went to. A plain-`http` server on another machine logs a WARN at load, since routed requests would cross the network decrypted.
  - `key` (string, required) — the API key sent to this server in place of the client's, in the header the client used: `X-Api-Key` when it sent one, `Authorization: Bearer` otherwise, both when it sent both. Printable ASCII, no spaces. `/config` and `/v1/pipeline` redact it.
  - `main` (string, required) — a word naming this server's model for an agent's main work, a request that carries tools, sent in place of a model the server refuses (see **Models**). It resolves to the newest model on the server's list whose name, without its provider prefix, contains the word, case ignored: `opus` against `aws/claude-opus-5-5` and `claude-opus-4-8` is the first. No `/` and no whitespace. The fields `opus`, `sonnet` and `haiku` it replaced are refused by name.
  - `helper` (string, required) — the same for a request without tools, such as a title.
- `agents` (map) — agent name, as agentop shows it (`claude-code`, `opencode`), to a server name. Each value must name a listed server. `unknown`, the name for requests with no User-Agent, cannot be routed.
- `sessions` (map) — session id to a server name, moving one running session to that server. Each value must name a listed server, or be `""` to stop routing that session. An entry beats the session's pin and every rule below, and the pin follows it, so removing the entry does not move the session back; `agentop server move` and the TUI's `S` on a session row write these; a `server move` from the shell also prunes an entry whose session is neither live nor archived. A synthetic id (`default`, `pending:<agent>`) is refused: it holds many conversations, not one to move.

```yaml
      - name: inference-router
        config:
          servers:
            ete:
              url: https://ete-litellm.example.com
              key: sk-…
              main: opus          # the newest listed model whose name contains it
              helper: haiku
            glm:
              url: https://glm-litellm.example.com
              key: sk-…
              main: glm
              helper: nemotron
          agents:
            claude-code: glm     # only agents listed here are routed
            opencode: glm
          sessions:
            8e298f11-8bf1-…: ete  # one running session, moved ("" to stop routing it)
```

**What it does to a request.** Only a request the forward proxy re-sends — a plain
proxied one, or one decrypted by the TLS bridge — can be routed. A `CONNECT` or a
transparently redirected connection is dialed where the client chose, and is
`skip/not_redirectable`. On a server's host every path is handled,
`/v1/models` and `count_tokens` included. A request to any other host is
**captured** when it is a routed agent's `POST` that `inference-parser` read as
inference, with a path ending in `/chat/completions`, `/v1/messages` or
`/responses`: it is sent to the agent's server at `/v1/chat/completions`,
`/v1/messages` or `/v1/responses`, the query string kept, and its `modify/routed`
carries `captured` with that path. The parse is required, not just the path: an
agent's own tools can `POST` to any API whose path ends the same way. Anything else
to a host no server has is `skip/not_an_inference_server`. Then the session
decides:

- A session is pinned by the first request that decides it, by the order below,
  to where that request went: a server when the redirect took effect, or "not
  routed" when the request stayed where the client sent it — because the agent is
  not routed, or because the router runs under `on_error: observe`, even for an
  agent with a server. A session started under observe therefore stays unrouted
  after a switch to enforce. A request whose redirect failed pins nothing, and the
  next one decides again. Every request renews
  the pin, which lapses 30 days after the last. On a local install pins are kept in
  `~/.cortex/plugin-state.json`, which a restart keeps; elsewhere in the process
  store, which a restart empties.
- A request in a session its agent has not pinned is decided in this order, for a
  routed agent:
  1. The session's history: the latest earlier inference request, other than a
     side request (3), that agent sent in the session to a server's host keeps
     the session on that server, from then with that server's key. The history
     is the session store's and, where the proxy keeps a session archive, up to
     2000 of the archive's events from before the oldest the store holds. A
     request the router would capture, recorded where the agent sent it, was
     made while the router was not routing the agent — before it was routed, or
     under observe — so the session ran unrouted and is not routed now: routing
     an agent does not move a conversation already running on its provider. Any
     other request is no evidence.
  2. A request `inference-parser` did not read as inference — `count_tokens`,
     `/v1/models` — goes to its agent's current server and pins nothing.
  3. A Claude Code request's own turn: an opening turn, with no assistant reply
     yet, goes to its agent's current server; a continuation, a conversation the
     router did not see begin, is not routed, and is pinned so; a one-shot with no
     tools, or a subagent's request, pins nothing. A one-shot — the title request,
     auto mode's classifier, the permission monitor — goes to its agent's current
     server, since it is no part of the conversation and a new session's title
     request can arrive before its opening turn. A subagent's request to a server's
     host is not routed: it is a conversation of its own, which a `/clear` can carry
     onto a new session id mid-task.
  4. Otherwise its agent's current server.

  The router reads the `inference` record `inference-parser` puts on each request,
  so a chain without `inference-parser` before the router is refused.
- **Moving a running session.** A `sessions:` entry is the user's explicit move, and
  it is the one thing that moves a running conversation: the request goes to the
  entry's server (or, for `""`, where the client sent it), and the session's pin is
  settled to it, so removing the entry later leaves the session where the move put
  it. An entry applies to the session id whatever agent's request carries it; the
  pin it settles keeps its agent binding. The moved history loses only the
  reasoning the new server cannot read (see Reasoning below); what a move costs is
  the cache — the session's whole context is
  re-sent uncached on its first turn at the new server, which `agentop server move`
  and the TUI's confirmation say before the write.
- A pin is its agent's. A request from another agent filed under the same session
  id — by process attribution, which files a command an agent runs under that
  agent's session and is on by default on a laptop; by the active-session fallback
  with client affinity off; or by a header id two clients share — is decided as if
  the session were unpinned, from that agent's own history and server, and leaves
  the pin alone.
- Changing `agents` therefore moves only new sessions: the pins and the history
  survive a hot reload, and on a local install a restart too. A proxy with neither
  a plugin store nor a session archive loses both on restart, and a running
  session's next request is then decided as an unpinned one's. The store also drops
  quiet sessions while the proxy runs — `session.max_sessions` (100 by default)
  evicts the least recently used, and a configured `session.ttl` expires them — and
  a session dropped before the router pinned it has only the archive's history,
  where there is one; a pin outlives that. A request with no session follows its agent's
  current server, unpinned, and so does one filed under a synthetic session — the
  `default` bucket or a `pending:<agent>` id — since each holds many conversations,
  not one.
- Not routed: `skip/not_routed`, and the request goes where the client sent it, its key
  untouched. Its body changes only for a moved session (see Reasoning).
- Pinned to a server since removed: `deny/pinned_server_removed`, a 503 asking for
  a new session. The router never moves the conversation on its own; a
  `sessions:` entry can move it.
- Otherwise the request is redirected to the server, and the key is replaced only
  once the redirect has taken effect, so it goes to the server and nowhere else.
  Every routed request is redirected, even one that already names the server's
  host: the `Host` header is the client's word, and a TLS-bridged request is
  otherwise dialed to the host the client `CONNECT`ed to, which need not be the
  same. Each therefore carries the framework's `modify/redirected` record — with
  `from` equal to `to` when the agent's base URL (`ANTHROPIC_BASE_URL`, for Claude
  Code) already names the server — followed by the router's `modify/routed` with
  `server` and `pin` (`new`, `existing`, `moved` or `none`; `moved` is a `sessions:`
  entry moving a session that already held a pin).
- **Models.** A routed request goes with the model the agent asked for: the router
  does not read the server's list to judge it, because a LiteLLM server serves
  aliases it does not list. When the server refuses the model — LiteLLM's 403
  `team_model_access_denied` or 400 `Invalid model name passed in model=`, Ollama's
  404 or llama.cpp's 400 `model <m> not found`, or vLLM's 404 ``The model `<m>`
  does not exist.``, where `<m>` is the name the request was sent with, quoted in
  any of the styles a server has been seen to use — the router
  records the refusal for an hour and, through `pipeline.Resender`, has the forward proxy send the request
  again, once and before the agent sees anything, with the server's substitute:
  `main` for a request with tools, `helper` for one without, as the server's list
  resolves them. The re-sent request's row carries `modify/resent` with `server`,
  `model` and `substitute`, after the framework's `modify/body_rewritten` and
  `modify/model_rewritten`; the refused attempt has rows of its own, under its own
  request id. While a refusal is recorded, requests for that model are sent with
  the substitute from the start, with no refused round trip. A session keeps the
  substitute it was given, so a conversation never changes model part-way. The
  inference record's `model` becomes the server's, with `requestedModel` keeping
  the agent's: settlement prices the server's model, and agentop's detail pane
  shows both.

  A request already sent with a substitute is never sent again, so a refused
  substitute reaches the agent; so does a refusal when the server's list names no
  model for the word, or none has been read yet, recorded as `skip/no_substitute`.
  Any other answer — a bad key's `401`, a rate limit, a server error — reaches the
  agent as the server sent it. The lists are fetched from `GET /v1/models` with
  each server's key when the router starts and hourly after, the last good one
  kept in the plugin store. A request whose body names no model is routed as it is.
- **Limits.** When the server refuses a switched request — one the router sent to
  another host, or with another model, than the agent asked for — because its output
  field or its prompt is too large for the model it was sent with, the router learns
  that limit from the error message
  (`inference-router/limit/<server>/<model>`, 30 days), fits the request and resends it
  once (`modify/resent` with `limit`, `value`, `stated`). Later requests are fitted
  before they are sent: `modify/output_capped` lowers `max_tokens`,
  `max_completion_tokens` or `max_output_tokens`, except that it is left alone when the
  new value would not be above the body's `thinking.budget_tokens`, which Anthropic and
  Bedrock refuse; `modify/context_trimmed` drops the oldest turns after the first user
  message once a request passes 85% of the window, down to under 60%, and puts a
  one-line note in the first message kept. Trimming applies to Anthropic messages and
  OpenAI chat-completions bodies only: a responses-format request (`input`) has only
  its output field lowered. A request one turn of which is larger than the window has
  no turns dropped, though its output field may still be lowered (`skip/limit_unfittable`);
  one for which a limit is learned but fitting changes nothing is recorded as
  `skip/limit_not_fitted`. The agent is not told: its own display keeps the whole
  conversation.
- **Reasoning.** A session's reasoning artifacts can be read only by the server
  that wrote them: `thinking` and `redacted_thinking` blocks in Anthropic messages,
  and `reasoning` items in a responses-format `input`. Another server ignores them
  at best; Bedrock 4.6 models stop thinking with another server's in their history,
  and Bedrock refuses Ollama's unsigned blocks. So once a session has moved, each
  request it sends keeps the artifacts its destination wrote and drops the rest,
  before it is sent (a message the drop empties goes whole: an empty content array
  is refused at Anthropic). A session moved back gets its first server's artifacts
  back. Chat-completions bodies are never touched: their reasoning fields are plain
  text every server measured accepts. A session that never moves has nothing
  dropped and nothing recorded. To know who wrote what, the router keeps, per moved
  session, which server each artifact was first sent after: an artifact first
  appears in the request that follows its writer's answer, so its writer is where
  that conversation's previous request went (`inference-router/reasoning/<session>`,
  in the plugin store with the pin's TTL, renewed with the pin). Moves to the
  client's own provider are covered too, though their requests are not routed.
  Each drop is recorded as `modify/reasoning_stripped` with the number of
  artifacts dropped.
- A redirect the listener refuses: `deny/redirect_failed`, a 503.
- Under `on_error: observe` nothing moves, the client's key stays and no model is
  chosen or re-sent. The timeline shows two rows: the framework's shadow
  `modify/redirected`, and the router's `observe/would_route`.

**Put it last in the outbound chain**, where `agentop server add` puts it. A
redirect moves `pctx.Host`, so the plugins before the router decide on the host
the client asked for, and a plugin after it would see the server's instead; put
nothing after it that keys on the host. Session events, usage and cost follow the
server's host, and each event's `requestedHost` keeps the one asked for when it
differs. The router also rewrites the request body to send a substitute, so the
framework holds it after every body reader: a chain with a parser after the router
is refused, at startup and on reload.
