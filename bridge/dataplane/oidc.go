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
// not exposed: the bridge only needs sub/aud/iss/exp/iat.
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

	// Expiry is the exp claim as a wall-clock time.
	Expiry time.Time

	// IssuedAt is the iat claim as a wall-clock time. Zero when the
	// token lacks an iat claim.
	IssuedAt time.Time
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
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	// go-oidc threads the http client through context. Without this the
	// library falls back to http.DefaultClient internally and we lose
	// the ability to inject a fixture transport in tests.
	ctx = oidc.ClientContext(ctx, httpClient)

	var provider *oidc.Provider
	if cfg.JWKSURL != "" {
		// Operator-supplied JWKS endpoint — bypass discovery. We still
		// require the iss claim to match cfg.Issuer; only the *fetch*
		// URL changes. This handles the local-dev case (kubectl proxy
		// terminates outside the cluster but tokens still claim the
		// cluster-internal issuer) and the production case where the
		// bridge sits behind an internal LB.
		provider = (&oidc.ProviderConfig{
			IssuerURL: cfg.Issuer,
			JWKSURL:   cfg.JWKSURL,
		}).NewProvider(ctx)
	} else {
		p, err := oidc.NewProvider(ctx, cfg.Issuer)
		if err != nil {
			return nil, fmt.Errorf("oidc: discovery for issuer %q: %w", cfg.Issuer, err)
		}
		provider = p
	}

	verifier := provider.Verifier(&oidc.Config{
		// Audience is per-HarborAccess and varies by request. We disable
		// the library's audience check and validate aud in the handler
		// against the matched CR's trustPolicy.audience.
		SkipClientIDCheck: true,
		// SkipExpiryCheck deliberately left false: go-oidc enforces exp.
	})

	return &goOIDCValidator{verifier: verifier}, nil
}

type goOIDCValidator struct {
	verifier *oidc.IDTokenVerifier
}

func (v *goOIDCValidator) Validate(ctx context.Context, rawToken string) (*Claims, error) {
	idToken, err := v.verifier.Verify(ctx, rawToken)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	var raw rawClaims
	if err := idToken.Claims(&raw); err != nil {
		return nil, fmt.Errorf("%w: parse claims: %v", ErrInvalidToken, err)
	}
	out := &Claims{
		Subject:  raw.Sub,
		Audience: []string(raw.Aud),
		Issuer:   idToken.Issuer,
		Expiry:   idToken.Expiry,
	}
	if raw.Iat != 0 {
		out.IssuedAt = time.Unix(raw.Iat, 0)
	}
	return out, nil
}

// rawClaims is the JSON shape we unmarshal into when extracting claims
// from the verified token. Kept private — callers receive Claims.
type rawClaims struct {
	Sub string       `json:"sub"`
	Aud jsonAudience `json:"aud"`
	Iat int64        `json:"iat"`
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
