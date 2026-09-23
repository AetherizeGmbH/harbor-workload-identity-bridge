// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

// Package robotsecret is the single definition of the per-HarborAccess
// robot-password Secret contract: its name, its ownership labels, the
// annotations the control plane stamps on it, and its data keys.
//
// The control plane writes these Secrets and the data plane reads them.
// Both import this leaf package, so the contract cannot drift between
// them. That does not weaken ADR-0002: the rule is that the control plane
// never imports the data plane (and this package imports neither). See
// ADR-0023. The kubelet plugin is unaffected — it never sees the Secret.
package robotsecret

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// NamePrefix is the constant the bridge prepends to robot-password Secret
// names so an administrator can recognise bridge-managed Secrets.
const NamePrefix = "robot-"

// Data keys of the Secret.
const (
	KeyUsername = "username"
	KeyPassword = "password"
)

// Label keys stamped on every bridge-managed robot Secret. They make
// ownership observable from outside the bridge, let `kubectl get -l`
// filter for them, and let the control plane map a Secret event back to
// its HarborAccess.
const (
	LabelManagedBy             = "harbor.aetherize.io/managed-by"
	LabelManagedByValue        = "harbor-workload-identity-bridge"
	LabelCluster               = "harbor.aetherize.io/cluster"
	LabelHarborAccessNamespace = "harbor.aetherize.io/harboraccess-namespace"
	LabelHarborAccessName      = "harbor.aetherize.io/harboraccess-name"
)

// Annotation keys stamped by the control plane.
const (
	// AnnotationRobotID records the Harbor robot ID whose password the
	// Secret holds. When Harbor reports a different ID for the robot name
	// (the robot was deleted and re-created out of band), the stored
	// password belongs to a robot that no longer exists and the control
	// plane must rotate.
	AnnotationRobotID = "harbor.aetherize.io/robot-id"

	// AnnotationRotationNotBefore is the control plane's promise: it will
	// not rotate this password before the given instant (RFC 3339, UTC)
	// unless it is forced to (the Secret vanished or the robot was
	// replaced). The data plane caps the kubelet cache duration it hands
	// out so that no node caches the password past this instant, which is
	// what makes the scheduled daily rotation invisible to image pulls.
	AnnotationRotationNotBefore = "harbor.aetherize.io/rotation-not-before"
)

const (
	nameMax     = 253 // Kubernetes object-name limit (DNS subdomain).
	nameHashLen = 16  // 64 bits, matching harbor.RobotName's overflow handling.
)

// Name computes the robot-password Secret name for a HarborAccess. The
// namespace and name are dot-joined for injectivity (ADR-0018; the
// namespace is a dot-free RFC 1123 label, so the first dot is an
// unambiguous boundary). When the natural name would exceed the 253-char
// Kubernetes limit it is hash-truncated, mirroring harbor.RobotName's
// overflow handling — only that path is probabilistic rather than provably
// injective. HarborAccess names are limited to 63 characters by the CRD,
// so the overflow path is unreachable for admitted objects.
func Name(haNamespace, haName string) string {
	full := NamePrefix + haNamespace + "." + haName
	if len(full) <= nameMax {
		return full
	}
	sum := sha256.Sum256([]byte(haNamespace + "\x00" + haName))
	digest := hex.EncodeToString(sum[:])[:nameHashLen]
	budget := nameMax - len(NamePrefix) - 1 - nameHashLen
	mid := haNamespace + "." + haName
	if len(mid) > budget {
		mid = mid[:budget]
	}
	mid = strings.TrimRight(mid, "-._")
	if mid == "" {
		return NamePrefix + digest
	}
	return NamePrefix + mid + "." + digest
}

// Labels returns the ownership labels for the Secret of the given
// HarborAccess in the given cluster.
func Labels(cluster, haNamespace, haName string) map[string]string {
	return map[string]string{
		LabelManagedBy:             LabelManagedByValue,
		LabelCluster:               cluster,
		LabelHarborAccessNamespace: haNamespace,
		LabelHarborAccessName:      haName,
	}
}

// IsManaged reports whether the Secret carries the bridge's managed-by
// label. Secrets without it (hand-created, or written by a bridge that
// predates the labels) are not treated as owned by anyone.
func IsManaged(s *corev1.Secret) bool {
	return s.Labels[LabelManagedBy] == LabelManagedByValue
}

// Owner returns the HarborAccess a managed Secret is stamped for. ok is
// false for Secrets that are not managed or lack either owner label.
func Owner(s *corev1.Secret) (haNamespace, haName string, ok bool) {
	if !IsManaged(s) {
		return "", "", false
	}
	haNamespace, haName = s.Labels[LabelHarborAccessNamespace], s.Labels[LabelHarborAccessName]
	return haNamespace, haName, haNamespace != "" && haName != ""
}

// StampedForOther reports whether the Secret is a managed Secret stamped
// for a DIFFERENT HarborAccess than (haNamespace, haName). This is the
// collision backstop (audit F2) both planes enforce: never overwrite, and
// never hand out, another HarborAccess's robot password. An unmanaged
// Secret is not "stamped for other" — it is adoptable, so upgrades from
// pre-label bridges keep working.
func StampedForOther(s *corev1.Secret, haNamespace, haName string) bool {
	if !IsManaged(s) {
		return false
	}
	return s.Labels[LabelHarborAccessNamespace] != haNamespace ||
		s.Labels[LabelHarborAccessName] != haName
}

// RobotID returns the robot ID annotation. ok is false when the
// annotation is absent or unparseable.
func RobotID(s *corev1.Secret) (id int64, ok bool) {
	raw, present := s.Annotations[AnnotationRobotID]
	if !present {
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// FormatRobotID renders a robot ID for AnnotationRobotID.
func FormatRobotID(id int64) string {
	return strconv.FormatInt(id, 10)
}

// RotationNotBefore returns the parsed AnnotationRotationNotBefore. ok is
// false when the annotation is absent or unparseable; callers must then
// fall back to their own cache bound.
func RotationNotBefore(s *corev1.Secret) (t time.Time, ok bool) {
	raw, present := s.Annotations[AnnotationRotationNotBefore]
	if !present {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// FormatRotationNotBefore renders an instant for
// AnnotationRotationNotBefore at seconds precision, rounded up. The
// control plane reads the annotation back (not the unrounded instant) to
// decide when a rotation is due, so the rendered value IS the promise.
func FormatRotationNotBefore(t time.Time) string {
	up := t.UTC().Truncate(time.Second)
	if up.Before(t) {
		up = up.Add(time.Second)
	}
	return up.Format(time.RFC3339)
}

// Credentials returns the username and password keys, or an error naming
// what is missing.
func Credentials(s *corev1.Secret) (username, password string, err error) {
	username, password = string(s.Data[KeyUsername]), string(s.Data[KeyPassword])
	if username == "" || password == "" {
		return "", "", fmt.Errorf("robot Secret %s/%s missing %q and/or %q keys",
			s.Namespace, s.Name, KeyUsername, KeyPassword)
	}
	return username, password, nil
}
