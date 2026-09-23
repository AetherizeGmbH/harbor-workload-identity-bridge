// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeProc builds a /proc-shaped tree with the given processes.
func fakeProc(t *testing.T, procs map[int]struct {
	comm    string
	cmdline []string
}) string {
	t.Helper()
	root := t.TempDir()
	for pid, p := range procs {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(p.comm+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		raw := strings.Join(p.cmdline, "\x00") + "\x00"
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Non-PID entries must be skipped gracefully.
	if err := os.MkdirAll(filepath.Join(root, "sys"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestDiscoverKubelet_FlagsPresent(t *testing.T) {
	root := fakeProc(t, map[int]struct {
		comm    string
		cmdline []string
	}{
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
	root := fakeProc(t, map[int]struct {
		comm    string
		cmdline []string
	}{
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
	root := fakeProc(t, map[int]struct {
		comm    string
		cmdline []string
	}{
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
