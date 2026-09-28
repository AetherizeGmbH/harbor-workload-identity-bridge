// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ADR-0033: a kubelet restart that fails or does not verify is rolled back,
// and its content is not applied again.

// failRestarts makes the kubelet that the n-th restart starts (counting
// from 1) exit at startup, for every n in fail: its unit (Restart=always)
// then only reports "activating". Every other restart starts a kubelet that
// stays up.
func failRestarts(env *testEnv, fail ...int) {
	env.kubelet.statusFn = func() unitStatus {
		if slices.Contains(fail, env.restarts) {
			return unitStatus{ActiveState: "activating", NRestarts: -1}
		}
		return unitStatus{ActiveState: "active", MainPID: fakeKubeletPID + env.restarts, NRestarts: -1}
	}
}

// renderedWith is renderedConfig with its registry replaced: another
// provider entry, as a helm upgrade renders it.
func renderedWith(registry string) string {
	return strings.Replace(renderedConfig, `"harbor.example.com"`, `"`+registry+`"`, 1)
}

func setRendered(t *testing.T, env *testEnv, doc string) {
	t.Helper()
	if err := os.WriteFile(env.cfg.SourceConfig, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustLoadState(t *testing.T, env *testEnv) *state {
	t.Helper()
	st, err := loadState(env.statePath())
	if err != nil || st == nil {
		t.Fatalf("state: %+v, %v", st, err)
	}
	return st
}

func TestRun_PatchRollsBackContentKubeletRejects(t *testing.T) {
	env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	writeHostFile(t, env, defaultKubeletPath, "# node settings\nKUBELET_EXTRA_ARGS=\"--max-pods=42\"\n")
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	config0 := env.hostFile(t, env.cfg.ownConfigPath())
	env0 := env.hostFile(t, defaultKubeletPath)
	verified := mustLoadState(t, env)
	entry0 := renderedEntry(t, env)

	// A helm upgrade renders content kubelet exits on.
	setRendered(t, env, renderedWith("harbor-alt.example.com"))
	entry1 := renderedEntry(t, env)
	failRestarts(env, 2)
	err := run(env.cfg)
	if err == nil || !strings.Contains(err.Error(), "did not become stably active") ||
		!strings.Contains(err.Error(), "restored the previous") || !strings.Contains(err.Error(), "runs on them again") {
		t.Fatalf("got %v, want a verification failure followed by a successful rollback", err)
	}
	if env.restarts != 3 {
		t.Fatalf("restarts = %d, want 3: the install, the rejected content, the rollback", env.restarts)
	}
	if got := env.hostFile(t, env.cfg.ownConfigPath()); got != config0 {
		t.Fatalf("config not restored:\n%s", got)
	}
	if got := env.hostFile(t, defaultKubeletPath); got != env0 {
		t.Fatalf("/etc/default/kubelet not restored:\n%s", got)
	}
	// The rejected content is what the .bak holds now.
	if got := env.hostFile(t, env.cfg.ownConfigPath()+".bak"); !strings.Contains(got, "harbor-alt.example.com") {
		t.Fatalf("config .bak does not hold the rejected content:\n%s", got)
	}
	// The record vouches for the entry the config holds again, so other
	// installs keep it (ADR-0029).
	rec := env.cfg.readRecord(filepath.Join(binDir, env.cfg.files().Record))
	for name, e := range map[string]map[string]any{"restored": entry0, "rejected": entry1} {
		raw, err := entryBytes(e)
		if err != nil {
			t.Fatal(err)
		}
		if !rec.holds(raw) {
			t.Fatalf("record does not hold the %s entry: %+v", name, rec)
		}
	}
	st := mustLoadState(t, env)
	if st.Rejected == nil || st.EntryHash != verified.EntryHash || st.AppliedHash != verified.AppliedHash {
		t.Fatalf("state after rollback = %+v, want the verified record %+v plus a rejection", st, verified)
	}

	// The same content again: refused before anything is written, and
	// kubelet is left alone.
	env.kubelet.statusFn = nil
	err = run(env.cfg)
	if err == nil || !strings.Contains(err.Error(), "refusing to restart kubelet onto content it already rejected") {
		t.Fatalf("got %v, want a refusal of the rejected content", err)
	}
	if env.restarts != 3 {
		t.Fatalf("the rejected content restarted kubelet again (restarts = %d)", env.restarts)
	}
	if got := env.hostFile(t, env.cfg.ownConfigPath()); got != config0 {
		t.Fatalf("a refused pass rewrote the config:\n%s", got)
	}

	// New content is applied, and the rejection goes.
	setRendered(t, env, renderedWith("harbor-new.example.com"))
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	if env.restarts != 4 {
		t.Fatalf("restarts = %d, want 4", env.restarts)
	}
	if st := mustLoadState(t, env); st.Rejected != nil || st.EntryHash == verified.EntryHash {
		t.Fatalf("state after a verified restart = %+v, want the new content without a rejection", st)
	}
}

// TestRun_PatchFirstInstallRollbackRemovesItsWiring: on a node without
// /etc/default/kubelet and without a chart-owned config, the rollback
// removes both again; kubelet starts without credential providers, as it
// did before.
func TestRun_PatchFirstInstallRollbackRemovesItsWiring(t *testing.T) {
	env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet"})
	failRestarts(env, 1)
	if err := run(env.cfg); err == nil || !strings.Contains(err.Error(), "runs on them again") {
		t.Fatalf("got %v, want a rollback", err)
	}
	if env.restarts != 2 {
		t.Fatalf("restarts = %d, want 2", env.restarts)
	}
	assertAbsent(t, env, defaultKubeletPath, env.cfg.ownConfigPath())
	if w, err := discoverKubelet(env.cfg.ProcRoot); err != nil || w.wired() {
		t.Fatalf("kubelet after the rollback: %+v, %v; want it without credential-provider flags", w, err)
	}
	for range 2 {
		if err := run(env.cfg); err == nil || !strings.Contains(err.Error(), "already rejected") {
			t.Fatalf("got %v, want a refusal", err)
		}
	}
	if env.restarts != 2 {
		t.Fatalf("retries restarted kubelet (restarts = %d)", env.restarts)
	}
}

func TestRun_MergeRollsBackTheNodesConfig(t *testing.T) {
	env := newTestEnv(t, modeAuto, []string{
		"/usr/bin/kubelet",
		"--image-credential-provider-bin-dir=/cloud/bin",
		"--image-credential-provider-config=/cloud/config.json",
	})
	writeCloudConfig(t, env, "/cloud/bin", "/cloud/config.json", eksConfig)
	failRestarts(env, 1)
	if err := run(env.cfg); err == nil || !strings.Contains(err.Error(), "restored the previous /cloud/config.json") {
		t.Fatalf("got %v, want a rollback of the cloud's config", err)
	}
	if got := env.hostFile(t, "/cloud/config.json"); got != eksConfig {
		t.Fatalf("cloud config not restored byte for byte:\n%s", got)
	}
	if env.restarts != 2 {
		t.Fatalf("restarts = %d, want 2", env.restarts)
	}
	if err := run(env.cfg); err == nil || !strings.Contains(err.Error(), "already rejected") {
		t.Fatalf("got %v, want a refusal", err)
	}
	if env.restarts != 2 || env.hostFile(t, "/cloud/config.json") != eksConfig {
		t.Fatalf("a refused pass restarted kubelet (%d) or wrote the cloud's config", env.restarts)
	}
}

// TestRun_VerificationFailureDoesNotRestartKubeletOnEveryRetry: a kubelet
// unit that does not pass /etc/default/kubelet on to kubelet fails patch
// mode's verification on every pass. The init container is retried without
// end; before ADR-0033 every retry restarted kubelet again.
func TestRun_VerificationFailureDoesNotRestartKubeletOnEveryRetry(t *testing.T) {
	env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	env.kubelet.ignoreEnvFile = true
	for pass := 1; pass <= 3; pass++ {
		err := run(env.cfg)
		if err == nil || !strings.Contains(err.Error(), "does the kubelet unit pass") {
			t.Fatalf("pass %d: got %v, want the verification failure", pass, err)
		}
		if pass > 1 && !strings.Contains(err.Error(), "already rejected") {
			t.Fatalf("pass %d: got %v, want a refusal that names the recorded failure", pass, err)
		}
	}
	if env.restarts != 2 {
		t.Fatalf("restarts = %d, want 2: the first pass and its rollback, none for the retries", env.restarts)
	}
}

// TestRun_CrashWindowFailureIsRecordedWithoutRestore: a pass that changed
// no file but restarts kubelet because its state does not record the
// content has nothing of its own to restore. It says so and records the
// rejection, so the retries restart nothing.
func TestRun_CrashWindowFailureIsRecordedWithoutRestore(t *testing.T) {
	env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	config0 := env.hostFile(t, env.cfg.ownConfigPath())
	if err := os.Remove(env.statePath()); err != nil {
		t.Fatal(err)
	}
	failRestarts(env, 2)
	err := run(env.cfg)
	if err == nil || !strings.Contains(err.Error(), "this pass did not change") {
		t.Fatalf("got %v, want a failure that says there is nothing to restore", err)
	}
	if env.restarts != 2 {
		t.Fatalf("restarts = %d, want 2 (no rollback restart)", env.restarts)
	}
	if got := env.hostFile(t, env.cfg.ownConfigPath()); got != config0 {
		t.Fatal("the files changed although there was nothing to restore")
	}
	if st := mustLoadState(t, env); st.Rejected == nil {
		t.Fatalf("no rejection recorded: %+v", st)
	}
	if err := run(env.cfg); err == nil || !strings.Contains(err.Error(), "already rejected") {
		t.Fatalf("got %v, want a refusal", err)
	}
	if env.restarts != 2 {
		t.Fatalf("the retry restarted kubelet (restarts = %d)", env.restarts)
	}
}

// TestRun_FailedRollbackIsReported: when kubelet does not come back on the
// restored files either, the error says so, and the content is still
// recorded as rejected.
func TestRun_FailedRollbackIsReported(t *testing.T) {
	env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	setRendered(t, env, renderedWith("harbor-alt.example.com"))
	failRestarts(env, 2, 3)
	err := run(env.cfg)
	if err == nil || !strings.Contains(err.Error(), "did not verify either") {
		t.Fatalf("got %v, want a report of the failed rollback", err)
	}
	if st := mustLoadState(t, env); st.Rejected == nil {
		t.Fatalf("no rejection recorded: %+v", st)
	}
}

// TestRun_RefusesAnEntryKubeletRejects: kubelet validates every provider at
// startup and exits on an invalid one. The chart fills these fields from
// values it does not check; the installer refuses them before it writes
// anything or restarts kubelet.
func TestRun_RefusesAnEntryKubeletRejects(t *testing.T) {
	for name, tc := range map[string]struct{ from, to, want string }{
		"day unit":          {`defaultCacheDuration: "1h"`, `defaultCacheDuration: "1d"`, "defaultCacheDuration"},
		"number":            {`defaultCacheDuration: "1h"`, `defaultCacheDuration: 3600`, "defaultCacheDuration"},
		"negative":          {`defaultCacheDuration: "1h"`, `defaultCacheDuration: "-1h"`, "negative"},
		"invalid port":      {`"harbor.example.com"`, `"harbor.example.com:*"`, "matchImages"},
		"space in host":     {`"harbor.example.com"`, `"harbor example.com"`, "matchImages"},
		"empty audience":    {`serviceAccountTokenAudience: "harbor-bridge"`, `serviceAccountTokenAudience: ""`, "serviceAccountTokenAudience"},
		"unknown cacheType": {`cacheType: ServiceAccount`, `cacheType: Pod`, "cacheType"},
	} {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet"})
			if !strings.Contains(renderedConfig, tc.from) {
				t.Fatalf("fixture lacks %q", tc.from)
			}
			setRendered(t, env, strings.Replace(renderedConfig, tc.from, tc.to, 1))
			err := run(env.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want a refusal naming %s", err, tc.want)
			}
			if env.restarts != 0 {
				t.Fatal("kubelet restarted")
			}
			assertAbsent(t, env, defaultKubeletPath, env.cfg.ownConfigPath(), binDir+"/harbor-bridge-plugin")
		})
	}
}

func TestValidateEntry(t *testing.T) {
	valid := func() map[string]any { return mustEntry(t, renderedConfig, defaultProviderName) }
	if err := validateEntry(valid()); err != nil {
		t.Fatalf("the chart's entry: %v", err)
	}
	ok := map[string]func(map[string]any){
		"zero duration": func(e map[string]any) { e["defaultCacheDuration"] = "0s" },
		"compound":      func(e map[string]any) { e["defaultCacheDuration"] = "1h30m" },
		"glob and port": func(e map[string]any) { e["matchImages"] = []any{"*.harbor.example.com:5000/project"} },
		"ipv6 registry": func(e map[string]any) { e["matchImages"] = []any{"[fd00::1]:5000"} },
		"no token attrs": func(e map[string]any) {
			delete(e, "tokenAttributes")
			e["apiVersion"] = "credentialprovider.kubelet.k8s.io/v1beta1"
		},
		"token cache type": func(e map[string]any) { e["tokenAttributes"].(map[string]any)["cacheType"] = "Token" },
	}
	for name, mutate := range ok {
		e := valid()
		mutate(e)
		if err := validateEntry(e); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
	bad := map[string]func(map[string]any){
		"no duration":             func(e map[string]any) { delete(e, "defaultCacheDuration") },
		"no matchImages":          func(e map[string]any) { delete(e, "matchImages") },
		"non-string image":        func(e map[string]any) { e["matchImages"] = []any{42.0} },
		"unknown apiVersion":      func(e map[string]any) { e["apiVersion"] = "credentialprovider.kubelet.k8s.io/v2" },
		"token attrs on v1beta1":  func(e map[string]any) { e["apiVersion"] = "credentialprovider.kubelet.k8s.io/v1beta1" },
		"requireServiceAccount":   func(e map[string]any) { delete(e["tokenAttributes"].(map[string]any), "requireServiceAccount") },
		"token attrs not an obj":  func(e map[string]any) { e["tokenAttributes"] = "x" },
		"missing cacheType":       func(e map[string]any) { delete(e["tokenAttributes"].(map[string]any), "cacheType") },
		"missing audience":        func(e map[string]any) { delete(e["tokenAttributes"].(map[string]any), "serviceAccountTokenAudience") },
		"duration without a unit": func(e map[string]any) { e["defaultCacheDuration"] = "60" },
		"no endpoint":             func(e map[string]any) { e["env"] = []any{} },
	}
	for name, mutate := range bad {
		e := valid()
		mutate(e)
		if err := validateEntry(e); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestState_RejectsExactlyTheRecordedContent(t *testing.T) {
	rejected := target{mode: modePatch, binDir: "/b", configFile: "/c", envFile: defaultKubeletPath, entryHash: "e", fileHash: "f"}
	st := &state{Rejected: rejected.rejection("kubelet", "", "boom", time.Unix(0, 0))}
	if r := st.rejects(rejected, "kubelet"); r == nil || r.Reason != "boom" || r.At != "1970-01-01T00:00:00Z" {
		t.Fatalf("rejects = %+v", r)
	}
	for name, change := range map[string]func(*target){
		"mode":        func(t *target) { t.mode = modeMerge },
		"bin dir":     func(t *target) { t.binDir = "/b2" },
		"config file": func(t *target) { t.configFile = "/c2" },
		"env file":    func(t *target) { t.envFile = "/etc/sysconfig/kubelet" },
		"entry":       func(t *target) { t.entryHash = "e2" },
		"shared file": func(t *target) { t.fileHash = "f2" },
	} {
		other := rejected
		change(&other)
		if st.rejects(other, "kubelet") != nil {
			t.Errorf("%s changed, still rejected", name)
		}
	}
	if st.rejects(rejected, "kubelet.service") != nil {
		t.Error("another unit, still rejected")
	}
	if (*state)(nil).rejects(rejected, "kubelet") != nil || (&state{}).rejects(rejected, "kubelet") != nil {
		t.Error("a state without a rejection rejects")
	}
}

// withKubelet gives env's kubelet the binary content binary at
// /usr/bin/kubelet, and the running process an exe link to it.
func withKubelet(t *testing.T, env *testEnv, binary string) {
	t.Helper()
	writeHostFile(t, env, "/usr/bin/kubelet", binary)
	env.kubelet.exe = "/usr/bin/kubelet"
	raw, err := os.ReadFile(filepath.Join(env.cfg.ProcRoot, strconv.Itoa(fakeKubeletPID), "cmdline"))
	if err != nil {
		t.Fatal(err)
	}
	writeProcEntry(t, env.cfg.ProcRoot, fakeKubeletPID, procEntry{comm: "kubelet", cmdline: splitCmdline(raw), exe: env.kubelet.exe})
}

// TestRun_RejectedContentIsTriedAgainOnAnotherKubelet: content that the
// kubelet of a node rejected (for example tokenAttributes on a 1.33
// kubelet with the feature gate off) is not applied again to the same
// kubelet. Another kubelet binary, command line or config file did not test
// it: the next pass tries it once more, without a manual deletion of the
// state file. The same binary content written anew is the same kubelet.
func TestRun_RejectedContentIsTriedAgainOnAnotherKubelet(t *testing.T) {
	const kubeletConfig = "/var/lib/kubelet/config.yaml"
	for name, tc := range map[string]struct {
		change  func(t *testing.T, env *testEnv)
		retried bool
	}{
		"kubelet upgraded": {change: func(t *testing.T, env *testEnv) {
			writeHostFile(t, env, "/usr/bin/kubelet", "kubelet v1.34.0")
		}, retried: true},
		"feature gate in the config file": {change: func(t *testing.T, env *testEnv) {
			writeHostFile(t, env, kubeletConfig, "kind: KubeletConfiguration\nfeatureGates:\n  KubeletServiceAccountTokenForCredentialProviders: true\n")
		}, retried: true},
		"feature gate on the command line": {change: func(t *testing.T, env *testEnv) {
			// The operator adds the flag and restarts kubelet.
			env.kubelet.baseArgs = append(env.kubelet.baseArgs, "--feature-gates=KubeletServiceAccountTokenForCredentialProviders=true")
			raw, err := os.ReadFile(filepath.Join(env.cfg.ProcRoot, strconv.Itoa(fakeKubeletPID), "cmdline"))
			if err != nil {
				t.Fatal(err)
			}
			args := append(splitCmdline(raw), "--feature-gates=KubeletServiceAccountTokenForCredentialProviders=true")
			writeProcEntry(t, env.cfg.ProcRoot, fakeKubeletPID, procEntry{comm: "kubelet", cmdline: args, exe: env.kubelet.exe})
		}, retried: true},
		"same binary reinstalled": {change: func(t *testing.T, env *testEnv) {
			path := env.cfg.hostPath("/usr/bin/kubelet")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			writeHostFile(t, env, "/usr/bin/kubelet", "kubelet v1.33.4")
		}},
		"nothing changed": {change: func(*testing.T, *testEnv) {}},
	} {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet", "--config=" + kubeletConfig})
			withKubelet(t, env, "kubelet v1.33.4")
			writeHostFile(t, env, kubeletConfig, "kind: KubeletConfiguration\n")
			if err := run(env.cfg); err != nil {
				t.Fatal(err)
			}
			setRendered(t, env, renderedWith("harbor-alt.example.com"))
			failRestarts(env, 2)
			if err := run(env.cfg); err == nil || !strings.Contains(err.Error(), "recorded as rejected") {
				t.Fatalf("got %v, want the rejection", err)
			}
			rejected := mustLoadState(t, env).Rejected
			if rejected == nil || rejected.Kubelet == "" {
				t.Fatalf("rejection = %+v, want one that names the kubelet", rejected)
			}
			env.kubelet.statusFn = nil
			if err := run(env.cfg); err == nil || !strings.Contains(err.Error(), "already rejected") {
				t.Fatalf("same kubelet: got %v, want a refusal", err)
			}
			tc.change(t, env)
			err := run(env.cfg)
			switch {
			case tc.retried && (err != nil || env.restarts != 4):
				t.Fatalf("got %v with %d restarts, want the content tried again (4 restarts)", err, env.restarts)
			case tc.retried:
				if st := mustLoadState(t, env); st.Rejected != nil {
					t.Fatalf("state = %+v, want no rejection after the verified restart", st)
				}
			case err == nil || !strings.Contains(err.Error(), "already rejected") || env.restarts != 3:
				t.Fatalf("got %v with %d restarts, want a refusal without a restart", err, env.restarts)
			}
		})
	}
}

// TestRun_RejectedAgainByTheNewKubeletRecordsIt: when the new kubelet
// rejects the content too, its identity replaces the old one, and the
// retries restart nothing.
func TestRun_RejectedAgainByTheNewKubeletRecordsIt(t *testing.T) {
	env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	withKubelet(t, env, "kubelet v1.33.4")
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	setRendered(t, env, renderedWith("harbor-alt.example.com"))
	failRestarts(env, 2, 4)
	if err := run(env.cfg); err == nil {
		t.Fatal("want the rejection")
	}
	first := mustLoadState(t, env).Rejected
	writeHostFile(t, env, "/usr/bin/kubelet", "kubelet v1.34.0")
	if err := run(env.cfg); err == nil || !strings.Contains(err.Error(), "recorded as rejected") {
		t.Fatalf("got %v, want a second rejection", err)
	}
	second := mustLoadState(t, env).Rejected
	if env.restarts != 5 || second == nil || second.Kubelet == "" || second.Kubelet == first.Kubelet {
		t.Fatalf("restarts = %d, rejections %+v then %+v: want the new kubelet recorded", env.restarts, first, second)
	}
	if err := run(env.cfg); err == nil || !strings.Contains(err.Error(), "already rejected") || env.restarts != 5 {
		t.Fatalf("got %v with %d restarts, want a refusal", err, env.restarts)
	}
}

func TestRejection_TestedBy(t *testing.T) {
	r := &rejection{Kubelet: "a"}
	if !r.testedBy("a") || r.testedBy("b") {
		t.Fatal("a known kubelet must match only itself")
	}
	if !r.testedBy("") || !(&rejection{}).testedBy("b") {
		t.Fatal("an unknown kubelet on either side must keep the rejection")
	}
}

// TestKubeletIdentity: the identity follows the binary at the path of the
// running kubelet (also when a package upgrade replaced it after the start,
// which /proc shows as "(deleted)"), its command line without the
// credential-provider flags, and its --config file; it is "" when the
// binary cannot be read.
func TestKubeletIdentity(t *testing.T) {
	env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet", "--config=/var/lib/kubelet/config.yaml", "--v=2"})
	if got := env.cfg.kubeletIdentity(); got != "" {
		t.Fatalf("identity without an exe link = %q, want none", got)
	}
	withKubelet(t, env, "kubelet v1.33.4")
	base := env.cfg.kubeletIdentity()
	if base == "" {
		t.Fatal("no identity for a readable binary without its config file")
	}
	writeHostFile(t, env, "/var/lib/kubelet/config.yaml", "kind: KubeletConfiguration\n")
	withConfig := env.cfg.kubeletIdentity()
	if withConfig == "" || withConfig == base {
		t.Fatalf("config file did not count: %q vs %q", withConfig, base)
	}
	// Our own flags are no part of it: patch mode adds them with the
	// content it tests.
	writeProcEntry(t, env.cfg.ProcRoot, fakeKubeletPID, procEntry{comm: "kubelet", exe: "/usr/bin/kubelet (deleted)", cmdline: []string{
		"/usr/bin/kubelet", "--config=/var/lib/kubelet/config.yaml", "--v=2",
		flagBinDir + "=/etc/kubernetes/credential-provider", flagConfigFile, "/etc/kubernetes/credential-provider-config/" + configFileName,
	}})
	if got := env.cfg.kubeletIdentity(); got != withConfig {
		t.Fatalf("identity with the credential-provider flags and a replaced binary = %q, want %q", got, withConfig)
	}
	writeProcEntry(t, env.cfg.ProcRoot, fakeKubeletPID, procEntry{comm: "kubelet", exe: "/usr/bin/kubelet", cmdline: []string{"/usr/bin/kubelet", "--config=/var/lib/kubelet/config.yaml", "--v=4"}})
	if got := env.cfg.kubeletIdentity(); got == withConfig {
		t.Fatal("another command line did not count")
	}
	if err := os.Remove(env.cfg.hostPath("/usr/bin/kubelet")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/opt/kubelet", env.cfg.hostPath("/usr/bin/kubelet")); err != nil {
		t.Fatal(err)
	}
	if got := env.cfg.kubeletIdentity(); got != "" {
		t.Fatalf("identity of a symlinked binary = %q, want none", got)
	}
}
