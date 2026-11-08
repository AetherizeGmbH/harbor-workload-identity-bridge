#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Decide the govulncheck and Trivy checks from their JSON reports (ADR-0036).

Usage:
  vuln-gate.py govulncheck HEAD.json [--base BASE.json] [--fixable-only]
  vuln-gate.py trivy HEAD.json [--base BASE.json] [--base-prefix DIR/]

HEAD.json is the scan of the revision under test. Without --base (pushes to
main, the daily schedule) every finding fails the check. With --base, the
scan of the pull request's base commit, a finding fails the check only when
the head has it more often than the base: a pull request fails for what it
adds, not for advisories published against code it did not touch. Findings
the base has too are printed as warnings.

What counts as a finding:
  govulncheck  an OSV entry with at least one symbol-level finding, the same
               set that makes `govulncheck ./...` exit 3
  trivy        every vulnerability, failed misconfiguration and secret in the
               report (the scan already applies severity, --ignore-unfixed
               and the ignore file)

--base-prefix strips the directory the base was checked out into from Trivy
targets, so head and base name the same file the same way.

--fixable-only (govulncheck) turns findings without a fixed version into
warnings. The release gate uses it (ADR-0040): a release stops only for
a vulnerability that an update can remove.

Exit status: 0 pass, 1 findings fail the check, 2 a report is missing or not
a report of that scanner (a failed scan must never pass).
"""
import argparse
import collections
import json
import os
import sys


class ReportError(Exception):
    pass


def read(path):
    try:
        with open(path, encoding="utf-8") as f:
            return f.read()
    except OSError as e:
        raise ReportError(f"{path}: {e}") from e


def govulncheck_findings(path):
    """Map each called OSV ID to one line that describes it.

    Also returns the keys of the called OSV IDs that no finding names a
    fixed version for.
    """
    text = read(path)
    decoder = json.JSONDecoder()
    pos, seen_config, summaries, called, fixable = 0, False, {}, {}, set()
    while True:
        while pos < len(text) and text[pos].isspace():
            pos += 1
        if pos >= len(text):
            break
        try:
            obj, pos = decoder.raw_decode(text, pos)
        except json.JSONDecodeError as e:
            raise ReportError(f"{path}: not govulncheck JSON output: {e}") from e
        if not isinstance(obj, dict):
            raise ReportError(f"{path}: not govulncheck JSON output")
        if "config" in obj:
            seen_config = True
        if "osv" in obj:
            entry = obj["osv"]
            summaries[entry.get("id")] = entry.get("summary", "")
        finding = obj.get("finding")
        if finding:
            trace = finding.get("trace") or [{}]
            frame = trace[0]
            if frame.get("function"):
                where = frame.get("package") or frame.get("module") or "?"
                fixed = finding.get("fixed_version") or "no fix"
                called.setdefault(finding["osv"], f"{where}@{frame.get('version', '?')}, fixed in {fixed}")
                if finding.get("fixed_version"):
                    fixable.add(finding["osv"])
    if not seen_config:
        raise ReportError(f"{path}: no govulncheck config message; the scan did not run")
    counts = collections.Counter({(osv,): 1 for osv in called})
    labels = {(osv,): f"{osv}: {summaries.get(osv, '')} ({where})" for osv, where in called.items()}
    unfixable = {(osv,) for osv in called if osv not in fixable}
    return counts, labels, unfixable


def trivy_findings(path, strip=""):
    """Count Trivy findings by a key that survives unrelated edits (no line numbers)."""
    try:
        doc = json.loads(read(path))
    except json.JSONDecodeError as e:
        raise ReportError(f"{path}: not Trivy JSON output: {e}") from e
    if not isinstance(doc, dict) or "SchemaVersion" not in doc:
        raise ReportError(f"{path}: no SchemaVersion; not a Trivy JSON report")
    counts, labels = collections.Counter(), {}
    for result in doc.get("Results") or []:
        target = result.get("Target", "")
        if strip and target.startswith(strip):
            target = target[len(strip):]
        for v in result.get("Vulnerabilities") or []:
            key = ("vulnerability", target, v.get("PkgName"), v.get("VulnerabilityID"))
            counts[key] += 1
            labels[key] = (
                f"{target}: {v.get('PkgName')} {v.get('InstalledVersion')} "
                f"{v.get('VulnerabilityID')} ({v.get('Severity')}, fixed in {v.get('FixedVersion') or '?'})"
            )
        for m in result.get("Misconfigurations") or []:
            if m.get("Status", "FAIL") != "FAIL":
                continue
            key = ("misconfiguration", target, m.get("AVDID") or m.get("ID"))
            counts[key] += 1
            labels[key] = f"{target}: {m.get('ID')} {m.get('Title')} ({m.get('Severity')})"
        for s in result.get("Secrets") or []:
            key = ("secret", target, s.get("RuleID"))
            counts[key] += 1
            labels[key] = f"{target}:{s.get('StartLine')}: secret {s.get('RuleID')} ({s.get('Severity')})"
    return counts, labels


def annotate(level, tool, text):
    # GitHub workflow commands end at a newline; keep each finding on one line.
    print(f"::{level} title={tool}::{text}".replace("\n", " "))


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("tool", choices=["govulncheck", "trivy"])
    parser.add_argument("head")
    parser.add_argument("--base")
    parser.add_argument("--base-prefix", default="")
    parser.add_argument("--fixable-only", action="store_true")
    args = parser.parse_args(argv)
    if args.fixable_only and args.tool != "govulncheck":
        print(f"::error title={args.tool}::--fixable-only applies to govulncheck only")
        return 2

    try:
        unfixable = set()
        if args.tool == "govulncheck":
            head, labels, unfixable = govulncheck_findings(args.head)
            base = govulncheck_findings(args.base)[0] if args.base else collections.Counter()
        else:
            head, labels = trivy_findings(args.head)
            base = trivy_findings(args.base, args.base_prefix)[0] if args.base else collections.Counter()
    except ReportError as e:
        print(f"::error title={args.tool}::{e}")
        return 2

    waiting = collections.Counter()
    if args.fixable_only:
        waiting = collections.Counter({k: n for k, n in head.items() if k in unfixable})
        head = head - waiting
    added = head - base
    kept = head - added
    summary = []
    for key in sorted(added):
        extra = f" ({added[key]} more than the base)" if base[key] else ""
        annotate("error", args.tool, labels[key] + extra)
        summary.append(f"- new: {labels[key]}{extra}")
    for key in sorted(kept):
        annotate("warning", args.tool, f"already on the base commit: {labels[key]}")
        summary.append(f"- on the base commit too: {labels[key]}")
    for key in sorted(waiting):
        annotate("warning", args.tool, f"no fixed version yet: {labels[key]}")
        summary.append(f"- no fixed version yet: {labels[key]}")

    if args.base:
        verdict = (
            f"{sum(added.values())} finding(s) added by this pull request, "
            f"{sum(kept.values())} already on the base commit"
        )
    else:
        verdict = f"{sum(added.values())} finding(s)"
    if args.fixable_only:
        verdict += f" with a fixed version, {sum(waiting.values())} without one yet"
    print(f"{args.tool}: {verdict}")
    step_summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if step_summary:
        with open(step_summary, "a", encoding="utf-8") as f:
            f.write(f"### {args.tool}: {verdict}\n\n" + "\n".join(summary) + "\n")
    return 1 if added else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
