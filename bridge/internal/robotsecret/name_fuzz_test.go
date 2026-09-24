// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package robotsecret

import (
	"regexp"
	"testing"
)

var (
	dnsLabel     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	dnsSubdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

// Two HarborAccess objects must never share a password Secret (ADR-0018).
func FuzzName_Injective(f *testing.F) {
	f.Add("a", "b.c", "a.b", "c")
	f.Add("team-a", "x", "team", "a-x")
	f.Fuzz(func(t *testing.T, ns1, n1, ns2, n2 string) {
		valid := func(ns, n string) bool {
			return len(ns) <= 63 && dnsLabel.MatchString(ns) && len(n) <= 63 && dnsSubdomain.MatchString(n)
		}
		if !valid(ns1, n1) || !valid(ns2, n2) {
			return
		}
		if (ns1 != ns2 || n1 != n2) && Name(ns1, n1) == Name(ns2, n2) {
			t.Fatalf("%s/%s and %s/%s share Secret %q", ns1, n1, ns2, n2, Name(ns1, n1))
		}
	})
}
