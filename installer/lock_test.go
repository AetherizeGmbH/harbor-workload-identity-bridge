// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLockFile_ExcludesUntilReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := lockFile(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan error, 1)
	go func() {
		unlock2, err := lockFile(path, 10*time.Second)
		if err == nil {
			unlock2()
		}
		acquired <- err
	}()
	select {
	case err := <-acquired:
		t.Fatalf("second lock acquired while the first was held (err %v)", err)
	case <-time.After(3 * lockPollInterval):
	}
	unlock()
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("second lock after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second lock not acquired after release")
	}
	// Released locks can be taken again.
	unlock, err = lockFile(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestLockFile_TimesOutWithAClearError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := lockFile(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	_, err = lockFile(path, 2*lockPollInterval)
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "another harbor-bridge installer") {
		t.Fatalf("got %v, want a timeout naming the other installer", err)
	}
}

// TestLockFile_RefusesSymlink: a pod with a hostPath on the config
// directory must not turn the installer's lock file into a root-created
// file elsewhere (files.go rules).
func TestLockFile_RefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere")
	path := filepath.Join(dir, "x.lock")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := lockFile(path, time.Second); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("got %v, want a refusal of the symlink", err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("the symlink target was created (err %v)", err)
	}
}

func TestLockFile_RefusesDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := lockFile(path, time.Second); err == nil {
		t.Fatal("a directory in place of the lock file was accepted")
	}
}
