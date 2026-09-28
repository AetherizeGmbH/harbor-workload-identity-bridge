// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
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

// sharedPatchInstalls installs a and b, both in auto mode on the default
// directories: a wires kubelet (patch), b adds its entry to a's config.
func sharedPatchInstalls(t *testing.T) (a, b *testEnv) {
	t.Helper()
	a = newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet"})
	b = newSiblingEnv(t, a, modeAuto, euName)
	for _, env := range []*testEnv{a, b} {
		if err := run(env.cfg); err != nil {
			t.Fatal(err)
		}
		if st := mustLoadState(t, env); st.Mode != modePatch {
			t.Fatalf("%s: state = %+v, want patch", env.cfg.ProviderName, st)
		}
	}
	return a, b
}

// assertKubeletWiring fails unless the running kubelet has wiring want and
// its config holds exactly the provider entries names.
func assertKubeletWiring(t *testing.T, env *testEnv, want kubeletWiring, names ...string) {
	t.Helper()
	if got, err := discoverKubelet(env.cfg.ProcRoot); err != nil || got != want {
		t.Fatalf("kubelet wiring = %+v (err %v), want %+v", got, err, want)
	}
	_, order := providersIn(t, env, want.ConfigFile)
	if !slices.Equal(sorted(order), sorted(names)) {
		t.Fatalf("kubelet's config %s holds %q, want %q", want.ConfigFile, order, names)
	}
	for _, name := range names {
		if !isExecutableHostFile(env.cfg.hostPath(filepath.Join(want.BinDir, name))) {
			t.Fatalf("no binary of %s in kubelet's bin dir %s", name, want.BinDir)
		}
	}
}

func sorted(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}

var directoryChanges = map[string]func(*config){
	"bin dir":    func(c *config) { c.HostBinDir = "/opt/hb/bin" },
	"config dir": func(c *config) { c.HostConfigDir = "/opt/hb/config" },
	"both":       func(c *config) { c.HostBinDir, c.HostConfigDir = "/opt/hb/bin", "/opt/hb/config" },
}

// TestRun_AutoStaysOnSharedOldDirectoriesAndStagesItsFiles: another
// install's entry is in the config of the old directories, and that install
// has not moved. Moving kubelet would drop it, so auto mode keeps kubelet
// there as a patch install, restarts nothing, and puts its own files into
// the new directories (ADR-0035). It used to merge into the old config and
// record merge mode, which kept it from ever trying the move again, and
// restarted kubelet for nothing.
func TestRun_AutoStaysOnSharedOldDirectoriesAndStagesItsFiles(t *testing.T) {
	for name, change := range directoryChanges {
		t.Run(name, func(t *testing.T) {
			a, _ := sharedPatchInstalls(t)
			old := a.cfg.ownWiring()
			change(a.cfg)
			for pass := 1; pass <= 2; pass++ {
				if err := run(a.cfg); err != nil {
					t.Fatalf("pass %d: %v", pass, err)
				}
				assertKubeletWiring(t, a, old, defaultProviderName, euName)
				if st := mustLoadState(t, a); st.Mode != modePatch || st.BinDir != old.BinDir || st.ConfigFile != old.ConfigFile {
					t.Fatalf("pass %d: state = %+v, want patch on %+v", pass, st, old)
				}
				if a.restarts != 1 {
					t.Fatalf("pass %d: restarts = %d, want only the install's", pass, a.restarts)
				}
			}
			// Staged: the binary and record in the new bin dir, the entry in
			// the new chart-owned config.
			want := a.cfg.ownWiring()
			if !isExecutableHostFile(a.cfg.hostPath(filepath.Join(want.BinDir, defaultProviderName))) {
				t.Fatal("binary not staged in the new bin dir")
			}
			raw, err := entryBytes(renderedEntry(t, a))
			if err != nil {
				t.Fatal(err)
			}
			if rec := a.cfg.readRecord(filepath.Join(want.BinDir, a.cfg.files().Record)); !rec.holds(raw) {
				t.Fatalf("record in the new bin dir = %+v", rec)
			}
			if byName, _ := providersIn(t, a, want.ConfigFile); byName[defaultProviderName] == nil {
				t.Fatal("entry not staged in the new chart-owned config")
			}
		})
	}
}

// TestRun_AutoMovesSharedDirectoriesWhenEveryInstallChangesThem: all
// installs on the directories change them, as patch mode's refusal tells
// operators to. Each refused the move because of the others' entries, fell
// back to merge mode, and kubelet never moved (review of #92). Now every
// earlier pass stages its files, and the last one moves kubelet with all of
// them (ADR-0035).
func TestRun_AutoMovesSharedDirectoriesWhenEveryInstallChangesThem(t *testing.T) {
	for name, change := range directoryChanges {
		t.Run(name, func(t *testing.T) {
			a, b := sharedPatchInstalls(t)
			old := a.cfg.ownWiring()
			change(a.cfg)
			change(b.cfg)
			if err := run(a.cfg); err != nil {
				t.Fatal(err)
			}
			assertKubeletWiring(t, a, old, defaultProviderName, euName)
			if err := run(b.cfg); err != nil {
				t.Fatal(err)
			}
			want := b.cfg.ownWiring()
			assertKubeletWiring(t, b, want, defaultProviderName, euName)
			if st := mustLoadState(t, b); st.Mode != modePatch || st.BinDir != want.BinDir || st.ConfigFile != want.ConfigFile {
				t.Fatalf("state of the install that moved = %+v, want patch on %+v", st, want)
			}
			// a's next pass finds kubelet on its directories.
			if err := run(a.cfg); err != nil {
				t.Fatal(err)
			}
			assertKubeletWiring(t, a, want, defaultProviderName, euName)
			if st := mustLoadState(t, a); st.Mode != modePatch || st.BinDir != want.BinDir || st.ConfigFile != want.ConfigFile {
				t.Fatalf("state = %+v, want patch on %+v", st, want)
			}
		})
	}
}

// TestRun_AutoMovesOnceTheOtherInstallLeft: the install that kept kubelet on
// the old directories is uninstalled (its entry, binary and record removed,
// as README describes). The next pass moves kubelet. The merge fallback
// recorded merge mode and never tried again.
func TestRun_AutoMovesOnceTheOtherInstallLeft(t *testing.T) {
	a, b := sharedPatchInstalls(t)
	old := a.cfg.ownWiring()
	a.cfg.HostBinDir = "/opt/hb/bin"
	if err := run(a.cfg); err != nil {
		t.Fatal(err)
	}
	assertKubeletWiring(t, a, old, defaultProviderName, euName)

	// Uninstall b.
	doc := parseConfig(t, []byte(a.hostFile(t, old.ConfigFile)))
	var kept []any
	for _, p := range doc["providers"].([]any) {
		if p.(map[string]any)["name"] != euName {
			kept = append(kept, p)
		}
	}
	doc["providers"] = kept
	out, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	writeHostFile(t, a, old.ConfigFile, string(out))
	for _, f := range []string{filepath.Join(old.BinDir, euName), filepath.Join(old.BinDir, b.cfg.files().Record)} {
		if err := os.Remove(a.cfg.hostPath(f)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if err := os.Remove(b.statePath()); err != nil {
		t.Fatal(err)
	}

	if err := run(a.cfg); err != nil {
		t.Fatal(err)
	}
	want := a.cfg.ownWiring()
	assertKubeletWiring(t, a, want, defaultProviderName)
	if a.restarts != 2 {
		t.Fatalf("restarts = %d, want 2: the install and the move", a.restarts)
	}
}

// TestRun_PatchMovesKubeletOnlyWithInstallsStagedWhereItGoes: patch mode
// may move kubelet away from another install's entry only when the config
// it moves to keeps that entry next to its binary and record in the new bin
// dir. A binary without a record there, or a record for other content, is
// not enough.
func TestRun_PatchMovesKubeletOnlyWithInstallsStagedWhereItGoes(t *testing.T) {
	stage := map[string]func(t *testing.T, a, b *testEnv){
		"binary only": func(t *testing.T, a, _ *testEnv) {
			writeHostFile(t, a, "/opt/hb/bin/"+euName, "ELF-fake-plugin")
			if err := os.Chmod(a.cfg.hostPath("/opt/hb/bin/"+euName), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"record of other content": func(t *testing.T, a, _ *testEnv) {
			writeHostFile(t, a, "/opt/hb/bin/"+euName, "ELF-fake-plugin")
			if err := os.Chmod(a.cfg.hostPath("/opt/hb/bin/"+euName), 0o755); err != nil {
				t.Fatal(err)
			}
			other := mustEntry(t, strings.Replace(renderedConfigFor(euName), euName+".example.com", "other.example.com", 1), euName)
			writeHostFile(t, a, "/opt/hb/bin/"+filesFor(euName).Record, recordOf(t, configDir+"/"+configFileName, other))
		},
		"staged by its installer": func(t *testing.T, _, b *testEnv) {
			b.cfg.HostBinDir = "/opt/hb/bin"
			if err := run(b.cfg); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, stage := range stage {
		t.Run(name, func(t *testing.T) {
			a, b := sharedPatchInstalls(t)
			old := a.cfg.ownWiring()
			stage(t, a, b)
			a.cfg.Mode = modePatch
			a.cfg.HostBinDir = "/opt/hb/bin"
			err := run(a.cfg)
			if name != "staged by its installer" {
				if err == nil || !strings.Contains(err.Error(), `holds the provider entries "harbor-bridge-eu"`) {
					t.Fatalf("got %v, want a refusal", err)
				}
				assertKubeletWiring(t, a, old, defaultProviderName, euName)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertKubeletWiring(t, a, a.cfg.ownWiring(), defaultProviderName, euName)
		})
	}
}

// TestRun_AutoRefusesToStayOnOldDirectoriesInItsConfigDir: the old bin dir
// is inside the new plugin.hostConfigDir, which every release's sync
// container can write: the records there would decide which entries stay
// in the old config. Refused, as a merge pass into it is.
func TestRun_AutoRefusesToStayOnOldDirectoriesInItsConfigDir(t *testing.T) {
	a, _ := sharedPatchInstalls(t)
	a.cfg.HostBinDir, a.cfg.HostConfigDir = "/opt/hb/bin", "/etc/kubernetes"
	err := run(a.cfg)
	if err == nil || !strings.Contains(err.Error(), "must not be the same directory or inside one another") {
		t.Fatalf("got %v, want a refusal", err)
	}
	if a.restarts != 1 {
		t.Fatalf("restarts = %d", a.restarts)
	}
}
