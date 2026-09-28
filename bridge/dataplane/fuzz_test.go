// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/registryhost"
)

// The aud claim comes from a token anyone can send to the NodePort; it is
// decoded before the signature is checked against a CR's audience.
func FuzzJSONAudience(f *testing.F) {
	for _, seed := range []string{`"a"`, `["a","b"]`, `[]`, `null`, `1`, `{"x":1}`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		var a jsonAudience
		if err := json.Unmarshal(raw, &a); err != nil {
			return
		}
		for _, v := range a {
			_ = v
		}
	})
}

// The key set parses every token sent to the NodePort before any trust
// decision; arbitrary input must never panic or verify.
func FuzzCachedKeySet_VerifySignature(f *testing.F) {
	fi := newFixtureIssuer(f)
	srv := httptest.NewServer(http.HandlerFunc(fi.handleJWKS))
	f.Cleanup(srv.Close)
	ks := newCachedKeySet(srv.URL, srv.Client())
	f.Add("")
	f.Add("a.b.c")
	f.Add("eyJhbGciOiJSUzI1NiIsImtpZCI6IngifQ.e30.AAAA")
	f.Add("eyJhbGciOiJub25lIn0.e30.")
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 1<<14 {
			return
		}
		if _, err := ks.VerifySignature(context.Background(), raw); err == nil {
			t.Fatalf("arbitrary input verified: %q", raw)
		}
	})
}

// The image is caller-supplied and routed before the token is checked
// (ADR-0033 decision f). Routing must never panic, must pick at most one
// backend, and must pick one only for an image whose registry host (the
// text before the first '/', which the plugin keys the credentials by and
// containerd sends them to) is one of that backend's hosts: credentials
// never go to a host their backend does not own.
func FuzzRoute(f *testing.F) {
	parse := func(raw string) []registryhost.Host {
		hosts, err := registryhost.ParseList(raw)
		if err != nil {
			f.Fatal(err)
		}
		return hosts
	}
	harborHosts := parse("harbor.example.com,registry.example.com/harbor,[fd00::1]:443")
	nexusHosts := parse("nexus.example.com:8443,registry.example.com:5000/nexus/team,[fd00::10]:5000,10.0.0.7")
	h := &Handler{Config: HandlerConfig{HarborRegistryHosts: harborHosts, Nexus: &NexusBackend{RegistryHosts: nexusHosts}}}
	for _, seed := range []string{
		"nexus.example.com:8443/app:v1", "registry.example.com:5000/nexus/team/app@sha256:00",
		"registry.example.com:5000/nexus/team-old/app", "[fd00::10]:5000/a/b:c", "10.0.0.7/x",
		"harbor.example.com/p/app", "registry.example.com/harbor/x", "nginx", "", "a@b/c", " x/y ",
		"nexus.example.com:8443@evil.test/app", "evil.test/nexus.example.com:8443/app",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, image string) {
		if len(image) > 1<<12 {
			return
		}
		kind := h.route(image)
		var owned []registryhost.Host
		switch kind {
		case AccessKindNone:
			return
		case AccessKindNexus:
			owned = nexusHosts
		case AccessKindHarbor:
			owned = harborHosts
		default:
			t.Fatalf("route(%q) = %q", image, kind)
		}
		host, _, found := strings.Cut(strings.TrimSpace(image), "/")
		if !found {
			t.Fatalf("route(%q) = %s for an image without a registry host", image, kind)
		}
		host = strings.ToLower(host)
		for _, o := range owned {
			if o.HostPort == host {
				return
			}
		}
		t.Fatalf("route(%q) = %s, but its registry host %q is none of that backend's", image, kind, host)
	})
}
