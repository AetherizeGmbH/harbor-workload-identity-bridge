// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
)

// state records what the installer last successfully applied AND
// restarted kubelet for. It exists to survive the crash window between
// "files written" and "kubelet restarted": after such a crash the
// on-disk files already match the desired content, so a byte compare
// alone would skip the still-needed restart. The state hash is only
// persisted after a successful restart. CA/mTLS content is
// deliberately not part of the hash — the plugin reads those files on
// every exec, so their rotation never needs a restart (ADR-0021).
//
// Every install on a node has its own state file (nodeFiles.State,
// ADR-0029): the config file kubelet reads is shared, but each installer
// answers only for its own entry being live.
type state struct {
	SchemaVersion int    `json:"schemaVersion"`
	Mode          string `json:"mode"`
	BinDir        string `json:"binDir"`
	ConfigFile    string `json:"configFile"`
	// AppliedHash covers the restart-relevant content, as named by
	// HashScheme.
	AppliedHash string `json:"appliedHash"`
	// HashScheme says what AppliedHash covers. Empty: the whole effective
	// credential-provider config file plus, in patch mode, the
	// /etc/default/kubelet bytes (installers before ADR-0029).
	// hashSchemeEntry: only this install's provider entry plus, in patch
	// mode, the /etc/default/kubelet bytes. Other installs' entries in the
	// shared file are their own installers' concern, so a change there
	// does not make this installer restart kubelet again.
	HashScheme string `json:"hashScheme,omitempty"`
}

const stateSchemaVersion = 1

const hashSchemeEntry = "entry"

// target is what one install pass wants kubelet to run with.
type target struct {
	mode, binDir, configFile string
	// hash is the hashSchemeEntry hash of the desired content.
	hash string
	// legacyHash is what an installer before ADR-0029 recorded for the
	// same content: the hash over the whole resulting file.
	legacyHash string
}

// state returns the record of a successful restart for t.
func (t target) state() *state {
	return &state{Mode: t.mode, BinDir: t.binDir, ConfigFile: t.configFile, AppliedHash: t.hash, HashScheme: hashSchemeEntry}
}

func loadState(path string) (*state, error) {
	raw, err := readHostFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read installer state: %w", err)
	}
	s := &state{}
	if err := json.Unmarshal(raw, s); err != nil {
		// A corrupt state file must not brick the install — treat it
		// as absent (worst case: one redundant kubelet restart).
		logf("ignoring unparsable state file: %v", err)
		return nil, nil
	}
	if s.SchemaVersion != stateSchemaVersion {
		logf("ignoring state file with schemaVersion %d (want %d)", s.SchemaVersion, stateSchemaVersion)
		return nil, nil
	}
	return s, nil
}

func saveState(path string, s *state) error {
	s.SchemaVersion = stateSchemaVersion
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal installer state: %w", err)
	}
	if _, err := writeFileAtomic(path, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("write installer state: %w", err)
	}
	return nil
}

// matches reports whether the recorded state covers t (same mode, same
// paths, same restart-relevant content).
func (s *state) matches(t target) bool {
	return s != nil &&
		s.Mode == t.mode &&
		s.BinDir == t.binDir &&
		s.ConfigFile == t.configFile &&
		s.HashScheme == hashSchemeEntry &&
		s.AppliedHash == t.hash
}

// matchesLegacy reports whether a state file written before ADR-0029
// recorded a restart for exactly the files as they are now.
func (s *state) matchesLegacy(t target) bool {
	return s != nil &&
		s.Mode == t.mode &&
		s.BinDir == t.binDir &&
		s.ConfigFile == t.configFile &&
		s.HashScheme == "" &&
		s.AppliedHash == t.legacyHash
}

// contentHash hashes the restart-relevant byte slices in order.
func contentHash(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		// Length-prefix each part so concatenation ambiguity cannot
		// produce hash collisions between different part splits.
		// hash.Hash.Write never returns an error.
		_, _ = fmt.Fprintf(h, "%d:", len(p))
		_, _ = h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}
