// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const configFileName = "credential-provider-config.yaml"

const defaultKubeletPath = "/etc/default/kubelet"

// ownConfigPath is where patch and none mode write the provider config.
// Every install on the node that uses the same hostConfigDir shares it
// (ADR-0029).
func (c *config) ownConfigPath() string {
	return filepath.Join(c.HostConfigDir, configFileName)
}

// run performs one install pass. See ADR-0021 for the mode semantics
// and the restart policy, ADR-0029 for several installs on one node.
func run(cfg *config) error {
	rendered, err := os.ReadFile(cfg.SourceConfig)
	if err != nil {
		return fmt.Errorf("read rendered credential-provider config: %w", err)
	}
	rendered, err = substituteNodeIP(rendered, cfg.NodeIP)
	if err != nil {
		return err
	}
	// The rendered config must carry the entry of our provider name, or
	// the chart and the installer disagree on the name.
	entry, err := renderedProvider(rendered, cfg.ProviderName)
	if err != nil {
		return err
	}

	mode := cfg.Mode
	if mode != modeNone {
		// Discovery, the edits of the shared files, the kubelet restart
		// and its verification of one install must not interleave with
		// another install's on this node (lock.go).
		unlock, err := lockFile(cfg.hostPath(nodeLockPath), cfg.lockTimeout)
		if err != nil {
			return err
		}
		defer unlock()
	}

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
		return runNone(cfg, rendered, entry)
	case modePatch:
		return runPatch(cfg, rendered, entry)
	case modeMerge:
		return runMerge(cfg, entry, wiring)
	default:
		return fmt.Errorf("unreachable mode %q", mode)
	}
}

// runNone drops the binary and config into the chart-owned dirs and
// leaves kubelet alone — the operator owns the flags.
func runNone(cfg *config, rendered []byte, entry map[string]any) error {
	configPath := cfg.ownConfigPath()
	unlock, err := lockFile(cfg.hostPath(configPath+lockSuffix), cfg.lockTimeout)
	if err != nil {
		return err
	}
	defer unlock()
	existing, desired, err := ownConfig(cfg, rendered, entry)
	if err != nil {
		return err
	}
	if err := installPluginBinary(cfg, cfg.HostBinDir, existing); err != nil {
		return err
	}
	changed, err := writeFileAtomic(cfg.hostPath(configPath), desired, 0o644)
	if err != nil {
		return err
	}
	logf("mode none: files installed (config changed: %v); kubelet wiring is the operator's responsibility", changed)
	return nil
}

// runPatch owns the config file and wires kubelet via a parse-merge of
// /etc/default/kubelet, restarting kubelet when restart-relevant
// content changed.
func runPatch(cfg *config, rendered []byte, entry map[string]any) error {
	configPath := cfg.ownConfigPath()
	unlock, err := lockFile(cfg.hostPath(configPath+lockSuffix), cfg.lockTimeout)
	if err != nil {
		return err
	}
	defer unlock()

	// Compute everything that can be refused BEFORE touching the host,
	// so a refusal never leaves a half-install behind.
	existingConfig, desiredConfig, err := ownConfig(cfg, rendered, entry)
	if err != nil {
		return err
	}
	existingEnv, err := readHostFile(cfg.hostPath(defaultKubeletPath))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", defaultKubeletPath, err)
	}
	desiredEnv, err := mergeExtraArgs(existingEnv, cfg.HostBinDir, configPath)
	if err != nil {
		return err
	}
	entryJSON, err := entryBytes(entry)
	if err != nil {
		return err
	}

	if err := installPluginBinary(cfg, cfg.HostBinDir, existingConfig); err != nil {
		return err
	}
	configChanged, err := writeFileAtomic(cfg.hostPath(configPath), desiredConfig, 0o644)
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
	t := target{
		mode: modePatch, binDir: cfg.HostBinDir, configFile: configPath,
		hash:       contentHash(entryJSON, desiredEnv),
		legacyHash: contentHash(desiredConfig, desiredEnv),
	}
	return finishWithRestart(cfg, t, configChanged || envChanged, verify)
}

// ownConfig reads the chart-owned provider config of patch and none mode
// and returns it (nil when absent) together with the content it must
// have after this install. The caller holds the config lock.
//
// While no other provider is in the file, that content is the rendered
// config verbatim, byte for byte what installers before ADR-0029 wrote.
// Once other installs share the file, only this install's entry is
// replaced or appended; theirs round-trip untouched.
func ownConfig(cfg *config, rendered []byte, entry map[string]any) (existing, desired []byte, err error) {
	path := cfg.ownConfigPath()
	existing, err = readHostFile(cfg.hostPath(path))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, rendered, nil
	case err != nil:
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	others, err := otherProviders(existing, cfg.ProviderName)
	if err != nil {
		// Kubelet cannot start with this file either. It is the chart's
		// own file and was always replaced; it still is.
		logf("replacing %s, which is not a credential-provider config to merge into: %v", path, err)
		return existing, rendered, nil
	}
	if others == 0 {
		return existing, rendered, nil
	}
	merged, _, err := mergeProvider(existing, entry)
	if err != nil {
		return nil, nil, fmt.Errorf("merge into %s: %w", path, err)
	}
	return existing, merged, nil
}

// runMerge injects our provider entry into the node's existing
// credential-provider config and drops the binary into the existing
// bin dir; kubelet's flags are not touched.
func runMerge(cfg *config, entry map[string]any, wiring kubeletWiring) error {
	unlock, err := lockFile(cfg.hostPath(wiring.ConfigFile+lockSuffix), cfg.lockTimeout)
	if err != nil {
		return err
	}
	defer unlock()

	// Read, validate, and merge first: an unknown schema or a missing file
	// is refused before anything is written (no half-install).
	existing, err := readHostFile(cfg.hostPath(wiring.ConfigFile))
	if err != nil {
		// Kubelet refuses to start when the flag points at a missing
		// file, so on a live node this indicates a wrong override.
		return fmt.Errorf("read node credential-provider config %s: %w", wiring.ConfigFile, err)
	}
	merged, mergeChanged, err := mergeProvider(existing, entry)
	if err != nil {
		return err
	}
	entryJSON, err := entryBytes(entry)
	if err != nil {
		return err
	}

	if err := installPluginBinary(cfg, wiring.BinDir, existing); err != nil {
		return err
	}
	written, err := writeFileAtomic(cfg.hostPath(wiring.ConfigFile), merged, 0o644)
	if err != nil {
		return err
	}
	if mergeChanged {
		logf("merged provider %q into %s", cfg.ProviderName, wiring.ConfigFile)
	}

	t := target{
		mode: modeMerge, binDir: wiring.BinDir, configFile: wiring.ConfigFile,
		hash:       contentHash(entryJSON),
		legacyHash: contentHash(merged),
	}
	return finishWithRestart(cfg, t, mergeChanged || written, nil)
}

// finishWithRestart applies the ADR-0021 restart policy: restart iff
// restart-relevant content changed on this pass OR the state file does
// not record a successful restart for exactly this content (covers the
// crash window between write and restart). The state is persisted only
// after the restart is VERIFIED (unit stably active and, when verify is
// set, the running kubelet wired as intended).
func finishWithRestart(cfg *config, t target, changedNow bool, verify func() error) error {
	stateDir := cfg.hostPath(cfg.StateDir)
	statePath := filepath.Join(stateDir, cfg.files().State)
	st, err := loadState(statePath)
	if err != nil {
		return err
	}
	if !changedNow {
		if st.matches(t) {
			logf("kubelet wiring is current; not restarting")
			return nil
		}
		if st.matchesLegacy(t) {
			// An installer before ADR-0029 restarted kubelet for exactly
			// the files as they are now: record that under the
			// per-install hash instead of restarting again.
			if err := saveState(statePath, t.state()); err != nil {
				return err
			}
			logf("kubelet wiring is current; converted the state file to the per-install hash, not restarting")
			return nil
		}
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
	if err := saveState(statePath, t.state()); err != nil {
		return err
	}
	logf("install complete (mode %s); kubelet verified healthy", t.mode)
	return nil
}

// installPluginBinary copies the plugin into binDir (a node path) under
// the provider name, which is the file kubelet runs for the entry.
// nodeConfig is the provider config kubelet reads, as it was before this
// pass (nil when absent).
func installPluginBinary(cfg *config, binDir string, nodeConfig []byte) error {
	dst := filepath.Join(binDir, cfg.files().Binary)
	if err := checkBinaryOwnership(cfg, dst, nodeConfig); err != nil {
		return err
	}
	changed, err := copyFile(cfg.SourcePlugin, cfg.hostPath(dst), 0o755)
	if err != nil {
		return err
	}
	if changed {
		logf("installed plugin binary → %s", dst)
	}
	return nil
}

// checkBinaryOwnership refuses to replace a file at dst (a node path) that
// is not this plugin. The operator picks a non-default provider name, and
// kubelet's bin dir is shared: GKE keeps kubelet itself in it. The file
// counts as ours when the config kubelet reads already holds a bridge
// entry of this name, or when it has exactly the bytes we would write (a
// pass interrupted before the config write). The default name is this
// project's own and always ours.
func checkBinaryOwnership(cfg *config, dst string, nodeConfig []byte) error {
	if cfg.ProviderName == defaultProviderName || hasBridgeProvider(nodeConfig, cfg.ProviderName) {
		return nil
	}
	current, err := readHostFile(cfg.hostPath(dst))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err == nil:
		ours, rerr := os.ReadFile(cfg.SourcePlugin)
		if rerr != nil {
			return fmt.Errorf("read %s: %w", cfg.SourcePlugin, rerr)
		}
		if bytes.Equal(current, ours) {
			return nil
		}
		err = errors.New("different content")
	}
	return fmt.Errorf("%s already exists and does not belong to this plugin (%w): plugin.providerName %q names a file of another program in kubelet's bin dir; choose another name, or delete the file if an interrupted install left it behind", dst, err, cfg.ProviderName)
}

// syncAuxFiles copies the CA (and mTLS client pair when enabled) into
// HostConfigDir. Shared by the install pass and the --sync loop.
func syncAuxFiles(cfg *config) error {
	files := cfg.files()
	caDst := filepath.Join(cfg.HostConfigDir, files.CA)
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
	certDst := filepath.Join(cfg.HostConfigDir, files.ClientCert)
	changed, err = copyFile(cfg.SourceClientCert, cfg.hostPath(certDst), 0o644)
	if err != nil {
		return err
	}
	if changed {
		logf("installed mTLS client cert → %s", certDst)
	}
	keyDst := filepath.Join(cfg.HostConfigDir, files.ClientKey)
	changed, err = copyFile(cfg.SourceClientKey, cfg.hostPath(keyDst), 0o600)
	if err != nil {
		return err
	}
	if changed {
		logf("installed mTLS client key → %s", keyDst)
	}
	return nil
}
