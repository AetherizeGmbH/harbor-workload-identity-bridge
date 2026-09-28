{{/*
=====================================================================
Name + chart helpers (Helm convention)
=====================================================================
*/}}

{{- define "harbor-bridge.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
fullname defaults to the release name (cleaner than the chart-name-
prepended Helm convention given how long this chart's name is).
Operators can still set `fullnameOverride` to lock in a name across
upgrades, e.g. when the release was created with a different name.
*/}}
{{- define "harbor-bridge.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "harbor-bridge.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* The bridge is the "primary" component; it carries the bare release name. */}}
{{- define "harbor-bridge.bridge.fullname" -}}
{{ include "harbor-bridge.fullname" . }}
{{- end -}}

{{- define "harbor-bridge.plugin.fullname" -}}
{{- printf "%s-plugin" (include "harbor-bridge.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Cluster-scoped objects (ClusterRole, ClusterRoleBinding) must remain
unique when the chart is installed multiple times across namespaces.
Suffix the name with the release namespace so two installs in different
namespaces don't collide on cluster-scoped names.
*/}}
{{- define "harbor-bridge.bridge.clusterScopedName" -}}
{{- printf "%s-%s" (include "harbor-bridge.bridge.fullname" .) .Release.Namespace | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
=====================================================================
Labels — common + selector. Selector labels are immutable on
Deployment/DaemonSet, so they are the minimum stable subset.
=====================================================================
*/}}

{{/*
labels.common is the non-selector subset (versioned, mutable). The
selector labels are emitted separately by the component helpers and
spliced together for metadata.labels — keeping them split avoids
emitting duplicate keys when both are included on the same object.
*/}}
{{- define "harbor-bridge.labels.common" -}}
helm.sh/chart: {{ include "harbor-bridge.chart" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: harbor-workload-identity-bridge
{{- end -}}

{{- define "harbor-bridge.bridge.selectorLabels" -}}
app.kubernetes.io/name: {{ include "harbor-bridge.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: bridge
{{- end -}}

{{- define "harbor-bridge.plugin.selectorLabels" -}}
app.kubernetes.io/name: {{ include "harbor-bridge.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: plugin
{{- end -}}

{{- define "harbor-bridge.bridge.labels" -}}
{{ include "harbor-bridge.labels.common" . }}
{{ include "harbor-bridge.bridge.selectorLabels" . }}
{{- end -}}

{{- define "harbor-bridge.plugin.labels" -}}
{{ include "harbor-bridge.labels.common" . }}
{{ include "harbor-bridge.plugin.selectorLabels" . }}
{{- end -}}

{{/*
=====================================================================
ServiceAccount names. Both components use distinct SAs so we can
RBAC-scope them independently and tell them apart in audit logs.
=====================================================================
*/}}

{{- define "harbor-bridge.bridge.serviceAccountName" -}}
{{ include "harbor-bridge.bridge.fullname" . }}
{{- end -}}

{{- define "harbor-bridge.plugin.serviceAccountName" -}}
{{ include "harbor-bridge.plugin.fullname" . }}
{{- end -}}

{{/*
=====================================================================
Required-value gates. Use `required` for fail-fast at template time;
errors surface during `helm install` with the message text intact.
=====================================================================
*/}}

{{/*
harborAccessSelector renders bridge.harborAccessSelector as a label
selector string (sorted k=v pairs); the bridge validates the syntax.
validateRequiredValues admits only string values: %s would render a
boolean or number as %!s(bool=true).
*/}}
{{- define "harbor-bridge.harborAccessSelector" -}}
{{- $pairs := list -}}
{{- range $k, $v := .Values.bridge.harborAccessSelector -}}
{{- $pairs = append $pairs (printf "%s=%s" $k $v) -}}
{{- end -}}
{{- join "," $pairs -}}
{{- end -}}

{{- define "harbor-bridge.instance" -}}
{{- default .Release.Name .Values.bridge.instance -}}
{{- end -}}

{{/*
finalizer is the finalizer the bridge sets on the HarborAccess objects it
manages: a per-instance one with a selector (ADR-0026,
bridge/controlplane/config.go Finalizer).
*/}}
{{- define "harbor-bridge.finalizer" -}}
{{- if .Values.bridge.harborAccessSelector -}}
harbor.aetherize.io/robot-{{ include "harbor-bridge.instance" . }}
{{- else -}}
harbor.aetherize.io/robot
{{- end -}}
{{- end -}}

{{- define "harbor-bridge.validateRequiredValues" -}}
{{- if not .Values.clusterName -}}
{{- fail "clusterName is REQUIRED. Set --set clusterName=<dns-label> or values.yaml. Must be unique across clusters sharing one Harbor (ADR-0009)." -}}
{{- end -}}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" .Values.clusterName) -}}
{{- fail (printf "clusterName=%q does not match DNS label regex ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ (max 63 chars)" .Values.clusterName) -}}
{{- end -}}
{{- if gt (len .Values.clusterName) 63 -}}
{{- fail (printf "clusterName=%q exceeds 63 chars" .Values.clusterName) -}}
{{- end -}}
{{- if contains "--" .Values.clusterName -}}
{{- fail (printf "clusterName=%q must not contain consecutive hyphens: it begins every Harbor robot name (bridge-<clusterName>.<namespace>.<serviceaccount>), and Harbor accepts only single separators, so the bridge could create no robot." .Values.clusterName) -}}
{{- end -}}
{{- if not .Values.harbor.url -}}
{{- fail "harbor.url is REQUIRED. The bridge needs the Harbor base URL to manage robots." -}}
{{- end -}}
{{- include "harbor-bridge.validateURL" (list "harbor.url" .Values.harbor.url "the bridge authenticates to Harbor only with harbor.adminCredsSecret") -}}
{{- include "harbor-bridge.validateURL" (list "bridge.oidcIssuer" .Values.bridge.oidcIssuer "a token's iss claim never carries one, so no token would match") -}}
{{- with .Values.bridge.oidcJWKSURL -}}
{{- include "harbor-bridge.validateURL" (list "bridge.oidcJWKSURL" . "") -}}
{{- end -}}
{{- if and (hasPrefix "http://" (lower (trim .Values.harbor.url))) (not .Values.harbor.allowInsecureHTTP) -}}
{{- fail "harbor.url uses plain http: the Harbor admin credentials and robot passwords would travel unencrypted. Use https (harbor.caSecret for a private CA), or set harbor.allowInsecureHTTP=true." -}}
{{- end -}}
{{- if not .Values.harbor.adminCredsSecret.name -}}
{{- fail "harbor.adminCredsSecret.name is REQUIRED. Pre-create a Secret in the release namespace holding Harbor admin {username,password}." -}}
{{- end -}}
{{- range $k, $v := .Values.bridge.harborAccessSelector -}}
{{- if not (kindIs "string" $v) -}}
{{- fail (printf "bridge.harborAccessSelector.%s must be a string, but it was read as the %s %v: values files and --set read unquoted label values such as true or 1 as booleans or numbers, which do not render as the value you wrote. Quote the value in the values file or pass it with --set-string." $k (kindOf $v) $v) -}}
{{- end -}}
{{- end -}}
{{- $leaderElection := .Values.bridge.leaderElection -}}
{{- if not (or (kindIs "invalid" $leaderElection) (kindIs "bool" $leaderElection)) -}}
{{- fail (printf "bridge.leaderElection must be true, false or null (null: on when bridge.replicas > 1), but it was read as the %s %v." (kindOf $leaderElection) $leaderElection) -}}
{{- end -}}
{{- if and (kindIs "bool" $leaderElection) (not $leaderElection) (gt (int .Values.bridge.replicas) 1) -}}
{{- fail (printf "bridge.leaderElection=false with bridge.replicas=%v: every replica would run the reconciler and the janitor, which only the leader may run (ADR-0025). They race on robot creation and password rotation and can leave a robot Secret with a password Harbor has already replaced. Leave bridge.leaderElection unset (on when bridge.replicas > 1) or set it to true." .Values.bridge.replicas) -}}
{{- end -}}
{{- $burst := .Values.bridge.rateLimit.burst -}}
{{- $burstOK := false -}}
{{- if or (kindIs "float64" $burst) (kindIs "int64" $burst) (kindIs "int" $burst) -}}
{{- $burstOK = and (eq (float64 (int64 $burst)) (float64 $burst)) (ge (int64 $burst) 1) -}}
{{- else if kindIs "string" $burst -}}
{{- $burstOK = regexMatch "^[1-9][0-9]*$" $burst -}}
{{- end -}}
{{- if not $burstOK -}}
{{- fail (printf "bridge.rateLimit.burst=%v must be a positive whole number of requests; the bridge refuses to start otherwise." $burst) -}}
{{- end -}}
{{- if .Values.bridge.harborAccessSelector -}}
{{- $instance := include "harbor-bridge.instance" . -}}
{{- if or (gt (len $instance) 50) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $instance)) -}}
{{- fail (printf "bridge.instance %q (default: the release name) must be a DNS label of at most 50 characters; it names this bridge's HarborAccess finalizer (ADR-0026)." $instance) -}}
{{- end -}}
{{- end -}}
{{- if include "harbor-bridge.plugin.split" . -}}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" .Values.plugin.namespace) -}}
{{- fail (printf "plugin.namespace %q must be a DNS label." .Values.plugin.namespace) -}}
{{- end -}}
{{- if .Values.plugin.enabled -}}
{{- $src := .Values.plugin.caBundle.source -}}
{{- $sources := 0 -}}
{{- if $src.secret.name -}}{{- $sources = add1 $sources -}}{{- end -}}
{{- if $src.configMap.name -}}{{- $sources = add1 $sources -}}{{- end -}}
{{- if ne $sources 1 -}}
{{- fail "plugin.namespace is set, so the plugin runs in its own namespace and needs the bridge CA through trust-manager: set exactly one of plugin.caBundle.source.secret.name or plugin.caBundle.source.configMap.name (a Secret/ConfigMap in trust-manager's trust namespace holding the CA that signs the bridge's serving certificate). ADR-0027." -}}
{{- end -}}
{{- end -}}
{{- if and .Values.bridge.mTLS.enabled (ne .Values.bridge.mTLS.clientIssuerRef.kind "ClusterIssuer") -}}
{{- fail "bridge.mTLS.clientIssuerRef.kind must be ClusterIssuer when plugin.namespace is set: the plugin's client certificate is issued in the plugin namespace. ADR-0027." -}}
{{- end -}}
{{- end -}}
{{- $maxLifetime := include "harbor-bridge.tokenMaxLifetime" . -}}
{{- if or (not (regexMatch "^([0-9]+(\\.[0-9]+)?(ns|us|ms|s|m|h))+$" $maxLifetime)) (not (regexMatch "[1-9]" $maxLifetime)) -}}
{{- fail (printf "bridge.tokenValidation.maxLifetime=%q must be a positive Go duration such as 1h (ADR-0028)." $maxLifetime) -}}
{{- end -}}
{{- $requirePodBinding := dig "requirePodBinding" true (.Values.bridge.tokenValidation | default dict) -}}
{{- if not (kindIs "bool" $requirePodBinding) -}}
{{- fail (printf "bridge.tokenValidation.requirePodBinding=%q must be true or false (ADR-0028)." (toString $requirePodBinding)) -}}
{{- end -}}
{{- if not .Values.plugin.audience -}}
{{- fail "plugin.audience is REQUIRED. Must match spec.trustPolicy.audience on every HarborAccess CR. Recommend embedding the cluster name (e.g. harbor-bridge-prod)." -}}
{{- end -}}
{{- $rawProviderName := .Values.plugin.providerName -}}
{{- if not (or (kindIs "invalid" $rawProviderName) (kindIs "string" $rawProviderName)) -}}
{{- fail (printf "plugin.providerName must be a string, but it was read as the %s %v: values files and --set read unquoted values such as 123, 1e3, yes or on as numbers or booleans. Quote the name in the values file (providerName: \"123\") or pass it with --set-string (ADR-0029)." (kindOf $rawProviderName) $rawProviderName) -}}
{{- end -}}
{{- $providerName := include "harbor-bridge.plugin.providerName" . -}}
{{- if or (gt (len $providerName) 63) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $providerName)) -}}
{{- fail (printf "plugin.providerName=%q must be a DNS label (lower-case letters, digits and -, at most 63 characters): it names the kubelet credential-provider entry and the plugin binary on every node. Keep the default harbor-bridge-plugin unless you run several installs per cluster; then give each its own name, e.g. harbor-bridge-eu (ADR-0029)." $providerName) -}}
{{- end -}}
{{- if .Values.tls.enabled -}}
{{- if not .Values.tls.issuerRef.name -}}
{{- fail "tls.issuerRef.name is REQUIRED when tls.enabled=true. Provide a cert-manager (Cluster)Issuer." -}}
{{- end -}}
{{- else if not .Values.tls.existingSecret -}}
{{- fail "tls.existingSecret is REQUIRED when tls.enabled=false. The bridge always serves TLS and the plugin always verifies it: name a Secret in the release namespace with tls.crt, tls.key and ca.crt." -}}
{{- end -}}
{{- if .Values.bridge.mTLS.enabled -}}
{{- if not .Values.bridge.mTLS.clientIssuerRef.name -}}
{{- fail "bridge.mTLS.clientIssuerRef.name is REQUIRED when bridge.mTLS.enabled=true." -}}
{{- end -}}
{{- end -}}
{{- if hasKey .Values.plugin "patchKubelet" -}}
{{- fail "plugin.patchKubelet was removed (ADR-0021). Use plugin.install.mode instead: patchKubelet=true → mode: auto (or patch), patchKubelet=false → mode: none. See MIGRATION.md." -}}
{{- end -}}
{{- if .Values.plugin.enabled -}}
{{- if not .Values.plugin.matchImages -}}
{{- fail "plugin.matchImages is REQUIRED when plugin.enabled=true. Without match patterns kubelet never invokes the plugin." -}}
{{- end -}}
{{- $install := .Values.plugin.install | default dict -}}
{{- if not (has $install.mode (list "auto" "merge" "patch" "none")) -}}
{{- fail (printf "plugin.install.mode=%q is invalid. Must be one of: auto, merge, patch, none (ADR-0021)." (toString $install.mode)) -}}
{{- end -}}
{{- if ne (empty $install.binDir) (empty $install.configFile) -}}
{{- fail "plugin.install.binDir and plugin.install.configFile must be set together (both name merge-mode targets)." -}}
{{- end -}}
{{- /* Auto mode discovers kubelet's paths to choose a mode and would
       ignore the overrides; patch and none mode have no use for them.
       The installer checks the same (loadConfig). */}}
{{- if and $install.binDir (ne (toString $install.mode) "merge") -}}
{{- fail (printf "plugin.install.binDir and plugin.install.configFile name merge-mode targets and are used only with plugin.install.mode=merge, not %q: in auto mode the installer discovers kubelet's paths and would ignore them. Set plugin.install.mode=merge, or clear them." (toString $install.mode)) -}}
{{- end -}}
{{- if not (regexMatch "^[A-Za-z0-9][A-Za-z0-9:_.@-]*$" (toString $install.kubeletUnit)) -}}
{{- fail (printf "plugin.install.kubeletUnit=%q is not a valid systemd unit name." (toString $install.kubeletUnit)) -}}
{{- end -}}
{{- range $k, $v := dict "plugin.hostBinaryDir" .Values.plugin.hostBinaryDir "plugin.hostConfigDir" .Values.plugin.hostConfigDir "plugin.install.stateDir" $install.stateDir "plugin.install.binDir" $install.binDir "plugin.install.configFile" $install.configFile -}}
{{- if and $v (or (not (regexMatch "^/[A-Za-z0-9._/-]+$" (toString $v))) (contains "/../" (printf "%s/" $v)) (contains "/./" (printf "%s/" $v)) (contains "//" (toString $v)) (hasSuffix "/" (toString $v))) -}}
{{- fail (printf "%s=%q must be an absolute, clean node path (letters, digits and . _ - / only)." $k (toString $v)) -}}
{{- end -}}
{{- end -}}
{{- /* plugin.hostConfigDir is writable by the sync container of every
       release. The installer trusts the plugin binaries and entry records
       in the bin dir, and its state file decides about kubelet restarts, so
       neither may be in reach of it (ADR-0029). plugin.install.configFile
       names a cloud's config, whose entries merge mode keeps as they are,
       so it may not be inside it either. Compared per path segment:
       /a/b-c is not inside /a/b. The installer checks the same (loadConfig). */}}
{{- $configDir := .Values.plugin.hostConfigDir -}}
{{- range $k, $v := dict "plugin.hostBinaryDir" .Values.plugin.hostBinaryDir "plugin.install.binDir" $install.binDir -}}
{{- if and $v $configDir (or (eq $v $configDir) (hasPrefix (printf "%s/" $v) $configDir) (hasPrefix (printf "%s/" $configDir) $v)) -}}
{{- fail (printf "%s=%q and plugin.hostConfigDir=%q must not be the same directory or inside one another: the sync container of every release can write plugin.hostConfigDir, and the installer trusts what is in the plugin bin dir (ADR-0029)." $k (toString $v) $configDir) -}}
{{- end -}}
{{- end -}}
{{- if and $install.stateDir $configDir (or (eq $install.stateDir $configDir) (hasPrefix (printf "%s/" $configDir) (toString $install.stateDir))) -}}
{{- fail (printf "plugin.install.stateDir=%q must not be plugin.hostConfigDir=%q or inside it: the sync container of every release can write plugin.hostConfigDir, and the state file decides about kubelet restarts (ADR-0029)." (toString $install.stateDir) $configDir) -}}
{{- end -}}
{{- if and $install.configFile $configDir (hasPrefix (printf "%s/" $configDir) (toString $install.configFile)) -}}
{{- fail (printf "plugin.install.configFile=%q must not be inside plugin.hostConfigDir=%q: the sync container of every release can write plugin.hostConfigDir, and merge mode keeps every other entry of that config for kubelet (ADR-0029). The chart-owned config there is for plugin.install.mode=patch or none." (toString $install.configFile) $configDir) -}}
{{- end -}}
{{- if not .Values.plugin.allowSelfMatchImages -}}
{{- $pluginHost := include "harbor-bridge.registryHost" .Values.plugin.image.repository -}}
{{- $bridgeHost := include "harbor-bridge.registryHost" .Values.bridge.image.repository -}}
{{- range .Values.plugin.matchImages -}}
{{- if or (eq . $pluginHost) (eq . $bridgeHost) -}}
{{- fail (printf "plugin.matchImages entry %q matches the registry of the plugin/bridge images — the plugin cannot authenticate the pull of its own image (chicken-and-egg). Pull these images from a registry outside matchImages, or set plugin.allowSelfMatchImages=true if you accept the bootstrap ordering. ADR-0021." .) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
validateURL checks a URL setting of the bridge (bridge/controlplane/config.go
requireURL) at template time: an http or https scheme and a host, no
literal "@" after the host part (a "/", "?" or "#" inside a password ends
the host early), and no "@" at all when the setting takes no
user:password@ part. A non-empty third element is the reason it takes
none: harbor.url and bridge.oidcIssuer take none, bridge.oidcJWKSURL may
carry one (net/http sends it as Basic auth to the JWKS endpoint). Messages
never repeat the value: it may hold a credential.
Usage: include "harbor-bridge.validateURL" (list "<setting>" <value> "<reason or empty>")
*/}}
{{- define "harbor-bridge.validateURL" -}}
{{- $name := index . 0 -}}
{{- $url := index . 1 | toString | trim -}}
{{- $noUserinfo := index . 2 -}}
{{- $omitted := "The value is left out of this message: it could hold a credential." -}}
{{- if and $noUserinfo (contains "@" $url) -}}
{{- fail (printf "%s must not contain \"@\". A user:password@ part never takes effect (%s) and only ends up in logs; an \"@\" after the host part usually means a \"/\", \"?\" or \"#\" inside a password ended the host early. Write an \"@\" that belongs to the path, query or fragment as %%40. %s" $name $noUserinfo $omitted) -}}
{{- end -}}
{{- if not (regexMatch "^(?i:https?)://([^/?#]*@)?[^/?#@]+([/?#].*)?$" $url) -}}
{{- fail (printf "%s must be an http:// or https:// URL with a host; the bridge refuses to start otherwise. %s" $name $omitted) -}}
{{- end -}}
{{- if regexMatch "^[^:]+://[^/?#]*[/?#].*@" $url -}}
{{- fail (printf "%s has an \"@\" after its host part: a \"/\", \"?\" or \"#\" inside a user:password@ part ends the host early, and the credentials never reach the endpoint. Percent-encode them (%%2F, %%3F, %%23), and write an \"@\" that belongs to the path, query or fragment as %%40. %s" $name $omitted) -}}
{{- end -}}
{{- end -}}

{{/*
registryHost extracts the registry host from an image repository
string: the first path segment when it looks like a host (contains a
dot or colon, or is "localhost"), else docker.io — mirroring the
container-runtime convention. Takes the repository string as its
context (not the root context).
*/}}
{{- define "harbor-bridge.registryHost" -}}
{{- $first := splitList "/" . | first -}}
{{- if or (contains "." $first) (contains ":" $first) (eq $first "localhost") -}}
{{- $first -}}
{{- else -}}
docker.io
{{- end -}}
{{- end -}}

{{/*
=====================================================================
Derived values that don't fit cleanly inline.
=====================================================================
*/}}

{{- define "harbor-bridge.bridge.image" -}}
{{- with .Values.bridge.image.digest -}}
{{- if not (regexMatch "^sha256:[a-f0-9]{64}$" .) -}}
{{- fail (printf "bridge.image.digest %q must be sha256:<64 hex characters>" .) -}}
{{- end -}}
{{- printf "%s@%s" $.Values.bridge.image.repository . -}}
{{- else -}}
{{- $tag := .Values.bridge.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.bridge.image.repository $tag -}}
{{- end -}}
{{- end -}}

{{- define "harbor-bridge.plugin.image" -}}
{{- with .Values.plugin.image.digest -}}
{{- if not (regexMatch "^sha256:[a-f0-9]{64}$" .) -}}
{{- fail (printf "plugin.image.digest %q must be sha256:<64 hex characters>" .) -}}
{{- end -}}
{{- printf "%s@%s" $.Values.plugin.image.repository . -}}
{{- else -}}
{{- $tag := .Values.plugin.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.plugin.image.repository $tag -}}
{{- end -}}
{{- end -}}

{{/*
rateLimitBurst renders bridge.rateLimit.burst as the integer the bridge
parses (strconv.Atoi): a values file reads numbers as float64, and a
float64 of 1000000 or more would otherwise render as 1e+06.
validateRequiredValues admits only positive whole numbers.
*/}}
{{- define "harbor-bridge.rateLimitBurst" -}}
{{- if kindIs "string" .Values.bridge.rateLimit.burst -}}
{{- .Values.bridge.rateLimit.burst -}}
{{- else -}}
{{- int64 .Values.bridge.rateLimit.burst -}}
{{- end -}}
{{- end -}}

{{/* Leader election is auto-enabled when replicas > 1 unless forced. */}}
{{- define "harbor-bridge.bridge.leaderElection" -}}
{{- if ne (kindOf .Values.bridge.leaderElection) "invalid" -}}
{{- .Values.bridge.leaderElection -}}
{{- else if gt (int .Values.bridge.replicas) 1 -}}
true
{{- else -}}
false
{{- end -}}
{{- end -}}

{{/*
The Secret holding the bridge's serving pair and the CA the plugin
trusts: chart-managed (cert-manager) or operator-provided.
*/}}
{{- define "harbor-bridge.tlsSecretName" -}}
{{- if .Values.tls.enabled -}}
{{- printf "%s-tls" (include "harbor-bridge.bridge.fullname" .) -}}
{{- else -}}
{{- .Values.tls.existingSecret -}}
{{- end -}}
{{- end -}}

{{/*
Plugin namespace (ADR-0027): plugin.namespace, or the release namespace.
split is "true" when the plugin runs in a namespace of its own.
*/}}
{{- define "harbor-bridge.plugin.namespace" -}}
{{- default .Release.Namespace .Values.plugin.namespace -}}
{{- end -}}

{{- define "harbor-bridge.plugin.split" -}}
{{- if and .Values.plugin.namespace (ne .Values.plugin.namespace .Release.Namespace) -}}true{{- end -}}
{{- end -}}

{{- define "harbor-bridge.plugin.caBundleName" -}}
{{- printf "%s-ca" (include "harbor-bridge.plugin.fullname" .) -}}
{{- end -}}

{{- define "harbor-bridge.mTLSClientSecretName" -}}
{{- printf "%s-mtls-client" (include "harbor-bridge.plugin.fullname" .) -}}
{{- end -}}

{{/*
providerName is plugin.providerName (ADR-0029), the one place templates
read it from. A missing key renders the default: `helm upgrade
--reuse-values` renders this chart with the previous chart's values,
which predate the key, and a null value unsets it. validateRequiredValues
rejects every value that is not a string, because a number or boolean
would not render back as the name the operator wrote.
*/}}
{{- define "harbor-bridge.plugin.providerName" -}}
{{- if kindIs "invalid" .Values.plugin.providerName -}}
harbor-bridge-plugin
{{- else -}}
{{- .Values.plugin.providerName -}}
{{- end -}}
{{- end -}}

{{/*
legacyNames is "true" for the default provider name, whose node files
keep the names they had before provider names became configurable.
*/}}
{{- define "harbor-bridge.plugin.legacyNames" -}}
{{- if eq (include "harbor-bridge.plugin.providerName" .) "harbor-bridge-plugin" -}}true{{- end -}}
{{- end -}}

{{/*
configKey and configMountPath name where the install container reads the
rendered provider config: the ConfigMap key, and the directory the
ConfigMap is mounted at. The default provider name keeps the layout every
installer reads (/config/credential-provider-config.yaml), so existing
installs render the same manifests. Any other name uses a key and a mount
path that installers before ADR-0029 never read: such an installer (an
older plugin image) ignores PROVIDER_NAME and would install the entry of
the new name without a binary of that name, which keeps kubelet from
starting. With this layout it fails at reading the rendered config, before
it writes anything on the node, and its sync container never starts. The
installer derives the same path (installer/names.go sourceConfigPath).
*/}}
{{- define "harbor-bridge.plugin.configKey" -}}
{{- if include "harbor-bridge.plugin.legacyNames" . -}}
credential-provider-config.yaml
{{- else -}}
credential-provider-config.v2.yaml
{{- end -}}
{{- end -}}

{{- define "harbor-bridge.plugin.configMountPath" -}}
{{- if include "harbor-bridge.plugin.legacyNames" . -}}
/config
{{- else -}}
/config-v2
{{- end -}}
{{- end -}}

{{/*
providerNameYAML is the provider name as a YAML scalar. The default stays
bare, as it always rendered (one changed byte in the rendered config would
restart kubelet on upgrade); any other name is quoted, because YAML 1.1
reads DNS labels such as "yes", "on" or "123" as booleans or numbers when
the installer parses the rendered config.
*/}}
{{- define "harbor-bridge.plugin.providerNameYAML" -}}
{{- if include "harbor-bridge.plugin.legacyNames" . -}}
harbor-bridge-plugin
{{- else -}}
{{- include "harbor-bridge.plugin.providerName" . | quote -}}
{{- end -}}
{{- end -}}

{{/*
hostFile is the node path of one of this install's files in
plugin.hostConfigDir: harbor-bridge-<suffix> for the default provider
name, <providerName>.<suffix> otherwise. A provider name has no dot, so
two installs never share a file. The installer derives the same names
(installer/names.go). Usage: include "harbor-bridge.plugin.hostFile" (list . "ca.crt")
*/}}
{{- define "harbor-bridge.plugin.hostFile" -}}
{{- $ctx := index . 0 -}}
{{- $suffix := index . 1 -}}
{{- if include "harbor-bridge.plugin.legacyNames" $ctx -}}
{{- printf "%s/harbor-bridge-%s" $ctx.Values.plugin.hostConfigDir $suffix -}}
{{- else -}}
{{- printf "%s/%s.%s" $ctx.Values.plugin.hostConfigDir (include "harbor-bridge.plugin.providerName" $ctx) $suffix -}}
{{- end -}}
{{- end -}}

{{/*
bridgeEndpoint is the URL the on-node plugin calls. Default is the
ADR-0008 loopback NodePort; plugin.bridgeEndpoint overrides it (the
installer substitutes a literal $(NODE_IP) with the node's IP).
*/}}
{{- define "harbor-bridge.plugin.bridgeEndpoint" -}}
{{- if .Values.plugin.bridgeEndpoint -}}
{{- .Values.plugin.bridgeEndpoint -}}
{{- else -}}
https://127.0.0.1:{{ .Values.service.nodePort }}
{{- end -}}
{{- end -}}

{{/*
Token validation settings (ADR-0028), defaulted here because `helm upgrade
--reuse-values` from a chart without bridge.tokenValidation keeps the old
values and never sees the new defaults in values.yaml.
*/}}
{{- define "harbor-bridge.tokenMaxLifetime" -}}
{{- dig "maxLifetime" "1h" (.Values.bridge.tokenValidation | default dict) | toString -}}
{{- end -}}

{{- define "harbor-bridge.requirePodBinding" -}}
{{- dig "requirePodBinding" true (.Values.bridge.tokenValidation | default dict) | toString -}}
{{- end -}}
