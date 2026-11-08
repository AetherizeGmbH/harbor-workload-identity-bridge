#!/usr/bin/env bash
# semantic-release runs this as verifyReleaseCmd, that is only when a
# release is due, before it tags anything (ADR-0040). It fails the release
# when govulncheck finds a vulnerability that the code calls and that a
# fixed version removes. A finding without a fixed version is a warning:
# no update could remove it, so holding the release back would only hold
# back its other fixes.
#
# The scan uses the toolchain of go.mod's toolchain line, the one the
# images build with (Dockerfile.bridge, Dockerfile.plugin), as the
# govulncheck check of vulnerabilities.yml does.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"
report="$(mktemp)"
trap 'rm -f -- "$report"' EXIT
GOTOOLCHAIN="$(awk '$1 == "toolchain" { print $2 }' go.mod)" \
  make -s govulncheck GOVULNCHECK_ARGS="-format json" > "$report"
python3 -B hack/vuln-gate.py govulncheck "$report" --fixable-only
