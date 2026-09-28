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
	"time"
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
	// AppliedHash covers the whole effective credential-provider config
	// file plus, in patch mode, the /etc/default/kubelet bytes, as of the
	// last kubelet restart this installer verified or converted. It keeps
	// the meaning it had before ADR-0029, when it was the only hash: an
	// older installer (a rollback) compares exactly this field with its own
	// whole-file hash, so for a single, unchanged install it finds its own
	// record and does not restart kubelet.
	AppliedHash string `json:"appliedHash"`
	// EntryHash covers only this install's provider entry plus, in patch
	// mode, the /etc/default/kubelet bytes (ADR-0029). It decides about
	// restarts: other installs' entries in the shared file are their own
	// installers' concern, so a change there does not make this installer
	// restart kubelet again. Empty in a record written before ADR-0029, or
	// by an older installer after a rollback (it drops fields it does not
	// know).
	EntryHash string `json:"entryHash,omitempty"`
	// Rejected records the content of the last pass whose kubelet restart
	// did not verify (ADR-0033): a later pass with exactly this content
	// refuses instead of restarting kubelet onto it again. A verified
	// restart writes a record without it. The other fields keep describing
	// the last verified restart.
	Rejected *rejection `json:"rejected,omitempty"`
}

// rejection is content kubelet did not come up healthy with after this
// installer restarted it (state.Rejected).
type rejection struct {
	Mode       string `json:"mode"`
	BinDir     string `json:"binDir"`
	ConfigFile string `json:"configFile"`
	// Unit is the kubelet unit the pass restarted: a pass that restarted
	// the wrong unit did not test the content.
	Unit        string `json:"unit"`
	EntryHash   string `json:"entryHash"`
	AppliedHash string `json:"appliedHash"`
	// Reason is the error the pass failed with, At when (RFC 3339, UTC).
	Reason string `json:"reason"`
	At     string `json:"at"`
}

const stateSchemaVersion = 1

// target is what one install pass wants kubelet to run with.
type target struct {
	mode, binDir, configFile string
	// entryHash is the EntryHash of the desired content.
	entryHash string
	// fileHash is the AppliedHash of the desired content: the hash over
	// the whole resulting file, as installers before ADR-0029 compute it.
	fileHash string
}

// state returns the record of a successful restart for t.
func (t target) state() *state {
	return &state{Mode: t.mode, BinDir: t.binDir, ConfigFile: t.configFile, AppliedHash: t.fileHash, EntryHash: t.entryHash}
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

// rejection returns the record (state.Rejected) that marks t, restarted
// through unit, as content kubelet rejected at at, with the error reason.
// Every field of t counts: a changed entry, a changed shared file or
// /etc/default/kubelet, other paths or another unit are new content, which
// a pass may try again.
func (t target) rejection(unit, reason string, at time.Time) *rejection {
	return &rejection{
		Mode: t.mode, BinDir: t.binDir, ConfigFile: t.configFile, Unit: unit,
		EntryHash: t.entryHash, AppliedHash: t.fileHash,
		Reason: reason, At: at.UTC().Format(time.RFC3339),
	}
}

// rejects returns the rejection recorded for t restarted through unit, or
// nil when the state records none for exactly that content.
func (s *state) rejects(t target, unit string) *rejection {
	if s == nil || s.Rejected == nil {
		return nil
	}
	r := s.Rejected
	if r.Mode == t.mode && r.BinDir == t.binDir && r.ConfigFile == t.configFile && r.Unit == unit &&
		r.EntryHash == t.entryHash && r.AppliedHash == t.fileHash {
		return r
	}
	return nil
}

// matches reports whether the recorded state covers t (same mode, same
// paths, same entry content).
func (s *state) matches(t target) bool {
	return s != nil &&
		s.Mode == t.mode &&
		s.BinDir == t.binDir &&
		s.ConfigFile == t.configFile &&
		s.EntryHash != "" &&
		s.EntryHash == t.entryHash
}

// matchesLegacy reports whether a record without an entry hash (written
// before ADR-0029, or by an older installer after a rollback) records a
// restart for exactly the files as they are now.
func (s *state) matchesLegacy(t target) bool {
	return s != nil &&
		s.Mode == t.mode &&
		s.BinDir == t.binDir &&
		s.ConfigFile == t.configFile &&
		s.EntryHash == "" &&
		s.AppliedHash == t.fileHash
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
