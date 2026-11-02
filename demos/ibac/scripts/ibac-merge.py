#!/usr/bin/env python3
"""Merge IBAC pipeline additions into the operator-rendered authbridge
config.yaml.

Reads:
  - argv[1]: path to k8s/ibac-patch.yaml (the additions doc)
  - stdin:   the operator's current config.yaml content

Writes the merged YAML to stdout.

Idempotent: re-running with already-merged input is a no-op (entries
matched by `name` aren't duplicated).
"""

import sys

import yaml


def main() -> int:
    if len(sys.argv) != 2:
        sys.stderr.write("usage: ibac-merge.py <patch-file>\n")
        return 2

    patch_path = sys.argv[1]

    operator = yaml.safe_load(sys.stdin) or {}
    with open(patch_path) as f:
        patch = yaml.safe_load(f) or {}

    pipeline = operator.setdefault("pipeline", {})
    inbound = pipeline.setdefault("inbound", {})
    outbound = pipeline.setdefault("outbound", {})
    in_plugins = inbound.setdefault("plugins", [])
    out_plugins = outbound.setdefault("plugins", [])

    in_names = {p.get("name") for p in in_plugins}
    out_names = {p.get("name") for p in out_plugins}

    # Reverse-then-prepend preserves the patch's natural order at the
    # front of the chain.
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
