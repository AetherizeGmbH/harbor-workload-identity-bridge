// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"

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
	out, err = marshalMatching(cfg, isJSON(existing))
	if err != nil {
		return nil, false, fmt.Errorf("marshal merged credential-provider config: %w", err)
	}
	return out, true, nil
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

// hasBridgeProvider reports whether the CredentialProviderConfig in doc
// holds a harbor-bridge entry named name. A document it cannot read holds
// none.
func hasBridgeProvider(doc []byte, name string) bool {
	cfg := map[string]any{}
	if err := yaml.Unmarshal(doc, &cfg); err != nil {
		return false
	}
	providers, err := providerList(cfg)
	if err != nil {
		return false
	}
	for _, p := range providers {
		if pm, ok := p.(map[string]any); ok && pm["name"] == name {
			return isBridgeProvider(pm)
		}
	}
	return false
}

// otherProviders counts the providers in the CredentialProviderConfig in
// doc that are not named name: the entries of other installs (ADR-0029)
// and of anything else. It refuses what mergeProvider refuses.
func otherProviders(doc []byte, name string) (int, error) {
	cfg := map[string]any{}
	if err := yaml.Unmarshal(doc, &cfg); err != nil {
		return 0, fmt.Errorf("parse: %w", err)
	}
	if av, ok := cfg["apiVersion"].(string); !ok || av != credentialProviderConfigAPIVersion {
		return 0, fmt.Errorf("apiVersion is %v, want %s", cfg["apiVersion"], credentialProviderConfigAPIVersion)
	}
	providers, err := providerList(cfg)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range providers {
		if pm, ok := p.(map[string]any); !ok || pm["name"] != name {
			n++
		}
	}
	return n, nil
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
// with the node's actual IP from the downward API.
func substituteNodeIP(rendered []byte, nodeIP string) ([]byte, error) {
	if !bytes.Contains(rendered, []byte("$(NODE_IP)")) {
		return rendered, nil
	}
	if nodeIP == "" {
		return nil, fmt.Errorf("rendered config references $(NODE_IP) but NODE_IP is not set")
	}
	return bytes.ReplaceAll(rendered, []byte("$(NODE_IP)"), []byte(nodeIP)), nil
}
