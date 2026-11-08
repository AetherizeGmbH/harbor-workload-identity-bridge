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
// the binaries stay, as in a removal (decision 3.8), and their records
// vouch for nothing any more. In the other modes a binary goes only while
// kubelet's config holds no entry of its name: the pass may not have
// written the config kubelet reads (a mode change together with the
// rename), and kubelet does not start while a configured provider's binary
// is missing.
func (c *config) retire(binDirs []string) error {
	for _, name := range c.retiringNames() {
		pc := *c
		pc.ProviderName = name
		pc.retiring = nil
		r := &remover{cfg: &pc, rep: &removalReport{}}
		if c.Mode == modeNone {
			if err := r.emptyRecord(); err != nil {
				return err
			}
			if err := r.removeBinaryBackups(); err != nil {
				return err
			}
			if err := r.removeAuxFiles(); err != nil {
				return err
			}
			continue
		}
		held, err := c.kubeletHolds(name)
		if err != nil {
			return err
		}
		if held {
			logf("keeping the files of provider %q: kubelet's config still holds an entry of that name", name)
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

// kubeletHolds reports whether the config the running kubelet reads holds
// a bridge entry named name.
func (c *config) kubeletHolds(name string) (bool, error) {
	w, err := discoverKubelet(c.ProcRoot)
	if err != nil || !w.wired() {
		return false, err
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
