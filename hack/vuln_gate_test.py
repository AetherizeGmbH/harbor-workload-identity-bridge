#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Tests for vuln-gate.py.

Run: python3 -B -m unittest discover -s hack -p '*_test.py'
"""
import json
import pathlib
import subprocess
import sys
import tempfile
import unittest

HACK = pathlib.Path(__file__).resolve().parent
SCRIPT = HACK / "vuln-gate.py"

# govulncheck -format json prints a stream of indented objects, not one
# document: config first, then progress, osv and finding messages.
CONFIG = {"config": {"protocol_version": "v1.0.0", "scanner_name": "govulncheck", "scan_level": "symbol"}}


def osv(osv_id, summary):
    return {"osv": {"id": osv_id, "summary": summary}}


def finding(osv_id, level, module="stdlib", version="v1.27.1", package="crypto/x509", fixed="v1.27.3"):
    frame = {"module": module, "version": version}
    if level in ("package", "symbol"):
        frame["package"] = package
    if level == "symbol":
        frame["function"] = "Verify"
    return {"finding": {"osv": osv_id, "fixed_version": fixed, "trace": [frame]}}


def govulncheck_stream(*objs):
    return "\n".join(json.dumps(o, indent=2) for o in (CONFIG, *objs)) + "\n"


def vuln(pkg, vid, target="go.mod"):
    return {
        "Target": target,
        "Class": "lang-pkgs",
        "Type": "gomod",
        "Vulnerabilities": [
            {"VulnerabilityID": vid, "PkgName": pkg, "InstalledVersion": "v0.58.0", "FixedVersion": "0.60.0", "Severity": "HIGH"}
        ],
    }


def misconfig(target, check_id, count=1):
    return {
        "Target": target,
        "Class": "config",
        "Type": "dockerfile",
        "Misconfigurations": [
            {"ID": check_id, "AVDID": "AVD-" + check_id, "Title": "t", "Severity": "HIGH", "Status": "FAIL"}
        ]
        * count,
    }


def secret(target, rule):
    return {"Target": target, "Class": "secret", "Secrets": [{"RuleID": rule, "Severity": "CRITICAL", "StartLine": 3}]}


def trivy_report(*results):
    return json.dumps({"SchemaVersion": 2, "ArtifactName": ".", "Results": list(results)})


class VulnGateTest(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.dir = pathlib.Path(self._tmp.name)

    def tearDown(self):
        self._tmp.cleanup()

    def write(self, name, text):
        path = self.dir / name
        path.write_text(text, encoding="utf-8")
        return str(path)

    def gate(self, *args):
        return subprocess.run(
            [sys.executable, str(SCRIPT), *args], capture_output=True, text=True, check=False, env={}
        )

    # --- govulncheck -----------------------------------------------------

    def test_govulncheck_called_vulnerability_fails_without_base(self):
        head = self.write("h.json", govulncheck_stream(osv("GO-1", "x509 bug"), finding("GO-1", "symbol")))
        r = self.gate("govulncheck", head)
        self.assertEqual(r.returncode, 1, r.stdout)
        self.assertIn("::error title=govulncheck::GO-1: x509 bug (crypto/x509@v1.27.1, fixed in v1.27.3)", r.stdout)

    def test_govulncheck_module_and_package_findings_do_not_fail(self):
        # Same rule as `govulncheck ./...`: only code we call makes it exit 3.
        head = self.write(
            "h.json",
            govulncheck_stream(osv("GO-2", "html bug"), finding("GO-2", "module"), finding("GO-2", "package")),
        )
        r = self.gate("govulncheck", head)
        self.assertEqual(r.returncode, 0, r.stdout)
        self.assertIn("0 finding(s)", r.stdout)

    def test_govulncheck_fixable_only_fails_for_a_fixed_version(self):
        # The release gate (ADR-0040): an update can remove this one.
        head = self.write("h.json", govulncheck_stream(osv("GO-1", "x509 bug"), finding("GO-1", "symbol")))
        r = self.gate("govulncheck", head, "--fixable-only")
        self.assertEqual(r.returncode, 1, r.stdout)
        self.assertIn("1 finding(s) with a fixed version, 0 without one yet", r.stdout)

    def test_govulncheck_fixable_only_warns_without_a_fixed_version(self):
        head = self.write(
            "h.json",
            govulncheck_stream(
                osv("GO-1", "x509 bug"), finding("GO-1", "symbol"),
                osv("GO-3", "no fix yet"), finding("GO-3", "symbol", fixed=""),
            ),
        )
        r = self.gate("govulncheck", head, "--fixable-only")
        self.assertEqual(r.returncode, 1, r.stdout)
        self.assertIn("::warning title=govulncheck::no fixed version yet: GO-3", r.stdout)
        self.assertIn("1 finding(s) with a fixed version, 1 without one yet", r.stdout)
        only_unfixed = self.write("u.json", govulncheck_stream(osv("GO-3", "no fix yet"), finding("GO-3", "symbol", fixed="")))
        r = self.gate("govulncheck", only_unfixed, "--fixable-only")
        self.assertEqual(r.returncode, 0, r.stdout)
        # Without the flag every called finding fails, fixed version or not.
        self.assertEqual(self.gate("govulncheck", only_unfixed).returncode, 1)

    def test_fixable_only_is_refused_for_trivy(self):
        head = self.write("t.json", trivy_report())
        r = self.gate("trivy", head, "--fixable-only")
        self.assertEqual(r.returncode, 2, r.stdout)

    def test_govulncheck_finding_on_base_only_warns(self):
        stream = govulncheck_stream(osv("GO-1", "x509 bug"), finding("GO-1", "symbol"), finding("GO-1", "symbol"))
        r = self.gate("govulncheck", self.write("h.json", stream), "--base", self.write("b.json", stream))
        self.assertEqual(r.returncode, 0, r.stdout)
        self.assertIn("::warning title=govulncheck::already on the base commit: GO-1", r.stdout)
        self.assertIn("0 finding(s) added by this pull request, 1 already on the base commit", r.stdout)

    def test_govulncheck_new_vulnerability_fails_next_to_old_ones(self):
        base = govulncheck_stream(osv("GO-1", "old"), finding("GO-1", "symbol"))
        head = govulncheck_stream(
            osv("GO-1", "old"), finding("GO-1", "symbol"), osv("GO-3", "new"), finding("GO-3", "symbol")
        )
        r = self.gate("govulncheck", self.write("h.json", head), "--base", self.write("b.json", base))
        self.assertEqual(r.returncode, 1, r.stdout)
        self.assertIn("::error title=govulncheck::GO-3: new", r.stdout)
        self.assertNotIn("::error title=govulncheck::GO-1", r.stdout)

    def test_govulncheck_fix_passes(self):
        base = govulncheck_stream(osv("GO-1", "old"), finding("GO-1", "symbol"))
        r = self.gate("govulncheck", self.write("h.json", govulncheck_stream()), "--base", self.write("b.json", base))
        self.assertEqual(r.returncode, 0, r.stdout)

    def test_govulncheck_report_without_config_is_an_error(self):
        # An empty file is what a scan that failed before printing leaves behind.
        for text in ("", "{}\n", "not json"):
            with self.subTest(text=text):
                r = self.gate("govulncheck", self.write("h.json", text))
                self.assertEqual(r.returncode, 2, r.stdout)

    def test_missing_base_report_is_an_error(self):
        head = self.write("h.json", govulncheck_stream())
        r = self.gate("govulncheck", head, "--base", str(self.dir / "missing.json"))
        self.assertEqual(r.returncode, 2, r.stdout)

    # --- trivy -----------------------------------------------------------

    def test_trivy_any_finding_fails_without_base(self):
        for result in (vuln("golang.org/x/net", "CVE-1"), misconfig("Dockerfile", "DS-0002"), secret("a.env", "aws")):
            with self.subTest(result=result["Target"]):
                r = self.gate("trivy", self.write("h.json", trivy_report(result)))
                self.assertEqual(r.returncode, 1, r.stdout)

    def test_trivy_clean_report_passes(self):
        r = self.gate("trivy", self.write("h.json", trivy_report()))
        self.assertEqual(r.returncode, 0, r.stdout)

    def test_trivy_findings_on_base_only_warn(self):
        report = trivy_report(vuln("golang.org/x/net", "CVE-1"), misconfig("Dockerfile", "DS-0002"))
        r = self.gate("trivy", self.write("h.json", report), "--base", self.write("b.json", report))
        self.assertEqual(r.returncode, 0, r.stdout)
        self.assertEqual(r.stdout.count("::warning title=trivy::"), 2, r.stdout)

    def test_trivy_base_targets_are_compared_without_the_checkout_prefix(self):
        head = trivy_report(vuln("golang.org/x/net", "CVE-1", target="go.mod"))
        base = trivy_report(vuln("golang.org/x/net", "CVE-1", target=".vulngate-base/go.mod"))
        h, b = self.write("h.json", head), self.write("b.json", base)
        self.assertEqual(self.gate("trivy", h, "--base", b, "--base-prefix", ".vulngate-base/").returncode, 0)
        self.assertEqual(self.gate("trivy", h, "--base", b).returncode, 1)

    def test_trivy_same_check_in_another_file_is_new(self):
        base = trivy_report(misconfig("Dockerfile.bridge", "DS-0002"))
        head = trivy_report(misconfig("Dockerfile.bridge", "DS-0002"), misconfig("Dockerfile.new", "DS-0002"))
        r = self.gate("trivy", self.write("h.json", head), "--base", self.write("b.json", base))
        self.assertEqual(r.returncode, 1, r.stdout)
        self.assertIn("::error title=trivy::Dockerfile.new: DS-0002", r.stdout)

    def test_trivy_second_instance_in_the_same_file_is_new(self):
        base = trivy_report(misconfig("chart.yaml", "KSV-0017"))
        head = trivy_report(misconfig("chart.yaml", "KSV-0017", count=2))
        r = self.gate("trivy", self.write("h.json", head), "--base", self.write("b.json", base))
        self.assertEqual(r.returncode, 1, r.stdout)
        self.assertIn("(1 more than the base)", r.stdout)

    def test_trivy_new_vulnerability_fails_next_to_old_ones(self):
        base = trivy_report(vuln("golang.org/x/net", "CVE-1"))
        head = trivy_report(vuln("golang.org/x/net", "CVE-1"), vuln("github.com/go-jose/go-jose/v4", "CVE-2"))
        r = self.gate("trivy", self.write("h.json", head), "--base", self.write("b.json", base))
        self.assertEqual(r.returncode, 1, r.stdout)
        self.assertIn("go-jose", r.stdout.split("::warning")[0])

    def test_trivy_passed_misconfigurations_are_ignored(self):
        result = misconfig("Dockerfile", "DS-0002")
        result["Misconfigurations"][0]["Status"] = "PASS"
        r = self.gate("trivy", self.write("h.json", trivy_report(result)))
        self.assertEqual(r.returncode, 0, r.stdout)

    def test_trivy_report_without_schema_version_is_an_error(self):
        for text in ("", "{}", "[]"):
            with self.subTest(text=text):
                r = self.gate("trivy", self.write("h.json", text))
                self.assertEqual(r.returncode, 2, r.stdout)

    def test_step_summary_is_written(self):
        summary = self.dir / "summary.md"
        head = self.write("h.json", trivy_report(vuln("golang.org/x/net", "CVE-1")))
        subprocess.run(
            [sys.executable, str(SCRIPT), "trivy", head],
            capture_output=True,
            text=True,
            check=False,
            env={"GITHUB_STEP_SUMMARY": str(summary)},
        )
        text = summary.read_text(encoding="utf-8")
        self.assertIn("### trivy: 1 finding(s)", text)
        self.assertIn("- new: go.mod: golang.org/x/net v0.58.0 CVE-1", text)


if __name__ == "__main__":
    unittest.main()
