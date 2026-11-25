#!/usr/bin/env python3
"""Tests for gen_dashboards.py. Standard library only:

    python3 -m unittest discover -s deploy/central/base/grafana -p 'test_*.py'

The PromQL behaviour of the ratio queries is tested by promtool in
prometheus/tests/dashboards_test.yml. These tests keep that file in step with the generator and
check rules that hold for every panel.
"""

import os
import re
import unittest

import gen_dashboards as g

HERE = os.path.dirname(os.path.abspath(__file__))
PROMTOOL_TEST = os.path.join(HERE, "..", "prometheus", "tests", "dashboards_test.yml")


def exprs():
    """(dashboard file, panel title, expr) for every query target of every dashboard."""
    for name, fn in g.DASHBOARDS.items():
        for p in fn()["panels"]:
            for t in p.get("targets", []):
                yield name, p["title"], t["expr"]


def filled(expr, rng="1h", rate_interval="5m", interval="5m"):
    """expr with the Grafana variables replaced the way dashboards_test.yml replaces them."""
    return (
        expr.replace('"$proxy"', '".*"')
        .replace('"$machine"', '".*"')
        .replace("$__range", rng)
        .replace("$__rate_interval", rate_interval)
        .replace("$__interval", interval)
    )


class RatioTest(unittest.TestCase):
    def test_ratio_gives_no_data_for_a_zero_divisor(self):
        self.assertEqual(g.ratio("a", "b"), "a / ((b) > 0)")

    def test_no_panel_clamps_a_divisor(self):
        # clamp_min(<zero>, 1e-9) turns a zero divisor into a numerator times a billion, and
        # clamp_min(<zero>, 1) hides "no traffic" as 0 %. Every ratio goes through ratio().
        bad = [(n, t) for n, t, e in exprs() if re.search(r"/\s*clamp_min\(", e)]
        self.assertEqual(bad, [], "divide with gen_dashboards.ratio(), not clamp_min")


class IncTest(unittest.TestCase):
    def test_default_falls_back_to_last_over_time_for_a_new_series(self):
        # A series born inside the window started at 0; increase() alone does not know that and
        # can undercount it (2026-11-19). The fallback is PromQL-side only: no OTLP start
        # timestamps, no new collector or Prometheus flag, no proxy change.
        m = 'm{proxy=~"$proxy", machine=~"$machine"}'
        got = g.inc("m")
        self.assertEqual(
            got,
            f"sum((increase({m}[$__range]) and last_over_time({m}[30d] offset $__range)) "
            f"or (increase({m}[$__range]) >= last_over_time({m}[$__range])) "
            f"or last_over_time({m}[$__range]))",
        )

    def test_new_series_fallback_false_keeps_the_plain_query(self):
        # fleet()'s "Net saved per day" bars pick their window by replacing "[$__range]" with
        # "[1d] offset -1d" in the returned text. The fallback's own "offset $__range" has no
        # brackets around $__range, so that replace would not reach it and the two offsets would
        # collide into invalid PromQL. new_series_fallback=False avoids generating it there.
        got = g.inc("m", new_series_fallback=False)
        self.assertEqual(got, 'sum(increase(m{proxy=~"$proxy", machine=~"$machine"}[$__range]))')
        self.assertNotIn("offset", got)

    def test_only_fleets_daily_bars_opt_out_of_the_fallback(self):
        # Every other increase()-based query must also carry the last_over_time() fallback.
        opted_out = {t for _, t, e in exprs() if "increase(" in e and "last_over_time(" not in e}
        self.assertEqual(opted_out, {"Net saved per day"}, opted_out)


class NetTest(unittest.TestCase):
    # accountant docs/metrics/metrics.md, "The net saving": the net is the operations plus the
    # cache effects plus the interaction. Leaving the effects out shows a ping as pure cost.
    EFFECTS = ("cache_effect_usd", "interaction_usd")

    def test_net_includes_effects_for_every_label_they_carry(self):
        for by in g.NET_LABELS:
            got = g.net(by)
            for m in self.EFFECTS:
                self.assertIn(m, got, by)

    def test_net_by_component_or_kind_is_operations_only(self):
        # A cache effect has no component or kind label, so it cannot be split by them.
        for by in ("component", "kind"):
            got = g.net(by)
            for m in self.EFFECTS:
                self.assertNotIn(m, got, by)

    def test_the_fleet_headline_uses_the_full_net(self):
        p = [p for p in g.fleet()["panels"] if p["title"] == "NET SAVED"][0]
        self.assertEqual(p["targets"][0]["expr"], g.net())


def matches(selector, labels):
    """Whether a series with labels (which holds __name__) matches selector, a string of
    ,name<op>"value" matchers as gen_dashboards writes them. A missing label is the empty string,
    and a regex is fully anchored, as in Prometheus."""
    for name, op, value in re.findall(r'(\w+)(=~|!~|!=|=)"([^"]*)"', selector):
        got = labels.get(name, "")
        ok = {
            "=": got == value,
            "!=": got != value,
            "=~": re.fullmatch(value, got) is not None,
            "!~": re.fullmatch(value, got) is None,
        }[op]
        if not ok:
            return False
    return True


class SourceTest(unittest.TestCase):
    # One series of every shape the net holds. An operation has a component and a kind, and no
    # direction; a cache effect has an effect, a cause and a direction; the interaction has a direction.
    OPERATORS = ("mask", "summarize", "toolfilter", "tool-prune", "model") + g.CACHE_COMPONENTS
    EFFECTS = ("break", "rescue", "split", "ttl_premium", "ttl_rescue", "mark_premium", "mark_payback")

    def series(self):
        for side in ("saved", "added"):
            for c in self.OPERATORS:
                for kind in ("rewrite", "spend", "setting"):
                    yield side, {"__name__": f"llm_savings_{side}_usd_total", "component": c, "kind": kind}
            for e in self.EFFECTS:
                yield side, {"__name__": "llm_savings_cache_effect_usd_total", "effect": e, "direction": side}
            yield side, {"__name__": "llm_savings_interaction_usd_total", "direction": side}

    def source_of(self, side, labels):
        i = 0 if side == "saved" else 1
        return [name for name, sel in g.SOURCES.items() if matches(sel[i], labels)]

    def test_every_series_of_the_net_is_in_exactly_one_source_on_its_own_side(self):
        for side, labels in self.series():
            net_side = g.SAVED_SIDE if side == "saved" else g.ADDED_SIDE
            self.assertTrue(matches(net_side, labels), labels)
            self.assertEqual(len(self.source_of(side, labels)), 1, (side, labels))
            other = "added" if side == "saved" else "saved"
            self.assertEqual(self.source_of(other, labels), [], ("wrong side", side, labels))

    def test_breaks_are_context_rewrite_and_the_cache_operators_are_cache_management(self):
        brk = {"__name__": "llm_savings_cache_effect_usd_total", "effect": "break", "direction": "added"}
        self.assertEqual(self.source_of("added", brk), ["Context rewrite"])
        for c in g.CACHE_COMPONENTS:
            op = {"__name__": "llm_savings_added_usd_total", "component": c, "kind": "spend"}
            self.assertEqual(self.source_of("added", op), ["Cache management"], c)
        interaction = {"__name__": "llm_savings_interaction_usd_total", "direction": "saved"}
        self.assertEqual(self.source_of("saved", interaction), ["Cache management"])

    def test_an_other_operator_is_context_rewrite(self):
        # The accountant's sink pushes an operator outside its allow-list as "other". An old
        # context-guru build pushes keeper, headttl and tailbreakpoint that way (the table's
        # description says so); pin where those series land.
        for side, metric in (("saved", "llm_savings_saved_usd_total"), ("added", "llm_savings_added_usd_total")):
            for kind in ("rewrite", "spend", "setting"):
                op = {"__name__": metric, "component": "other", "kind": kind}
                self.assertEqual(self.source_of(side, op), ["Context rewrite"], (side, kind))

    def test_the_headline_shows_the_rate_and_no_tile_of_its_own_does(self):
        panels = {p["title"]: p for p in g.fleet()["panels"]}
        self.assertNotIn("Saving rate", panels)
        head = panels["NET SAVED"]
        self.assertEqual([t["expr"] for t in head["targets"]], [g.net(), g.ratio(g.net(), g.baseline())])
        rate = next(o for o in head["fieldConfig"]["overrides"] if o["matcher"]["options"] == "B")
        self.assertIn({"id": "unit", "value": "percentunit"}, rate["properties"])

    def test_the_source_table_is_tested(self):
        with open(PROMTOOL_TEST) as f:
            tested = f.read()
        p = next(p for p in g.fleet()["panels"] if p["title"] == "Net saved by source")
        for t in p["targets"]:
            self.assertIn(filled(t["expr"]), tested, t["refId"])
        self.assertEqual(p["transformations"][0]["options"]["byField"], "source")


class PromtoolTestInStepTest(unittest.TestCase):
    def test_every_promtool_expr_is_a_dashboard_expr(self):
        with open(PROMTOOL_TEST) as f:
            tested = re.findall(r"^\s*expr: '(.*)'\s*$", f.read(), re.M)
        self.assertTrue(tested, "no expr found in dashboards_test.yml")
        written = {filled(e) for _, _, e in exprs()}
        stale = [e for e in tested if e not in written]
        self.assertEqual(stale, [], "dashboards_test.yml tests a query that no dashboard has")

    def test_the_keepalive_stats_are_tested(self):
        with open(PROMTOOL_TEST) as f:
            tested = f.read()
        for _, title, e in exprs():
            if title in ("Pings that paid", "Estimated ping cost", "Ping failure ratio"):
                self.assertIn(filled(e), tested, title)


class MoneyTrendTest(unittest.TestCase):
    def test_no_money_panel_is_a_rate_times_3600(self):
        # A dollar counter moves in steps. rate() * 3600 over a ~1 min window shows one $1.75 ping
        # as a $105/h spike (context-guru #1273). A money trend is bar(): the dollars in each bar.
        bad = [
            p["title"]
            for fn in g.DASHBOARDS.values()
            for p in fn()["panels"]
            if p.get("fieldConfig", {}).get("defaults", {}).get("unit") == g.USD
            and any("* 3600" in t["expr"] for t in p.get("targets", []))
        ]
        self.assertEqual(bad, [], "show money over time with gen_dashboards.bar(), not rate() * 3600")

    def test_the_keepalive_bars_are_tested(self):
        with open(PROMTOOL_TEST) as f:
            tested = f.read()
        p = next(p for p in g.cache()["panels"] if p["title"] == "Keep-alive: cost, rescue and net (USD)")
        for t in p["targets"]:
            self.assertIn(filled(t["expr"]), tested, t["legendFormat"])


class CountTrendTest(unittest.TestCase):
    def test_no_count_bars_are_a_rate_times_3600(self):
        # rate() over $__rate_interval (~90 s) is wider than a bar (~30 s at 1h), so one miss drew
        # three bars of 40/h (2026-11-22). A count over time is count_bar(): the count in each bar.
        bad = [
            p["title"]
            for fn in g.DASHBOARDS.values()
            for p in fn()["panels"]
            if p.get("fieldConfig", {}).get("defaults", {}).get("custom", {}).get("drawStyle") == "bars"
            and any("$__rate_interval" in t["expr"] for t in p.get("targets", []))
        ]
        self.assertEqual(bad, [], "show a count over time with gen_dashboards.count_bar(), not rate() * 3600")

    def test_the_miss_bars_are_tested(self):
        with open(PROMTOOL_TEST) as f:
            tested = f.read()
        p = next(p for p in g.cache()["panels"] if p["title"] == "Misses by cause")
        self.assertIn(filled(p["targets"][0]["expr"]), tested)


class DayBarTest(unittest.TestCase):
    def test_the_cache_effect_day_bars_are_tested(self):
        with open(PROMTOOL_TEST) as f:
            tested = f.read()
        p = next(p for p in g.savings()["panels"] if p["title"] == "Cache effects (USD per day)")
        self.assertIn(filled(p["targets"][0]["expr"]), tested)
        self.assertIn(" offset -1d", p["targets"][1]["expr"])


class LegendTest(unittest.TestCase):
    def test_a_by_label_legend_never_falls_back_to_value(self):
        # Older cortex builds push llm_cache_misses_total and llm_keepalive_pings_total with no cause or
        # outcome label. Grafana names such a series "Value". The panel must name it.
        cache = g.cache()
        for title, label in (
            ("Misses by cause", "cause"),
            ("Miss cost by cause (USD, selected range)", "cause"),
        ):
            p = next(p for p in cache["panels"] if p["title"] == title)
            self.assertIn(f'"{label}", "unattributed", "{label}", ""', p["targets"][0]["expr"], title)


class PingFailureRatioTest(unittest.TestCase):
    def test_skipped_and_unattributed_pings_are_not_in_the_ratio(self):
        p = next(p for p in g.cache()["panels"] if p["title"] == "Ping failure ratio")
        e = p["targets"][0]["expr"]
        self.assertNotIn("skipped", e)
        self.assertIn('outcome=~"failed|timeout"', e)
        self.assertIn('outcome=~"ok|failed|timeout"', e)

class DescriptionTest(unittest.TestCase):
    def test_estimated_ping_cost_names_both_meanings(self):
        p = next(p for p in g.cache()["panels"] if p["title"] == "Estimated ping cost")
        self.assertIn("timed out", p["description"])
        self.assertIn("fallback rate", p["description"])


if __name__ == "__main__":
    unittest.main()
