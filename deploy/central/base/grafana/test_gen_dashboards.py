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


def filled(expr, rng="1h", rate_interval="5m"):
    """expr with the Grafana variables replaced the way dashboards_test.yml replaces them."""
    return (expr.replace('"$proxy"', '".*"').replace('"$machine"', '".*"')
            .replace("$__range", rng).replace("$__rate_interval", rate_interval))


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
        got = g.inc("m")
        self.assertEqual(got, 'sum((increase(m{proxy=~"$proxy", machine=~"$machine"}[$__range]) '
                               'and m{proxy=~"$proxy", machine=~"$machine"} offset $__range) '
                               'or last_over_time(m{proxy=~"$proxy", machine=~"$machine"}[$__range]))')

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
            if title in ("Pings that paid", "Estimated ping cost"):
                self.assertIn(filled(e), tested, title)


class LegendTest(unittest.TestCase):
    def test_a_by_label_legend_never_falls_back_to_value(self):
        # Older cortex builds push llm_cache_misses_total and llm_keepalive_pings_total with no cause or
        # outcome label. Grafana names such a series "Value". The panel must name it.
        cache = g.cache()
        for title, label in (("Misses by cause (per hour)", "cause"), ("Miss cost by cause (USD, selected range)", "cause"),
                             ("Ping outcomes (per hour)", "outcome")):
            p = next(p for p in cache["panels"] if p["title"] == title)
            self.assertIn(f'"{label}", "unattributed", "{label}", ""', p["targets"][0]["expr"], title)


class DescriptionTest(unittest.TestCase):
    def test_estimated_ping_cost_names_both_meanings(self):
        p = next(p for p in g.cache()["panels"] if p["title"] == "Estimated ping cost")
        self.assertIn("timed out", p["description"])
        self.assertIn("fallback rate", p["description"])


if __name__ == "__main__":
    unittest.main()
