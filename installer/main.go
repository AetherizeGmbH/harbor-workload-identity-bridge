// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

// Binary harbor-bridge-installer wires the harbor-bridge kubelet
// credential-provider plugin into a node. It replaces the inline shell
// script the plugin DaemonSet used before ADR-0021 and supports four
// modes:
//
//   - auto:  inspect the live kubelet command line; merge if the
//     --image-credential-provider-* flags are already set (managed
//     clouds), patch otherwise (kind, kubeadm). Distributions that embed
//     or supervise kubelet themselves (k3s, RKE2, Talos) are not
//     supported by auto/merge/patch — use none, or plugin.enabled=false.
//   - merge: inject our provider entry into the node's existing
//     CredentialProviderConfig and drop the binary into the existing
//     bin dir. Foreign providers and unknown fields are preserved.
//   - patch: own bin/config dirs + parse-merge of /etc/default/kubelet
//     (KUBELET_EXTRA_ARGS), then restart kubelet.
//   - none:  copy files only; the operator owns the kubelet flags.
//
// Kubelet reads the credential-provider config once at startup but
// execs plugin binaries per pull, so the installer restarts kubelet
// only when restart-relevant content actually changed (content-hash
// idempotency via a host state file). CA/mTLS rotation and binary
// updates never restart kubelet.
//
// With --sync the installer instead runs as the DaemonSet's
// long-running container: it re-copies the CA/mTLS files to the host
// whenever the mounted Secret volumes rotate. It never touches kubelet.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

func main() {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "harbor-bridge-installer: config error: %v\n", err)
		os.Exit(1)
	}
	if len(os.Args) > 1 && os.Args[1] == "--sync" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		if err := runSync(ctx, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "harbor-bridge-installer: sync error: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "harbor-bridge-installer: %v\n", err)
		os.Exit(1)
	}
}

// Install modes. See ADR-0021.
const (
	modeAuto  = "auto"
	modeMerge = "merge"
	modePatch = "patch"
	modeNone  = "none"
)

// config is what the installer reads from the environment. The chart's
// plugin DaemonSet plumbs these through the init/sync container env; the
// remaining fields are fixed paths inside the plugin image and pod, set
// to their defaults by loadConfig and overridden only by tests.
type config struct {
	Mode string // INSTALL_MODE: auto|merge|patch|none.

	// HostRoot is where the node's / is mounted inside the container.
	// In none mode (and for the sync container) only the narrow host dirs
	// are mounted, at HostRoot+HostBinDir / HostRoot+HostConfigDir, so all
	// node paths are uniformly addressed as HostRoot+<node path>.
	HostRoot string // HOST_ROOT, default /host.

	HostBinDir    string // HOST_BIN_DIR: node path for the plugin binary (patch/none).
	HostConfigDir string // HOST_CONFIG_DIR: node path for config + CA/mTLS files (all modes).

	// Merge-mode overrides. When set, discovery of the kubelet flags
	// is skipped and these node paths are used directly.
	MergeBinDir     string // INSTALL_MERGE_BIN_DIR, optional.
	MergeConfigFile string // INSTALL_MERGE_CONFIG_FILE, optional.

	MTLSEnabled bool   // MTLS_ENABLED == "true".
	KubeletUnit string // KUBELET_UNIT, default kubelet.
	StateDir    string // STATE_DIR: node path, default /var/lib/harbor-bridge.
	NodeIP      string // NODE_IP: substituted for the literal $(NODE_IP) in the rendered config.

	// Fixed locations inside the plugin image / pod.
	SourcePlugin     string
	SourceConfig     string
	SourceCA         string
	SourceClientCert string
	SourceClientKey  string
	ProviderName     string
	ProcRoot         string
	SyncInterval     time.Duration

	kubelet kubeletControl
	verify  verifyTiming
}

func loadConfig(getenv func(string) string) (*config, error) {
	def := func(key, fallback string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return fallback
	}
	c := &config{
		Mode:             def("INSTALL_MODE", modeAuto),
		HostRoot:         def("HOST_ROOT", "/host"),
		HostBinDir:       getenv("HOST_BIN_DIR"),
		HostConfigDir:    getenv("HOST_CONFIG_DIR"),
		MergeBinDir:      getenv("INSTALL_MERGE_BIN_DIR"),
		MergeConfigFile:  getenv("INSTALL_MERGE_CONFIG_FILE"),
		MTLSEnabled:      getenv("MTLS_ENABLED") == "true",
		KubeletUnit:      def("KUBELET_UNIT", "kubelet"),
		StateDir:         def("STATE_DIR", "/var/lib/harbor-bridge"),
		NodeIP:           getenv("NODE_IP"),
		SourcePlugin:     "/plugin/harbor-bridge-plugin",
		SourceConfig:     "/config/credential-provider-config.yaml",
		SourceCA:         "/tls/ca.crt",
		SourceClientCert: "/mtls/tls.crt",
		SourceClientKey:  "/mtls/tls.key",
		ProviderName:     "harbor-bridge-plugin",
		ProcRoot:         "/proc",
		SyncInterval:     60 * time.Second,
		kubelet:          nsenterControl{},
		verify:           defaultVerifyTiming,
	}
	switch c.Mode {
	case modeAuto, modeMerge, modePatch, modeNone:
	default:
		return nil, fmt.Errorf("INSTALL_MODE must be one of auto|merge|patch|none (got %q)", c.Mode)
	}
	if c.HostConfigDir == "" {
		return nil, fmt.Errorf("HOST_CONFIG_DIR is required")
	}
	if c.HostBinDir == "" && c.Mode != modeMerge {
		return nil, fmt.Errorf("HOST_BIN_DIR is required for mode %q", c.Mode)
	}
	if (c.MergeBinDir == "") != (c.MergeConfigFile == "") {
		return nil, fmt.Errorf("INSTALL_MERGE_BIN_DIR and INSTALL_MERGE_CONFIG_FILE must be set together")
	}
	for name, v := range map[string]string{
		"HOST_BIN_DIR": c.HostBinDir, "HOST_CONFIG_DIR": c.HostConfigDir, "STATE_DIR": c.StateDir,
		"INSTALL_MERGE_BIN_DIR": c.MergeBinDir, "INSTALL_MERGE_CONFIG_FILE": c.MergeConfigFile,
	} {
		if v == "" {
			continue
		}
		if err := validNodePath(v); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	if err := validUnitName(c.KubeletUnit); err != nil {
		return nil, fmt.Errorf("KUBELET_UNIT: %w", err)
	}
	return c, nil
}

// nodePathChars is the character set of node paths. The paths are written
// unquoted into KUBELET_EXTRA_ARGS in /etc/default/kubelet (an
// EnvironmentFile) and into the provider config, where a space, quote or
// "$" would split or expand them.
var nodePathChars = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

// validNodePath accepts only absolute, already-clean node paths from a
// restricted character set. They are joined under HostRoot, so a relative
// or ".."-carrying value would address something other than what it names.
func validNodePath(p string) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p || !nodePathChars.MatchString(p) {
		return fmt.Errorf("path %q must be absolute, clean, and use only letters, digits and . _ - /", p)
	}
	return nil
}

// hostPath maps a node path to the path the container reads/writes it
// at, under the HostRoot mount.
func (c *config) hostPath(nodePath string) string {
	return filepath.Join(c.HostRoot, filepath.Clean("/"+nodePath))
}

func logf(format string, args ...any) {
	fmt.Printf("harbor-bridge-installer: "+format+"\n", args...)
}
