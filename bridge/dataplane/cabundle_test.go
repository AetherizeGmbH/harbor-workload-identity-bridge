// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"os"
	"path/filepath"
	"testing"
)

// A client-CA file caught empty or half-written mid-rotation must not
// replace the pool in use (ADR-0025): every mTLS handshake would fail.
func TestCABundle_ReloadKeepsTheLastGoodPool(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.crt")
	write := func(b []byte) {
		t.Helper()
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(newTestCA(t, "first").pem)
	c := &caBundle{path: path}
	if err := c.reload(); err != nil {
		t.Fatal(err)
	}
	good := c.pool.Load()
	if good == nil {
		t.Fatal("no pool after the first load")
	}

	for _, tc := range []struct {
		name    string
		content []byte
	}{
		{"empty", nil},
		{"not PEM", []byte("-----BEGIN CERTIF")},
	} {
		write(tc.content)
		if err := c.reload(); err == nil {
			t.Errorf("%s: reload succeeded", tc.name)
		}
		if c.pool.Load() != good {
			t.Fatalf("%s: the pool was replaced", tc.name)
		}
	}

	write(newTestCA(t, "second").pem)
	if err := c.reload(); err != nil {
		t.Fatal(err)
	}
	if c.pool.Load() == good {
		t.Fatal("a valid new bundle was not picked up")
	}
}
