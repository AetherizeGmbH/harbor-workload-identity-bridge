// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// A removal pass (INSTALL_ACTION=remove, ADR-0039) takes this install's
// provider entry and files off the node and leaves everything else as it
// is: other installs' entries, a cloud's providers and the kubelet flags
// another install or the cloud set. The rules, in short:
//
//   - every check runs before the first write, and a refused pass leaves
//     the node as it found it, with kubelet running;
//   - the same locks as an install pass; no lock file is ever removed;
//   - first the config kubelet reads at its next start loses this
//     install's entry (or, for the last patch install on its directories,
//     the kubelet flags go), then kubelet restarts and the restart is
//     verified and rolled back as in ADR-0033, and only then go the
//     entries in configs kubelet does not read, the binary, the record,
//     the CA and mTLS files, their .bak and .tmp-* copies, and the state
//     file last;
//   - kubelet never gets a config without providers;
//   - none mode cannot see kubelet's flags and keeps the binary.

// Removal results (removalReport.Result).
const (
	resultRemoved = "removed"
	resultNothing = "nothing"
	resultKept    = "kept"
	resultFailed  = "failed"
)

// removalReport is what a removal pass reports in its termination message
// (ADR-0039, decision 2.9), which the uninstall hook reads from the pod's
// status. Its JSON form is pinned by testdata/removal-report.golden.json,
// which the hook's tests read too.
type removalReport struct {
	Action           string   `json:"action"`
	Provider         string   `json:"provider"`
	Result           string   `json:"result"`
	KubeletRestarted bool     `json:"kubeletRestarted"`
	Kept             []string `json:"kept,omitempty"`
	Error            string   `json:"error,omitempty"`
}

// maxReportBytes keeps the report under kubelet's limit for a termination
// message (4096 bytes): a cut-off report would not parse.
const maxReportBytes = 4000

// encode returns the report as one JSON line of at most maxReportBytes,
// shortening the error and dropping kept entries when it is longer.
func (r removalReport) encode() []byte {
	for {
		raw, err := json.Marshal(r)
		if err != nil {
			return []byte(`{"action":"remove","result":"failed","error":"cannot encode the report"}`)
		}
		switch {
		case len(raw) <= maxReportBytes:
			return append(raw, '\n')
		case len(r.Error) > 1100:
			r.Error = r.Error[:1000] + "... (see the pod log)"
		case len(r.Kept) > 0:
			r.Kept = r.Kept[:len(r.Kept)-1]
		default:
			r.Error = "the report is too long; see the pod log"
		}
	}
}

// writeReport writes the report of a removal pass that ended with err to
// the termination message file path. A failure to write it is logged: the
// pass's own outcome stays what decides the exit code.
func writeReport(path string, rep *removalReport, err error) {
	r := *rep
	if err != nil {
		r.Result, r.Error = resultFailed, err.Error()
	}
	// Kubelet creates the file and reads it as root; its mode stays.
	if werr := os.WriteFile(path, r.encode(), 0o600); werr != nil {
		logf("cannot write the removal report to %s: %v", path, werr)
	}
}

// remover is the state of one removal pass.
type remover struct {
	cfg *config
	rep *removalReport
	// removed counts the files and entries the pass removed.
	removed int
	// foreignBinDir is kubelet's bin dir when kubelet's config holds an
	// entry of this name that is not a bridge entry: kubelet runs
	// <bin dir>/<name> for it, so that file stays, whatever owns it.
	foreignBinDir string
}

func (r *remover) keep(what, why string) {
	r.rep.Kept = append(r.rep.Kept, what+": "+why)
	logf("keeping %s: %s", what, why)
}

// runRemove performs one removal pass (ADR-0039) and returns its report.
func runRemove(cfg *config) (*removalReport, error) {
	r := &remover{cfg: cfg, rep: &removalReport{Action: actionRemove, Provider: cfg.ProviderName}}
	var err error
	if cfg.Mode == modeNone {
		err = r.removeNone()
	} else {
		err = r.removeWired()
	}
	switch {
	case err != nil:
		r.rep.Result = resultFailed
	case len(r.rep.Kept) > 0:
		r.rep.Result = resultKept
	case r.removed > 0 || r.rep.KubeletRestarted:
		r.rep.Result = resultRemoved
	default:
		r.rep.Result = resultNothing
	}
	if err == nil {
		logf("removal of provider %q complete: %s", cfg.ProviderName, r.rep.Result)
	}
	return r.rep, err
}

// configRef is a credential-provider config that may hold this install's
// entry, with the bin dir whose records vouch for the other entries in it
// when it is a chart-owned config.
type configRef struct {
	path, binDir string
}

// removeWired is the removal pass of auto, merge and patch mode, which see
// and restart kubelet. It holds the node lock for the whole pass.
func (r *remover) removeWired() error {
	cfg := r.cfg
	unlock, err := lockFile(cfg.hostPath(nodeLockPath), cfg.lockTimeout)
	if err != nil {
		return err
	}
	defer unlock()

	w, err := discoverKubelet(cfg.ProcRoot)
	if err != nil {
		return fmt.Errorf("remove: %w", err)
	}
	st, err := loadState(cfg.statePath())
	if err != nil {
		return err
	}
	if st != nil {
		for _, p := range []string{st.BinDir, st.ConfigFile, st.EnvFile} {
			if p != "" && validNodePath(p) != nil {
				return fmt.Errorf("the state file %s names the path %q, which is not a clean absolute node path; refusing to act on it", cfg.statePath(), p)
			}
		}
	}
	if err := r.checkNextStart(st, w); err != nil {
		return err
	}
	// Earlier provider names of this release go too (decision 6).
	binDirs := r.binDirs(st, w)
	if err := cfg.findRetiring(binDirs); err != nil {
		return err
	}
	stripped, err := r.removeFromKubeletConfig(st, w)
	if err != nil {
		return err
	}

	// Kubelet no longer reads this install's entry. What is left are files
	// kubelet does not use: configs it does not read and their entries, the
	// binaries and records, the CA and mTLS files, and the state file.
	var unread []configRef
	add := func(path, binDir string) {
		if path == "" || binDir == "" || (path == w.ConfigFile && !stripped) {
			return
		}
		for _, c := range unread {
			if c.path == path {
				return
			}
		}
		unread = append(unread, configRef{path: path, binDir: binDir})
	}
	if w.wired() {
		add(w.ConfigFile, w.BinDir)
	}
	if cfg.HostConfigDir != "" && cfg.HostBinDir != "" {
		add(cfg.ownConfigPath(), cfg.HostBinDir)
	}
	if st != nil {
		add(st.ConfigFile, st.BinDir)
	}
	for _, d := range binDirs {
		if rec := cfg.readRecord(filepath.Join(d, cfg.files().Record)); rec != nil && validNodePath(rec.ChartOwnedConfig) == nil {
			add(rec.ChartOwnedConfig, d)
		}
	}
	for _, c := range unread {
		if err := r.removeFromUnreadConfig(c); err != nil {
			return err
		}
	}
	for _, d := range binDirs {
		if err := r.removeBinaryAndRecord(d); err != nil {
			return err
		}
	}
	if err := r.removeAuxFiles(); err != nil {
		return err
	}
	if err := r.removeState(); err != nil {
		return err
	}
	return cfg.retire(binDirs)
}

// holdsOwn reports whether the CredentialProviderConfig in doc holds a
// bridge entry of this install's name or of one of its retiring names, and
// whether it holds an entry of this install's name that is not a bridge
// entry (foreign).
func (r *remover) holdsOwn(doc []byte) (holds, foreign bool, err error) {
	holds, foreign, err = entriesNamed(doc, r.cfg.ProviderName)
	for _, name := range r.cfg.retiringNames() {
		if err != nil || holds {
			break
		}
		holds, err = holdsBridgeEntry(doc, name)
	}
	return holds, foreign, err
}

// checkNextStart refuses a removal while kubelet would start with another
// wiring than the one it runs (ADR-0039, decision 3.3): this install's
// state records patch mode, and the kubelet environment file sets the
// flags to other directories, because an install pass died between
// writing them and restarting kubelet. The pass would otherwise clean
// files kubelet reads at its next start.
func (r *remover) checkNextStart(st *state, w kubeletWiring) error {
	if st == nil || st.Mode != modePatch {
		return nil
	}
	envFile, err := r.cfg.kubeletEnvFile()
	if err != nil {
		return fmt.Errorf("remove: this install's state records patch mode: %w", err)
	}
	prior, err := readPriorHostFile(r.cfg.hostPath(envFile))
	if err != nil {
		return fmt.Errorf("read %s: %w", envFile, err)
	}
	e, err := parseExtraArgs(prior.data)
	if err != nil {
		return fmt.Errorf("%s: %w", envFile, err)
	}
	next := kubeletWiring{BinDir: flagValue(e.args, flagBinDir), ConfigFile: flagValue(e.args, flagConfigFile)}
	if e.appended >= 0 {
		next = e.appendedWiring
	}
	if next.wired() && next != w {
		return fmt.Errorf("kubelet runs with bin-dir=%q config=%q, but %s sets bin-dir=%q config=%q, which kubelet reads at its next start: an install pass died between writing the flags and restarting kubelet. Refusing to remove anything until kubelet runs what its environment file sets: restart kubelet on the node (systemctl restart %s), or run an install pass again, then remove", w.BinDir, w.ConfigFile, envFile, next.BinDir, next.ConfigFile, r.cfg.KubeletUnit)
	}
	return nil
}

// binDirs returns the bin dirs this install may have its binary and record
// in: kubelet's, plugin.hostBinaryDir, the merge override and the one its
// state records, each once.
func (r *remover) binDirs(st *state, w kubeletWiring) []string {
	var dirs []string
	for _, d := range []string{w.BinDir, r.cfg.HostBinDir, r.cfg.MergeBinDir} {
		if d != "" && !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	if st != nil && st.BinDir != "" && !slices.Contains(dirs, st.BinDir) {
		dirs = append(dirs, st.BinDir)
	}
	return dirs
}

// removeFromKubeletConfig takes this install's entry out of the config
// kubelet reads (w), or, for the last patch install on its directories,
// the kubelet flags (decision 3.1), restarts kubelet and verifies it,
// rolling back as ADR-0033 does. It reports whether it removed the flags,
// after which kubelet no longer reads w.ConfigFile. It also restarts
// kubelet when an earlier removal pass changed that config and died
// before its restart (decision 2.8). It holds the config's lock until it
// returns.
func (r *remover) removeFromKubeletConfig(st *state, w kubeletWiring) (stripped bool, err error) {
	cfg := r.cfg
	if !w.wired() {
		return false, nil
	}
	if fi, err := os.Lstat(cfg.hostPath(w.ConfigFile)); err == nil && fi.IsDir() {
		// Kubelet 1.34+ reads every config file of a directory; the installer
		// edits none of them (runMerge).
		docs, err := configDocs(cfg.hostPath(w.ConfigFile))
		if err != nil {
			return false, fmt.Errorf("read kubelet's credential-provider config directory %s: %w", w.ConfigFile, err)
		}
		for _, doc := range docs {
			if holds, _ := holdsBridgeEntry(doc, cfg.ProviderName); holds {
				return false, fmt.Errorf("kubelet's credential-provider config %s is a directory with a file that holds provider %q; the installer does not edit such a directory: remove the entry from it by hand, restart kubelet, then remove again", w.ConfigFile, cfg.ProviderName)
			}
		}
		return false, nil
	}
	dir, err := cfg.openDirOf(w.ConfigFile, false)
	if err != nil {
		return false, fmt.Errorf("open the directory of kubelet's credential-provider config %s: %w", w.ConfigFile, err)
	}
	defer func() { _ = dir.Close() }()
	name := filepath.Base(w.ConfigFile)
	unlock, err := lockFileIn(dir, name+lockSuffix, cfg.lockTimeout)
	if err != nil {
		return false, err
	}
	defer unlock()
	prior, err := readPrior(dir, name)
	if err != nil {
		return false, fmt.Errorf("read kubelet's credential-provider config %s: %w", w.ConfigFile, err)
	}
	holds, foreign := false, false
	if prior.existed {
		if holds, foreign, err = r.holdsOwn(prior.data); err != nil {
			return false, fmt.Errorf("kubelet's credential-provider config %s: %w; refusing to guess whether it holds provider %q", w.ConfigFile, err, cfg.ProviderName)
		}
	}
	if foreign {
		r.foreignBinDir = w.BinDir
	}
	if !holds {
		return false, r.restartAfterEarlierRemoval(st, w, prior.data)
	}

	chartOwned := w.ConfigFile == cfg.ownConfigPath() || withinDir(w.ConfigFile, cfg.HostConfigDir)
	if !chartOwned {
		if chartOwned, err = cfg.claimsChartOwned(w.BinDir, w.ConfigFile); err != nil {
			return false, err
		}
	}
	var desired []byte
	remaining := 0
	if chartOwned {
		var dropped []string
		if desired, dropped, remaining, err = composeWithout(prior.data, cfg.ProviderName, cfg.siblingIn(w.BinDir)); err != nil {
			return false, fmt.Errorf("kubelet's credential-provider config %s: %w; refusing to edit it", w.ConfigFile, err)
		}
		for _, d := range dropped {
			logf("dropping provider %s from %s: not an entry of another harbor-bridge install on this node, or not as that install recorded it", d, w.ConfigFile)
		}
	} else {
		if desired, _, remaining, err = removeProvider(prior.data, cfg.ProviderName); err != nil {
			return false, err
		}
		var rest int
		if desired, _, rest, err = cfg.withoutRetiring(desired); err != nil {
			return false, err
		}
		if rest >= 0 {
			remaining = rest
		}
	}
	if remaining == 0 {
		return r.removeOwnFlags(st, w)
	}
	if err := cfg.checkKubeletCanStart(desired, w); err != nil {
		return false, err
	}
	t := removalTarget(w, "", cfg.ProviderName, desired, nil)
	if err := cfg.refuseRejected(t); err != nil {
		return false, err
	}
	var written bool
	if chartOwned {
		written, err = writeFileIn(dir, name, desired, 0o644)
	} else {
		// Usually the cloud's file: it keeps its owner and at most its
		// permissions (writeForeignIn).
		written, err = writeForeignIn(dir, name, desired)
	}
	if err != nil {
		return false, err
	}
	logf("removed provider %q from %s", cfg.ProviderName, w.ConfigFile)
	r.removed++
	var rb rollback
	rb.file(w.ConfigFile, func() error { return restoreIn(dir, name, prior) })
	return false, r.restart(st, t, written, func() error {
		got, err := discoverKubelet(cfg.ProcRoot)
		if err != nil {
			return err
		}
		if got != w {
			return fmt.Errorf("running kubelet has bin-dir=%q config=%q after the restart, want %q/%q", got.BinDir, got.ConfigFile, w.BinDir, w.ConfigFile)
		}
		return nil
	}, &rb)
}

// removeOwnFlags is decision 3.1: kubelet's config holds no entry that
// stays, so the flags that point kubelet at it must go, which the pass
// does only for the wiring its own verified patch-mode restart set, from
// the environment file the kubelet unit takes KUBELET_EXTRA_ARGS from.
// Kubelet then reads no credential-provider config at all; the chart-owned
// config goes later, as a config kubelet does not read.
func (r *remover) removeOwnFlags(st *state, w kubeletWiring) (bool, error) {
	cfg := r.cfg
	wired := func(s *state) bool {
		return s != nil && s.Mode == modePatch && s.BinDir == w.BinDir && s.ConfigFile == w.ConfigFile
	}
	if !wired(st) {
		// The release may have set the flags under an earlier provider name
		// (decision 6), whose state file then records it.
		for _, name := range cfg.retiringNames() {
			prev, err := loadState(filepath.Join(cfg.hostPath(cfg.StateDir), filesFor(name).State))
			if err != nil {
				return false, err
			}
			if wired(prev) {
				logf("the kubelet flags were set by this release under its earlier provider name %q", name)
				st = prev
				break
			}
		}
	}
	if !wired(st) {
		return false, fmt.Errorf("removing provider %q would leave kubelet's credential-provider config %s without providers, which kubelet refuses at its next start, and this install did not set the kubelet flags that point at it (its state file %s records no patch-mode restart onto bin-dir=%q config=%q). Remove the flags, or the entry, by hand, restart kubelet, then remove again (docs/operations.md, Uninstalling)", cfg.ProviderName, w.ConfigFile, filepath.Join(cfg.StateDir, cfg.files().State), w.BinDir, w.ConfigFile)
	}
	envFile, err := cfg.kubeletEnvFile()
	if err != nil {
		return false, fmt.Errorf("remove: %w", err)
	}
	if envFile != st.envFile() {
		return false, fmt.Errorf("the kubelet unit %q takes KUBELET_EXTRA_ARGS from %s, but this install's patch-mode pass put the flags into %s; refusing to guess where kubelet's flags come from. Remove them by hand, restart kubelet, then remove again", cfg.KubeletUnit, envFile, st.envFile())
	}
	envPath := cfg.hostPath(envFile)
	priorEnv, err := readPriorHostFile(envPath)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", envFile, err)
	}
	desiredEnv, err := removeExtraArgs(priorEnv.data, w.BinDir, w.ConfigFile)
	if err != nil {
		return false, fmt.Errorf("%s: %w", envFile, err)
	}
	t := removalTarget(w, envFile, cfg.ProviderName, nil, desiredEnv)
	if err := cfg.refuseRejected(t); err != nil {
		return false, err
	}
	// The environment file is the node's, not the chart's: it keeps its
	// owner and at most its permissions (writeForeignIn).
	changed, err := writeForeignFile(envPath, desiredEnv)
	if err != nil {
		return false, err
	}
	if changed {
		logf("removed the --image-credential-provider-* flags from KUBELET_EXTRA_ARGS in %s: provider %q was the last entry of %s", envFile, cfg.ProviderName, w.ConfigFile)
		r.removed++
	}
	var rb rollback
	rb.file(envFile, func() error { return restoreHostFile(envPath, priorEnv) })
	err = r.restart(st, t, changed, func() error {
		got, err := discoverKubelet(cfg.ProcRoot)
		if err != nil {
			return err
		}
		if got == w {
			return fmt.Errorf("running kubelet still has bin-dir=%q config=%q after the restart; does the %s unit take KUBELET_EXTRA_ARGS from %s?", got.BinDir, got.ConfigFile, cfg.KubeletUnit, envFile)
		}
		return nil
	}, &rb)
	return err == nil, err
}

// restartAfterEarlierRemoval handles kubelet's config without this
// install's entry. When the state records a verified install onto exactly
// the running wiring, kubelet read the entry at that restart, and an
// earlier removal pass (or something else) took it out since without a
// verified restart: kubelet still runs it, and a restart makes kubelet run
// what it reads at its next start (decision 2.8). A state that records a
// verified removal, another wiring, or no state at all needs none.
func (r *remover) restartAfterEarlierRemoval(st *state, w kubeletWiring, current []byte) error {
	if st == nil || st.Mode == modeRemove || st.BinDir != w.BinDir || st.ConfigFile != w.ConfigFile {
		return nil
	}
	t := removalTarget(w, "", r.cfg.ProviderName, current, nil)
	if err := r.cfg.refuseRejected(t); err != nil {
		return err
	}
	logf("kubelet's credential-provider config %s no longer holds provider %q, but this install's state records a restart onto it: restarting kubelet so that it stops running the entry", w.ConfigFile, r.cfg.ProviderName)
	var rb rollback
	return r.restart(st, t, false, nil, &rb)
}

// restart is finishWithRestart for a removal; it records in the report
// whether kubelet restarted.
func (r *remover) restart(st *state, t target, changedNow bool, verify func() error, rb *rollback) error {
	if changedNow || !st.matches(t) {
		r.rep.KubeletRestarted = true
	}
	return finishWithRestart(r.cfg, t, changedNow, verify, rb)
}

// removalTarget is the content of a removal pass for the restart policy
// and the rejection record (ADR-0033): kubelet on w, with config the
// config kubelet reads after the pass (nil when the flags go) and env the
// environment file after it (nil when it does not change).
func removalTarget(w kubeletWiring, envFile, name string, config, env []byte) target {
	return target{
		mode: modeRemove, binDir: w.BinDir, configFile: w.ConfigFile, envFile: envFile,
		entryHash: contentHash([]byte("removed "+name), env),
		fileHash:  contentHash(config, env),
	}
}

// removeFromUnreadConfig takes this install's entry out of a config
// kubelet does not read (decision 3.7): this install's own chart-owned
// config when kubelet reads another one, the config its state records,
// one its record names, and kubelet's former config after the flags went.
// No restart is needed. A chart-owned config left without entries is
// deleted with its .bak; a cloud's config is never deleted.
func (r *remover) removeFromUnreadConfig(c configRef) error {
	cfg := r.cfg
	if _, err := os.Lstat(cfg.hostPath(c.path)); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	unlock, err := lockConfig(cfg, cfg.configLockPath(c.path))
	if err != nil {
		return err
	}
	defer unlock()
	dir, err := cfg.openDirOf(c.path, false)
	if err != nil {
		return fmt.Errorf("open the directory of %s: %w", c.path, err)
	}
	defer func() { _ = dir.Close() }()
	name := filepath.Base(c.path)
	prior, err := readPrior(dir, name)
	switch {
	case err != nil:
		r.keep(c.path, fmt.Sprintf("cannot read it (%v)", err))
		return nil
	case !prior.existed:
		return nil
	}
	holds, _, err := r.holdsOwn(prior.data)
	switch {
	case err != nil:
		r.keep(c.path, fmt.Sprintf("kubelet does not read it, and it is not a credential-provider config the installer can parse (%v)", err))
		return nil
	case !holds:
		return nil
	}
	chartOwned := c.path == cfg.ownConfigPath() || withinDir(c.path, cfg.HostConfigDir)
	if !chartOwned {
		if chartOwned, err = cfg.claimsChartOwned(c.binDir, c.path); err != nil {
			return err
		}
	}
	if !chartOwned {
		desired, changed, _, err := removeProvider(prior.data, cfg.ProviderName)
		var retired bool
		if err == nil {
			desired, retired, _, err = cfg.withoutRetiring(desired)
		}
		if err != nil {
			r.keep(c.path, err.Error())
			return nil
		}
		if changed || retired {
			if _, err := writeForeignIn(dir, name, desired); err != nil {
				return err
			}
			logf("removed provider %q from %s, which kubelet does not read", cfg.ProviderName, c.path)
			r.removed++
		}
		return nil
	}
	desired, _, remaining, err := composeWithout(prior.data, cfg.ProviderName, cfg.siblingIn(c.binDir))
	if err != nil {
		r.keep(c.path, err.Error())
		return nil
	}
	if remaining == 0 {
		if err := removeRegularIn(dir, name); err != nil {
			return err
		}
		if err := removeRegularIn(dir, name+".bak"); err != nil {
			return err
		}
		logf("deleted %s: kubelet does not read it, and provider %q was its last entry", c.path, cfg.ProviderName)
		r.removed++
		return nil
	}
	if _, err := writeFileIn(dir, name, desired, 0o644); err != nil {
		return err
	}
	logf("removed provider %q from %s, which kubelet does not read", cfg.ProviderName, c.path)
	r.removed++
	return nil
}

// removeBinaryAndRecord deletes this install's binary and then its record
// from the bin dir binDir (a node path), each with its .bak and .tmp-*
// copies. The binary is this install's when its record is next to it, the
// name is the default name, or it has the bytes of the plugin in the image
// (checkBinaryOwnership); any other file of that name stays.
func (r *remover) removeBinaryAndRecord(binDir string) error {
	cfg := r.cfg
	root, err := os.OpenRoot(cfg.hostPath(binDir))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("open %s: %w", binDir, err)
	}
	defer func() { _ = root.Close() }()
	files := cfg.files()
	if binDir == r.foreignBinDir {
		r.keep(filepath.Join(binDir, files.Binary), "kubelet's config holds an entry of this name that is not a harbor-bridge entry, and kubelet runs this file for it")
		return r.removeOwnFile(root, binDir, files.Record)
	}
	ours, err := r.ownsBinary(root, binDir)
	if err != nil {
		return err
	}
	if ours {
		if err := r.removeOwnFile(root, binDir, files.Binary); err != nil {
			return err
		}
	}
	return r.removeOwnFile(root, binDir, files.Record)
}

// ownsBinary reports whether <binDir>/<name> is this install's to delete,
// or absent; a file of another program keeps its name and is reported.
func (r *remover) ownsBinary(root *os.Root, binDir string) (bool, error) {
	cfg := r.cfg
	files := cfg.files()
	if cfg.ProviderName == defaultProviderName || isRegularHostFile(cfg.hostPath(filepath.Join(binDir, files.Record))) {
		return true, nil
	}
	current, err := readFileIn(root, files.Binary)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return true, nil
	case err != nil:
		r.keep(filepath.Join(binDir, files.Binary), fmt.Sprintf("not a regular file this installer can read (%v)", err))
		return false, nil
	}
	plugin, err := os.ReadFile(cfg.SourcePlugin)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", cfg.SourcePlugin, err)
	}
	if !bytes.Equal(current, plugin) {
		r.keep(filepath.Join(binDir, files.Binary), "no record of this install next to it and not this plugin: a file of another program")
		return false, nil
	}
	return true, nil
}

// tmpSuffix matches the suffix replaceFile gives its temp files.
var tmpSuffix = regexp.MustCompile(`^\.tmp-[0-9a-f]{16}$`)

// removeOwnFile deletes the file name of this install in root (the node
// directory dir), its .bak, and the temp files an interrupted write of
// either left behind. Only regular files go (removeRegularIn); anything
// else under these names is reported and stays.
func (r *remover) removeOwnFile(root *os.Root, dir, name string) error {
	targets := []string{name, name + ".bak"}
	d, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("list %s: %w", dir, err)
	}
	entries, err := d.ReadDir(-1)
	_ = d.Close()
	if err != nil {
		return fmt.Errorf("list %s: %w", dir, err)
	}
	for _, e := range entries {
		for _, base := range []string{name, name + ".bak"} {
			if rest, ok := strings.CutPrefix(e.Name(), base); ok && tmpSuffix.MatchString(rest) {
				targets = append(targets, e.Name())
			}
		}
	}
	for _, n := range targets {
		lfi, err := root.Lstat(n)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return fmt.Errorf("stat %s: %w", filepath.Join(dir, n), err)
		case !lfi.Mode().IsRegular():
			r.keep(filepath.Join(dir, n), fmt.Sprintf("not a regular file (%s)", lfi.Mode().Type()))
			continue
		}
		if err := removeRegularIn(root, n); err != nil {
			return err
		}
		logf("deleted %s", filepath.Join(dir, n))
		r.removed++
	}
	return nil
}

// removeAuxFiles deletes this install's CA and mTLS files from
// plugin.hostConfigDir, whether mTLS is on now or not. Kubelet does not
// need them to start; the plugin reads them per pull.
func (r *remover) removeAuxFiles() error {
	cfg := r.cfg
	if cfg.HostConfigDir == "" {
		return nil
	}
	if _, err := os.Lstat(cfg.hostPath(cfg.HostConfigDir)); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	dir, err := cfg.openConfigDir()
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	files := cfg.files()
	for _, name := range []string{files.CA, files.ClientCert, files.ClientKey} {
		if err := r.removeOwnFile(dir, cfg.HostConfigDir, name); err != nil {
			return err
		}
	}
	return nil
}

// removeState deletes this install's state file, last: until then it
// records what kubelet was last restarted onto (decision 2.8).
func (r *remover) removeState() error {
	root, err := os.OpenRoot(r.cfg.hostPath(r.cfg.StateDir))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("open %s: %w", r.cfg.StateDir, err)
	}
	defer func() { _ = root.Close() }()
	return r.removeOwnFile(root, r.cfg.StateDir, r.cfg.files().State)
}

// removeNone is the removal pass of none mode (decision 3.8). Its pod sees
// only plugin.hostBinaryDir and plugin.hostConfigDir, not kubelet's flags,
// and cannot restart kubelet. Kubelet may read the chart-owned config or a
// config of the operator's that names the binary, so the binary stays, and
// the entry stays when no other entry would be left in the chart-owned
// config. The CA and mTLS files go.
func (r *remover) removeNone() error {
	cfg := r.cfg
	files := cfg.files()
	binary := filepath.Join(cfg.HostBinDir, files.Binary)
	configPath := cfg.ownConfigPath()
	if err := cfg.findRetiring(uniqueDirs(cfg.HostBinDir)); err != nil {
		return err
	}
	entryStays := false
	if _, statErr := os.Lstat(cfg.hostPath(cfg.HostConfigDir)); statErr == nil {
		stays, err := r.removeNoneEntry()
		if err != nil {
			return err
		}
		entryStays = stays
	}
	if entryStays {
		r.keep(configPath, fmt.Sprintf("provider %q is its last entry, and kubelet refuses a config without providers if its flags point here; remove the flags (or the entry) by hand, restart kubelet, then delete %s and its record", cfg.ProviderName, binary))
	} else if err := r.emptyRecord(); err != nil {
		return err
	}
	if isRegularHostFile(cfg.hostPath(binary)) {
		r.keep(binary, "none mode cannot see which config kubelet reads, and kubelet does not start while a configured provider's binary is missing; delete it and its record once kubelet runs without the entry")
	}
	if err := r.removeBinaryBackups(); err != nil {
		return err
	}
	if err := r.removeAuxFiles(); err != nil {
		return err
	}
	return cfg.retire(uniqueDirs(cfg.HostBinDir))
}

// removeBinaryBackups deletes the .bak copies of the binary and the record
// in plugin.hostBinaryDir (none mode, which keeps the binary itself): the
// earlier plugin in the .bak is read by no kubelet config.
func (r *remover) removeBinaryBackups() error {
	cfg := r.cfg
	root, err := os.OpenRoot(cfg.hostPath(cfg.HostBinDir))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("open %s: %w", cfg.HostBinDir, err)
	}
	defer func() { _ = root.Close() }()
	files := cfg.files()
	for _, name := range []string{files.Binary + ".bak", files.Record + ".bak"} {
		if err := r.removeOwnFile(root, cfg.HostBinDir, name); err != nil {
			return err
		}
	}
	return nil
}

// removeNoneEntry drops this install's entry from the chart-owned config
// in none mode when another install's vouched entry stays in it, and
// reports whether the entry stays.
func (r *remover) removeNoneEntry() (bool, error) {
	cfg := r.cfg
	dir, err := cfg.openConfigDir()
	if err != nil {
		return false, err
	}
	defer func() { _ = dir.Close() }()
	unlock, err := lockFileIn(dir, configFileName+lockSuffix, cfg.lockTimeout)
	if err != nil {
		return false, err
	}
	defer unlock()
	prior, err := readPrior(dir, configFileName)
	if err != nil || !prior.existed {
		return false, err
	}
	holds, _, err := r.holdsOwn(prior.data)
	if err != nil {
		return false, fmt.Errorf("%s: %w; refusing to guess whether it holds provider %q", cfg.ownConfigPath(), err, cfg.ProviderName)
	}
	if !holds {
		return false, nil
	}
	desired, _, remaining, err := composeWithout(prior.data, cfg.ProviderName, cfg.siblingIn(cfg.HostBinDir))
	if err != nil {
		return false, fmt.Errorf("%s: %w; refusing to edit it", cfg.ownConfigPath(), err)
	}
	if remaining == 0 {
		return true, nil
	}
	if _, err := writeFileIn(dir, configFileName, desired, 0o644); err != nil {
		return false, err
	}
	logf("removed provider %q from %s; kubelet stops running it at its next start", cfg.ProviderName, cfg.ownConfigPath())
	r.removed++
	return false, nil
}

// emptyRecord reduces this install's record in plugin.hostBinaryDir to no
// entries (none mode, entry gone): it keeps the binary this install's for
// a later install (checkBinaryOwnership), and vouches for no copy of the
// entry in any chart-owned config (siblingIn).
func (r *remover) emptyRecord() error {
	cfg := r.cfg
	path := filepath.Join(cfg.HostBinDir, cfg.files().Record)
	rec := cfg.readRecord(path)
	if rec == nil || len(rec.Entries) == 0 {
		return nil
	}
	if err := cfg.writeRecord(cfg.HostBinDir, entryRecord{Entries: []json.RawMessage{}, Release: rec.Release}); err != nil {
		return err
	}
	logf("emptied the record %s: it vouches for no entry of provider %q any more", path, cfg.ProviderName)
	r.removed++
	return nil
}
