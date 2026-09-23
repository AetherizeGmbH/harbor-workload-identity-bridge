// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testEnv wires a full fake node: a HostRoot tree, source volume
// files, a fake /proc, and a stubbed kubelet restart.
type testEnv struct {
	cfg      *config
	restarts int
}

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

	procs := map[int]struct {
		comm    string
		cmdline []string
	}{1: {comm: "systemd", cmdline: []string{"/sbin/init"}}}
	if kubeletCmdline != nil {
		procs[321] = struct {
			comm    string
			cmdline []string
		}{comm: "kubelet", cmdline: kubeletCmdline}
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
		ProviderName:     "harbor-bridge-plugin",
		KubeletUnit:      "kubelet",
		StateDir:         "/var/lib/harbor-bridge",
		ProcRoot:         fakeProc(t, procs),
	}}

	orig := restartKubelet
	restartKubelet = func(unit string) error {
		if unit != "kubelet" {
			t.Fatalf("unexpected unit %q", unit)
		}
		env.restarts++
		return nil
	}
	t.Cleanup(func() { restartKubelet = orig })
	return env
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
	env := newTestEnv(t, modePatch, nil)

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
	env := newTestEnv(t, modePatch, nil)
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
	env := newTestEnv(t, modePatch, nil)
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
	env := newTestEnv(t, modePatch, nil)
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	// Simulate the crash between write and restart on a previous pass:
	// files are current but the state file records nothing.
	if err := os.Remove(env.cfg.hostPath(filepath.Join(env.cfg.StateDir, stateFileName))); err != nil {
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
	cloudCfg := env.cfg.hostPath("/cloud/config.json")
	if err := os.MkdirAll(filepath.Dir(cloudCfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cloudCfg, []byte(eksConfig), 0o644); err != nil {
		t.Fatal(err)
	}

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
	cloudCfg := env.cfg.hostPath("/cloud/config.yaml")
	if err := os.MkdirAll(filepath.Dir(cloudCfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cloudCfg, []byte(gkeConfig), 0o644); err != nil {
		t.Fatal(err)
	}
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
	if _, err := os.Stat(env.cfg.hostPath(filepath.Join(env.cfg.StateDir, stateFileName))); err == nil {
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
	if _, err := loadConfig(env(base)); err != nil {
		t.Fatalf("defaults must validate: %v", err)
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
	dir := t.TempDir()
	want := &state{Mode: modePatch, BinDir: "/b", ConfigFile: "/c", AppliedHash: "h"}
	if err := saveState(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !got.matches(modePatch, "/b", "/c", "h") {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if got.matches(modeMerge, "/b", "/c", "h") {
		t.Fatal("matches must be mode-sensitive")
	}

	// Corrupt state must degrade to nil, not error.
	if err := os.WriteFile(filepath.Join(dir, stateFileName), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = loadState(dir)
	if err != nil || got != nil {
		t.Fatalf("corrupt state must load as nil, got %+v err %v", got, err)
	}

	// Absent state → nil, no error. (*state)(nil).matches must be safe.
	got, err = loadState(t.TempDir())
	if err != nil || got != nil {
		t.Fatalf("absent state must load as nil, got %+v err %v", got, err)
	}
	if got.matches(modePatch, "/b", "/c", "h") {
		t.Fatal("nil state must not match")
	}
}

func TestContentHash_PartBoundaries(t *testing.T) {
	if contentHash([]byte("ab"), []byte("c")) == contentHash([]byte("a"), []byte("bc")) {
		t.Fatal("hash must be sensitive to part boundaries")
	}
	if contentHash([]byte("x")) != contentHash([]byte("x")) {
		t.Fatal("hash must be deterministic")
	}
}
