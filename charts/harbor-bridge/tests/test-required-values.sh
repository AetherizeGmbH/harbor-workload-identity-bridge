#!/usr/bin/env bash
# Verify every required value gate fails template rendering with a
# clear message. If any of these *succeeds*, a required-value check
# was silently dropped — caught by the test.
set -euo pipefail

CHART_DIR="${CHART_DIR:-charts/harbor-bridge}"
COMPLETE="${CHART_DIR}/tests/values-complete.yaml"
NEXUS="${CHART_DIR}/tests/values-nexus.yaml"
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
  "plugin.install.configFile inside plugin.hostConfigDir|--set|plugin.install.binDir=/etc/cp-bin,plugin.install.configFile=/etc/kubernetes/credential-provider-config/credential-provider-config.yaml|plugin.install.configFile=\"/etc/kubernetes/credential-provider-config/credential-provider-config.yaml\" must not be inside plugin.hostConfigDir"
  "harbor.registryHosts with a scheme (without Nexus too)|--set|harbor.registryHosts={https://harbor.example.com}|harbor.registryHosts entry \"https://harbor.example.com\" must not carry a scheme"
  "harbor.registryHosts as a string|--set-string|harbor.registryHosts=harbor.example.com|harbor.registryHosts must be a list"
  "nexus.enabled not a boolean|--set-string|nexus.enabled=true|nexus.enabled=\"true\" must be true or false"
)

# The Sonatype Nexus Repository backend (ADR-0036), on top of
# values-nexus.yaml, whose plugin.matchImages covers every registry host.
nexus_cases=(
  "nexus.url|--set|nexus.url=|nexus.url is REQUIRED"
  "nexus.url over plain http|--set|nexus.url=http://nexus.example.com|nexus.url uses plain http"
  "nexus.url with credentials|--set|nexus.url=https://admin:s3cr3t-pw@nexus.example.com|nexus.url must be an http(s) base URL"
  "nexus.url with a fragment|--set|nexus.url=https://nexus.example.com/#x|nexus.url must be an http(s) base URL"
  "nexus.url without a scheme|--set|nexus.url=nexus.example.com|nexus.url must be an http(s) base URL"
  "nexus.allowInsecureHTTP not a boolean|--set-string|nexus.allowInsecureHTTP=yes|nexus.allowInsecureHTTP=\"yes\" must be true or false"
  "nexus.adminCredsSecret.name|--set|nexus.adminCredsSecret.name=|nexus.adminCredsSecret.name is REQUIRED"
  "nexus.adminCredsSecret missing|--set|nexus.adminCredsSecret=null|nexus.adminCredsSecret.name is REQUIRED"
  "nexus.adminCredsSecret missing without the plugin|--set|nexus.adminCredsSecret=null,plugin.enabled=false|nexus.adminCredsSecret.name is REQUIRED"
  "nexus.adminCredsSecret.keys.password empty|--set|nexus.adminCredsSecret.keys.password=|nexus.adminCredsSecret.keys.password must name the key"
  "nexus.caSecret.key empty|--set|nexus.caSecret.key=|nexus.caSecret.key must name the key"
  "nexus.rateLimitBackoff without a unit|--set|nexus.rateLimitBackoff=900|nexus.rateLimitBackoff=\"900\" must be a positive Go duration"
  "nexus.rateLimitBackoff zero|--set|nexus.rateLimitBackoff=0s|must be a positive Go duration"
  "nexus.registryHosts missing|--set|nexus.registryHosts=null|nexus.registryHosts is REQUIRED"
  "nexus.registryHosts as a string|--set-string|nexus.registryHosts=nexus.example.com:8082|nexus.registryHosts must be a list"
  "nexus.registryHosts with a scheme|--set|nexus.registryHosts={https://nexus.example.com}|nexus.registryHosts entry \"https://nexus.example.com\" must not carry a scheme"
  "nexus.registryHosts with a glob|--set|nexus.registryHosts={*.example.com}|no credentials, query, fragment, wildcard or whitespace"
  "nexus.registryHosts with a trailing slash|--set|nexus.registryHosts={nexus.example.com:8082/team/}|path segment \"\", which is not a repository path component"
  "nexus.registryHosts with an upper-case path|--set|nexus.registryHosts={nexus.example.com:8082/Team}|path segment \"Team\", which is not a repository path component"
  "nexus.registryHosts port with a leading zero|--set|nexus.registryHosts={nexus.example.com:08082}|port \"08082\", which must be a number from 1 to 65535 without leading zeros"
  "nexus.registryHosts port out of range|--set|nexus.registryHosts={nexus.example.com:65536}|port \"65536\", which must be a number from 1 to 65535"
  "nexus.registryHosts empty port|--set|nexus.registryHosts={nexus.example.com:}|port \"\", which must be a number"
  "nexus.registryHosts IPv6 without brackets|--set|nexus.registryHosts={fd00::1}|must write an IPv6 address in brackets"
  "nexus.registryHosts host name with an underscore|--set|nexus.registryHosts={nexus_1.example.com}|\"nexus_1.example.com\", which is not a host name or IP address"
  "nexus.registryHosts label over 63 characters|--set|nexus.registryHosts={$(printf 'a%.0s' {1..64}).example.com}|which is not a host name or IP address"
  "Nexus host not in plugin.matchImages|--set|plugin.matchImages={harbor.example.com}|nexus.registryHosts entry \"nexus.example.com:8082\" is not covered by plugin.matchImages"
  "plugin.matchImages without the port|--set|plugin.matchImages={harbor.example.com,nexus.example.com,*.example.com/nexus}|entry \"nexus.example.com:8082\" is not covered"
  "plugin.matchImages with another port|--set|plugin.matchImages={harbor.example.com,nexus.example.com:8083,*.example.com/nexus}|entry \"nexus.example.com:8082\" is not covered"
  "plugin.matchImages with a literal /* path|--set|plugin.matchImages={harbor.example.com,nexus.example.com:8082/*,*.example.com/nexus}|entry \"nexus.example.com:8082\" is not covered"
  "plugin.matchImages path with a trailing slash|--set|plugin.matchImages={harbor.example.com,nexus.example.com:8082,*.example.com/nexus/}|entry \"registry.example.com/nexus\" is not covered"
  "plugin.matchImages path longer than the prefix|--set|plugin.matchImages={harbor.example.com,nexus.example.com:8082,registry.example.com/nexus/app}|entry \"registry.example.com/nexus\" is not covered"
  "plugin.matchImages glob covering fewer labels|--set|plugin.matchImages={harbor.example.com,nexus.example.com:8082,*.com/nexus}|entry \"registry.example.com/nexus\" is not covered"
  "plugin.matchImages glob with a question mark (a URL query to kubelet)|--set|plugin.matchImages={harbor.example.com,nexus.example.com:8082,registr?.example.com/nexus}|entry \"registry.example.com/nexus\" is not covered"
  "plugin.matchImages in upper case|--set|plugin.matchImages={harbor.example.com,NEXUS.example.com:8082,*.example.com/nexus}|entry \"nexus.example.com:8082\" is not covered"
  "Harbor registry host not in plugin.matchImages|--set|harbor.registryHosts={harbor-2.example.com}|Harbor's registry host \"harbor-2.example.com\" (harbor.registryHosts) is not covered by plugin.matchImages"
  "Harbor registry host from harbor.url not in plugin.matchImages|--set|harbor.registryHosts=null,harbor.url=https://harbor-core.harbor.svc|Harbor's registry host \"harbor-core.harbor.svc\" (the host of harbor.url) is not covered by plugin.matchImages"
  "harbor.url host no registry host|--set|harbor.registryHosts=null,harbor.url=https://harbor.example.com:0443|the host of harbor.url is no registry host: it has the port \"0443\""
  "a host:port both backends name|--set|harbor.registryHosts={nexus.example.com:8082}|registry host \"nexus.example.com:8082\" is both Harbor's (harbor.registryHosts) and Nexus's (nexus.registryHosts)"
  "a host both backends name, in another case|--set|harbor.registryHosts={NEXUS.Example.com:8082}|registry host \"nexus.example.com:8082\" is both Harbor's"
  "a host both backends name, from harbor.url|--set|harbor.registryHosts=null,harbor.url=https://registry.example.com|registry host \"registry.example.com\" is both Harbor's (the host of harbor.url) and Nexus's"
)

failed=0
run_cases() {
  local base="$1"; shift
  local case label flag setval want out
  for case in "$@"; do
    IFS='|' read -r label flag setval want <<< "${case}"
    out=$(render -f "${base}" "${flag}" "${setval}" 2>&1 || true)
    if echo "${out}" | grep -qF "${want}"; then
      echo "PASS  ${label}"
    else
      echo "FAIL  ${label}"
      echo "      expected error containing: ${want}"
      echo "      got: ${out}" | head -3
      failed=$((failed+1))
    fi
  done
}
run_cases "${COMPLETE}" "${cases[@]}"
run_cases "${NEXUS}" "${nexus_cases[@]}"

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

# ADR-0029: a non-default provider name publishes the rendered config under
# a ConfigMap key and at a mount path that installers before ADR-0029 never
# read (they read /config/credential-provider-config.yaml), so an older
# plugin image fails at that read, before it writes anything on the node.
# The default name keeps the layout every installer reads.
out=$(render -f "${COMPLETE}" --set plugin.providerName=harbor-bridge-eu 2>&1 || true)
if echo "${out}" | grep -qxF '  credential-provider-config.v2.yaml: |' \
   && ! echo "${out}" | grep -qxF '  credential-provider-config.yaml: |' \
   && [ "$(echo "${out}" | grep -cxE ' +mountPath: /config-v2')" -eq 2 ] \
   && ! echo "${out}" | grep -qxE ' +mountPath: /config'; then
  echo "PASS  non-default plugin.providerName: rendered config where older installers never read"
else
  echo "FAIL  non-default plugin.providerName: rendered config where older installers never read"
  echo "${out}" | grep -E 'credential-provider-config|mountPath: /config|Error' | head -5
  failed=$((failed+1))
fi
out=$(render -f "${COMPLETE}" 2>&1 || true)
if echo "${out}" | grep -qxF '  credential-provider-config.yaml: |' \
   && [ "$(echo "${out}" | grep -cxE ' +mountPath: /config')" -eq 2 ] \
   && ! echo "${out}" | grep -qE 'config-v2|config\.v2'; then
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
  echo "      got: ${out}" | head -3
  failed=$((failed+1))
fi

# The overlap check compares per path segment: the defaults
# /etc/kubernetes/credential-provider and
# /etc/kubernetes/credential-provider-config share a string prefix but no
# directory, and the state dir may be a parent of plugin.hostConfigDir. A
# merge config whose name merely extends plugin.hostConfigDir is not in it.
if render -f "${COMPLETE}" --set plugin.install.stateDir=/etc/kubernetes > /dev/null 2>&1 \
   && render -f "${COMPLETE}" --set plugin.install.binDir=/etc/cp-bin,plugin.install.configFile=/etc/kubernetes/credential-provider-config.yaml > /dev/null 2>&1; then
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

# --- Sonatype Nexus Repository (ADR-0036): what must render ---------------

# check LABEL COMMAND...: PASS when COMMAND succeeds.
check() {
  local label="$1"; shift
  if "$@"; then
    echo "PASS  ${label}"
  else
    echo "FAIL  ${label}"
    failed=$((failed+1))
  fi
}

# env_is RENDER NAME VALUE: the bridge env var NAME has the quoted VALUE.
env_is() {
  echo "$1" | grep -A1 -xE " +- name: $2" | grep -qxF "              value: \"$3\""
}

# renders ARGS...: the chart renders with ARGS; the output is in ${out}.
renders() {
  out=$(render "$@" 2>&1) || { echo "      got: ${out}" | head -3; return 1; }
}

# A credential in nexus.url is never repeated in the error.
no_url_credential() {
  out=$(render -f "${NEXUS}" --set nexus.url=https://admin:s3cr3t-pw@nexus.example.com 2>&1 || true)
  echo "${out}" | grep -qF 'nexus.url must be an http(s) base URL' && ! echo "${out}" | grep -qF 's3cr3t-pw'
}
check "nexus.url's credential is not repeated in the error" no_url_credential

# nexus.enabled=false renders nothing of the backend, whatever else is set;
# harbor.registryHosts still reaches the bridge, which parses it anyway.
nexus_disabled() {
  renders -f "${NEXUS}" --set nexus.enabled=false \
    && ! echo "${out}" | grep -qE 'BRIDGE_NEXUS_|nexus\.aetherize\.io|nexusaccesses|nexus-admin|nexus-ca' \
    && env_is "${out}" BRIDGE_HARBOR_REGISTRY_HOSTS harbor.example.com
}
check "nexus.enabled=false renders nothing of the Nexus backend" nexus_disabled

# `helm upgrade --reuse-values` from a chart before the Nexus backend
# renders with values that lack nexus and harbor.registryHosts; a null
# --set removes them the same way. The render must equal the default one.
no_nexus_block() {
  [ "$(render -f "${COMPLETE}" 2>&1)" = "$(render -f "${COMPLETE}" --set nexus=null --set harbor.registryHosts=null 2>&1)" ]
}
check "a missing nexus block renders the defaults (--reuse-values)" no_nexus_block

# `--reuse-values --set nexus.enabled=true,...` lacks the other defaults of
# values.yaml: they render anyway.
partial_nexus_block() {
  renders -f "${NEXUS}" --set nexus.rateLimitBackoff=null,nexus.adminCredsSecret.keys=null,nexus.caSecret=null,nexus.allowInsecureHTTP=null \
    && env_is "${out}" BRIDGE_NEXUS_RATE_LIMIT_BACKOFF 15m \
    && env_is "${out}" BRIDGE_NEXUS_ALLOW_INSECURE_HTTP false \
    && ! echo "${out}" | grep -qE 'BRIDGE_NEXUS_CA_FILE|nexus-ca' \
    && echo "${out}" | grep -A1 -xF '              - key: "username"' | grep -qxF '                path: username' \
    && echo "${out}" | grep -A1 -xF '              - key: "password"' | grep -qxF '                path: password'
}
check "a partial nexus block renders the defaults of the rest" partial_nexus_block

# Harbor-only installs keep today's behaviour: harbor.registryHosts renders
# as given and is not checked against plugin.matchImages.
harbor_hosts_without_nexus() {
  renders -f "${COMPLETE}" --set 'harbor.registryHosts={harbor-2.example.com,harbor.example.com:8443/team}' \
    && env_is "${out}" BRIDGE_HARBOR_REGISTRY_HOSTS 'harbor-2.example.com,harbor.example.com:8443/team'
}
check "harbor.registryHosts without Nexus renders, unchecked against matchImages" harbor_hosts_without_nexus

# Coverage under kubelet's rules: a host glob with the port, a bare host
# over a path prefix, and raw path prefixes shorter than the entry's.
check "plugin.matchImages: a host glob with the port and a bare host cover" \
  renders -f "${NEXUS}" --set 'plugin.matchImages={harbor.example.com,*.example.com:8082,registry.example.com}'
check "plugin.matchImages: raw path prefixes shorter than the entry's cover" \
  renders -f "${NEXUS}" --set 'plugin.matchImages={harbor.example.com,nexus.example.com:8082/,registry.example.com/nex}'

# Without the chart's plugin there is no matchImages to check.
plugin_disabled() {
  renders -f "${NEXUS}" --set plugin.enabled=false,plugin.matchImages=null \
    && env_is "${out}" BRIDGE_NEXUS_URL https://nexus.example.com
}
check "plugin.enabled=false skips the matchImages coverage check" plugin_disabled

allow_http() {
  renders -f "${NEXUS}" --set nexus.url=http://nexus.nexus.svc:8081,nexus.allowInsecureHTTP=true \
    && env_is "${out}" BRIDGE_NEXUS_URL http://nexus.nexus.svc:8081 \
    && env_is "${out}" BRIDGE_NEXUS_ALLOW_INSECURE_HTTP true
}
check "nexus.allowInsecureHTTP=true admits an http nexus.url" allow_http

# IPv6: kubelet matches a bracketed address with a port label by label; a
# bracketed address without a port stays in brackets, which filepath.Match
# reads as a character class, so no matchImages entry covers it.
cat <<'YAML' > "${TMP}/values-nexus-ipv6.yaml"
nexus:
  registryHosts: ["[FD00::1]:5000"]
plugin:
  matchImages: ["harbor.example.com", "[fd00::1]:5000"]
YAML
ipv6_with_port() {
  renders -f "${NEXUS}" -f "${TMP}/values-nexus-ipv6.yaml" \
    && env_is "${out}" BRIDGE_NEXUS_REGISTRY_HOSTS '[FD00::1]:5000'
}
check "an IPv6 registry host with a port is covered by the same literal" ipv6_with_port
cat <<'YAML' > "${TMP}/values-nexus-ipv6-noport.yaml"
nexus:
  registryHosts: ["[fd00::1]"]
plugin:
  matchImages: ["harbor.example.com", "[fd00::1]"]
YAML
ipv6_without_port() {
  out=$(render -f "${NEXUS}" -f "${TMP}/values-nexus-ipv6-noport.yaml" 2>&1 || true)
  echo "${out}" | grep -qF 'nexus.registryHosts entry "[fd00::1]" is not covered by plugin.matchImages'
}
check "an IPv6 registry host without a port is never covered" ipv6_without_port

# A character class or an escape in a host label is not evaluated: such an
# entry never counts as covering, even where kubelet would match.
cat <<'YAML' > "${TMP}/values-nexus-class.yaml"
plugin:
  matchImages: ["harbor.example.com", "nexus.example.com:8082", "[r]egistry.example.com"]
YAML
character_class() {
  out=$(render -f "${NEXUS}" -f "${TMP}/values-nexus-class.yaml" 2>&1 || true)
  echo "${out}" | grep -qF 'nexus.registryHosts entry "registry.example.com/nexus" is not covered'
}
check "a matchImages host label with a character class never covers" character_class

if [ "${failed}" -gt 0 ]; then
  echo
  echo "${failed} required-value test(s) failed"
  exit 1
fi
echo
echo "all required-value gates fire as expected"
