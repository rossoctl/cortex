#!/usr/bin/env python3
"""Generate the central Grafana dashboards. Run it, and commit the JSON it writes.

Why a generator: a Grafana dashboard is a long JSON file in which a wrong query looks exactly like a
right one. Here every panel is one short line with its query, so a review can read them all.

    python3 gen_dashboards.py          # writes dashboards/*.json
    python3 gen_dashboards.py --check  # exit 1 if the committed JSON is out of date
"""

import json
import os
import sys

DS = {"type": "prometheus", "uid": "central-prom"}
FILTER = 'proxy=~"$proxy", machine=~"$machine"'


GREEN, ORANGE, RED, YELLOW, BLUE, PURPLE = "#73BF69", "#FF9830", "#F2495C", "#FADE2A", "#5794F2", "#B877D9"
USD = "currencyUSD"
# Label filters. They are constants because Python before 3.12 allows no backslash in an f-string.
X5 = ',status_class="5xx"'
DIR_SAVED, DIR_ADDED = ',direction="saved"', ',direction="added"'
RESCUE_SAVED, RESCUE_ADDED = ',effect="rescue",direction="saved"', ',effect="rescue",direction="added"'
TIER_READ = ',tier="cache_read"'
TIER_PROMPT = ',tier=~"cache_read|cache_write_5m|cache_write_1h|input"'


def var(name, label, query, multi=True):
    return {
        "name": name,
        "label": label,
        "type": "query",
        "datasource": DS,
        "query": {"query": query, "refId": "v"},
        "refresh": 2,
        "sort": 1,
        "includeAll": True,
        "allValue": ".*",
        "multi": multi,
        "current": {"selected": True, "text": "All", "value": "$__all"},
    }


# The two sides of the net saving, as one selector each (accountant docs/metrics/metrics.md, "The net
# saving"). The operation counters have no direction label, so direction=~"saved|" matches them and
# the saved side of the cache effects and of the interaction. No counter contains another.
SAVED_SIDE = ',__name__=~"llm_savings_(saved_usd|cache_effect_usd|interaction_usd)_total",direction=~"saved|"'
ADDED_SIDE = ',__name__=~"llm_savings_(added_usd|cache_effect_usd|interaction_usd)_total",direction=~"added|"'
# Labels that every counter of both sides has. A cache effect has no component, kind or tier.
NET_LABELS = ("", "machine", "proxy", "preset")

# The net saving split in two by source, as context-guru's own dashboard splits it (dash/savings.go,
# savingsArithmetic: compaction, then the prefix-cache split and the keep-alive's net).
#
# The operators that manage the cache rather than rewrite the context: keeper is
# accountant.PingOps's operator (the keep-alive pings and their rescues); cachesplit, headttl and
# tailbreakpoint are context-guru's (apply/ops.go, apply/headttl.go); cacheinject is context-guru's
# breakpoint component. Every other operator rewrites the request: its content, its tool
# declarations or (a setting) its model.
#
# The accountant's sink pushes an operator outside its WithComponents list as component="other".
# Cortex lists all five names (core/accounting Components). context-guru lists keeper, headttl and
# tailbreakpoint only from rossoctl/context-guru#1362 on; an older context-guru build pushes them as
# "other", which this split counts under Context rewrite. The dashboard cannot tell that "other"
# from an unlisted rewrite operator, so the fix is at the source, and the table's description says so.
CACHE_COMPONENTS = ("keeper", "cachesplit", "cacheinject", "headttl", "tailbreakpoint")
_CACHE_RE = "|".join(CACHE_COMPONENTS)
# Context rewrite: the rewriting operators, their own model calls (a spend), and every cache break.
# A break counts against the rewrite, as context-guru's compaction figure counts it (its actual cost
# holds the re-written prefix). Every break that reaches the counter was caused by a change: a break
# nobody caused is InBaseline, and the accountant's sink does not book it (accountant
# metrics/metrics.go). A break's cause (none, ended_change, model_changed) is a rewrite or a setting.
# A cache effect has no component label, so component!~ lets the breaks through, and an operation
# has no effect label, so effect=~"break|" lets the operations through.
REWRITE_SAVED = (
    ',__name__=~"llm_savings_(saved_usd|cache_effect_usd)_total",direction=~"saved|",effect=~"break|",'
    f'component!~"{_CACHE_RE}"'
)
REWRITE_ADDED = (
    ',__name__=~"llm_savings_(added_usd|cache_effect_usd)_total",direction=~"added|",effect=~"break|",'
    f'component!~"{_CACHE_RE}"'
)
# Cache management: the cache operators (a keep-alive ping is a spend there), every cache effect
# that is not a break (rescue, split, ttl_premium, ttl_rescue, mark_premium, mark_payback), and the
# interaction, which only a rescued request has. The trailing "|" in component=~ admits the effects
# and the interaction, which have no component. It cannot admit an operation, because every
# operation has a component (accountant docs/metrics/metrics.md, rule 7).
CACHE_SAVED = (
    ',__name__=~"llm_savings_(saved_usd|cache_effect_usd|interaction_usd)_total",direction=~"saved|",'
    f'effect!="break",component=~"{_CACHE_RE}|"'
)
CACHE_ADDED = (
    ',__name__=~"llm_savings_(added_usd|cache_effect_usd|interaction_usd)_total",direction=~"added|",'
    f'effect!="break",component=~"{_CACHE_RE}|"'
)
# Each series of SAVED_SIDE is in exactly one of REWRITE_SAVED and CACHE_SAVED, and the same holds
# for the added side, so the two sources add up to net(). dashboards_test.yml proves it.
SOURCES = {
    "Context rewrite": (REWRITE_SAVED, REWRITE_ADDED),
    "Cache management": (CACHE_SAVED, CACHE_ADDED),
}
# How far before a range inc() looks for a sample that makes a series OLD (see inc()). Matches
# Prometheus's own retention.time (deployment.yaml): a shorter lookback misclassifies a series
# that returns after a longer silence (a laptop asleep or off the VPN over a weekend, or the
# collector down for a day) as newly BORN, and bar()/day_bar() then draw its whole lifetime
# total as one bar — every Monday morning, for every machine left asleep with the proxy running
# (review of #2064). A series is OLD if it has ANY sample in the retention window before win, so
# this has to cover the window a reasonably silent machine could still be found in.
OLD_LOOKBACK = "30d"


def inc(metric, by="", win="$__range", extra="", new_series_fallback=True, anchored=False, ahead=False):
    """Total over win: sum(increase(metric{filter}[win])), optionally by a label.

    A series born inside win started at 0, but increase() does not know that: with fewer than a
    full win of history to extrapolate from, it can read far too low — 2026-11-19, a ping-cost
    counter created partway into the selected 7d range, with a flat history after that,
    extrapolated to exactly 0 (see ratio(), the other half of that incident; a counter that
    simply RESTARTS under the same labels is unaffected, since increase() already compensates
    for that reset on its own).

    So, with new_series_fallback (the default), a series is one of three cases:

    1. OLD: it has a sample in the OLD_LOOKBACK before win. It keeps its increase(): resets are
       compensated, and its absolute value holds history from before win. The lookback is long,
       not Prometheus's 5m, because a push outage at the start of win (a laptop asleep or off the
       VPN, 2026-11-19 23:11 to 06:53) leaves no sample for hours. Seen as born, its
       last_over_time() would add all the history from before win.
    2. BORN inside win: it started at 0, so its total is at least each of two lower bounds, and the
       larger is taken. last_over_time() is exact when the series did not reset inside win, but
       after a reset it holds only the part since the reset. increase() handles the reset but
       loses the first sample (it does not know the series started at 0). 2026-11-19: the rescue
       series was born at 22:35 and reset at 22:48; last_over_time() read $3.7 of about $5.0,
       increase() $4.5.
    3. Neither bound is exact for a born series that also reset: the first sample stays lost.
       That is an undercount, the safe direction for a saving.

    new_series_fallback=False keeps the old, increase()-only query. Use it only where the
    returned text is itself rewritten by later string substitution, because the fallback's own
    "offset {win}" (outside any [brackets]) would not survive a substitution keyed on "[{win}]"
    (fleet()'s "Net saved per day" bars, which pick their window this way).

    anchored=True adds Prometheus's anchored modifier to the increase() (see count_bar()).

    ahead=True moves win to START at the evaluation time, not end there (see day_bar()). A sample
    before win is then any sample at or before the evaluation time, so the OLD check has no offset."""
    g = f" by ({by})" if by else ""
    sel = f"{metric}{{{FILTER}{extra}}}"
    if not new_series_fallback:
        return f"sum{g}(increase({sel}[{win}]))"
    shift = f" offset -{win}" if ahead else ""
    grew = f"increase({sel}[{win}]{' anchored' if anchored else ''}{shift})"
    last = f"last_over_time({sel}[{win}]{shift})"
    old = f"last_over_time({sel}[{OLD_LOOKBACK}]{'' if ahead else f' offset {win}'})"
    return f"sum{g}(({grew} and {old}) or ({grew} >= {last}) or {last})"


# A dollar counter moves in steps: one event is one step. rate() * 3600 over a ~1 min window turns a
# $1.75 step into a $105/h spike for one minute (context-guru #1273). So money over time is shown as
# the dollars in each bar: increase() over $__interval, with a floor of BAR so each bar spans several
# 30 s pushes. inc() keeps its new-series fallback here: a keep-alive series is born with its first
# ping already in it, and increase() alone would lose that ping. The increase() is anchored, for the
# reasons in count_bar(): without it a $1.75 ping read as $2.19, and a ping between two bars was lost.
BAR = "5m"
BAR_DESC = (
    " Each bar is the dollars in that bar's time span. The span grows with the dashboard range"
    " and is at least 5 minutes."
)


def bar(metric, by="", extra=""):
    """The dollars in each bar: inc() anchored over $__interval. Use with series(..., bars=True, interval=BAR)."""
    return inc(metric, by, "$__interval", extra, anchored=True)


# An event counter (misses, pings, requests, tokens) has the same problem as a dollar counter, and
# one of its own: rate() over $__rate_interval (about 90 s) is wider than a bar (about 30 s at 1h), so
# one miss drew three bars of 40/h each. And plain increase() over $__interval is still wrong for a
# count: it extrapolates one miss to 1.11, and it loses a step that falls between the last sample
# of one bar and the first sample of the next (2026-11-22, a miss at 17:45:30 was in neither bar).
# The anchored modifier (Prometheus 3.7 or later) takes the step from the last sample before the
# bar, with no extrapolation, so each bar is the exact count in its span and the bars add up to the
# total. That covers an OLD series. A series born inside the bar can arrive already holding its
# first event, with no 0 before it (2026-11-22, a ttl_expiry series born at 17:29:29 holding 1):
# anchored has nothing to take the step from, so it loses that event, as rate() did. inc()'s
# new-series fallback counts it.
COUNT_DESC = (
    " Each bar is the count in that bar's time span. The span grows with the dashboard range and is at least 5 minutes."
)


def count_bar(metric, by="", extra=""):
    """The exact count in each bar: bar(), which is inc() anchored over $__interval. Use with series(..., bars=True, interval=BAR)."""
    return bar(metric, by, extra)


def day_bar(metric, by="", extra=""):
    """The dollars of each UTC day: inc() anchored over the day that STARTS at the bar. Use with
    series(..., bars=True, interval="1d"). The 1d step puts one point at 00:00 UTC of each day, and
    a window that ends there is the PREVIOUS day: the bar marked 11-22 held 11-21, and today never
    showed (2026-11-22, Cache effects). ahead=True makes each bar its own day, and today's bar the
    total so far, as "Net saved per day" does with "offset -1d"."""
    return inc(metric, by, "1d", extra, anchored=True, ahead=True)


def rate(metric, by="", extra=""):
    g = f" by ({by})" if by else ""
    return f"sum{g}(rate({metric}{{{FILTER}{extra}}}[$__rate_interval]))"


def saved_side(by="", fn="inc", new_series_fallback=True):
    """The saved side of the net: the operations' saved, the cache effects' saved and the
    interaction's saved. Only for a by in NET_LABELS."""
    assert by in NET_LABELS, by
    return (
        inc("", by, extra=SAVED_SIDE, new_series_fallback=new_series_fallback)
        if fn == "inc"
        else bar("", by, SAVED_SIDE)
    )


def added_side(by="", fn="inc", new_series_fallback=True):
    """The added side of the net: the operations' added (a keep-alive ping is a spend there), the
    cache effects' added (a break, a premium) and the interaction's added."""
    assert by in NET_LABELS, by
    return (
        inc("", by, extra=ADDED_SIDE, new_series_fallback=new_series_fallback)
        if fn == "inc"
        else bar("", by, ADDED_SIDE)
    )


def ratio(num, den):
    """num / den, with NO DATA where den is 0. Never divide by clamp_min(den, 1e-9): a zero den then
    shows num times a billion (2026-11-19: "Pings that paid" read 1164430250x). Never by
    clamp_min(den, 1) either: no traffic then reads 0 %, which looks like a measured value.
    "> 0" drops the zero series, so the division has nothing to match and returns no sample."""
    return f"{num} / (({den}) > 0)"


def unattributed(expr, label):
    """expr with an empty label filled in as "unattributed". A by-label panel otherwise names a
    series without the label "Value": cortex builds before feat/ping-miss-labels push
    llm_cache_misses_total with no cause and llm_keepalive_pings_total with no outcome, and
    Prometheus keeps their series for 30 days."""
    return f'label_replace({expr}, "{label}", "unattributed", "{label}", "")'


def spend(by=""):
    """Total priced spend: llm_inference_cost_usd_total. Under the shared metrics contract
    (context-guru docs/metrics/spec.md 5.1) cost_usd is every request's whole price, and
    llm_inference_estimated_cost_usd_total is the part of it priced from a fallback rate: a
    subset, never an addition. Adding the two would count the fallback-priced dollars twice.
    inference.json's "Spend priced from a fallback rate" panel divides the subset by this total."""
    return inc("llm_inference_cost_usd_total", by)


def net(by="", fn="inc", new_series_fallback=True):
    """The net saving, saved side minus added side, per label value. For a by in NET_LABELS, both
    sides include the cache effects and the interaction (accountant docs/metrics/metrics.md, "The net
    saving"): leaving them out shows a keep-alive ping as a cost and never its rescue. For any other
    by (component, kind), a cache effect has no such label, so the net is the operations' alone.

    PromQL drops a label value that is on one side only, and a component that only ever ADDS cost
    (a compaction call, say) is exactly the one this must show. So each side is padded with the
    other side's label values times zero. The total (no by) is padded the same way: a fleet whose
    only activity so far is keep-alive pings has an added side and no saved side, and an unpadded
    difference shows that as no data instead of a cost. new_series_fallback is forwarded to inc()
    (ignored when fn="bar", which always keeps it); see inc()'s own doc comment."""
    if by in NET_LABELS:
        s = saved_side(by, fn, new_series_fallback)
        a = added_side(by, fn, new_series_fallback)
    else:
        side = (
            (lambda m: inc(m, by, new_series_fallback=new_series_fallback)) if fn == "inc" else (lambda m: bar(m, by))
        )
        s, a = side("llm_savings_saved_usd_total"), side("llm_savings_added_usd_total")
    return f"((({s}) or ({a}) * 0) - (({a}) or ({s}) * 0))"


def source_net(source):
    """The net saving of one source in SOURCES, saved side minus added side. Each side is padded with
    the other side times zero, as net() pads a label value: a source with only an added side (the
    keep-alive's pings before their first rescue) must show as a cost, not as no data."""
    saved, added = SOURCES[source]
    s, a = inc("", extra=saved), inc("", extra=added)
    return f"((({s}) or ({a}) * 0) - (({a}) or ({s}) * 0))"


def baseline():
    """What the traffic would have cost without the proxy: spend plus net saved. The exact counter
    (llm_savings_baseline_usd_total) is not built yet."""
    return f"{spend()} + {net()}"


def by_source(fn, total):
    """One series per source, and the total, each with a source label, for a table row each."""
    rows = [(name, fn(name)) for name in SOURCES] + [("Total", total)]
    return " or ".join(f'label_replace({e}, "source", "{name}", "", "")' for name, e in rows)


def color(c):
    return {"mode": "fixed", "fixedColor": c}


def stat(pid, title, expr, x, y, w=4, h=4, unit="short", desc="", decimals=None, c=None, big=False):
    d = {
        "id": pid,
        "type": "stat",
        "title": title,
        "description": desc,
        "datasource": DS,
        "gridPos": {"x": x, "y": y, "w": w, "h": h},
        "targets": [{"expr": expr, "refId": "A", "instant": True, "datasource": DS}],
        "fieldConfig": {"defaults": {"unit": unit, "noValue": "no data"}, "overrides": []},
        "options": {
            "reduceOptions": {"calcs": ["lastNotNull"]},
            "colorMode": "value",
            "graphMode": "none",
            "textMode": "value",
        },
    }
    if decimals is not None:
        d["fieldConfig"]["defaults"]["decimals"] = decimals
    if c:
        d["fieldConfig"]["defaults"]["color"] = color(c)
    if big:
        d["options"]["text"] = {"valueSize": 56}
    return d


def stat_with_rate(pid, title, expr, rate_expr, x, y, w, h, value_name, desc="", c=None):
    """A big money stat with its rate beside it: target A in USD, target B as a percent. Each value is
    shown with its name, so the two are not read as one number."""
    d = stat(pid, title, expr, x, y, w=w, h=h, unit=USD, desc=desc, decimals=2, c=c, big=True)
    d["targets"].append({"expr": rate_expr, "refId": "B", "instant": True, "datasource": DS})
    d["options"]["textMode"] = "value_and_name"
    d["options"]["text"]["titleSize"] = 14
    d["fieldConfig"]["overrides"] = [
        {"matcher": {"id": "byFrameRefID", "options": "A"}, "properties": [{"id": "displayName", "value": value_name}]},
        {
            "matcher": {"id": "byFrameRefID", "options": "B"},
            "properties": [
                {"id": "displayName", "value": "of baseline"},
                {"id": "unit", "value": "percentunit"},
                {"id": "decimals", "value": 1},
            ],
        },
    ]
    return d


def series(
    pid,
    title,
    targets,
    x,
    y,
    w=12,
    h=8,
    unit="short",
    stack=False,
    desc="",
    kind="timeseries",
    colors=None,
    bars=False,
    vertical=False,
    instant=False,
    time_from=None,
    interval=None,
    fixed_color=None,
    lines=(),
):
    d = {
        "id": pid,
        "type": kind,
        "title": title,
        "description": desc,
        "datasource": DS,
        "gridPos": {"x": x, "y": y, "w": w, "h": h},
        "targets": [
            {"expr": e, "legendFormat": legend, "refId": chr(65 + i), "datasource": DS}
            for i, (e, legend) in enumerate(targets)
        ],
        "fieldConfig": {
            "defaults": {"unit": unit, "custom": {"stacking": {"mode": "normal" if stack else "none"}}},
            "overrides": [],
        },
        "options": {"legend": {"displayMode": "table", "placement": "bottom"}},
    }
    if instant:
        for t in d["targets"]:
            t["instant"] = True
    if interval:
        for t in d["targets"]:
            t["interval"] = interval
    if time_from:
        d["timeFrom"] = time_from
    if bars:
        # increase() and rate() at t cover the span that ENDS at t, so the bar ends at t, not centered on it
        d["fieldConfig"]["defaults"]["custom"].update(
            {"drawStyle": "bars", "fillOpacity": 80, "lineWidth": 0, "barAlignment": -1}
        )
    if kind == "bargauge":
        d["options"] = {
            "orientation": "vertical" if vertical else "horizontal",
            "displayMode": "basic",
            "showUnfilled": True,
            "reduceOptions": {"calcs": ["lastNotNull"]},
        }
        # a bar below zero is a loss: red. Above zero: green. Colors by value, not by series.
        d["fieldConfig"]["defaults"]["color"] = {"mode": "thresholds"}
        d["fieldConfig"]["defaults"]["thresholds"] = {
            "mode": "absolute",
            "steps": [{"color": RED, "value": None}, {"color": GREEN, "value": 0}],
        }
        # A bar that is a cost, not a net saving, has no good side of zero: one fixed colour.
        if fixed_color:
            d["fieldConfig"]["defaults"]["color"] = color(fixed_color)
    if kind == "barchart":
        d["options"] = {"legend": {"displayMode": "hidden"}, "xTickLabelRotation": 0}
        d["fieldConfig"]["defaults"]["color"] = color(GREEN)
    for name, c in (colors or {}).items():
        d["fieldConfig"]["overrides"].append(
            {"matcher": {"id": "byName", "options": name}, "properties": [{"id": "color", "value": color(c)}]}
        )
    # a total drawn over stacked bars: a line, outside the stack, so it does not add to it
    for name in lines:
        d["fieldConfig"]["overrides"].append(
            {
                "matcher": {"id": "byName", "options": name},
                "properties": [
                    {"id": "custom.drawStyle", "value": "line"},
                    {"id": "custom.lineWidth", "value": 2},
                    {"id": "custom.fillOpacity", "value": 0},
                    {"id": "custom.stacking", "value": {"mode": "none"}},
                ],
            }
        )
    return d


def table(pid, title, targets, x, y, w, h, join, transforms, desc="", unit_by_col=None):
    """Instant queries that share a label are joined into one row each."""
    d = {
        "id": pid,
        "type": "table",
        "title": title,
        "description": desc,
        "datasource": DS,
        "gridPos": {"x": x, "y": y, "w": w, "h": h},
        "targets": [
            {"expr": e, "refId": chr(65 + i), "datasource": DS, "instant": True, "format": "table"}
            for i, e in enumerate(targets)
        ],
        "transformations": [{"id": "joinByField", "options": {"byField": join, "mode": "outer"}}] + transforms,
        "fieldConfig": {
            "defaults": {},
            "overrides": [
                {
                    "matcher": {"id": "byName", "options": col},
                    "properties": [{"id": "unit", "value": u}, {"id": "decimals", "value": 2}],
                }
                for col, u in (unit_by_col or {}).items()
            ],
        },
        "options": {"showHeader": True},
    }
    return d


def text(pid, title, md, x, y, w, h):
    return {
        "id": pid,
        "type": "text",
        "title": title,
        "gridPos": {"x": x, "y": y, "w": w, "h": h},
        "options": {"mode": "markdown", "content": md},
    }


def dash(uid, title, desc, panels, tags):
    return {
        "uid": uid,
        "title": title,
        "description": desc,
        "tags": ["central"] + tags,
        "schemaVersion": 39,
        "version": 1,
        "editable": False,
        "refresh": "1m",
        "time": {"from": "now-7d", "to": "now"},
        "timezone": "browser",
        "templating": {
            "list": [
                var("proxy", "Proxy", "label_values(llm_proxy_info, proxy)"),
                var("machine", "Machine", 'label_values(llm_proxy_info{proxy=~"$proxy"}, machine)'),
            ]
        },
        "panels": panels,
    }


def source_table(pid, x, y, w, h):
    """NET SAVED split by source: one row per source in SOURCES and a Total row, each with its net in
    USD and its share of the baseline. The rows add up to the Total, and the Total is NET SAVED. Rows
    sort by name, which puts Total last."""
    d = table(
        pid,
        "Net saved by source",
        [by_source(source_net, net()), by_source(lambda s: ratio(source_net(s), baseline()), ratio(net(), baseline()))],
        x,
        y,
        w,
        h,
        "source",
        [
            {
                "id": "organize",
                "options": {
                    "excludeByName": {"Time": True},
                    "renameByName": {"Value #A": "Net saved", "Value #B": "Of baseline"},
                },
            },
            {"id": "sortBy", "options": {"sort": [{"field": "source"}]}},
        ],
        desc="Where NET SAVED comes from, as context-guru's dashboard splits it. "
        "Context rewrite: every operator that rewrites the request (its content, its tool declarations, its model), net of its own model calls and of the cache breaks the changes cause. "
        "Cache management: the keep-alive (its rescues, net of its pings), the prefix-cache split, the 1h head TTL and the breakpoints the proxy adds. "
        "Of baseline: each net as a share of what the traffic would have cost without the proxy (spend plus net saved); the two sources add up to the Total. "
        'For proxy="context-guru", the split is right only for builds with rossoctl/context-guru#1362: an older build pushes the keep-alive, head-TTL and breakpoint operators as component="other", which counts under Context rewrite. The Total is right for every build.',
    )
    # A loss reads red, a saving green, in both value columns. The source column keeps the text colour.
    value = [
        {"id": "custom.cellOptions", "value": {"type": "color-text"}},
        {"id": "color", "value": {"mode": "thresholds"}},
        {
            "id": "thresholds",
            "value": {"mode": "absolute", "steps": [{"color": RED, "value": None}, {"color": GREEN, "value": 0}]},
        },
    ]
    d["fieldConfig"]["overrides"] = [
        {
            "matcher": {"id": "byName", "options": "Net saved"},
            "properties": [{"id": "unit", "value": USD}, {"id": "decimals", "value": 2}] + value,
        },
        {
            "matcher": {"id": "byName", "options": "Of baseline"},
            "properties": [{"id": "unit", "value": "percentunit"}, {"id": "decimals", "value": 1}] + value,
        },
    ]
    return d


def fleet():
    f = FILTER
    P = []
    # Money first (spec 6.0, rule 1). The tiles cover the selected time range (default: 7 days).
    # Two money tiles: the headline (net and its rate), and the same split by source.
    P.append(
        stat_with_rate(
            1,
            "NET SAVED",
            net(),
            ratio(net(), baseline()),
            0,
            0,
            8,
            6,
            "net saved",
            c=GREEN,
            desc="Saved minus added over the selected range, cache effects included: a keep-alive rescue is saved, a cache break is added. This is the number that says whether the proxy pays. "
            "Beside it, the net as a share of the baseline: what the traffic would have cost without the proxy, which is spend (actual plus estimated) plus net saved. "
            "The exact baseline counter (llm_savings_baseline_usd_total) is not built yet.",
        )
    )
    P.append(source_table(14, 8, 0, 8, 6))
    P.append(
        stat(
            2,
            "Gross saved",
            saved_side(),
            16,
            0,
            w=4,
            h=3,
            unit=USD,
            decimals=2,
            desc="Cost the proxy removed, before what it cost to do so: the operations' saving, the cache effects that lowered the bill (a keep-alive rescue, a split) and the interaction.",
        )
    )
    P.append(
        stat(
            3,
            "Added cost",
            added_side(),
            20,
            0,
            w=4,
            h=3,
            unit=USD,
            decimals=2,
            c=ORANGE,
            desc="What the proxy's own work cost: its model calls, its keep-alive pings, the cache writes it caused, and the cache effects that raised the bill (a break, a breakpoint premium).",
        )
    )
    P.append(
        stat(
            4,
            "Spend",
            spend(),
            16,
            3,
            w=8,
            h=3,
            unit=USD,
            decimals=2,
            desc="What the providers billed over the selected range: actual (a configured or confirmed rate) plus estimated (a bundled/fallback rate, when no pricing.endpoints entry matches the gateway). Each proxy prices with its own rate table.",
        )
    )
    # A timeseries in bar mode, not a barchart: Grafana's barchart draws nothing for a single row,
    # which is what a fleet younger than two days returns.
    # The 1d step puts one point at 00:00 UTC of each day. A plain [1d] window there ends at that
    # point, so it is the PREVIOUS day's total, and today would never show. "offset -1d" moves the
    # window to start at the point: each bar is its own day, and today's bar is the total so far.
    p5 = series(
        5,
        "Net saved per day",
        [(net(fn="inc", new_series_fallback=False).replace("[$__range]", "[1d] anchored offset -1d"), "net")],
        0,
        6,
        24,
        7,
        unit=USD,
        bars=True,
        time_from="30d",
        interval="1d",
        desc="One bar per UTC day, for 30 days whatever the dashboard range. Today's bar is the total so far. A bar below zero is a day the proxy cost more than it saved.",
    )
    # A bar below zero is a loss: red. Above zero: green. Colors by value, as bargauge does.
    p5["fieldConfig"]["defaults"]["color"] = {"mode": "thresholds"}
    p5["fieldConfig"]["defaults"]["thresholds"] = {
        "mode": "absolute",
        "steps": [{"color": RED, "value": None}, {"color": GREEN, "value": 0}],
    }
    p5["fieldConfig"]["defaults"]["custom"]["gradientMode"] = "scheme"
    p5["options"]["legend"]["showLegend"] = False
    P.append(p5)
    P.append(
        series(
            6,
            "Saved vs added (USD)",
            [(saved_side(fn="bar"), "saved"), (f"-{added_side(fn='bar')}", "added")],
            0,
            13,
            12,
            8,
            unit=USD,
            stack=True,
            bars=True,
            interval=BAR,
            colors={"saved": GREEN, "added": ORANGE},
            desc="Both sides of the net, cache effects included: saved above zero, added below." + BAR_DESC,
        )
    )
    P.append(
        series(
            7,
            "Net saved by machine",
            [(f"topk(8, {net('machine')})", "{{machine}}")],
            12,
            13,
            12,
            8,
            unit=USD,
            kind="bargauge",
            instant=True,
            desc="The 8 best machines over the selected range. The full list is on the Machine comparison dashboard.",
        )
    )
    P.append(
        stat(
            8,
            "Machines reporting",
            f"count(count by (machine) (llm_proxy_info{{{f}}}))",
            0,
            21,
            w=5,
            h=4,
            desc="Machines that pushed in the last few minutes. llm_proxy_info is always 1, so an idle machine still counts.",
        )
    )
    P.append(
        stat(
            9,
            "Silent machines",
            f"count(max by (proxy, machine) (max_over_time(llm_proxy_info{{{f}}}[1d])) unless max by (proxy, machine) (llm_proxy_info{{{f}}})) or vector(0)",
            5,
            21,
            w=5,
            h=4,
            c=YELLOW,
            desc="Machines that pushed in the last day and have pushed nothing for a few minutes. The alert MachineSilent fires after 15.",
        )
    )
    P.append(
        stat(
            10,
            "5xx error rate",
            ratio(inc("llm_inference_requests_total", extra=X5), inc("llm_inference_requests_total")),
            10,
            21,
            w=5,
            h=4,
            unit="percentunit",
            decimals=2,
            c=GREEN,
            desc="Server errors only. A 4xx is usually the client's own request.",
        )
    )
    # A fixed 5m window, not $__rate_interval: that one grows with the time range, so the stat
    # would mean a different window on every zoom level.
    P.append(
        stat(
            11,
            "Requests per minute",
            f"{rate('llm_inference_requests_total').replace('$__rate_interval', '5m')} * 60",
            15,
            21,
            w=5,
            h=4,
            decimals=0,
            desc="Requests per minute over the last 5 minutes, summed across the selected proxies and machines. "
            "A request counts when it finishes, whatever its status (2xx, 4xx, 5xx). Policy denials count; tunnelled traffic does not.",
        )
    )
    P.append(
        stat(
            12,
            "Alerts firing",
            'count(ALERTS{alertstate="firing"}) or vector(0)',
            20,
            21,
            w=4,
            h=4,
            c=GREEN,
            desc="Alerts that Prometheus has in the firing state. The list is in Alertmanager.",
        )
    )
    return dash(
        "central-fleet",
        "Fleet overview",
        "Do we save money, and is the fleet healthy? Tokens are the same for every proxy. Money is each proxy's own rate table.",
        P,
        ["fleet"],
    )


def savings():
    P = []
    P.append(
        series(
            1,
            "Net by component (USD, selected range)",
            [(net("component"), "{{component}}")],
            0,
            0,
            9,
            9,
            unit=USD,
            kind="bargauge",
            instant=True,
            desc="Saved minus added of each component's operations. Cache effects are not in it: they have no component label (see Cache effects). So a keep-alive rescue is not here, and the keeper shows only its ping cost: its whole net is under Cache management in the Fleet dashboard's Net saved by source. A red bar is a component whose operations cost more than they save.",
        )
    )
    P.append(
        series(
            2,
            "Net by kind (USD)",
            [(net("kind", fn="bar"), "{{kind}}")],
            9,
            0,
            15,
            9,
            unit=USD,
            stack=True,
            bars=True,
            interval=BAR,
            desc="rewrite, spend and setting, operations only. A spend is a component's own model call or a keep-alive ping, and is always a cost. Cache effects have no kind: see Cache effects."
            + BAR_DESC,
        )
    )
    P.append(
        series(
            3,
            "Saved tokens by tier (selected range)",
            [(inc("llm_savings_saved_tokens_total", "tier"), "{{tier}}")],
            0,
            9,
            7,
            8,
            kind="bargauge",
            vertical=True,
            instant=True,
            desc="Tokens the provider did not bill. The tier decides the price of each token.",
        )
    )
    P.append(
        series(
            4,
            "Cache effects (USD per day)",
            [
                (day_bar("llm_savings_cache_effect_usd_total", "effect", DIR_SAVED), "{{effect}} saved"),
                (f"-{day_bar('llm_savings_cache_effect_usd_total', 'effect', DIR_ADDED)}", "{{effect}} added"),
            ],
            7,
            9,
            9,
            8,
            unit=USD,
            stack=True,
            bars=True,
            interval="1d",
            desc="How the cache changed the bill, saved above zero and added below. One bar per UTC day; today's bar is the total so far. These are not in the operations' saved or added counters. NET SAVED on the Fleet dashboard adds them.",
        )
    )
    P.append(
        series(
            5,
            "Break cost by cause",
            [
                (
                    inc("llm_savings_cache_effect_usd_total", "cause", extra=',effect="break",direction="added"'),
                    "{{cause}}",
                )
            ],
            16,
            9,
            8,
            8,
            unit=USD,
            kind="bargauge",
            instant=True,
            desc="What cache breaks cost, by why they happened.",
        )
    )
    P.append(
        stat(
            6,
            "Estimated share",
            ratio(inc("llm_savings_estimated_total"), inc("llm_savings_requests_total")),
            0,
            17,
            w=4,
            h=8,
            unit="percentunit",
            decimals=1,
            c=YELLOW,
            desc="Share of requests whose figure is an estimate. Above 20 percent, read the headline as a range.",
        )
    )
    P.append(
        series(
            7,
            "Unpriced requests by reason (per minute)",
            [(f"{rate('llm_savings_unpriced_total', 'reason')} * 60", "{{reason}}")],
            4,
            17,
            9,
            8,
            stack=True,
            desc="Operations the accountant did not price, and why.",
        )
    )
    P.append(
        table(
            8,
            "Components with a negative net",
            [inc("llm_savings_saved_usd_total", "component"), inc("llm_savings_added_usd_total", "component")],
            13,
            17,
            11,
            8,
            "component",
            [
                {
                    "id": "calculateField",
                    "options": {
                        "mode": "binary",
                        "alias": "net",
                        "binary": {"left": "Value #A", "operator": "-", "right": "Value #B"},
                        "replaceFields": False,
                    },
                },
                {
                    "id": "filterByValue",
                    "options": {
                        "type": "include",
                        "match": "all",
                        "filters": [{"fieldName": "net", "config": {"id": "lower", "options": {"value": 0}}}],
                    },
                },
                {
                    "id": "organize",
                    "options": {
                        "excludeByName": {"Time": True},
                        "renameByName": {"Value #A": "saved", "Value #B": "added"},
                    },
                },
            ],
            desc="Read this table first: a component here has operations that cost more than they save. Cache effects are not in it, so the keeper shows its pings and not its rescues: read it on the Cache dashboard. A component with no saved series at all shows no row; it appears in Net by component.",
            unit_by_col={"saved": USD, "added": USD, "net": USD},
        )
    )
    return dash(
        "central-savings", "Savings", "Where does the saving come from, and where does it leak?", P, ["savings"]
    )


def cache():
    # or vector(0): with pings and no rescue yet (or no "added" side), the difference would have no
    # series, and rescue and net would vanish while the ping cost still shows
    def rescue(e):
        return f"({bar('llm_savings_cache_effect_usd_total', extra=e)} or vector(0))"

    P = []
    P.append(
        series(
            1,
            "Cache read ratio",
            [
                (
                    ratio(
                        rate("llm_inference_tokens_total", "model", TIER_READ),
                        rate("llm_inference_tokens_total", "model", TIER_PROMPT),
                    ),
                    "{{model}}",
                )
            ],
            0,
            0,
            12,
            8,
            unit="percentunit",
            desc="cache_read / (cache_read + cache_write + input). Output tokens are not part of the prompt.",
        )
    )
    P.append(
        series(
            2,
            "Misses by cause",
            [(unattributed(count_bar("llm_cache_misses_total", "cause"), "cause"), "{{cause}}")],
            12,
            0,
            12,
            8,
            stack=True,
            bars=True,
            interval=BAR,
            desc="Cache misses, by why." + COUNT_DESC,
        )
    )
    P.append(
        series(
            3,
            "Miss cost by cause (USD, selected range)",
            [(unattributed(inc("llm_cache_miss_cost_usd_total", "cause"), "cause"), "{{cause}}")],
            0,
            8,
            8,
            8,
            unit=USD,
            kind="bargauge",
            instant=True,
            fixed_color=ORANGE,
            desc="The extra amount paid because the prefix was written again instead of read: cache-write tokens priced at "
            "the write rate minus the read rate (docs/metrics/spec.md 5.4). Output and fresh input are not in it. "
            "A model with no rate counts the miss and books no money.",
        )
    )
    P.append(
        series(
            4,
            "Ping outcomes",
            [(unattributed(count_bar("llm_keepalive_pings_total", "outcome"), "outcome"), "{{outcome}}")],
            8,
            8,
            10,
            8,
            stack=True,
            bars=True,
            interval=BAR,
            colors={"ok": GREEN, "skipped": BLUE, "failed": RED, "timeout": ORANGE},
            desc="Pings, by outcome. A timeout is a ping that ran to its 60 s limit; it is booked at its projected cost (#754). A skipped ping leaves no row, so it does not show. unattributed is an older cortex build, which did not label the outcome."
            + COUNT_DESC,
        )
    )
    P.append(
        stat(
            5,
            "Pings that paid",
            ratio(
                f"({inc('llm_savings_cache_effect_usd_total', extra=RESCUE_SAVED)} - {inc('llm_savings_cache_effect_usd_total', extra=RESCUE_ADDED)})",
                inc("llm_keepalive_cost_usd_total"),
            ),
            18,
            8,
            w=6,
            h=4,
            unit="suffix: x",
            decimals=1,
            c=GREEN,
            big=True,
            desc="Rescue value divided by ping cost over the selected range. Above 1, the pings paid. No data means the ping cost did not increase in the range.",
        )
    )
    P.append(
        stat(
            8,
            "Estimated ping cost",
            ratio(inc("llm_keepalive_estimated_cost_usd_total"), inc("llm_keepalive_cost_usd_total")),
            18,
            12,
            w=6,
            h=4,
            unit="percentunit",
            decimals=1,
            c=ORANGE,
            desc="Share of the ping cost that is not a measured figure. Each proxy books a different part here. context-guru: pings that timed out with no answer, booked at their projected cost (#754). cortex: pings priced from a fallback rate, because the model has no rate of its own, and pings that timed out, booked at the keeper's projected cost. No data means the ping cost did not increase in the range.",
        )
    )
    P.append(
        series(
            6,
            "Keep-alive: cost, rescue and net (USD)",
            [
                (f"-{bar('llm_keepalive_cost_usd_total')}", "ping cost"),
                (f"({rescue(RESCUE_SAVED)} - {rescue(RESCUE_ADDED)})", "rescue (net of reads)"),
                (f"(({rescue(RESCUE_SAVED)} - {rescue(RESCUE_ADDED)}) - {bar('llm_keepalive_cost_usd_total')})", "net"),
            ],
            0,
            16,
            24,
            8,
            unit=USD,
            stack=True,
            bars=True,
            interval=BAR,
            lines=("net",),
            colors={"ping cost": ORANGE, "rescue (net of reads)": GREEN, "net": BLUE},
            desc="Do the pings pay? The rescue is a write that became a read, so its value is the write saved minus the read paid. Ping cost is below zero, rescue above, and the line is the net."
            + BAR_DESC,
        )
    )
    return dash("central-cache", "Cache and keep-alive", "Does the cache pay, and do pings pay?", P, ["cache"])


def machines():
    f = FILTER

    def per1k(by):
        return ratio(f"1000 * {net(by)}", inc("llm_inference_requests_total", by))

    P = []
    P.append(
        series(
            1,
            "Net saved by machine (USD, selected range)",
            [(f"sort_desc({net('machine')})", "{{machine}}")],
            0,
            0,
            10,
            12,
            unit=USD,
            kind="bargauge",
            instant=True,
            desc="Every machine, best first.",
        )
    )
    P.append(
        table(
            2,
            "By preset",
            [
                f"count by (preset) (count by (preset, machine) (llm_proxy_info{{{f}}}))",
                net("preset"),
                ratio(net("preset"), f"{spend('preset')} + {net('preset')}"),
                per1k("preset"),
                ratio(inc("llm_savings_estimated_total", "preset"), inc("llm_savings_requests_total", "preset")),
            ],
            10,
            0,
            14,
            12,
            "preset",
            [
                {
                    "id": "organize",
                    "options": {
                        "excludeByName": {"Time": True},
                        "renameByName": {
                            "Value #A": "machines",
                            "Value #B": "net saved",
                            "Value #C": "saving rate",
                            "Value #D": "net per 1,000 requests",
                            "Value #E": "estimated",
                        },
                    },
                }
            ],
            desc="Saving rate is net saved divided by spend (actual plus estimated) plus net saved (the exact baseline counter is not built). Net per 1,000 requests compares a busy machine with a quiet one.",
            unit_by_col={
                "net saved": USD,
                "saving rate": "percentunit",
                "net per 1,000 requests": USD,
                "estimated": "percentunit",
            },
        )
    )
    P.append(
        series(
            3,
            "Net saved by proxy (USD, selected range)",
            [(net("proxy"), "{{proxy}}")],
            0,
            12,
            10,
            9,
            unit=USD,
            kind="bargauge",
            vertical=True,
            instant=True,
            desc="Each proxy prices with its own rate table, so compare these by tokens, not by money, when the tables differ.",
        )
    )
    P.append(
        series(
            4,
            "Net saved per 1,000 requests, by machine",
            [(f"sort_desc({per1k('machine')})", "{{machine}}")],
            10,
            12,
            14,
            9,
            unit=USD,
            kind="bargauge",
            vertical=True,
            instant=True,
            desc="A fair comparison between a busy machine and a quiet one.",
        )
    )
    return dash("central-machines", "Machine comparison", "Which machines or configs save the most?", P, ["machines"])


def quant(q, metric, by="", extra=""):
    """histogram_quantile(q) of a duration histogram, as a rate over the graph step."""
    g = f"le, {by}" if by else "le"
    return f"histogram_quantile({q}, sum by ({g}) (rate({metric}_bucket{{{FILTER}{extra}}}[$__rate_interval])))"


def requests():
    def cls(c):
        return f',status_class="{c}"'

    req = "llm_inference_requests_total"
    total = inc(req)
    P = []
    P.append(
        stat(
            1,
            "Requests",
            total,
            0,
            0,
            w=4,
            h=4,
            decimals=0,
            desc="Requests that reached the proxies in the selected range. Keep-alive pings and /compact are not counted.",
        )
    )
    P.append(
        stat(
            2,
            "2xx",
            ratio(inc(req, extra=cls("2xx")), total),
            4,
            0,
            w=4,
            h=4,
            unit="percentunit",
            decimals=2,
            c=GREEN,
            desc="Share of requests the provider answered with success.",
        )
    )
    P.append(
        stat(
            3,
            "4xx",
            ratio(inc(req, extra=cls("4xx")), total),
            8,
            0,
            w=4,
            h=4,
            unit="percentunit",
            decimals=2,
            c=ORANGE,
            desc="Share of requests that failed because of the request itself: bad key, too large, rate limited.",
        )
    )
    P.append(
        stat(
            4,
            "5xx",
            ratio(inc(req, extra=X5), total),
            12,
            0,
            w=4,
            h=4,
            unit="percentunit",
            decimals=2,
            c=RED,
            desc="Share of requests that failed on the provider or the proxy.",
        )
    )
    P.append(
        stat(
            5,
            "Median duration",
            quant(0.5, "llm_inference_duration_seconds").replace("$__rate_interval", "$__range"),
            16,
            0,
            w=4,
            h=4,
            unit="s",
            decimals=2,
            desc="p50 of total time, from request to last byte, over the selected range.",
        )
    )
    P.append(
        stat(
            6,
            "p95 duration",
            quant(0.95, "llm_inference_duration_seconds").replace("$__rate_interval", "$__range"),
            20,
            0,
            w=4,
            h=4,
            unit="s",
            decimals=2,
            c=YELLOW,
            desc="p95 of total time over the selected range.",
        )
    )
    P.append(
        series(
            7,
            "Requests by status",
            [(count_bar(req, "status_class"), "{{status_class}}")],
            0,
            4,
            16,
            9,
            stack=True,
            bars=True,
            interval=BAR,
            colors={"2xx": GREEN, "3xx": BLUE, "4xx": ORANGE, "5xx": RED, "none": "#8E8E8E"},
            desc="Requests, by status. 'none' is a request with no status: the client left or the connection broke."
            + COUNT_DESC,
        )
    )
    P.append(
        series(
            8,
            "Failed requests per hour",
            [(f"{rate(req, extra=cls('4xx'))} * 3600", "4xx"), (f"{rate(req, extra=X5)} * 3600", "5xx")],
            16,
            4,
            8,
            9,
            colors={"4xx": ORANGE, "5xx": RED},
            desc="The two error classes alone, so a small number is visible next to a large success count.",
        )
    )
    P.append(
        series(
            9,
            "Duration (seconds)",
            [
                (quant(0.5, "llm_inference_duration_seconds"), "p50"),
                (quant(0.95, "llm_inference_duration_seconds"), "p95"),
                (quant(0.99, "llm_inference_duration_seconds"), "p99"),
            ],
            0,
            13,
            12,
            8,
            unit="s",
            colors={"p50": GREEN, "p95": YELLOW, "p99": RED},
            desc="Total time from request to last byte. A streamed answer is slow by nature, so read this with the stream split below.",
        )
    )
    P.append(
        series(
            10,
            "Duration p95 by model (seconds)",
            [(quant(0.95, "llm_inference_duration_seconds", "model"), "{{model}}")],
            12,
            13,
            12,
            8,
            unit="s",
            desc="Which model is slow.",
        )
    )
    P.append(
        series(
            11,
            "Request size (bytes per request)",
            [(ratio(rate("llm_inference_request_bytes_total"), rate(req)), "request")],
            0,
            21,
            12,
            8,
            unit="bytes",
            colors={"request": BLUE},
            desc="Body bytes sent upstream, as forwarded, divided by requests. NO DATA YET: the proxy does not count body sizes on every path, so llm_inference_request_bytes_total is not built (design 5.1). The panel fills when it is.",
        )
    )
    P.append(
        series(
            12,
            "Response size (bytes per request)",
            [(ratio(rate("llm_inference_response_bytes_total"), rate(req)), "response")],
            12,
            21,
            12,
            8,
            unit="bytes",
            colors={"response": PURPLE},
            desc="Body bytes received from the upstream, divided by requests. NO DATA YET: llm_inference_response_bytes_total is not built.",
        )
    )
    P.append(
        series(
            13,
            "Requests by model",
            [(count_bar(req, "model"), "{{model}}")],
            0,
            29,
            16,
            8,
            stack=True,
            bars=True,
            interval=BAR,
            desc="Who is busy." + COUNT_DESC,
        )
    )
    P.append(
        series(
            14,
            "Streamed or not",
            [(count_bar(req, "stream"), "stream={{stream}}")],
            16,
            29,
            8,
            8,
            stack=True,
            bars=True,
            interval=BAR,
            desc="Streamed answers against whole answers." + COUNT_DESC,
        )
    )
    return dash(
        "central-requests",
        "Requests",
        "How many requests, how many fail, how long do they take, how big are they?",
        P,
        ["requests"],
    )


def inference():
    tok = "llm_inference_tokens_total"

    def tier(t):
        return f',tier="{t}"'

    P = []
    P.append(
        stat(
            1,
            "Input tokens",
            inc(tok, extra=tier("input")),
            0,
            0,
            w=5,
            h=4,
            decimals=0,
            desc="Fresh prompt tokens, not read from cache.",
        )
    )
    P.append(
        stat(
            2,
            "Cache read tokens",
            inc(tok, extra=tier("cache_read")),
            5,
            0,
            w=5,
            h=4,
            decimals=0,
            c=GREEN,
            desc="Prompt tokens read from the cache (cheap).",
        )
    )
    P.append(
        stat(
            3,
            "Cache write tokens",
            inc(tok, extra=',tier=~"cache_write_5m|cache_write_1h"'),
            10,
            0,
            w=5,
            h=4,
            decimals=0,
            c=ORANGE,
            desc="Prompt tokens written to the cache (a premium over input).",
        )
    )
    P.append(
        stat(
            4,
            "Output tokens",
            inc(tok, extra=tier("output")),
            15,
            0,
            w=5,
            h=4,
            decimals=0,
            c=BLUE,
            desc="Tokens the model wrote.",
        )
    )
    P.append(
        stat(
            5,
            "Cache read ratio",
            ratio(inc(tok, extra=TIER_READ), inc(tok, extra=TIER_PROMPT)),
            20,
            0,
            w=4,
            h=4,
            unit="percentunit",
            decimals=1,
            c=GREEN,
            desc="cache_read / (cache_read + cache_write + input).",
        )
    )
    P.append(
        series(
            6,
            "Tokens by tier",
            [(count_bar(tok, "tier"), "{{tier}}")],
            0,
            4,
            16,
            9,
            stack=True,
            bars=True,
            interval=BAR,
            colors={
                "input": YELLOW,
                "cache_read": GREEN,
                "cache_write_5m": ORANGE,
                "cache_write_1h": RED,
                "output": BLUE,
            },
            desc="Provider-billed tokens, by tier. Tokens are the same for every proxy, so this is comparable where money is not."
            + COUNT_DESC,
        )
    )
    P.append(
        series(
            7,
            "Tokens by model (selected range)",
            [(f"sort_desc({inc(tok, 'model')})", "{{model}}")],
            16,
            4,
            8,
            9,
            kind="bargauge",
            instant=True,
            desc="Total tokens, all tiers, per model.",
        )
    )
    P.append(
        series(
            8,
            "Time to first token (seconds)",
            [
                (quant(0.5, "llm_inference_ttfb_seconds"), "p50"),
                (quant(0.95, "llm_inference_ttfb_seconds"), "p95"),
                (quant(0.99, "llm_inference_ttfb_seconds"), "p99"),
            ],
            0,
            13,
            12,
            8,
            unit="s",
            colors={"p50": GREEN, "p95": YELLOW, "p99": RED},
            desc="Time from request to the first byte of a streamed answer. A buffered stream has no true first byte and is left out.",
        )
    )
    P.append(
        series(
            9,
            "Time to first token p95 by model (seconds)",
            [(quant(0.95, "llm_inference_ttfb_seconds", "model"), "{{model}}")],
            12,
            13,
            12,
            8,
            unit="s",
        )
    )
    P.append(
        series(
            10,
            "Output tokens per request",
            [
                (
                    ratio(
                        rate(tok, "model", tier("output")),
                        f'sum by (model) (rate(llm_inference_requests_total{{{FILTER},status_class="2xx"}}[$__rate_interval]))',
                    ),
                    "{{model}}",
                )
            ],
            0,
            21,
            12,
            8,
            desc="Not a speed. Output tokens divided by successful requests, by model. Streamed and buffered answers both count: llm_inference_tokens_total has no stream label, so a streamed-only denominator would divide every answer's tokens by part of the requests. A true tokens per second needs the time spent writing, which is not recorded.",
        )
    )
    P.append(
        series(
            11,
            "Time the proxy added (seconds)",
            [
                (quant(0.5, "llm_inference_added_seconds"), "p50"),
                (quant(0.95, "llm_inference_added_seconds"), "p95"),
                (quant(0.99, "llm_inference_added_seconds"), "p99"),
            ],
            12,
            21,
            12,
            8,
            unit="s",
            colors={"p50": GREEN, "p95": YELLOW, "p99": RED},
            desc="What the proxy's own work added on top of the upstream time (R46).",
        )
    )
    P.append(
        series(
            12,
            "Spend by model (USD)",
            [(bar("llm_inference_cost_usd_total", "model"), "{{model}}")],
            0,
            29,
            16,
            8,
            unit=USD,
            stack=True,
            bars=True,
            interval=BAR,
            desc="Provider cost at request time. Each proxy prices with its own rate table." + BAR_DESC,
        )
    )
    P.append(
        stat(
            13,
            "Spend priced from a fallback rate",
            ratio(inc("llm_inference_estimated_cost_usd_total"), inc("llm_inference_cost_usd_total")),
            16,
            29,
            w=8,
            h=8,
            unit="percentunit",
            decimals=1,
            c=YELLOW,
            big=True,
            desc="Share of spend where the model had no rate of its own. A high value means the money panels are a guess.",
        )
    )
    return dash(
        "central-inference", "Inference", "What do the models consume, and how fast do they answer?", P, ["inference"]
    )


DASHBOARDS = {
    "fleet.json": fleet,
    "savings.json": savings,
    "cache.json": cache,
    "machines.json": machines,
    "requests.json": requests,
    "inference.json": inference,
}


def main():
    out = os.path.join(os.path.dirname(os.path.abspath(__file__)), "dashboards")
    os.makedirs(out, exist_ok=True)
    stale = False
    for name, fn in DASHBOARDS.items():
        text = json.dumps(fn(), indent=2, sort_keys=False) + "\n"
        path = os.path.join(out, name)
        if "--check" in sys.argv:
            if not os.path.exists(path) or open(path).read() != text:
                print(f"out of date: {path}")
                stale = True
        else:
            open(path, "w").write(text)
            print("wrote", path)
    sys.exit(1 if stale else 0)


if __name__ == "__main__":
    main()
