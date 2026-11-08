// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"slices"
	"strings"
	"testing"
)

// ADR-0039, decision 6: a rename of plugin.providerName removes the
// release's earlier name in the pass that installs the new one.

const testRelease = "harbor-bridge-system/harbor-bridge"

// defaultFiles are the node paths of the default name's own files in the
// chart's directories, each with its .bak.
func defaultFiles(bin string) []string {
	f := filesFor(defaultProviderName)
	var paths []string
	for _, p := range []string{
		bin + "/" + f.Binary, bin + "/" + f.Record,
		configDir + "/" + f.CA, configDir + "/" + f.ClientCert, configDir + "/" + f.ClientKey,
		stateDir + "/" + f.State,
	} {
		paths = append(paths, p, p+".bak")
	}
	return paths
}

// TestRename_Patch: the entry of the old name leaves the chart-owned config
// in the write that adds the new one, kubelet restarts once, and the old
// name's files go; the flags stay.
func TestRename_Patch(t *testing.T) {
	env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet"})
	env.cfg.ReleaseID = testRelease
	env.cfg.MTLSEnabled = true
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	withName(t, env, euName)
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	if env.restarts != 2 {
		t.Fatalf("restarts = %d, want one for the rename", env.restarts)
	}
	assertKubeletWiring(t, env, env.cfg.ownWiring(), euName)
	assertAbsent(t, env, defaultFiles(binDir)...)
	assertPresent(t, env, binDir+"/"+euName, binDir+"/"+filesFor(euName).Record, configDir+"/"+filesFor(euName).CA)

	// A second pass finds nothing to retire and restarts nothing.
	if err := run(env.cfg); err != nil || env.restarts != 2 {
		t.Fatalf("second pass: %v, restarts %d", err, env.restarts)
	}
}

// TestRename_Merge: in a cloud's config the old name's entry goes, the
// cloud's stays, and the old binary leaves kubelet's bin dir.
func TestRename_Merge(t *testing.T) {
	cloud := kubeletWiring{BinDir: "/cloud/bin", ConfigFile: "/cloud/config.yaml"}
	env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet", flagBinDir + "=" + cloud.BinDir, flagConfigFile + "=" + cloud.ConfigFile})
	env.cfg.ReleaseID = testRelease
	writeCloudConfig(t, env, cloud.BinDir, cloud.ConfigFile, gkeConfig)
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	withName(t, env, euName)
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	if env.restarts != 2 {
		t.Fatalf("restarts = %d, want one for the rename", env.restarts)
	}
	if _, order := providersIn(t, env, cloud.ConfigFile); !slices.Equal(order, []string{"auth-provider-gcp", euName}) {
		t.Fatalf("providers = %v", order)
	}
	assertAbsent(t, env, defaultFiles(cloud.BinDir)...)
	assertPresent(t, env, cloud.BinDir+"/auth-provider-gcp", cloud.BinDir+"/"+euName)
}

// TestRename_None: none mode drops the old entry (the new one keeps the
// config non-empty), keeps the old binary and empties its record.
func TestRename_None(t *testing.T) {
	env := newTestEnv(t, modeNone, nil)
	env.cfg.ReleaseID = testRelease
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	withName(t, env, euName)
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	if _, order := providersIn(t, env, env.cfg.ownConfigPath()); !slices.Equal(order, []string{euName}) {
		t.Fatalf("providers = %v", order)
	}
	assertPresent(t, env, binDir+"/"+defaultProviderName)
	if rec := env.cfg.readRecord(binDir + "/" + filesFor(defaultProviderName).Record); rec == nil || len(rec.Entries) != 0 {
		t.Fatalf("old record = %+v, want one that vouches for nothing", rec)
	}
	assertAbsent(t, env, configDir+"/harbor-bridge-ca.crt")
	if env.restarts != 0 {
		t.Fatal("none mode restarted kubelet")
	}
}

// TestRename_OnlyItsOwnRelease: another release's name stays, and so does a
// name whose record names no release (written before ADR-0039, or by a
// chart without RELEASE_ID): a release must run this version once before a
// rename removes its old name.
func TestRename_OnlyItsOwnRelease(t *testing.T) {
	a, b := sharedPatchInstalls(t)
	b.cfg.ReleaseID = "other/release"
	if err := run(b.cfg); err != nil {
		t.Fatal(err)
	}
	// a has no release in its record yet; it gets one and renames at once.
	a.cfg.ReleaseID = testRelease
	withName(t, a, "harbor-bridge-x")
	if err := run(a.cfg); err != nil {
		t.Fatal(err)
	}
	assertKubeletWiring(t, a, a.cfg.ownWiring(), defaultProviderName, euName, "harbor-bridge-x")

	// Once a's record names the release, the next rename retires the name.
	withName(t, a, "harbor-bridge-y")
	if err := run(a.cfg); err != nil {
		t.Fatal(err)
	}
	assertKubeletWiring(t, a, a.cfg.ownWiring(), defaultProviderName, euName, "harbor-bridge-y")
	assertAbsent(t, a, binDir+"/harbor-bridge-x", binDir+"/"+filesFor("harbor-bridge-x").Record)
}

// TestRename_Swap: two releases swap their names. Each name's record names
// the release that wrote it last, so neither pass removes the other's
// entry, whichever runs first.
func TestRename_Swap(t *testing.T) {
	for _, aFirst := range []bool{true, false} {
		a := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet"})
		a.cfg.ReleaseID = "a/a"
		b := newSiblingEnv(t, a, modeAuto, euName)
		b.cfg.ReleaseID = "b/b"
		for _, env := range []*testEnv{a, b} {
			if err := run(env.cfg); err != nil {
				t.Fatal(err)
			}
		}
		withName(t, a, euName)
		withName(t, b, defaultProviderName)
		order := []*testEnv{a, b}
		if !aFirst {
			order = []*testEnv{b, a}
		}
		for _, env := range order {
			if err := run(env.cfg); err != nil {
				t.Fatalf("a first %v: %v", aFirst, err)
			}
		}
		assertKubeletWiring(t, a, a.cfg.ownWiring(), defaultProviderName, euName)
		for name, release := range map[string]string{euName: "a/a", defaultProviderName: "b/b"} {
			if rec := a.cfg.readRecord(binDir + "/" + filesFor(name).Record); rec == nil || rec.Release != release {
				t.Fatalf("a first %v: record of %s = %+v, want release %s", aFirst, name, rec, release)
			}
		}
	}
}

// TestRemove_AlsoRemovesEarlierNames: a removal under the new name, on a
// node where the rename has not run yet, removes the release's earlier
// name, the flags too when it was the last entry.
func TestRemove_AlsoRemovesEarlierNames(t *testing.T) {
	env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet"})
	env.cfg.ReleaseID = testRelease
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	withName(t, env, euName)
	rep := mustRemove(t, env, resultRemoved)
	if !rep.KubeletRestarted {
		t.Fatal("kubelet not restarted")
	}
	if w, _ := discoverKubelet(env.cfg.ProcRoot); w.wired() {
		t.Fatalf("kubelet still wired: %+v", w)
	}
	if strings.Contains(env.hostFile(t, defaultKubeletPath), flagBinDir) {
		t.Fatal("flags left")
	}
	assertAbsent(t, env, defaultFiles(binDir)...)
	assertAbsent(t, env, env.cfg.ownConfigPath())
}
