// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"slices"
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

// FuzzMergeExtraArgs also reads input and output as systemd reads an
// EnvironmentFile (parseEnvFile): the output must assign KUBELET_EXTRA_ARGS
// exactly once, to the operator's args followed by ours, and keep every
// other assignment. Idempotency alone holds for an output that appends a
// second assignment, which is how operator args got lost.
func FuzzMergeExtraArgs(f *testing.F) {
	for _, seed := range []string{
		"", "KUBELET_EXTRA_ARGS=\n", "KUBELET_EXTRA_ARGS=--node-ip=10.0.0.1 --v=2\n", "# comment\nKUBELET_EXTRA_ARGS='--a --b'\n",
		"KUBELET_EXTRA_ARGS = \"--max-pods=42\"\n", "KUBELET_EXTRA_ARGS\t=--a\nKUBELET_EXTRA_ARGS=--b\n",
		"FOO=\"x\nKUBELET_EXTRA_ARGS=--a\n\"\n", "export KUBELET_EXTRA_ARGS=--a\r\n", "# c \\\nKUBELET_EXTRA_ARGS=--a\n",
	} {
		f.Add([]byte(seed))
	}
	const binDir, configFile = "/opt/harbor-bridge/bin", "/etc/kubernetes/credential-provider/config.yaml"
	ours := []string{flagBinDir + "=" + binDir, flagConfigFile + "=" + configFile}
	f.Fuzz(func(t *testing.T, existing []byte) {
		if len(existing) > 1<<16 {
			return
		}
		out, err := mergeExtraArgs(existing, binDir, configFile)
		if err != nil {
			return
		}
		again, err := mergeExtraArgs(out, binDir, configFile)
		if err != nil {
			t.Fatalf("merging into our own output failed: %v\n%s", err, out)
		}
		if !bytes.Equal(out, again) {
			t.Fatalf("merge is not idempotent:\nfirst:\n%s\nsecond:\n%s", out, again)
		}
		split := func(doc []byte) (extra [][]string, others []envAssignment) {
			assignments, err := parseEnvFile(string(doc))
			if err != nil {
				t.Fatalf("parse %q: %v", doc, err)
			}
			for _, a := range assignments {
				if a.key == extraArgsKey {
					extra = append(extra, splitArgs(a.value))
				} else {
					others = append(others, envAssignment{key: a.key, value: a.value})
				}
			}
			return extra, others
		}
		before, othersBefore := split(existing)
		after, othersAfter := split(out)
		want := slices.Clone(ours)
		if len(before) > 0 {
			want = append(stripCredentialProviderFlags(before[len(before)-1]), ours...)
		}
		if len(after) != 1 || !slices.Equal(after[0], want) {
			t.Fatalf("systemd reads KUBELET_EXTRA_ARGS as %q, want exactly %q:\ninput:\n%q\noutput:\n%q", after, want, existing, out)
		}
		if !slices.Equal(othersBefore, othersAfter) {
			t.Fatalf("other assignments changed:\nbefore %+v\nafter  %+v", othersBefore, othersAfter)
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
