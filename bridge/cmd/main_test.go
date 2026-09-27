// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"

	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/dataplane"
)

// TestValidatorConfig_TokenPolicy follows the token policy from the env
// vars the chart renders to the validator's config (ADR-0028). The two
// pod-binding settings mean opposite things; an inverted mapping would
// accept tokens bound to no pod by default, and nothing else would notice:
// kubelet's tokens are pod-bound either way.
func TestValidatorConfig_TokenPolicy(t *testing.T) {
	const issuer = "https://kubernetes.default.svc"
	tests := []struct {
		name        string
		maxLifetime string // "" = unset
		requirePod  string // "" = unset
		jwksURL     string // "" = unset
		want        dataplane.Config
	}{
		{
			name: "defaults require pod binding and cap at 1h",
			want: dataplane.Config{Issuer: issuer, MaxTokenLifetime: time.Hour},
		},
		{
			name:       "pod binding required explicitly",
			requirePod: "true",
			want:       dataplane.Config{Issuer: issuer, MaxTokenLifetime: time.Hour},
		},
		{
			name:       "pod binding turned off",
			requirePod: "false",
			want:       dataplane.Config{Issuer: issuer, MaxTokenLifetime: time.Hour, AllowNonPodBoundTokens: true},
		},
		{
			name:        "custom maximum lifetime",
			maxLifetime: "90m",
			want:        dataplane.Config{Issuer: issuer, MaxTokenLifetime: 90 * time.Minute},
		},
		{
			name:    "JWKS URL override",
			jwksURL: "https://127.0.0.1:8001/openid/v1/jwks",
			want: dataplane.Config{
				Issuer:           issuer,
				JWKSURL:          "https://127.0.0.1:8001/openid/v1/jwks",
				MaxTokenLifetime: time.Hour,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// An empty value counts as unset for every variable below.
			for k, v := range map[string]string{
				controlplane.EnvClusterName:          "prod",
				controlplane.EnvNamespace:            "harbor-bridge-system",
				controlplane.EnvOIDCIssuer:           issuer,
				controlplane.EnvHarborURL:            "https://harbor.example.com",
				controlplane.EnvHarborAdminDir:       "/var/run/secrets/harbor-admin",
				controlplane.EnvAudience:             "harbor-bridge-prod",
				controlplane.EnvOIDCJWKSURL:          tt.jwksURL,
				controlplane.EnvTokenMaxLifetime:     tt.maxLifetime,
				controlplane.EnvRequirePodBoundToken: tt.requirePod,
			} {
				t.Setenv(k, v)
			}
			cfg, err := controlplane.LoadFromEnv()
			if err != nil {
				t.Fatalf("LoadFromEnv: %v", err)
			}
			if got := validatorConfig(cfg); got != tt.want {
				t.Errorf("validatorConfig() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestLogWeakTokenValidation(t *testing.T) {
	const (
		unbound = "tokens not bound to a pod are accepted"
		tooLow  = "below the lifetime of kubelet's tokens"
	)
	tests := []struct {
		name string
		vc   dataplane.Config
		want []string // one fragment per expected line, in order
	}{
		{name: "defaults", vc: dataplane.Config{MaxTokenLifetime: time.Hour}},
		{name: "longer cap", vc: dataplane.Config{MaxTokenLifetime: 2 * time.Hour}},
		{
			name: "unbound tokens accepted",
			vc:   dataplane.Config{MaxTokenLifetime: time.Hour, AllowNonPodBoundTokens: true},
			want: []string{unbound},
		},
		{
			name: "cap below kubelet's tokens",
			vc:   dataplane.Config{MaxTokenLifetime: time.Hour - time.Second},
			want: []string{tooLow},
		},
		{
			name: "both",
			vc:   dataplane.Config{MaxTokenLifetime: 30 * time.Minute, AllowNonPodBoundTokens: true},
			want: []string{unbound, tooLow},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var lines []string
			log := funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{})
			logWeakTokenValidation(log, tt.vc)
			if len(lines) != len(tt.want) {
				t.Fatalf("logged %d lines, want %d: %q", len(lines), len(tt.want), lines)
			}
			for i, fragment := range tt.want {
				if !strings.Contains(lines[i], fragment) {
					t.Errorf("line %d = %q, want it to contain %q", i, lines[i], fragment)
				}
			}
		})
	}
}
