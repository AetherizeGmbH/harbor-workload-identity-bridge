// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

const (
	testBinDir     = "/etc/kubernetes/credential-provider"
	testConfigFile = "/etc/kubernetes/credential-provider-config/credential-provider-config.yaml"
)

func TestMergeExtraArgs_EmptyFile(t *testing.T) {
	out, err := mergeExtraArgs(nil, testBinDir, testConfigFile)
	if err != nil {
		t.Fatalf("mergeExtraArgs: %v", err)
	}
	want := `KUBELET_EXTRA_ARGS="--image-credential-provider-bin-dir=` + testBinDir +
		` --image-credential-provider-config=` + testConfigFile + `"` + "\n"
	if string(out) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestMergeExtraArgs_PreservesForeignArgsAndLines(t *testing.T) {
	in := []byte(`# operator-managed
KUBELET_EXTRA_ARGS="--max-pods=42 --node-labels=team=a"
OTHER_VAR=keep-me
`)
	out, err := mergeExtraArgs(in, testBinDir, testConfigFile)
	if err != nil {
		t.Fatalf("mergeExtraArgs: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"# operator-managed\n",
		"OTHER_VAR=keep-me\n",
		"--max-pods=42",
		"--node-labels=team=a",
		"--image-credential-provider-bin-dir=" + testBinDir,
		"--image-credential-provider-config=" + testConfigFile,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("output missing %q:\n%s", want, s)
		}
	}
	if strings.Count(s, "KUBELET_EXTRA_ARGS=") != 1 {
		t.Fatalf("expected exactly one KUBELET_EXTRA_ARGS line:\n%s", s)
	}
}

func TestMergeExtraArgs_StripsStaleFlags(t *testing.T) {
	in := []byte(`KUBELET_EXTRA_ARGS="--image-credential-provider-bin-dir=/old --image-credential-provider-config /old/cfg.yaml --max-pods=42"` + "\n")
	out, err := mergeExtraArgs(in, testBinDir, testConfigFile)
	if err != nil {
		t.Fatalf("mergeExtraArgs: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "/old") {
		t.Fatalf("stale flag values not stripped:\n%s", s)
	}
	if !strings.Contains(s, "--max-pods=42") {
		t.Fatalf("foreign arg lost:\n%s", s)
	}
	if strings.Count(s, "--image-credential-provider-bin-dir") != 1 {
		t.Fatalf("expected exactly one bin-dir flag:\n%s", s)
	}
}

func TestMergeExtraArgs_Idempotent(t *testing.T) {
	first, err := mergeExtraArgs(nil, testBinDir, testConfigFile)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := mergeExtraArgs(first, testBinDir, testConfigFile)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("not idempotent:\n%s\nvs\n%s", first, second)
	}
}

func TestMergeExtraArgs_SingleQuotes(t *testing.T) {
	in := []byte("KUBELET_EXTRA_ARGS='--max-pods=42'\n")
	out, err := mergeExtraArgs(in, testBinDir, testConfigFile)
	if err != nil {
		t.Fatalf("mergeExtraArgs: %v", err)
	}
	if !strings.Contains(string(out), "--max-pods=42") {
		t.Fatalf("single-quoted value lost:\n%s", out)
	}
}

func TestMergeExtraArgs_MultipleLinesRefused(t *testing.T) {
	in := []byte("KUBELET_EXTRA_ARGS=a\nKUBELET_EXTRA_ARGS=b\n")
	if _, err := mergeExtraArgs(in, testBinDir, testConfigFile); err == nil {
		t.Fatal("expected refusal on multiple KUBELET_EXTRA_ARGS lines")
	}
}
