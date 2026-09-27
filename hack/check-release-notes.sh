#!/usr/bin/env bash
# Proves that the semantic-release plugins pinned in package-lock.json
# render release notes with .releaserc.json. The release job runs only on
# main, so without this check a plugin bump is first exercised by a real
# release: conventional-changelog-conventionalcommits 10.x rendered every
# release from 0.3.2 to 0.4.0 with an empty body and nothing failed. This
# installs exactly what the release job installs (npm ci).
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"
npm ci --ignore-scripts --no-audit --no-fund --silent
node hack/check-release-notes.mjs .releaserc.json
