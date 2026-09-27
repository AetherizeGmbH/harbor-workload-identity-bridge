// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Several installs of the chart (ADR-0029) run one installer each on every
// node, possibly at the same moment. They share kubelet's one
// credential-provider config, /etc/default/kubelet and the kubelet unit, so
// every read-modify-write of those files and every kubelet restart runs
// under an exclusive flock(2). The locks, always taken in this order:
//
//   - the node lock (nodeLockPath), a fixed node path that every install
//     finds whatever its chart values. Every mode that may touch kubelet
//     (auto, merge, patch) holds it for the whole pass: discovery, the
//     edits, the restart, its verification and the state file.
//   - config locks (configLockPath), <provider config>+lockSuffix next to
//     the file they guard. Every mode takes the lock of the config it edits
//     before it reads it. Patch mode, which may point kubelet away from the
//     config kubelet reads now, first takes that config's lock, then its
//     own (one lock when both are the same), and holds both from before it
//     checks the current config (checkRewire) until the pass ends
//     (lockPatchConfigs): none mode, whose pod mounts only the two plugin
//     directories, takes only its own config lock and cannot see the node
//     lock, and could otherwise add an entry to either config after patch
//     mode checked it.
//
// none mode holds one lock at a time and every other mode takes the node
// lock first, so no two installers wait for each other in a cycle.
//
// A flock belongs to the open file and dies with the process: a crashed
// installer never leaves a stale lock behind.

// nodeLockPath lives on /run (tmpfs): the lock needs no persistence.
const nodeLockPath = "/run/harbor-bridge-installer.lock"

const lockSuffix = ".lock"

// defaultLockTimeout bounds the wait for another installer. One pass holds
// the node lock for at most the kubelet restart plus its verification
// (defaultVerifyTiming), so this covers several installs queueing up.
const defaultLockTimeout = 10 * time.Minute

const lockPollInterval = 200 * time.Millisecond

// lockFile takes the exclusive flock on the lock file at path (a host path,
// created when missing) and returns the function that releases it. It
// polls instead of blocking so that it can time out with a clear error.
func lockFile(path string, timeout time.Duration) (unlock func(), err error) {
	f, err := openLockFile(path)
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	deadline := time.Now().Add(timeout)
	waiting := false
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("timed out after %s waiting for %s: another harbor-bridge installer on this node still holds it", timeout, path)
		}
		if !waiting {
			logf("waiting for %s, held by another harbor-bridge installer on this node", path)
			waiting = true
		}
		time.Sleep(lockPollInterval)
	}
	// Closing the file releases the flock.
	return func() { _ = f.Close() }, nil
}

// configLockPath returns the node path of the config lock that guards the
// credential-provider config at configPath (a node path): <file>.lock next
// to a file. For a directory of config files, which kubelet reads since
// Kubernetes 1.34, it is the lock a none-mode install whose
// plugin.hostConfigDir is that directory takes before it writes its
// chart-owned config there (chartOwnedPathIn). That layout is unsupported
// (every writer of plugin.hostConfigDir could add config files that kubelet
// loads), but an installer must not race one that uses it.
func (c *config) configLockPath(configPath string) string {
	return c.chartOwnedPathIn(configPath) + lockSuffix
}

// chartOwnedPathIn returns the node path of the chart-owned config a
// patch- or none-mode install keeps at kubelet's config path configPath:
// configPath itself for a file, and
// <directory>/credential-provider-config.yaml for a directory of config
// files, where a none-mode install whose plugin.hostConfigDir is that
// directory writes it.
func (c *config) chartOwnedPathIn(configPath string) string {
	if fi, err := os.Lstat(c.hostPath(configPath)); err == nil && fi.IsDir() {
		return filepath.Join(configPath, configFileName)
	}
	return configPath
}

// lockConfig takes the config lock at lockPath (a node path). Its
// directory is created when missing, as writeFileAtomic creates the
// directory of the config it writes: an installer must be able to lock a
// config before it exists.
func lockConfig(cfg *config, lockPath string) (unlock func(), err error) {
	if err := mkdirNodeDir(filepath.Dir(cfg.hostPath(lockPath))); err != nil {
		return nil, fmt.Errorf("lock %s: %w", lockPath, err)
	}
	return lockFile(cfg.hostPath(lockPath), cfg.lockTimeout)
}

// openLockFile creates the lock file when it is missing and opens it under
// the same rules as every other host file (files.go): relative to its
// directory, and only if the last component is a regular file, never a
// symlink, FIFO or device. A lock file is never replaced or removed.
// flock needs no write access, so the file is opened read-only. The
// directory must exist: /run on every systemd node, and the directory of
// the config file otherwise, which the caller has already created or
// discovered.
func openLockFile(path string) (*os.File, error) {
	dir, name := filepath.Dir(path), filepath.Base(path)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	// O_CREATE|O_EXCL never follows a symlink and fails on any existing
	// name, which openRegular then checks.
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	switch {
	case err == nil:
		if err := f.Close(); err != nil {
			return nil, err
		}
	case errors.Is(err, fs.ErrExist):
	default:
		return nil, err
	}
	return openRegular(root, name)
}
