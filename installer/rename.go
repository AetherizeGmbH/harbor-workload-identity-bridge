// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// A rename of plugin.providerName (ADR-0039, decision 6). Every record
// names the release that wrote it last (entryRecord.Release, RELEASE_ID).
// A pass of this install finds the records of the same release under
// other provider names: those names are this release's earlier ones, and
// they retire. Their entries leave the configs in the same write that adds
// this install's entry (siblingIn no longer vouches for them), so kubelet
// restarts once, and their files go after that restart verified. A name
// another release took over has a record of that release, and stays.

// findRetiring sets c.retiring to the provider names whose record in one
// of binDirs (node paths) names this install's release, other than this
// install's own. Without a release (RELEASE_ID unset, an older chart) no
// name retires; a record without one (written before ADR-0039) belongs to
// no release.
func (c *config) findRetiring(binDirs []string) error {
	c.retiring = map[string]bool{}
	if c.ReleaseID == "" {
		return nil
	}
	for _, d := range binDirs {
		root, err := os.OpenRoot(c.hostPath(d))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return fmt.Errorf("list the records in %s: %w", d, err)
		}
		dir, err := root.Open(".")
		if err != nil {
			_ = root.Close()
			return fmt.Errorf("list the records in %s: %w", d, err)
		}
		entries, err := dir.ReadDir(-1)
		_ = dir.Close()
		_ = root.Close()
		if err != nil {
			return fmt.Errorf("list the records in %s: %w", d, err)
		}
		for _, e := range entries {
			name, ok := strings.CutSuffix(e.Name(), recordSuffix)
			if !ok || e.IsDir() || name == c.ProviderName || validProviderName(name) != nil {
				continue
			}
			if rec := c.readRecord(filepath.Join(d, e.Name())); rec != nil && rec.Release == c.ReleaseID && !c.retiring[name] {
				c.retiring[name] = true
				logf("provider %q is an earlier name of release %s (its record in %s says so): its entry and files go in this pass", name, c.ReleaseID, d)
			}
		}
	}
	return nil
}

// retiringNames returns c.retiring in a fixed order.
func (c *config) retiringNames() []string {
	var names []string
	for n := range c.retiring {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// withoutRetiring takes the entries of the retiring names out of doc, a
// cloud's config (removeProvider); a chart-owned config loses them through
// siblingIn.
func (c *config) withoutRetiring(doc []byte) (out []byte, changed bool, remaining int, err error) {
	out = doc
	remaining = -1
	for _, name := range c.retiringNames() {
		var ch bool
		if out, ch, remaining, err = removeProvider(out, name); err != nil {
			return nil, false, 0, err
		}
		changed = changed || ch
	}
	return out, changed, remaining, nil
}

// uniqueDirs returns the non-empty dirs, each once, in order.
func uniqueDirs(dirs ...string) []string {
	var out []string
	for _, d := range dirs {
		if d != "" && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

// retire deletes the files of the retiring names after a pass that took
// their entries out of every config it writes (decision 6). In none mode
// the binaries, CA and mTLS files stay, as in a removal (decision 3.8):
// kubelet runs the old entry until someone restarts it. Their records
// vouch for nothing any more. In the other modes the old entry first
// leaves every other config on the node that may hold it
// (retiringConfigs), and a binary goes only once no config holds its
// entry: the pass may not have written the config kubelet reads (a mode
// change together with the rename), kubelet may read a cloud's config
// again, and kubelet does not start while a configured provider's binary
// is missing. What stays is added to rep, the removal's report, or only
// logged in an install pass (rep nil).
func (c *config) retire(binDirs []string, rep *removalReport) error {
	if rep == nil {
		rep = &removalReport{}
	}
	for _, name := range c.retiringNames() {
		pc := *c
		pc.ProviderName = name
		pc.retiring = nil
		r := &remover{cfg: &pc, rep: rep}
		if c.Mode == modeNone {
			// Kubelet runs the old entry until someone restarts it, and the
			// old entry reads the old CA and mTLS files on every pull: they
			// stay with the binary (decision 6).
			if err := r.emptyRecord(); err != nil {
				return err
			}
			if err := r.removeBinaryBackups(); err != nil {
				return err
			}
			continue
		}
		w, _, err := c.removalWiring()
		if err != nil {
			return err
		}
		held, err := c.configHolds(w, name)
		if err != nil {
			return err
		}
		if held {
			r.keep(filepath.Join(w.BinDir, filesFor(name).Binary), fmt.Sprintf("kubelet's config %s still holds an entry of the earlier provider name %q", w.ConfigFile, name))
			continue
		}
		// Every other config on the node that may hold the old entry loses
		// it first: kubelet may read one of them again (a cloud's config,
		// when this install's flags go), and does not start while a
		// configured provider's binary is missing.
		for _, cr := range c.retiringConfigs(name, binDirs, w) {
			if err := r.removeFromUnreadConfig(cr); err != nil {
				return err
			}
		}
		if r.entryKept {
			r.keep("the binaries and records of the earlier provider name "+strconv.Quote(name), "a credential-provider config on the node still holds its entry (above)")
			continue
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
	}
	return nil
}

// retiringConfigs returns the configs other than kubelet's (w) that may
// hold an entry of the earlier name: this install's chart-owned config, the
// one the earlier name's state records, the ones its records name, and the
// merge-mode override, each with the bin dir whose records vouch for the
// other entries in it.
func (c *config) retiringConfigs(name string, binDirs []string, w kubeletWiring) []configRef {
	var out []configRef
	add := func(path, binDir string) {
		if path == "" || binDir == "" || path == w.ConfigFile || validNodePath(path) != nil || validNodePath(binDir) != nil {
			return
		}
		for _, cr := range out {
			if cr.path == path {
				return
			}
		}
		out = append(out, configRef{path: path, binDir: binDir})
	}
	if c.HostConfigDir != "" {
		add(c.ownConfigPath(), c.HostBinDir)
	}
	if st, err := loadState(filepath.Join(c.hostPath(c.StateDir), filesFor(name).State)); err == nil && st != nil {
		add(st.ConfigFile, st.BinDir)
	}
	for _, d := range binDirs {
		if rec := c.readRecord(filepath.Join(d, filesFor(name).Record)); rec != nil {
			add(rec.ChartOwnedConfig, d)
		}
	}
	add(c.MergeConfigFile, c.MergeBinDir)
	return out
}

// configHolds reports whether the config of the wiring w holds a bridge
// entry named name.
func (c *config) configHolds(w kubeletWiring, name string) (bool, error) {
	if !w.wired() {
		return false, nil
	}
	docs, err := configDocs(c.hostPath(w.ConfigFile))
	if err != nil {
		return false, fmt.Errorf("read kubelet's credential-provider config %s: %w", w.ConfigFile, err)
	}
	for _, doc := range docs {
		if held, err := holdsBridgeEntry(doc, name); err != nil || held {
			return held, err
		}
	}
	return false, nil
}
