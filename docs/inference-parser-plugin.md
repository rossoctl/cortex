# `inference-parser`

Parses outbound LLM inference requests/responses into `pctx.Extensions.Inference` for
downstream policy plugins, **and prices the finished response** — it is the one place
tokens become dollars.

It reads three dialects, chosen by how the request path ends, under any prefix: a path ending
in `/completions` is OpenAI chat completions (or legacy completions), and one ending in
`/v1/messages` is Anthropic Messages. A path ending in `/responses` is the OpenAI
Responses API — the public `/v1/responses` and Codex's `chatgpt.com`-hosted gateway at
`/backend-api/codex/responses` both match. That covers providers that mount the same API under a
prefix of their own — IBM Bob's `/inference`, OpenCode Zen's `/zen`, OpenRouter's `/api`,
Groq's `/openai`, Azure's `/openai/deployments/<d>` — with no code change. A body is taken
for inference only if it carries a `messages` array, or a `prompt` for legacy completions.
A body that fails that check is recorded the way an unrecognised path is: with no inference
record, and still priced from a gateway's cost header. Other dialects — Gemini's native
API, Bedrock's native API — are not parsed.

Costing lives here because this is the only component that knows when usage is *final*: it
owns the three response-finalization paths and the assembled-usage handling (Claude Code's
`?beta=true` path reports cache counts on `message_delta`, not `message_start`). So each
priced request has its record, with or without `litellm-budget-track` in the pipeline.

A record is not the same as a price. Where no rate resolves for the model, the record is
still published — carrying the token counts, any avoided cost, and no dollar figure — and the
gap is named in `/v1/usage`'s `unpricedBy` so an operator knows which `pricing:` entry to
add. An absent figure is reported as absent, never as `$0.00`.

The arithmetic and the gateway header semantics are in `core/cost/settle`, not in the parser:
a provider-shaped body parser has no business knowing one gateway's header names. The result
is published as a cost record keyed `cost` on the session event (see
[Cost records](pricing.md#cost-records)).

No configuration — no config struct, does not implement `Configurable`. Rates arrive by
injection from the top-level [`pricing:`](pricing.md#overriding-in-config) section; with none configured the parser
still parses and reports the traffic as unpriced.
