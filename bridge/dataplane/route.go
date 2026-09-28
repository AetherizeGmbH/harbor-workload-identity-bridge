// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/nexussecret"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/registryhost"
)

// Access kinds: the backend a credential request is routed to, recorded as
// the access_kind field of every audit line once a second backend is
// configured (ADR-0036 decision f).
const (
	// AccessKindHarbor serves the request from a HarborAccess and its
	// robot Secret.
	AccessKindHarbor = "harbor"

	// AccessKindNexus serves the request from a NexusAccess and its
	// nexususer Secret. The value is the Secret's access-kind label.
	AccessKindNexus = nexussecret.AccessKindNexus

	// AccessKindNone is an image that belongs to no configured backend.
	// It is refused (reason no_backend).
	AccessKindNone = "none"
)

// NexusBackend is what the handler needs to serve NexusAccess objects
// (ADR-0036). main builds it from the Nexus configuration; nil leaves the
// handler as it was before the backend existed.
type NexusBackend struct {
	// RegistryHosts are the registry hosts (host[:port][/path-prefix])
	// whose images are Nexus's. None shares a host[:port] with
	// HandlerConfig.HarborRegistryHosts; startup refuses that.
	RegistryHosts []registryhost.Host

	// IdentityName returns the Nexus identity name of the given
	// ServiceAccount: nexus.IdentityName with the bridge's cluster name.
	// Every user id of the identity starts with it. Required; nil serves
	// nothing.
	IdentityName func(saNamespace, saName string) (string, error)

	// UserIdentity returns the identity name of a Nexus user id
	// (nexus.ParseUserID); ok is false for an id the control plane never
	// builds. Required; nil serves nothing.
	UserIdentity func(userID string) (identity string, ok bool)
}

// routing reports whether requests are routed by their image. With Harbor
// alone they are not: every request is Harbor's and the image is audit
// information only, as before the Nexus backend existed.
func (h *Handler) routing() bool {
	return h.Config.Nexus != nil
}

// route returns the backend whose registry hosts the image belongs to:
// the host and port must be equal and a path prefix must end on a '/'
// boundary of the image's repository path (registryhost.Host.Matches).
// Without routing every image is Harbor's. With it, an image that matches
// neither backend, that cannot be parsed, or that is absent is
// AccessKindNone; so is one that matches both, which startup refuses and
// which the handler therefore never resolves in favour of either.
func (h *Handler) route(image string) string {
	if !h.routing() {
		return AccessKindHarbor
	}
	hostPort, path, err := registryhost.SplitImage(image)
	if err != nil {
		return AccessKindNone
	}
	nexus := matchesAny(h.Config.Nexus.RegistryHosts, hostPort, path)
	harbor := matchesAny(h.Config.HarborRegistryHosts, hostPort, path)
	switch {
	case nexus && !harbor:
		return AccessKindNexus
	case harbor && !nexus:
		return AccessKindHarbor
	}
	return AccessKindNone
}

func matchesAny(hosts []registryhost.Host, hostPort, path string) bool {
	for _, h := range hosts {
		if h.Matches(hostPort, path) {
			return true
		}
	}
	return false
}
