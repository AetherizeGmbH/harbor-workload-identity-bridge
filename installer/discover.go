// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Kubelet's credential-provider settings are flag-only — there is no
// KubeletConfiguration field and no drop-in equivalent — so the live
// command line is authoritative for whether and where a node has
// credential providers wired. See ADR-0021.
const (
	flagBinDir     = "--image-credential-provider-bin-dir"
	flagConfigFile = "--image-credential-provider-config"
)

// kubeletWiring is what discovery finds on the live kubelet process.
type kubeletWiring struct {
	BinDir     string
	ConfigFile string
}

// wired reports whether both credential-provider flags are present.
func (w kubeletWiring) wired() bool {
	return w.BinDir != "" && w.ConfigFile != ""
}

// discoverKubelet scans procRoot (normally /proc — hostPID makes the
// host's processes visible there) for the kubelet process and parses
// its command line for the credential-provider flags.
func discoverKubelet(procRoot string) (kubeletWiring, error) {
	pid, cmdline, err := findKubelet(procRoot)
	if err != nil {
		return kubeletWiring{}, err
	}
	w := kubeletWiring{
		BinDir:     flagValue(cmdline, flagBinDir),
		ConfigFile: flagValue(cmdline, flagConfigFile),
	}
	logf("found kubelet (pid %d): bin-dir=%q config=%q", pid, w.BinDir, w.ConfigFile)
	return w, nil
}

// findKubelet returns the PID and NUL-split command line of the first
// process whose comm is "kubelet".
func findKubelet(procRoot string) (int, []string, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return 0, nil, fmt.Errorf("read %s: %w", procRoot, err)
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		comm, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "comm"))
		if err != nil {
			continue // process vanished or unreadable — skip
		}
		if strings.TrimSpace(string(comm)) != "kubelet" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		cmdline := splitCmdline(raw)
		if len(cmdline) == 0 {
			continue
		}
		return pid, cmdline, nil
	}
	return 0, nil, fmt.Errorf("no kubelet process found under %s (is hostPID enabled?)", procRoot)
}

// splitCmdline splits the NUL-separated /proc/<pid>/cmdline content.
func splitCmdline(raw []byte) []string {
	parts := bytes.Split(bytes.TrimRight(raw, "\x00"), []byte{0})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if len(p) > 0 {
			out = append(out, string(p))
		}
	}
	return out
}

// flagValue extracts a flag's value from an argv slice, handling both
// the "--flag=value" and "--flag value" forms. Returns "" when absent.
func flagValue(argv []string, flag string) string {
	for i, arg := range argv {
		if v, ok := strings.CutPrefix(arg, flag+"="); ok {
			return v
		}
		if arg == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}
