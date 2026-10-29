# WebSocket upgrades on intercepted connections

**Date:** 2026-10-28
**Status:** designed
**Issue:** #1488 (parent #1490)
**Contract:** MOCA MI1 §8.5 item 5

## Goal

A sandbox tool that speaks WebSockets through an intercepted host works, with the
CONNECT-host check applied to the upgrade request and one audit event recorded.

## The contract clause

MOCA §8.5 item 5, "Every decrypted request", asks for two things this document treats
separately because they have very different costs:

| Clause | Cost |
|---|---|
| "`Host` (HTTP/1.1) or `:authority` (HTTP/2) must equal the CONNECT host, else `421` and an audit event" | small, and implemented alongside this note |
| "HTTP/2 and WebSocket upgrades are allowed; the `Host` check applies to the upgrade request" | a new relay path — the subject of this note |

The first is unconditional and applies to every decrypted request, not only upgrades.
It is the narrower, better-understood half, so it ships first and independently; the
upgrade work then has a host check to apply rather than needing to invent one.

## Prior art: PR #1467 already carries a WebSocket

**Open PR #1467 ("Carry Codex's Responses WebSocket and parse every turn on it",
`feat/codex-websocket`, `CHANGES_REQUESTED` as of 2026-10-28) builds the relay this
note's §"Proposed shape" describes** — `core/listener/forwardproxy/websocket.go` plus
an RFC 6455 codec in `core/listener/internal/wsframe`. It is scoped to Codex against
OpenAI's hosts (`chatgpt.com`, `api.openai.com`) and gated by
`listener.responses_websockets: parse | decline | forward`, default `parse`.

That changes this note from a build plan into a scoping question, and it settles
several of the cost arguments below empirically rather than by estimate:

| This note's claim | What #1467 shows |
|---|---|
| `Client.Do` cannot surrender the connection on a `101` | Confirmed — it completes the upgrade over the bridge's upstream client and hijacks both sides, which is why it needed a new file rather than an edit |
| Frames must not reach the body-reading plugins | Confirmed, and it goes further: it *parses* turns deliberately, feeding `response.create` messages through the pipeline as synthetic `POST`s |
| The handshake needs the `Sec-*` headers intact | Confirmed, and it strips `permessage-deflate` from the offer so frames stay readable |
| A `WritesResponseBody` / `WritesDestination` plugin is a problem | Confirmed — such a pipeline declines the socket with a 426 and the client falls back to HTTPS |

It also did **not** amend `TestStripForUpstream_StillRemovesTheUpgrade`: the strip
survives as the explicit `forward` mode, with the test pinned to that mode. So the
deliberate-decision reading below is right, and the carve-out is additive to the strip
rather than a replacement for it — a useful precedent for #1488.

**What #1467 does not do, and #1488 still needs:**

- **No Host check.** No `421` / `StatusMisdirectedRequest` anywhere in its diff, so the
  "the `Host` check applies to the upgrade request" half of item 5 is unmet by it. That
  is the half this branch implements, and it applies to #1467's handshake for free,
  because the handshake arrives through `bridgedHandler`.
- **Wrong hosts for MOCA.** The carry is deliberately limited to OpenAI, because what
  the socket carries is only known for Codex. A MOCA sandbox's tools are arbitrary, so
  #1488 needs a path that relays frames it does **not** understand — opaque relay, not
  turn parsing.

**The open question #1467 raises for #1488** is whether to generalise its machinery —
reuse `wsframe` and the hijack/relay with parsing switched off for non-OpenAI
intercepted hosts — or to keep a separate opaque path. Generalising is clearly
preferable to a second codec, but it should wait for #1467 to land, since it is under
review and its internals may move. Recommend #1488's relay be specified as "`wsframe`
relay with no turn parsing, on an intercepted host, behind the Host check" once #1467
merges, and not started before then.

## What Cortex does today

`stripForUpstream` (`core/listener/forwardproxy/server.go:1046`) deletes `Upgrade`
along with the rest of the hop-by-hop set, per RFC 9110 §7.6.1. It also deletes
`Connection`, which matters: a WebSocket handshake needs both.

The strip is **deliberate and asserted**. PR #1414 added `pctx.RecordStrippedUpgrade`
(`core/pipeline/context.go:844`), called at `server.go:464`, so that the resulting
failure is explainable rather than mysterious — the Codex case, where
`wss://chatgpt.com/backend-api/codex/responses` loses its `Upgrade`, the origin sees a
bare `GET` on an upgrade-or-POST endpoint, and answers `405`. The marker reaches the
timeline as `UpgradeStripped` (`core/pipeline/session.go:252`) and the TUI
(`cmd/agentop/tui/detail_pane.go:126`).

`upgradestripped_test.go:83`, `TestStripForUpstream_StillRemovesTheUpgrade`, pins the
strip, and its comment is explicit that recording the value while leaving the header in
place "would pass the two tests above while breaking the behaviour they describe".

**So this is not a bug to fix. It is a deliberate decision to revisit for a case it did
not consider: an intercepted connection whose CONNECT host is known.** Any change here
amends that test rather than merely adding to it, and the amendment should keep its
intent — that a *tunnelled or non-upgrade* request still has `Upgrade` removed.

## Why the header is the small part

Keeping `Upgrade` and `Connection` on the forwarded request is necessary and nowhere
near sufficient. The current outbound path cannot carry a handshake:

1. **The request is made with `s.Client.Do`** (`server.go:1096-1130`). Go's
   `http.Transport` does not surrender the connection on a `101`: `Do` returns the
   response, and there is no documented route to the underlying `net.Conn` for raw
   framing. A WebSocket relay needs the socket, so this path needs a dedicated
   round-trip that hijacks both sides rather than `Client.Do`.
2. **`stripForUpstream` runs on resends too** (the `MaxResends` loop at `:1061`). A
   resend of an upgrade is meaningless — after a `101` there is no request to resend —
   so the upgrade path must bypass the resend loop, not just the strip.
3. **The handshake has four more headers.** `Sec-WebSocket-Key`,
   `Sec-WebSocket-Version`, and conditionally `Sec-WebSocket-Protocol` and
   `-Extensions` must pass through unmodified, and the `101`'s
   `Sec-WebSocket-Accept` must reach the client unrewritten. Any body-rewriting plugin
   on the response path would corrupt the handshake.
4. **Downstream needs a hijack.** The bridged handler writes through an
   `http.ResponseWriter` (`bridgedHandler`, `server.go:941`). Relaying frames means
   hijacking that too and copying bytes both ways until either side closes.
5. **Plugins must not see frames.** §8.5 says bodies on this path are "streamed, never
   inspected or buffered". WebSocket frames are not HTTP bodies; the SSE/MCP/inference
   parsers would read them as garbage. The relay must run with the body-reading
   pipeline disengaged.

That is a new code path parallel to the existing request path, not an edit to it. It is
the reason this note stops at design and does not carry an implementation.

## Proposed shape

Written before #1467 was found; read it as the shape to reach by **generalising
#1467's relay**, not as a separate implementation to start from scratch. The
conditions and the event accounting below still stand.

A narrow carve-out, taken only where all three conditions hold:

- the connection is **intercepted** (`bridgedHandler`, so a CONNECT authority is known),
- the request is a well-formed WebSocket upgrade (`Connection` contains `Upgrade`,
  `Upgrade: websocket`, `Sec-WebSocket-Key` present, `GET`),
- the Host check passes.

Everything else keeps today's strip and today's marker. Concretely:

1. In `bridgedHandler`, apply the Host check (shipping now, see below). On mismatch,
   `421` + audit event; the upgrade never starts.
2. Detect the upgrade and branch to a `serveUpgrade` path before `serveOutbound`.
3. `serveUpgrade` dials the origin itself, replays the handshake with the `Sec-*`
   headers and `Upgrade`/`Connection` intact, and requires a `101`. A non-`101` is
   relayed back as an ordinary response — the origin declining is not our error.
4. On `101`, hijack the downstream conn, write the response head verbatim, then
   `io.Copy` both directions with the pipeline disengaged.
5. Record **one** audit event at handshake time, in §8.5's shape, with the
   `UpgradeStripped` field left empty — nothing was stripped — and a new field saying
   the upgrade was *allowed*. The "one event" in the issue's "Done when" is this one;
   frames are not individually evented.

### Open questions for review

Three of the four below are **answered by #1467**, which is why adopting its machinery
is the recommendation rather than one option among several.

- **~~`UpgradeStripped` vs. a new field~~ — answered.** #1467 needs no new field: a
  carried socket simply leaves `UpgradeStripped` empty (asserted at
  `websocket_review_test.go`, "carries upgradeStripped=%q on a carried socket") and
  the socket is identified by `tunnelReason: "websocket"` on its open/close pair
  instead. Reuse that vocabulary — an `UpgradeAllowed` sibling is unnecessary.
- **~~Idle timeouts~~ — answered.** #1467 models the socket as a *tunnel*, not a
  request: the handshake `GET` is the open, the close carries status 101 with the whole
  socket's duration and bytes, and turns are request/response pairs between them.
  Tunnel rows are already excluded from `/v1/usage` ("a tunnel's lifetime is not a
  request latency"), which is exactly the accounting a long-lived relay needs.
- **~~h2c~~ — answered, and moot.** #1467 records that an HTTP/2 client never reaches
  the handler at all: Go's HTTP/2 server refuses a request carrying `Upgrade` with a
  400. Combined with the terminator offering `h2` via ALPN
  (`core/tlsbridge/terminator.go:14`), intercepted h2 arrives negotiated and never
  upgraded. Scope h2c out of #1488.
- **Interaction with `MOCA_EGRESS_INSPECT` — still open.** A host on the inspect list
  wants request detail, which an opaque frame relay cannot give. #1467 does not face
  this because it *parses* its one known protocol; a generalised opaque relay does.
  The likely answer is that an inspect-listed host refuses the upgrade rather than
  relaying blind, but that is MOCA's call to make.

## The Host check, shipping now

Independent of the above, and the prerequisite for it.

**Finding: Cortex enforces nothing like this today.** A search for `421` /
`StatusMisdirectedRequest` across the tree returns only an unrelated number in a
comment (`core/pipeline/promptcontext.go:34`). An intercepted connection currently
accepts any `Host` on a decrypted request.

This matters beyond upgrades. `bridgedHandler` sets `r.URL.Host = authority`
(`server.go:952`) — the CONNECT authority — so the request is *dialed* to the right
origin regardless. But `Host` is what a plugin reads for its credential decision
(`pctx.Host`), and §8.5 wants the credential decision made "always for the host the
connection is dialed to". A guest that opens a CONNECT to an allowlisted host and then
sends `Host: other.example` inside the tunnel presents one host to the policy layer and
another to the socket. Closing that is the point of the clause.

`bridgedHandler` is the correct and only insertion point: it is the sole path where a
decrypted request has a known CONNECT authority to be checked against.

Comparison is on host only, with the port excluded — `hostOnly(authority)` is already
how the terminator is given its name (`server.go:826`), a client may legitimately omit
a default `:443` from `Host`, and a port mismatch cannot reach a different origin
because the dial uses the authority, not `Host`.

## Testing

In-process Go, no cluster: the "Done when" is a WebSocket echo through an intercepted
host and one audit event, which an `httptest` origin plus a real `Server` covers
directly. #1467 proves the approach — its `websocket_test.go` drives a real CONNECT
bridge to a TLS WebSocket origin addressed as `chatgpt.com` — so #1488's echo test
should be built on that fixture rather than a new one. A Kind-based MOCA sandbox run is
worth doing once the relay exists, to exercise a real tool against a real origin, but
it would prove nothing the Go test cannot about the handshake itself.

The Host check ships in this branch with tests for: a mismatch answering `421` and
recording a `host_mismatch` denial event naming both hosts; a matching `Host` reaching
a TLS origin; four port and case variants (default `:443` omitted, port present,
non-443 origin, differing case) all matching; and an empty `Host` refused — that last
pinning against reading the `Host` *header* instead of `r.Host`, which would see `""`
on both HTTP/1.1 and HTTP/2 and so pass everything.
