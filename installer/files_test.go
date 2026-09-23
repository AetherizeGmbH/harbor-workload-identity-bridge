// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileAtomic_CreateWriteBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "file.txt")

	changed, err := writeFileAtomic(path, []byte("one"), 0o644)
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	if !changed {
		t.Fatal("first write must report changed")
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Fatal("first write must not create a backup")
	}

	changed, err = writeFileAtomic(path, []byte("one"), 0o644)
	if err != nil {
		t.Fatalf("identical write: %v", err)
	}
	if changed {
		t.Fatal("identical write must report unchanged")
	}

	changed, err = writeFileAtomic(path, []byte("two"), 0o644)
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if !changed {
		t.Fatal("second write must report changed")
	}
	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(bak) != "one" {
		t.Fatalf("backup content = %q, want %q", bak, "one")
	}
	now, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(now) != "two" {
		t.Fatalf("content = %q, want %q", now, "two")
	}
}

func TestWriteFileAtomic_EnforcesMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	if _, err := writeFileAtomic(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Same content, different pre-existing mode: chmod must be enforced.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := writeFileAtomic(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %o, want 0600", perm)
	}
}

func TestWriteFileAtomic_NoTempLeftovers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if _, err := writeFileAtomic(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := copyFile(src, dst, 0o755)
	if err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	if !changed {
		t.Fatal("first copy must report changed")
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Fatalf("mode = %o, want 0755", perm)
	}
}
