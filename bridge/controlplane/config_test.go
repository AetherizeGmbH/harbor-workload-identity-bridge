// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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
		EnvForceLocalValidation, EnvLogLevel, EnvAudience, EnvHarborAccessSelector, EnvInstance,
		EnvTokenMaxLifetime, EnvRequirePodBoundToken,
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
		EnvAudience:             "harbor-bridge-prod",
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
		EnvAudience:       "harbor-bridge-prod",
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
	// ADR-0028: kubelet's tokens last exactly one hour and are pod-bound.
	if cfg.TokenMaxLifetime != time.Hour {
		t.Errorf("TokenMaxLifetime default expected 1h; got %s", cfg.TokenMaxLifetime)
	}
	if !cfg.RequirePodBoundToken {
		t.Errorf("RequirePodBoundToken default expected true; got false")
	}
}

func TestLoadFromEnv_TokenPolicy(t *testing.T) {
	base := map[string]string{
		EnvClusterName:    "prod",
		EnvNamespace:      "harbor-bridge-system",
		EnvOIDCIssuer:     "https://kubernetes.default.svc",
		EnvHarborURL:      "https://harbor.example.com",
		EnvHarborAdminDir: "/var/run/secrets/harbor-admin",
		EnvAudience:       "harbor-bridge-prod",
	}
	tests := []struct {
		name        string
		maxLifetime string // "" = unset
		requirePod  string // "" = unset
		wantMax     time.Duration
		wantPod     bool
		mustHave    string // non-empty = expected error fragment
	}{
		{name: "defaults", wantMax: time.Hour, wantPod: true},
		{name: "custom", maxLifetime: "90m", requirePod: "false", wantMax: 90 * time.Minute, wantPod: false},
		{name: "explicit true", maxLifetime: "1h", requirePod: "true", wantMax: time.Hour, wantPod: true},
		{name: "not a duration", maxLifetime: "forever", mustHave: EnvTokenMaxLifetime + ` "forever" must be a duration`},
		{name: "bare number", maxLifetime: "3600", mustHave: EnvTokenMaxLifetime + ` "3600" must be a duration`},
		{name: "zero", maxLifetime: "0s", mustHave: EnvTokenMaxLifetime + ` "0s" must be positive`},
		{name: "negative", maxLifetime: "-1h", mustHave: EnvTokenMaxLifetime + ` "-1h" must be positive`},
		{name: "not a boolean", requirePod: "maybe", mustHave: EnvRequirePodBoundToken + ` "maybe" must be a boolean`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearAllEnv(t)
			setEnv(t, base)
			if tt.maxLifetime != "" {
				setEnv(t, map[string]string{EnvTokenMaxLifetime: tt.maxLifetime})
			}
			if tt.requirePod != "" {
				setEnv(t, map[string]string{EnvRequirePodBoundToken: tt.requirePod})
			}
			cfg, err := LoadFromEnv()
			if tt.mustHave != "" {
				if err == nil || !strings.Contains(err.Error(), tt.mustHave) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.mustHave)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.TokenMaxLifetime != tt.wantMax || cfg.RequirePodBoundToken != tt.wantPod {
				t.Errorf("TokenMaxLifetime=%s RequirePodBoundToken=%v, want %s %v",
					cfg.TokenMaxLifetime, cfg.RequirePodBoundToken, tt.wantMax, tt.wantPod)
			}
			m := cfg.Sanitized()
			if m[EnvTokenMaxLifetime] != tt.wantMax.String() || m[EnvRequirePodBoundToken] != strconv.FormatBool(tt.wantPod) {
				t.Errorf("Sanitized() does not report the token policy: %v", m)
			}
		})
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
				EnvAudience:             "harbor-bridge-prod",
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
				EnvAudience:       "harbor-bridge-prod",
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
		EnvAudience:             "harbor-bridge-prod",
		EnvForceLocalValidation: "perhaps",
		EnvLogLevel:             "loud",
		EnvTokenMaxLifetime:     "0",
		EnvRequirePodBoundToken: "sometimes",
	})

	_, err := LoadFromEnv()
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, fragment := range []string{
		EnvClusterName, EnvNamespace, EnvOIDCIssuer, EnvHarborURL, EnvHarborAdminDir,
		EnvForceLocalValidation, EnvLogLevel, EnvTokenMaxLifetime, EnvRequirePodBoundToken,
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
		EnvAudience:       "harbor-bridge-prod",
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

func TestLoadFromEnv_HarborHTTPNeedsOptIn(t *testing.T) {
	base := map[string]string{
		EnvClusterName:    "prod",
		EnvNamespace:      "harbor-bridge-system",
		EnvOIDCIssuer:     "https://kubernetes.default.svc",
		EnvHarborURL:      "http://harbor-core.harbor.svc",
		EnvHarborAdminDir: "/var/run/secrets/harbor-admin",
		EnvAudience:       "harbor-bridge-prod",
	}
	clearAllEnv(t)
	_ = os.Unsetenv(EnvHarborAllowHTTP)
	setEnv(t, base)
	if _, err := LoadFromEnv(); err == nil || !strings.Contains(err.Error(), "plain http") {
		t.Fatalf("err = %v, want the plain-http refusal", err)
	}
	setEnv(t, map[string]string{EnvHarborAllowHTTP: "true"})
	cfg, err := LoadFromEnv()
	if err != nil || !cfg.HarborAllowHTTP {
		t.Fatalf("with opt-in: cfg=%+v err=%v", cfg, err)
	}
}

// validURLEnv is a complete, valid configuration for the URL tests below.
func validURLEnv() map[string]string {
	return map[string]string{
		EnvClusterName:    "prod",
		EnvNamespace:      "harbor-bridge-system",
		EnvOIDCIssuer:     "https://kubernetes.default.svc",
		EnvHarborURL:      "https://harbor.example.com",
		EnvHarborAdminDir: "/var/run/secrets/harbor-admin",
		EnvAudience:       "harbor-bridge-prod",
		EnvOIDCJWKSURL:    "",
	}
}

// The Harbor client ignores user:password@ in BRIDGE_HARBOR_URL (it
// authenticates with BRIDGE_HARBOR_ADMIN_DIR), and an issuer carrying one
// can never match a token's iss claim: both are refused at startup, and
// the refusal must not repeat the password.
func TestLoadFromEnv_RejectsCredentialsInURLs(t *testing.T) {
	for _, tc := range []struct{ env, value, mustHave string }{
		{EnvHarborURL, "https://admin:s3cret@harbor.example.com", EnvHarborAdminDir},
		{EnvOIDCIssuer, "https://admin:s3cret@kubernetes.default.svc", "iss claim"},
	} {
		t.Run(tc.env, func(t *testing.T) {
			clearAllEnv(t)
			env := validURLEnv()
			env[tc.env] = tc.value
			setEnv(t, env)
			_, err := LoadFromEnv()
			if err == nil {
				t.Fatalf("%s with credentials accepted", tc.env)
			}
			if !strings.Contains(err.Error(), tc.env) || !strings.Contains(err.Error(), "credentials") || !strings.Contains(err.Error(), tc.mustHave) {
				t.Errorf("error %q does not explain the refusal of %s", err, tc.env)
			}
			if strings.Contains(err.Error(), "s3cret") {
				t.Errorf("error repeats the password: %q", err)
			}
		})
	}
}

// Every refusal of a URL setting must leave out a credential the value
// carries, including values that fail to parse or carry no scheme (then
// the user name parses as the scheme and the password sits in the opaque
// part, which URL.Redacted keeps), and values where a '/', '?' or '#'
// inside the password ends the host part early (the parser then quotes
// the password's start as a port, or accepts an all-digit start as one
// and keeps the rest in the path).
func TestLoadFromEnv_URLErrorsDoNotRepeatCredentials(t *testing.T) {
	bad := []struct {
		value   string
		secrets []string // no part of the value that may hold a credential
	}{
		{"ftp://admin:s3cret@harbor.example.com", []string{"s3cret"}},        // wrong scheme
		{"admin:s3cret@harbor.example.com", []string{"s3cret"}},              // no scheme: opaque URL
		{"tok3n:s3cret@harbor.example.com", []string{"tok3n", "s3cret"}},     // the user name parses as the scheme
		{"https://admin:s3cret@", []string{"s3cret"}},                        // no host
		{"https://admin:s3cret@harbor.example.com:port", []string{"s3cret"}}, // parse error after the userinfo
		{"https://admin:s3cr/et@harbor.example.com", []string{"s3cr", "et@"}},
		{"https://admin:s3cr?et@harbor.example.com", []string{"s3cr", "et@"}},
		{"https://admin:s3cr#et@harbor.example.com", []string{"s3cr", "et@"}},
		{"https://admin:9173/s3cret@harbor.example.com", []string{"9173", "s3cret"}}, // parses: host admin:9173
		{"http://admin:s3cret@harbor.example.com", []string{"s3cret"}},               // plain http without opt-in
	}
	for _, name := range []string{EnvHarborURL, EnvOIDCIssuer, EnvOIDCJWKSURL} {
		for _, tc := range bad {
			t.Run(name+"="+tc.value, func(t *testing.T) {
				clearAllEnv(t)
				_ = os.Unsetenv(EnvHarborAllowHTTP)
				env := validURLEnv()
				env[name] = tc.value
				setEnv(t, env)
				cfg, err := LoadFromEnv()
				if err == nil {
					// http:// is valid for the OIDC settings, and the JWKS
					// URL may carry credentials; they must then at least
					// not be logged.
					if name != EnvOIDCJWKSURL {
						t.Fatalf("%s=%q accepted", name, tc.value)
					}
					for k, v := range cfg.Sanitized() {
						for _, secret := range tc.secrets {
							if strings.Contains(v, secret) {
								t.Errorf("Sanitized()[%s] = %q repeats %q", k, v, secret)
							}
						}
					}
					return
				}
				for _, secret := range tc.secrets {
					if strings.Contains(err.Error(), secret) {
						t.Errorf("error repeats %q: %q", secret, err)
					}
				}
			})
		}
	}
}

// BRIDGE_OIDC_JWKS_URL may carry user:password@ (net/http sends it as
// Basic auth to the JWKS endpoint), so it is accepted and kept, but the
// startup log shows it redacted.
func TestSanitized_RedactsURLCredentials(t *testing.T) {
	clearAllEnv(t)
	env := validURLEnv()
	env[EnvOIDCJWKSURL] = "https://jwks:s3cret@jwks.example.com/keys"
	setEnv(t, env)
	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if pw, _ := cfg.OIDCJWKSURL.User.Password(); pw != "s3cret" {
		t.Fatalf("JWKS URL lost its credentials: %s", cfg.OIDCJWKSURL.Redacted())
	}
	got := cfg.Sanitized()
	for k, v := range got {
		if strings.Contains(v, "s3cret") {
			t.Errorf("Sanitized()[%s] = %q repeats the password", k, v)
		}
	}
	if got[EnvOIDCJWKSURL] != "https://xxxxx@jwks.example.com/keys" {
		t.Errorf("Sanitized()[%s] = %q, want the redacted URL", EnvOIDCJWKSURL, got[EnvOIDCJWKSURL])
	}
}

// An '@' written as %40 in the path, query or fragment cannot stem from a
// user:password@ part and stays allowed.
func TestLoadFromEnv_AllowsPercentEncodedAtAfterHost(t *testing.T) {
	clearAllEnv(t)
	env := validURLEnv()
	env[EnvOIDCJWKSURL] = "https://jwks.example.com/keys%40v1?kid=a%40b#x%40y"
	setEnv(t, env)
	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("percent-encoded '@' refused: %v", err)
	}
	if got := cfg.OIDCJWKSURL.String(); got != env[EnvOIDCJWKSURL] {
		t.Errorf("JWKS URL = %q, want %q unchanged", got, env[EnvOIDCJWKSURL])
	}
}

// A credential given as the user name alone (sent as Basic auth
// "TOKEN:") must be redacted too: URL.Redacted hides only a password.
func TestSanitized_RedactsUserOnlyCredential(t *testing.T) {
	clearAllEnv(t)
	env := validURLEnv()
	env[EnvOIDCJWKSURL] = "https://TOKEN123@jwks.example.com/keys"
	setEnv(t, env)
	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OIDCJWKSURL.User.Username() != "TOKEN123" {
		t.Fatalf("JWKS URL lost its credential: %s", cfg.OIDCJWKSURL.Redacted())
	}
	got := cfg.Sanitized()
	if got[EnvOIDCJWKSURL] != "https://xxxxx@jwks.example.com/keys" {
		t.Errorf("Sanitized()[%s] = %q, want the user name redacted", EnvOIDCJWKSURL, got[EnvOIDCJWKSURL])
	}
}
