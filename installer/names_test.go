// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// TestFilesFor_DefaultKeepsPreADR0029Names pins the node paths of every
// existing install: an upgrade must neither move nor orphan a file.
func TestFilesFor_DefaultKeepsPreADR0029Names(t *testing.T) {
	want := nodeFiles{
		Binary:     "harbor-bridge-plugin",
		CA:         "harbor-bridge-ca.crt",
		ClientCert: "harbor-bridge-client.crt",
		ClientKey:  "harbor-bridge-client.key",
		State:      "installer-state.json",
	}
	if got := filesFor(defaultProviderName); got != want {
		t.Fatalf("filesFor(default) = %+v, want %+v", got, want)
	}
}

func TestFilesFor_OtherNamesDeriveTheirFiles(t *testing.T) {
	want := nodeFiles{
		Binary:     "harbor-bridge-eu",
		CA:         "harbor-bridge-eu.ca.crt",
		ClientCert: "harbor-bridge-eu.client.crt",
		ClientKey:  "harbor-bridge-eu.client.key",
		State:      "harbor-bridge-eu.installer-state.json",
	}
	if got := filesFor("harbor-bridge-eu"); got != want {
		t.Fatalf("filesFor(harbor-bridge-eu) = %+v, want %+v", got, want)
	}
	// "harbor-bridge" + "-ca.crt" would be the default install's CA file;
	// the dot-delimited scheme keeps them apart.
	if got := filesFor("harbor-bridge"); got.CA == filesFor(defaultProviderName).CA || got.ClientCert == filesFor(defaultProviderName).ClientCert {
		t.Fatalf("name harbor-bridge collides with the default install: %+v", got)
	}
}

func TestValidProviderName(t *testing.T) {
	for _, ok := range []string{defaultProviderName, "a", "harbor-bridge-eu", "0x", strings.Repeat("a", 63)} {
		if err := validProviderName(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Harbor", "a.b", "a/b", "a b", ".", "..", "-a", "a-", "a_b", strings.Repeat("a", 64)} {
		if err := validProviderName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// installFiles lists every file name one install creates in a directory it
// shares with other installs: its own files and their .bak copies
// (writeFileAtomic).
func installFiles(name string) []string {
	f := filesFor(name)
	var out []string
	for _, n := range []string{f.Binary, f.CA, f.ClientCert, f.ClientKey, f.State} {
		out = append(out, n, n+".bak")
	}
	return out
}

// FuzzNodeFiles_Injective: two different valid provider names never share
// a node file, and no install's file is one of the shared files.
func FuzzNodeFiles_Injective(f *testing.F) {
	f.Add(defaultProviderName, "harbor-bridge")
	f.Add("harbor-bridge-eu", "harbor-bridge-eu-ca")
	f.Add("a", "a-client")
	f.Add("credential-provider-config", "installer-state")
	f.Fuzz(func(t *testing.T, a, b string) {
		if a == b || validProviderName(a) != nil || validProviderName(b) != nil {
			return
		}
		shared := map[string]bool{
			configFileName: true, configFileName + lockSuffix: true, configFileName + ".bak": true,
		}
		seen := map[string]string{}
		for _, name := range []string{a, b} {
			for _, file := range installFiles(name) {
				if shared[file] {
					t.Fatalf("provider name %q owns the shared file %q", name, file)
				}
				if other, ok := seen[file]; ok && other != name {
					t.Fatalf("provider names %q and %q share the file %q", other, name, file)
				}
				seen[file] = name
			}
		}
	})
}
