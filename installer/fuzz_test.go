// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"testing"
)

// The installer parses files on the node as root. A pod can influence
// some of them (a fake kubelet's command line) and a broken node image
// others; parsing must never panic, and applying our own output again
// must change nothing (a second run would otherwise restart kubelet).

func FuzzMergeProvider(f *testing.F) {
	for _, seed := range []string{eksConfig, gkeConfig, aksConfig, renderedConfig, "", "{}", "apiVersion: x"} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, existing []byte) {
		if len(existing) > 1<<16 {
			return
		}
		entry, err := renderedProvider([]byte(renderedConfig), "harbor-bridge-plugin")
		if err != nil {
			t.Fatal(err)
		}
		out, _, err := mergeProvider(existing, entry)
		if err != nil {
			return
		}
		entry, err = renderedProvider([]byte(renderedConfig), "harbor-bridge-plugin")
		if err != nil {
			t.Fatal(err)
		}
		again, changed, err := mergeProvider(out, entry)
		if err != nil {
			t.Fatalf("merging into our own output failed: %v\n%s", err, out)
		}
		if changed || !bytes.Equal(out, again) {
			t.Fatalf("merge is not idempotent:\nfirst:\n%s\nsecond:\n%s", out, again)
		}
	})
}

func FuzzMergeExtraArgs(f *testing.F) {
	for _, seed := range []string{"", "KUBELET_EXTRA_ARGS=\n", "KUBELET_EXTRA_ARGS=--node-ip=10.0.0.1 --v=2\n", "# comment\nKUBELET_EXTRA_ARGS='--a --b'\n"} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, existing []byte) {
		if len(existing) > 1<<16 {
			return
		}
		out, err := mergeExtraArgs(existing, "/opt/harbor-bridge/bin", "/etc/kubernetes/credential-provider/config.yaml")
		if err != nil {
			return
		}
		again, err := mergeExtraArgs(out, "/opt/harbor-bridge/bin", "/etc/kubernetes/credential-provider/config.yaml")
		if err != nil {
			t.Fatalf("merging into our own output failed: %v\n%s", err, out)
		}
		if !bytes.Equal(out, again) {
			t.Fatalf("merge is not idempotent:\nfirst:\n%s\nsecond:\n%s", out, again)
		}
	})
}

func FuzzKubeletCmdline(f *testing.F) {
	f.Add([]byte("/usr/bin/kubelet\x00--image-credential-provider-config=/etc/c.yaml\x00--image-credential-provider-bin-dir\x00/opt/bin\x00"))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, raw []byte) {
		argv := splitCmdline(raw)
		_ = flagValue(argv, flagConfigFile)
		_ = flagValue(argv, flagBinDir)
	})
}
