#!/usr/bin/env python3
"""Dump Cortex in-memory session data to files via the session API.

STOPGAP. Durable session storage is tracked in rossoctl/cortex#901 ("persist
sessions"), which will have the proxy write sessions itself rather than depending
on someone running this first. Prefer commenting on #901 over growing this script.

The session store is in memory only -- nothing survives a proxy restart. This
walks the read APIs on listener.session_api_addr (127.0.0.1:47601 in the
laptop preset) and writes what is currently resident to disk.

What "currently resident" means depends on the `session:` config block
(core/config/config.go). With the defaults, session.max_sessions is 100 and
least-recently-updated sessions are evicted past that; max_events and ttl are
unlimited, so individual sessions are not trimmed. A dump is still a
point-in-time snapshot: a restart or an eviction loses what it did not capture.

Layout under --out:
    sessions.json               the /v1/sessions index
    usage.json                  /v1/usage aggregates
    pipeline.json, plugins.json active pipeline + plugin catalog
    sessions/<id>.jsonl         one JSON event per line, ascending seq

Examples:
    ./cortex-session-dump.py                    # all sessions, summary view
    ./cortex-session-dump.py --full             # include request/response bodies
    ./cortex-session-dump.py --session <id> --full
    ./cortex-session-dump.py --active           # only sessions marked active

See docs/session-dump.md for the field reference and the privacy note on --full.
"""

import argparse
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request

# The API caps limit at 2000; ask for the max so large sessions need few round trips.
PAGE_LIMIT = 2000


def get(base, path, **params):
    url = f"{base}{path}"
    if params:
        url += "?" + urllib.parse.urlencode(params)
    # noproxy: the session API is loopback, and this host has HTTPS_PROXY set to
    # the Cortex forward proxy itself -- routing through it would be circular.
    req = urllib.request.Request(url, headers={"Accept": "application/json"})
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(req, timeout=30) as resp:
        return json.load(resp)


def dump_events(base, session_id, out_path, full):
    """Page backward from the newest event, writing the result ascending.

    Ordering is by `at`, then `seq`: seq is unique only within one incarnation
    of a session, and whole-session eviction restarts it at 1 (see the Session
    API notes in docs/session-api.md). Sorting by seq would interleave two
    incarnations; `at` stays monotonic across both.
    """
    params = {"limit": PAGE_LIMIT}
    if not full:
        params["view"] = "summary"

    by_seq = {}
    oldest_seq = None
    before = None
    while True:
        page_params = dict(params)
        if before is not None:
            page_params["before"] = before
        page = get(base, f"/v1/sessions/{session_id}", **page_params)
        batch = page.get("events") or []
        if oldest_seq is None:
            # Absent entirely when the first response was the whole session, and
            # from proxies predating paging -- meaning "do not page here".
            oldest_seq = page.get("oldestSeq")
        if not batch:
            break

        # A cursor above everything held makes the endpoint answer with the tail,
        # because every event it holds precedes the cursor. Without this check
        # that is an infinite loop re-fetching the same page forever.
        fresh = [ev for ev in batch if ev.get("seq") not in by_seq]
        if not fresh:
            break
        for ev in fresh:
            by_seq[ev.get("seq")] = ev

        first_seq = batch[0].get("seq")
        # No seq at all: this proxy cannot page, so one response is all there is.
        if first_seq is None:
            break
        # oldestSeq is the lowest seq still held -- 1 unless max_events or ttl
        # trimmed the front. Stop there rather than paging down to 1 on empties.
        if oldest_seq is not None and first_seq <= oldest_seq:
            break
        if first_seq <= 1:
            break
        before = first_seq

    events = sorted(by_seq.values(), key=lambda ev: (ev.get("at") or "", ev.get("seq") or 0))
    with open(out_path, "w") as fh:
        for ev in events:
            fh.write(json.dumps(ev, separators=(",", ":")) + "\n")
    return len(events)


def write_json(path, obj):
    with open(path, "w") as fh:
        json.dump(obj, fh, indent=2)


def resolve_api(explicit):
    """Return a base URL whose /v1/sessions answers, or exit saying what was tried.

    Two ports are both "the default" depending on how Cortex was started: the
    laptop preset (`cortex --local`) pins 47601, while the mode presets
    use 9094, which is also what a `kubectl port-forward` is usually pointed at.
    Probing both beats making every laptop user pass --api.
    """
    if explicit:
        return explicit.rstrip("/")

    tried = []
    for base in (f"http://127.0.0.1:{port}" for port in (47601, 9094)):
        try:
            get(base, "/v1/sessions")
            return base
        except Exception as exc:
            tried.append(f"  {base}  {exc}")

    sys.exit(
        "no Cortex session API answered on either default port:\n"
        + "\n".join(tried)
        + "\n\nPass --api if it listens elsewhere, and check listener.session_api_addr\n"
        "in your config (~/.cortex/config.yaml for a laptop install). For an\n"
        "in-cluster proxy, port-forward 9094 first."
    )


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--api", default=None, help="session API base URL (default: probe 127.0.0.1:47601 then :9094)")
    ap.add_argument("--out", default="cortex-dump", help="output directory")
    ap.add_argument("--session", action="append", dest="sessions", help="dump only this session id (repeatable)")
    ap.add_argument("--active", action="store_true", help="dump only sessions currently marked active")
    ap.add_argument("--full", action="store_true", help="include message bodies (much larger; may contain secrets)")
    ap.add_argument(
        "--reuse-dir",
        action="store_true",
        help="write into an existing dump directory, leaving session files this run did not produce",
    )
    args = ap.parse_args()

    base = resolve_api(args.api)
    try:
        index = get(base, "/v1/sessions", **({"archived": "true"} if args.sessions else {}))
    except urllib.error.URLError as exc:
        sys.exit(f"cannot reach session API at {base}: {exc}")

    print(f"session API: {base}")

    # Refuse a directory that already holds a dump, rather than merging into it.
    # A --session or --active run writes only its selection, so reusing a previous
    # full dump silently leaves session files this run did not produce -- next to a
    # sessions.json that no longer lists some of them, and including sessions since
    # evicted. Refusing beats deleting: --out is a user-supplied path, and this
    # script has no business removing files it cannot prove it wrote.
    sessions_dir = os.path.join(args.out, "sessions")
    if not args.reuse_dir and os.path.isdir(sessions_dir) and os.listdir(sessions_dir):
        sys.exit(
            f"{sessions_dir} already contains a dump.\n"
            "Use a new --out directory, delete that one, or pass --reuse-dir to write\n"
            "into it anyway (leaving session files this run does not produce)."
        )

    os.makedirs(sessions_dir, exist_ok=True)
    write_json(os.path.join(args.out, "sessions.json"), index)

    # Side metadata: nice to have, but never fail the dump over it.
    for path, name in (("/v1/usage", "usage.json"), ("/v1/pipeline", "pipeline.json"), ("/v1/plugins", "plugins.json")):
        try:
            write_json(os.path.join(args.out, name), get(base, path))
        except Exception as exc:
            print(f"  skipped {name}: {exc}", file=sys.stderr)

    wanted = index.get("sessions") or []
    failed = []
    if args.sessions:
        requested = set(args.sessions)
        wanted = [s for s in wanted if s["id"] in requested]
        # Naming an id is a claim that it exists, so a miss is a failure and not an
        # empty result: a typo, or a session evicted between agentop showing it and
        # this running, would otherwise be indistinguishable from success. `--active`
        # matching nothing is different -- that is a legitimate empty answer.
        missing = sorted(requested - {s["id"] for s in wanted})
        for sid in missing:
            print(f"  {sid}: NOT IN INDEX (unknown or already evicted)", file=sys.stderr)
        failed.extend(missing)
    if args.active:
        wanted = [s for s in wanted if s.get("active")]

    total = 0
    ok = 0
    written = {}
    for meta in wanted:
        sid = meta["id"]
        # A session id is a path component here. Ids come from the proxy today, but
        # --api points this at an arbitrary host, which is where trusting them stops
        # being free -- a "/" or ".." would otherwise write outside sessions/.
        safe_sid = sid.replace(os.sep, "_").replace("/", "_").strip(".") or "unnamed"
        out_path = os.path.join(args.out, "sessions", f"{safe_sid}.jsonl")
        # That mapping is many-to-one: "a/b" and "a_b" both land on a_b.jsonl. Needs
        # the same hostile-host precondition as the traversal above, but a silent
        # overwrite is worse than a rejected one -- the run would report both
        # sessions dumped and exit 0, leaving a dump that looks complete and is not.
        #
        # Tracked per run rather than with os.path.exists: under --reuse-dir the file
        # may be left from an EARLIER run, which is that flag's whole purpose, and
        # testing the filesystem would turn a deliberate re-dump into a failure.
        if safe_sid in written:
            print(
                f"  {sid}: FAILED sanitized name {safe_sid!r} collides with {written[safe_sid]!r}",
                file=sys.stderr,
            )
            failed.append(sid)
            continue
        try:
            n = dump_events(base, sid, out_path, args.full)
        except Exception as exc:
            print(f"  {sid}: FAILED {exc}", file=sys.stderr)
            failed.append(sid)
            continue
        written[safe_sid] = sid
        total += n
        ok += 1
        print(f"  {sid}  {n} events (reported {meta.get('eventCount', '?')})")

    print(f"\n{ok} sessions, {total} events -> {args.out}/")
    if args.full:
        print("NOTE: --full output contains request/response bodies. Do not commit.")

    # Counted and exited on separately from the successes: a partial dump that
    # exits 0 is one a cron job or a `&&` chain reads as a complete one, and the
    # per-session line scrolls past. Both doors count -- a session that failed
    # mid-dump and one that was never attempted because the index did not list it.
    # Side metadata above stays non-fatal -- it is not what was asked for.
    if failed:
        print(f"{len(failed)} session(s) FAILED: {', '.join(failed)}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
