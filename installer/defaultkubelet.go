// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"
)

// mergeExtraArgs rewrites the KUBELET_EXTRA_ARGS line of an
// /etc/default/kubelet file: existing args are preserved, stale
// --image-credential-provider-* occurrences are stripped, and ours are
// appended. Every other line round-trips verbatim. The pre-ADR-0021
// script overwrote the whole file, clobbering operator-set args.
func mergeExtraArgs(existing []byte, binDir, configFile string) ([]byte, error) {
	const prefix = "KUBELET_EXTRA_ARGS="

	ours := []string{
		flagBinDir + "=" + binDir,
		flagConfigFile + "=" + configFile,
	}

	lines := strings.Split(string(existing), "\n")
	// Drop a single trailing empty segment so we can re-join with a
	// guaranteed final newline.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}

	found := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, prefix) {
			continue
		}
		if found {
			return nil, fmt.Errorf("multiple KUBELET_EXTRA_ARGS lines in /etc/default/kubelet — refusing to guess which one kubelet uses")
		}
		found = true
		val, err := unquoteExtraArgs(strings.TrimPrefix(trimmed, prefix))
		if err != nil {
			return nil, err
		}
		args := stripCredentialProviderFlags(strings.Fields(val))
		args = append(args, ours...)
		lines[i] = prefix + `"` + strings.Join(args, " ") + `"`
	}
	if !found {
		lines = append(lines, prefix+`"`+strings.Join(ours, " ")+`"`)
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

// unquoteExtraArgs strips exactly one matching pair of outer quotes. The
// value is an EnvironmentFile assignment that the kubelet unit expands as
// unquoted $KUBELET_EXTRA_ARGS (whitespace-split into argv). The installer
// re-renders it as a double-quoted, space-joined list, which is only
// faithful when no argument carries quotes, escapes, or embedded spaces of
// its own. Anything else is refused rather than rewritten: a mangled line
// can keep kubelet from starting after the restart (audit M5). The old
// strings.Trim stripped ANY number of quote characters from both ends,
// unbalancing a value like --register-with-taints="x:NoSchedule".
func unquoteExtraArgs(val string) (string, error) {
	if n := len(val); n >= 2 && (val[0] == '"' || val[0] == '\'') && val[n-1] == val[0] {
		val = val[1 : n-1]
	}
	if strings.ContainsAny(val, "\"'\\$`") {
		return "", fmt.Errorf("KUBELET_EXTRA_ARGS in /etc/default/kubelet contains quoting, escapes, or expansions (%q) the installer cannot rewrite safely; add the two --image-credential-provider-* flags yourself and use plugin.install.mode=none", val)
	}
	return val, nil
}

// stripCredentialProviderFlags removes both the "--flag=value" and
// "--flag value" forms of the two credential-provider flags.
func stripCredentialProviderFlags(args []string) []string {
	out := make([]string, 0, len(args))
	skipNext := false
	for _, arg := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if arg == flagBinDir || arg == flagConfigFile {
			skipNext = true
			continue
		}
		if strings.HasPrefix(arg, flagBinDir+"=") || strings.HasPrefix(arg, flagConfigFile+"=") {
			continue
		}
		out = append(out, arg)
	}
	return out
}
