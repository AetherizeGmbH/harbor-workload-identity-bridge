// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package registryhost

import (
	"testing"
)

func TestParse(t *testing.T) {
	for raw, want := range map[string]Host{
		"nexus.example.com":              {HostPort: "nexus.example.com"},
		" Nexus.Example.COM ":            {HostPort: "nexus.example.com"},
		"nexus.example.com:5000":         {HostPort: "nexus.example.com:5000"},
		"nexus.example.com/docker":       {HostPort: "nexus.example.com", PathPrefix: "docker"},
		"nexus.example.com:8443/a/b-c.d": {HostPort: "nexus.example.com:8443", PathPrefix: "a/b-c.d"},
		"localhost:5000":                 {HostPort: "localhost:5000"},
		"10.0.0.1:5000/x":                {HostPort: "10.0.0.1:5000", PathPrefix: "x"},
		"[::1]:5000":                     {HostPort: "[::1]:5000"},
		"[FD00::1]":                      {HostPort: "[fd00::1]"},
	} {
		got, err := Parse(raw)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", raw, got, err, want)
		}
	}
	for _, raw := range []string{
		"", "https://nexus.example.com", "user:pw@nexus.example.com", "nexus.example.com?x",
		"*.example.com", "nexus.example.com:0", "nexus.example.com:65536", "nexus.example.com:05000",
		"nexus.example.com:", "nexus.example.com:port", "nexus.example.com/", "nexus.example.com//x",
		"nexus.example.com/Docker", "nexus..example.com", "-nexus.example.com", "::1", "[::1", "[nexus]:5000",
		"[::1]5000", "nexus.example.com/a b",
	} {
		if h, err := Parse(raw); err == nil {
			t.Errorf("Parse(%q) = %+v, want an error", raw, h)
		}
	}
}

func TestParseList(t *testing.T) {
	got, err := ParseList("a.example.com, b.example.com:5000/x ,a.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].String() != "a.example.com" || got[1].String() != "b.example.com:5000/x" {
		t.Errorf("ParseList = %v", got)
	}
	if _, err := ParseList("a.example.com,,b.example.com"); err == nil {
		t.Error("an empty entry was accepted")
	}
	if _, err := ParseList(""); err == nil {
		t.Error("an empty list was accepted")
	}
}

func TestSplitImage(t *testing.T) {
	for image, want := range map[string][2]string{
		"nexus.example.com/team/app:1.0":            {"nexus.example.com", "team/app"},
		"nexus.example.com:5000/app":                {"nexus.example.com:5000", "app"},
		"nexus.example.com:5000/app:1@sha256:abcd":  {"nexus.example.com:5000", "app"},
		"Nexus.Example.com/app":                     {"nexus.example.com", "app"},
		"localhost/app":                             {"localhost", "app"},
		"[::1]:5000/app:tag":                        {"[::1]:5000", "app"},
		"nginx":                                     {"docker.io", "nginx"},
		"nginx:1.27":                                {"docker.io", "nginx"},
		"library/nginx":                             {"docker.io", "library/nginx"},
		"nexus.example.com/nexus-old/img@sha256:ff": {"nexus.example.com", "nexus-old/img"},
	} {
		host, path, err := SplitImage(image)
		if err != nil || host != want[0] || path != want[1] {
			t.Errorf("SplitImage(%q) = %q, %q, %v; want %q, %q", image, host, path, err, want[0], want[1])
		}
	}
	for _, image := range []string{"", "@sha256:ab", "nexus.example.com/"} {
		if h, p, err := SplitImage(image); err == nil {
			t.Errorf("SplitImage(%q) = %q, %q, want an error", image, h, p)
		}
	}
}

// Matching is segment-aware on the path and exact on host and port
// (ADR-0033 decision f).
func TestMatchImage(t *testing.T) {
	hosts, err := ParseList("nexus.example.com/nexus,registry.example.com:5000")
	if err != nil {
		t.Fatal(err)
	}
	for image, want := range map[string]bool{
		"nexus.example.com/nexus/img:1":     true,
		"nexus.example.com/nexus":           true,
		"nexus.example.com/nexus-old/img":   false,
		"nexus.example.com/other/img":       false,
		"nexus.example.com:443/nexus/img":   false,
		"registry.example.com:5000/any/img": true,
		"registry.example.com/any/img":      false,
		"registry.example.com:50000/img":    false,
		"nginx":                             false,
		"":                                  false,
	} {
		if got := MatchImage(hosts, image); got != want {
			t.Errorf("MatchImage(%q) = %v, want %v", image, got, want)
		}
	}
}

func TestSharedHostPort(t *testing.T) {
	a, _ := ParseList("harbor.example.com,shared.example.com:5000/x")
	b, _ := ParseList("nexus.example.com,shared.example.com:5000/y")
	if hp, ok := SharedHostPort(a, b); !ok || hp != "shared.example.com:5000" {
		t.Errorf("SharedHostPort = %q, %v", hp, ok)
	}
	c, _ := ParseList("shared.example.com")
	if hp, ok := SharedHostPort(a[:1], c); ok {
		t.Errorf("distinct hosts reported as shared: %q", hp)
	}
	d, _ := ParseList("harbor.example.com:443")
	if _, ok := SharedHostPort(a[:1], d); ok {
		t.Error("host and host:443 are different registries to kubelet")
	}
}
