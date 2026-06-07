// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"strings"
	"testing"
)

func TestRobotSecretNameFor(t *testing.T) {
	// Natural (non-truncated) dot form.
	if got, want := robotSecretNameFor("team-a", "flux-access"), "robot-team-a.flux-access"; got != want {
		t.Errorf("robotSecretNameFor = %q, want %q", got, want)
	}
	// Overflow: a name past the 253-char k8s limit must be hash-truncated,
	// stay within the limit, keep the prefix, and be deterministic.
	long := strings.Repeat("z", 300)
	got := robotSecretNameFor("team", long)
	if len(got) > secretNameMax {
		t.Errorf("len(%q) = %d, want <= %d", got, len(got), secretNameMax)
	}
	if !strings.HasPrefix(got, SecretNamePrefix) {
		t.Errorf("truncated name lost its %q prefix: %q", SecretNamePrefix, got)
	}
	if got2 := robotSecretNameFor("team", long); got != got2 {
		t.Errorf("not deterministic: %q vs %q", got, got2)
	}
	// Distinct long inputs that share a truncation prefix must still differ
	// (the hash suffix carries the uniqueness).
	a := robotSecretNameFor("team", strings.Repeat("a", 300))
	b := robotSecretNameFor("team", strings.Repeat("b", 300))
	if a == b {
		t.Errorf("distinct long inputs produced identical Secret names: %q", a)
	}
}

func TestRobotDescription_RoundTrip(t *testing.T) {
	desc := RobotDescription("prod-eu-west", "harbor-bridge-system", "flux-access")
	if !RobotBelongsToCluster(desc, "prod-eu-west") {
		t.Errorf("description %q not recognised as belonging to its own cluster", desc)
	}
	ns, name, ok := ParseRobotDescription(desc)
	if !ok {
		t.Fatalf("ParseRobotDescription returned !ok for %q", desc)
	}
	if ns != "harbor-bridge-system" || name != "flux-access" {
		t.Errorf("Parse returned (%q,%q), want (harbor-bridge-system,flux-access)", ns, name)
	}
}

func TestRobotBelongsToCluster_RejectsWrongCluster(t *testing.T) {
	// Defense-in-depth (ADR-0009/ADR-0012): cluster "prod-eu"'s robot has
	// cluster="prod-eu" in its description, so cluster "prod" must see it as
	// foreign. Since ADR-0018 the name prefix "bridge-prod." already excludes
	// "bridge-prod-eu.…" (the dot terminator); this description check is the
	// second, independent layer.
	foreignDesc := RobotDescription("prod-eu", "harbor-bridge-system", "flux-access")
	if RobotBelongsToCluster(foreignDesc, "prod") {
		t.Errorf("description %q (cluster=prod-eu) wrongly claimed by cluster=prod", foreignDesc)
	}
}

func TestRobotBelongsToCluster_RejectsForeignDescription(t *testing.T) {
	// A robot whose description we did not write must never be claimed,
	// even if it happens to contain "cluster=foo" inside other text.
	cases := []string{
		"",
		"managed by someone else cluster=prod",
		"cluster=prod managed-by=something-else",
		"randomtext",
	}
	for _, d := range cases {
		if RobotBelongsToCluster(d, "prod") {
			t.Errorf("description %q wrongly accepted as ours", d)
		}
	}
}

func TestParseRobotDescription_RejectsForeign(t *testing.T) {
	tag := robotDescriptionTag
	cases := []string{
		"",
		"managed-by=other harboraccess=foo/bar",
		tag + " cluster=prod", // missing harboraccess
		tag + " cluster=prod harboraccess=just-name", // no slash
		tag + " cluster=prod harboraccess=/no-ns",
		tag + " cluster=prod harboraccess=ns/",
	}
	for _, d := range cases {
		if _, _, ok := ParseRobotDescription(d); ok {
			t.Errorf("ParseRobotDescription wrongly accepted %q", d)
		}
	}
}
