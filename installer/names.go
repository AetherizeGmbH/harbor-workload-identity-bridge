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
	Binary string // in the kubelet bin dir: kubelet runs <bin-dir>/<provider name>
	// Record is next to Binary in the kubelet bin dir: the canonical bytes
	// of this install's provider entry (entryRecord).
	Record     string
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
// The record did not exist before ADR-0029 and is "<name>.entry" for every
// name: dotted, so never a provider name, and never a binary kubelet runs.
func filesFor(name string) nodeFiles {
	if name == defaultProviderName {
		return nodeFiles{
			Binary:     defaultProviderName,
			Record:     defaultProviderName + recordSuffix,
			CA:         "harbor-bridge-ca.crt",
			ClientCert: "harbor-bridge-client.crt",
			ClientKey:  "harbor-bridge-client.key",
			State:      "installer-state.json",
		}
	}
	return nodeFiles{
		Binary:     name,
		Record:     name + recordSuffix,
		CA:         name + ".ca.crt",
		ClientCert: name + ".client.crt",
		ClientKey:  name + ".client.key",
		State:      name + ".installer-state.json",
	}
}

// recordSuffix ends the name of every install's record (nodeFiles.Record).
const recordSuffix = ".entry"

// The install container reads the rendered provider config from the
// chart's ConfigMap, mounted at one of two layouts
// (harbor-bridge.plugin.configKey and configMountPath). The default
// provider name keeps the layout every installer reads. Any other name
// uses a key and a mount path that installers before ADR-0029 never read:
// such an installer ignores PROVIDER_NAME and would install the entry of
// the new name without a binary of that name, which keeps kubelet from
// starting. On this layout it fails at reading the config instead, before
// it writes anything on the node.
const (
	legacySourceConfig = "/config/credential-provider-config.yaml"
	v2SourceConfig     = "/config-v2/credential-provider-config.v2.yaml"
)

// sourceConfigPath is where the install container of provider name finds
// the rendered provider config.
func sourceConfigPath(name string) string {
	if name == defaultProviderName {
		return legacySourceConfig
	}
	return v2SourceConfig
}
