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
	want := "bridge-prod-eu-west-flux-system-source-controller"
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
	// prefix = "bridge-prod-" (12)
	// remainder budget = 240 - 12 = 228
	// "<ns>-<sa>" must total 228 chars.
	ns := strings.Repeat("n", 100)
	sa := strings.Repeat("s", 127) // 100 + 1 (-) + 127 = 228
	got, err := RobotName("prod", ns, sa)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != RobotNameCap {
		t.Errorf("len(RobotName) = %d, want exactly %d", len(got), RobotNameCap)
	}
	// The result should NOT include a hash suffix since we did not truncate.
	if strings.Count(got, "-") != 3 {
		// bridge-prod-<ns>-<sa> has 3 hyphens (between bridge|prod, prod|ns, ns|sa)
		t.Errorf("RobotName %q has unexpected hyphen count, expected non-truncated form", got)
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
	if !strings.HasPrefix(a, "bridge-prod-") {
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
	// prefix length: 7 ("bridge-") + 4 ("prod") + 1 ("-") = 12
	// budget = 240 - 12 - 1 - 16 = 211
	// We want mid[211-1:] to begin with a separator so trimming changes the result.
	// Pick ns of exactly 211 chars; sa contributes nothing in the truncated form.
	ns := strings.Repeat("x", 210) + "-" // 211 chars, ends with "-"
	sa := strings.Repeat("y", 50)
	full := "bridge-prod-" + ns + "-" + sa
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
		{"prod", "bridge-prod-flux-system-source-controller"},
		{"prod-eu-west", "bridge-prod-eu-west-flux-system-source-controller"},
		{"a", "bridge-a-b"}, // minimum-length both sides

		// Harbor adds "robot$" to system-level robot names on read paths
		// (GET /robots and GET /robots/{id}). POST /robots accepts the
		// un-prefixed form we send. So both forms must match — this is
		// the bug discovered in the first manual e2e: GetByName was
		// looking for "bridge-..." but Harbor returned "robot$bridge-...",
		// and every subsequent reconcile re-took the create branch and
		// got 409.
		{"prod", "robot$bridge-prod-flux-system-source-controller"},
		{"prod-eu-west", "robot$bridge-prod-eu-west-flux"},
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
		{"prod", "bridge-staging-flux"},          // different cluster
		{"prod", "robot$bridge-staging-flux"},    // ditto, with Harbor's prefix
		{"prod", "bridgeXprod-flux"},             // different separator
		{"prod", "bridge-production-flux"},       // longer cluster name, no hyphen boundary
		{"prod", "robot$bridge-production-flux"}, // ditto with prefix
		{"prod", "bridge-prod"},                  // missing trailing "-"
		{"prod", ""},                             // empty robot name
		{"", "bridge-prod-flux"},                 // empty cluster — must refuse to claim anything
	}
	for _, c := range cases {
		if OwnsRobot(c.cluster, c.robot) {
			t.Errorf("OwnsRobot(%q, %q) = true, want false", c.cluster, c.robot)
		}
	}
}

// TestOwnsRobot_DocumentsPrefixCollisionLimitation pins down the known
// false-positive class flagged in ADR-0009: cluster "prod" considers cluster
// "prod-eu"'s robots its own because "bridge-prod-eu-..." DOES start with
// "bridge-prod-". This is operator-responsibility per ADR-0009 ("cluster
// names must not be hyphen-prefixes of each other"). The test pins the
// behaviour so a future change has to acknowledge the trade-off explicitly.
func TestOwnsRobot_DocumentsPrefixCollisionLimitation(t *testing.T) {
	// Cluster "prod" sees a robot intended for cluster "prod-eu" as its own.
	got := OwnsRobot("prod", "bridge-prod-eu-flux-system-source-controller")
	if !got {
		t.Errorf(
			"BEHAVIOUR CHANGE: OwnsRobot no longer reports the documented " +
				"prefix-collision case as true. Update ADR-0009 and this test.",
		)
	}
}

func TestClusterPrefix(t *testing.T) {
	if got := ClusterPrefix("prod-eu-west"); got != "bridge-prod-eu-west-" {
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
