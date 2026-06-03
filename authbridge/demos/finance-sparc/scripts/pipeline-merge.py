#!/usr/bin/env python3
"""Merge pipeline additions into the operator-rendered authbridge config.yaml.

Generic over the plugin set: the patch file (argv[1]) declares
`inbound_prepend` / `outbound_append` lists of plugin entries; this merges
them into the operator's config (read from stdin) and writes the result to
stdout. Idempotent — entries matched by `name` are not duplicated.

(Identical in shape to demos/ibac/scripts/ibac-merge.py; kept demo-local so
the finance-sparc demo is self-contained.)
"""

import sys

import yaml


def main() -> int:
    if len(sys.argv) != 2:
        sys.stderr.write("usage: pipeline-merge.py <patch-file>\n")
        return 2

    operator = yaml.safe_load(sys.stdin) or {}
    with open(sys.argv[1]) as f:
        patch = yaml.safe_load(f) or {}

    pipeline = operator.setdefault("pipeline", {})
    in_plugins = pipeline.setdefault("inbound", {}).setdefault("plugins", [])
    out_plugins = pipeline.setdefault("outbound", {}).setdefault("plugins", [])
    in_names = {p.get("name") for p in in_plugins}
    out_names = {p.get("name") for p in out_plugins}

    # Reverse-then-prepend preserves the patch's natural order at the front.
    for entry in reversed(patch.get("inbound_prepend", []) or []):
        if entry.get("name") not in in_names:
            in_plugins.insert(0, entry)
            in_names.add(entry["name"])

    for entry in patch.get("outbound_append", []) or []:
        if entry.get("name") not in out_names:
            out_plugins.append(entry)
            out_names.add(entry["name"])

    sys.stdout.write(yaml.safe_dump(operator, default_flow_style=False, sort_keys=False))
    return 0


if __name__ == "__main__":
    sys.exit(main())
