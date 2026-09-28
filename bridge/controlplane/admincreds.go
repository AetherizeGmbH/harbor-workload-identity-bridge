// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/go-logr/logr"
)

// kubeletDataDir is the symlink through which kubelet swaps the contents
// of a Secret volume atomically: <dir>/..data points at a timestamped
// directory holding the files, and each key is a symlink through ..data.
const kubeletDataDir = "..data"

// adminCredsReadAttempts bounds the retries when kubelet swaps the volume
// while it is being read. kubelet swaps once per Secret update, so a
// second attempt practically always succeeds.
const adminCredsReadAttempts = 5

// readAdminCredsDir reads username and password from one generation of the
// Secret volume at dir. Reading the two key symlinks one after the other
// could straddle a swap and pair the old username with the new password,
// so ..data is resolved once and both files are read below its target. If
// ..data cannot be resolved, or kubelet removed its target in between, the
// read is retried. Only a directory without ..data (a plain directory,
// local development) is read as is.
func readAdminCredsDir(dir string) (*AdminCreds, error) {
	var err error
	for range adminCredsReadAttempts {
		var base, generation string
		if base, generation, err = resolveDataDir(dir); err != nil {
			continue
		}
		var creds *AdminCreds
		if creds, err = readAdminCredsFrom(base); err == nil {
			return creds, nil
		}
		if _, now, resolveErr := resolveDataDir(dir); resolveErr == nil && now == generation {
			break // not a swap: report the error
		}
	}
	return nil, err
}

// resolveDataDir returns the directory holding the current files of the
// volume at dir and the ..data target it resolved; generation is "" when
// dir has no ..data. Any other failure to resolve ..data is an error, never
// a reason to read the key symlinks instead: they lead through ..data one
// after the other and could pair two versions.
func resolveDataDir(dir string) (base, generation string, err error) {
	link := filepath.Join(dir, kubeletDataDir)
	target, err := os.Readlink(link)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		return dir, "", nil
	default:
		return "", "", fmt.Errorf("resolve %s: %w", link, err)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	return target, target, nil
}

func readAdminCredsFrom(dir string) (*AdminCreds, error) {
	username, err := readSecretFile(filepath.Join(dir, adminUsernameKey))
	if err != nil {
		return nil, err
	}
	password, err := readSecretFile(filepath.Join(dir, adminPasswordKey))
	if err != nil {
		return nil, err
	}
	return &AdminCreds{Username: username, Password: password}, nil
}

// AdminCredsReader reads the Harbor admin credentials from HarborAdminDir
// on every call. The chart mounts the operator's Secret as a volume, which
// kubelet updates in place, so a rotated admin password or system-robot
// secret takes effect on the next Harbor call without a restart. Harbor
// calls come from reconciles and janitor sweeps only, so reading two small
// files each time costs nothing worth caching. A failed read fails the
// call; the previous credentials are never reused silently.
type AdminCredsReader struct {
	// dir returns the directory to read, evaluated on every call.
	dir     func() string
	backend string
	log     logr.Logger

	mu   sync.Mutex
	last AdminCreds
}

// NewAdminCredsReader returns a reader for cfg.HarborAdminDir that logs,
// without the values, when the credentials on disk change.
func NewAdminCredsReader(cfg *Config, log logr.Logger) *AdminCredsReader {
	return &AdminCredsReader{dir: func() string { return cfg.HarborAdminDir }, backend: "Harbor", log: log}
}

// NewNexusAdminCredsReader returns a reader for n.AdminDir, the Nexus
// admin credentials (ADR-0036), with the same behaviour.
func NewNexusAdminCredsReader(n *NexusConfig, log logr.Logger) *AdminCredsReader {
	return &AdminCredsReader{dir: func() string { return n.AdminDir }, backend: "Nexus", log: log}
}

// Read returns the current credentials. Its signature matches
// harbor.CredentialSource and nexus.CredentialSource.
func (r *AdminCredsReader) Read() (username, password string, err error) {
	dir := r.dir()
	creds, err := readAdminCredsDir(dir)
	if err != nil {
		return "", "", err
	}
	r.mu.Lock()
	changed := r.last != (AdminCreds{}) && r.last != *creds
	r.last = *creds
	r.mu.Unlock()
	if changed {
		r.log.Info(r.backend+" admin credentials changed on disk; using the new ones", "dir", dir)
	}
	return creds.Username, creds.Password, nil
}
