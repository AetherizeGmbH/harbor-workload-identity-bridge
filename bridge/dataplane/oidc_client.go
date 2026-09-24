// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// oidcFetchTimeout bounds one discovery or JWKS request.
const oidcFetchTimeout = 30 * time.Second

// NewOIDCHTTPClient builds the client for OIDC discovery and JWKS
// fetches. caFile, when set, is the only trust root (the cluster CA for
// the in-cluster apiserver). tokenFile, when set, is the bridge's own
// ServiceAccount token: the apiserver serves discovery and JWKS only to
// authenticated callers. The token is re-read on every request (kubelet
// rotates it) and sent only over https to hosts NewValidator allows: the
// in-cluster apiserver and the jwks_uri its discovery names. The token
// authenticates to the apiserver, so any other host (a cloud issuer, a
// CDN, a redirect target) could replay it with the bridge's RBAC, which
// includes reading every robot password Secret. Redirects are not
// followed.
func NewOIDCHTTPClient(caFile, tokenFile string) (*http.Client, error) {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: oidcFetchTimeout,
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file %s: %w", caFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA file %s contained no PEM blocks", caFile)
		}
		transport.TLSClientConfig.RootCAs = pool
	}

	var rt http.RoundTripper = transport
	if tokenFile != "" {
		// Fail fast on a typo or a missing volume mount.
		if _, err := os.ReadFile(tokenFile); err != nil {
			return nil, fmt.Errorf("read token file %s: %w", tokenFile, err)
		}
		rt = &tokenTransport{base: transport, tokenFile: tokenFile, allowed: map[string]bool{}}
	}
	return &http.Client{
		Transport: rt,
		Timeout:   oidcFetchTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

// tokenTransport adds the bridge's ServiceAccount token to requests for
// allowed hosts.
type tokenTransport struct {
	base      http.RoundTripper
	tokenFile string

	mu      sync.RWMutex
	allowed map[string]bool // host[:port] as in URL.Host
}

func (t *tokenTransport) allow(host string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.allowed[host] = true
}

func (t *tokenTransport) isAllowed(u *url.URL) bool {
	if u.Scheme != "https" {
		return false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.allowed[u.Host]
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.isAllowed(req.URL) {
		return t.base.RoundTrip(req)
	}
	raw, err := os.ReadFile(t.tokenFile)
	if err != nil {
		return nil, fmt.Errorf("read SA token: %w", err)
	}
	req2 := req.Clone(req.Context())
	req2.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(raw)))
	return t.base.RoundTrip(req2)
}

// isInClusterAPIServer reports whether rawURL points at the cluster's own
// apiserver: the kubernetes Service's DNS names or the address in
// KUBERNETES_SERVICE_HOST.
func isInClusterAPIServer(u *url.URL) bool {
	host := u.Hostname()
	if svc := os.Getenv("KUBERNETES_SERVICE_HOST"); svc != "" && net.ParseIP(host) != nil && host == svc {
		return true
	}
	switch host {
	case "kubernetes", "kubernetes.default", "kubernetes.default.svc":
		return true
	}
	return strings.HasPrefix(host, "kubernetes.default.svc.")
}
