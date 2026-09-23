// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// renderedConfig mirrors what the chart's plugin-configmap.yaml renders.
const renderedConfig = `apiVersion: kubelet.config.k8s.io/v1
kind: CredentialProviderConfig
providers:
  - name: harbor-bridge-plugin
    apiVersion: credentialprovider.kubelet.k8s.io/v1
    matchImages:
      - "harbor.example.com"
    defaultCacheDuration: "1h"
    env:
      - name: HARBOR_BRIDGE_ENDPOINT
        value: "https://127.0.0.1:31443"
      - name: HARBOR_BRIDGE_CA_BUNDLE
        value: "/etc/kubernetes/credential-provider-config/harbor-bridge-ca.crt"
    tokenAttributes:
      serviceAccountTokenAudience: "harbor-bridge"
      requireServiceAccount: true
      cacheType: ServiceAccount
`

// eksConfig is shaped like /etc/eks/image-credential-provider/config.json
// on AL2023, including a field our types don't know about.
const eksConfig = `{
  "apiVersion": "kubelet.config.k8s.io/v1",
  "kind": "CredentialProviderConfig",
  "providers": [
    {
      "name": "ecr-credential-provider",
      "matchImages": ["*.dkr.ecr.*.amazonaws.com", "public.ecr.aws"],
      "defaultCacheDuration": "12h",
      "apiVersion": "credentialprovider.kubelet.k8s.io/v1",
      "args": ["get-credentials"],
      "unknownVendorField": {"keep": "me"}
    }
  ]
}
`

// gkeConfig is a YAML-shaped cloud config (GKE/AKS style).
const gkeConfig = `apiVersion: kubelet.config.k8s.io/v1
kind: CredentialProviderConfig
providers:
  - name: auth-provider-gcp
    apiVersion: credentialprovider.kubelet.k8s.io/v1
    matchImages:
      - "container.cloud.google.com"
      - "*.pkg.dev"
    defaultCacheDuration: "1m"
    args:
      - get-credentials
      - --v=3
`

func TestRenderedProvider(t *testing.T) {
	entry, err := renderedProvider([]byte(renderedConfig), "harbor-bridge-plugin")
	if err != nil {
		t.Fatalf("renderedProvider: %v", err)
	}
	if entry["name"] != "harbor-bridge-plugin" {
		t.Fatalf("wrong entry extracted: %v", entry["name"])
	}
	if _, ok := entry["tokenAttributes"]; !ok {
		t.Fatal("tokenAttributes lost in extraction")
	}
}

func TestRenderedProvider_MissingName(t *testing.T) {
	if _, err := renderedProvider([]byte(renderedConfig), "nope"); err == nil {
		t.Fatal("expected error for missing provider name")
	}
}

func TestMergeProvider_AppendsToJSONPreservingUnknownFields(t *testing.T) {
	entry, err := renderedProvider([]byte(renderedConfig), "harbor-bridge-plugin")
	if err != nil {
		t.Fatalf("renderedProvider: %v", err)
	}
	merged, changed, err := mergeProvider([]byte(eksConfig), entry)
	if err != nil {
		t.Fatalf("mergeProvider: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true on first merge")
	}
	if !isJSON(merged) {
		t.Fatalf("JSON input must stay JSON, got:\n%s", merged)
	}
	out := parseConfig(t, merged)
	providers := out["providers"].([]any)
	if len(providers) != 2 {
		t.Fatalf("want 2 providers, got %d", len(providers))
	}
	ecr := providers[0].(map[string]any)
	if ecr["name"] != "ecr-credential-provider" {
		t.Fatalf("foreign provider moved: %v", ecr["name"])
	}
	if _, ok := ecr["unknownVendorField"]; !ok {
		t.Fatal("unknown vendor field dropped by merge")
	}
	ours := providers[1].(map[string]any)
	if ours["name"] != "harbor-bridge-plugin" {
		t.Fatalf("our provider not appended: %v", ours["name"])
	}
}

func TestMergeProvider_AppendsToYAMLStayingYAML(t *testing.T) {
	entry, err := renderedProvider([]byte(renderedConfig), "harbor-bridge-plugin")
	if err != nil {
		t.Fatalf("renderedProvider: %v", err)
	}
	merged, changed, err := mergeProvider([]byte(gkeConfig), entry)
	if err != nil {
		t.Fatalf("mergeProvider: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true on first merge")
	}
	if isJSON(merged) {
		t.Fatalf("YAML input must stay YAML, got:\n%s", merged)
	}
	out := parseConfig(t, merged)
	providers := out["providers"].([]any)
	if len(providers) != 2 {
		t.Fatalf("want 2 providers, got %d", len(providers))
	}
	gcp := providers[0].(map[string]any)
	if gcp["name"] != "auth-provider-gcp" {
		t.Fatalf("foreign provider moved: %v", gcp["name"])
	}
}

func TestMergeProvider_SecondMergeIsIdempotent(t *testing.T) {
	entry, err := renderedProvider([]byte(renderedConfig), "harbor-bridge-plugin")
	if err != nil {
		t.Fatalf("renderedProvider: %v", err)
	}
	first, _, err := mergeProvider([]byte(eksConfig), entry)
	if err != nil {
		t.Fatalf("first merge: %v", err)
	}
	second, changed, err := mergeProvider(first, entry)
	if err != nil {
		t.Fatalf("second merge: %v", err)
	}
	if changed {
		t.Fatalf("second merge with identical entry must report changed=false, diff:\n%s\nvs\n%s", first, second)
	}
	if string(first) != string(second) {
		t.Fatal("second merge altered the document")
	}
}

func TestMergeProvider_ReplacesExistingEntry(t *testing.T) {
	entry, err := renderedProvider([]byte(renderedConfig), "harbor-bridge-plugin")
	if err != nil {
		t.Fatalf("renderedProvider: %v", err)
	}
	first, _, err := mergeProvider([]byte(eksConfig), entry)
	if err != nil {
		t.Fatalf("first merge: %v", err)
	}
	// Simulate a helm upgrade changing matchImages.
	updated := map[string]any{}
	for k, v := range entry {
		updated[k] = v
	}
	updated["matchImages"] = []any{"harbor.example.com", "harbor-alt.example.com"}

	merged, changed, err := mergeProvider(first, updated)
	if err != nil {
		t.Fatalf("replace merge: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true when the entry differs")
	}
	out := parseConfig(t, merged)
	providers := out["providers"].([]any)
	if len(providers) != 2 {
		t.Fatalf("replace must not duplicate the entry, got %d providers", len(providers))
	}
}

func TestMergeProvider_RefusesUnknownAPIVersion(t *testing.T) {
	entry := map[string]any{"name": "harbor-bridge-plugin"}
	old := strings.Replace(eksConfig, "kubelet.config.k8s.io/v1", "kubelet.config.k8s.io/v1alpha1", 1)
	if _, _, err := mergeProvider([]byte(old), entry); err == nil {
		t.Fatal("expected refusal on non-v1 apiVersion")
	}
}

func TestSubstituteNodeIP(t *testing.T) {
	in := []byte(`value: "https://$(NODE_IP):31443"`)
	out, err := substituteNodeIP(in, "10.0.0.7")
	if err != nil {
		t.Fatalf("substituteNodeIP: %v", err)
	}
	if string(out) != `value: "https://10.0.0.7:31443"` {
		t.Fatalf("unexpected substitution result: %s", out)
	}
}

func TestSubstituteNodeIP_MissingIPFails(t *testing.T) {
	if _, err := substituteNodeIP([]byte("$(NODE_IP)"), ""); err == nil {
		t.Fatal("expected error when NODE_IP unset but referenced")
	}
}

func TestSubstituteNodeIP_NoPlaceholderPassesThrough(t *testing.T) {
	in := []byte("no placeholder here")
	out, err := substituteNodeIP(in, "")
	if err != nil {
		t.Fatalf("substituteNodeIP: %v", err)
	}
	if string(out) != string(in) {
		t.Fatal("document without placeholder must pass through unchanged")
	}
}

func parseConfig(t *testing.T, doc []byte) map[string]any {
	t.Helper()
	out := map[string]any{}
	if err := yaml.Unmarshal(doc, &out); err != nil {
		t.Fatalf("parse merged doc: %v\n%s", err, doc)
	}
	return out
}
