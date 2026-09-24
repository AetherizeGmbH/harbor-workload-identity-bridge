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

// ownConfigPath is where patch mode writes the provider config.
func (c *config) ownConfigPath() string {
	return filepath.Join(c.HostConfigDir, configFileName)
}

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
		switch {
		case !wiring.wired():
			mode = modePatch
		case wiring.BinDir == cfg.HostBinDir && wiring.ConfigFile == cfg.ownConfigPath():
			// The flags point at OUR patch-mode paths: this is our own
			// earlier patch install, not a cloud-provisioned config to
			// merge into. Resolving to merge here flipped every re-roll
			// after the first install to merge mode and restarted
			// kubelet for nothing (audit H3).
			mode = modePatch
		default:
			mode = modeMerge
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
	changed, err := writeFileAtomic(cfg.hostPath(cfg.ownConfigPath()), rendered, 0o644)
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
	configPath := cfg.ownConfigPath()

	// Compute everything that can be refused BEFORE touching the host,
	// so a refusal never leaves a half-install behind.
	existingEnv, err := readHostFile(cfg.hostPath(defaultKubeletPath))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", defaultKubeletPath, err)
	}
	desiredEnv, err := mergeExtraArgs(existingEnv, cfg.HostBinDir, configPath)
	if err != nil {
		return err
	}

	if err := installPluginBinary(cfg, cfg.HostBinDir); err != nil {
		return err
	}
	configChanged, err := writeFileAtomic(cfg.hostPath(configPath), rendered, 0o644)
	if err != nil {
		return err
	}
	envChanged, err := writeFileAtomic(cfg.hostPath(defaultKubeletPath), desiredEnv, 0o644)
	if err != nil {
		return err
	}

	want := kubeletWiring{BinDir: cfg.HostBinDir, ConfigFile: configPath}
	verify := func() error {
		// The flags only reach kubelet if its unit actually sources
		// /etc/default/kubelet; confirm on the live process.
		got, err := discoverKubelet(cfg.ProcRoot)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("running kubelet has bin-dir=%q config=%q, want %q/%q — does the %s unit source %s?",
				got.BinDir, got.ConfigFile, want.BinDir, want.ConfigFile, cfg.KubeletUnit, defaultKubeletPath)
		}
		return nil
	}
	hash := contentHash(rendered, desiredEnv)
	return finishWithRestart(cfg, modePatch, cfg.HostBinDir, configPath, hash, configChanged || envChanged, verify)
}

// runMerge injects our provider entry into the node's existing
// credential-provider config and drops the binary into the existing
// bin dir; kubelet's flags are not touched.
func runMerge(cfg *config, rendered []byte, wiring kubeletWiring) error {
	// Read, validate, and merge first: an unknown schema or a missing file
	// is refused before anything is written (no half-install).
	existing, err := readHostFile(cfg.hostPath(wiring.ConfigFile))
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

	if err := installPluginBinary(cfg, wiring.BinDir); err != nil {
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
	return finishWithRestart(cfg, modeMerge, wiring.BinDir, wiring.ConfigFile, hash, mergeChanged || written, nil)
}

// finishWithRestart applies the ADR-0021 restart policy: restart iff
// restart-relevant content changed on this pass OR the state file does
// not record a successful restart for exactly this content (covers the
// crash window between write and restart). The state is persisted only
// after the restart is VERIFIED (unit stably active and, when verify is
// set, the running kubelet wired as intended).
func finishWithRestart(cfg *config, mode, binDir, configFile, hash string, changedNow bool, verify func() error) error {
	stateDir := cfg.hostPath(cfg.StateDir)
	st, err := loadState(stateDir)
	if err != nil {
		return err
	}
	if !changedNow && st.matches(mode, binDir, configFile, hash) {
		logf("kubelet wiring is current; not restarting")
		return nil
	}
	// Prove the state can be recorded BEFORE restarting: otherwise a
	// persistently unwritable state dir would restart kubelet on every
	// single re-roll.
	if err := ensureWritableDir(stateDir); err != nil {
		return fmt.Errorf("state dir %s: %w", cfg.StateDir, err)
	}
	logf("restarting kubelet unit %q (config or flags changed)", cfg.KubeletUnit)
	if err := cfg.kubelet.restart(cfg.KubeletUnit); err != nil {
		return err
	}
	if err := waitKubeletHealthy(cfg.kubelet, cfg.KubeletUnit, cfg.verify, verify); err != nil {
		return err
	}
	if err := saveState(stateDir, &state{
		Mode:        mode,
		BinDir:      binDir,
		ConfigFile:  configFile,
		AppliedHash: hash,
	}); err != nil {
		return err
	}
	logf("install complete (mode %s); kubelet verified healthy", mode)
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
