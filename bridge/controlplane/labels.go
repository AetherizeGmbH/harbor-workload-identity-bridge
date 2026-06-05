// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"fmt"
	"strings"
	"time"
)

// FinalizerName is the finalizer the reconciler attaches to every
// HarborAccess it manages so it gets a chance to delete the Harbor robot
// before the CR is removed.
const FinalizerName = "harbor.aetherize.io/robot"

// Kubernetes label keys used on bridge-managed Secrets. They make ownership
// observable from outside the bridge and let kubectl filter for them.
const (
	LabelManagedBy             = "harbor.aetherize.io/managed-by"
	LabelManagedByValue        = "harbor-workload-identity-bridge"
	LabelCluster               = "harbor.aetherize.io/cluster"
	LabelHarborAccessNamespace = "harbor.aetherize.io/harboraccess-namespace"
	LabelHarborAccessName      = "harbor.aetherize.io/harboraccess-name"
)

// SecretNamePrefix is the constant the bridge prepends to robot-password
// Secret names so an administrator can `kubectl get secrets -l ...` and
// reason about which Secrets are bridge-managed.
const SecretNamePrefix = "robot-"

// PasswordRotationInterval is the maximum age of a robot password before
// the reconciler refreshes it (ADR-0003: rotate daily).
const PasswordRotationInterval = 24 * time.Hour

// robotDescriptionTag is the constant first token in every robot description
// the bridge writes. The reconciler and janitor use it (combined with the
// cluster tag) to decide whether a robot belongs to this bridge — providing
// defense-in-depth on top of the ownership-prefix check from ADR-0009.
const robotDescriptionTag = "managed-by=" + LabelManagedByValue

// RobotDescription builds the description string the bridge writes onto
// every Harbor robot it creates. The format is space-separated key=value
// tokens so the janitor can parse it back. Tokens, in order:
//
//	managed-by=harbor-workload-identity-bridge
//	cluster=<cluster>
//	harboraccess=<haNamespace>/<haName>
//
// Changing this format is a compatibility break for any janitor that has
// to recognise robots created by older bridges; bump the format only
// alongside a documented migration.
func RobotDescription(cluster, haNamespace, haName string) string {
	return fmt.Sprintf("%s cluster=%s harboraccess=%s/%s",
		robotDescriptionTag, cluster, haNamespace, haName)
}

// RobotBelongsToCluster reports whether the given robot description marks
// the robot as belonging to the given cluster. This is the defense-in-depth
// check from ADR-0009 that catches the documented prefix-collision class
// (cluster "prod" picking up cluster "prod-eu"'s robots via name prefix):
// even when the name prefix matches by accident, the cluster tag must
// match exactly.
//
// Returns false for any description the bridge did not create.
func RobotBelongsToCluster(description, cluster string) bool {
	if !strings.HasPrefix(description, robotDescriptionTag+" ") {
		return false
	}
	for _, tok := range strings.Fields(description) {
		k, v, ok := strings.Cut(tok, "=")
		if ok && k == "cluster" {
			return v == cluster
		}
	}
	return false
}

// ParseRobotDescription extracts the HarborAccess namespace and name from
// a bridge-managed robot description. Returns "", "", false when the
// description was not written by the bridge or does not contain a
// harboraccess= token.
func ParseRobotDescription(description string) (haNamespace, haName string, ok bool) {
	if !strings.HasPrefix(description, robotDescriptionTag+" ") {
		return "", "", false
	}
	for _, tok := range strings.Fields(description) {
		k, v, hasEq := strings.Cut(tok, "=")
		if hasEq && k == "harboraccess" {
			ns, name, hasSlash := strings.Cut(v, "/")
			if hasSlash && ns != "" && name != "" {
				return ns, name, true
			}
		}
	}
	return "", "", false
}
