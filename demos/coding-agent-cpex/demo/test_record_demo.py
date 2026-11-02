"""Test scenario list and caption checks. Run from demos/coding-agent-cpex:

python3 -m unittest discover -s demo
"""

import shutil
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import policy_slide  # noqa: E402
import record_demo  # noqa: E402


class CheckTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp)
        src = record_demo.DEMO_DIR
        shutil.copytree(src / "scenarios", self.tmp / "scenarios")
        (self.tmp / "demo").mkdir()
        shutil.copy(src / "demo" / "steps.md", self.tmp / "demo" / "steps.md")
        shutil.copy(src / "walkthrough.sh", self.tmp / "walkthrough.sh")

    def edit(self, rel, old, new):
        path = self.tmp / rel
        text = path.read_text()
        self.assertIn(old, text)
        path.write_text(text.replace(old, new, 1))

    def test_repository_has_no_drift(self):
        self.assertEqual(record_demo.check(), [])

    def test_copy_has_no_drift(self):
        self.assertEqual(record_demo.check(self.tmp), [])

    def test_new_scenario_not_in_steps_or_walkthrough(self):
        (self.tmp / "scenarios" / "99-new.sh").write_text("#!/usr/bin/env bash\n")
        problems = record_demo.check(self.tmp)
        self.assertIn("demo/steps.md is missing 99-new.sh", problems)
        self.assertIn("walkthrough.sh is missing 99-new.sh", problems)

    def test_step_names_a_missing_scenario(self):
        (self.tmp / "scenarios" / "09-intent-deny.sh").unlink()
        problems = record_demo.check(self.tmp)
        self.assertIn("demo/steps.md names 09-intent-deny.sh, which is not in scenarios/", problems)

    def test_walkthrough_out_of_order(self):
        self.edit("walkthrough.sh", "run 00-sandbox-boundary.sh", "run SWAP")
        self.edit("walkthrough.sh", "run 01-unapproved-host-deny.sh", "run 00-sandbox-boundary.sh")
        self.edit("walkthrough.sh", "run SWAP", "run 01-unapproved-host-deny.sh")
        problems = record_demo.check(self.tmp)
        self.assertEqual(len(problems), 1)
        self.assertIn("out of order", problems[0])

    def test_step_listed_twice(self):
        self.edit(
            "demo/steps.md", "| 2 | Sandbox | `01-unapproved-host-deny.sh`", "| 2 | Sandbox | `00-sandbox-boundary.sh`"
        )
        problems = record_demo.check(self.tmp)
        self.assertIn("demo/steps.md is missing 01-unapproved-host-deny.sh", problems)
        self.assertIn("demo/steps.md names 00-sandbox-boundary.sh more than once", problems)

    def test_every_step_has_a_caption(self):
        caps = record_demo.captions(custody=False)
        self.assertEqual(sorted(caps), [s[:-3] for s in record_demo.scenarios(record_demo.DEMO_DIR)])
        self.assertTrue(caps["03-secret-in-report-deny"].startswith("4/8 · "))
        self.assertTrue(caps["09-intent-deny"].startswith("8/8 · "))
        self.assertNotIn("/", caps["07-custody-allow"].partition(" · ")[0])

    def test_custody_steps_numbered_with_custody(self):
        caps = record_demo.captions(custody=True)
        self.assertTrue(caps["03-secret-in-report-deny"].startswith("4/10 · "))
        self.assertTrue(caps["07-custody-allow"].startswith("8/10 · "))
        self.assertTrue(caps["09-intent-deny"].startswith("10/10 · "))

    def test_missing_caption(self):
        text = (self.tmp / "demo" / "steps.md").read_text()
        line = next(x for x in text.splitlines() if "03-secret-in-report-deny.sh" in x)
        (self.tmp / "demo" / "steps.md").write_text(text.replace(line, line.rsplit("|", 2)[0] + "| |"))
        self.assertIn("demo/steps.md has no caption for 03-secret-in-report-deny.sh", record_demo.check(self.tmp))

    def test_missing_closing_card(self):
        self.edit("demo/steps.md", "Closing:", "Ending:")
        self.assertIn("demo/steps.md has no Closing: line", record_demo.check(self.tmp))

    def test_main_check_exit_code(self):
        self.assertEqual(record_demo.main(["--check"]), 0)


class PolicySlideTest(unittest.TestCase):
    def test_every_scenario_has_a_slide(self):
        self.assertEqual(sorted(policy_slide.SLIDES), [s[:-3] for s in record_demo.scenarios(record_demo.DEMO_DIR)])

    def test_every_slide_marker_exists(self):
        for name in policy_slide.SLIDES:
            with self.subTest(name):
                self.assertIn(policy_slide.SLIDES[name][0], policy_slide.render(name, 80))

    def test_excerpt_drops_comments(self):
        lines = policy_slide.excerpt(policy_slide.POLICY, "egress-allowlist")
        self.assertTrue(lines[0].startswith("- cel:"))
        self.assertFalse(any(x.lstrip().startswith("#") for x in lines))


if __name__ == "__main__":
    unittest.main()
