#!/usr/bin/env bash
# Verify every required value gate fails template rendering with a
# clear message. If any of these *succeeds*, a required-value check
# was silently dropped — caught by the test.
set -euo pipefail
# Feed a render to grep and head with here-strings (grep -q ... <<<"${out}"),
# never through a pipe from echo: grep -q and head exit before they have read
# everything, echo then dies of SIGPIPE (exit 141) while it still writes the
# render, and pipefail turns that into a failed check (or, outside a
# condition, set -e into an aborted run).

CHART_DIR="${CHART_DIR:-charts/harbor-bridge}"
COMPLETE="${CHART_DIR}/tests/values-complete.yaml"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

render() {
  helm template harbor-bridge "${CHART_DIR}" --kube-version 1.34.0 \
    --namespace harbor-bridge-system "$@"
}

# notes renders like render, plus the NOTES (helm template leaves them out).
notes() {
  helm install harbor-bridge "${CHART_DIR}" --dry-run=client \
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
  "plugin.install.binDir/configFile in mode auto|--set|plugin.install.binDir=/cloud/bin,plugin.install.configFile=/cloud/c.yaml|are used only with plugin.install.mode=merge, not \"auto\""
  "plugin.install.binDir/configFile in mode patch|--set|plugin.install.mode=patch,plugin.install.binDir=/cloud/bin,plugin.install.configFile=/cloud/c.yaml|are used only with plugin.install.mode=merge, not \"patch\""
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
  "plugin.install.binDir inside plugin.hostConfigDir|--set|plugin.install.mode=merge,plugin.install.binDir=/etc/kubernetes/credential-provider-config/bin,plugin.install.configFile=/etc/cp.yaml|plugin.install.binDir=\"/etc/kubernetes/credential-provider-config/bin\" and plugin.hostConfigDir"
  "plugin.install.stateDir equal to plugin.hostConfigDir|--set|plugin.install.stateDir=/etc/kubernetes/credential-provider-config|plugin.install.stateDir=\"/etc/kubernetes/credential-provider-config\" must not be plugin.hostConfigDir"
  "plugin.install.stateDir inside plugin.hostConfigDir|--set|plugin.install.stateDir=/etc/kubernetes/credential-provider-config/state|must not be plugin.hostConfigDir"
  "plugin.install.configFile inside plugin.hostConfigDir|--set|plugin.install.mode=merge,plugin.install.binDir=/etc/cp-bin,plugin.install.configFile=/etc/kubernetes/credential-provider-config/credential-provider-config.yaml|plugin.install.configFile=\"/etc/kubernetes/credential-provider-config/credential-provider-config.yaml\" must not be inside plugin.hostConfigDir"
  "clusterName with consecutive hyphens|--set|clusterName=prod--eu|clusterName=\"prod--eu\" must not contain consecutive hyphens"
  "harbor.url with credentials|--set|harbor.url=https://admin:s3cret@harbor.example.com|harbor.url must not contain \"@\""
  "harbor.url with an @ after the host|--set|harbor.url=https://harbor.example.com/a@b|harbor.url must not contain \"@\""
  "harbor.url without a scheme|--set|harbor.url=harbor.example.com|harbor.url must be an http:// or https:// URL with a host"
  "harbor.url over plain http in upper case|--set|harbor.url=HTTP://harbor.example.com|uses plain http"
  "bridge.oidcIssuer with credentials|--set|bridge.oidcIssuer=https://user:s3cret@kubernetes.default.svc.cluster.local|bridge.oidcIssuer must not contain \"@\""
  "bridge.oidcIssuer empty|--set|bridge.oidcIssuer=|bridge.oidcIssuer must be an http:// or https:// URL with a host"
  "bridge.oidcJWKSURL with an @ after the host|--set|bridge.oidcJWKSURL=https://jwks:s3cr/et@jwks.example.com/keys|bridge.oidcJWKSURL has an \"@\" after its host part"
  "bridge.oidcJWKSURL without a host|--set|bridge.oidcJWKSURL=https://jwks:s3cret@/keys|bridge.oidcJWKSURL must be an http:// or https:// URL with a host"
  "bridge.leaderElection not a boolean|--set|bridge.leaderElection=on|bridge.leaderElection must be true, false or null (null: on when bridge.replicas > 1), but it was read as the string on"
  "bridge.leaderElection=false with several replicas|--set|bridge.leaderElection=false|bridge.leaderElection=false with bridge.replicas=2: every replica would run the reconciler and the janitor"
  "bridge.harborAccessSelector value read as a boolean|--set|bridge.harborAccessSelector.eu=true|bridge.harborAccessSelector.eu must be a string, but it was read as the bool true"
  "bridge.harborAccessSelector value read as a number|--set|bridge.harborAccessSelector.tier=1|bridge.harborAccessSelector.tier must be a string, but it was read as the int64 1"
  "bridge.rateLimit.burst not a whole number|--set|bridge.rateLimit.burst=1.5|bridge.rateLimit.burst=1.5 must be a positive whole number"
  "bridge.rateLimit.burst zero|--set|bridge.rateLimit.burst=0|bridge.rateLimit.burst=0 must be a positive whole number"
)

failed=0
for case in "${cases[@]}"; do
  IFS='|' read -r label flag setval want <<< "${case}"
  out=$(render -f "${COMPLETE}" "${flag}" "${setval}" 2>&1 || true)
  if grep -qF "${want}" <<<"${out}"; then
    echo "PASS  ${label}"
  else
    echo "FAIL  ${label}"
    echo "      expected error containing: ${want}"
    head -3 <<<"      got: ${out}"
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
if grep -q "plugin.matchImages is REQUIRED" <<<"${out}"; then
  echo "PASS  plugin.matchImages (empty list)"
else
  echo "FAIL  plugin.matchImages (empty list)"
  head -3 <<<"      got: ${out}"
  failed=$((failed+1))
fi

# plugin.enabled=false: matchImages is not required, and no plugin object
# may render (issue #46).
if out=$(render -f "${TMP}/values-no-matchimages.yaml" --set plugin.enabled=false 2>&1); then
  if grep -qE '^kind: DaemonSet|name: harbor-bridge-plugin$' <<<"${out}"; then
    echo "FAIL  plugin.enabled=false still renders plugin objects"
    failed=$((failed+1))
  elif ! grep -q 'request-serviceaccounts-token-audience' <<<"${out}"; then
    echo "FAIL  plugin.enabled=false dropped the kubelet audience RBAC"
    failed=$((failed+1))
  else
    echo "PASS  plugin.enabled=false renders bridge + audience RBAC only"
  fi
else
  echo "FAIL  plugin.enabled=false without matchImages must render"
  head -3 <<<"      got: ${out}"
  failed=$((failed+1))
fi

# An unquoted yes in a values file is YAML's boolean true, not the name
# "yes": refused, never rendered as the provider name "true" (ADR-0029).
printf 'plugin:\n  providerName: yes\n' > "${TMP}/values-provider-yes.yaml"
out=$(render -f "${COMPLETE}" -f "${TMP}/values-provider-yes.yaml" 2>&1 || true)
if grep -qF "plugin.providerName must be a string, but it was read as the bool true" <<<"${out}"; then
  echo "PASS  plugin.providerName: yes (unquoted) in a values file"
else
  echo "FAIL  plugin.providerName: yes (unquoted) in a values file"
  head -3 <<<"      got: ${out}"
  failed=$((failed+1))
fi

# The same name as a string renders it in the entry and in every node path.
if out=$(render -f "${COMPLETE}" --set-string plugin.providerName=123 2>&1) \
   && grep -qF -- '- name: "123"' <<<"${out}" \
   && grep -qF '/etc/kubernetes/credential-provider-config/123.ca.crt"' <<<"${out}" \
   && ! grep -qF '%!' <<<"${out}"; then
  echo "PASS  plugin.providerName=123 via --set-string renders the name everywhere"
else
  echo "FAIL  plugin.providerName=123 via --set-string renders the name everywhere"
  grep -m 3 -E 'name: "123"|ca.crt|%!|Error' <<<"${out}" || true
  failed=$((failed+1))
fi

# ADR-0029: a non-default provider name publishes the rendered config under
# a ConfigMap key and at a mount path that installers before ADR-0029 never
# read (they read /config/credential-provider-config.yaml), so an older
# plugin image fails at that read, before it writes anything on the node.
# The default name keeps the layout every installer reads.
out=$(render -f "${COMPLETE}" --set plugin.providerName=harbor-bridge-eu 2>&1 || true)
if grep -qxF '  credential-provider-config.v2.yaml: |' <<<"${out}" \
   && ! grep -qxF '  credential-provider-config.yaml: |' <<<"${out}" \
   && [ "$(grep -cxE ' +mountPath: /config-v2' <<<"${out}")" -eq 2 ] \
   && ! grep -qxE ' +mountPath: /config' <<<"${out}"; then
  echo "PASS  non-default plugin.providerName: rendered config where older installers never read"
else
  echo "FAIL  non-default plugin.providerName: rendered config where older installers never read"
  grep -m 5 -E 'credential-provider-config|mountPath: /config|Error' <<<"${out}" || true
  failed=$((failed+1))
fi
out=$(render -f "${COMPLETE}" 2>&1 || true)
if grep -qxF '  credential-provider-config.yaml: |' <<<"${out}" \
   && [ "$(grep -cxE ' +mountPath: /config' <<<"${out}")" -eq 2 ] \
   && ! grep -qE 'config-v2|config\.v2' <<<"${out}"; then
  echo "PASS  default plugin.providerName keeps the rendered config's layout"
else
  echo "FAIL  default plugin.providerName keeps the rendered config's layout"
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
  head -3 <<<"      got: ${out}"
  failed=$((failed+1))
fi

# The overlap check compares per path segment: the defaults
# /etc/kubernetes/credential-provider and
# /etc/kubernetes/credential-provider-config share a string prefix but no
# directory, and the state dir may be a parent of plugin.hostConfigDir. A
# merge config whose name merely extends plugin.hostConfigDir is not in it.
if render -f "${COMPLETE}" --set plugin.install.stateDir=/etc/kubernetes > /dev/null 2>&1 \
   && render -f "${COMPLETE}" --set plugin.install.mode=merge,plugin.install.binDir=/etc/cp-bin,plugin.install.configFile=/etc/kubernetes/credential-provider-config.yaml > /dev/null 2>&1; then
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

# A refused URL setting never repeats the credential it may carry, neither
# in the error nor in the NOTES (they print harbor.url).
leaked=""
for setval in 'harbor.url=https://admin:s3cret@harbor.example.com' \
              'bridge.oidcIssuer=https://admin:s3cret@kubernetes.default.svc.cluster.local' \
              'bridge.oidcJWKSURL=https://jwks:s3cr/et@jwks.example.com/keys' \
              'bridge.oidcJWKSURL=s3cret:pw@jwks.example.com/keys'; do
  for cmd in render notes; do
    out=$("${cmd}" -f "${COMPLETE}" --set "${setval}" 2>&1 || true)
    if ! grep -qF 'it could hold a credential' <<<"${out}" || grep -q 's3cr' <<<"${out}"; then
      leaked+=" ${cmd}:${setval%%=*}"
    fi
  done
done
if [ -z "${leaked}" ]; then
  echo "PASS  refused URL settings never repeat their credentials"
else
  echo "FAIL  refused URL settings never repeat their credentials:${leaked}"
  failed=$((failed+1))
fi

# bridge.oidcJWKSURL may carry user:password@ (net/http sends it as Basic
# auth), and an @ written as %40 after the host stays allowed everywhere.
if out=$(render -f "${COMPLETE}" --set 'bridge.oidcJWKSURL=https://jwks:pw@jwks.example.com/keys%40v1' \
     --set 'harbor.url=https://harbor.example.com/a%40b' 2>&1) \
   && grep -qF 'value: "https://jwks:pw@jwks.example.com/keys%40v1"' <<<"${out}" \
   && grep -qF 'value: "https://harbor.example.com/a%40b"' <<<"${out}"; then
  echo "PASS  bridge.oidcJWKSURL with credentials, and %40 after the host, render"
else
  echo "FAIL  bridge.oidcJWKSURL with credentials, and %40 after the host, render"
  head -3 <<<"      got: ${out}"
  failed=$((failed+1))
fi

# The NOTES print harbor.url; the check above keeps credentials out of it.
if out=$(notes -f "${COMPLETE}" 2>&1) \
   && grep -qF 'Harbor URL   : https://harbor.example.com' <<<"${out}"; then
  echo "PASS  NOTES print harbor.url"
else
  echo "FAIL  NOTES print harbor.url"
  head -3 <<<"      got: ${out}"
  failed=$((failed+1))
fi

# One replica may run without leader election; true stays true.
if out=$(render -f "${COMPLETE}" --set bridge.replicas=1,bridge.leaderElection=false 2>&1) \
   && grep -q 'value: "false"' <<<"$(grep -A1 'name: BRIDGE_ENABLE_LEADER_ELECTION' <<<"${out}")" \
   && out=$(render -f "${COMPLETE}" --set bridge.leaderElection=true 2>&1) \
   && grep -q 'value: "true"' <<<"$(grep -A1 'name: BRIDGE_ENABLE_LEADER_ELECTION' <<<"${out}")"; then
  echo "PASS  bridge.leaderElection=false with one replica, and true, render"
else
  echo "FAIL  bridge.leaderElection=false with one replica, and true, render"
  head -3 <<<"      got: ${out}"
  failed=$((failed+1))
fi

# Unquoted label values in a values file are read as numbers or booleans:
# refused instead of rendered as tier=%!s(float64=1). Quoted, they render
# as written.
printf 'bridge:\n  harborAccessSelector:\n    tier: 1\n' > "${TMP}/values-selector-number.yaml"
out=$(render -f "${COMPLETE}" -f "${TMP}/values-selector-number.yaml" 2>&1 || true)
if grep -qF 'bridge.harborAccessSelector.tier must be a string, but it was read as the float64 1' <<<"${out}"; then
  echo "PASS  unquoted bridge.harborAccessSelector value in a values file"
else
  echo "FAIL  unquoted bridge.harborAccessSelector value in a values file"
  head -3 <<<"      got: ${out}"
  failed=$((failed+1))
fi
printf 'bridge:\n  harborAccessSelector:\n    tier: "1"\n    harbor.aetherize.io/eu: "true"\n' > "${TMP}/values-selector-strings.yaml"
if out=$(render -f "${COMPLETE}" -f "${TMP}/values-selector-strings.yaml" 2>&1) \
   && grep -qF 'value: "harbor.aetherize.io/eu=true,tier=1"' <<<"${out}"; then
  echo "PASS  quoted bridge.harborAccessSelector values render as written"
else
  echo "FAIL  quoted bridge.harborAccessSelector values render as written"
  head -3 <<<"      got: ${out}"
  failed=$((failed+1))
fi

# A values file reads numbers as float64; a burst of a million must still
# render as the integer the bridge parses, not as 1e+06.
printf 'bridge:\n  rateLimit:\n    burst: 1000000\n' > "${TMP}/values-burst.yaml"
if out=$(render -f "${COMPLETE}" -f "${TMP}/values-burst.yaml" 2>&1) \
   && grep -q 'value: "1000000"' <<<"$(grep -A1 'name: BRIDGE_RATE_LIMIT_BURST' <<<"${out}")"; then
  echo "PASS  bridge.rateLimit.burst from a values file renders as an integer"
else
  echo "FAIL  bridge.rateLimit.burst from a values file renders as an integer"
  grep -m 2 -A1 -E 'BRIDGE_RATE_LIMIT_BURST|Error' <<<"${out}" || true
  failed=$((failed+1))
fi

# ADR-0028: the token policy reaches the bridge's environment.
if out=$(render -f "${COMPLETE}" --set bridge.tokenValidation.maxLifetime=90m \
     --set bridge.tokenValidation.requirePodBinding=false 2>&1) \
   && grep -q 'value: "90m"' <<<"$(grep -A1 'name: BRIDGE_TOKEN_MAX_LIFETIME' <<<"${out}")" \
   && grep -q 'value: "false"' <<<"$(grep -A1 'name: BRIDGE_REQUIRE_POD_BOUND_TOKEN' <<<"${out}")"; then
  echo "PASS  bridge.tokenValidation renders into the bridge env"
else
  echo "FAIL  bridge.tokenValidation renders into the bridge env"
  head -3 <<<"      got: ${out}"
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
