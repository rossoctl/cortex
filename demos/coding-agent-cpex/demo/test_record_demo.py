"""Tests for record_demo.py's drift guard. Run from demos/coding-agent-cpex:

python3 -m unittest discover -s demo
"""

import shutil
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

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
        self.edit("demo/steps.md", "| 2 | Sandbox | `01-unapproved-host-deny.sh`", "| 2 | Sandbox | `00-sandbox-boundary.sh`")
        problems = record_demo.check(self.tmp)
        self.assertIn("demo/steps.md is missing 01-unapproved-host-deny.sh", problems)
        self.assertIn("demo/steps.md names 00-sandbox-boundary.sh more than once", problems)

    def test_main_check_exit_code(self):
        self.assertEqual(record_demo.main(["--check"]), 0)


if __name__ == "__main__":
    unittest.main()
