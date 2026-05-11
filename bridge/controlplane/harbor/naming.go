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
// The natural form is "bridge-<cluster>-<ns>-<sa>". If that exceeds
// RobotNameCap, the function falls back to a deterministic truncation:
// the prefix is preserved, the ns+sa portion is truncated to fit, and a
// hex-encoded SHA-256 suffix of the full pre-truncation name disambiguates
// collisions.
//
// All inputs must satisfy Harbor's robot-name regex segment rules
// (lowercase alphanumerics + . _ - separators); the caller is responsible
// for validating this upstream (CRD pattern markers on serviceAccountRef
// fields and BRIDGE_CLUSTER_NAME validation handle this today).
func RobotName(cluster, saNamespace, saName string) (string, error) {
	full := fmt.Sprintf("%s%s-%s-%s", robotNamePrefix, cluster, saNamespace, saName)
	if len(full) <= RobotNameCap {
		return full, nil
	}
	prefix := fmt.Sprintf("%s%s-", robotNamePrefix, cluster)
	digest := hashOf(full)

	// budget = chars available between prefix and trailing "-<digest>".
	budget := RobotNameCap - len(prefix) - 1 - hashSuffixLen
	if budget < 1 {
		return "", fmt.Errorf("%w: cluster %q", ErrClusterNameTooLong, cluster)
	}
	mid := saNamespace + "-" + saName
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
	return prefix + mid + "-" + digest, nil
}

// ClusterPrefix returns the ownership prefix for the given cluster. Robots
// whose names do not begin with this string are not managed by this bridge
// (ADR-0009 safety invariant).
func ClusterPrefix(cluster string) string {
	return robotNamePrefix + cluster + "-"
}

// OwnsRobot reports whether the given robot name belongs to the bridge in
// the given cluster. A bridge MUST NOT list, modify, or delete a robot for
// which OwnsRobot returns false; this is enforced at every Harbor write site.
//
// Caveat: prefix matching has a known false-positive class when cluster
// names are hyphen-prefixes of each other (e.g. cluster "prod" would see
// cluster "prod-eu"'s robots as its own). This is documented in ADR-0009
// as an operator responsibility (cluster names must be chosen so none is a
// hyphen-prefix of another).
func OwnsRobot(cluster, robotName string) bool {
	if cluster == "" {
		return false
	}
	return strings.HasPrefix(robotName, ClusterPrefix(cluster))
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
