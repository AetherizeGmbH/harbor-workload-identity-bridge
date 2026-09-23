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
		val := strings.TrimPrefix(trimmed, prefix)
		val = strings.Trim(val, `"'`)
		args := stripCredentialProviderFlags(strings.Fields(val))
		args = append(args, ours...)
		lines[i] = prefix + `"` + strings.Join(args, " ") + `"`
	}
	if !found {
		lines = append(lines, prefix+`"`+strings.Join(ours, " ")+`"`)
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
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
