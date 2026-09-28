// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testEnv wires a full fake node: a HostRoot tree, source volume
// files, a fake /proc, and a fake kubelet unit.
type testEnv struct {
	cfg      *config
	restarts int
	kubelet  *fakeKubelet
}

// fakeKubelet stands in for systemd + kubelet. A restart re-reads
// /etc/default/kubelet the way the real unit does (EnvironmentFile), so
// patch-mode verification sees the flags only if the installer wrote
// them; states scripts the ActiveState that `systemctl show` reports.
type fakeKubelet struct {
	t          *testing.T
	env        *testEnv
	baseArgs   []string
	restartErr error
	// states is consumed one per status() call; the last value repeats. An
	// active unit reports the main PID of the kubelet the last restart
	// started, any other state none.
	states []string
	// statusFn, when set, answers status() instead of states.
	statusFn func() unitStatus
	// pid is the main PID of the kubelet the last restart started.
	pid int
	// ignoreEnvFile models a unit that does not source /etc/default/kubelet.
	ignoreEnvFile bool
}

const fakeKubeletPID = 321

func (f *fakeKubelet) restart(unit string) error {
	if unit != "kubelet" {
		f.t.Fatalf("unexpected unit %q", unit)
	}
	f.env.restarts++
	if f.restartErr != nil {
		return f.restartErr
	}
	f.pid = fakeKubeletPID + f.env.restarts
	args := append([]string(nil), f.baseArgs...)
	if !f.ignoreEnvFile {
		if raw, err := os.ReadFile(f.env.cfg.hostPath(defaultKubeletPath)); err == nil {
			for _, line := range strings.Split(string(raw), "\n") {
				if v, ok := strings.CutPrefix(line, "KUBELET_EXTRA_ARGS="); ok {
					args = append(args, strings.Fields(strings.Trim(v, `"`))...)
				}
			}
		}
	}
	writeProcEntry(f.t, f.env.cfg.ProcRoot, fakeKubeletPID, procEntry{comm: "kubelet", cmdline: args})
	return nil
}

func (f *fakeKubelet) status(string) (unitStatus, error) {
	if f.statusFn != nil {
		return f.statusFn(), nil
	}
	st := "active"
	if len(f.states) > 0 {
		st = f.states[0]
		if len(f.states) > 1 {
			f.states = f.states[1:]
		}
	}
	if st != "active" {
		return unitStatus{ActiveState: st, NRestarts: -1}, nil
	}
	pid := f.pid
	if pid == 0 {
		pid = fakeKubeletPID
	}
	return unitStatus{ActiveState: st, MainPID: pid, NRestarts: -1}, nil
}

// newTestEnv builds a fake node. kubeletCmdline is the command line of the
// running kubelet; nil means no kubelet process at all, which only none
// mode and merge mode with explicit targets get by with (patch mode reads
// kubelet's wiring before it writes, checkRewire).
func newTestEnv(t *testing.T, mode string, kubeletCmdline []string) *testEnv {
	t.Helper()
	root := t.TempDir()
	src := t.TempDir()

	mustWrite := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(filepath.Join(src, "plugin", "harbor-bridge-plugin"), "ELF-fake-plugin")
	mustWrite(filepath.Join(src, "config", configFileName), renderedConfig)
	mustWrite(filepath.Join(src, "tls", "ca.crt"), "CA-PEM")
	mustWrite(filepath.Join(src, "mtls", "tls.crt"), "CLIENT-CERT")
	mustWrite(filepath.Join(src, "mtls", "tls.key"), "CLIENT-KEY")

	procs := map[int]procEntry{1: {comm: "systemd", cmdline: []string{"/sbin/init"}}}
	base := []string{"/usr/bin/kubelet"}
	if kubeletCmdline != nil {
		procs[fakeKubeletPID] = procEntry{comm: "kubelet", cmdline: kubeletCmdline}
		base = kubeletCmdline
	}

	env := &testEnv{cfg: &config{
		Mode:             mode,
		HostRoot:         root,
		HostBinDir:       "/etc/kubernetes/credential-provider",
		HostConfigDir:    "/etc/kubernetes/credential-provider-config",
		SourcePlugin:     filepath.Join(src, "plugin", "harbor-bridge-plugin"),
		SourceConfig:     filepath.Join(src, "config", configFileName),
		SourceCA:         filepath.Join(src, "tls", "ca.crt"),
		SourceClientCert: filepath.Join(src, "mtls", "tls.crt"),
		SourceClientKey:  filepath.Join(src, "mtls", "tls.key"),
		ProviderName:     defaultProviderName,
		KubeletUnit:      "kubelet",
		StateDir:         "/var/lib/harbor-bridge",
		ProcRoot:         fakeProc(t, procs),
		verify:           verifyTiming{timeout: time.Second, interval: time.Millisecond, settle: time.Millisecond},
		lockTimeout:      10 * time.Second,
	}}
	// Every systemd node has /run, where the node lock lives.
	if err := os.MkdirAll(env.cfg.hostPath("/run"), 0o755); err != nil {
		t.Fatal(err)
	}
	env.kubelet = &fakeKubelet{t: t, env: env, baseArgs: base}
	env.cfg.kubelet = env.kubelet
	return env
}

// statePath is where this env's installer keeps its state file.
func (e *testEnv) statePath() string {
	return e.cfg.hostPath(filepath.Join(e.cfg.StateDir, e.cfg.files().State))
}

func (e *testEnv) hostFile(t *testing.T, nodePath string) string {
	t.Helper()
	data, err := os.ReadFile(e.cfg.hostPath(nodePath))
	if err != nil {
		t.Fatalf("read %s: %v", nodePath, err)
	}
	return string(data)
}

func TestRun_PatchFirstInstallThenIdempotent(t *testing.T) {
	env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})

	if err := run(env.cfg); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if env.restarts != 1 {
		t.Fatalf("first install must restart kubelet once, got %d", env.restarts)
	}
	if got := env.hostFile(t, "/etc/kubernetes/credential-provider/harbor-bridge-plugin"); got != "ELF-fake-plugin" {
		t.Fatal("plugin binary not installed")
	}
	if got := env.hostFile(t, "/etc/kubernetes/credential-provider-config/"+configFileName); got != renderedConfig {
		t.Fatal("config not installed verbatim in patch mode")
	}
	if got := env.hostFile(t, "/etc/kubernetes/credential-provider-config/harbor-bridge-ca.crt"); got != "CA-PEM" {
		t.Fatal("CA not installed")
	}
	envFile := env.hostFile(t, defaultKubeletPath)
	if !strings.Contains(envFile, flagBinDir+"=/etc/kubernetes/credential-provider") {
		t.Fatalf("kubelet env file not patched:\n%s", envFile)
	}

	// Second pass: nothing changed → no restart.
	if err := run(env.cfg); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if env.restarts != 1 {
		t.Fatalf("no-op rerun must not restart kubelet, got %d restarts", env.restarts)
	}
}

func TestRun_PatchPreservesOperatorArgs(t *testing.T) {
	env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	envPath := env.cfg.hostPath(defaultKubeletPath)
	if err := os.MkdirAll(filepath.Dir(envPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte("KUBELET_EXTRA_ARGS=\"--max-pods=42\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	got := env.hostFile(t, defaultKubeletPath)
	if !strings.Contains(got, "--max-pods=42") {
		t.Fatalf("operator arg clobbered:\n%s", got)
	}
	if !strings.Contains(got, flagConfigFile+"=") {
		t.Fatalf("our flag missing:\n%s", got)
	}
}

func TestRun_PatchConfigChangeRestarts(t *testing.T) {
	env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	// helm upgrade: rendered config content changes.
	updated := strings.Replace(renderedConfig, `"harbor.example.com"`, `"harbor-alt.example.com"`, 1)
	if err := os.WriteFile(env.cfg.SourceConfig, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	if env.restarts != 2 {
		t.Fatalf("config content change must restart kubelet, got %d restarts", env.restarts)
	}
}

func TestRun_PatchCrashWindowRetriesRestart(t *testing.T) {
	env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	// Simulate the crash between write and restart on a previous pass:
	// files are current but the state file records nothing.
	if err := os.Remove(env.statePath()); err != nil {
		t.Fatal(err)
	}
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	if env.restarts != 2 {
		t.Fatalf("missing state must force a restart, got %d restarts", env.restarts)
	}
}

func TestRun_AutoResolvesToMergeAndPreservesCloudProvider(t *testing.T) {
	env := newTestEnv(t, modeAuto, []string{
		"/usr/bin/kubelet",
		"--image-credential-provider-bin-dir=/cloud/bin",
		"--image-credential-provider-config=/cloud/config.json",
	})
	writeCloudConfig(t, env, "/cloud/bin", "/cloud/config.json", eksConfig)

	if err := run(env.cfg); err != nil {
		t.Fatalf("merge run: %v", err)
	}
	if env.restarts != 1 {
		t.Fatalf("first merge must restart kubelet once, got %d", env.restarts)
	}
	if got := env.hostFile(t, "/cloud/bin/harbor-bridge-plugin"); got != "ELF-fake-plugin" {
		t.Fatal("plugin binary not installed into discovered bin dir")
	}
	merged := env.hostFile(t, "/cloud/config.json")
	if !strings.Contains(merged, "ecr-credential-provider") {
		t.Fatalf("cloud provider entry lost:\n%s", merged)
	}
	if !strings.Contains(merged, "harbor-bridge-plugin") {
		t.Fatalf("our entry missing:\n%s", merged)
	}
	if !strings.Contains(merged, "unknownVendorField") {
		t.Fatalf("unknown vendor field dropped:\n%s", merged)
	}
	if !isJSON([]byte(merged)) {
		t.Fatalf("JSON cloud config must stay JSON:\n%s", merged)
	}
	// Our config file in HostConfigDir must NOT be written in merge
	// mode — kubelet reads the cloud file.
	if _, err := os.Stat(env.cfg.hostPath("/etc/kubernetes/credential-provider-config/" + configFileName)); err == nil {
		t.Fatal("merge mode must not write the chart-owned config file")
	}

	// Idempotent second pass.
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	if env.restarts != 1 {
		t.Fatalf("no-op merge rerun must not restart, got %d", env.restarts)
	}
}

func TestRun_AutoResolvesToPatchWithoutFlags(t *testing.T) {
	env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet", "--max-pods=110"})
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	if env.restarts != 1 {
		t.Fatalf("auto→patch must restart once, got %d", env.restarts)
	}
	if got := env.hostFile(t, defaultKubeletPath); !strings.Contains(got, flagBinDir) {
		t.Fatalf("auto→patch did not patch kubelet env:\n%s", got)
	}
}

func TestRun_MergeWithoutFlagsFailsLoudly(t *testing.T) {
	env := newTestEnv(t, modeMerge, []string{"/usr/bin/kubelet"})
	err := run(env.cfg)
	if err == nil {
		t.Fatal("merge without kubelet flags must fail")
	}
	if !strings.Contains(err.Error(), "nothing to merge into") {
		t.Fatalf("error must be actionable, got: %v", err)
	}
	if env.restarts != 0 {
		t.Fatal("failed merge must not restart kubelet")
	}
}

func TestRun_MergeExplicitOverridesSkipDiscovery(t *testing.T) {
	// No kubelet process at all — overrides must not need discovery.
	env := newTestEnv(t, modeMerge, nil)
	env.cfg.MergeBinDir = "/cloud/bin"
	env.cfg.MergeConfigFile = "/cloud/config.yaml"
	writeCloudConfig(t, env, "/cloud/bin", "/cloud/config.yaml", gkeConfig)
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	merged := env.hostFile(t, "/cloud/config.yaml")
	if !strings.Contains(merged, "auth-provider-gcp") || !strings.Contains(merged, "harbor-bridge-plugin") {
		t.Fatalf("override merge failed:\n%s", merged)
	}
}

func TestRun_NoneInstallsFilesOnly(t *testing.T) {
	env := newTestEnv(t, modeNone, nil)
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	if env.restarts != 0 {
		t.Fatal("mode none must never restart kubelet")
	}
	if got := env.hostFile(t, "/etc/kubernetes/credential-provider/harbor-bridge-plugin"); got != "ELF-fake-plugin" {
		t.Fatal("plugin binary not installed")
	}
	if _, err := os.Stat(env.cfg.hostPath(defaultKubeletPath)); err == nil {
		t.Fatal("mode none must not touch /etc/default/kubelet")
	}
	if _, err := os.Stat(env.statePath()); err == nil {
		t.Fatal("mode none must not write a state file")
	}
}

func TestRun_MTLSFilesInstalled(t *testing.T) {
	env := newTestEnv(t, modeNone, nil)
	env.cfg.MTLSEnabled = true
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	if got := env.hostFile(t, "/etc/kubernetes/credential-provider-config/harbor-bridge-client.crt"); got != "CLIENT-CERT" {
		t.Fatal("client cert not installed")
	}
	keyPath := env.cfg.hostPath("/etc/kubernetes/credential-provider-config/harbor-bridge-client.key")
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("client key mode = %o, want 0600", perm)
	}
}

func TestRun_NodeIPSubstitution(t *testing.T) {
	env := newTestEnv(t, modeNone, nil)
	withPlaceholder := strings.Replace(renderedConfig,
		"https://127.0.0.1:31443", "https://$(NODE_IP):31443", 1)
	if err := os.WriteFile(env.cfg.SourceConfig, []byte(withPlaceholder), 0o644); err != nil {
		t.Fatal(err)
	}
	env.cfg.NodeIP = "192.0.2.9"
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	got := env.hostFile(t, "/etc/kubernetes/credential-provider-config/"+configFileName)
	if !strings.Contains(got, "https://192.0.2.9:31443") {
		t.Fatalf("NODE_IP not substituted:\n%s", got)
	}
}

func TestLoadConfig_Validation(t *testing.T) {
	env := func(vals map[string]string) func(string) string {
		return func(k string) string { return vals[k] }
	}
	base := map[string]string{
		"HOST_BIN_DIR":    "/b",
		"HOST_CONFIG_DIR": "/c",
	}
	withBase := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	for name, extra := range map[string]map[string]string{
		"relative host dir":    {"HOST_BIN_DIR": "etc/kubernetes"},
		"dot-dot host dir":     {"HOST_CONFIG_DIR": "/etc/../tmp"},
		"relative state dir":   {"STATE_DIR": "var/lib/x"},
		"shell in unit name":   {"KUBELET_UNIT": "kubelet; reboot"},
		"option as unit name":  {"KUBELET_UNIT": "--help"},
		"relative merge paths": {"INSTALL_MODE": "merge", "INSTALL_MERGE_BIN_DIR": "bin", "INSTALL_MERGE_CONFIG_FILE": "/c.yaml"},
		"provider name dot":    {"PROVIDER_NAME": "harbor.bridge"},
		"provider name slash":  {"PROVIDER_NAME": "../kubelet"},
	} {
		if _, err := loadConfig(env(withBase(extra))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// plugin.hostConfigDir is writable by every release's sync container:
	// no bin dir or state dir in reach of it.
	for name, extra := range map[string]map[string]string{
		"bin dir equals config dir":        {"HOST_BIN_DIR": "/c"},
		"bin dir inside config dir":        {"HOST_BIN_DIR": "/c/bin"},
		"config dir inside bin dir":        {"HOST_BIN_DIR": "/c", "HOST_CONFIG_DIR": "/c/config"},
		"merge bin dir inside config dir":  {"INSTALL_MODE": "merge", "INSTALL_MERGE_BIN_DIR": "/c/bin", "INSTALL_MERGE_CONFIG_FILE": "/x.yaml"},
		"merge bin dir equals config dir":  {"INSTALL_MODE": "merge", "INSTALL_MERGE_BIN_DIR": "/c", "INSTALL_MERGE_CONFIG_FILE": "/x.yaml"},
		"merge config inside config dir":   {"INSTALL_MODE": "merge", "INSTALL_MERGE_BIN_DIR": "/x", "INSTALL_MERGE_CONFIG_FILE": "/c/credential-provider-config.yaml"},
		"merge config deep in config dir":  {"INSTALL_MODE": "merge", "INSTALL_MERGE_BIN_DIR": "/x", "INSTALL_MERGE_CONFIG_FILE": "/c/a/b.yaml"},
		"state dir equals config dir":      {"STATE_DIR": "/c"},
		"state dir inside config dir":      {"STATE_DIR": "/c/state"},
		"default state dir in config dir":  {"HOST_CONFIG_DIR": "/var/lib"},
		"config dir inside bin dir (deep)": {"HOST_BIN_DIR": "/opt", "HOST_CONFIG_DIR": "/opt/a/b"},
	} {
		if _, err := loadConfig(env(withBase(extra))); err == nil || !strings.Contains(err.Error(), "must not be") {
			t.Errorf("%s: got %v, want an overlap refusal", name, err)
		}
	}
	// Per path segment: a shared string prefix is no overlap, and the
	// state dir may be a parent of the config dir.
	for name, extra := range map[string]map[string]string{
		"chart defaults":             {"HOST_BIN_DIR": "/etc/kubernetes/credential-provider", "HOST_CONFIG_DIR": "/etc/kubernetes/credential-provider-config"},
		"config dir name extends":    {"HOST_BIN_DIR": "/c", "HOST_CONFIG_DIR": "/cc"},
		"state dir above config dir": {"STATE_DIR": "/var/lib", "HOST_CONFIG_DIR": "/var/lib/hb"},
		"state dir name extends":     {"STATE_DIR": "/c-state"},
		"merge config next to it":    {"INSTALL_MODE": "merge", "INSTALL_MERGE_BIN_DIR": "/x", "INSTALL_MERGE_CONFIG_FILE": "/c.yaml"},
	} {
		if _, err := loadConfig(env(withBase(extra))); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
	if c, err := loadConfig(env(base)); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	} else if c.ProviderName != defaultProviderName || c.SourceConfig != "/config/credential-provider-config.yaml" {
		t.Fatalf("default provider name = %q, rendered config at %q", c.ProviderName, c.SourceConfig)
	}
	// A non-default name reads the rendered config where the chart puts it
	// for such a name and installers before ADR-0029 never look.
	if c, err := loadConfig(env(withBase(map[string]string{"PROVIDER_NAME": "harbor-bridge-eu"}))); err != nil ||
		c.ProviderName != "harbor-bridge-eu" || c.SourceConfig != "/config-v2/credential-provider-config.v2.yaml" {
		t.Fatalf("PROVIDER_NAME not taken: %+v, %v", c, err)
	}
	if _, err := loadConfig(env(map[string]string{"INSTALL_MODE": "yolo", "HOST_BIN_DIR": "/b", "HOST_CONFIG_DIR": "/c"})); err == nil {
		t.Fatal("invalid mode must fail")
	}
	if _, err := loadConfig(env(map[string]string{"HOST_BIN_DIR": "/b"})); err == nil {
		t.Fatal("missing HOST_CONFIG_DIR must fail")
	}
	if _, err := loadConfig(env(map[string]string{
		"INSTALL_MODE": "merge", "HOST_CONFIG_DIR": "/c", "INSTALL_MERGE_BIN_DIR": "/x",
	})); err == nil {
		t.Fatal("merge overrides must be set together")
	}
	// Merge mode does not require HOST_BIN_DIR (target dir is discovered).
	if _, err := loadConfig(env(map[string]string{"INSTALL_MODE": "merge", "HOST_CONFIG_DIR": "/c"})); err != nil {
		t.Fatalf("merge without HOST_BIN_DIR must validate: %v", err)
	}
}

func TestStateRoundtripAndCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "installer-state.json")
	want := target{mode: modePatch, binDir: "/b", configFile: "/c", entryHash: "e", fileHash: "f"}
	if err := saveState(path, want.state()); err != nil {
		t.Fatal(err)
	}
	got, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.matches(want) {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if got.AppliedHash != "f" || got.EntryHash != "e" {
		t.Fatalf("appliedHash must stay the whole-file hash and entryHash the entry hash: %+v", got)
	}
	if got.matchesLegacy(want) {
		t.Fatal("a record with an entry hash must not match as legacy")
	}
	otherMode := want
	otherMode.mode = modeMerge
	if got.matches(otherMode) {
		t.Fatal("matches must be mode-sensitive")
	}
	otherEntry := want
	otherEntry.entryHash = "e2"
	if got.matches(otherEntry) {
		t.Fatal("matches must compare the entry hash")
	}
	otherFile := want
	otherFile.fileHash = "f2"
	if !got.matches(otherFile) {
		t.Fatal("another install's change to the shared file must not count")
	}

	// A record without entryHash is from an installer before ADR-0029 (or
	// an older one after a rollback): it matches only the whole-file hash.
	legacy := &state{Mode: modePatch, BinDir: "/b", ConfigFile: "/c", AppliedHash: "f"}
	if err := saveState(path, legacy); err != nil {
		t.Fatal(err)
	}
	if got, err = loadState(path); err != nil {
		t.Fatal(err)
	}
	if got.matches(want) || !got.matchesLegacy(want) {
		t.Fatalf("legacy record: matches=%v matchesLegacy=%v", got.matches(want), got.matchesLegacy(want))
	}

	// Corrupt state must degrade to nil, not error.
	if err := os.WriteFile(path, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = loadState(path)
	if err != nil || got != nil {
		t.Fatalf("corrupt state must load as nil, got %+v err %v", got, err)
	}

	// Absent state → nil, no error. (*state)(nil).matches must be safe.
	got, err = loadState(filepath.Join(t.TempDir(), "installer-state.json"))
	if err != nil || got != nil {
		t.Fatalf("absent state must load as nil, got %+v err %v", got, err)
	}
	if got.matches(want) || got.matchesLegacy(want) {
		t.Fatal("nil state must not match")
	}
}

func TestContentHash_PartBoundaries(t *testing.T) {
	if contentHash([]byte("ab"), []byte("c")) == contentHash([]byte("a"), []byte("bc")) {
		t.Fatal("hash must be sensitive to part boundaries")
	}
	if first, second := contentHash([]byte("x")), contentHash([]byte("x")); first != second {
		t.Fatal("hash must be deterministic")
	}
}

// TestRun_AutoStaysPatchAfterOwnInstall is the audit H3 regression: after
// a patch-mode install, kubelet's flags point at our own paths. auto used
// to see "flags present" on the next re-roll, switch to merge, and restart
// kubelet for nothing.
func TestRun_AutoStaysPatchAfterOwnInstall(t *testing.T) {
	env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet", "--max-pods=110"})
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	if env.restarts != 1 {
		t.Fatalf("first install restarts = %d, want 1", env.restarts)
	}
	// The fake restart rewrote the kubelet cmdline with our flags.
	w, err := discoverKubelet(env.cfg.ProcRoot)
	if err != nil || !w.wired() {
		t.Fatalf("setup: kubelet not wired after install: %+v %v", w, err)
	}
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	if env.restarts != 1 {
		t.Fatalf("no-op re-roll restarted kubelet (restarts = %d)", env.restarts)
	}
	st, err := loadState(env.statePath())
	if err != nil || st == nil || st.Mode != modePatch {
		t.Fatalf("state mode = %+v (err %v), want patch", st, err)
	}
	if got := env.hostFile(t, env.cfg.ownConfigPath()); got != renderedConfig {
		t.Fatal("the chart-rendered config was rewritten (merge-normalised) on a no-op re-roll")
	}
}

// TestRun_RestartVerification_FailsOnCrashLoop is audit M6: systemctl
// restart succeeds even when kubelet then crash-loops. The install must
// fail and must not record success.
func TestRun_RestartVerification_FailsOnCrashLoop(t *testing.T) {
	env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	env.kubelet.states = []string{"activating", "failed"}
	err := run(env.cfg)
	if err == nil || !strings.Contains(err.Error(), "did not become stably active") {
		t.Fatalf("got %v, want a verification failure", err)
	}
	if st, _ := loadState(env.statePath()); st != nil {
		t.Fatal("success state recorded for an unverified restart")
	}
}

// TestRun_RestartVerification_FlapAfterActiveIsCaught: "active" once is not
// enough — the unit must still be active after the settle period.
func TestRun_RestartVerification_FlapAfterActiveIsCaught(t *testing.T) {
	env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	env.kubelet.states = []string{"active", "failed"}
	if err := run(env.cfg); err == nil {
		t.Fatal("a kubelet that fails right after reporting active passed verification")
	}
}

// TestRun_RestartVerification_AutoRestartedKubeletIsCaught: a unit with
// Restart=always never reports "failed" for a kubelet that exits at
// startup; after RestartSec it reports "active" for the next kubelet. That
// is not a verified restart, and no success may be recorded.
func TestRun_RestartVerification_AutoRestartedKubeletIsCaught(t *testing.T) {
	env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	env.cfg.verify = verifyTiming{timeout: 300 * time.Millisecond, interval: 10 * time.Millisecond, settle: 50 * time.Millisecond}
	loop := &crashLoopUnit{start: time.Now(), up: 20 * time.Millisecond, down: 20 * time.Millisecond}
	env.kubelet.statusFn = func() unitStatus {
		st, _ := loop.status("kubelet")
		return st
	}
	err := run(env.cfg)
	if err == nil || !strings.Contains(err.Error(), "did not become stably active") {
		t.Fatalf("got %v, want a verification failure", err)
	}
	if st, _ := loadState(env.statePath()); st != nil {
		t.Fatal("success state recorded for a crash-looping kubelet")
	}
}

// TestRun_PatchVerification_UnitIgnoresEnvFile: if the kubelet unit does not
// source /etc/default/kubelet, the flags never reach kubelet. Before, the
// install "succeeded" and the plugin was silently never invoked.
func TestRun_PatchVerification_UnitIgnoresEnvFile(t *testing.T) {
	env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	env.kubelet.ignoreEnvFile = true
	err := run(env.cfg)
	if err == nil || !strings.Contains(err.Error(), "does the kubelet unit source") {
		t.Fatalf("got %v, want an explanation that the flags did not reach kubelet", err)
	}
}

func TestRun_RestartError_IsReturnedAndNotRecorded(t *testing.T) {
	env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	env.kubelet.restartErr = errors.New("systemctl: unit not found")
	if err := run(env.cfg); err == nil {
		t.Fatal("restart failure swallowed")
	}
	if st, _ := loadState(env.statePath()); st != nil {
		t.Fatal("success state recorded although the restart failed")
	}
}

// TestRun_UnwritableStateDir_NoRestart: a state dir that cannot be written
// would otherwise restart kubelet on every re-roll forever.
func TestRun_UnwritableStateDir_NoRestart(t *testing.T) {
	env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	stateParent := filepath.Dir(env.cfg.hostPath(env.cfg.StateDir))
	if err := os.MkdirAll(stateParent, 0o755); err != nil {
		t.Fatal(err)
	}
	// A FILE where the state dir should be: never writable, even as root.
	if err := os.WriteFile(env.cfg.hostPath(env.cfg.StateDir), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(env.cfg); err == nil {
		t.Fatal("unwritable state dir accepted")
	}
	if env.restarts != 0 {
		t.Fatalf("kubelet restarted (%d) although the result could not be recorded", env.restarts)
	}
}

// TestRun_MergeRefusalLeavesNoHalfInstall: an unknown node config schema is
// refused BEFORE the binary lands in the cloud's bin dir.
func TestRun_MergeRefusalLeavesNoHalfInstall(t *testing.T) {
	env := newTestEnv(t, modeMerge, nil)
	env.cfg.MergeBinDir = "/cloud/bin"
	env.cfg.MergeConfigFile = "/cloud/config.yaml"
	cloudCfg := env.cfg.hostPath("/cloud/config.yaml")
	if err := os.MkdirAll(filepath.Dir(cloudCfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cloudCfg, []byte("apiVersion: kubelet.config.k8s.io/v9\nkind: CredentialProviderConfig\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(env.cfg); err == nil {
		t.Fatal("unknown schema accepted")
	}
	if _, err := os.Stat(env.cfg.hostPath("/cloud/bin/harbor-bridge-plugin")); err == nil {
		t.Fatal("binary installed although the merge was refused")
	}
}

func TestRun_PatchRefusalLeavesNoHalfInstall(t *testing.T) {
	env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	envPath := env.cfg.hostPath(defaultKubeletPath)
	if err := os.MkdirAll(filepath.Dir(envPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte(`KUBELET_EXTRA_ARGS="--node-labels='a=b c'"`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(env.cfg); err == nil {
		t.Fatal("unsafe quoting accepted")
	}
	if _, err := os.Stat(env.cfg.hostPath("/etc/kubernetes/credential-provider/harbor-bridge-plugin")); err == nil {
		t.Fatal("binary installed although the kubelet env file was refused")
	}
	if env.restarts != 0 {
		t.Fatal("kubelet restarted after a refusal")
	}
}

func TestRunSync_CopiesRotatedCAUntilCancelled(t *testing.T) {
	env := newTestEnv(t, modeNone, nil)
	env.cfg.SyncInterval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runSync(ctx, env.cfg) }()

	if err := os.WriteFile(env.cfg.SourceCA, []byte("ROTATED-CA"), 0o644); err != nil {
		t.Fatal(err)
	}
	caPath := env.cfg.hostPath("/etc/kubernetes/credential-provider-config/harbor-bridge-ca.crt")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, err := os.ReadFile(caPath); err == nil && string(b) == "ROTATED-CA" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("rotated CA never reached the host")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if env.restarts != 0 {
		t.Fatal("the sync loop restarted kubelet")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runSync returned %v on cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runSync did not stop on cancellation")
	}
}

func TestRunSync_RejectsNonPositiveInterval(t *testing.T) {
	env := newTestEnv(t, modeNone, nil)
	env.cfg.SyncInterval = 0
	if err := runSync(context.Background(), env.cfg); err == nil {
		t.Fatal("zero interval accepted")
	}
}

// TestRun_MergeRefusesAConfigDirectory: kubelet 1.34+ accepts a directory
// for --image-credential-provider-config. The installer does not merge
// into one; it says so before it writes anything next to it, not even a
// lock file.
func TestRun_MergeRefusesAConfigDirectory(t *testing.T) {
	env := newTestEnv(t, modeMerge, nil)
	env.cfg.MergeBinDir = "/cloud/bin"
	env.cfg.MergeConfigFile = "/cloud/providers.d"
	if err := os.MkdirAll(env.cfg.hostPath("/cloud/providers.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := run(env.cfg)
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("got %v, want a refusal naming the directory", err)
	}
	for _, p := range []string{"/cloud/providers.d.lock", "/cloud/bin/harbor-bridge-plugin"} {
		if _, err := os.Lstat(env.cfg.hostPath(p)); !os.IsNotExist(err) {
			t.Errorf("%s written although the merge was refused (err %v)", p, err)
		}
	}
}
