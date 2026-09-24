// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

// A pod that can write next to the installer's files (the sync container,
// a hostPath volume) must not redirect a root write or read through a
// symlink. These reproduce the attacks from the 2026-09 audit.

func TestWriteFileAtomic_BackupSymlinkIsReplacedNotFollowed(t *testing.T) {
	node := t.TempDir()
	confDir := filepath.Join(node, "etc", "kubernetes", "credential-provider")
	cronDir := filepath.Join(node, "etc", "cron.d")
	for _, d := range []string{confDir, cronDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(confDir, "harbor-bridge-ca.crt")
	if err := os.WriteFile(path, []byte("* * * * * root /tmp/pwn\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../cron.d/pwn", path+".bak"); err != nil {
		t.Fatal(err)
	}

	if _, err := writeFileAtomic(path, []byte("real CA"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(cronDir, "pwn")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the backup was written through the symlink into cron.d (lstat err = %v)", err)
	}
	fi, err := os.Lstat(path + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsRegular() {
		t.Fatalf(".bak is %s, want a regular file that replaced the symlink", fi.Mode().Type())
	}
}

func TestWriteFileAtomic_RefusesSymlinkedTarget(t *testing.T) {
	node := t.TempDir()
	confDir := filepath.Join(node, "etc", "kubernetes", "credential-provider")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(node, "etc", "shadow")
	if err := os.WriteFile(secret, []byte("root:$6$secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(confDir, "harbor-bridge-ca.crt")
	if err := os.Symlink("../../shadow", path); err != nil {
		t.Fatal(err)
	}

	_, err := writeFileAtomic(path, []byte("real CA"), 0o644)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err = %v, want a refusal to follow the symlink", err)
	}
	if _, err := os.Lstat(path + ".bak"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a backup was created (it would hold the symlink target's content): lstat err = %v", err)
	}
	got, err := os.ReadFile(secret)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "root:$6$secret" {
		t.Fatalf("symlink target changed to %q", got)
	}
}

func TestWriteFileAtomic_RefusesFIFO(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "harbor-bridge-ca.crt")
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := writeFileAtomic(path, []byte("real CA"), 0o644)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v, want a not-a-regular-file refusal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writeFileAtomic blocked on a FIFO")
	}
}

func TestReadHostFile_RefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink("target", link); err != nil {
		t.Fatal(err)
	}
	if _, err := readHostFile(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err = %v, want a refusal to follow the symlink", err)
	}
	if _, err := readHostFile(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: err = %v, want fs.ErrNotExist", err)
	}
}
