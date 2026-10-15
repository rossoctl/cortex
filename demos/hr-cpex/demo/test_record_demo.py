"""Tests for record_demo.py's drift guard. Run from demos/hr-cpex:

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
        (self.tmp / "scenarios" / "13-workload-delegation.sh").unlink()
        problems = record_demo.check(self.tmp)
        self.assertIn("demo/steps.md names 13-workload-delegation.sh, which is not in scenarios/", problems)

    def test_walkthrough_out_of_order(self):
        self.edit("walkthrough.sh", "run 01-bob-allow.sh", "run SWAP")
        self.edit("walkthrough.sh", "run 02-alice-deny.sh", "run 01-bob-allow.sh")
        self.edit("walkthrough.sh", "run SWAP", "run 02-alice-deny.sh")
        problems = record_demo.check(self.tmp)
        self.assertEqual(len(problems), 1)
        self.assertIn("out of order", problems[0])

    def test_step_listed_twice(self):
        self.edit("demo/steps.md", "| 2 | Workday | `02-alice-deny.sh`", "| 2 | Workday | `01-bob-allow.sh`")
        problems = record_demo.check(self.tmp)
        self.assertIn("demo/steps.md is missing 02-alice-deny.sh", problems)
        self.assertIn("demo/steps.md names 01-bob-allow.sh more than once", problems)

    def test_main_check_exit_code(self):
        self.assertEqual(record_demo.main(["--check"]), 0)


if __name__ == "__main__":
    unittest.main()
