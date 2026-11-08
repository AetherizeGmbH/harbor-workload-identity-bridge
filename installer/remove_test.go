// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// ADR-0039: a removal pass takes this install's entry and files off the
// node, restarts kubelet when what kubelet reads changed, and leaves
// everything else.

// removeOnce runs one removal pass of env and checks that its report
// encodes as one JSON line within kubelet's termination message limit.
func removeOnce(t *testing.T, env *testEnv) (*removalReport, error) {
	t.Helper()
	env.cfg.Action = actionRemove
	rep, err := runRemove(env.cfg)
	raw := rep.encode()
	var back removalReport
	if len(raw) > maxReportBytes+1 || json.Unmarshal(raw, &back) != nil || back.Provider != env.cfg.ProviderName {
		t.Fatalf("report %q does not encode as one JSON line of this provider", raw)
	}
	return rep, err
}

// mustRemove is removeOnce for a pass that must succeed with result.
func mustRemove(t *testing.T, env *testEnv, result string) *removalReport {
	t.Helper()
	rep, err := removeOnce(t, env)
	if err != nil {
		t.Fatalf("removal: %v", err)
	}
	if rep.Result != result {
		t.Fatalf("result = %q, want %q (report %+v)", rep.Result, result, rep)
	}
	return rep
}

func assertPresent(t *testing.T, env *testEnv, nodePaths ...string) {
	t.Helper()
	for _, p := range nodePaths {
		if _, err := os.Lstat(env.cfg.hostPath(p)); err != nil {
			t.Errorf("%s is gone: %v", p, err)
		}
	}
}

// withBackups gives every own file of env's install a .bak: a second
// install pass with another plugin binary, CA, client pair and entry.
func withBackups(t *testing.T, env *testEnv) {
	t.Helper()
	for path, content := range map[string]string{
		env.cfg.SourcePlugin:     "ELF-fake-plugin-v2",
		env.cfg.SourceCA:         "CA-PEM-v2",
		env.cfg.SourceClientCert: "CLIENT-CERT-v2",
		env.cfg.SourceClientKey:  "CLIENT-KEY-v2",
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	setRendered(t, env, strings.Replace(string(mustRead(t, env.cfg.SourceConfig)), `"harbor.example.com"`, `"harbor-v2.example.com"`, 1))
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// ownFiles are the node paths of env's own files in the chart's default
// directories, each with its .bak.
func ownFiles(env *testEnv, binDir string) []string {
	f := env.cfg.files()
	var paths []string
	for _, p := range []string{
		filepath.Join(binDir, f.Binary), filepath.Join(binDir, f.Record),
		filepath.Join(env.cfg.HostConfigDir, f.CA), filepath.Join(env.cfg.HostConfigDir, f.ClientCert),
		filepath.Join(env.cfg.HostConfigDir, f.ClientKey), filepath.Join(env.cfg.StateDir, f.State),
	} {
		paths = append(paths, p, p+".bak")
	}
	return paths
}

// TestRemove_PatchLastInstall: a single patch install leaves nothing of
// itself: the flags go from KUBELET_EXTRA_ARGS (the operator's args stay),
// kubelet restarts once and runs without credential providers, and every
// own file goes with its .bak, the chart-owned config too. The lock files
// and the environment file's .bak stay (decision 3.9). A second pass finds
// nothing, and a new install works.
func TestRemove_PatchLastInstall(t *testing.T) {
	env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet"})
	env.cfg.MTLSEnabled = true
	const operator = "# node settings\nKUBELET_EXTRA_ARGS=\"--max-pods=42\"\n"
	writeHostFile(t, env, defaultKubeletPath, operator)
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	withBackups(t, env)
	if env.restarts != 2 {
		t.Fatalf("restarts = %d after two install passes", env.restarts)
	}
	// mTLS off now: the client pair the install wrote still goes.
	env.cfg.MTLSEnabled = false

	rep := mustRemove(t, env, resultRemoved)
	if !rep.KubeletRestarted || env.restarts != 3 {
		t.Fatalf("restarted %v, restarts = %d, want one restart", rep.KubeletRestarted, env.restarts)
	}
	if got := env.hostFile(t, defaultKubeletPath); got != operator {
		t.Fatalf("%s = %q, want the operator's args back", defaultKubeletPath, got)
	}
	if w, err := discoverKubelet(env.cfg.ProcRoot); err != nil || w.wired() {
		t.Fatalf("kubelet wiring after the removal = %+v, %v", w, err)
	}
	assertAbsent(t, env, ownFiles(env, binDir)...)
	assertAbsent(t, env, env.cfg.ownConfigPath(), env.cfg.ownConfigPath()+".bak")
	assertPresent(t, env, nodeLockPath, env.cfg.ownConfigPath()+lockSuffix, defaultKubeletPath+".bak")

	mustRemove(t, env, resultNothing)
	if env.restarts != 3 {
		t.Fatalf("a second removal restarted kubelet (restarts = %d)", env.restarts)
	}

	env.cfg.Action = actionInstall
	if err := run(env.cfg); err != nil {
		t.Fatalf("install after a removal: %v", err)
	}
	if w, _ := discoverKubelet(env.cfg.ProcRoot); w != env.cfg.ownWiring() || env.restarts != 4 {
		t.Fatalf("install after a removal: wiring %+v, restarts %d", w, env.restarts)
	}
}

// TestRemove_PatchRPMUnitKeepsAnEmptyAssignment: the flags go from
// /etc/sysconfig/kubelet, and its assignment stays, empty, as RPM packages
// ship it (ADR-0034).
func TestRemove_PatchRPMUnitKeepsAnEmptyAssignment(t *testing.T) {
	env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet"})
	rpmUnit(t, env)
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	mustRemove(t, env, resultRemoved)
	if got := env.hostFile(t, rpmKubeletEnvFile); got != "KUBELET_EXTRA_ARGS=\"\"\n" {
		t.Fatalf("%s = %q", rpmKubeletEnvFile, got)
	}
	if w, _ := discoverKubelet(env.cfg.ProcRoot); w.wired() {
		t.Fatalf("kubelet still wired: %+v", w)
	}
	assertAbsent(t, env, defaultKubeletPath)
}

// TestRemove_SharedPatchInstalls: the first install to leave drops only its
// entry; the flags and the other install's entry and files stay. The last
// one to leave removes the flags (decision 3.2, 3.1).
func TestRemove_SharedPatchInstalls(t *testing.T) {
	a, b := sharedPatchInstalls(t)
	wiring := a.cfg.ownWiring()
	bFiles := []string{binDir + "/" + euName, binDir + "/" + filesFor(euName).Record, configDir + "/" + filesFor(euName).CA}

	rep := mustRemove(t, a, resultRemoved)
	if !rep.KubeletRestarted || a.restarts != 2 {
		t.Fatalf("restarted %v, restarts = %d", rep.KubeletRestarted, a.restarts)
	}
	assertKubeletWiring(t, a, wiring, euName)
	assertAbsent(t, a, binDir+"/"+defaultProviderName, binDir+"/"+filesFor(defaultProviderName).Record,
		configDir+"/harbor-bridge-ca.crt", stateDir+"/installer-state.json")
	assertPresent(t, a, bFiles...)

	// The other install's next pass keeps working without a's entry.
	if err := run(b.cfg); err != nil {
		t.Fatal(err)
	}
	assertKubeletWiring(t, b, wiring, euName)

	mustRemove(t, b, resultRemoved)
	if w, _ := discoverKubelet(b.cfg.ProcRoot); w.wired() {
		t.Fatalf("the last install left kubelet wired: %+v", w)
	}
	assertAbsent(t, b, append(bFiles, wiring.ConfigFile)...)
}

// TestRemove_MergeKeepsTheCloud: in a cloud's config only this install's
// entry goes; the cloud's provider and binary and the flags stay, and the
// binary and record go from kubelet's bin dir only after the verified
// restart (decision 3.4).
func TestRemove_MergeKeepsTheCloud(t *testing.T) {
	cloud := kubeletWiring{BinDir: "/cloud/bin", ConfigFile: "/cloud/config.json"}
	env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet", flagBinDir + "=" + cloud.BinDir, flagConfigFile + "=" + cloud.ConfigFile})
	writeCloudConfig(t, env, cloud.BinDir, cloud.ConfigFile, eksConfig)
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	// The binary must still be there when kubelet restarts onto the
	// config without the entry: a removal that deleted it first would be
	// found by a restart that does not verify.
	var binaryAtRestart bool
	env.kubelet.statusFn = func() unitStatus {
		binaryAtRestart = isRegularHostFile(env.cfg.hostPath(cloud.BinDir + "/" + defaultProviderName))
		return unitStatus{ActiveState: "active", MainPID: fakeKubeletPID + env.restarts, NRestarts: -1}
	}

	rep := mustRemove(t, env, resultRemoved)
	if !rep.KubeletRestarted || env.restarts != 2 || !binaryAtRestart {
		t.Fatalf("restarted %v, restarts %d, binary there during the restart %v", rep.KubeletRestarted, env.restarts, binaryAtRestart)
	}
	got := env.hostFile(t, cloud.ConfigFile)
	if !isJSON([]byte(got)) || !reflect.DeepEqual(parseConfig(t, []byte(got)), parseConfig(t, []byte(eksConfig))) {
		t.Fatalf("cloud config after the removal:\n%s", got)
	}
	if w, _ := discoverKubelet(env.cfg.ProcRoot); w != cloud {
		t.Fatalf("kubelet wiring = %+v, want the cloud's", w)
	}
	assertAbsent(t, env, ownFiles(env, cloud.BinDir)...)
	assertPresent(t, env, cloud.BinDir+"/ecr-credential-provider", cloud.ConfigFile+".bak")
}

// TestRemove_RefusesToEmptyAConfigItDidNotWire: kubelet refuses a config
// without providers, and only flags this install set may go: its own
// patch-mode restart, an earlier name's, or flags that name its own
// directories (TestRemove_FlagsLiveWithoutState). Every other last entry
// is refused before anything changes: in a cloud's config, in a
// chart-owned config whose flags another install set, and without a state
// file when the flags name other directories (decisions 3.4 to 3.6).
func TestRemove_RefusesToEmptyAConfigItDidNotWire(t *testing.T) {
	assertRefused := func(t *testing.T, env *testEnv, configPath string) {
		t.Helper()
		before := env.hostFile(t, configPath)
		restarts := env.restarts
		rep, err := removeOnce(t, env)
		if err == nil || !strings.Contains(err.Error(), "without providers") || rep.Result != resultFailed {
			t.Fatalf("got %v (report %+v), want a refusal", err, rep)
		}
		if env.restarts != restarts || env.hostFile(t, configPath) != before {
			t.Fatal("a refused removal changed the config or restarted kubelet")
		}
	}

	t.Run("the cloud's config", func(t *testing.T) {
		cloud := kubeletWiring{BinDir: "/cloud/bin", ConfigFile: "/cloud/config.yaml"}
		env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet", flagBinDir + "=" + cloud.BinDir, flagConfigFile + "=" + cloud.ConfigFile})
		writeCloudConfig(t, env, cloud.BinDir, cloud.ConfigFile, gkeConfig)
		if err := run(env.cfg); err != nil {
			t.Fatal(err)
		}
		// The cloud agent rewrote its config since: only this install's
		// entry is left.
		cfg := parseConfig(t, []byte(env.hostFile(t, cloud.ConfigFile)))
		cfg["providers"] = cfg["providers"].([]any)[1:]
		raw, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		writeHostFile(t, env, cloud.ConfigFile, string(raw))
		assertRefused(t, env, cloud.ConfigFile)
		assertPresent(t, env, cloud.BinDir+"/"+defaultProviderName)
	})

	t.Run("another install's wiring", func(t *testing.T) {
		// a wired kubelet to its chart-owned config (patch); b, with other
		// directories, merged into it (mergedIntoPatch); a left by hand
		// (helm uninstall --no-hooks and the manual steps, or a chart
		// before ADR-0039), which its own removal refuses
		// (TestRemove_RefusesToStrandAMergedInstall).
		a, b := mergedIntoPatch(t)
		bEntry := renderedEntry(t, b)
		writeHostFile(t, a, a.cfg.ownConfigPath(), string(configWith(t, bEntry)))
		for _, p := range []string{a.statePath(), a.cfg.hostPath(binDir + "/" + euName), a.cfg.hostPath(binDir + "/" + filesFor(euName).Record)} {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
		}
		assertRefused(t, b, a.cfg.ownConfigPath())
		if _, err := removeOnce(t, b); err == nil || !strings.Contains(err.Error(), "uninstall such an install before the one whose config it merged into") {
			t.Fatalf("got %v, want the ordering hint", err)
		}
		assertPresent(t, b, binDir+"/"+defaultProviderName, binDir+"/"+b.cfg.files().Record)
	})

	t.Run("no state file, flags of other directories", func(t *testing.T) {
		a, b := mergedIntoPatch(t)
		writeHostFile(t, a, a.cfg.ownConfigPath(), string(configWith(t, renderedEntry(t, b))))
		for _, p := range []string{a.statePath(), b.statePath(), a.cfg.hostPath(binDir + "/" + euName), a.cfg.hostPath(binDir + "/" + filesFor(euName).Record)} {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
		}
		assertRefused(t, b, a.cfg.ownConfigPath())
	})
}

// TestRemove_StagedFiles: an install that ADR-0035 kept on the old
// directories while it staged its files in the new ones leaves both; the
// other install, which kept it there, keeps its entry, files and flags
// (decision 3.7).
func TestRemove_StagedFiles(t *testing.T) {
	a, _ := sharedPatchInstalls(t)
	old := a.cfg.ownWiring()
	a.cfg.HostBinDir, a.cfg.HostConfigDir = "/opt/hb/bin", "/opt/hb/config"
	if err := run(a.cfg); err != nil {
		t.Fatal(err)
	}
	staged := a.cfg.ownWiring()

	mustRemove(t, a, resultRemoved)
	assertKubeletWiring(t, a, old, euName)
	assertAbsent(t, a, staged.ConfigFile, staged.BinDir+"/"+defaultProviderName, staged.BinDir+"/"+a.cfg.files().Record,
		old.BinDir+"/"+defaultProviderName, old.BinDir+"/"+a.cfg.files().Record, stateDir+"/installer-state.json")
	assertPresent(t, a, old.BinDir+"/"+euName, old.BinDir+"/"+filesFor(euName).Record)
}

// TestRemove_None: none mode cannot see kubelet's flags. It keeps the
// binary, drops the entry only when another vouched entry stays in the
// chart-owned config (and then empties its record), deletes the CA and
// mTLS files, and never touches kubelet (decision 3.8).
func TestRemove_None(t *testing.T) {
	t.Run("last entry", func(t *testing.T) {
		env := newTestEnv(t, modeNone, nil)
		env.cfg.MTLSEnabled = true
		if err := run(env.cfg); err != nil {
			t.Fatal(err)
		}
		withBackups(t, env)
		record := env.hostFile(t, binDir+"/"+env.cfg.files().Record)
		rep := mustRemove(t, env, resultKept)
		if env.restarts != 0 || rep.KubeletRestarted {
			t.Fatal("none mode restarted kubelet")
		}
		if got := env.hostFile(t, env.cfg.ownConfigPath()); !strings.Contains(got, "harbor-v2.example.com") {
			t.Fatalf("the last entry must stay:\n%s", got)
		}
		if got := env.hostFile(t, binDir+"/"+env.cfg.files().Record); got != record {
			t.Fatalf("the record of an entry that stays changed: %s", got)
		}
		if len(rep.Kept) != 2 || !strings.HasPrefix(rep.Kept[0], env.cfg.ownConfigPath()+":") || !strings.HasPrefix(rep.Kept[1], binDir+"/"+defaultProviderName+":") {
			t.Fatalf("kept = %q", rep.Kept)
		}
		assertPresent(t, env, binDir+"/"+defaultProviderName)
		assertAbsent(t, env, binDir+"/"+defaultProviderName+".bak", binDir+"/"+env.cfg.files().Record+".bak")
		f := env.cfg.files()
		for _, name := range []string{f.CA, f.ClientCert, f.ClientKey} {
			assertAbsent(t, env, configDir+"/"+name, configDir+"/"+name+".bak")
		}
	})

	t.Run("another entry stays", func(t *testing.T) {
		a := newTestEnv(t, modeNone, nil)
		b := newSiblingEnv(t, a, modeNone, euName)
		for _, env := range []*testEnv{a, b} {
			if err := run(env.cfg); err != nil {
				t.Fatal(err)
			}
		}
		rep := mustRemove(t, a, resultKept)
		if len(rep.Kept) != 1 || a.restarts != 0 {
			t.Fatalf("kept %q, restarts %d", rep.Kept, a.restarts)
		}
		_, order := providersIn(t, a, a.cfg.ownConfigPath())
		if !slices.Equal(order, []string{euName}) {
			t.Fatalf("providers = %v", order)
		}
		raw, err := entryBytes(renderedEntry(t, a))
		if err != nil {
			t.Fatal(err)
		}
		if rec := a.cfg.readRecord(binDir + "/" + a.cfg.files().Record); rec == nil || len(rec.Entries) != 0 || rec.holds(raw) {
			t.Fatalf("record = %+v, want one that vouches for nothing", rec)
		}
		assertPresent(t, a, binDir+"/"+defaultProviderName, configDir+"/"+filesFor(euName).CA)
		assertAbsent(t, a, configDir+"/harbor-bridge-ca.crt")

		// The other install's next pass drops nothing of its own and keeps
		// a's binary, which the empty record still makes a's.
		if err := run(b.cfg); err != nil {
			t.Fatal(err)
		}
		if _, order := providersIn(t, b, b.cfg.ownConfigPath()); !slices.Equal(order, []string{euName}) {
			t.Fatalf("providers after b's pass = %v", order)
		}
		mustRemove(t, a, resultKept)
	})
}

// TestRemove_RollbackAndRejection: a removal restart that does not verify
// is rolled back (ADR-0033): the environment file and the config are as
// before, the binary, record, CA and state stay, and the content is
// recorded as rejected. The same removal is refused without a restart; on
// another kubelet it is tried again.
func TestRemove_RollbackAndRejection(t *testing.T) {
	env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet"})
	withKubelet(t, env, "kubelet v1.34.0")
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	envBefore := env.hostFile(t, defaultKubeletPath)
	configBefore := env.hostFile(t, env.cfg.ownConfigPath())
	failRestarts(env, 2)

	rep, err := removeOnce(t, env)
	if err == nil || !strings.Contains(err.Error(), "runs on them again") || rep.Result != resultFailed {
		t.Fatalf("got %v, want a rolled-back removal", err)
	}
	if env.restarts != 3 {
		t.Fatalf("restarts = %d, want the install, the removal, the rollback", env.restarts)
	}
	if env.hostFile(t, defaultKubeletPath) != envBefore || env.hostFile(t, env.cfg.ownConfigPath()) != configBefore {
		t.Fatal("the rollback did not restore the environment file and the config")
	}
	if w, _ := discoverKubelet(env.cfg.ProcRoot); w != env.cfg.ownWiring() {
		t.Fatalf("kubelet after the rollback = %+v", w)
	}
	assertPresent(t, env, binDir+"/"+defaultProviderName, binDir+"/"+env.cfg.files().Record, configDir+"/harbor-bridge-ca.crt")
	if st := mustLoadState(t, env); st.Mode != modePatch || st.Rejected == nil || st.Rejected.Mode != modeRemove {
		t.Fatalf("state = %+v, want the install's record plus a rejected removal", st)
	}

	env.kubelet.statusFn = nil
	if _, err := removeOnce(t, env); err == nil || !strings.Contains(err.Error(), "already rejected") {
		t.Fatalf("got %v, want a refusal of the rejected removal", err)
	}
	if env.restarts != 3 {
		t.Fatal("the rejected removal restarted kubelet again")
	}
	// An install pass (the operator gave up on the uninstall) does not
	// count the rejected removal as its content.
	env.cfg.Action = actionInstall
	if err := run(env.cfg); err != nil || env.restarts != 3 {
		t.Fatalf("install after a rejected removal: %v, restarts %d", err, env.restarts)
	}

	writeHostFile(t, env, "/usr/bin/kubelet", "kubelet v1.35.0")
	mustRemove(t, env, resultRemoved)
	if env.restarts != 4 {
		t.Fatalf("restarts = %d, want one more on the new kubelet", env.restarts)
	}
}

// TestRemove_CrashWindows: an earlier removal pass that died after a write
// and before its verified restart, or after the restart and before the
// last file, leaves a node the next pass finishes (decision 2.8).
func TestRemove_CrashWindows(t *testing.T) {
	t.Run("flags removed, kubelet not restarted", func(t *testing.T) {
		env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet"})
		if err := run(env.cfg); err != nil {
			t.Fatal(err)
		}
		stripped, err := removeExtraArgs([]byte(env.hostFile(t, defaultKubeletPath)), binDir, env.cfg.ownConfigPath())
		if err != nil {
			t.Fatal(err)
		}
		writeHostFile(t, env, defaultKubeletPath, string(stripped))
		rep := mustRemove(t, env, resultRemoved)
		if !rep.KubeletRestarted || env.restarts != 2 {
			t.Fatalf("restarted %v, restarts %d", rep.KubeletRestarted, env.restarts)
		}
		if w, _ := discoverKubelet(env.cfg.ProcRoot); w.wired() {
			t.Fatalf("kubelet still wired: %+v", w)
		}
		assertAbsent(t, env, env.cfg.ownConfigPath(), binDir+"/"+defaultProviderName)
	})

	cloud := kubeletWiring{BinDir: "/cloud/bin", ConfigFile: "/cloud/config.yaml"}
	mergeInstall := func(t *testing.T) *testEnv {
		env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet", flagBinDir + "=" + cloud.BinDir, flagConfigFile + "=" + cloud.ConfigFile})
		writeCloudConfig(t, env, cloud.BinDir, cloud.ConfigFile, gkeConfig)
		if err := run(env.cfg); err != nil {
			t.Fatal(err)
		}
		return env
	}

	t.Run("entry removed, kubelet not restarted", func(t *testing.T) {
		env := mergeInstall(t)
		without, _, _, err := removeProvider([]byte(env.hostFile(t, cloud.ConfigFile)), defaultProviderName)
		if err != nil {
			t.Fatal(err)
		}
		writeHostFile(t, env, cloud.ConfigFile, string(without))
		rep := mustRemove(t, env, resultRemoved)
		if !rep.KubeletRestarted || env.restarts != 2 {
			t.Fatalf("restarted %v, restarts %d", rep.KubeletRestarted, env.restarts)
		}
		assertAbsent(t, env, ownFiles(env, cloud.BinDir)...)
	})

	t.Run("restart verified, files left", func(t *testing.T) {
		env := mergeInstall(t)
		mustRemove(t, env, resultRemoved)
		// The pass died after its verified restart: its state records the
		// removal, and the binary, record and state file are still there.
		t0 := removalTarget(cloud, "", defaultProviderName, []byte(env.hostFile(t, cloud.ConfigFile)), nil)
		if err := saveState(env.statePath(), t0.state()); err != nil {
			t.Fatal(err)
		}
		writeHostFile(t, env, cloud.BinDir+"/"+defaultProviderName, "ELF-fake-plugin")
		writeHostFile(t, env, cloud.BinDir+"/"+env.cfg.files().Record, recordOf(t, ""))
		mustRemove(t, env, resultRemoved)
		if env.restarts != 2 {
			t.Fatalf("restarts = %d: the verified removal restarted kubelet again", env.restarts)
		}
		assertAbsent(t, env, ownFiles(env, cloud.BinDir)...)
	})
}

// TestRemove_RefusesWhileTheFlagsAreNotLive: kubelet runs with other
// directories than the environment file names, because an install pass
// died before its restart; cleaning the files kubelet reads at its next
// start would keep it from starting (decision 3.3).
func TestRemove_RefusesWhileTheFlagsAreNotLive(t *testing.T) {
	env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet"})
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	moved, err := mergeExtraArgs([]byte(env.hostFile(t, defaultKubeletPath)), "/opt/hb/bin", "/opt/hb/config/"+configFileName)
	if err != nil {
		t.Fatal(err)
	}
	writeHostFile(t, env, defaultKubeletPath, string(moved))
	if _, err := removeOnce(t, env); err == nil || !strings.Contains(err.Error(), "died between writing the flags and restarting kubelet") {
		t.Fatalf("got %v, want a refusal", err)
	}
	if env.restarts != 1 || env.hostFile(t, defaultKubeletPath) != string(moved) {
		t.Fatal("the refused removal changed the node")
	}
	assertPresent(t, env, env.cfg.ownConfigPath(), binDir+"/"+defaultProviderName)
}

// TestRemove_LeavesWhatIsNotItsOwn: a file of another program under this
// install's name, and a symlink where a .bak would be, stay; the removal
// reports them (decision 2.6).
func TestRemove_LeavesWhatIsNotItsOwn(t *testing.T) {
	env := withName(t, newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet"}), euName)
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	f := env.cfg.files()
	if err := os.Remove(env.cfg.hostPath(binDir + "/" + f.Record)); err != nil {
		t.Fatal(err)
	}
	writeHostFile(t, env, binDir+"/"+f.Binary, "OTHER-PROGRAM")
	writeHostFile(t, env, "/etc/passwd", "root:x:0:0")
	if err := os.Symlink(env.cfg.hostPath("/etc/passwd"), env.cfg.hostPath(configDir+"/"+f.CA+".bak")); err != nil {
		t.Fatal(err)
	}
	rep := mustRemove(t, env, resultKept)
	if got := env.hostFile(t, binDir+"/"+f.Binary); got != "OTHER-PROGRAM" {
		t.Fatalf("another program's file was replaced: %q", got)
	}
	if env.hostFile(t, "/etc/passwd") != "root:x:0:0" {
		t.Fatal("the symlink's target changed")
	}
	if len(rep.Kept) != 2 {
		t.Fatalf("kept = %q", rep.Kept)
	}
	assertAbsent(t, env, configDir+"/"+f.CA, env.cfg.ownConfigPath())
}

// TestRemove_WaitsForTheNodeLock: a removal does not interleave with
// another install's pass on the node.
func TestRemove_WaitsForTheNodeLock(t *testing.T) {
	env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet"})
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockFile(env.cfg.hostPath(nodeLockPath), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	env.cfg.Action = actionRemove
	done := make(chan error, 1)
	go func() {
		_, err := runRemove(env.cfg)
		done <- err
	}()
	time.Sleep(3 * lockPollInterval)
	select {
	case err := <-done:
		t.Fatalf("removal finished while the node lock was held (err %v)", err)
	default:
	}
	unlock()
	awaitPass(t, done)
	assertAbsent(t, env, binDir+"/"+defaultProviderName)
}

// TestRemove_ReleaseInRecords: every record names the release that wrote
// it (decision 4), for the rename of decision 6.
func TestRemove_ReleaseInRecords(t *testing.T) {
	env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet"})
	env.cfg.ReleaseID = "harbor-bridge-system/harbor-bridge"
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	if rec := env.cfg.readRecord(binDir + "/" + env.cfg.files().Record); rec == nil || rec.Release != env.cfg.ReleaseID {
		t.Fatalf("record = %+v", rec)
	}
}

// TestRunSync_IdlesOnRemove: the sync container of a removal pod copies
// nothing back.
func TestRunSync_IdlesOnRemove(t *testing.T) {
	env := newTestEnv(t, modeNone, nil)
	env.cfg.Action = actionRemove
	env.cfg.SyncInterval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runSync(ctx, env.cfg) }()
	time.Sleep(50 * time.Millisecond)
	assertAbsent(t, env, configDir+"/harbor-bridge-ca.crt")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestRemovalReport pins the report's JSON form, which the uninstall hook
// reads (testdata/removal-report.golden.json, read by its tests too), and
// keeps a long one within kubelet's termination message limit.
func TestRemovalReport(t *testing.T) {
	rep := removalReport{
		Action: actionRemove, Provider: "harbor-bridge-eu", Result: resultKept, KubeletRestarted: true,
		Kept: []string{"/etc/kubernetes/credential-provider/harbor-bridge-eu: none mode keeps the binary"},
	}
	golden, err := os.ReadFile(filepath.Join("testdata", "removal-report.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := rep.encode(); string(got) != string(golden) {
		t.Fatalf("report = %s, golden = %s", got, golden)
	}

	long := removalReport{Action: actionRemove, Provider: "p", Result: resultFailed, Error: strings.Repeat("x", 9000)}
	for range 100 {
		long.Kept = append(long.Kept, strings.Repeat("k", 100))
	}
	raw := long.encode()
	var back removalReport
	if len(raw) > maxReportBytes+1 || json.Unmarshal(raw, &back) != nil || back.Result != resultFailed || back.Error == "" {
		t.Fatalf("long report: %d bytes, %+v", len(raw), back)
	}

	path := filepath.Join(t.TempDir(), "termination-log")
	writeReport(path, &removalReport{Action: actionRemove, Provider: "p", Result: resultNothing}, os.ErrPermission)
	if err := json.Unmarshal(mustRead(t, path), &back); err != nil || back.Result != resultFailed || back.Error != os.ErrPermission.Error() {
		t.Fatalf("report of a failed pass = %+v, %v", back, err)
	}
}

func TestLoadConfig_ActionAndRelease(t *testing.T) {
	env := func(vals map[string]string) func(string) string {
		return func(k string) string { return vals[k] }
	}
	base := func(k, v string) map[string]string {
		return map[string]string{"HOST_BIN_DIR": "/b", "HOST_CONFIG_DIR": "/c", k: v}
	}
	c, err := loadConfig(env(base("", "")))
	if err != nil || c.Action != actionInstall || c.ReleaseID != "" || c.TerminationLog != "/dev/termination-log" {
		t.Fatalf("defaults = %+v, %v", c, err)
	}
	for _, ok := range []map[string]string{
		base("INSTALL_ACTION", "remove"), base("RELEASE_ID", "harbor-bridge-system/harbor-bridge"),
		base("RELEASE_ID", "a/b.c-1"),
	} {
		if _, err := loadConfig(env(ok)); err != nil {
			t.Errorf("%v refused: %v", ok, err)
		}
	}
	for _, bad := range []map[string]string{
		base("INSTALL_ACTION", "uninstall"), base("RELEASE_ID", "no-namespace"),
		base("RELEASE_ID", "a/b/c"), base("RELEASE_ID", "A/b"), base("RELEASE_ID", "a/"+strings.Repeat("b", 54)),
	} {
		if _, err := loadConfig(env(bad)); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

// TestRemove_KeepsTheBinaryOfAForeignEntry: kubelet's config holds an entry
// of this install's name that is not a bridge entry; kubelet runs the file
// of that name for it, so the removal keeps it, also for the default name,
// whose file it otherwise takes for its own.
func TestRemove_KeepsTheBinaryOfAForeignEntry(t *testing.T) {
	cloud := kubeletWiring{BinDir: "/cloud/bin", ConfigFile: "/cloud/config.yaml"}
	env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet", flagBinDir + "=" + cloud.BinDir, flagConfigFile + "=" + cloud.ConfigFile})
	writeCloudConfig(t, env, cloud.BinDir, cloud.ConfigFile, strings.Replace(gkeConfig, "auth-provider-gcp", defaultProviderName, 1))
	rep := mustRemove(t, env, resultKept)
	if rep.KubeletRestarted || env.restarts != 0 {
		t.Fatal("kubelet restarted")
	}
	if got := env.hostFile(t, cloud.BinDir+"/"+defaultProviderName); got != "CLOUD-BINARY" {
		t.Fatalf("the foreign provider's binary = %q", got)
	}
}
