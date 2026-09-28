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
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const configFileName = "credential-provider-config.yaml"

// defaultKubeletPath is the kubelet environment file of kubeadm's Debian
// packages (kubeletEnvFiles).
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
	if err := validateEntry(entry); err != nil {
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
	// movingOwn is set when auto mode found kubelet on the directories of
	// this install's own last patch-mode pass, which it moves to the
	// current ones.
	movingOwn := false
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
			if movingOwn, err = cfg.wiredByOwnPatch(wiring); err != nil {
				return fmt.Errorf("mode auto: %w", err)
			}
			if movingOwn {
				// The same, after plugin.hostBinaryDir or
				// plugin.hostConfigDir changed: the flags point at the
				// directories this install's last verified patch-mode
				// pass wired. Merging would keep kubelet there and ignore
				// the new values for good.
				mode = modePatch
				logf("kubelet runs with the directories of this install's last patch-mode pass (bin-dir=%q config=%q); moving it to bin-dir=%q config=%q. The files in the old directories stay", wiring.BinDir, wiring.ConfigFile, cfg.HostBinDir, cfg.ownConfigPath())
			} else {
				mode = modeMerge
			}
		}
		logf("mode auto resolved to %s", mode)
	case modePatch:
		// Patch mode points kubelet at this install's directories; where
		// kubelet points now decides whether that is safe (blockingInstalls).
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

	if mode == modePatch {
		// Held until the pass ends: no installer may add an entry to the
		// config kubelet reads now, or to this install's own config,
		// between blockingInstalls and the rewire.
		unlock, err := lockPatchConfigs(cfg, wiring)
		if err != nil {
			return err
		}
		defer unlock()
		others, err := cfg.blockingInstalls(wiring, rendered, entry)
		switch {
		case err != nil:
			return err
		case len(others) > 0 && movingOwn:
			// Other installs that have not moved yet share the old
			// directories (ADR-0035).
			return stayAndStage(cfg, rendered, entry, wiring, others)
		case len(others) > 0:
			return rewireRefusal(cfg, wiring, others)
		}
	}
	if mode == modeMerge && dirsOverlap(wiring.BinDir, cfg.HostConfigDir) {
		// The binaries and records in the bin dir must be out of reach
		// of the writers of plugin.hostConfigDir (loadConfig).
		return fmt.Errorf("kubelet's credential-provider bin dir %s and plugin.hostConfigDir %s must not be the same directory or inside one another: every release's sync container can write plugin.hostConfigDir; choose another plugin.hostConfigDir", wiring.BinDir, cfg.HostConfigDir)
	}

	switch mode {
	case modeNone:
		return runNone(cfg, rendered, entry)
	case modePatch:
		return runPatch(cfg, rendered, entry)
	case modeMerge:
		return runMerge(cfg, rendered, entry, wiring)
	default:
		return fmt.Errorf("unreachable mode %q", mode)
	}
}

// wiredByOwnPatch reports whether kubelet's wiring is what this install's
// last verified patch-mode pass set up, according to its state file: auto
// mode then moves kubelet to the current directories (audit H3, after a
// change of plugin.hostBinaryDir or plugin.hostConfigDir), or stays a
// patch install on them while other installs still use them (stayAndStage,
// ADR-0035), so that a later pass moves it. The flags alone cannot tell:
// an operator or another install may have wired them to the same kind of
// files through the same environment file, and patch mode would drop
// their entries. Without a state file (removed, or a
// plugin.install.stateDir that changed too) auto mode merges, as before.
func (c *config) wiredByOwnPatch(wiring kubeletWiring) (bool, error) {
	st, err := loadState(c.statePath())
	if err != nil {
		return false, err
	}
	return st != nil && st.Mode == modePatch && st.BinDir == wiring.BinDir && st.ConfigFile == wiring.ConfigFile, nil
}

// runNone drops the binary and config into the chart-owned dirs and
// leaves kubelet alone — the operator owns the flags.
func runNone(cfg *config, rendered []byte, entry map[string]any) error {
	configPath := cfg.ownConfigPath()
	dir, err := cfg.openConfigDir()
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	unlock, err := lockFileIn(dir, configFileName+lockSuffix, cfg.lockTimeout)
	if err != nil {
		return err
	}
	defer unlock()
	desired, _, err := ownConfig(cfg, dir, rendered, entry)
	if err != nil {
		return err
	}
	entryJSON, err := entryBytes(entry)
	if err != nil {
		return err
	}
	if _, err := installFiles(cfg, cfg.HostBinDir, entryJSON, configPath); err != nil {
		return err
	}
	changed, err := writeFileIn(dir, configFileName, desired, 0o644)
	if err != nil {
		return err
	}
	if err := cfg.commitRecord(cfg.HostBinDir, entryJSON, configPath); err != nil {
		return err
	}
	logf("mode none: files installed (config changed: %v); kubelet wiring is the operator's responsibility", changed)
	return nil
}

// runPatch owns the config file and wires kubelet via a parse-merge of
// the environment file its unit reads (kubeletEnvFile, ADR-0034), usually
// /etc/default/kubelet, restarting kubelet when restart-relevant
// content changed. The caller holds the node lock and the config locks
// (lockPatchConfigs).
func runPatch(cfg *config, rendered []byte, entry map[string]any) error {
	return runPatchOn(cfg, rendered, entry, cfg.ownWiring())
}

// ownWiring is the wiring patch mode gives kubelet: plugin.hostBinaryDir and
// the chart-owned config in plugin.hostConfigDir.
func (c *config) ownWiring() kubeletWiring {
	return kubeletWiring{BinDir: c.HostBinDir, ConfigFile: c.ownConfigPath()}
}

// runPatchOn is runPatch for the wiring own: this install's (ownWiring),
// or the one of its last patch-mode pass while auto mode cannot move it
// yet (stayAndStage). own.ConfigFile is a chart-owned config.
func runPatchOn(cfg *config, rendered []byte, entry map[string]any, own kubeletWiring) error {
	configPath := own.ConfigFile
	configName := filepath.Base(configPath)
	dir, err := openNodeDir(cfg.HostRoot, filepath.Dir(configPath))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()

	// Compute everything that can be refused BEFORE touching the host,
	// so a refusal never leaves a half-install behind.
	desiredConfig, priorConfig, err := chartOwnedConfigIn(cfg, dir, own, rendered, entry)
	if err != nil {
		return err
	}
	// The environment file the kubelet unit takes KUBELET_EXTRA_ARGS from
	// (ADR-0034).
	envFile, err := cfg.kubeletEnvFile()
	if err != nil {
		return fmt.Errorf("mode patch: %w", err)
	}
	envPath := cfg.hostPath(envFile)
	existingEnv, err := readHostFile(envPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", envFile, err)
	}
	priorEnv := priorContent{existed: err == nil, data: existingEnv}
	desiredEnv, err := mergeExtraArgs(existingEnv, own.BinDir, configPath)
	if err != nil {
		return fmt.Errorf("%s: %w", envFile, err)
	}
	entryJSON, err := entryBytes(entry)
	if err != nil {
		return err
	}
	t := target{
		mode: modePatch, binDir: own.BinDir, configFile: configPath, envFile: envFile,
		entryHash: contentHash(entryJSON, desiredEnv),
		fileHash:  contentHash(desiredConfig, desiredEnv),
	}
	if err := cfg.refuseRejected(t); err != nil {
		return err
	}

	begun, err := installFiles(cfg, own.BinDir, entryJSON, configPath)
	if err != nil {
		return err
	}
	configChanged, err := writeFileIn(dir, configName, desiredConfig, 0o644)
	if err != nil {
		return err
	}
	if err := cfg.commitRecord(own.BinDir, entryJSON, configPath); err != nil {
		return err
	}
	envChanged, err := writeFileAtomic(envPath, desiredEnv, 0o644)
	if err != nil {
		return err
	}
	// What kubelet reads at startup, as this pass found it (ADR-0033). The
	// record first: while the config is restored it holds both entries.
	var rb rollback
	rb.step(func() error { return cfg.writeRecord(own.BinDir, *begun) })
	rb.file(configPath, func() error { return restoreIn(dir, configName, priorConfig, 0o644) })
	rb.file(envFile, func() error { return restoreHostFile(envPath, priorEnv, 0o644) })

	want := own
	verify := func() error {
		// The flags only reach kubelet if its unit passes
		// $KUBELET_EXTRA_ARGS on; confirm on the live process.
		got, err := discoverKubelet(cfg.ProcRoot)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("running kubelet has bin-dir=%q config=%q, want %q/%q — does the %s unit pass $KUBELET_EXTRA_ARGS from %s on to kubelet?",
				got.BinDir, got.ConfigFile, want.BinDir, want.ConfigFile, cfg.KubeletUnit, envFile)
		}
		return nil
	}
	return finishWithRestart(cfg, t, configChanged || envChanged, verify, &rb)
}

// lockPatchConfigs takes the config locks that a patch-mode pass holds
// from blockingInstalls to its end and returns the function that releases
// them: first the lock of the config kubelet reads now (current), when kubelet
// has one, then the lock of this install's own config, unless both are the
// same lock (current is this install's own config, or the directory
// plugin.hostConfigDir, and only the bin dir moves). The caller holds the
// node lock, which every auto, merge and patch installer takes for its
// whole pass; a none-mode installer takes only the config lock of the
// config it writes, and that config can be current or this install's own.
// Without these locks such an installer could add its entry after
// blockingInstalls read the config and before this pass points kubelet
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

// blockingInstalls returns the provider names of the other installs that
// keep a patch-mode pass from pointing kubelet away from the config it
// reads now (current), ADR-0029: kubelet has one config and one bin dir,
// so moving the config drops another install's entry, and moving only the
// bin dir leaves kubelet without its binary, so that kubelet does not
// start. An install blocks when current holds an entry of it that counts
// (what counts depends on who can write current, rewireCounts) and the
// config the pass moves kubelet to does not keep an entry of it: that
// config, as the pass writes it (ownConfig), keeps another install's entry
// only next to that install's binary and record in plugin.hostBinaryDir
// (siblingIn), where auto mode stages them (stageOwnFiles, ADR-0035). A
// single install that changed its own directories still moves. The caller
// holds the node lock and the config locks of current and of this
// install's own config (lockPatchConfigs) until the pass ends, so no
// installer adds an entry to either between this read and the rewire.
func (c *config) blockingInstalls(current kubeletWiring, rendered []byte, entry map[string]any) ([]string, error) {
	if !current.wired() || current == c.ownWiring() {
		return nil, nil
	}
	docs, err := configDocs(c.hostPath(current.ConfigFile))
	var counts func(map[string]any) bool
	if err == nil {
		counts, err = c.rewireCounts(current)
	}
	var others []string
	for _, doc := range docs {
		others = append(others, otherBridgeProviders(doc, c.ProviderName, counts)...)
	}
	var kept map[string]bool
	if err == nil && len(others) > 0 {
		kept, err = c.keptSiblings(rendered, entry)
	}
	if err != nil {
		return nil, fmt.Errorf("mode patch: cannot tell whether kubelet's credential-provider config %s holds other installs' entries, so it stays wired to it: %w", current.ConfigFile, err)
	}
	return slices.DeleteFunc(others, func(name string) bool { return kept[name] }), nil
}

// keptSiblings returns the provider names of the other installs' entries in
// this install's own config as a patch-mode pass writes it (ownConfig).
func (c *config) keptSiblings(rendered []byte, entry map[string]any) (map[string]bool, error) {
	dir, err := c.openConfigDir()
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	desired, _, err := ownConfig(c, dir, rendered, entry)
	if err != nil {
		return nil, err
	}
	kept := map[string]bool{}
	for _, name := range otherBridgeProviders(desired, c.ProviderName, nil) {
		kept[name] = true
	}
	return kept, nil
}

// rewireRefusal is the error of a patch-mode pass that others, other
// installs' provider names (blockingInstalls), keep from moving kubelet
// away from current.
func rewireRefusal(cfg *config, current kubeletWiring, others []string) error {
	want := cfg.ownWiring()
	return fmt.Errorf("mode patch: kubelet runs with bin-dir=%q config=%q, which holds the provider entries %s of other harbor-bridge installs; moving kubelet to this install's bin-dir=%q config=%q would break them, because they do not have their binaries and records there, and their entries in that config, yet. Give every install the same plugin.hostBinaryDir and plugin.hostConfigDir and use plugin.install.mode=auto, which moves kubelet once every install has its files in the new directories (ADR-0035)",
		current.BinDir, current.ConfigFile, quoteAll(others), want.BinDir, want.ConfigFile)
}

func quoteAll(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = strconv.Quote(n)
	}
	return strings.Join(quoted, ", ")
}

// stayAndStage is auto mode's pass when other installs (others,
// blockingInstalls) keep it from moving its own install away from old, the
// wiring of its last verified patch-mode pass (ADR-0035). It stays a patch
// install on old, so that its state keeps recording old and the next pass
// tries the move again (wiredByOwnPatch), and then stages its files in the
// current directories (stageOwnFiles): the pass of the last install that
// moves finds them there and takes this install along. The caller holds the
// node lock and the config locks of old and of this install's own config
// (lockPatchConfigs).
func stayAndStage(cfg *config, rendered []byte, entry map[string]any, old kubeletWiring, others []string) error {
	// The checks of a merge pass into a chart-owned config (runMerge): the
	// records in the old bin dir decide which entries stay in the old
	// config, and its writers must not reach them.
	switch {
	case dirsOverlap(old.BinDir, cfg.HostConfigDir):
		return fmt.Errorf("kubelet's credential-provider bin dir %s and plugin.hostConfigDir %s must not be the same directory or inside one another: every release's sync container can write plugin.hostConfigDir; choose another plugin.hostConfigDir", old.BinDir, cfg.HostConfigDir)
	case withinDir(old.ConfigFile, cfg.HostConfigDir) && old.ConfigFile != cfg.ownConfigPath():
		return fmt.Errorf("kubelet's credential-provider config %s is inside plugin.hostConfigDir %s, which every release's sync container can write, and is not the chart-owned config there (%s); choose another plugin.hostConfigDir", old.ConfigFile, cfg.HostConfigDir, cfg.ownConfigPath())
	}
	want := cfg.ownWiring()
	logf("not moving kubelet to bin-dir=%q config=%q yet: its config %s holds the provider entries %s of other installs, which do not have their files in the new directories yet. Kubelet stays on the directories of this install's last patch-mode pass; this install's files go into the new directories too, and kubelet moves in the pass of the last of these installs that has the new directories (ADR-0035)",
		want.BinDir, want.ConfigFile, old.ConfigFile, quoteAll(others))
	if err := runPatchOn(cfg, rendered, entry, old); err != nil {
		return err
	}
	return stageOwnFiles(cfg, rendered, entry, old)
}

// stageOwnFiles writes this install's files into its current directories
// while kubelet stays on old (stayAndStage): its binary and record into
// plugin.hostBinaryDir and its entry into the chart-owned config in
// plugin.hostConfigDir, as none mode does, leaving out what the pass on old
// wrote already (a directory that did not change). The record comes before
// the entry, as always (entryRecord). The caller holds the config lock of
// this install's own config.
func stageOwnFiles(cfg *config, rendered []byte, entry map[string]any, old kubeletWiring) error {
	want := cfg.ownWiring()
	entryJSON, err := entryBytes(entry)
	if err != nil {
		return err
	}
	newBinDir := want.BinDir != old.BinDir
	if newBinDir {
		if _, err := installFiles(cfg, want.BinDir, entryJSON, want.ConfigFile); err != nil {
			return err
		}
	}
	if want.ConfigFile != old.ConfigFile {
		dir, err := cfg.openConfigDir()
		if err != nil {
			return err
		}
		defer func() { _ = dir.Close() }()
		desired, _, err := ownConfig(cfg, dir, rendered, entry)
		if err != nil {
			return err
		}
		if _, err := writeFileIn(dir, configFileName, desired, 0o644); err != nil {
			return err
		}
	}
	if newBinDir {
		if err := cfg.commitRecord(want.BinDir, entryJSON, want.ConfigFile); err != nil {
			return err
		}
	}
	logf("staged this install's files in bin-dir=%q config=%q; kubelet does not read them yet", want.BinDir, want.ConfigFile)
	return nil
}

// rewireCounts returns what blockingInstalls takes for another install's entry
// in kubelet's current config (current). A config that writers of a
// plugin.hostConfigDir reach holds whatever they planted there: a file or
// directory inside this install's plugin.hostConfigDir, or a chart-owned
// config that a record in kubelet's bin dir names (claimsChartOwned; for a
// directory of config files, the chart-owned config of a none-mode install
// whose plugin.hostConfigDir it is). There only the entries another
// installer wrote count (siblingIn): a planted entry must neither keep
// kubelet on that config, where it runs at every kubelet start, nor keep
// this install from moving kubelet to its own config, where this installer
// drops it. Any other config (a cloud's, or one edited by hand) is not the
// chart's, and every bridge entry in it counts (nil).
func (c *config) rewireCounts(current kubeletWiring) (func(map[string]any) bool, error) {
	if withinDir(current.ConfigFile, c.HostConfigDir) {
		return c.siblingIn(current.BinDir), nil
	}
	claimed, err := c.claimsChartOwned(current.BinDir, c.chartOwnedPathIn(current.ConfigFile))
	if err != nil || !claimed {
		return nil, err
	}
	return c.siblingIn(current.BinDir), nil
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
// from dir, plugin.hostConfigDir (openConfigDir), and returns the content it
// must have after this install (chartOwnedConfig) and the content it has
// now. The caller holds the config lock.
func ownConfig(cfg *config, dir *os.Root, rendered []byte, entry map[string]any) ([]byte, priorContent, error) {
	return chartOwnedConfigIn(cfg, dir, cfg.ownWiring(), rendered, entry)
}

// chartOwnedConfigIn is ownConfig for the chart-owned config own.ConfigFile
// in dir, whose binaries kubelet runs from own.BinDir.
func chartOwnedConfigIn(cfg *config, dir *os.Root, own kubeletWiring, rendered []byte, entry map[string]any) ([]byte, priorContent, error) {
	path := own.ConfigFile
	prior, err := readPrior(dir, filepath.Base(path))
	switch {
	case err != nil:
		return nil, priorContent{}, fmt.Errorf("read %s: %w", path, err)
	case !prior.existed:
		return rendered, prior, nil
	}
	return chartOwnedConfig(path, prior.data, rendered, entry, cfg.siblingIn(own.BinDir)), prior, nil
}

// chartOwnedConfig returns the content a chart-owned provider config at
// path (a node path) must have after this install, from its content
// existing. sibling tells another install's entry (siblingIn).
//
// The file is the chart's: installers before ADR-0029 replaced it with
// the rendered config on every pass. It still gets exactly that while it
// holds no other install's entry. Other installs' entries stay in place;
// this install's entry is replaced whatever it holds; everything else is
// dropped, as it always was.
func chartOwnedConfig(path string, existing, rendered []byte, entry map[string]any, sibling func(map[string]any) bool) []byte {
	desired, dropped, err := composeOwnConfig(existing, rendered, entry, sibling)
	if err != nil {
		// Kubelet cannot start with this file either. It is the chart's
		// own file and was always replaced; it still is.
		logf("replacing %s, which is not a credential-provider config to merge into: %v", path, err)
		return rendered
	}
	for _, name := range dropped {
		logf("dropping provider %s from %s: not an entry of another harbor-bridge install on this node, or not as that install recorded it", name, path)
	}
	return desired
}

// siblingIn returns the function that reports whether entry, found in a
// chart-owned config whose binaries kubelet runs from binDir (a node
// path), is another install's (ADR-0029) and stays there. That file is in
// a plugin.hostConfigDir, which the sync container of every release and
// any pod with a hostPath on that directory can write, and kubelet runs
// whatever entry it holds after its next restart, which this installer may
// trigger itself. An entry therefore stays only when it is exactly what
// another installer wrote: a bridge entry with a valid provider name,
// whose binary is an executable regular file in binDir and whose record
// there (entryRecord) holds the entry's canonical bytes. binDir is
// plugin.hostBinaryDir in patch and none mode and kubelet's bin dir in
// merge mode; no writer of plugin.hostConfigDir can write it (loadConfig
// and run refuse overlapping directories). Every installer adds its entry
// to its record and writes its binary before it writes the entry, and
// drops an earlier entry from the record only after the config holds the
// new one, so the entry another install has in the file passes, also
// after that install's pass died halfway, while a planted entry, or
// another install's entry changed in the file into anything that install
// did not write, never does.
func (c *config) siblingIn(binDir string) func(entry map[string]any) bool {
	return func(entry map[string]any) bool {
		name, ok := entry["name"].(string)
		if !ok || name == c.ProviderName || validProviderName(name) != nil || !isBridgeProvider(entry) {
			return false
		}
		files := filesFor(name)
		if !isExecutableHostFile(c.hostPath(filepath.Join(binDir, files.Binary))) {
			return false
		}
		got, err := entryBytes(entry)
		return err == nil && c.readRecord(filepath.Join(binDir, files.Record)).holds(got)
	}
}

// runMerge injects our provider entry into the node's existing
// credential-provider config and drops the binary into the existing
// bin dir; kubelet's flags are not touched.
//
// Kubelet's config is usually a cloud's, and every other entry in it stays
// as it is. It can also be the chart-owned config of a patch- or none-mode
// install (ADR-0029): this install's own, when only kubelet's bin dir
// differs from this install's, or another release's, whose directories
// differ. That file is in a plugin.hostConfigDir, which pods other than
// installers can write, and this pass may restart kubelet onto it; it then
// keeps only the entries the records in kubelet's bin dir vouch for, as
// patch and none mode do (chartOwnedConfig). Every patch- and none-mode
// installer names its chart-owned config in its record
// (entryRecord.ChartOwnedConfig).
func runMerge(cfg *config, rendered []byte, entry map[string]any, wiring kubeletWiring) error {
	// Kubelet 1.34+ also accepts a directory of config files. The
	// installer only edits a file; say so instead of failing on the read
	// below (and before a lock file lands next to the directory).
	if fi, err := os.Lstat(cfg.hostPath(wiring.ConfigFile)); err == nil && fi.IsDir() {
		return fmt.Errorf("kubelet's credential-provider config %s is a directory, which the installer does not merge into; point kubelet's --image-credential-provider-config at a file, or put this install's entry into a file of that directory by other means and set plugin.enabled=false. Never make that directory plugin.hostConfigDir: every release's sync container can write plugin.hostConfigDir, and kubelet loads every config file of the directory at every start", wiring.ConfigFile)
	}
	own := cfg.ownConfigPath()
	if withinDir(wiring.ConfigFile, cfg.HostConfigDir) && wiring.ConfigFile != own {
		return fmt.Errorf("kubelet's credential-provider config %s is inside plugin.hostConfigDir %s, which every release's sync container can write, and is not the chart-owned config there (%s); keep kubelet's config out of plugin.hostConfigDir, or choose another plugin.hostConfigDir", wiring.ConfigFile, cfg.HostConfigDir, own)
	}
	dir, err := cfg.openDirOf(wiring.ConfigFile, false)
	if err != nil {
		return fmt.Errorf("open the directory of node credential-provider config %s: %w", wiring.ConfigFile, err)
	}
	defer func() { _ = dir.Close() }()
	name := filepath.Base(wiring.ConfigFile)
	unlock, err := lockFileIn(dir, name+lockSuffix, cfg.lockTimeout)
	if err != nil {
		return err
	}
	defer unlock()

	// Read, validate, and merge first: an unknown schema or a missing file
	// is refused before anything is written (no half-install).
	existing, err := readFileIn(dir, name)
	if err != nil {
		// Kubelet refuses to start when the flag points at a missing
		// file, so on a live node this indicates a wrong override.
		return fmt.Errorf("read node credential-provider config %s: %w", wiring.ConfigFile, err)
	}
	chartOwned := wiring.ConfigFile == own
	if !chartOwned {
		// Under the config's lock: a none-mode install writes its record
		// under the lock of its chart-owned config.
		if chartOwned, err = cfg.claimsChartOwned(wiring.BinDir, wiring.ConfigFile); err != nil {
			return err
		}
	}
	var merged []byte
	var mergeChanged bool
	claim := ""
	if chartOwned {
		claim = wiring.ConfigFile
		logf("kubelet's credential-provider config %s is a chart-owned config: keeping only the entries the records in %s vouch for", wiring.ConfigFile, wiring.BinDir)
		merged = chartOwnedConfig(wiring.ConfigFile, existing, rendered, entry, cfg.siblingIn(wiring.BinDir))
		mergeChanged = !bytes.Equal(merged, existing)
	} else {
		if merged, mergeChanged, err = mergeProvider(existing, entry); err != nil {
			return err
		}
		if err := cfg.checkKubeletCanStart(merged, wiring); err != nil {
			return err
		}
	}
	entryJSON, err := entryBytes(entry)
	if err != nil {
		return err
	}
	t := target{
		mode: modeMerge, binDir: wiring.BinDir, configFile: wiring.ConfigFile,
		entryHash: contentHash(entryJSON),
		fileHash:  contentHash(merged),
	}
	if err := cfg.refuseRejected(t); err != nil {
		return err
	}

	begun, err := installFiles(cfg, wiring.BinDir, entryJSON, claim)
	if err != nil {
		return err
	}
	written, err := writeFileIn(dir, name, merged, 0o644)
	if err != nil {
		return err
	}
	if err := cfg.commitRecord(wiring.BinDir, entryJSON, claim); err != nil {
		return err
	}
	if mergeChanged {
		logf("merged provider %q into %s", cfg.ProviderName, wiring.ConfigFile)
	}
	// Kubelet's config as this pass found it (ADR-0033); the record first.
	var rb rollback
	rb.step(func() error { return cfg.writeRecord(wiring.BinDir, *begun) })
	rb.file(wiring.ConfigFile, func() error {
		return restoreIn(dir, name, priorContent{existed: true, data: existing}, 0o644)
	})
	return finishWithRestart(cfg, t, mergeChanged || written, nil, &rb)
}

// checkKubeletCanStart refuses the merged config of a merge pass into a
// cloud's config when kubelet would not start with it
// (kubeletStartProblems), before this pass writes it or restarts kubelet
// onto it: kubelet exits at startup on such a config (ADR-0029, Context).
// The other entries are the cloud's and other installs', which this
// installer does not remove. A binary counts when it is an executable
// regular file in kubelet's bin dir, or a symlink, which kubelet follows
// on the node and this installer cannot resolve from its mount.
func (c *config) checkKubeletCanStart(merged []byte, wiring kubeletWiring) error {
	root, err := os.OpenRoot(c.hostPath(wiring.BinDir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("open kubelet's credential-provider bin dir %s: %w", wiring.BinDir, err)
	}
	if root != nil {
		defer func() { _ = root.Close() }()
	}
	hasBinary := func(name string) bool {
		if root == nil {
			return false
		}
		fi, err := root.Lstat(name)
		if err != nil {
			return false
		}
		return fi.Mode()&fs.ModeSymlink != 0 || (fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0)
	}
	problems, err := kubeletStartProblems(merged, c.ProviderName, hasBinary)
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		return fmt.Errorf("kubelet would not start with the credential-provider config %s (bin dir %s) after this pass: %s; refusing to write it or restart kubelet. Fix the config or the bin dir on the node", wiring.ConfigFile, wiring.BinDir, strings.Join(problems, "; "))
	}
	return nil
}

// finishWithRestart applies the ADR-0021 restart policy: restart iff
// restart-relevant content changed on this pass OR the state file does
// not record a successful restart for exactly this content (covers the
// crash window between write and restart). The state is persisted only
// after the restart is VERIFIED (unit stably active and, when verify is
// set, the running kubelet wired as intended). A restart that fails or
// does not verify is rolled back (rb) and its content recorded as rejected
// (rejectRestart, ADR-0033).
func finishWithRestart(cfg *config, t target, changedNow bool, verify func() error, rb *rollback) error {
	stateDir := cfg.hostPath(cfg.StateDir)
	statePath := cfg.statePath()
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
	// single re-roll. Kubelet keeps running what it read at its last
	// start; the files go back to that too, so the node is as the pass
	// found it.
	if err := ensureWritableDir(stateDir); err != nil {
		err = fmt.Errorf("state dir %s: %w", cfg.StateDir, err)
		if changedNow {
			if rerr := rb.restore(); rerr != nil {
				return fmt.Errorf("%w; restoring %s failed: %w", err, rb.describe(), rerr)
			}
			logf("restored %s: kubelet was not restarted onto this pass's content", rb.describe())
		}
		return err
	}
	logf("restarting kubelet unit %q (config or flags changed)", cfg.KubeletUnit)
	err = cfg.kubelet.restart(cfg.KubeletUnit)
	if err == nil {
		err = waitKubeletHealthy(cfg.kubelet, cfg.KubeletUnit, cfg.verify, verify)
	}
	if err != nil {
		return cfg.rejectRestart(statePath, st, t, changedNow, rb, err)
	}
	if err := saveState(statePath, t.state()); err != nil {
		return err
	}
	logf("install complete (mode %s); kubelet verified healthy", t.mode)
	return nil
}

// rejectRestart handles a kubelet restart onto t that failed or did not
// verify (cause), ADR-0033. When this pass changed files, it restores them
// as the pass found them (rb), restarts kubelet onto them and verifies that
// it stays up. A pass that changed nothing (the crash window: an earlier
// pass wrote the files and did not verify its restart) has nothing of its
// own to restore. Either way it records t as rejected in the state file
// (st, the record it loaded), so that no later pass restarts kubelet onto
// the same content again (refuseRejected), and returns the error the pass
// fails with.
func (c *config) rejectRestart(statePath string, st *state, t target, changedNow bool, rb *rollback, cause error) error {
	var msg string
	if !changedNow {
		msg = fmt.Sprintf("%v; this pass did not change %s: an earlier pass wrote them and did not verify its kubelet restart, so this pass has no earlier content to restore. The .bak copies next to them hold what that pass replaced; check `journalctl -u %s` on the node", cause, rb.describe(), c.KubeletUnit)
	} else {
		rerr := rb.restore()
		if rerr != nil {
			logf("restoring %s failed: %v", rb.describe(), rerr)
		}
		logf("restarting kubelet unit %q onto the restored %s", c.KubeletUnit, rb.describe())
		kerr := c.kubelet.restart(c.KubeletUnit)
		if kerr == nil {
			kerr = waitKubeletHealthy(c.kubelet, c.KubeletUnit, c.verify, nil)
		}
		switch {
		case rerr == nil && kerr == nil:
			msg = fmt.Sprintf("%v; restored the previous %s and restarted kubelet, which runs on them again", cause, rb.describe())
		case kerr == nil:
			msg = fmt.Sprintf("%v; restoring the previous %s failed (%v); kubelet runs again after one more restart", cause, rb.describe(), rerr)
		case rerr == nil:
			msg = fmt.Sprintf("%v; restored the previous %s, but the kubelet restart onto them did not verify either: %v. Check `journalctl -u %s` on the node", cause, rb.describe(), kerr, c.KubeletUnit)
		default:
			msg = fmt.Sprintf("%v; restoring the previous %s failed (%v), and the kubelet restart after it did not verify either: %v. Check `journalctl -u %s` on the node", cause, rb.describe(), rerr, kerr, c.KubeletUnit)
		}
	}
	rec := &state{}
	if st != nil {
		kept := *st
		rec = &kept
	}
	// After the rollback kubelet runs again, from the binary that rejected
	// the content (kubeletIdentity).
	rec.Rejected = t.rejection(c.KubeletUnit, c.kubeletIdentity(), msg, time.Now())
	stateFile := filepath.Join(c.StateDir, c.files().State)
	if err := saveState(statePath, rec); err != nil {
		return fmt.Errorf("%s. Recording the rejected content in %s failed too (%w), so the next pass restarts kubelet onto it again", msg, stateFile, err)
	}
	return fmt.Errorf("%s. The content is recorded as rejected in %s; later passes with the same content restart nothing (ADR-0033)", msg, stateFile)
}

// refuseRejected refuses a pass whose content t an earlier pass restarted
// kubelet onto without a verified result (state.Rejected, ADR-0033). It
// runs before the pass writes anything, so the refusal changes nothing on
// the node and restarts nothing.
func (c *config) refuseRejected(t target) error {
	st, err := loadState(c.statePath())
	if err != nil {
		return err
	}
	r := st.rejects(t, c.KubeletUnit)
	if r == nil {
		return nil
	}
	// Another kubelet did not test this content: a kubelet upgrade, or a
	// feature gate turned on, can make kubelet accept what it rejected. The
	// pass tries once; if kubelet rejects it again, that kubelet is recorded.
	if r.Kubelet != "" {
		if now := c.kubeletIdentity(); !r.testedBy(now) {
			logf("the content kubelet rejected at %s is tried again: kubelet's binary, command line or --config file changed since (ADR-0033)", r.At)
			return nil
		}
	}
	return fmt.Errorf("refusing to restart kubelet onto content it already rejected: the pass at %s that restarted kubelet unit %q onto exactly this content failed: %s. This pass changes nothing on the node. Fix the cause and roll out new content (a changed plugin.* value renders a new provider entry), change kubelet (another kubelet binary, command line or --config file tries the same content once more), or fix the node and delete %s on it to try the same content again (ADR-0033)",
		r.At, r.Unit, r.Reason, filepath.Join(c.StateDir, c.files().State))
}

// statePath is the host path of this install's state file.
func (c *config) statePath() string {
	return filepath.Join(c.hostPath(c.StateDir), c.files().State)
}

// rollback restores what a pass changed that decides how kubelet starts:
// the provider config kubelet reads, the kubelet environment file in patch mode,
// and the install's record (ADR-0033). Its steps run in the order they were
// added, every one even when an earlier one failed.
type rollback struct {
	files []string // node paths of the files it restores, for messages
	steps []func() error
}

// step adds a restore step that is not one of the files the messages name.
func (r *rollback) step(restore func() error) {
	r.steps = append(r.steps, restore)
}

// file adds the restore step of the node file nodePath.
func (r *rollback) file(nodePath string, restore func() error) {
	r.files = append(r.files, nodePath)
	r.steps = append(r.steps, restore)
}

func (r *rollback) restore() error {
	var errs []error
	for _, restore := range r.steps {
		if err := restore(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *rollback) describe() string {
	return strings.Join(r.files, " and ")
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
// entry into the config chartOwned (empty for a cloud's config): the CA
// and mTLS files in HostConfigDir, then the record and the plugin binary
// in binDir. Everything that can refuse the pass (ownership, a foreign
// entry, an unreadable config) runs before this, so a refused pass changes
// no config, binary, record, CA, mTLS or state file; only the lock files
// it took (lock.go) and their directories may be new.
func installFiles(cfg *config, binDir string, entryJSON []byte, chartOwned string) (*entryRecord, error) {
	if err := checkBinaryOwnership(cfg, binDir); err != nil {
		return nil, err
	}
	// CA (+ optional mTLS client pair) always live under HostConfigDir:
	// the provider entry references them by absolute path, so they are
	// independent of which config file kubelet reads. Their rotation
	// never restarts kubelet — the plugin reads them on every exec.
	if err := syncAuxFiles(cfg); err != nil {
		return nil, err
	}
	return installPluginBinary(cfg, binDir, entryJSON, chartOwned)
}

// installPluginBinary adds entryJSON, the canonical bytes of the entry this
// pass writes into kubelet's config, to this install's record
// (entryRecord, beginRecord) and then copies the plugin into binDir (a
// node path) under the provider name, which is the file kubelet runs for
// the entry. The record comes first: a pass interrupted between the two
// leaves a binary that the next pass still recognises as its own
// (checkBinaryOwnership), whatever version it then installs. The caller
// reduces the record to entryJSON once the config holds it
// (commitRecord). It returns the record beginRecord wrote.
func installPluginBinary(cfg *config, binDir string, entryJSON []byte, chartOwned string) (*entryRecord, error) {
	files := cfg.files()
	begun, err := cfg.beginRecord(binDir, entryJSON, chartOwned)
	if err != nil {
		return nil, err
	}
	dst := filepath.Join(binDir, files.Binary)
	changed, err := copyFile(cfg.SourcePlugin, cfg.hostPath(dst), 0o755)
	if err != nil {
		return nil, err
	}
	if changed {
		logf("installed plugin binary → %s", dst)
	}
	return begun, nil
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
	dir, err := cfg.openConfigDir()
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	caDst := filepath.Join(cfg.HostConfigDir, files.CA)
	changed, err := copyFileIn(cfg.SourceCA, dir, files.CA, 0o644)
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
	changed, err = copyFileIn(cfg.SourceClientCert, dir, files.ClientCert, 0o644)
	if err != nil {
		return err
	}
	if changed {
		logf("installed mTLS client cert → %s", certDst)
	}
	keyDst := filepath.Join(cfg.HostConfigDir, files.ClientKey)
	changed, err = copyFileIn(cfg.SourceClientKey, dir, files.ClientKey, 0o600)
	if err != nil {
		return err
	}
	if changed {
		logf("installed mTLS client key → %s", keyDst)
	}
	return nil
}
