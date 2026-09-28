// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// kubeletEnvFiles are the files patch mode may edit to put its flags into
// KUBELET_EXTRA_ARGS (ADR-0034): the operator file of kubeadm's Debian
// packages and that of its RPM packages (kubernetes/release,
// cmd/krel/templates/latest/kubeadm/10-kubeadm.conf and kubeadm.spec).
// Never another file a unit reads: kubeadm regenerates
// /var/lib/kubelet/kubeadm-flags.env on join and upgrade.
var kubeletEnvFiles = []string{defaultKubeletPath, "/etc/sysconfig/kubelet"}

// setsExtraArgs matches an Environment= property that assigns
// KUBELET_EXTRA_ARGS.
var setsExtraArgs = regexp.MustCompile(`(^|[\s"'])` + extraArgsKey + `=`)

// kubeletEnvFile returns the node path of the environment file patch mode
// puts its flags into (ADR-0034): the one KUBELET_EXTRA_ARGS of the kubelet
// unit comes from. systemd reads the unit's EnvironmentFile= files in
// order and a later assignment wins, so that is the last listed file that
// assigns it, read as systemd reads it (parseEnvFile); when none does, the
// last listed of kubeletEnvFiles. It refuses, before the pass writes
// anything, a unit that lists neither of kubeletEnvFiles, an assignment
// that comes from another file, and a KUBELET_EXTRA_ARGS set only with
// Environment=, which a line in the file would override.
func (c *config) kubeletEnvFile() (string, error) {
	env, err := c.kubelet.environment(c.KubeletUnit)
	if err != nil {
		return "", fmt.Errorf("find the environment files of kubelet unit %q: %w", c.KubeletUnit, err)
	}
	var assigning, lastOperator string
	for _, f := range env.Files {
		if !filepath.IsAbs(f) {
			return "", fmt.Errorf("kubelet unit %q names the environment file %q, which is not an absolute path", c.KubeletUnit, f)
		}
		if slices.Contains(kubeletEnvFiles, f) {
			lastOperator = f
		}
		data, err := readHostFile(c.hostPath(f))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return "", fmt.Errorf("read %s, an environment file of kubelet unit %q: %w", f, c.KubeletUnit, err)
		}
		assignments, err := parseEnvFile(string(data))
		if err != nil {
			return "", fmt.Errorf("%s: %w", f, err)
		}
		for _, a := range assignments {
			if a.key == extraArgsKey {
				assigning = f
			}
		}
	}
	switch {
	case assigning != "" && !slices.Contains(kubeletEnvFiles, assigning):
		return "", fmt.Errorf("kubelet unit %q takes KUBELET_EXTRA_ARGS from %s, which the installer does not edit (it edits only %s); add the two --image-credential-provider-* flags yourself and use plugin.install.mode=none", c.KubeletUnit, assigning, strings.Join(kubeletEnvFiles, " or "))
	case assigning != "":
		return assigning, nil
	case lastOperator == "":
		return "", fmt.Errorf("kubelet unit %q reads neither %s (its environment files: %s), so patch mode has nowhere to put the kubelet flags; add the two --image-credential-provider-* flags yourself and use plugin.install.mode=none", c.KubeletUnit, strings.Join(kubeletEnvFiles, " nor "), describeFiles(env.Files))
	case setsExtraArgs.MatchString(env.Assignments):
		return "", fmt.Errorf("kubelet unit %q sets KUBELET_EXTRA_ARGS with Environment=, which a KUBELET_EXTRA_ARGS line in %s would override and drop; move it into %s, or add the two --image-credential-provider-* flags yourself and use plugin.install.mode=none", c.KubeletUnit, lastOperator, lastOperator)
	}
	return lastOperator, nil
}

func describeFiles(files []string) string {
	if len(files) == 0 {
		return "none"
	}
	return strings.Join(files, ", ")
}
