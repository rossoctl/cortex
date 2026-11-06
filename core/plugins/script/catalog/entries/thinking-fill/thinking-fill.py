#!/usr/bin/env python3
"""Keeps Claude's earlier reasoning through a LiteLLM gateway that drops it.

LiteLLM 1.100.0 to 1.105.1, and 1.106.0, delete every thinking block in a
request's history whose text is empty, before they send the request on
(BerriAI/litellm#41361, fixed in 1.105.2 and 1.106.1). Claude Opus 5.x and
Fable return their reasoning in exactly that shape by default: an empty
"thinking" text and an encrypted "signature" that holds the reasoning. So
behind such a gateway the model never sees its earlier reasoning, and after
every tool call it works its plan out again.

This script gives each such block one character of text. The gateway then
forwards it, and the provider reads the reasoning from the signature, which
the script does not change.

It needs no configuration. A LiteLLM gateway names its version in the
x-litellm-version header of every response; the script acts only on a host
whose latest response named an affected version. Requests to any other host,
and every request before a host has answered once, pass unchanged. When the
gateway is upgraded, its next response turns the script off for that host.

It changes only Anthropic Messages requests (a path ending in /v1/messages),
and only blocks that carry a signature and empty text.

See "thinking-fill" in docs/script-plugin.md.
"""

import re

from cortex import on, run

FILL = "."

# host -> whether its latest response named an affected LiteLLM version.
# Kept for the life of the script, which restarts when Cortex does.
affected = {}


def drops_signed_thinking(version):
    """True for the LiteLLM versions that delete signed empty thinking blocks:
    1.100.0 through 1.105.1, and 1.106.0 (pre-releases count as their release)."""
    m = re.match(r"v?(\d+)\.(\d+)\.(\d+)", version or "")
    if not m:
        return False
    v = tuple(int(x) for x in m.groups())
    return (1, 100, 0) <= v <= (1, 105, 1) or v == (1, 106, 0)


def _host(flow):
    return (flow.host or "").split(":")[0].lower()


def _messages_path(flow):
    return (flow.path or "").split("?")[0].endswith("/v1/messages")


@on.response
def note_gateway_version(flow):
    version = flow.response.headers.get("x-litellm-version") if flow.response else None
    if version is not None:
        affected[_host(flow)] = drops_signed_thinking(version)


@on.request
def fill_empty_thinking(flow):
    if not affected.get(_host(flow)) or not _messages_path(flow):
        return
    body = flow.json
    if not isinstance(body, dict):
        return
    filled = 0
    for msg in body.get("messages") or []:
        if not isinstance(msg, dict) or msg.get("role") != "assistant":
            continue
        content = msg.get("content")
        if not isinstance(content, list):
            continue
        for block in content:
            if (
                isinstance(block, dict)
                and block.get("type") == "thinking"
                and block.get("thinking") == ""
                and isinstance(block.get("signature"), str)
                and block["signature"]
            ):
                block["thinking"] = FILL
                filled += 1
    if filled:
        flow.set_body(body)
        print(f"thinking-fill: kept {filled} earlier thinking block(s)")


if __name__ == "__main__":
    run(enforce=True)
