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
  "harbor.url over plain http|--set|harbor.url=http://harbor.example.com|uses plain http"
  "malformed image digest|--set|plugin.image.digest=sha256:nothex|must be sha256:<64 hex characters>"
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
  "plugin.namespace without a CA bundle source|--set|plugin.namespace=harbor-bridge-plugin|needs the bridge CA through trust-manager"
  "plugin.namespace with a namespaced mTLS issuer|--set|plugin.namespace=harbor-bridge-plugin,plugin.caBundle.source.secret.name=ca,bridge.mTLS.enabled=true,bridge.mTLS.clientIssuerRef.name=x,bridge.mTLS.clientIssuerRef.kind=Issuer|must be ClusterIssuer when plugin.namespace is set"
  "bridge.instance with a selector|--set|bridge.harborAccessSelector.a=b,bridge.instance=Bad_Name|must be a DNS label of at most 50 characters"
  "bridge.tokenValidation.maxLifetime not a duration|--set|bridge.tokenValidation.maxLifetime=3600|must be a positive Go duration"
  "bridge.tokenValidation.maxLifetime zero|--set|bridge.tokenValidation.maxLifetime=0s|must be a positive Go duration"
  "bridge.tokenValidation.maxLifetime negative|--set|bridge.tokenValidation.maxLifetime=-1h|must be a positive Go duration"
  "bridge.tokenValidation.requirePodBinding not a boolean|--set-string|bridge.tokenValidation.requirePodBinding=yes|must be true or false"
  "plugin.providerName with a dot|--set|plugin.providerName=harbor.bridge|plugin.providerName=\"harbor.bridge\" must be a DNS label"
  "plugin.providerName with a path|--set|plugin.providerName=../kubelet|plugin.providerName=\"../kubelet\" must be a DNS label"
  "plugin.providerName in upper case|--set|plugin.providerName=Harbor|plugin.providerName=\"Harbor\" must be a DNS label"
  "plugin.providerName over 63 characters|--set|plugin.providerName=$(printf 'a%.0s' {1..64})|must be a DNS label (lower-case letters, digits and -, at most 63 characters): it names the kubelet"
  "plugin.providerName empty|--set|plugin.providerName=|plugin.providerName=\"\" must be a DNS label"
  "plugin.providerName checked with plugin.enabled=false|--set|plugin.enabled=false,plugin.providerName=a_b|plugin.providerName=\"a_b\" must be a DNS label"
  "plugin.providerName read as a number|--set|plugin.providerName=123|plugin.providerName must be a string, but it was read as the int64 123"
  "plugin.hostBinaryDir equal to plugin.hostConfigDir|--set|plugin.hostBinaryDir=/etc/kubernetes/credential-provider-config|plugin.hostBinaryDir=\"/etc/kubernetes/credential-provider-config\" and plugin.hostConfigDir=\"/etc/kubernetes/credential-provider-config\" must not be the same directory or inside one another"
  "plugin.hostBinaryDir inside plugin.hostConfigDir|--set|plugin.hostBinaryDir=/etc/kubernetes/credential-provider-config/bin|must not be the same directory or inside one another"
  "plugin.hostConfigDir inside plugin.hostBinaryDir|--set|plugin.hostConfigDir=/etc/kubernetes/credential-provider/config|must not be the same directory or inside one another"
  "plugin.install.binDir inside plugin.hostConfigDir|--set|plugin.install.binDir=/etc/kubernetes/credential-provider-config/bin,plugin.install.configFile=/etc/cp.yaml|plugin.install.binDir=\"/etc/kubernetes/credential-provider-config/bin\" and plugin.hostConfigDir"
  "plugin.install.stateDir equal to plugin.hostConfigDir|--set|plugin.install.stateDir=/etc/kubernetes/credential-provider-config|plugin.install.stateDir=\"/etc/kubernetes/credential-provider-config\" must not be plugin.hostConfigDir"
  "plugin.install.stateDir inside plugin.hostConfigDir|--set|plugin.install.stateDir=/etc/kubernetes/credential-provider-config/state|must not be plugin.hostConfigDir"
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

# An unquoted yes in a values file is YAML's boolean true, not the name
# "yes": refused, never rendered as the provider name "true" (ADR-0029).
printf 'plugin:\n  providerName: yes\n' > "${TMP}/values-provider-yes.yaml"
out=$(render -f "${COMPLETE}" -f "${TMP}/values-provider-yes.yaml" 2>&1 || true)
if echo "${out}" | grep -qF "plugin.providerName must be a string, but it was read as the bool true"; then
  echo "PASS  plugin.providerName: yes (unquoted) in a values file"
else
  echo "FAIL  plugin.providerName: yes (unquoted) in a values file"
  echo "      got: ${out}" | head -3
  failed=$((failed+1))
fi

# The same name as a string renders it in the entry and in every node path.
if out=$(render -f "${COMPLETE}" --set-string plugin.providerName=123 2>&1) \
   && echo "${out}" | grep -qF -- '- name: "123"' \
   && echo "${out}" | grep -qF '/etc/kubernetes/credential-provider-config/123.ca.crt"' \
   && ! echo "${out}" | grep -qF '%!'; then
  echo "PASS  plugin.providerName=123 via --set-string renders the name everywhere"
else
  echo "FAIL  plugin.providerName=123 via --set-string renders the name everywhere"
  echo "      got: ${out}" | grep -E 'name: "123"|ca.crt|%!|Error' | head -3
  failed=$((failed+1))
fi

# A missing plugin.providerName renders exactly the default. `helm upgrade
# --reuse-values` from a chart before ADR-0029 renders with that chart's
# values, which lack the key; a null value removes the key the same way.
if out=$(render -f "${COMPLETE}" --set plugin.providerName=null 2>&1) \
   && [ "${out}" = "$(render -f "${COMPLETE}")" ]; then
  echo "PASS  missing plugin.providerName (--reuse-values) renders the default"
else
  echo "FAIL  missing plugin.providerName (--reuse-values) renders the default"
  echo "      got: ${out}" | head -3
  failed=$((failed+1))
fi

# The overlap check compares per path segment: the defaults
# /etc/kubernetes/credential-provider and
# /etc/kubernetes/credential-provider-config share a string prefix but no
# directory, and the state dir may be a parent of plugin.hostConfigDir.
if render -f "${COMPLETE}" --set plugin.install.stateDir=/etc/kubernetes > /dev/null 2>&1; then
  echo "PASS  plugin directories compared per path segment"
else
  echo "FAIL  plugin directories compared per path segment"
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

# ADR-0028: the token policy reaches the bridge's environment.
if out=$(render -f "${COMPLETE}" --set bridge.tokenValidation.maxLifetime=90m \
     --set bridge.tokenValidation.requirePodBinding=false 2>&1) \
   && echo "${out}" | grep -A1 'name: BRIDGE_TOKEN_MAX_LIFETIME' | grep -q 'value: "90m"' \
   && echo "${out}" | grep -A1 'name: BRIDGE_REQUIRE_POD_BOUND_TOKEN' | grep -q 'value: "false"'; then
  echo "PASS  bridge.tokenValidation renders into the bridge env"
else
  echo "FAIL  bridge.tokenValidation renders into the bridge env"
  echo "      got: ${out}" | head -3
  failed=$((failed+1))
fi

# `helm upgrade --reuse-values` from a release that predates
# bridge.tokenValidation keeps the old values, which lack the key. A null
# --set removes it the same way; the render must equal the default one.
if [ "$(render -f "${COMPLETE}" 2>&1)" = "$(render -f "${COMPLETE}" --set bridge.tokenValidation=null 2>&1)" ]; then
  echo "PASS  a missing bridge.tokenValidation renders the defaults (--reuse-values)"
else
  echo "FAIL  a missing bridge.tokenValidation renders the defaults (--reuse-values)"
  failed=$((failed+1))
fi

if [ "${failed}" -gt 0 ]; then
  echo
  echo "${failed} required-value test(s) failed"
  exit 1
fi
echo
echo "all required-value gates fire as expected"
