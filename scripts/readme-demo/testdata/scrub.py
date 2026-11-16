"""Scrub one recorded Claude Code session into a README-demo replay fixture.

Keeps what the timeline reads: times, durations, token counts, models, tool names,
tool ids, finish reasons, and the return markers. Every other string becomes a hash.
Usage: python3 -I scrub.py <session.json> <out.json.gz>
It fails if the home directory, the user name or the model gateway's host survives.
"""

import getpass
import gzip
import hashlib
import json
import os
import re
import sys

src, dst = sys.argv[1], sys.argv[2]
view = json.load(open(src))


def H(s, n=8):
    return hashlib.sha256(s.encode()).hexdigest()[:n]


ids = {}


def sub_id(x):  # subagent ids: 17 chars, keep the shape
    return ids.setdefault(x, "a" + H("sub" + x, 16))


def tool_id(x):
    return ids.setdefault(x, "toolu_" + H("tool" + x, 20))


# The model gateway is whatever host served inference; it becomes api.anthropic.com.
LLM = {e.get("host") for e in view["events"] if e.get("inference")} | {
    e.get("requestedHost") for e in view["events"] if e.get("inference")
}
LLM.discard(None)


def is_llm(h):
    return bool(h) and (h in LLM or h.split(":")[0] in {x.split(":")[0] for x in LLM})


def host(h):
    if not is_llm(h):
        return h
    return "api.anthropic.com:443" if h.endswith(":443") else "api.anthropic.com"


MARKERS = [
    (
        re.compile(r"^Command running in background with ID: ([A-Za-z0-9_-]+)"),
        "Command running in background with ID: {}",
    ),
    (re.compile(r'<agent-message from="([A-Za-z0-9_-]+)"'), '<agent-message from="{}">'),
    (
        re.compile(r"<task-notification>\s*<task-id>([A-Za-z0-9_-]+)</task-id>"),
        "<task-notification><task-id>{}</task-id>",
    ),
    (re.compile(r"agentId: ([A-Za-z0-9_-]+)"), "agentId: {}"),
]
PROMPTS = ["Describe your most recent action in 3-5 words", "The user stepped away and is coming back"]


def text(s):
    if not s:
        return s
    keep = []
    for rx, fmt in MARKERS:
        for m in rx.finditer(s):
            keep.append(fmt.format(sub_id(m.group(1)) if m.group(1).startswith("a") else m.group(1)))
    keep += [p for p in PROMPTS if p in s]
    keep.append(H(s))
    return " ".join(keep)


# Invented labels, in spawn order. The real ones named this repo's internals.
LABELS = iter(
    ["Audit the auth flow", "Review retry logic", "Check DB migrations", "Scan deps for CVEs", "Review docs and CI"]
)
labels = {}
BASH = ["go test ./...", "rg -n retry src/", "go vet ./...", "git log --oneline -20", "ls src/"]


def args(c, i):
    try:
        a = json.loads(c.get("arguments") or "{}")
    except ValueError:
        a = {}
    n = c["name"]
    if n == "Agent":
        d = labels.setdefault(c["id"], next(LABELS, "Review the code"))
        out = {"description": d, "subagent_type": a.get("subagent_type", "general-purpose")}
        if a.get("run_in_background"):
            out["run_in_background"] = True
        return json.dumps(out)
    if n == "Bash":
        out = {"command": BASH[int(H(c["id"]), 16) % len(BASH)]}
        if a.get("run_in_background"):
            out["run_in_background"] = True
        return json.dumps(out)
    if n == "Read":
        return json.dumps({"file_path": "src/retry/handler.go"})
    if n == "Skill":
        return json.dumps({"skill": "code-review"})
    return "{}"


def calls(cs):
    out = []
    for i, c in enumerate(cs or []):
        out.append({"id": tool_id(c["id"]) if c.get("id") else "", "name": c["name"], "arguments": args(c, i)})
    return out or None


INF_KEEP = [
    "model",
    "maxTokens",
    "thinkingType",
    "agentRole",
    "isAction",
    "stream",
    "effort",
    "servedModel",
    "promptTokens",
    "totalTokens",
    "inputTokens",
    "cacheReadTokens",
    "cacheWriteTokens",
    "outputTokens",
    "completionTokens",
    "reasoningTokens",
    "finishReason",
    "presentKinds",
    "streamedResponse",
    "responseThinking",
    "historyThinking",
]
TOP_KEEP = [
    "seq",
    "at",
    "direction",
    "phase",
    "requestId",
    "httpMethod",
    "httpPath",
    "statusCode",
    "durationMs",
    "ttfbMs",
    "bytesUp",
    "bytesDown",
    "error",
    "tunnel",
    "tunnelReason",
    "client",
]

prev, delta, seen = {}, {}, {}  # agent -> last full scrubbed history, and the delta kept for it


def inference(i, agent, request):
    o = {k: i[k] for k in INF_KEEP if k in i}
    full = [
        {
            k: v
            for k, v in {
                "role": m.get("role"),
                "content": text(m.get("content")),
                "contentBytes": m.get("contentBytes"),
                "toolUseIds": ", ".join(tool_id(t) for t in (m.get("toolUseIds") or "").split(", ") if t) or None,
                "toolCallNames": m.get("toolCallNames"),
            }.items()
            if v
        }
        for m in i.get("messages") or []
    ]
    if request:
        # Keep only what is new since this agent's last request: a history is re-sent
        # whole every turn. An unchanged history is a retry, so it keeps the last delta.
        p = prev.get(agent, [])
        k = 0
        while k < min(len(p), len(full)) and p[k] == full[k]:
            k += 1
        d = delta.get(agent, full) if full == p else full[k:]
        prev[agent], delta[agent] = full, d
        o["messages"] = d
    if i.get("tools"):
        o["toolCount"] = len(i["tools"])  # the timeline reads only that a manifest is there
    if i.get("toolCalls"):
        o["toolCalls"] = calls(i["toolCalls"])
    if i.get("toolResults"):
        # Each result once per agent: the timeline reads a tool's end from the first request carrying it.
        got = seen.setdefault(agent, set())
        fresh = [r for r in i["toolResults"] if r.get("toolUseId") not in got]
        got.update(r.get("toolUseId") for r in fresh)
        o["toolResults"] = [
            {
                k: v
                for k, v in {
                    "toolUseId": tool_id(r["toolUseId"]) if r.get("toolUseId") else "",
                    "content": text(r.get("content")),
                    "isError": r.get("isError"),
                }.items()
                if v
            }
            for r in fresh
        ] or None
        if not o["toolResults"]:
            del o["toolResults"]
    if i.get("completion"):
        o["completion"] = text(i["completion"])
    return o


events = []
for e in view["events"]:
    # Only the model host and GitHub: the Go-module fetches crowd the host rows.
    if not is_llm(e.get("host")) and "github.com" not in (e.get("host") or ""):
        continue
    o = {k: e[k] for k in TOP_KEEP if k in e}
    o["host"] = host(e.get("host"))
    if e.get("requestedHost"):
        o["requestedHost"] = host(e["requestedHost"])
    if e.get("subagent"):
        o["subagent"] = sub_id(e["subagent"])
    if e.get("parentSubagent"):
        o["parentSubagent"] = sub_id(e["parentSubagent"])
    if e.get("plugins"):
        o["plugins"] = {"cost": e["plugins"]["cost"]} if "cost" in e["plugins"] else None
        if not o["plugins"]:
            del o["plugins"]
    if e.get("inference"):
        o["inference"] = inference(e["inference"], o.get("subagent", "main"), e["phase"] == "request")
    if o.get("error"):
        o["error"] = {k: v for k, v in o["error"].items() if k != "message"}
    events.append(o)

blob = json.dumps({"id": "replay", "events": events}, separators=(",", ":"))
# Self-check: nothing from the source machine survives.
bad = {"/Users/", "/home/", os.path.expanduser("~"), getpass.getuser()}
bad |= {h.split(":")[0] for h in LLM}
for b in bad:
    assert b not in blob, b
with gzip.open(dst, "wt") as f:
    f.write(blob)
print(len(events), "events,", len(blob), "bytes before gzip, labels:", list(labels.values()))
