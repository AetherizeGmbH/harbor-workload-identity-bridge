// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setEnv saves the current env, applies the supplied overrides, and returns
// a function the test can defer to restore. Unset values (empty string)
// remove the variable.
func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

// clearAllEnv removes every bridge env var so individual tests start from a
// known empty baseline.
func clearAllEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		EnvClusterName, EnvNamespace, EnvOIDCIssuer, EnvHarborURL, EnvHarborAdminDir,
		EnvForceLocalValidation, EnvLogLevel,
	} {
		t.Setenv(k, "")
		// t.Setenv with empty string doesn't actually unset on every Go
		// version; explicitly unset to be safe.
		_ = os.Unsetenv(k)
	}
}

func TestLoadFromEnv_HappyPath(t *testing.T) {
	clearAllEnv(t)
	setEnv(t, map[string]string{
		EnvClusterName:          "prod-eu-west",
		EnvNamespace:            "harbor-bridge-system",
		EnvOIDCIssuer:           "https://kubernetes.default.svc",
		EnvHarborURL:            "https://harbor.example.com",
		EnvHarborAdminDir:       "/var/run/secrets/harbor-admin",
		EnvForceLocalValidation: "true",
		EnvLogLevel:             "debug",
	})

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if cfg.ClusterName != "prod-eu-west" {
		t.Errorf("ClusterName = %q", cfg.ClusterName)
	}
	if cfg.Namespace != "harbor-bridge-system" {
		t.Errorf("Namespace = %q", cfg.Namespace)
	}
	if cfg.OIDCIssuer.String() != "https://kubernetes.default.svc" {
		t.Errorf("OIDCIssuer = %s", cfg.OIDCIssuer)
	}
	if !cfg.ForceLocalValidation {
		t.Errorf("ForceLocalValidation expected true")
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q", cfg.LogLevel)
	}
}

func TestLoadFromEnv_AppliesDefaults(t *testing.T) {
	clearAllEnv(t)
	setEnv(t, map[string]string{
		EnvClusterName:    "prod",
		EnvNamespace:      "harbor-bridge-system",
		EnvOIDCIssuer:     "https://kubernetes.default.svc",
		EnvHarborURL:      "https://harbor.example.com",
		EnvHarborAdminDir: "/var/run/secrets/harbor-admin",
	})

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.ForceLocalValidation {
		t.Errorf("ForceLocalValidation default expected true; got false")
	}
	if cfg.LogLevel != defaultLogLevel {
		t.Errorf("LogLevel default expected %q; got %q", defaultLogLevel, cfg.LogLevel)
	}
}

func TestLoadFromEnv_ValidationErrors(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		mustHave string
	}{
		{
			name:     "missing cluster name",
			env:      map[string]string{},
			mustHave: EnvClusterName + " is required",
		},
		{
			name: "cluster name too long",
			env: map[string]string{
				EnvClusterName: strings.Repeat("a", clusterNameMaxLen+1),
			},
			mustHave: "exceeds 63-char DNS-label limit",
		},
		{
			name: "cluster name invalid chars",
			env: map[string]string{
				EnvClusterName: "Prod_EU",
			},
			mustHave: "must match",
		},
		{
			name: "cluster name leading hyphen",
			env: map[string]string{
				EnvClusterName: "-prod",
			},
			mustHave: "must match",
		},
		{
			name: "issuer with no scheme",
			env: map[string]string{
				EnvClusterName: "prod",
				EnvNamespace:   "ns",
				EnvOIDCIssuer:  "kubernetes.default.svc",
			},
			mustHave: EnvOIDCIssuer,
		},
		{
			name: "issuer with wrong scheme",
			env: map[string]string{
				EnvClusterName: "prod",
				EnvNamespace:   "ns",
				EnvOIDCIssuer:  "ftp://kubernetes.default.svc",
			},
			mustHave: "must use http or https",
		},
		{
			name: "invalid bool for forceLocalValidation",
			env: map[string]string{
				EnvClusterName:          "prod",
				EnvNamespace:            "ns",
				EnvOIDCIssuer:           "https://k",
				EnvHarborURL:            "https://h",
				EnvHarborAdminDir:       "/d",
				EnvForceLocalValidation: "maybe",
			},
			mustHave: "must be a boolean",
		},
		{
			name: "unknown log level",
			env: map[string]string{
				EnvClusterName:    "prod",
				EnvNamespace:      "ns",
				EnvOIDCIssuer:     "https://k",
				EnvHarborURL:      "https://h",
				EnvHarborAdminDir: "/d",
				EnvLogLevel:       "trace",
			},
			mustHave: "must be one of",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearAllEnv(t)
			setEnv(t, tt.env)
			_, err := LoadFromEnv()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.mustHave) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.mustHave)
			}
		})
	}
}

func TestLoadFromEnv_ReportsAllErrorsAtOnce(t *testing.T) {
	clearAllEnv(t)
	// Every var invalid in a distinct way so we can confirm all of them are
	// reported in one error (errors.Join + %w).
	setEnv(t, map[string]string{
		EnvClusterName:          "INVALID",
		EnvNamespace:            "INVALID",
		EnvOIDCIssuer:           "not-a-url",
		EnvHarborURL:            "",
		EnvHarborAdminDir:       "",
		EnvForceLocalValidation: "perhaps",
		EnvLogLevel:             "loud",
	})

	_, err := LoadFromEnv()
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, fragment := range []string{
		EnvClusterName, EnvNamespace, EnvOIDCIssuer, EnvHarborURL, EnvHarborAdminDir,
		EnvForceLocalValidation, EnvLogLevel,
	} {
		if !strings.Contains(msg, fragment) {
			t.Errorf("aggregated error missing reference to %s: %s", fragment, msg)
		}
	}
}

func TestSanitized_DoesNotIncludeCredentials(t *testing.T) {
	clearAllEnv(t)
	setEnv(t, map[string]string{
		EnvClusterName:    "prod",
		EnvNamespace:      "harbor-bridge-system",
		EnvOIDCIssuer:     "https://kubernetes.default.svc",
		EnvHarborURL:      "https://harbor.example.com",
		EnvHarborAdminDir: "/var/run/secrets/harbor-admin",
	})
	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Sanitized()
	for k, v := range m {
		// The only secret-adjacent thing we expose is the mount path, by
		// design — credential contents are loaded separately via LoadAdminCreds.
		if strings.Contains(strings.ToLower(k), "password") {
			t.Errorf("sanitized map exposes a password-like key %q=%q", k, v)
		}
	}
}

func TestLoadAdminCreds_HappyPath(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "username"), "harbor-system-robot")
	mustWrite(t, filepath.Join(dir, "password"), "s3cret!\n")
	cfg := &Config{HarborAdminDir: dir}

	creds, err := cfg.LoadAdminCreds()
	if err != nil {
		t.Fatal(err)
	}
	if creds.Username != "harbor-system-robot" {
		t.Errorf("Username = %q", creds.Username)
	}
	if creds.Password != "s3cret!" {
		t.Errorf("Password = %q (trailing whitespace must be trimmed)", creds.Password)
	}
}

func TestLoadAdminCreds_MissingFile(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "username"), "u")
	// password file deliberately missing
	cfg := &Config{HarborAdminDir: dir}
	_, err := cfg.LoadAdminCreds()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "password") {
		t.Errorf("error should reference the missing file path: %v", err)
	}
}

func TestLoadAdminCreds_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "username"), "u")
	mustWrite(t, filepath.Join(dir, "password"), "")
	cfg := &Config{HarborAdminDir: dir}
	_, err := cfg.LoadAdminCreds()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error should explain emptiness: %v", err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
