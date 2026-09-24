// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

// Package dataplane implements the bridge's HTTPS server that validates SA
// tokens from the kubelet credential-provider plugin, matches each request
// to a HarborAccess CR, and returns the per-CR robot's Basic Auth
// credentials so containerd can complete the Harbor registry handshake
// itself. See docs/adr/0002-bridge-control-plane-data-plane-split.md for
// the split, and docs/adr/0013-return-robot-basic-auth-credentials.md for
// why this package does not pre-mint Docker JWTs. The entire package is
// retired once upstream Harbor implements OIDC trust policies
// (goharbor/harbor#17520).
package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Validator verifies a Kubernetes service-account token's signature,
// expiration, and issuer. Each Validator instance is bound at construction
// time to one cluster's issuer (ADR-0009: one bridge per cluster, one
// issuer per bridge), so per-request checks need only confirm audience
// and subject — those are HarborAccess-CR-specific and live in the
// handler, not here.
type Validator interface {
	Validate(ctx context.Context, rawToken string) (*Claims, error)
}

// Claims is the projection of JWT claims the data plane needs to make
// its trust decision. Kubernetes-specific claims
// (kubernetes.io/serviceaccount/*, groups, etc.) are intentionally
// not exposed: the bridge only needs sub/aud/iss (go-oidc enforces exp).
type Claims struct {
	// Subject is the sub claim. For Kubernetes SA tokens this is
	// "system:serviceaccount:<namespace>:<name>".
	Subject string

	// Audience is the aud claim, normalised to a slice. RFC 7519 allows
	// aud to be either a string or a slice of strings; we always return
	// a slice so callers don't have to handle both forms.
	Audience []string

	// Issuer is the iss claim. Always equal to the Validator's configured
	// issuer when Validate returns nil (go-oidc enforces this).
	Issuer string

	// Expiry is the exp claim as a wall-clock time. go-oidc has already
	// rejected expired tokens; exposed for logging and tests.
	Expiry time.Time

	// Pod, PodUID and Node come from the kubernetes.io claim of a bound
	// ServiceAccount token (empty for unbound tokens). They are for the
	// audit log only, never for a trust decision.
	Pod    string
	PodUID string
	Node   string
}

// ErrInvalidToken wraps every Validate failure so callers can branch on
// "invalid for any reason" without inspecting the underlying go-oidc
// error category. Specific causes (expiry, signature, issuer mismatch)
// are surfaced in the wrapped error message for log readability.
var ErrInvalidToken = errors.New("invalid token")

// Config supplies the knobs Validator construction needs.
type Config struct {
	// Issuer is the OIDC issuer URL. Must be byte-for-byte equal to the
	// iss claim of incoming tokens — go-oidc strict-matches. For a
	// Kubernetes cluster this is typically the value returned by
	// `kubectl get --raw /.well-known/openid-configuration`, often
	// "https://kubernetes.default.svc.cluster.local".
	Issuer string

	// JWKSURL is the URL to fetch the JSON Web Key Set from. When empty
	// (the in-cluster default), the validator runs OIDC discovery
	// against Issuer and uses whichever jwks_uri the discovery response
	// reports. When set, the validator skips discovery entirely and
	// fetches JWKS directly from this URL. The Issuer field is still
	// the expected iss claim of incoming tokens; only the *transport*
	// is overridden. Use this when the bridge runs outside the cluster
	// (local dev via `kubectl proxy`) or behind a network topology
	// where the cluster-internal URLs do not resolve.
	JWKSURL string

	// HTTPClient is used for OIDC discovery and JWKS fetching. Pass a
	// client with a custom transport for httptest, mTLS, or audit
	// instrumentation. nil means http.DefaultClient.
	HTTPClient *http.Client
}

// NewValidator constructs a Validator that verifies tokens issued by
// cfg.Issuer. When cfg.JWKSURL is empty, the constructor performs OIDC
// discovery synchronously so a misconfigured issuer fails at bridge
// startup, not on the first kubelet request. When cfg.JWKSURL is set,
// discovery is skipped and the validator goes straight to the supplied
// JWKS endpoint with cfg.Issuer as the expected iss claim.
func NewValidator(ctx context.Context, cfg Config) (Validator, error) {
	if cfg.Issuer == "" {
		return nil, errors.New("oidc: issuer is required")
	}
	issuerURL, err := url.Parse(cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc: issuer %q: %w", cfg.Issuer, err)
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		// Still bounded and redirect-free, just without a CA or token.
		if httpClient, err = NewOIDCHTTPClient("", ""); err != nil {
			return nil, err
		}
	}
	// The bridge's token goes only to the in-cluster apiserver (see
	// NewOIDCHTTPClient).
	tt, _ := httpClient.Transport.(*tokenTransport)
	allowIfAPIServer := func(u *url.URL) {
		if tt != nil && isInClusterAPIServer(u) {
			tt.allow(u.Host)
		}
	}
	allowIfAPIServer(issuerURL)

	jwksURL := cfg.JWKSURL
	var algs []string
	if jwksURL != "" {
		u, err := url.Parse(jwksURL)
		if err != nil {
			return nil, fmt.Errorf("oidc: JWKS URL %q: %w", jwksURL, err)
		}
		allowIfAPIServer(u)
	} else {
		provider, err := oidc.NewProvider(oidc.ClientContext(ctx, httpClient), cfg.Issuer)
		if err != nil {
			return nil, fmt.Errorf("oidc: discovery for issuer %q: %w", cfg.Issuer, err)
		}
		var meta struct {
			JWKSURI string   `json:"jwks_uri"`
			Algs    []string `json:"id_token_signing_alg_values_supported"`
		}
		if err := provider.Claims(&meta); err != nil {
			return nil, fmt.Errorf("oidc: discovery document for issuer %q: %w", cfg.Issuer, err)
		}
		u, err := url.Parse(meta.JWKSURI)
		if err != nil || meta.JWKSURI == "" {
			return nil, fmt.Errorf("oidc: discovery for issuer %q names no usable jwks_uri (%q)", cfg.Issuer, meta.JWKSURI)
		}
		jwksURL = meta.JWKSURI
		// The apiserver's discovery names its JWKS endpoint by its own
		// advertised address, which is not one of the Service names.
		if tt != nil && isInClusterAPIServer(issuerURL) {
			tt.allow(u.Host)
		}
		algs = supportedAlgs(meta.Algs)
	}

	verifier := oidc.NewVerifier(cfg.Issuer, newCachedKeySet(jwksURL, httpClient), &oidc.Config{
		SkipClientIDCheck:    true,
		SupportedSigningAlgs: algs,
	})
	return &goOIDCValidator{verifier: verifier}, nil
}

// supportedAlgs keeps the discovery-advertised algorithms the key set can
// verify. Empty means the verifier's default, RS256.
func supportedAlgs(advertised []string) []string {
	var out []string
	for _, a := range advertised {
		for _, known := range jwtSigningAlgs {
			if a == string(known) {
				out = append(out, a)
				break
			}
		}
	}
	return out
}

type goOIDCValidator struct {
	verifier *oidc.IDTokenVerifier
}

func (v *goOIDCValidator) Validate(ctx context.Context, rawToken string) (*Claims, error) {
	idToken, err := v.verifier.Verify(ctx, rawToken)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	var raw rawClaims
	if err := idToken.Claims(&raw); err != nil {
		return nil, fmt.Errorf("%w: parse claims: %w", ErrInvalidToken, err)
	}
	return &Claims{
		Subject:  raw.Sub,
		Audience: []string(raw.Aud),
		Issuer:   idToken.Issuer,
		Expiry:   idToken.Expiry,
		Pod:      raw.K8s.Pod.Name,
		PodUID:   raw.K8s.Pod.UID,
		Node:     raw.K8s.Node.Name,
	}, nil
}

// rawClaims is the JSON shape we unmarshal into when extracting claims
// from the verified token. Kept private — callers receive Claims.
type rawClaims struct {
	Sub string       `json:"sub"`
	Aud jsonAudience `json:"aud"`
	K8s struct {
		Pod struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"pod"`
		Node struct {
			Name string `json:"name"`
		} `json:"node"`
	} `json:"kubernetes.io"`
}

// jsonAudience handles the OIDC quirk that aud may be either a string or
// a string slice. RFC 7519 §4.1.3 permits both forms. go-oidc itself
// internally normalises this for its audience check but does not expose
// the normalised slice via the public IDToken type, so we re-parse here.
type jsonAudience []string

func (a *jsonAudience) UnmarshalJSON(data []byte) error {
	// Try a single string first; aud is the much more common form in
	// Kubernetes SA tokens.
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*a = []string{single}
		return nil
	}
	var slice []string
	if err := json.Unmarshal(data, &slice); err != nil {
		return fmt.Errorf("aud must be string or []string: %w", err)
	}
	*a = slice
	return nil
}
