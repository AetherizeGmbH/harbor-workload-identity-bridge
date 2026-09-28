// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"strings"
	"testing"
)

// TestRun_MergeKeepsTheCloudConfigsMode: a node that keeps its
// credential-provider config at 0600 (its providers' env may carry
// secrets) got it back world-readable, with a world-readable .bak, and
// widened again on every later pass.
func TestRun_MergeKeepsTheCloudConfigsMode(t *testing.T) {
	env := newTestEnv(t, modeAuto, []string{
		"/usr/bin/kubelet",
		"--image-credential-provider-bin-dir=/cloud/bin",
		"--image-credential-provider-config=/cloud/config.yaml",
	})
	writeCloudConfig(t, env, "/cloud/bin", "/cloud/config.yaml", gkeConfig)
	cfgPath := env.cfg.hostPath("/cloud/config.yaml")
	if err := os.Chmod(cfgPath, 0o600); err != nil {
		t.Fatal(err)
	}
	for pass := 1; pass <= 2; pass++ {
		if err := run(env.cfg); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{cfgPath, cfgPath + ".bak"} {
			if m := modeOf(t, p); m != 0o600 {
				t.Fatalf("pass %d: %s has mode %o, want 0600", pass, p, m)
			}
		}
	}
	if !strings.Contains(env.hostFile(t, "/cloud/config.yaml"), defaultProviderName) {
		t.Fatal("entry not merged")
	}
}

func TestRun_PatchKeepsTheEnvironmentFilesMode(t *testing.T) {
	env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	writeHostFile(t, env, defaultKubeletPath, "KUBELET_EXTRA_ARGS=--max-pods=42\n")
	envPath := env.cfg.hostPath(defaultKubeletPath)
	if err := os.Chmod(envPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{envPath, envPath + ".bak"} {
		if m := modeOf(t, p); m != 0o600 {
			t.Fatalf("%s has mode %o, want 0600", p, m)
		}
	}
	// The chart's own config keeps the chart's mode.
	if m := modeOf(t, env.cfg.hostPath(env.cfg.ownConfigPath())); m != 0o644 {
		t.Fatalf("chart-owned config mode %o, want 0644", m)
	}
}

// TestRun_MergeRollbackRestoresTheMode: the rollback puts the cloud's
// config back as it was, permissions included (ADR-0033).
func TestRun_MergeRollbackRestoresTheMode(t *testing.T) {
	env := newTestEnv(t, modeAuto, []string{
		"/usr/bin/kubelet",
		"--image-credential-provider-bin-dir=/cloud/bin",
		"--image-credential-provider-config=/cloud/config.json",
	})
	writeCloudConfig(t, env, "/cloud/bin", "/cloud/config.json", eksConfig)
	cfgPath := env.cfg.hostPath("/cloud/config.json")
	if err := os.Chmod(cfgPath, 0o640); err != nil {
		t.Fatal(err)
	}
	failRestarts(env, 1)
	if err := run(env.cfg); err == nil || !strings.Contains(err.Error(), "runs on them again") {
		t.Fatalf("got %v, want a rollback", err)
	}
	if got := env.hostFile(t, "/cloud/config.json"); got != eksConfig {
		t.Fatalf("not restored:\n%s", got)
	}
	if m := modeOf(t, cfgPath); m != 0o640 {
		t.Fatalf("restored with mode %o, want 0640", m)
	}
}
