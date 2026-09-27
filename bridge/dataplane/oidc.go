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
// expiration, and issuer, and enforces the maximum lifetime and the pod
// binding (ADR-0028). Each Validator instance is bound at construction
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
// not exposed: the bridge matches on sub/aud/iss; go-oidc enforces exp,
// and Validate the lifetime and the pod binding (ADR-0028).
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

	// IssuedAt is the iat claim. Validate has already checked that
	// Expiry - IssuedAt is within the maximum token lifetime.
	IssuedAt time.Time

	// Pod, PodUID and Node come from the kubernetes.io claim of a
	// pod-bound ServiceAccount token. Validate requires Pod and PodUID
	// unless Config.AllowNonPodBoundTokens (ADR-0028); Node may be empty.
	// Which pod a token names is for the audit log only: authorization
	// stays with the ServiceAccount (ADR-0010).
	Pod    string
	PodUID string
	Node   string
}

// ErrInvalidToken wraps every Validate failure but
// ErrSigningKeysUnavailable, so callers can branch on "invalid for any
// reason" without inspecting the underlying go-oidc error category.
// Specific causes (expiry, signature, issuer mismatch) are surfaced in
// the wrapped error message for log readability.
var ErrInvalidToken = errors.New("invalid token")

// ErrSigningKeysUnavailable marks a token Validate could not judge: its
// signing key is not among the keys the bridge holds, and the bridge
// could not fetch the current key set (the JWKS endpoint failed, or the
// request ended while the fetch ran). A server-side outage, not a bad
// token; not wrapped in ErrInvalidToken.
var ErrSigningKeysUnavailable = errors.New("token signing keys unavailable")

// ErrTokenLifetime marks a token whose lifetime (exp - iat) exceeds the
// maximum or cannot be determined (ADR-0028). Wrapped in ErrInvalidToken.
var ErrTokenLifetime = errors.New("token lifetime not accepted")

// ErrTokenNotPodBound marks a token without the kubernetes.io pod claim
// while pod binding is required (ADR-0028). Wrapped in ErrInvalidToken.
var ErrTokenNotPodBound = errors.New("token not bound to a pod")

// maxIssuedAtSkew is how far in the future a token's iat may lie: the
// leeway go-oidc allows for nbf, which Kubernetes sets to iat. A token
// issued further ahead could stay valid for longer than the maximum
// lifetime from now.
const maxIssuedAtSkew = 5 * time.Minute

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

	// MaxTokenLifetime is the longest lifetime (exp - iat) an accepted
	// token may have. Required, positive. Kubelet's credential-provider
	// tokens have exactly one hour, the TokenRequest default (ADR-0028).
	MaxTokenLifetime time.Duration

	// AllowNonPodBoundTokens accepts tokens without the kubernetes.io pod
	// claim. The zero value requires the claim, which every kubelet token
	// carries; true weakens the bridge and exists for hand-minted tokens
	// in local development (ADR-0028).
	AllowNonPodBoundTokens bool
}

// NewValidator constructs a Validator that verifies tokens issued by
// cfg.Issuer. When cfg.JWKSURL is empty, the constructor performs OIDC
// discovery; when it is set, discovery is skipped and the validator goes
// straight to the supplied JWKS endpoint with cfg.Issuer as the expected
// iss claim. Either way it fetches the signing keys once before it
// returns, so a misconfigured issuer, JWKS URL, CA or RBAC fails at
// bridge startup, not on the first kubelet request.
func NewValidator(ctx context.Context, cfg Config) (Validator, error) {
	if cfg.Issuer == "" {
		return nil, errors.New("oidc: issuer is required")
	}
	if cfg.MaxTokenLifetime <= 0 {
		return nil, fmt.Errorf("oidc: max token lifetime must be positive, got %s", cfg.MaxTokenLifetime)
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

	keys := newCachedKeySet(jwksURL, httpClient)
	if err := keys.prime(ctx); err != nil {
		return nil, fmt.Errorf("oidc: fetch the token signing keys from %q: %w", jwksURL, err)
	}
	verifier := oidc.NewVerifier(cfg.Issuer, keys, &oidc.Config{
		SkipClientIDCheck:    true,
		SupportedSigningAlgs: algs,
	})
	return &goOIDCValidator{
		verifier:               verifier,
		keys:                   keys,
		maxLifetime:            cfg.MaxTokenLifetime,
		allowNonPodBoundTokens: cfg.AllowNonPodBoundTokens,
	}, nil
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
	verifier               *oidc.IDTokenVerifier
	keys                   *cachedKeySet // the verifier's key set; tests steer its clock
	maxLifetime            time.Duration
	allowNonPodBoundTokens bool
}

func (v *goOIDCValidator) Validate(ctx context.Context, rawToken string) (*Claims, error) {
	verifyCtx, outcome := withVerifyOutcome(ctx)
	idToken, err := v.verifier.Verify(verifyCtx, rawToken)
	if err != nil {
		if outcome.keysUnavailable {
			return nil, fmt.Errorf("%w: %w", ErrSigningKeysUnavailable, err)
		}
		return nil, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	var raw rawClaims
	if err := idToken.Claims(&raw); err != nil {
		return nil, fmt.Errorf("%w: parse claims: %w", ErrInvalidToken, err)
	}
	if err := checkLifetime(idToken.IssuedAt, idToken.Expiry, time.Now(), v.maxLifetime); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	if !v.allowNonPodBoundTokens && (raw.K8s.Pod.Name == "" || raw.K8s.Pod.UID == "") {
		return nil, fmt.Errorf("%w: %w: no kubernetes.io pod name and uid", ErrInvalidToken, ErrTokenNotPodBound)
	}
	return &Claims{
		Subject:  raw.Sub,
		Audience: []string(raw.Aud),
		Issuer:   idToken.Issuer,
		Expiry:   idToken.Expiry,
		IssuedAt: idToken.IssuedAt,
		Pod:      raw.K8s.Pod.Name,
		PodUID:   raw.K8s.Pod.UID,
		Node:     raw.K8s.Node.Name,
	}, nil
}

// checkLifetime enforces the lifetime cap (ADR-0028). go-oidc has already
// checked exp against now. A token without iat has no determinable
// lifetime; go-oidc leaves IssuedAt zero when the claim is absent.
func checkLifetime(iat, exp, now time.Time, maxLifetime time.Duration) error {
	switch {
	case iat.IsZero():
		return fmt.Errorf("%w: no iat claim", ErrTokenLifetime)
	case iat.After(now.Add(maxIssuedAtSkew)):
		return fmt.Errorf("%w: issued in the future (iat %s)", ErrTokenLifetime, iat.UTC().Format(time.RFC3339))
	case exp.Sub(iat) > maxLifetime:
		return fmt.Errorf("%w: lifetime %s exceeds the maximum %s", ErrTokenLifetime, exp.Sub(iat), maxLifetime)
	}
	return nil
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
