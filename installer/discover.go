// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

// validate rejects flag values the installer must not act on: the paths
// are joined under the host-root mount, so a relative or unclean value
// could address something other than what kubelet reads.
func (w kubeletWiring) validate() error {
	for flag, v := range map[string]string{flagBinDir: w.BinDir, flagConfigFile: w.ConfigFile} {
		if v == "" {
			continue
		}
		if err := validNodePath(v); err != nil {
			return fmt.Errorf("kubelet %s: %w", flag, err)
		}
	}
	return nil
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
	if err := w.validate(); err != nil {
		return kubeletWiring{}, err
	}
	logf("found kubelet (pid %d): bin-dir=%q config=%q", pid, w.BinDir, w.ConfigFile)
	return w, nil
}

// findKubelet returns the PID and NUL-split command line of THE node
// kubelet.
//
// The installer runs privileged with hostPID and sees every process on
// the node, including those of unprivileged pods — and any pod can name
// its process "kubelet" and fake a command line. Picking the first match
// would let such a pod steer where the privileged installer writes a
// root-owned binary and config, and silently skip the real kubelet
// (audit H4). A candidate therefore must also be a direct child of the
// host's init (PPID 1: the systemd unit) and must not live in a pod
// cgroup. Container processes are never re-parented to PID 1 — containerd
// shims are subreapers — so a pod cannot pass the first check. If several
// candidates still remain, the installer refuses to guess.
func findKubelet(procRoot string) (int, []string, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return 0, nil, fmt.Errorf("read %s: %w", procRoot, err)
	}
	// The host kubelet shares init's mount namespace; a process in any pod
	// does not, whatever it calls itself or whoever its parent is.
	hostMnt, err := os.Readlink(filepath.Join(procRoot, "1", "ns", "mnt"))
	if err != nil {
		return 0, nil, fmt.Errorf("read init's mount namespace under %s (needs hostPID and a privileged container): %w", procRoot, err)
	}
	type candidate struct {
		pid     int
		cmdline []string
	}
	var found []candidate
	var rejected []string
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(procRoot, e.Name())
		comm, err := os.ReadFile(filepath.Join(dir, "comm"))
		if err != nil || strings.TrimSpace(string(comm)) != "kubelet" {
			continue // not kubelet, or vanished / unreadable
		}
		ppid, err := readPPID(dir)
		if err != nil {
			rejected = append(rejected, fmt.Sprintf("pid %d: %v", pid, err))
			continue
		}
		if ppid != 1 {
			rejected = append(rejected, fmt.Sprintf("pid %d: parent is pid %d, not init", pid, ppid))
			continue
		}
		// Fail closed: a candidate whose cgroup or mount namespace cannot
		// be read is not the host kubelet as far as the installer knows.
		cg, err := os.ReadFile(filepath.Join(dir, "cgroup"))
		if err != nil {
			rejected = append(rejected, fmt.Sprintf("pid %d: cannot read cgroup: %v", pid, err))
			continue
		}
		if bytes.Contains(cg, []byte("kubepods")) {
			rejected = append(rejected, fmt.Sprintf("pid %d: runs in a pod cgroup", pid))
			continue
		}
		if mnt, err := os.Readlink(filepath.Join(dir, "ns", "mnt")); err != nil || mnt != hostMnt {
			rejected = append(rejected, fmt.Sprintf("pid %d: not in the host mount namespace", pid))
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, "cmdline"))
		if err != nil {
			continue
		}
		cmdline := splitCmdline(raw)
		if len(cmdline) == 0 {
			continue
		}
		found = append(found, candidate{pid: pid, cmdline: cmdline})
	}
	switch len(found) {
	case 1:
		for _, r := range rejected {
			logf("ignored process named kubelet (%s)", r)
		}
		return found[0].pid, found[0].cmdline, nil
	case 0:
		msg := fmt.Sprintf("no kubelet process found under %s (is hostPID enabled?)", procRoot)
		if len(rejected) > 0 {
			sort.Strings(rejected)
			msg += "; rejected: " + strings.Join(rejected, "; ")
		}
		return 0, nil, fmt.Errorf("%s. Distributions that embed or supervise kubelet themselves (k3s, RKE2, Talos) are not supported by auto/merge/patch: use plugin.install.mode=none and wire the flags through the distribution's own configuration, or plugin.enabled=false", msg)
	default:
		pids := make([]string, len(found))
		for i, c := range found {
			pids[i] = strconv.Itoa(c.pid)
		}
		return 0, nil, fmt.Errorf("several processes look like the node kubelet (pids %s); refusing to guess which one to wire", strings.Join(pids, ", "))
	}
}

// readPPID parses the parent PID from /proc/<pid>/stat. The comm field is
// parenthesised and may itself contain spaces and parentheses, so the
// fields are read after the LAST ')'.
func readPPID(procDir string) (int, error) {
	raw, err := os.ReadFile(filepath.Join(procDir, "stat"))
	if err != nil {
		return 0, fmt.Errorf("read stat: %w", err)
	}
	i := bytes.LastIndexByte(raw, ')')
	if i < 0 {
		return 0, fmt.Errorf("malformed stat")
	}
	fields := strings.Fields(string(raw[i+1:]))
	// fields[0] = state, fields[1] = ppid
	if len(fields) < 2 {
		return 0, fmt.Errorf("malformed stat")
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, fmt.Errorf("malformed stat ppid %q", fields[1])
	}
	return ppid, nil
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
// The last occurrence wins, as it does for kubelet's own flag parser
// (a drop-in can repeat a flag); taking the first would wire a file
// kubelet does not read.
func flagValue(argv []string, flag string) string {
	value := ""
	for i, arg := range argv {
		if v, ok := strings.CutPrefix(arg, flag+"="); ok {
			value = v
		} else if arg == flag && i+1 < len(argv) {
			value = argv[i+1]
		}
	}
	return value
}
