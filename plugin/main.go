// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

// Binary harbor-bridge-plugin is the kubelet image-credential-provider
// plugin for the Harbor Workload Identity Bridge. It speaks the KEP-4412
// stdin/stdout protocol to kubelet on one side and HTTPS to the bridge on
// the other.
//
// Wire format (in/out): credentialprovider.kubelet.k8s.io/v1.
// Bridge API: POST /v1/credentials, Authorization: Bearer <SA-token>.
// See bridge/dataplane/handler.go for the bridge side of the contract.
//
// Behaviour summary (full table in docs/PHASES.md, Phase 4):
//   - 200          → translate response, emit CredentialProviderResponse on stdout
//   - 401 / 403    → empty auth map, cacheKeyType=Image, cacheDuration=0
//   - 503          → retry once after 1s; second 503 exits non-zero
//   - other 5xx    → exits non-zero
//   - network err  → exits non-zero
//
// Non-zero exits write the cause to stderr; kubelet surfaces stderr in
// node events when the credential provider fails.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// credentialsPath must match bridge/dataplane.CredentialsPath. Duplicated
// here per ADR-0015 so the plugin does not import the dataplane package
// (and with it controller-runtime + k8s.io/api).
const credentialsPath = "/v1/credentials"

// Wire types — duplicated from k8s.io/kubelet/pkg/apis/credentialprovider/v1
// per ADR-0015. Keeps the plugin off the k8s.io/kubelet module's version
// curve and avoids the cacheKeyType="ServiceAccount" enum mismatch in
// older upstream Go enums.
const (
	credentialProviderAPIVersion = "credentialprovider.kubelet.k8s.io/v1"
	requestKind                  = "CredentialProviderRequest"
	responseKind                 = "CredentialProviderResponse"

	cacheKeyTypeImage = "Image"
)

type credentialProviderRequest struct {
	APIVersion          string `json:"apiVersion"`
	Kind                string `json:"kind"`
	Image               string `json:"image"`
	ServiceAccountToken string `json:"serviceAccountToken,omitempty"`
}

type credentialProviderResponse struct {
	APIVersion    string                `json:"apiVersion"`
	Kind          string                `json:"kind"`
	CacheKeyType  string                `json:"cacheKeyType"`
	CacheDuration string                `json:"cacheDuration"`
	Auth          map[string]authConfig `json:"auth"`
}

type authConfig struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// bridgeFetcher is the dependency run() needs from the bridge HTTP client.
// Kept as an interface so tests can swap in a fake without standing up an
// httptest server for every behavioural case.
type bridgeFetcher interface {
	fetch(image, token string) (*bridgeResponse, error)
}

func main() {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "harbor-bridge-plugin: config error: %v\n", err)
		os.Exit(1)
	}
	bc, err := newBridgeClient(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "harbor-bridge-plugin: %v\n", err)
		os.Exit(1)
	}
	if err := run(os.Stdin, os.Stdout, bc); err != nil {
		fmt.Fprintf(os.Stderr, "harbor-bridge-plugin: %v\n", err)
		os.Exit(1)
	}
}

// run reads the CredentialProviderRequest from stdin, talks to the bridge
// via fetcher, and writes the CredentialProviderResponse to stdout.
// Splitting this from main() keeps the binary's logic testable without
// fork/exec.
func run(stdin io.Reader, stdout io.Writer, fetcher bridgeFetcher) error {
	var req credentialProviderRequest
	if err := json.NewDecoder(stdin).Decode(&req); err != nil {
		return fmt.Errorf("decode CredentialProviderRequest from stdin: %w", err)
	}
	// kubelet always sends a typed v1 request. Anything else is not a
	// request this plugin understands — refuse instead of guessing.
	if req.APIVersion != credentialProviderAPIVersion || req.Kind != requestKind {
		return fmt.Errorf("unsupported request %q/%q, want %s/%s",
			req.APIVersion, req.Kind, credentialProviderAPIVersion, requestKind)
	}
	if req.Image == "" {
		return errors.New("request image field is empty")
	}
	if req.ServiceAccountToken == "" {
		return errors.New("request serviceAccountToken field is empty")
	}

	resp, err := fetcher.fetch(req.Image, req.ServiceAccountToken)
	switch {
	case errors.Is(err, errBridgeRefused):
		// The bridge rejected the SA token (401) or refused to authorize
		// it (403). Emit an empty auth map with no caching so kubelet
		// retries on the next pull — the operator may be mid-applying
		// the HarborAccess CR.
		return writeRefusedResponse(stdout, req.Image)
	case err != nil:
		return err
	}

	return writeOKResponse(stdout, resp, req.Image)
}

// serverNamePattern is a DNS name (letters, digits, '-' and '.').
var serverNamePattern = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9.]*[A-Za-z0-9])?$`)

// maxCacheDuration bounds what the plugin tells kubelet: the bridge never
// asks for more than the longest tokenTTL (24h).
const maxCacheDuration = 24 * time.Hour

// cacheKeyTypes are the values kubelet accepts for cacheKeyType.
var cacheKeyTypes = map[string]bool{"Image": true, "Registry": true, "Global": true}

func writeOKResponse(w io.Writer, r *bridgeResponse, image string) error {
	host := imageHost(image)
	if host == "" {
		return fmt.Errorf("cannot derive registry host from image %q", image)
	}
	// The plugin trusts the bridge over TLS, but it still refuses to hand
	// kubelet values kubelet cannot use: an unknown cache key type, or a
	// negative or overflowing cache duration.
	if !cacheKeyTypes[r.CacheKeyType] {
		return fmt.Errorf("bridge returned cache key type %q; want Image, Registry or Global", r.CacheKeyType)
	}
	secs := r.ExpiresInSecs
	if secs < 0 {
		secs = 0
	}
	dur := time.Duration(secs) * time.Second
	if secs > int(maxCacheDuration/time.Second) {
		dur = maxCacheDuration
	}
	out := credentialProviderResponse{
		APIVersion:    credentialProviderAPIVersion,
		Kind:          responseKind,
		CacheKeyType:  r.CacheKeyType,
		CacheDuration: dur.String(),
		Auth: map[string]authConfig{
			host: {Username: r.Username, Password: r.Password},
		},
	}
	return json.NewEncoder(w).Encode(out)
}

func writeRefusedResponse(w io.Writer, image string) error {
	out := credentialProviderResponse{
		APIVersion:    credentialProviderAPIVersion,
		Kind:          responseKind,
		CacheKeyType:  cacheKeyTypeImage,
		CacheDuration: time.Duration(0).String(),
		Auth:          map[string]authConfig{},
	}
	// image is unused in the body but recorded on stderr for kubelet's
	// event stream so an operator can map "no creds for X" to the image
	// that triggered it.
	fmt.Fprintf(os.Stderr, "harbor-bridge-plugin: bridge refused credentials for %s; returning empty auth (no cache)\n", image)
	return json.NewEncoder(w).Encode(out)
}

// imageHost returns the registry-host portion of an OCI image reference.
// For "harbor.example.com:8443/proj/repo:tag" it returns
// "harbor.example.com:8443". The kubelet credential-provider config
// (Phase 5 chart) restricts which images we are invoked for, so we do not
// need to handle Docker Hub's implicit "docker.io" insertion.
func imageHost(image string) string {
	if i := strings.IndexByte(image, '/'); i > 0 {
		return image[:i]
	}
	return ""
}

// config is what the plugin reads from the environment. The chart in
// Phase 5 plumbs these through the kubelet credential-provider config's
// env stanza.
type config struct {
	Endpoint   string // HARBOR_BRIDGE_ENDPOINT, required, base URL of the bridge.
	CABundle   string // HARBOR_BRIDGE_CA_BUNDLE, optional, a path to a PEM file or the PEM body itself (inline PEM suits hand-written provider configs, e.g. Talos).
	ClientCert string // HARBOR_BRIDGE_CLIENT_CERT, optional, path to mTLS client cert (ADR-0008).
	ClientKey  string // HARBOR_BRIDGE_CLIENT_KEY, optional, path to mTLS client key.
	ServerName string // HARBOR_BRIDGE_SERVER_NAME, optional, the name to verify the bridge certificate against when the endpoint's host is not in it (e.g. $(NODE_IP)).
}

// loadConfig populates a config from a getenv-like function. Taking the
// lookup as a parameter rather than reading os.Getenv directly keeps the
// test paths hermetic.
func loadConfig(getenv func(string) string) (*config, error) {
	c := &config{
		Endpoint:   getenv("HARBOR_BRIDGE_ENDPOINT"),
		CABundle:   getenv("HARBOR_BRIDGE_CA_BUNDLE"),
		ClientCert: getenv("HARBOR_BRIDGE_CLIENT_CERT"),
		ClientKey:  getenv("HARBOR_BRIDGE_CLIENT_KEY"),
		ServerName: getenv("HARBOR_BRIDGE_SERVER_NAME"),
	}
	if c.Endpoint == "" {
		return nil, errors.New("HARBOR_BRIDGE_ENDPOINT is required")
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("HARBOR_BRIDGE_ENDPOINT is not a valid URL: %w", err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("HARBOR_BRIDGE_ENDPOINT must use https (got scheme %q)", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("HARBOR_BRIDGE_ENDPOINT must include a host")
	}
	if c.ServerName != "" && !serverNamePattern.MatchString(c.ServerName) {
		return nil, fmt.Errorf("HARBOR_BRIDGE_SERVER_NAME %q is not a DNS name", c.ServerName)
	}
	if (c.ClientCert == "") != (c.ClientKey == "") {
		return nil, errors.New("HARBOR_BRIDGE_CLIENT_CERT and HARBOR_BRIDGE_CLIENT_KEY must be set together")
	}
	return c, nil
}
