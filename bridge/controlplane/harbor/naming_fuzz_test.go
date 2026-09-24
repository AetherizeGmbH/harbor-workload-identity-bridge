// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package harbor

import (
	"regexp"
	"testing"
)

// dnsLabel is the pattern the CRD and BRIDGE_CLUSTER_NAME enforce on the
// cluster name and the ServiceAccount namespace and name.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// The robot name is a multi-tenancy boundary (ADR-0018, audit F2): two
// different identities must never share a robot, and no cluster may
// claim another cluster's robot.
func FuzzRobotName_Injective(f *testing.F) {
	f.Add("c", "a", "b-x", "c", "a-b", "x")
	f.Add("prod", "ns", "sa", "prod-eu", "ns", "sa")
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
