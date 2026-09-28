// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package nexus

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// The user and role ids are a multi-tenancy boundary (ADR-0018, ADR-0036):
// two different identities must never share a role or a user, two
// generations of one identity never share a user, no cluster may claim
// another cluster's names, and every name must be one Nexus accepts and
// ParseUserID recovers.
func FuzzNexusName_Injective(f *testing.F) {
	f.Add("c", "a", "b-x", uint64(1), "c", "a-b", "x", uint64(1))
	f.Add("prod", "ns", "sa", uint64(1), "prod-eu", "ns", "sa", uint64(1))
	f.Add("c", "team--a", "sa", uint64(7), "c", "team", "a-sa", uint64(7))
	f.Add("c", "ns", "sa", uint64(1), "c", "ns", "sa", uint64(2))
	f.Add(strings.Repeat("c", 63), strings.Repeat("n", 63), strings.Repeat("s", 253), uint64(0),
		strings.Repeat("c", 63), strings.Repeat("n", 63), strings.Repeat("s", 252)+"t", uint64(0))
	f.Fuzz(func(t *testing.T, c1, ns1, sa1 string, g1 uint64, c2, ns2, sa2 string, g2 uint64) {
		gen1, gen2 := fmt.Sprintf("%016x", g1), fmt.Sprintf("%016x", g2)
		r1, err1 := RoleID(c1, ns1, sa1)
		r2, err2 := RoleID(c2, ns2, sa2)
		valid := func(c, ns, sa string) bool {
			return len(c) <= 63 && dnsLabel.MatchString(c) &&
				len(ns) <= 63 && dnsLabel.MatchString(ns) &&
				len(sa) <= 253 && dnsLabel.MatchString(sa)
		}
		for _, x := range []struct {
			c, ns, sa string
			err       error
		}{{c1, ns1, sa1, err1}, {c2, ns2, sa2, err2}} {
			if valid(x.c, x.ns, x.sa) != (x.err == nil) {
				t.Fatalf("RoleID(%q, %q, %q): error %v, but the inputs are valid=%v", x.c, x.ns, x.sa, x.err, valid(x.c, x.ns, x.sa))
			}
			if x.err != nil && !errors.Is(x.err, ErrInvalidIdentity) {
				t.Fatalf("RoleID(%q, %q, %q): unexpected error %v", x.c, x.ns, x.sa, x.err)
			}
		}
		if err1 != nil || err2 != nil {
			return
		}
		u1, err := UserID(c1, ns1, sa1, gen1)
		if err != nil {
			t.Fatal(err)
		}
		u2, err := UserID(c2, ns2, sa2, gen2)
		if err != nil {
			t.Fatal(err)
		}

		for _, n := range []string{r1, u1} {
			if len(n) > NameCap || !idPattern.MatchString(n) || strings.ToLower(n) != n {
				t.Fatalf("name %q is not a lower-case id of at most %d characters", n, NameCap)
			}
		}
		if identity, gen, ok := ParseUserID(u1); !ok || identity != r1 || gen != gen1 {
			t.Fatalf("ParseUserID(%q) = %q, %q, %v; want %q, %q", u1, identity, gen, ok, r1, gen1)
		}
		if _, _, ok := ParseUserID(r1); ok {
			t.Fatalf("role id %q parses as a user id", r1)
		}
		if !OwnsName(c1, r1) || !OwnsName(c1, u1) {
			t.Fatalf("cluster %q does not own its names %q, %q", c1, r1, u1)
		}
		if c1 != c2 && (OwnsName(c1, r2) || OwnsName(c1, u2)) {
			t.Fatalf("cluster %q claims %q or %q of cluster %q", c1, r2, u2, c2)
		}
		sameIdentity := c1 == c2 && ns1 == ns2 && sa1 == sa2
		if !sameIdentity && r1 == r2 {
			t.Fatalf("(%q,%q,%q) and (%q,%q,%q) share role %q", c1, ns1, sa1, c2, ns2, sa2, r1)
		}
		if (!sameIdentity || gen1 != gen2) && u1 == u2 {
			t.Fatalf("(%q,%q,%q,%s) and (%q,%q,%q,%s) share user %q", c1, ns1, sa1, gen1, c2, ns2, sa2, gen2, u1)
		}
		if strings.Count(r1, ".") != 2 && strings.Count(r1, ".") != 3 {
			t.Fatalf("identity name %q has neither the natural nor the truncated shape", r1)
		}
	})
}

// Whatever a server puts into an error body, the rendered message never
// holds a credential or password of the request and stays one bounded
// line.
func FuzzRenderMessage_WithholdsSecrets(f *testing.F) {
	f.Add([]byte(`{"id":"*","message":"\"bad S3cret\""}`), "application/json", false, "S3cret")
	f.Add([]byte("rejected S3c\x00ret"), "text/plain", false, "S3cret")
	f.Add([]byte("ends in S3c"), "text/plain", true, "S3cret")
	f.Add([]byte(`[{"id":"a","message":"x"},{"id":"b","message":"y"}]`), "application/vnd.siesta-validation-errors-v1+json", false, "x; b: y")
	// Bytes of the ellipsis that ends a shortened message (U+2026 is
	// E2 80 A6) are the client's, not the server's.
	f.Add([]byte(strings.Repeat("0", 600)), "", false, "\x80")
	f.Add([]byte(strings.Repeat("0", 600)), "", false, "0\xe2")
	f.Add([]byte(strings.Repeat("0", 600)), "", false, "0…")
	f.Fuzz(func(t *testing.T, body []byte, contentType string, truncated bool, secret string) {
		// A secret that the withheld notice contains can appear without
		// being disclosed.
		if secret == "" || strings.Contains(messageWithheld, secret) {
			return
		}
		got := renderMessage(body, contentType, truncated, []string{secret})
		if got == messageWithheld {
			return
		}
		// A shortened message is the server's text plus the client's
		// ellipsis; only the server's text can disclose anything.
		text := got
		if utf8.RuneCountInString(got) == maxMessageLen+1 {
			text = strings.TrimSuffix(got, "…")
		}
		if strings.Contains(text, secret) || (sanitize(secret) != "" && strings.Contains(text, sanitize(secret))) {
			t.Fatalf("renderMessage(%q, %q) = %q holds the secret %q", body, contentType, got, secret)
		}
		if got != sanitize(got) && !strings.HasSuffix(got, "…") {
			t.Fatalf("renderMessage = %q holds control characters or invalid UTF-8", got)
		}
		if n := len([]rune(got)); n > maxMessageLen+1 {
			t.Fatalf("renderMessage has %d runes, limit %d", n, maxMessageLen)
		}
	})
}
