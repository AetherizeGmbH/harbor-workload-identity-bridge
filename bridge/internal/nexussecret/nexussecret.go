// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

// Package nexussecret is the single definition of the per-NexusAccess
// user-password Secret contract (ADR-0036 decision e): its name, its
// ownership labels, the annotations the control plane stamps on it, and
// its data keys.
//
// The control plane writes these Secrets and the data plane reads them.
// Both import this leaf package, like bridge/internal/robotsecret for
// HarborAccess, so the contract cannot drift between them (ADR-0002 only
// forbids the control plane from importing the data plane). The kubelet
// plugin never sees the Secret.
//
// A Nexus Secret and a robot Secret can never be mistaken for each other:
// their name prefixes are disjoint ("nexususer-" and "robot-", neither a
// prefix of the other), a Nexus Secret carries the access-kind label
// "nexus" and no harboraccess-* labels, and a robot Secret carries neither
// the access-kind label nor the app.kubernetes.io/managed-by label.
package nexussecret

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/robotsecret"
)

// NamePrefix is the constant the bridge prepends to NexusAccess Secret
// names. It differs from robotsecret.NamePrefix, and neither is a prefix
// of the other, so a Nexus Secret name never equals a robot Secret name
// (ADR-0036 Context 9).
const NamePrefix = "nexususer-"

// Data keys of the Secret: the Nexus user id and its password, the same
// keys as a robot Secret.
const (
	KeyUsername = robotsecret.KeyUsername
	KeyPassword = robotsecret.KeyPassword
)

// Label keys and values stamped on every bridge-managed Nexus Secret.
const (
	// LabelManagedBy marks the Secret as written by the bridge.
	LabelManagedBy      = "app.kubernetes.io/managed-by"
	LabelManagedByValue = robotsecret.LabelManagedByValue

	// LabelCluster is the cluster label robot Secrets carry too: the
	// clusterName of the bridge that wrote the Secret.
	LabelCluster = robotsecret.LabelCluster

	// LabelAccessKind names the kind of access object a Secret belongs
	// to. Nexus Secrets carry AccessKindNexus; robot Secrets do not carry
	// the label. The data plane serves a Secret for a NexusAccess only
	// with this label, and never for a HarborAccess.
	LabelAccessKind = "harbor.aetherize.io/access-kind"
	AccessKindNexus = "nexus"

	// LabelNexusAccessNamespace and LabelNexusAccessName name the
	// NexusAccess the Secret belongs to.
	LabelNexusAccessNamespace = "nexus.aetherize.io/nexusaccess-namespace"
	LabelNexusAccessName      = "nexus.aetherize.io/nexusaccess-name"
)

// Annotation keys stamped by the control plane.
const (
	// AnnotationRotationNotBefore is the control plane's promise not to
	// retire the stored password's user before the given instant (RFC
	// 3339, UTC) unless it is forced to. The key and its meaning are those
	// of robotsecret.AnnotationRotationNotBefore (ADR-0023): the data
	// plane caps the kubelet cache duration at it.
	AnnotationRotationNotBefore = robotsecret.AnnotationRotationNotBefore

	// AnnotationUserID records the Nexus user id whose password the Secret
	// holds; it equals the username key. The control plane writes both
	// with every new password and treats a Secret whose two values differ
	// as incomplete.
	AnnotationUserID = "nexus.aetherize.io/user-id"

	// AnnotationPendingUserID names the user the control plane is about to
	// create (ADR-0036 decision c). It is written before the user is
	// created, so the janitor can tell a user being created from an orphan
	// (Nexus reports no creation time), and removed with the write that
	// stores the user's password.
	AnnotationPendingUserID = "nexus.aetherize.io/pending-user-id"

	// AnnotationRetiringUserID and AnnotationRetireAfter name the user a
	// rotation replaced and the instant (RFC 3339, UTC) after which the
	// control plane deletes it. The previous user outlives the Secret
	// write by a grace period, so that a pull that fetched its credentials
	// just before the rotation, and a data-plane replica whose cache has
	// not yet seen the new Secret, do not fail against a deleted user.
	AnnotationRetiringUserID = "nexus.aetherize.io/retiring-user-id"
	AnnotationRetireAfter    = "nexus.aetherize.io/retire-after"

	// AnnotationGrantsIncomplete is present while the NexusAccess's role
	// lacks privileges its spec names because a repository does not exist,
	// or because Nexus reports a privilege under one of its names that is
	// not its own built-in privilege (ADR-0036 decision d, note 19). Its
	// value lists those repositories. The data plane serves no credentials
	// from a Secret that carries it, whatever its value.
	AnnotationGrantsIncomplete = "nexus.aetherize.io/grants-incomplete"
)

const (
	nameMax     = 253 // Kubernetes object-name limit (DNS subdomain).
	nameHashLen = 16  // 64 bits, as robotsecret.Name.
)

// Name computes the Secret name of a NexusAccess:
// "nexususer-<namespace>.<name>". The namespace is a dot-free RFC 1123
// label, so the first dot is an unambiguous boundary (ADR-0018). A name
// that would exceed 253 characters is hash-truncated exactly like
// robotsecret.Name; NexusAccess names are limited to 63 characters by the
// CRD, so admitted objects never reach that path.
func Name(nxaNamespace, nxaName string) string {
	full := NamePrefix + nxaNamespace + "." + nxaName
	if len(full) <= nameMax {
		return full
	}
	sum := sha256.Sum256([]byte(nxaNamespace + "\x00" + nxaName))
	digest := hex.EncodeToString(sum[:])[:nameHashLen]
	budget := nameMax - len(NamePrefix) - 1 - nameHashLen
	mid := nxaNamespace + "." + nxaName
	if len(mid) > budget {
		mid = mid[:budget]
	}
	mid = strings.TrimRight(mid, "-._")
	if mid == "" {
		return NamePrefix + digest
	}
	return NamePrefix + mid + "." + digest
}

// ParseName returns the NexusAccess a Secret name was computed for. It
// inverts Name for names that were not hash-truncated, which covers every
// admitted NexusAccess. The result says which NexusAccess the name points
// at, not who owns the Secret: ownership is the labels' business (Owner).
func ParseName(name string) (nxaNamespace, nxaName string, ok bool) {
	rest, found := strings.CutPrefix(name, NamePrefix)
	if !found {
		return "", "", false
	}
	nxaNamespace, nxaName, found = strings.Cut(rest, ".")
	if !found || nxaNamespace == "" || nxaName == "" {
		return "", "", false
	}
	return nxaNamespace, nxaName, true
}

// Labels returns the labels of the Secret of the given NexusAccess in the
// given cluster.
func Labels(cluster, nxaNamespace, nxaName string) map[string]string {
	return map[string]string{
		LabelManagedBy:            LabelManagedByValue,
		LabelCluster:              cluster,
		LabelAccessKind:           AccessKindNexus,
		LabelNexusAccessNamespace: nxaNamespace,
		LabelNexusAccessName:      nxaName,
	}
}

// IsManaged reports whether the Secret is a Nexus Secret the bridge wrote:
// it carries the managed-by label and the access kind "nexus". A robot
// Secret is not one, and neither is a hand-made Secret. Unlike robot
// Secrets no Nexus Secret predates the labels, so a Secret without them is
// never adopted.
func IsManaged(s *corev1.Secret) bool {
	return s.Labels[LabelManagedBy] == LabelManagedByValue && s.Labels[LabelAccessKind] == AccessKindNexus
}

// Owner returns the NexusAccess a managed Nexus Secret is stamped for. ok
// is false for any other Secret.
func Owner(s *corev1.Secret) (nxaNamespace, nxaName string, ok bool) {
	if !IsManaged(s) {
		return "", "", false
	}
	nxaNamespace, nxaName = s.Labels[LabelNexusAccessNamespace], s.Labels[LabelNexusAccessName]
	return nxaNamespace, nxaName, nxaNamespace != "" && nxaName != ""
}

// OwnedBy reports whether the Secret is a managed Nexus Secret stamped for
// exactly the NexusAccess (nxaNamespace, nxaName). Both planes use it as
// the collision backstop (audit F2): the control plane writes a Secret
// only when it is absent or OwnedBy the object, and the data plane serves
// it only when OwnedBy the matched object.
func OwnedBy(s *corev1.Secret, nxaNamespace, nxaName string) bool {
	ns, name, ok := Owner(s)
	return ok && ns == nxaNamespace && name == nxaName
}

// StampedForOther reports whether the Secret is a managed Nexus Secret
// stamped for a different NexusAccess than (nxaNamespace, nxaName).
func StampedForOther(s *corev1.Secret, nxaNamespace, nxaName string) bool {
	return IsManaged(s) && !OwnedBy(s, nxaNamespace, nxaName)
}

// UserID returns AnnotationUserID; "" when absent.
func UserID(s *corev1.Secret) string {
	return s.Annotations[AnnotationUserID]
}

// PendingUserID returns AnnotationPendingUserID; "" when absent.
func PendingUserID(s *corev1.Secret) string {
	return s.Annotations[AnnotationPendingUserID]
}

// Retiring returns the user a rotation replaced and when it may be
// deleted. ok is false when no user is retiring; a retiring user whose
// instant is missing or unparseable is returned with the zero time, which
// has passed.
func Retiring(s *corev1.Secret) (userID string, after time.Time, ok bool) {
	userID = s.Annotations[AnnotationRetiringUserID]
	if userID == "" {
		return "", time.Time{}, false
	}
	after, err := time.Parse(time.RFC3339, s.Annotations[AnnotationRetireAfter])
	if err != nil {
		return userID, time.Time{}, true
	}
	return userID, after, true
}

// FormatTime renders an instant for AnnotationRetireAfter and
// AnnotationRotationNotBefore: RFC 3339 at seconds precision, rounded up
// (robotsecret.FormatRotationNotBefore).
func FormatTime(t time.Time) string {
	return robotsecret.FormatRotationNotBefore(t)
}

// GrantsIncomplete reports whether the Secret carries
// AnnotationGrantsIncomplete, and the repositories it lists.
func GrantsIncomplete(s *corev1.Secret) (missing []string, incomplete bool) {
	raw, present := s.Annotations[AnnotationGrantsIncomplete]
	if !present {
		return nil, false
	}
	for _, r := range strings.Split(raw, ",") {
		if r = strings.TrimSpace(r); r != "" {
			missing = append(missing, r)
		}
	}
	return missing, true
}

// FormatGrantsIncomplete renders the missing repositories for
// AnnotationGrantsIncomplete, sorted and without duplicates.
func FormatGrantsIncomplete(missing []string) string {
	out := slices.Clone(missing)
	slices.Sort(out)
	return strings.Join(slices.Compact(out), ",")
}

// RotationNotBefore returns the parsed AnnotationRotationNotBefore. ok is
// false when the annotation is absent or unparseable.
func RotationNotBefore(s *corev1.Secret) (time.Time, bool) {
	return robotsecret.RotationNotBefore(s)
}

// Credentials returns the username and password keys, or an error naming
// what is missing. A Secret the control plane created only to record a
// pending user has neither.
func Credentials(s *corev1.Secret) (username, password string, err error) {
	username, password = string(s.Data[KeyUsername]), string(s.Data[KeyPassword])
	if username == "" || password == "" {
		return "", "", fmt.Errorf("nexus user Secret %s/%s missing %q and/or %q keys",
			s.Namespace, s.Name, KeyUsername, KeyPassword)
	}
	return username, password, nil
}
