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
	"strings"
)

// entryRecord is the content of an install's record, <bin dir>/<name>.entry
// (filesFor, ADR-0029): what that install's installer wrote into kubelet's
// credential-provider config, kept next to its binary, where no writer of
// plugin.hostConfigDir reaches. siblingIn keeps another install's entry
// only when its record holds it, and checkBinaryOwnership takes the
// record's presence as proof that the binary next to it is this install's.
//
// The record and the config are two files, which no pass can replace
// together. A pass therefore first adds the entry it is about to write to
// the entries the record already holds (beginRecord), then writes its
// binary and the config, and only then reduces the record to that entry
// (commitRecord). A pass that dies in between, after a helm upgrade
// changed the entry, leaves a record that holds both the entry still in
// the config and the one it meant to write, so the other installs keep
// whichever of the two the config holds until this install's next pass.
//
// The record also names the chart-owned config the install writes its
// entry into (ChartOwnedConfig). Merge mode edits kubelet's config as it
// is, but when that config is the chart-owned config of a patch- or
// none-mode install, a writer of that install's plugin.hostConfigDir can
// plant or change entries in it; merge mode then keeps only the entries
// the records vouch for, as patch and none mode do (runMerge).
type entryRecord struct {
	// Entries are the canonical bytes (entryBytes) of the entries this
	// install may have in the config: after a completed pass exactly the
	// entry it wrote.
	Entries []json.RawMessage `json:"entries"`
	// ChartOwnedConfig is the node path of the chart-owned config this
	// install writes its entry into: its own config in patch and none
	// mode, and in merge mode kubelet's config when that is a chart-owned
	// config. Empty when it merges into a cloud's config.
	ChartOwnedConfig string `json:"chartOwnedConfig,omitempty"`
}

// holds reports whether the record holds the canonical entry entryJSON.
func (r *entryRecord) holds(entryJSON []byte) bool {
	if r == nil {
		return false
	}
	for _, e := range r.Entries {
		if bytes.Equal(e, entryJSON) {
			return true
		}
	}
	return false
}

// readRecord returns the record at the node path path. A missing record,
// one that is not a regular file, or one it cannot parse is nil: it
// vouches for nothing.
func (c *config) readRecord(path string) *entryRecord {
	raw, err := readHostFile(c.hostPath(path))
	if err != nil {
		return nil
	}
	var r entryRecord
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil
	}
	return &r
}

// writeRecord writes this install's record in binDir (a node path),
// readable and writable by the root installers only (recordFileMode).
func (c *config) writeRecord(binDir string, r entryRecord) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal entry record: %w", err)
	}
	_, err = writeFileAtomic(c.hostPath(filepath.Join(binDir, c.files().Record)), raw, recordFileMode)
	return err
}

// beginRecord records entryJSON in binDir before the pass writes the binary
// and the config chartOwned (empty for a cloud's config): the entries the
// record held, one of which the config may hold now, plus entryJSON. A
// record that is missing or cannot be parsed contributes none.
func (c *config) beginRecord(binDir string, entryJSON []byte, chartOwned string) error {
	r := c.readRecord(filepath.Join(binDir, c.files().Record))
	if r == nil {
		r = &entryRecord{}
	}
	if !r.holds(entryJSON) {
		r.Entries = append(r.Entries, entryJSON)
	}
	r.ChartOwnedConfig = chartOwned
	return c.writeRecord(binDir, *r)
}

// commitRecord reduces the record in binDir to entryJSON once the config
// holds it.
func (c *config) commitRecord(binDir string, entryJSON []byte, chartOwned string) error {
	return c.writeRecord(binDir, entryRecord{Entries: []json.RawMessage{entryJSON}, ChartOwnedConfig: chartOwned})
}

// claimsChartOwned reports whether a record in binDir (a node path) names
// configPath as a chart-owned config (entryRecord.ChartOwnedConfig). Only
// root on the node writes binDir (run and loadConfig keep it out of reach
// of plugin.hostConfigDir). A missing binDir holds no records; one it
// cannot list is an error, so the caller does not guess.
func (c *config) claimsChartOwned(binDir, configPath string) (bool, error) {
	root, err := os.OpenRoot(c.hostPath(binDir))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("list the records in %s: %w", binDir, err)
	}
	defer func() { _ = root.Close() }()
	d, err := root.Open(".")
	if err != nil {
		return false, fmt.Errorf("list the records in %s: %w", binDir, err)
	}
	entries, err := d.ReadDir(-1)
	_ = d.Close()
	if err != nil {
		return false, fmt.Errorf("list the records in %s: %w", binDir, err)
	}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), recordSuffix)
		if !ok || e.IsDir() || validProviderName(name) != nil {
			continue
		}
		if r := c.readRecord(filepath.Join(binDir, e.Name())); r != nil && r.ChartOwnedConfig == configPath {
			return true, nil
		}
	}
	return false, nil
}
