// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

// Binary harbor-bridge-installer wires the harbor-bridge kubelet
// credential-provider plugin into a node. It replaces the inline shell
// script the plugin DaemonSet used before ADR-0021 and supports four
// modes:
//
//   - auto:  inspect the live kubelet command line; merge if the
//     --image-credential-provider-* flags are already set (managed
//     clouds), patch otherwise (kind, kubeadm, k3s).
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
	"fmt"
	"os"
)

func main() {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "harbor-bridge-installer: config error: %v\n", err)
		os.Exit(1)
	}
	if len(os.Args) > 1 && os.Args[1] == "--sync" {
		if err := runSync(cfg); err != nil {
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
// plugin DaemonSet plumbs these through the init/sync container env.
type config struct {
	Mode string // INSTALL_MODE: auto|merge|patch|none.

	// HostRoot is where the node's / is mounted inside the container.
	// In none mode only the two narrow host dirs are mounted, at
	// HostRoot+HostBinDir / HostRoot+HostConfigDir, so all node paths
	// are uniformly addressed as HostRoot+<node path>.
	HostRoot string // HOST_ROOT, default /host.

	HostBinDir    string // HOST_BIN_DIR: node path for the plugin binary (patch/none).
	HostConfigDir string // HOST_CONFIG_DIR: node path for config + CA/mTLS files (all modes).

	// Merge-mode overrides. When set, discovery of the kubelet flags
	// is skipped and these node paths are used directly.
	MergeBinDir     string // INSTALL_MERGE_BIN_DIR, optional.
	MergeConfigFile string // INSTALL_MERGE_CONFIG_FILE, optional.

	SourcePlugin     string // SOURCE_PLUGIN, default /plugin/harbor-bridge-plugin.
	SourceConfig     string // SOURCE_CONFIG, default /config/credential-provider-config.yaml.
	SourceCA         string // SOURCE_CA, default /tls/ca.crt.
	MTLSEnabled      bool   // MTLS_ENABLED == "true".
	SourceClientCert string // SOURCE_CLIENT_CERT, default /mtls/tls.crt.
	SourceClientKey  string // SOURCE_CLIENT_KEY, default /mtls/tls.key.

	ProviderName string // PROVIDER_NAME, default harbor-bridge-plugin.
	KubeletUnit  string // KUBELET_UNIT, default kubelet.
	StateDir     string // STATE_DIR: node path, default /var/lib/harbor-bridge.
	NodeIP       string // NODE_IP: substituted for the literal $(NODE_IP) in the rendered config.
	ProcRoot     string // PROC_ROOT, default /proc. Overridden only by tests.

	SyncInterval string // SYNC_INTERVAL, default 60s (Go duration).
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
		SourcePlugin:     def("SOURCE_PLUGIN", "/plugin/harbor-bridge-plugin"),
		SourceConfig:     def("SOURCE_CONFIG", "/config/credential-provider-config.yaml"),
		SourceCA:         def("SOURCE_CA", "/tls/ca.crt"),
		MTLSEnabled:      getenv("MTLS_ENABLED") == "true",
		SourceClientCert: def("SOURCE_CLIENT_CERT", "/mtls/tls.crt"),
		SourceClientKey:  def("SOURCE_CLIENT_KEY", "/mtls/tls.key"),
		ProviderName:     def("PROVIDER_NAME", "harbor-bridge-plugin"),
		KubeletUnit:      def("KUBELET_UNIT", "kubelet"),
		StateDir:         def("STATE_DIR", "/var/lib/harbor-bridge"),
		NodeIP:           getenv("NODE_IP"),
		ProcRoot:         def("PROC_ROOT", "/proc"),
		SyncInterval:     def("SYNC_INTERVAL", "60s"),
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
	return c, nil
}

// hostPath maps a node path to the path the container reads/writes it
// at, under the HostRoot mount.
func (c *config) hostPath(nodePath string) string {
	return c.HostRoot + nodePath
}

func logf(format string, args ...any) {
	fmt.Printf("harbor-bridge-installer: "+format+"\n", args...)
}
