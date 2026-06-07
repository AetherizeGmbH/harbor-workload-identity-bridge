// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

// Package harbor wraps github.com/goharbor/go-client with the small surface
// the bridge control plane needs (create / delete / list / get / refresh
// robot accounts) plus the bridge-specific naming and ownership invariants
// defined in docs/adr/0009-multi-cluster-topology.md.
package harbor

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	// RobotNameCap is the maximum length of a Harbor robot username. The
	// authoritative source is the postgres column robot.name varchar(255)
	// in goharbor/harbor's schema migrations. We use a soft cap below the
	// hard limit to leave headroom for Harbor-internal name handling and
	// for future, unannounced length tightening.
	RobotNameCap = 240

	// hashSuffixLen is the number of hex chars from the SHA-256 digest used
	// to disambiguate truncated robot names. 16 hex chars = 64 bits of
	// collision space, well below the birthday bound for any realistic
	// fleet of HarborAccess CRs.
	hashSuffixLen = 16

	// robotNamePrefix is the constant the bridge prepends to every robot
	// it owns. Combined with the cluster name (and a trailing dash) it
	// forms the ownership prefix that gates every Harbor write call.
	robotNamePrefix = "bridge-"

	// HarborRobotPrefix is the literal Harbor prepends server-side to
	// every system-level robot name on read paths (GET /robots,
	// GET /robots/{id}). POST /robots accepts the un-prefixed name and
	// Harbor adds the prefix on store. So the bridge sends
	// "bridge-<cluster>.<ns>.<sa>" to Create but reads back
	// "robot$bridge-<cluster>.<ns>.<sa>". Every comparison between an
	// internally-constructed name and a name from Harbor must reckon
	// with this asymmetry — see OwnsRobot and GetByName.
	HarborRobotPrefix = "robot$"
)

// robotNameRegex mirrors Harbor's server-side validateName check:
//
//	^[a-z0-9]+(?:[._-][a-z0-9]+)*$
//
// See src/server/v2.0/handler/robot.go in goharbor/harbor.
var robotNameRegex = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

// ErrClusterNameTooLong is returned by RobotName when the configured cluster
// name leaves no room for any portion of the SA identity inside the robot
// name cap. The fix is operator-side: shorten BRIDGE_CLUSTER_NAME.
var ErrClusterNameTooLong = errors.New("cluster name leaves no room for SA identity within robot name limit")

// RobotName computes the deterministic Harbor robot name for the given
// (cluster, SA namespace, SA name) tuple. Reconciles must produce the same
// output for the same input, so this function is intentionally pure.
//
// The natural form is "bridge-<cluster>.<ns>.<sa>" — the three identity
// fields are joined with '.', NOT '-'. This is what makes the mapping
// injective (ADR-0018): cluster, SA namespace, and SA name are all
// dash-allowed DNS labels, so a '-' delimiter is ambiguous
// ("bridge-c-a-b-x" could be ns "a"/sa "b-x" or ns "a-b"/sa "x"). A '.'
// delimiter is unambiguous because every field LEFT of the last dot is a
// Kubernetes namespace or the cluster label, none of which may contain a
// dot (RFC 1123 label). The trailing field (SA name) may contain dots
// without breaking the split. The same '.'-after-cluster boundary also
// retires ADR-0009's hyphen-prefix ownership footgun (see OwnsRobot).
//
// If the natural form exceeds RobotNameCap, the function falls back to a
// deterministic truncation: the "bridge-<cluster>." prefix is preserved,
// the ns.sa portion is truncated to fit, and a hex-encoded SHA-256 suffix
// of the full pre-truncation name disambiguates (probabilistically — this
// is the only path where injectivity rests on the hash rather than the
// delimiter).
//
// All inputs must satisfy Harbor's robot-name regex segment rules
// (lowercase alphanumerics + . _ - separators) AND the injectivity
// invariant above (no dots in cluster or SA namespace); the caller is
// responsible for validating this upstream (CRD pattern markers on
// serviceAccountRef fields and BRIDGE_CLUSTER_NAME validation handle this
// today — both forbid dots).
func RobotName(cluster, saNamespace, saName string) (string, error) {
	full := fmt.Sprintf("%s%s.%s.%s", robotNamePrefix, cluster, saNamespace, saName)
	if len(full) <= RobotNameCap {
		return full, nil
	}
	prefix := fmt.Sprintf("%s%s.", robotNamePrefix, cluster)
	digest := hashOf(full)

	// budget = chars available between prefix and trailing "-<digest>".
	budget := RobotNameCap - len(prefix) - 1 - hashSuffixLen
	if budget < 1 {
		return "", fmt.Errorf("%w: cluster %q", ErrClusterNameTooLong, cluster)
	}
	mid := saNamespace + "." + saName
	if len(mid) > budget {
		mid = mid[:budget]
	}
	// A truncated mid can end with a separator, which breaks Harbor's
	// segment regex. Trim trailing separator chars and fall back to
	// hash-only form when nothing identifying remains.
	mid = strings.TrimRight(mid, "-._")
	if mid == "" {
		return prefix + digest, nil
	}
	// Join the disambiguating digest with '.' to match the rest of the
	// scheme (ADR-0018). budget already reserves one char for this separator.
	return prefix + mid + "." + digest, nil
}

// ClusterPrefix returns the ownership prefix for the given cluster. Robots
// whose names do not begin with this string are not managed by this bridge
// (ADR-0009 safety invariant).
func ClusterPrefix(cluster string) string {
	return robotNamePrefix + cluster + "."
}

// OwnsRobot reports whether the given robot name belongs to the bridge in
// the given cluster. A bridge MUST NOT list, modify, or delete a robot for
// which OwnsRobot returns false; this is enforced at every Harbor write site.
//
// Accepts both the internal name (what RobotName returns, what we send to
// POST /robots) and the Harbor-on-wire name (what List/Get return,
// "robot$<internal>"). Callers should not have to know which form they
// hold — this is the single normalization point.
//
// The ownership prefix is "bridge-<cluster>." (dot-terminated, ADR-0018).
// Because the cluster field is a dot-free DNS label, distinct cluster names
// produce non-prefixing ownership prefixes — "bridge-prod." is NOT a prefix
// of "bridge-prod-eu.flux.svc" (the char after "bridge-prod" is '-', not the
// required '.'). This retires ADR-0009's hyphen-prefix false-positive class
// (where cluster "prod" saw cluster "prod-eu"'s robots): the dot terminator
// is the boundary the old '-' terminator could not provide. The
// description-tag check (RobotBelongsToCluster) remains as defense-in-depth.
func OwnsRobot(cluster, robotName string) bool {
	if cluster == "" {
		return false
	}
	n := strings.TrimPrefix(robotName, HarborRobotPrefix)
	return strings.HasPrefix(n, ClusterPrefix(cluster))
}

// IsValidHarborRobotName reports whether the given name would be accepted
// by Harbor's server-side validateName check. Exposed for tests and for
// defensive checks inside the Harbor client wrapper.
func IsValidHarborRobotName(name string) bool {
	return robotNameRegex.MatchString(name)
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:hashSuffixLen]
}
