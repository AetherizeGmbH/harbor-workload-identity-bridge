// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"fmt"
	"strings"

	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/nexus"
)

// NexusFinalizerName is the finalizer the Nexus reconciler sets on every
// NexusAccess it provisions, so that it can delete the Nexus users and the
// role before the object goes. With a selector the bridge sets
// NexusFinalizerName + "-<instance>" instead (Config.NexusFinalizer,
// ADR-0026, ADR-0032).
const NexusFinalizerName = "nexus.aetherize.io/user"

// NexusAccessNameMaxLen is the maximum metadata.name length of a
// NexusAccess: the name is a label value on its Secret.
const NexusAccessNameMaxLen = 63

// nexusUserEmail is the e-mail address of every Nexus user the bridge
// creates. Nexus requires one and allows duplicates (verified on 3.76.1);
// the .invalid TLD guarantees that nothing is ever delivered.
const nexusUserEmail = "nexus-bridge@harbor-workload-identity-bridge.invalid"

// Ownership markers on Nexus objects, the ADR-0012 contract (ADR-0036
// decision b). They use the robot description's tokens:
//
//	user firstName    managed-by=harbor-workload-identity-bridge cluster=<cluster>
//	user lastName     nexusaccess=<namespace>/<name>
//	role description  managed-by=harbor-workload-identity-bridge cluster=<cluster> nexusaccess=<namespace>/<name>
//
// Changing the format is a compatibility break for every janitor that has
// to recognise users and roles of older bridges.
const nexusAccessToken = "nexusaccess"

// NexusUserFirstName is the firstName marker of a user of the given
// cluster.
func NexusUserFirstName(cluster string) string {
	return fmt.Sprintf("%s cluster=%s", robotDescriptionTag, cluster)
}

// NexusUserLastName is the lastName marker of a user of the given
// NexusAccess.
func NexusUserLastName(nxaNamespace, nxaName string) string {
	return fmt.Sprintf("%s=%s/%s", nexusAccessToken, nxaNamespace, nxaName)
}

// NexusRoleDescription is the description marker of a role of the given
// NexusAccess in the given cluster.
func NexusRoleDescription(cluster, nxaNamespace, nxaName string) string {
	return NexusUserFirstName(cluster) + " " + NexusUserLastName(nxaNamespace, nxaName)
}

// parseNexusOwner reads the NexusAccess and cluster a set of marker tokens
// names: the bridge's tag first, then cluster= and nexusaccess= tokens.
// ok is false when the tag is missing or either token is absent or empty.
func parseNexusOwner(markers string) (cluster, nxaNamespace, nxaName string, ok bool) {
	if !strings.HasPrefix(markers, robotDescriptionTag+" ") {
		return "", "", "", false
	}
	for _, tok := range strings.Fields(markers) {
		k, v, hasEq := strings.Cut(tok, "=")
		switch {
		case !hasEq:
		case k == "cluster" && cluster == "":
			cluster = v
		case k == nexusAccessToken && nxaNamespace == "":
			ns, name, hasSlash := strings.Cut(v, "/")
			if hasSlash && ns != "" && name != "" {
				nxaNamespace, nxaName = ns, name
			}
		}
	}
	return cluster, nxaNamespace, nxaName, cluster != "" && nxaNamespace != ""
}

// nexusUserOwner returns the cluster and NexusAccess a user's markers
// name. The two markers are joined as one token list.
func nexusUserOwner(u *nexus.User) (cluster, nxaNamespace, nxaName string, ok bool) {
	return parseNexusOwner(u.FirstName + " " + u.LastName)
}

// nexusUserOwnedBy reports whether u is a user the bridge of cluster
// created for the NexusAccess nxaNamespace/nxaName. All layers must hold
// (ADR-0036 decision b, as robotOwnedBy for robots): the id lies in the
// cluster's ownership prefix and has the shape of a user id the bridge
// builds, it is a local user, and its markers carry the bridge's tag,
// exactly this cluster and exactly this NexusAccess.
func nexusUserOwnedBy(cluster string, u *nexus.User, nxaNamespace, nxaName string) bool {
	if !nexus.OwnsName(cluster, u.UserID) || u.Source != nexus.DefaultSource {
		return false
	}
	if _, _, ok := nexus.ParseUserID(u.UserID); !ok {
		return false
	}
	c, ns, name, ok := nexusUserOwner(u)
	return ok && c == cluster && ns == nxaNamespace && name == nxaName
}

// nexusRoleOwnedBy is nexusUserOwnedBy for roles.
func nexusRoleOwnedBy(cluster string, r *nexus.Role, nxaNamespace, nxaName string) bool {
	if !nexus.OwnsName(cluster, r.ID) {
		return false
	}
	c, ns, name, ok := parseNexusOwner(r.Description)
	return ok && c == cluster && ns == nxaNamespace && name == nxaName
}
