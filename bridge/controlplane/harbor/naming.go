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
	// it owns. Combined with the cluster name (and a trailing dot) it
	// forms the ownership prefix that gates every Harbor write call.
	// Harbor's own robot_name_prefix ("robot$") is a different thing: the
	// client strips it on read paths (see DefaultRobotPrefix and
	// WithRobotPrefix), so every name in this file is an internal name.
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

// ErrInvalidRobotName is returned by RobotName when the identity maps to a
// name Harbor refuses to create (robotNameRegex). Cluster, namespace and
// ServiceAccount names are DNS labels, which may contain consecutive
// hyphens ("team--a", "xn--…"); Harbor allows only single separators. No
// robot can exist for such an identity, so the error is permanent, not a
// Harbor outage to retry.
var ErrInvalidRobotName = errors.New("identity maps to a robot name Harbor does not accept")

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
// dot (RFC 1123 label). The same '.'-after-cluster boundary also retires
// ADR-0009's hyphen-prefix ownership footgun (see OwnsRobot).
//
// If the natural form exceeds RobotNameCap, the function falls back to a
// deterministic truncation: the "bridge-<cluster>." prefix is preserved,
// the ns.sa portion is truncated to fit, and the first 16 hex characters
// of the SHA-256 of the full natural name are appended after a '.'. Such a
// name, "bridge-<cluster>.<ns>.<sa prefix>.<digest>", has three dots after
// "bridge-", a natural name exactly two, so the two sets are disjoint as
// long as (ADR-0031):
//
//   - the SA name is dot-free (the CRD pattern). An SA name with a dot
//     would give a natural name the shape of a truncated one, and because
//     the digest is an unkeyed hash of public inputs, anyone could choose
//     the SA name that equals another identity's truncated name. The
//     trailing SA field may therefore NOT take dots without first joining
//     the digest with a character no DNS name contains (e.g. '_'), which
//     renames every truncated robot.
//   - the cut falls inside the SA name. It does for every real input: a
//     cluster name has at most 63 characters (config) and a namespace too
//     (Kubernetes), so ns plus separator always fits the budget. Only a
//     serviceAccountRef.namespace longer than any namespace can be (the
//     CRD allows 253) cuts inside the namespace, and no token can come
//     from such a namespace.
//
// Between two truncated names injectivity rests on the 64-bit digest.
//
// The inputs must not contain dots in cluster or SA namespace (the
// injectivity invariant above) nor in the SA name (the truncation
// disjointness above); the CRD pattern markers on the serviceAccountRef
// fields and BRIDGE_CLUSTER_NAME validation enforce this. They do NOT keep
// the result valid for Harbor: DNS labels may contain "--", which Harbor
// refuses. RobotName checks the result and returns ErrInvalidRobotName for
// such identities.
func RobotName(cluster, saNamespace, saName string) (string, error) {
	name, err := robotName(cluster, saNamespace, saName)
	if err != nil {
		return "", err
	}
	if !IsValidHarborRobotName(name) {
		return "", fmt.Errorf("%w: %q (Harbor allows lower-case letters and digits separated by single '.', '_' or '-'; "+
			"a cluster, namespace or ServiceAccount name with consecutive hyphens cannot be mapped to a Harbor robot)", ErrInvalidRobotName, name)
	}
	return name, nil
}

func robotName(cluster, saNamespace, saName string) (string, error) {
	full := fmt.Sprintf("%s%s.%s.%s", robotNamePrefix, cluster, saNamespace, saName)
	if len(full) <= RobotNameCap {
		return full, nil
	}
	prefix := fmt.Sprintf("%s%s.", robotNamePrefix, cluster)
	digest := hashOf(full)

	// budget = chars available between prefix and the trailing ".<digest>".
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
// robotName must be an internal name: what RobotName returns, or
// Robot.Name from the client, which strips Harbor's robot_name_prefix on
// every read path (ADR-0023). Never pass Robot.WireName: with any prefix
// the answer would be false, and stripping a hard-coded "robot$" here
// would claim unstripped names under a prefix mismatch, which the client
// reports as ErrRobotPrefixMismatch instead.
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
	return strings.HasPrefix(robotName, ClusterPrefix(cluster))
}

// OwnsLegacyRobot reports whether robotName is a robot this cluster's
// bridge created under the pre-ADR-0018 dash-delimited scheme
// ("bridge-<cluster>-<ns>-<sa>", shipped in releases up to 0.2.x). Those
// robots are never adopted — only recognised so the janitor and the
// deletion path can revoke them instead of leaking them with valid
// passwords after an upgrade.
//
// The dash prefix is NOT injective ("bridge-prod-" also prefixes every
// robot of a cluster named "prod-eu"), so this is deliberately a weak
// first filter: a legacy name never contains a dot (the old scheme had
// none, and every current name has one right after the cluster field),
// and callers MUST additionally require the description's cluster tag
// to equal cluster exactly (RobotBelongsToCluster). That tag is what
// tells "prod"'s legacy robots apart from "prod-eu"'s. Like OwnsRobot it
// takes the internal name (Robot.Name), never the on-wire name.
func OwnsLegacyRobot(cluster, robotName string) bool {
	if cluster == "" {
		return false
	}
	rest, ok := strings.CutPrefix(robotName, robotNamePrefix+cluster+"-")
	return ok && rest != "" && !strings.Contains(rest, ".")
}

// IsValidHarborRobotName reports whether the given name would be accepted
// by Harbor's server-side validateName check. RobotName refuses every name
// that fails it (ErrInvalidRobotName).
func IsValidHarborRobotName(name string) bool {
	return robotNameRegex.MatchString(name)
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:hashSuffixLen]
}

// projectNamePattern is Harbor's own rule for project names
// (goharbor/harbor src/pkg/project/manager.go: restrictedNameChars).
var projectNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

// ProjectNameMaxLen is Harbor's project name length limit.
const ProjectNameMaxLen = 255

// ValidateProjectName reports whether name can be a Harbor project. It
// rejects in particular "*", which Harbor reads in a robot permission as
// "every project, including future ones": a HarborAccess must name the
// projects it grants.
func ValidateProjectName(name string) error {
	if len(name) == 0 || len(name) > ProjectNameMaxLen || !projectNamePattern.MatchString(name) {
		return fmt.Errorf("project %q is not a Harbor project name (lower-case letters and digits, separated by single '.', '_' or '-'; at most %d characters)", name, ProjectNameMaxLen)
	}
	return nil
}

func validatePermissions(perms []ProjectPermission) error {
	for _, p := range perms {
		if err := ValidateProjectName(p.Project); err != nil {
			return err
		}
	}
	return nil
}
