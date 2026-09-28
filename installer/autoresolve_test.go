// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// TestRun_AutoMovesItsOwnPatchInstallToChangedDirectories: after a
// patch-mode install, kubelet's flags point at this install's directories.
// When plugin.hostBinaryDir or plugin.hostConfigDir changes, auto mode used
// to take them for a cloud's config and merge into the old files: the new
// value was ignored for good, the recorded mode flipped to merge and
// kubelet restarted for nothing. The state file shows the flags are the
// install's own; auto mode moves kubelet as patch mode does.
func TestRun_AutoMovesItsOwnPatchInstallToChangedDirectories(t *testing.T) {
	for name, change := range map[string]func(*config){
		"bin dir":    func(c *config) { c.HostBinDir = "/opt/hb/bin" },
		"config dir": func(c *config) { c.HostConfigDir = "/opt/hb/config" },
		"both":       func(c *config) { c.HostBinDir, c.HostConfigDir = "/opt/hb/bin", "/opt/hb/config" },
	} {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet"})
			if err := run(env.cfg); err != nil {
				t.Fatal(err)
			}
			change(env.cfg)
			if err := run(env.cfg); err != nil {
				t.Fatal(err)
			}
			want := kubeletWiring{BinDir: env.cfg.HostBinDir, ConfigFile: env.cfg.ownConfigPath()}
			if got, err := discoverKubelet(env.cfg.ProcRoot); err != nil || got != want {
				t.Fatalf("kubelet wiring = %+v (err %v), want %+v", got, err, want)
			}
			st := mustLoadState(t, env)
			if st.Mode != modePatch || st.BinDir != want.BinDir || st.ConfigFile != want.ConfigFile {
				t.Fatalf("state = %+v, want patch mode on %+v", st, want)
			}
			if got := env.hostFile(t, want.BinDir+"/harbor-bridge-plugin"); got != "ELF-fake-plugin" {
				t.Fatal("binary not in the new bin dir")
			}
			if env.restarts != 2 {
				t.Fatalf("restarts = %d, want 2", env.restarts)
			}
			if err := run(env.cfg); err != nil || env.restarts != 2 {
				t.Fatalf("no-op re-run: %v, restarts = %d", err, env.restarts)
			}
		})
	}
}

// TestRun_AutoMergesWhenTheFlagsAreNotItsOwnPatchInstall: flags that no
// state file of this install records as its own patch-mode wiring are a
// cloud's (or someone else's) config: auto mode merges into it. A state
// file in merge mode on the same paths is no reason to patch either.
func TestRun_AutoMergesWhenTheFlagsAreNotItsOwnPatchInstall(t *testing.T) {
	env := newTestEnv(t, modeAuto, []string{
		"/usr/bin/kubelet",
		"--image-credential-provider-bin-dir=/cloud/bin",
		"--image-credential-provider-config=/cloud/config.json",
	})
	writeCloudConfig(t, env, "/cloud/bin", "/cloud/config.json", eksConfig)
	for pass := 1; pass <= 2; pass++ {
		if err := run(env.cfg); err != nil {
			t.Fatal(err)
		}
		if st := mustLoadState(t, env); st.Mode != modeMerge {
			t.Fatalf("pass %d: state = %+v, want merge", pass, st)
		}
	}
	if env.restarts != 1 {
		t.Fatalf("restarts = %d, want 1", env.restarts)
	}
	assertAbsent(t, env, defaultKubeletPath)
}

// TestRun_AutoKeepsSharedOldDirectoriesWhenOtherInstallsUseThem: another
// install's entry is in the config of the old directories. Moving kubelet
// would drop it (checkRewire), so auto mode keeps kubelet there and merges,
// as it did before it recognised its own directories.
func TestRun_AutoKeepsSharedOldDirectoriesWhenOtherInstallsUseThem(t *testing.T) {
	a := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet"})
	b := newSiblingEnv(t, a, modeAuto, euName)
	for _, env := range []*testEnv{a, b} {
		if err := run(env.cfg); err != nil {
			t.Fatal(err)
		}
	}
	a.cfg.HostBinDir = "/opt/hb/bin"
	if err := run(a.cfg); err != nil {
		t.Fatalf("got %v, want a merge into the shared config", err)
	}
	old := kubeletWiring{BinDir: binDir, ConfigFile: configDir + "/" + configFileName}
	if got, err := discoverKubelet(a.cfg.ProcRoot); err != nil || got != old {
		t.Fatalf("kubelet wiring = %+v (err %v), want the shared %+v", got, err, old)
	}
	byName, _ := providersIn(t, a, old.ConfigFile)
	if _, ok := byName[euName]; !ok {
		t.Fatal("the other install's entry was dropped")
	}
	if st := mustLoadState(t, a); st.Mode != modeMerge || st.BinDir != binDir {
		t.Fatalf("state = %+v, want merge on the shared directories", st)
	}
	if !strings.Contains(a.hostFile(t, binDir+"/harbor-bridge-plugin"), "ELF") {
		t.Fatal("binary missing from kubelet's bin dir")
	}
}
