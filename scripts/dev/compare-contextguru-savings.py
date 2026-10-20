#!/usr/bin/env python3
"""compare-contextguru-savings.py — one Claude Code session per arm, savings read from each side's API.

Checks that the savings context-guru computes reach Cortex correctly, by running the SAME prompt
schedule through three arms at the same time:

  A  cortex     claude -p -> HTTPS_PROXY -> cortex (context-guru, xhigh)  -> LiteLLM
  B  cg-proxy   claude -p -> ANTHROPIC_BASE_URL -> context-guru-proxy (xhigh, dashboard) -> LiteLLM
  C  control    claude -p -> HTTPS_PROXY -> cortex (no context-guru)      -> LiteLLM

Schedule: fill (Read fixture files until arm C's prompt context reaches --fill-frac x --window),
small (short questions), freeze (--freeze-s, longer than the 5-minute cache TTL, so keep-alive
pings), resume (short questions). Both engines get the same preset, window, keep-alive and rates.

A poller writes metrics.jsonl every --poll-s; each turn writes turns.jsonl; the end writes final
snapshots, checks.json and the plots (the `plot` subcommand re-draws them from a kept work dir).

Checks:
  P1  pass-through, per request (arm A)  each priced context-guru saving on a response row equals
                                          the plugin's own event on that request: tokens exactly,
                                          USD by compactionDraft's rule at the override rates
  P2  session total (arm A)               avoidedMicros on /v1/sessions = sum of the row savings
  X1  A vs B vs measured                  INFO: Cortex's avoided, the cg proxy's saved/net, and
                                          the measured C-A and C-B spend differences
  K1  keep-alive during the freeze        A and B each ping; the first request after the freeze
                                          reads from cache (C, without keep-alive, does not)

Isolation: temp HOME and CLAUDE_CONFIG_DIR per process, ports in 48700-48799, cost ledger and
session archive off. Nothing touches the laptop Cortex (:47600, ~/.cortex).

Usage:
  python3 -I scripts/dev/compare-contextguru-savings.py run --cg-repo ../context-guru [--smoke] [--keep]
  python3 -I scripts/dev/compare-contextguru-savings.py plot <work-dir>
The key is LITELLM_API_KEY, else read from --env-file (default: .env beside the main checkout).
"""
import argparse, concurrent.futures, importlib.util, json, os, random, re, shutil, subprocess, sys
import tempfile, threading, time, uuid

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.abspath(os.path.join(HERE, "..", ".."))
_spec = importlib.util.spec_from_file_location("cgit", os.path.join(HERE, "integration-contextguru-litellm.py"))
cgit = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(cgit)

UPSTREAM = cgit.UPSTREAM
HOST = UPSTREAM.split("://", 1)[1].split("/", 1)[0]
MODEL = "aws/claude-haiku-4-5"
# USD per million tokens: input, cache write (5m), cache read, output. Published rates, given to
# BOTH engines so a difference between arms is never a rate difference. --model picks one.
MODEL_RATES = {
    "aws/claude-haiku-4-5": {"input": 1.00, "cache_write": 1.25, "cache_read": 0.10, "output": 5.00},
    "claude-sonnet-4-6": {"input": 3.00, "cache_write": 3.75, "cache_read": 0.30, "output": 15.00},
}
RATES = MODEL_RATES[MODEL]
IDLE_S, MAX_PINGS, MIN_PREFIX = 270, 2, 80000  # the cg proxy's own keep-alive defaults
ARMS = ("A", "B", "C")
ARM_NAME = {"A": "cortex + context-guru", "B": "context-guru proxy", "C": "control (no compaction)"}

# xhigh's extract_llm block, verbatim from context-guru config/config.go, except the model: the
# preset names claude-haiku-4-5, which this gateway does not serve. A user block replaces the
# preset's block whole, so every key is restated.
EXTRACT_LLM = {
    "aggressiveness": "medium", "context": "recent", "context_messages": 2, "economic_gate": True,
    "fire_on": "pressure", "llm_every_n_requests": 1, "llm_max_per_request": 8,
    "llm_max_per_session": 0, "min_tokens": 3000, "model": {"model": MODEL, "source": "incoming"},
    "strategy": "code", "trigger": {"min_request_tokens": 3000},
}
ENGINE = {"preset": "xhigh", "components": {"extract_llm": EXTRACT_LLM}}


def set_model(model):
    """Point every model-dependent setting at model: the agent, extract_llm, and the rates."""
    global MODEL, RATES
    if model not in MODEL_RATES:
        sys.exit(f"no rates for {model}; known: {', '.join(MODEL_RATES)}")
    MODEL, RATES = model, MODEL_RATES[model]
    EXTRACT_LLM["model"] = {"model": model, "source": "incoming"}


def price_glob():
    """The Cortex pricing key for MODEL: its name without the gateway prefix, as a glob."""
    return "*" + MODEL.split("/", 1)[-1] + "*"

LOCK = threading.Lock()
PHASE = {"name": "start", "step": 0}


def jl(path, obj):
    with LOCK, open(path, "a") as f:
        f.write(json.dumps(obj) + "\n")


def api_key(env_file):
    k = os.environ.get("LITELLM_API_KEY", "")
    if k:
        return k
    if env_file is None:
        common = subprocess.check_output(["git", "-C", REPO, "rev-parse", "--path-format=absolute",
                                          "--git-common-dir"], text=True).strip()
        env_file = os.path.join(os.path.dirname(common), ".env")
    for line in open(env_file):
        m = re.match(r"\s*(?:export\s+)?LITELLM_API_KEY\s*=\s*['\"]?([^'\"\s]+)", line)
        if m:
            return m.group(1)
    sys.exit(f"LITELLM_API_KEY not set and not in {env_file}")


# ---------------------------------------------------------------- the three servers

def cortex_yaml(ports, work, name, with_cg):
    fwd, sess, health, stats, trans = ports
    plugins = "      - name: inference-parser\n"
    if with_cg:
        cfg = {"engine": ENGINE,
               "keep_alive": {"enabled": True, "idle_seconds": IDLE_S, "max_pings": MAX_PINGS,
                              "min_prefix_tokens": MIN_PREFIX},
               "context_window": {"model_info": "off", "windows": {MODEL: WINDOW}}}
        # JSON is YAML: one flow mapping keeps the nesting exact without a YAML library.
        plugins += f"      - name: context-guru\n        config: {json.dumps(cfg)}\n"
    r = RATES
    return f"""mode: proxy-sidecar
pricing:
  endpoints:
    - hosts: ["{HOST}"]
      models:
        "{price_glob()}": {{input_cost_per_million: {r['input']}, cache_write_cost_per_million: {r['cache_write']}, cache_read_cost_per_million: {r['cache_read']}, output_cost_per_million: {r['output']}}}
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
  ca_dir: {work}/ca-{name}
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
{plugins}"""


def build_cg(cg_repo, work):
    out = os.path.join(work, "context-guru-proxy")
    subprocess.check_call(["go", "build", "-o", out, "./cmd/context-guru-proxy"], cwd=cg_repo,
                          env=dict(os.environ, GOWORK="off"))
    return out


class CGProxy:
    def __init__(self, binary, work, port):
        self.port = port
        self.log_path = os.path.join(work, "cg-proxy.log")
        cfg = dict(ENGINE, cache={"keepalive": True, "keepalive_idle_seconds": IDLE_S,
                                  "keepalive_max_pings": MAX_PINGS, "keepalive_min_prefix_tokens": MIN_PREFIX})
        cfg_path, prices = os.path.join(work, "cg-proxy.yaml"), os.path.join(work, "cg-prices.yaml")
        with open(cfg_path, "w") as f:
            json.dump(cfg, f, indent=1)
        r = RATES
        with open(prices, "w") as f:
            json.dump({"models": [{"match": MODEL, "in": r["input"], "out": r["output"], "cache_read": r["cache_read"],
                                   "cache_write": r["cache_write"], "window": WINDOW}]}, f, indent=1)
        home = os.path.join(work, "home-cg"); os.makedirs(home, exist_ok=True)
        env = {k: v for k, v in os.environ.items()
               if k.lower() not in ("http_proxy", "https_proxy", "all_proxy", "no_proxy", "anthropic_api_key")}
        env.update(HOME=home, MODEL_PRICES=prices)
        self.proc = subprocess.Popen(
            [binary, "--listen", f"127.0.0.1:{port}", "--config", cfg_path, "--anthropic-upstream", UPSTREAM,
             "--dashboard", "--dashboard-db", os.path.join(work, "cg-dashboard.db")],
            cwd=work, env=env, stdout=open(self.log_path, "w"), stderr=subprocess.STDOUT)

    def get(self, path):
        return cgit.local_get(self.port, path)

    def healthy(self, secs=30):
        end = time.time() + secs
        while time.time() < end:
            if self.proc.poll() is not None:
                return False
            try:
                self.get("/stats"); return True
            except Exception:
                time.sleep(0.3)
        return False

    def log(self):
        return open(self.log_path, errors="replace").read()

    def stop(self):
        if self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(10)
            except subprocess.TimeoutExpired:
                self.proc.kill()


# ---------------------------------------------------------------- fixtures and the agent

def make_fixtures(d, n, rnd):
    """n files of ~20KB (~5-6k tokens) each, in four shapes the engine treats differently."""
    os.makedirs(d, exist_ok=True)
    names = []
    for i in range(1, n + 1):
        kind = ("log", "json", "py", "md")[i % 4]
        if kind == "log":
            body = "\n".join(f"2026-10-15T0{i % 10}:{j // 60:02d}:{j % 60:02d}Z {rnd.choice(['INFO', 'INFO', 'WARN'])} "
                             f"worker[{j % 6}] job={i}-{j} took {rnd.randint(20, 900)}ms queue={rnd.randint(0, 40)} ok"
                             for j in range(230))
        elif kind == "json":
            body = json.dumps([{"id": f"svc-{i}-{j}", "region": rnd.choice(["us-east", "eu-de", "jp-tok"]),
                                "status": rnd.choice(["healthy"] * 6 + ["degraded"]), "cpu": round(rnd.random(), 3),
                                "owner": f"team-{rnd.randint(1, 9)}", "replicas": rnd.randint(1, 12)}
                               for j in range(110)], indent=2)
        elif kind == "py":
            body = "\n\n".join(
                f"def handler_{i}_{j}(req, ctx):\n    \"\"\"Handle route /{i}/{j}; owner team-{rnd.randint(1, 9)}.\"\"\"\n"
                f"    if not req.get('id'):\n        raise ValueError('missing id for route {i}/{j}')\n"
                f"    limit = ctx.config.get('limit_{j}', {rnd.randint(5, 500)})\n"
                f"    rows = ctx.db.query('SELECT * FROM t{j % 13} WHERE k = ?', req['id'])[:limit]\n"
                f"    return {{'route': '{i}/{j}', 'count': len(rows), 'rows': rows}}"
                for j in range(55))
        else:
            # Plain sentences, not word salad: Sonnet 4.6's usage classifier refused a random word
            # sequence ("can't help with this ... [bio]") on one turn in two of three arms.
            body = "\n\n".join(f"## Note {i}.{j}\nReference code FACT-{i:02d}{j:02d}-{rnd.randint(1000, 9999)}. "
                               + " ".join(rnd.choice(NOTE_SENTENCES).format(n=rnd.randint(2, 9000),
                                                                            r=rnd.choice(REGIONS))
                                          for _ in range(7)) for j in range(22))
        name = f"part-{i:02d}.{ 'txt' if kind == 'md' else kind}"
        with open(os.path.join(d, name), "w") as f:
            f.write(body + "\n")
        names.append(name)
    return names


NOTE_SENTENCES = (
    "The billing service migration moves {n} ledger rows to the new schema.",
    "Consumers keep reading the old cache until the staged rollout reaches {r}.",
    "The rollout pauses for {n} minutes after each region to compare invoice totals.",
    "Old rows stay readable for {n} days so that refunds can still be reconciled.",
    "The schema change adds a currency column and an index on the tenant id.",
    "Operators confirm that the replica in {r} has caught up before the switch.",
    "A failed batch is retried {n} times and then reported to the on-call engineer.",
    "The ledger export runs at night in {r} and takes about {n} seconds.",
    "Each consumer must acknowledge the new event version before the old one is retired.",
    "The cache is warmed with the {n} most used accounts before traffic moves.",
)
REGIONS = ("us-east", "us-south", "eu-de", "eu-gb", "jp-tok", "au-syd")


class Agent:
    """One Claude Code session, driven turn by turn with claude -p."""

    def __init__(self, arm, work, fixtures, key, base_url, proxy=None, ca=None):
        self.arm, self.fixtures, self.sid, self.turns = arm, fixtures, str(uuid.uuid4()), 0
        home = os.path.join(work, f"home-claude-{arm}")
        cfgdir = os.path.join(home, ".claude"); os.makedirs(cfgdir, exist_ok=True)
        with open(os.path.join(cfgdir, ".claude.json"), "w") as f:
            json.dump({"autoCompactEnabled": False, "hasCompletedOnboarding": True}, f)
        env = {k: v for k, v in os.environ.items()
               if not k.startswith(("ANTHROPIC_", "CLAUDE")) and k.lower() not in
               ("http_proxy", "https_proxy", "all_proxy", "no_proxy", "node_extra_ca_certs")}
        env.update(HOME=home, CLAUDE_CONFIG_DIR=cfgdir, ANTHROPIC_BASE_URL=base_url, ANTHROPIC_AUTH_TOKEN=key,
                   ANTHROPIC_MODEL=MODEL, ANTHROPIC_SMALL_FAST_MODEL=MODEL, ANTHROPIC_DEFAULT_HAIKU_MODEL=MODEL,
                   DISABLE_AUTO_COMPACT="1", DISABLE_COMPACT="1", CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC="1",
                   CLAUDE_CODE_MAX_CONTEXT_TOKENS=str(WINDOW),
                   ANTHROPIC_CUSTOM_HEADERS=f"X-Session-Id: {self.sid}\nx-context-guru-session: {self.sid}")
        if proxy:
            env.update(HTTPS_PROXY=proxy, https_proxy=proxy, NODE_EXTRA_CA_CERTS=ca)
        self.env = env

    def turn(self, prompt, turns_path):
        cmd = ["claude", "-p", prompt, "--output-format", "json", "--model", MODEL,
               "--tools", "Read,Bash", "--allowed-tools", "Read", "Bash(cat:*)", "--permission-mode", "dontAsk"]
        cmd += ["--session-id", self.sid] if self.turns == 0 else ["--resume", self.sid]
        t0 = time.time()
        p = subprocess.run(cmd, cwd=self.fixtures, env=self.env, capture_output=True, text=True, timeout=900)
        self.turns += 1
        try:
            res = json.loads(p.stdout)
        except ValueError:
            res = {"unparsed": p.stdout[-2000:]}
        row = {"t": t0, "dur": time.time() - t0, "arm": self.arm, "phase": PHASE["name"], "step": PHASE["step"],
               "prompt": prompt[:200], "rc": p.returncode, "stderr": p.stderr[-1000:],
               "is_error": res.get("is_error"), "usage": res.get("usage"), "total_cost_usd": res.get("total_cost_usd"),
               "num_turns": res.get("num_turns"), "result": str(res.get("result", ""))[:300]}
        jl(turns_path, row)
        return row


# ---------------------------------------------------------------- polling

def cortex_row(port, sid):
    for s in cgit.local_get(port, "/v1/sessions").get("sessions", []):
        if s.get("id") == sid:
            return s
    return None


def cg_row(cg, sid):
    rows = cg.get(f"/api/sessions?session={sid}").get("sessions") or []
    return next((r for r in rows if r.get("session_id") == sid), rows[0] if rows else None)


KV = re.compile(r'(\w+)=("(?:[^"\\]|\\.)*"|\S+)')


def log_lines(text, msg):
    """slog text lines whose msg is msg, as dicts."""
    out = []
    for line in text.splitlines():
        if f'msg="{msg}"' in line or f"msg={msg} " in line:
            out.append({k: v.strip('"') for k, v in KV.findall(line)})
    return out


def usd(u):
    """Price a usage dict (input/cacheWrite/cacheRead/output) at RATES."""
    return (int(u.get("input", 0)) * RATES["input"] + int(u.get("cacheWrite", 0)) * RATES["cache_write"]
            + int(u.get("cacheRead", 0)) * RATES["cache_read"] + int(u.get("output", 0)) * RATES["output"]) / 1e6


def plugin_spend(log, sid):
    """Arm A's context-guru spend that only the log carries (spend.LogRecorder), priced at RATES."""
    rows = [r for r in log_lines(log, "plugin spend") if r.get("session") == sid]
    by = {}
    for r in rows:
        by[r.get("purpose", "?")] = by.get(r.get("purpose", "?"), 0.0) + usd(r)
    return {"charges": len(rows), "usd_by_purpose": by}


def snapshot(ctx):
    t, out = time.time(), {"t": time.time(), "phase": PHASE["name"], "step": PHASE["step"]}
    for arm in ("A", "C"):
        try:
            out[arm] = cortex_row(ctx["ports"][arm][1], ctx["agents"][arm].sid)
        except Exception as e:
            out[arm] = {"error": str(e)}
    try:
        out["B"] = cg_row(ctx["cg"], ctx["agents"]["B"].sid)
        out["B_stats_savings"] = (ctx["cg"].get("/stats") or {}).get("savings")
    except Exception as e:
        out["B"] = {"error": str(e)}
    out["A_log_spend"] = plugin_spend(ctx["cx"]["A"].log(), ctx["agents"]["A"].sid)
    out["A_log_pings"] = len([r for r in log_lines(ctx["cx"]["A"].log(), "context-guru keep-alive ping")
                              if r.get("session") == ctx["agents"]["A"].sid])
    return out


def poller(ctx, stop, every):
    while not stop.wait(every):
        try:
            jl(ctx["metrics"], snapshot(ctx))
        except Exception as e:
            jl(ctx["metrics"], {"t": time.time(), "error": str(e)})


# ---------------------------------------------------------------- final reads and checks

def cortex_events(port, sid):
    """Every event of a session, paged backward from the tail."""
    evs, before = [], None
    while True:
        q = f"/v1/sessions/{sid}?limit=500" + (f"&before={before}" if before else "")
        page = cgit.local_get(port, q)
        got = page.get("events", [])
        evs = got + evs
        oldest = page.get("oldestSeq")
        if not got or oldest is None or got[0].get("seq") in (None, 0, oldest):
            break
        before = got[0]["seq"]
    return evs


def find_key(obj, key):
    """First value under key anywhere in obj."""
    if isinstance(obj, dict):
        if key in obj:
            return obj[key]
        for v in obj.values():
            r = find_key(v, key)
            if r is not None:
                return r
    elif isinstance(obj, list):
        for v in obj:
            r = find_key(v, key)
            if r is not None:
                return r
    return None


def check_passthrough(events):
    """P1/P2 input: for each request that carried a context-guru event, the saving its response priced."""
    by_req = {}
    for e in events:
        rid = e.get("requestId") or e.get("request_id")
        if rid:
            by_req.setdefault(rid, []).append(e)
    rows = []
    for rid, evs in by_req.items():
        cg = next((e["plugins"]["context-guru"] for e in evs if isinstance(e.get("plugins"), dict)
                   and isinstance(e["plugins"].get("context-guru"), dict)), None)
        resp = next((e for e in evs if e.get("phase") == "response"), None)
        cost = (resp or {}).get("plugins", {}).get("cost") if resp else None
        saving = next((s for s in (find_key(cost, "avoided") or []) if s.get("component") == "context-guru"), None)
        if cg is None and saving is None:
            continue
        rows.append({"requestId": rid, "at": (resp or evs[0]).get("at"), "event": cg, "saving": saving,
                     "cost": cost, "usage": (resp or {}).get("inference") or {}})
    return rows


def expected_saving(ev, usage):
    """compactionDraft's rule, re-done here from the response's own cache counts: engine tokens when
    the engine counted, split by savedUnique/savedGross. The unique part is a cache write when the
    turn wrote cache, else input; the repeated part is a cache read on a hit, a cache write when the
    write covers the prompt, else input. The byte-ratio fallback is not re-derived; such rows report
    tokens only."""
    tb, ta = int(ev.get("tokensBefore", 0)), int(ev.get("tokensAfter", 0))
    tokens = tb - ta if tb > ta and ta > 0 else None
    if tokens is None:
        return None, None
    inp, cr, cw = (int(usage.get(k, 0)) for k in ("inputTokens", "cacheReadTokens", "cacheWriteTokens"))
    gross = int(ev.get("savedGross", 0))
    frac = min(1.0, max(0.0, int(ev.get("savedUnique", 0)) / gross)) if gross > 0 else 1.0
    unique = round(tokens * frac)
    u_rate = RATES["cache_write"] if cw > 0 else RATES["input"]
    r_rate = RATES["cache_read"] if cr > 0 else RATES["cache_write"] if cw > 0 and cw >= inp else RATES["input"]
    return tokens, (unique * u_rate + (tokens - unique) * r_rate) / 1e6


def run_checks(ctx, final):
    checks = []

    def rec(cid, status, detail):
        checks.append({"id": cid, "status": status, "detail": detail})
        print(f"{status:4} {cid:24} {detail}", flush=True)

    rows = check_passthrough(final["A_events"])
    bad, matched, total_usd = [], 0, 0.0
    for r in rows:
        ev, s = r["event"] or {}, r["saving"]
        tokens, want = expected_saving(ev, r["usage"])
        if s is None:
            if int(ev.get("bytesRemoved", 0)) > 0:
                bad.append(f"{r['requestId']}: event removed {ev.get('bytesRemoved')}B, no priced saving")
            continue
        got_t, got_usd, pen = int(s.get("tokensAvoided", 0)), float(s.get("usd", 0)), float(s.get("penaltyUsd", 0))
        total_usd += max(0.0, got_usd - pen)
        if tokens is not None and got_t != tokens:
            bad.append(f"{r['requestId']}: tokens {got_t} != engine {tokens}")
        elif want is not None and abs(got_usd - want) > 1e-6:
            bad.append(f"{r['requestId']}: usd {got_usd:.6f} != {want:.6f} (tier {s.get('tier')})")
        else:
            matched += 1
    rec("P1-row-passthrough", "PASS" if rows and not bad else "FAIL",
        f"{matched}/{len(rows)} rows match" + (f"; first mismatches: {bad[:3]}" if bad else ""))
    sess = final["A_row"] or {}
    am = int(sess.get("avoidedMicros", 0))
    rec("P2-session-total", "PASS" if abs(am - round(total_usd * 1e6)) <= max(2, len(rows)) else "FAIL",
        f"avoidedMicros={am} vs sum of row savings net of penalty={round(total_usd * 1e6)}")

    b, c = final["B_row"] or {}, final["C_row"] or {}
    a_cost = int(sess.get("costMicros", 0)) / 1e6
    c_cost = int(c.get("costMicros", 0)) / 1e6
    a_extra = sum(final["A_log_spend"]["usd_by_purpose"].values())
    b_cost = float(b.get("cost_usd", 0)) + float(b.get("cg_llm_cost_usd", 0)) + float(b.get("keepalive_ping_usd", 0))
    rec("X1-spend", "INFO", f"A agent ${a_cost:.4f} + plugin ${a_extra:.4f} (log) | B ${b_cost:.4f} "
        f"(agent {b.get('cost_usd', 0):.4f}, cg llm {b.get('cg_llm_cost_usd', 0):.4f}, pings {b.get('keepalive_ping_usd', 0):.4f}) "
        f"| C ${c_cost:.4f}")
    rec("X1-savings", "INFO", f"A avoided ${am / 1e6:.4f} | B saved_usd ${b.get('saved_usd', 0):.4f}, "
        f"baseline-cost ${float(b.get('baseline_cost_usd', 0)) - float(b.get('cost_usd', 0)):.4f}, "
        f"keepalive net ${b.get('keepalive_net_usd', 0):.4f} | measured C-A ${c_cost - a_cost - a_extra:.4f}, "
        f"C-B ${c_cost - b_cost:.4f}")
    rec("X1-tokens", "INFO", f"engine saved: A sum={sum(max(0, int((r['event'] or {}).get('tokensBefore', 0)) - int((r['event'] or {}).get('tokensAfter', 0))) for r in rows)} "
        f"| B saved={b.get('saved')} saved_unique={b.get('saved_unique')}")

    pings_a, pings_b = final["A_log_pings"], int(b.get("keepalive_pings", 0))
    rec("K1-pings", "PASS" if pings_a >= 1 and pings_b >= 1 else ("INFO" if ctx["smoke"] else "FAIL"),
        f"A pings={pings_a} (log), B pings={pings_b} (dashboard)")
    for arm in ARMS:
        post = [t for t in final["turns"] if t["arm"] == arm and t["phase"] == "resume"]
        if post:
            u = post[0].get("usage") or {}
            rec(f"K1-after-freeze-{arm}", "INFO", f"first resume turn: cache_read={u.get('cache_read_input_tokens')} "
                f"cache_write={u.get('cache_creation_input_tokens')} input={u.get('input_tokens')}")
    for arm in ARMS:
        errs = [t for t in final["turns"] if t["arm"] == arm and (t["rc"] != 0 or t.get("is_error"))]
        rec(f"T-turns-{arm}", "PASS" if not errs else "FAIL",
            f"{len([t for t in final['turns'] if t['arm'] == arm])} turns, {len(errs)} failed"
            + (f": {errs[0]['stderr'][-200:] or errs[0]['result'][:200]}" if errs else ""))
    return checks


# ---------------------------------------------------------------- plot

def plot(work):
    import matplotlib
    matplotlib.use("Agg")
    import matplotlib.pyplot as plt
    snaps = [json.loads(l) for l in open(os.path.join(work, "metrics.jsonl")) if '"error"' not in l[:40]]
    snaps = [s for s in snaps if "A" in s]
    turns = [json.loads(l) for l in open(os.path.join(work, "turns.jsonl"))]
    if not snaps:
        print("no metrics to plot"); return
    t0 = min(s["t"] for s in snaps)
    mins = lambda t: (t - t0) / 60
    color = {"A": "#2a6fdb", "B": "#e07b00", "C": "#6b6b6b", "CA": "#2a6fdb", "CB": "#e07b00"}

    def series(f):
        xs, ys = [], []
        for s in snaps:
            try:
                v = f(s)
            except Exception:
                v = None
            if v is not None:
                xs.append(mins(s["t"])); ys.append(v)
        return xs, ys

    num = lambda d, k: float((d or {}).get(k) or 0)
    a_cost = lambda s: num(s["A"], "costMicros") / 1e6 + sum((s.get("A_log_spend") or {}).get("usd_by_purpose", {}).values())
    b_cost = lambda s: num(s["B"], "cost_usd") + num(s["B"], "cg_llm_cost_usd") + num(s["B"], "keepalive_ping_usd")
    c_cost = lambda s: num(s["C"], "costMicros") / 1e6

    fig, ax = plt.subplots(3, 1, figsize=(11, 12), sharex=True)
    for arm, f in (("A", a_cost), ("B", b_cost), ("C", c_cost)):
        ax[0].plot(*series(f), label=ARM_NAME[arm], color=color[arm], lw=2)
    ax[0].set_ylabel("cumulative spend (USD)")
    ax[0].set_title("Spend per arm (A includes plugin spend from the log; B includes cg LLM calls and pings)")

    ax[1].plot(*series(lambda s: num(s["A"], "avoidedMicros") / 1e6), label="A: Cortex avoidedMicros",
               color=color["A"], lw=2)
    ax[1].plot(*series(lambda s: num(s["B"], "saved_usd")), label="B: cg saved_usd", color=color["B"], lw=2)
    ax[1].plot(*series(lambda s: num(s["B"], "keepalive_net_usd") + num(s["B"], "saved_usd")),
               label="B: saved_usd + keep-alive net", color=color["B"], lw=1.5, ls="--")
    ax[1].plot(*series(lambda s: c_cost(s) - a_cost(s)), label="measured C - A", color=color["CA"], lw=1.2, ls=":")
    ax[1].plot(*series(lambda s: c_cost(s) - b_cost(s)), label="measured C - B", color=color["CB"], lw=1.2, ls=":")
    ax[1].axhline(0, color="#999", lw=0.8)
    ax[1].set_ylabel("cumulative savings (USD)")
    ax[1].set_title("Reported savings vs measured difference to the control arm")

    for arm in ARMS:
        pts = [(mins(t["t"] + t["dur"]), sum(int((t.get("usage") or {}).get(k) or 0) for k in
                ("input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens")) / max(1, t.get("num_turns") or 1))
               for t in turns if t["arm"] == arm]
        if pts:
            ax[2].plot(*zip(*pts), marker="o", ms=3, label=ARM_NAME[arm], color=color[arm], lw=1.2)
    ax[2].set_ylabel("prompt tokens per API call (turn avg)")
    ax[2].set_xlabel("minutes since start")
    ax[2].set_title("Prompt size per turn")

    starts, last = [], None
    for s in snaps:
        if s["phase"] != last:
            starts.append((mins(s["t"]), s["phase"])); last = s["phase"]
    for a in ax:
        for x, name in starts:
            a.axvline(x, color="#bbb", lw=0.8, ls="--")
        a.legend(loc="upper left", fontsize=8, frameon=False)
        a.grid(alpha=0.25)
    for x, name in starts:
        ax[0].text(x, ax[0].get_ylim()[1], f" {name}", va="top", fontsize=8, color="#555")
    pings = series(lambda s: s.get("A_log_pings"))
    for i in range(1, len(pings[1])):
        if pings[1][i] > pings[1][i - 1]:
            for a in ax:
                a.axvline(pings[0][i], color=color["A"], lw=0.8, alpha=0.6)
    fig.tight_layout()
    out = os.path.join(work, "savings.png")
    fig.savefig(out, dpi=130)
    print("plot:", out)


# ---------------------------------------------------------------- main

WINDOW = 200000


def fetch(name):
    """The instruction that puts one fixture into the context. Source and notes go through Read
    (line-numbered, which extract_llm skips as a file read); JSON and logs through `cat`, a plain
    command output the engine may compact — the mix an agent session produces."""
    if name.endswith((".py", ".txt")):
        return f"Use the Read tool to read {name}"
    return f"Use the Bash tool to run `cat {name}`"


def prompt_ctx(port, sid):
    row = cortex_row(port, sid) or {}
    return int((row.get("promptContext") or {}).get("tokens") or 0)


def step_all(ctx, prompts):
    """One turn in every arm at once; prompts maps arm -> prompt."""
    PHASE["step"] += 1
    with concurrent.futures.ThreadPoolExecutor(3) as ex:
        futs = {arm: ex.submit(ctx["agents"][arm].turn, prompts[arm], ctx["turns"]) for arm in ARMS}
        res = {arm: f.result() for arm, f in futs.items()}
    for arm in ARMS:
        r = res[arm]
        print(f"  step {PHASE['step']:3} {PHASE['name']:6} {arm} rc={r['rc']} {r['dur']:5.1f}s "
              f"cost={r.get('total_cost_usd')} {r['result'][:60]!r}", flush=True)
    return res


def run(a):
    global WINDOW
    WINDOW = a.window
    set_model(a.model)
    key = api_key(a.env_file)
    work = tempfile.mkdtemp(prefix="cg-compare-")
    print("work dir:", work, flush=True)
    ctx = {"smoke": a.smoke, "metrics": os.path.join(work, "metrics.jsonl"), "turns": os.path.join(work, "turns.jsonl")}
    binary = cgit.build(work)
    cgbin = build_cg(a.cg_repo, work)
    ports = {"A": cgit.free_ports(5, lo=48700), "C": None}
    ports["C"] = cgit.free_ports(5, lo=ports["A"][-1] + 1)
    cg_port = cgit.free_ports(1, lo=ports["C"][-1] + 1)[0]
    ctx["ports"] = ports
    cx = {arm: cgit.Cortex(binary, work, f"cortex-{arm}", cortex_yaml(ports[arm], work, arm, arm == "A")) for arm in ("A", "C")}
    cg = CGProxy(cgbin, work, cg_port)
    ctx["cx"], ctx["cg"] = cx, cg
    stop = threading.Event()
    try:
        for arm in ("A", "C"):
            if not cgit.wait_healthy(cx[arm], ports[arm][2]):
                sys.exit(f"cortex-{arm} not healthy:\n" + cx[arm].log()[-2000:])
        if not cg.healthy():
            sys.exit("context-guru-proxy not healthy:\n" + cg.log()[-2000:])
        cas = {}
        for arm in ("A", "C"):
            ca = os.path.join(work, f"ca-{arm}", "ca.crt")
            for _ in range(100):
                if os.path.exists(ca):
                    break
                time.sleep(0.1)
            cas[arm] = ca
        fx = os.path.join(work, "fixtures")
        rnd = random.Random(15)
        names = make_fixtures(fx, a.files, rnd)
        nonce = uuid.uuid4().hex[:8]  # a fresh prompt per run, so no arm starts on another run's cache
        ctx["agents"] = {
            "A": Agent("A", work, fx, key, UPSTREAM, f"http://127.0.0.1:{ports['A'][0]}", cas["A"]),
            "B": Agent("B", work, fx, key, f"http://127.0.0.1:{cg_port}/anthropic"),
            "C": Agent("C", work, fx, key, UPSTREAM, f"http://127.0.0.1:{ports['C'][0]}", cas["C"]),
        }
        json.dump({"window": WINDOW, "fill_target": int(a.fill_frac * WINDOW), "rates": RATES, "model": MODEL,
                   "sessions": {arm: ag.sid for arm, ag in ctx["agents"].items()}, "ports": ports, "cg_port": cg_port,
                   "freeze_s": a.freeze_s, "engine": ENGINE}, open(os.path.join(work, "run.json"), "w"), indent=1)
        threading.Thread(target=poller, args=(ctx, stop, a.poll_s), daemon=True).start()

        target = int(a.fill_frac * WINDOW)
        PHASE["name"] = "fill"
        same = lambda p: {arm: p for arm in ARMS}
        step_all(ctx, same(f"Run {nonce}. You will look at files in this directory when asked. "
                           f"{fetch(names[0])}, then reply with only the word OK."))
        time.sleep(2)
        last = prompt_ctx(ports["C"][1], ctx["agents"]["C"].sid)
        i, growth = 1, 0
        while i < len(names):
            # Every third turn repeats an earlier cat, so dedup has an identical output to find.
            again = [n for n in names[:i] if not n.endswith((".py", ".txt"))]
            files = [names[i]] + ([again[-2]] if i % 3 == 0 and len(again) >= 2 else [])
            i += 1
            step_all(ctx, same(" Then ".join(fetch(f) for f in files) + ". Then reply with only the word OK."))
            time.sleep(2)
            now = prompt_ctx(ports["C"][1], ctx["agents"]["C"].sid)
            growth = max(growth, now - last); last = now
            print(f"  arm C prompt context {now} / target {target} (growth {growth}/turn)", flush=True)
            if now + growth > target:
                break
        else:
            print("  WARN: ran out of fixture files before the target; raise --files", flush=True)

        PHASE["name"] = "small"
        for q in range(a.small):
            step_all(ctx, same(f"Without using any tool, answer in one short sentence: what kind of content "
                               f"was in {names[(q * 5) % i]}?"))
        jl(ctx["metrics"], snapshot(ctx))
        PHASE["name"] = "freeze"
        print(f"  freeze {a.freeze_s}s", flush=True)
        time.sleep(a.freeze_s)
        jl(ctx["metrics"], snapshot(ctx))
        PHASE["name"] = "resume"
        for q in range(a.resume):
            step_all(ctx, same(f"Without using any tool, answer in one short sentence: how many files have you "
                               f"read so far{' in total' if q else ''}?"))
        time.sleep(5)
        PHASE["name"] = "end"
        jl(ctx["metrics"], snapshot(ctx))
        stop.set()

        final = snapshot(ctx)
        final["A_events"] = cortex_events(ports["A"][1], ctx["agents"]["A"].sid)
        final["C_events"] = cortex_events(ports["C"][1], ctx["agents"]["C"].sid)
        final["A_row"], final["B_row"], final["C_row"] = final["A"], final["B"], final["C"]
        final["turns"] = [json.loads(l) for l in open(ctx["turns"])]
        for path, name in (("/api/requests?session=" + ctx["agents"]["B"].sid + "&limit=1000", "B_requests"),
                           ("/api/keepalive/sessions", "B_keepalive"), ("/stats", "B_stats")):
            try:
                final[name] = cg.get(path)
            except Exception as e:
                final[name] = {"error": str(e)}
        json.dump(final, open(os.path.join(work, "final.json"), "w"), indent=1, default=str)
        checks = run_checks(ctx, final)
        json.dump(checks, open(os.path.join(work, "checks.json"), "w"), indent=1)
        plot(work)
        return 1 if any(c["status"] == "FAIL" for c in checks) else 0
    finally:
        stop.set()
        for c in list(cx.values()) + [cg]:
            c.stop()
        print("work dir kept:", work)


def main():
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)
    r = sub.add_parser("run")
    r.add_argument("--cg-repo", required=True, help="context-guru checkout to build context-guru-proxy from")
    r.add_argument("--env-file", default=None)
    r.add_argument("--window", type=int, default=200000)
    r.add_argument("--model", default=MODEL, choices=sorted(MODEL_RATES))
    # 0.8, not 0.95: Claude Code refuses a prompt past about 180k of a 200k window ("Prompt is too
    # long"), so the control arm stopped short of a 0.95 target.
    r.add_argument("--fill-frac", type=float, default=0.8)
    r.add_argument("--files", type=int, default=60)
    r.add_argument("--small", type=int, default=5)
    r.add_argument("--resume", type=int, default=3)
    r.add_argument("--freeze-s", type=int, default=360)
    r.add_argument("--poll-s", type=int, default=15)
    r.add_argument("--smoke", action="store_true", help="tiny run to check the plumbing: fill to 45k, 1+1 questions, 20s freeze")
    p = sub.add_parser("plot")
    p.add_argument("work")
    a = ap.parse_args()
    if a.cmd == "plot":
        plot(a.work); return 0
    if a.smoke:
        a.fill_frac, a.small, a.resume, a.freeze_s, a.poll_s = 45000 / a.window, 1, 1, 20, 5
    return run(a)


if __name__ == "__main__":
    sys.exit(main())
