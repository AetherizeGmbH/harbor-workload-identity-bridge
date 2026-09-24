#!/usr/bin/env bash
# Verify every required value gate fails template rendering with a
# clear message. If any of these *succeeds*, a required-value check
# was silently dropped — caught by the test.
set -euo pipefail

CHART_DIR="${CHART_DIR:-charts/harbor-bridge}"
COMPLETE="${CHART_DIR}/tests/values-complete.yaml"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

render() {
  helm template harbor-bridge "${CHART_DIR}" --kube-version 1.34.0 \
    --namespace harbor-bridge-system "$@"
}

# Each case: (label, --set arg to clear the required field, expected
# error substring). We render with values-complete.yaml as the base
# and override the field-under-test to empty/null.
cases=(
  "clusterName|--set|clusterName=|clusterName is REQUIRED"
  "harbor.url|--set|harbor.url=|harbor.url is REQUIRED"
  "harbor.adminCredsSecret.name|--set|harbor.adminCredsSecret.name=|harbor.adminCredsSecret.name is REQUIRED"
  "plugin.audience|--set|plugin.audience=|plugin.audience is REQUIRED"
  "tls.issuerRef.name when tls.enabled|--set|tls.issuerRef.name=|tls.issuerRef.name is REQUIRED"
  "tls.existingSecret when tls disabled|--set|tls.enabled=false|tls.existingSecret is REQUIRED"
  "clusterName must be a DNS label|--set|clusterName=NotADnsLabel|does not match DNS label regex"
  "plugin.patchKubelet tombstone|--set|plugin.patchKubelet=true|plugin.patchKubelet was removed"
  "plugin.install.mode enum|--set|plugin.install.mode=yolo|is invalid. Must be one of"
  "plugin.install null|--set|plugin.install=null|is invalid. Must be one of"
  "plugin.install.binDir without configFile|--set|plugin.install.binDir=/x|must be set together"
  "plugin.install.kubeletUnit injection|--set|plugin.install.kubeletUnit=kubelet;reboot|not a valid systemd unit name"
  "relative plugin.hostBinaryDir|--set|plugin.hostBinaryDir=etc/kubernetes|must be an absolute, clean node path"
  "dot-dot plugin.hostConfigDir|--set|plugin.hostConfigDir=/etc/../tmp|must be an absolute, clean node path"
  "plugin.hostConfigDir with a space|--set|plugin.hostConfigDir=/etc/kube config|must be an absolute, clean node path"
  "matchImages covering own image registry|--set|plugin.matchImages={ghcr.io}|chicken-and-egg"
)

failed=0
for case in "${cases[@]}"; do
  IFS='|' read -r label flag setval want <<< "${case}"
  out=$(render -f "${COMPLETE}" "${flag}" "${setval}" 2>&1 || true)
  if echo "${out}" | grep -qF "${want}"; then
    echo "PASS  ${label}"
  else
    echo "FAIL  ${label}"
    echo "      expected error containing: ${want}"
    echo "      got: ${out}" | head -3
    failed=$((failed+1))
  fi
done

# plugin.matchImages defaults to [] which is "set" but empty; clearing via
# --set doesn't reproduce the empty-list path. Use a values overlay.
cat <<'YAML' > "${TMP}/values-no-matchimages.yaml"
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
YAML
out=$(render -f "${TMP}/values-no-matchimages.yaml" 2>&1 || true)
if echo "${out}" | grep -q "plugin.matchImages is REQUIRED"; then
  echo "PASS  plugin.matchImages (empty list)"
else
  echo "FAIL  plugin.matchImages (empty list)"
  echo "      got: ${out}" | head -3
  failed=$((failed+1))
fi

# plugin.enabled=false: matchImages is not required, and no plugin object
# may render (issue #46).
if out=$(render -f "${TMP}/values-no-matchimages.yaml" --set plugin.enabled=false 2>&1); then
  if echo "${out}" | grep -qE '^kind: DaemonSet|name: harbor-bridge-plugin$'; then
    echo "FAIL  plugin.enabled=false still renders plugin objects"
    failed=$((failed+1))
  elif ! echo "${out}" | grep -q 'request-serviceaccounts-token-audience'; then
    echo "FAIL  plugin.enabled=false dropped the kubelet audience RBAC"
    failed=$((failed+1))
  else
    echo "PASS  plugin.enabled=false renders bridge + audience RBAC only"
  fi
else
  echo "FAIL  plugin.enabled=false without matchImages must render"
  echo "      got: ${out}" | head -3
  failed=$((failed+1))
fi

# Inverse case: the chicken-and-egg guard must be bypassable for
# air-gapped mirrors (plugin.allowSelfMatchImages=true → render succeeds).
if render -f "${COMPLETE}" --set 'plugin.matchImages={ghcr.io}' \
     --set plugin.allowSelfMatchImages=true > /dev/null 2>&1; then
  echo "PASS  allowSelfMatchImages bypasses the chicken-and-egg guard"
else
  echo "FAIL  allowSelfMatchImages bypasses the chicken-and-egg guard"
  failed=$((failed+1))
fi

if [ "${failed}" -gt 0 ]; then
  echo
  echo "${failed} required-value test(s) failed"
  exit 1
fi
echo
echo "all required-value gates fire as expected"
