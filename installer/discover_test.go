// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// procEntry is one fake /proc/<pid> directory.
type procEntry struct {
	comm    string
	cmdline []string
	ppid    int    // 0 = default: 1 (a child of init), or 0 for pid 1 itself
	cgroup  string // "" = default: 0::/system.slice/<comm>.service
}

// writeProcEntry (re)writes one fake /proc/<pid> directory.
func writeProcEntry(t *testing.T, root string, pid int, p procEntry) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ppid := p.ppid
	if ppid == 0 && pid != 1 {
		ppid = 1
	}
	cgroup := p.cgroup
	if cgroup == "" {
		cgroup = "0::/system.slice/" + p.comm + ".service"
	}
	files := map[string]string{
		"comm":    p.comm + "\n",
		"cmdline": strings.Join(p.cmdline, "\x00") + "\x00",
		// Real stat lines: "pid (comm) state ppid pgrp ...". A comm with
		// spaces and parentheses must not confuse the parser.
		"stat":   fmt.Sprintf("%d (%s) S %d %d %d 0 -1 4194560\n", pid, p.comm, ppid, pid, pid),
		"cgroup": cgroup + "\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// fakeProc builds a /proc-shaped tree with the given processes.
func fakeProc(t *testing.T, procs map[int]procEntry) string {
	t.Helper()
	root := t.TempDir()
	for pid, p := range procs {
		writeProcEntry(t, root, pid, p)
	}
	// Non-PID entries must be skipped gracefully.
	if err := os.MkdirAll(filepath.Join(root, "sys"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestDiscoverKubelet_FlagsPresent(t *testing.T) {
	root := fakeProc(t, map[int]procEntry{
		1: {comm: "systemd", cmdline: []string{"/sbin/init"}},
		777: {comm: "kubelet", cmdline: []string{
			"/usr/bin/kubelet",
			"--config=/etc/kubernetes/kubelet-config.yaml",
			"--image-credential-provider-bin-dir=/home/kubernetes/bin",
			"--image-credential-provider-config", "/home/kubernetes/cred.yaml",
		}},
	})
	w, err := discoverKubelet(root)
	if err != nil {
		t.Fatalf("discoverKubelet: %v", err)
	}
	if w.BinDir != "/home/kubernetes/bin" {
		t.Fatalf("BinDir = %q", w.BinDir)
	}
	if w.ConfigFile != "/home/kubernetes/cred.yaml" {
		t.Fatalf("ConfigFile = %q (space-separated form must work)", w.ConfigFile)
	}
	if !w.wired() {
		t.Fatal("wired() must be true when both flags present")
	}
}

func TestDiscoverKubelet_FlagsAbsent(t *testing.T) {
	root := fakeProc(t, map[int]procEntry{
		42: {comm: "kubelet", cmdline: []string{"/usr/bin/kubelet", "--max-pods=110"}},
	})
	w, err := discoverKubelet(root)
	if err != nil {
		t.Fatalf("discoverKubelet: %v", err)
	}
	if w.wired() {
		t.Fatal("wired() must be false without the flags")
	}
}

func TestDiscoverKubelet_NoKubelet(t *testing.T) {
	root := fakeProc(t, map[int]procEntry{
		1: {comm: "systemd", cmdline: []string{"/sbin/init"}},
	})
	if _, err := discoverKubelet(root); err == nil {
		t.Fatal("expected error when no kubelet process exists")
	}
}

func TestFlagValue_Forms(t *testing.T) {
	argv := []string{"kubelet", "--a=1", "--b", "2", "--c"}
	if v := flagValue(argv, "--a"); v != "1" {
		t.Fatalf("= form: %q", v)
	}
	if v := flagValue(argv, "--b"); v != "2" {
		t.Fatalf("space form: %q", v)
	}
	if v := flagValue(argv, "--missing"); v != "" {
		t.Fatalf("missing flag: %q", v)
	}
	if v := flagValue(argv, "--c"); v != "" {
		t.Fatalf("trailing valueless flag: %q", v)
	}
}

// TestFindKubelet_IgnoresSpoofedProcesses is the audit H4 regression: any
// pod can name its process "kubelet" and fake a command line; the
// privileged installer must not be steered by it.
func TestFindKubelet_IgnoresSpoofedProcesses(t *testing.T) {
	real := []string{"/usr/bin/kubelet", "--image-credential-provider-bin-dir=/real/bin", "--image-credential-provider-config=/real/cfg.yaml"}
	spoof := []string{"kubelet", "--image-credential-provider-bin-dir=/etc/cron.d", "--image-credential-provider-config=/etc/evil.yaml"}
	root := fakeProc(t, map[int]procEntry{
		1: {comm: "systemd", cmdline: []string{"/sbin/init"}},
		// Sorts BEFORE the real kubelet: the old first-match picked it.
		100: {comm: "kubelet", cmdline: spoof, ppid: 4242, cgroup: "0::/kubelet.slice/kubelet-kubepods.slice/kubepods-besteffort.slice/cri-containerd-x.scope"},
		// Re-parented to init but still inside a pod cgroup.
		101: {comm: "kubelet", cmdline: spoof, ppid: 1, cgroup: "0::/kubepods.slice/kubepods-pod1.slice/cri-containerd-y.scope"},
		777: {comm: "kubelet", cmdline: real},
	})
	w, err := discoverKubelet(root)
	if err != nil {
		t.Fatal(err)
	}
	if w.BinDir != "/real/bin" || w.ConfigFile != "/real/cfg.yaml" {
		t.Fatalf("discovery was steered by a spoofed process: %+v", w)
	}
}

func TestFindKubelet_RefusesToGuess(t *testing.T) {
	root := fakeProc(t, map[int]procEntry{
		10: {comm: "kubelet", cmdline: []string{"/usr/bin/kubelet"}},
		11: {comm: "kubelet", cmdline: []string{"/opt/bin/kubelet"}},
	})
	if _, err := discoverKubelet(root); err == nil || !strings.Contains(err.Error(), "refusing to guess") {
		t.Fatalf("two plausible kubelets: got %v", err)
	}
}

func TestFindKubelet_OnlySpoofedIsAnError(t *testing.T) {
	root := fakeProc(t, map[int]procEntry{
		100: {comm: "kubelet", cmdline: []string{"kubelet"}, ppid: 999},
	})
	_, err := discoverKubelet(root)
	if err == nil || !strings.Contains(err.Error(), "parent is pid 999") {
		t.Fatalf("got %v, want an error that names the rejected candidate", err)
	}
}

func TestDiscoverKubelet_RejectsRelativeOrUncleanFlagPaths(t *testing.T) {
	for _, bad := range []string{"rel/bin", "/etc/../../tmp", "/etc/kubernetes/"} {
		root := fakeProc(t, map[int]procEntry{
			7: {comm: "kubelet", cmdline: []string{"/usr/bin/kubelet", flagBinDir + "=" + bad, flagConfigFile + "=/c.yaml"}},
		})
		if _, err := discoverKubelet(root); err == nil {
			t.Errorf("discovered bin dir %q accepted", bad)
		}
	}
}

func TestReadPPID_CommWithParens(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "stat"), []byte("55 (evil) 1 (x)) S 77 55 55 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ppid, err := readPPID(root); err != nil || ppid != 77 {
		t.Fatalf("readPPID = %d, %v; want 77", ppid, err)
	}
}
