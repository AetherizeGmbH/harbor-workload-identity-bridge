// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"regexp"
)

// defaultProviderName is the provider name of an install that does not set
// plugin.providerName. Its node file names predate ADR-0029 and stay as
// they were, so upgrading an existing install moves nothing on the node.
const defaultProviderName = "harbor-bridge-plugin"

// providerNameRegex is a DNS label: a subset of what kubelet accepts as a
// provider name (no "/", no space, not "." or ".."), safe as a file name,
// and without dots, which filesFor relies on. The chart enforces the same
// rule (harbor-bridge.validateRequiredValues).
var providerNameRegex = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func validProviderName(name string) error {
	if len(name) > 63 || !providerNameRegex.MatchString(name) {
		return fmt.Errorf("provider name %q must be a DNS label (lower-case letters, digits and -, at most 63 characters)", name)
	}
	return nil
}

// nodeFiles are the base names of one install's own files on the node
// (ADR-0029). Only these belong to one install; the credential-provider
// config, the bin dir, /etc/default/kubelet and the kubelet unit are
// shared by every install on the node.
type nodeFiles struct {
	Binary     string // in the kubelet bin dir: kubelet runs <bin-dir>/<provider name>
	CA         string // in HostConfigDir
	ClientCert string // in HostConfigDir
	ClientKey  string // in HostConfigDir
	State      string // in StateDir
}

// filesFor derives an install's file names from its provider name. The
// default name keeps the names from before ADR-0029. Any other name
// prefixes them with "<name>.": a provider name has no dot, so two names
// never share a file, and no derived name equals one of the default name's
// files. The chart derives the same paths (harbor-bridge.plugin.hostFile).
func filesFor(name string) nodeFiles {
	if name == defaultProviderName {
		return nodeFiles{
			Binary:     defaultProviderName,
			CA:         "harbor-bridge-ca.crt",
			ClientCert: "harbor-bridge-client.crt",
			ClientKey:  "harbor-bridge-client.key",
			State:      "installer-state.json",
		}
	}
	return nodeFiles{
		Binary:     name,
		CA:         name + ".ca.crt",
		ClientCert: name + ".client.crt",
		ClientKey:  name + ".client.key",
		State:      name + ".installer-state.json",
	}
}
