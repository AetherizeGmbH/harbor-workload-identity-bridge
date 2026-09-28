// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"slices"
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

// TestMergeExtraArgs_QuoteHandling pins audit M5: exactly one matching
// outer quote pair is stripped, and values the installer cannot rewrite
// faithfully are refused instead of mangled (a mangled line can keep
// kubelet from starting after the restart).
func TestMergeExtraArgs_QuoteHandling(t *testing.T) {
	ok := map[string]string{
		`KUBELET_EXTRA_ARGS="--max-pods=42"`: "--max-pods=42",
		`KUBELET_EXTRA_ARGS='--max-pods=42'`: "--max-pods=42",
		`KUBELET_EXTRA_ARGS=--max-pods=42`:   "--max-pods=42",
		`KUBELET_EXTRA_ARGS=`:                "",
	}
	for in, keep := range ok {
		out, err := mergeExtraArgs([]byte(in+"\n"), "/b", "/c.yaml")
		if err != nil {
			t.Errorf("%s: unexpected error %v", in, err)
			continue
		}
		if keep != "" && !strings.Contains(string(out), keep) {
			t.Errorf("%s: lost %q:\n%s", in, keep, out)
		}
		if strings.Count(string(out), `"`) != 2 {
			t.Errorf("%s: output is not one balanced double-quoted value:\n%s", in, out)
		}
	}
	for _, in := range []string{
		`KUBELET_EXTRA_ARGS="--register-with-taints="x:NoSchedule""`, // what strings.Trim used to unbalance
		`KUBELET_EXTRA_ARGS="--node-labels='a=b'"`,
		`KUBELET_EXTRA_ARGS="--root-dir=\"/var/lib/k\""`,
		`KUBELET_EXTRA_ARGS="--max-pods=$PODS"`,
		`KUBELET_EXTRA_ARGS="--x=1'`, // mismatched outer quotes
	} {
		if _, err := mergeExtraArgs([]byte(in+"\n"), "/b", "/c.yaml"); err == nil {
			t.Errorf("%s: accepted, want a refusal", in)
		}
	}
}

// extraArgsAssignments returns the values systemd assigns to
// KUBELET_EXTRA_ARGS when it reads env as an EnvironmentFile, in order.
func extraArgsAssignments(t *testing.T, env []byte) []string {
	t.Helper()
	assignments, err := parseEnvFile(string(env))
	if err != nil {
		t.Fatalf("parse %q: %v", env, err)
	}
	var values []string
	for _, a := range assignments {
		if a.key == extraArgsKey {
			values = append(values, a.value)
		}
	}
	return values
}

// TestMergeExtraArgs_SystemdKeyForms: systemd reads an assignment with
// whitespace around the "=" or before the value as KUBELET_EXTRA_ARGS too.
// The installer used to match only the literal "KUBELET_EXTRA_ARGS=" and
// append a second assignment, which systemd prefers: the operator's args
// silently left kubelet's command line.
func TestMergeExtraArgs_SystemdKeyForms(t *testing.T) {
	for _, line := range []string{
		`KUBELET_EXTRA_ARGS = "--max-pods=42"`,
		`KUBELET_EXTRA_ARGS ="--max-pods=42"`,
		"KUBELET_EXTRA_ARGS\t=--max-pods=42",
		`KUBELET_EXTRA_ARGS= "--max-pods=42"`,
		`  KUBELET_EXTRA_ARGS = '--max-pods=42'  `,
		"KUBELET_EXTRA_ARGS=\"--max-pods=42\"\r",
	} {
		in := "# node\n" + line + "\nOTHER=1\n"
		out, err := mergeExtraArgs([]byte(in), "/b", "/c.yaml")
		if err != nil {
			t.Errorf("%q: %v", line, err)
			continue
		}
		want := []string{"--max-pods=42 " + flagBinDir + "=/b " + flagConfigFile + "=/c.yaml"}
		if got := extraArgsAssignments(t, out); !slices.Equal(got, want) {
			t.Errorf("%q: systemd assigns KUBELET_EXTRA_ARGS %q, want %q:\n%s", line, got, want, out)
		}
		if !strings.HasPrefix(string(out), "# node\n") || !strings.Contains(string(out), "\nOTHER=1\n") {
			t.Errorf("%q: other lines changed:\n%s", line, out)
		}
	}
}

func TestMergeExtraArgs_SystemdSemanticsRefusals(t *testing.T) {
	for name, in := range map[string]string{
		"spaced duplicate":             "KUBELET_EXTRA_ARGS=--a\nKUBELET_EXTRA_ARGS = --b\n",
		"value over two lines":         "KUBELET_EXTRA_ARGS=\"--a\n--b\"\n",
		"escaped line break":           "KUBELET_EXTRA_ARGS=--a \\\n--b\n",
		"carriage return inside":       "FOO=1\rKUBELET_EXTRA_ARGS=--a\n",
		"after a continued comment":    "# old systemd continues this \\\nKUBELET_EXTRA_ARGS=--a\n",
		"append after open quote":      "FOO=\"unterminated\n",
		"append after escape":          "FOO=bar\\",
		"append after continued comm.": "# note \\\n",
	} {
		if out, err := mergeExtraArgs([]byte(in), "/b", "/c.yaml"); err == nil {
			t.Errorf("%s: accepted:\n%s", name, out)
		}
	}
}

// TestMergeExtraArgs_NotAnAssignment: lines systemd does not read as a
// KUBELET_EXTRA_ARGS assignment stay as they are, and the installer adds
// one of its own.
func TestMergeExtraArgs_NotAnAssignment(t *testing.T) {
	for name, in := range map[string]string{
		// systemd keeps "export" in the key, which is then not a variable name.
		"export":            "export KUBELET_EXTRA_ARGS=--max-pods=42\n",
		"semicolon comment": "; KUBELET_EXTRA_ARGS=--max-pods=42\n",
		"hash comment":      "  # KUBELET_EXTRA_ARGS=--max-pods=42\n",
		"inside a value":    "FOO=\"x\nKUBELET_EXTRA_ARGS=--max-pods=42\n\"\n",
		"longer key":        "KUBELET_EXTRA_ARGSX=--max-pods=42\n",
		"no equals sign":    "KUBELET_EXTRA_ARGS --max-pods=42\n",
	} {
		out, err := mergeExtraArgs([]byte(in), "/b", "/c.yaml")
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !strings.HasPrefix(string(out), in) {
			t.Errorf("%s: the existing lines changed:\n%s", name, out)
		}
		want := []string{flagBinDir + "=/b " + flagConfigFile + "=/c.yaml"}
		if got := extraArgsAssignments(t, out); !slices.Equal(got, want) {
			t.Errorf("%s: systemd assigns KUBELET_EXTRA_ARGS %q, want %q", name, got, want)
		}
	}
}

// TestParseEnvFile pins the port of systemd's parser against the cases of
// systemd's own src/test/test-env-file.c that matter here.
func TestParseEnvFile(t *testing.T) {
	got, err := parseEnvFile("one=BAR\n  two   =   bar    \n# three=x\n; four=y\nfive=\"a b\" c\nsix='x\ny'\nseven=a\\\nb\nten=ignored\nten=\nexport nine=x\neleven")
	if err != nil {
		t.Fatal(err)
	}
	type kv struct {
		key, value  string
		first, last int
	}
	want := []kv{
		{"one", "BAR", 0, 0},
		{"two", "bar", 1, 1},
		{"five", "a bc", 4, 4},
		{"six", "x\ny", 5, 6},
		{"seven", "ab", 7, 8},
		{"ten", "ignored", 9, 9},
		{"ten", "", 10, 10},
		{"export nine", "x", 11, 11},
	}
	var have []kv
	for _, a := range got {
		have = append(have, kv{a.key, a.value, a.first, a.last})
	}
	if !slices.Equal(have, want) {
		t.Fatalf("got  %+v\nwant %+v", have, want)
	}
	if got, err := parseEnvFile("a=\"x\\\"y\\\\z\\q\""); err != nil || len(got) != 1 || got[0].value != `x"y\z\q` {
		t.Fatalf("double-quote escapes: %+v, %v", got, err)
	}
}
