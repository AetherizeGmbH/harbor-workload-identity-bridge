// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package nexus

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Naming (ADR-0033). One workload identity (cluster, ServiceAccount
// namespace, ServiceAccount name) maps to one Nexus role and to one Nexus
// local user at a time:
//
//	identity name  bridge-<cluster>.<saNamespace>.<saName>   (the role id)
//	user id        <identity name>_<generation>              (16 hex digits)
//
// The identity name follows ADR-0018/ADR-0031: dot-delimited, so it is
// injective over DNS-label inputs. Every password rotation replaces the
// user with one under a fresh generation, because Nexus keeps a user's
// docker bearer token valid across a password change and across a delete
// and re-create under the same id (ADR-0033, verified against Nexus
// 3.76.1); only a user id that never exists again revokes it.
const (
	// NameCap bounds every user id (identity name, separator and
	// generation). Nexus stores user ids in VARCHAR(200) (a 201-character
	// id fails with 500 on Nexus 3.76.1, ADR-0033); the cap leaves room
	// below that limit, and role ids (identity names) are shorter still.
	NameCap = 190

	// GenerationLen is the length of a user id's generation: 64 random
	// bits as lower-case hex.
	GenerationLen = 16

	// identityNameCap bounds an identity name so that a user id built
	// from it fits NameCap.
	identityNameCap = NameCap - len(generationSeparator) - GenerationLen

	// hashSuffixLen is the number of hex characters of the SHA-256 of the
	// natural identity name that disambiguate a truncated one (64 bits),
	// as in harbor.RobotName.
	hashSuffixLen = 16

	// namePrefix starts every name the bridge owns. With the cluster name
	// and a trailing dot it forms the ownership prefix (ClusterPrefix).
	namePrefix = "bridge-"

	// generationSeparator joins identity name and generation. No DNS
	// label, no '.' and no hex digest contains it, so the last '_' of a
	// user id is always the boundary (ParseUserID).
	generationSeparator = "_"

	clusterNameMaxLen = 63  // BRIDGE_CLUSTER_NAME is a DNS label
	namespaceMaxLen   = 63  // Kubernetes namespaces are RFC 1123 labels
	saNameMaxLen      = 253 // Kubernetes object-name limit
)

// dnsLabel is the pattern of the cluster name and of the ServiceAccount
// namespace and name (CRD pattern and BRIDGE_CLUSTER_NAME validation).
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

var generationPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// ErrInvalidIdentity is returned by IdentityName, RoleID and UserID when
// an input is not a DNS label of the allowed length. The naming guarantees
// rest on dot-free, lower-case inputs, and a namespace longer than 63
// characters could move the truncation cut out of the ServiceAccount name
// (see IdentityName), so such inputs are refused instead of named.
var ErrInvalidIdentity = errors.New("identity cannot be mapped to a Nexus name")

// IdentityName computes the deterministic name of the given identity:
// "bridge-<cluster>.<saNamespace>.<saName>". It is the Nexus role id of
// the identity and the stem of each of its user ids.
//
// Injectivity (ADR-0018, ADR-0031). Every input is a DNS label (checked),
// so no field contains a dot, and the dots split a natural name
// unambiguously. When the natural name exceeds the identity-name budget
// (NameCap minus the generation suffix, 173 characters), the function keeps
// "bridge-<cluster>.", cuts "<ns>.<sa>" to fit and appends '.' plus the
// first 16 hex digits of the SHA-256 of the natural name. With a cluster
// name and a namespace of at most 63 characters each (checked), at least
// 85 characters remain for "<ns>.<sa>", so the cut always falls inside the
// ServiceAccount name and keeps at least one of its characters: a
// truncated name has exactly three dots after "bridge-", a natural name
// exactly two, and the two sets are disjoint. Two truncated names differ
// by their digest (64 bits).
func IdentityName(cluster, saNamespace, saName string) (string, error) {
	if err := validateIdentity(cluster, saNamespace, saName); err != nil {
		return "", err
	}
	full := namePrefix + cluster + "." + saNamespace + "." + saName
	if len(full) <= identityNameCap {
		return full, nil
	}
	prefix := ClusterPrefix(cluster)
	budget := identityNameCap - len(prefix) - 1 - hashSuffixLen
	mid := (saNamespace + "." + saName)[:budget]
	// A cut can end in a hyphen of the ServiceAccount name; trim it so the
	// digest is not preceded by "-.". The cut lies at least 21 characters
	// into the ServiceAccount name, whose first character is alphanumeric,
	// so trimming never reaches the dot before it.
	mid = strings.TrimRight(mid, "-")
	return prefix + mid + "." + hashOf(full), nil
}

// RoleID returns the id of the Nexus role that holds the identity's
// repository privileges. It is the identity name: the role persists across
// password rotations, only the user is replaced.
func RoleID(cluster, saNamespace, saName string) (string, error) {
	return IdentityName(cluster, saNamespace, saName)
}

// UserID returns the id of the identity's Nexus user for the given
// generation (NewGeneration): "<identity name>_<generation>".
func UserID(cluster, saNamespace, saName, generation string) (string, error) {
	if !generationPattern.MatchString(generation) {
		return "", fmt.Errorf("%w: generation %q is not %d lower-case hex digits", ErrInvalidIdentity, generation, GenerationLen)
	}
	identity, err := IdentityName(cluster, saNamespace, saName)
	if err != nil {
		return "", err
	}
	return identity + generationSeparator + generation, nil
}

// NewGeneration returns a fresh random generation for UserID. A rotation
// must never reuse the generation of an earlier user of the same identity:
// a user id that existed before may still have a docker bearer token in
// Nexus's API key store, which would become valid again (ADR-0033). 64
// random bits make a repeat negligible.
func NewGeneration() (string, error) {
	var b [GenerationLen / 2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate user generation: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ParseUserID splits a user id built by UserID into its identity name and
// generation. ok is false for any other id, including role ids.
func ParseUserID(userID string) (identity, generation string, ok bool) {
	i := strings.LastIndex(userID, generationSeparator)
	if i < 0 {
		return "", "", false
	}
	identity, generation = userID[:i], userID[i+1:]
	if !generationPattern.MatchString(generation) || !strings.HasPrefix(identity, namePrefix) ||
		strings.Contains(identity, generationSeparator) || len(userID) > NameCap {
		return "", "", false
	}
	return identity, generation, true
}

// ClusterPrefix returns the ownership prefix of the given cluster:
// "bridge-<cluster>.". Users and roles whose ids do not begin with it are
// not managed by this bridge.
func ClusterPrefix(cluster string) string {
	return namePrefix + cluster + "."
}

// OwnsName reports whether a Nexus user or role id lies in the given
// cluster's ownership prefix. The bridge MUST NOT modify or delete a user
// or role for which it returns false. Like harbor.OwnsRobot the prefix is
// dot-terminated and the cluster name dot-free, so "bridge-prod." is no
// prefix of cluster "prod-eu"'s names (ADR-0018). The comparison is
// case-sensitive: Nexus's user search matches ids case-insensitively
// (ADR-0033), the ownership check does not. The ownership markers on the
// user and the role (ADR-0033) remain the second layer.
func OwnsName(cluster, id string) bool {
	if cluster == "" {
		return false
	}
	return strings.HasPrefix(id, ClusterPrefix(cluster))
}

func validateIdentity(cluster, saNamespace, saName string) error {
	for _, f := range []struct {
		what, value string
		max         int
	}{
		{"cluster name", cluster, clusterNameMaxLen},
		{"ServiceAccount namespace", saNamespace, namespaceMaxLen},
		{"ServiceAccount name", saName, saNameMaxLen},
	} {
		if len(f.value) == 0 || len(f.value) > f.max || !dnsLabel.MatchString(f.value) {
			return fmt.Errorf("%w: %s %q is not a DNS label of at most %d characters", ErrInvalidIdentity, f.what, f.value, f.max)
		}
	}
	return nil
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:hashSuffixLen]
}

// Format is the Nexus repository format a NexusAccess grants.
type Format string

// Formats the bridge grants. Nexus 3.94 and later serve OCI repositories
// in a format of their own, with their own privileges.
const (
	FormatDocker Format = "docker"
	FormatOCI    Format = "oci"
)

// Access is the permission a NexusAccess grants on a repository, in the
// same terms as HarborAccess.
type Access string

// Access values.
const (
	AccessPull     Access = "pull"
	AccessPush     Access = "push"
	AccessPullPush Access = "pull,push"
)

// repositoryNamePattern is Nexus's own rule for repository names (the
// name pattern of AbstractApiRepository in the Nexus REST API). It
// rejects in particular "*", which a repository-view privilege reads as
// "every repository".
var repositoryNamePattern = regexp.MustCompile(`^[a-zA-Z0-9-][a-zA-Z0-9_.-]*$`)

// RepositoryNameMaxLen bounds a repository name so that every privilege
// name derived from it (RepositoryPrivileges) has at most 200 characters,
// the width of Nexus's user and role id columns. The width Nexus allows a
// role's privilege reference is not verified; 200 is the conservative
// assumption.
const RepositoryNameMaxLen = 200 - len("nx-repository-view-docker--browse")

// ValidateRepositoryName reports whether name can be a Nexus repository
// the bridge grants access to.
func ValidateRepositoryName(name string) error {
	if len(name) == 0 || len(name) > RepositoryNameMaxLen || !repositoryNamePattern.MatchString(name) {
		return fmt.Errorf("repository %q is not a Nexus repository name (letters, digits, '-', then also '_' and '.'; at most %d characters; no wildcards)", name, RepositoryNameMaxLen)
	}
	return nil
}

// privilegeActions maps an access value to the actions of Nexus's built-in
// repository-view privileges (nx-repository-view-<format>-<repo>-<action>),
// least privilege as verified with crane against Nexus 3.76.1 (ADR-0033):
// read alone serves pulls, tag listings and the catalog; a push needs add
// and edit (without edit even a new image fails) plus read, so, as with
// Harbor, a pusher declares "pull,push". browse (UI browsing) and delete
// are never granted.
var privilegeActions = map[Access][]string{
	AccessPull:     {"read"},
	AccessPush:     {"add", "edit"},
	AccessPullPush: {"add", "edit", "read"},
}

// RepositoryPrivileges returns the names of the built-in repository-view
// privileges that grant access on the repository, sorted. Nexus creates
// these privileges with the repository and removes them (and strips them
// from every role) when the repository is deleted; GET
// /v1/security/privileges/{name} answers 404 for a repository that does
// not exist (ADR-0033). The repository name is used verbatim: privilege
// names are case-sensitive.
func RepositoryPrivileges(format Format, repository string, access Access) ([]string, error) {
	if format != FormatDocker && format != FormatOCI {
		return nil, fmt.Errorf("repository format %q is not one of %q, %q", format, FormatDocker, FormatOCI)
	}
	if err := ValidateRepositoryName(repository); err != nil {
		return nil, err
	}
	actions, ok := privilegeActions[access]
	if !ok {
		return nil, fmt.Errorf("access %q is not one of %q, %q, %q", access, AccessPull, AccessPush, AccessPullPush)
	}
	out := make([]string, 0, len(actions))
	for _, a := range actions {
		out = append(out, "nx-repository-view-"+string(format)+"-"+repository+"-"+a)
	}
	sort.Strings(out)
	return out, nil
}
