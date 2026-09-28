// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// The merge works on map[string]any, not on typed kubelet config
// structs, so that foreign providers' unknown fields round-trip
// untouched (typed round-tripping would silently drop them) and the
// installer stays off the k8s.io module curve. sigs.k8s.io/yaml is the
// one permitted sigs.k8s.io import (ADR-0021): it parses YAML and JSON
// alike, which is exactly the on-disk reality (EKS ships JSON, GKE and
// AKS ship YAML).

const credentialProviderConfigAPIVersion = "kubelet.config.k8s.io/v1"

// bridgeEndpointEnv is the env var every harbor-bridge provider entry sets
// (the plugin cannot run without it). It tells a bridge entry apart from a
// foreign provider that happens to have the same name.
const bridgeEndpointEnv = "HARBOR_BRIDGE_ENDPOINT"

// renderedProvider extracts the provider entry named name from the
// chart-rendered CredentialProviderConfig bytes.
func renderedProvider(rendered []byte, name string) (map[string]any, error) {
	cfg := map[string]any{}
	if err := yaml.Unmarshal(rendered, &cfg); err != nil {
		return nil, fmt.Errorf("parse rendered credential-provider config: %w", err)
	}
	providers, err := providerList(cfg)
	if err != nil {
		return nil, fmt.Errorf("rendered credential-provider config: %w", err)
	}
	for _, p := range providers {
		entry, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if entry["name"] == name {
			return entry, nil
		}
	}
	return nil, fmt.Errorf("rendered credential-provider config has no provider named %q", name)
}

// credentialProviderAPIVersions are the provider apiVersions kubelet
// accepts (pkg/credentialprovider/plugin/config.go, apiVersions);
// tokenAttributes needs the first.
var credentialProviderAPIVersions = []string{
	"credentialprovider.kubelet.k8s.io/v1",
	"credentialprovider.kubelet.k8s.io/v1beta1",
	"credentialprovider.kubelet.k8s.io/v1alpha1",
}

// validateEntry refuses this install's rendered provider entry when kubelet
// would reject it at startup for a reason that does not depend on kubelet's
// version (ADR-0033): kubelet validates every provider
// (pkg/credentialprovider/plugin/config.go) and exits when one fails, and
// the chart fills matchImages, defaultCacheDuration and the audience from
// values it does not check. Checked before anything is written, like every
// refusal. Standard library only (ADR-0021): matchImages entries parse as
// kubelet's ParseSchemelessURL does (url.Parse of "https://" + entry), and
// defaultCacheDuration as metav1.Duration does (a string for
// time.ParseDuration). A feature gate kubelet has off (tokenAttributes) is
// not checked; the rollback after a restart that does not verify covers it.
func validateEntry(entry map[string]any) error {
	var problems []string
	apiVersion, _ := entry["apiVersion"].(string)
	if !slices.Contains(credentialProviderAPIVersions, apiVersion) {
		problems = append(problems, fmt.Sprintf("apiVersion %v is not one of %s", entry["apiVersion"], strings.Join(credentialProviderAPIVersions, ", ")))
	}
	images, _ := entry["matchImages"].([]any)
	if len(images) == 0 {
		problems = append(problems, "matchImages is empty")
	}
	for _, img := range images {
		s, ok := img.(string)
		if !ok {
			problems = append(problems, fmt.Sprintf("matchImages entry %v is not a string", img))
			continue
		}
		if _, err := url.Parse("https://" + s); err != nil {
			problems = append(problems, fmt.Sprintf("matchImages entry %q is not a valid image host pattern: %v", s, err))
		}
	}
	switch d, ok := entry["defaultCacheDuration"].(string); {
	case !ok:
		problems = append(problems, fmt.Sprintf("defaultCacheDuration %v is not a duration string such as \"1h\"", entry["defaultCacheDuration"]))
	default:
		if dur, err := time.ParseDuration(d); err != nil {
			problems = append(problems, fmt.Sprintf("defaultCacheDuration %q is not a Go duration (units h, m, s, ms, us, ns): %v", d, err))
		} else if dur < 0 {
			problems = append(problems, fmt.Sprintf("defaultCacheDuration %q is negative", d))
		}
	}
	if problem := bridgeEndpointProblem(entry); problem != "" {
		problems = append(problems, problem)
	}
	if raw, present := entry["tokenAttributes"]; present {
		ta, _ := raw.(map[string]any)
		if apiVersion != credentialProviderAPIVersions[0] {
			problems = append(problems, fmt.Sprintf("tokenAttributes needs apiVersion %s", credentialProviderAPIVersions[0]))
		}
		if aud, _ := ta["serviceAccountTokenAudience"].(string); aud == "" {
			problems = append(problems, "tokenAttributes.serviceAccountTokenAudience is empty")
		}
		if _, ok := ta["requireServiceAccount"].(bool); !ok {
			problems = append(problems, "tokenAttributes.requireServiceAccount is not true or false")
		}
		if ct, _ := ta["cacheType"].(string); ct != "ServiceAccount" && ct != "Token" {
			problems = append(problems, fmt.Sprintf("tokenAttributes.cacheType %v is not ServiceAccount or Token", ta["cacheType"]))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("kubelet would refuse the rendered provider entry %v and not start, or the plugin would fail on every pull: %s; fix the chart values (plugin.matchImages, plugin.defaultCacheDuration, plugin.audience, plugin.bridgeEndpoint)", entry["name"], strings.Join(problems, "; "))
	}
	return nil
}

// bridgeEndpointProblem describes what is wrong with the entry's
// HARBOR_BRIDGE_ENDPOINT, the checks the plugin runs on every exec
// (plugin/main.go loadConfig): an https URL with a host. The plugin
// refuses anything else and returns no credentials for any pull, which
// kubelet only logs; the installer refuses it before it writes the entry
// and restarts kubelet onto it.
func bridgeEndpointProblem(entry map[string]any) string {
	env, _ := entry["env"].([]any)
	for _, e := range env {
		m, _ := e.(map[string]any)
		if m["name"] != bridgeEndpointEnv {
			continue
		}
		value, _ := m["value"].(string)
		u, err := url.Parse(value)
		switch {
		case err != nil:
			return fmt.Sprintf("%s %q is not a valid URL: %v", bridgeEndpointEnv, value, err)
		case u.Scheme != "https":
			return fmt.Sprintf("%s %q does not use https", bridgeEndpointEnv, value)
		case u.Host == "":
			return fmt.Sprintf("%s %q has no host", bridgeEndpointEnv, value)
		}
		return ""
	}
	return fmt.Sprintf("%s is not set", bridgeEndpointEnv)
}

// mergeProvider inserts entry into the CredentialProviderConfig in
// existing (JSON or YAML), replacing a same-name entry or appending.
// It returns the re-marshaled document in the format it found (so a
// cloud provider's JSON file stays JSON) and whether the document
// changed. Everything except our own entry round-trips untouched.
func mergeProvider(existing []byte, entry map[string]any) (out []byte, changed bool, err error) {
	cfg := map[string]any{}
	if err := yaml.Unmarshal(existing, &cfg); err != nil {
		return nil, false, fmt.Errorf("parse node credential-provider config: %w", err)
	}
	if av, ok := cfg["apiVersion"].(string); !ok || av != credentialProviderConfigAPIVersion {
		return nil, false, fmt.Errorf("node credential-provider config has apiVersion %v, want %s — refusing to merge into an unknown schema",
			cfg["apiVersion"], credentialProviderConfigAPIVersion)
	}
	providers, err := providerList(cfg)
	if err != nil {
		return nil, false, fmt.Errorf("node credential-provider config: %w", err)
	}

	name, ok := entry["name"].(string)
	if !ok || name == "" {
		return nil, false, fmt.Errorf("provider entry has no name")
	}
	replaced := false
	for i, p := range providers {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if pm["name"] == name {
			// Every other install and every foreign provider keeps its
			// entry; ours replaces only an entry that is a bridge entry
			// too (ADR-0029). A non-default provider name can otherwise
			// take over a cloud provider's entry (and its binary).
			if !isBridgeProvider(pm) {
				return nil, false, fmt.Errorf("the node credential-provider config already has a provider named %q that is not a harbor-bridge plugin (no %s env) — refusing to replace it; choose another plugin.providerName", name, bridgeEndpointEnv)
			}
			providers[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		providers = append(providers, entry)
	}
	cfg["providers"] = providers

	// Compare parsed documents, not bytes: when our entry is already
	// there, return the file's own bytes untouched. Re-marshaling is not a
	// fixed point for every YAML input (YAML 1.1 reads keys like `012` or
	// `00:8002` as numbers, which come back as quoted strings sorted
	// differently), so a byte comparison could rewrite the file on every
	// run and restart kubelet each time (found by FuzzMergeProvider).
	// Untouched bytes also keep the node's own formatting.
	orig := map[string]any{}
	if err := yaml.Unmarshal(existing, &orig); err != nil {
		return nil, false, fmt.Errorf("reparse node credential-provider config: %w", err)
	}
	if reflect.DeepEqual(orig, cfg) {
		return existing, false, nil
	}
	asJSON := isJSON(existing)
	out, err = marshalMatching(cfg, asJSON)
	if err != nil {
		return nil, false, fmt.Errorf("marshal merged credential-provider config: %w", err)
	}
	if err := checkRoundTrip(out, cfg, asJSON); err != nil {
		return nil, false, fmt.Errorf("merged credential-provider config: %w; refusing to write it", err)
	}
	return out, true, nil
}

// errNoRoundTrip marks a config the installer would write that does not
// read back as the document it meant to write (checkRoundTrip).
var errNoRoundTrip = errors.New("does not read back as the document it was written from")

// checkRoundTrip refuses out, the config the installer is about to write
// for the document want, unless reading it back gives want again: with
// sigs.k8s.io/yaml, as the installer reads every config on its next pass
// and kubelet's decoder reads YAML (YAML to JSON), and for JSON output also
// with encoding/json, as kubelet's decoder reads a file that starts with
// "{". encoding/json writes some characters raw that YAML reads
// differently: U+0085 (NEL) is a line break to YAML (found by
// FuzzMergeProvider), so the next pass could not parse the file, or would
// read and later write back a changed value of another provider.
func checkRoundTrip(out []byte, want map[string]any, asJSON bool) error {
	back := map[string]any{}
	if err := yaml.Unmarshal(out, &back); err != nil {
		return fmt.Errorf("%w: parsing it again fails: %w", errNoRoundTrip, err)
	}
	if !reflect.DeepEqual(back, want) {
		return fmt.Errorf("%w (YAML)", errNoRoundTrip)
	}
	if !asJSON {
		return nil
	}
	back = map[string]any{}
	if err := json.Unmarshal(out, &back); err != nil {
		return fmt.Errorf("%w: parsing it again as JSON fails: %w", errNoRoundTrip, err)
	}
	if !reflect.DeepEqual(back, want) {
		return fmt.Errorf("%w (JSON)", errNoRoundTrip)
	}
	return nil
}

// isBridgeProvider reports whether a provider entry belongs to a
// harbor-bridge plugin: it sets bridgeEndpointEnv.
func isBridgeProvider(entry map[string]any) bool {
	env, _ := entry["env"].([]any)
	for _, e := range env {
		if m, ok := e.(map[string]any); ok && m["name"] == bridgeEndpointEnv {
			return true
		}
	}
	return false
}

// composeOwnConfig builds the chart-owned credential-provider config of
// patch and none mode (ADR-0029) from the rendered config and the file as
// it is (existing). The result holds this install's rendered entry and
// every entry of existing that sibling accepts as another install's, in the
// order of existing: the rendered entry takes the place of the first entry
// of its name, or comes last. Every other entry of existing, including a
// second one of a kept name (kubelet refuses duplicate names), is dropped
// and described in dropped. With nothing kept the result is rendered byte
// for byte, which is what installers before ADR-0029 always wrote; when the
// result equals existing it is existing byte for byte (no rewrite, no
// restart). It refuses what mergeProvider refuses as a document.
func composeOwnConfig(existing, rendered []byte, entry map[string]any, sibling func(map[string]any) bool) (out []byte, dropped []string, err error) {
	cur := map[string]any{}
	if err := yaml.Unmarshal(existing, &cur); err != nil {
		return nil, nil, fmt.Errorf("parse: %w", err)
	}
	if av, ok := cur["apiVersion"].(string); !ok || av != credentialProviderConfigAPIVersion {
		return nil, nil, fmt.Errorf("apiVersion is %v, want %s", cur["apiVersion"], credentialProviderConfigAPIVersion)
	}
	providers, err := providerList(cur)
	if err != nil {
		return nil, nil, err
	}
	name, ok := entry["name"].(string)
	if !ok || name == "" {
		return nil, nil, fmt.Errorf("provider entry has no name")
	}

	kept := []any{}
	seen := map[string]bool{}
	for _, p := range providers {
		pm, isMap := p.(map[string]any)
		pname, _ := pm["name"].(string)
		switch {
		case isMap && pname == name && !seen[name]:
			kept = append(kept, entry)
			seen[name] = true
		case isMap && pname != name && !seen[pname] && sibling(pm):
			kept = append(kept, pm)
			seen[pname] = true
		default:
			dropped = append(dropped, strconv.Quote(pname))
		}
	}
	if !seen[name] {
		kept = append(kept, entry)
	}
	if len(kept) == 1 {
		return rendered, dropped, nil
	}

	doc := map[string]any{}
	if err := yaml.Unmarshal(rendered, &doc); err != nil {
		return nil, nil, fmt.Errorf("parse rendered credential-provider config: %w", err)
	}
	doc["providers"] = kept
	if reflect.DeepEqual(cur, doc) {
		return existing, dropped, nil
	}
	out, err = yaml.Marshal(doc)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal credential-provider config: %w", err)
	}
	if err := checkRoundTrip(out, doc, false); err != nil {
		return nil, nil, err
	}
	return out, dropped, nil
}

// kubeletStartProblems describes what in the CredentialProviderConfig doc
// keeps kubelet from starting and does not depend on kubelet's version
// (ADR-0029, Context; pkg/credentialprovider/plugin/config.go and
// plugin.go): an entry that is not an object or has no string name, a name
// with "/" or a space, "." or "..", a name that occurs twice, and, for
// every name but own, whose binary this pass writes, a binary that
// hasBinary does not find. It does not repeat kubelet's other schema checks.
func kubeletStartProblems(doc []byte, own string, hasBinary func(name string) bool) ([]string, error) {
	cfg := map[string]any{}
	if err := yaml.Unmarshal(doc, &cfg); err != nil {
		return nil, fmt.Errorf("parse credential-provider config: %w", err)
	}
	providers, err := providerList(cfg)
	if err != nil {
		return nil, err
	}
	var problems []string
	seen := map[string]bool{}
	for i, p := range providers {
		pm, _ := p.(map[string]any)
		name, ok := pm["name"].(string)
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("provider %d has no string name", i+1))
			continue
		case name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/ "):
			problems = append(problems, fmt.Sprintf("kubelet refuses the provider name %q", name))
		case seen[name]:
			problems = append(problems, fmt.Sprintf("the provider name %q occurs more than once", name))
		case name != own && !hasBinary(name):
			problems = append(problems, fmt.Sprintf("the binary of provider %q is missing from kubelet's bin dir", name))
		}
		seen[name] = true
	}
	return problems, nil
}

// otherBridgeProviders returns the names of the harbor-bridge entries in
// the CredentialProviderConfig in doc that are not named name and that
// counts accepts (every one when counts is nil): the entries of other
// installs (ADR-0029). A document it cannot read holds none.
func otherBridgeProviders(doc []byte, name string, counts func(entry map[string]any) bool) []string {
	cfg := map[string]any{}
	if err := yaml.Unmarshal(doc, &cfg); err != nil {
		return nil
	}
	providers, err := providerList(cfg)
	if err != nil {
		return nil
	}
	var names []string
	for _, p := range providers {
		if pm, ok := p.(map[string]any); ok && pm["name"] != name && isBridgeProvider(pm) && (counts == nil || counts(pm)) {
			names = append(names, fmt.Sprint(pm["name"]))
		}
	}
	return names
}

// entryBytes is the canonical form of a provider entry for the state hash:
// JSON with sorted keys, so it depends on the entry's content only, not on
// the formatting of the file it sits in.
func entryBytes(entry map[string]any) ([]byte, error) {
	out, err := json.Marshal(entry)
	if err != nil {
		return nil, fmt.Errorf("marshal provider entry: %w", err)
	}
	return out, nil
}

// marshalMatching renders cfg in the format the node file uses, so a
// cloud provider's JSON file stays JSON. JSON output is indented like the
// typical cloud layout (EKS) and newline-terminated; stable output keeps
// the change detection meaningful.
func marshalMatching(cfg map[string]any, asJSON bool) ([]byte, error) {
	if !asJSON {
		return yaml.Marshal(cfg)
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// providerList returns cfg's providers slice (empty when absent).
func providerList(cfg map[string]any) ([]any, error) {
	raw, ok := cfg["providers"]
	if !ok || raw == nil {
		return []any{}, nil
	}
	providers, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("providers is not a list")
	}
	return providers, nil
}

// isJSON reports whether the document's first non-space byte starts a
// JSON object. YAML files never start with '{' in the layouts cloud
// providers ship.
func isJSON(doc []byte) bool {
	trimmed := bytes.TrimLeft(doc, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// substituteNodeIP replaces the literal $(NODE_IP) placeholder the
// chart may render into the provider config (plugin.bridgeEndpoint)
// with the node's actual IP from the downward API (status.hostIP, the
// node's primary address). The placeholder is a URL host: an IPv6 address
// goes in brackets ("https://[fd00::5]:31443"), which a bare IPv6 address
// in a URL needs, also when the operator already wrote "[$(NODE_IP)]" to
// work around its absence; an IPv4 address in brackets is no valid host
// and is refused. NODE_IP must be an IP address without a zone.
func substituteNodeIP(rendered []byte, nodeIP string) ([]byte, error) {
	const placeholder = "$(NODE_IP)"
	if !bytes.Contains(rendered, []byte(placeholder)) {
		return rendered, nil
	}
	if nodeIP == "" {
		return nil, fmt.Errorf("rendered config references $(NODE_IP) but NODE_IP is not set")
	}
	addr, err := netip.ParseAddr(nodeIP)
	if err != nil || addr.Zone() != "" {
		return nil, fmt.Errorf("rendered config references $(NODE_IP), but NODE_IP %q is not an IP address", nodeIP)
	}
	bracketed := []byte("[" + placeholder + "]")
	if addr.Is4() {
		if bytes.Contains(rendered, bracketed) {
			return nil, fmt.Errorf("plugin.bridgeEndpoint puts $(NODE_IP) in brackets, which is no valid URL host for this node's IPv4 address %s; write it without brackets (the installer brackets IPv6 addresses itself)", nodeIP)
		}
		return bytes.ReplaceAll(rendered, []byte(placeholder), []byte(addr.String())), nil
	}
	host := []byte("[" + addr.String() + "]")
	out := bytes.ReplaceAll(rendered, bracketed, host)
	return bytes.ReplaceAll(out, []byte(placeholder), host), nil
}
