// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Several installs on one node (ADR-0029): per-install files, entry
// ownership in the shared kubelet config, and the locks.

const euName = "harbor-bridge-eu"

// renderedConfigFor is renderedConfig as the chart renders it for another
// install: its own provider name, CA file, registry, NodePort and audience.
func renderedConfigFor(name string) string {
	return strings.NewReplacer(
		"name: harbor-bridge-plugin", "name: "+name,
		"/harbor-bridge-ca.crt", "/"+filesFor(name).CA,
		`"harbor.example.com"`, `"`+name+`.example.com"`,
		"127.0.0.1:31443", "127.0.0.1:31444",
		`serviceAccountTokenAudience: "harbor-bridge"`, `serviceAccountTokenAudience: "`+name+`"`,
	).Replace(renderedConfig)
}

// newSiblingEnv is a second install on first's node: the same host root and
// kubelet, its own pod volumes (rendered config, CA) and provider name.
func newSiblingEnv(t *testing.T, first *testEnv, mode, name string) *testEnv {
	t.Helper()
	src := t.TempDir()
	for path, content := range map[string]string{
		"plugin/harbor-bridge-plugin": "ELF-fake-plugin",
		"config/" + configFileName:    renderedConfigFor(name),
		"tls/ca.crt":                  "CA-PEM-" + name,
		"mtls/tls.crt":                "CLIENT-CERT-" + name,
		"mtls/tls.key":                "CLIENT-KEY-" + name,
	} {
		p := filepath.Join(src, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := *first.cfg
	cfg.Mode = mode
	cfg.ProviderName = name
	cfg.SourcePlugin = filepath.Join(src, "plugin", "harbor-bridge-plugin")
	cfg.SourceConfig = filepath.Join(src, "config", configFileName)
	cfg.SourceCA = filepath.Join(src, "tls", "ca.crt")
	cfg.SourceClientCert = filepath.Join(src, "mtls", "tls.crt")
	cfg.SourceClientKey = filepath.Join(src, "mtls", "tls.key")
	env := &testEnv{cfg: &cfg}
	env.kubelet = &fakeKubelet{t: t, env: env, baseArgs: first.kubelet.baseArgs}
	env.cfg.kubelet = env.kubelet
	return env
}

// withName switches env to provider name, rendering the chart's config for it.
func withName(t *testing.T, env *testEnv, name string) *testEnv {
	t.Helper()
	env.cfg.ProviderName = name
	if err := os.WriteFile(env.cfg.SourceConfig, []byte(renderedConfigFor(name)), 0o644); err != nil {
		t.Fatal(err)
	}
	return env
}

// providersIn parses the providers of a CredentialProviderConfig on the
// node, keyed by name, and returns their names in file order.
func providersIn(t *testing.T, env *testEnv, nodePath string) (map[string]any, []string) {
	t.Helper()
	cfg := parseConfig(t, []byte(env.hostFile(t, nodePath)))
	byName := map[string]any{}
	var order []string
	for _, p := range cfg["providers"].([]any) {
		name := p.(map[string]any)["name"].(string)
		byName[name] = p
		order = append(order, name)
	}
	return byName, order
}

// renderedEntry is the entry env's installer should put into kubelet's config.
func renderedEntry(t *testing.T, env *testEnv) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(env.cfg.SourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := renderedProvider(raw, env.cfg.ProviderName)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func writeHostFile(t *testing.T, env *testEnv, nodePath, content string) {
	t.Helper()
	p := env.cfg.hostPath(nodePath)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeSiblingFiles puts the plugin binary and the record of another
// install named name into the chart's bin dir, as that install's installer
// does before it writes its entry, the one renderedConfigFor(name) holds.
func writeSiblingFiles(t *testing.T, env *testEnv, name string) {
	t.Helper()
	writeHostFile(t, env, filepath.Join(binDir, name), "ELF-fake-plugin")
	if err := os.Chmod(env.cfg.hostPath(filepath.Join(binDir, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	writeHostFile(t, env, filepath.Join(binDir, filesFor(name).Record),
		recordOf(t, configDir+"/"+configFileName, mustEntry(t, renderedConfigFor(name), name)))
}

// recordOf is the record (entryRecord) of an install that holds entries and
// writes them into the chart-owned config chartOwned ("" for a cloud's).
func recordOf(t *testing.T, chartOwned string, entries ...map[string]any) string {
	t.Helper()
	r := entryRecord{ChartOwnedConfig: chartOwned}
	for _, e := range entries {
		raw, err := entryBytes(e)
		if err != nil {
			t.Fatal(err)
		}
		r.Entries = append(r.Entries, raw)
	}
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// writeCloudConfig puts a cloud's credential-provider config doc at the
// node path configPath and an executable binary for each of its providers
// into cloudBinDir, as a managed node image has them.
func writeCloudConfig(t *testing.T, env *testEnv, cloudBinDir, configPath, doc string) {
	t.Helper()
	writeHostFile(t, env, configPath, doc)
	for _, p := range parseConfig(t, []byte(doc))["providers"].([]any) {
		bin := filepath.Join(cloudBinDir, p.(map[string]any)["name"].(string))
		writeHostFile(t, env, bin, "CLOUD-BINARY")
		if err := os.Chmod(env.cfg.hostPath(bin), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func assertAbsent(t *testing.T, env *testEnv, nodePaths ...string) {
	t.Helper()
	for _, p := range nodePaths {
		if _, err := os.Lstat(env.cfg.hostPath(p)); !os.IsNotExist(err) {
			t.Errorf("%s exists (err %v)", p, err)
		}
	}
}

const (
	binDir    = "/etc/kubernetes/credential-provider"
	configDir = "/etc/kubernetes/credential-provider-config"
	stateDir  = "/var/lib/harbor-bridge"
)

func TestRun_NonDefaultNameDerivesItsFiles(t *testing.T) {
	env := withName(t, newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"}), euName)
	env.cfg.MTLSEnabled = true
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	for nodePath, want := range map[string]string{
		binDir + "/harbor-bridge-eu":               "ELF-fake-plugin",
		configDir + "/harbor-bridge-eu.ca.crt":     "CA-PEM",
		configDir + "/harbor-bridge-eu.client.crt": "CLIENT-CERT",
		configDir + "/harbor-bridge-eu.client.key": "CLIENT-KEY",
		configDir + "/" + configFileName:           renderedConfigFor(euName),
	} {
		if got := env.hostFile(t, nodePath); got != want {
			t.Errorf("%s = %q, want %q", nodePath, got, want)
		}
	}
	if _, err := os.Stat(env.cfg.hostPath(stateDir + "/harbor-bridge-eu.installer-state.json")); err != nil {
		t.Errorf("per-install state file: %v", err)
	}
	// Nothing of the default install's files may appear.
	assertAbsent(t, env,
		binDir+"/harbor-bridge-plugin",
		configDir+"/harbor-bridge-ca.crt",
		configDir+"/harbor-bridge-client.crt",
		configDir+"/harbor-bridge-client.key",
		stateDir+"/installer-state.json",
	)
}

func TestRun_ProviderNameMustMatchTheRenderedEntry(t *testing.T) {
	env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	env.cfg.ProviderName = euName // the chart rendered harbor-bridge-plugin
	err := run(env.cfg)
	if err == nil || !strings.Contains(err.Error(), `no provider named "harbor-bridge-eu"`) {
		t.Fatalf("got %v, want a name mismatch", err)
	}
	if env.restarts != 0 {
		t.Fatal("kubelet restarted after a refusal")
	}
	assertAbsent(t, env, binDir+"/harbor-bridge-eu", configDir+"/"+configFileName)
}

// TestRun_PatchInstallsShareTheConfig: on a self-managed node every install
// patches kubelet to the same chart-owned config, and each owns only its
// own entry in it.
func TestRun_PatchInstallsShareTheConfig(t *testing.T) {
	a := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet"})
	b := newSiblingEnv(t, a, modeAuto, euName)
	configPath := configDir + "/" + configFileName

	if err := run(a.cfg); err != nil {
		t.Fatal(err)
	}
	if got := a.hostFile(t, configPath); got != renderedConfig {
		t.Fatal("a single install must write the rendered config verbatim")
	}
	if err := run(b.cfg); err != nil {
		t.Fatal(err)
	}
	if a.restarts != 1 || b.restarts != 1 {
		t.Fatalf("restarts a=%d b=%d, want 1 each", a.restarts, b.restarts)
	}
	byName, order := providersIn(t, a, configPath)
	if !reflect.DeepEqual(order, []string{defaultProviderName, euName}) {
		t.Fatalf("providers = %v", order)
	}
	if !reflect.DeepEqual(byName[defaultProviderName], renderedEntry(t, a)) {
		t.Fatal("the second install changed the first install's entry")
	}
	if got := b.hostFile(t, binDir+"/harbor-bridge-eu"); got != "ELF-fake-plugin" {
		t.Fatal("second binary missing")
	}
	env := a.hostFile(t, defaultKubeletPath)
	if strings.Count(env, flagConfigFile) != 1 || strings.Count(env, flagBinDir) != 1 {
		t.Fatalf("kubelet flags duplicated:\n%s", env)
	}

	// No-op re-rolls of either install: no restart, although the first
	// install's recorded state predates the second entry.
	if err := run(a.cfg); err != nil {
		t.Fatal(err)
	}
	if err := run(b.cfg); err != nil {
		t.Fatal(err)
	}
	if a.restarts != 1 || b.restarts != 1 {
		t.Fatalf("no-op re-rolls restarted kubelet: a=%d b=%d", a.restarts, b.restarts)
	}

	// helm upgrade of the first install: only its entry changes.
	bEntry := byName[euName]
	upgraded := strings.Replace(renderedConfig, `"harbor.example.com"`, `"harbor-alt.example.com"`, 1)
	if err := os.WriteFile(a.cfg.SourceConfig, []byte(upgraded), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(a.cfg); err != nil {
		t.Fatal(err)
	}
	if a.restarts != 2 {
		t.Fatalf("an entry change must restart kubelet, restarts = %d", a.restarts)
	}
	byName, _ = providersIn(t, a, configPath)
	if !reflect.DeepEqual(byName[euName], bEntry) {
		t.Fatal("upgrading the first install changed the second install's entry")
	}
	if !reflect.DeepEqual(byName[defaultProviderName], renderedEntry(t, a)) {
		t.Fatal("the upgrade did not reach the first install's entry")
	}
}

// TestRun_MergeInstallsKeepEachOtherAndTheCloudProvider: two of our
// providers plus the cloud's in one managed-node config.
func TestRun_MergeInstallsKeepEachOtherAndTheCloudProvider(t *testing.T) {
	a := newTestEnv(t, modeAuto, []string{
		"/usr/bin/kubelet",
		"--image-credential-provider-bin-dir=/cloud/bin",
		"--image-credential-provider-config=/cloud/config.json",
	})
	writeCloudConfig(t, a, "/cloud/bin", "/cloud/config.json", eksConfig)
	b := newSiblingEnv(t, a, modeAuto, euName)

	if err := run(a.cfg); err != nil {
		t.Fatal(err)
	}
	cloudEntry, _ := providersIn(t, a, "/cloud/config.json")
	if err := run(b.cfg); err != nil {
		t.Fatal(err)
	}
	merged := a.hostFile(t, "/cloud/config.json")
	if !isJSON([]byte(merged)) {
		t.Fatalf("JSON cloud config must stay JSON:\n%s", merged)
	}
	byName, order := providersIn(t, a, "/cloud/config.json")
	if !reflect.DeepEqual(order, []string{"ecr-credential-provider", defaultProviderName, euName}) {
		t.Fatalf("providers = %v", order)
	}
	if !reflect.DeepEqual(byName["ecr-credential-provider"], cloudEntry["ecr-credential-provider"]) {
		t.Fatal("the cloud provider's entry changed")
	}
	if !reflect.DeepEqual(byName[defaultProviderName], renderedEntry(t, a)) {
		t.Fatal("the second install changed the first install's entry")
	}
	for _, bin := range []string{"/cloud/bin/harbor-bridge-plugin", "/cloud/bin/harbor-bridge-eu"} {
		if got := a.hostFile(t, bin); got != "ELF-fake-plugin" {
			t.Fatalf("%s = %q", bin, got)
		}
	}

	if err := run(a.cfg); err != nil {
		t.Fatal(err)
	}
	if a.restarts != 1 || b.restarts != 1 {
		t.Fatalf("restarts a=%d b=%d, want 1 each", a.restarts, b.restarts)
	}

	// helm upgrade of the second install leaves the others alone.
	upgraded := strings.Replace(renderedConfigFor(euName), "31444", "31445", 1)
	if err := os.WriteFile(b.cfg.SourceConfig, []byte(upgraded), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(b.cfg); err != nil {
		t.Fatal(err)
	}
	after, _ := providersIn(t, a, "/cloud/config.json")
	for _, name := range []string{"ecr-credential-provider", defaultProviderName} {
		if !reflect.DeepEqual(after[name], byName[name]) {
			t.Errorf("upgrading the second install changed %s", name)
		}
	}
	if !reflect.DeepEqual(after[euName], renderedEntry(t, b)) {
		t.Fatal("the upgrade did not reach the second install's entry")
	}
}

func TestRun_NoneInstallsShareTheConfig(t *testing.T) {
	a := newTestEnv(t, modeNone, nil)
	b := newSiblingEnv(t, a, modeNone, euName)
	for _, env := range []*testEnv{a, b, a} {
		if err := run(env.cfg); err != nil {
			t.Fatal(err)
		}
	}
	_, order := providersIn(t, a, configDir+"/"+configFileName)
	if !reflect.DeepEqual(order, []string{defaultProviderName, euName}) {
		t.Fatalf("providers = %v", order)
	}
	if got := a.hostFile(t, configDir+"/harbor-bridge-eu.ca.crt"); got != "CA-PEM-"+euName {
		t.Fatalf("second install's CA = %q", got)
	}
	if got := a.hostFile(t, configDir+"/harbor-bridge-ca.crt"); got != "CA-PEM" {
		t.Fatalf("first install's CA = %q", got)
	}
	if a.restarts+b.restarts != 0 {
		t.Fatal("mode none restarted kubelet")
	}
}

// TestRun_RefusesToTakeOverAForeignProvider: a provider name that equals a
// cloud provider's must neither replace its entry nor its binary.
func TestRun_RefusesToTakeOverAForeignProvider(t *testing.T) {
	env := newTestEnv(t, modeMerge, nil)
	env.cfg.MergeBinDir = "/cloud/bin"
	env.cfg.MergeConfigFile = "/cloud/config.json"
	withName(t, env, "ecr-credential-provider")
	writeHostFile(t, env, "/cloud/config.json", eksConfig)
	writeHostFile(t, env, "/cloud/bin/ecr-credential-provider", "ECR-BINARY")

	err := run(env.cfg)
	if err == nil || !strings.Contains(err.Error(), "not a harbor-bridge plugin") {
		t.Fatalf("got %v, want a refusal", err)
	}
	if got := env.hostFile(t, "/cloud/bin/ecr-credential-provider"); got != "ECR-BINARY" {
		t.Fatal("the cloud provider's binary was overwritten")
	}
	if got := env.hostFile(t, "/cloud/config.json"); got != eksConfig {
		t.Fatal("the cloud config was changed")
	}
	if env.restarts != 0 {
		t.Fatal("kubelet restarted after a refusal")
	}
	// A refused pass writes no file of the install, not even the CA (only
	// lock files).
	assertAbsent(t, env, configDir+"/ecr-credential-provider.ca.crt", "/cloud/bin/ecr-credential-provider.entry")
}

// TestRun_RefusesToOverwriteAForeignBinary: GKE keeps kubelet itself in its
// credential-provider bin dir; plugin.providerName=kubelet must not
// replace it.
func TestRun_RefusesToOverwriteAForeignBinary(t *testing.T) {
	env := newTestEnv(t, modeAuto, []string{
		"/usr/bin/kubelet",
		"--image-credential-provider-bin-dir=/home/kubernetes/bin",
		"--image-credential-provider-config=/etc/srv/kubernetes/cri_auth_config.yaml",
	})
	withName(t, env, "kubelet")
	writeCloudConfig(t, env, "/home/kubernetes/bin", "/etc/srv/kubernetes/cri_auth_config.yaml", gkeConfig)
	writeHostFile(t, env, "/home/kubernetes/bin/kubelet", "KUBELET-BINARY")

	err := run(env.cfg)
	if err == nil || !strings.Contains(err.Error(), "does not belong to this plugin") {
		t.Fatalf("got %v, want a refusal", err)
	}
	if got := env.hostFile(t, "/home/kubernetes/bin/kubelet"); got != "KUBELET-BINARY" {
		t.Fatal("kubelet's binary was overwritten")
	}
	if got := env.hostFile(t, "/etc/srv/kubernetes/cri_auth_config.yaml"); got != gkeConfig {
		t.Fatal("the cloud config was changed")
	}
	// A refused pass writes no file of the install, not even the CA or the
	// record (only lock files).
	assertAbsent(t, env, configDir+"/kubelet.ca.crt", "/home/kubernetes/bin/kubelet.entry")

	// A bridge entry of that name in the config does not make the file
	// ours: in patch and none mode pods other than installers can write
	// that config.
	merged, _, err := mergeProvider([]byte(gkeConfig), renderedEntry(t, env))
	if err != nil {
		t.Fatal(err)
	}
	writeHostFile(t, env, "/etc/srv/kubernetes/cri_auth_config.yaml", string(merged))
	if err := run(env.cfg); err == nil || !strings.Contains(err.Error(), "does not belong to this plugin") {
		t.Fatalf("got %v, want a refusal although the config holds an entry of the name", err)
	}
	if got := env.hostFile(t, "/home/kubernetes/bin/kubelet"); got != "KUBELET-BINARY" {
		t.Fatal("kubelet's binary was overwritten")
	}
	writeHostFile(t, env, "/etc/srv/kubernetes/cri_auth_config.yaml", gkeConfig)

	// Our own bytes from an interrupted pass (binary written, entry not)
	// are not foreign.
	writeHostFile(t, env, "/home/kubernetes/bin/kubelet", "ELF-fake-plugin")
	if err := run(env.cfg); err != nil {
		t.Fatalf("a leftover of our own binary was refused: %v", err)
	}
	// From now on the record next to it vouches for the file, whatever
	// its version.
	if err := os.WriteFile(env.cfg.SourcePlugin, []byte("ELF-fake-plugin-v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(env.cfg); err != nil {
		t.Fatalf("upgrading our own binary was refused: %v", err)
	}
}

// TestRun_LegacyStateIsConvertedWithoutRestart: the first pass of this
// installer version on a node an older one installed must not restart
// kubelet (ADR-0029, decision 6).
func TestRun_LegacyStateIsConvertedWithoutRestart(t *testing.T) {
	for _, tc := range []struct {
		name   string
		env    func(t *testing.T) *testEnv
		legacy func(t *testing.T, e *testEnv) *state
	}{
		{
			name: "patch",
			env:  func(t *testing.T) *testEnv { return newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"}) },
			legacy: func(t *testing.T, e *testEnv) *state {
				cfgPath := configDir + "/" + configFileName
				return &state{Mode: modePatch, BinDir: binDir, ConfigFile: cfgPath,
					AppliedHash: contentHash([]byte(e.hostFile(t, cfgPath)), []byte(e.hostFile(t, defaultKubeletPath)))}
			},
		},
		{
			name: "merge",
			env: func(t *testing.T) *testEnv {
				e := newTestEnv(t, modeMerge, nil)
				e.cfg.MergeBinDir, e.cfg.MergeConfigFile = "/cloud/bin", "/cloud/config.yaml"
				writeCloudConfig(t, e, "/cloud/bin", "/cloud/config.yaml", gkeConfig)
				return e
			},
			legacy: func(t *testing.T, e *testEnv) *state {
				return &state{Mode: modeMerge, BinDir: "/cloud/bin", ConfigFile: "/cloud/config.yaml",
					AppliedHash: contentHash([]byte(e.hostFile(t, "/cloud/config.yaml")))}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := tc.env(t)
			if err := run(env.cfg); err != nil {
				t.Fatal(err)
			}
			// What the previous installer version left behind.
			if err := saveState(env.statePath(), tc.legacy(t, env)); err != nil {
				t.Fatal(err)
			}
			if err := run(env.cfg); err != nil {
				t.Fatal(err)
			}
			if env.restarts != 1 {
				t.Fatalf("a matching legacy state restarted kubelet (restarts = %d)", env.restarts)
			}
			st, err := loadState(env.statePath())
			if err != nil || st == nil || st.EntryHash == "" || st.AppliedHash != tc.legacy(t, env).AppliedHash {
				t.Fatalf("state not converted (entry hash added, whole-file hash kept): %+v (err %v)", st, err)
			}

			// A legacy state for other content still restarts.
			stale := tc.legacy(t, env)
			stale.AppliedHash = "stale"
			if err := saveState(env.statePath(), stale); err != nil {
				t.Fatal(err)
			}
			if err := run(env.cfg); err != nil {
				t.Fatal(err)
			}
			if env.restarts != 2 {
				t.Fatalf("a stale legacy state did not restart kubelet (restarts = %d)", env.restarts)
			}
		})
	}
}

// preADR0029State is the state record exactly as installers before
// ADR-0029 (0.10.0 and older, installer/state.go) read and write it. Their
// JSON decoding ignores fields they do not know, such as entryHash.
type preADR0029State struct {
	SchemaVersion int    `json:"schemaVersion"`
	Mode          string `json:"mode"`
	BinDir        string `json:"binDir"`
	ConfigFile    string `json:"configFile"`
	AppliedHash   string `json:"appliedHash"`
}

// preADR0029Hash is the hash an installer before ADR-0029 computes for a
// pass of env's install on the node as it is now: contentHash(rendered,
// desiredEnv) in patch mode, contentHash(merged) in merge mode (0.10.0,
// installer/run.go runPatch and runMerge).
func preADR0029Hash(t *testing.T, env *testEnv, mode, cloudConfig string) string {
	t.Helper()
	rendered, err := os.ReadFile(env.cfg.SourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	switch mode {
	case modePatch:
		desiredEnv, err := mergeExtraArgs([]byte(env.hostFile(t, defaultKubeletPath)), binDir, configDir+"/"+configFileName)
		if err != nil {
			t.Fatal(err)
		}
		return contentHash(rendered, desiredEnv)
	case modeMerge:
		merged, _, err := mergeProvider([]byte(env.hostFile(t, cloudConfig)), renderedEntry(t, env))
		if err != nil {
			t.Fatal(err)
		}
		return contentHash(merged)
	}
	t.Fatalf("mode %q", mode)
	return ""
}

// preADR0029Matches is the comparison an installer before ADR-0029 makes
// before it decides not to restart kubelet (state.matches in 0.10.0).
func preADR0029Matches(t *testing.T, env *testEnv, mode, binDir, configFile, hash string) bool {
	t.Helper()
	raw, err := os.ReadFile(env.statePath())
	if err != nil {
		t.Fatal(err)
	}
	var st preADR0029State
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	return st.SchemaVersion == 1 && st.Mode == mode && st.BinDir == binDir && st.ConfigFile == configFile && st.AppliedHash == hash
}

// stateCompatCases are a single default-name install in patch and in merge
// mode, with the paths an installer before ADR-0029 records.
var stateCompatCases = []struct {
	mode, binDir, configFile string
	env                      func(t *testing.T) *testEnv
}{
	{
		mode: modePatch, binDir: binDir, configFile: configDir + "/" + configFileName,
		env: func(t *testing.T) *testEnv { return newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"}) },
	},
	{
		mode: modeMerge, binDir: "/cloud/bin", configFile: "/cloud/config.yaml",
		env: func(t *testing.T) *testEnv {
			e := newTestEnv(t, modeMerge, nil)
			e.cfg.MergeBinDir, e.cfg.MergeConfigFile = "/cloud/bin", "/cloud/config.yaml"
			writeCloudConfig(t, e, "/cloud/bin", "/cloud/config.yaml", gkeConfig)
			return e
		},
	},
}

// TestRun_ForwardUpgradeFromPreADR0029StateDoesNotRestart: the first pass
// of this installer on a node that 0.10.0 installed finds the state file
// as 0.10.0 wrote it and does not restart kubelet.
func TestRun_ForwardUpgradeFromPreADR0029StateDoesNotRestart(t *testing.T) {
	for _, tc := range stateCompatCases {
		t.Run(tc.mode, func(t *testing.T) {
			env := tc.env(t)
			if err := run(env.cfg); err != nil {
				t.Fatal(err)
			}
			// 0.10.0 wrote no record next to the binary.
			record := tc.binDir + "/" + filesFor(defaultProviderName).Record
			if err := os.Remove(env.cfg.hostPath(record)); err != nil {
				t.Fatal(err)
			}
			// Replace the state with the one 0.10.0 writes for the same
			// files: its fields only, its hash.
			old := preADR0029State{SchemaVersion: 1, Mode: tc.mode, BinDir: tc.binDir, ConfigFile: tc.configFile,
				AppliedHash: preADR0029Hash(t, env, tc.mode, tc.configFile)}
			raw, err := json.MarshalIndent(old, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(env.statePath(), append(raw, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}

			if err := run(env.cfg); err != nil {
				t.Fatal(err)
			}
			if env.restarts != 1 {
				t.Fatalf("the upgrade restarted kubelet (restarts = %d)", env.restarts)
			}
			st, err := loadState(env.statePath())
			if err != nil || st == nil || st.EntryHash == "" || st.AppliedHash != old.AppliedHash {
				t.Fatalf("state = %+v (err %v), want the entry hash added and appliedHash kept", st, err)
			}
			// Other installs keep this install's entry only once its
			// record exists.
			if _, err := os.Lstat(env.cfg.hostPath(record)); err != nil {
				t.Fatalf("the upgrade did not write the record: %v", err)
			}
		})
	}
}

// TestRun_RollbackFindsItsOwnStateHash: an installer before ADR-0029 that
// runs after this one (a rollback of the chart or the plugin image) reads
// the state file this installer wrote. For a single, unchanged install its
// comparison must match, or the rollback restarts kubelet on every node.
func TestRun_RollbackFindsItsOwnStateHash(t *testing.T) {
	for _, tc := range stateCompatCases {
		t.Run(tc.mode, func(t *testing.T) {
			env := tc.env(t)
			// First install, then a no-op re-roll, then a helm upgrade
			// that changes the entry: every record this installer
			// writes must be readable by the old comparison.
			for i, pass := range []func(){
				func() {},
				func() {},
				func() {
					upgraded := strings.Replace(renderedConfig, `"harbor.example.com"`, `"harbor-alt.example.com"`, 1)
					if err := os.WriteFile(env.cfg.SourceConfig, []byte(upgraded), 0o644); err != nil {
						t.Fatal(err)
					}
				},
			} {
				pass()
				if err := run(env.cfg); err != nil {
					t.Fatal(err)
				}
				if !preADR0029Matches(t, env, tc.mode, tc.binDir, tc.configFile, preADR0029Hash(t, env, tc.mode, tc.configFile)) {
					raw, _ := os.ReadFile(env.statePath())
					t.Fatalf("pass %d: an installer before ADR-0029 would not find its own hash in\n%s", i, raw)
				}
			}
		})
	}
}

// TestRun_EntryHashDrivesRestartsWithSeveralInstalls: another install's
// entry changes the shared file, and with it the whole-file hash, but only
// a change of an install's own entry makes it restart kubelet. appliedHash
// keeps the whole-file hash of this install's last restart.
func TestRun_EntryHashDrivesRestartsWithSeveralInstalls(t *testing.T) {
	a := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	b := newSiblingEnv(t, a, modePatch, euName)
	if err := run(a.cfg); err != nil {
		t.Fatal(err)
	}
	aFirst, err := loadState(a.statePath())
	if err != nil || aFirst == nil {
		t.Fatalf("state: %+v (err %v)", aFirst, err)
	}
	if err := run(b.cfg); err != nil {
		t.Fatal(err)
	}
	fileNow := contentHash([]byte(a.hostFile(t, configDir+"/"+configFileName)), []byte(a.hostFile(t, defaultKubeletPath)))
	if fileNow == aFirst.AppliedHash {
		t.Fatal("precondition: the second entry must change the whole-file hash")
	}
	bState, err := loadState(b.statePath())
	if err != nil || bState == nil || bState.AppliedHash != fileNow {
		t.Fatalf("the second install's appliedHash must be the file it restarted kubelet for: %+v (err %v)", bState, err)
	}

	if err := run(a.cfg); err != nil {
		t.Fatal(err)
	}
	if a.restarts != 1 {
		t.Fatalf("another install's entry made this install restart kubelet (restarts = %d)", a.restarts)
	}
	if st, _ := loadState(a.statePath()); st == nil || *st != *aFirst {
		t.Fatalf("a pass without a restart rewrote the state: %+v, want %+v", st, aFirst)
	}

	upgraded := strings.Replace(renderedConfig, `"harbor.example.com"`, `"harbor-alt.example.com"`, 1)
	if err := os.WriteFile(a.cfg.SourceConfig, []byte(upgraded), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(a.cfg); err != nil {
		t.Fatal(err)
	}
	if a.restarts != 2 {
		t.Fatalf("a change of this install's entry must restart kubelet (restarts = %d)", a.restarts)
	}
	if err := run(b.cfg); err != nil {
		t.Fatal(err)
	}
	if b.restarts != 1 {
		t.Fatalf("the first install's entry change made the second install restart kubelet (restarts = %d)", b.restarts)
	}
}

// TestRun_TakesTheNodeLockBeforeReadingSharedFiles: while another
// installer holds the node lock, a pass neither reads nor writes; after
// the other installer's write it merges into the file as it is then.
func TestRun_TakesTheNodeLockBeforeReadingSharedFiles(t *testing.T) {
	env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	unlock, err := lockFile(env.cfg.hostPath(nodeLockPath), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- run(env.cfg) }()
	time.Sleep(3 * lockPollInterval)
	select {
	case err := <-done:
		t.Fatalf("pass finished while the node lock was held (err %v)", err)
	default:
	}
	assertAbsent(t, env, configDir+"/harbor-bridge-ca.crt", binDir+"/harbor-bridge-plugin")

	// Meanwhile, the lock holder installs its binary and its entry.
	writeSiblingFiles(t, env, euName)
	writeHostFile(t, env, configDir+"/"+configFileName, renderedConfigFor(euName))
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pass did not continue after the lock was released")
	}
	_, order := providersIn(t, env, configDir+"/"+configFileName)
	if !reflect.DeepEqual(order, []string{euName, defaultProviderName}) {
		t.Fatalf("providers = %v: the pass read the config before it held the lock", order)
	}
}

// TestRun_NoneTakesTheConfigLock: none mode cannot reach the node lock, so
// it serialises on the config lock that every mode takes.
func TestRun_NoneTakesTheConfigLock(t *testing.T) {
	env := newTestEnv(t, modeNone, nil)
	configPath := configDir + "/" + configFileName
	if err := os.MkdirAll(env.cfg.hostPath(configDir), 0o755); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockFile(env.cfg.hostPath(configPath+lockSuffix), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- run(env.cfg) }()
	time.Sleep(3 * lockPollInterval)
	select {
	case err := <-done:
		t.Fatalf("pass finished while the config lock was held (err %v)", err)
	default:
	}
	assertAbsent(t, env, binDir+"/harbor-bridge-plugin", configPath)
	writeSiblingFiles(t, env, euName)
	writeHostFile(t, env, configPath, renderedConfigFor(euName))
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pass did not continue after the lock was released")
	}
	_, order := providersIn(t, env, configPath)
	if !reflect.DeepEqual(order, []string{euName, defaultProviderName}) {
		t.Fatalf("providers = %v", order)
	}
}

// TestRun_ConcurrentInstallsBothLand: two installers starting at the same
// moment on a fresh node both end up in kubelet's config.
func TestRun_ConcurrentInstallsBothLand(t *testing.T) {
	for i := 0; i < 5; i++ {
		a := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet"})
		b := newSiblingEnv(t, a, modeAuto, euName)
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for j, env := range []*testEnv{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[j] = run(env.cfg)
			}()
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		byName, _ := providersIn(t, a, configDir+"/"+configFileName)
		if len(byName) != 2 || byName[defaultProviderName] == nil || byName[euName] == nil {
			names, _ := json.Marshal(byName)
			t.Fatalf("run %d: providers after concurrent installs: %s", i, names)
		}
	}
}

// TestRun_ChartOwnedConfigDropsPlantedEntries: plugin.hostConfigDir, where
// the chart-owned config of patch and none mode lives, is writable by the
// sync container of every release and by any pod with a hostPath on it;
// the bin dir is not. Installers before ADR-0029 replaced that file with
// the rendered config on every pass, which removed anything planted there.
// An entry planted next to ours must still not survive a pass, and in
// patch mode the installer must restart kubelet onto the clean file, not
// keep the planted entry for its next restart.
func TestRun_ChartOwnedConfigDropsPlantedEntries(t *testing.T) {
	configPath := configDir + "/" + configFileName
	// A bridge entry with a valid name but no binary in the bin dir.
	plantedNoBinary := strings.Replace(plantedEntry, "name: harbor-bridge-plugin.bak", "name: evil", 1)
	for _, tc := range []struct {
		mode         string
		wantRestarts int
	}{{modePatch, 2}, {modeNone, 0}} {
		t.Run(tc.mode, func(t *testing.T) {
			env := newTestEnv(t, tc.mode, []string{"/usr/bin/kubelet"})
			if err := run(env.cfg); err != nil {
				t.Fatal(err)
			}
			// A plugin upgrade leaves the old binary as <name>.bak in the
			// bin dir: a working copy of our plugin under another name.
			if err := os.WriteFile(env.cfg.SourcePlugin, []byte("ELF-fake-plugin-v2"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := run(env.cfg); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(env.cfg.hostPath(binDir + "/harbor-bridge-plugin.bak")); err != nil {
				t.Fatalf("precondition: %v", err)
			}

			writeHostFile(t, env, configPath, renderedConfig+plantedEntry+plantedNoBinary)
			if err := run(env.cfg); err != nil {
				t.Fatal(err)
			}
			if got := env.hostFile(t, configPath); got != renderedConfig {
				t.Fatalf("planted entries survived the pass:\n%s", got)
			}
			if env.restarts != tc.wantRestarts {
				t.Fatalf("restarts = %d, want %d (onto the clean file)", env.restarts, tc.wantRestarts)
			}
		})
	}

	// With another install on the node, its entry stays and the planted
	// ones still go.
	a := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	b := newSiblingEnv(t, a, modePatch, euName)
	for _, env := range []*testEnv{a, b} {
		if err := run(env.cfg); err != nil {
			t.Fatal(err)
		}
	}
	before, order := providersIn(t, a, configPath)
	planted := mustEntry(t, "apiVersion: kubelet.config.k8s.io/v1\nproviders:\n"+plantedNoBinary, "evil")
	writeHostFile(t, a, configPath, string(configWith(t,
		before[order[0]].(map[string]any), before[order[1]].(map[string]any), planted)))
	if err := run(a.cfg); err != nil {
		t.Fatal(err)
	}
	after, afterOrder := providersIn(t, a, configPath)
	if !reflect.DeepEqual(afterOrder, order) || !reflect.DeepEqual(after, before) {
		t.Fatalf("providers = %v, want %v with their entries unchanged", afterOrder, order)
	}
	if a.restarts != 2 {
		t.Fatalf("restarts = %d, want 2", a.restarts)
	}
}

// TestSiblingIn: an entry in the chart-owned config counts as another
// install's only if it is exactly what that install's installer wrote:
// its binary and its record are in the bin dir, which no writer of
// plugin.hostConfigDir can write, and the record holds the entry.
func TestSiblingIn(t *testing.T) {
	env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	eu := mustEntry(t, renderedConfigFor(euName), euName)
	euBin := env.cfg.hostPath(binDir + "/" + euName)
	euRecord := binDir + "/" + filesFor(euName).Record
	record := recordOf(t, configDir+"/"+configFileName, eu)
	sibling := env.cfg.siblingIn(binDir)

	if sibling(eu) {
		t.Fatal("an entry without a binary counts")
	}
	writeHostFile(t, env, binDir+"/"+euName, "ELF-fake-plugin")
	writeHostFile(t, env, euRecord, record)
	if sibling(eu) {
		t.Fatal("an entry whose binary kubelet cannot execute counts")
	}
	if err := os.Chmod(euBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if !sibling(eu) {
		t.Fatal("another install's entry does not count")
	}

	// Without its record, or with a record of other content, a binary
	// alone vouches for nothing: an uninstalled release leaves its binary
	// behind, and every program in the bin dir is executable.
	if err := os.Remove(env.cfg.hostPath(euRecord)); err != nil {
		t.Fatal(err)
	}
	if sibling(eu) {
		t.Fatal("an entry without a record counts")
	}
	changed := mustEntry(t, strings.Replace(renderedConfigFor(euName), "127.0.0.1:31444", "203.0.113.7:443", 1), euName)
	writeHostFile(t, env, euRecord, record)
	if sibling(changed) {
		t.Fatal("an entry that differs from its record counts")
	}
	// A pass that died between its record and its config holds the entry
	// in the config and the one it meant to write: both count.
	upgraded := mustEntry(t, strings.Replace(renderedConfigFor(euName), "31444", "31445", 1), euName)
	writeHostFile(t, env, euRecord, recordOf(t, configDir+"/"+configFileName, eu, upgraded))
	if !sibling(eu) || !sibling(upgraded) {
		t.Fatal("an entry of a record with two entries does not count")
	}
	if sibling(changed) {
		t.Fatal("an entry that is in neither of the record's entries counts")
	}
	// A record that is not an installer's record vouches for nothing: the
	// bare entry, or something that does not parse.
	raw, err := entryBytes(eu)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{string(raw), "{nope", `{"entries":"x"}`} {
		writeHostFile(t, env, euRecord, bad)
		if sibling(eu) {
			t.Fatalf("an entry counts under the record %q", bad)
		}
	}
	writeHostFile(t, env, euRecord, record)
	// A symlinked record, even to the right content, is not a record an
	// installer wrote.
	writeHostFile(t, env, "/tmp/record", record)
	if err := os.Remove(env.cfg.hostPath(euRecord)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(env.cfg.hostPath("/tmp/record"), env.cfg.hostPath(euRecord)); err != nil {
		t.Fatal(err)
	}
	if sibling(eu) {
		t.Fatal("an entry whose record is a symlink counts")
	}
	if err := os.Remove(env.cfg.hostPath(euRecord)); err != nil {
		t.Fatal(err)
	}
	writeHostFile(t, env, euRecord, record)

	noBridge := map[string]any{}
	for k, v := range eu {
		noBridge[k] = v
	}
	delete(noBridge, "env")
	if sibling(noBridge) {
		t.Fatal("an entry that is not a bridge entry counts")
	}

	ours := renderedEntry(t, env)
	writeSiblingFiles(t, env, defaultProviderName)
	if sibling(ours) {
		t.Fatal("this install's own entry counts as another install's")
	}

	dotted := mustEntry(t, renderedConfigFor(defaultProviderName+".bak"), defaultProviderName+".bak")
	writeSiblingFiles(t, env, defaultProviderName+".bak")
	if sibling(dotted) {
		t.Fatal("an entry whose name is no provider name counts")
	}

	// A symlink in the bin dir is not a binary an install wrote.
	if err := os.Remove(euBin); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(env.cfg.hostPath(binDir+"/"+defaultProviderName), euBin); err != nil {
		t.Fatal(err)
	}
	if sibling(eu) {
		t.Fatal("an entry whose binary is a symlink counts")
	}
}

// TestRun_PatchDoesNotMoveKubeletAwayFromAnotherInstall: kubelet reads one
// config from one bin dir. A patch-mode install with other directories
// than an install already on the node would take the other install's
// entry out of kubelet's view, or, with only the bin dir different, leave
// kubelet without its binary. It refuses before it writes any file of the
// install.
func TestRun_PatchDoesNotMoveKubeletAwayFromAnotherInstall(t *testing.T) {
	for _, tc := range []struct {
		name              string
		binDir, configDir string
	}{
		{"other config and bin dir", "/etc/kubernetes/hb-eu-bin", "/etc/kubernetes/hb-eu"},
		{"other bin dir only", "/etc/kubernetes/hb-eu-bin", configDir},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
			if err := run(a.cfg); err != nil {
				t.Fatal(err)
			}
			envBefore := a.hostFile(t, defaultKubeletPath)
			configBefore := a.hostFile(t, configDir+"/"+configFileName)

			b := newSiblingEnv(t, a, modePatch, euName)
			b.cfg.HostBinDir, b.cfg.HostConfigDir = tc.binDir, tc.configDir
			err := run(b.cfg)
			if err == nil || !strings.Contains(err.Error(), `holds the provider entries "harbor-bridge-plugin" of other harbor-bridge installs`) {
				t.Fatalf("got %v, want a refusal naming the other install's entry", err)
			}
			if b.restarts != 0 {
				t.Fatal("kubelet restarted after a refusal")
			}
			assertAbsent(t, b, tc.binDir+"/"+euName, tc.configDir+"/harbor-bridge-eu.ca.crt")
			if a.hostFile(t, defaultKubeletPath) != envBefore || a.hostFile(t, configDir+"/"+configFileName) != configBefore {
				t.Fatal("the refused pass changed kubelet's flags or config")
			}
			got, err := discoverKubelet(a.cfg.ProcRoot)
			if err != nil || got != (kubeletWiring{BinDir: binDir, ConfigFile: configDir + "/" + configFileName}) {
				t.Fatalf("kubelet wiring = %+v (err %v)", got, err)
			}
		})
	}

	// A single install that changes its own directories still moves.
	a := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	if err := run(a.cfg); err != nil {
		t.Fatal(err)
	}
	a.cfg.HostBinDir, a.cfg.HostConfigDir = "/etc/kubernetes/hb-bin", "/etc/kubernetes/hb"
	if err := run(a.cfg); err != nil {
		t.Fatalf("moving the only install's directories was refused: %v", err)
	}
	got, err := discoverKubelet(a.cfg.ProcRoot)
	if err != nil || got != (kubeletWiring{BinDir: "/etc/kubernetes/hb-bin", ConfigFile: "/etc/kubernetes/hb/" + configFileName}) {
		t.Fatalf("kubelet wiring = %+v (err %v)", got, err)
	}
	if a.restarts != 2 {
		t.Fatalf("restarts = %d, want 2", a.restarts)
	}
}

// TestRun_PatchReadsAConfigDirectoryBeforeMovingKubelet: kubelet 1.34+ also
// reads a directory of config files. An install in none mode may have put
// its entry into one; patch mode reads them all before it moves kubelet.
func TestRun_PatchReadsAConfigDirectoryBeforeMovingKubelet(t *testing.T) {
	env := newTestEnv(t, modePatch, []string{
		"/usr/bin/kubelet",
		"--image-credential-provider-bin-dir=/opt/cp/bin",
		"--image-credential-provider-config=/opt/cp/providers.d",
	})
	withName(t, env, euName)
	writeHostFile(t, env, "/opt/cp/providers.d/00-cloud.yaml", gkeConfig)
	writeHostFile(t, env, "/opt/cp/providers.d/README", "not a config")
	if err := os.MkdirAll(env.cfg.hostPath("/opt/cp/providers.d/sub.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Without another install's entry in the directory, patch mode moves
	// kubelet to its own directories, as it always did.
	if err := checkRewire(env.cfg, kubeletWiring{BinDir: "/opt/cp/bin", ConfigFile: "/opt/cp/providers.d"}); err != nil {
		t.Fatalf("a directory without bridge entries was refused: %v", err)
	}

	writeHostFile(t, env, "/opt/cp/providers.d/10-bridge.yaml", renderedConfig)
	err := run(env.cfg)
	if err == nil || !strings.Contains(err.Error(), `holds the provider entries "harbor-bridge-plugin"`) {
		t.Fatalf("got %v, want a refusal", err)
	}

	// A symlink, which kubelet would follow, cannot be vetted.
	if err := os.Remove(env.cfg.hostPath("/opt/cp/providers.d/10-bridge.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("00-cloud.yaml", env.cfg.hostPath("/opt/cp/providers.d/10-bridge.yaml")); err != nil {
		t.Fatal(err)
	}
	err = run(env.cfg)
	if err == nil || !strings.Contains(err.Error(), "cannot tell whether") {
		t.Fatalf("got %v, want a refusal", err)
	}
	if env.restarts != 0 {
		t.Fatal("kubelet restarted after a refusal")
	}
}

// TestRun_PatchMovesKubeletAwayFromPlantedEntries: when kubelet's current
// config is in reach of the writers of a plugin.hostConfigDir, only the
// entries an installer recorded keep patch mode from moving kubelet. A
// planted entry must not: it would keep kubelet on a config where it runs
// at every kubelet start, and keep this install from its own config, where
// the installer drops it. A cloud's or hand-edited config stays the
// conservative case: every bridge entry in it counts.
func TestRun_PatchMovesKubeletAwayFromPlantedEntries(t *testing.T) {
	// plant puts an entry for the program name into the file nodePath and
	// an executable of that name into bin, without a record: a leftover
	// binary (writeFileAtomic leaves the previous plugin as <name>.bak),
	// or anything else kubelet can run.
	plant := func(t *testing.T, env *testEnv, bin, nodePath, name string) {
		t.Helper()
		writeHostFile(t, env, nodePath, "apiVersion: kubelet.config.k8s.io/v1\nkind: CredentialProviderConfig\nproviders:\n"+plantedAs(name))
		writeHostFile(t, env, bin+"/"+name, "ELF-fake-plugin")
		if err := os.Chmod(env.cfg.hostPath(bin+"/"+name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	own := kubeletWiring{BinDir: binDir, ConfigFile: configDir + "/" + configFileName}
	for _, tc := range []struct {
		name string
		// current is kubelet's wiring before the pass; prepare plants the
		// entries.
		current kubeletWiring
		prepare func(t *testing.T, env *testEnv)
	}{
		{"plugin.hostConfigDir as kubelet's config directory",
			kubeletWiring{BinDir: binDir, ConfigFile: configDir},
			func(t *testing.T, env *testEnv) {
				plant(t, env, binDir, configDir+"/evil.yaml", defaultProviderName+".bak")
				plant(t, env, binDir, configDir+"/other.yml", "evil")
			}},
		{"this install's own config, other bin dir",
			kubeletWiring{BinDir: "/opt/cp-bin", ConfigFile: own.ConfigFile},
			func(t *testing.T, env *testEnv) {
				plant(t, env, "/opt/cp-bin", own.ConfigFile, "evil")
			}},
		{"another release's chart-owned config",
			kubeletWiring{BinDir: "/opt/cp-bin", ConfigFile: "/etc/kubernetes/hb-eu/" + configFileName},
			func(t *testing.T, env *testEnv) {
				// The other release's entry is gone (a writer removed it);
				// its record in kubelet's bin dir still names the file.
				writeHostFile(t, env, "/opt/cp-bin/"+filesFor(euName).Record,
					recordOf(t, "/etc/kubernetes/hb-eu/"+configFileName, mustEntry(t, renderedConfigFor(euName), euName)))
				plant(t, env, "/opt/cp-bin", "/etc/kubernetes/hb-eu/"+configFileName, "evil")
			}},
		{"another release's plugin.hostConfigDir as kubelet's config directory",
			kubeletWiring{BinDir: "/opt/cp-bin", ConfigFile: "/etc/kubernetes/hb-eu"},
			func(t *testing.T, env *testEnv) {
				// A none-mode release keeps its chart-owned config in
				// that directory; its record names that file.
				writeHostFile(t, env, "/opt/cp-bin/"+filesFor(euName).Record,
					recordOf(t, "/etc/kubernetes/hb-eu/"+configFileName, mustEntry(t, renderedConfigFor(euName), euName)))
				plant(t, env, "/opt/cp-bin", "/etc/kubernetes/hb-eu/evil.yaml", "evil")
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, modePatch, []string{
				"/usr/bin/kubelet",
				"--image-credential-provider-bin-dir=" + tc.current.BinDir,
				"--image-credential-provider-config=" + tc.current.ConfigFile,
			})
			tc.prepare(t, env)
			if err := run(env.cfg); err != nil {
				t.Fatalf("a planted entry kept kubelet on its config: %v", err)
			}
			if got, err := discoverKubelet(env.cfg.ProcRoot); err != nil || got != own {
				t.Fatalf("kubelet wiring = %+v (err %v), want %+v", got, err, own)
			}
			if got := env.hostFile(t, own.ConfigFile); got != renderedConfig {
				t.Fatalf("this install's config holds more than its entry:\n%s", got)
			}
			if env.restarts != 1 {
				t.Fatalf("restarts = %d, want 1", env.restarts)
			}
		})
	}

	// The same planted entry in a cloud's config, which no record names and
	// which is outside plugin.hostConfigDir, still counts.
	env := newTestEnv(t, modePatch, []string{
		"/usr/bin/kubelet",
		"--image-credential-provider-bin-dir=/opt/cp-bin",
		"--image-credential-provider-config=/opt/cp/config.yaml",
	})
	plant(t, env, "/opt/cp-bin", "/opt/cp/config.yaml", "evil")
	err := run(env.cfg)
	if err == nil || !strings.Contains(err.Error(), `holds the provider entries "evil"`) {
		t.Fatalf("got %v, want a refusal", err)
	}
	if env.restarts != 0 {
		t.Fatal("kubelet restarted after a refusal")
	}
}

// TestRun_PatchLocksKubeletsCurrentConfigBeforeCheckingIt: a none-mode
// install takes only the config lock of the config it writes, and kubelet
// may read exactly that config (a file, or since Kubernetes 1.34 the
// directory it writes into, a layout the docs rule out but patch mode must
// not race either). Patch mode with other directories must wait
// for that lock before it reads the config (checkRewire), or it moves
// kubelet away from an entry the none-mode install adds meanwhile. That
// holds too when kubelet reads this install's own config (or the directory
// plugin.hostConfigDir) and only the bin dir differs: then the lock of the
// current config is this install's own config lock.
func TestRun_PatchLocksKubeletsCurrentConfigBeforeCheckingIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		// flag is kubelet's --image-credential-provider-config; the
		// none-mode install's plugin.hostConfigDir is noneDir.
		flag, noneDir string
	}{
		{"config file", "/opt/cp/" + configFileName, "/opt/cp"},
		{"config directory", "/opt/cp.d", "/opt/cp.d"},
		{"own config file, other bin dir", configDir + "/" + configFileName, configDir},
		{"own config directory, other bin dir", configDir, configDir},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newTestEnv(t, modePatch, []string{
				"/usr/bin/kubelet",
				"--image-credential-provider-bin-dir=/opt/cp-bin",
				"--image-credential-provider-config=" + tc.flag,
			})
			if err := os.MkdirAll(b.cfg.hostPath(tc.noneDir), 0o755); err != nil {
				t.Fatal(err)
			}
			noneConfig := tc.noneDir + "/" + configFileName
			// The none-mode install is in the middle of its pass.
			unlock, err := lockFile(b.cfg.hostPath(noneConfig+lockSuffix), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			done := make(chan error, 1)
			go func() { done <- run(b.cfg) }()
			time.Sleep(3 * lockPollInterval)
			select {
			case err := <-done:
				t.Fatalf("patch pass finished while the current config's lock was held (err %v)", err)
			default:
			}

			// It writes its record, its binary and its entry, as its
			// installer does, then releases the lock.
			writeHostFile(t, b, "/opt/cp-bin/"+filesFor(euName).Record,
				recordOf(t, noneConfig, mustEntry(t, renderedConfigFor(euName), euName)))
			writeHostFile(t, b, "/opt/cp-bin/"+euName, "ELF-fake-plugin")
			if err := os.Chmod(b.cfg.hostPath("/opt/cp-bin/"+euName), 0o755); err != nil {
				t.Fatal(err)
			}
			writeHostFile(t, b, noneConfig, renderedConfigFor(euName))
			unlock()
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("patch pass did not continue after the lock was released")
			}
			if err == nil || !strings.Contains(err.Error(), `holds the provider entries "harbor-bridge-eu"`) {
				t.Fatalf("got %v, want a refusal naming the entry written under the lock", err)
			}
			if b.restarts != 0 {
				t.Fatal("kubelet restarted after a refusal")
			}
			if got := b.hostFile(t, noneConfig); got != renderedConfigFor(euName) {
				t.Fatalf("the refused pass changed the config the holder wrote:\n%s", got)
			}
			assertAbsent(t, b, binDir+"/"+defaultProviderName, configDir+"/harbor-bridge-ca.crt", defaultKubeletPath)
			if noneConfig != configDir+"/"+configFileName {
				assertAbsent(t, b, configDir+"/"+configFileName)
			}
		})
	}
}

// waitBlocked starts a pass of env's installer and fails the test unless it
// is still waiting after a few lock polls. The returned channel delivers
// the pass's result.
func waitBlocked(t *testing.T, env *testEnv, what string) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- run(env.cfg) }()
	time.Sleep(3 * lockPollInterval)
	select {
	case err := <-done:
		t.Fatalf("pass finished while %s was held (err %v)", what, err)
	default:
	}
	return done
}

func awaitPass(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pass did not continue after the lock was released")
	}
}

// TestRun_PatchTakesTheConfigLock: with only the config lock held (a
// none-mode install mid-pass holds no node lock), a patch pass neither
// reads nor writes the chart-owned config, and afterwards keeps the entry
// the holder wrote.
func TestRun_PatchTakesTheConfigLock(t *testing.T) {
	env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	configPath := configDir + "/" + configFileName
	if err := os.MkdirAll(env.cfg.hostPath(configDir), 0o755); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockFile(env.cfg.hostPath(configPath+lockSuffix), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	done := waitBlocked(t, env, "the config lock")
	assertAbsent(t, env, configPath, binDir+"/"+defaultProviderName, configDir+"/harbor-bridge-ca.crt", defaultKubeletPath)

	writeSiblingFiles(t, env, euName)
	writeHostFile(t, env, configPath, renderedConfigFor(euName))
	unlock()
	awaitPass(t, done)
	_, order := providersIn(t, env, configPath)
	if !reflect.DeepEqual(order, []string{euName, defaultProviderName}) {
		t.Fatalf("providers = %v: the pass read the config before it held the lock", order)
	}
	if env.restarts != 1 {
		t.Fatalf("restarts = %d, want 1", env.restarts)
	}
}

// TestRun_AutoMergeTakesTheConfigLock: auto mode resolved to merge edits
// the cloud's config only under that config's lock, and merges into the
// file as the holder left it.
func TestRun_AutoMergeTakesTheConfigLock(t *testing.T) {
	env := newTestEnv(t, modeAuto, []string{
		"/usr/bin/kubelet",
		"--image-credential-provider-bin-dir=/cloud/bin",
		"--image-credential-provider-config=/cloud/config.yaml",
	})
	writeCloudConfig(t, env, "/cloud/bin", "/cloud/config.yaml", gkeConfig)
	unlock, err := lockFile(env.cfg.hostPath("/cloud/config.yaml"+lockSuffix), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	done := waitBlocked(t, env, "the cloud config's lock")
	if got := env.hostFile(t, "/cloud/config.yaml"); got != gkeConfig {
		t.Fatal("the cloud config changed while its lock was held")
	}
	assertAbsent(t, env, "/cloud/bin/"+defaultProviderName, configDir+"/harbor-bridge-ca.crt")

	// The holder installs its binary and merges its entry.
	holder := renderedEntry(t, withName(t, newSiblingEnv(t, env, modeAuto, euName), euName))
	merged, _, err := mergeProvider([]byte(gkeConfig), holder)
	if err != nil {
		t.Fatal(err)
	}
	writeHostFile(t, env, "/cloud/bin/"+euName, "ELF-fake-plugin")
	if err := os.Chmod(env.cfg.hostPath("/cloud/bin/"+euName), 0o755); err != nil {
		t.Fatal(err)
	}
	writeHostFile(t, env, "/cloud/config.yaml", string(merged))
	unlock()
	awaitPass(t, done)
	byName, order := providersIn(t, env, "/cloud/config.yaml")
	if len(order) != 3 || order[1] != euName || order[2] != defaultProviderName {
		t.Fatalf("providers = %v: the pass read the config before it held the lock", order)
	}
	if !reflect.DeepEqual(byName[euName], holder) {
		t.Fatal("the holder's entry changed")
	}
}

// TestRun_ConcurrentNoneAndPatchInstallsBothLand: a none-mode and a
// patch-mode install with the same directories, started at once, share
// the chart-owned config through its lock.
func TestRun_ConcurrentNoneAndPatchInstallsBothLand(t *testing.T) {
	for i := 0; i < 5; i++ {
		a := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
		b := newSiblingEnv(t, a, modeNone, euName)
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for j, env := range []*testEnv{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[j] = run(env.cfg)
			}()
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		byName, _ := providersIn(t, a, configDir+"/"+configFileName)
		if len(byName) != 2 || byName[defaultProviderName] == nil || byName[euName] == nil {
			names, _ := json.Marshal(byName)
			t.Fatalf("run %d: providers after concurrent installs: %s", i, names)
		}
	}
}

// probingKubelet is a fakeKubelet whose restart first tries each of locks
// (lock files, node paths) as another installer would, and records the
// ones it could take. The installer restarts kubelet after it wrote the
// config, so a lock free at the restart is one the pass did not hold
// through its read-modify-write of the config.
type probingKubelet struct {
	*fakeKubelet
	locks []string
	free  []string
}

func (p *probingKubelet) restart(unit string) error {
	for _, lock := range p.locks {
		if unlock, err := lockFile(p.env.cfg.hostPath(lock), 0); err == nil {
			unlock()
			p.free = append(p.free, lock)
		}
	}
	return p.fakeKubelet.restart(unit)
}

// probe makes env's kubelet a probingKubelet over locks.
func probe(env *testEnv, locks ...string) *probingKubelet {
	p := &probingKubelet{fakeKubelet: env.kubelet, locks: locks}
	env.cfg.kubelet = p
	return p
}

// TestRun_ConfigLocksAreHeldUntilThePassEnds: the config locks a pass takes
// guard the whole read-modify-write of the config, not only the read or the
// rewire check. A none-mode installer takes only the config lock; had the
// pass released it early, such an installer could write its entry between
// this pass's read and write, and the pass would drop that entry (ADR-0029,
// decision 5). The pass must still hold them when it restarts kubelet,
// after the config write.
func TestRun_ConfigLocksAreHeldUntilThePassEnds(t *testing.T) {
	ownLock := configDir + "/" + configFileName + lockSuffix
	t.Run("patch, own config", func(t *testing.T) {
		env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
		p := probe(env, ownLock)
		if err := run(env.cfg); err != nil {
			t.Fatal(err)
		}
		if env.restarts != 1 || len(p.free) != 0 {
			t.Fatalf("restarts = %d, locks free at the restart = %v", env.restarts, p.free)
		}
	})
	t.Run("patch, kubelet's current config and own config", func(t *testing.T) {
		env := newTestEnv(t, modePatch, []string{
			"/usr/bin/kubelet",
			"--image-credential-provider-bin-dir=/opt/cp-bin",
			"--image-credential-provider-config=/opt/cp/" + configFileName,
		})
		writeCloudConfig(t, env, "/opt/cp-bin", "/opt/cp/"+configFileName, gkeConfig)
		p := probe(env, "/opt/cp/"+configFileName+lockSuffix, ownLock)
		if err := run(env.cfg); err != nil {
			t.Fatal(err)
		}
		if got, err := discoverKubelet(env.cfg.ProcRoot); err != nil || got.ConfigFile != configDir+"/"+configFileName {
			t.Fatalf("precondition: kubelet was not moved to this install's config (wiring %+v, err %v)", got, err)
		}
		if env.restarts != 1 || len(p.free) != 0 {
			t.Fatalf("restarts = %d, locks free at the restart = %v", env.restarts, p.free)
		}
	})
	t.Run("merge into a chart-owned config", func(t *testing.T) {
		// A patch-mode release wired kubelet; a release in auto mode with
		// other directories merges into its chart-owned config, which the
		// none-mode installs of that directory lock too.
		a := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
		if err := run(a.cfg); err != nil {
			t.Fatal(err)
		}
		b := newSiblingEnv(t, a, modeAuto, euName)
		b.cfg.HostBinDir, b.cfg.HostConfigDir = "/etc/kubernetes/hb-eu-bin", "/etc/kubernetes/hb-eu"
		p := probe(b, ownLock)
		if err := run(b.cfg); err != nil {
			t.Fatal(err)
		}
		if b.restarts != 1 || len(p.free) != 0 {
			t.Fatalf("restarts = %d, locks free at the restart = %v", b.restarts, p.free)
		}
	})
	t.Run("merge into a cloud's config", func(t *testing.T) {
		env := newTestEnv(t, modeAuto, []string{
			"/usr/bin/kubelet",
			"--image-credential-provider-bin-dir=/cloud/bin",
			"--image-credential-provider-config=/cloud/config.yaml",
		})
		writeCloudConfig(t, env, "/cloud/bin", "/cloud/config.yaml", gkeConfig)
		p := probe(env, "/cloud/config.yaml"+lockSuffix)
		if err := run(env.cfg); err != nil {
			t.Fatal(err)
		}
		if env.restarts != 1 || len(p.free) != 0 {
			t.Fatalf("restarts = %d, locks free at the restart = %v", env.restarts, p.free)
		}
	})
}

// TestRun_MergeRefusesABinDirInReachOfHostConfigDir: kubelet's discovered
// bin dir must be out of reach of plugin.hostConfigDir's writers, like
// HOST_BIN_DIR (loadConfig).
func TestRun_MergeRefusesABinDirInReachOfHostConfigDir(t *testing.T) {
	env := newTestEnv(t, modeAuto, []string{
		"/usr/bin/kubelet",
		"--image-credential-provider-bin-dir=" + configDir + "/bin",
		"--image-credential-provider-config=/cloud/config.yaml",
	})
	writeHostFile(t, env, "/cloud/config.yaml", gkeConfig)
	err := run(env.cfg)
	if err == nil || !strings.Contains(err.Error(), "must not be the same directory or inside one another") {
		t.Fatalf("got %v, want a refusal", err)
	}
	if got := env.hostFile(t, "/cloud/config.yaml"); got != gkeConfig {
		t.Fatal("the cloud config was changed")
	}
	assertAbsent(t, env, configDir+"/bin/"+defaultProviderName, configDir+"/harbor-bridge-ca.crt")
	if env.restarts != 0 {
		t.Fatal("kubelet restarted after a refusal")
	}
}

// plantedAs is plantedEntry under the provider name name.
func plantedAs(name string) string {
	return strings.Replace(plantedEntry, "name: harbor-bridge-plugin.bak", "name: "+name, 1)
}

// TestRun_ChartOwnedConfigDropsEntriesThatDoNotMatchTheirRecord: a binary
// in the bin dir does not make an entry of its name another install's.
// Only the record an installer writes next to it does, and only for
// exactly the entry it records.
func TestRun_ChartOwnedConfigDropsEntriesThatDoNotMatchTheirRecord(t *testing.T) {
	configPath := configDir + "/" + configFileName
	for _, tc := range []struct {
		name string
		// prepare puts files into the bin dir and returns the entry
		// planted next to this install's.
		prepare func(t *testing.T, env *testEnv) string
	}{
		{"leftover binary without a record", func(t *testing.T, env *testEnv) string {
			// e.g. a bridge installed by hand before ADR-0029, removed
			// without cleaning the node.
			writeHostFile(t, env, binDir+"/harbor-bridge-old", "ELF-fake-plugin")
			if err := os.Chmod(env.cfg.hostPath(binDir+"/harbor-bridge-old"), 0o755); err != nil {
				t.Fatal(err)
			}
			return plantedAs("harbor-bridge-old")
		}},
		{"another program without a record", func(t *testing.T, env *testEnv) string {
			writeHostFile(t, env, binDir+"/sh", "#!/bin/busybox")
			if err := os.Chmod(env.cfg.hostPath(binDir+"/sh"), 0o755); err != nil {
				t.Fatal(err)
			}
			return plantedAs("sh")
		}},
		{"leftover binary with the record of another entry", func(t *testing.T, env *testEnv) string {
			// An uninstalled release leaves its binary and its record;
			// the entry it recorded is not the one planted now.
			writeSiblingFiles(t, env, euName)
			return plantedAs(euName)
		}},
	} {
		for _, mode := range []string{modePatch, modeNone} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				env := newTestEnv(t, mode, []string{"/usr/bin/kubelet"})
				if err := run(env.cfg); err != nil {
					t.Fatal(err)
				}
				planted := tc.prepare(t, env)
				writeHostFile(t, env, configPath, renderedConfig+planted)
				if err := run(env.cfg); err != nil {
					t.Fatal(err)
				}
				if got := env.hostFile(t, configPath); got != renderedConfig {
					t.Fatalf("the planted entry survived the pass:\n%s", got)
				}
				if want := map[string]int{modePatch: 2, modeNone: 0}[mode]; env.restarts != want {
					t.Fatalf("restarts = %d, want %d (onto the clean file)", env.restarts, want)
				}
			})
		}
	}
}

// TestRun_ChangedSiblingEntryIsDropped: a writer of plugin.hostConfigDir
// changes another install's entry in the shared file. The next installer
// must not keep the change, least of all restart kubelet onto it; the
// owning install's next pass writes its entry back.
func TestRun_ChangedSiblingEntryIsDropped(t *testing.T) {
	configPath := configDir + "/" + configFileName
	a := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	b := newSiblingEnv(t, a, modePatch, euName)
	for _, env := range []*testEnv{a, b} {
		if err := run(env.cfg); err != nil {
			t.Fatal(err)
		}
	}
	before, order := providersIn(t, a, configPath)
	changed := mustEntry(t, strings.Replace(renderedConfigFor(euName), "https://127.0.0.1:31444", "https://attacker.example", 1), euName)
	writeHostFile(t, a, configPath, string(configWith(t, before[order[0]].(map[string]any), changed)))

	if err := run(a.cfg); err != nil {
		t.Fatal(err)
	}
	if got := a.hostFile(t, configPath); got != renderedConfig {
		t.Fatalf("the changed entry survived the pass:\n%s", got)
	}
	if a.restarts != 2 {
		t.Fatalf("restarts = %d, want 2 (onto the file without the changed entry)", a.restarts)
	}

	if err := run(b.cfg); err != nil {
		t.Fatal(err)
	}
	after, afterOrder := providersIn(t, a, configPath)
	if !reflect.DeepEqual(afterOrder, []string{defaultProviderName, euName}) || !reflect.DeepEqual(after, before) {
		t.Fatalf("providers = %v, want both entries as their installers wrote them", afterOrder)
	}
	if b.restarts != 2 {
		t.Fatalf("the owning install must restart kubelet onto its entry again (restarts = %d)", b.restarts)
	}
}

// TestRun_OwnBinaryUpgradeAfterTheEntryVanished: a non-default install's
// binary stays its own while the config kubelet reads no longer holds its
// entry: its record in the bin dir proves it.
func TestRun_OwnBinaryUpgradeAfterTheEntryVanished(t *testing.T) {
	t.Run("patch moves plugin.hostConfigDir", func(t *testing.T) {
		env := withName(t, newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"}), euName)
		if err := run(env.cfg); err != nil {
			t.Fatal(err)
		}
		// helm upgrade: a new plugin and another config directory in one
		// pass. The new directory has no entry yet.
		env.cfg.HostConfigDir = "/etc/kubernetes/hb-eu"
		if err := os.WriteFile(env.cfg.SourcePlugin, []byte("ELF-fake-plugin-v2"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := run(env.cfg); err != nil {
			t.Fatalf("upgrading this install's own binary was refused: %v", err)
		}
		if got := env.hostFile(t, binDir+"/"+euName); got != "ELF-fake-plugin-v2" {
			t.Fatalf("binary = %q", got)
		}
		got, err := discoverKubelet(env.cfg.ProcRoot)
		if err != nil || got.ConfigFile != "/etc/kubernetes/hb-eu/"+configFileName {
			t.Fatalf("kubelet wiring = %+v (err %v)", got, err)
		}
	})
	t.Run("merge after the cloud rewrote its config", func(t *testing.T) {
		env := withName(t, newTestEnv(t, modeMerge, nil), euName)
		env.cfg.MergeBinDir, env.cfg.MergeConfigFile = "/cloud/bin", "/cloud/config.yaml"
		writeCloudConfig(t, env, "/cloud/bin", "/cloud/config.yaml", gkeConfig)
		if err := run(env.cfg); err != nil {
			t.Fatal(err)
		}
		// The node's own tooling rewrote the config without our entry,
		// and the next pass brings a new plugin.
		writeHostFile(t, env, "/cloud/config.yaml", gkeConfig)
		if err := os.WriteFile(env.cfg.SourcePlugin, []byte("ELF-fake-plugin-v2"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := run(env.cfg); err != nil {
			t.Fatalf("upgrading this install's own binary was refused: %v", err)
		}
		if got := env.hostFile(t, "/cloud/bin/"+euName); got != "ELF-fake-plugin-v2" {
			t.Fatalf("binary = %q", got)
		}
		byName, _ := providersIn(t, env, "/cloud/config.yaml")
		if !reflect.DeepEqual(byName[euName], renderedEntry(t, env)) {
			t.Fatal("the entry was not merged back")
		}
	})
}

// TestRun_RecordComesBeforeTheBinary: installPluginBinary writes the record
// before the binary. A pass that fails between the two must leave nothing a
// later pass takes for another program's file: had the binary come first, a
// non-default install whose pass died after it would refuse every later
// pass with a new plugin image (no record, other bytes), and only deleting
// the file by hand would get it out.
func TestRun_RecordComesBeforeTheBinary(t *testing.T) {
	for _, mode := range []string{modePatch, modeNone} {
		t.Run(mode, func(t *testing.T) {
			env := withName(t, newTestEnv(t, mode, []string{"/usr/bin/kubelet"}), euName)
			record := env.cfg.hostPath(binDir + "/" + filesFor(euName).Record)
			// Something in the way of the record makes its write fail.
			if err := os.MkdirAll(record, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := run(env.cfg); err == nil {
				t.Fatal("the pass succeeded although its record could not be written")
			}
			if err := os.Remove(record); err != nil {
				t.Fatal(err)
			}
			// The next pass brings a new plugin image.
			if err := os.WriteFile(env.cfg.SourcePlugin, []byte("ELF-fake-plugin-v2"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := run(env.cfg); err != nil {
				t.Fatalf("the pass after an interrupted one was refused: %v", err)
			}
			if got := env.hostFile(t, binDir+"/"+euName); got != "ELF-fake-plugin-v2" {
				t.Fatalf("binary = %q", got)
			}
		})
	}
}

// TestRun_RecordHoldsTheEntry: every pass records the entry it writes, next
// to the binary, readable by root only and not executable.
func TestRun_RecordHoldsTheEntry(t *testing.T) {
	for _, name := range []string{defaultProviderName, euName} {
		t.Run(name, func(t *testing.T) {
			env := withName(t, newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"}), name)
			if err := run(env.cfg); err != nil {
				t.Fatal(err)
			}
			recordPath := binDir + "/" + name + ".entry"
			fi, err := os.Lstat(env.cfg.hostPath(recordPath))
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode() != 0o600 {
				t.Fatalf("record mode = %v, want -rw-------", fi.Mode())
			}
			byName, _ := providersIn(t, env, configDir+"/"+configFileName)
			if got, want := env.hostFile(t, recordPath), recordOf(t, configDir+"/"+configFileName, byName[name].(map[string]any)); got != want {
				t.Fatalf("record = %s, want the entry in the config %s", got, want)
			}
		})
	}
}

// TestEntryRecord_RoundTripsEveryEntry: a record holds an entry's canonical
// bytes exactly, also for characters that JSON encoders escape.
func TestEntryRecord_RoundTripsEveryEntry(t *testing.T) {
	env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	entry := mustEntry(t, renderedConfig, defaultProviderName)
	entry["args"] = []any{"<a&b>", "line sep ", `quote " and \ backslash`, "ü"}
	raw, err := entryBytes(entry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.cfg.beginRecord(binDir, raw, ""); err != nil {
		t.Fatal(err)
	}
	if !env.cfg.readRecord(binDir + "/" + filesFor(defaultProviderName).Record).holds(raw) {
		t.Fatalf("the record does not hold %s", raw)
	}
}

// TestRun_InterruptedSiblingPassKeepsItsLiveEntry: another install's pass
// that changes its entry and dies after its record, before its config
// write, must not make the next installer drop that install's entry, which
// is still the one in the file, and restart kubelet without it.
func TestRun_InterruptedSiblingPassKeepsItsLiveEntry(t *testing.T) {
	configPath := configDir + "/" + configFileName
	a := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	b := newSiblingEnv(t, a, modePatch, euName)
	for _, env := range []*testEnv{a, b} {
		if err := run(env.cfg); err != nil {
			t.Fatal(err)
		}
	}
	before, order := providersIn(t, a, configPath)

	// helm upgrade of the second install; its pass fails at the binary,
	// after its record and before its config.
	upgraded := strings.Replace(renderedConfigFor(euName), "31444", "31445", 1)
	if err := os.WriteFile(b.cfg.SourceConfig, []byte(upgraded), 0o644); err != nil {
		t.Fatal(err)
	}
	plugin := b.cfg.SourcePlugin
	b.cfg.SourcePlugin = plugin + ".missing"
	if err := run(b.cfg); err == nil {
		t.Fatal("the second install's pass succeeded without its plugin")
	}
	if !reflect.DeepEqual(func() map[string]any { m, _ := providersIn(t, a, configPath); return m }(), before) {
		t.Fatal("precondition: the failed pass changed the config")
	}

	if err := run(a.cfg); err != nil {
		t.Fatal(err)
	}
	after, afterOrder := providersIn(t, a, configPath)
	if !reflect.DeepEqual(afterOrder, order) || !reflect.DeepEqual(after, before) {
		t.Fatalf("providers = %v, want %v unchanged", afterOrder, order)
	}
	if a.restarts != 1 {
		t.Fatalf("restarts = %d, want 1: nothing of the first install changed", a.restarts)
	}

	// The second install's next pass lands its entry and keeps only it in
	// its record.
	b.cfg.SourcePlugin = plugin
	if err := run(b.cfg); err != nil {
		t.Fatal(err)
	}
	after, _ = providersIn(t, a, configPath)
	if !reflect.DeepEqual(after[euName], renderedEntry(t, b)) {
		t.Fatal("the second install's entry was not updated")
	}
	if got, want := a.hostFile(t, binDir+"/"+filesFor(euName).Record), recordOf(t, configDir+"/"+configFileName, renderedEntry(t, b)); got != want {
		t.Fatalf("record = %s, want only the entry in the config %s", got, want)
	}
}

// TestRun_SiblingPassThatDiesAtItsConfigWriteKeepsItsLiveEntry: another
// install's pass changes its entry and fails at the config write itself,
// after its binary. Its record must still hold the entry that is in the
// file: a pass reduces its record to the new entry only once the config
// holds it (record.go). Had it done so before the config write, the next
// installer would drop that install's live entry and restart kubelet
// without it.
func TestRun_SiblingPassThatDiesAtItsConfigWriteKeepsItsLiveEntry(t *testing.T) {
	configPath := configDir + "/" + configFileName
	for _, tc := range []struct {
		name string
		mode string
		// dirs are the second install's plugin.hostBinaryDir and
		// plugin.hostConfigDir.
		binDir, configDir string
	}{
		{"patch", modePatch, binDir, configDir},
		{"none", modeNone, binDir, configDir},
		{"merge into a chart-owned config", modeAuto, "/etc/kubernetes/hb-eu-bin", "/etc/kubernetes/hb-eu"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
			b := newSiblingEnv(t, a, tc.mode, euName)
			b.cfg.HostBinDir, b.cfg.HostConfigDir = tc.binDir, tc.configDir
			for _, env := range []*testEnv{a, b} {
				if err := run(env.cfg); err != nil {
					t.Fatal(err)
				}
			}
			before, order := providersIn(t, a, configPath)
			bEntry := renderedEntry(t, b)

			// helm upgrade of the second install. Its config write fails:
			// a directory is in the way of the backup of the old config.
			upgraded := strings.Replace(renderedConfigFor(euName), "31444", "31445", 1)
			if err := os.WriteFile(b.cfg.SourceConfig, []byte(upgraded), 0o644); err != nil {
				t.Fatal(err)
			}
			bak := a.cfg.hostPath(configPath + ".bak")
			if err := os.RemoveAll(bak); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(bak, "in-the-way"), 0o755); err != nil {
				t.Fatal(err)
			}
			err := run(b.cfg)
			if err == nil || !strings.Contains(err.Error(), "write backup") {
				t.Fatalf("got %v, want the second install's config write to fail", err)
			}
			if !reflect.DeepEqual(func() map[string]any { m, _ := providersIn(t, a, configPath); return m }(), before) {
				t.Fatal("precondition: the failed pass changed the config")
			}
			if err := os.RemoveAll(bak); err != nil {
				t.Fatal(err)
			}

			if err := run(a.cfg); err != nil {
				t.Fatal(err)
			}
			after, afterOrder := providersIn(t, a, configPath)
			if !reflect.DeepEqual(afterOrder, order) || !reflect.DeepEqual(after, before) || !reflect.DeepEqual(after[euName], bEntry) {
				t.Fatalf("providers = %v, want %v unchanged: the second install's live entry was dropped", afterOrder, order)
			}
			if a.restarts != 1 {
				t.Fatalf("restarts = %d, want 1: nothing of the first install changed", a.restarts)
			}

			// The second install's next pass lands its entry and keeps only
			// it in its record.
			if err := run(b.cfg); err != nil {
				t.Fatal(err)
			}
			after, _ = providersIn(t, a, configPath)
			if !reflect.DeepEqual(after[euName], renderedEntry(t, b)) {
				t.Fatal("the second install's entry was not updated")
			}
			if got, want := a.hostFile(t, binDir+"/"+filesFor(euName).Record), recordOf(t, configPath, renderedEntry(t, b)); got != want {
				t.Fatalf("record = %s, want only the entry in the config %s", got, want)
			}
		})
	}
}

// TestRun_OlderInstallerCannotReadANonDefaultNamesConfig: for a non-default
// provider name the chart mounts the rendered config where installers
// before ADR-0029 never look (golden second-instance.yaml: the ConfigMap
// key credential-provider-config.v2.yaml at /config-v2). Such an installer
// (an older plugin image) ignores PROVIDER_NAME; had it read the config, it
// would install the entry of the new name without a binary of that name,
// and kubelet does not start with that entry.
func TestRun_OlderInstallerCannotReadANonDefaultNamesConfig(t *testing.T) {
	env := withName(t, newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"}), euName)
	pod := t.TempDir() // the install container's file system
	rendered, err := os.ReadFile(env.cfg.SourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(pod, v2SourceConfig)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pod, v2SourceConfig), rendered, 0o644); err != nil {
		t.Fatal(err)
	}

	// An installer before ADR-0029 starts every pass by reading the fixed
	// path /config/credential-provider-config.yaml (0.10.0:
	// installer/main.go loadConfig, installer/run.go run) and exits when
	// that fails. Its first step fails here, before any other.
	if _, err := os.ReadFile(filepath.Join(pod, legacySourceConfig)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the path installers before ADR-0029 read exists (err %v)", err)
	}
	older := *env.cfg
	older.ProviderName = defaultProviderName // it knows no PROVIDER_NAME
	older.SourceConfig = filepath.Join(pod, legacySourceConfig)
	if err := run(&older); err == nil || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("got %v, want the read of the rendered config to fail", err)
	}
	assertAbsent(t, env, binDir+"/"+defaultProviderName, binDir+"/"+euName, configDir+"/"+configFileName,
		configDir+"/harbor-bridge-ca.crt", defaultKubeletPath, stateDir+"/installer-state.json")
	if env.restarts != 0 {
		t.Fatal("kubelet restarted")
	}

	// This installer derives the path from PROVIDER_NAME.
	env.cfg.SourceConfig = filepath.Join(pod, sourceConfigPath(euName))
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	if got := env.hostFile(t, configDir+"/"+configFileName); got != string(rendered) {
		t.Fatalf("config = %q", got)
	}
}

// TestRun_NonDefaultNameRefusesTheLegacyLayout: a non-default name whose
// rendered config is missing at the ADR-0029 path means that the chart and
// the plugin image disagree about the layout; the pass says so and writes
// nothing.
func TestRun_NonDefaultNameRefusesTheLegacyLayout(t *testing.T) {
	env := withName(t, newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"}), euName)
	pod := t.TempDir()
	rendered, err := os.ReadFile(env.cfg.SourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(pod, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pod, legacySourceConfig), rendered, 0o644); err != nil {
		t.Fatal(err)
	}
	env.cfg.SourceConfig = filepath.Join(pod, sourceConfigPath(euName))
	err = run(env.cfg)
	if err == nil || !strings.Contains(err.Error(), "ADR-0029") || !strings.Contains(err.Error(), "plugin.image") {
		t.Fatalf("got %v, want a refusal naming the layout mismatch", err)
	}
	assertAbsent(t, env, binDir+"/"+euName, configDir+"/"+configFileName, configDir+"/harbor-bridge-eu.ca.crt", defaultKubeletPath)
}

// TestRun_MergeIntoAChartOwnedConfigKeepsOnlyRecordedEntries: merge mode
// can meet a chart-owned config of patch or none mode, which lives in a
// plugin.hostConfigDir that pods other than installers can write, and its
// pass may restart kubelet onto it. It must then keep only the entries the
// records in kubelet's bin dir vouch for, as patch and none mode do, not a
// changed or planted entry: this install's own config with merge mode set
// explicitly, and another release's config in auto mode with other
// directories.
func TestRun_MergeIntoAChartOwnedConfigKeepsOnlyRecordedEntries(t *testing.T) {
	configPath := configDir + "/" + configFileName
	for _, tc := range []struct {
		name string
		mode string
		// dirs are the second install's plugin.hostBinaryDir and
		// plugin.hostConfigDir.
		binDir, configDir string
	}{
		{"explicit merge, same directories", modeMerge, binDir, configDir},
		{"auto, other directories", modeAuto, "/etc/kubernetes/hb-eu-bin", "/etc/kubernetes/hb-eu"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
			if err := run(a.cfg); err != nil {
				t.Fatal(err)
			}
			aEntry := renderedEntry(t, a)
			// A writer of plugin.hostConfigDir points the first install's
			// entry at another endpoint, and plants an entry for a program
			// in the bin dir that no install recorded.
			changed := mustEntry(t, strings.Replace(renderedConfig, "https://127.0.0.1:31443", "https://203.0.113.7:443", 1), defaultProviderName)
			planted := mustEntry(t, "apiVersion: kubelet.config.k8s.io/v1\nproviders:\n"+plantedAs("evil"), "evil")
			writeHostFile(t, a, configPath, string(configWith(t, changed, planted)))
			writeHostFile(t, a, binDir+"/evil", "ELF-fake-plugin")
			if err := os.Chmod(a.cfg.hostPath(binDir+"/evil"), 0o755); err != nil {
				t.Fatal(err)
			}

			b := newSiblingEnv(t, a, tc.mode, euName)
			b.cfg.HostBinDir, b.cfg.HostConfigDir = tc.binDir, tc.configDir
			if err := run(b.cfg); err != nil {
				t.Fatal(err)
			}
			if got := a.hostFile(t, configPath); got != renderedConfigFor(euName) {
				t.Fatalf("the changed or the planted entry survived the merge pass:\n%s", got)
			}
			if b.restarts != 1 {
				t.Fatalf("restarts = %d, want 1 (onto the file without them)", b.restarts)
			}
			// Kubelet's bin dir holds the second install's binary and its
			// record, which names the chart-owned config it wrote into.
			if got, want := a.hostFile(t, binDir+"/"+filesFor(euName).Record), recordOf(t, configPath, renderedEntry(t, b)); got != want {
				t.Fatalf("record = %s, want %s", got, want)
			}

			// The first install's next pass writes its entry back and keeps
			// the second one's.
			if err := run(a.cfg); err != nil {
				t.Fatal(err)
			}
			byName, order := providersIn(t, a, configPath)
			if !reflect.DeepEqual(order, []string{euName, defaultProviderName}) ||
				!reflect.DeepEqual(byName[defaultProviderName], aEntry) || !reflect.DeepEqual(byName[euName], renderedEntry(t, b)) {
				t.Fatalf("providers = %v, want both entries as their installers wrote them", order)
			}
			// And a no-op pass of the second install keeps both.
			if err := run(b.cfg); err != nil {
				t.Fatal(err)
			}
			if after, _ := providersIn(t, a, configPath); !reflect.DeepEqual(after, byName) {
				t.Fatal("a no-op merge pass changed the chart-owned config")
			}
		})
	}
}

// TestRun_MergeRefusesAnotherConfigInHostConfigDir: a config in
// plugin.hostConfigDir other than the chart-owned one is not the chart's,
// and every release's sync container can write it.
func TestRun_MergeRefusesAnotherConfigInHostConfigDir(t *testing.T) {
	env := newTestEnv(t, modeAuto, []string{
		"/usr/bin/kubelet",
		"--image-credential-provider-bin-dir=/opt/cp-bin",
		"--image-credential-provider-config=" + configDir + "/hand-written.yaml",
	})
	writeCloudConfig(t, env, "/opt/cp-bin", configDir+"/hand-written.yaml", gkeConfig)
	err := run(env.cfg)
	if err == nil || !strings.Contains(err.Error(), "is inside plugin.hostConfigDir") {
		t.Fatalf("got %v, want a refusal", err)
	}
	if got := env.hostFile(t, configDir+"/hand-written.yaml"); got != gkeConfig {
		t.Fatal("the config was changed")
	}
	assertAbsent(t, env, "/opt/cp-bin/"+defaultProviderName, configDir+"/harbor-bridge-ca.crt", configDir+"/hand-written.yaml.lock")
	if env.restarts != 0 {
		t.Fatal("kubelet restarted after a refusal")
	}
}

// TestRun_MergeRefusesAConfigKubeletCannotStartWith: merge mode keeps the
// cloud's and other installs' entries, but does not write or restart
// kubelet onto a config that kubelet exits on: a provider whose binary is
// missing or not executable, a name that occurs twice, or a name kubelet
// refuses.
func TestRun_MergeRefusesAConfigKubeletCannotStartWith(t *testing.T) {
	planted := func(name string) string {
		return gkeConfig + strings.Replace(plantedEntry, "name: harbor-bridge-plugin.bak", "name: "+name, 1)
	}
	for _, tc := range []struct {
		name    string
		doc     string
		prepare func(t *testing.T, env *testEnv)
		want    string
	}{
		{"missing binary", planted("evil"), nil, `the binary of provider "evil" is missing`},
		{"binary not executable", planted("evil"), func(t *testing.T, env *testEnv) {
			writeHostFile(t, env, "/cloud/bin/evil", "#!/bin/sh")
		}, `the binary of provider "evil" is missing`},
		{"binary is a directory", planted("evil"), func(t *testing.T, env *testEnv) {
			if err := os.MkdirAll(env.cfg.hostPath("/cloud/bin/evil"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, `the binary of provider "evil" is missing`},
		{"name outside the bin dir", planted(`"../../../usr/bin/sh"`), nil, `kubelet refuses the provider name "../../../usr/bin/sh"`},
		{"repeated name", planted("auth-provider-gcp"), nil, `the provider name "auth-provider-gcp" occurs more than once`},
		{"repeated own name", gkeConfig + strings.TrimPrefix(renderedConfig, "apiVersion: kubelet.config.k8s.io/v1\nkind: CredentialProviderConfig\nproviders:\n") +
			strings.TrimPrefix(renderedConfig, "apiVersion: kubelet.config.k8s.io/v1\nkind: CredentialProviderConfig\nproviders:\n"), nil,
			`the provider name "harbor-bridge-plugin" occurs more than once`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, modeMerge, nil)
			env.cfg.MergeBinDir, env.cfg.MergeConfigFile = "/cloud/bin", "/cloud/config.yaml"
			writeCloudConfig(t, env, "/cloud/bin", "/cloud/config.yaml", gkeConfig)
			writeHostFile(t, env, "/cloud/config.yaml", tc.doc)
			if tc.prepare != nil {
				tc.prepare(t, env)
			}
			err := run(env.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "kubelet would not start") {
				t.Fatalf("got %v, want a refusal containing %q", err, tc.want)
			}
			if got := env.hostFile(t, "/cloud/config.yaml"); got != tc.doc {
				t.Fatal("the config was changed")
			}
			assertAbsent(t, env, "/cloud/bin/"+defaultProviderName, "/cloud/bin/"+filesFor(defaultProviderName).Record, configDir+"/harbor-bridge-ca.crt")
			if env.restarts != 0 {
				t.Fatal("kubelet restarted after a refusal")
			}
		})
	}

	// A provider whose binary is an executable file or a symlink (which
	// kubelet follows on the node) is fine, and so is a cloud's entry the
	// installer does not know.
	env := newTestEnv(t, modeMerge, nil)
	env.cfg.MergeBinDir, env.cfg.MergeConfigFile = "/cloud/bin", "/cloud/config.yaml"
	writeCloudConfig(t, env, "/cloud/bin", "/cloud/config.yaml", gkeConfig)
	doc := planted("evil") + strings.Replace(plantedEntry, "name: harbor-bridge-plugin.bak", "name: linked", 1)
	writeHostFile(t, env, "/cloud/config.yaml", doc)
	writeHostFile(t, env, "/cloud/bin/evil", "#!/bin/sh")
	if err := os.Chmod(env.cfg.hostPath("/cloud/bin/evil"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/libexec/linked", env.cfg.hostPath("/cloud/bin/linked")); err != nil {
		t.Fatal(err)
	}
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	if _, order := providersIn(t, env, "/cloud/config.yaml"); !reflect.DeepEqual(order, []string{"auth-provider-gcp", "evil", "linked", defaultProviderName}) {
		t.Fatalf("providers = %v", order)
	}
}

// TestRun_MergeKeepsAHandInstalledBridgeInACloudConfig: a bridge installed
// by hand next to a chart-managed one (docs/install-external-plugin.md)
// has an entry in the cloud's config and a binary, but no record. In a
// cloud's config, which the writers of plugin.hostConfigDir cannot reach,
// merge mode keeps it as it keeps every other entry there.
func TestRun_MergeKeepsAHandInstalledBridgeInACloudConfig(t *testing.T) {
	env := newTestEnv(t, modeAuto, []string{
		"/usr/bin/kubelet",
		"--image-credential-provider-bin-dir=/cloud/bin",
		"--image-credential-provider-config=/cloud/config.json",
	})
	hand := mustEntry(t, renderedConfigFor(euName), euName)
	merged, _, err := mergeProvider([]byte(eksConfig), hand)
	if err != nil {
		t.Fatal(err)
	}
	writeCloudConfig(t, env, "/cloud/bin", "/cloud/config.json", string(merged))
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	byName, order := providersIn(t, env, "/cloud/config.json")
	if !reflect.DeepEqual(order, []string{"ecr-credential-provider", euName, defaultProviderName}) || !reflect.DeepEqual(byName[euName], hand) {
		t.Fatalf("providers = %v, want the hand-installed entry kept", order)
	}
	if got, want := env.hostFile(t, "/cloud/bin/"+filesFor(defaultProviderName).Record), recordOf(t, "", renderedEntry(t, env)); got != want {
		t.Fatalf("record = %s, want %s: a cloud's config is not chart-owned", got, want)
	}
}

// TestRun_RefusesASymlinkInHostConfigDir: a release whose own
// plugin.hostConfigDir is inside another release's (a per-release
// directory under the default one) is in reach of that release's sync
// container, which can replace the inner directory with a symlink. The
// installer must refuse to follow it, and write nothing into the directory
// it points at: not the config, the CA, the mTLS files or a lock file.
func TestRun_RefusesASymlinkInHostConfigDir(t *testing.T) {
	for _, mode := range []string{modeNone, modePatch} {
		t.Run(mode, func(t *testing.T) {
			env := withName(t, newTestEnv(t, mode, []string{"/usr/bin/kubelet"}), euName)
			env.cfg.MTLSEnabled = true
			env.cfg.HostConfigDir = configDir + "/eu"
			for _, dir := range []string{configDir, "/etc/elsewhere"} {
				if err := os.MkdirAll(env.cfg.hostPath(dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink("../../elsewhere", env.cfg.hostPath(configDir+"/eu")); err != nil {
				t.Fatal(err)
			}
			err := run(env.cfg)
			if err == nil || !strings.Contains(err.Error(), "refusing to follow symlink eu") {
				t.Fatalf("got %v, want a refusal", err)
			}
			if err := syncAuxFiles(env.cfg); err == nil || !strings.Contains(err.Error(), "refusing to follow symlink eu") {
				t.Fatalf("sync: got %v, want a refusal", err)
			}
			if entries, err := os.ReadDir(env.cfg.hostPath("/etc/elsewhere")); err != nil || len(entries) != 0 {
				t.Fatalf("the installer wrote into the symlink's target: %v (err %v)", entries, err)
			}
			if env.restarts != 0 {
				t.Fatal("kubelet restarted")
			}
		})
	}
}
