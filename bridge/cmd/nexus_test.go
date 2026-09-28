// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/runtime"

	nexusv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/nexus/v1alpha1"
	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/nexus"
)

// harborOnlyEnv is the environment of a bridge without the Nexus backend.
func harborOnlyEnv() map[string]string {
	return map[string]string{
		controlplane.EnvClusterName:    "prod",
		controlplane.EnvNamespace:      "harbor-bridge-system",
		controlplane.EnvOIDCIssuer:     "https://kubernetes.default.svc",
		controlplane.EnvHarborURL:      "https://harbor.example.com",
		controlplane.EnvHarborAdminDir: "/var/run/secrets/harbor-admin",
		controlplane.EnvAudience:       "harbor-bridge-prod",
	}
}

// nexusEnv adds the Nexus backend to harborOnlyEnv.
func nexusEnv() map[string]string {
	env := harborOnlyEnv()
	env[controlplane.EnvNexusURL] = "https://nexus.example.com"
	env[controlplane.EnvNexusAdminDir] = "/var/run/secrets/nexus-admin"
	env[controlplane.EnvNexusRegistryHosts] = "nexus.example.com:8443,registry.example.com/nexus"
	return env
}

func loadConfig(t *testing.T, env map[string]string) *controlplane.Config {
	t.Helper()
	for _, k := range []string{
		controlplane.EnvNexusURL, controlplane.EnvNexusAdminDir, controlplane.EnvNexusCAFile,
		controlplane.EnvNexusAllowHTTP, controlplane.EnvNexusRegistryHosts, controlplane.EnvNexusRateLimitBackoff,
		controlplane.EnvHarborRegistryHosts,
	} {
		t.Setenv(k, "")
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	cfg, err := controlplane.LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv: %v", err)
	}
	return cfg
}

// With Harbor alone the handler gets exactly the configuration it had
// before the Nexus backend: no routing, no registry hosts.
func TestHandlerConfig_HarborAlone(t *testing.T) {
	cfg := loadConfig(t, harborOnlyEnv())
	hc := handlerConfig(cfg)
	if hc.Nexus != nil || hc.HarborRegistryHosts != nil {
		t.Errorf("Harbor-only handler config routes: Nexus %+v, Harbor hosts %v", hc.Nexus, hc.HarborRegistryHosts)
	}
	if hc.BridgeNamespace != cfg.Namespace || hc.Audience != cfg.Audience || !hc.ForceLocalValidation || hc.RobotUsername == nil {
		t.Errorf("handler config = %+v", hc)
	}
}

// With Nexus the handler routes by both backends' registry hosts, as the
// configuration parsed them; Harbor's default to BRIDGE_HARBOR_URL's host.
func TestHandlerConfig_WithNexus(t *testing.T) {
	cfg := loadConfig(t, nexusEnv())
	hc := handlerConfig(cfg)
	if hc.Nexus == nil {
		t.Fatal("Nexus backend not wired into the handler")
	}
	if got := hostStrings(hc.Nexus.RegistryHosts); got != "nexus.example.com:8443,registry.example.com/nexus" {
		t.Errorf("Nexus registry hosts = %s", got)
	}
	if got := hostStrings(hc.HarborRegistryHosts); got != "harbor.example.com" {
		t.Errorf("Harbor registry hosts = %s, want BRIDGE_HARBOR_URL's host", got)
	}
	if hc.Nexus.IdentityName == nil || hc.Nexus.UserIdentity == nil {
		t.Fatal("Nexus naming not wired")
	}

	env := nexusEnv()
	env[controlplane.EnvHarborRegistryHosts] = "harbor.example.com:8443/team"
	cfg = loadConfig(t, env)
	if got := hostStrings(handlerConfig(cfg).HarborRegistryHosts); got != "harbor.example.com:8443/team" {
		t.Errorf("Harbor registry hosts = %s, want BRIDGE_HARBOR_REGISTRY_HOSTS", got)
	}
}

func hostStrings[T interface{ String() string }](hosts []T) string {
	parts := make([]string, len(hosts))
	for i, h := range hosts {
		parts[i] = h.String()
	}
	return strings.Join(parts, ",")
}

// The data plane serves a Nexus Secret only when its user id parses back
// to the NexusAccess's identity, so the two functions main wires must
// accept exactly the user ids the control plane creates: every generation
// of the identity, truncated identity names included, and nothing of
// another cluster or ServiceAccount.
func TestNexusNaming_AcceptsTheControlPlanesUserIDs(t *testing.T) {
	cfg := &controlplane.Config{ClusterName: "prod"}
	identityName := nexusIdentityName(cfg)
	long := strings.Repeat("s", 200)
	for _, sa := range [][2]string{{"team-a", "web"}, {"team-a", long}, {strings.Repeat("n", 63), long}} {
		want, err := identityName(sa[0], sa[1])
		if err != nil {
			t.Fatalf("identity of %s/%s: %v", sa[0], sa[1][:10], err)
		}
		for range 3 {
			generation, err := nexus.NewGeneration()
			if err != nil {
				t.Fatal(err)
			}
			userID, err := nexus.UserID(cfg.ClusterName, sa[0], sa[1], generation)
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := nexusUserIdentity(userID); !ok || got != want {
				t.Errorf("nexusUserIdentity(%q) = %q, %v; want %q", userID, got, ok, want)
			}
		}
	}

	other, err := nexus.UserID("staging", "team-a", "web", "0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	mine, _ := identityName("team-a", "web")
	if got, ok := nexusUserIdentity(other); ok && got == mine {
		t.Errorf("a user of cluster staging maps to cluster prod's identity %q", mine)
	}
	for _, id := range []string{"admin", mine, mine + "_", mine + "_0123456789ABCDEF", "BRIDGE-PROD.team-a.web_0123456789abcdef"} {
		if got, ok := nexusUserIdentity(id); ok {
			t.Errorf("nexusUserIdentity(%q) = %q, true; want no identity", id, got)
		}
	}
	if _, err := identityName("Team-A", "web"); err == nil {
		t.Error("an identity outside the naming's domain was named")
	}
}

// A Harbor-only bridge's scheme does not know NexusAccess, so nothing in
// it can read one; with the backend it does.
func TestAddToScheme(t *testing.T) {
	gvk := nexusv1alpha1.GroupVersion.WithKind("NexusAccess")
	harbor := runtime.NewScheme()
	if err := addToScheme(harbor, &controlplane.Config{}); err != nil {
		t.Fatal(err)
	}
	if !harbor.Recognizes(harborv1alpha1.GroupVersion.WithKind("HarborAccess")) || harbor.Recognizes(gvk) {
		t.Errorf("Harbor-only scheme: HarborAccess %v, NexusAccess %v; want true, false",
			harbor.Recognizes(harborv1alpha1.GroupVersion.WithKind("HarborAccess")), harbor.Recognizes(gvk))
	}
	both := runtime.NewScheme()
	if err := addToScheme(both, &controlplane.Config{Nexus: &controlplane.NexusConfig{}}); err != nil {
		t.Fatal(err)
	}
	if !both.Recognizes(gvk) {
		t.Error("scheme with the Nexus backend does not know NexusAccess")
	}
}

// A Harbor-only bridge exports exactly the data-plane series it did
// before; the Nexus ones exist only with the backend.
func TestNewMetrics_NexusSeriesOnlyWithNexus(t *testing.T) {
	names := func(cfg *controlplane.Config) (string, bool) {
		reg := prometheus.NewRegistry()
		m := newMetrics(cfg, reg)
		mfs, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, mf := range mfs {
			out = append(out, mf.GetName())
		}
		return strings.Join(out, ","), m.Nexus != nil
	}
	harbor, harborNexus := names(&controlplane.Config{})
	if harborNexus || strings.Contains(harbor, "nexus") {
		t.Errorf("Harbor-only metrics: %s (Nexus collectors %v)", harbor, harborNexus)
	}
	both, bothNexus := names(&controlplane.Config{Nexus: &controlplane.NexusConfig{}})
	for _, want := range []string{"bridge_nexus_credential_issuances_total", "bridge_credential_issuances_total"} {
		if !bothNexus || !strings.Contains(both, want) {
			t.Errorf("metrics with Nexus lack %s: %s", want, both)
		}
	}
}

// The manager caches NexusAccess, with the HarborAccess selector, only
// with the backend (ADR-0026, ADR-0033).
func TestManagerOptions_CachesNexusAccessOnlyWithNexus(t *testing.T) {
	cached := func(cfg *controlplane.Config) bool {
		for obj := range managerOptions(cfg, false).Cache.ByObject {
			if _, ok := obj.(*nexusv1alpha1.NexusAccess); ok {
				return true
			}
		}
		return false
	}
	if cached(loadConfig(t, harborOnlyEnv())) {
		t.Error("Harbor-only manager caches NexusAccess")
	}
	if !cached(loadConfig(t, nexusEnv())) {
		t.Error("manager with the Nexus backend does not cache NexusAccess")
	}
}
