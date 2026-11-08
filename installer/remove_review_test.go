// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Cases found in the review of the node-side removal (ADR-0039). The fake
// kubelet exits at startup as kubelet does (fakeKubelet.startProblem), so a
// removal that leaves a node kubelet cannot start from fails its restart.

// TestRemove_RefusesFlagsNotYetLiveWithoutState: the first install pass
// wrote the kubelet flags and died before its verified restart, so no
// state was saved and kubelet still runs without the flags. The flags name
// this install's own directories: deleting the config or the binary would
// keep kubelet from starting at its next restart (finding 2).
func TestRemove_RefusesFlagsNotYetLiveWithoutState(t *testing.T) {
	env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet"})
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(env.statePath()); err != nil {
		t.Fatal(err)
	}
	writeProcEntry(t, env.cfg.ProcRoot, fakeKubeletPID, procEntry{comm: "kubelet", cmdline: []string{"/usr/bin/kubelet"}})
	envBefore := env.hostFile(t, defaultKubeletPath)
	if _, err := removeOnce(t, env); err == nil || !strings.Contains(err.Error(), "died between writing the flags and restarting kubelet") {
		t.Fatalf("got %v, want a refusal", err)
	}
	if env.hostFile(t, defaultKubeletPath) != envBefore {
		t.Fatal("the refused removal changed the environment file")
	}
	assertPresent(t, env, env.cfg.ownConfigPath(), binDir+"/"+defaultProviderName)
}

// TestRemove_FlagsLiveWithoutState: helm install and a quick helm
// uninstall: the install pass restarted kubelet onto its flags and was
// stopped before it saved its state. The flags name this install's own
// directories and kubelet runs them, so the removal takes them as its own
// instead of refusing forever (finding 2, ADR-0039 decision 3.1).
func TestRemove_FlagsLiveWithoutState(t *testing.T) {
	env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet"})
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(env.statePath()); err != nil {
		t.Fatal(err)
	}
	mustRemove(t, env, resultRemoved)
	if w, _ := discoverKubelet(env.cfg.ProcRoot); w.wired() {
		t.Fatalf("kubelet still wired: %+v", w)
	}
	assertAbsent(t, env, env.cfg.ownConfigPath(), binDir+"/"+defaultProviderName)
}

// TestRemove_RefusesAMissingKubeletConfig: kubelet's config was deleted by
// hand; kubelet does not start while its flag names a missing file, so the
// removal refuses instead of restarting it (finding 7).
func TestRemove_RefusesAMissingKubeletConfig(t *testing.T) {
	env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet"})
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(env.cfg.hostPath(env.cfg.ownConfigPath())); err != nil {
		t.Fatal(err)
	}
	if _, err := removeOnce(t, env); err == nil || !strings.Contains(err.Error(), "is missing") {
		t.Fatalf("got %v, want a refusal", err)
	}
	if env.restarts != 1 {
		t.Fatalf("restarts = %d: the refusal restarted kubelet", env.restarts)
	}
	assertPresent(t, env, binDir+"/"+defaultProviderName)
}

// TestRemove_KeepsAnUnreadCloudConfigsLastEntry: a cloud's config kubelet
// does not read now holds only this install's entry. Written without it,
// it would have no providers, which kubelet refuses if its flags ever
// point there again: the entry stays, and the binary it needs (finding 6).
func TestRemove_KeepsAnUnreadCloudConfigsLastEntry(t *testing.T) {
	cloud := kubeletWiring{BinDir: "/cloud/bin", ConfigFile: "/cloud/config.yaml"}
	env := newTestEnv(t, modeMerge, []string{"/usr/bin/kubelet", flagBinDir + "=" + cloud.BinDir, flagConfigFile + "=" + cloud.ConfigFile})
	writeCloudConfig(t, env, cloud.BinDir, cloud.ConfigFile, gkeConfig)
	writeCloudConfig(t, env, "/old/bin", "/old/config.yaml", gkeConfig)
	env.cfg.MergeBinDir, env.cfg.MergeConfigFile = "/old/bin", "/old/config.yaml"
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	cfg := parseConfig(t, []byte(env.hostFile(t, "/old/config.yaml")))
	cfg["providers"] = cfg["providers"].([]any)[1:]
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	writeHostFile(t, env, "/old/config.yaml", string(raw))
	env.cfg.MergeBinDir, env.cfg.MergeConfigFile = "", ""

	rep := mustRemove(t, env, resultKept)
	if env.hostFile(t, "/old/config.yaml") != string(raw) {
		t.Fatal("the cloud's config was written without providers")
	}
	assertPresent(t, env, "/old/bin/"+defaultProviderName, "/old/bin/"+filesFor(defaultProviderName).Record)
	if len(rep.Kept) == 0 || !strings.HasPrefix(rep.Kept[0], "/old/config.yaml:") {
		t.Fatalf("kept = %q", rep.Kept)
	}
}

// TestRemove_MergeOverrides: merge mode with plugin.install.binDir and
// configFile installs without discovering kubelet; the removal uses the
// same targets instead of failing on discovery.
func TestRemove_MergeOverrides(t *testing.T) {
	env := newTestEnv(t, modeMerge, nil)
	env.cfg.MergeBinDir, env.cfg.MergeConfigFile = "/cloud/bin", "/cloud/config.yaml"
	writeCloudConfig(t, env, "/cloud/bin", "/cloud/config.yaml", gkeConfig)
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	rep := mustRemove(t, env, resultRemoved)
	if !rep.KubeletRestarted {
		t.Fatal("kubelet not restarted")
	}
	if _, order := providersIn(t, env, "/cloud/config.yaml"); len(order) != 1 || order[0] != "auth-provider-gcp" {
		t.Fatalf("providers = %v", order)
	}
	assertAbsent(t, env, ownFiles(env, "/cloud/bin")...)
}

// mergedIntoPatch is a, a patch install of euName that wired kubelet, and
// b, the default name with other directories, which merged its entry into
// a's chart-owned config (decision 3.5).
func mergedIntoPatch(t *testing.T) (a, b *testEnv) {
	t.Helper()
	a = withName(t, newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet"}), euName)
	b = newSiblingEnv(t, a, modeAuto, defaultProviderName)
	b.cfg.HostBinDir, b.cfg.HostConfigDir = "/opt/b/bin", "/opt/b/config"
	setRendered(t, b, renderedConfig)
	for _, env := range []*testEnv{a, b} {
		if err := run(env.cfg); err != nil {
			t.Fatal(err)
		}
	}
	if st := mustLoadState(t, b); st.Mode != modeMerge {
		t.Fatalf("b's state = %+v, want merge into a's config", st)
	}
	return a, b
}

// TestRemove_MergedInstallFirst: the install that merged into another
// install's chart-owned config leaves first; the other's entry and flags
// stay (decision 3.5). Then the install that set the flags removes them.
func TestRemove_MergedInstallFirst(t *testing.T) {
	a, b := mergedIntoPatch(t)
	rep := mustRemove(t, b, resultRemoved)
	if !rep.KubeletRestarted {
		t.Fatal("kubelet not restarted")
	}
	assertKubeletWiring(t, a, a.cfg.ownWiring(), euName)
	assertAbsent(t, b, binDir+"/"+defaultProviderName, binDir+"/"+filesFor(defaultProviderName).Record)
	mustRemove(t, a, resultRemoved)
	if w, _ := discoverKubelet(a.cfg.ProcRoot); w.wired() {
		t.Fatalf("kubelet still wired: %+v", w)
	}
}

// TestRemove_RefusesToStrandAMergedInstall: the install that set the flags
// may not leave before an install that merged into its config: that one
// cannot remove the flags and could never leave (finding 9). The refusal
// names it and says to uninstall it first.
func TestRemove_RefusesToStrandAMergedInstall(t *testing.T) {
	a, _ := mergedIntoPatch(t)
	configBefore := a.hostFile(t, a.cfg.ownConfigPath())
	_, err := removeOnce(t, a)
	if err == nil || !strings.Contains(err.Error(), `Uninstall the release(s) of "harbor-bridge-plugin" first`) {
		t.Fatalf("got %v, want a refusal that names the merged install", err)
	}
	if a.restarts != 1 || a.hostFile(t, a.cfg.ownConfigPath()) != configBefore {
		t.Fatal("the refused removal changed the node")
	}
}

// TestRemove_AnotherFlagsOwnerStays: a third install patched the same
// directories as a, so it set the flags too. a may leave before the merged
// install b, because the third one removes the flags last; that one then
// refuses while b's entry is left.
func TestRemove_AnotherFlagsOwnerStays(t *testing.T) {
	const thirdName = "harbor-bridge-us"
	a, b := mergedIntoPatch(t)
	c := newSiblingEnv(t, a, modeAuto, thirdName)
	if err := run(c.cfg); err != nil {
		t.Fatal(err)
	}
	if st := mustLoadState(t, c); st.Mode != modePatch || st.ConfigFile != a.cfg.ownConfigPath() {
		t.Fatalf("c's state = %+v, want patch onto a's config", st)
	}
	mustRemove(t, a, resultRemoved)
	assertKubeletWiring(t, c, c.cfg.ownWiring(), defaultProviderName, thirdName)
	if _, err := removeOnce(t, c); err == nil || !strings.Contains(err.Error(), `Uninstall the release(s) of "harbor-bridge-plugin" first`) {
		t.Fatalf("got %v, want a refusal that names the merged install", err)
	}
	mustRemove(t, b, resultRemoved)
	mustRemove(t, c, resultRemoved)
	if w, _ := discoverKubelet(c.cfg.ProcRoot); w.wired() {
		t.Fatalf("kubelet still wired: %+v", w)
	}
}

// TestRemove_DirectoryConfig: kubelet 1.34+ reads a directory of config
// files, which the installer never edits. A file there that holds this
// install's entry refuses the removal; a directory without one is left
// alone.
func TestRemove_DirectoryConfig(t *testing.T) {
	dir := "/etc/kubernetes/cp.d"
	args := []string{"/usr/bin/kubelet", flagBinDir + "=/cloud/bin", flagConfigFile + "=" + dir}

	env := newTestEnv(t, modeAuto, args)
	writeCloudConfig(t, env, "/cloud/bin", dir+"/cloud.yaml", gkeConfig)
	writeHostFile(t, env, dir+"/harbor.yaml", renderedConfig)
	if _, err := removeOnce(t, env); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("got %v, want a refusal", err)
	}

	env = newTestEnv(t, modeAuto, args)
	writeCloudConfig(t, env, "/cloud/bin", dir+"/cloud.yaml", gkeConfig)
	mustRemove(t, env, resultNothing)
	if env.restarts != 0 || env.hostFile(t, dir+"/cloud.yaml") != gkeConfig {
		t.Fatal("the removal touched kubelet or its config directory")
	}
}

// TestRemove_NoneKeepsAForeignBinary: in none mode a file of another
// program under this install's name, without its record, stays as it is.
func TestRemove_NoneKeepsAForeignBinary(t *testing.T) {
	env := withName(t, newTestEnv(t, modeNone, nil), euName)
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(env.cfg.hostPath(binDir + "/" + filesFor(euName).Record)); err != nil {
		t.Fatal(err)
	}
	writeHostFile(t, env, binDir+"/"+euName, "OTHER-PROGRAM")
	mustRemove(t, env, resultKept)
	if got := env.hostFile(t, binDir+"/"+euName); got != "OTHER-PROGRAM" {
		t.Fatalf("the other program's file = %q", got)
	}
	assertAbsent(t, env, binDir+"/"+filesFor(euName).Record)
}
