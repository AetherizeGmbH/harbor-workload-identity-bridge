// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"fmt"
	"strings"
	"time"

	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/harbor"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/robotsecret"
)

// FinalizerName is the finalizer the reconciler attaches to every
// HarborAccess it manages so it gets a chance to delete the Harbor robot
// before the CR is removed.
const FinalizerName = "harbor.aetherize.io/robot"

// PasswordRotationInterval is the maximum age of a robot password before
// the reconciler refreshes it (ADR-0003: rotate daily).
const PasswordRotationInterval = 24 * time.Hour

// RotationSafetyMargin is how long after a Secret's rotation-not-before
// instant the reconciler waits before rotating. The data plane never lets
// kubelet cache credentials past rotation-not-before (ADR-0023), so the
// margin only has to absorb request latency and clock skew between the
// bridge replicas; a minute is generous.
const RotationSafetyMargin = time.Minute

// ResyncInterval bounds how long a Ready HarborAccess goes without the
// reconciler re-reading its robot from Harbor. That re-read is what
// notices out-of-band drift — a robot deleted, disabled, or re-scoped in
// the Harbor UI — so it must not depend on controller-runtime's implicit
// cache resync (10h by default, and tunable away entirely).
const ResyncInterval = time.Hour

// HarborAccessNameMaxLen is the maximum metadata.name length of a
// HarborAccess. The name is stamped as a label value on the robot Secret
// (63-character limit), so a longer name could never be reconciled.
const HarborAccessNameMaxLen = 63

// robotDescriptionTag is the constant first token in every robot description
// the bridge writes. The reconciler and janitor use it (combined with the
// cluster tag) to decide whether a robot belongs to this bridge — providing
// defense-in-depth on top of the ownership-prefix check from ADR-0009.
const robotDescriptionTag = "managed-by=" + robotsecret.LabelManagedByValue

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

// robotOwnedBy reports whether robot is a robot the bridge of cluster
// created for the HarborAccess haNamespace/haName. All three layers must
// hold (ADR-0009 + ADR-0023):
//
//  1. the name is in the cluster's ownership prefix — the current
//     dot-terminated one, or the pre-ADR-0018 dash-delimited one;
//  2. the description carries the bridge's tag with exactly this cluster
//     (the only layer that separates "prod"'s legacy robots from
//     "prod-eu"'s);
//  3. the description names exactly this HarborAccess.
func robotOwnedBy(cluster string, robot *harbor.Robot, haNamespace, haName string) bool {
	if !harbor.OwnsRobot(cluster, robot.Name) && !harbor.OwnsLegacyRobot(cluster, robot.Name) {
		return false
	}
	if !RobotBelongsToCluster(robot.Description, cluster) {
		return false
	}
	ns, name, ok := ParseRobotDescription(robot.Description)
	return ok && ns == haNamespace && name == haName
}
