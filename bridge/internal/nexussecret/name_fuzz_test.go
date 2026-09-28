// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package nexussecret

import (
	"regexp"
	"testing"

	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/robotsecret"
)

var (
	dnsLabel     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	dnsSubdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

// Two NexusAccess objects never share a Secret, a NexusAccess never shares
// one with a HarborAccess (the cross-kind case of ADR-0033 decision e),
// and the name of an admitted NexusAccess's Secret leads back to it.
func FuzzNexusSecretName_Injective(f *testing.F) {
	f.Add("a", "b.c", "a.b", "c")
	f.Add("team-a", "x", "team", "a-x")
	f.Add("nexus", "team.app", "team", "app")
	f.Fuzz(func(t *testing.T, ns1, n1, ns2, n2 string) {
		valid := func(ns, n string) bool {
			return len(ns) <= 63 && dnsLabel.MatchString(ns) && len(n) <= 63 && dnsSubdomain.MatchString(n)
		}
		if !valid(ns1, n1) || !valid(ns2, n2) {
			return
		}
		if (ns1 != ns2 || n1 != n2) && Name(ns1, n1) == Name(ns2, n2) {
			t.Fatalf("NexusAccess %s/%s and %s/%s share Secret %q", ns1, n1, ns2, n2, Name(ns1, n1))
		}
		if Name(ns1, n1) == robotsecret.Name(ns2, n2) {
			t.Fatalf("NexusAccess %s/%s and HarborAccess %s/%s share Secret %q", ns1, n1, ns2, n2, Name(ns1, n1))
		}
		if ns, n, ok := ParseName(Name(ns1, n1)); !ok || ns != ns1 || n != n1 {
			t.Fatalf("ParseName(Name(%q, %q)) = %q, %q, %v", ns1, n1, ns, n, ok)
		}
	})
}
