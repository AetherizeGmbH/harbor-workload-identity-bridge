#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Tests for render-compat-table.py.

Run: python3 -B -m unittest discover -s hack -p '*_test.py'
"""
import json
import pathlib
import re
import subprocess
import sys
import tempfile
import unittest

HACK = pathlib.Path(__file__).resolve().parent
SCRIPT = HACK / "render-compat-table.py"
README = HACK.parent / "README.md"

ROW = re.compile(
    r"^\| (?P<app>[0-9.]+)(?: \([a-z, ]+\))? \| (?P<chart>[0-9.]+) \| (?P<mark>✅|❌) \| (?P<date>[0-9-]+) \|$"
)


def run(*args):
    return subprocess.run(
        [sys.executable, str(SCRIPT), *args], capture_output=True, text=True, check=False
    )


class RenderCompatTableTest(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.dir = pathlib.Path(self._tmp.name)

    def tearDown(self):
        self._tmp.cleanup()

    def results(self, *rows, date="2026-09-24"):
        """Write one result file per (chart, app, status) like the matrix legs do."""
        for chart, app, status in rows:
            leg = self.dir / f"compat-result-{chart}"
            leg.mkdir()
            record = {"chart": chart, "app": app, "status": status, "date": date}
            (leg / "result.json").write_text(json.dumps(record), encoding="utf-8")
        return str(self.dir / "compat-result-*" / "result.json")

    def summary_rows(self, results_glob):
        out = run("--summary", results_glob)
        self.assertEqual(out.returncode, 0, out.stderr)
        return [line for line in out.stdout.splitlines() if ROW.match(line)]

    def test_readme_table_renders_back_byte_identical(self):
        """The rows the README holds today render back to the same bytes."""
        readme = README.read_text(encoding="utf-8")
        block = readme[readme.index("<!-- BEGIN HARBOR-COMPAT"):readme.index("<!-- END HARBOR-COMPAT")]
        rows, dates = [], set()
        for line in block.splitlines():
            m = ROW.match(line)
            if m:
                rows.append((m["chart"], m["app"], "pass" if m["mark"] == "✅" else "fail"))
                dates.add(m["date"])
        self.assertTrue(rows, "no table rows found between the README markers")
        self.assertEqual(len(dates), 1, "the test assumes one run date")
        copy = self.dir / "README.md"
        copy.write_text(readme, encoding="utf-8")
        out = run(self.results(*rows, date=dates.pop()), str(copy))
        self.assertEqual(out.returncode, 0, out.stderr)
        self.assertEqual(copy.read_text(encoding="utf-8"), readme)

    def test_floor_and_ceiling_tags(self):
        rows = self.summary_rows(self.results(
            ("1.13.5", "2.9.5", "pass"),
            ("1.15.2", "2.11.2", "pass"),
            ("1.19.1", "2.15.1", "pass"),
        ))
        self.assertEqual(rows, [
            "| 2.15.1 (ceiling) | 1.19.1 | ✅ | 2026-09-24 |",
            "| 2.11.2 | 1.15.2 | ✅ | 2026-09-24 |",
            "| 2.9.5 (floor) | 1.13.5 | ✅ | 2026-09-24 |",
        ])

    def test_single_passing_row_is_floor_and_ceiling(self):
        rows = self.summary_rows(self.results(
            ("1.13.5", "2.9.5", "fail"),
            ("1.19.1", "2.15.1", "pass"),
        ))
        self.assertEqual(rows, [
            "| 2.15.1 (floor, ceiling) | 1.19.1 | ✅ | 2026-09-24 |",
            "| 2.9.5 | 1.13.5 | ❌ | 2026-09-24 |",
        ])

    def test_summary_prints_the_table_and_writes_nothing(self):
        results_glob = self.results(("1.11.4", "2.7.4", "fail"), ("1.12.6", "2.8.6", "pass"))
        before = sorted(p.relative_to(self.dir) for p in self.dir.rglob("*"))
        out = run("--summary", results_glob)
        self.assertEqual(out.returncode, 0, out.stderr)
        self.assertNotIn("HARBOR-COMPAT", out.stdout)
        self.assertIn("| 2.8.6 (floor, ceiling) | 1.12.6 | ✅ |", out.stdout)
        self.assertEqual(sorted(p.relative_to(self.dir) for p in self.dir.rglob("*")), before)

    def test_no_results_fails(self):
        missing = str(self.dir / "nothing-*" / "result.json")
        self.assertEqual(run("--summary", missing).returncode, 1)
        readme = self.dir / "README.md"
        readme.write_text("<!-- BEGIN HARBOR-COMPAT -->\n<!-- END HARBOR-COMPAT -->\n", encoding="utf-8")
        self.assertEqual(run(missing, str(readme)).returncode, 1)

    def test_bad_usage(self):
        self.assertEqual(run().returncode, 2)
        self.assertEqual(run("--summary").returncode, 2)
        self.assertEqual(run("--bogus", "x").returncode, 2)


if __name__ == "__main__":
    unittest.main()
