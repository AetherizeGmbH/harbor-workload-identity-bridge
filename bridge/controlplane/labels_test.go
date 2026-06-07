// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import "testing"

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
