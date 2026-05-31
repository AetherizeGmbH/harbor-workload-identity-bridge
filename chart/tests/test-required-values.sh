#!/usr/bin/env bash
# Verify every required value gate fails template rendering with a
# clear message. If any of these *succeeds*, a required-value check
# was silently dropped — caught by the test.
set -euo pipefail

CHART_DIR="${CHART_DIR:-chart}"
COMPLETE="${CHART_DIR}/tests/values-complete.yaml"

# Each case: (label, --set arg to clear the required field, expected
# error substring). We render with values-complete.yaml as the base
# and override the field-under-test to empty/null.
cases=(
  "clusterName|--set|clusterName=|clusterName is REQUIRED"
  "harbor.url|--set|harbor.url=|harbor.url is REQUIRED"
  "harbor.adminCredsSecret.name|--set|harbor.adminCredsSecret.name=|harbor.adminCredsSecret.name is REQUIRED"
  "plugin.audience|--set|plugin.audience=|plugin.audience is REQUIRED"
  "tls.issuerRef.name when tls.enabled|--set|tls.issuerRef.name=|tls.issuerRef.name is REQUIRED"
  "clusterName must be a DNS label|--set|clusterName=NotADnsLabel|does not match DNS label regex"
)

failed=0
for case in "${cases[@]}"; do
  IFS='|' read -r label flag setval want <<< "${case}"

  out=$(helm template harbor-bridge "${CHART_DIR}" -f "${COMPLETE}" \
        --namespace harbor-bridge-system \
        "${flag}" "${setval}" 2>&1 || true)

  if echo "${out}" | grep -q "${want}"; then
    echo "PASS  ${label}"
  else
    echo "FAIL  ${label}"
    echo "      expected error containing: ${want}"
    echo "      got: ${out}" | head -3
    failed=$((failed+1))
  fi
done

# Special case: plugin.matchImages defaults to [] which is "set" but
# empty; clearing via --set doesn't reproduce the empty-list path
# correctly. Use a values overlay file instead.
cat <<'EOF' > /tmp/values-no-matchimages.yaml
clusterName: prod-eu-west
harbor:
  url: https://harbor.example.com
  adminCredsSecret:
    name: harbor-admin
plugin:
  matchImages: []
  audience: harbor-bridge-prod-eu-west
tls:
  issuerRef:
    name: harbor-bridge-ca
EOF
out=$(helm template harbor-bridge "${CHART_DIR}" -f /tmp/values-no-matchimages.yaml \
      --namespace harbor-bridge-system 2>&1 || true)
if echo "${out}" | grep -q "plugin.matchImages is REQUIRED"; then
  echo "PASS  plugin.matchImages (empty list)"
else
  echo "FAIL  plugin.matchImages (empty list)"
  echo "      got: ${out}" | head -3
  failed=$((failed+1))
fi

if [ "${failed}" -gt 0 ]; then
  echo
  echo "${failed} required-value test(s) failed"
  exit 1
fi
echo
echo "all required-value gates fire as expected"
