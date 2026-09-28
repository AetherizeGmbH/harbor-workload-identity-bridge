// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"testing"

	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/registryhost"
)

func mustHosts(t *testing.T, raw string) []registryhost.Host {
	t.Helper()
	hosts, err := registryhost.ParseList(raw)
	if err != nil {
		t.Fatalf("ParseList(%q): %v", raw, err)
	}
	return hosts
}

// TestRoute pins how an image selects its backend (ADR-0036 decision f):
// the host and port exactly, as kubelet keys its credential cache by them;
// a path prefix only on '/' boundaries; and nothing for an image of
// neither backend, so that no backend's credentials reach another
// registry.
func TestRoute(t *testing.T) {
	h := &Handler{Config: HandlerConfig{
		HarborRegistryHosts: mustHosts(t, "harbor.example.com, registry.example.com/harbor"),
		Nexus: &NexusBackend{RegistryHosts: mustHosts(t,
			"nexus.example.com:8443, registry.example.com:5000/nexus/team, [fd00::10]:5000, [fd00::20], 10.0.0.7:8082")},
	}}
	for _, tc := range []struct {
		image, want string
	}{
		// Exact host and port.
		{"nexus.example.com:8443/app:v1", AccessKindNexus},
		{"nexus.example.com:8443/team/app@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", AccessKindNexus},
		{"nexus.example.com:8443/team/app:v1@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", AccessKindNexus},
		{"NEXUS.Example.COM:8443/app", AccessKindNexus},  // host names are case-insensitive
		{"nexus.example.com/app:v1", AccessKindNone},     // no port is another registry
		{"nexus.example.com:443/app:v1", AccessKindNone}, // so is another port
		{"nexus.example.com:08443/app", AccessKindNone},  // and a port kubelet would not equate
		{"nexus.example.com.evil.test:8443/app", AccessKindNone},
		{"evil.test/nexus.example.com:8443/app", AccessKindNone},
		{"10.0.0.7:8082/app:1", AccessKindNexus},
		{"10.0.0.7/app:1", AccessKindNone},

		// Path prefixes match whole segments only.
		{"registry.example.com:5000/nexus/team/app:v1", AccessKindNexus},
		{"registry.example.com:5000/nexus/team:v1", AccessKindNexus}, // the prefix itself as repository
		{"registry.example.com:5000/nexus/team-old/app", AccessKindNone},
		{"registry.example.com:5000/nexus/teamx/app", AccessKindNone},
		{"registry.example.com:5000/nexus/app", AccessKindNone},
		{"registry.example.com:5000/nexus", AccessKindNone},
		{"registry.example.com/harbor/app", AccessKindHarbor},
		{"registry.example.com/harbor-old/app", AccessKindNone},
		{"registry.example.com/nexus/team/app", AccessKindNone}, // Nexus's prefix, but on another port

		// IPv6 literals, bracketed as in image references.
		{"[fd00::10]:5000/team/app:v1", AccessKindNexus},
		{"[FD00::10]:5000/team/app:v1", AccessKindNexus},
		{"[fd00::10]/team/app:v1", AccessKindNone},
		{"[fd00::10]:5001/team/app:v1", AccessKindNone},
		{"[fd00::20]/team/app:v1", AccessKindNexus},
		{"[fd00::20]:443/team/app:v1", AccessKindNone},
		{"[fd00::2]/team/app:v1", AccessKindNone},

		// Harbor's own hosts.
		{"harbor.example.com/production/app:v1", AccessKindHarbor},
		{"HARBOR.example.com/production/app:v1", AccessKindHarbor},
		{"harbor.example.com:443/production/app:v1", AccessKindNone},

		// Images of neither backend, and requests that name none.
		{"nginx", AccessKindNone}, // docker.io/library/nginx
		{"library/nginx:1.27", AccessKindNone},
		{"docker.io/library/nginx", AccessKindNone},
		{"ghcr.io/org/app:v1", AccessKindNone},
		{"", AccessKindNone},
		{"   ", AccessKindNone},
		{"@sha256:0123", AccessKindNone},
	} {
		if got := h.route(tc.image); got != tc.want {
			t.Errorf("route(%q) = %q, want %q", tc.image, got, tc.want)
		}
	}
}

// With Harbor alone the image is audit information only, as before the
// Nexus backend: every request is Harbor's, whatever it names, also none.
func TestRoute_HarborAloneRoutesEverythingToHarbor(t *testing.T) {
	h := &Handler{Config: HandlerConfig{HarborRegistryHosts: mustHosts(t, "harbor.example.com")}}
	for _, image := range []string{"harbor.example.com/p/app", "nexus.example.com:8443/app", "nginx", "", "not an image"} {
		if got := h.route(image); got != AccessKindHarbor {
			t.Errorf("route(%q) = %q, want %q", image, got, AccessKindHarbor)
		}
	}
}

// Startup refuses a host[:port] both backends name. A handler built with
// one anyway must not resolve it in favour of either backend.
func TestRoute_ImageOfBothBackendsIsRefused(t *testing.T) {
	h := &Handler{Config: HandlerConfig{
		HarborRegistryHosts: mustHosts(t, "registry.example.com"),
		Nexus:               &NexusBackend{RegistryHosts: mustHosts(t, "registry.example.com/nexus")},
	}}
	if got := h.route("registry.example.com/nexus/app"); got != AccessKindNone {
		t.Errorf("route of an image both backends claim = %q, want %q", got, AccessKindNone)
	}
	if got := h.route("registry.example.com/other/app"); got != AccessKindHarbor {
		t.Errorf("route of an image only Harbor claims = %q, want %q", got, AccessKindHarbor)
	}
}

// With Nexus configured and no Harbor host (a configuration startup never
// produces), no image is Harbor's: routing fails closed.
func TestRoute_NoHarborHostsFailsClosed(t *testing.T) {
	h := &Handler{Config: HandlerConfig{Nexus: &NexusBackend{RegistryHosts: mustHosts(t, "nexus.example.com:8443")}}}
	if got := h.route("harbor.example.com/p/app"); got != AccessKindNone {
		t.Errorf("route = %q, want %q", got, AccessKindNone)
	}
}
