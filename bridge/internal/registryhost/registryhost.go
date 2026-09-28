// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

// Package registryhost parses the registry hosts a backend serves
// ("host[:port][/path-prefix]", BRIDGE_HARBOR_REGISTRY_HOSTS and
// BRIDGE_NEXUS_REGISTRY_HOSTS) and matches image references against them
// (ADR-0036 decision f). The control plane validates the configuration
// with it and the data plane routes a credential request with it; both
// import this leaf package (ADR-0002 forbids only the control plane
// importing the data plane).
//
// Matching is segment-aware: the host and port must be equal, and the
// path prefix must end on a '/' boundary of the image's repository path
// ("host/nexus" matches "host/nexus/app", not "host/nexus-old/app").
// kubelet's matchImages compares path prefixes as raw strings; the chart
// checks that every configured host is covered by it.
package registryhost

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// Host is one registry host a backend serves.
type Host struct {
	// HostPort is the registry's host name or IP address, lower-case,
	// with ":<port>" when the images name a port. "host" and "host:443"
	// are different registries to kubelet, so they are to Host as well.
	HostPort string

	// PathPrefix narrows the entry to images whose repository path starts
	// with these segments, without leading or trailing '/'. Empty matches
	// every image of HostPort.
	PathPrefix string
}

// String renders the entry as it is configured.
func (h Host) String() string {
	if h.PathPrefix == "" {
		return h.HostPort
	}
	return h.HostPort + "/" + h.PathPrefix
}

var (
	// dnsLabel is one label of a host name (RFC 1123, lower-case).
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

	// pathComponent is one component of a repository path, the rule of
	// the distribution reference grammar. Repository paths are
	// lower-case, so a prefix with upper-case letters could never match.
	pathComponent = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
)

// Parse parses one "host[:port][/path-prefix]" entry.
func Parse(raw string) (Host, error) {
	s := strings.TrimSpace(raw)
	switch {
	case s == "":
		return Host{}, errors.New("empty registry host")
	case strings.Contains(s, "://"):
		return Host{}, fmt.Errorf("registry host %q must not carry a scheme: write host[:port][/path-prefix]", s)
	case strings.ContainsAny(s, "@?#*\\ \t"):
		return Host{}, fmt.Errorf("registry host %q may contain only a host, a port and a path prefix (no credentials, query, fragment, wildcard or whitespace)", s)
	}
	hostPort, path, hasPath := strings.Cut(s, "/")
	host, port, err := splitHostPort(hostPort)
	if err != nil {
		return Host{}, fmt.Errorf("registry host %q: %w", s, err)
	}
	h := Host{HostPort: host}
	if port != "" {
		h.HostPort = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		h.HostPort = "[" + host + "]"
	}
	if hasPath {
		for _, seg := range strings.Split(path, "/") {
			if !pathComponent.MatchString(seg) {
				return Host{}, fmt.Errorf("registry host %q: path segment %q is not a repository path component (lower-case letters and digits, separated by '.', '_', '__' or '-'; no empty segment, no trailing '/')", s, seg)
			}
		}
		h.PathPrefix = path
	}
	return h, nil
}

// splitHostPort splits "host", "host:port", "[v6]" or "[v6]:port" and
// validates both parts. The host is returned lower-case and without
// brackets.
func splitHostPort(s string) (host, port string, err error) {
	switch {
	case strings.HasPrefix(s, "["):
		end := strings.Index(s, "]")
		if end < 0 {
			return "", "", errors.New("unterminated '[' in an IPv6 address")
		}
		host, rest := s[1:end], s[end+1:]
		if ip := net.ParseIP(host); ip == nil || !strings.Contains(host, ":") {
			return "", "", fmt.Errorf("%q is not an IPv6 address", host)
		}
		if rest != "" {
			p, ok := strings.CutPrefix(rest, ":")
			if !ok {
				return "", "", fmt.Errorf("unexpected %q after the IPv6 address", rest)
			}
			if err := validatePort(p); err != nil {
				return "", "", err
			}
			port = p
		}
		return strings.ToLower(host), port, nil
	case strings.Count(s, ":") > 1:
		return "", "", errors.New("an IPv6 address must be written in brackets: [address]:port")
	}
	host, port, _ = strings.Cut(s, ":")
	if strings.Contains(s, ":") {
		if err := validatePort(port); err != nil {
			return "", "", err
		}
	}
	host = strings.ToLower(host)
	if net.ParseIP(host) == nil {
		if len(host) == 0 || len(host) > 253 {
			return "", "", fmt.Errorf("host name %q must have 1 to 253 characters", host)
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) > 63 || !dnsLabel.MatchString(label) {
				return "", "", fmt.Errorf("%q is not a host name or IP address", host)
			}
		}
	}
	return host, port, nil
}

func validatePort(p string) error {
	n, err := strconv.Atoi(p)
	// A leading zero would make the entry differ from the image's port
	// as a string, which is how kubelet compares registries.
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != p {
		return fmt.Errorf("port %q must be a number from 1 to 65535 without leading zeros", p)
	}
	return nil
}

// ParseList parses a comma-separated list of entries. Surrounding
// whitespace is ignored; an empty entry is an error. Duplicate entries are
// kept once.
func ParseList(raw string) ([]Host, error) {
	var out []Host
	var errs []error
	seen := map[Host]bool{}
	for _, part := range strings.Split(raw, ",") {
		h, err := Parse(part)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return out, nil
}

// SplitImage returns the registry host (lower-case, with the port the
// image names) and the repository path of an image reference, without
// tag or digest. An image without a registry component ("nginx",
// "library/nginx") is Docker Hub's, reported as "docker.io", as the
// distribution reference grammar has it.
func SplitImage(image string) (hostPort, path string, err error) {
	ref := strings.TrimSpace(image)
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if ref == "" {
		return "", "", errors.New("empty image reference")
	}
	first, rest, hasSlash := strings.Cut(ref, "/")
	if !hasSlash || (!strings.ContainsAny(first, ".:") && first != "localhost" && strings.ToLower(first) == first) {
		hostPort, rest = "docker.io", ref
	} else {
		hostPort = strings.ToLower(first)
	}
	// A tag follows the last ':' after the last '/'.
	if i := strings.LastIndex(rest, ":"); i >= 0 && !strings.Contains(rest[i:], "/") {
		rest = rest[:i]
	}
	if rest == "" {
		return "", "", fmt.Errorf("image reference %q has no repository path", image)
	}
	return hostPort, rest, nil
}

// Matches reports whether an image with the given registry host and
// repository path (SplitImage) belongs to the entry.
func (h Host) Matches(hostPort, path string) bool {
	if hostPort != h.HostPort {
		return false
	}
	return h.PathPrefix == "" || path == h.PathPrefix || strings.HasPrefix(path, h.PathPrefix+"/")
}

// MatchImage reports whether the image belongs to any of the entries. An
// image reference that cannot be split matches none.
func MatchImage(hosts []Host, image string) bool {
	hostPort, path, err := SplitImage(image)
	if err != nil {
		return false
	}
	for _, h := range hosts {
		if h.Matches(hostPort, path) {
			return true
		}
	}
	return false
}

// SharedHostPort returns a host[:port] that appears in both lists, and
// whether there is one. Two backends must not share one: kubelet caches
// credentials per registry host (cacheKeyType Registry, ADR-0016), so a
// shared host would serve one backend's credentials for the other's
// images (ADR-0036 decision f).
func SharedHostPort(a, b []Host) (string, bool) {
	for _, x := range a {
		for _, y := range b {
			if x.HostPort == y.HostPort {
				return x.HostPort, true
			}
		}
	}
	return "", false
}
