// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// ADR-0034: patch mode edits the environment file the kubelet unit reads.

const (
	rpmKubeletEnvFile = "/etc/sysconfig/kubelet"
	kubeadmFlagsEnv   = "/var/lib/kubelet/kubeadm-flags.env"
)

// rpmUnit makes env's kubelet unit the one of kubeadm's RPM packages: it
// reads /etc/sysconfig/kubelet, which the kubelet package ships with an
// empty KUBELET_EXTRA_ARGS.
func rpmUnit(t *testing.T, env *testEnv) {
	t.Helper()
	env.kubelet.envFiles = []string{kubeadmFlagsEnv, rpmKubeletEnvFile}
	writeHostFile(t, env, kubeadmFlagsEnv, "KUBELET_KUBEADM_ARGS=\"--container-runtime-endpoint=unix:///run/containerd/containerd.sock\"\n")
	writeHostFile(t, env, rpmKubeletEnvFile, "KUBELET_EXTRA_ARGS=\n")
}

// TestRun_AutoPatchesTheRPMEnvironmentFile: on an RPM-packaged kubeadm node
// the default mode used to write /etc/default/kubelet, which nothing reads,
// restart kubelet and fail its verification.
func TestRun_AutoPatchesTheRPMEnvironmentFile(t *testing.T) {
	env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet"})
	rpmUnit(t, env)
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	if got := env.hostFile(t, rpmKubeletEnvFile); !strings.Contains(got, flagBinDir+"="+binDir) || strings.Count(got, extraArgsKey) != 1 {
		t.Fatalf("%s not patched in place:\n%s", rpmKubeletEnvFile, got)
	}
	assertAbsent(t, env, defaultKubeletPath)
	if w, err := discoverKubelet(env.cfg.ProcRoot); err != nil || w != (kubeletWiring{BinDir: binDir, ConfigFile: env.cfg.ownConfigPath()}) {
		t.Fatalf("kubelet after the install: %+v, %v", w, err)
	}
	if st := mustLoadState(t, env); st.EnvFile != rpmKubeletEnvFile {
		t.Fatalf("state records envFile %q, want %q", st.EnvFile, rpmKubeletEnvFile)
	}
	if err := run(env.cfg); err != nil || env.restarts != 1 {
		t.Fatalf("no-op re-run: %v, restarts = %d", err, env.restarts)
	}
}

// TestRun_PatchRefusesAUnitWithoutAnOperatorEnvFile: a unit that reads
// neither file is refused before anything is written or restarted.
func TestRun_PatchRefusesAUnitWithoutAnOperatorEnvFile(t *testing.T) {
	for name, files := range map[string][]string{
		"only kubeadm's flags": {kubeadmFlagsEnv},
		"none":                 {},
	} {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet"})
			env.kubelet.envFiles = files
			err := run(env.cfg)
			if err == nil || !strings.Contains(err.Error(), "reads neither") || !strings.Contains(err.Error(), "plugin.install.mode=none") {
				t.Fatalf("got %v, want a refusal that points to mode none", err)
			}
			if env.restarts != 0 {
				t.Fatal("kubelet restarted")
			}
			assertAbsent(t, env, defaultKubeletPath, rpmKubeletEnvFile, env.cfg.ownConfigPath(), binDir+"/harbor-bridge-plugin", stateDir)
		})
	}
}

// TestKubeletEnvFile_PicksTheAssignmentThatCounts: systemd reads the files
// in order and the last assignment wins; that is the one to edit.
func TestKubeletEnvFile_PicksTheAssignmentThatCounts(t *testing.T) {
	for name, tc := range map[string]struct {
		files    []string
		contents map[string]string
		env      string
		want     string // "" is a refusal
	}{
		"debian":                     {files: kubeadmEnvFiles, want: defaultKubeletPath},
		"rpm":                        {files: []string{kubeadmFlagsEnv, rpmKubeletEnvFile}, want: rpmKubeletEnvFile},
		"both, none assigns":         {files: []string{rpmKubeletEnvFile, defaultKubeletPath}, want: defaultKubeletPath},
		"both, earlier assigns":      {files: []string{rpmKubeletEnvFile, defaultKubeletPath}, contents: map[string]string{rpmKubeletEnvFile: "KUBELET_EXTRA_ARGS=--max-pods=42\n", defaultKubeletPath: "OTHER=1\n"}, want: rpmKubeletEnvFile},
		"both assign":                {files: []string{rpmKubeletEnvFile, defaultKubeletPath}, contents: map[string]string{rpmKubeletEnvFile: "KUBELET_EXTRA_ARGS=--a\n", defaultKubeletPath: "KUBELET_EXTRA_ARGS = --b\n"}, want: defaultKubeletPath},
		"missing file listed":        {files: []string{rpmKubeletEnvFile}, want: rpmKubeletEnvFile},
		"assigned in kubeadm's file": {files: []string{defaultKubeletPath, kubeadmFlagsEnv}, contents: map[string]string{kubeadmFlagsEnv: "KUBELET_EXTRA_ARGS=--v=2\n"}},
		"assigned in a drop-in file": {files: []string{defaultKubeletPath, "/etc/kubelet.env"}, contents: map[string]string{"/etc/kubelet.env": "KUBELET_EXTRA_ARGS=--v=2\n"}},
		"only Environment= sets it":  {files: kubeadmEnvFiles, env: `KUBELET_EXTRA_ARGS=--v=2 FOO=1`},
		"Environment= overridden":    {files: kubeadmEnvFiles, contents: map[string]string{defaultKubeletPath: "KUBELET_EXTRA_ARGS=--v=3\n"}, env: `KUBELET_EXTRA_ARGS=--v=2`, want: defaultKubeletPath},
		"Environment= other var":     {files: kubeadmEnvFiles, env: `MY_KUBELET_EXTRA_ARGS=1`, want: defaultKubeletPath},
		"relative path":              {files: []string{"etc/default/kubelet"}},
		"unreadable (a symlink)":     {files: []string{defaultKubeletPath, "/etc/link.env"}},
	} {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
			env.kubelet.envFiles = tc.files
			env.kubelet.envAssignments = tc.env
			for path, content := range tc.contents {
				writeHostFile(t, env, path, content)
			}
			if name == "unreadable (a symlink)" {
				writeHostFile(t, env, "/etc/target.env", "KUBELET_EXTRA_ARGS=--v=2\n")
				if err := os.Symlink("target.env", env.cfg.hostPath("/etc/link.env")); err != nil {
					t.Fatal(err)
				}
			}
			got, err := env.cfg.kubeletEnvFile()
			switch {
			case tc.want == "" && err == nil:
				t.Fatalf("got %s, want a refusal", got)
			case tc.want != "" && (err != nil || got != tc.want):
				t.Fatalf("got %q, %v; want %s", got, err, tc.want)
			}
		})
	}
}

// TestRun_PatchPreservesArgsOfTheFileThatCounts: with both operator files
// listed, the installer edits the one whose assignment kubelet gets and
// keeps its args; appending to the other would override them.
func TestRun_PatchPreservesArgsOfTheFileThatCounts(t *testing.T) {
	env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	env.kubelet.envFiles = []string{rpmKubeletEnvFile, defaultKubeletPath}
	writeHostFile(t, env, rpmKubeletEnvFile, "KUBELET_EXTRA_ARGS=\"--max-pods=42\"\n")
	writeHostFile(t, env, defaultKubeletPath, "OTHER=1\n")
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	if got := env.hostFile(t, defaultKubeletPath); got != "OTHER=1\n" {
		t.Fatalf("%s changed:\n%s", defaultKubeletPath, got)
	}
	if got := env.hostFile(t, rpmKubeletEnvFile); !strings.Contains(got, "--max-pods=42 "+flagBinDir) {
		t.Fatalf("%s:\n%s", rpmKubeletEnvFile, got)
	}
}

// TestRun_EnvFileIsPartOfTheRestartDecision: the content hash covers the
// file's bytes, not its name. A unit that moves to another file with the
// same bytes still needs a restart; a record from before ADR-0034, which
// has no envFile, is for /etc/default/kubelet and needs none.
func TestRun_EnvFileIsPartOfTheRestartDecision(t *testing.T) {
	env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	// A record written before ADR-0034.
	raw, err := os.ReadFile(env.statePath())
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec["envFile"] != defaultKubeletPath {
		t.Fatalf("state records envFile %v", rec["envFile"])
	}
	delete(rec, "envFile")
	raw, err = json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.statePath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(env.cfg); err != nil || env.restarts != 1 {
		t.Fatalf("upgrade from a record without envFile: %v, restarts = %d, want no restart", err, env.restarts)
	}

	// The unit now reads /etc/sysconfig/kubelet, with the same bytes.
	writeHostFile(t, env, rpmKubeletEnvFile, env.hostFile(t, defaultKubeletPath))
	env.kubelet.envFiles = []string{rpmKubeletEnvFile}
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	if env.restarts != 2 {
		t.Fatalf("restarts = %d, want 2: kubelet reads another file now", env.restarts)
	}
}

func TestParseUnitEnvironment(t *testing.T) {
	out := "EnvironmentFiles=/var/lib/kubelet/kubeadm-flags.env (ignore_errors=yes)\n" +
		"EnvironmentFiles=/etc/sysconfig/kubelet (ignore_errors=no)\n" +
		"Environment=KUBELET_KUBECONFIG_ARGS=--kubeconfig=/etc/kubernetes/kubelet.conf \"A=b c\"\n"
	got := parseUnitEnvironment([]byte(out))
	if strings.Join(got.Files, ",") != kubeadmFlagsEnv+","+rpmKubeletEnvFile {
		t.Fatalf("files = %q", got.Files)
	}
	if !strings.HasPrefix(got.Assignments, "KUBELET_KUBECONFIG_ARGS=") {
		t.Fatalf("assignments = %q", got.Assignments)
	}
	if got := parseUnitEnvironment([]byte("EnvironmentFiles=\nEnvironment=\n")); len(got.Files) != 0 || got.Assignments != "" {
		t.Fatalf("empty unit: %+v", got)
	}
}
