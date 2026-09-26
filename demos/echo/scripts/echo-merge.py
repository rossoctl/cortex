#!/usr/bin/env python3
"""Merge credential placeholder-swap settings into the operator-rendered
authbridge config.yaml.

Reads:
  - argv[1]: path to k8s/echo-patch.yaml (the additions fragment)
  - stdin:   the operator's current config.yaml content

Writes the merged YAML to stdout.

Unlike ibac-merge.py (which APPENDS whole plugins to the pipeline), this
script EDITS the existing plugin configs in place:

  * inbound  jwt-validation : set placeholder_mode + placeholder_ttl
  * outbound token-exchange : set resolve_placeholders + add the
                              echo-upstream route under
                              config.routes.rules

The route key shape is dictated by the token-exchange plugin's Go config
struct (core/plugins/tokenexchange/plugin.go):

    tokenExchangeConfig.Routes  json:"routes"        -> config.routes
    tokenExchangeRoutes.Rules   json:"rules"         -> config.routes.rules
    tokenExchangeRoute.Host           json:"host"
    tokenExchangeRoute.TargetAudience json:"target_audience"
    tokenExchangeRoute.TokenScopes    json:"token_scopes"

So an inline route lives at:
    token-exchange.config.routes.rules: [ {host, target_audience, token_scopes} ]

Idempotent: re-running with already-merged input is a no-op (flags that
are already set stay set; routes already present by `host` aren't
duplicated). Output uses safe_dump(..., sort_keys=False) so the
hot-reload SHA comparison in patch-echo-config.sh is deterministic.
"""

import sys
import yaml


def merge_inbound(plugins, jwt_cfg):
    """Set placeholder_mode / placeholder_ttl on every jwt-validation
    plugin entry's config. Creates the config map if missing."""
    for p in plugins:
        if p.get("name") != "jwt-validation":
            continue
        cfg = p.setdefault("config", {})
        if "placeholder_mode" in jwt_cfg:
            cfg["placeholder_mode"] = jwt_cfg["placeholder_mode"]
        if "placeholder_ttl" in jwt_cfg:
            cfg["placeholder_ttl"] = jwt_cfg["placeholder_ttl"]


def merge_outbound(plugins, te_cfg, routes):
    """Set resolve_placeholders on every token-exchange plugin entry and
    append the patch's routes into config.routes.rules (by `host`)."""
    for p in plugins:
        if p.get("name") != "token-exchange":
            continue
        cfg = p.setdefault("config", {})
        if "resolve_placeholders" in te_cfg:
            cfg["resolve_placeholders"] = te_cfg["resolve_placeholders"]

        # Merge inline routes into config.routes.rules. The plugin decodes
        # routes at config.routes.rules[] (see module docstring), so we
        # build that nesting explicitly rather than placing a bare list.
        routes_node = cfg.setdefault("routes", {})
        rules = routes_node.setdefault("rules", [])
        existing_hosts = {r.get("host") for r in rules if isinstance(r, dict)}
        for route in routes:
            host = route.get("host")
            if host in existing_hosts:
                continue
            rules.append(
                {
                    "host": host,
                    "target_audience": route.get("target_audience"),
                    "token_scopes": route.get("token_scopes"),
                }
            )
            existing_hosts.add(host)


def main() -> int:
    if len(sys.argv) != 2:
        sys.stderr.write("usage: echo-merge.py <patch-file>\n")
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

    jwt_cfg = (patch.get("inbound_plugin_config") or {}).get("jwt-validation") or {}
    te_cfg = (patch.get("outbound_plugin_config") or {}).get("token-exchange") or {}
    routes = patch.get("routes") or []

    merge_inbound(in_plugins, jwt_cfg)
    merge_outbound(out_plugins, te_cfg, routes)

    sys.stdout.write(yaml.safe_dump(operator, default_flow_style=False, sort_keys=False))
    return 0


if __name__ == "__main__":
    sys.exit(main())
