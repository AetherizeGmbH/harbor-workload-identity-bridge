// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package harbor

import (
	"errors"
	"strings"
	"testing"
)

func TestRobotName_HappyPath(t *testing.T) {
	got, err := RobotName("prod-eu-west", "flux-system", "source-controller")
	if err != nil {
		t.Fatal(err)
	}
	want := "bridge-prod-eu-west.flux-system.source-controller"
	if got != want {
		t.Errorf("RobotName = %q, want %q", got, want)
	}
	if !IsValidHarborRobotName(got) {
		t.Errorf("RobotName %q is not valid per Harbor regex", got)
	}
}

func TestRobotName_LengthExactlyAtCap(t *testing.T) {
	// Construct inputs that produce exactly RobotNameCap chars: avoids
	// hitting the truncation branch.
	// prefix = "bridge-prod." (12)
	// remainder budget = 240 - 12 = 228
	// "<ns>.<sa>" must total 228 chars.
	ns := strings.Repeat("n", 100)
	sa := strings.Repeat("s", 127) // 100 + 1 (.) + 127 = 228
	got, err := RobotName("prod", ns, sa)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != RobotNameCap {
		t.Errorf("len(RobotName) = %d, want exactly %d", len(got), RobotNameCap)
	}
	// The natural (non-truncated) form is exactly "bridge-prod.<ns>.<sa>":
	// one hyphen (in the "bridge-" prefix) and two dot delimiters, no hash.
	if want := "bridge-prod." + ns + "." + sa; got != want {
		t.Errorf("RobotName = %q, want non-truncated dot form %q", got, want)
	}
}

func TestRobotName_TruncatesDeterministically(t *testing.T) {
	// SA name long enough to force truncation.
	longSA := strings.Repeat("z", 300)
	a, err := RobotName("prod", "flux-system", longSA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := RobotName("prod", "flux-system", longSA)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("RobotName not deterministic: %q vs %q", a, b)
	}
	if len(a) > RobotNameCap {
		t.Errorf("truncated name exceeds cap: %d > %d", len(a), RobotNameCap)
	}
	if !strings.HasPrefix(a, "bridge-prod.") {
		t.Errorf("truncated name lost its prefix: %q", a)
	}
	if !IsValidHarborRobotName(a) {
		t.Errorf("truncated name %q is invalid per Harbor regex", a)
	}
}

func TestRobotName_DifferentInputsProduceDifferentNames(t *testing.T) {
	// Two distinct very-long SA names must produce distinct truncated
	// robot names (so the hash actually contributes uniqueness).
	a, err := RobotName("prod", "flux-system", strings.Repeat("a", 300))
	if err != nil {
		t.Fatal(err)
	}
	b, err := RobotName("prod", "flux-system", strings.Repeat("b", 300))
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Errorf("distinct inputs produced identical robot names: %q", a)
	}
}

// TestRobotName_DotDelimiterIsInjective pins ADR-0018's core property: inputs
// that collide under a '-' join must NOT collide under the '.' join. These are
// exactly the F2 collision pairs ("a-b"/"c" vs "a"/"b-c"); under the old dash
// scheme several of these mapped to the same robot name.
func TestRobotName_DotDelimiterIsInjective(t *testing.T) {
	pairs := []struct{ ns, sa string }{
		{"a-b", "c"}, {"a", "b-c"},
		{"team-a", "svc"}, {"team", "a-svc"},
		{"x", "y-z-w"}, {"x-y", "z-w"}, {"x-y-z", "w"},
	}
	seen := map[string]string{} // robot name -> "ns/sa" that produced it
	for _, p := range pairs {
		name, err := RobotName("prod", p.ns, p.sa)
		if err != nil {
			t.Fatalf("RobotName(prod, %q, %q): %v", p.ns, p.sa, err)
		}
		key := p.ns + "/" + p.sa
		if prev, ok := seen[name]; ok && prev != key {
			t.Errorf("collision: (%s) and (%s) both map to robot name %q", prev, key, name)
		}
		seen[name] = key
	}
}

// Cluster, namespace and ServiceAccount names are DNS labels, which may
// contain consecutive hyphens; Harbor refuses robot names with doubled
// separators (validateName). RobotName must say so with a permanent error
// instead of returning a name Harbor answers with 400 on every retry.
func TestRobotName_RejectsNamesHarborRefuses(t *testing.T) {
	for _, in := range [][3]string{
		{"prod", "team--a", "sa"},
		{"prod", "ns", "build--runner"},
		{"prod--eu", "ns", "sa"},
		{"prod", "xn--80ak6aa92e", "sa"}, // punycode namespace
		{"prod", "ns", strings.Repeat("s", 150) + "--" + strings.Repeat("t", 100)}, // "--" in the kept part of a truncated name
	} {
		name, err := RobotName(in[0], in[1], in[2])
		if !errors.Is(err, ErrInvalidRobotName) {
			t.Errorf("RobotName(%q, %q, %q) = %q, %v; want ErrInvalidRobotName", in[0], in[1], in[2], name, err)
		}
	}
	// A "--" that truncation cuts off does not reach Harbor.
	sa := strings.Repeat("s", 250) + "--x"
	name, err := RobotName("prod", "ns", sa)
	if err != nil || !IsValidHarborRobotName(name) {
		t.Errorf("RobotName with \"--\" only in the truncated-away part = %q, %v; want a valid name", name, err)
	}
}

// TestRobotName_TruncatedNamesAreDisjointFromNaturalNames pins the property
// the overflow path relies on (see RobotName): with real input lengths
// (cluster and namespace at most 63 characters) the cut falls inside the
// dot-free SA name, so a truncated name has three dots after "bridge-" and
// a natural name exactly two.
func TestRobotName_TruncatedNamesAreDisjointFromNaturalNames(t *testing.T) {
	for _, in := range [][3]string{
		{strings.Repeat("c", 63), strings.Repeat("n", 63), strings.Repeat("s", 253)},
		{"c", "n", strings.Repeat("s", 253)},
		{"prod", strings.Repeat("n", 63), strings.Repeat("s", 175)},
	} {
		name, err := RobotName(in[0], in[1], in[2])
		if err != nil {
			t.Fatal(err)
		}
		natural := "bridge-" + in[0] + "." + in[1] + "." + in[2]
		if len(natural) <= RobotNameCap {
			t.Fatalf("test premise: %d-character natural name is not truncated", len(natural))
		}
		if got := strings.Count(name, "."); got != 3 {
			t.Errorf("truncated name %q has %d dots, want 3 (natural names have 2)", name, got)
		}
		if !strings.HasPrefix(name, "bridge-"+in[0]+"."+in[1]+".") {
			t.Errorf("truncated name %q does not keep the cluster and namespace fields", name)
		}
	}
}

func TestRobotName_ClusterNameTooLong(t *testing.T) {
	// A 230-char cluster name leaves no room for the ns/sa even with the
	// hash. Note: real configurations cap clusterName at 63 chars via the
	// config validator, so this scenario is defensive only.
	hugeCluster := strings.Repeat("c", 230)
	_, err := RobotName(hugeCluster, "ns", "sa")
	if err == nil {
		t.Fatal("expected error for too-long cluster")
	}
	if !errors.Is(err, ErrClusterNameTooLong) {
		t.Errorf("error not ErrClusterNameTooLong: %v", err)
	}
}

func TestRobotName_TrailingSeparatorTrimmed(t *testing.T) {
	// Construct a case where, after truncation, the mid section would end
	// with a separator. We need ns+"-"+sa whose budget cut lands on the "-".
	// Easier: use a separator-containing name that hits the boundary.
	cluster := "prod"
	// prefix length: 7 ("bridge-") + 4 ("prod") + 1 (".") = 12
	// budget = 240 - 12 - 1 - 16 = 211
	// We want mid[211-1:] to begin with a separator so trimming changes the result.
	// Pick ns of exactly 211 chars; sa contributes nothing in the truncated form.
	ns := strings.Repeat("x", 210) + "-" // 211 chars, ends with "-"
	sa := strings.Repeat("y", 50)
	full := "bridge-prod." + ns + "." + sa
	if len(full) <= RobotNameCap {
		t.Fatalf("test premise: full name %d <= cap %d, won't truncate", len(full), RobotNameCap)
	}
	got, err := RobotName(cluster, ns, sa)
	if err != nil {
		t.Fatal(err)
	}
	// The result MUST be valid per Harbor regex (no trailing or doubled separators).
	if !IsValidHarborRobotName(got) {
		t.Errorf("truncated name %q failed Harbor regex", got)
	}
}

func TestOwnsRobot_PositiveCases(t *testing.T) {
	cases := []struct {
		cluster string
		robot   string
	}{
		{"prod", "bridge-prod.flux-system.source-controller"},
		{"prod-eu-west", "bridge-prod-eu-west.flux-system.source-controller"},
		{"a", "bridge-a.b"}, // minimum-length both sides
	}
	for _, c := range cases {
		if !OwnsRobot(c.cluster, c.robot) {
			t.Errorf("OwnsRobot(%q, %q) = false, want true", c.cluster, c.robot)
		}
	}
}

func TestOwnsRobot_NegativeCases(t *testing.T) {
	cases := []struct {
		cluster string
		robot   string
	}{
		{"prod", "bridge-staging-flux"},    // different cluster
		{"prod", "bridgeXprod-flux"},       // different separator
		{"prod", "bridge-production-flux"}, // longer cluster name, no hyphen boundary
		{"prod", "bridge-prod"},            // missing trailing "."
		{"prod", ""},                       // empty robot name
		{"", "bridge-prod-flux"},           // empty cluster — must refuse to claim anything
	}
	for _, c := range cases {
		if OwnsRobot(c.cluster, c.robot) {
			t.Errorf("OwnsRobot(%q, %q) = true, want false", c.cluster, c.robot)
		}
	}
}

// TestOwnsRobot_DotDelimiterFixesPrefixCollision pins the ADR-0018 fix for the
// hyphen-prefix false-positive that ADR-0009 had to push onto operators:
// cluster "prod" used to consider cluster "prod-eu"'s robots its own because
// "bridge-prod-eu-..." started with "bridge-prod-". With the dot-terminated
// ownership prefix "bridge-prod.", a robot named for cluster "prod-eu" is
// "bridge-prod-eu.flux..." — the char after "bridge-prod" is '-', not the
// required '.', so it is correctly NOT owned by "prod".
func TestOwnsRobot_DotDelimiterFixesPrefixCollision(t *testing.T) {
	if OwnsRobot("prod", "bridge-prod-eu.flux-system.source-controller") {
		t.Error("OwnsRobot(\"prod\", prod-eu's robot) = true; dot delimiter should make it false (ADR-0018)")
	}
	// And the bridge still owns its own cluster's robot.
	if !OwnsRobot("prod", "bridge-prod.flux-system.source-controller") {
		t.Error("OwnsRobot(\"prod\", prod's own robot) = false; want true")
	}
}

// TestOwnsRobot_TakesInternalNamesOnly pins ADR-0023: the Harbor client
// strips Harbor's robot_name_prefix on every read path, and ownership is
// decided on the internal name only. On-wire names ("robot$bridge-…") are
// not claimed: stripping a hard-coded "robot$" here made a bridge whose
// prefix did not match Harbor's claim, and the janitor delete, robots it
// could not otherwise recognise.
func TestOwnsRobot_TakesInternalNamesOnly(t *testing.T) {
	for _, wire := range []string{
		"robot$bridge-prod.flux-system.source-controller",
		"robot_bridge-prod.flux-system.source-controller",
	} {
		if OwnsRobot("prod", wire) {
			t.Errorf("OwnsRobot(\"prod\", %q) = true; on-wire names must not be claimed", wire)
		}
		if legacy := strings.ReplaceAll(wire, ".", "-"); OwnsLegacyRobot("prod", legacy) {
			t.Errorf("OwnsLegacyRobot(\"prod\", %q) = true; on-wire names must not be claimed", legacy)
		}
	}
}

func TestClusterPrefix(t *testing.T) {
	if got := ClusterPrefix("prod-eu-west"); got != "bridge-prod-eu-west." {
		t.Errorf("ClusterPrefix = %q", got)
	}
}

func TestIsValidHarborRobotName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"bridge-prod-flux-system-source-controller", true},
		{"bridge.prod.flux", true}, // dot separators legal
		{"bridge_prod_flux", true}, // underscore separators legal
		{"a", true},                // single char
		{"a1b2c3", true},
		{"-bridge", false},      // leading separator
		{"bridge-", false},      // trailing separator
		{"bridge--prod", false}, // doubled separator
		{"Bridge-prod", false},  // uppercase
		{"bridge-prod-flux_sys.tem-controller", true},
		{"bridge-prod-flux$controller", false}, // illegal char
	}
	for _, c := range cases {
		if got := IsValidHarborRobotName(c.name); got != c.ok {
			t.Errorf("IsValidHarborRobotName(%q) = %v, want %v", c.name, got, c.ok)
		}
	}
}

func TestOwnsLegacyRobot(t *testing.T) {
	cases := []struct {
		cluster, name string
		want          bool
	}{
		{"prod", "bridge-prod-ns-sa", true},
		{"prod", "robot$bridge-prod-ns-sa", false}, // on-wire name: the client strips the prefix first
		{"prod", "bridge-prod-eu-ns-sa", true},     // weak by design: the description tag must decide
		{"prod", "bridge-prod.ns.sa", false},       // current scheme, not legacy
		{"prod", "bridge-prod-eu.ns.sa", false},
		{"prod", "bridge-prod-", false},
		{"prod", "bridge-staging-ns-sa", false},
		{"", "bridge--ns-sa", false},
	}
	for _, tc := range cases {
		if got := OwnsLegacyRobot(tc.cluster, tc.name); got != tc.want {
			t.Errorf("OwnsLegacyRobot(%q, %q) = %v, want %v", tc.cluster, tc.name, got, tc.want)
		}
	}
}

func TestValidateProjectName(t *testing.T) {
	for _, ok := range []string{"a", "production", "team-a", "team_a", "team.a", "a1-b2.c3_d4", strings.Repeat("a", ProjectNameMaxLen)} {
		if err := ValidateProjectName(ok); err != nil {
			t.Errorf("%q: unexpected error %v", ok, err)
		}
	}
	for _, bad := range []string{"", "*", "prod*", "Prod", "a..b", "-a", "a-", "a/b", "a b", strings.Repeat("a", ProjectNameMaxLen+1)} {
		if err := ValidateProjectName(bad); err == nil {
			t.Errorf("%q: accepted, want an error", bad)
		}
	}
}
