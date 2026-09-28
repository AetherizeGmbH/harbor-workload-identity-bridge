{{/*
=====================================================================
Sonatype Nexus Repository backend (ADR-0036) and the registry hosts the
bridge routes by once two backends are configured.

Every read of .Values.nexus goes through `.Values.nexus | default dict`
and `dig`: `helm upgrade --reuse-values` from a chart without the nexus
block renders with values that lack it, and `--reuse-values --set
nexus.enabled=true` lacks the other defaults of values.yaml.
=====================================================================
*/}}

{{/* nexus.enabled renders "true" when the Nexus backend is enabled. */}}
{{- define "harbor-bridge.nexus.enabled" -}}
{{- if eq (toString (dig "enabled" false (.Values.nexus | default dict))) "true" -}}true{{- end -}}
{{- end -}}

{{/*
registryEntry.parse parses one host[:port][/path-prefix] entry of
harbor.registryHosts or nexus.registryHosts the way the bridge does
(bridge/internal/registryhost Parse), so that a mistake fails at template
time instead of at bridge start. It renders JSON with hostPort (the host
lower-case, an IPv6 address in brackets, ":<port>" when one is given),
prefix (the path prefix without leading or trailing '/', empty for none)
and error (empty when the entry is valid; otherwise the rest of a
sentence that starts with the entry). The bridge stays the authority: of
an IPv6 address the chart checks only the characters.
*/}}
{{- define "harbor-bridge.registryEntry.parse" -}}
{{- $s := trim (toString .) -}}
{{- $err := "" -}}
{{- $host := "" -}}
{{- $port := "" -}}
{{- $hasPort := false -}}
{{- $v6 := false -}}
{{- $prefix := "" -}}
{{- if eq $s "" -}}
{{- $err = "is empty" -}}
{{- else if contains "://" $s -}}
{{- $err = "must not carry a scheme: write host[:port][/path-prefix]" -}}
{{- else if regexMatch "[@?#*\\\\ \t]" $s -}}
{{- $err = "may contain only a host, a port and a path prefix (no credentials, query, fragment, wildcard or whitespace)" -}}
{{- else -}}
{{- $hostPort := regexReplaceAll "/.*$" $s "" -}}
{{- if hasPrefix "[" $hostPort -}}
{{- if regexMatch "^\\[[0-9A-Fa-f.:]*:[0-9A-Fa-f.:]*\\](:.*)?$" $hostPort -}}
{{- $v6 = true -}}
{{- $host = regexReplaceAll "^\\[([^\\]]*)\\].*$" $hostPort "${1}" | lower -}}
{{- $hasPort = regexMatch "^\\[[^\\]]*\\]:" $hostPort -}}
{{- $port = regexReplaceAll "^\\[[^\\]]*\\]:?" $hostPort "" -}}
{{- else -}}
{{- $err = "must write an IPv6 address as [address] or [address]:port" -}}
{{- end -}}
{{- else if gt (len (splitList ":" $hostPort)) 2 -}}
{{- $err = "must write an IPv6 address in brackets: [address]:port" -}}
{{- else -}}
{{- $host = regexReplaceAll ":.*$" $hostPort "" | lower -}}
{{- $hasPort = contains ":" $hostPort -}}
{{- $port = regexReplaceAll "^[^:]*:?" $hostPort "" -}}
{{- if or (eq (len $host) 0) (gt (len $host) 253) -}}
{{- $err = printf "has the host name %q, which must have 1 to 253 characters" $host -}}
{{- else -}}
{{- range splitList "." $host -}}
{{- if and (not $err) (or (gt (len .) 63) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" .))) -}}
{{- $err = printf "has %q, which is not a host name or IP address" $host -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if and (not $err) $hasPort (or (not (regexMatch "^[1-9][0-9]{0,4}$" $port)) (gt (atoi $port) 65535)) -}}
{{- $err = printf "has the port %q, which must be a number from 1 to 65535 without leading zeros" $port -}}
{{- end -}}
{{- if and (not $err) (contains "/" $s) -}}
{{- $prefix = trimPrefix (printf "%s/" $hostPort) $s -}}
{{- range splitList "/" $prefix -}}
{{- if and (not $err) (not (regexMatch "^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$" .)) -}}
{{- $err = printf "has the path segment %q, which is not a repository path component (lower-case letters and digits, separated by '.', '_', '__' or '-'; no empty segment, no trailing '/')" . -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $hp := $host -}}
{{- if $v6 -}}{{- $hp = printf "[%s]" $host -}}{{- end -}}
{{- if $hasPort -}}{{- $hp = printf "%s:%s" $hp $port -}}{{- end -}}
{{- dict "hostPort" $hp "prefix" $prefix "error" $err | toJson -}}
{{- end -}}

{{/*
registryEntry.string renders a parsed entry as the bridge writes it:
hostPort, then "/" and the prefix when there is one.
*/}}
{{- define "harbor-bridge.registryEntry.string" -}}
{{- .hostPort -}}{{- if .prefix -}}/{{ .prefix }}{{- end -}}
{{- end -}}

{{/*
harbor.registryEntries renders, as JSON {"items": [...]}, the registry
hosts the bridge routes to Harbor once Nexus is enabled:
harbor.registryHosts, or else the host[:port] of harbor.url
(bridge/controlplane/config.go loadRegistryHostsFromEnv). Each item is
registryEntry.parse's JSON plus "source", the value it came from.
*/}}
{{- define "harbor-bridge.harbor.registryEntries" -}}
{{- $items := list -}}
{{- $hosts := .Values.harbor.registryHosts | default (list) -}}
{{- if and (kindIs "slice" $hosts) $hosts -}}
{{- range $hosts -}}
{{- $items = append $items (merge (include "harbor-bridge.registryEntry.parse" . | fromJson) (dict "source" "harbor.registryHosts")) -}}
{{- end -}}
{{- else -}}
{{- $authority := regexReplaceAll "^[A-Za-z][A-Za-z0-9+.-]*://([^/?#]*).*$" (toString .Values.harbor.url) "${1}" -}}
{{- $host := regexReplaceAll "^.*@" $authority "" -}}
{{- $items = append $items (merge (include "harbor-bridge.registryEntry.parse" $host | fromJson) (dict "source" "the host of harbor.url")) -}}
{{- end -}}
{{- dict "items" $items | toJson -}}
{{- end -}}

{{/*
kubelet.splitHostPort splits a host[:port] as kubelet's SplitURL does
(pkg/credentialprovider/keyring.go): net.SplitHostPort, and on its error
the whole string as the host without a port. Renders JSON {host, port}.
*/}}
{{- define "harbor-bridge.kubelet.splitHostPort" -}}
{{- if regexMatch "^\\[[^\\[\\]]*\\]:[^:\\[\\]]*$" . -}}
{{- dict "host" (regexReplaceAll "^\\[([^\\[\\]]*)\\]:.*$" . "${1}") "port" (regexReplaceAll "^.*:" . "") | toJson -}}
{{- else if regexMatch "^[^:\\[\\]]*:[^:\\[\\]]*$" . -}}
{{- dict "host" (regexReplaceAll ":.*$" . "") "port" (regexReplaceAll "^.*:" . "") | toJson -}}
{{- else -}}
{{- dict "host" . "port" "" | toJson -}}
{{- end -}}
{{- end -}}

{{/*
kubelet.labelMatches renders "true" when the matchImages host label
(index 0) matches the registry host label (index 1) under
filepath.Match. A label with '[' or '\' (a character class or an escape)
never matches here: the chart does not evaluate those, so it may only
report an entry as not covered that kubelet would cover, never the
reverse.
*/}}
{{- define "harbor-bridge.kubelet.labelMatches" -}}
{{- $glob := index . 0 -}}
{{- if not (regexMatch "[\\[\\\\]" $glob) -}}
{{- if regexMatch (printf "^%s$" (regexQuoteMeta $glob | replace "\\*" ".*")) (index . 1) -}}true{{- end -}}
{{- end -}}
{{- end -}}

{{/*
kubelet.covers renders "true" when the plugin.matchImages entry (index 0)
matches every image the registry entry (index 1, registryEntry.parse's
JSON) routes to its backend. It mirrors kubelet's URLsMatch
(pkg/credentialprovider/keyring.go), which the credential-provider
plugin's matchImages uses:
  - the entry is parsed as the URL https://<entry>, so a '?' or '#' ends
    it (a '?' is no glob there) and a user@ part is dropped;
  - the port must be equal (none equals none);
  - the host must have as many '.'-separated labels, each matched by the
    entry's label (kubelet.labelMatches);
  - the entry's path must be a raw string prefix of the image's path.
An image the registry entry host[:port]/p routes has the path /p or
/p/..., so the matchImages path must be a prefix of /p: empty, "/p", or
a shorter prefix of it; "/p/" misses the image named p itself. Without p
the path must be empty or "/".
*/}}
{{- define "harbor-bridge.kubelet.covers" -}}
{{- $glob := regexReplaceAll "[?#].*$" (toString (index . 0)) "" -}}
{{- $entry := index . 1 -}}
{{- $authority := regexReplaceAll "/.*$" $glob "" -}}
{{- $path := trimPrefix $authority $glob -}}
{{- $g := include "harbor-bridge.kubelet.splitHostPort" (regexReplaceAll "^.*@" $authority "") | fromJson -}}
{{- $e := include "harbor-bridge.kubelet.splitHostPort" $entry.hostPort | fromJson -}}
{{- if and (eq $g.port $e.port) (hasPrefix $path (printf "/%s" $entry.prefix)) -}}
{{- $gl := splitList "." $g.host -}}
{{- $el := splitList "." $e.host -}}
{{- if eq (len $gl) (len $el) -}}
{{- $ok := true -}}
{{- range $i, $l := $gl -}}
{{- if not (include "harbor-bridge.kubelet.labelMatches" (list $l (index $el $i))) -}}
{{- $ok = false -}}
{{- end -}}
{{- end -}}
{{- if $ok -}}true{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
kubelet.coveredBy renders "true" when one entry of plugin.matchImages
(index 1) covers the parsed registry entry (index 0) by itself.
*/}}
{{- define "harbor-bridge.kubelet.coveredBy" -}}
{{- $entry := index . 0 -}}
{{- $covered := false -}}
{{- range index . 1 -}}
{{- if include "harbor-bridge.kubelet.covers" (list . $entry) -}}
{{- $covered = true -}}
{{- end -}}
{{- end -}}
{{- if $covered -}}true{{- end -}}
{{- end -}}

{{/*
nexus.validate is the part of validateRequiredValues for the registry
hosts and the Nexus backend. Nothing of it applies to an install without
nexus.enabled but the syntax of harbor.registryHosts, which the bridge
parses whenever it is set.
*/}}
{{- define "harbor-bridge.nexus.validate" -}}
{{- $nexus := .Values.nexus | default dict -}}
{{- $enabled := dig "enabled" false $nexus -}}
{{- if not (kindIs "bool" $enabled) -}}
{{- fail (printf "nexus.enabled=%q must be true or false." (toString $enabled)) -}}
{{- end -}}
{{- $harborHosts := .Values.harbor.registryHosts | default (list) -}}
{{- if not (kindIs "slice" $harborHosts) -}}
{{- fail "harbor.registryHosts must be a list of host[:port][/path-prefix] entries, e.g. [\"harbor.example.com\"] (ADR-0036)." -}}
{{- end -}}
{{- range $harborHosts -}}
{{- $e := include "harbor-bridge.registryEntry.parse" . | fromJson -}}
{{- if $e.error -}}
{{- fail (printf "harbor.registryHosts entry %q %s (ADR-0036)." (toString .) $e.error) -}}
{{- end -}}
{{- end -}}
{{- if $enabled -}}
{{- $url := toString (dig "url" "" $nexus) -}}
{{- if not $url -}}
{{- fail "nexus.url is REQUIRED when nexus.enabled=true: the base URL of Nexus, e.g. https://nexus.example.com (ADR-0036)." -}}
{{- end -}}
{{- /* The value is never repeated: it may carry user:password@. The
       shared check refuses every "@" and a URL without an http(s) scheme
       and a host, as for harbor.url; the bridge refuses a query or a
       fragment in its base URL as well (config.go loadNexus). */}}
{{- include "harbor-bridge.validateURL" (list "nexus.url" $url "a user:password@ part never takes effect (the bridge authenticates to Nexus only with nexus.adminCredsSecret) and only ends up in logs") -}}
{{- if regexMatch "[?#[:space:]]" (trim $url) -}}
{{- fail "nexus.url must be the base URL of Nexus, a scheme, a host and an optional path, without a query, a fragment or whitespace. The value is left out of this message: it could hold a credential." -}}
{{- end -}}
{{- $allowHTTP := dig "allowInsecureHTTP" false $nexus -}}
{{- if not (kindIs "bool" $allowHTTP) -}}
{{- fail (printf "nexus.allowInsecureHTTP=%q must be true or false." (toString $allowHTTP)) -}}
{{- end -}}
{{- if and (hasPrefix "http://" (lower $url)) (not $allowHTTP) -}}
{{- fail "nexus.url uses plain http: the Nexus admin credentials and every new user's password would travel unencrypted. Use https (nexus.caSecret for a private CA), or set nexus.allowInsecureHTTP=true." -}}
{{- end -}}
{{- if not (dig "adminCredsSecret" "name" "" $nexus) -}}
{{- fail "nexus.adminCredsSecret.name is REQUIRED when nexus.enabled=true. Pre-create a Secret in the release namespace holding the Nexus credential {username,password} the bridge manages users and roles with (ADR-0036)." -}}
{{- end -}}
{{- range $k := list "username" "password" -}}
{{- if not (dig "adminCredsSecret" "keys" $k $k $nexus) -}}
{{- fail (printf "nexus.adminCredsSecret.keys.%s must name the key of nexus.adminCredsSecret.name that holds the %s (default %s)." $k $k $k) -}}
{{- end -}}
{{- end -}}
{{- if and (dig "caSecret" "name" "" $nexus) (not (dig "caSecret" "key" "ca.crt" $nexus)) -}}
{{- fail "nexus.caSecret.key must name the key of nexus.caSecret.name that holds the CA (default ca.crt)." -}}
{{- end -}}
{{- $backoff := toString (dig "rateLimitBackoff" "15m" $nexus) -}}
{{- if or (not (regexMatch "^([0-9]+(\\.[0-9]+)?(ns|us|ms|s|m|h))+$" $backoff)) (not (regexMatch "[1-9]" $backoff)) -}}
{{- fail (printf "nexus.rateLimitBackoff=%q must be a positive Go duration such as 15m (ADR-0036)." $backoff) -}}
{{- end -}}
{{- $nexusHosts := dig "registryHosts" (list) $nexus | default (list) -}}
{{- if not (kindIs "slice" $nexusHosts) -}}
{{- fail "nexus.registryHosts must be a list of host[:port][/path-prefix] entries, e.g. [\"nexus.example.com:8082\"] (ADR-0036)." -}}
{{- end -}}
{{- if not $nexusHosts -}}
{{- fail "nexus.registryHosts is REQUIRED when nexus.enabled=true: the registry hosts (host[:port][/path-prefix]) kubelet pulls Nexus's images from. The bridge routes each credential request to Nexus by the image's registry host (ADR-0036)." -}}
{{- end -}}
{{- $nexusEntries := list -}}
{{- range $nexusHosts -}}
{{- $e := include "harbor-bridge.registryEntry.parse" . | fromJson -}}
{{- if $e.error -}}
{{- fail (printf "nexus.registryHosts entry %q %s (ADR-0036)." (toString .) $e.error) -}}
{{- end -}}
{{- $nexusEntries = append $nexusEntries $e -}}
{{- end -}}
{{- $harborEntries := (include "harbor-bridge.harbor.registryEntries" . | fromJson).items -}}
{{- range $harborEntries -}}
{{- if .error -}}
{{- fail (printf "the host of harbor.url is no registry host: it %s. Set harbor.registryHosts to the registry hosts kubelet pulls Harbor's images from (ADR-0036)." .error) -}}
{{- end -}}
{{- end -}}
{{- range $h := $harborEntries -}}
{{- range $n := $nexusEntries -}}
{{- if eq $h.hostPort $n.hostPort -}}
{{- fail (printf "registry host %q is both Harbor's (%s) and Nexus's (nexus.registryHosts): kubelet caches credentials per registry host, so it would hand one backend's credentials to the other's images. Give each backend its own host[:port]%s (ADR-0036)." $h.hostPort $h.source (ternary "; set harbor.registryHosts if kubelet pulls Harbor's images from another address than harbor.url" "" (ne $h.source "harbor.registryHosts"))) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if .Values.plugin.enabled -}}
{{- $matchImages := .Values.plugin.matchImages | default (list) -}}
{{- range $nexusEntries -}}
{{- if not (include "harbor-bridge.kubelet.coveredBy" (list . $matchImages)) -}}
{{- $want := include "harbor-bridge.registryEntry.string" . -}}
{{- fail (printf "nexus.registryHosts entry %q is not covered by plugin.matchImages: kubelet calls the plugin only for images a matchImages entry matches, so pulls of its images would get no credentials from the bridge. Add %q to plugin.matchImages. Kubelet compares the port exactly, takes globs only in the host (e.g. *.example.com) and compares the path as a raw string prefix: a covering entry is the host[:port] itself, or host[:port] followed by a prefix of the path prefix without a trailing '/' (ADR-0036)." $want $want) -}}
{{- end -}}
{{- end -}}
{{- range $harborEntries -}}
{{- if not (include "harbor-bridge.kubelet.coveredBy" (list . $matchImages)) -}}
{{- $want := include "harbor-bridge.registryEntry.string" . -}}
{{- fail (printf "Harbor's registry host %q (%s) is not covered by plugin.matchImages. With nexus.enabled the bridge routes each image by its registry host, and kubelet calls the plugin only for images a matchImages entry matches. If kubelet pulls Harbor's images from another address than harbor.url, set harbor.registryHosts to it; otherwise add %q to plugin.matchImages. Kubelet compares the port exactly, takes globs only in the host and compares the path as a raw string prefix (ADR-0036)." $want .source $want) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
