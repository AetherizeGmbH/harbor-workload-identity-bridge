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
{{- if not .Values.harbor.url -}}
{{- fail "harbor.url is REQUIRED. The bridge needs the Harbor base URL to manage robots." -}}
{{- end -}}
{{- if not .Values.harbor.adminCredsSecret.name -}}
{{- fail "harbor.adminCredsSecret.name is REQUIRED. Pre-create a Secret in the release namespace holding Harbor admin {username,password}." -}}
{{- end -}}
{{- if not .Values.plugin.matchImages -}}
{{- fail "plugin.matchImages is REQUIRED. Without match patterns kubelet never invokes the plugin." -}}
{{- end -}}
{{- if not .Values.plugin.audience -}}
{{- fail "plugin.audience is REQUIRED. Must match spec.trustPolicy.audience on every HarborAccess CR. Recommend embedding the cluster name (e.g. harbor-bridge-prod)." -}}
{{- end -}}
{{- if .Values.tls.enabled -}}
{{- if not .Values.tls.issuerRef.name -}}
{{- fail "tls.issuerRef.name is REQUIRED when tls.enabled=true. Provide a cert-manager (Cluster)Issuer." -}}
{{- end -}}
{{- end -}}
{{- if .Values.bridge.mTLS.enabled -}}
{{- if not .Values.bridge.mTLS.clientIssuerRef.name -}}
{{- fail "bridge.mTLS.clientIssuerRef.name is REQUIRED when bridge.mTLS.enabled=true." -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
=====================================================================
Derived values that don't fit cleanly inline.
=====================================================================
*/}}

{{- define "harbor-bridge.bridge.image" -}}
{{- $tag := .Values.bridge.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.bridge.image.repository $tag -}}
{{- end -}}

{{- define "harbor-bridge.plugin.image" -}}
{{- $tag := .Values.plugin.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.plugin.image.repository $tag -}}
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

{{- define "harbor-bridge.tlsSecretName" -}}
{{- printf "%s-tls" (include "harbor-bridge.bridge.fullname" .) -}}
{{- end -}}

{{- define "harbor-bridge.mTLSClientSecretName" -}}
{{- printf "%s-mtls-client" (include "harbor-bridge.plugin.fullname" .) -}}
{{- end -}}
