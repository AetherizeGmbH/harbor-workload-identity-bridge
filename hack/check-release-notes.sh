#!/usr/bin/env bash
# Proves that the semantic-release plugins pinned in release.yml render
# release notes with .releaserc.json. The release job runs only on main,
# so without this check a plugin bump is first exercised by a real
# release: conventional-changelog-conventionalcommits 10.x rendered every
# release from 0.3.2 to 0.4.0 with an empty body and nothing failed.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
workflow="$root/.github/workflows/release.yml"

semver="$(sed -nE 's/^[[:space:]]*semantic_version:[[:space:]]*([0-9.]+).*/\1/p' "$workflow")"
pins="$(grep -oE '(@semantic-release/[a-z-]+|conventional-changelog-[a-z-]+)@[0-9]+\.[0-9]+\.[0-9]+' "$workflow" | sort -u)"
if [ -z "$semver" ] || [ -z "$pins" ]; then
  echo "could not read semantic_version / extra_plugins from $workflow" >&2
  exit 1
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
cp "$root/hack/check-release-notes.mjs" "$tmp/"
cd "$tmp"
npm init -y >/dev/null
# shellcheck disable=SC2086 # $pins is a newline-separated list of name@version
npm install --silent --no-audit --no-fund "semantic-release@$semver" $pins
node check-release-notes.mjs "$root/.releaserc.json"
