// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"slices"
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
		if err == nil || !strings.Contains(err.Error(), "does the kubelet unit source") {
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
	rejected := target{mode: modePatch, binDir: "/b", configFile: "/c", entryHash: "e", fileHash: "f"}
	st := &state{Rejected: rejected.rejection("kubelet", "boom", time.Unix(0, 0))}
	if r := st.rejects(rejected, "kubelet"); r == nil || r.Reason != "boom" || r.At != "1970-01-01T00:00:00Z" {
		t.Fatalf("rejects = %+v", r)
	}
	for name, other := range map[string]target{
		"mode":        {modeMerge, "/b", "/c", "e", "f"},
		"bin dir":     {modePatch, "/b2", "/c", "e", "f"},
		"config file": {modePatch, "/b", "/c2", "e", "f"},
		"entry":       {modePatch, "/b", "/c", "e2", "f"},
		"shared file": {modePatch, "/b", "/c", "e", "f2"},
	} {
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
