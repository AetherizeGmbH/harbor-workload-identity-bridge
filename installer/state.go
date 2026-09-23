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
	"os"
	"path/filepath"
)

// state records what the installer last successfully applied AND
// restarted kubelet for. It exists to survive the crash window between
// "files written" and "kubelet restarted": after such a crash the
// on-disk files already match the desired content, so a byte compare
// alone would skip the still-needed restart. The state hash is only
// persisted after a successful restart. CA/mTLS content is
// deliberately not part of the hash — the plugin reads those files on
// every exec, so their rotation never needs a restart (ADR-0021).
type state struct {
	SchemaVersion int    `json:"schemaVersion"`
	Mode          string `json:"mode"`
	BinDir        string `json:"binDir"`
	ConfigFile    string `json:"configFile"`
	// AppliedHash covers the restart-relevant content: the effective
	// credential-provider config bytes plus, in patch mode, the
	// /etc/default/kubelet bytes.
	AppliedHash string `json:"appliedHash"`
}

const stateSchemaVersion = 1

const stateFileName = "installer-state.json"

func loadState(stateDir string) (*state, error) {
	raw, err := os.ReadFile(filepath.Join(stateDir, stateFileName))
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

func saveState(stateDir string, s *state) error {
	s.SchemaVersion = stateSchemaVersion
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal installer state: %w", err)
	}
	if _, err := writeFileAtomic(filepath.Join(stateDir, stateFileName), append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("write installer state: %w", err)
	}
	return nil
}

// matches reports whether the recorded state covers the given desired
// deployment (same mode, same paths, same restart-relevant content).
func (s *state) matches(mode, binDir, configFile, hash string) bool {
	return s != nil &&
		s.Mode == mode &&
		s.BinDir == binDir &&
		s.ConfigFile == configFile &&
		s.AppliedHash == hash
}

// contentHash hashes the restart-relevant byte slices in order.
func contentHash(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		// Length-prefix each part so concatenation ambiguity cannot
		// produce hash collisions between different part splits.
		fmt.Fprintf(h, "%d:", len(p))
		h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}
