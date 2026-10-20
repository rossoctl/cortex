#!/usr/bin/env python3
"""integration-contextguru-litellm.py — live integration test of the context-guru plugin.

Runs the REAL cortex binary (built with context-guru) as a TLS-bridging forward proxy in front of a
REAL LiteLLM gateway, drives Anthropic Messages, OpenAI Chat Completions and OpenAI Responses
traffic through it on cheap models, and checks what reaches the session API and the log.

Plan: docs/superpowers/plans/2026-10-13-contextguru-litellm-integration-test-plan.md

Scenarios (ids match the plan):
  C1  config reaches the plugin        /v1/pipeline shows the context-guru config as written
  C2  bad engine config is refused     a retired model.api_key fails startup with the hint
  D*  dialect x model matrix           200 through the proxy; engine ran; request compacted
  K1  cost on the response row         a priced cost record on every response event
  K2  savings on the response row      a context-guru saving on the record (expected: absent)
  A1  keep-alive ping (Anthropic)      one ping, status 200, cache read > 0, no re-entry
  A2  keep-alive with summary          cache_aware_summarizer substitutes for the ping
                                       (expected: not reachable in Cortex)
  S*  summarize at 90% of the window   one Anthropic session grown turn by turn past 0.9 x the
                                       window: the summary is commissioned, resolves, and is
                                       spliced in (S1 window, S2 commissioned, S3 resolved,
                                       S4 spliced, S5 recall). The window is set small with
                                       context_window.windows (--s-window, default 60000) so a
                                       run costs cents, not the ~$15 of filling a real 1M window.

Isolation: HOME is a temp dir, every listener is on a free 127.0.0.1 port in 48700-48799, the
cost ledger and session archive are off, and only PIDs this script started are killed.

Usage:
  LITELLM_API_KEY=sk-... python3 -I scripts/dev/integration-contextguru-litellm.py [--keep] [--only D,A,S] [--s-window N] [--shared]
Needs: go, python3 on PATH. Spend: a few cents on haiku / gpt-5-mini / gpt-5.3-codex.
"""
import argparse, json, os, re, shutil, signal, socket, ssl, subprocess, sys, tempfile, time
import urllib.request, urllib.error

UPSTREAM = os.environ.get("LITELLM_BASE", "https://ete-litellm.ai-models.vpc.res.ibm.com")
REPO = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
IDLE_S = 8  # keep_alive.idle_seconds

TOKENS = {}  # matrix id -> per-response billed tokens and cost
RESULTS = []  # (id, status, detail) where status in PASS/FAIL/GAP/INFO


def rec(sid, status, detail):
    RESULTS.append((sid, status, detail))
    print(f"{status:4} {sid:28} {detail}", flush=True)


# ---------------------------------------------------------------- environment

def free_ports(n, lo=48700, hi=48800):
    out = []
    for p in range(lo, hi):
        s = socket.socket()
        try:
            s.bind(("127.0.0.1", p)); out.append(p)
        except OSError:
            pass
        finally:
            s.close()
        if len(out) == n:
            return out
    sys.exit("not enough free ports")


def build(work):
    tags = subprocess.check_output(["go", "-C", os.path.join(REPO, "scripts/profile-tags"), "run", ".", "full"], text=True).strip()
    tags += ",include_plugin_contextguru"
    env = dict(os.environ, GOWORK="off")
    subprocess.check_call(["go", "build", "-tags", tags, "-o", os.path.join(work, "cortex"), "."],
                          cwd=os.path.join(REPO, "cmd/cortex"), env=env)
    return os.path.join(work, "cortex")


def config_yaml(ports, work, engine_yaml, extra_plugin_cfg="", pricing=""):
    fwd, sess, health, stats, trans = ports
    return f"""mode: proxy-sidecar
{pricing}
listener:
  roles: [forward]
  bind_loopback_only: true
  forward_proxy_addr: 127.0.0.1:{fwd}
  session_api_addr: 127.0.0.1:{sess}
  health_addr: 127.0.0.1:{health}
  transparent_proxy_addr: 127.0.0.1:{trans}
stats:
  address: 127.0.0.1:{stats}
cost_ledger:
  enabled: false
tls_bridge:
  mode: enabled
  ca_dir: {work}/ca
  generate_ca: true
  ports: [443]
session:
  archive:
    enabled: false
  id_headers:
    - "X-Session-Id"
pipeline:
  outbound:
    plugins:
      - name: inference-parser
      - name: context-guru
        config:
{extra_plugin_cfg}          engine:
{engine_yaml}
          keep_alive:
            enabled: true
            idle_seconds: {IDLE_S}
            max_pings: 1
            min_prefix_tokens: 1
"""


class Cortex:
    def __init__(self, binary, work, name, cfg):
        self.work, self.name = work, name
        self.cfg_path = os.path.join(work, f"{name}.yaml")
        self.log_path = os.path.join(work, f"{name}.log")
        with open(self.cfg_path, "w") as f:
            f.write(cfg)
        home = os.path.join(work, "home"); os.makedirs(home, exist_ok=True)
        env = {k: v for k, v in os.environ.items() if k.lower() not in ("http_proxy", "https_proxy", "all_proxy", "no_proxy")}
        env.update(HOME=home, LOG_LEVEL="debug")
        self.proc = subprocess.Popen([binary, "--config", self.cfg_path], cwd=work, env=env,
                                     stdout=open(self.log_path, "w"), stderr=subprocess.STDOUT)

    def log(self):
        with open(self.log_path) as f:
            return f.read()

    def stop(self):
        if self.proc.poll() is None:
            self.proc.send_signal(signal.SIGTERM)
            try:
                self.proc.wait(10)
            except subprocess.TimeoutExpired:
                self.proc.kill()


def local_get(port, path):
    op = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with op.open(f"http://127.0.0.1:{port}{path}", timeout=30) as r:
        return json.loads(r.read())


def wait_healthy(cx, health_port, secs=30):
    op = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    end = time.time() + secs
    while time.time() < end:
        if cx.proc.poll() is not None:
            return False
        try:
            op.open(f"http://127.0.0.1:{health_port}/healthz", timeout=2).read()
            return True
        except Exception:
            time.sleep(0.2)
    return False


# ---------------------------------------------------------------- traffic

def big_log():
    lines = []
    for i in range(220):
        lines.append(f"2026-10-13T10:{i//60:02d}:{i%60:02d}Z INFO  worker[{i%4}] processed batch {i} "
                     f"in {100 + (i*7)%50}ms status=ok queue_depth={i%9} retries=0")
    return "\n".join(lines)


def big_json():
    rows = [{"id": i, "name": f"service-{i}", "region": ["us-east", "eu-de", "jp-tok"][i % 3],
             "status": "healthy" if i % 7 else "degraded", "cpu": round(0.1 + (i % 10) / 13, 3),
             "owner": f"team-{i % 5}"} for i in range(90)]
    return json.dumps(rows, indent=2)


HANDBOOK = "\n".join(
    f"Rule {i}: When working in module m{i % 37}, prefer explicit error handling, keep functions short, "
    f"document exported identifiers, and run the module's own tests before reporting a change as done."
    for i in range(260))  # ~6k tokens: above Haiku 4.5's 4096-token cache minimum

SYSTEM = "You are a terse assistant. Answer in one short sentence.\n\n" + HANDBOOK
Q1 = "Read the worker log and the service inventory. In one sentence: is anything unhealthy?"
Q2 = "And in one sentence: which region has the most services?"
TOOLS_A = [{"name": "read_file", "description": "Read a file", "input_schema": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}},
           {"name": "list_services", "description": "List services", "input_schema": {"type": "object", "properties": {}}}]
TOOLS_CC = [{"type": "function", "function": {"name": t["name"], "description": t["description"], "parameters": t["input_schema"]}} for t in TOOLS_A]
TOOLS_R = [{"type": "function", "name": t["name"], "description": t["description"], "parameters": t["input_schema"]} for t in TOOLS_A]


def anthropic_body(model, turn2, stream):
    log, inv = big_log(), big_json()
    msgs = [
        {"role": "user", "content": "Check the system."},
        {"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_1", "name": "read_file", "input": {"path": "worker.log"}}]},
        {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_1", "content": log}]},
        {"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_2", "name": "read_file", "input": {"path": "worker.log"}}]},
        {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_2", "content": log}]},
        {"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_3", "name": "list_services", "input": {}}]},
        {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_3", "content": inv}]},
        {"role": "assistant", "content": "I have the log and the inventory."},
        {"role": "user", "content": Q1},
    ]
    if turn2:
        msgs += [{"role": "assistant", "content": "Some services are degraded; the log is clean."}, {"role": "user", "content": Q2}]
    b = {"model": model, "max_tokens": 64, "tools": TOOLS_A,
         "system": [{"type": "text", "text": SYSTEM, "cache_control": {"type": "ephemeral"}}], "messages": msgs}
    if stream:
        b["stream"] = True
    return b


def chat_body(model, turn2, stream):
    log, inv = big_log(), big_json()
    def call(i, name, args):
        return {"role": "assistant", "content": None, "tool_calls": [{"id": f"call_{i}", "type": "function", "function": {"name": name, "arguments": json.dumps(args)}}]}
    msgs = [
        {"role": "system", "content": SYSTEM},
        {"role": "user", "content": "Check the system."},
        call(1, "read_file", {"path": "worker.log"}), {"role": "tool", "tool_call_id": "call_1", "content": log},
        call(2, "read_file", {"path": "worker.log"}), {"role": "tool", "tool_call_id": "call_2", "content": log},
        call(3, "list_services", {}), {"role": "tool", "tool_call_id": "call_3", "content": inv},
        {"role": "assistant", "content": "I have the log and the inventory."},
        {"role": "user", "content": Q1},
    ]
    if turn2:
        msgs += [{"role": "assistant", "content": "Some services are degraded; the log is clean."}, {"role": "user", "content": Q2}]
    b = {"model": model, "messages": msgs, "tools": TOOLS_CC, "max_completion_tokens": 2000}
    if stream:
        b["stream"] = True; b["stream_options"] = {"include_usage": True}
    return b


def responses_body(model, turn2, stream):
    log, inv = big_log(), big_json()
    def call(i, name, args):
        return {"type": "function_call", "call_id": f"call_{i}", "name": name, "arguments": json.dumps(args)}
    def out(i, o):
        return {"type": "function_call_output", "call_id": f"call_{i}", "output": o}
    items = [
        {"role": "user", "content": "Check the system."},
        call(1, "read_file", {"path": "worker.log"}), out(1, log),
        call(2, "read_file", {"path": "worker.log"}), out(2, log),
        call(3, "list_services", {}), out(3, inv),
        {"role": "assistant", "content": "I have the log and the inventory."},
        {"role": "user", "content": Q1},
    ]
    if turn2:
        items += [{"role": "assistant", "content": "Some services are degraded; the log is clean."}, {"role": "user", "content": Q2}]
    b = {"model": model, "instructions": SYSTEM, "input": items, "tools": TOOLS_R, "max_output_tokens": 2000, "store": False}
    if stream:
        b["stream"] = True
    return b


MATRIX = [  # id, path, model, builder, stream
    ("D1-anthropic-haiku", "/v1/messages", "claude-haiku-4-5-20251001", anthropic_body, False),
    ("D2-anthropic-haiku-stream", "/v1/messages", "claude-haiku-4-5-20251001", anthropic_body, True),
    ("D3-chat-gpt5mini", "/v1/chat/completions", "azure/gpt-5-mini", chat_body, False),
    ("D4-chat-codex", "/v1/chat/completions", "azure/gpt-5.3-codex", chat_body, False),
    ("D5-chat-haiku", "/v1/chat/completions", "claude-haiku-4-5-20251001", chat_body, False),
    ("D6-chat-gpt5mini-stream", "/v1/chat/completions", "azure/gpt-5-mini", chat_body, True),
    ("D7-responses-gpt5mini", "/v1/responses", "azure/gpt-5-mini", responses_body, False),
    ("D8-responses-codex", "/v1/responses", "azure/gpt-5.3-codex", responses_body, False),
    ("D9-responses-codex-stream", "/v1/responses", "azure/gpt-5.3-codex", responses_body, True),
]


def send(fwd_port, ca, path, body, session, key):
    ctx = ssl.create_default_context(cafile=ca)
    op = urllib.request.build_opener(urllib.request.ProxyHandler({"https": f"http://127.0.0.1:{fwd_port}"}),
                                     urllib.request.HTTPSHandler(context=ctx))
    data = json.dumps(body).encode()
    hdr = {"Content-Type": "application/json", "Authorization": f"Bearer {key}", "X-Session-Id": session,
           "User-Agent": "cg-integration/1"}
    if path == "/v1/messages":
        hdr["anthropic-version"] = "2023-06-01"
    req = urllib.request.Request(UPSTREAM + path, data=data, headers=hdr, method="POST")
    t0 = time.time()
    try:
        with op.open(req, timeout=180) as r:
            raw = r.read(); status = r.status; headers = dict(r.headers.items())
    except urllib.error.HTTPError as e:
        raw = e.read(); status = e.code; headers = dict(e.headers.items())
    return status, headers, raw.decode("utf-8", "replace"), time.time() - t0, len(data)


# ---------------------------------------------------------------- checks

def session_events(sess_port, sid):
    try:
        return local_get(sess_port, f"/v1/sessions/{sid}?limit=200").get("events", [])
    except Exception:
        return []


def find_cost(ev):
    plugins = ev.get("plugins") or {}
    for k in ("cost", "inference-parser"):
        v = plugins.get(k)
        if isinstance(v, dict) and ("costUsd" in v or "cost_usd" in v or "avoided" in v or "priced" in v or "usd" in json.dumps(v).lower()):
            return k, v
    return None, None


def answer_text(raw, stream):
    """The model's visible text, from a JSON body or an SSE stream of any of the three dialects."""
    if not stream:
        try:
            d = json.loads(raw)
        except ValueError:
            return ""
        if "content" in d:
            return " ".join(c.get("text", "") for c in d["content"] if c.get("type") == "text")
        if "choices" in d:
            return d["choices"][0]["message"].get("content") or ""
        return " ".join(c.get("text", "") for o in d.get("output", []) if o.get("type") == "message" for c in o.get("content", []))
    out = []
    for line in raw.splitlines():
        if not line.startswith("data:") or line.strip() == "data: [DONE]":
            continue
        try:
            d = json.loads(line[5:])
        except ValueError:
            continue
        if d.get("type") == "content_block_delta":
            out.append(d.get("delta", {}).get("text", ""))
        elif d.get("type") == "response.output_text.delta":
            out.append(d.get("delta", ""))
        elif d.get("choices"):
            out.append((d["choices"][0].get("delta") or {}).get("content") or "")
    return "".join(out)


def run_matrix(cx, ports, ca, key, work, only_ids, static_model="", preset="xhigh"):
    fwd, sess = ports[0], ports[1]
    for did, path, model, builder, stream in MATRIX:
        if only_ids and not any(did.startswith(o) for o in only_ids):
            continue
        sid = f"cgit-{did}-{os.getpid()}"
        rows = []
        for turn in (False, True):
            st, hd, body, dt, nbytes = send(fwd, ca, path, builder(model, turn, stream), sid, key)
            rows.append((st, hd, body, dt, nbytes))
            with open(os.path.join(work, f"{did}-turn{2 if turn else 1}.resp"), "w") as f:
                f.write(json.dumps({"status": st, "headers": hd}) + "\n" + body)
        ok = all(r[0] == 200 for r in rows)
        rec(did, "PASS" if ok else "FAIL",
            f"{model} {path}{' stream' if stream else ''}: status {[r[0] for r in rows]} in {sum(r[3] for r in rows):.1f}s"
            + ("" if ok else f" body={rows[-1][2][:300]!r}"))
        ans = answer_text(rows[0][2], stream)
        rec(did + "/answer", "PASS" if "degraded" in ans.lower() else "FAIL",
            f"turn-1 answer {'names' if 'degraded' in ans.lower() else 'does NOT name'} the degraded services: {ans[:110]!r}")
        time.sleep(0.5)
        log = cx.log()
        comp = [l for l in log.splitlines() if sid in l and "context-guru compacted request" in l]
        rew = [l for l in log.splitlines() if sid in l and "rewrote request body" in l]
        if comp:
            m = re.findall(r'pctSaved=(\S+)', comp[-1]); comps = re.findall(r'components="?\[([^\]]*)\]', comp[-1])
            rec(did + "/compact", "PASS", f"{len(comp)} compacted, last pctSaved={m[-1] if m else '?'} components=[{comps[-1] if comps else '?'}]")
        else:
            rec(did + "/compact", "FAIL" if preset != "off" else "INFO", "no 'context-guru compacted request' line for this session")
        evs = session_events(sess, sid)
        with open(os.path.join(work, f"{did}-session.json"), "w") as f:
            json.dump(evs, f, indent=1)
        resp = [e for e in evs if e.get("phase") == "response"]
        costs = [find_cost(e) for e in resp]
        priced = [c for k, c in costs if c is not None]
        if len(resp) >= 2 and len(priced) == len(resp):
            usd = [c.get("costUsd", c.get("cost_usd")) for c in priced]
            src = sorted({c.get("source", "?") for c in priced})
            rec(did + "/K1-cost", "PASS", f"cost record on {len(priced)}/{len(resp)} response rows, usd={usd} source={src}")
        elif stream and path != "/v1/messages" and not any("X-Litellm-Response-Cost" in r[1] for r in rows):
            rec(did + "/K1-cost", "GAP", f"cost on {len(priced)}/{len(resp)} rows: a streamed response carries no "
                "X-Litellm-Response-Cost header and the bundled rate table has no rate for " + model + " (fix: pricing: override, --pricing)")
        else:
            keys = sorted({k for e in resp for k in (e.get("plugins") or {})})
            rec(did + "/K1-cost", "FAIL", f"cost on {len(priced)}/{len(resp)} response rows; plugin keys={keys}")
        TOKENS[did] = [{"input": (e.get("inference") or {}).get("inputTokens"), "cacheRead": (e.get("inference") or {}).get("cacheReadTokens"),
                        "cacheWrite": (e.get("inference") or {}).get("cacheWriteTokens"), "output": (e.get("inference") or {}).get("outputTokens"),
                        "usd": (c or {}).get("cost_usd")} for e, (k, c) in zip(resp, costs)]
        av = [c.get("avoided") for c in priced if c.get("avoided")]
        cg_av = [a for a in av if any((s or {}).get("component") == "context-guru" for s in a)]
        rec(did + "/K2-savings", "PASS" if cg_av else "GAP",
            f"context-guru saving on {len(cg_av)}/{len(priced)} cost records" + ("" if cg_av else " (settle.savingComponents lists only tool-prune)"))
        calls = [l for l in log.splitlines() if sid in l and "cg.extract_llm.call" in l]
        if calls:
            f = dict(re.findall(r'(\w+)=("[^"]*"|\S+)', calls[-1]))
            rec(did + "/extract_llm-call", "INFO", f"{len(calls)} call(s); logged model={f.get('model')} (configured name — "
                f"the call runs on {model if path == '/v1/chat/completions' else static_model or 'nothing'} — "
                f"{'incoming' if path == '/v1/chat/completions' else 'static'} source, and cgModel has no Remodeler) input_tokens={f.get('input_tokens')} cost_usd={f.get('cost_usd')}")
        gates = sorted(set(re.findall(r'component=cache_aware_summarizer kind=offload verdict=(\w+)[^\n]*?(?:gates="([^"]*)")?\n', "\n".join(l for l in log.splitlines() if sid in l) + "\n")))
        rec(did + "/summarizer", "INFO", f"cache_aware_summarizer verdicts/gates: {gates}")
        cg_rows = [e for e in evs if "context-guru" in (e.get("plugins") or {})]
        rec(did + "/plugin-event", "INFO", f"context-guru plugin event on {len(cg_rows)}/{len(evs)} rows")


def run_keepalive(cx, ports, work, ran):
    sess = ports[1]
    anth = [f"cgit-{d}-{os.getpid()}" for d, path, *_ in MATRIX if path == "/v1/messages" and d in ran]
    deadline = time.time() + IDLE_S + 40
    pings = []
    while time.time() < deadline:
        pings = [l for l in cx.log().splitlines() if "context-guru keep-alive ping" in l]
        if all(any(f"session={sid} " in l for l in pings) for sid in anth):
            break
        time.sleep(1)
    for sid in anth:
        mine = [l for l in pings if f"session={sid} " in l]
        name = "A1-ping/" + sid.split("-", 1)[1].rsplit("-", 1)[0]
        if len(mine) != 1:
            rec(name, "FAIL", f"{len(mine)} pings (want exactly 1, max_pings: 1)")
            continue
        f = dict(re.findall(r'(\w+)=("[^"]*"|\S+)', mine[0]))
        good = f.get("status") == "200" and int(f.get("cacheRead", "0")) > 0
        rec(name, "PASS" if good else "FAIL",
            f"status={f.get('status')} cacheRead={f.get('cacheRead')} cacheWrite={f.get('cacheWrite')} output={f.get('output')} costUSD={f.get('costUSD')} ms={f.get('ms')}")
    other = [l for l in pings if not any(f"session={sid} " in l for sid in anth)]
    rec("A1-openai-not-pinged", "PASS" if not other else "FAIL",
        f"{len(other)} pings on OpenAI-wire sessions (none eligible: OpenAI needs GPT-5.6+ on /v1/responses)")
    total_req, nsess = 0, 0
    for s in local_get(sess, "/v1/sessions").get("sessions", []):
        nsess += 1
        total_req += sum(1 for e in session_events(sess, s["id"]) if e.get("phase") == "request" and (e.get("httpPath") or "").startswith("/v1/"))
    want = 2 * len(ran)
    rec("A1-no-reentry", "PASS" if total_req == want else "FAIL",
        f"{total_req} inference request rows across {nsess} sessions; want {want} (2 per matrix row; a re-entered ping would add one)")
    rec("A1-ping-spend", "INFO", "ping spend appears only in the log line; no session row and no cost record (by design until a Cortex CostRecorder)")
    log = cx.log()
    ok = [l for l in log.splitlines() if "keep-alive ping (via cache_aware_summarizer)" in l]
    notoff = re.findall(r'substitute not offered[^\n]*reason=(\S+)', log)
    rec("A2-keepalive-summary", "PASS" if ok else "GAP",
        f"summary substitutes={len(ok)}; 'not offered' reasons={sorted(set(notoff))} — Cortex's cgModel has no "
        "MessagesModel/PrefixAsker and Cortex passes no context window, so cache_aware_summarizer never registers a candidate")


def run_config_checks(binary, work, ports, cx, a_preset):
    pipe = local_get(ports[1], "/v1/pipeline")
    with open(os.path.join(work, "pipeline.json"), "w") as f:
        json.dump(pipe, f, indent=1)
    cg = [p for p in pipe.get("outbound", []) if p.get("name") == "context-guru"]
    if not cg:
        rec("C1-config-visible", "FAIL", "context-guru not in /v1/pipeline outbound")
    else:
        cfg = cg[0].get("config") or {}
        s = json.dumps(cfg)
        ok = a_preset in s and cfg.get("keep_alive", {}).get("idle_seconds") == IDLE_S
        rec("C1-config-visible", "PASS" if ok else "FAIL", f"config on /v1/pipeline: {s[:240]}")
    log = cx.log()
    ka = [l for l in log.splitlines() if "keep-alive enabled" in l]
    rec("C1-keepalive-policy", "PASS" if ka and f"idle={IDLE_S}s" in ka[0] else "FAIL", (ka[0][-200:] if ka else "no keep-alive enabled line"))
    conf = [l for l in log.splitlines() if "context-guru configured" in l]
    rec("C1-engine-loaded", "PASS" if conf else "FAIL", conf[0][-160:] if conf else "no 'context-guru configured' line")


def run_bad_config(binary, work):
    ports = free_ports(5, 48760, 48800)
    eng = """            preset: xhigh
            components:
              extract_llm:
                model:
                  source: incoming
                  api_key: sk-should-be-refused"""
    cx = Cortex(binary, work, "badcfg", config_yaml(ports, work + "/bad", eng))
    try:
        cx.proc.wait(30)
    except subprocess.TimeoutExpired:
        cx.stop(); rec("C2-retired-key-refused", "FAIL", "cortex kept running with model.api_key in engine config"); return
    log = cx.log()
    hint = "model.api_key is no longer a component setting" in log
    rec("C2-retired-key-refused", "PASS" if hint else "FAIL",
        f"exit={cx.proc.returncode}; hint in log={hint}; " + (re.findall(r'.*api_key.*', log) or ["?"])[-1][:240])


S_MODEL = "claude-haiku-4-5-20251001"
S_WORDS = ("ledger settlement invoice refund latency replica quorum shard cursor batch window token "
           "cache prefix tenant region gateway policy audit drift budget quota schema index vector "
           "stream frame buffer socket handshake cipher bundle anchor rotation lease epoch commit").split()


def s_part(i, rnd, n_chars):
    """One unique tool output: filler no reducer can shrink, with one fact to recall later."""
    lines = [f"part {i:03d}"]
    while sum(len(l) + 1 for l in lines) < n_chars:
        lines.append(" ".join(rnd.choice(S_WORDS) for _ in range(rnd.randint(10, 18))) + f" ({rnd.randint(1, 99999)}).")
    lines.insert(len(lines) // 2, f"Fact {i:03d}: the reference code of part {i:03d} is RC-{rnd.randint(10000, 99999)}.")
    return "\n".join(lines)


SHARED_CFG = os.path.expanduser("~/.cortex/config.yaml")


class SharedCortex:
    """The laptop's running Cortex, used in place of one this script starts (--shared). Nothing
    is built, started or stopped; log() reads ~/.cortex/proxy.log from where the run began, so
    earlier traffic cannot satisfy a check."""

    def __init__(self):
        self.log_path = os.path.expanduser("~/.cortex/proxy.log")
        self.offset = os.path.getsize(self.log_path)
        self.proc = None

    def log(self):
        with open(self.log_path, errors="replace") as f:
            f.seek(self.offset)
            return f.read()

    def stop(self):
        pass


def shared_ports():
    """(forward, session API, health, stats, transparent) from the laptop config."""
    cfg = open(SHARED_CFG).read()
    def port(key):
        m = re.search(rf"^\s*{key}:\s*\"?[\w.]*:(\d+)", cfg, re.M)
        return int(m.group(1)) if m else 0
    stats = re.search(r"^stats:\s*\n\s+address:\s*\"?[\w.]*:(\d+)", cfg, re.M)
    return (port("forward_proxy_addr"), port("session_api_addr"), port("health_addr"),
            int(stats.group(1)) if stats else 0, port("transparent_proxy_addr"))


def reload_count(stats_port):
    try:
        s = local_get(stats_port, "/reload/status")
        return s.get("reloads_ok", 0), s.get("reloads_failed", 0)
    except Exception:
        return None


def shared_window_override(stats_port, window):
    """Adds a context_window block for S_MODEL to the laptop config's context-guru entry, waits
    for the hot-reload, and returns the original text to restore. Only S_MODEL's window changes;
    every other model keeps its own."""
    orig = open(SHARED_CFG).read()
    m = re.search(r"^(\s*)- name: context-guru\n(?:\1  .*\n)*?(\1    )engine:\n(?:\2  .*\n)+", orig, re.M)
    if not m:
        return None, "no context-guru entry with an engine: block in " + SHARED_CFG
    ind = m.group(2)
    block = (f"{ind}# TEMPORARY: integration test S (--shared); removed when the run ends.\n"
             f"{ind}context_window:\n{ind}  windows:\n{ind}    \"{S_MODEL}\": {window}\n")
    before = reload_count(stats_port)
    with open(SHARED_CFG, "w") as f:
        f.write(orig[:m.end()] + block + orig[m.end():])
    for _ in range(30):
        time.sleep(1)
        now = reload_count(stats_port)
        if now and before and now != before:
            if now[1] > before[1]:
                with open(SHARED_CFG, "w") as f:
                    f.write(orig)
                return None, "the shared Cortex refused the override; config restored"
            return orig, ""
    with open(SHARED_CFG, "w") as f:
        f.write(orig)
    return None, "no reload seen within 30 s; config restored"


def run_summarize(binary, work, key, window, shared=False):
    """S*: grow one Anthropic session past 0.9 x window and watch cache_aware_summarizer act.

    Its own Cortex, so the window override and the debug log belong to this scenario alone. The
    summary is two-turn: the turn whose PREVIOUS response billed >= 0.9 x window commissions a
    detached summary and forwards unchanged; a later turn finds it and splices it in."""
    import random
    rnd = random.Random(7)
    sid = f"cgit-S-{os.getpid()}"
    restore = None
    if shared:
        ports = shared_ports()
        cx = SharedCortex()
        restore, why = shared_window_override(ports[3], window)
        if restore is None:
            rec("S-start", "FAIL", why)
            return
    else:
        ports = free_ports(5, lo=48750)
        extra = (f"          context_window:\n            model_info: \"off\"\n"
                 f"            windows:\n              \"{S_MODEL}\": {window}\n")
        cx = Cortex(binary, work, "cortex-s", config_yaml(ports, work, '            preset: "xhigh"', extra))
    try:
        if not shared and not wait_healthy(cx, ports[2]):
            rec("S-start", "FAIL", "cortex did not become healthy: " + cx.log()[-400:].replace("\n", " "))
            return
        ca = os.path.expanduser("~/.cortex/ca/ca.crt") if shared else os.path.join(work, "ca", "ca.crt")
        for _ in range(50):
            if os.path.exists(ca):
                break
            time.sleep(0.1)
        part_chars = max(4000, window // 6)  # ~window/24 tokens per turn: crosses 0.9 in ~22 turns
        msgs = [{"role": "user", "content": "Read every part file with read_file, one call each, until I stop you."}]
        first_fact = None
        billed, peak, after_splice, resolved_at = [], 0, None, None
        max_turns = 60
        for turn in range(1, max_turns + 1):
            part = s_part(turn, rnd, part_chars)
            if turn == 1:
                first_fact = re.search(r"RC-\d+", part.split("Fact 001")[1]).group(0)
            msgs += [{"role": "assistant", "content": [{"type": "tool_use", "id": f"toolu_{turn}", "name": "read_file",
                                                         "input": {"path": f"part-{turn:03d}.txt"}}]},
                     {"role": "user", "content": [{"type": "tool_result", "tool_use_id": f"toolu_{turn}", "content": part}]}]
            body = {"model": S_MODEL, "max_tokens": 16, "tools": TOOLS_A,
                    "system": [{"type": "text", "text": SYSTEM, "cache_control": {"type": "ephemeral"}}], "messages": msgs}
            status, _, raw, _, _ = send(ports[0], ca, "/v1/messages", body, sid, key)
            if status != 200:
                rec("S-traffic", "FAIL", f"turn {turn}: HTTP {status} {raw[:200]}")
                return
            u = json.loads(raw).get("usage", {})
            b = u.get("input_tokens", 0) + u.get("cache_read_input_tokens", 0) + u.get("cache_creation_input_tokens", 0)
            billed.append(b); peak = max(peak, b)
            log = cx.log()
            if resolved_at is None and "detached summary resolved" in log:
                resolved_at = turn
            if resolved_at is not None and turn > resolved_at and b < 0.9 * window:
                after_splice = (turn, b)
                break
        log = cx.log()
        mine = [l for l in log.splitlines() if sid in l or "cache_aware" in l]
        if shared:
            # The laptop logs at INFO, and the window is only on cg.cache_boundary's DEBUG line.
            rec("S1-window", "INFO", "not checkable at INFO level; S2 firing at 0.9 x the window shows it was used")
        else:
            rec("S1-window", "PASS" if f"ctx_window={window}" in log else "FAIL",
                f"engine saw ctx_window={window}" if f"ctx_window={window}" in log else "no cg.cache_boundary line with the configured window")
        crossed = next((i + 1 for i, b in enumerate(billed) if b >= 0.9 * window), None)
        comm = [l for l in mine if "commissioned a detached summary" in l]
        declines = sorted(set(re.findall(r"cache_aware_summarizer[^\n]*?(?:reason|gate)=(\S+)", "\n".join(mine))))
        rec("S2-commissioned", "PASS" if comm else "FAIL",
            f"billed crossed 0.9 x {window} on turn {crossed}; commissions={len(comm)}; decline reasons={declines}")
        rec("S3-resolved", "PASS" if resolved_at else "FAIL",
            f"detached summary resolved by turn {resolved_at}" if resolved_at else "no 'detached summary resolved' line")
        spliced = [l for l in log.splitlines() if "context-guru compacted request" in l and sid in l and "cache_aware_summarizer" in l]
        if after_splice and spliced:
            rec("S4-spliced", "PASS", f"turn {after_splice[0]} billed {after_splice[1]} (< 0.9 x {window}; peak {peak}); "
                + spliced[-1].split("tokensBefore=", 1)[-1][:120])
        else:
            rec("S4-spliced", "FAIL", f"no splice seen in {len(billed)} turns; peak billed {peak}; "
                f"compacted lines with cache_aware_summarizer={len(spliced)}")
        if after_splice:
            msgs.append({"role": "assistant", "content": "Done reading."})
            msgs.append({"role": "user", "content": "What is the reference code of part 001? Reply with the code only."})
            body = {"model": S_MODEL, "max_tokens": 32, "tools": TOOLS_A,
                    "system": [{"type": "text", "text": SYSTEM}], "messages": msgs}
            status, _, raw, _, _ = send(ports[0], ca, "/v1/messages", body, sid, key)
            ans = answer_text(raw, False) if status == 200 else f"HTTP {status}"
            rec("S5-recall", "INFO", f"want {first_fact}, got {ans.strip()[:60]!r} — a summary may drop filler facts")
        with open(os.path.join(work, "summarize-billed.json"), "w") as f:
            json.dump({"window": window, "billed": billed}, f)
    finally:
        cx.stop()
        if restore is not None:
            with open(SHARED_CFG, "w") as f:
                f.write(restore)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--keep", action="store_true", help="keep the work dir")
    ap.add_argument("--only", default="", help="comma list of scenario prefixes: C,D,A,S or matrix ids like D1,D7")
    ap.add_argument("--preset", default="xhigh", help="engine preset (off = uncompacted baseline)")
    ap.add_argument("--pricing", action="store_true", help="add a pricing: override for the azure/gpt-5* models "
                    "(list rates, assumed) so streamed OpenAI rows can be priced without the gateway header")
    ap.add_argument("--s-window", type=int, default=60000, help="S*: the exact context window given to "
                    f"{S_MODEL} via context_window.windows; the summary fires past 0.9 x this")
    ap.add_argument("--shared", action="store_true", help="run D, K and S against the laptop's running "
                    "Cortex (~/.cortex) instead of starting one; C and A are skipped. S adds a temporary "
                    f"context_window override for {S_MODEL} and removes it when the run ends")
    ap.add_argument("--static-model", default="", help="also configure the plugin's static model: block with this model id")
    a = ap.parse_args()
    key = os.environ.get("LITELLM_API_KEY") or sys.exit("set LITELLM_API_KEY")
    only = [o for o in a.only.split(",") if o]
    want = lambda p: not only or any(o.startswith(p) for o in only)
    work = tempfile.mkdtemp(prefix="cg-litellm-")
    print(f"work dir: {work}", flush=True)
    if a.shared:
        try:
            run_shared(a, key, work, want, only)
        finally:
            finish(a, work)
        sys.exit(1 if any(r[1] == "FAIL" for r in RESULTS) else 0)
    binary = build(work); rec("build", "PASS", "cortex built (full + include_plugin_contextguru)")
    if want("C"):
        run_bad_config(binary, work)
    ports = free_ports(5)
    extra = ""
    if a.static_model:
        extra = (f"          model:\n            base_url: \"{UPSTREAM}\"\n            model: \"{a.static_model}\"\n"
                 f"            api_key: \"${{LITELLM_API_KEY}}\"\n            max_tokens: 1024\n")
    pricing = ""
    if a.pricing:
        host = UPSTREAM.split("://", 1)[1].split("/", 1)[0]
        pricing = (f"pricing:\n  endpoints:\n    - hosts: [\"{host}\"]\n      models:\n"
                   "        \"*gpt-5-mini*\": {input_cost_per_million: 0.25, cache_read_cost_per_million: 0.025, output_cost_per_million: 2.00}\n"
                   "        \"*gpt-5.3-codex*\": {input_cost_per_million: 1.75, cache_read_cost_per_million: 0.175, output_cost_per_million: 14.00}\n")
    cx = Cortex(binary, work, "cortex", config_yaml(ports, work, f"            preset: \"{a.preset}\"", extra, pricing))
    try:
        if not wait_healthy(cx, ports[2]):
            print(cx.log()[-3000:]); sys.exit("cortex did not become healthy")
        ca = os.path.join(work, "ca", "ca.crt")
        for _ in range(50):
            if os.path.exists(ca):
                break
            time.sleep(0.1)
        rec("start", "PASS", f"cortex healthy; forward :{ports[0]} session API :{ports[1]}; static model={a.static_model or 'none'}")
        if want("C"):
            run_config_checks(binary, work, ports, cx, a.preset)
        if want("D") or want("A"):
            dsel = [o for o in only if o.startswith("D") and len(o) > 1]
            ran = [d for d, *_ in MATRIX if not dsel or any(d.startswith(o) for o in dsel)]
            run_matrix(cx, ports, ca, key, work, dsel, a.static_model, a.preset)
        if want("A"):
            run_keepalive(cx, ports, work, ran)
        if want("S"):
            run_summarize(binary, work, key, a.s_window)
        log = cx.log()
        with open(os.path.join(work, "components-debug.txt"), "w") as f:
            f.write("\n".join(l for l in log.splitlines() if "context-guru component" in l or "cache_aware" in l))
    finally:
        cx.stop()
        finish(a, work)
    sys.exit(1 if any(r[1] == "FAIL" for r in RESULTS) else 0)


def finish(a, work):
    with open(os.path.join(work, "tokens.json"), "w") as f:
        json.dump({"preset": a.preset, "rows": TOKENS}, f, indent=1)
    print("\n==== summary ====")
    for r in RESULTS:
        print(f"{r[1]:4} {r[0]:28} {r[2][:160]}")
    fails = sum(1 for r in RESULTS if r[1] == "FAIL")
    print(f"\n{fails} FAIL, {sum(1 for r in RESULTS if r[1] == 'GAP')} GAP; artifacts in {work}")
    if not a.keep and fails == 0:
        shutil.rmtree(work, ignore_errors=True)


def run_shared(a, key, work, want, only):
    """--shared: the laptop's running Cortex instead of one this script starts. C needs a Cortex
    this script can start and break, and A needs keep_alive on with an 8 s idle, which the laptop
    config does not have, so both are skipped. D, K and S run, and every session they make shows
    in agentop observe under its cgit-* id."""
    ports = shared_ports()
    cx = SharedCortex()
    op = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        op.open(f"http://127.0.0.1:{ports[2]}/healthz", timeout=3).read()
    except Exception as e:
        rec("start", "FAIL", f"shared Cortex not healthy on :{ports[2]}: {e}")
        return
    ca = os.path.expanduser("~/.cortex/ca/ca.crt")
    rec("start", "PASS", f"shared Cortex; forward :{ports[0]} session API :{ports[1]}; C and A skipped")
    if want("D"):
        dsel = [o for o in only if o.startswith("D") and len(o) > 1]
        run_matrix(cx, ports, ca, key, work, dsel, "", a.preset)
    if want("S"):
        run_summarize(None, work, key, a.s_window, shared=True)


if __name__ == "__main__":
    main()
