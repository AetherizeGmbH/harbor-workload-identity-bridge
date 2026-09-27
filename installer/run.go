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
	"sort"
	"strconv"
	"strings"
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
	switch {
	case errors.Is(err, fs.ErrNotExist) && cfg.ProviderName != defaultProviderName:
		return fmt.Errorf("read rendered credential-provider config: %w: for a plugin.providerName other than %s the chart publishes it at %s (ADR-0029); the chart and the plugin image (plugin.image.tag/digest) must both be a version with ADR-0029", err, defaultProviderName, cfg.SourceConfig)
	case err != nil:
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
	case modePatch:
		// Patch mode points kubelet at this install's directories; where
		// kubelet points now decides whether that is safe (checkRewire).
		wiring, err = discoverKubelet(cfg.ProcRoot)
		if err != nil {
			return fmt.Errorf("mode patch: %w", err)
		}
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

	if mode == modeMerge && dirsOverlap(wiring.BinDir, cfg.HostConfigDir) {
		// The binaries and records in the bin dir must be out of reach
		// of the writers of plugin.hostConfigDir (loadConfig).
		return fmt.Errorf("kubelet's credential-provider bin dir %s and plugin.hostConfigDir %s must not be the same directory or inside one another: every release's sync container can write plugin.hostConfigDir; choose another plugin.hostConfigDir", wiring.BinDir, cfg.HostConfigDir)
	}
	if mode == modePatch {
		// Held until the pass ends: no installer may add an entry to the
		// config kubelet reads now, or to this install's own config,
		// between checkRewire and the rewire.
		unlock, err := lockPatchConfigs(cfg, wiring)
		if err != nil {
			return err
		}
		defer unlock()
		if err := checkRewire(cfg, wiring); err != nil {
			return err
		}
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
	unlock, err := lockConfig(cfg, configPath+lockSuffix)
	if err != nil {
		return err
	}
	defer unlock()
	desired, err := ownConfig(cfg, rendered, entry)
	if err != nil {
		return err
	}
	entryJSON, err := entryBytes(entry)
	if err != nil {
		return err
	}
	if err := installFiles(cfg, cfg.HostBinDir, entryJSON); err != nil {
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
// content changed. The caller holds the node lock and the config locks
// (lockPatchConfigs).
func runPatch(cfg *config, rendered []byte, entry map[string]any) error {
	configPath := cfg.ownConfigPath()

	// Compute everything that can be refused BEFORE touching the host,
	// so a refusal never leaves a half-install behind.
	desiredConfig, err := ownConfig(cfg, rendered, entry)
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

	if err := installFiles(cfg, cfg.HostBinDir, entryJSON); err != nil {
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
		entryHash: contentHash(entryJSON, desiredEnv),
		fileHash:  contentHash(desiredConfig, desiredEnv),
	}
	return finishWithRestart(cfg, t, configChanged || envChanged, verify)
}

// lockPatchConfigs takes the config locks that a patch-mode pass holds
// from checkRewire to its end and returns the function that releases them:
// first the lock of the config kubelet reads now (current), when kubelet
// has one, then the lock of this install's own config, unless both are the
// same lock (current is this install's own config, or the directory
// plugin.hostConfigDir, and only the bin dir moves). The caller holds the
// node lock, which every auto, merge and patch installer takes for its
// whole pass; a none-mode installer takes only the config lock of the
// config it writes, and that config can be current or this install's own.
// Without these locks such an installer could add its entry after
// checkRewire read the config and before this pass points kubelet
// elsewhere. The order is node lock, current config lock, own config lock
// (lock.go).
func lockPatchConfigs(cfg *config, current kubeletWiring) (func(), error) {
	own := cfg.ownConfigPath() + lockSuffix
	var locks []string
	if current.wired() {
		if lock := cfg.configLockPath(current.ConfigFile); lock != own {
			locks = append(locks, lock)
		}
	}
	locks = append(locks, own)
	var unlocks []func()
	release := func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}
	for _, lock := range locks {
		unlock, err := lockConfig(cfg, lock)
		if err != nil {
			release()
			return nil, err
		}
		unlocks = append(unlocks, unlock)
	}
	return release, nil
}

// checkRewire refuses a patch-mode pass that would point kubelet away from
// the config it reads now (current) while that config holds another
// install's entry (ADR-0029). Kubelet has one config and one bin dir:
// moving the config drops that install's entry, and moving only the bin
// dir leaves kubelet without that install's binary, so that kubelet does
// not start. A single install that changed its own directories still
// moves. The caller holds the node lock and the config locks of current
// and of this install's own config (lockPatchConfigs) until the pass ends,
// so no installer adds an entry to either between this read and the
// rewire.
func checkRewire(cfg *config, current kubeletWiring) error {
	want := kubeletWiring{BinDir: cfg.HostBinDir, ConfigFile: cfg.ownConfigPath()}
	if !current.wired() || current == want {
		return nil
	}
	docs, err := configDocs(cfg.hostPath(current.ConfigFile))
	if err != nil {
		return fmt.Errorf("mode patch: cannot tell whether kubelet's credential-provider config %s holds other installs' entries, so it stays wired to it: %w", current.ConfigFile, err)
	}
	var others []string
	for _, doc := range docs {
		for _, name := range otherBridgeProviders(doc, cfg.ProviderName) {
			others = append(others, strconv.Quote(name))
		}
	}
	if len(others) == 0 {
		return nil
	}
	return fmt.Errorf("mode patch: kubelet runs with bin-dir=%q config=%q, which holds the provider entries %s of other harbor-bridge installs; moving kubelet to this install's bin-dir=%q config=%q would break them. Give every install the same plugin.hostBinaryDir and plugin.hostConfigDir, or use plugin.install.mode=auto",
		current.BinDir, current.ConfigFile, strings.Join(others, ", "), want.BinDir, want.ConfigFile)
}

// configDocs returns the credential-provider config documents kubelet reads
// from path (a host path): the file, or, since Kubernetes 1.34, the
// *.json, *.yaml and *.yml files of a directory, in kubelet's order. A
// missing path has none. Every file must pass openRegular: a symlink,
// which kubelet would follow, is an error.
func configDocs(path string) ([][]byte, error) {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, err
	case !fi.IsDir():
		doc, err := readHostFile(path)
		if err != nil {
			return nil, err
		}
		return [][]byte{doc}, nil
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	d, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	entries, err := d.ReadDir(-1)
	_ = d.Close()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		// Kubelet skips directories, and so does this.
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var docs [][]byte
	for _, name := range names {
		switch filepath.Ext(name) {
		case ".json", ".yaml", ".yml":
		default:
			continue
		}
		f, err := openRegular(root, name)
		if err != nil {
			return nil, err
		}
		doc, err := readAllCapped(f)
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

// ownConfig reads the chart-owned provider config of patch and none mode
// and returns the content it must have after this install. The caller
// holds the config lock.
//
// The file is the chart's: installers before ADR-0029 replaced it with
// the rendered config on every pass. It still gets exactly that while it
// holds no other install's entry. Other installs' entries (siblingEntry)
// stay in place; this install's entry is replaced whatever it holds;
// everything else is dropped, as it always was.
func ownConfig(cfg *config, rendered []byte, entry map[string]any) ([]byte, error) {
	path := cfg.ownConfigPath()
	existing, err := readHostFile(cfg.hostPath(path))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return rendered, nil
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	desired, dropped, err := composeOwnConfig(existing, rendered, entry, cfg.siblingEntry)
	if err != nil {
		// Kubelet cannot start with this file either. It is the chart's
		// own file and was always replaced; it still is.
		logf("replacing %s, which is not a credential-provider config to merge into: %v", path, err)
		return rendered, nil
	}
	for _, name := range dropped {
		logf("dropping provider %s from %s: not an entry of another harbor-bridge install on this node, or not as that install recorded it", name, path)
	}
	return desired, nil
}

// siblingEntry reports whether entry, found in the chart-owned config of
// patch and none mode, is another install's (ADR-0029) and stays there.
// That file is in plugin.hostConfigDir, which the sync container of every
// release and any pod with a hostPath on that directory can write, and
// kubelet runs whatever entry it holds after its next restart, which this
// installer may trigger itself. An entry therefore stays only when it is
// exactly what another installer wrote: a bridge entry with a valid
// provider name, whose binary is an executable regular file in
// plugin.hostBinaryDir and whose record there (entryRecord) holds the
// entry's canonical bytes. No writer of plugin.hostConfigDir can write
// that directory (loadConfig refuses overlapping directories), and every
// installer writes its record and its binary before its entry, so the
// entry of another install always passes, while a planted entry, or
// another install's entry changed in the file, never does.
func (c *config) siblingEntry(entry map[string]any) bool {
	name, ok := entry["name"].(string)
	if !ok || name == c.ProviderName || validProviderName(name) != nil || !isBridgeProvider(entry) {
		return false
	}
	files := filesFor(name)
	if !isExecutableHostFile(c.hostPath(filepath.Join(c.HostBinDir, files.Binary))) {
		return false
	}
	record, err := readHostFile(c.hostPath(filepath.Join(c.HostBinDir, files.Record)))
	if err != nil {
		return false
	}
	got, err := entryBytes(entry)
	return err == nil && bytes.Equal(got, record)
}

// runMerge injects our provider entry into the node's existing
// credential-provider config and drops the binary into the existing
// bin dir; kubelet's flags are not touched.
func runMerge(cfg *config, entry map[string]any, wiring kubeletWiring) error {
	// Kubelet 1.34+ also accepts a directory of config files. The
	// installer only edits a file; say so instead of failing on the read
	// below (and before a lock file lands next to the directory).
	if fi, err := os.Lstat(cfg.hostPath(wiring.ConfigFile)); err == nil && fi.IsDir() {
		return fmt.Errorf("kubelet's credential-provider config %s is a directory, which the installer does not merge into; use plugin.install.mode=none and put the entry into a file of that directory yourself, or plugin.enabled=false", wiring.ConfigFile)
	}
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

	if err := installFiles(cfg, wiring.BinDir, entryJSON); err != nil {
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
		entryHash: contentHash(entryJSON),
		fileHash:  contentHash(merged),
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
			// the files as they are now: add the entry hash to that
			// record instead of restarting again. AppliedHash stays what
			// it was, which is t.fileHash.
			if err := saveState(statePath, t.state()); err != nil {
				return err
			}
			logf("kubelet wiring is current; added the entry hash to the state file, not restarting")
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

// recordFileMode is the mode of an install's record (entryRecord): read
// and written only by the root installers, and never executable, so that
// kubelet cannot run it even under a planted entry of its name, and
// isExecutableHostFile never takes it for a plugin. Its content is also in
// the world-readable provider config, so it is not secret; no other reader
// needs it.
const recordFileMode = 0o600

// installFiles refuses a pass that would replace a file of another
// program in binDir (a node path), and otherwise writes this install's
// files that the provider entry depends on, before the caller writes the
// entry: the CA and mTLS files in HostConfigDir, then the record and the
// plugin binary in binDir. Everything that can refuse the pass (ownership,
// a foreign entry, an unreadable config) runs before this, so a refused
// pass writes nothing on the node.
func installFiles(cfg *config, binDir string, entryJSON []byte) error {
	if err := checkBinaryOwnership(cfg, binDir); err != nil {
		return err
	}
	// CA (+ optional mTLS client pair) always live under HostConfigDir:
	// the provider entry references them by absolute path, so they are
	// independent of which config file kubelet reads. Their rotation
	// never restarts kubelet — the plugin reads them on every exec.
	if err := syncAuxFiles(cfg); err != nil {
		return err
	}
	return installPluginBinary(cfg, binDir, entryJSON)
}

// installPluginBinary writes this install's record (entryRecord) and then
// copies the plugin into binDir (a node path) under the provider name,
// which is the file kubelet runs for the entry. The record comes first: a
// pass interrupted between the two leaves a binary that the next pass
// still recognises as its own (checkBinaryOwnership), whatever version it
// then installs.
//
// entryRecord: <binDir>/<name>.entry holds entryJSON, the canonical bytes
// of the entry this pass writes into kubelet's config. It lives next to
// the binary, where no writer of plugin.hostConfigDir can write, and is
// what siblingEntry compares another install's entry with and what
// checkBinaryOwnership takes as proof that <binDir>/<name> is this
// install's.
func installPluginBinary(cfg *config, binDir string, entryJSON []byte) error {
	files := cfg.files()
	record := filepath.Join(binDir, files.Record)
	if _, err := writeFileAtomic(cfg.hostPath(record), entryJSON, recordFileMode); err != nil {
		return err
	}
	dst := filepath.Join(binDir, files.Binary)
	changed, err := copyFile(cfg.SourcePlugin, cfg.hostPath(dst), 0o755)
	if err != nil {
		return err
	}
	if changed {
		logf("installed plugin binary → %s", dst)
	}
	return nil
}

// checkBinaryOwnership refuses to replace a file at <binDir>/<name> (a
// node path) that is not this plugin. The operator picks a non-default
// provider name, and kubelet's bin dir is shared: GKE keeps kubelet itself
// in it. The file counts as ours when this install's record exists next
// to it (installPluginBinary writes it first), or when it has exactly the
// bytes we would write. The default name is this project's own and always
// ours; installers before ADR-0029 wrote no record. A provider entry in
// the config does not count: in patch and none mode that config is in
// plugin.hostConfigDir, which pods other than installers can write.
func checkBinaryOwnership(cfg *config, binDir string) error {
	files := cfg.files()
	if cfg.ProviderName == defaultProviderName || isRegularHostFile(cfg.hostPath(filepath.Join(binDir, files.Record))) {
		return nil
	}
	dst := filepath.Join(binDir, files.Binary)
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
