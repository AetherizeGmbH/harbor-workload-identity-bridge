// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"reflect"
	"slices"
	"testing"

	"sigs.k8s.io/yaml"
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
		"KUBELET_EXTRA_ARGS=\xcb   0\n", "FOO=\uFFFE\n", "FOO=a\x00b\n", damagedBy94,
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
		switch len(before) {
		case 0:
		case 1:
			want = append(stripCredentialProviderFlags(before[0]), ours...)
		default:
			// Only a line an earlier installer appended goes
			// (appendedByEarlierInstaller): it held nothing but the two
			// flags, and the operator's assignment before it counts.
			if len(before) != 2 || len(stripCredentialProviderFlags(before[1])) != 0 {
				t.Fatalf("accepted %d assignments, the last %q:\ninput:\n%q\noutput:\n%q", len(before), before[len(before)-1], existing, out)
			}
			want = append(stripCredentialProviderFlags(before[0]), ours...)
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

// FuzzRemoveProvider: a removal drops exactly the bridge entries of its
// name from a cloud's config, keeps every other entry in order, and a
// second removal changes nothing (ADR-0039).
func FuzzRemoveProvider(f *testing.F) {
	ours, err := renderedProvider([]byte(renderedConfig), defaultProviderName)
	if err != nil {
		f.Fatal(err)
	}
	for _, seed := range []string{eksConfig, gkeConfig, aksConfig, renderedConfig, "", "{}", "apiVersion: x"} {
		f.Add([]byte(seed))
		if merged, _, err := mergeProvider([]byte(seed), ours); err == nil {
			f.Add(merged)
		}
	}
	providersOf := func(t *testing.T, doc []byte) []any {
		cfg := map[string]any{}
		if err := yaml.Unmarshal(doc, &cfg); err != nil {
			t.Fatalf("parse %q: %v", doc, err)
		}
		providers, err := providerList(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return providers
	}
	f.Fuzz(func(t *testing.T, existing []byte) {
		if len(existing) > 1<<16 {
			return
		}
		out, changed, remaining, err := removeProvider(existing, defaultProviderName)
		if err != nil {
			return
		}
		if !changed && !bytes.Equal(out, existing) {
			t.Fatalf("unchanged, but the bytes differ:\n%q\n%q", existing, out)
		}
		want := slices.DeleteFunc(providersOf(t, existing), func(p any) bool {
			pm, ok := p.(map[string]any)
			return ok && pm["name"] == defaultProviderName && isBridgeProvider(pm)
		})
		got := providersOf(t, out)
		if len(got) != remaining || !reflect.DeepEqual(got, want) {
			t.Fatalf("providers after the removal %v (remaining %d), want %v\ninput:\n%q", got, remaining, want, existing)
		}
		again, changed, _, err := removeProvider(out, defaultProviderName)
		if err != nil || changed || !bytes.Equal(again, out) {
			t.Fatalf("a second removal changed the file (%v):\n%q\n%q", err, out, again)
		}
	})
}

// FuzzRemoveExtraArgs: removing what mergeExtraArgs added gives kubelet
// the operator's arguments without the two flags, as systemd reads the
// file, keeps every other assignment, and a second removal changes
// nothing (ADR-0039).
func FuzzRemoveExtraArgs(f *testing.F) {
	for _, seed := range []string{
		"", "KUBELET_EXTRA_ARGS=\n", "KUBELET_EXTRA_ARGS=--node-ip=10.0.0.1 --v=2\n", "# comment\nKUBELET_EXTRA_ARGS='--a --b'\n",
		"KUBELET_EXTRA_ARGS = \"--max-pods=42\"\n", "FOO=1\nKUBELET_EXTRA_ARGS=--a\nBAR=2", damagedBy94,
	} {
		f.Add([]byte(seed))
	}
	const binDir, configFile = "/opt/harbor-bridge/bin", "/etc/kubernetes/credential-provider/config.yaml"
	extraOf := func(t *testing.T, doc []byte) (extra [][]string, others []envAssignment) {
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
	f.Fuzz(func(t *testing.T, existing []byte) {
		if len(existing) > 1<<16 {
			return
		}
		if out, err := removeExtraArgs(existing, binDir, configFile); err == nil {
			if again, err := removeExtraArgs(out, binDir, configFile); err != nil || !bytes.Equal(again, out) {
				t.Fatalf("a second removal changed the file (%v):\n%q\n%q", err, out, again)
			}
		}
		merged, err := mergeExtraArgs(existing, binDir, configFile)
		if err != nil {
			return
		}
		out, err := removeExtraArgs(merged, binDir, configFile)
		if err != nil {
			t.Fatalf("removing our own flags failed: %v\n%q", err, merged)
		}
		mergedExtra, othersBefore := extraOf(t, merged)
		after, othersAfter := extraOf(t, out)
		want := stripCredentialProviderFlags(mergedExtra[0])
		if len(after) != 1 || !slices.Equal(after[0], want) {
			t.Fatalf("systemd reads KUBELET_EXTRA_ARGS as %q, want exactly %q:\nmerged:\n%q\nremoved:\n%q", after, want, merged, out)
		}
		if !slices.Equal(othersBefore, othersAfter) {
			t.Fatalf("other assignments changed:\nbefore %+v\nafter  %+v", othersBefore, othersAfter)
		}
		if again, err := removeExtraArgs(out, binDir, configFile); err != nil || !bytes.Equal(again, out) {
			t.Fatalf("a second removal changed the file (%v):\n%q\n%q", err, out, again)
		}
	})
}
