// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	nexusv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/nexus/v1alpha1"
)

// harborOnlyEnv is a complete configuration without the Nexus backend.
func harborOnlyEnv(t *testing.T) {
	t.Helper()
	clearAllEnv(t)
	setEnv(t, map[string]string{
		EnvClusterName:    "prod",
		EnvNamespace:      "harbor-bridge-system",
		EnvOIDCIssuer:     "https://kubernetes.default.svc",
		EnvHarborURL:      "https://harbor.example.com:8443",
		EnvHarborAdminDir: "/var/run/secrets/harbor-admin",
		EnvAudience:       "harbor-bridge-prod",
	})
}

// nexusEnv adds a complete Nexus backend to harborOnlyEnv.
func nexusEnv(t *testing.T) {
	t.Helper()
	harborOnlyEnv(t)
	setEnv(t, map[string]string{
		EnvNexusURL:           "https://nexus.example.com/nexus",
		EnvNexusAdminDir:      "/var/run/secrets/nexus-admin",
		EnvNexusRegistryHosts: "nexus.example.com:5000, nexus.example.com/docker",
	})
}

// A configuration without BRIDGE_NEXUS_URL is the bridge as it was: no
// Nexus backend, and the same startup log keys.
func TestLoadFromEnv_HarborOnlyHasNoNexusBackend(t *testing.T) {
	harborOnlyEnv(t)
	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Nexus != nil {
		t.Errorf("Nexus = %+v, want nil", cfg.Nexus)
	}
	if len(cfg.HarborRegistryHosts) != 1 || cfg.HarborRegistryHosts[0].String() != "harbor.example.com:8443" {
		t.Errorf("HarborRegistryHosts = %v, want the host of the Harbor URL", cfg.HarborRegistryHosts)
	}
	for k := range cfg.Sanitized() {
		if strings.HasPrefix(k, "BRIDGE_NEXUS_") || k == EnvHarborRegistryHosts {
			t.Errorf("Sanitized() reports %s without the Nexus backend", k)
		}
	}
}

// An unusual Harbor host that is no registry host must not fail a
// Harbor-only bridge: without a second backend nothing routes by it.
func TestLoadFromEnv_UnusualHarborHostOnlyMattersWithNexus(t *testing.T) {
	harborOnlyEnv(t)
	t.Setenv(EnvHarborURL, "https://harbor_core.internal")
	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("Harbor-only: %v", err)
	}
	if len(cfg.HarborRegistryHosts) != 0 {
		t.Errorf("HarborRegistryHosts = %v", cfg.HarborRegistryHosts)
	}
	nexusEnv(t)
	t.Setenv(EnvHarborURL, "https://harbor_core.internal")
	if _, err := LoadFromEnv(); err == nil || !strings.Contains(err.Error(), EnvHarborRegistryHosts) {
		t.Errorf("with Nexus: err = %v, want a hint to set %s", err, EnvHarborRegistryHosts)
	}
	t.Setenv(EnvHarborRegistryHosts, "harbor.example.com")
	if _, err := LoadFromEnv(); err != nil {
		t.Errorf("with explicit Harbor registry hosts: %v", err)
	}
}

func TestLoadFromEnv_Nexus(t *testing.T) {
	nexusEnv(t)
	t.Setenv(EnvNexusCAFile, "/etc/nexus-ca/ca.crt")
	t.Setenv(EnvHarborRegistryHosts, "harbor.example.com,registry.example.com/harbor")
	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	n := cfg.Nexus
	if n == nil {
		t.Fatal("Nexus backend not enabled")
	}
	if n.URL.String() != "https://nexus.example.com/nexus" || n.AdminDir != "/var/run/secrets/nexus-admin" ||
		n.CAFile != "/etc/nexus-ca/ca.crt" || n.AllowHTTP || n.RateLimitBackoff != DefaultNexusRateLimitBackoff {
		t.Errorf("Nexus = %+v", n)
	}
	if got := hostList(n.RegistryHosts); got != "nexus.example.com:5000,nexus.example.com/docker" {
		t.Errorf("Nexus registry hosts = %q", got)
	}
	if got := hostList(cfg.HarborRegistryHosts); got != "harbor.example.com,registry.example.com/harbor" {
		t.Errorf("Harbor registry hosts = %q", got)
	}
	m := cfg.Sanitized()
	for k, want := range map[string]string{
		EnvNexusURL:              "https://nexus.example.com/nexus",
		EnvNexusAdminDir:         "/var/run/secrets/nexus-admin",
		EnvNexusCAFile:           "/etc/nexus-ca/ca.crt",
		EnvNexusAllowHTTP:        "false",
		EnvNexusRegistryHosts:    "nexus.example.com:5000,nexus.example.com/docker",
		EnvNexusRateLimitBackoff: "15m0s",
		EnvHarborRegistryHosts:   "harbor.example.com,registry.example.com/harbor",
	} {
		if m[k] != want {
			t.Errorf("Sanitized()[%s] = %q, want %q", k, m[k], want)
		}
	}
}

func TestLoadFromEnv_NexusValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want []string
	}{
		"admin dir and registry hosts required": {
			env:  map[string]string{EnvNexusAdminDir: "", EnvNexusRegistryHosts: ""},
			want: []string{EnvNexusAdminDir + " is required", EnvNexusRegistryHosts + " is required"},
		},
		"plain http refused": {
			env:  map[string]string{EnvNexusURL: "http://nexus.example.com"},
			want: []string{"plain http", EnvNexusAllowHTTP},
		},
		"credentials in the URL refused": {
			env:  map[string]string{EnvNexusURL: "https://admin:s3cret@nexus.example.com"},
			want: []string{"must not contain credentials"},
		},
		"query refused": {
			env:  map[string]string{EnvNexusURL: "https://nexus.example.com/?x=1"},
			want: []string{"query or fragment"},
		},
		"host shared with Harbor": {
			env:  map[string]string{EnvNexusRegistryHosts: "harbor.example.com:8443/nexus"},
			want: []string{"harbor.example.com:8443 is both Harbor's"},
		},
		"invalid registry host": {
			env:  map[string]string{EnvNexusRegistryHosts: "https://nexus.example.com"},
			want: []string{EnvNexusRegistryHosts, "scheme"},
		},
		"invalid Harbor registry host": {
			env:  map[string]string{EnvHarborRegistryHosts: "harbor.example.com:0"},
			want: []string{EnvHarborRegistryHosts, "port"},
		},
		"zero backoff": {
			env:  map[string]string{EnvNexusRateLimitBackoff: "0s"},
			want: []string{EnvNexusRateLimitBackoff, "positive"},
		},
		"unparsable backoff": {
			env:  map[string]string{EnvNexusRateLimitBackoff: "15"},
			want: []string{EnvNexusRateLimitBackoff, "duration"},
		},
		"unparsable allow-http": {
			env:  map[string]string{EnvNexusAllowHTTP: "maybe"},
			want: []string{EnvNexusAllowHTTP, "boolean"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			nexusEnv(t)
			setEnv(t, tc.env)
			_, err := LoadFromEnv()
			if err == nil {
				t.Fatal("configuration accepted")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
			if strings.Contains(err.Error(), "s3cret") {
				t.Errorf("error repeats the URL's password: %v", err)
			}
		})
	}
}

func TestLoadFromEnv_NexusOptions(t *testing.T) {
	nexusEnv(t)
	t.Setenv(EnvNexusURL, "http://nexus.nexus.svc:8081")
	t.Setenv(EnvNexusAllowHTTP, "true")
	t.Setenv(EnvNexusRateLimitBackoff, "30m")
	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Nexus.AllowHTTP || cfg.Nexus.RateLimitBackoff != 30*time.Minute || cfg.Nexus.URL.Scheme != "http" {
		t.Errorf("Nexus = %+v", cfg.Nexus)
	}
}

// Half a Nexus configuration fails startup instead of silently serving
// Harbor only.
func TestLoadFromEnv_NexusSettingsWithoutURL(t *testing.T) {
	harborOnlyEnv(t)
	t.Setenv(EnvNexusRegistryHosts, "nexus.example.com")
	t.Setenv(EnvNexusAdminDir, "/x")
	_, err := LoadFromEnv()
	if err == nil || !strings.Contains(err.Error(), EnvNexusURL) ||
		!strings.Contains(err.Error(), EnvNexusRegistryHosts) || !strings.Contains(err.Error(), EnvNexusAdminDir) {
		t.Errorf("err = %v, want the stray settings and %s named", err, EnvNexusURL)
	}
}

func TestConfig_NexusFinalizers(t *testing.T) {
	plain := &Config{}
	if plain.NexusFinalizer() != NexusFinalizerName || !slices.Equal(plain.NexusReleasedFinalizers(), []string{NexusFinalizerName}) {
		t.Errorf("without a selector: %q, %q", plain.NexusFinalizer(), plain.NexusReleasedFinalizers())
	}
	sel, err := labels.Parse("bridge=eu")
	if err != nil {
		t.Fatal(err)
	}
	selective := &Config{HarborAccessSelector: sel, Instance: "eu"}
	if got := selective.NexusFinalizer(); got != "nexus.aetherize.io/user-eu" {
		t.Errorf("with a selector: %q", got)
	}
	if got := selective.NexusReleasedFinalizers(); !slices.Equal(got, []string{"nexus.aetherize.io/user-eu", NexusFinalizerName}) {
		t.Errorf("released with a selector: %q", got)
	}
	unselected := &Config{Instance: "eu"}
	if got := unselected.NexusReleasedFinalizers(); !slices.Equal(got, []string{NexusFinalizerName, "nexus.aetherize.io/user-eu"}) {
		t.Errorf("released after the selector was removed: %q", got)
	}
	// The Harbor finalizers are unchanged.
	if selective.Finalizer() != FinalizerName+"-eu" {
		t.Errorf("Harbor finalizer = %q", selective.Finalizer())
	}

	// The selector selects NexusAccess objects as it selects HarborAccess
	// objects (ADR-0026).
	in := &nexusv1alpha1.NexusAccess{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"bridge": "eu"}}}
	out := &nexusv1alpha1.NexusAccess{}
	if !selective.Selects(in) || selective.Selects(out) || !plain.Selects(out) {
		t.Error("Selects does not apply the selector to NexusAccess objects")
	}
}

func TestNexusAdminCredsReader(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "username"), "nexus-bridge-7f3a")
	mustWrite(t, filepath.Join(dir, "password"), "pw")
	n := &NexusConfig{AdminDir: dir}
	user, pass, err := NewNexusAdminCredsReader(n, logr.Discard()).Read()
	if err != nil || user != "nexus-bridge-7f3a" || pass != "pw" {
		t.Errorf("Read = %q, %q, %v", user, pass, err)
	}
	if creds, err := n.LoadAdminCreds(); err != nil || creds.Username != user {
		t.Errorf("LoadAdminCreds = %+v, %v", creds, err)
	}
}
