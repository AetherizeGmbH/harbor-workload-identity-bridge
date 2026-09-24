// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// maxHostFileSize bounds every read of a host file. The largest file the
// installer handles is the plugin binary (about 10 MiB).
const maxHostFileSize = 64 << 20

// Every host file is opened relative to its directory (os.Root), and its
// last path component must be a regular file, never a symlink. The sync
// container, and any pod with a hostPath volume on the same directory,
// can write next to the files this privileged installer reads and
// replaces. Before this check a planted symlink such as
// `harbor-bridge-ca.crt.bak -> ../../cron.d/x` turned the next install
// into a root write (or, for the read side, a copy of any host file into
// a world-readable backup) anywhere on the node.

// readHostFile returns the content of the regular file at path. A missing
// file returns an error that matches fs.ErrNotExist.
func readHostFile(path string) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	f, err := openRegular(root, filepath.Base(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readAllCapped(f)
}

// openRegular opens name in root for reading and returns an error unless
// it is a regular file within the size cap. os.Root blocks symlinks that
// leave the directory but follows ones that stay inside it, so the last
// component is checked with Lstat first, and the opened file must be the
// same file (no swap between the check and the open). O_NONBLOCK keeps a
// FIFO swapped in at that moment from blocking the open.
func openRegular(root *os.Root, name string) (*os.File, error) {
	lfi, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if lfi.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing to follow symlink %s in %s", name, root.Name())
	}
	if !lfi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s in %s is not a regular file (%s)", name, root.Name(), lfi.Mode().Type())
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !os.SameFile(lfi, fi) {
		_ = f.Close()
		return nil, fmt.Errorf("%s in %s changed while it was opened", name, root.Name())
	}
	if fi.Size() > maxHostFileSize {
		_ = f.Close()
		return nil, fmt.Errorf("%s in %s is larger than %d bytes", name, root.Name(), maxHostFileSize)
	}
	return f, nil
}

func readAllCapped(f *os.File) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(f, maxHostFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxHostFileSize {
		return nil, fmt.Errorf("%s is larger than %d bytes", f.Name(), maxHostFileSize)
	}
	return data, nil
}

// writeFileAtomic writes data to path via a same-directory temp file +
// rename so a reader (kubelet, containerd) never sees a torn write.
// When replacing different content it first preserves the old bytes at
// path+".bak". Returns whether the on-disk content changed. All
// operations are relative to the directory and never follow a symlink in
// the last component (see readHostFile); rename replaces a symlink
// instead of writing through it.
func writeFileAtomic(path string, data []byte, perm os.FileMode) (changed bool, err error) {
	dir, name := filepath.Dir(path), filepath.Base(path)
	// The parents are root-owned node directories (or the installer's
	// own mount points), outside what a pod on the node can write.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()

	f, err := openRegular(root, name)
	switch {
	case err == nil:
		old, rerr := readAllCapped(f)
		if rerr != nil {
			_ = f.Close()
			return false, fmt.Errorf("read %s: %w", path, rerr)
		}
		if bytes.Equal(old, data) {
			// Content is current; still enforce the mode (a prior
			// version may have written with different permissions).
			// fchmod on the open file, never chmod by name.
			cerr := f.Chmod(perm)
			_ = f.Close()
			if cerr != nil {
				return false, fmt.Errorf("chmod %s: %w", path, cerr)
			}
			return false, nil
		}
		_ = f.Close()
		if err := replaceFile(root, name+".bak", old, perm); err != nil {
			return false, fmt.Errorf("write backup %s.bak: %w", path, err)
		}
	case errors.Is(err, fs.ErrNotExist):
		// First write — no backup.
	default:
		return false, fmt.Errorf("read %s: %w", path, err)
	}

	if err := replaceFile(root, name, data, perm); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	return true, nil
}

// replaceFile atomically replaces name in root with data: a fresh
// O_EXCL temp file, fchmod, fsync, rename, then fsync of the directory.
// The syncs matter because these files decide whether kubelet starts:
// without them a power loss right after the install can leave an empty
// or truncated config that kubelet refuses at boot.
func replaceFile(root *os.Root, name string, data []byte, perm os.FileMode) error {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Errorf("random temp suffix: %w", err)
	}
	tmpName := name + ".tmp-" + hex.EncodeToString(suffix[:])
	tmp, err := root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer func() { _ = root.Remove(tmpName) }() // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := root.Rename(tmpName, name); err != nil {
		return fmt.Errorf("rename %s → %s: %w", tmpName, name, err)
	}
	d, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("open %s for sync: %w", root.Name(), err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", root.Name(), err)
	}
	return nil
}

// ensureWritableDir creates dir if needed and proves a file can be
// created in it.
func ensureWritableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	f, err := os.CreateTemp(dir, ".write-probe-*")
	if err != nil {
		return fmt.Errorf("not writable: %w", err)
	}
	name := f.Name()
	_ = f.Close()
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("remove write probe: %w", err)
	}
	return nil
}

// copyFile copies src to dst atomically, returning whether dst changed.
func copyFile(src, dst string, perm os.FileMode) (bool, error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", src, err)
	}
	return writeFileAtomic(dst, data, perm)
}
