// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"slices"
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
		dirs     []string // directories to create
		required []string // settings without "-"
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
		// systemd expands a wildcard expression; `systemctl show` prints
		// it as it is. The assignment in a matching file counts.
		"wildcard match assigns":    {files: []string{defaultKubeletPath, "/etc/kubelet.d/*.env"}, contents: map[string]string{defaultKubeletPath: "KUBELET_EXTRA_ARGS=--from-default\n", "/etc/kubelet.d/10.env": "KUBELET_EXTRA_ARGS=--from-glob\n"}},
		"wildcard match sets other": {files: []string{defaultKubeletPath, "/etc/kubelet.d/*.env"}, contents: map[string]string{"/etc/kubelet.d/10.env": "FOO=1\n"}, want: defaultKubeletPath},
		"wildcard matches nothing":  {files: []string{defaultKubeletPath, "/etc/nothing/*.env"}, want: defaultKubeletPath},
		"wildcard names the file":   {files: []string{"/etc/default/kube*"}, contents: map[string]string{defaultKubeletPath: "KUBELET_EXTRA_ARGS=--a\n"}, want: defaultKubeletPath},
		"wildcard over directories": {files: []string{defaultKubeletPath, "/etc/k/*/x.env"}, contents: map[string]string{"/etc/k/a/x.env": "KUBELET_EXTRA_ARGS=--a\n"}},
		"character class":           {files: []string{defaultKubeletPath, "/etc/kubelet.d/[[:digit:]]*.env"}, contents: map[string]string{"/etc/kubelet.d/10.env": "FOO=1\n"}},
		"optional directory":        {files: []string{defaultKubeletPath, "/etc/kubelet.d/*.env"}, dirs: []string{"/etc/kubelet.d/sub.env"}, want: defaultKubeletPath},
		"required directory":        {files: []string{defaultKubeletPath, "/etc/kubelet.d/*.env"}, dirs: []string{"/etc/kubelet.d/sub.env"}, required: []string{"/etc/kubelet.d/*.env"}},
		// A file systemd does not load sets nothing; behind "-" it is
		// skipped, without "-" the unit does not start. The file patch
		// mode edits must load.
		"optional unloadable file": {files: kubeadmEnvFiles, contents: map[string]string{kubeadmFlagsEnv: "KUBELET_EXTRA_ARGS=\xff\n"}, want: defaultKubeletPath},
		"required unloadable file": {files: kubeadmEnvFiles, contents: map[string]string{kubeadmFlagsEnv: "KUBELET_EXTRA_ARGS=\xff\n"}, required: []string{kubeadmFlagsEnv}},
		"unloadable operator file": {files: kubeadmEnvFiles, contents: map[string]string{defaultKubeletPath: "FOO=\xff\n"}},
	} {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
			env.kubelet.envFiles = tc.files
			env.kubelet.envAssignments = tc.env
			env.kubelet.requiredEnvFiles = map[string]bool{}
			for _, path := range tc.required {
				env.kubelet.requiredEnvFiles[path] = true
			}
			for path, content := range tc.contents {
				writeHostFile(t, env, path, content)
			}
			for _, dir := range tc.dirs {
				if err := os.MkdirAll(env.cfg.hostPath(dir), 0o755); err != nil {
					t.Fatal(err)
				}
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
	if want := []envFileRef{{kubeadmFlagsEnv, true}, {rpmKubeletEnvFile, false}}; !slices.Equal(got.Files, want) {
		t.Fatalf("files = %+v, want %+v", got.Files, want)
	}
	if !strings.HasPrefix(got.Assignments, "KUBELET_KUBECONFIG_ARGS=") {
		t.Fatalf("assignments = %q", got.Assignments)
	}
	if got := parseUnitEnvironment([]byte("EnvironmentFiles=\nEnvironment=\n")); len(got.Files) != 0 || got.Assignments != "" {
		t.Fatalf("empty unit: %+v", got)
	}
}

// TestRun_KindNode pins a kind node (kindest/node v1.37.0, systemd 257) as
// `systemctl show` reports its kubelet unit, with its shipped
// /etc/default/kubelet, which has no final newline and already sets
// KUBELET_EXTRA_ARGS. The e2e harness runs on such nodes.
func TestRun_KindNode(t *testing.T) {
	out := "Environment=\"KUBELET_KUBECONFIG_ARGS=--bootstrap-kubeconfig=/etc/kubernetes/bootstrap-kubelet.conf --kubeconfig=/etc/kubernetes/kubelet.conf\" KUBELET_CONFIG_ARGS=--config=/var/lib/kubelet/config.yaml\n" +
		"EnvironmentFiles=/var/lib/kubelet/kubeadm-flags.env (ignore_errors=yes)\n" +
		"EnvironmentFiles=/etc/default/kubelet (ignore_errors=yes)\n"
	unit := parseUnitEnvironment([]byte(out))
	env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet"})
	for _, f := range unit.Files {
		if !f.Optional {
			t.Fatalf("%s: kind's unit reads it with \"-\"", f.Path)
		}
		env.kubelet.envFiles = append(env.kubelet.envFiles, f.Path)
	}
	env.kubelet.envAssignments = unit.Assignments
	writeHostFile(t, env, defaultKubeletPath, "KUBELET_EXTRA_ARGS=--runtime-cgroups=/system.slice/containerd.service")
	if err := run(env.cfg); err != nil {
		t.Fatal(err)
	}
	want := "KUBELET_EXTRA_ARGS=\"--runtime-cgroups=/system.slice/containerd.service " + flagBinDir + "=" + binDir + " " + flagConfigFile + "=" + env.cfg.ownConfigPath() + "\"\n"
	if got := env.hostFile(t, defaultKubeletPath); got != want {
		t.Fatalf("/etc/default/kubelet:\n%q\nwant\n%q", got, want)
	}
}

// TestRun_PatchRefusesAnAssignmentFromAWildcardFile: the unit's second
// EnvironmentFile= setting is a wildcard expression whose match overrides
// KUBELET_EXTRA_ARGS of /etc/default/kubelet. The installer used to read
// the expression as a file name, missed the match, wrote
// /etc/default/kubelet, restarted kubelet and failed its verification.
// It now refuses before it writes anything or restarts kubelet.
func TestRun_PatchRefusesAnAssignmentFromAWildcardFile(t *testing.T) {
	env := newTestEnv(t, modeAuto, []string{"/usr/bin/kubelet"})
	env.kubelet.envFiles = []string{defaultKubeletPath, "/etc/kubelet.d/*.env"}
	writeHostFile(t, env, defaultKubeletPath, "KUBELET_EXTRA_ARGS=--from-default\n")
	writeHostFile(t, env, "/etc/kubelet.d/10.env", "KUBELET_EXTRA_ARGS=--from-glob\n")
	err := run(env.cfg)
	if err == nil || !strings.Contains(err.Error(), "/etc/kubelet.d/10.env") || !strings.Contains(err.Error(), "plugin.install.mode=none") {
		t.Fatalf("got %v, want a refusal that names the matching file", err)
	}
	if env.restarts != 0 {
		t.Fatalf("kubelet restarted %d times", env.restarts)
	}
	if got := env.hostFile(t, defaultKubeletPath); got != "KUBELET_EXTRA_ARGS=--from-default\n" {
		t.Fatalf("%s changed:\n%s", defaultKubeletPath, got)
	}
	assertAbsent(t, env, env.cfg.ownConfigPath(), binDir+"/harbor-bridge-plugin", stateDir)
}

// TestGlobMatch pins the matcher against glibc's fnmatch(FNM_PERIOD) in the
// C locale; the file cases were checked against systemd 257 (kindest/node
// v1.37.0) with EnvironmentFile= settings.
func TestGlobMatch(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"*.env", "10.env", true},
		{"*.env", ".hidden.env", false},
		{".*.env", ".hidden.env", true},
		{`\.*.env`, ".hidden.env", true},
		{"?a.env", ".a.env", false},
		{"[.]a.env", ".a.env", false},
		{"[0-9]*.env", "10.env", true},
		{"[0-9]*.env", "B.env", false},
		{"[!0-9]*.env", "B.env", true},
		{"[!0-9]*.env", "10.env", false},
		{"[^0-9a-z_]*.env", "B.env", true},
		{"[^0-9a-z_]*.env", "a.env", false},
		{"?0.env", "20.env", true},
		{"?0.env", "0.env", false},
		{"[x].env", "x.env", true},
		{"[x].env", "[x].env", false},
		{`\[x].env`, "[x].env", true},
		{"[x.env", "[x.env", true},
		{"[]]x.env", "]x.env", true},
		{"[!]]x.env", "-x.env", true},
		{"[!]]x.env", "]x.env", false},
		{"[a-]x.env", "-x.env", true},
		{`[\]]x.env`, "]x.env", true},
		{"a*b*c", "aXbYc", true},
		{"a*b*c", "aXbY", false},
		{"a*", "a", true},
		{"*a", "ba", true},
		{"*a*", "bab", true},
		{"**", "x", true},
		{`a\*`, "a*", true},
		{`a\*`, "ab", false},
		{`abc\`, `abc\`, false},
		{"k.env", "k.env", true},
		{"k.env", "k.envx", false},
	} {
		got, err := globMatch(tc.pattern, tc.name)
		if err != nil || got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v, %v; want %v", tc.pattern, tc.name, got, err, tc.want)
		}
	}
	for _, pattern := range []string{"[[:digit:]]0.env", "[[=a=]]", "[[.a.]]"} {
		if _, err := globMatch(pattern, "10.env"); err == nil {
			t.Errorf("%q: accepted", pattern)
		}
	}
}

// TestExpandEnvFile: the files and the order systemd reads for a setting,
// as systemd 257 read them from the same tree (sorted bytewise; a matched
// directory is returned and then skipped by the reader, a wildcard only
// descends into directories).
func TestExpandEnvFile(t *testing.T) {
	env := newTestEnv(t, modePatch, []string{"/usr/bin/kubelet"})
	for _, f := range []string{"10.env", "20.env", "B.env", "a.env", ".hidden.env", "_x.env", "[x].env", "notes.txt"} {
		writeHostFile(t, env, "/etc/g.d/"+f, "V=1\n")
	}
	for _, f := range []string{"/etc/g2/a/k.env", "/etc/g2/b/k.env", "/etc/g2/file"} {
		writeHostFile(t, env, f, "D=1\n")
	}
	if err := os.MkdirAll(env.cfg.hostPath("/etc/g.d/sub.env"), 0o755); err != nil {
		t.Fatal(err)
	}
	for pattern, want := range map[string][]string{
		"/etc/g.d/*.env":       {"/etc/g.d/10.env", "/etc/g.d/20.env", "/etc/g.d/B.env", "/etc/g.d/[x].env", "/etc/g.d/_x.env", "/etc/g.d/a.env", "/etc/g.d/sub.env"},
		"/etc/g.d/[0-9]*.env":  {"/etc/g.d/10.env", "/etc/g.d/20.env"},
		"/etc/g2/*/k.env":      {"/etc/g2/a/k.env", "/etc/g2/b/k.env"},
		"/etc/g2/*":            {"/etc/g2/a", "/etc/g2/b", "/etc/g2/file"},
		"/etc/*2/?/k.env":      {"/etc/g2/a/k.env", "/etc/g2/b/k.env"},
		"/etc/nothing/*.env":   nil,
		"/etc/g.d/*.none":      nil,
		`/etc/g.d/\[x].env`:    {"/etc/g.d/[x].env"},
		"/etc/default/kubelet": {"/etc/default/kubelet"},
	} {
		got, err := env.cfg.expandEnvFile(pattern)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%s: got %q, %v; want %q", pattern, got, err, want)
		}
	}
	if err := os.Symlink("g2", env.cfg.hostPath("/etc/link")); err != nil {
		t.Fatal(err)
	}
	if got, err := env.cfg.expandEnvFile("/etc/l*/a/k.env"); err == nil {
		t.Errorf("a symlink on the way: got %q, want an error", got)
	}
}
