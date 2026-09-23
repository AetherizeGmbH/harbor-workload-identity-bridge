// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const configFileName = "credential-provider-config.yaml"

const defaultKubeletPath = "/etc/default/kubelet"

// run performs one install pass. See ADR-0021 for the mode semantics
// and the restart policy.
func run(cfg *config) error {
	rendered, err := os.ReadFile(cfg.SourceConfig)
	if err != nil {
		return fmt.Errorf("read rendered credential-provider config: %w", err)
	}
	rendered, err = substituteNodeIP(rendered, cfg.NodeIP)
	if err != nil {
		return err
	}

	mode := cfg.Mode
	var wiring kubeletWiring
	switch mode {
	case modeAuto:
		wiring, err = discoverKubelet(cfg.ProcRoot)
		if err != nil {
			return fmt.Errorf("mode auto: %w", err)
		}
		if wiring.wired() {
			mode = modeMerge
		} else {
			mode = modePatch
		}
		logf("mode auto resolved to %s", mode)
	case modeMerge:
		if cfg.MergeBinDir != "" {
			wiring = kubeletWiring{BinDir: cfg.MergeBinDir, ConfigFile: cfg.MergeConfigFile}
			logf("merge targets overridden: bin-dir=%q config=%q", wiring.BinDir, wiring.ConfigFile)
		} else {
			wiring, err = discoverKubelet(cfg.ProcRoot)
			if err != nil {
				return fmt.Errorf("mode merge: %w", err)
			}
			if !wiring.wired() {
				return fmt.Errorf("mode merge: kubelet runs without --image-credential-provider-* flags — nothing to merge into; use mode patch (self-managed nodes) or set plugin.install.binDir/configFile")
			}
		}
	}

	// CA (+ optional mTLS client pair) always live under HostConfigDir:
	// the provider entry references them by absolute path, so they are
	// independent of which config file kubelet reads. Their rotation
	// never restarts kubelet — the plugin reads them on every exec.
	if err := syncAuxFiles(cfg); err != nil {
		return err
	}

	switch mode {
	case modeNone:
		return runNone(cfg, rendered)
	case modePatch:
		return runPatch(cfg, rendered)
	case modeMerge:
		return runMerge(cfg, rendered, wiring)
	default:
		return fmt.Errorf("unreachable mode %q", mode)
	}
}

// runNone drops the binary and config into the chart-owned dirs and
// leaves kubelet alone — the operator owns the flags.
func runNone(cfg *config, rendered []byte) error {
	if err := installPluginBinary(cfg, cfg.HostBinDir); err != nil {
		return err
	}
	configPath := filepath.Join(cfg.HostConfigDir, configFileName)
	changed, err := writeFileAtomic(cfg.hostPath(configPath), rendered, 0o644)
	if err != nil {
		return err
	}
	logf("mode none: files installed (config changed: %v); kubelet wiring is the operator's responsibility", changed)
	return nil
}

// runPatch owns the config file and wires kubelet via a parse-merge of
// /etc/default/kubelet, restarting kubelet when restart-relevant
// content changed.
func runPatch(cfg *config, rendered []byte) error {
	if err := installPluginBinary(cfg, cfg.HostBinDir); err != nil {
		return err
	}

	configPath := filepath.Join(cfg.HostConfigDir, configFileName)
	configChanged, err := writeFileAtomic(cfg.hostPath(configPath), rendered, 0o644)
	if err != nil {
		return err
	}

	existingEnv, err := os.ReadFile(cfg.hostPath(defaultKubeletPath))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", defaultKubeletPath, err)
	}
	desiredEnv, err := mergeExtraArgs(existingEnv, cfg.HostBinDir, configPath)
	if err != nil {
		return err
	}
	envChanged, err := writeFileAtomic(cfg.hostPath(defaultKubeletPath), desiredEnv, 0o644)
	if err != nil {
		return err
	}

	hash := contentHash(rendered, desiredEnv)
	return finishWithRestart(cfg, modePatch, cfg.HostBinDir, configPath, hash, configChanged || envChanged)
}

// runMerge injects our provider entry into the node's existing
// credential-provider config and drops the binary into the existing
// bin dir; kubelet's flags are not touched.
func runMerge(cfg *config, rendered []byte, wiring kubeletWiring) error {
	if err := installPluginBinary(cfg, wiring.BinDir); err != nil {
		return err
	}

	existing, err := os.ReadFile(cfg.hostPath(wiring.ConfigFile))
	if err != nil {
		// Kubelet refuses to start when the flag points at a missing
		// file, so on a live node this indicates a wrong override.
		return fmt.Errorf("read node credential-provider config %s: %w", wiring.ConfigFile, err)
	}
	entry, err := renderedProvider(rendered, cfg.ProviderName)
	if err != nil {
		return err
	}
	merged, mergeChanged, err := mergeProvider(existing, entry)
	if err != nil {
		return err
	}
	written, err := writeFileAtomic(cfg.hostPath(wiring.ConfigFile), merged, 0o644)
	if err != nil {
		return err
	}
	if mergeChanged {
		logf("merged provider %q into %s", cfg.ProviderName, wiring.ConfigFile)
	}

	hash := contentHash(merged)
	return finishWithRestart(cfg, modeMerge, wiring.BinDir, wiring.ConfigFile, hash, mergeChanged || written)
}

// finishWithRestart applies the ADR-0021 restart policy: restart iff
// restart-relevant content changed on this pass OR the state file does
// not record a successful restart for exactly this content (covers the
// crash window between write and restart). The state is persisted only
// after a successful restart.
func finishWithRestart(cfg *config, mode, binDir, configFile, hash string, changedNow bool) error {
	st, err := loadState(cfg.hostPath(cfg.StateDir))
	if err != nil {
		return err
	}
	if !changedNow && st.matches(mode, binDir, configFile, hash) {
		logf("kubelet wiring is current; not restarting")
		return nil
	}
	logf("restarting kubelet unit %q (config or flags changed)", cfg.KubeletUnit)
	if err := restartKubelet(cfg.KubeletUnit); err != nil {
		return err
	}
	if err := saveState(cfg.hostPath(cfg.StateDir), &state{
		Mode:        mode,
		BinDir:      binDir,
		ConfigFile:  configFile,
		AppliedHash: hash,
	}); err != nil {
		return err
	}
	logf("install complete (mode %s)", mode)
	return nil
}

// installPluginBinary copies the plugin into binDir (a node path).
func installPluginBinary(cfg *config, binDir string) error {
	dst := filepath.Join(binDir, "harbor-bridge-plugin")
	changed, err := copyFile(cfg.SourcePlugin, cfg.hostPath(dst), 0o755)
	if err != nil {
		return err
	}
	if changed {
		logf("installed plugin binary → %s", dst)
	}
	return nil
}

// syncAuxFiles copies the CA (and mTLS client pair when enabled) into
// HostConfigDir. Shared by the install pass and the --sync loop.
func syncAuxFiles(cfg *config) error {
	caDst := filepath.Join(cfg.HostConfigDir, "harbor-bridge-ca.crt")
	changed, err := copyFile(cfg.SourceCA, cfg.hostPath(caDst), 0o644)
	if err != nil {
		return err
	}
	if changed {
		logf("installed bridge CA → %s", caDst)
	}
	if !cfg.MTLSEnabled {
		return nil
	}
	certDst := filepath.Join(cfg.HostConfigDir, "harbor-bridge-client.crt")
	changed, err = copyFile(cfg.SourceClientCert, cfg.hostPath(certDst), 0o644)
	if err != nil {
		return err
	}
	if changed {
		logf("installed mTLS client cert → %s", certDst)
	}
	keyDst := filepath.Join(cfg.HostConfigDir, "harbor-bridge-client.key")
	changed, err = copyFile(cfg.SourceClientKey, cfg.hostPath(keyDst), 0o600)
	if err != nil {
		return err
	}
	if changed {
		logf("installed mTLS client key → %s", keyDst)
	}
	return nil
}
