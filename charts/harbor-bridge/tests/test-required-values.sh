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
  "bridge.oidcJWKSURL with credentials|--set|bridge.oidcJWKSURL=https://jwks:s3cret@jwks.example.com/keys|bridge.oidcJWKSURL must not contain \"@\": a user:password@ part here would be stored in plain text"
  "bridge.oidcJWKSURL with an @ after the host|--set|bridge.oidcJWKSURL=https://jwks:s3cr/et@jwks.example.com/keys|bridge.oidcJWKSURL must not contain \"@\""
  "bridge.oidcJWKSURL without a host|--set|bridge.oidcJWKSURL=https:///keys|bridge.oidcJWKSURL must be an http:// or https:// URL with a host"
  "bridge.oidcJWKSURL over plain http|--set|bridge.oidcJWKSURL=http://jwks.example.com/keys|bridge.oidcJWKSURL uses plain http"
  "bridge.oidcJWKSURL over plain http to a name that starts like a loopback address|--set|bridge.oidcJWKSURL=http://127.0.0.1.example.com/keys|bridge.oidcJWKSURL uses plain http"
  "bridge.oidcIssuer over plain http for discovery|--set|bridge.oidcIssuer=http://issuer.example.com|bridge.oidcIssuer uses plain http"
  "bridge.oidcIssuer over plain http in upper case|--set|bridge.oidcIssuer=HTTP://issuer.example.com|bridge.oidcIssuer uses plain http"
  "bridge.leaderElection not a boolean|--set|bridge.leaderElection=on|bridge.leaderElection must be true, false or null (null: on when bridge.replicas > 1), but it was read as the string on"
  "bridge.leaderElection=false with several replicas|--set|bridge.leaderElection=false|bridge.leaderElection=false with bridge.replicas=2: every replica would run the reconciler and the janitor"
  "bridge.harborAccessSelector value read as a boolean|--set|bridge.harborAccessSelector.eu=true|bridge.harborAccessSelector.eu must be a string, but it was read as the bool true"
  "bridge.harborAccessSelector value read as a number|--set|bridge.harborAccessSelector.tier=1|bridge.harborAccessSelector.tier must be a string, but it was read as the int64 1"
  "bridge.rateLimit.burst not a whole number|--set|bridge.rateLimit.burst=1.5|bridge.rateLimit.burst=1.5 must be a positive whole number"
  "bridge.rateLimit.burst zero|--set|bridge.rateLimit.burst=0|bridge.rateLimit.burst=0 must be a positive whole number"
  "plugin.defaultCacheDuration in days|--set|plugin.defaultCacheDuration=1d|plugin.defaultCacheDuration=\"1d\" must be a Go duration of at least 0"
  "plugin.defaultCacheDuration without a unit|--set|plugin.defaultCacheDuration=3600|plugin.defaultCacheDuration=\"3600\" must be a Go duration"
  "plugin.defaultCacheDuration with a space|--set|plugin.defaultCacheDuration=24 h|plugin.defaultCacheDuration=\"24 h\" must be a Go duration"
  "plugin.defaultCacheDuration negative|--set|plugin.defaultCacheDuration=-1h|plugin.defaultCacheDuration=\"-1h\" must be a Go duration of at least 0"
  "plugin.matchImages entry with a port glob|--set|plugin.matchImages={harbor.example.com:*}|plugin.matchImages entry \"harbor.example.com:*\" is not host[:port][/path]"
  "plugin.matchImages entry with a scheme|--set|plugin.matchImages={https://harbor.example.com}|plugin.matchImages entry \"https://harbor.example.com\" is not host[:port][/path]"
  "plugin.matchImages entry with a path glob|--set|plugin.matchImages={harbor.example.com/*}|plugin.matchImages entry \"harbor.example.com/*\" is not host[:port][/path]"
  "plugin.matchImages entry with a space|--set-json|plugin.matchImages=[\"harbor.example.com \"]|plugin.matchImages entry \"harbor.example.com \" is not host[:port][/path]"
  "plugin.matchImages entry with a %-escape|--set|plugin.matchImages={harbor.example.com/a%zz}|plugin.matchImages entry \"harbor.example.com/a%zz\" is not host[:port][/path]"
  "service.type ClusterIP with the default endpoint|--set|service.type=ClusterIP|service.type=ClusterIP gives the bridge no node port, but with plugin.bridgeEndpoint empty the plugin calls https://127.0.0.1:<service.nodePort> (ADR-0008). Use service.type=NodePort, or set plugin.bridgeEndpoint"
  "service.nodePort null with the default endpoint|--set|service.nodePort=null|service.nodePort=null must be a fixed port while plugin.bridgeEndpoint is empty"
  "service.nodePort not a port with the default endpoint|--set|service.nodePort=70000|service.nodePort=70000 must be a fixed port"
  "plugin.priorityClassName not a string|--set|plugin.priorityClassName=true|plugin.priorityClassName must be a string (a PriorityClass name, or \"\" to leave it out), but it was read as the bool true"
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
              'bridge.oidcJWKSURL=https://jwks:s3cret@jwks.example.com/keys' \
              'bridge.oidcJWKSURL=https://jwks:s3cr/et@jwks.example.com/keys' \
              'bridge.oidcJWKSURL=s3cret:pw@jwks.example.com/keys' \
              'bridge.oidcJWKSURL=http://s3cret.example.com/keys'; do
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

# An @ written as %40 after the host stays allowed.
if out=$(render -f "${COMPLETE}" --set 'bridge.oidcJWKSURL=https://jwks.example.com/keys%40v1' \
     --set 'harbor.url=https://harbor.example.com/a%40b' 2>&1) \
   && grep -qF 'value: "https://jwks.example.com/keys%40v1"' <<<"${out}" \
   && grep -qF 'value: "https://harbor.example.com/a%40b"' <<<"${out}"; then
  echo "PASS  %40 after the host renders"
else
  echo "FAIL  %40 after the host renders"
  head -3 <<<"      got: ${out}"
  failed=$((failed+1))
fi

# Plain http stays allowed where the bridge allows it: to a loopback host,
# and for an issuer the bridge fetches nothing from because a JWKS URL is
# set (bridge.oidcJWKSURL, or BRIDGE_OIDC_JWKS_URL in bridge.extraEnv, the
# way to pass one with credentials from a Secret).
refused=""
for args in 'bridge.oidcJWKSURL=http://127.0.0.1:8001/openid/v1/jwks' \
            'bridge.oidcJWKSURL=http://127.10.0.1:8001/keys' \
            'bridge.oidcJWKSURL=http://localhost:8001/keys' \
            'bridge.oidcJWKSURL=HTTP://LocalHost/keys' \
            'bridge.oidcJWKSURL=http://[::1]:8001/keys' \
            'bridge.oidcIssuer=http://127.0.0.1:8001' \
            'bridge.oidcIssuer=http://issuer.example.com,bridge.oidcJWKSURL=https://kubernetes.default.svc/openid/v1/jwks'; do
  render -f "${COMPLETE}" --set "${args}" > /dev/null 2>&1 || refused+=" ${args}"
done
render -f "${COMPLETE}" --set bridge.oidcIssuer=http://issuer.example.com \
  --set-json 'bridge.extraEnv=[{"name":"BRIDGE_OIDC_JWKS_URL","valueFrom":{"secretKeyRef":{"name":"jwks","key":"url"}}}]' \
  > /dev/null 2>&1 || refused+=" extraEnv"
if [ -z "${refused}" ]; then
  echo "PASS  plain http to a loopback host, and an http issuer with a JWKS URL, render"
else
  echo "FAIL  plain http to a loopback host, and an http issuer with a JWKS URL, render:${refused}"
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

# Durations kubelet accepts render as written, including 0 and a leading
# fraction.
ok=""
for d in 0 .5h 1h30m 90s; do
  out=$(render -f "${COMPLETE}" --set-string "plugin.defaultCacheDuration=${d}" 2>&1 || true)
  grep -qxF "        defaultCacheDuration: \"${d}\"" <<<"${out}" || ok+=" ${d}"
done
if [ -z "${ok}" ]; then
  echo "PASS  valid plugin.defaultCacheDuration values render"
else
  echo "FAIL  valid plugin.defaultCacheDuration values render:${ok}"
  failed=$((failed+1))
fi

# matchImages forms kubelet matches: host:port, a literal path prefix, a
# glob in a host label, an IPv6 literal, a digest path.
if out=$(render -f "${COMPLETE}" --set 'plugin.matchImages={harbor.e2e:30843/your-project,*.harbor.example.com,registry.internal:5000,[fd00::1]:5000,harbor.example.com/lib/img@sha256:ab}' 2>&1) \
   && grep -qxF '          - "[fd00::1]:5000"' <<<"${out}" \
   && grep -qxF '          - "harbor.e2e:30843/your-project"' <<<"${out}"; then
  echo "PASS  valid plugin.matchImages forms render"
else
  echo "FAIL  valid plugin.matchImages forms render"
  head -3 <<<"      got: ${out}"
  failed=$((failed+1))
fi

# ADR-0008: a LoadBalancer Service allocates node ports too, so it keeps
# the one the default endpoint names. With an explicit plugin.bridgeEndpoint
# any Service type works, and a null nodePort lets the apiserver pick one.
if out=$(render -f "${COMPLETE}" --set service.type=LoadBalancer 2>&1) \
   && grep -qxF '      nodePort: 31443' <<<"${out}" \
   && out=$(render -f "${COMPLETE}" --set service.type=ClusterIP,plugin.bridgeEndpoint=https://bridge.example.com:8443 2>&1) \
   && ! grep -q 'nodePort' <<<"${out}" \
   && out=$(render -f "${COMPLETE}" --set service.nodePort=null,plugin.bridgeEndpoint=https://bridge.example.com:8443 2>&1) \
   && grep -qxF '  type: NodePort' <<<"${out}" && ! grep -q 'nodePort' <<<"${out}" \
   && out=$(render -f "${COMPLETE}" --set plugin.bridgeEndpoint=https://bridge.example.com:8443 2>&1) \
   && grep -qxF '      nodePort: 31443' <<<"${out}"; then
  echo "PASS  service.type and service.nodePort with a node port or an explicit endpoint"
else
  echo "FAIL  service.type and service.nodePort with a node port or an explicit endpoint"
  head -3 <<<"      got: ${out}"
  failed=$((failed+1))
fi

# A LoadBalancer Service that the default endpoint of the chart's plugin
# does not use keeps the node port the apiserver picked: pinning it to
# service.nodePort would move it on upgrade, and fail when another Service
# (another release's default NodePort) holds that port.
if out=$(render -f "${COMPLETE}" --set service.type=LoadBalancer,plugin.bridgeEndpoint=https://10.0.0.5:8443 -s templates/bridge-service.yaml 2>&1) \
   && grep -qxF '  type: LoadBalancer' <<<"${out}" && ! grep -q 'nodePort' <<<"${out}" \
   && out=$(render -f "${COMPLETE}" --set service.type=LoadBalancer,plugin.enabled=false -s templates/bridge-service.yaml 2>&1) \
   && grep -qxF '  type: LoadBalancer' <<<"${out}" && ! grep -q 'nodePort' <<<"${out}"; then
  echo "PASS  LoadBalancer keeps its node port without the default endpoint of the chart's plugin"
else
  echo "FAIL  LoadBalancer keeps its node port without the default endpoint of the chart's plugin"
  head -3 <<<"      got: ${out}"
  failed=$((failed+1))
fi

# Without the chart's plugin nothing calls the default endpoint: any Service
# type and a dynamic node port render, and the NOTES print a placeholder
# (with the server name the chart's certificate needs) instead of a
# 127.0.0.1 URL that reaches nothing.
ok=""
for args in service.type=ClusterIP service.nodePort=null service.type=LoadBalancer; do
  if out=$(notes -f "${COMPLETE}" --set "plugin.enabled=false,${args}" 2>&1) \
     && grep -qF 'value: "<https URL of the bridge that every node reaches>"' <<<"${out}" \
     && grep -qF 'value: "harbor-bridge.harbor-bridge-system.svc"' <<<"$(grep -A1 'name: HARBOR_BRIDGE_SERVER_NAME' <<<"${out}")" \
     && ! grep -qF '127.0.0.1:31443' <<<"${out}"; then
    :
  else
    ok+=" ${args}"
  fi
done
if [ -z "${ok}" ]; then
  echo "PASS  plugin.enabled=false without a known node port: placeholder endpoint in the NOTES"
else
  echo "FAIL  plugin.enabled=false without a known node port: placeholder endpoint in the NOTES:${ok}"
  failed=$((failed+1))
fi

# The plugin verifies the chart's certificate against the bridge Service's
# name whenever the endpoint's host is not one the certificate names (the
# Service's names, localhost, 127.0.0.1): a cluster IP or a load balancer
# address, as $(NODE_IP) already did. Hosts the certificate names, and an
# operator-provided certificate, get no server name, so their rendered
# config does not change (a changed byte restarts kubelet).
svcname='value: "harbor-bridge.harbor-bridge-system.svc"'
servername() { grep -A1 'name: HARBOR_BRIDGE_SERVER_NAME' <<<"${out}" || true; }
if out=$(render -f "${COMPLETE}" --set service.type=ClusterIP,plugin.bridgeEndpoint=https://10.96.0.50:8443 -s templates/plugin-configmap.yaml 2>&1) \
   && grep -qF "${svcname}" <<<"$(servername)" \
   && grep -qF "# The endpoint's host is not in the bridge certificate; verify" <<<"${out}" \
   && out=$(render -f "${COMPLETE}" --set 'plugin.bridgeEndpoint=https://[::1]:31443' -s templates/plugin-configmap.yaml 2>&1) \
   && grep -qF "${svcname}" <<<"$(servername)" \
   && out=$(render -f "${COMPLETE}" --set 'plugin.bridgeEndpoint=https://$(NODE_IP):31443' -s templates/plugin-configmap.yaml 2>&1) \
   && grep -qF "${svcname}" <<<"$(servername)" \
   && grep -qF "# The node's IP is not in the bridge certificate; verify it" <<<"${out}" \
   && out=$(notes -f "${COMPLETE}" --set plugin.enabled=false,plugin.bridgeEndpoint=https://10.96.0.50:8443 2>&1) \
   && grep -qF "${svcname}" <<<"$(servername)" \
   && grep -qF 'value: "https://10.96.0.50:8443"' <<<"${out}"; then
  echo "PASS  explicit endpoint outside the certificate gets the Service's name as server name"
else
  echo "FAIL  explicit endpoint outside the certificate gets the Service's name as server name"
  grep -m 4 -E 'HARBOR_BRIDGE|Error' <<<"${out}" || true
  failed=$((failed+1))
fi
nameless=""
for args in plugin.bridgeEndpoint=https://localhost:31443 \
            plugin.bridgeEndpoint=https://harbor-bridge.harbor-bridge-system.svc.cluster.local:8443 \
            plugin.bridgeEndpoint=https://10.96.0.50:8443,tls.enabled=false,tls.existingSecret=bridge-tls; do
  out=$(render -f "${COMPLETE}" --set "${args}" -s templates/plugin-configmap.yaml 2>&1 || true)
  if ! grep -qF 'HARBOR_BRIDGE_ENDPOINT' <<<"${out}" || grep -qF 'HARBOR_BRIDGE_SERVER_NAME' <<<"${out}"; then
    nameless+=" ${args}"
  fi
done
if [ -z "${nameless}" ] \
   && [ "$(render -f "${COMPLETE}" --set plugin.bridgeEndpoint=https://127.0.0.1:31443 2>&1)" = "$(render -f "${COMPLETE}" 2>&1)" ]; then
  echo "PASS  endpoint the certificate names, or an operator certificate: no server name"
else
  echo "FAIL  endpoint the certificate names, or an operator certificate: no server name:${nameless}"
  failed=$((failed+1))
fi

# plugin.priorityClassName: the default keeps system-node-critical, a name
# replaces it, "" leaves the field out, and a missing key (--reuse-values
# from a chart without it) renders the default.
if out=$(render -f "${COMPLETE}" 2>&1) \
   && grep -qxF '      priorityClassName: system-node-critical' <<<"${out}" \
   && out=$(render -f "${COMPLETE}" --set plugin.priorityClassName=harbor-bridge-node 2>&1) \
   && grep -qxF '      priorityClassName: harbor-bridge-node' <<<"${out}" \
   && out=$(render -f "${COMPLETE}" --set plugin.priorityClassName= 2>&1) \
   && ! grep -q 'priorityClassName' <<<"${out}" \
   && [ "$(render -f "${COMPLETE}" --set plugin.priorityClassName=null 2>&1)" = "$(render -f "${COMPLETE}" 2>&1)" ]; then
  echo "PASS  plugin.priorityClassName default, override, empty and missing"
else
  echo "FAIL  plugin.priorityClassName default, override, empty and missing"
  head -3 <<<"      got: ${out}"
  failed=$((failed+1))
fi

# Both workloads select Linux nodes by default; keys a user adds are merged
# with it, and it can be dropped explicitly.
if out=$(render -f "${COMPLETE}" --set plugin.nodeSelector.pool=infra --set bridge.nodeSelector.pool=infra 2>&1) \
   && [ "$(grep -cxF '        kubernetes.io/os: linux' <<<"${out}")" -eq 2 ] \
   && [ "$(grep -cxF '        pool: infra' <<<"${out}")" -eq 2 ] \
   && out=$(render -f "${COMPLETE}" --set 'plugin.nodeSelector.kubernetes\.io/os=null' 2>&1) \
   && [ "$(grep -cxF '        kubernetes.io/os: linux' <<<"${out}")" -eq 1 ]; then
  echo "PASS  Linux nodeSelector merged with user keys, and removable"
else
  echo "FAIL  Linux nodeSelector merged with user keys, and removable"
  head -3 <<<"      got: ${out}"
  failed=$((failed+1))
fi

# NOTES, uninstall step: a selective bridge's objects carry its per-instance
# finalizer, and objects from before the selector the shared one as well.
if out=$(notes -f "${COMPLETE}" --set bridge.harborAccessSelector.a=b 2>&1) \
   && grep -qF 'the "harbor.aetherize.io/robot-harbor-bridge" finalizer by hand' <<<"${out}" \
   && grep -qF '(and "harbor.aetherize.io/robot", on objects that still carry it from' <<<"${out}" \
   && out=$(notes -f "${COMPLETE}" 2>&1) \
   && grep -qF 'the "harbor.aetherize.io/robot" finalizer by hand and delete' <<<"${out}"; then
  echo "PASS  NOTES name the finalizers to remove by hand"
else
  echo "FAIL  NOTES name the finalizers to remove by hand"
  grep -m 3 -F 'finalizer' <<<"${out}" || true
  failed=$((failed+1))
fi

# NOTES, provider entry for an external plugin (plugin.enabled=false): the
# server name when the endpoint is not the loopback default (the chart's
# certificate names only the Service and 127.0.0.1), the client pair with
# mTLS, and a warning that nothing replaces $(NODE_IP).
if out=$(notes -f "${COMPLETE}" --set plugin.enabled=false --set bridge.mTLS.enabled=true \
     --set bridge.mTLS.clientIssuerRef.name=ca --set 'plugin.bridgeEndpoint=https://$(NODE_IP):31443' 2>&1) \
   && grep -qF 'value: "harbor-bridge.harbor-bridge-system.svc"' <<<"$(grep -A1 'name: HARBOR_BRIDGE_SERVER_NAME' <<<"${out}")" \
   && grep -qF -- '- name: HARBOR_BRIDGE_CLIENT_CERT' <<<"${out}" \
   && grep -qF -- '- name: HARBOR_BRIDGE_CLIENT_KEY' <<<"${out}" \
   && grep -qF 'HARBOR_BRIDGE_ENDPOINT contains $(NODE_IP), which only the chart' <<<"${out}" \
   && out=$(notes -f "${COMPLETE}" --set plugin.enabled=false 2>&1) \
   && ! grep -qE 'HARBOR_BRIDGE_(SERVER_NAME|CLIENT_CERT)|contains \$\(NODE_IP\)' <<<"${out}" \
   && grep -qF 'value: "https://127.0.0.1:31443"' <<<"${out}"; then
  echo "PASS  NOTES provider entry for an external plugin"
else
  echo "FAIL  NOTES provider entry for an external plugin"
  grep -m 5 -E 'HARBOR_BRIDGE|NODE_IP|Error' <<<"${out}" || true
  failed=$((failed+1))
fi

# NOTES, mTLS: list only the certificates the chart requests, each in its
# namespace (split mode: the client certificate is in plugin.namespace).
if out=$(notes -f "${CHART_DIR}/tests/values-plugin-namespace.yaml" 2>&1) \
   && grep -qxF '  kubectl -n harbor-bridge-system get certificate harbor-bridge-tls' <<<"${out}" \
   && grep -qxF '  kubectl -n harbor-bridge-plugin get certificate harbor-bridge-plugin-mtls-client' <<<"${out}" \
   && out=$(notes -f "${COMPLETE}" --set plugin.enabled=false,tls.enabled=false,tls.existingSecret=bridge-tls \
     --set bridge.mTLS.enabled=true,bridge.mTLS.clientIssuerRef.name=ca 2>&1) \
   && grep -qF 'certificate that chains to the CA in ca.crt of Secret bridge-tls' <<<"${out}" \
   && ! grep -qF 'get certificate' <<<"${out}"; then
  echo "PASS  NOTES list the certificates the chart requests"
else
  echo "FAIL  NOTES list the certificates the chart requests"
  grep -m 5 -E 'certificate|Error' <<<"${out}" || true
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
