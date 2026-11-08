// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"sigs.k8s.io/yaml"
)

// Rename cases found in the review of ADR-0039, decision 6.

// renameMergeToPatch installs the default name merged into a cloud's config
// (cloud.yaml holds doc). The caller then renames it with mode patch:
// kubelet moves to the chart-owned config, and the cloud's config, which
// kubelet may read again once the patch flags go, still holds the old
// entry.
func renameMergeToPatch(t *testing.T, doc string) (*testEnv, kubeletWiring) {
	t.Helper()
	cloud := kubeletWiring{BinDir: "/cloud/bin", ConfigFile: "/cloud/config.yaml"}
	env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet", flagBinDir + "=" + cloud.BinDir, flagConfigFile + "=" + cloud.ConfigFile})
	env.cfg.ReleaseID = testRelease
	writeCloudConfig(t, env, cloud.BinDir, cloud.ConfigFile, doc)
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	if st := mustLoadState(t, env); st.Mode != modeMerge {
		t.Fatalf("state %+v", st)
	}
	return env, cloud
}

// TestRename_DropsTheOldEntryFromEveryConfig: the old name's binary goes
// only after its entry left every config on the node, the cloud's config
// kubelet does not read now included; kubelet later starts on the cloud's
// config again (finding 3).
func TestRename_DropsTheOldEntryFromEveryConfig(t *testing.T) {
	env, cloud := renameMergeToPatch(t, gkeConfig)
	withName(t, env, euName)
	env.cfg.Mode = modePatch
	if err := run(env.cfg); err != nil {
		t.Fatalf("rename and patch: %v", err)
	}
	if _, order := providersIn(t, env, cloud.ConfigFile); len(order) != 1 || order[0] != "auth-provider-gcp" {
		t.Fatalf("cloud providers = %v, want the old entry gone", order)
	}
	assertAbsent(t, env, cloud.BinDir+"/"+defaultProviderName, cloud.BinDir+"/"+filesFor(defaultProviderName).Record)
	// The removal takes the patch flags away; kubelet runs the cloud's
	// wiring again, which must start (the fake kubelet exits on a missing
	// binary).
	mustRemove(t, env, resultRemoved)
	if w, _ := discoverKubelet(env.cfg.ProcRoot); w != cloud {
		t.Fatalf("kubelet wiring = %+v, want the cloud's", w)
	}
}

// TestRename_KeepsTheOldBinaryForAConfigItCannotEmpty: the cloud's config
// holds only the old entry; written without it, it would have no
// providers. The old entry, binary and record stay (finding 3).
func TestRename_KeepsTheOldBinaryForAConfigItCannotEmpty(t *testing.T) {
	env, cloud := renameMergeToPatch(t, gkeConfig)
	cfg := parseConfig(t, []byte(env.hostFile(t, cloud.ConfigFile)))
	cfg["providers"] = cfg["providers"].([]any)[1:]
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	writeHostFile(t, env, cloud.ConfigFile, string(raw))
	withName(t, env, euName)
	env.cfg.Mode = modePatch
	if err := run(env.cfg); err != nil {
		t.Fatalf("rename and patch: %v", err)
	}
	if env.hostFile(t, cloud.ConfigFile) != string(raw) {
		t.Fatal("the cloud's config was written without providers")
	}
	assertPresent(t, env, cloud.BinDir+"/"+defaultProviderName, cloud.BinDir+"/"+filesFor(defaultProviderName).Record)
}

// TestRename_AndMove: a rename together with a change of
// plugin.hostBinaryDir and plugin.hostConfigDir in auto mode. The earlier
// name's patch-mode state counts as the install's own (wiredByOwnPatch),
// so kubelet moves to the new directories as a patch install, and the
// removal afterwards works (finding 4).
func TestRename_AndMove(t *testing.T) {
	env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet"})
	env.cfg.ReleaseID = testRelease
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	old := env.cfg.ownWiring()
	withName(t, env, euName)
	env.cfg.HostBinDir, env.cfg.HostConfigDir = "/opt/hb/bin", "/opt/hb/config"
	if err := run(env.cfg); err != nil {
		t.Fatalf("rename and move: %v", err)
	}
	assertKubeletWiring(t, env, env.cfg.ownWiring(), euName)
	if st := mustLoadState(t, env); st.Mode != modePatch {
		t.Fatalf("state = %+v, want patch on the new directories", st)
	}
	assertAbsent(t, env, old.ConfigFile, old.BinDir+"/"+defaultProviderName, stateDir+"/installer-state.json")
	mustRemove(t, env, resultRemoved)
	if w, _ := discoverKubelet(env.cfg.ProcRoot); w.wired() {
		t.Fatalf("kubelet still wired: %+v", w)
	}
}
