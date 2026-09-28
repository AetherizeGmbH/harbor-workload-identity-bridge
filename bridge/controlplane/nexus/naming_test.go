// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package nexus

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestIdentityName_Natural(t *testing.T) {
	got, err := IdentityName("prod-eu-west", "flux-system", "source-controller")
	if err != nil {
		t.Fatal(err)
	}
	if want := "bridge-prod-eu-west.flux-system.source-controller"; got != want {
		t.Errorf("IdentityName = %q, want %q", got, want)
	}
	role, err := RoleID("prod-eu-west", "flux-system", "source-controller")
	if err != nil || role != got {
		t.Errorf("RoleID = %q, %v; want the identity name %q", role, err, got)
	}
}

// The dash join of ADR-0009 collided ("team-a"/"svc" vs "team"/"a-svc");
// the dot join does not (ADR-0018).
func TestIdentityName_DotDelimiterIsInjective(t *testing.T) {
	a, err := IdentityName("c", "team-a", "svc")
	if err != nil {
		t.Fatal(err)
	}
	b, err := IdentityName("c", "team", "a-svc")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("distinct identities share %q", a)
	}
}

func TestIdentityName_LengthExactlyAtCap(t *testing.T) {
	// "bridge-prod." is 12 characters; "<ns>.<sa>" gets the rest of 173.
	ns := strings.Repeat("n", 63)
	sa := strings.Repeat("s", identityNameCap-12-63-1)
	got, err := IdentityName("prod", ns, sa)
	if err != nil {
		t.Fatal(err)
	}
	if got != "bridge-prod."+ns+"."+sa || len(got) != identityNameCap {
		t.Errorf("IdentityName = %q (%d characters), want the natural name of exactly %d", got, len(got), identityNameCap)
	}
}

// Past the cap the name keeps the cluster prefix, the namespace and the
// start of the ServiceAccount name, and ends in a digest of the natural
// name: exactly three dots after "bridge-", where a natural name has two.
func TestIdentityName_TruncatesDeterministically(t *testing.T) {
	cluster := strings.Repeat("c", 63)
	ns := strings.Repeat("n", 63)
	for _, sa := range []string{strings.Repeat("z", 253), strings.Repeat("z", 100) + "-" + strings.Repeat("y", 152), "a" + strings.Repeat("-b", 126)} {
		a, err := IdentityName(cluster, ns, sa)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := IdentityName(cluster, ns, sa)
		if a != b {
			t.Errorf("not deterministic: %q vs %q", a, b)
		}
		if len(a) > identityNameCap {
			t.Errorf("truncated name has %d characters, cap %d", len(a), identityNameCap)
		}
		if !strings.HasPrefix(a, ClusterPrefix(cluster)+ns+".") {
			t.Errorf("truncated name %q lost the cluster prefix or the namespace", a)
		}
		if n := strings.Count(a, "."); n != 3 {
			t.Errorf("truncated name %q has %d dots, want 3", a, n)
		}
		if strings.Contains(a, "-.") {
			t.Errorf("truncated name %q has a hyphen before the digest", a)
		}
		if !nameChars.MatchString(a) {
			t.Errorf("truncated name %q has characters outside [a-z0-9.-]", a)
		}
	}
	x, _ := IdentityName(cluster, ns, strings.Repeat("a", 253))
	y, _ := IdentityName(cluster, ns, strings.Repeat("a", 252)+"b")
	if x == y {
		t.Errorf("two long ServiceAccount names that differ only past the cut share %q", x)
	}
}

// A cut that ends in a hyphen of the ServiceAccount name drops it, so no
// "-." precedes the digest. With a 62-character cluster name the budget
// for "<ns>.<sa>" is 86 characters: the namespace, the dot and 22
// characters of the ServiceAccount name, whose 22nd is a hyphen here.
func TestIdentityName_TruncationTrimsAHyphenAtTheCut(t *testing.T) {
	cluster := strings.Repeat("c", 62)
	ns := strings.Repeat("n", 63)
	sa := "a" + strings.Repeat("-b", 126)
	if sa[21] != '-' {
		t.Fatalf("test setup: the ServiceAccount name's 22nd character is %q, want a hyphen", sa[21])
	}
	got, err := IdentityName(cluster, ns, sa)
	if err != nil {
		t.Fatal(err)
	}
	full := "bridge-" + cluster + "." + ns + "." + sa
	if want := "bridge-" + cluster + "." + ns + "." + sa[:21] + "." + hashOf(full); got != want {
		t.Errorf("IdentityName = %q, want %q", got, want)
	}
}

var nameChars = regexp.MustCompile(`^[a-z0-9.-]+$`)

// The naming guarantees rest on DNS-label inputs, and a namespace longer
// than a namespace can be would move the cut out of the ServiceAccount
// name (ADR-0031); such inputs are refused.
func TestIdentityName_RefusesNonLabels(t *testing.T) {
	long := strings.Repeat("a", 64)
	for _, tc := range [][3]string{
		{"", "ns", "sa"}, {"c", "", "sa"}, {"c", "ns", ""},
		{"Prod", "ns", "sa"}, {"c", "Ns", "sa"}, {"c", "ns", "Sa"},
		{"c.x", "ns", "sa"}, {"c", "ns.x", "sa"}, {"c", "ns", "sa.x"},
		{"c", "ns", "sa_x"}, {"-c", "ns", "sa"}, {"c", "ns-", "sa"},
		{long, "ns", "sa"}, {"c", long, "sa"}, {"c", "ns", strings.Repeat("a", 254)},
	} {
		if got, err := IdentityName(tc[0], tc[1], tc[2]); !errors.Is(err, ErrInvalidIdentity) {
			t.Errorf("IdentityName(%q, %q, %q) = %q, %v; want ErrInvalidIdentity", tc[0], tc[1], tc[2], got, err)
		}
	}
	// Nexus accepts "--" in user ids (verified on Nexus 3.76.1), so unlike
	// harbor.RobotName such identities are named.
	if _, err := IdentityName("c", "team--a", "xn--sa"); err != nil {
		t.Errorf("IdentityName with consecutive hyphens: %v", err)
	}
}

func TestUserID_AndParse(t *testing.T) {
	gen, err := NewGeneration()
	if err != nil {
		t.Fatal(err)
	}
	if !generationPattern.MatchString(gen) {
		t.Fatalf("NewGeneration = %q", gen)
	}
	id, err := UserID("prod", "flux-system", "source-controller", gen)
	if err != nil {
		t.Fatal(err)
	}
	if want := "bridge-prod.flux-system.source-controller_" + gen; id != want {
		t.Errorf("UserID = %q, want %q", id, want)
	}
	identity, g, ok := ParseUserID(id)
	if !ok || identity != "bridge-prod.flux-system.source-controller" || g != gen {
		t.Errorf("ParseUserID(%q) = %q, %q, %v", id, identity, g, ok)
	}
	const g16 = "_0123456789abcdef"
	for _, bad := range []string{
		"", "bridge-prod.flux-system.source-controller", "bridge-prod.a.b_", "bridge-prod.a.b_0123",
		"bridge-prod.a.b_0123456789ABCDEF", "bridge-prod.a_b" + g16, "admin" + g16, g16,
		// identity parts that are not lower-case DNS labels
		"bridge-X" + g16, "bridge-prod.Team.Svc" + g16, "bridge-.." + g16, "bridge-prod..sa" + g16,
		"bridge-prod.ns.sa-" + g16, "bridge-prod.-ns.sa" + g16, "bridge-" + strings.Repeat("c", 64) + ".ns.sa" + g16,
		// neither two nor three dots, or a truncated shape without a digest
		"bridge-prod.ns" + g16, "bridge-prod.ns.sa.x.y" + g16, "bridge-prod.ns.sa.0123456789abcdeg" + g16,
		"bridge-prod.ns.sa.0123456789ABCDEF" + g16,
		// longer than UserID ever builds
		"bridge-prod.ns." + strings.Repeat("s", 170) + g16,
	} {
		if _, _, ok := ParseUserID(bad); ok {
			t.Errorf("ParseUserID(%q) accepted a name UserID never builds", bad)
		}
	}
	truncated := "bridge-prod.ns.sa.0123456789abcdef" + g16
	if identity, _, ok := ParseUserID(truncated); !ok || identity != "bridge-prod.ns.sa.0123456789abcdef" {
		t.Errorf("ParseUserID(%q) = %q, %v; want the truncated shape accepted", truncated, identity, ok)
	}
	for _, g := range []string{"", "0123", "0123456789ABCDEF", "0123456789abcdefg", "0123456789abcdeg", "../../etc/passwd"} {
		if _, err := UserID("prod", "ns", "sa", g); !errors.Is(err, ErrInvalidIdentity) {
			t.Errorf("UserID with generation %q = %v, want ErrInvalidIdentity", g, err)
		}
	}
	if _, err := UserID("Prod", "ns", "sa", gen); !errors.Is(err, ErrInvalidIdentity) {
		t.Errorf("UserID with an invalid cluster = %v", err)
	}
}

func TestUserID_LongestFitsTheCap(t *testing.T) {
	id, err := UserID(strings.Repeat("c", 63), strings.Repeat("n", 63), strings.Repeat("s", 253), "0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if len(id) > NameCap || !idPattern.MatchString(id) {
		t.Errorf("UserID = %q (%d characters), want at most %d and a valid id", id, len(id), NameCap)
	}
	if _, _, ok := ParseUserID(id); !ok {
		t.Errorf("ParseUserID rejects the longest UserID %q", id)
	}
}

func TestNewGeneration_IsRandom(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		g, err := NewGeneration()
		if err != nil {
			t.Fatal(err)
		}
		if seen[g] {
			t.Fatalf("NewGeneration repeated %q", g)
		}
		seen[g] = true
	}
}

// The dot-terminated ownership prefix separates cluster "prod" from
// "prod-eu" (ADR-0018), and the comparison is case-sensitive because
// Nexus's user search is not.
func TestOwnsName(t *testing.T) {
	cases := []struct {
		cluster, id string
		want        bool
	}{
		{"prod", "bridge-prod.ns.sa", true},
		{"prod", "bridge-prod.ns.sa_0123456789abcdef", true},
		{"prod", "bridge-prod-eu.ns.sa", false},
		{"prod", "BRIDGE-PROD.ns.sa", false},
		{"prod", "bridge-prod", false},
		{"prod", "admin", false},
		{"", "bridge-.ns.sa", false},
	}
	for _, tc := range cases {
		if got := OwnsName(tc.cluster, tc.id); got != tc.want {
			t.Errorf("OwnsName(%q, %q) = %v, want %v", tc.cluster, tc.id, got, tc.want)
		}
	}
}

func TestRepositoryPrivileges(t *testing.T) {
	cases := []struct {
		format Format
		repo   string
		access Access
		want   []string
	}{
		{FormatDocker, "apps", AccessPull, []string{"nx-repository-view-docker-apps-read"}},
		{FormatDocker, "apps", AccessPush, []string{"nx-repository-view-docker-apps-add", "nx-repository-view-docker-apps-edit"}},
		{FormatDocker, "Mixed-Case.repo_1", AccessPullPush, []string{
			"nx-repository-view-docker-Mixed-Case.repo_1-add", "nx-repository-view-docker-Mixed-Case.repo_1-edit",
			"nx-repository-view-docker-Mixed-Case.repo_1-read",
		}},
	}
	for _, tc := range cases {
		got, err := RepositoryPrivileges(tc.format, tc.repo, tc.access)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("RepositoryPrivileges(%s, %s, %s) = %v, want %v", tc.format, tc.repo, tc.access, got, tc.want)
		}
		for _, p := range got {
			if strings.HasSuffix(p, "-delete") || strings.HasSuffix(p, "-*") || strings.HasSuffix(p, "-browse") {
				t.Errorf("privilege %q grants more than a registry client needs", p)
			}
			if len(p) > maxIDLen || !idPattern.MatchString(p) {
				t.Errorf("privilege %q is not a valid id", p)
			}
		}
	}
	for _, tc := range []struct {
		format Format
		repo   string
		access Access
	}{
		{"maven2", "apps", AccessPull},
		{"oci", "apps", AccessPull}, // not analysed yet (ADR-0033)
		{"*", "apps", AccessPull},
		{FormatDocker, "*", AccessPull},
		{FormatDocker, "", AccessPull},
		{FormatDocker, ".apps", AccessPull},
		{FormatDocker, "a/b", AccessPull},
		{FormatDocker, strings.Repeat("a", RepositoryNameMaxLen+1), AccessPull},
		{FormatDocker, "apps", "delete"},
		{FormatDocker, "apps", "*"},
	} {
		if got, err := RepositoryPrivileges(tc.format, tc.repo, tc.access); err == nil {
			t.Errorf("RepositoryPrivileges(%q, %q, %q) = %v, want an error", tc.format, tc.repo, tc.access, got)
		}
	}
	longest, err := RepositoryPrivileges(FormatDocker, strings.Repeat("a", RepositoryNameMaxLen), AccessPullPush)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range longest {
		if len(p) > maxIDLen {
			t.Errorf("privilege of the longest repository name has %d characters: %q", len(p), p)
		}
	}
}
