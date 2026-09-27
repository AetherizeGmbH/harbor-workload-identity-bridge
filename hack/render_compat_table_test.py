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

DEFAULT_DATE = "2026-09-24"

# One rendered table row. Every cell is taken as the renderer prints it: each
# matrix leg records its own date, and a leg whose `helm show chart` failed
# records app "unknown" (.github/workflows/harbor-compat.yml).
ROW = re.compile(
    r"^\| (?P<app>.+?)(?: \((?:floor|ceiling|floor, ceiling)\))?"
    r" \| (?P<chart>[^|]+?) \| (?P<mark>✅|❌) \| (?P<date>[^|]+?) \|$"
)
HEADER_SEPARATOR = "| --- | --- | --- | --- |"


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

    def results(self, *rows):
        """Write one result file per (chart, app, status[, date]) like the matrix legs do.

        Each leg records its own date; it defaults to DEFAULT_DATE. The
        directories are named like the workflow's downloaded artifacts, so
        rows that sort equal keep the order the workflow gives them.
        """
        results_dir = pathlib.Path(tempfile.mkdtemp(dir=self.dir))
        for chart, app, status, *date in rows:
            leg = results_dir / f"compat-result-{chart}"
            leg.mkdir()
            record = {"chart": chart, "app": app, "status": status, "date": date[0] if date else DEFAULT_DATE}
            (leg / "result.json").write_text(json.dumps(record), encoding="utf-8")
        return str(results_dir / "compat-result-*" / "result.json")

    def table_rows(self, readme):
        """Parse the rows between the README markers back into result tuples.

        Every body line must parse: a row the pattern skipped would vanish
        from the re-rendered table and fail the comparison for the wrong reason.
        """
        block = readme[readme.index("<!-- BEGIN HARBOR-COMPAT"):readme.index("<!-- END HARBOR-COMPAT")]
        lines = block.splitlines()
        self.assertIn(HEADER_SEPARATOR, lines, "no table header between the README markers")
        rows = []
        for line in lines[lines.index(HEADER_SEPARATOR) + 1:]:
            if not line:
                continue
            m = ROW.match(line)
            self.assertIsNotNone(m, f"cannot parse table row {line!r}")
            rows.append((m["chart"], m["app"], "pass" if m["mark"] == "✅" else "fail", m["date"]))
        self.assertTrue(rows, "no table rows found between the README markers")
        return rows

    def assert_renders_back_byte_identical(self, readme):
        copy = self.dir / "README.md"
        copy.write_text(readme, encoding="utf-8")
        out = run(self.results(*self.table_rows(readme)), str(copy))
        self.assertEqual(out.returncode, 0, out.stderr)
        self.assertEqual(copy.read_text(encoding="utf-8"), readme)

    def summary_rows(self, results_glob):
        out = run("--summary", results_glob)
        self.assertEqual(out.returncode, 0, out.stderr)
        return [line for line in out.stdout.splitlines() if ROW.match(line)]

    def test_readme_table_renders_back_byte_identical(self):
        """The rows the README holds today render back to the same bytes."""
        self.assert_renders_back_byte_identical(README.read_text(encoding="utf-8"))

    def test_generated_tables_render_back_byte_identical(self):
        """Every table the workflow can write into the README round-trips.

        The README test above runs on every PR, including the App's table
        PR, so it must accept anything the renderer produces from real legs.
        """
        cases = {
            "legs on different dates, one app unknown": [
                ("1.17.5", "2.13.5", "pass", "2026-09-29"),
                ("1.13.5", "2.9.5", "pass", "2026-09-28"),
                ("1.19.2", "unknown", "fail", "2026-09-28"),
            ],
            "single passing row": [
                ("1.13.5", "2.9.5", "fail"),
                ("1.19.2", "2.15.2", "pass"),
            ],
            "nothing passes": [
                ("1.13.5", "2.9.5", "fail"),
                ("1.19.2", "2.15.2", "fail"),
            ],
            "unknown apps that pass, tied in sort order": [
                ("1.15.2", "unknown", "pass"),
                ("1.13.5", "unknown", "pass"),
                ("1.19.2", "2.15.2", "pass", "2026-09-29"),
            ],
        }
        for name, rows in cases.items():
            with self.subTest(name):
                generated = self.dir / "generated.md"
                generated.write_text(
                    "# Title\n\n<!-- BEGIN HARBOR-COMPAT -->\n<!-- END HARBOR-COMPAT -->\n\nTrailer.\n",
                    encoding="utf-8",
                )
                out = run(self.results(*rows), str(generated))
                self.assertEqual(out.returncode, 0, out.stderr)
                self.assert_renders_back_byte_identical(generated.read_text(encoding="utf-8"))

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

    def test_unknown_app_is_never_floor_or_ceiling(self):
        rows = self.summary_rows(self.results(
            ("1.13.5", "unknown", "pass"),
            ("1.15.2", "2.11.2", "pass"),
            ("1.19.2", "unknown", "fail"),
        ))
        self.assertEqual(rows, [
            "| 2.11.2 (floor, ceiling) | 1.15.2 | ✅ | 2026-09-24 |",
            "| unknown | 1.13.5 | ✅ | 2026-09-24 |",
            "| unknown | 1.19.2 | ❌ | 2026-09-24 |",
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
