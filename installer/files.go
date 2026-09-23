// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// writeFileAtomic writes data to path via a same-directory temp file +
// rename so a reader (kubelet, containerd) never sees a torn write.
// When replacing different content it first preserves the old bytes at
// path+".bak". Returns whether the on-disk content changed.
func writeFileAtomic(path string, data []byte, perm os.FileMode) (changed bool, err error) {
	old, err := os.ReadFile(path)
	switch {
	case err == nil:
		if bytes.Equal(old, data) {
			// Content is current; still enforce the mode (a prior
			// version may have written with different permissions).
			if err := os.Chmod(path, perm); err != nil {
				return false, fmt.Errorf("chmod %s: %w", path, err)
			}
			return false, nil
		}
		if err := os.WriteFile(path+".bak", old, perm); err != nil {
			return false, fmt.Errorf("write backup %s.bak: %w", path, err)
		}
	case errors.Is(err, fs.ErrNotExist):
		// First write — no backup.
	default:
		return false, fmt.Errorf("read %s: %w", path, err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return false, fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return false, fmt.Errorf("rename %s → %s: %w", tmpName, path, err)
	}
	return true, nil
}

// copyFile copies src to dst atomically, returning whether dst changed.
func copyFile(src, dst string, perm os.FileMode) (bool, error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", src, err)
	}
	return writeFileAtomic(dst, data, perm)
}
