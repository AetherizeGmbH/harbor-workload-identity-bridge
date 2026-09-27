// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package harbor

import (
	"errors"
	"regexp"
	"testing"
)

// dnsLabel is the pattern the CRD and BRIDGE_CLUSTER_NAME enforce on the
// cluster name and the ServiceAccount namespace and name.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// The robot name is a multi-tenancy boundary (ADR-0018, audit F2): two
// different identities must never share a robot, and no cluster may
// claim another cluster's robot. Every name RobotName returns must also be
// one Harbor accepts; identities that cannot have one (DNS labels with
// "--") must get ErrInvalidRobotName, never a name Harbor refuses.
func FuzzRobotName_Injective(f *testing.F) {
	f.Add("c", "a", "b-x", "c", "a-b", "x")
	f.Add("prod", "ns", "sa", "prod-eu", "ns", "sa")
	f.Add("c", "team--a", "sa", "c", "team", "a-sa")
	f.Add("prod--eu", "ns", "sa", "prod", "ns", "build--runner")
	f.Fuzz(func(t *testing.T, c1, ns1, sa1, c2, ns2, sa2 string) {
		valid := func(c, ns, sa string) bool {
			return len(c) <= 63 && dnsLabel.MatchString(c) &&
				len(ns) <= 63 && dnsLabel.MatchString(ns) &&
				len(sa) <= 253 && dnsLabel.MatchString(sa)
		}
		if !valid(c1, ns1, sa1) || !valid(c2, ns2, sa2) {
			return
		}
		n1, err1 := RobotName(c1, ns1, sa1)
		n2, err2 := RobotName(c2, ns2, sa2)
		for _, err := range []error{err1, err2} {
			if err != nil && !errors.Is(err, ErrInvalidRobotName) {
				t.Fatalf("RobotName on valid DNS labels: unexpected error %v", err)
			}
		}
		if err1 == nil && !IsValidHarborRobotName(n1) {
			t.Fatalf("RobotName(%q,%q,%q) = %q, which Harbor refuses", c1, ns1, sa1, n1)
		}
		if err1 != nil || err2 != nil {
			return
		}
		if !OwnsRobot(c1, n1) {
			t.Fatalf("cluster %q does not own its own robot %q", c1, n1)
		}
		if c1 != c2 && OwnsRobot(c1, n2) {
			t.Fatalf("cluster %q claims robot %q of cluster %q", c1, n2, c2)
		}
		if (c1 != c2 || ns1 != ns2 || sa1 != sa2) && n1 == n2 {
			t.Fatalf("(%q,%q,%q) and (%q,%q,%q) both map to %q", c1, ns1, sa1, c2, ns2, sa2, n1)
		}
	})
}
